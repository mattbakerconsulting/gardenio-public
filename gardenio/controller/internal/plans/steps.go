package plans

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
)

// AddStep mints a new task under the plan epic, or under one of its phases
// when phaseID is set, and returns the new step's id. It is the same
// CreateBead+AddDependency compose `gb plan add-step` performs daemon-direct
// (cmd/gb/plan_write.go addPlanStep).
//
// The parent-child edge is also the single reactive-collaboration trigger: the
// daemon publishes beads.dependency.added on it and the one PlanStepReactor
// fires the debounced nudge/wake. Do not prompt the assistant from here; a
// second seam re-introduces the double-spawn race the single reactor exists to
// remove (epic ka-0000002170).
func (s *Service) AddStep(ctx context.Context, planID, title, phaseID string, a Actor) (string, error) {
	ctx = operatorCtx(ctx, a)
	epic, err := s.planEpic(ctx, planID)
	if err != nil {
		return "", err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fail(KindInvalid, "Step title is required.", nil)
	}
	if len(title) > MaxStepTitleLen {
		return "", fail(KindTooLarge, "Step title is too long.", nil)
	}

	// An optional phase target (ka-0000002361) parents the step to that phase
	// sub-epic so it renders inline under the phase. The phase must be a current
	// phase of this plan, else a stray id could adopt the step under an
	// unrelated epic. A blank phase keeps the flat-parent behavior.
	parentID := planID
	parentLabels := epic.Labels
	if phaseID = strings.TrimSpace(phaseID); phaseID != "" {
		phaseEpic, perr := s.resolvePhaseEpic(ctx, epic, phaseID)
		if perr != nil {
			return "", fail(KindInvalid, perr.Error(), nil)
		}
		parentID = phaseEpic.ID
		parentLabels = phaseEpic.Labels
	}

	req := beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "task",
		CreatedBy: a.Email,
	}
	if project := projectFromLabels(parentLabels); project != "" {
		req.Labels = []string{"project:" + project}
	}
	stepID, err := s.Daemon.CreateBead(ctx, req)
	if err != nil {
		s.Logger.Error("plan add step: create", "plan", planID, "actor", a.Email, "error", err)
		return "", fail(KindUpstream, "Failed to add step.", err)
	}
	if err := s.Daemon.AddDependency(ctx, stepID, parentID, "parent-child", a.Email); err != nil {
		s.Logger.Error("plan add step: link parent", "plan", planID, "parent", parentID, "step", stepID, "error", err)
		// The step exists but isn't linked: surface it, don't silently orphan.
		return stepID, fail(KindUpstream, "Step created but could not be linked to the plan.", err)
	}
	return stepID, nil
}

// resolvePhaseEpic verifies phaseID is a current phase of the plan: a
// direct-child epic listed in fields.phase_order (the membership rule the
// timeline renders by, ka-0000001034). It fails closed.
func (s *Service) resolvePhaseEpic(ctx context.Context, epic *beadsapi.BeadDetail, phaseID string) (*beadsapi.BeadDetail, error) {
	if !slices.Contains(PhaseOrder(epic), phaseID) {
		return nil, fmt.Errorf("phase %s is not a phase of this plan (not in phase_order)", phaseID)
	}
	phaseEpic, err := s.Daemon.GetBead(ctx, phaseID)
	if err != nil || phaseEpic == nil || phaseEpic.Type != "epic" {
		return nil, fmt.Errorf("phase %s is not a valid phase epic", phaseID)
	}
	return phaseEpic, nil
}

// CloseStep closes one step. A plain close on the step bead: the daemon's
// auto-close cascade (which skips plan epics, flagging them autoclose-ready)
// is the invariant, and it lives in the daemon.
func (s *Service) CloseStep(ctx context.Context, planID, stepID string, a Actor) error {
	if err := s.Daemon.CloseBeadAs(ctx, stepID, a.Email, nil); err != nil {
		s.Logger.Error("plan close step: close", "plan", planID, "step", stepID, "actor", a.Email, "error", err)
		return fail(KindUpstream, "Failed to close step.", err)
	}
	return nil
}

