package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// gardener_chain_spawn.go — engine-side v1 gardener succession (C4, epic
// ka-0000002506; gardener-chaining design §3.2 / §7.1). When a gardener
// TERMINATES because its terminal goal was achieved AND it declared a
// `chain.to` with mode "spawn", the trigger-engine materializes the heir
// gardener itself — create + enable the heir bead, stamp the `succeeds: <self>`
// provenance edge so the workflow rail renders, and parent it under the same
// epic. This is the convenience path the design defers to v1 (§3.2): the v0
// agent-side nudge (ComposeChainHandoffNudge) hands the baton via the wrap-up,
// whereas v1 hands it off with zero agent latency, at the cost of the engine
// gaining create-gardener authority.
//
// The blast radius of that authority is bounded three ways, matching the
// design's guard rails (§3.4):
//
//   - It only fires on a GOAL-achievement stop kind (specific_bead_closed /
//     bead_query / goal_satisfied), never on operator-willed (manual_force,
//     hard_stop) or premature (max_runs/wall_clock/…) termination. Premature
//     stops never even reach gardenerTerminate — they route to
//     resume-or-escalate — but we re-check defensively here.
//   - It runs ChainHeirGuard (cycle + depth ≤ maxChainDepth) before creating
//     anything; a guard failure stamps a refuse comment and creates NO heir,
//     leaving the chain for the operator to extend or break (refuse-and-decide).
//   - Mode must be "spawn"; "arm" (dormant successor) is reserved and is a
//     no-op here.
//
// The bead-shaping logic (heirBeadSpec) is a deterministic function of the
// predecessor's detail + chain.to, so it is unit-testable with a fake daemon.

// chainSpawnDaemon is the optional daemon capability set the engine-side heir
// spawn needs ON TOP of the gardenerDaemon surface: create the heir bead, label
// it, parent it, comment provenance, and resolve the existing succeeds-chain
// (GetMentions) for the guard. The production *beadsapi.Client satisfies it;
// gardenerTerminate type-asserts for it and degrades to the v0/no-op path when a
// mock daemon does not implement it (mirrors the GetBeadStatus
// optional-capability pattern in gardener_evaluator.go).
type chainSpawnDaemon interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddLabel(ctx context.Context, beadID, label string) error
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	AddComment(ctx context.Context, beadID, author, text string) error
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	GetMentions(ctx context.Context, beadID string, limit int) ([]beadsapi.BeadReferenceSummary, error)
}

// chainSpawnAuthor is the created_by / comment author stamped on engine-created
// heirs so their provenance reads "the engine did this", distinct from an
// agent- or operator-authored gardener.
const chainSpawnAuthor = "trigger-engine"

// stopKindIsGoalAchievement reports whether a terminal stop kind means the
// gardener's goal was genuinely REACHED (so an heir handoff is warranted), as
// opposed to operator-willed termination (manual_force / hard_stop) or an
// exit-condition close. Only goal-achievement kinds arm the v1 succession;
// everything else terminates without spawning an heir.
//
// The premature kinds (max_runs/wall_clock/no_progress_for/consecutive_failures)
// are deliberately absent: they never reach gardenerTerminate (the fire path
// converts them to resume-or-escalate), and even if one did it is NOT a goal
// achievement, so it must not hand off.
func stopKindIsGoalAchievement(kind string) bool {
	switch kind {
	case scheduler.StopKindGoalSatisfied,
		scheduler.StopKindSpecificBeadClosed,
		scheduler.StopKindBeadQuery:
		return true
	default:
		return false
	}
}

