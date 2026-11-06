package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestFormatFieldValue pins the per-value rendering used by the `kd show`
// Fields section: strings verbatim, booleans as true/false, integral numbers
// without a trailing .0, and composites as compact JSON (ka-0000002579).
func TestFormatFieldValue(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want string
	}{
		{"string", "https://x.io/v", "https://x.io/v"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"integer", float64(42), "42"},
		{"fraction", float64(1.5), "1.5"},
		{"nil", nil, ""},
		{"array", []any{"a", "b"}, `["a","b"]`},
		{"object", map[string]any{"k": "v"}, `{"k":"v"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatFieldValue(tc.in); got != tc.want {
				t.Errorf("formatFieldValue(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestPrintBeadTable_BooleanFieldRenders is the regression guard for
// ka-0000002579 acceptance #5: a bead carrying a boolean field (gate_passed)
// must still render its Fields section. The prior map[string]string unmarshal
// failed on the first non-string value and silently dropped the WHOLE section,
// so `kd show` on a task with gate_passed=true showed no fields at all.
func TestPrintBeadTable_BooleanFieldRenders(t *testing.T) {
	bead := &model.Bead{
		ID:     "gd-repro-show",
		Title:  "emit step",
		Type:   "task",
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
		Fields: json.RawMessage(`{"gate_passed":true,"repro_evidence":"https://x.io/v","repro_ticket":"FSM-468"}`),
	}
	out := captureStdout(t, func() { printBeadTable(bead) })

	if !strings.Contains(out, "Fields:") {
		t.Fatalf("Fields section missing from output:\n%s", out)
	}
	for _, want := range []string{"gate_passed:", "true", "repro_evidence:", "https://x.io/v", "repro_ticket:", "FSM-468"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

// TestFormatOpenBlockers_OutOfScope pins the table rendering for a redacted
// cross-project blocker (ka-0000007462): it must show the id + status AND an
// "out-of-scope" marker so a reader sees the row IS gated even though the
// blocker's title is withheld. An empty/"-" cell here would reproduce the very
// false-all-clear this fix removes.
func TestFormatOpenBlockers_OutOfScope(t *testing.T) {
	blockers := []*model.OpenBlocker{
		{ID: "gd-inproj", Status: model.StatusOpen, Type: "task", EdgeType: model.DepBlocks, Title: "visible"},
		{ID: "gd-xproj", Status: model.StatusOpen, Type: "escalation", EdgeType: model.DepBlocks, OutOfScope: true},
	}
	got := formatOpenBlockers(&blockers)
	if !strings.Contains(got, "gd-inproj [open]") {
		t.Errorf("in-scope blocker missing from cell: %q", got)
	}
	if !strings.Contains(got, "gd-xproj [open, out-of-scope]") {
		t.Errorf("out-of-scope blocker must render with an out-of-scope marker, got %q", got)
	}
	if got == "-" {
		t.Errorf("a gated row must never render as unblocked (-): %q", got)
	}
}
