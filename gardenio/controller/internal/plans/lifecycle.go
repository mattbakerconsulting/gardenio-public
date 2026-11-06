package plans

import (
	"context"
	"slices"

	"gardenio/controller/internal/beadsapi"
)

// Mark toggles the `plan` label on an epic: the one-click "mark as plan" /
// "unmark". Marking also binds the plan's durable gardener (ka-0000002863) so a
// promoted plan is watched like a browser-created one; the bind is best-effort
// and never fails the mark.
func (s *Service) Mark(ctx context.Context, planID string, a Actor) error {
	bead, err := s.planEpic(ctx, planID)
	if err != nil {
		return err
	}
	if slices.Contains(bead.Labels, Label) {
		if err := s.Daemon.RemoveLabel(ctx, planID, Label); err != nil {
			s.Logger.Error("plan mark: remove label", "plan", planID, "error", err)
			return fail(KindUpstream, "Failed to update plan label", err)
		}
		return nil
	}
	if err := s.Daemon.AddLabel(ctx, planID, Label); err != nil {
		s.Logger.Error("plan mark: add label", "plan", planID, "error", err)
		return fail(KindUpstream, "Failed to update plan label", err)
	}
	// Re-read so the bind sees the fresh label and any plan_state (which decides
	// born enabled or paused). The operator stamp is what keeps the kbeads audit
	// resolver from failing the bind's CreateBead closed.
	octx := operatorCtx(ctx, a)
	if marked, err := s.Daemon.GetBead(octx, planID); err != nil {
		s.Logger.Warn("plan mark: re-read for gardener bind", "plan", planID, "error", err)
	} else {
		s.Gardeners.BindGardener(octx, marked, a.Email)
	}
	return nil
}

// SetState moves the plan between draft and ready: "Mark ready" / "Keep
// editing", finalizing or reopening a plan without building it. requested is
// StateReady or StateDraft; anything else toggles. A building plan is refused
// (it would strand an enabled gardener on a non-building plan; the operator
// pauses the gardener to step back). Asking for the current state is a no-op.
func (s *Service) SetState(ctx context.Context, planID string, requested State, a Actor) error {
	epic, err := s.planEpicStrict(ctx, planID)
	if err != nil {
		return err
	}
	cur := StateOf(epic)
	next := requested
	if next != StateReady && next != StateDraft {
		next = StateReady
		if cur == StateReady {
			next = StateDraft
		}
	}
	if cur == StateBuilding {
		return fail(KindRefused, "This plan is building — pause the gardener to step back to editing.", nil)
	}
	if next == cur {
		return nil
	}
	if err := s.setState(ctx, planID, next, a.Email); err != nil {
		return fail(KindUpstream, "Failed to update the plan state.", err)
	}
	return nil
}

// Build is the single deliberate "Approve & build" (§6.4): it flips a ready
// plan to building and enables the bound gardener in one gesture. Already
// building is a no-op.
//
// The state write goes first, then the enable. If the enable fails the plan is
// left building with a paused gardener, which one Resume click recovers; the
// inverse (an enabled gardener on a still-ready plan) would defeat the gate.
func (s *Service) Build(ctx context.Context, planID string, a Actor) error {
	epic, err := s.planEpicStrict(ctx, planID)
	if err != nil {
		return err
	}
	switch StateOf(epic) {
	case StateBuilding:
		return nil
	case StateReady:
	default:
		return fail(KindRefused, "Mark the plan ready before building — it still has open questions.", nil)
	}
	gardener := s.Gardeners.PlanGardener(ctx, planID)
	if gardener == nil {
		return fail(KindConflict, "No gardener is bound to this plan — bind one with `gb plan gardener "+planID+"` before building.", nil)
	}
	if err := s.setState(ctx, planID, StateBuilding, a.Email); err != nil {
		return fail(KindUpstream, "Failed to approve the plan for build.", err)
	}
	if err := s.Gardeners.SetGardenerEnabled(ctx, gardener, true, a.Email); err != nil {
		return fail(KindUpstream, "Plan approved, but starting the gardener failed: "+err.Error()+" — click Resume to start it.", err)
	}
	return nil
}

// Complete is the terminal "Mark complete" (ka-0000000616). A plan already
// complete and closed is a no-op. It does not block on open steps: the UI
// confirms those, and an operator can always retire a plan.
func (s *Service) Complete(ctx context.Context, planID string, a Actor) error {
	epic, err := s.planEpicStrict(ctx, planID)
	if err != nil {
		return err
	}
	if StateOf(epic) == StateComplete && !isOpen(epic.Status) {
		return nil
	}
	return s.CompleteEpic(ctx, epic, a)
}

