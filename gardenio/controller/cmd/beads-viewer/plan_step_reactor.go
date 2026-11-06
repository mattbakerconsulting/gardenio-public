package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// plan_step_reactor.go — the SINGLE reactive-collaboration seam (epic
// ka-0000002170, grandparent ka-0000000187; born as the interim seam
// ka-0000007841 under P2 ka-0000005932).
//
// WHY THIS EXISTS. The SHIPPED reactive-collaboration contract (epic
// ka-0000004813) is: adding a step to a plan nudges the plan's live agent
// sub-second, wakes a parked one, or spawns a fresh one — behind a per-plan
// debounce. That contract was originally driven ONLY from the browser's
// operator-gated "Add step" POST (handlePlanAddStep -> promptPlanAssistant).
// When P2 (ka-0000004615) moved `gb plan add-step` OFF that viewer POST and onto
// the daemon-direct write composition (cmd/gb/plan_write.go), the CLI/gardener
// add-step stopped firing the nudge. This reactor restored it; then P3
// (ka-0000002533) DELETED the browser handler's inline promptPlanAssistant so
// this reactor is now the SOLE reactive path for BOTH surfaces — killing the
// fork/double-spawn risk (decision ka-0000007006) by construction.
//
// THE SEAM. Rather than re-plumb a daemon->viewer signal, the viewer OBSERVES
// the new step edge: the daemon already publishes beads.dependency.added on
// every AddDependency (server.go / http_relations.go recordAndPublish), and a
// plan step is exactly a parent-child edge whose parent is the plan epic (a
// flat step) or one of the plan's phase sub-epics (a phased step). So this
// reactor holds ONE long-lived upstream EventStream("beads.dependency.added")
// — mirroring BeadFeedHub's reconnecting consumer — and, for each new step edge
// landing under a plan that ALREADY has a bound agent, fires
// s.promptPlanAssistant behind the s.shouldNudgePlanAssistant per-plan debounce.
//
// NUDGE/WAKE ONLY — NOT COLD-SPAWN. This reactor NEVER cold-spawns a fresh
// gardener from a bare edge. It observes EVERY dependency-added event in the
// fleet (seed fixtures, `kd dep add`, formula pours, reparents), none of which
// carry an explicit add-step intent, so cold-spawning would mint a spurious
// plan-gardener pod on any parent-child edge under a plan (it broke the e2e
// "a fresh plan has no agent" precondition). So the seam only keeps the NUDGE
// firing for a plan an operator is already steering; a plan with no bound agent
// is left untouched. Full cold-spawn parity for an agent-less plan remains the
// epic's tracked exit — it needs a signal that distinguishes a genuine add-step
// intent from a passive edge (the daemon's first-class beads.plan.step.added,
// server-side-resolved + fail-closed + open-plan-only), which this reactor does
// not yet consume; do NOT enable cold-spawn off the noisy dependency stream.
//
// EXACTLY-ONE PATH. Since ka-0000002533 the browser handler no longer nudges
// inline — it only mints + links the step, and its AddDependency emits the edge
// this reactor observes. So a browser add and a daemon-direct CLI add BOTH reach
// collaboration solely here, through one debounce, one decision engine. The
// DURABLE per-plan debounce (fields.last_nudge_at on the plan epic, via
// shouldNudgePlanAssistant, ka-0000002855) coalesces a burst of adds to one
// nudge — and, because the window lives on the epic rather than in a volatile
// per-process map, the dedup holds across a viewer restart and across writer
// processes too.
//
// STILL VIEWER-COUPLED, BY DESIGN. This couples the reactive seam to a running
// VIEWER process: if the viewer is down, a CLI add-step lands the step but fires
// no nudge until the next add within a live viewer. The epic's end-state is a
// single CONTROLLER-side reactor that owns nudge AND spawn for both surfaces off
// the first-class signal and removes the viewer-observation coupling; the engine
// was already lifted into internal/planagent (ka-0000002739) so that relocation
// is a clean lift. This file is deliberately thin.

// planStepReactorTopic is the single daemon SSE topic the reactor consumes.
// beads.dependency.added fires once per AddDependency; the reactor filters to
// the parent-child edges that are plan steps in-process.
const planStepReactorTopic = "beads.dependency.added"

// planStepReactorReconnectWait mirrors beadFeedReconnectWait: how long the
// upstream loop waits before re-opening the stream after it closes (daemon
// restart, blip). A missed edge during the gap only means a missed nudge — the
// step still landed, and the agent sees it on its next turn — so no backfill is
// needed, matching the best-effort contract of the browser nudge it replaces.
const planStepReactorReconnectWait = beadFeedReconnectWait

