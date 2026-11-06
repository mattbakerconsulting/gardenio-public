package main

import (
	"context"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// restorePlanPhaseGlobals snapshots the phase-command globals so a test's
// overrides never leak into another test in the package (mirrors
// restorePlanGlobals for the phase verbs).
func restorePlanPhaseGlobals(t *testing.T) {
	t.Helper()
	pt, rd, op, js := planAddPhaseTitle, planReorderPhaseDir, operatedBy, jsonOutput
	t.Cleanup(func() {
		planAddPhaseTitle, planReorderPhaseDir, operatedBy, jsonOutput = pt, rd, op, js
	})
}

// TestRunPlanAddPhase_EndToEnd drives runPlanAddPhase through its full RunE path:
// operator validation + the daemon-direct addPlanPhase helper — asserting the fake
// writer minted a phase epic, linked it parent-child, and grew phase_order via the
// atomic append primitive.
func TestRunPlanAddPhase_EndToEnd(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PlanXYZ", Type: "epic", Status: "open", Labels: []string{"plan", "project:gardenio"}})
	withFakePlanWriter(t, f)

	restorePlanPhaseGlobals(t)
	planAddPhaseTitle = "Validation"
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	cmd := planAddPhaseCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddPhase(cmd, []string{"gd-PlanXYZ"}); err != nil {
		t.Fatalf("runPlanAddPhase: %v", err)
	}

	create := f.opsOfKind("create")
	if len(create) != 1 || create[0].req.Type != "epic" || create[0].req.Title != "Validation" {
		t.Fatalf("create = %+v, want a single phase epic titled 'Validation'", create)
	}
	phaseID := create[0].bead
	if create[0].req.ParentBeadID != "gd-PlanXYZ" {
		t.Errorf("create ParentBeadID = %q, want gd-PlanXYZ", create[0].req.ParentBeadID)
	}
	if dep := f.opsOfKind("dep"); len(dep) != 0 {
		t.Errorf("dep = %+v, want none (parent-child wired by CreateBead.ParentBeadID)", dep)
	}
	app := f.opsOfKind("append")
	if len(app) != 1 || app[0].bead != "gd-PlanXYZ" || app[0].field != planPhaseFieldKey || app[0].element != phaseID {
		t.Errorf("append = %+v, want phase_order += %s on gd-PlanXYZ", app, phaseID)
	}
	if f.lastOperator != "oidc:matt@acme.example" {
		t.Errorf("operator = %q, want oidc:matt@acme.example", f.lastOperator)
	}
}

func TestRunPlanAddPhase_MissingTitle(t *testing.T) {
	restorePlanPhaseGlobals(t)
	planAddPhaseTitle = ""
	operatedBy = "oidc:matt@acme.example"

	cmd := planAddPhaseCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddPhase(cmd, []string{"gd-P"}); err == nil {
		t.Fatal("want error when --title is empty")
	}
}

func TestRunPlanAddPhase_NonHumanOperatorRejected(t *testing.T) {
	restorePlanPhaseGlobals(t)
	planAddPhaseTitle = "t"
	operatedBy = "svc:steering-gardener"

	cmd := planAddPhaseCmd
	cmd.SetContext(context.Background())
	if err := runPlanAddPhase(cmd, []string{"gd-P"}); err == nil {
		t.Fatal("want error when operated-by is not a human (oidc:<email> or slack:U…) id")
	}
}

// TestRunPlanReorderPhase_EndToEnd drives runPlanReorderPhase through its full
// RunE path: the daemon-direct reorderPlanPhase helper permutes phase_order via
// the CAS primitive, and the command reports the landed order.
func TestRunPlanReorderPhase_EndToEnd(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PlanXYZ", Type: "epic", Status: "open",
		Fields: map[string]string{"phase_order": `["gd-PhaseA","gd-PhaseB"]`}})
	f.add(&beadsapi.BeadDetail{ID: "gd-PhaseA", Type: "epic", Status: "open"})
	f.add(&beadsapi.BeadDetail{ID: "gd-PhaseB", Type: "epic", Status: "open"})
	f.children["gd-PlanXYZ"] = []string{"gd-PhaseA", "gd-PhaseB"}
	withFakePlanWriter(t, f)

	restorePlanPhaseGlobals(t)
	planReorderPhaseDir = "down"
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	cmd := planReorderPhaseCmd
	cmd.SetContext(context.Background())
	if err := runPlanReorderPhase(cmd, []string{"gd-PlanXYZ", "gd-PhaseA"}); err != nil {
		t.Fatalf("runPlanReorderPhase: %v", err)
	}
	reorders := f.opsOfKind("reorder")
	if len(reorders) != 1 {
		t.Fatalf("want 1 reorder, got %d", len(reorders))
	}
	if !reflect.DeepEqual(reorders[0].next, []string{"gd-PhaseB", "gd-PhaseA"}) {
		t.Errorf("CAS next = %v, want [gd-PhaseB gd-PhaseA]", reorders[0].next)
	}
}

// TestRunPlanReorderPhase_BoundaryNoop drives the boundary no-op path: moving the
// first phase up writes nothing and the command reports it as a no-op.
func TestRunPlanReorderPhase_BoundaryNoop(t *testing.T) {
	f := newFakePlanWriter()
	f.add(&beadsapi.BeadDetail{ID: "gd-PlanXYZ", Type: "epic", Status: "open",
		Fields: map[string]string{"phase_order": `["gd-PhaseA","gd-PhaseB"]`}})
	f.add(&beadsapi.BeadDetail{ID: "gd-PhaseA", Type: "epic", Status: "open"})
	f.add(&beadsapi.BeadDetail{ID: "gd-PhaseB", Type: "epic", Status: "open"})
	f.children["gd-PlanXYZ"] = []string{"gd-PhaseA", "gd-PhaseB"}
	withFakePlanWriter(t, f)

	restorePlanPhaseGlobals(t)
	planReorderPhaseDir = "up"
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	cmd := planReorderPhaseCmd
	cmd.SetContext(context.Background())
	if err := runPlanReorderPhase(cmd, []string{"gd-PlanXYZ", "gd-PhaseA"}); err != nil {
		t.Fatalf("runPlanReorderPhase(noop): %v", err)
	}
	if len(f.opsOfKind("reorder")) != 0 {
		t.Error("a boundary no-op must not write")
	}
}

func TestRunPlanReorderPhase_BadDir(t *testing.T) {
	restorePlanPhaseGlobals(t)
	planReorderPhaseDir = "sideways"
	operatedBy = "oidc:matt@acme.example"

	cmd := planReorderPhaseCmd
	cmd.SetContext(context.Background())
	if err := runPlanReorderPhase(cmd, []string{"gd-P", "gd-Ph"}); err == nil {
		t.Fatal("want error when --dir is not up or down")
	}
}

func TestRunPlanReorderPhase_NonHumanOperatorRejected(t *testing.T) {
	restorePlanPhaseGlobals(t)
	planReorderPhaseDir = "up"
	operatedBy = "svc:steering-gardener"

	cmd := planReorderPhaseCmd
	cmd.SetContext(context.Background())
	if err := runPlanReorderPhase(cmd, []string{"gd-P", "gd-Ph"}); err == nil {
		t.Fatal("want error when operated-by is not a human (oidc:<email> or slack:U…) id")
	}
}
