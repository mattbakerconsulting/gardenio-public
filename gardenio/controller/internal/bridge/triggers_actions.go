package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/shorttitle"
)

// fireAction dispatches the configured action for a matched item.
// It checks permissions before executing and records an audit trail.
//
// Returns the spawned agent bead ID for spawn_agent / apply_formula actions
// (empty for notify / create_bead). The gardener post-fire bookkeeping in
// evalOne uses this to stamp LastAgentID on the trigger's Gardener block so
// one_spawn_at_a_time can gate against the still-running run.
func (e *TriggerEngine) fireAction(ctx context.Context, trig TriggerDef, m TriggerMatch) (string, error) {
	// Stamp the trigger's operator onto the context before any daemon write
	// (including audit-bead creation on the deny / allow / error paths).
	// kbeads R2 rejects daemon writes without X-Operated-By; without this,
	// recordTriggerAudit fails with HTTP 400 "operator could not be resolved"
	// even when the action itself stamps via ctxWithTriggerOperator (ka-0000006696,
	// same root cause as ka-0000001047 / ka-0000001619). ctxWithTriggerOperator
	// is idempotent, so the per-action calls remain harmless.
	//
	// Attribution precedence for an autonomous fire:
	//   1. the trigger bead's own HUMAN author (ctxWithTriggerOperator);
	//   2. a source-resolved match operator — e.g. the Jira ticket reporter
	//      mapped to a fleet human (matchOperatorResolver, ka-0000001781);
	//   3. the engine's DefaultOperator; then fleet auto-attribution.
	// The match resolver fills ONLY when the trigger author is not itself a
	// human, so a human-authored trigger keeps its attribution, and it runs
	// before the default so a real reporter beats the catch-all operator.
	ctx = ctxWithTriggerOperator(ctx, trig)
	if e.matchOperatorResolver != nil && !IsHumanOperator(beadsapi.OperatorFromContext(ctx)) {
		if op := e.matchOperatorResolver(ctx, m); IsHumanOperator(op) {
			ctx = beadsapi.WithOperator(ctx, op)
		}
	}
	ctx = e.ctxWithTriggerOperatorOrDefault(ctx, trig)

	// Materialize fire-time template vars before any template expansion.
	// Sources that fire on a clock publish {{fire_time}} (RFC3339 UTC), but
	// trigger authors commonly write {{fire_time_iso}}; alias it here — at
	// the single dispatch chokepoint — so every source and every action
	// resolves both spellings. Event-driven sources that publish no fire
	// time at all get the evaluation time, so a fire-time placeholder never
	// passes through expansion as a literal (ka-0000006473).
	ensureFireTimeVars(&m, time.Now())

	// Authorization check (advisory fields on the trigger bead).
	authResult := checkTriggerAuth(trig)
	if !authResult.Allowed {
		e.logger.Warn("triggers: action denied",
			"trigger", trig.ID, "match", m.Key, "action", trig.Action,
			"created_by", trig.CreatedBy, "reason", authResult.Reason)
		recordTriggerAudit(ctx, e.daemon, e.logger, TriggerAuditEntry{
			TriggerID:    trig.ID,
			TriggerTitle: trig.Title,
			Source:       trig.Source,
			Action:       trig.Action,
			MatchKey:     m.Key,
			MatchTitle:   m.Title,
			Project:      trig.Project,
			Role:         trig.Role,
			CreatedBy:    trig.CreatedBy,
			Outcome:      "deny",
			Reason:       authResult.Reason,
			Timestamp:    time.Now(),
		})
		return "", fmt.Errorf("trigger action denied: %s", authResult.Reason)
	}

	// Authoritative spawn-scope recheck at fire time. AllowedProjects /
	// AllowedRoles on the trigger bead are client-supplied advisory fields
	// (B1 finding). For spawn-producing actions (spawn_agent, apply_formula)
	// we re-validate project/role against the trigger creator's CURRENT
	// user-bead scopes — this is what
	// kbeads authorizeAgentSpawn checks at bead-create time, repeated here so
	// that a trigger crafted with AllowedRoles=["admin"] (or a creator whose
	// scopes were revoked since) cannot spawn out of scope at fire time.
	if spawnResult := recheckSpawnAuthzAtFire(ctx, e.daemon, trig); !spawnResult.Allowed {
		e.logger.Warn("triggers: spawn denied at fire time",
			"trigger", trig.ID, "match", m.Key, "action", trig.Action,
			"created_by", trig.CreatedBy, "project", trig.Project, "role", trig.Role,
			"reason", spawnResult.Reason)
		recordTriggerAudit(ctx, e.daemon, e.logger, TriggerAuditEntry{
			TriggerID:    trig.ID,
			TriggerTitle: trig.Title,
			Source:       trig.Source,
			Action:       trig.Action,
			MatchKey:     m.Key,
			MatchTitle:   m.Title,
			Project:      trig.Project,
			Role:         trig.Role,
			CreatedBy:    trig.CreatedBy,
			Outcome:      "deny",
			Reason:       spawnResult.Reason,
			Timestamp:    time.Now(),
		})
		return "", fmt.Errorf("trigger action denied: %s", spawnResult.Reason)
	}

	// Runaway-protection: gate spawn-producing actions on the shared
	// SpawnGuard. Notify and create_bead don't create agent pods, so they
	// bypass the guard. Record() is called only after successful spawn so
	// failures don't consume the cap.
	spawnsAgent := trig.Action == TriggerActionSpawnAgent || trig.Action == TriggerActionApplyFormula
	if spawnsAgent && e.guard != nil {
		if ok, reason := e.guard.Allow(); !ok {
			e.logger.Warn("triggers: spawn blocked by spawnguard",
				"trigger", trig.ID, "match", m.Key, "action", trig.Action,
				"reason", string(reason))
			return "", fmt.Errorf("runaway protection: %s", reason)
		}
	}

	var (
		agentID string
		err     error
	)
	switch trig.Action {
	case TriggerActionSpawnAgent:
		agentID, err = e.actionSpawnAgent(ctx, trig, m)
	case TriggerActionNotify:
		err = e.actionNotify(ctx, trig, m)
	case TriggerActionCreateBead:
		err = e.actionCreateBead(ctx, trig, m)
	case TriggerActionAssert:
		err = e.actionAssert(ctx, trig, m)
	case TriggerActionApplyFormula:
		agentID, err = e.actionApplyFormula(ctx, trig, m)
	case TriggerActionWakePROwner:
		err = e.actionWakePROwner(ctx, trig, m)
	default:
		err = fmt.Errorf("unknown trigger action: %s", trig.Action)
	}
	if spawnsAgent && err == nil && e.guard != nil {
		e.guard.Record()
	}
	if err == nil && agentID != "" {
		e.stampGardenerRunLabel(ctx, trig, agentID)
	}

	// Record audit for user-created triggers (skip for system triggers to
	// avoid extra bead creation noise in the common case).
	if trig.CreatedBy != "" {
		outcome := "allow"
		reason := authResult.Reason
		if err != nil {
			outcome = "error"
			reason = err.Error()
		}
		recordTriggerAudit(ctx, e.daemon, e.logger, TriggerAuditEntry{
			TriggerID:    trig.ID,
			TriggerTitle: trig.Title,
			Source:       trig.Source,
			Action:       trig.Action,
			MatchKey:     m.Key,
			MatchTitle:   m.Title,
			Project:      trig.Project,
			Role:         trig.Role,
			CreatedBy:    trig.CreatedBy,
			Outcome:      outcome,
			Reason:       reason,
			Timestamp:    time.Now(),
		})
	}

	return agentID, err
}