// planReactor is the slice of *Server the reactor drives. It is satisfied by
// *Server; kept as a narrow interface so the reactor is unit-testable against a
// stub that records nudge calls without a full server (and so the dependency
// direction reads reactor -> server, never the reverse).
type planReactor interface {
	// reactToStepEdge resolves whether beadID's new parent-child edge onto
	// parentID is a plan step and, if so, drives the debounced nudge/wake/spawn
	// attributed to `operator` (the identity the daemon stamped on the edge).
	// Best-effort: it swallows its own errors (logged) so a bad event never
	// wedges the upstream pump.
	reactToStepEdge(ctx context.Context, beadID, parentID, operator string)
}

// PlanStepReactor observes beads.dependency.added and drives the reactive
// plan-collaboration nudge for step edges added by ANY writer (browser or the
// daemon-direct CLI). One upstream stream, reconnecting like BeadFeedHub.
//
// A nil *PlanStepReactor is safe: Start/Close are no-ops, so a deployment with
// no daemon (nil client) simply runs without the interim seam.
type PlanStepReactor struct {
	daemon  beadAPI
	reactor planReactor
	logger  *slog.Logger

	cancel context.CancelFunc
	done   chan struct{}
}

// NewPlanStepReactor builds a reactor over the daemon client + the server that
// owns the nudge machinery. Returns nil when daemon or reactor is nil (nothing
// to observe / nothing to drive), which every method tolerates — mirroring
// NewBeadFeedHub's nil-when-unwired contract.
func NewPlanStepReactor(daemon beadAPI, reactor planReactor, logger *slog.Logger) *PlanStepReactor {
	if daemon == nil || reactor == nil {
		return nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &PlanStepReactor{
		daemon:  daemon,
		reactor: reactor,
		logger:  logger,
		done:    make(chan struct{}),
	}
}

// Start launches the upstream consumer loop in a background goroutine. Safe to
// call on a nil reactor (no-op).
func (p *PlanStepReactor) Start() {
	if p == nil {
		return
	}
	// Event-driven, no human behind the stream: an explicit system read.
	ctx, cancel := context.WithCancel(beadsapi.WithSystemRead(context.Background()))
	p.cancel = cancel
	go p.run(ctx)
}

// run is the upstream pump: (re)connect to the daemon SSE stream and dispatch
// every dependency-added event until the context is canceled. On stream close
// it waits planStepReactorReconnectWait and retries, so a daemon restart
// self-heals — identical to BeadFeedHub.run.
func (p *PlanStepReactor) run(ctx context.Context) {
	defer close(p.done)
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := p.daemon.EventStream(ctx, planStepReactorTopic)
		if err != nil {
			p.logger.Warn("plan step reactor: opening upstream stream — retrying", "error", err)
			if !sleepCtx(ctx, planStepReactorReconnectWait) {
				return
			}
			continue
		}
		p.logger.Info("plan step reactor: upstream stream open", "topic", planStepReactorTopic)
		p.pump(ctx, events)
		if ctx.Err() != nil {
			return
		}
		p.logger.Info("plan step reactor: upstream stream closed — reconnecting")
		if !sleepCtx(ctx, planStepReactorReconnectWait) {
			return
		}
	}
}

// pump drains one upstream stream, dispatching each event, until it closes or
// the context is canceled.
func (p *PlanStepReactor) pump(ctx context.Context, events <-chan beadsapi.SSEEvent) {
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			p.dispatch(ctx, evt)
		}
	}
}

// planDependencyAddedEvent is the minimal shape the reactor decodes from a
// beads.dependency.added SSE payload. Mirrors kbeads events.DependencyAdded
// (kept local so the viewer doesn't import the daemon's internal package, the
// same pattern agent_events.go uses for its event shapes). Only the fields the
// reactor filters on are decoded; a sibling field with an unexpected shape can
// never fail the whole decode.
type planDependencyAddedEvent struct {
	Dependency *planDependencyEdge `json:"dependency"`
}

