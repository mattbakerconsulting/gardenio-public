package main

import (
	"strings"
	"testing"
)

// renderDecisionOptions must never produce a blank "Options:" surface for a
// non-empty options field — the root symptom of ka-0000006724. It falls back
// to string rendering for a string-array (matching the Slack []string path)
// and to a visible warning for anything unparseable.

func TestRenderDecisionOptions_EmptyReturnsNil(t *testing.T) {
	if got := renderDecisionOptions("", "  "); got != nil {
		t.Errorf("empty options should yield nil, got %v", got)
	}
}

func TestRenderDecisionOptions_ObjectArray(t *testing.T) {
	raw := `[{"id":"a","short":"Alpha","label":"Do alpha","artifact_type":"plan"},{"id":"b","short":"Beta"}]`
	lines := renderDecisionOptions(raw, "  ")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "[a]") || !strings.Contains(lines[0], "Alpha") ||
		!strings.Contains(lines[0], "Do alpha") || !strings.Contains(lines[0], "artifact: plan") {
		t.Errorf("line 0 missing expected parts: %q", lines[0])
	}
	if !strings.Contains(lines[1], "[b]") || !strings.Contains(lines[1], "Beta") {
		t.Errorf("line 1 missing expected parts: %q", lines[1])
	}
}

// The exact malformed shape from ka-0000000201: a JSON array of plain
// strings. Must render one visible line per string, never blank.
func TestRenderDecisionOptions_StringArrayFallback(t *testing.T) {
	raw := `["Dual-accept","Big-bang","Hybrid"]`
	lines := renderDecisionOptions(raw, "  ")
	if len(lines) != 3 {
		t.Fatalf("expected 3 fallback lines, got %d: %v", len(lines), lines)
	}
	for i, want := range []string{"Dual-accept", "Big-bang", "Hybrid"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d should contain %q, got %q", i, want, lines[i])
		}
	}
}

func TestRenderDecisionOptions_NotAnArrayWarns(t *testing.T) {
	lines := renderDecisionOptions(`{"id":"a"}`, "  ")
	if len(lines) != 1 || !strings.Contains(lines[0], "malformed") {
		t.Errorf("expected a single malformed-warning line, got %v", lines)
	}
}

func TestRenderDecisionOptions_MixedObjectAndString(t *testing.T) {
	raw := `[{"id":"a","short":"A","artifact_type":"plan"},"oops"]`
	lines := renderDecisionOptions(raw, "  ")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "[a]") {
		t.Errorf("line 0 should render the object: %q", lines[0])
	}
	if !strings.Contains(lines[1], "oops") {
		t.Errorf("line 1 should render the bare string: %q", lines[1])
	}
}

func TestRenderDecisionOptions_IndentApplied(t *testing.T) {
	lines := renderDecisionOptions(`[{"id":"a","short":"A"}]`, "    ")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "    ") {
		t.Errorf("expected 4-space indent prefix, got %q", lines)
	}
}
