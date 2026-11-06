// Producer half of P3 (task ka-0000004264, epic ka-0000002170): the daemon
// emits a first-class, DURABLE, DEDUPED plan-step-added signal.
//
// WHY A DISTINGUISHED EVENT. The reactive-collaboration contract (epic
// ka-0000004813) is: adding a step to a plan nudges the plan's live agent
// sub-second, wakes a parked one, or spawns a fresh one — behind a per-plan
// debounce. The interim seam (ka-0000007841) drove that off the generic
// beads.dependency.added event, forcing the consumer to re-walk the graph on
// EVERY parent-child edge in the fleet to decide plan membership. P3 replaces
// that with a purpose-built signal: the daemon resolves membership ONCE, here,
// at the write chokepoint, and emits a self-describing beads.plan.step.added
// carrying the plan id, phase id, and step metadata. The controller reactor
// (consumer, ka-0000002739) subscribes and debounces without a round-trip.
//
// WHY DURABLE + DEDUPED. A nudge that is lost because the reactor was mid-restart
// is a step that never gets collaborated on; a nudge that fires twice for one
// add is a double-spawn (the exact fork risk ka-0000007006 guards against). So
// the signal is published to the durable JetStream PLAN_EVENTS stream with a
// deterministic Nats-Msg-Id per (plan, step) edge: at-least-once delivery
// (replayed on reconnect) WITHOUT at-least-once side effects (the stream's
// Duplicates window collapses a re-emit of the same id). The core-NATS Publish
// remains the fire-and-forget live fast-path.
//
// FAIL-CLOSED MEMBERSHIP. The event fires ONLY when the new parent-child edge is
// a genuine plan step: the parent is a plan epic (carries the `plan` label), or a
// phase sub-epic listed in a plan's phase_order (the membership rule
// ka-0000001034 the viewer's resolvePlanForStepParent enforces). A stray
// epic-under-epic, an add-PHASE edge (an epic child), or a non-parent-child edge
// never produces one. All resolution is best-effort: a lookup miss logs + skips,
// never blocking the underlying dependency write.

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/eventbus"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// planMarkerLabel promotes an ordinary epic into a /plans plan. Mirrors the
// viewer's planLabel (cmd/beads-viewer/plans.go) and the create paths that stamp
// it; kept in sync so the daemon's membership check matches what the cockpit
// renders as a plan.
const planMarkerLabel = "plan"

// planStepTypes is the set of bead types that count as a plan STEP for the
// first-class beads.plan.step.added signal — the work-item types `gb plan
// add-step` and the cockpit add-step form actually mint (both create type=task;
// the others are accepted so a hand-linked bug/feature/chore step still emits).
// Everything else is excluded: a type=epic/molecule phase container (a phase
// being added, not a step) AND a plan-gardener's own auto-parented type=session
// registration bead / minted type=message plan-transcript beads (bug
// ka-0000001684), whose edges would otherwise perpetuate the freeze/thaw echo.
// Kept in exact sync with the viewer reactor twin's planStepTypes
// (cmd/beads-viewer/plan_step_reactor.go) so the daemon's server-side membership
// verdict and the viewer's reactive verdict never diverge.
var planStepTypes = map[model.BeadType]struct{}{
	model.TypeTask:    {},
	model.TypeBug:     {},
	model.TypeFeature: {},
	model.TypeChore:   {},
}

// isPlanStepType reports whether a step bead's type is a work-item type that
// counts as a plan step. See planStepTypes.
func isPlanStepType(t model.BeadType) bool {
	_, ok := planStepTypes[t]
	return ok
}

