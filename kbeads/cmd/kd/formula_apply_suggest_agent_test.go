package main

import (
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Tests for suggest_new_agent plumbing (ka-0000001730).
//
// suggest_new_agent opts a step into the gardenio controller's step-role
// dispatcher: when the step's last blocking dep closes, spawn a fresh agent
// carrying that step's role instead of relying on the molecule's driver still
// being alive to walk into it. The flag has to survive apply — before this it
// was declared only in the gardenio bridge's own struct, so the daemon silently
// stripped it and the dispatcher never saw an opted-in step.

// stepFieldsOf decodes the Fields JSON of a captured CreateBead call.
func stepFieldsOf(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	if len(raw) == 0 {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decoding step fields %s: %v", raw, err)
	}
	return out
}

func applyOneStepFormula(t *testing.T, step FormulaStep, defaultRoles []string) map[string]any {
	t.Helper()
	mc := newMockClient()
	withMockClient(t, mc)

	mc.Beads["gd-f"] = &model.Bead{
		ID:     "gd-f",
		Type:   "formula",
		Title:  "F",
		Fields: makeFormulaFieldsFull(nil, nil, []FormulaStep{step}, defaultRoles, ""),
	}
	mc.CreateIDs = []string{"gd-mol-1", "gd-step-1"}
	mc.ListBeadsResult = nil

	if err := runFormulaApply(formulaApplyOpts{FormulaID: "gd-f"}); err != nil {
		t.Fatalf("runFormulaApply: %v", err)
	}
	if len(mc.CreateBeadCalls) != 2 {
		t.Fatalf("expected molecule + 1 step CreateBead call, got %d", len(mc.CreateBeadCalls))
	}
	return stepFieldsOf(t, mc.CreateBeadCalls[1].Fields)
}

// TestApply_SuggestNewAgentStampedOnStepBead is the plumbing regression: the
// flag must reach the bead, because that field is the dispatcher's opt-in gate.
func TestApply_SuggestNewAgentStampedOnStepBead(t *testing.T) {
	fields := applyOneStepFormula(t, FormulaStep{
		ID:              "handoff",
		Title:           "Prove the fix",
		Type:            "task",
		Roles:           []string{"monorepo-fix"},
		SuggestNewAgent: true,
	}, nil)

	if got := fields["suggest_new_agent"]; got != true {
		t.Errorf("suggest_new_agent = %v, want true — without it the dispatcher never spawns for this step", got)
	}
	// The resolved role rides along so the dispatcher need not re-derive it.
	if got := fields["step_role"]; got != "monorepo-fix" {
		t.Errorf("step_role = %v, want monorepo-fix", got)
	}
}

// TestApply_SuggestNewAgentUsesDefaultRoles: a step that inherits its role from
// the formula must still stamp a usable step_role.
func TestApply_SuggestNewAgentUsesDefaultRoles(t *testing.T) {
	fields := applyOneStepFormula(t, FormulaStep{
		ID:              "handoff",
		Title:           "Step",
		Type:            "task",
		SuggestNewAgent: true,
	}, []string{"monorepo-publish"})

	if got := fields["step_role"]; got != "monorepo-publish" {
		t.Errorf("step_role = %v, want monorepo-publish inherited from default_roles", got)
	}
}

// TestApply_NoSuggestNewAgentLeavesFieldUnset is the blast-radius guard. Most
// formulas want ONE driver holding context across every step; a stray
// suggest_new_agent would mint a pod per step.
func TestApply_NoSuggestNewAgentLeavesFieldUnset(t *testing.T) {
	fields := applyOneStepFormula(t, FormulaStep{
		ID:    "ordinary",
		Title: "Step",
		Type:  "task",
		Roles: []string{"monorepo-fix"},
	}, nil)

	if _, present := fields["suggest_new_agent"]; present {
		t.Errorf("suggest_new_agent must be absent when not opted in, got fields %v", fields)
	}
	if _, present := fields["step_role"]; present {
		t.Errorf("step_role should only be stamped alongside the opt-in, got fields %v", fields)
	}
}

// TestApply_SuggestNewAgentWithoutRoleStampsNoStepRole: the dispatcher falls
// back to the role: label, so an empty step_role must simply be absent rather
// than stamped as "".
func TestApply_SuggestNewAgentWithoutRoleStampsNoStepRole(t *testing.T) {
	fields := applyOneStepFormula(t, FormulaStep{
		ID:              "roleless",
		Title:           "Step",
		Type:            "task",
		SuggestNewAgent: true,
	}, nil)

	if got := fields["suggest_new_agent"]; got != true {
		t.Errorf("suggest_new_agent = %v, want true", got)
	}
	if got, present := fields["step_role"]; present {
		t.Errorf("step_role should be absent when no role is declared, got %q", got)
	}
}

// TestFormulaStep_SuggestNewAgentRoundTrips pins the JSON contract: the field
// must survive a formula dump/update cycle, which is how operators edit these.
func TestFormulaStep_SuggestNewAgentRoundTrips(t *testing.T) {
	in := FormulaStep{ID: "s", Title: "S", SuggestNewAgent: true, Roles: []string{"r"}}
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"suggest_new_agent":true`) {
		t.Fatalf("field must serialize (a dump/update cycle would drop it otherwise): %s", raw)
	}

	var out FormulaStep
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if !out.SuggestNewAgent {
		t.Error("suggest_new_agent lost on round-trip")
	}

	// omitempty: an un-opted step must not carry noise into every dump.
	bare, _ := json.Marshal(FormulaStep{ID: "s", Title: "S"})
	if strings.Contains(string(bare), "suggest_new_agent") {
		t.Errorf("false should be omitted, got %s", bare)
	}
}
