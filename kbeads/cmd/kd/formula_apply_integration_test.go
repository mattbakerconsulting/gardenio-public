package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// makeFormulaFields builds JSON fields for a formula bead with the given vars and steps.
func makeFormulaFields(vars []FormulaVarDef, steps []FormulaStep) json.RawMessage {
	data, _ := json.Marshal(struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{Vars: vars, Steps: steps})
	return data
}

// makeFormulaFieldsWithAgent builds JSON fields for a formula bead including an assigned agent.
func makeFormulaFieldsWithAgent(vars []FormulaVarDef, steps []FormulaStep, agent string) json.RawMessage {
	data, _ := json.Marshal(struct {
		Vars          []FormulaVarDef `json:"vars"`
		Steps         []FormulaStep   `json:"steps"`
		AssignedAgent string          `json:"assigned_agent,omitempty"`
	}{Vars: vars, Steps: steps, AssignedAgent: agent})
	return data
}

// makeFormulaFieldsWithRoles builds JSON fields for a formula bead including default_roles.
func makeFormulaFieldsWithRoles(vars []FormulaVarDef, steps []FormulaStep, defaultRoles []string) json.RawMessage {
	data, _ := json.Marshal(struct {
		Vars         []FormulaVarDef `json:"vars"`
		Steps        []FormulaStep   `json:"steps"`
		DefaultRoles []string        `json:"default_roles,omitempty"`
	}{Vars: vars, Steps: steps, DefaultRoles: defaultRoles})
	return data
}

func TestRunFormulaApply_BasicPour(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1", "gd-step-2"}
	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:          "gd-formula-1",
		Title:       "Setup {{component}}",
		Description: "Create {{component}} service",
		Type:        "formula",
		Priority:    2,
		Fields: makeFormulaFields(
			[]FormulaVarDef{
				{Name: "component", Required: true},
			},
			[]FormulaStep{
				{ID: "s1", Title: "Design {{component}}", Type: "task"},
				{ID: "s2", Title: "Implement {{component}}", Type: "feature", DependsOn: []string{"s1"}},
			},
		),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"component=auth"},
		Labels:    []string{"project:foo"},
		Assignee:  "alice",
	})
	if err != nil {
		t.Fatalf("runFormulaApply: %v", err)
	}

	// Should create 3 beads: 1 molecule + 2 steps.
	if got := len(mc.CreateBeadCalls); got != 3 {
		t.Fatalf("expected 3 CreateBead calls, got %d", got)
	}

	// First call: molecule.
	molReq := mc.CreateBeadCalls[0]
	if molReq.Type != "molecule" {
		t.Errorf("molecule type = %q, want molecule", molReq.Type)
	}
	if molReq.Title != "Setup auth" {
		t.Errorf("molecule title = %q, want 'Setup auth'", molReq.Title)
	}
	if molReq.Description != "Create auth service" {
		t.Errorf("molecule description = %q, want 'Create auth service'", molReq.Description)
	}
	if molReq.Priority != 2 {
		t.Errorf("molecule priority = %d, want 2", molReq.Priority)
	}
	if molReq.Assignee != "alice" {
		t.Errorf("molecule assignee = %q, want alice", molReq.Assignee)
	}

	// Check molecule fields contain formula_id.
	var molFields map[string]any
	if err := json.Unmarshal(molReq.Fields, &molFields); err != nil {
		t.Fatalf("unmarshal molecule fields: %v", err)
	}
	if molFields["formula_id"] != "gd-formula-1" {
		t.Errorf("molecule formula_id = %v, want gd-formula-1", molFields["formula_id"])
	}
	// Pour should not have ephemeral field.
	if _, ok := molFields["ephemeral"]; ok {
		t.Error("pour molecule should not have ephemeral field")
	}

	// Step 1.
	s1Req := mc.CreateBeadCalls[1]
	if s1Req.Title != "Design auth" {
		t.Errorf("step 1 title = %q, want 'Design auth'", s1Req.Title)
	}
	if s1Req.Type != "task" {
		t.Errorf("step 1 type = %q, want task", s1Req.Type)
	}

	// Step 2.
	s2Req := mc.CreateBeadCalls[2]
	if s2Req.Title != "Implement auth" {
		t.Errorf("step 2 title = %q, want 'Implement auth'", s2Req.Title)
	}
	if s2Req.Type != "feature" {
		t.Errorf("step 2 type = %q, want feature", s2Req.Type)
	}

	// Check labels propagated.
	if len(s1Req.Labels) != 1 || s1Req.Labels[0] != "project:foo" {
		t.Errorf("step 1 labels = %v, want [project:foo]", s1Req.Labels)
	}

	// D5 (ka-0000007622): step parent-child edges are now wired by the
	// server via CreateBead.ParentBeadID, not by a separate AddDependency
	// call. Verify each step CreateBead carries the molecule as its parent
	// and that only the blocks edge remains as an explicit AddDependency.
	if s1Req.ParentBeadID != "gd-mol-1" {
		t.Errorf("step 1 ParentBeadID = %q, want gd-mol-1", s1Req.ParentBeadID)
	}
	if s2Req.ParentBeadID != "gd-mol-1" {
		t.Errorf("step 2 ParentBeadID = %q, want gd-mol-1", s2Req.ParentBeadID)
	}

	// Only the blocks dep should remain as an explicit AddDependency call.
	if got := len(mc.AddDepCalls); got != 1 {
		t.Fatalf("expected 1 AddDependency call (blocks only); parent-child now wired by CreateBead.ParentBeadID; got %d", got)
	}
	blk := mc.AddDepCalls[0]
	if blk.BeadID != "gd-step-2" || blk.DependsOnID != "gd-step-1" || blk.Type != "blocks" {
		t.Errorf("dep 0: got %s→%s (%s), want gd-step-2→gd-step-1 (blocks)", blk.BeadID, blk.DependsOnID, blk.Type)
	}
}

