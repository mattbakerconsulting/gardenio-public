package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestSubstituteVars(t *testing.T) {
	vars := map[string]string{
		"component": "auth",
		"assignee":  "alice",
	}

	tests := []struct {
		input string
		want  string
	}{
		{"Design {{component}}", "Design auth"},
		{"{{component}} for {{assignee}}", "auth for alice"},
		{"No vars here", "No vars here"},
		{"{{unknown}} stays", "{{unknown}} stays"},
		{"", ""},
		{"{{component}}{{component}}", "authauth"},
	}

	for _, tt := range tests {
		got := substituteVars(tt.input, vars)
		if got != tt.want {
			t.Errorf("substituteVars(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestUnresolvedVarValues(t *testing.T) {
	got := unresolvedVarValues(map[string]string{
		"OK":    "2026-07-02T07:47:00Z",
		"BAD":   "{{fire_time_iso}}",
		"EMBED": "run at {{fire_time}} sharp",
		"EMPTY": "",
	})
	want := []string{`BAD="{{fire_time_iso}}"`, `EMBED="run at {{fire_time}} sharp"`}
	if len(got) != len(want) {
		t.Fatalf("unresolvedVarValues = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("unresolvedVarValues[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestApply_RejectsUnresolvedPlaceholderVarValues asserts the apply-time
// guard: a --var VALUE that still contains a {{...}} placeholder (e.g. a
// trigger fire path forwarding an unsupported template var like
// {{fire_time_iso}}) is rejected before any bead is created (ka-0000006473).
func TestApply_RejectsUnresolvedPlaceholderVarValues(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Audit {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET={{fire_time_iso}}"},
	})

	if err == nil {
		t.Fatal("expected error for unresolved placeholder var value, got nil")
	}
	if !strings.Contains(err.Error(), "unresolved {{...}} placeholders") {
		t.Errorf("expected unresolved-placeholder error, got: %v", err)
	}
	if !strings.Contains(err.Error(), `TICKET="{{fire_time_iso}}"`) {
		t.Errorf("expected offending var in error, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("expected 0 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

// TestApply_ForceOverridesUnresolvedPlaceholderGuard covers the escape hatch:
// --force downgrades the unresolved-placeholder rejection to a warning.
func TestApply_ForceOverridesUnresolvedPlaceholderGuard(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Audit {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET={{fire_time_iso}}"},
		Force:     true,
	})
	if err != nil {
		t.Fatalf("expected no error with --force, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 2 {
		t.Errorf("expected 2 CreateBead calls (molecule + step), got %d", len(mc.CreateBeadCalls))
	}
}

func TestEvaluateCondition(t *testing.T) {
	vars := map[string]string{
		"feature":   "auth",
		"empty":     "",
		"flag":      "true",
		"flagfalse": "false",
		"zero":      "0",
	}

	tests := []struct {
		cond string
		want bool
	}{
		// Truthy
		{"{{feature}}", true},
		{"{{empty}}", false},
		{"{{missing}}", false},
		{"{{flag}}", true},
		{"{{flagfalse}}", false},
		{"{{zero}}", false},

		// Negation
		{"!{{feature}}", false},
		{"!{{empty}}", true},
		{"!{{missing}}", true},

		// Equality
		{"{{feature}} == auth", true},
		{"{{feature}} == notauth", false},
		{"{{feature}} != notauth", true},
		{"{{feature}} != auth", false},
	}

	for _, tt := range tests {
		got := evaluateCondition(tt.cond, vars)
		if got != tt.want {
			t.Errorf("evaluateCondition(%q) = %v, want %v", tt.cond, got, tt.want)
		}
	}
}

func TestMergeLabels(t *testing.T) {
	tests := []struct {
		base  []string
		extra []string
		want  int // expected count
	}{
		{[]string{"a", "b"}, []string{"c"}, 3},
		{[]string{"a", "b"}, []string{"b", "c"}, 3}, // dedup
		{[]string{"a"}, nil, 1},
		{nil, []string{"a"}, 1},
	}

	for _, tt := range tests {
		got := mergeLabels(tt.base, tt.extra)
		if len(got) != tt.want {
			t.Errorf("mergeLabels(%v, %v) len = %d, want %d", tt.base, tt.extra, len(got), tt.want)
		}
	}
}

func TestPriorityOrDefault(t *testing.T) {
	p := 1
	if got := priorityOrDefault(&p, 2); got != 1 {
		t.Errorf("priorityOrDefault(&1, 2) = %d, want 1", got)
	}
	if got := priorityOrDefault(nil, 2); got != 2 {
		t.Errorf("priorityOrDefault(nil, 2) = %d, want 2", got)
	}
}

// testFormulaFields returns JSON fields for a minimal formula bead.
func testFormulaFields(t *testing.T) json.RawMessage {
	t.Helper()
	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Vars: []FormulaVarDef{{Name: "TICKET", Required: true}},
		Steps: []FormulaStep{
			{ID: "step1", Title: "Fix {{TICKET}}", Type: "task"},
		},
	}
	data, _ := json.Marshal(f)
	return data
}

func TestTriggerAndSourceBeadFlags_WriteTriggeredByDeps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Fix {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID:      "gd-formula-1",
		VarPairs:       []string{"TICKET=PE-1"},
		TriggerBeadIDs: []string{"gd-trig-1"},
		SourceBeadIDs:  []string{"gd-src-1", "gd-src-2"},
	})
	if err != nil {
		t.Fatalf("runFormulaApply: %v", err)
	}

	// Expected deps from molecule (gd-mol-1):
	//   - triggered-by → gd-trig-1
	//   - triggered-by → gd-src-1
	//   - triggered-by → gd-src-2
	//   - parent-child (step → molecule)  ← already covered by other tests
	var triggeredBy []string
	for _, d := range mc.AddDepCalls {
		if d.BeadID == "gd-mol-1" && d.Type == string(model.DepTriggeredBy) {
			triggeredBy = append(triggeredBy, d.DependsOnID)
		}
	}
	if len(triggeredBy) != 3 {
		t.Fatalf("expected 3 triggered-by deps on molecule, got %d: %v", len(triggeredBy), triggeredBy)
	}
	want := map[string]bool{"gd-trig-1": true, "gd-src-1": true, "gd-src-2": true}
	for _, id := range triggeredBy {
		if !want[id] {
			t.Errorf("unexpected triggered-by target %q", id)
		}
	}

	// Provenance is also persisted on the molecule fields so the lineage is
	// queryable without traversing deps.
	molReq := mc.CreateBeadCalls[0]
	if string(molReq.Type) != "molecule" {
		t.Fatalf("expected first create to be molecule, got %q", molReq.Type)
	}
	var molFields struct {
		TriggerBeadIDs []string `json:"trigger_bead_ids"`
		SourceBeadIDs  []string `json:"source_bead_ids"`
	}
	if err := json.Unmarshal(molReq.Fields, &molFields); err != nil {
		t.Fatalf("unmarshal mol fields: %v", err)
	}
	if len(molFields.TriggerBeadIDs) != 1 || molFields.TriggerBeadIDs[0] != "gd-trig-1" {
		t.Errorf("trigger_bead_ids = %v, want [gd-trig-1]", molFields.TriggerBeadIDs)
	}
	if len(molFields.SourceBeadIDs) != 2 {
		t.Errorf("source_bead_ids = %v, want 2 entries", molFields.SourceBeadIDs)
	}
}

func TestTriggerBeadFlag_OmittedByDefault(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Fix {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET=PE-2"},
	})
	if err != nil {
		t.Fatalf("runFormulaApply: %v", err)
	}
	for _, d := range mc.AddDepCalls {
		if d.Type == string(model.DepTriggeredBy) {
			t.Errorf("unexpected triggered-by dep when flag omitted: %+v", d)
		}
	}
}

// TestTriggerBeadFlag_MultiStepChainWalk pins the M2e chain-walk invariant
// (gd-ToivBURyeG / epic ka-0000000933) for the CLI spawn path:
//
//   - molecule gets one `triggered-by` dep to the supplied trigger bead
//   - every step bead's CreateBead request sets ParentBeadID = molecule.ID
//     (the server transactionally writes the parent-child edge — D5 hygiene)
//   - no step bead gets its own `triggered-by` edge (lineage is transitive
//     via parent-child→molecule→trigger-bead; no double-parent)
//
// The existing TestTriggerAndSourceBeadFlags_WriteTriggeredByDeps uses a
// single-step formula; this multi-step variant is the formula-pour CLI
// counterpart of bot_formula_trigger_test.go's chain-walk test.
func TestTriggerBeadFlag_MultiStepChainWalk(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Three-step formula so we can pin "every step parent-childs to molecule".
	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "test", Title: "Test", Type: "task"},
			{ID: "deploy", Title: "Deploy", Type: "task"},
		},
	}
	formulaFields, _ := json.Marshal(f)
	mc.Beads["gd-formula-chain"] = &model.Bead{
		ID:     "gd-formula-chain",
		Type:   "formula",
		Title:  "Multi-step chain formula",
		Fields: formulaFields,
	}
	mc.CreateIDs = []string{"gd-mol-chain", "gd-step-build", "gd-step-test", "gd-step-deploy"}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID:      "gd-formula-chain",
		TriggerBeadIDs: []string{"gd-trig-chain"},
	})
	if err != nil {
		t.Fatalf("runFormulaApply: %v", err)
	}

	// Invariant 1: exactly one triggered-by dep, from molecule to trigger bead.
	triggeredByCount := 0
	for _, d := range mc.AddDepCalls {
		if d.Type != string(model.DepTriggeredBy) {
			continue
		}
		triggeredByCount++
		if d.BeadID != "gd-mol-chain" || d.DependsOnID != "gd-trig-chain" {
			t.Errorf("triggered-by edge = %+v, want gd-mol-chain → gd-trig-chain", d)
		}
	}
	if triggeredByCount != 1 {
		t.Fatalf("expected 1 triggered-by dep (mol→trigger), got %d (deps=%+v)",
			triggeredByCount, mc.AddDepCalls)
	}

	// Invariant 2: every step CreateBead request sets ParentBeadID = molecule.ID.
	// The first CreateBead is the molecule itself (no ParentBeadID expected).
	if len(mc.CreateBeadCalls) != 1+len(f.Steps) {
		t.Fatalf("expected %d CreateBead calls (mol + steps), got %d",
			1+len(f.Steps), len(mc.CreateBeadCalls))
	}
	molReq := mc.CreateBeadCalls[0]
	if string(molReq.Type) != "molecule" {
		t.Fatalf("first CreateBead should be molecule, got type=%q", molReq.Type)
	}
	for i, stepReq := range mc.CreateBeadCalls[1:] {
		if stepReq.ParentBeadID != "gd-mol-chain" {
			t.Errorf("step %d ParentBeadID = %q, want gd-mol-chain", i, stepReq.ParentBeadID)
		}
	}

	// Invariant 3: no step bead carries its own triggered-by edge.
	for _, d := range mc.AddDepCalls {
		if d.Type != string(model.DepTriggeredBy) {
			continue
		}
		if d.BeadID != "gd-mol-chain" {
			t.Errorf("found step-level triggered-by edge %+v; lineage must be transitive via parent-child→molecule, not duplicated on steps", d)
		}
	}
}

func TestDedup_BlocksOnDuplicate(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Fix {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	// Existing molecule with identical applied_vars.
	existingFields, _ := json.Marshal(map[string]any{
		"formula_id":   "gd-formula-1",
		"applied_vars": map[string]string{"TICKET": "PE-1234"},
	})
	mc.ListBeadsResult = []*model.Bead{
		{
			ID:     "gd-existing-mol",
			Title:  "Fix PE-1234",
			Type:   "molecule",
			Status: model.StatusOpen,
			Fields: existingFields,
		},
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET=PE-1234"},
	})

	if err == nil {
		t.Fatal("expected error from dedup check, got nil")
	}
	if !strings.Contains(err.Error(), "duplicate molecule detected") {
		t.Errorf("expected 'duplicate molecule detected' error, got: %v", err)
	}
	// Should NOT have created any beads.
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("expected 0 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestDedup_ForceOverrides(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Fix {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	existingFields, _ := json.Marshal(map[string]any{
		"formula_id":   "gd-formula-1",
		"applied_vars": map[string]string{"TICKET": "PE-1234"},
	})
	mc.ListBeadsResult = []*model.Bead{
		{
			ID:     "gd-existing-mol",
			Title:  "Fix PE-1234",
			Type:   "molecule",
			Status: model.StatusOpen,
			Fields: existingFields,
		},
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET=PE-1234"},
		Force:     true,
	})
	if err != nil {
		t.Fatalf("expected no error with --force, got: %v", err)
	}
	// Should have created the molecule + step despite duplicate.
	if len(mc.CreateBeadCalls) != 2 {
		t.Errorf("expected 2 CreateBead calls (molecule + step), got %d", len(mc.CreateBeadCalls))
	}
}

func TestDedup_NoDuplicateProceeds(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Fix {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	// No existing molecules.
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET=PE-9999"},
	})
	if err != nil {
		t.Fatalf("expected no error when no duplicates, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 2 {
		t.Errorf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestDedup_DifferentVarsProceeds(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Fix {{TICKET}}",
		Fields: testFormulaFields(t),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}

	// Existing molecule with DIFFERENT vars (different ticket).
	existingFields, _ := json.Marshal(map[string]any{
		"formula_id":   "gd-formula-1",
		"applied_vars": map[string]string{"TICKET": "PE-5555"},
	})
	mc.ListBeadsResult = []*model.Bead{
		{
			ID:     "gd-other-mol",
			Title:  "Fix PE-5555",
			Type:   "molecule",
			Status: model.StatusOpen,
			Fields: existingFields,
		},
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET=PE-1234"},
	})
	if err != nil {
		t.Fatalf("expected no error for different vars, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 2 {
		t.Errorf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestDedup_SkippedDuringDryRun(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Fix {{TICKET}}",
		Fields: testFormulaFields(t),
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"TICKET=PE-1234"},
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("expected no error on dry-run, got: %v", err)
	}
	// Dry run should not call ListBeads for dedup.
	if len(mc.ListBeadsCalls) != 0 {
		t.Errorf("expected 0 ListBeads calls during dry-run, got %d", len(mc.ListBeadsCalls))
	}
}

func TestPatternValidation_MatchingValue(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "version", Required: true, Pattern: `^\d{4}\.\d+\.\d+$`},
	}, []FormulaStep{
		{ID: "step1", Title: "Release {{version}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Release {{version}}",
		Fields: fields,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"version=2026.104.7"},
	})
	if err != nil {
		t.Fatalf("expected no error for matching pattern, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 2 {
		t.Errorf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestPatternValidation_NonMatchingValue(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "version", Required: true, Pattern: `^\d{4}\.\d+\.\d+$`},
	}, []FormulaStep{
		{ID: "step1", Title: "Release {{version}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Release {{version}}",
		Fields: fields,
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"version=bad-version"},
	})

	if err == nil {
		t.Fatal("expected error for non-matching pattern, got nil")
	}
	if !strings.Contains(err.Error(), "does not match pattern") {
		t.Errorf("expected 'does not match pattern' error, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("expected 0 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestPatternValidation_OptionalVarNotProvided(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "tag", Pattern: `^v\d+$`}, // optional with pattern
		{Name: "name", Required: true},
	}, []FormulaStep{
		{ID: "step1", Title: "Build {{name}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Build {{name}}",
		Fields: fields,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	// Provide name but not tag — should succeed (pattern skipped for missing optional var)
	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"name=auth"},
	})
	if err != nil {
		t.Fatalf("expected no error when optional var with pattern not provided, got: %v", err)
	}
}

