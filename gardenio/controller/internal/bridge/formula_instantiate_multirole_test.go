package bridge

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestInstantiateFormula_MultiRoleStepWarns is the multi-role-truncation guard
// (ka-0000006285). A pod runs exactly one role, so a step declaring more than
// one role dispatches only the first (effectiveRole); the rest become role:
// labels but no agent is ever spawned for them. That silent drop is the bug —
// so the pour path must WARN, naming the winning role and the full set, before
// the molecule goes live.
func TestInstantiateFormula_MultiRoleStepWarns(t *testing.T) {
	daemon := newFormulaMockDaemon()

	var buf bytes.Buffer
	logger := captureLogger(&buf)

	formula := &beadsapi.BeadDetail{ID: "gd-formula-mr", Title: "Multi-role formula"}
	steps := []formulaStep{
		{ID: "single", Title: "One role", Roles: []string{"monorepo-fix"}},
		{ID: "double", Title: "Two roles", Roles: []string{"monorepo-repro", "monorepo-fix"}},
	}

	_, _, err := instantiateFormula(
		context.Background(), daemon, logger, formula, steps, nil, false, "monorepo", "test", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "step declares multiple roles") {
		t.Fatalf("want a multi-role warning at pour time, got logs:\n%s", out)
	}
	// The warning must name the multi-role step, not the single-role one.
	if !strings.Contains(out, `"step":"double"`) {
		t.Errorf("warning should identify the two-role step 'double', got:\n%s", out)
	}
	if strings.Contains(out, `"step":"single"`) {
		t.Errorf("single-role step must NOT warn, got:\n%s", out)
	}
	// It must surface which role actually drives dispatch so the author knows
	// what got dropped.
	if !strings.Contains(out, "monorepo-repro") {
		t.Errorf("warning should name the dispatch role (roles[0]), got:\n%s", out)
	}
}

// TestInstantiateFormula_SingleRoleStepSilent: the common case (one role, or
// none) must not warn — the warning is only for the genuine truncation.
func TestInstantiateFormula_SingleRoleStepSilent(t *testing.T) {
	daemon := newFormulaMockDaemon()

	var buf bytes.Buffer
	logger := captureLogger(&buf)

	formula := &beadsapi.BeadDetail{ID: "gd-formula-sr", Title: "Single-role formula"}
	steps := []formulaStep{
		{ID: "roleful", Title: "One role", Roles: []string{"monorepo-fix"}},
		{ID: "roleless", Title: "No role"},
	}

	if _, _, err := instantiateFormula(
		context.Background(), daemon, logger, formula, steps, nil, false, "monorepo", "test", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Contains(buf.String(), "step declares multiple roles") {
		t.Errorf("no step declares multiple roles, so there must be no warning; got:\n%s", buf.String())
	}
}