// gardenerRunLabel is the label every gardener-fired run carries so the run is
// gardener-marked regardless of the role the trigger spawns it under.
const gardenerRunLabel = "role:gardener"

// stampGardenerRunLabel guarantees a run spawned by a gardener-shape trigger
// (trig.Gardener != nil) carries role:gardener. SpawnAgent labels the run
// role:<spawn role> from the trigger's top-level `role` field, which for
// gardeners authored before the captain->gardener rename is still "captain" —
// so with the dual-written role:captain label gone (phase 5) those runs were
// created marked role:captain only, and the saturation gate / any
// role:gardener reader missed them. The spawn role itself is left untouched
// (it also drives the pod role, spawn scopes and advice), so a plan-gardener
// trigger keeps role=plan-gardener and is merely also gardener-marked.
// Best-effort: a label failure never fails the fire.
func (e *TriggerEngine) stampGardenerRunLabel(ctx context.Context, trig TriggerDef, agentID string) {
	if trig.Gardener == nil || agentID == "" {
		return
	}
	for _, r := range strings.Split(trig.Role, ",") {
		if strings.TrimSpace(r) == "gardener" {
			return // SpawnAgent already stamped role:gardener
		}
	}
	if err := e.daemon.AddLabel(ctx, agentID, gardenerRunLabel); err != nil {
		e.logger.Warn("triggers: failed to stamp role:gardener on gardener run",
			"trigger", trig.ID, "agent", agentID, "error", err)
	}
}