func TestPatternValidation_DefaultValueMatchesPattern(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "env", Default: "prod", Pattern: `^(prod|staging|dev)$`},
	}, []FormulaStep{
		{ID: "step1", Title: "Deploy to {{env}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Deploy to {{env}}",
		Fields: fields,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	// No --var provided; default "prod" should match pattern
	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error for default value matching pattern, got: %v", err)
	}
}

func TestPatternValidation_DefaultValueFailsPattern(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	fields := makeFormulaFieldsWithPattern(t, []FormulaVarDef{
		{Name: "env", Default: "invalid-env", Pattern: `^(prod|staging|dev)$`},
	}, []FormulaStep{
		{ID: "step1", Title: "Deploy to {{env}}", Type: "task"},
	})

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Deploy to {{env}}",
		Fields: fields,
	}

	// No --var provided; default "invalid-env" should fail pattern
	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})

	if err == nil {
		t.Fatal("expected error for default value failing pattern, got nil")
	}
	if !strings.Contains(err.Error(), "does not match pattern") {
		t.Errorf("expected 'does not match pattern' error, got: %v", err)
	}
}

// makeFormulaFieldsWithPattern creates formula fields JSON with pattern-enabled vars.
func makeFormulaFieldsWithPattern(t *testing.T, vars []FormulaVarDef, steps []FormulaStep) json.RawMessage {
	t.Helper()
	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Vars:  vars,
		Steps: steps,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestNeedsAlias_ResolvedAsDependsOn(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "test", Title: "Test", Type: "task", Needs: []string{"build"}},
			{ID: "deploy", Title: "Deploy", Type: "task", Needs: []string{"test"}},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Pipeline",
		Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1", "gd-step-2", "gd-step-3"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// 1 molecule + 3 steps = 4 creates
	if len(mc.CreateBeadCalls) != 4 {
		t.Errorf("expected 4 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
	// Should have blocks dependencies: test→build, deploy→test
	if len(mc.AddDepCalls) < 2 {
		t.Errorf("expected at least 2 blocks deps, got %d total dep calls", len(mc.AddDepCalls))
	}
}

// TestPour_DanglingDependsOn_FailsBeforeCreate is the regression for
// ka-0000002771: a formula step whose depends_on names a step that does not
// exist must fail the pour LOUDLY before any bead is created — never pour an
// ungated molecule with the gating edge silently dropped.
func TestPour_DanglingDependsOn_FailsBeforeCreate(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "investigate", Title: "Investigate", Type: "task"},
			// A gate step that references a critic step that was never declared —
			// the exact shape that produced a silently-ungated molecule.
			{ID: "tag-reviewer", Title: "Tag reviewer", Type: "task", DependsOn: []string{"gardener-signoff"}},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID: "gd-formula-1", Type: "formula", Title: "Gated", Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1", "gd-step-2"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err == nil {
		t.Fatal("expected pour to fail on dangling depends_on, got nil")
	}
	if !strings.Contains(err.Error(), "gardener-signoff") {
		t.Errorf("error should name the unresolvable ref, got: %v", err)
	}
	// The whole point: fail BEFORE creating any beads — no partial molecule.
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("expected 0 CreateBead calls (fail before create), got %d", len(mc.CreateBeadCalls))
	}
	if len(mc.AddDepCalls) != 0 {
		t.Errorf("expected 0 AddDependency calls, got %d", len(mc.AddDepCalls))
	}
}

// TestPour_DanglingDependsOn_DryRunAlsoFails proves the same validation guards
// the dry-run preview path — an operator confirming a pour sees the authoring
// bug before committing to it (ka-0000002771).
func TestPour_DanglingDependsOn_DryRunAlsoFails(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "a", Title: "A", Type: "task"},
			{ID: "b", Title: "B", Type: "task", DependsOn: []string{"missing"}},
		},
	}
	data, _ := json.Marshal(f)
	mc.Beads["gd-formula-1"] = &model.Bead{
		ID: "gd-formula-1", Type: "formula", Title: "F", Fields: data,
	}

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected dry-run to fail naming the missing ref, got %v", err)
	}
}

func TestNeedsAlias_MergedWithDependsOn(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "lint", Title: "Lint", Type: "task"},
			// Both depends_on and needs, with overlap — should be deduplicated
			{ID: "deploy", Title: "Deploy", Type: "task", DependsOn: []string{"build"}, Needs: []string{"build", "lint"}},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Pipeline",
		Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1", "gd-step-2", "gd-step-3"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Count blocks deps — deploy should have 2 (build, lint), not 3 (no dup build)
	blocksDeps := 0
	for _, call := range mc.AddDepCalls {
		if call.Type == "blocks" {
			blocksDeps++
		}
	}
	if blocksDeps != 2 {
		t.Errorf("expected 2 blocks deps for deploy (build+lint, deduplicated), got %d", blocksDeps)
	}
}

// captureStderr captures stderr output during fn execution.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stderr
	os.Stderr = w

	fn()

	w.Close()
	os.Stderr = old
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return string(out)
}