// maybeSpawnChainHeir is called from gardenerTerminate just before the gardener
// bead is closed. When the gardener terminated on a goal-achievement kind and
// declared a spawn-mode `chain.to`, it materializes the heir gardener. It is
// best-effort and never returns an error: a failure to spawn the heir must not
// block the predecessor's own clean termination (the chain can always be
// re-armed by the operator or the v0 nudge). All outcomes are logged; a guard
// refusal additionally stamps a comment on the predecessor so the broken link
// is visible.
func maybeSpawnChainHeir(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef, slot, kind string) {
	chain := trig.Chain
	if chain == nil || chain.To == nil {
		return // no chain declared — nothing to hand off
	}
	// Only loop_end goal achievement hands off; hard_stop / run_end do not.
	if slot != scheduler.StopSlotLoopEnd {
		return
	}
	if !stopKindIsGoalAchievement(kind) {
		return // operator-willed / exit-condition / premature — no handoff
	}
	if !strings.EqualFold(strings.TrimSpace(chain.On), "goal_satisfied") {
		return // only goal_satisfied arms the handoff in v1
	}
	if !strings.EqualFold(strings.TrimSpace(chain.Mode), "spawn") {
		// "arm" (dormant successor) is reserved; nothing to do engine-side.
		logger.Info("gardener chain: mode is not spawn, skipping engine handoff",
			"trigger", trig.ID, "mode", chain.Mode)
		return
	}

	sd, ok := daemon.(chainSpawnDaemon)
	if !ok {
		// Daemon lacks the create/label/dep/mention surface (e.g. a mock).
		// Degrade silently — the v0 agent-side nudge still covers handoff.
		logger.Debug("gardener chain: daemon lacks spawn capability, skipping engine handoff",
			"trigger", trig.ID)
		return
	}

	// Fan-out (C3, design §3.3): a predecessor's goal can arm MORE THAN ONE
	// heir (one per sub-epic). Materialize each declared heir independently —
	// each is guarded, spawned, and audited on its own, so one heir's guard
	// refusal or spawn failure never blocks its siblings. The predecessor's
	// detail is fetched once and shared across the fan-out.
	resolver := NewDaemonChainResolver(sd)
	pred, err := sd.GetBead(ctx, trig.ID)
	if err != nil {
		logger.Warn("gardener chain: fetch predecessor for handoff failed",
			"trigger", trig.ID, "error", err)
		return
	}
	heirs := chain.Heirs()
	for i, to := range heirs {
		// Guard: walk the existing succeeds-chain from the predecessor and refuse
		// a cycle or an over-cap chain (design §3.4). On refusal, stamp a comment
		// and create NO heir for this branch.
		chainLen, guardErr := ChainHeirGuard(ctx, resolver, trig.ID, to)
		if guardErr != nil {
			msg := fmt.Sprintf(
				"gardener chaining: heir %q NOT spawned — chaining guard refused (%s). "+
					"The chain is left for an operator to extend or break.", heirName(to), guardErr.Error())
			if cErr := sd.AddComment(ctx, trig.ID, chainSpawnAuthor, msg); cErr != nil {
				logger.Warn("gardener chain: guard-refuse comment failed",
					"trigger", trig.ID, "heir_index", i, "error", cErr)
			}
			logger.Warn("gardener chain: heir refused by guard",
				"trigger", trig.ID, "heir_index", i, "chain_len", chainLen, "error", guardErr)
			continue
		}

		heirID, err := spawnChainHeir(ctx, sd, logger, trig, pred, to)
		if err != nil {
			logger.Warn("gardener chain: heir spawn failed",
				"trigger", trig.ID, "heir_index", i, "error", err)
			continue
		}
		logger.Info("gardener chain: heir spawned (engine v1)",
			"predecessor", trig.ID, "heir", heirID,
			"phase", to.Phase, "chain_link", chainLen+1, "fan_out", len(heirs))
		// Best-effort breadcrumb on the predecessor so the handoff is auditable
		// without reading the heir.
		note := fmt.Sprintf("gardener chaining: handed off to heir %s (link %d of chain).", heirID, chainLen+1)
		if cErr := sd.AddComment(ctx, trig.ID, chainSpawnAuthor, note); cErr != nil {
			logger.Debug("gardener chain: handoff comment failed", "trigger", trig.ID, "error", cErr)
		}
	}
}

// heirBeadSpec is the fully-resolved shape of the heir gardener bead: the
// CreateBead request plus the labels and parent epic applied after creation.
// Splitting assembly (pure, given the predecessor detail) from the daemon
// writes keeps the field/label/provenance derivation unit-testable without a
// live daemon.
type heirBeadSpec struct {
	Req    beadsapi.CreateBeadRequest
	Labels []string
	Epic   string // parent-child edge target ("" → no parent edge)
	// OperatedBy is the human operator the heir inherits (from trig.OperatedBy
	// → predecessor operated_by). Surfaced on the spec so the human-operator
	// gate (task ka-0000002839) can inspect it without re-parsing Req.Fields.
	// Empty when neither source carried one.
	OperatedBy string
}

