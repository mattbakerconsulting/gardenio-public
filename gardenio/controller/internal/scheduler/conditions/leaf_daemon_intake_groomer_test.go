package conditions

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// leaf_daemon_intake_groomer_test.go pins the exact PROD intake-groomer
// pre-flight (trigger ka-0000006210) against the three named needs-intake beads
// from bug ka-0000006656, so the "lane goes dark" regression stays caught.
//
// THE BUG (ka-0000006656): the live condition opted task+bug into
// exclude_blocked_by_open_types (escalation ka-0000002943) to suppress
// needs-intake beads blocked on open task/bug work. subtractExternallyBlocked's
// old resolvesToOperator terminal-accepted ANY open bead of a wanted type — and
// the outer loop reused it to decide the MATCHED CHILDREN. A matched child is
// always type task/bug/feature/epic, so with task+bug in the set every matched
// task/bug child self-accepted on the first line of the recursion, BEFORE its
// blockers were inspected. The effective count floored to 0, the ">0" gate went
// false, and the lane sat dark for two days with 122 stranded beads.
//
// THE FIX splits the matched-child decision (childIsNonActionable) from the
// blocker-descent (blockerResolvesToOperator): a child is judged ONLY by its
// blockers/assignee/type, never accepted for its own type. These tests pin the
// intended count for the exact live condition + beads so it stays fixed.

// intakeGroomerCondition is the byte-faithful pre-flight from prod trigger
// ka-0000006210.fields.conditions[0] (captured 2026-07-31). It fires (">0")
// while the gardenio needs-intake lane has at least one groomable, genuinely
// actionable work bead.
var intakeGroomerCondition = Leaf{
	Type:                      "bead_query",
	Op:                        ">",
	Value:                     "0",
	Project:                   "gardenio",
	Labels:                    []string{"needs-intake"},
	Types:                     []string{"task", "bug", "feature", "epic"},
	Statuses:                  []string{"open"},
	ExcludeLabels:             []string{"source:multi", "source:schedule", "thread", "source:slack", "slack-thread"},
	ExcludeBlockedByOpenTypes: []string{"escalation", "decision", "task", "bug"},
}

// The three needs-intake beads named in ka-0000006656, with the deps the daemon
// actually returns for them (verified via the daemon JSON, not the CLI
// projection). All three are genuinely actionable: each is an open, unassigned
// leaf whose only deps are NON-blocking (related / parent-child), so none is
// blocked_by an open escalation/decision/task/bug and none must be subtracted.
const (
	igBugRelated  = "gd-CabIbtunSQ" // bug, unassigned, one `related` dep → CLOSED bead
	igBugNoDeps   = "ka-0000005502" // bug, unassigned, no deps at all
	igTaskPCChild = "ka-0000003411" // task, unassigned, one parent-child dep (non-blocking)
)

// relatedDep / parentChildDep build the exact NON-blocking edge shapes the three
// beads carry. Neither type is in blockingDepTypes, so neither is a wait edge.
func relatedDep(child, other string) beadsapi.Dependency {
	return beadsapi.Dependency{BeadID: child, DependsOnID: other, Type: "related"}
}

func parentChildDep(child, parent string) beadsapi.Dependency {
	return beadsapi.Dependency{BeadID: child, DependsOnID: parent, Type: "parent-child"}
}

// intakeGroomerRows / intakeGroomerDeps model exactly the three beads and their
// live deps. The `related`/`parent-child` targets are given CLOSED/OPEN detail
// only so a (buggy) walk that mis-treated them as blocking would still be
// exercised — the correct code never inspects them because their edge type is
// not blocking.
func intakeGroomerFixture() (rows []*beadsapi.BeadDetail, deps map[string][]beadsapi.Dependency, beads map[string]*beadsapi.BeadDetail) {
	rows = []*beadsapi.BeadDetail{
		bead(igBugRelated, "bug", "open", ""),
		bead(igBugNoDeps, "bug", "open", ""),
		bead(igTaskPCChild, "task", "open", ""),
	}
	deps = map[string][]beadsapi.Dependency{
		igBugRelated:  {relatedDep(igBugRelated, "gd-AN27Ilha0B")},
		igBugNoDeps:   nil,
		igTaskPCChild: {parentChildDep(igTaskPCChild, "gd-qiwIKN9K8T")},
	}
	beads = map[string]*beadsapi.BeadDetail{
		"gd-AN27Ilha0B": bead("gd-AN27Ilha0B", "bug", "closed", ""),
		"gd-qiwIKN9K8T": bead("gd-qiwIKN9K8T", "epic", "open", ""),
	}
	return rows, deps, beads
}