// actionSpawnAgent creates a task bead and spawns an agent. Returns the
// spawned agent bead ID so the gardener post-fire bookkeeping in evalOne can
// stamp LastAgentID for one_spawn_at_a_time gating.
//
// Gardener-shape fires (trig.Gardener != nil) absorb the scheduler-native
// spawn-time behavior (B6.1 / ka-0000005779) so that after the engine swap
// runs through TriggerEngine, gb prime still renders the "## Scheduled Loop
// Context" section and AssignBead-pinned gardeners stop creating per-fire
// task beads. Non-gardener triggers (trig.Gardener == nil) keep the original
// per-fire task semantics unchanged.
func (e *TriggerEngine) actionSpawnAgent(ctx context.Context, trig TriggerDef, m TriggerMatch) (string, error) {
	if strings.TrimSpace(trig.Project) == "" {
		// A project-less spawn writes an agent bead the controller's credential
		// guard then refuses permanently (spawn_credential_guard.go:70-77),
		// leaving an orphan. The daemon rejects such triggers at AUTHORING
		// (runaway_validation.go:275-306); this is the fire-time backstop for
		// triggers that predate that gate or were written around it.
		e.logger.Error("triggers: refusing to spawn — trigger has no project",
			"trigger", trig.ID, "action", trig.Action)
		return "", fmt.Errorf("trigger %s has no project — refusing to spawn a project-less agent", trig.ID)
	}

	// Inherit the trigger's authorizing operator onto every write made by this
	// firing — task bead, agent bead, audit row. Triggers fire autonomously,
	// so there is no per-event human author; the right answer is "the human
	// who created the trigger" (parallel to scheduled-agent inheritance from
	// P2). When trig.CreatedBy is missing or untyped, fall back to the
	// system sentinel keyed on source (R1 #15).
	ctx = e.ctxWithTriggerOperatorOrDefault(ctx, trig)

	// Idempotency guard (gd-pLf5LSHDEh): a re-fire of the same (trigger, match)
	// that slips past the last_match skip guard in Evaluate — e.g. the
	// last_match write raced with an engine restart — must find the agent
	// bead this trigger already spawned for this match instead of creating a
	// second one. Fail-open: a lookup error is logged and treated as "no
	// existing spawn" so a transient daemon hiccup can never permanently
	// block a trigger from firing.
	if existing, err := e.findExistingSpawnForMatch(ctx, trig, m); err != nil {
		e.logger.Warn("triggers: idempotency lookup failed, proceeding with spawn",
			"trigger", trig.ID, "match", m.Key, "error", err)
	} else if existing != "" {
		e.logger.Info("triggers: spawn already exists for match, skipping duplicate spawn",
			"trigger", trig.ID, "match", m.Key, "agent", existing)
		return existing, nil
	}

	// Gardener.AssignBead pinning (B6.1): skip per-fire task creation and
	// point the agent at the long-lived assign bead. Mirrors
	// scheduler.spawnScheduledAgent's AssignBead branch (scheduler.go:1178).
	// triggered-by lineage is intentionally omitted here — the assign bead
	// is a long-lived gardener home; stamping per-fire lineage on it would
	// accumulate noise. The agent bead still carries trigger_id via
	// extraFields below.
	var (
		taskID string
		err    error
	)
	switch {
	case trig.Gardener != nil && trig.Gardener.AssignBead != "":
		taskID = trig.Gardener.AssignBead
	case m.orphanTaskID != "":
		// Re-fire of a window whose earlier fire filed this task but spawned no
		// agent (gd-CZnK7jeKSG): spawn onto it rather than file a duplicate. Its
		// triggered-by lineage was written when it was filed.
		taskID = m.orphanTaskID
	default:
		taskID, err = e.createTaskBead(ctx, trig, m)
		if err != nil {
			return "", fmt.Errorf("creating task bead: %w", err)
		}

		// Stamp `triggered-by` lineage on the spawned tracking task so the agent's
		// bead carries an explicit edge to the trigger definition (and to any
		// originating source bead surfaced in the match). Auto-link M2a
		// (ka-0000007272, epic ka-0000000933).
		e.writeTriggerLineage(ctx, taskID, trig, m)
	}

	prompt := ExpandTemplate(e.firePrompt(ctx, trig, m), m.TemplateVars)
	if prompt == "" {
		prompt = fmt.Sprintf("Work on %s: %s", m.Key, m.Title)
	}

	agentName := triggerAgentName(trig, m.Key)

	extraFields := map[string]string{
		"trigger_id":    trig.ID,
		"trigger_title": trig.Title,
		"source":        trig.Source,
		"spawned_by":    "trigger:" + trig.ID,
	}
	// Per-trigger runtime backend opt-in (ka-0000003026): stamp the trigger's
	// execution_target onto the agent bead so the controller's agent-metadata
	// precedence layer routes this spawn (e.g. to lambda_microvm) without a
	// role- or project-wide flip. Empty means "inherit role/project/global".
	// The laptop marker is refused: it is gb's for a session on its owner's
	// laptop, and the controller never runs a pod for it, so a trigger (or a
	// plan gardener, whose spec lands here) stamping it would spawn nothing.
	switch {
	case podmanager.IsLaptopTarget(strings.TrimSpace(trig.ExecutionTarget)):
		e.logger.Warn("triggers: ignoring execution_target=laptop on trigger; it is reserved for gb runner sessions",
			"trigger", trig.ID)
	case trig.ExecutionTarget != "":
		extraFields["execution_target"] = trig.ExecutionTarget
	}
	// Top-level slack_channel routing (ka-0000003647): a plain schedule trigger
	// (bare `action: spawn_agent`, no gardener block) declares its target Slack
	// channel at the top level, so its recurring spawns (e.g. a daily standup
	// poster) route their card/thread/squawks to a non-project channel. Stamp
	// it as `schedule_slack_channel`, which the slack-bridge reads as the
	// agent's spawn channel (bot_agents.go). A gardener carries the channel in
	// its `gardener` block instead; stampGardenerLoopContext (below) stamps
	// spec.SlackChannel AFTER this, so a gardener's own channel wins when both
	// are set — matching the field's documented precedence.
	if trig.SlackChannel != "" {
		extraFields["schedule_slack_channel"] = trig.SlackChannel
	}
	// Human-operator gate (epic ka-0000004835, task ka-0000002839). The
	// operator on ctx is whatever ctxWithTriggerOperator resolved from the
	// trigger bead's author (trig.OperatedBy → trig.CreatedBy); it is empty
	// when the trigger has no typed author, and may be a non-human typed id
	// (jira:<accountId>, gitlab:<id>) that the daemon rejects. Refuse the
	// spawn in enforce mode rather than dispatch an agent whose pod boots
	// unattributed and dead-ends the resolver. Stamp only a HUMAN operator
	// onto the agent bead so it never carries a value the daemon fails closed
	// on (the "stamped jira:… that isHumanOperator rejects" bug, ka-0000002128).
	op, err := gateSpawnOperator(e.requireHumanMode, e.logger,
		"trigger:"+trig.Source, agentName, beadsapi.OperatorFromContext(ctx))
	if err != nil {
		return "", err
	}
	if IsHumanOperator(op) {
		extraFields["operated_by"] = op
		// Live-card footer navigability (ka-0000003131): stamp the human this
		// agent was spawned on behalf of as `owner` so the slack-bridge can
		// render "Owner: <human>" on the card without re-deriving it from
		// operated_by (which is also consumed by the audit/attribution path).
		// Only a HUMAN operator is stamped — a synthetic/non-human id would
		// render a meaningless owner line.
		extraFields["owner"] = op
	}

	// Single-claim guard (ka-0000004654): a per-fire / matched-step task is
	// single-claim work — opt the daemon-side guard in so two near-simultaneous
	// fires of the same gardener (e.g. a multi-source cron + bead_event trigger
	// whose two evalOne passes both clear the in-memory max_concurrent /
	// one_spawn_at_a_time gates before either persists its post-fire state)
	// cannot each spawn a crew that races to claim the SAME task bead. NOT set
	// for the AssignBead-pinned gardener path below, which intentionally reuses
	// one long-lived task across runs under its own one_spawn_at_a_time gate.
	if trig.Gardener == nil || trig.Gardener.AssignBead == "" {
		extraFields["single_claim_task"] = "1"
	}
	// Add match key to extra fields. Sanitize the source name into a valid
	// identifier so multi-word sources (e.g. "schedule-mirror") yield a
	// registered type:agent field (`schedule_mirror_key`) instead of a
	// hyphenated key that ValidateTypeFields rejects (ka-0000000883).
	if m.Key != "" {
		extraFields[triggerSourceFieldKey(trig.Source)] = m.Key
	}

	// Gardener loop-context push-at-spawn (B6.1). Mirrors the schedule_*
	// fields scheduler.spawnScheduledAgent stamps onto a gardener agent bead
	// (scheduler.go:1225-1267). gb prime's "## Scheduled Loop Context"
	// renderer (cmd/gb/prime_schedule.go) keys off these fields; without
	// them the agent doesn't know it's a gardener run and the run-budget /
	// prior-run-wrap-up sections silently disappear.
	if trig.Gardener != nil {
		stampGardenerLoopContext(extraFields, trig)

		// Gardener dispatch classifier (ka-0000003173, design ka-0000007305 §5):
		// recommend dynamic-workflow vs pod-fanout for the matched step bead.
		// Advisory only — we stamp the recommendation + signals onto the agent
		// bead (surfaced by gb prime / the agent card) and prepend a one-line
		// nudge to the prompt. The crew member reading it decides; nothing here
		// forces an execution mode. Fail-open: a nil/unfetchable step bead
		// just skips. m.Key is the matched step bead ID in the gardener path;
		// GetBead is already used elsewhere in this engine (see e.g. the
		// gate-bead lookups above), so reusing it keeps the dependency surface
		// unchanged.
		var stepBead *beadsapi.BeadDetail
		if m.Key != "" {
			if bd, err := e.daemon.GetBead(ctx, m.Key); err != nil {
				e.logger.Warn("triggers: dispatch classifier GetBead failed, skipping",
					"trigger", trig.ID, "step", m.Key, "err", err)
			} else {
				stepBead = bd
			}
		}
		if stepBead != nil {
			rec, sig := ClassifyStepDispatch(stepBead)
			stampDispatchRecommendation(extraFields, rec, sig)
			prompt = dispatchNudge(rec, sig) + prompt
			e.logger.Info("triggers: dispatch classifier",
				"trigger", trig.ID, "step", m.Key, "recommendation", string(rec),
				"from_exec_hint", sig.FromExecHint, "score", sig.Score,
				"uniform_skills", sig.UniformSkills, "inbound_fanout", sig.InboundFanout,
				"inbound_count", sig.InboundCount, "keyword", sig.Keyword,
				"annotation", sig.Annotation)
		}
	}

	agentID, err := e.daemon.SpawnAgent(ctx, agentName, trig.Project, taskID, trig.Role, prompt, extraFields)
	if err != nil {
		return "", fmt.Errorf("spawning agent: %w", err)
	}

	e.logger.Info("triggers: spawned agent",
		"trigger", trig.ID, "match", m.Key, "agent", agentID, "task", taskID)
	e.claimSourceEscalation(ctx, trig, m, agentID)
	return agentID, nil
}