// CompleteEpic retires an already-loaded plan epic: disable the bound
// gardener, stamp plan_state=complete, close the epic. The single-plan lever
// and the bulk verb share it so the two cannot drift on ordering or
// attribution. It is the same composition as `gb plan complete`
// (cmd/gb/plan_write.go completePlan).
func (s *Service) CompleteEpic(ctx context.Context, epic *beadsapi.BeadDetail, a Actor) error {
	return s.retire(ctx, epic, StateComplete, a,
		"Failed to mark the plan complete.",
		"Marked complete, but closing the plan failed — try again.")
}

// Abandon is the terminal "Abandon", the cheap exit for a plan dropped without
// finishing (ka-0000001377). It is Complete with plan_state=abandoned, so the
// board renders the plan as given up on rather than done. It does not gate on
// open steps: giving up on the leftover work is the point.
func (s *Service) Abandon(ctx context.Context, planID string, a Actor) error {
	epic, err := s.planEpicStrict(ctx, planID)
	if err != nil {
		return err
	}
	if StateOf(epic) == StateAbandoned && !isOpen(epic.Status) {
		return nil
	}
	return s.AbandonEpic(ctx, epic, a)
}

// AbandonEpic is CompleteEpic's twin for plan_state=abandoned, shared with the
// bulk verb. The same composition as `gb plan abandon`.
func (s *Service) AbandonEpic(ctx context.Context, epic *beadsapi.BeadDetail, a Actor) error {
	return s.retire(ctx, epic, StateAbandoned, a,
		"Failed to mark the plan abandoned.",
		"Marked abandoned, but closing the plan failed — try again.")
}

// retire disables the bound gardener, stamps the terminal state, then closes
// the epic. The order is load-bearing: disabling first means that even if a
// later write fails the loop is already quiet (a closed epic with a running
// gardener would keep waking a gardener that no-ops).
func (s *Service) retire(ctx context.Context, epic *beadsapi.BeadDetail, terminal State, a Actor, stampMsg, closeMsg string) error {
	id := epic.ID
	verb := "plan complete"
	if terminal == StateAbandoned {
		verb = "plan abandon"
	}
	// Best-effort: a plan may have no gardener, and the loop's cron pre-flight
	// goes dormant on a closed epic anyway.
	if gardener := s.Gardeners.PlanGardener(ctx, id); gardener != nil {
		if err := s.Gardeners.SetGardenerEnabled(ctx, gardener, false, a.Email); err != nil {
			s.Logger.Warn(verb+": disable gardener", "plan", id, "gardener", gardener.ID, "actor", a.Email, "error", err.Error())
		}
	}
	// The stamp precedes the close so a board reader sees the terminal state.
	if err := s.setState(ctx, id, terminal, a.Email); err != nil {
		return fail(KindUpstream, stampMsg, err)
	}
	if err := s.Daemon.CloseBeadAs(ctx, id, a.Operator, nil); err != nil {
		s.Logger.Error(verb+": close epic", "plan", id, "actor", a.Email, "error", err)
		return fail(KindUpstream, closeMsg, err)
	}
	return nil
}

// Reopen is the inverse of Complete and Abandon: status=open and
// plan_state=building. Building, not ready, because a terminal plan was past
// the approve gate already. The gardener stays paused: reopening must not
// restart the loop behind the operator's back (R6). A plan that is open and
// not terminal is a no-op.
func (s *Service) Reopen(ctx context.Context, planID string, a Actor) error {
	epic, err := s.planEpicStrict(ctx, planID)
	if err != nil {
		return err
	}
	if st := StateOf(epic); isOpen(epic.Status) && st != StateComplete && st != StateAbandoned {
		return nil
	}
	open := "open"
	if err := s.Daemon.UpdateBead(ctx, planID, beadsapi.UpdateBeadRequest{Status: &open}); err != nil {
		s.Logger.Error("plan reopen: reopen epic", "plan", planID, "actor", a.Email, "error", err)
		return fail(KindUpstream, "Failed to reopen the plan.", err)
	}
	if err := s.setState(ctx, planID, StateBuilding, a.Email); err != nil {
		return fail(KindUpstream, "Plan reopened, but restoring its build state failed — set it via the gardener bar.", err)
	}
	return nil
}

// setState writes plan_state and a best-effort audit comment. Only the field
// write can fail it.
func (s *Service) setState(ctx context.Context, planID string, next State, email string) error {
	if err := s.Daemon.UpdateBeadFieldsTyped(ctx, planID, map[string]any{StateField: string(next)}); err != nil {
		s.Logger.Error("plan lifecycle: set plan_state", "plan", planID, "next", next, "actor", email, "error", err)
		return err
	}
	if err := s.Daemon.AddComment(ctx, planID, email, "plan_state → "+string(next)+" by "+email); err != nil {
		s.Logger.Warn("plan lifecycle: state audit comment", "plan", planID, "actor", email, "error", err)
	}
	return nil
}
