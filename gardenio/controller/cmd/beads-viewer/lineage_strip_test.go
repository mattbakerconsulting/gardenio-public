package main

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// lineageStripServer builds a Server whose fake daemon is seeded with the
// supplied agent beads (keyed by ID) and a fixed clock, so the age labels and
// the walk are deterministic.
func lineageStripServer(t *testing.T, beads map[string]*beadsapi.BeadDetail) *Server {
	t.Helper()
	f := newFake()
	f.beads = beads
	s := NewServer(f, testLogger(), "", "", "", nil)
	s.now = func() time.Time { return time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC) }
	return s
}

// lineageAgentBead builds a type=agent BeadDetail with the succession
// back-pointer fields set, for the lineage-strip walk tests.
func lineageAgentBead(id, name, status, prior, schedPrior string) *beadsapi.BeadDetail {
	f := map[string]string{"agent": name}
	if prior != "" {
		f["prior_agent_id"] = prior
	}
	if schedPrior != "" {
		f["schedule_prior_agent_id"] = schedPrior
	}
	return &beadsapi.BeadDetail{
		ID:        id,
		Type:      "agent",
		Status:    status,
		Title:     name,
		Fields:    f,
		CreatedAt: time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC),
	}
}

func TestLineageStrip_NilForNonAgent(t *testing.T) {
	s := lineageStripServer(t, nil)
	b := &beadsapi.BeadDetail{ID: "gd-x", Type: "task", Fields: map[string]string{"prior_agent_id": "gd-y"}}
	if v := s.buildAgentLineageStrip(context.Background(), b); v != nil {
		t.Fatalf("expected nil for non-agent bead, got %+v", v)
	}
}

func TestLineageStrip_NilForFirstOfLine(t *testing.T) {
	s := lineageStripServer(t, nil)
	head := lineageAgentBead("gd-head", "agent-head", "in_progress", "", "")
	if v := s.buildAgentLineageStrip(context.Background(), head); v != nil {
		t.Fatalf("expected nil for an agent with no predecessor, got %+v", v)
	}
}

func TestLineageStrip_WalksChainToRoot(t *testing.T) {
	// root <- mid <- head (via prior_agent_id)
	beads := map[string]*beadsapi.BeadDetail{
		"gd-root": lineageAgentBead("gd-root", "agent-root", "closed", "", ""),
		"gd-mid":  lineageAgentBead("gd-mid", "agent-mid", "closed", "gd-root", ""),
		"gd-head": lineageAgentBead("gd-head", "agent-head", "working", "gd-mid", ""),
	}
	s := lineageStripServer(t, beads)
	v := s.buildAgentLineageStrip(context.Background(), beads["gd-head"])
	if v == nil {
		t.Fatal("expected a lineage view, got nil")
	}
	if v.Head == nil || v.Head.ID != "gd-head" || !v.Head.IsHead {
		t.Fatalf("head chip wrong: %+v", v.Head)
	}
	if len(v.Shown) != 2 {
		t.Fatalf("expected 2 predecessors shown, got %d", len(v.Shown))
	}
	// Newest-first: mid leads, then root.
	if v.Shown[0].ID != "gd-mid" || v.Shown[1].ID != "gd-root" {
		t.Fatalf("predecessor order wrong: %s, %s", v.Shown[0].ID, v.Shown[1].ID)
	}
	if v.EarlierCount != 0 {
		t.Errorf("EarlierCount = %d, want 0", v.EarlierCount)
	}
	if v.Pruned {
		t.Error("Pruned should be false for a chain that reached its root")
	}
	if v.RootID != "gd-root" {
		t.Errorf("RootID = %q, want gd-root", v.RootID)
	}
	if v.TotalKnown != 3 {
		t.Errorf("TotalKnown = %d, want 3", v.TotalKnown)
	}
}

func TestLineageStrip_FallsBackToSchedulePrior(t *testing.T) {
	// Legacy gardener chain: only schedule_prior_agent_id set.
	beads := map[string]*beadsapi.BeadDetail{
		"gd-root": lineageAgentBead("gd-root", "cap-1", "closed", "", ""),
		"gd-head": lineageAgentBead("gd-head", "cap-2", "working", "", "gd-root"),
	}
	s := lineageStripServer(t, beads)
	v := s.buildAgentLineageStrip(context.Background(), beads["gd-head"])
	if v == nil {
		t.Fatal("expected a lineage view from schedule_prior_agent_id fallback, got nil")
	}
	if len(v.Shown) != 1 || v.Shown[0].ID != "gd-root" {
		t.Fatalf("expected schedule_prior predecessor gd-root, got %+v", v.Shown)
	}
	if v.Pruned {
		t.Error("Pruned should be false")
	}
}

