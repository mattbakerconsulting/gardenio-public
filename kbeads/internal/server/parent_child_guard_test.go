package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// seedTypedBead inserts a minimal bead of the given type for the guard tests.
func seedTypedBead(ms *mockStore, id string, typ model.BeadType) {
	ms.beads[id] = &model.Bead{
		ID:     id,
		Kind:   model.KindIssue,
		Type:   typ,
		Title:  id,
		Status: model.StatusOpen,
	}
}

func TestCanonicalizeParentChildDep_FlipsReversedEpicEdge(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBead(ms, "gd-epic", model.TypeEpic)
	seedTypedBead(ms, "gd-child", model.TypeTask)

	// Reversed: bead_id=epic, depends_on_id=child.
	dep := &model.Dependency{
		BeadID:      "gd-epic",
		DependsOnID: "gd-child",
		Type:        model.DepParentChild,
	}
	if !s.canonicalizeParentChildDep(context.Background(), dep) {
		t.Fatalf("canonicalizeParentChildDep returned false; want true (reversed edge should be flipped)")
	}
	if dep.BeadID != "gd-child" || dep.DependsOnID != "gd-epic" {
		t.Fatalf("after canonicalize: bead_id=%q depends_on_id=%q; want bead_id=gd-child depends_on_id=gd-epic", dep.BeadID, dep.DependsOnID)
	}
}

func TestCanonicalizeParentChildDep_LeavesCanonicalEdge(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBead(ms, "gd-epic", model.TypeEpic)
	seedTypedBead(ms, "gd-child", model.TypeTask)

	// Canonical: bead_id=child, depends_on_id=epic. The child side is a
	// task (not an epic), so the guard must not touch it.
	dep := &model.Dependency{
		BeadID:      "gd-child",
		DependsOnID: "gd-epic",
		Type:        model.DepParentChild,
	}
	if s.canonicalizeParentChildDep(context.Background(), dep) {
		t.Fatalf("canonicalizeParentChildDep returned true; want false (canonical edge must be untouched)")
	}
	if dep.BeadID != "gd-child" || dep.DependsOnID != "gd-epic" {
		t.Fatalf("canonical edge was mutated: bead_id=%q depends_on_id=%q", dep.BeadID, dep.DependsOnID)
	}
}

func TestCanonicalizeParentChildDep_LeavesEpicToEpic(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBead(ms, "gd-superepic", model.TypeEpic)
	seedTypedBead(ms, "gd-subepic", model.TypeEpic)

	// epic -> epic is a legitimate sub-epic edge; both sides are epics so
	// the direction is ambiguous and the guard must leave it alone.
	dep := &model.Dependency{
		BeadID:      "gd-subepic",
		DependsOnID: "gd-superepic",
		Type:        model.DepParentChild,
	}
	if s.canonicalizeParentChildDep(context.Background(), dep) {
		t.Fatalf("canonicalizeParentChildDep returned true; want false (epic->epic must be untouched)")
	}
}

func TestCanonicalizeParentChildDep_IgnoresNonParentChild(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBead(ms, "gd-epic", model.TypeEpic)
	seedTypedBead(ms, "gd-child", model.TypeTask)

	dep := &model.Dependency{
		BeadID:      "gd-epic",
		DependsOnID: "gd-child",
		Type:        model.DepBlocks,
	}
	if s.canonicalizeParentChildDep(context.Background(), dep) {
		t.Fatalf("canonicalizeParentChildDep returned true; want false (only parent-child is guarded)")
	}
}

func TestCanonicalizeParentChildDep_FailOpenOnMissingBead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Neither bead exists — GetBead returns (nil, nil) on the mock.
	dep := &model.Dependency{
		BeadID:      "gd-missing-epic",
		DependsOnID: "gd-missing-child",
		Type:        model.DepParentChild,
	}
	if s.canonicalizeParentChildDep(context.Background(), dep) {
		t.Fatalf("canonicalizeParentChildDep returned true; want false (must fail-open when endpoints are unresolved)")
	}
}

// TestMaybeAddAutoParent_SkipsEpicChild verifies the auto-parent path
// (ka-0000006880) does NOT wire an edge when the freshly-created bead is
// itself an epic and the agent's task is a non-epic — that would store
// the reversed shape that breaks the subtree CTE.
func TestMaybeAddAutoParent_SkipsEpicChild(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedAgentBead(t, ms, "agent-x", "gd-task")

	// The new bead is an epic; agent's task_id (gd-task) is a task.
	epic := &model.Bead{ID: "gd-newepic", Type: model.TypeEpic, Status: model.StatusOpen}
	ms.beads["gd-newepic"] = epic

	ctx := withTestAgent(context.Background(), "agent-x")
	s.maybeAddAutoParent(ctx, epic, epic.CreatedAt)

	if deps := ms.deps["gd-newepic"]; len(deps) != 0 {
		t.Fatalf("auto-parent wrote %d dep(s) for an epic child; want 0 (reversed-edge guard must skip)", len(deps))
	}
}

