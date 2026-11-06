package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeRawFormula writes verbatim JSON bytes to a temp file and returns its
// path. Unlike writeTempFormula (which marshals a Go value and therefore always
// emits native arrays), this preserves the exact on-disk shape — including a
// `steps` field encoded as a JSON STRING, the dump→create round-trip shape that
// regressed in ka-0000005613.
func writeRawFormula(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "formula.json")
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	return path
}

// TestCreate_StringEncodedSteps is the end-to-end regression for ka-0000005613:
// `kd formula dump | kd formula create -f -` failed because the dump emits a
// formula whose persisted `steps` field is a JSON STRING (double-encoded array),
// but the create-side input struct parsed `steps` with the strict []FormulaStep
// type — yielding "cannot unmarshal string into Go struct field .steps of type
// []main.FormulaStep". The `vars` field already used the tolerant varDefList
// (ka-0000001116), so a dump whose vars were stringified worked while steps did
// not. The fix switches the create-input `Steps` field to the tolerant stepList,
// mirroring vars. This test asserts create succeeds on the string form AND that
// the persisted fields normalize `steps` back to a native array.
func TestCreate_StringEncodedSteps(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// steps as a JSON-string-encoded array (the dump shape), vars native.
	raw := `{"vars":[{"name":"tag","required":true}],"steps":"[{\"id\":\"build\",\"title\":\"Build {{tag}}\"},{\"id\":\"test\",\"title\":\"Test\",\"depends_on\":[\"build\"]}]"}`
	path := writeRawFormula(t, raw)

	if err := runCreate(t, "string-steps formula", path); err != nil {
		t.Fatalf("create with string-encoded steps failed (regression ka-0000005613): %v", err)
	}

	if len(mc.CreateBeadCalls) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(mc.CreateBeadCalls))
	}

	// The persisted fields must carry steps as a NATIVE array (normalized),
	// with depends_on validated against the parsed step IDs.
	var fields struct {
		Steps []FormulaStep `json:"steps"`
	}
	if err := json.Unmarshal(mc.CreateBeadCalls[0].Fields, &fields); err != nil {
		t.Fatalf("persisted fields did not re-marshal steps as a native array: %v", err)
	}
	if len(fields.Steps) != 2 || fields.Steps[0].ID != "build" || fields.Steps[1].ID != "test" {
		t.Fatalf("steps not parsed/normalized from string form: %+v", fields.Steps)
	}
	if len(fields.Steps[1].DependsOn) != 1 || fields.Steps[1].DependsOn[0] != "build" {
		t.Fatalf("depends_on not preserved through string-form parse: %+v", fields.Steps[1].DependsOn)
	}
}

// TestCreate_StringEncodedStepsAndVars covers the fully double-encoded dump shape
// where BOTH vars and steps are stringified — must also create cleanly.
func TestCreate_StringEncodedStepsAndVars(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	raw := `{"vars":"[{\"name\":\"tag\",\"required\":true}]","steps":"[{\"id\":\"build\",\"title\":\"Build {{tag}}\"}]"}`
	path := writeRawFormula(t, raw)

	if err := runCreate(t, "string-both formula", path); err != nil {
		t.Fatalf("create with string-encoded vars+steps failed (regression ka-0000005613): %v", err)
	}
	if len(mc.CreateBeadCalls) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(mc.CreateBeadCalls))
	}
}