// ReopenStep flips a closed step back to open.
func (s *Service) ReopenStep(ctx context.Context, planID, stepID string, a Actor) error {
	open := "open"
	if err := s.Daemon.UpdateBead(ctx, stepID, beadsapi.UpdateBeadRequest{Status: &open}); err != nil {
		s.Logger.Error("plan reopen step", "plan", planID, "step", stepID, "actor", a.Email, "error", err)
		return fail(KindUpstream, "Failed to reopen step.", err)
	}
	return nil
}

// Dispatch is what happened to a step handed to the plan's gardener.
type Dispatch struct {
	// Outcome is the delivery fate: nudged live, queued on a waking gardener,
	// or a cold spawn.
	Outcome planagent.DeliveryOutcome
	// Gardener is the gardener's name, when known.
	Gardener string
}

// DispatchStep hands one open step to the plan's gardener (thread
// ka-0000002484): a targeted instruction to dispatch crew onto it, spawning the
// gardener first if none is bound. It goes through the same nudge / wake /
// spawn primitive as the plan-agent message box and the PlanStepReactor, so a
// dispatch never mints a second, unwatched agent.
func (s *Service) DispatchStep(ctx context.Context, planID, stepID string, a Actor) (Dispatch, error) {
	ctx = operatorCtx(ctx, a)
	epic, err := s.planEpic(ctx, planID)
	if err != nil {
		return Dispatch{}, err
	}
	// Resolve the step so the instruction carries its title and so a closed step
	// is refused: dispatching a done step would only confuse the gardener.
	step, err := s.Daemon.GetBead(ctx, stepID)
	if err != nil || step == nil {
		return Dispatch{}, fail(KindUpstream, "Could not load that step to dispatch it.", err)
	}
	if !isOpen(step.Status) {
		return Dispatch{}, fail(KindRefused, "That step is already done — nothing to dispatch.", nil)
	}

	// The gardener's playbook already knows to `gb spawn --task <id> --role
	// crew` for open children; naming the step makes this one the priority.
	seed := "Please dispatch a crew agent to work step " + stepID
	if title := strings.TrimSpace(step.Title); title != "" {
		seed += " (\"" + title + "\")"
	}
	seed += " now — it's ready to start."

	outcome, name, err := s.Assistant.Prompt(ctx, epic, a.Email, seed)
	if err != nil {
		s.Logger.Error("plan dispatch step: prompt gardener", "plan", planID, "step", stepID, "actor", a.Email, "error", err)
		return Dispatch{}, fail(KindUpstream, "Failed to hand the step to the plan gardener.", err)
	}
	s.Logger.Info("plan_dispatch_step", "event", "plan_dispatch_step", "plan", planID, "step", stepID,
		"gardener", name, "outcome", string(outcome), "operator", a.Email)
	return Dispatch{Outcome: outcome, Gardener: name}, nil
}

// RetitleStep rewords one step in place. The step must be a rendered step of
// this plan (a direct issue child of the plan or of one of its phases) so the
// verb cannot retitle an arbitrary bead.
func (s *Service) RetitleStep(ctx context.Context, planID, stepID, title string, a Actor) error {
	ctx = operatorCtx(ctx, a)
	epic, err := s.planEpic(ctx, planID)
	if err != nil {
		return err
	}
	if stepID = strings.TrimSpace(stepID); stepID == "" {
		return fail(KindBadRequest, "Missing step id", nil)
	}
	if _, ok := s.stepParent(ctx, epic, stepID); !ok {
		return fail(KindNotFound, "Step is not part of this plan", nil)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return fail(KindInvalid, "Step title cannot be empty.", nil)
	}
	if len(title) > MaxStepTitleLen {
		return fail(KindTooLarge, "Step title is too long.", nil)
	}
	if err := s.Daemon.UpdateBead(ctx, stepID, beadsapi.UpdateBeadRequest{Title: &title}); err != nil {
		s.Logger.Error("plan step reword", "plan", planID, "step", stepID, "actor", a.Email, "error", err)
		return fail(KindUpstream, "Failed to reword step.", err)
	}
	return nil
}

