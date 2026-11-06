package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestInstantiateFormulaSteps_TriggerBeadDep asserts that when a non-empty
// triggerBeadID is supplied, the molecule receives a `triggered-by` dep
// pointing at the trigger source. This is the D3 lineage edge that records
// `/formula pour` provenance back to the originating Slack thread bead.
func TestInstantiateFormulaSteps_TriggerBeadDep(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{ID: "gd-formula-trig", Title: "Triggered formula"}
	steps := []formulaStep{{ID: "s1", Title: "Step", Type: "task"}}

	molID, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "gardenio", "gd-thread-bead")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	deps := daemon.getDeps()
	var found *depCall
	for i := range deps {
		if deps[i].DepType == "triggered-by" {
			found = &deps[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a triggered-by dep, got %+v", deps)
	}
	if found.BeadID != molID {
		t.Errorf("triggered-by dep should originate at molecule %q, got %q", molID, found.BeadID)
	}
	if found.DependsOnID != "gd-thread-bead" {
		t.Errorf("triggered-by dep should point at thread bead, got %q", found.DependsOnID)
	}
	if found.CreatedBy != "slack-bridge" {
		t.Errorf("triggered-by dep should be created by slack-bridge, got %q", found.CreatedBy)
	}
}

// TestInstantiateFormulaSteps_NoTriggerBeadDep asserts that no triggered-by
// dep is written when the caller does not provide a trigger source — pours
// from non-thread channels must not synthesize lineage edges.
func TestInstantiateFormulaSteps_NoTriggerBeadDep(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	formula := &beadsapi.BeadDetail{ID: "gd-formula-notrig", Title: "No trigger"}
	steps := []formulaStep{{ID: "s1", Title: "Step", Type: "task"}}

	_, _, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "gardenio", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for _, d := range daemon.getDeps() {
		if d.DepType == "triggered-by" {
			t.Errorf("did not expect any triggered-by dep, got %+v", d)
		}
	}
}

// TestInstantiateFormulaSteps_ChainWalkToTriggerBead pins the M2e contract
// (gd-ToivBURyeG / epic ka-0000000933): for a multi-step formula pour kicked
// off by an incoming bead, the molecule plus every child step bead must trace
// back to the originating trigger bead via the `triggered-by` (molecule) +
// `parent-child` (steps) chain, with the design-required invariant that
// steps do NOT carry their own direct `triggered-by` edge (no double-parent).
//
// This is the "chain walk" e2e plan §6.1 calls out and is the value-add on
// top of the existing single-step TriggerBeadDep test — it verifies the
// transitive lineage path holds for multi-step formulas and that step
// beads do not duplicate the molecule's trigger edge.
func TestInstantiateFormulaSteps_ChainWalkToTriggerBead(t *testing.T) {
	daemon := newFormulaMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.daemon = daemon

	const triggerBeadID = "gd-thread-bead-chain"
	formula := &beadsapi.BeadDetail{ID: "gd-formula-chain", Title: "Multi-step chain formula"}
	steps := []formulaStep{
		{ID: "build", Title: "Build", Type: "task"},
		{ID: "test", Title: "Test", Type: "task"},
		{ID: "deploy", Title: "Deploy", Type: "task"},
	}

	molID, nSteps, err := bot.instantiateFormulaSteps(
		context.Background(), formula, steps, nil, false, "gardenio", triggerBeadID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nSteps != len(steps) {
		t.Fatalf("expected %d steps created, got %d", len(steps), nSteps)
	}

	deps := daemon.getDeps()

	// Invariant 1: exactly one triggered-by dep, from molecule to trigger bead.
	var triggeredByDeps []depCall
	for _, d := range deps {
		if d.DepType == "triggered-by" {
			triggeredByDeps = append(triggeredByDeps, d)
		}
	}
	if len(triggeredByDeps) != 1 {
		t.Fatalf("expected exactly 1 triggered-by dep (molecule → trigger), got %d: %+v",
			len(triggeredByDeps), triggeredByDeps)
	}
	if triggeredByDeps[0].BeadID != molID || triggeredByDeps[0].DependsOnID != triggerBeadID {
		t.Errorf("triggered-by edge = %+v, want %s -> %s", triggeredByDeps[0], molID, triggerBeadID)
	}

	// Invariant 2: every step has a parent-child dep pointing at the molecule.
	stepParentChildCount := 0
	for _, d := range deps {
		if d.DepType != "parent-child" {
			continue
		}
		if d.DependsOnID != molID {
			t.Errorf("parent-child dep should point at molecule %s, got %+v", molID, d)
			continue
		}
		if d.BeadID == molID {
			t.Errorf("parent-child dep should originate at step bead, not molecule: %+v", d)
			continue
		}
		stepParentChildCount++
	}
	if stepParentChildCount != len(steps) {
		t.Errorf("expected %d step→molecule parent-child deps, got %d (deps=%+v)",
			len(steps), stepParentChildCount, deps)
	}

	// Invariant 3: no step bead has its own direct triggered-by edge to the
	// trigger bead — lineage is transitive via molecule, not duplicated.
	// (The only triggered-by dep is the one already validated in Invariant 1.)
	for _, d := range deps {
		if d.DepType != "triggered-by" {
			continue
		}
		if d.BeadID != molID {
			t.Errorf("expected no step-level triggered-by dep; found %+v (step beads must rely on parent-child→molecule)", d)
		}
	}
}
