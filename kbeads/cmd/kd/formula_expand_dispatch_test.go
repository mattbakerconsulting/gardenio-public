package main

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/formula"
	"gardenio/kbeads/internal/model"
)

// Tests for the on_complete deferred-expansion pour path (ka-0000003347).
//
// `kd formula expand` is a FOURTH formula-pour implementation: when a step with
// an on_complete handler completes, it fans out one step bead per iteration
// value by expanding a referenced expansion formula. Like the bridge pour, the
// CLI apply pour, and the daemon HTTP pour, it must stamp suggest_new_agent +
// step_role on a fanned-out step the expansion formula opts in — otherwise the
// controller's FormulaStepDispatcher (which hard-gates on suggest_new_agent)
// can never spawn a role-matched agent for it. Before this fix the on_complete
// path dropped both, so an opted-in on_complete step could never dispatch.

// runExpandForStep wires a trigger step bead (carrying the on_complete spec) and
// an expansion formula bead into the mock client, expands over the given values,
// and returns the captured CreateBead calls (one per fanned-out step) for
// assertion.
func runExpandForStep(t *testing.T, expSteps []FormulaStep, expDefaultRoles []string, values string) []map[string]any {
	t.Helper()
	mc := newMockClient()
	withMockClient(t, mc)

	// The trigger step bead: it hosts the on_complete spec pointing at the
	// expansion formula, plus source_step_id so the expanded IDs are prefixed.
	stepFields, err := json.Marshal(map[string]any{
		"source_step_id": "drive",
		"on_complete": map[string]any{
			"var":    "item",
			"expand": "gd-exp",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mc.Beads["gd-step"] = &model.Bead{
		ID:     "gd-step",
		Type:   "task",
		Title:  "Trigger step",
		Status: model.StatusOpen,
		Fields: stepFields,
	}

	// The expansion formula bead. formula_type MUST be "expansion" or runExpand
	// rejects it.
	expFields, err := json.Marshal(formula.Fields{
		FormulaType:  "expansion",
		Steps:        formula.StepList(expSteps),
		DefaultRoles: expDefaultRoles,
	})
	if err != nil {
		t.Fatal(err)
	}
	mc.Beads["gd-exp"] = &model.Bead{
		ID:     "gd-exp",
		Type:   "formula",
		Title:  "Expansion",
		Status: model.StatusOpen,
		Fields: expFields,
	}

	if err := runExpand(expandOpts{StepBeadID: "gd-step", Values: values}); err != nil {
		t.Fatalf("runExpand: %v", err)
	}

	out := make([]map[string]any, 0, len(mc.CreateBeadCalls))
	for _, c := range mc.CreateBeadCalls {
		out = append(out, stepFieldsOf(t, c.Fields))
	}
	return out
}

// TestExpandFormula_StampsStepRoleDispatchOptIn is the core regression: a
// fanned-out step whose expansion formula opts in must land suggest_new_agent
// (as a JSON boolean) plus a step_role resolved from its roles, so the
// dispatcher can spawn for it. Before the fix both were dropped.
func TestExpandFormula_StampsStepRoleDispatchOptIn(t *testing.T) {
	fields := runExpandForStep(t, []FormulaStep{
		{
			ID:              "work",
			Title:           "Do {{item}}",
			Type:            "task",
			Roles:           []string{"monorepo-fix"},
			SuggestNewAgent: true,
		},
	}, nil, "auth")

	if len(fields) != 1 {
		t.Fatalf("expected 1 fanned-out step, got %d", len(fields))
	}
	sf := fields[0]
	// suggest_new_agent must be a JSON boolean true (not the string "true" a
	// PATCH would 400 on) — createBead validates the boolean schema type.
	if got := sf["suggest_new_agent"]; got != true {
		t.Errorf("suggest_new_agent = %v (%T), want bool true — without it the dispatcher never spawns for this step", got, got)
	}
	if got := sf["step_role"]; got != "monorepo-fix" {
		t.Errorf("step_role = %v, want monorepo-fix (resolved from roles[0])", got)
	}
	// The pre-existing source tracing fields must still be present.
	if got := sf["source_formula"]; got != "gd-exp" {
		t.Errorf("source_formula = %v, want gd-exp", got)
	}
	if got := sf["expansion_var"]; got != "auth" {
		t.Errorf("expansion_var = %v, want auth", got)
	}
}

// TestExpandFormula_SuggestNewAgentUsesDefaultRoles: a fanned-out step that
// inherits its role from the expansion formula's default_roles must still stamp
// a usable step_role.
func TestExpandFormula_SuggestNewAgentUsesDefaultRoles(t *testing.T) {
	fields := runExpandForStep(t, []FormulaStep{
		{
			ID:              "work",
			Title:           "Do {{item}}",
			Type:            "task",
			SuggestNewAgent: true,
		},
	}, []string{"monorepo-publish"}, "auth")

	if got := fields[0]["step_role"]; got != "monorepo-publish" {
		t.Errorf("step_role = %v, want monorepo-publish inherited from default_roles", got)
	}
}

// TestExpandFormula_NoSuggestNewAgentLeavesFieldUnset is the blast-radius
// guard: a step that did not opt in must not carry suggest_new_agent or
// step_role, so it stays out of the dispatcher's candidate set and no stray pod
// is minted per fanned-out step.
func TestExpandFormula_NoSuggestNewAgentLeavesFieldUnset(t *testing.T) {
	fields := runExpandForStep(t, []FormulaStep{
		{
			ID:    "work",
			Title: "Do {{item}}",
			Type:  "task",
			Roles: []string{"monorepo-fix"},
		},
	}, nil, "auth")

	sf := fields[0]
	if _, present := sf["suggest_new_agent"]; present {
		t.Errorf("suggest_new_agent must be absent when not opted in, got fields %v", sf)
	}
	if _, present := sf["step_role"]; present {
		t.Errorf("step_role should only be stamped alongside the opt-in, got fields %v", sf)
	}
}

// TestExpandFormula_SuggestNewAgentWithoutRoleStampsNoStepRole: when a step
// opts in but declares no role (and the formula has no default_roles), the
// opt-in still lands but step_role must be absent rather than stamped as ""
// (the dispatcher falls back to the role: label).
func TestExpandFormula_SuggestNewAgentWithoutRoleStampsNoStepRole(t *testing.T) {
	fields := runExpandForStep(t, []FormulaStep{
		{
			ID:              "work",
			Title:           "Do {{item}}",
			Type:            "task",
			SuggestNewAgent: true,
		},
	}, nil, "auth")

	sf := fields[0]
	if got := sf["suggest_new_agent"]; got != true {
		t.Errorf("suggest_new_agent = %v, want true", got)
	}
	if got, present := sf["step_role"]; present {
		t.Errorf("step_role should be absent when no role is declared, got %q", got)
	}
}