// ReorderStep moves one step up or down within its group by rewriting
// fields.step_order on the step's direct parent: the plan epic for a flat step,
// the phase sub-epic for a phased one. The order is seeded from the group's
// rendered order every round, so the first reorder on a never-reordered group
// writes every sibling rather than a one-element list. A boundary move is a
// no-op, not an error.
func (s *Service) ReorderStep(ctx context.Context, planID, stepID, dir string, a Actor) error {
	ctx = operatorCtx(ctx, a)
	epic, err := s.planEpic(ctx, planID)
	if err != nil {
		return err
	}
	if stepID = strings.TrimSpace(stepID); stepID == "" {
		return fail(KindBadRequest, "Missing step id", nil)
	}
	if err := checkDir(dir); err != nil {
		return err
	}
	parent, ok := s.stepParent(ctx, epic, stepID)
	if !ok {
		return fail(KindNotFound, "Step is not part of this plan", nil)
	}
	// A reorder is a blind full-array rewrite, so it goes through the daemon's
	// compare-and-swap (ka-0000000988): expected is the parent's stored
	// step_order (nil on a never-reordered group, the daemon's [] default), next
	// is the reordered rendered order.
	if _, err := s.reorderArrayField(ctx, parent.ID, StepOrderField, parent,
		func(fresh *beadsapi.BeadDetail) (expected, next []string, ok bool) {
			next, ok = ReorderStep(s.orderedStepIDs(ctx, fresh), stepID, dir)
			return StepOrder(fresh), next, ok
		}); err != nil {
		s.Logger.Error("plan step reorder", "plan", planID, "parent", parent.ID, "step", stepID, "dir", dir, "actor", a.Email, "error", err)
		return ReorderError("Failed to reorder step.", err)
	}
	return nil
}

// ReorderPhase moves one phase up or down within the plan's phase_order,
// through the same compare-and-swap-with-retry composition as `gb plan
// reorder-phase` (cmd/gb/plan_write.go reorderPlanPhase). A boundary move is a
// no-op, not an error.
func (s *Service) ReorderPhase(ctx context.Context, planID, phaseID, dir string, a Actor) error {
	ctx = operatorCtx(ctx, a)
	if err := checkDir(dir); err != nil {
		return err
	}
	epic, err := s.planEpic(ctx, planID)
	if err != nil {
		return err
	}
	_, err = s.reorderArrayField(ctx, planID, PhaseOrderField, epic,
		func(fresh *beadsapi.BeadDetail) (expected, next []string, ok bool) {
			order := PhaseOrder(fresh)
			// Swap over the rendered phase subset the ▲▼ arrows key off, not the
			// raw array, so a stale id between two rendered phases cannot absorb
			// a click (ka-0000002075). On a fetch failure fall back to the raw
			// order: the prior behavior, never worse.
			resolved := order
			if revDeps, derr := s.Daemon.GetReverseDependencies(ctx, planID); derr != nil {
				s.Logger.Debug("plan phase reorder: reverse deps", "plan", planID, "error", derr)
			} else if children, berr := s.Daemon.BatchGetBeads(ctx, parentChildIDs(revDeps)); berr != nil {
				s.Logger.Debug("plan phase reorder: batch get children", "plan", planID, "error", berr)
			} else {
				resolved = ResolvedPhaseIDs(order, children)
			}
			next, ok = ReorderPhaseResolved(order, resolved, phaseID, dir)
			return order, next, ok
		})
	if err != nil {
		s.Logger.Error("plan phase reorder", "plan", planID, "phase", phaseID, "dir", dir, "actor", a.Email, "error", err)
		return ReorderError("Failed to reorder phase.", err)
	}
	return nil
}

