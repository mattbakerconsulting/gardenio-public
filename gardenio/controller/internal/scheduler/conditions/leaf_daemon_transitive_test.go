package conditions

import (
	"context"
	"strconv"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// leaf_daemon_transitive_test.go covers the transitive dormancy-brake fix
// (ka-0000004214): the exclude_blocked_by_open_types exclusion used to subtract
// only children DIRECTLY blocked by an open escalation/decision (ONE HOP), so a
// child gated behind an intermediate open task that is itself blocked by an
// escalation counted as actionable and the gardener never parked. The fix walks
// the forward blocking-dep chain to a fixpoint, and additionally subtracts
// operator-assigned leaves (fix option 3) and non-actionable bundle children.

// bead builds a detail row with the fields the exclusion inspects.
func bead(id, typ, status, assignee string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{ID: id, Type: typ, Status: status, Assignee: assignee}
}

// transitiveLoopEnd is the wedge-brake's actionable-count query shape: "==0 open
// task/bug/feature/epic children in the subtree, minus those gated behind an
// open escalation/decision".
var transitiveLoopEnd = Leaf{
	Type:                      "bead_query",
	Op:                        "==",
	Value:                     "0",
	Parent:                    "gd-EPIC",
	Types:                     []string{"task", "bug", "feature", "epic"},
	Statuses:                  []string{"open", "in_progress", "blocked"},
	ExcludeBlockedByOpenTypes: []string{"escalation", "decision"},
}

// evalTransitive runs transitiveLoopEnd against the given graph and reports
// whether loop_end (==0) is MET — i.e. the effective actionable count reached 0.
func evalTransitive(t *testing.T, rows []*beadsapi.BeadDetail, deps map[string][]beadsapi.Dependency, beads map[string]*beadsapi.BeadDetail) bool {
	t.Helper()
	bc := &stubBlockedResolver{rows: rows, depsByChild: deps, beadsByID: beads}
	got, err := Evaluate(context.Background(), Expression{Leaf: transitiveLoopEnd}, Env{Bead: bc})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return got
}

// TestExcludeBlocked_TransitiveTwoLevel is the minimal reproduction of the bug:
// the only open child is blocked by an open intermediate TASK that is itself
// blocked by an open escalation. One-hop counted it as actionable; the
// transitive walk subtracts it and loop_end fires.
func TestExcludeBlocked_TransitiveTwoLevel(t *testing.T) {
	const (
		child = "gd-child"
		mid   = "gd-mid"
		esc   = "gd-esc"
	)
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	deps := map[string][]beadsapi.Dependency{
		child: {blocksDep(child, mid)},
		mid:   {blocksDep(mid, esc)},
	}
	beads := map[string]*beadsapi.BeadDetail{
		mid: bead(mid, "task", "open", ""),
		esc: bead(esc, "escalation", "open", ""),
	}
	if !evalTransitive(t, rows, deps, beads) {
		t.Fatal("expected loop_end MET: child transitively blocked (child→mid→open escalation) must be subtracted")
	}
}

// TestExcludeBlocked_TransitiveThreeLevel matches the measured ka-0000000104
// shape: child → phase task → intermediate task → open escalation (3 hops).
func TestExcludeBlocked_TransitiveThreeLevel(t *testing.T) {
	const (
		child = "gd-child"
		a     = "gd-a"
		b     = "gd-b"
		esc   = "gd-esc"
	)
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	deps := map[string][]beadsapi.Dependency{
		child: {blocksDep(child, a)},
		a:     {blocksDep(a, b)},
		b:     {blocksDep(b, esc)},
	}
	beads := map[string]*beadsapi.BeadDetail{
		a:   bead(a, "task", "open", ""),
		b:   bead(b, "task", "open", ""),
		esc: bead(esc, "escalation", "open", ""),
	}
	if !evalTransitive(t, rows, deps, beads) {
		t.Fatal("expected loop_end MET: 3-hop transitively operator-blocked child must be subtracted")
	}
}

// TestExcludeBlocked_TransitiveWithLiveBranch verifies the "EVERY path must
// terminate in operator action" rule: if any live branch bottoms out at an
// agent-actionable task, the child is NOT subtracted (gardener stays awake).
func TestExcludeBlocked_TransitiveWithLiveBranch(t *testing.T) {
	const (
		child = "gd-child"
		mid   = "gd-mid"
		esc   = "gd-esc"
		live  = "gd-live" // open, unassigned, no deps → agent-actionable
	)
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	deps := map[string][]beadsapi.Dependency{
		child: {blocksDep(child, mid)},
		mid:   {blocksDep(mid, esc), blocksDep(mid, live)},
		live:  nil,
	}
	beads := map[string]*beadsapi.BeadDetail{
		mid:  bead(mid, "task", "open", ""),
		esc:  bead(esc, "escalation", "open", ""),
		live: bead(live, "task", "open", ""),
	}
	if evalTransitive(t, rows, deps, beads) {
		t.Fatal("expected loop_end NOT met: a live agent-actionable branch (gd-live) keeps the child actionable")
	}
}

// TestExcludeBlocked_ClosedIntermediateIsNotALiveWait verifies a CLOSED
// intermediate is not a live blocker: if the child's only blocking dep is a
// closed task, the child is unblocked work and must NOT be subtracted.
func TestExcludeBlocked_ClosedIntermediateIsNotALiveWait(t *testing.T) {
	const (
		child = "gd-child"
		mid   = "gd-mid"
		esc   = "gd-esc"
	)
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	deps := map[string][]beadsapi.Dependency{
		child: {blocksDep(child, mid)},
		mid:   {blocksDep(mid, esc)},
	}
	beads := map[string]*beadsapi.BeadDetail{
		mid: bead(mid, "task", "closed", ""), // CLOSED → child is unblocked
		esc: bead(esc, "escalation", "open", ""),
	}
	if evalTransitive(t, rows, deps, beads) {
		t.Fatal("expected loop_end NOT met: a closed intermediate is not a live wait, so the child is actionable")
	}
}

// TestExcludeBlocked_OperatorAssignedLeaf covers fix option 3: an open leaf task
// with a HUMAN assignee and no live blocking dep (the ka-0000006926 shape) is
// non-actionable — no crew agent can self-serve it — and must be subtracted.
func TestExcludeBlocked_OperatorAssignedLeaf(t *testing.T) {
	const child = "gd-child"
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "Matthew.Baker")}
	if !evalTransitive(t, rows, map[string][]beadsapi.Dependency{child: nil}, nil) {
		t.Fatal("expected loop_end MET: an operator-assigned leaf task is non-actionable")
	}
}

