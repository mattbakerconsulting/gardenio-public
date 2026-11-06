package main

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestExpand_PartialStepFiresOnComplete asserts that a step bead in 'partial'
// status fires its on_complete expansion exactly like a 'closed' step would.
// 'partial' is terminal-for-progression (design ka-0000007305 §2.3): the
// success path of a fan-out step is resolved (some descendants failed, durable
// follow-up beads were filed) and the deferred on_complete handler must still
// expand. The expand path is status-agnostic — it keys off the stored
// on_complete spec, not the step's status — so a partial step expands cleanly.
func TestExpand_PartialStepFiresOnComplete(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Step bead is in 'partial' status and carries a deferred on_complete spec.
	stepFields, _ := json.Marshal(map[string]any{
		"source_step_id": "scan",
		"on_complete": map[string]any{
			"for_each":    "{{scan.output.components}}",
			"var":         "component",
			"expand":      "gd-expansion-1",
			"expand_vars": map[string]string{"name": "{{component}}"},
		},
	})
	mc.Beads["gd-step-partial"] = &model.Bead{
		ID:       "gd-step-partial",
		Title:    "Scan (completed with failures)",
		Type:     "task",
		Status:   model.StatusPartial,
		Priority: 2,
		Fields:   stepFields,
	}

	// Expansion formula referenced by the on_complete handler.
	expFields, _ := json.Marshal(map[string]any{
		"formula_type": "expansion",
		"steps": []FormulaStep{
			{ID: "process", Title: "Process {{name}}", Type: "task"},
		},
	})
	mc.Beads["gd-expansion-1"] = &model.Bead{
		ID:     "gd-expansion-1",
		Title:  "Process component",
		Type:   "formula",
		Fields: expFields,
	}

	mc.CreateIDs = []string{"gd-new-1", "gd-new-2"}

	cmd := newExpandCmd()
	cmd.SetArgs([]string{"gd-step-partial", "--values", "auth,api"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expand on a partial step should succeed, got: %v", err)
	}

	// Two values × one expansion step = two new step beads created.
	if got := len(mc.CreateBeadCalls); got != 2 {
		t.Fatalf("expected 2 CreateBead calls from on_complete expansion, got %d", got)
	}
	if mc.CreateBeadCalls[0].Title != "Process auth" {
		t.Errorf("first expanded step title = %q, want 'Process auth'", mc.CreateBeadCalls[0].Title)
	}
	if mc.CreateBeadCalls[1].Title != "Process api" {
		t.Errorf("second expanded step title = %q, want 'Process api'", mc.CreateBeadCalls[1].Title)
	}

	// Each new step is linked back to the partial step as a parent-child edge.
	if got := len(mc.AddDepCalls); got != 2 {
		t.Fatalf("expected 2 AddDependency calls, got %d", got)
	}
	for _, d := range mc.AddDepCalls {
		if d.DependsOnID != "gd-step-partial" || d.Type != "parent-child" {
			t.Errorf("expanded step dep = %+v, want parent-child → gd-step-partial", d)
		}
	}
}
