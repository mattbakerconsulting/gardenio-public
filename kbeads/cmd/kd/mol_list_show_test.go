package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// --- molFormulaID ---

func TestMolFormulaID_FormulaID(t *testing.T) {
	b := &model.Bead{
		Fields: json.RawMessage(`{"formula_id":"gd-f-1"}`),
	}
	if got := molFormulaID(b); got != "gd-f-1" {
		t.Errorf("molFormulaID = %q, want gd-f-1", got)
	}
}

func TestMolFormulaID_Empty(t *testing.T) {
	b := &model.Bead{}
	if got := molFormulaID(b); got != "" {
		t.Errorf("molFormulaID = %q, want empty for no fields", got)
	}
}

func TestMolFormulaID_InvalidJSON(t *testing.T) {
	b := &model.Bead{Fields: json.RawMessage(`not json`)}
	if got := molFormulaID(b); got != "" {
		t.Errorf("molFormulaID = %q, want empty for invalid JSON", got)
	}
}

// --- printMolList ---

func TestPrintMolList_Empty(t *testing.T) {
	out := captureStdout(t, func() {
		printMolList(nil, 0)
	})
	if !strings.Contains(out, "No molecules found") {
		t.Errorf("output = %q, want 'No molecules found'", out)
	}
}

func TestPrintMolList_WithBeads(t *testing.T) {
	beads := []*model.Bead{
		{
			ID:       "gd-mol-1",
			Status:   model.StatusOpen,
			Title:    "Auth workflow",
			Assignee: "alice",
			Fields:   json.RawMessage(`{"formula_id":"gd-f-1"}`),
		},
		{
			ID:     "gd-mol-2",
			Status: model.StatusInProgress,
			Title:  "Deploy pipeline",
			Fields: json.RawMessage(`{"formula_id":"gd-f-old"}`),
		},
	}

	out := captureStdout(t, func() {
		printMolList(beads, 5)
	})

	for _, want := range []string{
		"gd-mol-1", "open", "Auth workflow", "gd-f-1", "alice",
		"gd-mol-2", "in_progress", "Deploy pipeline", "gd-f-old",
		"2 molecules (5 total)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestPrintMolList_LongTitle(t *testing.T) {
	longTitle := strings.Repeat("x", 50)
	beads := []*model.Bead{
		{ID: "gd-1", Title: longTitle},
	}
	out := captureStdout(t, func() {
		printMolList(beads, 1)
	})
	if strings.Contains(out, longTitle) {
		t.Error("expected long title to be truncated")
	}
	if !strings.Contains(out, "...") {
		t.Error("expected truncated title to have ellipsis")
	}
}

// --- molListCmd ---

func TestMolListCmd_DefaultFilters(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Override ListBeads to capture request and return results.
	cmd := molListCmd
	cmd.SetArgs([]string{})
	captureStdout(t, func() {
		if err := cmd.RunE(cmd, nil); err != nil {
			t.Fatalf("mol list: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	if len(req.Type) != 1 || req.Type[0] != "molecule" {
		t.Errorf("type filter = %v, want [molecule]", req.Type)
	}
	if len(req.Status) != 2 {
		t.Errorf("status filter = %v, want [open, in_progress]", req.Status)
	}
}

// --- molShowCmd ---

func TestMolShowCmd_DisplaysMolecule(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID:       "gd-mol-1",
		Title:    "Auth mol",
		Type:     "molecule",
		Status:   model.StatusOpen,
		Priority: 2,
		Fields:   json.RawMessage(`{"formula_id":"gd-f-1","applied_vars":{"env":"prod"}}`),
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-step-1", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	mc.Beads["gd-step-1"] = &model.Bead{
		ID: "gd-step-1", Title: "Design auth", Type: "task", Status: model.StatusOpen,
	}
	withMockClient(t, mc)

	cmd := molShowCmd
	out := captureStdout(t, func() {
		if err := cmd.RunE(cmd, []string{"gd-mol-1"}); err != nil {
			t.Fatalf("mol show: %v", err)
		}
	})

	for _, want := range []string{
		"gd-mol-1", "Auth mol",
		"Formula:     gd-f-1",
		"env = prod",
		"Steps:",
		"gd-step-1", "Design auth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q, got:\n%s", want, out)
		}
	}
}

func TestMolShowCmd_NotMolecule(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-task"] = &model.Bead{ID: "gd-task", Type: "task"}
	withMockClient(t, mc)

	cmd := molShowCmd
	err := cmd.RunE(cmd, []string{"gd-task"})
	if err == nil {
		t.Fatal("expected error for non-molecule")
	}
	if !strings.Contains(err.Error(), "not molecule") {
		t.Errorf("error = %q, want 'not molecule'", err)
	}
}

func TestMolShowCmd_UnresolvableChild(t *testing.T) {
	mc := newMockClient()
	mc.Beads["gd-mol-1"] = &model.Bead{
		ID: "gd-mol-1", Title: "Mol", Type: "molecule", Status: model.StatusOpen,
	}
	mc.RevDeps["gd-mol-1"] = []*model.Dependency{
		{BeadID: "gd-missing", DependsOnID: "gd-mol-1", Type: "parent-child"},
	}
	// gd-missing is not in mc.Beads — GetBead will return error.
	withMockClient(t, mc)

	cmd := molShowCmd
	out := captureStdout(t, func() {
		if err := cmd.RunE(cmd, []string{"gd-mol-1"}); err != nil {
			t.Fatalf("mol show: %v", err)
		}
	})

	if !strings.Contains(out, "could not resolve") {
		t.Errorf("output should show unresolved child, got:\n%s", out)
	}
}

func TestMolListCmd_WithLabels(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Build a fresh command with flag state.
	cmd := &cobra.Command{Use: "list", RunE: molListCmd.RunE}
	cmd.Flags().StringSliceP("label", "l", nil, "filter by label")
	cmd.Flags().StringSliceP("status", "s", nil, "filter by status")
	cmd.SetArgs([]string{"--label", "project:gardenio", "--status", "closed"})
	captureStdout(t, func() {
		if err := cmd.Execute(); err != nil {
			t.Fatalf("mol list with labels: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	if len(req.Labels) != 1 || req.Labels[0] != "project:gardenio" {
		t.Errorf("labels = %v, want [project:gardenio]", req.Labels)
	}
	if len(req.Status) != 1 || req.Status[0] != "closed" {
		t.Errorf("status = %v, want [closed]", req.Status)
	}
}