// TestExcludeBlocked_AgentAssignedLeafStaysActionable ensures a leaf claimed by
// an AGENT (lowercase-hyphen name) is NOT read as operator-owned — it is live
// crew work and keeps the gardener awake.
func TestExcludeBlocked_AgentAssignedLeafStaysActionable(t *testing.T) {
	const child = "gd-child"
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "gardenio-crew-abc-x1y")}
	if evalTransitive(t, rows, map[string][]beadsapi.Dependency{child: nil}, nil) {
		t.Fatal("expected loop_end NOT met: an agent-assigned leaf is live crew work, not operator-blocked")
	}
}

// TestExcludeBlocked_UnassignedLeafStaysActionable ensures an unclaimed open
// leaf with no blockers is treated as ready work (the base actionable case).
func TestExcludeBlocked_UnassignedLeafStaysActionable(t *testing.T) {
	const child = "gd-child"
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	if evalTransitive(t, rows, map[string][]beadsapi.Dependency{child: nil}, nil) {
		t.Fatal("expected loop_end NOT met: an unassigned unblocked leaf is actionable work")
	}
}

// TestExcludeBlocked_BundleChildAlwaysSubtracted covers the bundle handling: a
// type=bundle plan-of-record narrative child is never crew-actionable and is
// subtracted regardless of whether it carries any blocking dep.
func TestExcludeBlocked_BundleChildAlwaysSubtracted(t *testing.T) {
	// Bundle appears in the matched set (a caller whose Types include bundle, or
	// a broader query): it must not floor the count.
	leaf := transitiveLoopEnd
	leaf.Types = []string{"task", "bug", "feature", "epic", "bundle"}
	const bundle = "gd-bundle"
	bc := &stubBlockedResolver{
		rows:        []*beadsapi.BeadDetail{bead(bundle, "bundle", "open", "")},
		depsByChild: map[string][]beadsapi.Dependency{bundle: nil},
	}
	got, err := Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: bc})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !got {
		t.Fatal("expected loop_end MET: a type=bundle narrative child is non-actionable and must be subtracted")
	}
}