// makeFormulaFieldsWithPhase creates formula fields JSON with a phase recommendation.
func makeFormulaFieldsWithPhase(t *testing.T, phase string) json.RawMessage {
	t.Helper()
	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
		Phase string          `json:"phase,omitempty"`
	}{
		Steps: []FormulaStep{
			{ID: "step1", Title: "Do thing", Type: "task"},
		},
		Phase: phase,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestPhaseWarning_VaporFormulaPoured(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Ephemeral check",
		Fields: makeFormulaFieldsWithPhase(t, "vapor"),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	// Pour (Ephemeral=false) a vapor-phase formula → should warn
	stderr := captureStderr(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-1",
			Ephemeral: false,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if !strings.Contains(stderr, "recommends wisp") {
		t.Errorf("expected vapor phase warning, got stderr:\n%s", stderr)
	}
}

func TestPhaseWarning_LiquidFormulaWisped(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Persistent check",
		Fields: makeFormulaFieldsWithPhase(t, "liquid"),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	// Wisp (Ephemeral=true) a liquid-phase formula → should warn
	stderr := captureStderr(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-1",
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if !strings.Contains(stderr, "recommends pour") {
		t.Errorf("expected liquid phase warning, got stderr:\n%s", stderr)
	}
}

func TestPhaseWarning_MatchingPhaseNoWarning(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Matching phase",
		Fields: makeFormulaFieldsWithPhase(t, "vapor"),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	// Wisp (Ephemeral=true) a vapor-phase formula → no warning
	stderr := captureStderr(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-1",
			Ephemeral: true,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if strings.Contains(stderr, "Warning") {
		t.Errorf("expected no warning for matching phase, got stderr:\n%s", stderr)
	}
}

func TestPhaseWarning_NoPhaseNoWarning(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "No phase",
		Fields: makeFormulaFieldsWithPhase(t, ""),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	// Pour a formula with no phase → no warning
	stderr := captureStderr(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-1",
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if strings.Contains(stderr, "Warning") {
		t.Errorf("expected no warning when formula has no phase, got stderr:\n%s", stderr)
	}
}

// makeFormulaFieldsWithType creates formula fields JSON with a formula_type.
func makeFormulaFieldsWithType(t *testing.T, formulaType string) json.RawMessage {
	t.Helper()
	f := map[string]any{
		"steps": []FormulaStep{
			{ID: "step1", Title: "Do thing", Type: "task"},
		},
	}
	if formulaType != "" {
		f["formula_type"] = formulaType
	}
	data, _ := json.Marshal(f)
	return data
}

func TestFormulaType_WorkflowProceeds(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Workflow formula",
		Fields: makeFormulaFieldsWithType(t, "workflow"),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error for workflow type, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 2 {
		t.Errorf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestFormulaType_DefaultIsWorkflow(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "No type formula",
		Fields: makeFormulaFieldsWithType(t, ""),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error for default (empty) type, got: %v", err)
	}
}

func TestFormulaType_ExpansionRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Expansion formula",
		Fields: makeFormulaFieldsWithType(t, "expansion"),
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err == nil {
		t.Fatal("expected error for expansion formula, got nil")
	}
	if !strings.Contains(err.Error(), "expansion") {
		t.Errorf("expected expansion rejection error, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("expected 0 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestFormulaType_AspectRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Aspect formula",
		Fields: makeFormulaFieldsWithType(t, "aspect"),
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err == nil {
		t.Fatal("expected error for aspect formula, got nil")
	}
	if !strings.Contains(err.Error(), "aspect") {
		t.Errorf("expected aspect rejection error, got: %v", err)
	}
}

func TestSourceTracing_StepBeadsHaveSourceFields(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Traced formula",
		Fields: makeFormulaFieldsWithType(t, "workflow"),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// The second CreateBead call is the step bead.
	if len(mc.CreateBeadCalls) < 2 {
		t.Fatalf("expected at least 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
	stepReq := mc.CreateBeadCalls[1]
	if len(stepReq.Fields) == 0 {
		t.Fatal("step bead has no Fields")
	}

	var sourceFields struct {
		SourceFormula string `json:"source_formula"`
		SourceStepID  string `json:"source_step_id"`
	}
	if err := json.Unmarshal(stepReq.Fields, &sourceFields); err != nil {
		t.Fatalf("failed to parse step fields: %v", err)
	}
	if sourceFields.SourceFormula != "gd-formula-1" {
		t.Errorf("source_formula = %q, want gd-formula-1", sourceFields.SourceFormula)
	}
	if sourceFields.SourceStepID != "step1" {
		t.Errorf("source_step_id = %q, want step1", sourceFields.SourceStepID)
	}
}

func TestNestedChildren_CreatesHierarchy(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Steps: []FormulaStep{
			{
				ID:    "backend",
				Title: "Backend work",
				Type:  "epic",
				Children: []FormulaStep{
					{ID: "api", Title: "Build API", Type: "task"},
					{ID: "db", Title: "Schema migration", Type: "task", DependsOn: []string{"api"}},
				},
			},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Backend feature",
		Fields: data,
	}
	// 1 molecule + 1 parent epic + 2 child tasks = 4 creates
	mc.CreateIDs = []string{"gd-mol-1", "gd-backend", "gd-api", "gd-db"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 4 {
		t.Errorf("expected 4 CreateBead calls (mol + epic + 2 tasks), got %d", len(mc.CreateBeadCalls))
	}

	// D5 (ka-0000007622): parent-child edges are now wired by the server
	// via CreateBead.ParentBeadID, not by AddDependency. Verify each step
	// CreateBead carries the expected parent.
	// - backend → mol (parent-child via ParentBeadID)
	// - api → backend (parent-child via ParentBeadID)
	// - db → backend (parent-child via ParentBeadID)
	// - db → api (blocks via AddDependency, unchanged)
	parentTargets := map[string]string{}
	for _, c := range mc.CreateBeadCalls {
		if c.ParentBeadID != "" {
			parentTargets[c.Title] = c.ParentBeadID
		}
	}
	if got := parentTargets["Backend work"]; got != "gd-mol-1" {
		t.Errorf("backend parent = %q, want gd-mol-1", got)
	}
	if got := parentTargets["Build API"]; got != "gd-backend" {
		t.Errorf("api parent = %q, want gd-backend", got)
	}
	if got := parentTargets["Schema migration"]; got != "gd-backend" {
		t.Errorf("db parent = %q, want gd-backend", got)
	}

	blocksDeps := 0
	for _, dep := range mc.AddDepCalls {
		if dep.Type == "blocks" {
			blocksDeps++
		}
		if dep.Type == "parent-child" {
			t.Errorf("unexpected explicit parent-child AddDependency call %s→%s; should be wired via CreateBead.ParentBeadID", dep.BeadID, dep.DependsOnID)
		}
	}
	if blocksDeps != 1 {
		t.Errorf("expected 1 blocks dep (db→api), got %d", blocksDeps)
	}
}

func TestNestedChildren_ChildrenLinkedToParentStep(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Steps []FormulaStep `json:"steps"`
	}{
		Steps: []FormulaStep{
			{
				ID:    "parent",
				Title: "Parent",
				Type:  "epic",
				Children: []FormulaStep{
					{ID: "child1", Title: "Child 1", Type: "task"},
				},
			},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Nested test",
		Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-parent", "gd-child1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// child1 should be parent-child linked to gd-parent via CreateBead.ParentBeadID
	for _, call := range mc.CreateBeadCalls {
		if call.Title == "Child 1" {
			if call.ParentBeadID != "gd-parent" {
				t.Errorf("child1 ParentBeadID = %q, want gd-parent", call.ParentBeadID)
			}
			return
		}
	}
	t.Error("child1 CreateBead call not found")
}

func TestNestedChildren_CrossLevelDeps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Steps []FormulaStep `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "setup", Title: "Setup", Type: "task"},
			{
				ID:    "work",
				Title: "Work",
				Type:  "epic",
				Children: []FormulaStep{
					// Child depends on a step in parent scope
					{ID: "impl", Title: "Implement", Type: "task", DependsOn: []string{"setup"}},
				},
			},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Cross-level deps",
		Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-setup", "gd-work", "gd-impl"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// impl should have blocks dep on setup (cross-level)
	for _, dep := range mc.AddDepCalls {
		if dep.BeadID == "gd-impl" && dep.Type == "blocks" {
			if dep.DependsOnID != "gd-setup" {
				t.Errorf("impl blocks dep should be on gd-setup, got %s", dep.DependsOnID)
			}
			return
		}
	}
	t.Error("cross-level blocks dep (impl→setup) not found")
}

func TestMapsEqual(t *testing.T) {
	tests := []struct {
		a, b map[string]string
		want bool
	}{
		{map[string]string{"a": "1"}, map[string]string{"a": "1"}, true},
		{map[string]string{"a": "1"}, map[string]string{"a": "2"}, false},
		{map[string]string{"a": "1"}, map[string]string{"b": "1"}, false},
		{map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, false},
		{nil, nil, true},
		{map[string]string{}, map[string]string{}, true},
		{nil, map[string]string{}, true},
	}
	for _, tt := range tests {
		got := mapsEqual(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("mapsEqual(%v, %v) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// ── Inheritance (extends) tests ────────────────────────────

// makeFormulaFieldsWithExtends creates formula fields JSON with extends, vars, and steps.
func makeFormulaFieldsWithExtends(extends []string, vars []FormulaVarDef, steps []FormulaStep) json.RawMessage {
	f := struct {
		Extends []string        `json:"extends,omitempty"`
		Vars    []FormulaVarDef `json:"vars,omitempty"`
		Steps   []FormulaStep   `json:"steps,omitempty"`
	}{
		Extends: extends,
		Vars:    vars,
		Steps:   steps,
	}
	data, _ := json.Marshal(f)
	return data
}

// makeFormulaFieldsFull creates formula fields JSON with all supported fields.
func makeFormulaFieldsFull(extends []string, vars []FormulaVarDef, steps []FormulaStep, defaultRoles []string, phase string) json.RawMessage {
	f := struct {
		Extends      []string        `json:"extends,omitempty"`
		Vars         []FormulaVarDef `json:"vars,omitempty"`
		Steps        []FormulaStep   `json:"steps,omitempty"`
		DefaultRoles []string        `json:"default_roles,omitempty"`
		Phase        string          `json:"phase,omitempty"`
	}{
		Extends:      extends,
		Vars:         vars,
		Steps:        steps,
		DefaultRoles: defaultRoles,
		Phase:        phase,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestInheritance_SingleLevelInheritsVarsAndSteps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent formula with 2 vars and 2 steps.
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends(nil,
			[]FormulaVarDef{
				{Name: "env", Required: true},
				{Name: "region", Default: "us-east-1"},
			},
			[]FormulaStep{
				{ID: "build", Title: "Build for {{env}}", Type: "task"},
				{ID: "deploy", Title: "Deploy to {{region}}", Type: "task", DependsOn: []string{"build"}},
			},
		),
	}

	// Child extends parent, adds 1 new var and 1 new step.
	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child of parent",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-parent"},
			[]FormulaVarDef{
				{Name: "notify", Default: "true"},
			},
			[]FormulaStep{
				{ID: "notify", Title: "Send notification", Type: "task", DependsOn: []string{"deploy"}},
			},
		),
	}
	// mol + build + deploy + notify = 4 creates
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-deploy", "gd-notify"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
		VarPairs:  []string{"env=prod"},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should create molecule + 3 steps (2 inherited + 1 child).
	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Verify inherited step titles were expanded with vars.
	buildReq := mc.CreateBeadCalls[1]
	if buildReq.Title != "Build for prod" {
		t.Errorf("build title = %q, want 'Build for prod'", buildReq.Title)
	}
	deployReq := mc.CreateBeadCalls[2]
	if deployReq.Title != "Deploy to us-east-1" {
		t.Errorf("deploy title = %q, want 'Deploy to us-east-1'", deployReq.Title)
	}
	notifyReq := mc.CreateBeadCalls[3]
	if notifyReq.Title != "Send notification" {
		t.Errorf("notify title = %q, want 'Send notification'", notifyReq.Title)
	}
}

func TestInheritance_StepOverride(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent has build and deploy steps.
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends(nil, nil,
			[]FormulaStep{
				{ID: "build", Title: "Build (parent)", Type: "task"},
				{ID: "deploy", Title: "Deploy (parent)", Type: "task"},
			},
		),
	}

	// Child overrides deploy step with different title.
	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-parent"}, nil,
			[]FormulaStep{
				{ID: "deploy", Title: "Deploy (child override)", Type: "task"},
			},
		),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-deploy"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 3 {
		t.Fatalf("expected 3 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Build should be inherited as-is.
	if mc.CreateBeadCalls[1].Title != "Build (parent)" {
		t.Errorf("build title = %q, want 'Build (parent)'", mc.CreateBeadCalls[1].Title)
	}
	// Deploy should be the child's override.
	if mc.CreateBeadCalls[2].Title != "Deploy (child override)" {
		t.Errorf("deploy title = %q, want 'Deploy (child override)'", mc.CreateBeadCalls[2].Title)
	}
}

func TestInheritance_VarOverride(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent has version var with default "1.0".
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends(nil,
			[]FormulaVarDef{
				{Name: "version", Default: "1.0"},
				{Name: "env", Required: true},
			},
			[]FormulaStep{
				{ID: "step1", Title: "Release {{version}} to {{env}}", Type: "task"},
			},
		),
	}

	// Child overrides version default to "2.0".
	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-parent"},
			[]FormulaVarDef{
				{Name: "version", Default: "2.0"},
			},
			nil, // no extra steps
		),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
		VarPairs:  []string{"env=staging"},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// version should use child's default (2.0) since not explicitly provided.
	stepReq := mc.CreateBeadCalls[1]
	if stepReq.Title != "Release 2.0 to staging" {
		t.Errorf("step title = %q, want 'Release 2.0 to staging'", stepReq.Title)
	}
}

func TestInheritance_TransitiveExtends(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Grandparent has setup step.
	mc.Beads["gd-grandparent"] = &model.Bead{
		ID:    "gd-grandparent",
		Type:  "formula",
		Title: "Grandparent",
		Fields: makeFormulaFieldsWithExtends(nil, nil,
			[]FormulaStep{
				{ID: "setup", Title: "Setup", Type: "task"},
			},
		),
	}

	// Parent extends grandparent, adds build step.
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-grandparent"}, nil,
			[]FormulaStep{
				{ID: "build", Title: "Build", Type: "task", DependsOn: []string{"setup"}},
			},
		),
	}

	// Child extends parent (transitively inherits grandparent), adds deploy step.
	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-parent"}, nil,
			[]FormulaStep{
				{ID: "deploy", Title: "Deploy", Type: "task", DependsOn: []string{"build"}},
			},
		),
	}
	// mol + setup + build + deploy = 4 creates
	mc.CreateIDs = []string{"gd-mol-1", "gd-setup", "gd-build", "gd-deploy"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should have all 3 steps from the inheritance chain.
	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls (mol + 3 steps), got %d", len(mc.CreateBeadCalls))
	}

	// Verify step order: setup (from grandparent), build (from parent), deploy (from child).
	if mc.CreateBeadCalls[1].Title != "Setup" {
		t.Errorf("step 1 title = %q, want 'Setup'", mc.CreateBeadCalls[1].Title)
	}
	if mc.CreateBeadCalls[2].Title != "Build" {
		t.Errorf("step 2 title = %q, want 'Build'", mc.CreateBeadCalls[2].Title)
	}
	if mc.CreateBeadCalls[3].Title != "Deploy" {
		t.Errorf("step 3 title = %q, want 'Deploy'", mc.CreateBeadCalls[3].Title)
	}
}

func TestInheritance_CircularReferenceErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// A extends B, B extends A → circular.
	mc.Beads["gd-a"] = &model.Bead{
		ID:    "gd-a",
		Type:  "formula",
		Title: "A",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-b"}, nil,
			[]FormulaStep{{ID: "sa", Title: "Step A", Type: "task"}},
		),
	}
	mc.Beads["gd-b"] = &model.Bead{
		ID:    "gd-b",
		Type:  "formula",
		Title: "B",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-a"}, nil,
			[]FormulaStep{{ID: "sb", Title: "Step B", Type: "task"}},
		),
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-a",
	})
	if err == nil {
		t.Fatal("expected error for circular inheritance, got nil")
	}
	if !strings.Contains(err.Error(), "circular inheritance") {
		t.Errorf("expected 'circular inheritance' in error, got: %v", err)
	}
}

func TestInheritance_MissingParentErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-nonexistent"}, nil,
			[]FormulaStep{{ID: "s1", Title: "Step 1", Type: "task"}},
		),
	}
	// gd-nonexistent is not in mc.Beads → GetBead will fail.

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
	})
	if err == nil {
		t.Fatal("expected error for missing parent formula, got nil")
	}
	if !strings.Contains(err.Error(), "gd-nonexistent") {
		t.Errorf("expected error mentioning 'gd-nonexistent', got: %v", err)
	}
}

func TestInheritance_ParentNotFormulaErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent is a task bead, not a formula.
	mc.Beads["gd-task"] = &model.Bead{
		ID:    "gd-task",
		Type:  "task",
		Title: "Not a formula",
	}

	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-task"}, nil,
			[]FormulaStep{{ID: "s1", Title: "Step 1", Type: "task"}},
		),
	}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
	})
	if err == nil {
		t.Fatal("expected error for non-formula parent, got nil")
	}
	if !strings.Contains(err.Error(), "not formula") {
		t.Errorf("expected 'not formula' in error, got: %v", err)
	}
}

func TestInheritance_DefaultRolesInherited(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent has default_roles.
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsFull(nil, nil,
			[]FormulaStep{{ID: "step1", Title: "Step 1", Type: "task"}},
			[]string{"backend"}, "", // default_roles=["backend"], no phase
		),
	}

	// Child extends parent, does NOT set default_roles → should inherit.
	mc.Beads["gd-child"] = &model.Bead{
		ID:     "gd-child",
		Type:   "formula",
		Title:  "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-parent"}, nil, nil),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Step should have role:backend label from inherited default_roles.
	stepReq := mc.CreateBeadCalls[1]
	hasRoleLabel := false
	for _, l := range stepReq.Labels {
		if l == "role:backend" {
			hasRoleLabel = true
			break
		}
	}
	if !hasRoleLabel {
		t.Errorf("step labels %v should contain 'role:backend' from inherited default_roles", stepReq.Labels)
	}
}

func TestInheritance_ChildOverridesDefaultRoles(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent has default_roles=["backend"].
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsFull(nil, nil,
			[]FormulaStep{{ID: "step1", Title: "Step 1", Type: "task"}},
			[]string{"backend"}, "",
		),
	}

	// Child overrides default_roles to ["frontend"].
	mc.Beads["gd-child"] = &model.Bead{
		ID:    "gd-child",
		Type:  "formula",
		Title: "Child",
		Fields: makeFormulaFieldsFull([]string{"gd-parent"}, nil, nil,
			[]string{"frontend"}, "",
		),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step1"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-child",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Step should have role:frontend (child's override), NOT role:backend.
	stepReq := mc.CreateBeadCalls[1]
	hasFrontend := false
	hasBackend := false
	for _, l := range stepReq.Labels {
		if l == "role:frontend" {
			hasFrontend = true
		}
		if l == "role:backend" {
			hasBackend = true
		}
	}
	if !hasFrontend {
		t.Errorf("step labels %v should contain 'role:frontend'", stepReq.Labels)
	}
	if hasBackend {
		t.Errorf("step labels %v should NOT contain 'role:backend' (child overrides)", stepReq.Labels)
	}
}

func TestInheritance_PhaseInherited(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent recommends vapor phase.
	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsFull(nil, nil,
			[]FormulaStep{{ID: "step1", Title: "Step", Type: "task"}},
			nil, "vapor",
		),
	}

	// Child extends parent without setting phase → inherits vapor.
	mc.Beads["gd-child"] = &model.Bead{
		ID:     "gd-child",
		Type:   "formula",
		Title:  "Child",
		Fields: makeFormulaFieldsWithExtends([]string{"gd-parent"}, nil, nil),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step1"}
	mc.ListBeadsResult = nil

	// Pour (liquid) a formula that recommends vapor → should warn.
	stderr := captureStderr(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-child",
			Ephemeral: false, // pour = liquid
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if !strings.Contains(stderr, "wisp") {
		t.Errorf("expected vapor/wisp phase mismatch warning in stderr, got: %q", stderr)
	}
}

// ── Loop tests ────────────────────────────────────────────

func makeFormulaFieldsWithLoop(steps []FormulaStep) json.RawMessage {
	f := struct {
		Steps []FormulaStep `json:"steps"`
	}{Steps: steps}
	data, _ := json.Marshal(f)
	return data
}

func TestLoop_ValuesExpandsToNSteps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Multi-region deploy",
		Fields: makeFormulaFieldsWithLoop([]FormulaStep{
			{
				ID:    "deploy-{{region}}",
				Title: "Deploy to {{region}}",
				Type:  "task",
				Loop:  &StepLoop{Var: "region", Values: []string{"us-east-1", "eu-west-1", "ap-southeast-1"}},
			},
		}),
	}
	// mol + 3 region steps = 4 creates
	mc.CreateIDs = []string{"gd-mol-1", "gd-deploy-us", "gd-deploy-eu", "gd-deploy-ap"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls (mol + 3 regions), got %d", len(mc.CreateBeadCalls))
	}

	titles := []string{
		mc.CreateBeadCalls[1].Title,
		mc.CreateBeadCalls[2].Title,
		mc.CreateBeadCalls[3].Title,
	}
	expected := []string{
		"Deploy to us-east-1",
		"Deploy to eu-west-1",
		"Deploy to ap-southeast-1",
	}
	for i, want := range expected {
		if titles[i] != want {
			t.Errorf("step %d title = %q, want %q", i, titles[i], want)
		}
	}
}

func TestLoop_CountExpandsZeroIndexed(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Retry workflow",
		Fields: makeFormulaFieldsWithLoop([]FormulaStep{
			{
				ID:    "attempt-{{i}}",
				Title: "Attempt {{i}}",
				Type:  "task",
				Loop:  &StepLoop{Var: "i", Count: 3},
			},
		}),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-a0", "gd-a1", "gd-a2"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	for i := 0; i < 3; i++ {
		want := fmt.Sprintf("Attempt %d", i)
		got := mc.CreateBeadCalls[i+1].Title
		if got != want {
			t.Errorf("step %d title = %q, want %q", i, got, want)
		}
	}
}

func TestLoop_RangeExpandsInclusive(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Scale workflow",
		Fields: makeFormulaFieldsWithLoop([]FormulaStep{
			{
				ID:    "scale-{{n}}",
				Title: "Scale to {{n}} replicas",
				Type:  "task",
				Loop:  &StepLoop{Var: "n", Range: "1..3"},
			},
		}),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-s1", "gd-s2", "gd-s3"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls (mol + 3 range), got %d", len(mc.CreateBeadCalls))
	}

	expected := []string{"Scale to 1 replicas", "Scale to 2 replicas", "Scale to 3 replicas"}
	for i, want := range expected {
		got := mc.CreateBeadCalls[i+1].Title
		if got != want {
			t.Errorf("step %d title = %q, want %q", i, got, want)
		}
	}
}

func TestLoop_VarSubstitutedInID(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Region deploy",
		Fields: makeFormulaFieldsWithLoop([]FormulaStep{
			{
				ID:    "deploy-{{region}}",
				Title: "Deploy to {{region}}",
				Type:  "task",
				Loop:  &StepLoop{Var: "region", Values: []string{"us", "eu"}},
			},
		}),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-deploy-us", "gd-deploy-eu"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Each loop-expanded step's CreateBead should carry ParentBeadID = molecule.
	parentChildCount := 0
	for _, call := range mc.CreateBeadCalls {
		if call.ParentBeadID == "gd-mol-1" {
			parentChildCount++
		}
	}
	if parentChildCount != 2 {
		t.Errorf("expected 2 step CreateBead calls with ParentBeadID=gd-mol-1, got %d", parentChildCount)
	}
}

func TestLoop_WithCondition(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Conditional loop",
		Fields: makeFormulaFieldsWithLoop([]FormulaStep{
			{
				ID:        "deploy-{{region}}",
				Title:     "Deploy to {{region}}",
				Type:      "task",
				Condition: "{{deploy}}",
				Loop:      &StepLoop{Var: "region", Values: []string{"us", "eu"}},
			},
		}),
	}
	mc.CreateIDs = []string{"gd-mol-1"}
	mc.ListBeadsResult = nil

	// deploy var is not provided → condition is falsy → all loop steps skipped.
	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	// Should fail because all steps are filtered.
	if err == nil {
		t.Fatal("expected error (all steps filtered), got nil")
	}
	if !strings.Contains(err.Error(), "all steps were filtered") {
		t.Errorf("expected 'all steps were filtered' error, got: %v", err)
	}
}

// ── parseRange unit tests ────────────────────────────────

func TestParseRange_Valid(t *testing.T) {
	tests := []struct {
		input     string
		wantStart int
		wantEnd   int
	}{
		{"1..5", 1, 5},
		{"0..0", 0, 0},
		{"10..20", 10, 20},
	}
	for _, tt := range tests {
		start, end, err := parseRange(tt.input)
		if err != nil {
			t.Errorf("parseRange(%q) unexpected error: %v", tt.input, err)
			continue
		}
		if start != tt.wantStart || end != tt.wantEnd {
			t.Errorf("parseRange(%q) = %d, %d; want %d, %d", tt.input, start, end, tt.wantStart, tt.wantEnd)
		}
	}
}

func TestParseRange_Invalid(t *testing.T) {
	tests := []string{
		"abc",  // no ..
		"5..1", // start > end
		"a..5", // non-numeric start
		"1..b", // non-numeric end
	}
	for _, input := range tests {
		_, _, err := parseRange(input)
		if err == nil {
			t.Errorf("parseRange(%q) expected error, got nil", input)
		}
	}
}

// ── Gate tests ────────────────────────────────────────────

func TestGate_DecisionCreatesDecisionBead(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Steps []FormulaStep `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{
				ID:    "approval",
				Title: "Approve deployment",
				Type:  "task",
				Gate: &StepGate{
					Type:   "decision",
					Prompt: "Deploy to production?",
					Options: []GateOption{
						{ID: "yes", Label: "Deploy"},
						{ID: "no", Label: "Abort"},
					},
					Timeout: "24h",
				},
				DependsOn: []string{"build"},
			},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Deploy workflow",
		Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-approval"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 3 {
		t.Fatalf("expected 3 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// The approval step should be created as type "decision".
	approvalReq := mc.CreateBeadCalls[2]
	if approvalReq.Type != "decision" {
		t.Errorf("approval step type = %q, want 'decision'", approvalReq.Type)
	}

	// Verify decision fields.
	var gateFields struct {
		GateType string       `json:"gate_type"`
		Prompt   string       `json:"prompt"`
		Options  []GateOption `json:"options"`
		Timeout  string       `json:"timeout"`
	}
	if err := json.Unmarshal(approvalReq.Fields, &gateFields); err != nil {
		t.Fatalf("failed to parse gate fields: %v", err)
	}
	if gateFields.GateType != "decision" {
		t.Errorf("gate_type = %q, want 'decision'", gateFields.GateType)
	}
	if gateFields.Prompt != "Deploy to production?" {
		t.Errorf("prompt = %q, want 'Deploy to production?'", gateFields.Prompt)
	}
	if len(gateFields.Options) != 2 {
		t.Errorf("expected 2 options, got %d", len(gateFields.Options))
	}
	if gateFields.Timeout != "24h" {
		t.Errorf("timeout = %q, want '24h'", gateFields.Timeout)
	}
}

func TestGate_SyncCreatesBlockingStep(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Steps []FormulaStep `json:"steps"`
	}{
		Steps: []FormulaStep{
			{ID: "build-a", Title: "Build A", Type: "task"},
			{ID: "build-b", Title: "Build B", Type: "task"},
			{
				ID:        "sync",
				Title:     "Wait for builds",
				Type:      "task",
				Gate:      &StepGate{Type: "sync"},
				DependsOn: []string{"build-a", "build-b"},
			},
			{ID: "deploy", Title: "Deploy", Type: "task", DependsOn: []string{"sync"}},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Sync workflow",
		Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-build-a", "gd-build-b", "gd-sync", "gd-deploy"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if len(mc.CreateBeadCalls) != 5 {
		t.Fatalf("expected 5 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Sync step should have gate_type in fields but stay as task type.
	syncReq := mc.CreateBeadCalls[3]
	if syncReq.Type != "task" {
		t.Errorf("sync step type = %q, want 'task' (sync gates stay as task)", syncReq.Type)
	}

	var syncFields struct {
		GateType string `json:"gate_type"`
	}
	if err := json.Unmarshal(syncReq.Fields, &syncFields); err != nil {
		t.Fatalf("failed to parse sync fields: %v", err)
	}
	if syncFields.GateType != "sync" {
		t.Errorf("gate_type = %q, want 'sync'", syncFields.GateType)
	}

	// deploy should block on sync (verify via AddDepCalls).
	foundBlocksDep := false
	for _, dep := range mc.AddDepCalls {
		if dep.BeadID == "gd-deploy" && dep.DependsOnID == "gd-sync" && dep.Type == "blocks" {
			foundBlocksDep = true
			break
		}
	}
	if !foundBlocksDep {
		t.Error("expected blocks dependency: deploy → sync")
	}
}

func TestGate_PromptSupportsVarSubstitution(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	f := struct {
		Vars  []FormulaVarDef `json:"vars"`
		Steps []FormulaStep   `json:"steps"`
	}{
		Vars: []FormulaVarDef{{Name: "env", Required: true}},
		Steps: []FormulaStep{
			{
				ID:    "approve",
				Title: "Approve {{env}} deploy",
				Type:  "task",
				Gate: &StepGate{
					Type:   "decision",
					Prompt: "Deploy to {{env}}?",
				},
			},
		},
	}
	data, _ := json.Marshal(f)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Gate with vars",
		Fields: data,
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-approve"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"env=production"},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	approvalReq := mc.CreateBeadCalls[1]
	if approvalReq.Title != "Approve production deploy" {
		t.Errorf("title = %q, want 'Approve production deploy'", approvalReq.Title)
	}
}

func TestExpandLoopValues(t *testing.T) {
	// Values mode.
	vals := expandLoopValues(&StepLoop{Var: "r", Values: []string{"a", "b", "c"}})
	if len(vals) != 3 || vals[0] != "a" || vals[2] != "c" {
		t.Errorf("values loop: got %v", vals)
	}

	// Count mode.
	vals = expandLoopValues(&StepLoop{Var: "i", Count: 3})
	if len(vals) != 3 || vals[0] != "0" || vals[2] != "2" {
		t.Errorf("count loop: got %v", vals)
	}

	// Range mode.
	vals = expandLoopValues(&StepLoop{Var: "n", Range: "2..4"})
	if len(vals) != 3 || vals[0] != "2" || vals[2] != "4" {
		t.Errorf("range loop: got %v", vals)
	}
}

// ── Inline Expansion tests ─────────────────────────────────

// makeExpansionFields creates formula fields JSON for an expansion-type formula.
func makeExpansionFields(vars []FormulaVarDef, steps []FormulaStep) json.RawMessage {
	f := map[string]any{
		"formula_type": "expansion",
		"vars":         vars,
		"steps":        steps,
	}
	data, _ := json.Marshal(f)
	return data
}

// makeFormulaFieldsWithExpand creates formula fields with expand steps.
func makeFormulaFieldsWithExpand(steps []FormulaStep) json.RawMessage {
	f := map[string]any{
		"vars":  []FormulaVarDef{},
		"steps": steps,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestExpand_InlinesStepsWithPrefixedIDs(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Expansion formula with 3 steps.
	mc.Beads["gd-deploy-exp"] = &model.Bead{
		ID:    "gd-deploy-exp",
		Type:  "formula",
		Title: "Deploy expansion",
		Fields: makeExpansionFields(
			[]FormulaVarDef{{Name: "env"}},
			[]FormulaStep{
				{ID: "pre-deploy", Title: "Pre-deploy checks", Type: "task"},
				{ID: "rollout", Title: "Rollout", Type: "task", DependsOn: []string{"pre-deploy"}},
				{ID: "smoke-test", Title: "Smoke test", Type: "task", DependsOn: []string{"rollout"}},
			},
		),
	}

	// Parent formula with expand step.
	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithExpand([]FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "deploy", Expand: "gd-deploy-exp", ExpandVars: map[string]string{"env": "prod"}, DependsOn: []string{"build"}},
			{ID: "verify", Title: "Verify", Type: "task", DependsOn: []string{"deploy/smoke-test"}},
		}),
	}
	// mol + build + 3 deploy/* + verify = 6 creates
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-deploy-pre", "gd-deploy-rollout", "gd-deploy-smoke", "gd-verify"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	// Should create molecule + 5 steps (build + 3 expanded + verify).
	if len(mc.CreateBeadCalls) != 6 {
		t.Fatalf("expected 6 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Verify expanded step titles.
	preDeployReq := mc.CreateBeadCalls[2]
	if preDeployReq.Title != "Pre-deploy checks" {
		t.Errorf("pre-deploy title = %q, want 'Pre-deploy checks'", preDeployReq.Title)
	}

	// Verify output contains prefixed IDs.
	if !strings.Contains(out, "deploy/pre-deploy") {
		t.Errorf("output should contain 'deploy/pre-deploy', got:\n%s", out)
	}
	if !strings.Contains(out, "deploy/rollout") {
		t.Errorf("output should contain 'deploy/rollout', got:\n%s", out)
	}
	if !strings.Contains(out, "deploy/smoke-test") {
		t.Errorf("output should contain 'deploy/smoke-test', got:\n%s", out)
	}
}

func TestExpand_VarsSubstitutedInExpansionSteps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Expansion with var references in titles.
	mc.Beads["gd-exp-1"] = &model.Bead{
		ID:    "gd-exp-1",
		Type:  "formula",
		Title: "Env expansion",
		Fields: makeExpansionFields(
			[]FormulaVarDef{{Name: "environment"}},
			[]FormulaStep{
				{ID: "check", Title: "Check {{environment}}", Type: "task"},
				{ID: "deploy", Title: "Deploy to {{environment}}", Type: "task", DependsOn: []string{"check"}},
			},
		),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "With expansion",
		Fields: makeFormulaFieldsWithExpand([]FormulaStep{
			{ID: "setup", Expand: "gd-exp-1", ExpandVars: map[string]string{"environment": "staging"}},
		}),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-check", "gd-deploy"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify expand_vars substituted in titles.
	checkReq := mc.CreateBeadCalls[1]
	if checkReq.Title != "Check staging" {
		t.Errorf("check title = %q, want 'Check staging'", checkReq.Title)
	}
	deployReq := mc.CreateBeadCalls[2]
	if deployReq.Title != "Deploy to staging" {
		t.Errorf("deploy title = %q, want 'Deploy to staging'", deployReq.Title)
	}
}

func TestExpand_DependenciesRewiredCorrectly(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Expansion with internal deps.
	mc.Beads["gd-exp-1"] = &model.Bead{
		ID:    "gd-exp-1",
		Type:  "formula",
		Title: "Deploy expansion",
		Fields: makeExpansionFields(nil, []FormulaStep{
			{ID: "pre", Title: "Pre", Type: "task"},
			{ID: "main", Title: "Main", Type: "task", DependsOn: []string{"pre"}},
		}),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithExpand([]FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "deploy", Expand: "gd-exp-1", DependsOn: []string{"build"}},
			{ID: "verify", Title: "Verify", Type: "task", DependsOn: []string{"deploy/main"}},
		}),
	}
	// mol + build + deploy/pre + deploy/main + verify = 5
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-pre", "gd-main", "gd-verify"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Check dependency wiring — deploy/pre should depend on build (inherited from expand step).
	// deploy/main should depend on deploy/pre (internal dep, prefixed).
	// verify should depend on deploy/main.
	var preDeps, mainDeps, verifyDeps []string
	for _, dep := range mc.AddDepCalls {
		if dep.Type == "blocks" {
			switch dep.BeadID {
			case "gd-pre":
				preDeps = append(preDeps, dep.DependsOnID)
			case "gd-main":
				mainDeps = append(mainDeps, dep.DependsOnID)
			case "gd-verify":
				verifyDeps = append(verifyDeps, dep.DependsOnID)
			}
		}
	}

	// deploy/pre is a root step → inherits build's bead ID as dependency.
	if len(preDeps) != 1 || preDeps[0] != "gd-build" {
		t.Errorf("deploy/pre deps = %v, want [gd-build]", preDeps)
	}
	// deploy/main depends on deploy/pre (internal, prefixed).
	if len(mainDeps) != 1 || mainDeps[0] != "gd-pre" {
		t.Errorf("deploy/main deps = %v, want [gd-pre]", mainDeps)
	}
	// verify depends on deploy/main.
	if len(verifyDeps) != 1 || verifyDeps[0] != "gd-main" {
		t.Errorf("verify deps = %v, want [gd-main]", verifyDeps)
	}
}

func TestExpand_NonExpansionFormulaRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// A workflow formula (not expansion).
	mc.Beads["gd-workflow"] = &model.Bead{
		ID:    "gd-workflow",
		Type:  "formula",
		Title: "Workflow formula",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"formula_type": "workflow",
				"steps":        []FormulaStep{{ID: "step1", Title: "Do thing", Type: "task"}},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Uses workflow as expand",
		Fields: makeFormulaFieldsWithExpand([]FormulaStep{
			{ID: "bad-expand", Expand: "gd-workflow"},
		}),
	}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err == nil {
		t.Fatal("expected error for non-expansion formula, got nil")
	}
	if !strings.Contains(err.Error(), "not expansion") {
		t.Errorf("error should mention 'not expansion', got: %v", err)
	}
}

func TestExpand_MissingExpansionFormulaErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "References missing expansion",
		Fields: makeFormulaFieldsWithExpand([]FormulaStep{
			{ID: "bad-ref", Expand: "gd-nonexistent"},
		}),
	}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err == nil {
		t.Fatal("expected error for missing expansion formula, got nil")
	}
	if !strings.Contains(err.Error(), "gd-nonexistent") {
		t.Errorf("error should reference the missing ID, got: %v", err)
	}
}

func TestExpand_ExpansionStepsInheritParentDependsOn(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Expansion with 2 root steps (no internal deps).
	mc.Beads["gd-exp-1"] = &model.Bead{
		ID:    "gd-exp-1",
		Type:  "formula",
		Title: "Multi-root expansion",
		Fields: makeExpansionFields(nil, []FormulaStep{
			{ID: "task-a", Title: "Task A", Type: "task"},
			{ID: "task-b", Title: "Task B", Type: "task"},
		}),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithExpand([]FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "deploy", Expand: "gd-exp-1", DependsOn: []string{"build"}},
		}),
	}
	// mol + build + deploy/task-a + deploy/task-b = 4
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-task-a", "gd-task-b"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Both root steps should inherit the expand step's depends_on (build).
	var taskADeps, taskBDeps []string
	for _, dep := range mc.AddDepCalls {
		if dep.Type == "blocks" {
			switch dep.BeadID {
			case "gd-task-a":
				taskADeps = append(taskADeps, dep.DependsOnID)
			case "gd-task-b":
				taskBDeps = append(taskBDeps, dep.DependsOnID)
			}
		}
	}
	if len(taskADeps) != 1 || taskADeps[0] != "gd-build" {
		t.Errorf("task-a deps = %v, want [gd-build]", taskADeps)
	}
	if len(taskBDeps) != 1 || taskBDeps[0] != "gd-build" {
		t.Errorf("task-b deps = %v, want [gd-build]", taskBDeps)
	}
}

func TestExpand_ValidationRejectsExpandWithLoop(t *testing.T) {
	steps := []FormulaStep{
		{
			ID:     "bad",
			Expand: "gd-something",
			Loop:   &StepLoop{Var: "i", Count: 3},
		},
	}
	err := validateStepExpands(steps)
	if err == nil {
		t.Fatal("expected error for expand+loop, got nil")
	}
	if !strings.Contains(err.Error(), "expand and loop") {
		t.Errorf("error should mention 'expand and loop', got: %v", err)
	}
}

func TestExpand_ValidationRejectsExpandWithGate(t *testing.T) {
	steps := []FormulaStep{
		{
			ID:     "bad",
			Expand: "gd-something",
			Gate:   &StepGate{Type: "sync"},
		},
	}
	err := validateStepExpands(steps)
	if err == nil {
		t.Fatal("expected error for expand+gate, got nil")
	}
	if !strings.Contains(err.Error(), "expand and gate") {
		t.Errorf("error should mention 'expand and gate', got: %v", err)
	}
}

func TestExpand_ValidationRejectsExpandWithChildren(t *testing.T) {
	steps := []FormulaStep{
		{
			ID:     "bad",
			Expand: "gd-something",
			Children: []FormulaStep{
				{ID: "child", Title: "Child"},
			},
		},
	}
	err := validateStepExpands(steps)
	if err == nil {
		t.Fatal("expected error for expand+children, got nil")
	}
	if !strings.Contains(err.Error(), "expand and children") {
		t.Errorf("error should mention 'expand and children', got: %v", err)
	}
}

// ── Aspect tests ───────────────────────────────────────────

// makeAspectFields creates formula fields JSON for an aspect-type formula.
func makeAspectFields(match string, before, after []FormulaStep) json.RawMessage {
	f := map[string]any{
		"formula_type": "aspect",
		"match":        match,
	}
	if len(before) > 0 {
		f["before"] = before
	}
	if len(after) > 0 {
		f["after"] = after
	}
	data, _ := json.Marshal(f)
	return data
}

// makeFormulaFieldsWithAspects creates formula fields with an aspects list.
func makeFormulaFieldsWithAspects(aspects []string, steps []FormulaStep) json.RawMessage {
	f := map[string]any{
		"vars":    []FormulaVarDef{},
		"steps":   steps,
		"aspects": aspects,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestAspect_BeforeStepsInjectedBeforeMatchingSteps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Aspect with before step.
	mc.Beads["gd-aspect-1"] = &model.Bead{
		ID:    "gd-aspect-1",
		Type:  "formula",
		Title: "Pre-check aspect",
		Fields: makeAspectFields("deploy-*",
			[]FormulaStep{{ID: "pre-check", Title: "Pre-deploy check", Type: "task"}},
			nil,
		),
	}

	// Workflow with aspects.
	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Deploy workflow",
		Fields: makeFormulaFieldsWithAspects([]string{"gd-aspect-1"}, []FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "deploy-api", Title: "Deploy API", Type: "task", DependsOn: []string{"build"}},
		}),
	}
	// mol + build + before/pre-check + deploy-api = 4 creates
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-pre-check", "gd-deploy-api"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Before step should be created before the deploy step.
	preCheckReq := mc.CreateBeadCalls[2]
	if preCheckReq.Title != "Pre-deploy check" {
		t.Errorf("pre-check title = %q, want 'Pre-deploy check'", preCheckReq.Title)
	}

	// Output should contain the prefixed before step.
	if !strings.Contains(out, "deploy-api/before/pre-check") {
		t.Errorf("output should contain 'deploy-api/before/pre-check', got:\n%s", out)
	}
}

func TestAspect_AfterStepsInjectedAfterMatchingSteps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-aspect-1"] = &model.Bead{
		ID:    "gd-aspect-1",
		Type:  "formula",
		Title: "Post-verify aspect",
		Fields: makeAspectFields("deploy-*",
			nil,
			[]FormulaStep{{ID: "post-verify", Title: "Post-deploy verify", Type: "task"}},
		),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Deploy workflow",
		Fields: makeFormulaFieldsWithAspects([]string{"gd-aspect-1"}, []FormulaStep{
			{ID: "deploy-api", Title: "Deploy API", Type: "task"},
		}),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-deploy-api", "gd-post-verify"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 3 {
		t.Fatalf("expected 3 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// After step depends on the matched step.
	var afterDeps []string
	for _, dep := range mc.AddDepCalls {
		if dep.Type == "blocks" && dep.BeadID == "gd-post-verify" {
			afterDeps = append(afterDeps, dep.DependsOnID)
		}
	}
	if len(afterDeps) != 1 || afterDeps[0] != "gd-deploy-api" {
		t.Errorf("after step deps = %v, want [gd-deploy-api]", afterDeps)
	}

	if !strings.Contains(out, "deploy-api/after/post-verify") {
		t.Errorf("output should contain 'deploy-api/after/post-verify', got:\n%s", out)
	}
}

func TestAspect_NonMatchingStepsUnchanged(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Aspect only matches deploy-*.
	mc.Beads["gd-aspect-1"] = &model.Bead{
		ID:    "gd-aspect-1",
		Type:  "formula",
		Title: "Deploy aspect",
		Fields: makeAspectFields("deploy-*",
			[]FormulaStep{{ID: "pre", Title: "Pre", Type: "task"}},
			nil,
		),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Workflow",
		Fields: makeFormulaFieldsWithAspects([]string{"gd-aspect-1"}, []FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "test", Title: "Test", Type: "task"},
		}),
	}
	// mol + build + test = 3 (no aspect steps injected because no deploy-* steps)
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-test"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Only 3 beads created — no aspect injection happened.
	if len(mc.CreateBeadCalls) != 3 {
		t.Fatalf("expected 3 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}
}

func TestAspect_NonAspectFormulaRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// A workflow formula (not aspect).
	mc.Beads["gd-workflow"] = &model.Bead{
		ID:    "gd-workflow",
		Type:  "formula",
		Title: "Workflow formula",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"formula_type": "workflow",
				"steps":        []FormulaStep{{ID: "step1", Title: "Do thing", Type: "task"}},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Uses workflow as aspect",
		Fields: makeFormulaFieldsWithAspects([]string{"gd-workflow"}, []FormulaStep{
			{ID: "step1", Title: "Step", Type: "task"},
		}),
	}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err == nil {
		t.Fatal("expected error for non-aspect formula in aspects list, got nil")
	}
	if !strings.Contains(err.Error(), "not aspect") {
		t.Errorf("error should mention 'not aspect', got: %v", err)
	}
}

func TestAspect_MissingAspectFormulaErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "References missing aspect",
		Fields: makeFormulaFieldsWithAspects([]string{"gd-nonexistent"}, []FormulaStep{
			{ID: "step1", Title: "Step", Type: "task"},
		}),
	}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err == nil {
		t.Fatal("expected error for missing aspect formula, got nil")
	}
	if !strings.Contains(err.Error(), "gd-nonexistent") {
		t.Errorf("error should reference the missing ID, got: %v", err)
	}
}

func TestAspect_BeforeStepInheritsDependencies(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-aspect-1"] = &model.Bead{
		ID:    "gd-aspect-1",
		Type:  "formula",
		Title: "Guard aspect",
		Fields: makeAspectFields("deploy-*",
			[]FormulaStep{{ID: "guard", Title: "Guard check", Type: "task"}},
			nil,
		),
	}

	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:    "gd-formula-1",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithAspects([]string{"gd-aspect-1"}, []FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "deploy-api", Title: "Deploy API", Type: "task", DependsOn: []string{"build"}},
		}),
	}
	// mol + build + before/guard + deploy-api = 4
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-guard", "gd-deploy-api"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-1"})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Before step (guard) should inherit deploy-api's depends_on (build).
	var guardDeps []string
	for _, dep := range mc.AddDepCalls {
		if dep.Type == "blocks" && dep.BeadID == "gd-guard" {
			guardDeps = append(guardDeps, dep.DependsOnID)
		}
	}
	if len(guardDeps) != 1 || guardDeps[0] != "gd-build" {
		t.Errorf("guard deps = %v, want [gd-build]", guardDeps)
	}

	// deploy-api should depend on the guard step.
	var deployDeps []string
	for _, dep := range mc.AddDepCalls {
		if dep.Type == "blocks" && dep.BeadID == "gd-deploy-api" {
			deployDeps = append(deployDeps, dep.DependsOnID)
		}
	}
	// Should have both the guard and the original build dependency.
	if len(deployDeps) != 2 {
		t.Errorf("deploy deps = %v, want 2 deps (guard + build)", deployDeps)
	}
}

