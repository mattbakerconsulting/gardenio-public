package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// writeTempFormula writes formula JSON to a temp file and returns its path.
func writeTempFormula(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal formula: %v", err)
	}
	path := filepath.Join(t.TempDir(), "formula.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	return path
}

// runCreate invokes the formula-create RunE directly against the active mock
// client and returns the command error (nil on success). We call RunE directly
// rather than through rootCmd.Execute() so cobra's PersistentPreRunE (which
// would re-initialize the real beadsClient and clobber the mock) does not fire —
// the same reason the cook tests use a standalone command.
func runCreate(t *testing.T, name, path string) error {
	t.Helper()
	cmd := formulaCreateCmd
	if err := cmd.Flags().Set("file", path); err != nil {
		t.Fatalf("set --file: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Flags().Set("file", "") })
	return cmd.RunE(cmd, []string{name})
}

// TestCreate_ExtendsDepOnInheritedStep is the regression guard for the gap this
// MR fixes: a child formula's new step may depend_on a step inherited from the
// parent it extends. Before the fix, `formula create` validated depends_on only
// against the child's own step IDs and rejected this with "unknown step", even
// though `formula cook`/`apply` resolved the chain and accepted it.
func TestCreate_ExtendsDepOnInheritedStep(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	// Parent formula provides step "investigate-fix".
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
	mc.CreateIDs = []string{"gd-new-formula"}

	// Child extends parent; its new "fix-critic" step depends on the INHERITED
	// "investigate-fix" step. This must succeed.
	child := map[string]any{
		"extends": []string{"gd-parent"},
		"steps": []map[string]any{
			{
				"id":         "fix-critic",
				"title":      "Critic: does the fix actually fix it?",
				"type":       "task",
				"depends_on": []string{"investigate-fix"},
			},
		},
	}
	path := writeTempFormula(t, child)

	if err := runCreate(t, "Child formula", path); err != nil {
		t.Fatalf("create with dep on inherited step should succeed, got: %v", err)
	}
	if len(mc.CreateBeadCalls) != 1 {
		t.Fatalf("expected 1 CreateBead call (the formula bead), got %d", len(mc.CreateBeadCalls))
	}
}

// TestCreate_ExtendsDepOnUnknownStepStillFails ensures the extends-aware path
// did not weaken validation: a depends_on that matches neither a child nor an
// inherited step is still rejected.
func TestCreate_ExtendsDepOnUnknownStepStillFails(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-parent"] = &model.Bead{
		ID:    "gd-parent",
		Type:  "formula",
		Title: "Parent",
		Fields: makeFormulaFieldsWithExtends(nil, nil,
			[]FormulaStep{
				{ID: "investigate-fix", Title: "Investigate", Type: "task"},
			},
		),
	}

	child := map[string]any{
		"extends": []string{"gd-parent"},
		"steps": []map[string]any{
			{
				"id":         "fix-critic",
				"title":      "Critic",
				"type":       "task",
				"depends_on": []string{"does-not-exist"},
			},
		},
	}
	path := writeTempFormula(t, child)

	err := runCreate(t, "Child formula", path)
	if err == nil {
		t.Fatal("create with dep on truly-unknown step should fail, got nil")
	}
	if !strings.Contains(err.Error(), "unknown step") {
		t.Errorf("expected 'unknown step' error, got: %v", err)
	}
}