func evalIntakeGroomer(t *testing.T, leaf Leaf, rows []*beadsapi.BeadDetail, deps map[string][]beadsapi.Dependency, beads map[string]*beadsapi.BeadDetail) bool {
	t.Helper()
	bc := &stubBlockedResolver{rows: rows, depsByChild: deps, beadsByID: beads}
	got, err := Evaluate(context.Background(), Expression{Leaf: leaf}, Env{Bead: bc})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return got
}

// TestIntakeGroomer_ThreeActionableBeadsKeepLaneLive is the headline regression:
// the exact prod condition over the three named beads MUST fire (">0"), because
// all three are genuinely actionable. Before the fix every matched task/bug
// child self-subtracted and the gate returned false — the two-day dark lane.
func TestIntakeGroomer_ThreeActionableBeadsKeepLaneLive(t *testing.T) {
	rows, deps, beads := intakeGroomerFixture()
	if !evalIntakeGroomer(t, intakeGroomerCondition, rows, deps, beads) {
		t.Fatal("expected intake-groomer pre-flight MET (>0): three unblocked needs-intake beads are actionable; " +
			"a false here is the ka-0000006656 dark lane — matched task/bug children were self-subtracted")
	}
}

// TestIntakeGroomer_EffectiveCountIsThree pins the exact effective count, not
// just the boolean, so a future change that subtracts one of the three (or
// stops subtracting a genuinely-blocked bead) is caught precisely. We probe the
// count by sweeping the ">N" threshold: >2 must hold (count >= 3) and >3 must
// not (count is exactly 3, no phantom rows).
func TestIntakeGroomer_EffectiveCountIsThree(t *testing.T) {
	rows, deps, beads := intakeGroomerFixture()

	gtTwo := intakeGroomerCondition
	gtTwo.Value = "2"
	if !evalIntakeGroomer(t, gtTwo, rows, deps, beads) {
		t.Fatal("expected >2 MET: three actionable needs-intake beads must survive the exclusion")
	}

	gtThree := intakeGroomerCondition
	gtThree.Value = "3"
	if evalIntakeGroomer(t, gtThree, rows, deps, beads) {
		t.Fatal("expected >3 NOT met: the effective count is exactly 3, no phantom actionable rows")
	}
}

// TestIntakeGroomer_GenuinelyBlockedBeadIsStillSubtracted proves the fix does
// NOT regress escalation ka-0000002943's intent: a needs-intake bead that IS
// blocked_by an open task/bug is still subtracted. We add a fourth bead blocked
// by an open task; the effective count must stay 3 (the fourth is excluded),
// while the three actionable beads remain counted.
func TestIntakeGroomer_GenuinelyBlockedBeadIsStillSubtracted(t *testing.T) {
	const (
		blocked     = "gd-blocked4"
		openBlocker = "gd-openTask"
	)
	rows, deps, beads := intakeGroomerFixture()
	rows = append(rows, bead(blocked, "bug", "open", ""))
	deps[blocked] = []beadsapi.Dependency{blocksDep(blocked, openBlocker)}
	beads[openBlocker] = bead(openBlocker, "task", "open", "")

	// >3 must NOT hold: 4 matched, the blocked one subtracted → effective 3.
	gtThree := intakeGroomerCondition
	gtThree.Value = "3"
	if evalIntakeGroomer(t, gtThree, rows, deps, beads) {
		t.Fatal("expected >3 NOT met: the fourth bead is blocked_by an open task and must be subtracted (ka-0000002943 intent)")
	}
	// >2 still holds: the three actionable beads are never subtracted.
	gtTwo := intakeGroomerCondition
	gtTwo.Value = "2"
	if !evalIntakeGroomer(t, gtTwo, rows, deps, beads) {
		t.Fatal("expected >2 MET: the three actionable beads must survive even alongside a genuinely-blocked one")
	}
}