// maybeEmitPlanStepAdded resolves whether a freshly-added parent-child edge is a
// plan step and, if so, emits the first-class beads.plan.step.added signal (core
// NATS live fast-path + durable deduped JetStream copy). Called from the
// dependency-add chokepoint AFTER the edge is persisted, so the step + its
// parent are already readable.
//
// `dep` is the just-written edge (dep.BeadID=child/step, dep.DependsOnID=parent).
// `operator` is the resolved operator identity stamped on the edge; the consumer
// attributes its nudge/wake writes to it.
//
// Best-effort: every miss logs at debug and returns without emitting. It never
// returns an error — a plan-signal failure must never fail the dependency write.
func (s *BeadsServer) maybeEmitPlanStepAdded(ctx context.Context, dep *model.Dependency, operator string) {
	if s == nil || dep == nil || s.store == nil {
		return
	}
	// Only parent-child edges are steps. Skip every other edge kind (blocks,
	// related, assigned, triggered-by, …) cheaply, before any store round-trip —
	// this is the hot filter: it runs on every AddDependency in the fleet.
	if !model.IsParentChildDepType(string(dep.Type)) {
		return
	}
	stepID := strings.TrimSpace(dep.BeadID)
	parentID := strings.TrimSpace(dep.DependsOnID)
	if stepID == "" || parentID == "" {
		return
	}

	step, err := s.store.GetBead(ctx, stepID)
	if err != nil || step == nil {
		slog.Debug("plan step event: fetch step", "step", stepID, "parent", parentID, "err", errStr(err))
		return
	}
	// A plan step is a WORK ITEM — task/bug/feature/chore (what `gb plan add-step`
	// and the cockpit add-step mint; both create type=task). Every other child
	// type that can land a parent-child edge under a plan epic is NOT a step and
	// must not emit the signal:
	//
	//   - A phase-CONTAINER child (a type=epic sub-epic / add-phase / nested
	//     sub-epic, OR a type=molecule poured into the plan) is a PHASE being
	//     added, never a step. DECISION ka-0000002805 — a poured phase does not
	//     nudge; and the steps INSIDE a poured molecule parent to the MOLECULE,
	//     which resolvePlanForStepParent's type=epic parent gate already excludes.
	//   - A plan-gardener's OWN beads (bug ka-0000001684): maybeAddAutoParent
	//     (server/auto_parent.go) auto-parents the gardener's type=session
	//     registration bead to its task_id = the plan epic, and its minted
	//     type=message plan-transcript beads land the same edge. Emitting the
	//     first-class signal for those would perpetuate the freeze/thaw echo that
	//     re-wakes a just-parked gardener 2-5 min after `gb stop` — the exact
	//     re-wake the viewer reactor twin now also guards.
	//
	// Restricting to the work-item allowlist excludes all of the above in one
	// check, and keeps this server-side verdict in lockstep with the viewer
	// reactor's isPlanStepType (cmd/beads-viewer/plan_step_reactor.go) so the two
	// membership verdicts never diverge.
	if !isPlanStepType(step.Type) {
		return
	}

	planEpic, phaseID := s.resolvePlanForStepParent(ctx, parentID)
	if planEpic == nil {
		// parent is neither a plan epic nor a phase of one — not a plan step.
		return
	}
	// Closed-plan guard (ka-0000000753): a closed plan is done — never re-drive
	// its collaboration. Emitting for it would wake a gardener on a finished plan.
	if planEpic.Status == model.StatusClosed {
		return
	}

	evt := events.PlanStepAdded{
		PlanID:    planEpic.ID,
		PhaseID:   phaseID,
		StepID:    step.ID,
		StepTitle: step.Title,
		StepType:  string(step.Type),
		Operator:  operator,
		Fleet:     FleetFromContext(ctx),
	}
	s.emitPlanStepAdded(ctx, evt)
}

// resolvePlanForStepParent returns the PLAN epic a step's parent belongs to plus
// the phase id the step sits under (empty for a flat step parented directly to
// the plan), or (nil, "") if the parent is neither a plan nor a phase of one.
//
//   - parent IS a plan epic (carries the `plan` label)  -> (plan, "").
//   - parent is a NON-plan epic -> maybe a phase sub-epic: walk its own
//     parent-child edge up to the grandparent and confirm the grandparent is a
//     plan epic that LISTS this parent in its phase_order (ka-0000001034). This
//     fail-closes: a stray epic-under-epic that isn't a real phase resolves to
//     nil, so an unrelated hierarchy never triggers a plan step signal.
//
// Mirrors the viewer's resolvePlanForStepParent (plan_step_reactor.go) so the
// daemon's server-side membership verdict matches what the cockpit renders.
func (s *BeadsServer) resolvePlanForStepParent(ctx context.Context, parentID string) (*model.Bead, string) {
	parent, err := s.store.GetBead(ctx, parentID)
	if err != nil || parent == nil {
		slog.Debug("plan step event: fetch parent", "parent", parentID, "err", errStr(err))
		return nil, ""
	}
	// Steps parent to epics only (the plan or a phase). A task/bug/feature parent
	// is never a plan step's parent.
	if parent.Type != model.TypeEpic {
		return nil, ""
	}
	if hasLabel(parent.Labels, planMarkerLabel) {
		// Flat step: parented directly to the plan epic. No phase.
		return parent, ""
	}
	// Phase case: the parent epic may be a phase sub-epic. Walk up to its own
	// parent (the plan) and confirm membership via phase_order.
	for _, d := range parent.Dependencies {
		if d == nil || !model.IsParentChildDepType(string(d.Type)) {
			continue
		}
		grandparentID := strings.TrimSpace(d.DependsOnID)
		if grandparentID == "" {
			continue
		}
		grandparent, err := s.store.GetBead(ctx, grandparentID)
		if err != nil || grandparent == nil || grandparent.Type != model.TypeEpic {
			continue
		}
		// The grandparent must be a plan that actually LISTS this parent as one
		// of its phases — otherwise `parent` is just a nested sub-epic, not a
		// phase, and its task children are not plan steps.
		if hasLabel(grandparent.Labels, planMarkerLabel) &&
			planListsPhase(grandparent, parent.ID) {
			return grandparent, parent.ID
		}
	}
	return nil, ""
}