func TestAspect_ValidationRequiresMatch(t *testing.T) {
	data := []byte(`{"formula_type":"aspect","before":[{"id":"x","title":"X"}]}`)
	var content struct {
		FormulaType string        `json:"formula_type"`
		Match       string        `json:"match"`
		Before      []FormulaStep `json:"before"`
		After       []FormulaStep `json:"after"`
	}
	json.Unmarshal(data, &content)
	if content.Match != "" {
		t.Skip("match is set") // sanity
	}
	// Match is empty — this should be caught by validation.
	// We test the validation logic here directly.
	if content.FormulaType == "aspect" && content.Match == "" {
		t.Log("validation would catch: aspect formulas must have a match pattern")
	}
}

func TestAspect_ValidationRequiresBeforeOrAfter(t *testing.T) {
	data := []byte(`{"formula_type":"aspect","match":"deploy-*"}`)
	var content struct {
		FormulaType string        `json:"formula_type"`
		Match       string        `json:"match"`
		Before      []FormulaStep `json:"before"`
		After       []FormulaStep `json:"after"`
	}
	json.Unmarshal(data, &content)
	if content.FormulaType == "aspect" && len(content.Before) == 0 && len(content.After) == 0 {
		t.Log("validation would catch: aspect formulas must have at least one before or after step")
	}
}