// escalationClaimer is the claim-for verb (beadsapi.Client.ClaimEscalationFor).
// It is optional on TriggerBeadClient so test fakes need not implement it.
type escalationClaimer interface {
	ClaimEscalationFor(ctx context.Context, id, agentID string) (*beadsapi.BeadDetail, error)
}

// matchSourceBeadID is the bead a bead_event match fired on: the bead source's
// source_bead_id, or a multi trigger's bead_event entry's event bead.
func matchSourceBeadID(m TriggerMatch) string {
	if id := m.Fields["source_bead_id"]; id != "" {
		return id
	}
	if m.Fields["multi_entry_kind"] == scheduler.TriggerKindBeadEvent {
		return m.TemplateVars["{{event_bead_id}}"]
	}
	return ""
}

// claimSourceEscalation claims the escalation a bead_event fire is about for
// the agent it just spawned, so the row names its worker from the first
// second (gd-1yeWlbJUJi). The daemon decides whether the agent may hold it
// (claim-for: a member of the escalation's coordinator lane); a refusal or a
// claim already held is logged and never fails the fire.
func (e *TriggerEngine) claimSourceEscalation(ctx context.Context, trig TriggerDef, m TriggerMatch, agentID string) {
	claimer, ok := e.daemon.(escalationClaimer)
	srcID := matchSourceBeadID(m)
	if !ok || srcID == "" || agentID == "" {
		return
	}
	src, err := e.daemon.GetBead(ctx, srcID)
	if err != nil || src == nil || src.Type != "escalation" {
		return
	}
	if _, err := claimer.ClaimEscalationFor(ctx, srcID, agentID); err != nil {
		e.logger.Info("triggers: escalation not claimed for the spawned agent",
			"trigger", trig.ID, "escalation", srcID, "agent", agentID, "error", err)
		return
	}
	e.logger.Info("triggers: claimed escalation for the spawned agent",
		"trigger", trig.ID, "escalation", srcID, "agent", agentID)
}

