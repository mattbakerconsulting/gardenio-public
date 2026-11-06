package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// --- formula dump tests ---

func TestFormulaDump_Stdout(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f1"] = &model.Bead{
		ID:   "gd-f1",
		Type: "formula",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "version", Required: true}},
			[]FormulaStep{
				{ID: "build", Title: "Build {{version}}", Type: "task"},
				{ID: "deploy", Title: "Deploy {{version}}", Type: "task", DependsOn: []string{"build"}},
			},
		),
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := runFormulaDump("gd-f1", ""); err != nil {
			t.Fatalf("dump: %v", err)
		}
	})

	var parsed struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %s", err, out)
	}
	if len(parsed.Vars) != 1 {
		t.Errorf("vars count = %d, want 1", len(parsed.Vars))
	}
	if len(parsed.Steps) != 2 {
		t.Errorf("steps count = %d, want 2", len(parsed.Steps))
	}
	if parsed.Steps[0].ID != "build" {
		t.Errorf("first step id = %q, want build", parsed.Steps[0].ID)
	}
}

func TestFormulaDump_ToFile(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f2"] = &model.Bead{
		ID:   "gd-f2",
		Type: "formula",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "s1", Title: "Step 1"},
		}),
	}
	withMockClient(t, mc)

	outFile := filepath.Join(t.TempDir(), "out.json")
	if err := runFormulaDump("gd-f2", outFile); err != nil {
		t.Fatalf("dump --file: %v", err)
	}

	data, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	var parsed struct {
		Steps []FormulaStep `json:"steps"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("output file is not valid JSON: %v", err)
	}
	if len(parsed.Steps) != 1 {
		t.Errorf("steps count = %d, want 1", len(parsed.Steps))
	}
}

func TestFormulaDump_NotFormula(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "task"}
	withMockClient(t, mc)

	err := runFormulaDump("gd-task", "")
	if err == nil {
		t.Fatal("expected error for non-formula bead")
	}
	if !strings.Contains(err.Error(), "not formula") {
		t.Errorf("error = %q, want mention of 'not formula'", err)
	}
}

func TestFormulaDump_EmptyFields(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-empty"] = &model.Bead{ID: "gd-empty", Type: "formula"}
	withMockClient(t, mc)

	err := runFormulaDump("gd-empty", "")
	if err == nil {
		t.Fatal("expected error for empty fields")
	}
	if !strings.Contains(err.Error(), "no fields") {
		t.Errorf("error = %q, want 'no fields'", err)
	}
}

func TestFormulaDump_GetError(t *testing.T) {
	mc := newMockClient()
	mc.GetErr = fmt.Errorf("connection refused")
	withMockClient(t, mc)

	err := runFormulaDump("gd-missing", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "getting formula") {
		t.Errorf("error = %q, want 'getting formula'", err)
	}
}

// --- formula update tests ---

func TestFormulaUpdate_Basic(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f1"] = &model.Bead{
		ID:   "gd-f1",
		Type: "formula",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "old", Title: "Old step"},
		}),
	}
	withMockClient(t, mc)

	newFormula := `{
		"vars": [{"name": "env", "required": true}],
		"steps": [
			{"id": "build", "title": "Build for {{env}}", "type": "task"},
			{"id": "deploy", "title": "Deploy to {{env}}", "type": "task", "depends_on": ["build"]}
		]
	}`
	tmpFile := filepath.Join(t.TempDir(), "formula.json")
	if err := os.WriteFile(tmpFile, []byte(newFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runFormulaUpdate("gd-f1", tmpFile, "", false); err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}

	call := mc.UpdateBeadCalls[0]
	if call.ID != "gd-f1" {
		t.Errorf("update ID = %q, want gd-f1", call.ID)
	}

	var fields struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}
	if err := json.Unmarshal(call.Req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal update fields: %v", err)
	}
	if len(fields.Steps) != 2 {
		t.Errorf("steps = %d, want 2", len(fields.Steps))
	}
	if len(fields.Vars) != 1 {
		t.Errorf("vars = %d, want 1", len(fields.Vars))
	}
	if fields.Steps[0].ID != "build" {
		t.Errorf("first step id = %q, want build", fields.Steps[0].ID)
	}
}

func TestFormulaUpdate_NotFormula(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "task"}
	withMockClient(t, mc)

	tmpFile := filepath.Join(t.TempDir(), "f.json")
	os.WriteFile(tmpFile, []byte(`{"steps":[{"id":"s","title":"S"}]}`), 0o644)

	err := runFormulaUpdate("gd-task", tmpFile, "", false)
	if err == nil {
		t.Fatal("expected error for non-formula bead")
	}
	if !strings.Contains(err.Error(), "not formula") {
		t.Errorf("error = %q, want 'not formula'", err)
	}
}

func TestFormulaUpdate_MissingFile(t *testing.T) {
	err := runFormulaUpdate("gd-f", "", "", false)
	if err == nil {
		t.Fatal("expected error for missing --file")
	}
	if !strings.Contains(err.Error(), "--file or --assignee is required") {
		t.Errorf("error = %q, want '--file or --assignee is required'", err)
	}
}

func TestFormulaUpdate_NoSteps(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f"] = &model.Bead{ID: "gd-f", Type: "formula"}
	withMockClient(t, mc)

	tmpFile := filepath.Join(t.TempDir(), "f.json")
	os.WriteFile(tmpFile, []byte(`{"vars":[],"steps":[]}`), 0o644)

	err := runFormulaUpdate("gd-f", tmpFile, "", false)
	if err == nil {
		t.Fatal("expected error for empty steps")
	}
	if !strings.Contains(err.Error(), "at least one step") {
		t.Errorf("error = %q, want 'at least one step'", err)
	}
}

func TestFormulaUpdate_DuplicateStepID(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f"] = &model.Bead{ID: "gd-f", Type: "formula"}
	withMockClient(t, mc)

	tmpFile := filepath.Join(t.TempDir(), "f.json")
	os.WriteFile(tmpFile, []byte(`{"steps":[{"id":"s","title":"A"},{"id":"s","title":"B"}]}`), 0o644)

	err := runFormulaUpdate("gd-f", tmpFile, "", false)
	if err == nil {
		t.Fatal("expected error for duplicate step id")
	}
	if !strings.Contains(err.Error(), "duplicate step id") {
		t.Errorf("error = %q, want 'duplicate step id'", err)
	}
}

func TestFormulaUpdate_BadDependency(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f"] = &model.Bead{ID: "gd-f", Type: "formula"}
	withMockClient(t, mc)

	tmpFile := filepath.Join(t.TempDir(), "f.json")
	os.WriteFile(tmpFile, []byte(`{"steps":[{"id":"s1","title":"A","depends_on":["missing"]}]}`), 0o644)

	err := runFormulaUpdate("gd-f", tmpFile, "", false)
	if err == nil {
		t.Fatal("expected error for bad dependency")
	}
	if !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("error = %q, want 'unknown step'", err)
	}
}

func TestFormulaUpdate_ServerError(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f"] = &model.Bead{ID: "gd-f", Type: "formula"}
	mc.UpdateErr = fmt.Errorf("server unavailable")
	withMockClient(t, mc)

	tmpFile := filepath.Join(t.TempDir(), "f.json")
	os.WriteFile(tmpFile, []byte(`{"steps":[{"id":"s","title":"S"}]}`), 0o644)

	err := runFormulaUpdate("gd-f", tmpFile, "", false)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "updating formula") {
		t.Errorf("error = %q, want 'updating formula'", err)
	}
}

func TestFormulaUpdate_RoundTrip(t *testing.T) {
	// Dump a formula then update it back — fields should round-trip.
	mc := newMockClient()
	mc.Beads["gd-rt"] = &model.Bead{
		ID:   "gd-rt",
		Type: "formula",
		Fields: makeFormulaFields(
			[]FormulaVarDef{{Name: "env", Required: true, Default: "staging"}},
			[]FormulaStep{
				{ID: "build", Title: "Build {{env}}", Type: "task"},
				{ID: "deploy", Title: "Deploy {{env}}", Type: "task", DependsOn: []string{"build"}},
			},
		),
	}
	withMockClient(t, mc)

	// Dump to file.
	dumpFile := filepath.Join(t.TempDir(), "rt.json")
	if err := runFormulaDump("gd-rt", dumpFile); err != nil {
		t.Fatalf("dump: %v", err)
	}

	// Update from that same file.
	if err := runFormulaUpdate("gd-rt", dumpFile, "", false); err != nil {
		t.Fatalf("update: %v", err)
	}

	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}

	var fields struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}
	if err := json.Unmarshal(mc.UpdateBeadCalls[0].Req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields.Steps) != 2 {
		t.Errorf("round-trip steps = %d, want 2", len(fields.Steps))
	}
	if len(fields.Vars) != 1 {
		t.Errorf("round-trip vars = %d, want 1", len(fields.Vars))
	}
}

// TestFormulaUpdate_PreservesTopLevelFields verifies that `kd formula update
// --file` preserves every top-level formula field present in the file (phase,
// formula_type, default_roles, etc.), not just vars/steps. Regression: flipping
// a formula's `phase` from vapor→liquid silently never persisted because the
// update path rebuilt a {vars,steps}-only map (ka-0000005050).
func TestFormulaUpdate_PreservesTopLevelFields(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-f1"] = &model.Bead{
		ID:   "gd-f1",
		Type: "formula",
		Fields: makeFormulaFields(nil, []FormulaStep{
			{ID: "old", Title: "Old step"},
		}),
	}
	withMockClient(t, mc)

	newFormula := `{
		"phase": "liquid",
		"formula_type": "workflow",
		"default_roles": ["auto-worker-haiku"],
		"vars": [{"name": "env", "required": true}],
		"steps": [
			{"id": "build", "title": "Build for {{env}}", "type": "task"}
		]
	}`
	tmpFile := filepath.Join(t.TempDir(), "formula.json")
	if err := os.WriteFile(tmpFile, []byte(newFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := runFormulaUpdate("gd-f1", tmpFile, "", false); err != nil {
		t.Fatalf("update: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}

	var fields struct {
		Phase        string          `json:"phase"`
		FormulaType  string          `json:"formula_type"`
		DefaultRoles []string        `json:"default_roles"`
		Vars         []FormulaVarDef `json:"vars"`
		Steps        []FormulaStep   `json:"steps"`
	}
	if err := json.Unmarshal(mc.UpdateBeadCalls[0].Req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal update fields: %v", err)
	}
	if fields.Phase != "liquid" {
		t.Errorf("phase = %q, want liquid (top-level field dropped on update)", fields.Phase)
	}
	if fields.FormulaType != "workflow" {
		t.Errorf("formula_type = %q, want workflow", fields.FormulaType)
	}
	if len(fields.DefaultRoles) != 1 || fields.DefaultRoles[0] != "auto-worker-haiku" {
		t.Errorf("default_roles = %v, want [auto-worker-haiku]", fields.DefaultRoles)
	}
	if len(fields.Steps) != 1 || fields.Steps[0].ID != "build" {
		t.Errorf("steps = %+v, want single build step", fields.Steps)
	}
	if len(fields.Vars) != 1 {
		t.Errorf("vars = %d, want 1", len(fields.Vars))
	}
}

// TestFormulaUpdate_ExtendsDepOnInheritedStep is the regression guard for
// ka-0000005592: dumping an extending formula and updating it straight back —
// with zero edits — was rejected because update validated depends_on only
// against the file's own steps, ignoring the `extends` parent. A child step
// that depends_on an INHERITED step must round-trip through update, just as it
// already does through create/cook/apply.
func TestFormulaUpdate_ExtendsDepOnInheritedStep(t *testing.T) {
	mc := newMockClient()

	// Parent provides "check" and "investigate-fix".
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends(nil, nil,
			[]FormulaStep{
				{ID: "check", Title: "Check", Type: "task"},
				{ID: "investigate-fix", Title: "Investigate", Type: "task", DependsOn: []string{"check"}},
			},
		),
	}
	// Child extends parent and overrides only "fix-critic", whose depends_on
	// points at the INHERITED "investigate-fix" step.
	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-parent"}, nil,
			[]FormulaStep{
				{ID: "fix-critic", Title: "Critic", Type: "task", DependsOn: []string{"investigate-fix"}},
			},
		),
	}
	withMockClient(t, mc)

	// Dump the child (emits its stored fields verbatim, incl. extends).
	dumpFile := filepath.Join(t.TempDir(), "child.json")
	if err := runFormulaDump("gd-child", dumpFile); err != nil {
		t.Fatalf("dump: %v", err)
	}

	// Update straight back — must succeed.
	if err := runFormulaUpdate("gd-child", dumpFile, "", false); err != nil {
		t.Fatalf("update of extends formula w/ dep on inherited step should succeed, got: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}

	// The extends field must be preserved through the round-trip.
	var fields struct {
		Extends []string      `json:"extends"`
		Steps   []FormulaStep `json:"steps"`
	}
	if err := json.Unmarshal(mc.UpdateBeadCalls[0].Req.Fields, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(fields.Extends) != 1 || fields.Extends[0] != "gd-parent" {
		t.Errorf("extends = %v, want [gd-parent] (dropped on update)", fields.Extends)
	}
	if len(fields.Steps) != 1 || fields.Steps[0].ID != "fix-critic" {
		t.Errorf("steps = %+v, want single fix-critic step", fields.Steps)
	}
}

// TestFormulaUpdate_ExtendsDepOnUnknownStepStillFails ensures the extends-aware
// path did not weaken validation: a depends_on matching neither a child nor an
// inherited step is still rejected.
func TestFormulaUpdate_ExtendsDepOnUnknownStepStillFails(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends(nil, nil,
			[]FormulaStep{{ID: "investigate-fix", Title: "Investigate", Type: "task"}},
		),
	}
	mc.Beads["gd-child"] = &model.Bead{ID: "gd-child", Type: "formula", Title: "Child"}
	withMockClient(t, mc)

	tmpFile := filepath.Join(t.TempDir(), "child.json")
	os.WriteFile(tmpFile, []byte(`{"extends":["gd-parent"],"steps":[{"id":"fix-critic","title":"Critic","depends_on":["does-not-exist"]}]}`), 0o644)

	err := runFormulaUpdate("gd-child", tmpFile, "", false)
	if err == nil {
		t.Fatal("update with dep on truly-unknown step should fail, got nil")
	}
	if !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("error = %q, want 'unknown step'", err)
	}
}

// TestFormulaUpdate_ExtendsOnlyNoOwnSteps verifies a formula that extends a
// parent and overrides only vars (zero own steps) is accepted — the old
// "formula must have at least one step" check would have rejected it.
func TestFormulaUpdate_ExtendsOnlyNoOwnSteps(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends(nil, nil,
			[]FormulaStep{{ID: "s1", Title: "Step 1", Type: "task"}},
		),
	}
	mc.Beads["gd-child"] = &model.Bead{ID: "gd-child", Type: "formula", Title: "Child"}
	withMockClient(t, mc)

	tmpFile := filepath.Join(t.TempDir(), "child.json")
	os.WriteFile(tmpFile, []byte(`{"extends":["gd-parent"],"vars":[{"name":"env"}],"steps":[]}`), 0o644)

	if err := runFormulaUpdate("gd-child", tmpFile, "", false); err != nil {
		t.Fatalf("extends-only update (no own steps) should succeed, got: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}
}