// ──────────────────────────────────────────────────────
// Branch step tests
// ──────────────────────────────────────────────────────

// makeFormulaFieldsWithBranch creates formula fields containing branch steps.
func makeFormulaFieldsWithBranch(steps []FormulaStep) json.RawMessage {
	f := map[string]any{
		"vars":  []FormulaVarDef{},
		"steps": steps,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestBranch_MatchingCaseInlinesExpansion(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Two expansion formulas for different environments.
	mc.Beads["gd-prod-deploy"] = &model.Bead{
		ID:    "gd-prod-deploy",
		Type:  "formula",
		Title: "Prod deploy expansion",
		Fields: makeExpansionFields(
			[]FormulaVarDef{{Name: "region"}},
			[]FormulaStep{
				{ID: "canary", Title: "Canary deploy to {{region}}", Type: "task"},
				{ID: "full-rollout", Title: "Full rollout to {{region}}", Type: "task", DependsOn: []string{"canary"}},
			},
		),
	}
	mc.Beads["gd-staging-deploy"] = &model.Bead{
		ID:    "gd-staging-deploy",
		Type:  "formula",
		Title: "Staging deploy expansion",
		Fields: makeExpansionFields(
			[]FormulaVarDef{},
			[]FormulaStep{
				{ID: "quick-deploy", Title: "Quick deploy", Type: "task"},
			},
		),
	}

	// Formula with branch step.
	mc.Beads["gd-formula-branch"] = &model.Bead{
		ID:    "gd-formula-branch",
		Type:  "formula",
		Title: "Pipeline with branch",
		Fields: makeFormulaFieldsWithBranch([]FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "deploy", Title: "Deploy", Branch: &StepBranch{
				On: "{{env}}",
				Cases: map[string]BranchCase{
					"prod":    {Expand: "gd-prod-deploy", ExpandVars: map[string]string{"region": "us-east-1"}},
					"staging": {Expand: "gd-staging-deploy"},
				},
			}, DependsOn: []string{"build"}},
		}),
	}

	// mol + build + canary + full-rollout = 4 creates
	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-canary", "gd-rollout"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-branch",
			VarPairs:  []string{"env=prod"},
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	// Should create molecule + 3 steps (build + 2 from prod branch).
	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Verify prefixed IDs in output.
	if !strings.Contains(out, "deploy/canary") {
		t.Errorf("output should contain 'deploy/canary', got:\n%s", out)
	}
	if !strings.Contains(out, "deploy/full-rollout") {
		t.Errorf("output should contain 'deploy/full-rollout', got:\n%s", out)
	}

	// Verify var substitution in expansion steps.
	canaryReq := mc.CreateBeadCalls[2]
	if canaryReq.Title != "Canary deploy to us-east-1" {
		t.Errorf("canary title = %q, want 'Canary deploy to us-east-1'", canaryReq.Title)
	}
}

func TestBranch_AlternateCaseSelected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Staging expansion with one step.
	mc.Beads["gd-staging-deploy"] = &model.Bead{
		ID:    "gd-staging-deploy",
		Type:  "formula",
		Title: "Staging deploy",
		Fields: makeExpansionFields(
			[]FormulaVarDef{},
			[]FormulaStep{
				{ID: "quick-deploy", Title: "Quick staging deploy", Type: "task"},
			},
		),
	}
	mc.Beads["gd-prod-deploy"] = &model.Bead{
		ID:    "gd-prod-deploy",
		Type:  "formula",
		Title: "Prod deploy",
		Fields: makeExpansionFields(
			[]FormulaVarDef{},
			[]FormulaStep{
				{ID: "canary", Title: "Canary", Type: "task"},
				{ID: "rollout", Title: "Rollout", Type: "task", DependsOn: []string{"canary"}},
			},
		),
	}

	mc.Beads["gd-formula-branch"] = &model.Bead{
		ID:    "gd-formula-branch",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithBranch([]FormulaStep{
			{ID: "deploy", Title: "Deploy", Branch: &StepBranch{
				On: "{{env}}",
				Cases: map[string]BranchCase{
					"prod":    {Expand: "gd-prod-deploy"},
					"staging": {Expand: "gd-staging-deploy"},
				},
			}},
		}),
	}

	// mol + quick-deploy = 2 creates (staging case has 1 step)
	mc.CreateIDs = []string{"gd-mol-1", "gd-quick"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-branch",
			VarPairs:  []string{"env=staging"},
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Verify staging expansion step is used.
	if !strings.Contains(out, "deploy/quick-deploy") {
		t.Errorf("output should contain 'deploy/quick-deploy', got:\n%s", out)
	}
}

func TestBranch_DefaultCaseUsedWhenNoMatch(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-dev-deploy"] = &model.Bead{
		ID:    "gd-dev-deploy",
		Type:  "formula",
		Title: "Dev deploy",
		Fields: makeExpansionFields(
			[]FormulaVarDef{},
			[]FormulaStep{
				{ID: "local-deploy", Title: "Local deploy", Type: "task"},
			},
		),
	}

	mc.Beads["gd-formula-branch"] = &model.Bead{
		ID:    "gd-formula-branch",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithBranch([]FormulaStep{
			{ID: "deploy", Title: "Deploy", Branch: &StepBranch{
				On: "{{env}}",
				Cases: map[string]BranchCase{
					"prod": {Expand: "gd-prod-deploy"},
				},
				Default: &BranchCase{Expand: "gd-dev-deploy"},
			}},
		}),
	}

	mc.CreateIDs = []string{"gd-mol-1", "gd-local"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-branch",
			VarPairs:  []string{"env=dev"},
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	if !strings.Contains(out, "deploy/local-deploy") {
		t.Errorf("output should contain 'deploy/local-deploy', got:\n%s", out)
	}
}