// stampGardenerLoopContext writes the schedule_* loop-context fields onto the
// extraFields map handed to SpawnAgent. Mirrors the gardener-run stamping in
// scheduler.spawnScheduledAgent (scheduler/scheduler.go:1225-1267) so an
// agent spawned by the bridge trigger engine carries the same gardener
// context an agent spawned by the scheduler-native path would.
//
// RunsSpawned is incremented by 1 to record "you are run N of M" — the
// gardener post-fire bookkeeping bumps the persisted counter *after* this
// spawn returns, so the in-flight run number is RunsSpawned+1. Matches
// scheduler.go:1229.
func stampGardenerLoopContext(fields map[string]string, trig TriggerDef) {
	spec := trig.Gardener
	fields["schedule_id"] = trig.ID
	fields["schedule_title"] = trig.Title
	fields["schedule_runs_spawned"] = strconv.Itoa(spec.RunsSpawned + 1)
	fields["schedule_runs_completed"] = strconv.Itoa(spec.RunsCompleted)
	fields["schedule_max_runs"] = strconv.Itoa(spec.MaxRuns)
	fields["spawn_source"] = "schedule"
	if spec.TerminalGoal != "" {
		fields["schedule_terminal_goal"] = spec.TerminalGoal
	}
	if spec.Name != "" {
		fields["schedule_name"] = spec.Name
	}
	// Live-card footer navigability (ka-0000003131): stamp the resolved gardener
	// as a human-readable name so the slack-bridge can render "Resolved by:
	// <gardener>" on the live agent card without a gardener-bead fetch. Prefer
	// the gardener's declared name, falling back to the trigger bead title.
	// canonical gardener_id (a ka-XXX bead id) is retained separately for the
	// viewer's ownership bucketing; resolved_by is the display projection.
	if name := spec.Name; name != "" {
		fields["resolved_by"] = name
	} else if trig.Title != "" {
		fields["resolved_by"] = trig.Title
	}
	if spec.SlackChannel != "" {
		fields["schedule_slack_channel"] = spec.SlackChannel
	}
	if spec.SlackUserID != "" {
		fields["slack_user_id"] = spec.SlackUserID
	}
	if spec.UserBeadID != "" {
		fields["user_bead_id"] = spec.UserBeadID
	}
	// Capture the prior run's agent bead so gb prime can render the
	// prior-run wrap-up summary. At fire time spec.LastAgentID still refers
	// to the previous run; gardener_evaluator.gardenerPostFire updates it to
	// the new agent *after* this spawn returns. Matches scheduler.go:1265.
	if spec.LastAgentID != "" {
		fields["schedule_prior_agent_id"] = spec.LastAgentID
		// Neutral succession back-pointer (persistent-agents Phase 1, task
		// ka-0000006931, design memo §3.1). Written ADDITIVELY alongside — not
		// instead of — schedule_prior_agent_id, which the gardener path keeps
		// writing verbatim so its 4 readers + golden tests are untouched. The
		// viewer lineage walk reads prior_agent_id OR schedule_prior_agent_id,
		// so a gardener iteration joins the unified chain abstraction while the
		// schedule chain stays as-is. Same value as schedule_prior_agent_id on
		// this path (the prior iteration is the predecessor).
		fields["prior_agent_id"] = spec.LastAgentID
	}
	// lineage_root_id — the first iteration agent bead of this gardener's life,
	// carried on the spec (LineageRoot) and stamped on every subsequent
	// iteration so the viewer can group the whole life-thread by indexed lookup
	// and the per-chain continuation budget is bounded. Empty on the very first
	// fire (no root yet); gardenerPostFire seeds it from that first agent id.
	if spec.LineageRoot != "" {
		fields["lineage_root_id"] = spec.LineageRoot
	}
}

// actionNotify posts a notification without spawning an agent.
func (e *TriggerEngine) actionNotify(ctx context.Context, trig TriggerDef, m TriggerMatch) error {
	ctx = e.ctxWithTriggerOperatorOrDefault(ctx, trig)
	message := ExpandTemplate(trig.Prompt, m.TemplateVars)
	if message == "" {
		message = fmt.Sprintf("New match for trigger %q: %s — %s", trig.Title, m.Key, m.Title)
	}

	if e.notifier != nil {
		// trig.SlackChannel (the trigger bead's `slack_channel` field) routes
		// this notification to a specific channel; empty falls through to the
		// notifier's default channel (ka-0000008204). trig.Project labels the
		// notice bead into the trigger's land.
		e.notifier(WithNoticeProject(ctx, trig.Project), trig.Title, message, trig.SlackChannel)
	}

	e.logger.Info("triggers: notification sent",
		"trigger", trig.ID, "match", m.Key, "channel", trig.SlackChannel)
	return nil
}