// --- Cycle guard (ka-0000006899) ---------------------------------------

// addPC seeds a parent-child edge in the mock store (bead_id=child,
// depends_on_id=parent) so the cycle walk can traverse it.
func addPC(ms *mockStore, child, parent string) {
	ms.deps[child] = append(ms.deps[child], &model.Dependency{
		BeadID:      child,
		DependsOnID: parent,
		Type:        model.DepParentChild,
	})
}

func TestWouldCreateParentChildCycle_DirectTwoNodeCycle(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Existing: gd-child is a child of gd-epic.
	addPC(ms, "gd-child", "gd-epic")
	// New edge: gd-epic becomes a child of gd-child — closes a 2-cycle.
	dep := &model.Dependency{
		BeadID:      "gd-epic",
		DependsOnID: "gd-child",
		Type:        model.DepParentChild,
	}
	if !s.wouldCreateParentChildCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateParentChildCycle = false; want true (epic<->child 2-cycle)")
	}
}

func TestWouldCreateParentChildCycle_TransitiveCycle(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Existing chain: gd-c child of gd-b, gd-b child of gd-a.
	addPC(ms, "gd-c", "gd-b")
	addPC(ms, "gd-b", "gd-a")
	// New edge: gd-a becomes a child of gd-c — closes a 3-cycle a->b->c->a.
	dep := &model.Dependency{
		BeadID:      "gd-a",
		DependsOnID: "gd-c",
		Type:        model.DepParentChild,
	}
	if !s.wouldCreateParentChildCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateParentChildCycle = false; want true (transitive 3-cycle)")
	}
}

func TestWouldCreateParentChildCycle_SelfEdge(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	dep := &model.Dependency{
		BeadID:      "gd-x",
		DependsOnID: "gd-x",
		Type:        model.DepParentChild,
	}
	if !s.wouldCreateParentChildCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateParentChildCycle = false; want true (self-edge is the trivial 1-cycle)")
	}
}

func TestWouldCreateParentChildCycle_AcyclicAllowed(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Existing: gd-child is a child of gd-epic. Adding a sibling grandchild
	// deepens the tree but creates no cycle.
	addPC(ms, "gd-child", "gd-epic")
	dep := &model.Dependency{
		BeadID:      "gd-lq6hamdgda",
		DependsOnID: "gd-child",
		Type:        model.DepParentChild,
	}
	if s.wouldCreateParentChildCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateParentChildCycle = true; want false (acyclic deepening must be allowed)")
	}
}

func TestWouldCreateParentChildCycle_IgnoresNonParentChild(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	addPC(ms, "gd-child", "gd-epic")
	// A blocks edge in the reverse direction is not a parent-child cycle.
	dep := &model.Dependency{
		BeadID:      "gd-epic",
		DependsOnID: "gd-child",
		Type:        model.DepBlocks,
	}
	if s.wouldCreateParentChildCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateParentChildCycle = true; want false (only parent-child is guarded)")
	}
}

// TestWouldCreateParentChildCycle_TerminatesOnPreexistingCycle ensures the
// visited-set walk halts even when the stored graph already contains a cycle
// that does not involve the new edge's endpoints.
func TestWouldCreateParentChildCycle_TerminatesOnPreexistingCycle(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Pre-existing 2-cycle between gd-p and gd-q (corrupt data).
	addPC(ms, "gd-p", "gd-q")
	addPC(ms, "gd-q", "gd-p")
	// New edge unrelated to the target: gd-new child of gd-p. Walking up from
	// gd-p hits the p<->q cycle but never reaches gd-new — must return false
	// without looping forever.
	dep := &model.Dependency{
		BeadID:      "gd-new",
		DependsOnID: "gd-p",
		Type:        model.DepParentChild,
	}
	if s.wouldCreateParentChildCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateParentChildCycle = true; want false (unrelated edge atop a preexisting cycle)")
	}
}

// --- Closed-parent guard (ka-0000005756) -------------------------------

// seedTypedBeadStatus inserts a minimal bead with an explicit status so the
// closed-parent guard tests can vary the parent's lifecycle state.
func seedTypedBeadStatus(ms *mockStore, id string, typ model.BeadType, st model.Status) {
	ms.beads[id] = &model.Bead{
		ID:     id,
		Kind:   model.KindIssue,
		Type:   typ,
		Title:  id,
		Status: st,
	}
}

func TestClosedParentSide_FlagsClosedParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBeadStatus(ms, "gd-deadepic", model.TypeEpic, model.StatusClosed)
	seedTypedBeadStatus(ms, "gd-child", model.TypeTask, model.StatusOpen)

	dep := &model.Dependency{
		BeadID:      "gd-child",
		DependsOnID: "gd-deadepic",
		Type:        model.DepParentChild,
	}
	parent := s.closedParentSide(context.Background(), dep)
	if parent == nil {
		t.Fatalf("closedParentSide = nil; want the closed parent bead")
	}
	if parent.ID != "gd-deadepic" {
		t.Fatalf("closedParentSide returned %q; want gd-deadepic", parent.ID)
	}
}