type planDependencyEdge struct {
	BeadID      string `json:"bead_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	// CreatedBy / OperatedBy carry the identity the daemon stamped on the edge
	// (server.go AddDependency). The reactive nudge is attributed to this
	// operator so its audit shape matches the browser handler's (which stamps
	// the OIDC operator from the request). OperatedBy is the canonical
	// oidc:<email>/slack:U… form; CreatedBy is the fallback when it is absent.
	CreatedBy  string `json:"created_by"`
	OperatedBy string `json:"operated_by"`
}

// dispatch parses one SSE event and, for a new parent-child edge, hands the
// (child, parent) pair to the reactor. Non-parent-child edges (assigned,
// blocks, related, …) and malformed payloads are dropped silently — cheaply,
// before any daemon round-trip.
func (p *PlanStepReactor) dispatch(ctx context.Context, evt beadsapi.SSEEvent) {
	var payload planDependencyAddedEvent
	if err := json.Unmarshal(evt.Data, &payload); err != nil || payload.Dependency == nil {
		return
	}
	dep := payload.Dependency
	// A plan step is a parent-child edge. Skip every other edge kind cheaply —
	// this is the hottest filter (every AddDependency in the fleet lands here).
	if !isParentChildAlias(dep.Type) {
		return
	}
	beadID := strings.TrimSpace(dep.BeadID)
	parentID := strings.TrimSpace(dep.DependsOnID)
	if beadID == "" || parentID == "" {
		return
	}
	// Prefer the resolved operated_by identity; fall back to created_by. Empty
	// is tolerated downstream (reactToStepEdge canonicalizes / degrades).
	operator := strings.TrimSpace(dep.OperatedBy)
	if operator == "" {
		operator = strings.TrimSpace(dep.CreatedBy)
	}
	p.reactor.reactToStepEdge(ctx, beadID, parentID, operator)
}

// reactToStepEdge is the *Server half of the seam (satisfies planReactor).
// Given a NEW parent-child edge (beadID -> parentID), it resolves whether
// beadID is a plan STEP and, if so, NUDGES the plan's live agent (or WAKES its
// already-bound parked one) with a collaboration seed — attributed to the
// operator the daemon stamped on the edge. This is the SOLE reactive path for
// both browser and CLI adds since ka-0000002533 deleted the browser handler's
// inline nudge.
//
// NUDGE/WAKE ONLY — NEVER COLD-SPAWN. This reactor observes EVERY
// beads.dependency.added in the fleet — seed fixtures, `kd dep add`, formula
// pours, reparents — none of which carry an explicit add-step intent.
// Cold-spawning on a bare edge would mint a spurious plan-gardener pod every time
// any parent-child edge lands under a plan epic (and broke the e2e "a fresh plan
// has no agent" precondition). So the seam only keeps the NUDGE firing for a
// plan that ALREADY has an agent: if none is bound, it does nothing. Cold-spawn
// parity for an agent-less plan remains the epic's tracked exit — it needs a
// real add-step intent event (the daemon's first-class beads.plan.step.added)
// rather than this passive edge observation.
//
// A step is a non-epic child (a task) whose parent is either the plan epic (a
// flat step) or one of the plan's phase sub-epics (a phased step). An epic
// child is a phase being ADDED (or a nested sub-epic), which the browser's
// add-phase never nudged — so it is excluded here too.
//
// Best-effort throughout: every resolution miss or write failure is logged and
// swallowed so a malformed/unrelated event can never wedge the upstream pump.
// The s.shouldNudgePlanAssistant debounce coalesces a burst of adds to a plan
// (browser or CLI) into a single nudge.
func (s *Server) reactToStepEdge(ctx context.Context, beadID, parentID, operator string) {
	// Daemon-stream origin, no human bearer: act as the viewer's own machine
	// principal when configured (see Server.machineCtx). This ctx is used for
	// the whole reaction — NOT just the epic's last_nudge_at stamp — because
	// every daemon write below has nothing else to ride on: the viewer's static
	// token is downscoped to read-only and write-bearer enforcement fails a
	// bearer-less write closed before it is sent, so a wake issued on the plain
	// ctx would never leave the viewer and a step added to a parked plan would
	// silently not wake its gardener. What each write needs from the machine
	// role (gardenio-machine-plan-reactor = ownership of type=epic only):
	//   - last_nudge_at stamp on the plan epic: the epic belongs to a human, so
	//     it needs the typed epic ownership grant;
	//   - the parked gardener's WAKE (UpdateBead on the agent bead): needs no
	//     grant — type:agent is kind=config (exempt from the ownership gate) and
	//     has no min_role — and is pinned by kbeads'
	//     TestMachineGrants_PlanReactorWake_HTTP_ParkedAgent. Attributed to
	//     machine:<clientId>: kbeads ignores X-Operated-By for a machine caller.
	// The reactor never cold-spawns (see the NUDGE/WAKE ONLY gate below), so no
	// agent bead is ever created on this identity.
	ctx = s.machineCtx(ctx)
	parent, err := s.daemon.GetBead(ctx, parentID)
	if err != nil || parent == nil || parent.Type != "epic" {
		// Steps parent to epics only (the plan or a phase). Anything else — a
		// task under a non-plan epic's sibling, a blocks target, a fetch miss —
		// is not a plan step. Bail before the plan-membership walk.
		return
	}
	planEpic := s.resolvePlanForStepParent(ctx, parent)
	if planEpic == nil {
		// parent is neither a plan epic nor a phase of one — not a plan step.
		return
	}

	// Closed-plan guard (ka-0000000753): a closed plan is done — never re-drive
	// its gardener. This guard now lives solely on the reactive path (the browser
	// handler no longer does reactive work); the daemon also fail-closes the
	// first-class signal for a closed plan, so a closed-plan add can never wake a
	// gardener regardless of the eventual signal source.
	if statusOpenSortKey(planEpic.Status) != 0 {
		return
	}

	// NUDGE/WAKE-ONLY gate: only proceed if the plan ALREADY has a bound agent
	// (live or parked). No agent bound → do nothing; a passive edge observation
	// must never cold-spawn a gardener (see the method doc). This is also the
	// cheapest early-out for the overwhelmingly common case — a step edge under
	// a plan nobody is actively steering.
	assistant := s.planAssistant(ctx, planEpic.ID)
	if assistant == nil {
		return
	}

	// The step's own bead: its type excludes an add-PHASE edge (a phase CONTAINER
	// parented directly to the plan), and its title seeds the collaboration
	// message.
	step, err := s.daemon.GetBead(ctx, beadID)
	if err != nil || step == nil {
		return
	}
	if isPhaseContainer(step) {
		// A phase-container child under a plan (a type=epic sub-epic OR a
		// type=molecule poured into the plan) is a phase being ADDED, never a
		// step. Adding a phase never nudges, so neither do we.
		//
		// DECISION ka-0000002805 — a poured phase does not nudge. The steps INSIDE
		// a poured molecule phase are parented to the MOLECULE, so the parent.Type
		// == "epic" gate above (reactToStepEdge's first check) already excludes
		// them from the reactive nudge; and a molecule phase's steps are only ever
		// the formula-poured ones (the cockpit / gb plan add-step reject a non-epic
		// phase target, so no step is ever hand-added under a molecule). Emplacing
		// a pour is therefore a quiet, bulk phase-add — consistent with "+ New
		// phase"; the plan agent picks up the new phase on its next turn.
		return
	}
	if !isPlanStepType(step.Type) {
		// FREEZE/THAW ECHO GUARD (bug ka-0000001684). A plan step is a WORK ITEM —
		// task/bug/feature/chore (what `gb plan add-step` and the cockpit add-step
		// mint). A plan-gardener's OWN beads also auto-parent under its plan epic as
		// parent-child edges: kbeads maybeAddAutoParent (server/auto_parent.go)
		// links the gardener's type=session registration bead to its task_id (the
		// plan epic), and its minted type=message plan-transcript beads land the
		// same way — each publishing beads.dependency.added. Without this gate the
		// reactor consumed those non-work-item edges as "new steps", resolved the
		// plan, and WOKE the just-parked/frozen gardener (status=in_progress ->
		// AgentSpawn -> thaw) 2-5 min after `gb stop` — and the gardener's act of
		// waking mints the next session/message edge that re-wakes it, an endless
		// echo. Restricting the nudge to real work-item children breaks the loop.
		// Mirrors the kbeads daemon twin's isPlanStepType
		// (server/plan_step_event.go) so the two membership verdicts never diverge.
		return
	}
	if strings.TrimSpace(step.CreatedBy) != "" && step.CreatedBy == assistant.Title {
		// The bound gardener filed this step itself: it already knows, and a
		// nudge landing after its own `gb stop` would wake it for nothing
		// (gd-XCwA8MXX8m). The edge's operator cannot tell this apart, since a
		// gardener's writes carry the operator it acts for.
		return
	}

	// An operator is required to attribute the nudge/wake writes — the daemon
	// audit resolver fail-closes without one. A real browser or CLI add always
	// carries it (operated_by / created_by on the edge); skip defensively if
	// somehow absent rather than firing a write that 400s.
	operator = strings.TrimSpace(operator)
	if operator == "" {
		s.logger.Warn("plan step reactor: edge carries no operator — skipping nudge",
			"plan", planEpic.ID, "step", beadID)
		return
	}

	// Durable per-plan debounce (fields.last_nudge_at on the plan epic,
	// ka-0000002855): coalesces a burst of adds to this plan (from any writer —
	// browser or CLI) into a single nudge. Since ka-0000002533 this reactor is the
	// sole reactive path, so the window is persisted on the epic rather than in a
	// volatile in-memory map — the dedup holds across a viewer restart and across
	// writers.
	if !s.shouldNudgePlanAssistant(ctx, planEpic.ID) {
		return
	}

	// Stamp the operator on ctx so promptPlanAssistant's nudge/wake writes carry
	// X-Operated-By (the daemon audit resolver fail-closes without it).
	// canonicalOIDC tolerates an already-canonical (oidc:/slack:) id or a bare
	// email. Because an agent is bound (asserted above), promptPlanAssistant takes
	// the nudge path (live) or the wake path (parked — reuses the SAME bead id /
	// S3 session), never a fresh-mint cold spawn.
	nudgeCtx := beadsapi.WithOperator(ctx, canonicalOIDC(operator))
	seed := "I added a new step: " + step.Title + " — collaborate on it (bead " + beadID + ")."
	if _, _, _, err := s.promptPlanAssistant(nudgeCtx, planEpic, planReactorActor(operator), seed); err != nil {
		s.logger.Warn("plan step reactor: assistant collaboration", "plan", planEpic.ID, "step", beadID, "error", err)
	}
}

// resolvePlanForStepParent returns the PLAN epic a step's parent belongs to, or
// nil if the parent is neither a plan nor a phase of one. `parent` is the
// already-fetched parent epic of the new step edge:
//
//   - parent IS a plan epic (carries the `plan` label)  -> the plan itself.
//   - parent is a NON-plan epic -> maybe a phase sub-epic: walk its own
//     parent-child edge up to the grandparent and confirm the grandparent is a
//     plan epic that LISTS this parent in its phase_order (the same membership
//     rule the timeline + add-step phase target enforce, ka-0000001034). This
//     fail-closes: a stray epic-under-epic that isn't a real phase resolves to
//     nil, so an unrelated hierarchy never triggers a plan nudge.
func (s *Server) resolvePlanForStepParent(ctx context.Context, parent *beadsapi.BeadDetail) *beadsapi.BeadDetail {
	if parent == nil {
		return nil
	}
	if hasLabel(parent, planLabel) {
		return parent
	}
	// Phase case: find the parent epic's own parent-child parent (its plan).
	deps, err := s.daemon.GetDependencies(ctx, parent.ID)
	if err != nil {
		return nil
	}
	for _, d := range deps {
		if !isParentChildAlias(d.Type) || strings.TrimSpace(d.DependsOnID) == "" {
			continue
		}
		grandparent, err := s.daemon.GetBead(ctx, d.DependsOnID)
		if err != nil || grandparent == nil || grandparent.Type != "epic" {
			continue
		}
		// The grandparent must be a plan that actually LISTS this parent as one
		// of its phases — otherwise `parent` is just a nested sub-epic, not a
		// phase, and its task children are not plan steps.
		if hasLabel(grandparent, planLabel) && slices.Contains(planPhaseOrder(grandparent), parent.ID) {
			return grandparent
		}
	}
	return nil
}

// planStepTypes is the set of bead types that count as a plan STEP for the
// reactive nudge — the work-item types `gb plan add-step` and the cockpit
// add-step form actually mint (both create type=task; the others are accepted so
// a hand-linked bug/feature/chore step still nudges). Everything else — most
// importantly a plan-gardener's own auto-parented type=session registration bead
// and its minted type=message plan-transcript beads (bug ka-0000001684) — is NOT
// a step, so its edge never wakes the gardener. Kept in exact sync with the
// kbeads daemon twin isPlanStepType (server/plan_step_event.go) so the viewer's
// reactive verdict and the daemon's first-class beads.plan.step.added signal
// agree on membership.
var planStepTypes = map[string]struct{}{
	"task":    {},
	"bug":     {},
	"feature": {},
	"chore":   {},
}

// isPlanStepType reports whether a step bead's type is a work-item type that
// counts as a plan step. See planStepTypes.
func isPlanStepType(t string) bool {
	_, ok := planStepTypes[strings.TrimSpace(t)]
	return ok
}

// planReactorActor derives the bare-ish actor string the nudge is logged +
// attributed under (promptPlanAssistant's `actor`), mirroring the browser
// handler which passes the bare operator email. An oidc:<email> id yields the
// bare email; any other form (slack:U…, an already-bare email) passes through.
func planReactorActor(operator string) string {
	const oidcPrefix = "oidc:"
	if strings.HasPrefix(strings.ToLower(operator), oidcPrefix) {
		return strings.TrimSpace(operator[len(oidcPrefix):])
	}
	return operator
}

// Close cancels the upstream loop and waits for it to unwind. Safe on nil.
func (p *PlanStepReactor) Close() {
	if p == nil {
		return
	}
	if p.cancel != nil {
		p.cancel()
		<-p.done
	}
}