// actionCreateBead creates a task bead without spawning.
func (e *TriggerEngine) actionCreateBead(ctx context.Context, trig TriggerDef, m TriggerMatch) error {
	ctx = e.ctxWithTriggerOperatorOrDefault(ctx, trig)
	_, err := e.createTaskBead(ctx, trig, m)
	if err != nil {
		return fmt.Errorf("creating bead: %w", err)
	}
	e.logger.Info("triggers: bead created",
		"trigger", trig.ID, "match", m.Key)
	return nil
}

// actionAssert fires the alert(s) for a violated assertion (feature F5,
// ka-0000008157). Unlike notify/create_bead — which fire on a positive match —
// the bead-state source only produces a match for an assert trigger when its
// `assert` invariant is VIOLATED, so reaching here already means "the predicate
// failed". This action fans out to notify and/or create_bead per the trigger's
// on_violation modifier. It spawns no agent and therefore bypasses the
// SpawnGuard (see the spawnsAgent gate in fireAction).
//
// The match already carries the {{violated}} and {{values}} template vars
// (stamped by beadAssertViolationToMatch) alongside the standard bead vars, so
// the downstream notify message / create_bead body can name what failed and the
// observed values.
func (e *TriggerEngine) actionAssert(ctx context.Context, trig TriggerDef, m TriggerMatch) error {
	ctx = e.ctxWithTriggerOperatorOrDefault(ctx, trig)

	mode := strings.TrimSpace(trig.OnViolation)
	if mode == "" {
		mode = "notify"
	}

	var notify, createBead bool
	switch mode {
	case "notify":
		notify = true
	case "create_bead":
		createBead = true
	case "both":
		notify, createBead = true, true
	default:
		return fmt.Errorf("trigger %s: invalid on_violation %q (want notify|create_bead|both)", trig.ID, mode)
	}

	if notify {
		if err := e.actionNotify(ctx, trig, m); err != nil {
			return fmt.Errorf("assert notify: %w", err)
		}
	}
	if createBead {
		if err := e.actionCreateBead(ctx, trig, m); err != nil {
			return fmt.Errorf("assert create_bead: %w", err)
		}
	}

	e.logger.Info("triggers: assertion violated, alert fired",
		"trigger", trig.ID, "match", m.Key, "on_violation", mode,
		"violated", m.TemplateVars["{{violated}}"])
	return nil
}

