package main

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// stepBeadWith builds a step bead carrying an on_complete spec plus an
// arbitrary fields blob (typically including a dwf_result written by the
// closed step). The on_complete spec points at the given expansion formula.
func stepBeadWith(t *testing.T, id, expandID string, expandVars map[string]string, extra map[string]any) *model.Bead {
	t.Helper()
	fields := map[string]any{
		"source_formula": "gd-GhjJPkvpBG",
		"source_step_id": "scan",
		"on_complete": map[string]any{
			"for_each":    "{{step.output.succeeded}}",
			"var":         "component",
			"expand":      expandID,
			"expand_vars": expandVars,
		},
	}
	maps.Copy(fields, extra)
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal step fields: %v", err)
	}
	return &model.Bead{
		ID:       id,
		Title:    "Scan components",
		Type:     "task",
		Status:   model.StatusClosed,
		Priority: 2,
		Labels:   []string{"project:gardenio"},
		Fields:   raw,
	}
}

// expansionFormulaBead builds an expansion-type formula bead with one step
// whose title/description may carry {{component}} and {{step.output.X}} refs.
func expansionFormulaBead(t *testing.T, id string) *model.Bead {
	t.Helper()
	ff := formulaFields{
		FormulaType: "expansion",
		Steps: []FormulaStep{
			{
				ID:          "process",
				Title:       "Process {{component}} (run {{step.output.run_id}})",
				Description: "Handle {{component}} for run {{run_id}}",
				Type:        "task",
			},
		},
	}
	raw, err := json.Marshal(ff)
	if err != nil {
		t.Fatalf("marshal expansion formula: %v", err)
	}
	return &model.Bead{
		ID:     id,
		Title:  "component-process",
		Type:   "formula",
		Fields: raw,
	}
}

// dwfResult is a convenience for the dwf.result.v1 blob a host step writes
// back onto its own bead before closing.
func dwfResult(succeeded []string, runID string) map[string]any {
	return map[string]any{
		"dwf_result": map[string]any{
			"schema":    "dwf.result.v1",
			"succeeded": succeeded,
			"failed":    []any{},
			"total":     len(succeeded),
			"run_id":    runID,
		},
	}
}

// TestRunExpand_FromFieldResolvesArray verifies that --from-field
// dwf_result.succeeded resolves a closed step's structured-output array into
// the iteration values, creating one expanded step per array element.
func TestRunExpand_FromFieldResolvesArray(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	step := stepBeadWith(t, "gd-scan", "gd-expand", map[string]string{"run_id": "{{step.output.run_id}}"},
		dwfResult([]string{"controller", "kbeads"}, "wf_abc"))
	mc.Beads["gd-scan"] = step
	mc.Beads["gd-expand"] = expansionFormulaBead(t, "gd-expand")

	if err := runExpand(expandOpts{StepBeadID: "gd-scan", FromField: "dwf_result.succeeded"}); err != nil {
		t.Fatalf("runExpand: %v", err)
	}

	// One step per resolved value.
	if got := len(mc.CreateBeadCalls); got != 2 {
		t.Fatalf("expected 2 created beads, got %d", got)
	}

	titles := []string{mc.CreateBeadCalls[0].Title, mc.CreateBeadCalls[1].Title}
	// {{component}} comes from the per-iteration loop var; {{step.output.run_id}}
	// is threaded from dwf_result onto every molecule.
	wantA := "Process controller (run wf_abc)"
	wantB := "Process kbeads (run wf_abc)"
	if titles[0] != wantA || titles[1] != wantB {
		t.Fatalf("titles = %v; want [%q %q]", titles, wantA, wantB)
	}

	// run_id (from expand_vars {{step.output.run_id}}) must reach the
	// description via the merged vars.
	for _, c := range mc.CreateBeadCalls {
		if !strings.Contains(c.Description, "wf_abc") {
			t.Fatalf("description %q missing threaded run_id wf_abc", c.Description)
		}
	}

	// Each expanded step is linked as a child of the trigger step.
	if got := len(mc.AddDepCalls); got != 2 {
		t.Fatalf("expected 2 dependency links, got %d", got)
	}
	for _, d := range mc.AddDepCalls {
		if d.DependsOnID != "gd-scan" || d.Type != "parent-child" {
			t.Fatalf("unexpected dep %+v; want parent-child on gd-scan", d)
		}
	}
}