func TestLineageStrip_PaginatesBeyondK(t *testing.T) {
	// Build a chain of lineageStripShownK+3 predecessors under the head.
	n := lineageStripShownK + 3
	beads := map[string]*beadsapi.BeadDetail{}
	// oldest has no prior; each newer points at the previous.
	prevID := ""
	for i := 0; i < n; i++ {
		id := "gd-p" + itoa(i)
		beads[id] = lineageAgentBead(id, "agent-"+itoa(i), "closed", prevID, "")
		prevID = id
	}
	head := lineageAgentBead("gd-head", "agent-head", "working", prevID, "")
	beads["gd-head"] = head
	s := lineageStripServer(t, beads)
	v := s.buildAgentLineageStrip(context.Background(), head)
	if v == nil {
		t.Fatal("expected lineage view, got nil")
	}
	if len(v.Shown) != lineageStripShownK {
		t.Fatalf("Shown = %d, want %d", len(v.Shown), lineageStripShownK)
	}
	if v.EarlierCount != 3 {
		t.Errorf("EarlierCount = %d, want 3", v.EarlierCount)
	}
	if v.Pruned {
		t.Error("Pruned should be false — chain reached its root")
	}
	if v.RootID != "gd-p0" {
		t.Errorf("RootID = %q, want gd-p0 (the oldest run)", v.RootID)
	}
}

func TestLineageStrip_DanglingPredecessorIsPruned(t *testing.T) {
	// head <- mid <- (missing root). The mid points at gd-gone, which is not
	// seeded, so the walk terminates and flags Pruned.
	beads := map[string]*beadsapi.BeadDetail{
		"gd-mid":  lineageAgentBead("gd-mid", "agent-mid", "closed", "gd-gone", ""),
		"gd-head": lineageAgentBead("gd-head", "agent-head", "working", "gd-mid", ""),
	}
	s := lineageStripServer(t, beads)
	v := s.buildAgentLineageStrip(context.Background(), beads["gd-head"])
	if v == nil {
		t.Fatal("expected lineage view, got nil")
	}
	if len(v.Shown) != 1 || v.Shown[0].ID != "gd-mid" {
		t.Fatalf("expected the one resolved predecessor gd-mid, got %+v", v.Shown)
	}
	if !v.Pruned {
		t.Error("Pruned should be true when a predecessor pointer dangles")
	}
	if v.RootID != "" {
		t.Errorf("RootID should be empty on a pruned chain, got %q", v.RootID)
	}
}

func TestLineageStrip_UnresolvableFirstHopSuppresses(t *testing.T) {
	// The head points at a predecessor that cannot be resolved at all.
	head := lineageAgentBead("gd-head", "agent-head", "working", "gd-gone", "")
	beads := map[string]*beadsapi.BeadDetail{"gd-head": head}
	s := lineageStripServer(t, beads)
	if v := s.buildAgentLineageStrip(context.Background(), head); v != nil {
		t.Fatalf("expected nil when the sole predecessor is unresolvable, got %+v", v)
	}
}

// TestLineageStrip_CycleGuard guards ka-0000007852: a cyclic prior_agent_id
// chain stops the walk and flags the strip Pruned.
func TestLineageStrip_CycleGuard(t *testing.T) {
	// head <- a <- b <- a (cycle). Must terminate and flag Pruned.
	beads := map[string]*beadsapi.BeadDetail{
		"gd-a":    lineageAgentBead("gd-a", "agent-a", "closed", "gd-b", ""),
		"gd-b":    lineageAgentBead("gd-b", "agent-b", "closed", "gd-a", ""),
		"gd-head": lineageAgentBead("gd-head", "agent-head", "working", "gd-a", ""),
	}
	s := lineageStripServer(t, beads)
	v := s.buildAgentLineageStrip(context.Background(), beads["gd-head"])
	if v == nil {
		t.Fatal("expected lineage view, got nil")
	}
	if !v.Pruned {
		t.Error("Pruned should be true when the chain cycles")
	}
	// a and b are both resolved before the cycle is detected.
	if len(v.Shown) != 2 {
		t.Errorf("Shown = %d, want 2 (gd-a, gd-b before cycle)", len(v.Shown))
	}
}

func TestLineagePriorID_Precedence(t *testing.T) {
	// prior_agent_id wins over schedule_prior_agent_id.
	got := lineagePriorID(map[string]string{
		"prior_agent_id":          "gd-new",
		"schedule_prior_agent_id": "gd-legacy",
	})
	if got != "gd-new" {
		t.Errorf("lineagePriorID precedence = %q, want gd-new", got)
	}
	if got := lineagePriorID(map[string]string{"schedule_prior_agent_id": "gd-legacy"}); got != "gd-legacy" {
		t.Errorf("lineagePriorID fallback = %q, want gd-legacy", got)
	}
	if got := lineagePriorID(nil); got != "" {
		t.Errorf("lineagePriorID(nil) = %q, want empty", got)
	}
}
