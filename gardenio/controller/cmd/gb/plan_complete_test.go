package main

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// restorePlanLifecycleGlobals snapshots the completion-verb command globals so a
// test's overrides never leak into another test.
func restorePlanLifecycleGlobals(t *testing.T) {
	t.Helper()
	prevOp, prevJSON := operatedBy, jsonOutput
	t.Cleanup(func() {
		operatedBy, jsonOutput = prevOp, prevJSON
	})
}

// TestRunPlanComplete_EndToEnd drives runPlanComplete through its full RunE path:
// operator validation + the daemon-direct completePlan helper — asserting the fake
// writer closed the plan epic and stamped plan_state=complete.
func TestRunPlanComplete_EndToEnd(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-EpicXYZ", Type: "epic", Status: "open",
		Labels: []string{"plan"}, Fields: map[string]string{"plan_state": "building"}})
	withFakePlanWriter(t, f)

	restorePlanLifecycleGlobals(t)
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	cmd := planCompleteCmd
	cmd.SetContext(context.Background())
	if err := runPlanComplete(cmd, []string{"gd-EpicXYZ"}); err != nil {
		t.Fatalf("runPlanComplete: %v", err)
	}
	if len(f.opsOfKind("close")) != 1 {
		t.Errorf("want a single close of the plan epic, got %d", len(f.opsOfKind("close")))
	}
	if f.beads["gd-EpicXYZ"].Fields["plan_state"] != "complete" {
		t.Errorf("plan_state = %q, want complete", f.beads["gd-EpicXYZ"].Fields["plan_state"])
	}
	if f.beads["gd-EpicXYZ"].Status != "closed" {
		t.Errorf("status = %q, want closed", f.beads["gd-EpicXYZ"].Status)
	}
}

// TestRunPlanReopen_EndToEnd drives runPlanReopen through its full RunE path: the
// daemon-direct reopenPlan helper reopens the epic and restores plan_state=building.
func TestRunPlanReopen_EndToEnd(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-EpicXYZ", Type: "epic", Status: "closed",
		Labels: []string{"plan"}, Fields: map[string]string{"plan_state": "complete"}})
	withFakePlanWriter(t, f)

	restorePlanLifecycleGlobals(t)
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	cmd := planReopenCmd
	cmd.SetContext(context.Background())
	if err := runPlanReopen(cmd, []string{"gd-EpicXYZ"}); err != nil {
		t.Fatalf("runPlanReopen: %v", err)
	}
	if f.beads["gd-EpicXYZ"].Status != "open" {
		t.Errorf("status = %q, want open", f.beads["gd-EpicXYZ"].Status)
	}
	if f.beads["gd-EpicXYZ"].Fields["plan_state"] != "building" {
		t.Errorf("plan_state = %q, want building", f.beads["gd-EpicXYZ"].Fields["plan_state"])
	}
}

// TestRunPlanComplete_JSONOutput covers the jsonOutput=true branch (printJSON).
func TestRunPlanComplete_JSONOutput(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-EpicXYZ", Type: "epic", Status: "open",
		Labels: []string{"plan"}, Fields: map[string]string{"plan_state": "building"}})
	withFakePlanWriter(t, f)

	restorePlanLifecycleGlobals(t)
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = true

	cmd := planCompleteCmd
	cmd.SetContext(context.Background())
	if err := runPlanComplete(cmd, []string{"gd-EpicXYZ"}); err != nil {
		t.Fatalf("runPlanComplete: %v", err)
	}
	if len(f.opsOfKind("close")) != 1 {
		t.Errorf("want a single close in the JSON-output path, got %d", len(f.opsOfKind("close")))
	}
}

// TestRunPlanComplete_HelperErrorSurfaces proves a daemon-layer failure (a
// missing plan epic) is surfaced by the command, not swallowed.
func TestRunPlanComplete_HelperErrorSurfaces(t *testing.T) {
	f := newFakePlanWriter() // no plan epic seeded → loadPlanEpic fails
	withFakePlanWriter(t, f)

	restorePlanLifecycleGlobals(t)
	operatedBy = "oidc:matt@acme.example"

	cmd := planCompleteCmd
	cmd.SetContext(context.Background())
	if err := runPlanComplete(cmd, []string{"gd-MISSING"}); err == nil {
		t.Fatal("want error when the plan epic does not exist")
	}
}

// TestRunPlanComplete_NonHumanOperatorRejected: the verb requires a HUMAN
// operator (oidc:<email> or slack:U…); a svc: machine id is rejected.
func TestRunPlanComplete_NonHumanOperatorRejected(t *testing.T) {
	restorePlanLifecycleGlobals(t)
	operatedBy = "svc:steering-gardener"

	cmd := planCompleteCmd
	cmd.SetContext(context.Background())
	if err := runPlanComplete(cmd, []string{"gd-P"}); err == nil {
		t.Fatal("want error when operated-by is not a human (oidc:<email> or slack:U…) id")
	}
}

// TestRunPlanReopen_NonHumanOperatorRejected mirrors the complete guard for reopen.
func TestRunPlanReopen_NonHumanOperatorRejected(t *testing.T) {
	restorePlanLifecycleGlobals(t)
	operatedBy = "svc:steering-gardener"

	cmd := planReopenCmd
	cmd.SetContext(context.Background())
	if err := runPlanReopen(cmd, []string{"gd-P"}); err == nil {
		t.Fatal("want error when operated-by is not a human (oidc:<email> or slack:U…) id")
	}
}