// TestExcludeBlocked_CycleGuard ensures a blocking CYCLE never manufactures a
// phantom subtract: A→B→A cannot be proven to terminate in operator action, so
// the child stays counted (fail-safe).
func TestExcludeBlocked_CycleGuard(t *testing.T) {
	const (
		child = "gd-child"
		a     = "gd-a"
		b     = "gd-b"
	)
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	deps := map[string][]beadsapi.Dependency{
		child: {blocksDep(child, a)},
		a:     {blocksDep(a, b)},
		b:     {blocksDep(b, a)}, // cycle b→a
	}
	beads := map[string]*beadsapi.BeadDetail{
		a: bead(a, "task", "open", ""),
		b: bead(b, "task", "open", ""),
	}
	if evalTransitive(t, rows, deps, beads) {
		t.Fatal("expected loop_end NOT met: a blocking cycle is unprovable and must not be subtracted")
	}
}

// TestExcludeBlocked_DepthCapFailSafe ensures a chain longer than
// maxTransitiveBlockDepth leaves the tail unresolved and the child stays counted
// (fail-safe under-subtraction), never over-parking the gardener.
func TestExcludeBlocked_DepthCapFailSafe(t *testing.T) {
	const child = "gd-child"
	// child → t1 → t2 → ... → tN → esc, with N well past the depth cap.
	n := maxTransitiveBlockDepth + 3
	deps := map[string][]beadsapi.Dependency{}
	beads := map[string]*beadsapi.BeadDetail{}
	prev := child
	for i := 1; i <= n; i++ {
		id := "gd-t" + strconv.Itoa(i)
		deps[prev] = []beadsapi.Dependency{blocksDep(prev, id)}
		beads[id] = bead(id, "task", "open", "")
		prev = id
	}
	// terminal escalation past the horizon
	deps[prev] = []beadsapi.Dependency{blocksDep(prev, "gd-esc")}
	beads["gd-esc"] = bead("gd-esc", "escalation", "open", "")

	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	if evalTransitive(t, rows, deps, beads) {
		t.Fatal("expected loop_end NOT met: a chain past the depth cap is unprovable → fail-safe keeps the child counted")
	}
}

// TestExcludeBlocked_MissingIntermediateDetailFailSafe ensures that if an
// intermediate blocker's detail cannot be fetched, its dependent is treated as
// unprovable and stays counted.
func TestExcludeBlocked_MissingIntermediateDetailFailSafe(t *testing.T) {
	const (
		child = "gd-child"
		mid   = "gd-mid"
	)
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	deps := map[string][]beadsapi.Dependency{
		child: {blocksDep(child, mid)},
	}
	// mid deliberately absent from beadsByID → fetch miss.
	if evalTransitive(t, rows, deps, nil) {
		t.Fatal("expected loop_end NOT met: an unresolvable intermediate blocker must keep the child counted")
	}
}

// TestExcludeBlocked_RealisticMultiPhaseEpic reproduces the whole ka-0000000104
// frontier: 6 phase tasks all blocked solely by one intermediate task that is
// itself blocked by an open escalation, one operator-assigned policy leaf, and
// one intermediate task — every child must resolve so the effective count hits 0.
func TestExcludeBlocked_RealisticMultiPhaseEpic(t *testing.T) {
	const (
		mid = "gd-E1"  // v1.1-E1 intermediate task, blocked by esc
		esc = "gd-t7"  // open escalation on the front
		pol = "gd-pol" // operator-owned policy leaf (human assignee, no deps)
	)
	rows := []*beadsapi.BeadDetail{
		bead(mid, "task", "open", ""),
		bead(pol, "task", "open", "Matthew.Baker"),
	}
	deps := map[string][]beadsapi.Dependency{
		mid: {blocksDep(mid, esc)},
		pol: nil,
	}
	beads := map[string]*beadsapi.BeadDetail{
		esc: bead(esc, "escalation", "open", ""),
	}
	// 6 phase-F tasks, each blocked solely by mid.
	for i := 1; i <= 6; i++ {
		id := "gd-F" + strconv.Itoa(i)
		rows = append(rows, bead(id, "task", "open", ""))
		deps[id] = []beadsapi.Dependency{blocksDep(id, mid)}
	}
	// 8 children total, all non-actionable → effective count 0 → loop_end fires.
	if !evalTransitive(t, rows, deps, beads) {
		t.Fatal("expected loop_end MET: every child transitively operator-blocked or operator-assigned")
	}
}