// TestIntakeGroomer_LaneGenuinelyEmptyGoesDormant confirms the DUAL of the bug:
// when every matched bead really is blocked_by an open task/bug, the lane SHOULD
// go dormant (>0 false). This is the legitimate dormancy the escalation asked
// for — the fix must preserve it, not flip it into always-awake.
func TestIntakeGroomer_LaneGenuinelyEmptyGoesDormant(t *testing.T) {
	const openBlocker = "gd-openBlockerT"
	// All three beads blocked_by the same open task → all subtracted → count 0.
	rows := []*beadsapi.BeadDetail{
		bead(igBugRelated, "bug", "open", ""),
		bead(igBugNoDeps, "bug", "open", ""),
		bead(igTaskPCChild, "task", "open", ""),
	}
	deps := map[string][]beadsapi.Dependency{
		igBugRelated:  {blocksDep(igBugRelated, openBlocker)},
		igBugNoDeps:   {blocksDep(igBugNoDeps, openBlocker)},
		igTaskPCChild: {blocksDep(igTaskPCChild, openBlocker)},
	}
	beads := map[string]*beadsapi.BeadDetail{
		openBlocker: bead(openBlocker, "task", "open", ""),
	}
	if evalIntakeGroomer(t, intakeGroomerCondition, rows, deps, beads) {
		t.Fatal("expected >0 NOT met: every bead blocked_by an open task is legitimately non-actionable → dormant")
	}
}

// TestIntakeGroomer_ChildOfWantedTypeNotSelfAccepted is the tightest unit pin on
// the fix: a single matched task child with NO deps at all, under a wanted set
// that includes `task`, must remain COUNTED. This is the exact self-acceptance
// that the old code got wrong — the child's own type must never trip the
// terminal accept.
func TestIntakeGroomer_ChildOfWantedTypeNotSelfAccepted(t *testing.T) {
	const child = "gd-loneTask"
	leaf := intakeGroomerCondition
	rows := []*beadsapi.BeadDetail{bead(child, "task", "open", "")}
	deps := map[string][]beadsapi.Dependency{child: nil}
	if !evalIntakeGroomer(t, leaf, rows, deps, nil) {
		t.Fatal("expected >0 MET: an unblocked matched task child must NOT self-subtract merely because `task` is in exclude_blocked_by_open_types")
	}
}

// TestIntakeGroomer_TransitiveTaskBlockStillSubtracts confirms the transitive
// walk still works with task/bug in the wanted set: a needs-intake bead blocked
// by an open task that is ITSELF blocked by an open task is non-actionable and
// subtracted (the escalation's whole point — deep task/bug gating).
func TestIntakeGroomer_TransitiveTaskBlockStillSubtracts(t *testing.T) {
	const (
		child = "gd-childT"
		mid   = "gd-midTask"
		deep  = "gd-deepTask"
	)
	leaf := intakeGroomerCondition
	rows := []*beadsapi.BeadDetail{bead(child, "bug", "open", "")}
	deps := map[string][]beadsapi.Dependency{
		child: {blocksDep(child, mid)},
		mid:   {blocksDep(mid, deep)},
	}
	beads := map[string]*beadsapi.BeadDetail{
		mid:  bead(mid, "task", "open", ""),
		deep: bead(deep, "task", "open", ""),
	}
	// child → mid(open task) → deep(open task, leaf). deep is an open wanted-type
	// blocker (terminal accept as a BLOCKER, legitimate), so mid resolves, so
	// child is subtracted → count 0 → >0 false.
	if evalIntakeGroomer(t, leaf, rows, deps, beads) {
		t.Fatal("expected >0 NOT met: child transitively blocked child→task→task must be subtracted (ka-0000002943 deep-gating intent)")
	}
}