// spawnChainHeir materializes a single heir gardener bead from one of the
// predecessor's chain.to declarations and returns its ID. The predecessor's
// detail is passed in (fetched once by the caller and shared across a fan-out);
// this assembles the heir spec, creates the bead, then best-effort labels +
// parents it.
func spawnChainHeir(ctx context.Context, sd chainSpawnDaemon, logger *slog.Logger, trig TriggerDef, pred *beadsapi.BeadDetail, to *ChainTo) (string, error) {
	spec, err := buildHeirBeadSpec(trig, pred, to)
	if err != nil {
		return "", err
	}

	// Human-operator gate (epic ka-0000004835, task ka-0000002839). The heir
	// gardener is a long-lived autonomous spawner; if it inherits no human
	// operator it will spawn crew that boot unattributed and dead-end the
	// daemon resolver. Refuse the heir in enforce mode when the inherited
	// operated_by isn't a resolvable human, rather than chain an
	// un-attributable gardener forward. inheritedHeirFields already prefers
	// trig.OperatedBy → predecessor operated_by; we read the resolved value
	// back off the heir spec so the gate sees exactly what will be stamped.
	if _, gErr := gateSpawnOperator(HumanOperatorModeFromEnv(), logger,
		"gardener-chain-heir", heirName(to), spec.OperatedBy); gErr != nil {
		return "", gErr
	}

	id, err := sd.CreateBead(ctx, spec.Req)
	if err != nil {
		return "", fmt.Errorf("create heir bead: %w", err)
	}

	// Best-effort labels + parentage — log and continue; the heir already
	// exists and is enabled, so a label hiccup must not abort the handoff.
	for _, lbl := range spec.Labels {
		if lErr := sd.AddLabel(ctx, id, lbl); lErr != nil {
			logger.Debug("gardener chain: heir label failed", "heir", id, "label", lbl, "error", lErr)
		}
	}
	if spec.Epic != "" {
		if dErr := sd.AddDependency(ctx, id, spec.Epic, "parent-child", chainSpawnAuthor); dErr != nil {
			logger.Debug("gardener chain: heir parent edge failed", "heir", id, "epic", spec.Epic, "error", dErr)
		}
	}
	return id, nil
}