// actionApplyFormula instantiates the trigger's formula directly as a
// molecule + step beads (the same outcome as `kd formula apply`), stamping
// `triggered-by` lineage from the molecule to both the trigger bead and the
// source bead. This replaces the legacy create-bead-then-fill flow that left
// a `pending_formula` field for a separate runner — that runner was never
// implemented, so the field became dead weight (D6 ka-0000006284, epic
// ka-0000007507). Trigger lineage continues to flow through the same
// `triggered-by` dep type (D1 ka-0000005967).
//
// After a successful pour it spawns a DRIVER agent targeting the molecule,
// mirroring the manual Slack `/formula pour` path (bot_formula.go,
// handleFormulaPour). Without this the trigger poured inert molecules on
// every fire: step beads with an open parent-child dep to their molecule are
// invisible to `gb ready` (NoOpenDeps), the orphan sweep only labels, and no
// runner existed — so pours accumulated unworked (ka-0000000070, epic
// ka-0000001622, the FLAT gardener stall). Returns the spawned agent bead ID
// so evalOne's post-fire bookkeeping (gardener counters, last_run) sees this
// action like any other spawn-producing fire.
//
// The driver spawn is deliberately non-fatal: the pour has already happened,
// and failing the fire here would leave the match unseen so the next cycle
// re-pours a DUPLICATE molecule (molecules carry no per-fire dedup key that
// CatchUpKeys could recover). An orphaned molecule with a loud error log
// beats molecule pile-up; the poured-but-unworked liveness check
// (ka-0000000957) is the backstop for spawn-failed pours.
func (e *TriggerEngine) actionApplyFormula(ctx context.Context, trig TriggerDef, m TriggerMatch) (string, error) {
	if strings.TrimSpace(trig.Project) == "" {
		// A project-less spawn writes an agent bead the controller's credential
		// guard then refuses permanently (spawn_credential_guard.go:70-77),
		// leaving an orphan. The daemon rejects such triggers at AUTHORING
		// (runaway_validation.go:275-306); this is the fire-time backstop for
		// triggers that predate that gate or were written around it.
		e.logger.Error("triggers: refusing to spawn — trigger has no project",
			"trigger", trig.ID, "action", trig.Action)
		return "", fmt.Errorf("trigger %s has no project — refusing to spawn a project-less agent", trig.ID)
	}

	ctx = e.ctxWithTriggerOperatorOrDefault(ctx, trig)

	if trig.FormulaID == "" {
		return "", fmt.Errorf("trigger %s: apply_formula requires formula_id", trig.ID)
	}

	formula, err := e.daemon.GetBead(ctx, trig.FormulaID)
	if err != nil {
		return "", fmt.Errorf("resolving formula %s: %w", trig.FormulaID, err)
	}

	ff := parseFormulaFields(formula)
	if len(ff.Steps) == 0 {
		return "", fmt.Errorf("formula %s has no steps", trig.FormulaID)
	}

	// Trigger-defined formula_vars is a JSON object string. Expand template
	// placeholders against the match's template vars first, then parse into
	// a map for use during step substitution.
	vars := make(map[string]string)
	rawVars := strings.TrimSpace(ExpandTemplate(trig.FormulaVars, m.TemplateVars))
	if rawVars != "" {
		if err := json.Unmarshal([]byte(rawVars), &vars); err != nil {
			return "", fmt.Errorf("parsing formula_vars %q: %w", rawVars, err)
		}
	}

	// Apply defaults and validate required vars declared on the formula.
	for _, vd := range ff.Vars {
		if _, ok := vars[vd.Name]; !ok {
			if vd.Default != "" {
				vars[vd.Name] = vd.Default
			} else if vd.Required {
				return "", fmt.Errorf("formula %s: required variable {{%s}} not provided", trig.FormulaID, vd.Name)
			}
		}
	}

	// Reject vars whose values still carry {{...}} placeholders after
	// expansion — the firing source never published that template var.
	// Without this guard the literal (e.g. "{{fire_time_iso}}") is baked
	// into the molecule's applied_vars and every step title, stranding a
	// batch of unclaimable template beads on every fire (ka-0000006473).
	if bad := findUnresolvedVars(vars); len(bad) > 0 {
		return "", fmt.Errorf("trigger %s: formula_vars contain unresolved template placeholders after expansion: %s (source %s publishes: %s)",
			trig.ID, strings.Join(bad, ", "), trig.Source, knownTemplateVarNames(m.TemplateVars))
	}

	steps := expandFormulaSteps(ff.Steps, vars)
	if len(steps) == 0 {
		return "", fmt.Errorf("formula %s: all steps filtered by conditions", trig.FormulaID)
	}

	// Human-operator gate BEFORE the pour (same gate actionSpawnAgent runs).
	// In enforce mode an unattributed trigger must not pour at all: refusing
	// only the driver spawn would strand an inert molecule on every fire —
	// exactly the failure mode this action's driver exists to prevent.
	driverName := formulaDriverAgentName(trig, formula, m.Key)
	op, err := gateSpawnOperator(e.requireHumanMode, e.logger,
		"trigger:"+trig.Source, driverName, beadsapi.OperatorFromContext(ctx))
	if err != nil {
		return "", err
	}

	// Cross-pour anchor: the trigger may declare an UPSTREAM molecule's emit step
	// (usually the very bead that fired this trigger, e.g. "{{bead_id}}"), so the
	// poured molecule's step 1 gets a `blocks` edge onto it and the dispatcher
	// carries the cross-molecule handoff (decision ka-0000007596). Template-expand
	// against the match vars so a bead-event trigger can reference the closing
	// emit step directly; reject a leftover placeholder, which would write an edge
	// to a bead ID that does not exist.
	crossPourUpstream := strings.TrimSpace(ExpandTemplate(trig.CrossPourUpstreamStep, m.TemplateVars))
	if crossPourUpstream != "" && unresolvedPlaceholderPattern.MatchString(crossPourUpstream) {
		return "", fmt.Errorf("trigger %s: cross_pour_upstream_step %q still holds an unresolved placeholder after expansion (source %s publishes: %s)",
			trig.ID, crossPourUpstream, trig.Source, knownTemplateVarNames(m.TemplateVars))
	}

	molID, stepCount, err := instantiateFormula(ctx, e.daemon, e.logger,
		formula, steps, vars, false, trig.Project, "trigger:"+trig.ID, crossPourUpstream)
	if err != nil {
		return "", fmt.Errorf("instantiating formula %s: %w", trig.FormulaID, err)
	}

	// Stamp `triggered-by` lineage from the molecule onto BOTH the trigger
	// definition bead AND the source bead surfaced in the match. Epic
	// ka-0000007507 D1/D4; same primitive used by actionSpawnAgent.
	e.writeTriggerLineage(ctx, molID, trig, m)

	e.logger.Info("triggers: formula instantiated",
		"trigger", trig.ID, "match", m.Key,
		"formula", trig.FormulaID, "molecule", molID, "steps", stepCount)

	// Spawn the driver crew for the freshly poured molecule. Same extraFields
	// contract as actionSpawnAgent: `spawned_by: trigger:<id>` is what
	// liveSpawnCount keys the max_concurrent live lease on, so a still-running
	// driver from a prior pour defers the next fire instead of stacking
	// molecules; trigger_id/trigger_title/source feed the agent card and
	// audit trail.
	// Driver role precedence: an explicit trigger.Role wins (the operator set it
	// deliberately for this trigger); otherwise fall to the role the formula
	// itself declares (formulaDriverRole), and only then to the "crew"
	// generalist. Before this, an unset trigger.Role hardcoded "crew" and
	// dropped the formula's declared role — the pour-time half of the §7→§8
	// role handoff (ka-0000005928, bug ka-0000001755).
	role := trig.Role
	if role == "" {
		role = formulaDriverRole(formula, steps)
	}
	prompt := ExpandTemplate(trig.Prompt, m.TemplateVars)
	extraFields := map[string]string{
		"trigger_id":    trig.ID,
		"trigger_title": trig.Title,
		"source":        trig.Source,
		"spawned_by":    "trigger:" + trig.ID,
		// The molecule is single-claim work: two near-simultaneous fires
		// (multi-entry cron + bead_event) must not each put a driver on the
		// same molecule. Consumed by the SpawnAgent single-claim guard, not
		// persisted (ka-0000004654).
		"single_claim_task": "1",
	}
	if IsHumanOperator(op) {
		extraFields["operated_by"] = op
	}
	if m.Key != "" {
		extraFields[triggerSourceFieldKey(trig.Source)] = m.Key
	}

	agentID, spawnErr := e.daemon.SpawnAgent(ctx, driverName, trig.Project, molID, role, prompt, extraFields)
	if spawnErr != nil {
		e.logger.Error("triggers: formula poured but driver spawn FAILED — molecule is unworked until rescued",
			"trigger", trig.ID, "match", m.Key, "formula", trig.FormulaID,
			"molecule", molID, "agent_name", driverName, "error", spawnErr)
		return "", nil
	}

	e.logger.Info("triggers: spawned formula driver",
		"trigger", trig.ID, "match", m.Key, "formula", trig.FormulaID,
		"molecule", molID, "agent", agentID, "agent_name", driverName, "role", role)
	return agentID, nil
}

