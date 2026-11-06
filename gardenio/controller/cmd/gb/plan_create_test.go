package main

import (
	"context"
	"reflect"
	"testing"
)

// restorePlanCreateGlobals snapshots the create-command globals so a test's
// overrides never leak into another test in the package.
func restorePlanCreateGlobals(t *testing.T) {
	t.Helper()
	prevGoal, prevProj, prevOp, prevJSON := planCreateGoal, planCreateProject, operatedBy, jsonOutput
	t.Cleanup(func() {
		planCreateGoal, planCreateProject, operatedBy, jsonOutput = prevGoal, prevProj, prevOp, prevJSON
	})
}

// TestRunPlanCreate_EndToEnd drives runPlanCreate through its full RunE path:
// operator validation + the daemon-direct createPlanEpicDaemon helper — asserting
// the fake writer minted a plan epic with the born-on labels + plan_state=draft,
// stamped with the operator. Guards the command wiring, not just the helper.
func TestRunPlanCreate_EndToEnd(t *testing.T) {
	f := newFakePlanWriter()
	withFakePlanWriter(t, f)

	restorePlanCreateGlobals(t)
	planCreateGoal = "Wire the mirror"
	planCreateProject = "gardenio"
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = false

	cmd := planCreateCmd
	cmd.SetContext(context.Background())
	if err := runPlanCreate(cmd, nil); err != nil {
		t.Fatalf("runPlanCreate: %v", err)
	}

	create := f.opsOfKind("create")
	if len(create) != 1 {
		t.Fatalf("want a single plan epic create, got %d", len(create))
	}
	req := create[0].req
	if req.Type != "epic" {
		t.Errorf("create type = %q, want epic", req.Type)
	}
	if !reflect.DeepEqual(req.Labels, []string{planLabel, "project:gardenio"}) {
		t.Errorf("labels = %v, want [plan project:gardenio]", req.Labels)
	}
	if req.Description != "Wire the mirror" {
		t.Errorf("description = %q, want the verbatim goal", req.Description)
	}
	if create[0].operator != "oidc:matt@acme.example" {
		t.Errorf("create operator = %q, want oidc:matt@acme.example", create[0].operator)
	}
	if req.ShortTitle != "plan: Wire the mirror" {
		t.Errorf("short title = %q, want %q", req.ShortTitle, "plan: Wire the mirror")
	}
}

// TestRunPlanCreate_JSONOutput covers the jsonOutput=true branch (printJSON).
func TestRunPlanCreate_JSONOutput(t *testing.T) {
	f := newFakePlanWriter()
	withFakePlanWriter(t, f)

	restorePlanCreateGlobals(t)
	planCreateGoal = "Ship the widget"
	planCreateProject = "gardenio"
	operatedBy = "oidc:matt@acme.example"
	jsonOutput = true

	cmd := planCreateCmd
	cmd.SetContext(context.Background())
	if err := runPlanCreate(cmd, nil); err != nil {
		t.Fatalf("runPlanCreate: %v", err)
	}
	if len(f.opsOfKind("create")) != 1 {
		t.Errorf("want a single plan epic create in the JSON-output path, got %d", len(f.opsOfKind("create")))
	}
}

func TestRunPlanCreate_MissingGoal(t *testing.T) {
	restorePlanCreateGlobals(t)
	planCreateGoal = ""
	planCreateProject = "gardenio"
	operatedBy = "oidc:matt@acme.example"

	cmd := planCreateCmd
	cmd.SetContext(context.Background())
	if err := runPlanCreate(cmd, nil); err == nil {
		t.Fatal("want error when --goal is empty")
	}
}

func TestRunPlanCreate_MissingProject(t *testing.T) {
	restorePlanCreateGlobals(t)
	planCreateGoal = "a goal"
	planCreateProject = ""
	operatedBy = "oidc:matt@acme.example"

	cmd := planCreateCmd
	cmd.SetContext(context.Background())
	if err := runPlanCreate(cmd, nil); err == nil {
		t.Fatal("want error when --project is empty")
	}
}

func TestRunPlanCreate_NonHumanOperatorRejected(t *testing.T) {
	restorePlanCreateGlobals(t)
	planCreateGoal = "a goal"
	planCreateProject = "gardenio"
	operatedBy = "svc:steering-gardener"

	cmd := planCreateCmd
	cmd.SetContext(context.Background())
	if err := runPlanCreate(cmd, nil); err == nil {
		t.Fatal("want error when operated-by is not a human (oidc:<email> or slack:U…) id")
	}
}