// buildHeirBeadSpec assembles the heir gardener bead spec from a single
// chain.to heir declaration and the predecessor's live detail. Pure (no
// daemon): the epic resolution, field inheritance, gardener/conditions blocks,
// name, and the `succeeds: <predecessor>` provenance description are all
// deterministic, so every branch is unit-testable. `to` is the specific heir
// (the caller iterates a fan-out); it defaults to trig.Chain.To for the common
// single-heir case.
func buildHeirBeadSpec(trig TriggerDef, pred *beadsapi.BeadDetail, to *ChainTo) (heirBeadSpec, error) {
	if to == nil {
		to = trig.Chain.To
	}

	var predFields map[string]string
	var predLabels []string
	if pred != nil {
		predFields = pred.Fields
		predLabels = pred.Labels
	}

	// Resolve the heir's epic: chain.to.Epic, defaulting to the predecessor's
	// epic ({{same}} / empty → inherit) per design §3.1.
	epic := strings.TrimSpace(to.Epic)
	if epic == "" || epic == "{{same}}" {
		epic = predecessorEpic(predLabels)
	}

	// Inherit the declared binding fields from the predecessor, restricted to
	// the inherit allowlist (Normalize() filled the default set when empty).
	inherited := inheritedHeirFields(trig, predFields, to.Inherit)

	project := strings.TrimSpace(inherited["project"])
	if project == "" {
		project = strings.TrimSpace(trig.Project)
	}

	role := strings.TrimSpace(to.Role)
	if role == "" {
		role = "gardener"
	}

	// Assemble the heir gardener block. The loop_end is a parent-scoped
	// bead_query over the heir's epic — the canonical "drain the subtree" goal —
	// so the heir terminates (and, if it too declares a chain, hands off again)
	// when its epic's children are all closed.
	capBlock := map[string]any{
		"terminal_goal": to.TerminalGoal,
		"stop": map[string]any{
			"loop_end": map[string]any{
				"any": []any{
					map[string]any{
						"kind":     "bead_query",
						"value":    "0",
						"op":       "==",
						"types":    []string{"task", "bug", "epic"},
						"statuses": []string{"open", "in_progress", "blocked"},
						"parent":   epic,
					},
					map[string]any{"kind": "max_runs"},
				},
			},
		},
	}
	capJSON, err := json.Marshal(capBlock)
	if err != nil {
		return heirBeadSpec{}, fmt.Errorf("marshal heir gardener block: %w", err)
	}

	// Pre-flight conditions.
	//
	// DEFAULT: fire only while the heir's own epic still has open children (the
	// standard gardener preflight). The closed-root short-circuit (ka-0000008051)
	// dormants the heir once the epic closes.
	//
	// JOIN (fan-in, C3, design §3.3): when the heir declares a `join_parent`, it
	// is a "ship"-style gardener that must wait for ALL of its siblings under the
	// umbrella epic to close. Its preflight is instead `==0 open under
	// <join_parent>`, so it simply won't fire until that subtree is empty — no
	// new primitive, just a different preflight parent. The op/parent flip is the
	// whole join mechanism.
	preflightParent := epic
	preflightOp := ">"
	if jp := strings.TrimSpace(to.JoinParent); jp != "" {
		preflightParent = jp
		preflightOp = "=="
	}
	condJSON, err := json.Marshal([]map[string]any{{
		"type":     "bead_query",
		"op":       preflightOp,
		"value":    "0",
		"types":    []string{"task", "bug", "epic"},
		"statuses": []string{"open", "in_progress", "blocked"},
		"parent":   preflightParent,
	}})
	if err != nil {
		return heirBeadSpec{}, fmt.Errorf("marshal heir conditions: %w", err)
	}

	// `enabled` is boolean in the type:trigger schema — it must be a JSON bool
	// or the daemon 400-rejects the create (ka-0000003627); everything else
	// here is string-typed.
	fields := map[string]any{
		"action":     "spawn_agent",
		"role":       role,
		"project":    project,
		"enabled":    true,
		"gardener":   string(capJSON),
		"conditions": string(condJSON),
	}

	// BRANCH (conditional, C3, design §3.3): when the heir declares a
	// `branch_decision_key`, it is a branch-A/branch-B gardener that wakes on a
	// specific operator DECISION rather than on the predecessor's plain goal
	// handoff. Author it with a `decision_resolved` trigger entry keyed on that
	// decision bead — a real event source (choreography half), so the branch is
	// resilient. This makes the heir a trigger:multi gardener (it carries a
	// triggers[] array); TriggerSourceLabelForFields below routes the label
	// accordingly.
	if bk := strings.TrimSpace(to.BranchDecisionKey); bk != "" {
		trigJSON, tErr := json.Marshal([]map[string]any{{
			"kind":         "decision_resolved",
			"decision_key": bk,
		}})
		if tErr != nil {
			return heirBeadSpec{}, fmt.Errorf("marshal heir branch trigger: %w", tErr)
		}
		fields["triggers"] = string(trigJSON)
	}
	// Carry inherited bindings (operated_by, slack_*, thread_id, timezone, cron)
	// onto the heir so the workflow stays coherent. thread_id is the wish (thread
	// head) pointer — restamping it keeps an heir chained from a wish-bound
	// predecessor bound to that same wish (ka-0000006315). "slack_thread" is a
	// legacy alias that maps to no real bead field; it is kept here only so an
	// explicit Inherit list naming it does not error, and carries nothing.
	for k, v := range inherited {
		if v == "" {
			continue
		}
		switch k {
		case "operated_by", "slack_channel", "thread_id", "slack_thread", "timezone", "cron":
			fields[k] = v
		}
	}
	if to.PromptAdvice != "" {
		fields["prompt_advice"] = to.PromptAdvice
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return heirBeadSpec{}, fmt.Errorf("marshal heir fields: %w", err)
	}

	// Mislabel guardrail (bug ka-0000001688): the heir is a single-cron gardener
	// (it carries a flat `cron` + `conditions`, never a triggers[] array), so this
	// resolves to trigger:schedule in the common case. Routing it through the
	// shared helper keeps the engine-side authoring path honest if an inherited
	// field ever carries a non-empty triggers[] — it would then be born
	// trigger:multi rather than a silently-never-firing trigger:schedule.
	triggersField, _ := fields["triggers"].(string)
	labels := []string{
		"project:" + project,
		"role:gardener",
		TriggerSourceLabelForFields("trigger:schedule", triggersField),
	}
	if epic != "" {
		labels = append(labels, "gardener-epic:"+epic)
	}

	return heirBeadSpec{
		Req: beadsapi.CreateBeadRequest{
			Title:       heirName(to),
			Type:        "trigger",
			Kind:        "data",
			Description: composeHeirDescription(trig.ID, to),
			CreatedBy:   chainSpawnAuthor,
			Fields:      json.RawMessage(fieldsJSON),
		},
		Labels:     labels,
		Epic:       epic,
		OperatedBy: inherited["operated_by"],
	}, nil
}