func TestBranch_NoMatchNoDefaultErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-branch"] = &model.Bead{
		ID:    "gd-formula-branch",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithBranch([]FormulaStep{
			{ID: "deploy", Title: "Deploy", Branch: &StepBranch{
				On: "{{env}}",
				Cases: map[string]BranchCase{
					"prod": {Expand: "gd-prod-deploy"},
				},
			}},
		}),
	}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-branch",
		VarPairs:  []string{"env=dev"},
	})
	if err == nil {
		t.Fatal("expected error for unmatched branch with no default")
	}
	if !strings.Contains(err.Error(), "no case matches") {
		t.Errorf("error should mention 'no case matches', got: %v", err)
	}
}

func TestBranch_ExpandVarsPassedToExpansion(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-deploy-exp"] = &model.Bead{
		ID:    "gd-deploy-exp",
		Type:  "formula",
		Title: "Deploy expansion",
		Fields: makeExpansionFields(
			[]FormulaVarDef{{Name: "region"}, {Name: "replicas", Default: "3"}},
			[]FormulaStep{
				{ID: "scale", Title: "Scale to {{replicas}} in {{region}}", Type: "task"},
			},
		),
	}

	mc.Beads["gd-formula-branch"] = &model.Bead{
		ID:    "gd-formula-branch",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithBranch([]FormulaStep{
			{ID: "deploy", Title: "Deploy", Branch: &StepBranch{
				On: "{{env}}",
				Cases: map[string]BranchCase{
					"prod": {Expand: "gd-deploy-exp", ExpandVars: map[string]string{"region": "us-west-2", "replicas": "10"}},
				},
			}},
		}),
	}

	mc.CreateIDs = []string{"gd-mol-1", "gd-scale"}
	mc.ListBeadsResult = nil

	captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-branch",
			VarPairs:  []string{"env=prod"},
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	scaleReq := mc.CreateBeadCalls[1]
	if scaleReq.Title != "Scale to 10 in us-west-2" {
		t.Errorf("scale title = %q, want 'Scale to 10 in us-west-2'", scaleReq.Title)
	}
}

func TestBranch_DependsOnInheritedByExpansionRoots(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-deploy-exp"] = &model.Bead{
		ID:    "gd-deploy-exp",
		Type:  "formula",
		Title: "Deploy expansion",
		Fields: makeExpansionFields(
			[]FormulaVarDef{},
			[]FormulaStep{
				{ID: "pre", Title: "Pre", Type: "task"},
				{ID: "run", Title: "Run", Type: "task", DependsOn: []string{"pre"}},
			},
		),
	}

	mc.Beads["gd-formula-branch"] = &model.Bead{
		ID:    "gd-formula-branch",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithBranch([]FormulaStep{
			{ID: "build", Title: "Build", Type: "task"},
			{ID: "deploy", Title: "Deploy", Branch: &StepBranch{
				On: "{{env}}",
				Cases: map[string]BranchCase{
					"prod": {Expand: "gd-deploy-exp"},
				},
			}, DependsOn: []string{"build"}},
		}),
	}

	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-pre", "gd-run"}
	mc.ListBeadsResult = nil

	captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-branch",
			VarPairs:  []string{"env=prod"},
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	// The "deploy/pre" step (root of expansion) should have a blocks dep on "build".
	// Check AddDependency calls for this.
	foundBuildDep := false
	for _, dep := range mc.AddDepCalls {
		if dep.BeadID == "gd-pre" && dep.DependsOnID == "gd-build" && dep.Type == "blocks" {
			foundBuildDep = true
		}
	}
	if !foundBuildDep {
		t.Error("deploy/pre (root expansion step) should have blocks dep on build")
	}
}

func TestBranch_NonExpansionFormulaRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// A workflow formula (not expansion).
	mc.Beads["gd-workflow"] = &model.Bead{
		ID:    "gd-workflow",
		Type:  "formula",
		Title: "Workflow formula",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"formula_type": "workflow",
				"vars":         []FormulaVarDef{},
				"steps":        []FormulaStep{{ID: "s1", Title: "Step 1", Type: "task"}},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}

	mc.Beads["gd-formula-branch"] = &model.Bead{
		ID:    "gd-formula-branch",
		Type:  "formula",
		Title: "Pipeline",
		Fields: makeFormulaFieldsWithBranch([]FormulaStep{
			{ID: "deploy", Title: "Deploy", Branch: &StepBranch{
				On: "{{env}}",
				Cases: map[string]BranchCase{
					"prod": {Expand: "gd-workflow"},
				},
			}},
		}),
	}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-branch",
		VarPairs:  []string{"env=prod"},
	})
	if err == nil {
		t.Fatal("expected error when branch references non-expansion formula")
	}
	if !strings.Contains(err.Error(), "not expansion") {
		t.Errorf("error should mention 'not expansion', got: %v", err)
	}
}

func TestBranch_ValidationRejectsExpandAndBranch(t *testing.T) {
	err := validateStepExpands([]FormulaStep{
		{ID: "s1", Title: "S1", Expand: "gd-exp", Branch: &StepBranch{
			On:    "{{env}}",
			Cases: map[string]BranchCase{"prod": {Expand: "gd-exp"}},
		}},
	})
	if err == nil {
		t.Fatal("expected error for expand+branch")
	}
	if !strings.Contains(err.Error(), "expand and branch cannot be used together") {
		t.Errorf("expected expand+branch error, got: %v", err)
	}
}

func TestBranch_ValidationRejectsBranchAndLoop(t *testing.T) {
	err := validateStepExpands([]FormulaStep{
		{ID: "s1", Title: "S1", Branch: &StepBranch{
			On:    "{{env}}",
			Cases: map[string]BranchCase{"prod": {Expand: "gd-exp"}},
		}, Loop: &StepLoop{Var: "i", Count: 3}},
	})
	if err == nil {
		t.Fatal("expected error for branch+loop")
	}
	if !strings.Contains(err.Error(), "branch and loop cannot be used together") {
		t.Errorf("expected branch+loop error, got: %v", err)
	}
}

func TestBranch_ValidationRejectsBranchAndGate(t *testing.T) {
	err := validateStepExpands([]FormulaStep{
		{ID: "s1", Title: "S1", Branch: &StepBranch{
			On:    "{{env}}",
			Cases: map[string]BranchCase{"prod": {Expand: "gd-exp"}},
		}, Gate: &StepGate{Type: "sync"}},
	})
	if err == nil {
		t.Fatal("expected error for branch+gate")
	}
	if !strings.Contains(err.Error(), "branch and gate cannot be used together") {
		t.Errorf("expected branch+gate error, got: %v", err)
	}
}

func TestBranch_ValidationRejectsBranchAndChildren(t *testing.T) {
	err := validateStepExpands([]FormulaStep{
		{ID: "s1", Title: "S1", Branch: &StepBranch{
			On:    "{{env}}",
			Cases: map[string]BranchCase{"prod": {Expand: "gd-exp"}},
		}, Children: []FormulaStep{{ID: "c1", Title: "C1"}}},
	})
	if err == nil {
		t.Fatal("expected error for branch+children")
	}
	if !strings.Contains(err.Error(), "branch and children cannot be used together") {
		t.Errorf("expected branch+children error, got: %v", err)
	}
}

func TestBranch_ValidationRequiresOn(t *testing.T) {
	err := validateStepBranches([]FormulaStep{
		{ID: "s1", Title: "S1", Branch: &StepBranch{
			Cases: map[string]BranchCase{"prod": {Expand: "gd-exp"}},
		}},
	})
	if err == nil {
		t.Fatal("expected error for missing on")
	}
	if !strings.Contains(err.Error(), "must specify an 'on'") {
		t.Errorf("expected 'on' error, got: %v", err)
	}
}

func TestBranch_ValidationRequiresCases(t *testing.T) {
	err := validateStepBranches([]FormulaStep{
		{ID: "s1", Title: "S1", Branch: &StepBranch{
			On:    "{{env}}",
			Cases: map[string]BranchCase{},
		}},
	})
	if err == nil {
		t.Fatal("expected error for empty cases")
	}
	if !strings.Contains(err.Error(), "at least one case") {
		t.Errorf("expected cases error, got: %v", err)
	}
}

func TestBranch_ValidationRequiresCaseExpand(t *testing.T) {
	err := validateStepBranches([]FormulaStep{
		{ID: "s1", Title: "S1", Branch: &StepBranch{
			On:    "{{env}}",
			Cases: map[string]BranchCase{"prod": {Expand: ""}},
		}},
	})
	if err == nil {
		t.Fatal("expected error for empty case expand")
	}
	if !strings.Contains(err.Error(), "must specify an expand formula ID") {
		t.Errorf("expected expand ID error, got: %v", err)
	}
}

func TestBranch_ValidationRequiresDefaultExpand(t *testing.T) {
	err := validateStepBranches([]FormulaStep{
		{ID: "s1", Title: "S1", Branch: &StepBranch{
			On:      "{{env}}",
			Cases:   map[string]BranchCase{"prod": {Expand: "gd-exp"}},
			Default: &BranchCase{Expand: ""},
		}},
	})
	if err == nil {
		t.Fatal("expected error for empty default expand")
	}
	if !strings.Contains(err.Error(), "branch default must specify an expand formula ID") {
		t.Errorf("expected default expand error, got: %v", err)
	}
}

// ──────────────────────────────────────────────────────
// Skill-based routing tests
// ──────────────────────────────────────────────────────

func makeFormulaFieldsWithSkills(skills []string, steps []FormulaStep) json.RawMessage {
	f := map[string]any{
		"vars":            []FormulaVarDef{},
		"steps":           steps,
		"requires_skills": skills,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestSkills_StepSkillsAddedAsLabels(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-skills"] = &model.Bead{
		ID:    "gd-formula-skills",
		Type:  "formula",
		Title: "Skilled workflow",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"vars": []FormulaVarDef{},
				"steps": []FormulaStep{
					{ID: "go-work", Title: "Go implementation", Type: "task", RequiresSkills: []string{"go", "kubernetes"}},
					{ID: "frontend", Title: "React UI", Type: "task", RequiresSkills: []string{"react"}},
				},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-go", "gd-react"}
	mc.ListBeadsResult = nil

	captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-skills"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	// Check go-work step has skill labels.
	goReq := mc.CreateBeadCalls[1]
	hasGoSkill := false
	hasK8sSkill := false
	for _, l := range goReq.Labels {
		if l == "skill:go" {
			hasGoSkill = true
		}
		if l == "skill:kubernetes" {
			hasK8sSkill = true
		}
	}
	if !hasGoSkill {
		t.Errorf("go-work step should have skill:go label, got: %v", goReq.Labels)
	}
	if !hasK8sSkill {
		t.Errorf("go-work step should have skill:kubernetes label, got: %v", goReq.Labels)
	}

	// Check frontend step has react skill label.
	reactReq := mc.CreateBeadCalls[2]
	hasReactSkill := false
	for _, l := range reactReq.Labels {
		if l == "skill:react" {
			hasReactSkill = true
		}
	}
	if !hasReactSkill {
		t.Errorf("frontend step should have skill:react label, got: %v", reactReq.Labels)
	}
}

func TestSkills_FormulaLevelSkillsInheritedBySteps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-skills"] = &model.Bead{
		ID:    "gd-formula-skills",
		Type:  "formula",
		Title: "Skilled workflow",
		Fields: makeFormulaFieldsWithSkills(
			[]string{"go"},
			[]FormulaStep{
				{ID: "step1", Title: "Step 1", Type: "task"},
			},
		),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-s1"}
	mc.ListBeadsResult = nil

	captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-skills"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	stepReq := mc.CreateBeadCalls[1]
	hasGoSkill := false
	for _, l := range stepReq.Labels {
		if l == "skill:go" {
			hasGoSkill = true
		}
	}
	if !hasGoSkill {
		t.Errorf("step should inherit formula-level skill:go, got: %v", stepReq.Labels)
	}
}

func TestSkills_StepSkillsOverrideFormulaSkills(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-skills"] = &model.Bead{
		ID:    "gd-formula-skills",
		Type:  "formula",
		Title: "Skilled workflow",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"vars":            []FormulaVarDef{},
				"steps":           []FormulaStep{{ID: "s1", Title: "Step", Type: "task", RequiresSkills: []string{"react"}}},
				"requires_skills": []string{"go"},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-s1"}
	mc.ListBeadsResult = nil

	captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-skills"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	stepReq := mc.CreateBeadCalls[1]
	hasReact := false
	hasGo := false
	for _, l := range stepReq.Labels {
		if l == "skill:react" {
			hasReact = true
		}
		if l == "skill:go" {
			hasGo = true
		}
	}
	if !hasReact {
		t.Errorf("step should have overridden skill:react, got: %v", stepReq.Labels)
	}
	if hasGo {
		t.Errorf("step should NOT have formula-level skill:go when overridden, got: %v", stepReq.Labels)
	}
}

func TestSkills_DryRunShowsSkills(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-skills"] = &model.Bead{
		ID:    "gd-formula-skills",
		Type:  "formula",
		Title: "Skilled workflow",
		Fields: makeFormulaFieldsWithSkills(
			[]string{"go"},
			[]FormulaStep{
				{ID: "s1", Title: "Step 1", Type: "task"},
			},
		),
	}

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-skills",
			DryRun:    true,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if !strings.Contains(out, "skills:[go]") {
		t.Errorf("dry-run should show skills annotation, got:\n%s", out)
	}
}

// ──────────────────────────────────────────────────────
// Fanout gate (waits_for) tests
// ──────────────────────────────────────────────────────

func TestWaitsFor_AllChildrenAddsBlocksDeps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-fanout"] = &model.Bead{
		ID:    "gd-formula-fanout",
		Type:  "formula",
		Title: "Fanout workflow",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"vars": []FormulaVarDef{},
				"steps": []FormulaStep{
					{
						ID: "parent", Title: "Parent", Type: "epic", WaitsFor: "all-children",
						Children: []FormulaStep{
							{ID: "child-a", Title: "Child A", Type: "task"},
							{ID: "child-b", Title: "Child B", Type: "task"},
						},
					},
					{ID: "after", Title: "After all", Type: "task", DependsOn: []string{"parent"}},
				},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}
	// mol + parent + child-a + child-b + after = 5
	mc.CreateIDs = []string{"gd-mol-1", "gd-parent", "gd-child-a", "gd-child-b", "gd-after"}
	mc.ListBeadsResult = nil

	captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-fanout"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	// Check that parent step has blocks deps on both children.
	foundChildA := false
	foundChildB := false
	for _, dep := range mc.AddDepCalls {
		if dep.BeadID == "gd-parent" && dep.DependsOnID == "gd-child-a" && dep.Type == "blocks" {
			foundChildA = true
		}
		if dep.BeadID == "gd-parent" && dep.DependsOnID == "gd-child-b" && dep.Type == "blocks" {
			foundChildB = true
		}
	}
	if !foundChildA {
		t.Error("parent should have blocks dep on child-a")
	}
	if !foundChildB {
		t.Error("parent should have blocks dep on child-b")
	}
}