func TestRunFormulaApply_Wisp(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-wisp-1", "gd-ws-1"}
	mc.Beads["gd-f-1"] = &model.Bead{
		ID:    "gd-f-1",
		Title: "Quick check",
		Type:  "formula",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Run check", Type: "task"},
		}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-f-1",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("runFormulaApply (wisp): %v", err)
	}

	// Molecule should have ephemeral=true.
	molReq := mc.CreateBeadCalls[0]
	var molFields map[string]any
	if err := json.Unmarshal(molReq.Fields, &molFields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if molFields["ephemeral"] != true {
		t.Errorf("wisp molecule should have ephemeral=true, got %v", molFields["ephemeral"])
	}
}

func TestRunFormulaApply_DryRun(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f-dry"] = &model.Bead{
		ID:    "gd-f-dry",
		Title: "Deploy {{env}}",
		Type:  "formula",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "env", Default: "staging"}},
			[]FormulaStep{
				{ID: "s1", Title: "Build for {{env}}", Type: "task"},
				{ID: "s2", Title: "Deploy to {{env}}", Type: "task", DependsOn: []string{"s1"}},
			},
		),
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-f-dry",
			DryRun:    true,
		})
		if err != nil {
			t.Fatalf("dry-run: %v", err)
		}
	})

	// Should not create any beads.
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("dry-run created %d beads, want 0", len(mc.CreateBeadCalls))
	}

	// Output should mention formula, steps, and variables.
	for _, want := range []string{"Deploy staging", "Build for staging", "Deploy to staging", "molecule", "2 steps"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry-run output missing %q, got:\n%s", want, out)
		}
	}
}

func TestRunFormulaApply_DefaultVars(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m-1", "gd-s-1"}
	mc.Beads["gd-f-dv"] = &model.Bead{
		ID:    "gd-f-dv",
		Title: "{{greeting}} world",
		Type:  "formula",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "greeting", Default: "Hello"}},
			[]FormulaStep{{ID: "s1", Title: "Do {{greeting}}"}},
		),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-dv"})
	if err != nil {
		t.Fatalf("apply with defaults: %v", err)
	}

	molReq := mc.CreateBeadCalls[0]
	if molReq.Title != "Hello world" {
		t.Errorf("title = %q, want 'Hello world'", molReq.Title)
	}
	stepReq := mc.CreateBeadCalls[1]
	if stepReq.Title != "Do Hello" {
		t.Errorf("step title = %q, want 'Do Hello'", stepReq.Title)
	}
}