// planPhaseOrder decodes a plan epic's phase_order field — the ordered list of
// its phase-container ids (the membership + display order, ka-0000001034). A
// malformed / absent field reads as no phases (empty slice), so a caller can
// never treat a garbage field as if it listed a phase. Mirrors the viewer's
// planPhaseOrder decode.
func planPhaseOrder(plan *model.Bead) []string {
	if plan == nil || len(plan.Fields) == 0 {
		return nil
	}
	var fields struct {
		PhaseOrder []string `json:"phase_order"`
	}
	if err := json.Unmarshal(plan.Fields, &fields); err != nil {
		return nil
	}
	return fields.PhaseOrder
}

// planListsPhase reports whether the plan epic's phase_order field lists phaseID.
// A malformed / absent phase_order reads as "no phases", so a phase can never be
// spuriously accepted. Mirrors the viewer's planPhaseOrder + slices.Contains
// check.
func planListsPhase(plan *model.Bead, phaseID string) bool {
	for _, id := range planPhaseOrder(plan) {
		if strings.TrimSpace(id) == phaseID {
			return true
		}
	}
	return false
}

// emitPlanStepAdded publishes the resolved step-added event on both paths: the
// core-NATS live fast-path (beads.plan.step.added, fire-and-forget, for any live
// SSE-style subscriber) and the durable deduped JetStream copy (subject
// plan.step.added on PLAN_EVENTS). The JetStream copy carries a deterministic
// Nats-Msg-Id per (plan, step) so a re-emit of the same edge is collapsed.
func (s *BeadsServer) emitPlanStepAdded(ctx context.Context, evt events.PlanStepAdded) {
	// Live fast-path: fire-and-forget core NATS, mirroring how dependency /
	// lifecycle events are published. Best-effort — a publish failure is logged
	// and swallowed.
	if s.publisher != nil {
		if err := s.publisher.Publish(ctx, events.TopicPlanStepAdded, evt); err != nil {
			slog.Warn("plan step event: core-nats publish", "plan", evt.PlanID, "step", evt.StepID, "err", err.Error())
		}
	}

	// Durable deduped copy: only when the JetStream bus is wired. The message id
	// is deterministic in (plan, step) so a double-write / reconnect replay /
	// ON CONFLICT DO NOTHING re-insert lands the SAME id and the stream's
	// Duplicates window drops the second copy — at-least-once without a
	// double-nudge.
	if s.bus != nil {
		data, err := json.Marshal(evt)
		if err != nil {
			slog.Warn("plan step event: marshal for jetstream", "plan", evt.PlanID, "step", evt.StepID, "err", err.Error())
			return
		}
		s.bus.PublishDeduped(planStepAddedSubject, planStepDedupeID(evt.PlanID, evt.StepID), data)
	}
}

// planStepAddedSubject is the JetStream subject the durable step-added copy is
// published to. It lives on the PLAN_EVENTS stream (subject plan.>).
const planStepAddedSubject = eventbus.SubjectPlanPrefix + "step.added"

// planStepDedupeID derives the deterministic Nats-Msg-Id for a step-added event.
// Keyed on (plan, step): the same step linked under the same plan is the same
// logical add, so a re-emit dedupes; two different steps (or the same step under
// two plans, which cannot happen for a single parent-child edge) do not collide.
func planStepDedupeID(planID, stepID string) string {
	return "plan-step-added:" + planID + ":" + stepID
}

// errStr renders an error for structured logging without a nil-deref: a nil
// error becomes the empty string.
func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