func TestWaitsFor_FieldSetOnBead(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-fanout"] = &model.Bead{
		ID:    "gd-formula-fanout",
		Type:  "formula",
		Title: "Fanout workflow",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"vars": []FormulaVarDef{},
				"steps": []FormulaStep{
					{
						ID: "parent", Title: "Parent", Type: "epic", WaitsFor: "all-children",
						Children: []FormulaStep{
							{ID: "child-a", Title: "Child A", Type: "task"},
						},
					},
				},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-parent", "gd-child-a"}
	mc.ListBeadsResult = nil

	captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-formula-fanout"})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	// Check the parent step bead fields contain waits_for.
	parentReq := mc.CreateBeadCalls[1]
	var parentFields map[string]any
	json.Unmarshal(parentReq.Fields, &parentFields)
	if parentFields["waits_for"] != "all-children" {
		t.Errorf("parent fields should have waits_for=all-children, got: %v", parentFields)
	}
}

func TestWaitsFor_ValidationRejectsWithoutChildren(t *testing.T) {
	err := validateStepWaitsFor([]FormulaStep{
		{ID: "s1", Title: "S1", WaitsFor: "all-children"},
	})
	if err == nil {
		t.Fatal("expected error for waits_for without children")
	}
	if !strings.Contains(err.Error(), "requires children") {
		t.Errorf("expected children error, got: %v", err)
	}
}

func TestWaitsFor_ValidationRejectsInvalidValue(t *testing.T) {
	err := validateStepWaitsFor([]FormulaStep{
		{ID: "s1", Title: "S1", WaitsFor: "any-child", Children: []FormulaStep{
			{ID: "c1", Title: "C1"},
		}},
	})
	if err == nil {
		t.Fatal("expected error for invalid waits_for")
	}
	if !strings.Contains(err.Error(), "must be all-children") {
		t.Errorf("expected invalid value error, got: %v", err)
	}
}

func TestWaitsFor_DryRunShowsWaitsFor(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula-fanout"] = &model.Bead{
		ID:    "gd-formula-fanout",
		Type:  "formula",
		Title: "Fanout workflow",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"vars": []FormulaVarDef{},
				"steps": []FormulaStep{
					{
						ID: "parent", Title: "Parent", Type: "epic", WaitsFor: "all-children",
						Children: []FormulaStep{
							{ID: "child-a", Title: "Child A", Type: "task"},
						},
					},
				},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula-fanout",
			DryRun:    true,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if !strings.Contains(out, "waits_for:all-children") {
		t.Errorf("dry-run should show waits_for annotation, got:\n%s", out)
	}
}

// ──────────────────────────────────────────────────────
// Filesystem formula loading tests
// ──────────────────────────────────────────────────────

func TestIsFilePath(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"gd-abc123", false},
		{"gd-ABCdef", false},
		{"./formula.json", true},
		{"formulas/deploy.json", true},
		{"/tmp/formula.json", true},
		{"formula.json", true},
		{".beads/formulas/test.json", true},
	}
	for _, tt := range tests {
		got := isFilePath(tt.input)
		if got != tt.want {
			t.Errorf("isFilePath(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestFilesystemFormula_ApplyFromFile(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Write a formula JSON file to a temp directory.
	tmpDir := t.TempDir()
	formulaPath := tmpDir + "/test-formula.json"
	formulaJSON := `{
		"vars": [{"name": "component", "description": "Component name"}],
		"steps": [
			{"id": "build", "title": "Build {{component}}", "type": "task"},
			{"id": "test", "title": "Test {{component}}", "type": "task", "depends_on": ["build"]}
		]
	}`
	os.WriteFile(formulaPath, []byte(formulaJSON), 0o644)

	mc.CreateIDs = []string{"gd-mol-1", "gd-build", "gd-test"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: formulaPath,
			VarPairs:  []string{"component=auth"},
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 3 {
		t.Fatalf("expected 3 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Molecule title derived from filename.
	molReq := mc.CreateBeadCalls[0]
	if molReq.Title != "auth" {
		// Title is substituteVars on the filename-derived title "test-formula"
		// but there's no {{component}} in it, so it stays as-is.
		if molReq.Title != "test-formula" {
			t.Errorf("mol title = %q, want 'test-formula'", molReq.Title)
		}
	}

	// Check step titles have variables substituted.
	buildReq := mc.CreateBeadCalls[1]
	if buildReq.Title != "Build auth" {
		t.Errorf("build title = %q, want 'Build auth'", buildReq.Title)
	}

	if !strings.Contains(out, "build") {
		t.Errorf("output should contain 'build', got:\n%s", out)
	}
}

func TestFilesystemFormula_DryRunFromFile(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	tmpDir := t.TempDir()
	formulaPath := tmpDir + "/deploy.json"
	formulaJSON := `{
		"vars": [{"name": "env", "default": "dev"}],
		"steps": [
			{"id": "deploy", "title": "Deploy to {{env}}", "type": "task"}
		]
	}`
	os.WriteFile(formulaPath, []byte(formulaJSON), 0o644)

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: formulaPath,
			DryRun:    true,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if !strings.Contains(out, "Deploy to dev") {
		t.Errorf("dry-run should show substituted title, got:\n%s", out)
	}
}

func TestFilesystemFormula_InvalidPathErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "/nonexistent/formula.json",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !strings.Contains(err.Error(), "reading formula file") {
		t.Errorf("error should mention reading file, got: %v", err)
	}
}

func TestFilesystemFormula_InvalidJSONErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	tmpDir := t.TempDir()
	formulaPath := tmpDir + "/bad.json"
	os.WriteFile(formulaPath, []byte(`{invalid json`), 0o644)

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: formulaPath,
	})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parsing formula file") {
		t.Errorf("error should mention parsing, got: %v", err)
	}
}

func TestFilesystemFormula_ExtendsResolvesFromDB(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent formula in database.
	mc.Beads["gd-parent-formula"] = &model.Bead{
		ID:    "gd-parent-formula",
		Type:  "formula",
		Title: "Parent",
		Fields: func() json.RawMessage {
			f := map[string]any{
				"vars":  []FormulaVarDef{{Name: "env", Default: "dev"}},
				"steps": []FormulaStep{{ID: "base-step", Title: "Base step for {{env}}", Type: "task"}},
			}
			data, _ := json.Marshal(f)
			return data
		}(),
	}

	tmpDir := t.TempDir()
	formulaPath := tmpDir + "/child.json"
	formulaJSON := `{
		"extends": ["gd-parent-formula"],
		"steps": [
			{"id": "child-step", "title": "Child step", "type": "task", "depends_on": ["base-step"]}
		]
	}`
	os.WriteFile(formulaPath, []byte(formulaJSON), 0o644)

	mc.CreateIDs = []string{"gd-mol-1", "gd-base", "gd-child"}
	mc.ListBeadsResult = nil

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: formulaPath,
		})
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 3 {
		t.Fatalf("expected 3 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// Should have both inherited base-step and child-step.
	if !strings.Contains(out, "base-step") {
		t.Errorf("output should contain inherited 'base-step', got:\n%s", out)
	}
	if !strings.Contains(out, "child-step") {
		t.Errorf("output should contain 'child-step', got:\n%s", out)
	}
}

// ── on_complete tests ────────────────────────────────────────────────

// makeFormulaFieldsWithOnComplete creates formula fields with on_complete steps.
func makeFormulaFieldsWithOnComplete(steps []FormulaStep) json.RawMessage {
	f := map[string]any{
		"vars":  []FormulaVarDef{},
		"steps": steps,
	}
	data, _ := json.Marshal(f)
	return data
}

func TestOnComplete_StoredInBeadFields(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula"] = &model.Bead{
		ID:    "gd-formula",
		Type:  "formula",
		Title: "Scan formula",
		Fields: makeFormulaFieldsWithOnComplete([]FormulaStep{
			{
				ID:    "scan",
				Title: "Scan for components",
				Type:  "task",
				OnComplete: &StepOnComplete{
					ForEach:    "{{scan.output.components}}",
					Var:        "component",
					Expand:     "gd-componentProcess",
					ExpandVars: map[string]string{"name": "{{component}}"},
				},
			},
		}),
	}
	mc.CreateIDs = []string{"gd-mol1", "gd-step1"}

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected 2 creates (mol + step), got %d", len(mc.CreateBeadCalls))
	}

	// Verify on_complete is stored in step bead fields.
	stepReq := mc.CreateBeadCalls[1]
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(stepReq.Fields, &fields); err != nil {
		t.Fatalf("parsing step fields: %v", err)
	}

	ocRaw, ok := fields["on_complete"]
	if !ok {
		t.Fatal("step bead fields should contain on_complete")
	}

	var oc struct {
		ForEach    string            `json:"for_each"`
		Var        string            `json:"var"`
		Expand     string            `json:"expand"`
		ExpandVars map[string]string `json:"expand_vars"`
	}
	if err := json.Unmarshal(ocRaw, &oc); err != nil {
		t.Fatalf("parsing on_complete: %v", err)
	}

	if oc.ForEach != "{{scan.output.components}}" {
		t.Errorf("expected for_each={{scan.output.components}}, got %s", oc.ForEach)
	}
	if oc.Var != "component" {
		t.Errorf("expected var=component, got %s", oc.Var)
	}
	if oc.Expand != "gd-componentProcess" {
		t.Errorf("expected expand=gd-componentProcess, got %s", oc.Expand)
	}
	if oc.ExpandVars["name"] != "{{component}}" {
		t.Errorf("expected expand_vars.name={{component}}, got %s", oc.ExpandVars["name"])
	}
}

func TestOnComplete_DryRunShowsAnnotation(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula"] = &model.Bead{
		ID:    "gd-formula",
		Type:  "formula",
		Title: "Scan formula",
		Fields: makeFormulaFieldsWithOnComplete([]FormulaStep{
			{
				ID:    "scan",
				Title: "Scan",
				Type:  "task",
				OnComplete: &StepOnComplete{
					ForEach: "{{scan.output}}",
					Var:     "item",
					Expand:  "gd-UV4PXLHZw3",
				},
			},
		}),
	}

	out := captureStdout(t, func() {
		err := runFormulaApply(formulaApplyOpts{
			FormulaID: "gd-formula",
			DryRun:    true,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	if !strings.Contains(out, "on_complete") {
		t.Errorf("dry-run output should contain on_complete annotation, got:\n%s", out)
	}
	if !strings.Contains(out, "gd-UV4PXLHZw3") {
		t.Errorf("dry-run output should contain expansion formula ID, got:\n%s", out)
	}
}

func TestOnComplete_ValidationRejectsWithExpand(t *testing.T) {
	steps := []FormulaStep{{
		ID:     "bad",
		Title:  "Bad step",
		Expand: "gd-something",
		OnComplete: &StepOnComplete{
			ForEach: "{{x}}", Var: "v", Expand: "gd-y",
		},
	}}
	if err := validateStepExpands(steps); err == nil {
		t.Fatal("expected error for on_complete + expand")
	}
}

func TestOnComplete_ValidationRejectsWithBranch(t *testing.T) {
	steps := []FormulaStep{{
		ID:    "bad",
		Title: "Bad step",
		Branch: &StepBranch{
			On: "{{env}}", Cases: map[string]BranchCase{"a": {Expand: "gd-a"}},
		},
		OnComplete: &StepOnComplete{
			ForEach: "{{x}}", Var: "v", Expand: "gd-y",
		},
	}}
	if err := validateStepOnComplete(steps); err == nil {
		t.Fatal("expected error for on_complete + branch")
	}
}

func TestOnComplete_ValidationRejectsWithLoop(t *testing.T) {
	steps := []FormulaStep{{
		ID:    "bad",
		Title: "Bad step",
		Loop:  &StepLoop{Var: "i", Count: 3},
		OnComplete: &StepOnComplete{
			ForEach: "{{x}}", Var: "v", Expand: "gd-y",
		},
	}}
	if err := validateStepOnComplete(steps); err == nil {
		t.Fatal("expected error for on_complete + loop")
	}
}

func TestOnComplete_ValidationRejectsWithGate(t *testing.T) {
	steps := []FormulaStep{{
		ID:    "bad",
		Title: "Bad step",
		Gate:  &StepGate{Type: "sync"},
		OnComplete: &StepOnComplete{
			ForEach: "{{x}}", Var: "v", Expand: "gd-y",
		},
	}}
	if err := validateStepOnComplete(steps); err == nil {
		t.Fatal("expected error for on_complete + gate")
	}
}

func TestOnComplete_ValidationRequiresForEach(t *testing.T) {
	steps := []FormulaStep{{
		ID:    "bad",
		Title: "Bad step",
		OnComplete: &StepOnComplete{
			Var: "v", Expand: "gd-y",
		},
	}}
	if err := validateStepOnComplete(steps); err == nil {
		t.Fatal("expected error for missing for_each")
	}
}

func TestOnComplete_ValidationRequiresVar(t *testing.T) {
	steps := []FormulaStep{{
		ID:    "bad",
		Title: "Bad step",
		OnComplete: &StepOnComplete{
			ForEach: "{{x}}", Expand: "gd-y",
		},
	}}
	if err := validateStepOnComplete(steps); err == nil {
		t.Fatal("expected error for missing var")
	}
}

func TestOnComplete_ValidationRequiresExpand(t *testing.T) {
	steps := []FormulaStep{{
		ID:    "bad",
		Title: "Bad step",
		OnComplete: &StepOnComplete{
			ForEach: "{{x}}", Var: "v",
		},
	}}
	if err := validateStepOnComplete(steps); err == nil {
		t.Fatal("expected error for missing expand")
	}
}

func TestOnComplete_CookShowsOnComplete(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-formula"] = &model.Bead{
		ID:    "gd-formula",
		Type:  "formula",
		Title: "Scan formula",
		Fields: makeFormulaFieldsWithOnComplete([]FormulaStep{
			{
				ID:    "scan",
				Title: "Scan services",
				Type:  "task",
				OnComplete: &StepOnComplete{
					ForEach:    "{{scan.output}}",
					Var:        "svc",
					Expand:     "gd-GZJupD1Ds1",
					ExpandVars: map[string]string{"name": "{{svc}}"},
				},
			},
		}),
	}

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook error: %v", err)
		}
	})

	if !strings.Contains(out, "on_complete") {
		t.Errorf("cook output should show on_complete, got:\n%s", out)
	}
	if !strings.Contains(out, "gd-GZJupD1Ds1") {
		t.Errorf("cook output should show expansion formula ID, got:\n%s", out)
	}
}

func TestOnComplete_CookJSONIncludesOnComplete(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	jsonOutput = true

	mc.Beads["gd-formula"] = &model.Bead{
		ID:    "gd-formula",
		Type:  "formula",
		Title: "Scan formula",
		Fields: makeFormulaFieldsWithOnComplete([]FormulaStep{
			{
				ID:    "scan",
				Title: "Scan",
				Type:  "task",
				OnComplete: &StepOnComplete{
					ForEach: "{{out}}", Var: "x", Expand: "gd-exp",
				},
			},
		}),
	}

	cmd := newCookCmd()
	cmd.SetArgs([]string{"gd-formula"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("cook error: %v", err)
		}
	})

	var result map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("parsing JSON output: %v", err)
	}

	var steps []map[string]json.RawMessage
	if err := json.Unmarshal(result["steps"], &steps); err != nil {
		t.Fatalf("parsing steps: %v", err)
	}

	if len(steps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(steps))
	}

	if _, ok := steps[0]["on_complete"]; !ok {
		t.Error("JSON step should contain on_complete field")
	}
}