func TestClosedParentSide_IgnoresOpenParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBeadStatus(ms, "gd-liveepic", model.TypeEpic, model.StatusOpen)
	seedTypedBeadStatus(ms, "gd-child", model.TypeTask, model.StatusOpen)

	dep := &model.Dependency{
		BeadID:      "gd-child",
		DependsOnID: "gd-liveepic",
		Type:        model.DepParentChild,
	}
	if p := s.closedParentSide(context.Background(), dep); p != nil {
		t.Fatalf("closedParentSide = %q; want nil (open parent must not be flagged)", p.ID)
	}
}

// deferred/blocked parents are legitimately re-openable and must NOT be
// flagged — only StatusClosed (the drain-invisible case) is rejected.
func TestClosedParentSide_IgnoresDeferredParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBeadStatus(ms, "gd-defepic", model.TypeEpic, model.StatusDeferred)
	seedTypedBeadStatus(ms, "gd-child", model.TypeTask, model.StatusOpen)

	dep := &model.Dependency{
		BeadID:      "gd-child",
		DependsOnID: "gd-defepic",
		Type:        model.DepParentChild,
	}
	if p := s.closedParentSide(context.Background(), dep); p != nil {
		t.Fatalf("closedParentSide = %q; want nil (deferred parent must not be flagged)", p.ID)
	}
}

func TestClosedParentSide_IgnoresNonParentChild(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedTypedBeadStatus(ms, "gd-deadepic", model.TypeEpic, model.StatusClosed)
	seedTypedBeadStatus(ms, "gd-child", model.TypeTask, model.StatusOpen)

	dep := &model.Dependency{
		BeadID:      "gd-child",
		DependsOnID: "gd-deadepic",
		Type:        model.DepBlocks,
	}
	if p := s.closedParentSide(context.Background(), dep); p != nil {
		t.Fatalf("closedParentSide = %q; want nil (only parent-child is guarded)", p.ID)
	}
}

func TestClosedParentSide_FailOpenOnMissingParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Parent bead absent — GetBead returns (nil, nil) on the mock.
	dep := &model.Dependency{
		BeadID:      "gd-child",
		DependsOnID: "gd-missing-parent",
		Type:        model.DepParentChild,
	}
	if p := s.closedParentSide(context.Background(), dep); p != nil {
		t.Fatalf("closedParentSide = %q; want nil (must fail-open on unresolved parent)", p.ID)
	}
}

// TestMaybeAddAutoParent_SkipsClosedParent verifies the auto-parent path
// (ka-0000005756) does NOT wire an edge when the agent's task_id resolves to
// an already-CLOSED epic — the child would become a claimable orphan the
// auto-close cascade can never drain.
func TestMaybeAddAutoParent_SkipsClosedParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedAgentBead(t, ms, "agent-x", "gd-deadtask")
	// Override the seeded task to be a CLOSED epic.
	ms.beads["gd-deadtask"] = &model.Bead{
		ID:     "gd-deadtask",
		Kind:   model.KindIssue,
		Type:   model.TypeEpic,
		Title:  "dead epic",
		Status: model.StatusClosed,
	}

	newBead := &model.Bead{ID: "gd-newwork", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-newwork"] = newBead

	ctx := withTestAgent(context.Background(), "agent-x")
	s.maybeAddAutoParent(ctx, newBead, newBead.CreatedAt)

	if deps := ms.deps["gd-newwork"]; len(deps) != 0 {
		t.Fatalf("auto-parent wrote %d dep(s) onto a closed epic; want 0 (closed-parent guard must skip)", len(deps))
	}
}

// TestMaybeAddAutoParent_WiresOpenParent is the positive control: an OPEN
// task parent still auto-parents normally (the guard is scoped to closed).
func TestMaybeAddAutoParent_WiresOpenParent(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	seedAgentBead(t, ms, "agent-x", "gd-livetask")

	newBead := &model.Bead{ID: "gd-newwork", Type: model.TypeTask, Status: model.StatusOpen}
	ms.beads["gd-newwork"] = newBead

	ctx := withTestAgent(context.Background(), "agent-x")
	s.maybeAddAutoParent(ctx, newBead, newBead.CreatedAt)

	deps := ms.deps["gd-newwork"]
	if len(deps) != 1 {
		t.Fatalf("auto-parent wrote %d dep(s) onto an open parent; want 1", len(deps))
	}
	if deps[0].DependsOnID != "gd-livetask" || deps[0].Type != model.DepParentChild {
		t.Fatalf("auto-parent wrote %s->%s (%s); want gd-newwork->gd-livetask (parent-child)",
			deps[0].BeadID, deps[0].DependsOnID, deps[0].Type)
	}
}
