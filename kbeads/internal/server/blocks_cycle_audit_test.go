package server

import (
	"context"
	"reflect"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func edge(blocked, blocker string) model.DependencyEdge {
	return model.DependencyEdge{BeadID: blocked, DependsOnID: blocker}
}

func TestFindBlocksCycles_NoCycle(t *testing.T) {
	// A simple acyclic chain a->b->c (a blocked-by b, b blocked-by c).
	edges := []model.DependencyEdge{edge("gd-a", "gd-b"), edge("gd-b", "gd-c")}
	if got := findBlocksCycles(edges); len(got) != 0 {
		t.Fatalf("findBlocksCycles = %+v; want no cycles", got)
	}
}

func TestFindBlocksCycles_TwoNodeCycle(t *testing.T) {
	// The exact shape from the bug: A blocked-by B, B blocked-by A.
	edges := []model.DependencyEdge{edge("gd-a", "gd-b"), edge("gd-b", "gd-a")}
	got := findBlocksCycles(edges)
	want := []BlocksCycle{{Members: []string{"gd-a", "gd-b"}, Size: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findBlocksCycles = %+v; want %+v", got, want)
	}
}

func TestFindBlocksCycles_ThreeNodeCycle(t *testing.T) {
	edges := []model.DependencyEdge{edge("gd-a", "gd-b"), edge("gd-b", "gd-c"), edge("gd-c", "gd-a")}
	got := findBlocksCycles(edges)
	want := []BlocksCycle{{Members: []string{"gd-a", "gd-b", "gd-c"}, Size: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findBlocksCycles = %+v; want %+v", got, want)
	}
}

func TestFindBlocksCycles_SelfLoop(t *testing.T) {
	edges := []model.DependencyEdge{edge("gd-x", "gd-x")}
	got := findBlocksCycles(edges)
	want := []BlocksCycle{{Members: []string{"gd-x"}, Size: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findBlocksCycles = %+v; want %+v", got, want)
	}
}

func TestFindBlocksCycles_MultipleDisjointCycles(t *testing.T) {
	edges := []model.DependencyEdge{
		// cycle 1: a<->b
		edge("gd-a", "gd-b"), edge("gd-b", "gd-a"),
		// an acyclic tail hanging off the first cycle
		edge("gd-tail", "gd-a"),
		// cycle 2: p->q->r->p
		edge("gd-p", "gd-q"), edge("gd-q", "gd-r"), edge("gd-r", "gd-p"),
	}
	got := findBlocksCycles(edges)
	want := []BlocksCycle{
		{Members: []string{"gd-a", "gd-b"}, Size: 2},
		{Members: []string{"gd-p", "gd-q", "gd-r"}, Size: 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findBlocksCycles = %+v; want %+v", got, want)
	}
}

// TestFindBlocksCycles_SharedNodeTangle verifies overlapping cycles that share a
// node collapse into ONE strongly-connected component rather than being
// double-reported — a<->b and b<->c share b, and all three are mutually
// reachable, so they form a single 3-member SCC.
func TestFindBlocksCycles_SharedNodeTangle(t *testing.T) {
	edges := []model.DependencyEdge{
		edge("gd-a", "gd-b"), edge("gd-b", "gd-a"),
		edge("gd-b", "gd-c"), edge("gd-c", "gd-b"),
	}
	got := findBlocksCycles(edges)
	want := []BlocksCycle{{Members: []string{"gd-a", "gd-b", "gd-c"}, Size: 3}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("findBlocksCycles = %+v; want %+v", got, want)
	}
}

func TestFindBlocksCycles_Empty(t *testing.T) {
	if got := findBlocksCycles(nil); len(got) != 0 {
		t.Fatalf("findBlocksCycles(nil) = %+v; want empty", got)
	}
}

// TestAuditBlocksCycles_FullPath drives the server method against the mock store
// seeded with the concrete 2-cycle from the bug report.
func TestAuditBlocksCycles_FullPath(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	addBlocks(ms, "ka-0000001355", "ka-0000000696")
	addBlocks(ms, "ka-0000000696", "ka-0000001355")
	// An unrelated acyclic edge that must not be reported.
	addBlocks(ms, "gd-solo", "gd-other")

	audit, err := s.AuditBlocksCycles(context.Background())
	if err != nil {
		t.Fatalf("AuditBlocksCycles: %v", err)
	}
	if audit.CycleCount != 1 {
		t.Fatalf("CycleCount = %d; want 1 (audit=%+v)", audit.CycleCount, audit)
	}
	want := []string{"ka-0000000696", "ka-0000001355"}
	if !reflect.DeepEqual(audit.Cycles[0].Members, want) {
		t.Fatalf("cycle members = %v; want %v", audit.Cycles[0].Members, want)
	}
	if audit.EdgeCount != 3 {
		t.Errorf("EdgeCount = %d; want 3", audit.EdgeCount)
	}
}

func TestAuditBlocksCycles_Clean(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	addBlocks(ms, "gd-a", "gd-b")
	addBlocks(ms, "gd-b", "gd-c")

	audit, err := s.AuditBlocksCycles(context.Background())
	if err != nil {
		t.Fatalf("AuditBlocksCycles: %v", err)
	}
	if audit.CycleCount != 0 {
		t.Fatalf("CycleCount = %d; want 0 on an acyclic graph", audit.CycleCount)
	}
}