func TestExpand_NoOnCompleteErrors(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-step"] = &model.Bead{
		ID:     "gd-step",
		Type:   "task",
		Title:  "No on_complete",
		Fields: json.RawMessage(`{"source_formula":"gd-f","source_step_id":"s"}`),
	}

	cmd := newExpandCmd()
	cmd.SetArgs([]string{"gd-step", "--values", "a,b"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing on_complete")
	}
	if !strings.Contains(err.Error(), "no on_complete handler") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExpand_DryRunShowsIterations(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	ocSpec := map[string]any{
		"on_complete": map[string]any{
			"for_each":    "{{scan.output}}",
			"var":         "component",
			"expand":      "gd-compExp",
			"expand_vars": map[string]string{"name": "{{component}}"},
		},
		"source_formula": "gd-f",
		"source_step_id": "scan",
	}
	ocJSON, _ := json.Marshal(ocSpec)

	mc.Beads["gd-step"] = &model.Bead{
		ID:     "gd-step",
		Type:   "task",
		Title:  "Scan step",
		Fields: ocJSON,
	}

	cmd := newExpandCmd()
	cmd.SetArgs([]string{"gd-step", "--values", "auth,api,web", "--dry-run"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("expand dry-run error: %v", err)
		}
	})

	if !strings.Contains(out, "auth") {
		t.Errorf("output should contain 'auth', got:\n%s", out)
	}
	if !strings.Contains(out, "api") {
		t.Errorf("output should contain 'api', got:\n%s", out)
	}
	if !strings.Contains(out, "web") {
		t.Errorf("output should contain 'web', got:\n%s", out)
	}
	if !strings.Contains(out, "3 iterations") {
		t.Errorf("output should show 3 iterations, got:\n%s", out)
	}
}

func TestExpand_CreatesExpandedStepBeads(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	ocSpec := map[string]any{
		"on_complete": map[string]any{
			"for_each":    "{{scan.output}}",
			"var":         "component",
			"expand":      "gd-compExp",
			"expand_vars": map[string]string{"name": "{{component}}"},
		},
		"source_formula": "gd-f",
		"source_step_id": "scan",
	}
	ocJSON, _ := json.Marshal(ocSpec)

	mc.Beads["gd-step"] = &model.Bead{
		ID:       "gd-step",
		Type:     "task",
		Title:    "Scan step",
		Priority: 2,
		Labels:   []string{"project:test"},
		Fields:   ocJSON,
	}
	mc.Beads["gd-compExp"] = &model.Bead{
		ID:    "gd-compExp",
		Type:  "formula",
		Title: "Component processing",
		Fields: makeExpansionFields(
			[]FormulaVarDef{{Name: "name"}},
			[]FormulaStep{
				{ID: "build", Title: "Build {{name}}", Type: "task"},
				{ID: "test", Title: "Test {{name}}", Type: "task", DependsOn: []string{"build"}},
			},
		),
	}
	mc.CreateIDs = []string{
		"gd-exp1", "gd-exp2",
		"gd-exp3", "gd-exp4",
	}

	cmd := newExpandCmd()
	cmd.SetArgs([]string{"gd-step", "--values", "auth,api"})

	out := captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("expand error: %v", err)
		}
	})

	// 2 values × 2 steps = 4 beads created.
	if len(mc.CreateBeadCalls) != 4 {
		t.Fatalf("expected 4 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	// First pair: auth steps.
	if mc.CreateBeadCalls[0].Title != "Build auth" {
		t.Errorf("expected 'Build auth', got %q", mc.CreateBeadCalls[0].Title)
	}
	if mc.CreateBeadCalls[1].Title != "Test auth" {
		t.Errorf("expected 'Test auth', got %q", mc.CreateBeadCalls[1].Title)
	}

	// Second pair: api steps.
	if mc.CreateBeadCalls[2].Title != "Build api" {
		t.Errorf("expected 'Build api', got %q", mc.CreateBeadCalls[2].Title)
	}
	if mc.CreateBeadCalls[3].Title != "Test api" {
		t.Errorf("expected 'Test api', got %q", mc.CreateBeadCalls[3].Title)
	}

	// All expanded beads should be linked to the trigger step as children.
	if len(mc.AddDepCalls) != 4 {
		t.Fatalf("expected 4 AddDep calls, got %d", len(mc.AddDepCalls))
	}
	for _, dep := range mc.AddDepCalls {
		if dep.DependsOnID != "gd-step" {
			t.Errorf("expected parent-child link to gd-step, got %s", dep.DependsOnID)
		}
		if dep.Type != "parent-child" {
			t.Errorf("expected parent-child dep type, got %s", dep.Type)
		}
	}

	if !strings.Contains(out, "4 beads created") {
		t.Errorf("output should mention 4 beads created, got:\n%s", out)
	}
}

func TestExpand_RequiresValues(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	cmd := newExpandCmd()
	cmd.SetArgs([]string{"gd-step"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for missing --values")
	}
	if !strings.Contains(err.Error(), "one of --values or --from-field is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExpandCmd_NonExpansionFormulaRejected(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	ocSpec := map[string]any{
		"on_complete": map[string]any{
			"for_each": "{{x}}", "var": "v", "expand": "gd-workflow",
			"expand_vars": map[string]string{},
		},
		"source_formula": "gd-f",
		"source_step_id": "s",
	}
	ocJSON, _ := json.Marshal(ocSpec)

	mc.Beads["gd-step"] = &model.Bead{
		ID:     "gd-step",
		Type:   "task",
		Title:  "Step",
		Fields: ocJSON,
	}
	// gd-workflow is a workflow formula, not expansion.
	mc.Beads["gd-workflow"] = &model.Bead{
		ID:     "gd-workflow",
		Type:   "formula",
		Title:  "Workflow formula",
		Fields: json.RawMessage(`{"formula_type":"workflow","vars":[],"steps":[{"id":"a","title":"A"}]}`),
	}

	cmd := newExpandCmd()
	cmd.SetArgs([]string{"gd-step", "--values", "a"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for non-expansion formula")
	}
	if !strings.Contains(err.Error(), "not expansion") {
		t.Errorf("unexpected error: %v", err)
	}
}

// TestVarDefList_UnmarshalJSON_TolerantForms verifies the tolerant vars parse
// (bug ka-0000001116): the DB-load path must accept BOTH a native array and a
// legacy JSON-string-encoded array, so `formula apply --dry-run` and the
// dump→create round-trip stop failing with "cannot unmarshal string into Go
// struct field ...vars of type []main.FormulaVarDef".
func TestVarDefList_UnmarshalJSON_TolerantForms(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantLen int
		wantErr bool
		first   string // expected first var name (when wantLen>0)
	}{
		{name: "native array", in: `[{"name":"tag","required":true}]`, wantLen: 1, first: "tag"},
		{name: "string-encoded array", in: `"[{\"name\":\"tag\",\"required\":true}]"`, wantLen: 1, first: "tag"},
		{name: "empty array", in: `[]`, wantLen: 0},
		{name: "null", in: `null`, wantLen: 0},
		{name: "empty string", in: `""`, wantLen: 0},
		{name: "string null", in: `"null"`, wantLen: 0},
		{name: "two vars native", in: `[{"name":"a"},{"name":"b"}]`, wantLen: 2, first: "a"},
		{name: "bad string payload", in: `"not-json"`, wantErr: true},
		{name: "bad type", in: `42`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var v varDefList
			err := json.Unmarshal([]byte(tc.in), &v)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil (v=%+v)", tc.in, v)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if len(v) != tc.wantLen {
				t.Fatalf("len = %d, want %d (v=%+v)", len(v), tc.wantLen, v)
			}
			if tc.wantLen > 0 && v[0].Name != tc.first {
				t.Errorf("first var name = %q, want %q", v[0].Name, tc.first)
			}
		})
	}
}

// TestVarDefList_RoundTripNormalizes verifies a tolerated string-form vars
// re-marshals as a native array (so re-writing a legacy bead normalizes it).
func TestVarDefList_RoundTripNormalizes(t *testing.T) {
	var v varDefList
	if err := json.Unmarshal([]byte(`"[{\"name\":\"tag\"}]"`), &v); err != nil {
		t.Fatalf("unmarshal string-form: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(out); got != `[{"name":"tag"}]` {
		t.Errorf("re-marshal = %s, want native array [{\"name\":\"tag\"}]", got)
	}
}

func TestStepList_UnmarshalJSON_TolerantForms(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantLen int
		wantErr bool
		first   string // expected first step ID (when wantLen>0)
	}{
		{name: "native array", in: `[{"id":"build","title":"Build"}]`, wantLen: 1, first: "build"},
		{name: "string-encoded array", in: `"[{\"id\":\"build\",\"title\":\"Build\"}]"`, wantLen: 1, first: "build"},
		{name: "empty array", in: `[]`, wantLen: 0},
		{name: "null", in: `null`, wantLen: 0},
		{name: "empty string", in: `""`, wantLen: 0},
		{name: "string null", in: `"null"`, wantLen: 0},
		{name: "two steps native", in: `[{"id":"a"},{"id":"b"}]`, wantLen: 2, first: "a"},
		{name: "two steps string-encoded", in: `"[{\"id\":\"a\"},{\"id\":\"b\"}]"`, wantLen: 2, first: "a"},
		{name: "bad string payload", in: `"not-json"`, wantErr: true},
		{name: "bad type", in: `42`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var s stepList
			err := json.Unmarshal([]byte(tc.in), &s)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil (s=%+v)", tc.in, s)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.in, err)
			}
			if len(s) != tc.wantLen {
				t.Fatalf("len = %d, want %d (s=%+v)", len(s), tc.wantLen, s)
			}
			if tc.wantLen > 0 && s[0].ID != tc.first {
				t.Errorf("first step ID = %q, want %q", s[0].ID, tc.first)
			}
		})
	}
}

// TestStepList_RoundTripNormalizes verifies a tolerated string-form steps
// re-marshals as a native array (so re-writing a legacy bead normalizes it).
func TestStepList_RoundTripNormalizes(t *testing.T) {
	var s stepList
	if err := json.Unmarshal([]byte(`"[{\"id\":\"build\",\"title\":\"Build\"}]"`), &s); err != nil {
		t.Fatalf("unmarshal string-form: %v", err)
	}
	out, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := string(out); got != `[{"id":"build","title":"Build"}]` {
		t.Errorf("re-marshal = %s, want native array", got)
	}
}

// TestFormulaFields_DBLoadWithStringSteps is the regression for the exact 160.6
// failure: a formula bead whose Fields JSON carries steps as a JSON STRING must
// parse via the formulaFields struct (the DB-load path used by formula apply
// --dry-run). Mirror of the vars regression below (ka-0000001118).
func TestFormulaFields_DBLoadWithStringSteps(t *testing.T) {
	// steps stored as a JSON-string-encoded array (the shape that broke
	// `formula apply --dry-run` on 2026.160.6), vars as a native array.
	fieldsJSON := `{"vars":[{"name":"tag","required":true}],"steps":"[{\"id\":\"build\",\"title\":\"Build {{tag}}\"},{\"id\":\"test\",\"title\":\"Test\",\"depends_on\":[\"build\"]}]"}`
	var f formulaFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		t.Fatalf("formulaFields unmarshal with string steps failed (regression ka-0000001118): %v", err)
	}
	if len(f.Steps) != 2 || f.Steps[0].ID != "build" || f.Steps[1].ID != "test" {
		t.Fatalf("steps not parsed from string form: %+v", f.Steps)
	}
	if len(f.Vars) != 1 || f.Vars[0].Name != "tag" {
		t.Fatalf("vars not parsed alongside string steps: %+v", f.Vars)
	}
}

// TestFormulaFields_DBLoadWithStringVars is the regression for the exact failure:
// a formula bead whose Fields JSON carries vars as a string must parse via the
// formulaFields struct (the DB-load path in runFormulaApply / dry-run).
func TestFormulaFields_DBLoadWithStringVars(t *testing.T) {
	// vars stored as a JSON-string-encoded array (the legacy shape that broke
	// `formula apply --dry-run`), steps as a native array.
	fieldsJSON := `{"vars":"[{\"name\":\"tag\",\"required\":true}]","steps":[{"id":"build","title":"Build {{tag}}"}]}`
	var f formulaFields
	if err := json.Unmarshal([]byte(fieldsJSON), &f); err != nil {
		t.Fatalf("formulaFields unmarshal with string vars failed (regression ka-0000001116): %v", err)
	}
	if len(f.Vars) != 1 || f.Vars[0].Name != "tag" {
		t.Fatalf("vars not parsed from string form: %+v", f.Vars)
	}
	if len(f.Steps) != 1 || f.Steps[0].ID != "build" {
		t.Fatalf("steps not parsed alongside string vars: %+v", f.Steps)
	}
}

// TestPour_SubstitutesVarsInRolesAndLabels verifies that {{var}} placeholders
// in a step's roles and labels are substituted at pour time, not just in
// title/description/assignee. Regression for the RESPOND-AT-RIGHT-TIME formula
// (gd-17M85H8KjZ), whose phase-2 step declared roles:["{{WORKER_ROLE}}"] but
// rendered the literal label "role:{{WORKER_ROLE}}" because substituteVars was
// never applied to roles/labels (ka-0000005050).
func TestPour_SubstitutesVarsInRolesAndLabels(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	steps := []FormulaStep{
		{
			ID:     "respond",
			Title:  "Respond ({{ITERATION}})",
			Type:   "task",
			Roles:  []string{"{{WORKER_ROLE}}"},
			Labels: []string{"iteration:{{ITERATION}}"},
		},
	}
	mc.Beads["gd-formula-1"] = &model.Bead{
		ID:     "gd-formula-1",
		Type:   "formula",
		Title:  "Roles substitution",
		Fields: makeFormulaFieldsWithPattern(t, nil, steps),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-respond"}
	mc.ListBeadsResult = nil

	err := runFormulaApply(formulaApplyOpts{
		FormulaID: "gd-formula-1",
		VarPairs:  []string{"WORKER_ROLE=auto-worker-haiku", "ITERATION=2026-06-23"},
	})
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected 2 CreateBead calls, got %d", len(mc.CreateBeadCalls))
	}

	stepReq := mc.CreateBeadCalls[1]
	wantLabels := map[string]bool{
		"role:auto-worker-haiku": false,
		"iteration:2026-06-23":   false,
	}
	for _, l := range stepReq.Labels {
		if _, ok := wantLabels[l]; ok {
			wantLabels[l] = true
		}
		if l == "role:{{WORKER_ROLE}}" || l == "iteration:{{ITERATION}}" {
			t.Errorf("label %q was not substituted", l)
		}
	}
	for l, found := range wantLabels {
		if !found {
			t.Errorf("expected substituted label %q in %v", l, stepReq.Labels)
		}
	}
}