func TestRunFormulaApply_RequiredVarMissing(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f-req"] = &model.Bead{
		ID:    "gd-f-req",
		Title: "{{x}}",
		Type:  "formula",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "x", Required: true}},
			[]FormulaStep{{ID: "s1", Title: "Do {{x}}"}},
		),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-req"})
	if err == nil {
		t.Fatal("expected error for missing required var")
	}
	if !strings.Contains(err.Error(), "{{x}}") || !strings.Contains(err.Error(), "required") {
		t.Errorf("error = %q, want mention of required var x", err)
	}
}

func TestRunFormulaApply_EnumValidation(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f-enum"] = &model.Bead{
		ID:    "gd-f-enum",
		Title: "Deploy",
		Type:  "formula",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "env", Required: true, Enum: []string{"dev", "staging", "prod"}}},
			[]FormulaStep{{ID: "s1", Title: "Deploy to {{env}}"}},
		),
	}
	withMockClient(t, mc)

	// Valid enum value should work.
	mc.CreateIDs = []string{"gd-m", "gd-s"}
	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-enum", VarPairs: []string{"env=staging"}})
	if err != nil {
		t.Fatalf("valid enum: %v", err)
	}

	// Invalid enum value should fail.
	mc2 := newMockClient()
	mc2.Beads["gd-f-enum"] = mc.Beads["gd-f-enum"]
	withMockClient(t, mc2)
	err = runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-enum", VarPairs: []string{"env=invalid"}})
	if err == nil {
		t.Fatal("expected error for invalid enum value")
	}
	if !strings.Contains(err.Error(), "not in allowed values") {
		t.Errorf("error = %q, want 'not in allowed values'", err)
	}
}

func TestRunFormulaApply_ConditionalSteps(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1", "gd-s3"}
	mc.Beads["gd-f-cond"] = &model.Bead{
		ID:    "gd-f-cond",
		Title: "Workflow",
		Type:  "formula",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "skip_tests"}},
			[]FormulaStep{
				{ID: "s1", Title: "Build"},
				{ID: "s2", Title: "Run tests", Condition: "!{{skip_tests}}", DependsOn: []string{"s1"}},
				{ID: "s3", Title: "Deploy", DependsOn: []string{"s2"}},
			},
		),
	}
	withMockClient(t, mc)

	// With skip_tests=true, step s2 should be skipped and s3's dep on s2 removed.
	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-f-cond",
		VarPairs:  []string{"skip_tests=true"},
	})
	if err != nil {
		t.Fatalf("conditional: %v", err)
	}

	// Only 3 beads: molecule + s1 + s3 (s2 skipped).
	if got := len(mc.CreateBeadCalls); got != 3 {
		t.Fatalf("expected 3 CreateBead calls (s2 skipped), got %d", got)
	}

	// Check that s3 was created (Deploy).
	s3Req := mc.CreateBeadCalls[2]
	if s3Req.Title != "Deploy" {
		t.Errorf("step 3 title = %q, want Deploy", s3Req.Title)
	}

	// s3's blocking dep on s2 should be removed since s2 was skipped.
	// Should have 2 parent-child deps (s1→mol, s3→mol), no blocks (s2 skipped).
	blocksDeps := 0
	for _, d := range mc.AddDepCalls {
		if d.Type == "blocks" {
			blocksDeps++
		}
	}
	if blocksDeps != 0 {
		t.Errorf("expected 0 blocks deps (s2 skipped), got %d", blocksDeps)
	}
}

func TestRunFormulaApply_AllStepsFiltered(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f-allskip"] = &model.Bead{
		ID:    "gd-f-allskip",
		Title: "Noop",
		Type:  "formula",
		Fields: makeFormulaFields(
			nil,
			[]FormulaStep{
				{ID: "s1", Title: "Only if flag", Condition: "{{flag}}"},
			},
		),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-allskip"})
	if err == nil {
		t.Fatal("expected error when all steps filtered")
	}
	if !strings.Contains(err.Error(), "all steps were filtered") {
		t.Errorf("error = %q, want 'all steps were filtered'", err)
	}
}