// predecessorEpic returns the predecessor gardener's own epic — its
// gardener-epic:<id> label. Empty when the predecessor has no such label (the
// heir is then created epic-less and renders as a chain root).
func predecessorEpic(labels []string) string {
	for _, lbl := range labels {
		if rest, ok := strings.CutPrefix(lbl, "gardener-epic:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// inheritedHeirFields collects the binding fields the heir copies from the
// predecessor, restricted to the inherit allowlist (design §3.1 default set:
// project, slack_channel, operated_by, thread_id, slack_thread). Reads from the
// predecessor's raw bead fields. project / OperatedBy always carry even if not
// explicitly listed — an heir with no project can never render or fire.
func inheritedHeirFields(trig TriggerDef, predFields map[string]string, inherit []string) map[string]string {
	out := map[string]string{}
	for _, key := range inherit {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if v := strings.TrimSpace(predFields[key]); v != "" {
			out[key] = v
		}
	}
	if out["project"] == "" {
		if v := strings.TrimSpace(predFields["project"]); v != "" {
			out["project"] = v
		} else if strings.TrimSpace(trig.Project) != "" {
			out["project"] = strings.TrimSpace(trig.Project)
		}
	}
	if out["operated_by"] == "" {
		if v := strings.TrimSpace(trig.OperatedBy); v != "" {
			out["operated_by"] = v
		} else if v := strings.TrimSpace(predFields["operated_by"]); v != "" {
			out["operated_by"] = v
		}
	}
	return out
}

// heirName builds the heir gardener's display name from its phase (preferred),
// so the workflow rail labels the card meaningfully.
func heirName(to *ChainTo) string {
	if p := strings.TrimSpace(to.Phase); p != "" {
		return fmt.Sprintf("%s gardener (chain heir)", p)
	}
	return "gardener (chain heir)"
}

// composeHeirDescription writes the heir's description carrying the
// `succeeds: <predecessor>` typed mention. That mention is what materializes
// the chains_to/succeeds graph edge (model.DepChainsTo) so the /maps workflow
// rail draws the predecessor→heir arrow.
func composeHeirDescription(predID string, to *ChainTo) string {
	var b strings.Builder
	if p := strings.TrimSpace(to.Phase); p != "" {
		fmt.Fprintf(&b, "Chain heir (phase: %s) — created by the trigger-engine on the predecessor's goal handoff (gardener-chaining v1, design §3.2).\n\n", p)
	} else {
		b.WriteString("Chain heir — created by the trigger-engine on the predecessor's goal handoff (gardener-chaining v1, design §3.2).\n\n")
	}
	// The provenance edge: heir succeeds predecessor.
	fmt.Fprintf(&b, "succeeds: %s\n", predID)
	if to.TerminalGoal != "" {
		fmt.Fprintf(&b, "\nTerminal goal: %s\n", to.TerminalGoal)
	}
	if to.PromptAdvice != "" {
		fmt.Fprintf(&b, "\nPrompt advice: %s (read it with `gb advice show %s`).\n", to.PromptAdvice, to.PromptAdvice)
	}
	return b.String()
}