// CreatePhase grows phase_order: it mints a new epic under the plan, links it
// parent-child and appends its id to the plan's phase_order, returning the new
// phase's id. The same compose as `gb plan add-phase` (cmd/gb/plan_write.go
// addPlanPhase); the atomic append-if-absent is the daemon's primitive.
func (s *Service) CreatePhase(ctx context.Context, planID, title string, a Actor) (string, error) {
	ctx = operatorCtx(ctx, a)
	epic, err := s.planEpic(ctx, planID)
	if err != nil {
		return "", err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "", fail(KindInvalid, "Phase title is required.", nil)
	}
	if len(title) > MaxStepTitleLen {
		return "", fail(KindTooLarge, "Phase title is too long.", nil)
	}
	create := beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "epic",
		CreatedBy: a.Email,
	}
	if project := projectFromLabels(epic.Labels); project != "" {
		create.Labels = []string{"project:" + project}
	}
	phaseID, err := s.Daemon.CreateBead(ctx, create)
	if err != nil {
		s.Logger.Error("plan create phase: create", "plan", planID, "actor", a.Email, "error", err)
		return "", fail(KindUpstream, "Failed to create phase.", err)
	}
	// Without the edge the phase is an orphan the timeline can't resolve.
	if err := s.Daemon.AddDependency(ctx, phaseID, planID, "parent-child", a.Email); err != nil {
		s.Logger.Error("plan create phase: link parent", "plan", planID, "phase", phaseID, "actor", a.Email, "error", err)
		return phaseID, fail(KindUpstream, "Phase created but could not be linked to the plan.", err)
	}
	// The append is a single SQL UPDATE, so a concurrent phase add or reorder
	// can't be clobbered and a double-submit is idempotent.
	if err := s.Daemon.AppendBeadArrayField(ctx, planID, PhaseOrderField, phaseID); err != nil {
		s.Logger.Error("plan create phase: grow phase_order", "plan", planID, "phase", phaseID, "actor", a.Email, "error", err)
		// Linked but unlisted: it renders as a freestanding sub-plan.
		return phaseID, fail(KindUpstream, "Phase created but could not be added to the phase order.", err)
	}
	return phaseID, nil
}

func checkDir(dir string) error {
	if dir != "up" && dir != "down" {
		return fail(KindBadRequest, "dir must be up or down", nil)
	}
	return nil
}

// stepParent returns the epic that directly parents stepID within the plan
// (the plan epic for a flat step, the phase sub-epic for a phased one) and
// true iff stepID is a rendered issue-type step of the plan. It is the
// ownership check every step-mutating verb runs and the resolver for whose
// step_order a reorder rewrites.
//
// A step that is both a direct child of the plan and of a phase resolves to
// the phase, matching the timeline's rule that a phased step renders under its
// phase.
func (s *Service) stepParent(ctx context.Context, epic *beadsapi.BeadDetail, stepID string) (*beadsapi.BeadDetail, bool) {
	// Check the target is an issue-type bead up front, so the flat and phase
	// paths enforce it alike: a nested epic, doc or bundle under a phase is not
	// a step (ka-0000007445 review).
	step, err := s.Daemon.GetBead(ctx, stepID)
	if err != nil || step == nil || !IsIssueType(step.Type) {
		return nil, false
	}
	revDeps, err := s.Daemon.GetReverseDependencies(ctx, epic.ID)
	if err != nil {
		s.Logger.Debug("plan step parent: reverse deps", "plan", epic.ID, "error", err)
		return nil, false
	}
	childIDs := parentChildIDs(revDeps)
	children, err := s.Daemon.BatchGetBeads(ctx, childIDs)
	if err != nil {
		s.Logger.Debug("plan step parent: batch get children", "plan", epic.ID, "error", err)
		return nil, false
	}
	for _, pid := range PhaseOrder(epic) {
		c := children[pid]
		if !beadsapi.IsPhaseContainer(c) {
			continue
		}
		if s.isChildOf(ctx, pid, stepID) {
			return c, true
		}
	}
	if children[stepID] != nil {
		return epic, true
	}
	return nil, false
}

