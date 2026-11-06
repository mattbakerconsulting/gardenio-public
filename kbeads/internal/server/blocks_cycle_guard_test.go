package server

import (
	"context"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
)

// addBlocks seeds a blocks edge in the mock store (bead_id=blocked,
// depends_on_id=blocker) so the cycle walk can traverse it.
func addBlocks(ms *mockStore, blocked, blocker string) {
	ms.deps[blocked] = append(ms.deps[blocked], &model.Dependency{
		BeadID:      blocked,
		DependsOnID: blocker,
		Type:        model.DepBlocks,
	})
}

func TestWouldCreateBlocksCycle_DirectTwoNodeCycle(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Existing: gd-a is blocked-by gd-b.
	addBlocks(ms, "gd-a", "gd-b")
	// New edge: gd-b becomes blocked-by gd-a — closes the 2-cycle that pinned
	// the six gardenio beads for 12 days (ka-0000007984).
	dep := &model.Dependency{
		BeadID:      "gd-b",
		DependsOnID: "gd-a",
		Type:        model.DepBlocks,
	}
	if !s.wouldCreateBlocksCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateBlocksCycle = false; want true (a<->b 2-cycle)")
	}
}

func TestWouldCreateBlocksCycle_TransitiveCycle(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Existing chain: gd-a blocked-by gd-b, gd-b blocked-by gd-c.
	addBlocks(ms, "gd-a", "gd-b")
	addBlocks(ms, "gd-b", "gd-c")
	// New edge: gd-c blocked-by gd-a — closes a 3-cycle a->b->c->a.
	dep := &model.Dependency{
		BeadID:      "gd-c",
		DependsOnID: "gd-a",
		Type:        model.DepBlocks,
	}
	if !s.wouldCreateBlocksCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateBlocksCycle = false; want true (transitive 3-cycle)")
	}
}

func TestWouldCreateBlocksCycle_SelfEdge(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	dep := &model.Dependency{
		BeadID:      "gd-x",
		DependsOnID: "gd-x",
		Type:        model.DepBlocks,
	}
	if !s.wouldCreateBlocksCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateBlocksCycle = false; want true (self-edge is the trivial 1-cycle)")
	}
}

func TestWouldCreateBlocksCycle_AcyclicAllowed(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Existing: gd-a blocked-by gd-b. Adding a further blocker deepens the chain
	// but creates no cycle.
	addBlocks(ms, "gd-a", "gd-b")
	dep := &model.Dependency{
		BeadID:      "gd-b",
		DependsOnID: "gd-c",
		Type:        model.DepBlocks,
	}
	if s.wouldCreateBlocksCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateBlocksCycle = true; want false (acyclic deepening must be allowed)")
	}
}

// TestWouldCreateBlocksCycle_IgnoresParentChild ensures a parent-child edge in
// the reverse direction is NOT treated as a blocks cycle — the guard filters on
// edge type so an A blocked-by B pair coexisting with a B parent-of A hierarchy
// edge is legitimate.
func TestWouldCreateBlocksCycle_IgnoresParentChild(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	addBlocks(ms, "gd-a", "gd-b")
	dep := &model.Dependency{
		BeadID:      "gd-b",
		DependsOnID: "gd-a",
		Type:        model.DepParentChild,
	}
	if s.wouldCreateBlocksCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateBlocksCycle = true; want false (only blocks edges are guarded)")
	}
}

// TestWouldCreateBlocksCycle_IgnoresParentChildChainInGraph ensures the walk
// only follows blocks edges: a parent-child chain between the endpoints must
// not be mistaken for a blocks path.
func TestWouldCreateBlocksCycle_IgnoresParentChildChainInGraph(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// gd-a is a parent-child child of gd-b (not a blocker relationship).
	addPC(ms, "gd-a", "gd-b")
	// New blocks edge gd-b blocked-by gd-a: no blocks path a->...->b exists, so
	// this is acyclic in the blocks graph despite the parent-child chain.
	dep := &model.Dependency{
		BeadID:      "gd-b",
		DependsOnID: "gd-a",
		Type:        model.DepBlocks,
	}
	if s.wouldCreateBlocksCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateBlocksCycle = true; want false (parent-child edges must not count as blocks path)")
	}
}

// TestWouldCreateBlocksCycle_TerminatesOnPreexistingCycle ensures the
// visited-set walk halts even when the stored blocks graph already contains a
// cycle that does not involve the new edge's endpoints.
func TestWouldCreateBlocksCycle_TerminatesOnPreexistingCycle(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	// Pre-existing 2-cycle between gd-p and gd-q (corrupt data).
	addBlocks(ms, "gd-p", "gd-q")
	addBlocks(ms, "gd-q", "gd-p")
	// New edge unrelated to the target: gd-new blocked-by gd-p. Walking up from
	// gd-p hits the p<->q cycle but never reaches gd-new — must return false
	// without looping forever.
	dep := &model.Dependency{
		BeadID:      "gd-new",
		DependsOnID: "gd-p",
		Type:        model.DepBlocks,
	}
	if s.wouldCreateBlocksCycle(context.Background(), dep) {
		t.Fatalf("wouldCreateBlocksCycle = true; want false (unrelated edge atop a preexisting cycle)")
	}
}

// TestAddDependency_RejectsBlocksCycle exercises the full gRPC AddDependency
// path: an add that would close a blocks 2-cycle is refused with
// FailedPrecondition and no edge is written.
func TestAddDependency_RejectsBlocksCycle(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")
	seedBead(ms, "gd-b", model.TypeTask, model.StatusOpen, nil, nil)
	addBlocks(ms, "gd-a", "gd-b") // gd-a blocked-by gd-b

	_, err := srv.AddDependency(ctx, &beadsv1.AddDependencyRequest{
		BeadId: "gd-b", DependsOnId: "gd-a", Type: string(model.DepBlocks),
	})
	requireCode(t, err, codes.FailedPrecondition)
	if got := len(ms.deps["gd-b"]); got != 0 {
		t.Fatalf("blocks-cycle edge was written despite rejection: gd-b has %d dep(s)", got)
	}
}

// TestAddDependency_AllowsAcyclicBlocks confirms the guard does not over-reject:
// a plain, non-cycle-forming blocks edge still succeeds.
func TestAddDependency_AllowsAcyclicBlocks(t *testing.T) {
	srv, ms, ctx := testCtxAs(t, "matt@acme.example")
	seedBead(ms, "gd-a", model.TypeTask, model.StatusOpen, nil, nil)
	if _, err := srv.AddDependency(ctx, &beadsv1.AddDependencyRequest{
		BeadId: "gd-a", DependsOnId: "gd-b", Type: string(model.DepBlocks),
	}); err != nil {
		t.Fatalf("AddDependency rejected an acyclic blocks edge: %v", err)
	}
	if got := len(ms.deps["gd-a"]); got != 1 {
		t.Fatalf("acyclic blocks edge not written: gd-a has %d dep(s), want 1", got)
	}
}