func TestRunFormulaApply_NotFormula(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-task"] = &model.Bead{
		ID:   "gd-task",
		Type: "task",
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-task"})
	if err == nil {
		t.Fatal("expected error for non-formula bead")
	}
	if !strings.Contains(err.Error(), "not formula") {
		t.Errorf("error = %q, want 'not formula'", err)
	}
}

func TestRunFormulaApply_EmptyFields(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-empty"] = &model.Bead{
		ID:   "gd-empty",
		Type: "formula",
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-empty"})
	if err == nil {
		t.Fatal("expected error for empty formula")
	}
	if !strings.Contains(err.Error(), "no fields") {
		t.Errorf("error = %q, want 'no fields'", err)
	}
}

func TestRunFormulaApply_NoSteps(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-nosteps"] = &model.Bead{
		ID:     "gd-nosteps",
		Type:   "formula",
		Fields: makeFormulaFields(nil, nil),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-nosteps"})
	if err == nil {
		t.Fatal("expected error for formula with no steps")
	}
	if !strings.Contains(err.Error(), "no steps") {
		t.Errorf("error = %q, want 'no steps'", err)
	}
}

func TestRunFormulaApply_InvalidVarPair(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f"] = &model.Bead{
		ID:   "gd-f",
		Type: "formula",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Do thing"},
		}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-f",
		VarPairs:  []string{"badformat"},
	})
	if err == nil {
		t.Fatal("expected error for invalid var pair")
	}
	if !strings.Contains(err.Error(), "expected key=value") {
		t.Errorf("error = %q, want 'expected key=value'", err)
	}
}

func TestRunFormulaApply_StepLabels(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s"}
	mc.Beads["gd-f"] = &model.Bead{
		ID:    "gd-f",
		Type:  "formula",
		Title: "F",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Step", Labels: []string{"step-label"}},
		}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-f",
		Labels:    []string{"parent-label"},
	})
	if err != nil {
		t.Fatal(err)
	}

	stepReq := mc.CreateBeadCalls[1]
	// Should have both parent labels and step labels (merged).
	if len(stepReq.Labels) != 2 {
		t.Errorf("step labels = %v, want 2 labels", stepReq.Labels)
	}
}

func TestRunFormulaApply_StepDefaultType(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s"}
	mc.Beads["gd-f"] = &model.Bead{
		ID:    "gd-f",
		Type:  "formula",
		Title: "F",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Step"},
		}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f"})
	if err != nil {
		t.Fatal(err)
	}

	stepReq := mc.CreateBeadCalls[1]
	if stepReq.Type != "task" {
		t.Errorf("step type = %q, want 'task' (default)", stepReq.Type)
	}
}

func TestRunFormulaApply_StepAssigneeFallback(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1", "gd-s2"}
	mc.Beads["gd-f"] = &model.Bead{
		ID:    "gd-f",
		Type:  "formula",
		Title: "F",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Step 1", Assignee: "bob"},
			{ID: "s2", Title: "Step 2"},
		}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f", Assignee: "alice"})
	if err != nil {
		t.Fatal(err)
	}

	// Step with explicit assignee keeps it.
	s1 := mc.CreateBeadCalls[1]
	if s1.Assignee != "bob" {
		t.Errorf("step 1 assignee = %q, want bob", s1.Assignee)
	}

	// Step without assignee falls back to opts.Assignee.
	s2 := mc.CreateBeadCalls[2]
	if s2.Assignee != "alice" {
		t.Errorf("step 2 assignee = %q, want alice (fallback)", s2.Assignee)
	}
}

func TestRunFormulaApply_GetBeadError(t *testing.T) {
	mc := newMockClient()
	mc.GetErr = fmt.Errorf("connection refused")
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-missing"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "resolving formula") {
		t.Errorf("error = %q, want 'resolving formula'", err)
	}
}