// formulaDriverAgentName picks the agent name for an apply_formula driver.
// A formula may pin a well-known driver identity via its `assigned_agent`
// field (the same override the Slack pour path honors); otherwise fall back
// to the standard trigger-derived slug + random suffix.
func formulaDriverAgentName(trig TriggerDef, formula *beadsapi.BeadDetail, matchKey string) string {
	if formula.Fields != nil {
		if name := strings.TrimSpace(formula.Fields["assigned_agent"]); name != "" {
			return name
		}
	}
	return triggerAgentName(trig, matchKey)
}

// writeTriggerLineage stamps `triggered-by` deps from beadID onto the trigger
// definition bead and (if present in the match) the source bead. Failures are
// logged but do not fail the trigger fire — lineage is best-effort, the action
// itself has already succeeded.
func (e *TriggerEngine) writeTriggerLineage(ctx context.Context, beadID string, trig TriggerDef, m TriggerMatch) {
	const depType = "triggered-by"
	createdBy := "trigger:" + trig.ID

	if trig.ID != "" {
		if err := e.daemon.AddDependency(ctx, beadID, trig.ID, depType, createdBy); err != nil {
			e.logger.Warn("triggers: failed to add triggered-by dep to trigger bead",
				"bead", beadID, "trigger", trig.ID, "error", err)
		}
	}

	if sourceBeadID := m.Fields["source_bead_id"]; sourceBeadID != "" && sourceBeadID != trig.ID {
		if err := e.daemon.AddDependency(ctx, beadID, sourceBeadID, depType, createdBy); err != nil {
			e.logger.Warn("triggers: failed to add triggered-by dep to source bead",
				"bead", beadID, "source_bead", sourceBeadID, "error", err)
		}
	}
}

// createTaskBead creates a task bead from a trigger match.
func (e *TriggerEngine) createTaskBead(ctx context.Context, trig TriggerDef, m TriggerMatch) (string, error) {
	title := m.Title
	if m.Key != "" && !strings.HasPrefix(title, "[") {
		title = fmt.Sprintf("[%s] %s", m.Key, m.Title)
	}

	// Resolve and validate the bead type. Empty defaults to "task"; anything
	// outside the allowlist is rejected so a trigger cannot file an arbitrary
	// type. See ka-0000003075.
	beadType := trig.BeadType
	if beadType == "" {
		beadType = "task"
	}
	if !createBeadTypeAllowlist[beadType] {
		return "", fmt.Errorf("trigger %s: invalid bead_type %q (allowed: task, bug, feature, chore)", trig.ID, beadType)
	}

	labels := []string{
		"source:" + trig.Source,
		"trigger:" + trig.ID,
	}
	labels = append(labels, m.Labels...)
	labels = append(labels, trig.ExtraLabels...)
	if trig.Project != "" {
		labels = append(labels, "project:"+trig.Project)
	}

	fields := map[string]string{
		"trigger_id":   trig.ID,
		"trigger_name": trig.Title,
	}
	if trig.CreatedBy != "" {
		fields["trigger_created_by"] = trig.CreatedBy
	}
	if op := beadsapi.OperatorFromContext(ctx); op != "" {
		fields["operated_by"] = op
	}
	maps.Copy(fields, m.Fields)

	fieldsJSON, _ := json.Marshal(fields)

	req := beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      beadType,
		Labels:    labels,
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: "trigger:" + trig.ID,
	}
	if trig.BeadPriority != nil {
		req.Priority = *trig.BeadPriority
	}
	// A schedule run is named `<trigger title> HH:MM`, authored here from the
	// fireKey rather than guessed back out of the `[…] Schedule: …` title.
	if hhmm, ok := scheduleRunClock(trig, m); ok {
		req.ShortTitle = shorttitle.Clip(trig.Title, shorttitle.Max-len(hhmm)-1) + " " + hhmm
	}

	beadID, err := e.daemon.CreateBead(ctx, req)
	if err != nil {
		return "", err
	}

	// Wire the filed bead as a child of the configured parent epic, if any.
	// A dependency failure is logged but not fatal: the bead already exists,
	// and failing here would leave the seen-set / audit trail inconsistent.
	if trig.ParentBead != "" {
		if err := e.daemon.AddDependency(ctx, beadID, trig.ParentBead, "parent-child", "trigger:"+trig.ID); err != nil {
			e.logger.Warn("triggers: failed to add parent-child dep for filed bead",
				"trigger", trig.ID, "bead", beadID, "parent", trig.ParentBead, "error", err)
		}
	}

	return beadID, nil
}

// scheduleRunClock returns the HH:MM of a schedule-source match, whose Key is
// the fireKey `2006-01-02T15:04`. ok is false for any other source or key.
func scheduleRunClock(trig TriggerDef, m TriggerMatch) (string, bool) {
	if trig.Source != "schedule" {
		return "", false
	}
	if _, err := time.Parse("2006-01-02T15:04", m.Key); err != nil {
		return "", false
	}
	return m.Key[11:16], true
}