// isChildOf reports whether childID has a parent-child edge to parentID.
func (s *Service) isChildOf(ctx context.Context, parentID, childID string) bool {
	revDeps, err := s.Daemon.GetReverseDependencies(ctx, parentID)
	if err != nil {
		return false
	}
	return slices.Contains(parentChildIDs(revDeps), childID)
}

// orderedStepIDs returns parent's issue-type children in the order the
// timeline renders them: the step_order overlay first, then creation order. It
// is the seed a reorder swaps within. Returns nil when parent has no steps.
func (s *Service) orderedStepIDs(ctx context.Context, parent *beadsapi.BeadDetail) []string {
	revDeps, err := s.Daemon.GetReverseDependencies(ctx, parent.ID)
	if err != nil {
		s.Logger.Debug("plan ordered steps: reverse deps", "parent", parent.ID, "error", err)
		return nil
	}
	childIDs := parentChildIDs(revDeps)
	children, err := s.Daemon.BatchGetBeads(ctx, childIDs)
	if err != nil {
		s.Logger.Debug("plan ordered steps: batch get children", "parent", parent.ID, "error", err)
		return nil
	}
	stepBeads := make([]*beadsapi.BeadDetail, 0, len(childIDs))
	for _, id := range childIDs {
		if c := children[id]; c != nil && IsIssueType(c.Type) {
			stepBeads = append(stepBeads, c)
		}
	}
	if len(stepBeads) == 0 {
		return nil
	}
	SortStepBeads(stepBeads, StepOrder(parent))
	ids := make([]string, len(stepBeads))
	for i, c := range stepBeads {
		ids[i] = c.ID
	}
	return ids
}

// MaxCASReorderAttempts bounds the compare-and-swap retry loop. A 409 means a
// concurrent writer changed the array between our read and our write; after
// this many the caller surfaces the conflict and the operator clicks again.
const MaxCASReorderAttempts = 4

// reorderArrayField compare-and-swaps the string-array field on beadID, using
// derive to compute (expected, next) from a freshly read bead. initial is the
// already-loaded bead for the first attempt; on a CAS conflict the bead is
// re-read and derive re-runs, so a concurrent append is picked up rather than
// clobbered. derive returns ok=false for a boundary or absent no-op.
//
// Returns (written, err): written=false with a nil err is the no-op. An
// exhausted-retry err still reports beadsapi.IsCASConflict.
func (s *Service) reorderArrayField(ctx context.Context, beadID, field string, initial *beadsapi.BeadDetail, derive func(fresh *beadsapi.BeadDetail) (expected, next []string, ok bool)) (bool, error) {
	fresh := initial
	var lastErr error
	for attempt := 0; attempt < MaxCASReorderAttempts; attempt++ {
		if fresh == nil {
			reread, ferr := s.Daemon.GetBead(ctx, beadID)
			if ferr != nil {
				return false, fmt.Errorf("reorder %s: re-read %s: %w", field, beadID, ferr)
			}
			if reread == nil {
				return false, fmt.Errorf("reorder %s: bead %s not found on re-read", field, beadID)
			}
			fresh = reread
		}
		expected, next, ok := derive(fresh)
		if !ok {
			return false, nil
		}
		err := s.Daemon.ReorderBeadArrayField(ctx, beadID, field, expected, next)
		if err == nil {
			return true, nil
		}
		if !beadsapi.IsCASConflict(err) {
			return false, err
		}
		lastErr = err
		fresh = nil
	}
	return false, lastErr
}

// ReorderError classifies a failed reorder: an exhausted-retry CAS conflict is
// contention the operator can retry (KindConflict), anything else is a write
// failure carrying base as its message.
func ReorderError(base string, err error) *Error {
	if beadsapi.IsCASConflict(err) {
		return fail(KindConflict, "Reorder hit repeated concurrent edits — try again.", err)
	}
	return fail(KindUpstream, base, err)
}