// TestRunExpand_StepOutputScalarThreadedFromValues verifies that with explicit
// --values, a {{step.output.X}} ref in expand_vars still resolves the scalar
// from dwf_result and stamps it on every molecule.
func TestRunExpand_StepOutputScalarThreadedFromValues(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	step := stepBeadWith(t, "gd-scan", "gd-expand", map[string]string{"run_id": "{{step.output.run_id}}"},
		dwfResult([]string{"web"}, "wf_xyz"))
	mc.Beads["gd-scan"] = step
	mc.Beads["gd-expand"] = expansionFormulaBead(t, "gd-expand")

	if err := runExpand(expandOpts{StepBeadID: "gd-scan", Values: "auth,api"}); err != nil {
		t.Fatalf("runExpand: %v", err)
	}

	if got := len(mc.CreateBeadCalls); got != 2 {
		t.Fatalf("expected 2 created beads, got %d", got)
	}
	// Loop var drives {{component}}; threaded scalar drives {{step.output.run_id}}.
	if mc.CreateBeadCalls[0].Title != "Process auth (run wf_xyz)" {
		t.Fatalf("title[0] = %q", mc.CreateBeadCalls[0].Title)
	}
	if mc.CreateBeadCalls[1].Title != "Process api (run wf_xyz)" {
		t.Fatalf("title[1] = %q", mc.CreateBeadCalls[1].Title)
	}
}

// TestRunExpand_StepOutputRefInStepTitle verifies {{step.output.X}} refs that
// appear directly in an expansion step's title (not via expand_vars) are
// resolved against the host step's dwf_result.
func TestRunExpand_StepOutputRefInStepTitle(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	step := stepBeadWith(t, "gd-scan", "gd-expand", nil,
		dwfResult([]string{"only"}, "wf_title"))
	mc.Beads["gd-scan"] = step
	mc.Beads["gd-expand"] = expansionFormulaBead(t, "gd-expand")

	if err := runExpand(expandOpts{StepBeadID: "gd-scan", Values: "x"}); err != nil {
		t.Fatalf("runExpand: %v", err)
	}
	if got := mc.CreateBeadCalls[0].Title; got != "Process x (run wf_title)" {
		t.Fatalf("title = %q; want run_id resolved from dwf_result", got)
	}
}