func TestRunFormulaApply_CreateMolError(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f"] = &model.Bead{
		ID:    "gd-f",
		Type:  "formula",
		Title: "F",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Step"},
		}),
	}
	mc.CreateErr = fmt.Errorf("disk full")
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "creating molecule") {
		t.Errorf("error = %q, want 'creating molecule'", err)
	}
}

func TestRunFormulaApply_StepPriority(t *testing.T) {
	p3 := 3
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1", "gd-s2"}
	mc.Beads["gd-f"] = &model.Bead{
		ID:       "gd-f",
		Type:     "formula",
		Title:    "F",
		Priority: 2,
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Step 1", Priority: &p3},
			{ID: "s2", Title: "Step 2"},
		}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f"})
	if err != nil {
		t.Fatal(err)
	}

	// Step with explicit priority keeps it.
	s1 := mc.CreateBeadCalls[1]
	if s1.Priority != 3 {
		t.Errorf("step 1 priority = %d, want 3", s1.Priority)
	}

	// Step without priority inherits formula's priority.
	s2 := mc.CreateBeadCalls[2]
	if s2.Priority != 2 {
		t.Errorf("step 2 priority = %d, want 2 (from formula)", s2.Priority)
	}
}

func TestRunFormulaApply_AssigneeVarSubstitution(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s"}
	mc.Beads["gd-f"] = &model.Bead{
		ID:    "gd-f",
		Type:  "formula",
		Title: "F",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "who"}},
			[]FormulaStep{{ID: "s1", Title: "Step", Assignee: "{{who}}"}},
		),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-f",
		VarPairs:  []string{"who=charlie"},
	})
	if err != nil {
		t.Fatal(err)
	}

	s1 := mc.CreateBeadCalls[1]
	if s1.Assignee != "charlie" {
		t.Errorf("step assignee = %q, want charlie", s1.Assignee)
	}
}

func TestRunFormulaApply_AssignedAgent(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1"}
	mc.Beads["gd-f-agent"] = &model.Bead{
		ID:    "gd-f-agent",
		Type:  "formula",
		Title: "Agent workflow",
		Fields: makeFormulaFieldsWithAgent(nil, []FormulaStep{
			{ID: "s1", Title: "Do work"},
		}, "my-agent"),
	}
	withMockClient(t, mc)

	// No --assignee on the command line — should use formula's assigned_agent.
	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-agent"})
	if err != nil {
		t.Fatalf("apply with assigned_agent: %v", err)
	}

	molReq := mc.CreateBeadCalls[0]
	if molReq.Assignee != "my-agent" {
		t.Errorf("molecule assignee = %q, want 'my-agent'", molReq.Assignee)
	}
	stepReq := mc.CreateBeadCalls[1]
	if stepReq.Assignee != "my-agent" {
		t.Errorf("step assignee = %q, want 'my-agent' (from formula)", stepReq.Assignee)
	}
}

func TestRunFormulaApply_AssignedAgentOverride(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1"}
	mc.Beads["gd-f-agent2"] = &model.Bead{
		ID:    "gd-f-agent2",
		Type:  "formula",
		Title: "Agent workflow",
		Fields: makeFormulaFieldsWithAgent(nil, []FormulaStep{
			{ID: "s1", Title: "Do work"},
		}, "default-agent"),
	}
	withMockClient(t, mc)

	// --assignee on the command line should override formula's assigned_agent.
	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-f-agent2",
		Assignee:  "override-agent",
	})
	if err != nil {
		t.Fatalf("apply with assignee override: %v", err)
	}

	molReq := mc.CreateBeadCalls[0]
	if molReq.Assignee != "override-agent" {
		t.Errorf("molecule assignee = %q, want 'override-agent'", molReq.Assignee)
	}
}