// TestRunExpand_MutualExclusion verifies --values and --from-field cannot both
// be supplied.
func TestRunExpand_MutualExclusion(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	err := runExpand(expandOpts{StepBeadID: "gd-scan", Values: "a,b", FromField: "dwf_result.succeeded"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
	if len(mc.GetBeadCalls) != 0 {
		t.Fatalf("should fail before fetching the step bead, got %d GetBead calls", len(mc.GetBeadCalls))
	}
}

// TestRunExpand_NoSource verifies that neither --values nor --from-field is an
// error.
func TestRunExpand_NoSource(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	err := runExpand(expandOpts{StepBeadID: "gd-scan"})
	if err == nil || !strings.Contains(err.Error(), "one of --values or --from-field is required") {
		t.Fatalf("expected required-source error, got %v", err)
	}
}

// TestRunExpand_FromFieldEmptyArray verifies an empty dwf_result array is a
// no-op (no beads created, no error).
func TestRunExpand_FromFieldEmptyArray(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	step := stepBeadWith(t, "gd-scan", "gd-expand", nil, dwfResult([]string{}, "wf_empty"))
	mc.Beads["gd-scan"] = step
	mc.Beads["gd-expand"] = expansionFormulaBead(t, "gd-expand")

	if err := runExpand(expandOpts{StepBeadID: "gd-scan", FromField: "dwf_result.succeeded"}); err != nil {
		t.Fatalf("empty array should be a no-op, got %v", err)
	}
	if len(mc.CreateBeadCalls) != 0 {
		t.Fatalf("expected no beads created for empty array, got %d", len(mc.CreateBeadCalls))
	}
}

// TestRunExpand_FromFieldMissing verifies a missing field path errors clearly.
func TestRunExpand_FromFieldMissing(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	step := stepBeadWith(t, "gd-scan", "gd-expand", nil, dwfResult([]string{"a"}, "wf_x"))
	mc.Beads["gd-scan"] = step

	err := runExpand(expandOpts{StepBeadID: "gd-scan", FromField: "dwf_result.missing"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

// TestRunExpand_FromFieldNotArray verifies pointing --from-field at a scalar
// (rather than an array) errors.
func TestRunExpand_FromFieldNotArray(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	step := stepBeadWith(t, "gd-scan", "gd-expand", nil, dwfResult([]string{"a"}, "wf_x"))
	mc.Beads["gd-scan"] = step

	err := runExpand(expandOpts{StepBeadID: "gd-scan", FromField: "dwf_result.run_id"})
	if err == nil || !strings.Contains(err.Error(), "not an array") {
		t.Fatalf("expected not-an-array error, got %v", err)
	}
}

// TestRunExpand_DryRun verifies --dry-run resolves --from-field and threads
// scalars without creating any beads.
func TestRunExpand_DryRun(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	step := stepBeadWith(t, "gd-scan", "gd-expand", map[string]string{"run_id": "{{step.output.run_id}}"},
		dwfResult([]string{"controller", "kbeads"}, "wf_dry"))
	mc.Beads["gd-scan"] = step
	// Note: dry-run must not fetch/validate the expansion formula.
	mc.Beads["gd-expand"] = expansionFormulaBead(t, "gd-expand")

	if err := runExpand(expandOpts{StepBeadID: "gd-scan", FromField: "dwf_result.succeeded", DryRun: true}); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if len(mc.CreateBeadCalls) != 0 {
		t.Fatalf("dry-run created %d beads; want 0", len(mc.CreateBeadCalls))
	}
	if len(mc.AddDepCalls) != 0 {
		t.Fatalf("dry-run added %d deps; want 0", len(mc.AddDepCalls))
	}
}

// TestRunExpand_NoOnComplete verifies a step bead without an on_complete
// handler errors.
func TestRunExpand_NoOnComplete(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	raw, _ := json.Marshal(map[string]any{"source_step_id": "scan"})
	mc.Beads["gd-scan"] = &model.Bead{ID: "gd-scan", Type: "task", Fields: raw}

	err := runExpand(expandOpts{StepBeadID: "gd-scan", Values: "a"})
	if err == nil || !strings.Contains(err.Error(), "no on_complete handler") {
		t.Fatalf("expected no-on_complete error, got %v", err)
	}
}

// TestResolveStepOutputRefs_Unresolvable verifies that unresolvable
// {{step.output.X}} placeholders are left in place (matching substituteVars).
func TestResolveStepOutputRefs_Unresolvable(t *testing.T) {
	fields, _ := json.Marshal(dwfResult([]string{"a"}, "wf_x"))
	// Missing key → left as-is.
	if got := resolveStepOutputRefs("v={{step.output.nope}}", fields); got != "v={{step.output.nope}}" {
		t.Fatalf("missing key should be left in place, got %q", got)
	}
	// Resolvable scalar.
	if got := resolveStepOutputRefs("v={{step.output.run_id}}", fields); got != "v=wf_x" {
		t.Fatalf("run_id should resolve, got %q", got)
	}
	// No placeholder → identity.
	if got := resolveStepOutputRefs("plain", fields); got != "plain" {
		t.Fatalf("plain string should be unchanged, got %q", got)
	}
}

// TestFieldValueAtPath_Number verifies numeric output values render exactly
// (no float formatting drift) thanks to json.Number decoding.
func TestFieldValueAtPath_Number(t *testing.T) {
	fields, _ := json.Marshal(map[string]any{"dwf_result": map[string]any{"total": 1234567890123}})
	v, err := fieldValueAtPath(fields, "dwf_result.total")
	if err != nil {
		t.Fatalf("fieldValueAtPath: %v", err)
	}
	s, err := scalarToString(v)
	if err != nil {
		t.Fatalf("scalarToString: %v", err)
	}
	if s != "1234567890123" {
		t.Fatalf("number rendered as %q; want exact integer", s)
	}
}