func TestRunFormulaApply_LabelPropagation(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1"}
	mc.Beads["gd-f-labels"] = &model.Bead{
		ID:     "gd-f-labels",
		Type:   "formula",
		Title:  "Labeled formula",
		Labels: []string{"project:gardenio", "role:crew"},
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Do work", Labels: []string{"step-extra"}},
		}),
	}
	withMockClient(t, mc)

	// Formula labels should propagate to molecule and steps.
	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-f-labels",
		Labels:    []string{"extra:label"},
	})
	if err != nil {
		t.Fatalf("apply with label propagation: %v", err)
	}

	// Molecule should have formula labels + command labels.
	molReq := mc.CreateBeadCalls[0]
	molLabelSet := make(map[string]bool)
	for _, l := range molReq.Labels {
		molLabelSet[l] = true
	}
	for _, want := range []string{"project:gardenio", "role:crew", "extra:label"} {
		if !molLabelSet[want] {
			t.Errorf("molecule labels %v missing %q", molReq.Labels, want)
		}
	}

	// Steps should also get the merged labels + their own step labels.
	stepReq := mc.CreateBeadCalls[1]
	stepLabelSet := make(map[string]bool)
	for _, l := range stepReq.Labels {
		stepLabelSet[l] = true
	}
	for _, want := range []string{"project:gardenio", "role:crew", "extra:label", "step-extra"} {
		if !stepLabelSet[want] {
			t.Errorf("step labels %v missing %q", stepReq.Labels, want)
		}
	}
}

func TestRunFormulaApply_DefaultRoles(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1", "gd-s2"}
	mc.Beads["gd-f-roles"] = &model.Bead{
		ID:    "gd-f-roles",
		Type:  "formula",
		Title: "Multi-role workflow",
		Fields: makeFormulaFieldsWithRoles(nil, []FormulaStep{
			{ID: "s1", Title: "Research"},
			{ID: "s2", Title: "Implement", DependsOn: []string{"s1"}},
		}, []string{"crew", "worker"}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-roles"})
	if err != nil {
		t.Fatalf("apply with default_roles: %v", err)
	}

	// Both steps should have role:crew and role:worker labels from default_roles.
	for i, name := range []string{"s1", "s2"} {
		stepReq := mc.CreateBeadCalls[i+1]
		labelSet := make(map[string]bool)
		for _, l := range stepReq.Labels {
			labelSet[l] = true
		}
		for _, want := range []string{"role:crew", "role:worker"} {
			if !labelSet[want] {
				t.Errorf("step %s labels %v missing %q", name, stepReq.Labels, want)
			}
		}
	}
}

func TestRunFormulaApply_StepRolesOverrideDefault(t *testing.T) {
	mc := newMockClient()
	mc.CreateIDs = []string{"gd-m", "gd-s1", "gd-s2"}
	mc.Beads["gd-f-roles2"] = &model.Bead{
		ID:    "gd-f-roles2",
		Type:  "formula",
		Title: "Override roles",
		Fields: makeFormulaFieldsWithRoles(nil, []FormulaStep{
			{ID: "s1", Title: "Step with default roles"},
			{ID: "s2", Title: "Step with own roles", Roles: []string{"gardener"}},
		}, []string{"crew"}),
	}
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f-roles2"})
	if err != nil {
		t.Fatalf("apply with step role override: %v", err)
	}

	// Step 1 should use default_roles → role:crew.
	s1 := mc.CreateBeadCalls[1]
	s1Labels := make(map[string]bool)
	for _, l := range s1.Labels {
		s1Labels[l] = true
	}
	if !s1Labels["role:crew"] {
		t.Errorf("step 1 labels %v missing role:crew (from default_roles)", s1.Labels)
	}
	if s1Labels["role:gardener"] {
		t.Errorf("step 1 labels %v should not have role:gardener", s1.Labels)
	}

	// Step 2 should use its own roles → role:gardener, NOT role:crew.
	s2 := mc.CreateBeadCalls[2]
	s2Labels := make(map[string]bool)
	for _, l := range s2.Labels {
		s2Labels[l] = true
	}
	if !s2Labels["role:gardener"] {
		t.Errorf("step 2 labels %v missing role:gardener (from step roles)", s2.Labels)
	}
	if s2Labels["role:crew"] {
		t.Errorf("step 2 labels %v should not have role:crew (overridden by step roles)", s2.Labels)
	}
}

func TestCapitalize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"molecule", "Molecule"},
		{"wisp", "Wisp"},
		{"", ""},
		{"A", "A"},
	}
	for _, tt := range tests {
		if got := capitalize(tt.in); got != tt.want {
			t.Errorf("capitalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
