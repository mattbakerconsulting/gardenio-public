package server

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// nestedPourFormulaFixture seeds a formula with one top-level step that has
// its own nested child step, plus a sibling top-level step — enough shape to
// exercise both "direct child of molecule" and "grandchild via a step
// parent" propagation.
func nestedPourFormulaFixture(t *testing.T, ms *mockStore) {
	t.Helper()
	fields, err := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{
				"id":    "design",
				"title": "Design",
				"type":  "task",
				"children": []map[string]any{
					{"id": "design-sub", "title": "Design sub-step", "type": "task"},
				},
			},
			{"id": "implement", "title": "Implement", "type": "task", "depends_on": []string{"design"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-taint"] = &model.Bead{
		ID: "gd-formula-taint", Kind: model.KindData, Type: "formula",
		Title: "Taint pour formula", Status: model.StatusOpen, Fields: fields,
	}
}

// TestPourFormula_MoleculeTaintsPropagateToSteps pins ADR docs/adr/taints.md
// §4.2 row 6: "formula pour / molecule expansion | children get the
// molecule's taints | inherit:parent". The pouring caller here is an
// untainted OIDC human (the default doJSON identity), so the ONLY taint in
// play is the one the target plan carries; the molecule picks it up via the
// generic row-1 parent-inherit mechanism when it is created as the plan's
// child (ParentBeadID wiring at emplacement), and every step bead the same
// pour creates — top-level and nested — must in turn carry it, attributed
// inherit:parent to its own immediate parent bead (the molecule for
// top-level steps, the parent step for a nested child).
func TestPourFormula_MoleculeTaintsPropagateToSteps(t *testing.T) {
	_, ms, h := newTestServer()
	nestedPourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	ms.taints = map[string][]int64{"gd-plan-1": {1}}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-taint/pour", map[string]any{
		"target_plan": "gd-plan-1",
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	molID := resp.Molecule.ID
	designID, ok := resp.Steps["design"]
	if !ok {
		t.Fatal("missing design step bead id")
	}
	implementID, ok := resp.Steps["implement"]
	if !ok {
		t.Fatal("missing implement step bead id")
	}
	designSubID, ok := resp.Steps["design-sub"]
	if !ok {
		t.Fatal("missing design-sub step bead id")
	}

	// Molecule: inherits the plan's taint at creation, source=inherit:parent.
	molRow, ok := beadTaintByID(ms.beadTaints[molID], 1)
	if !ok {
		t.Fatalf("molecule %s missing taint 1: %v", molID, ms.beadTaints[molID])
	}
	if molRow.Source != model.TaintSourceInheritParent {
		t.Errorf("molecule taint source = %q, want %q", molRow.Source, model.TaintSourceInheritParent)
	}
	if molRow.SourceBeadID != "gd-plan-1" {
		t.Errorf("molecule taint source_bead_id = %q, want plan gd-plan-1", molRow.SourceBeadID)
	}

	// Top-level steps: inherit the molecule's taint, source=inherit:parent,
	// attributed to the molecule directly.
	for _, id := range []string{designID, implementID} {
		row, ok := beadTaintByID(ms.beadTaints[id], 1)
		if !ok {
			t.Fatalf("step %s missing taint 1: %v", id, ms.beadTaints[id])
		}
		if row.Source != model.TaintSourceInheritParent {
			t.Errorf("step %s taint source = %q, want %q", id, row.Source, model.TaintSourceInheritParent)
		}
		if row.SourceBeadID != molID {
			t.Errorf("step %s taint source_bead_id = %q, want molecule %q", id, row.SourceBeadID, molID)
		}
	}

	// Nested child: inherits transitively, attributed to its immediate
	// parent (the design step), not the molecule.
	subRow, ok := beadTaintByID(ms.beadTaints[designSubID], 1)
	if !ok {
		t.Fatalf("design-sub %s missing taint 1: %v", designSubID, ms.beadTaints[designSubID])
	}
	if subRow.Source != model.TaintSourceInheritParent {
		t.Errorf("design-sub taint source = %q, want %q", subRow.Source, model.TaintSourceInheritParent)
	}
	if subRow.SourceBeadID != designID {
		t.Errorf("design-sub taint source_bead_id = %q, want parent step %q", subRow.SourceBeadID, designID)
	}
}

// TestPourFormula_UntaintedPlanProducesNoStepTaints is the negative
// counterpart: an untainted target plan (and untainted pouring caller)
// produces a molecule and steps carrying no taints at all — propagation only
// fires when there is something to propagate.
func TestPourFormula_UntaintedPlanProducesNoStepTaints(t *testing.T) {
	_, ms, h := newTestServer()
	nestedPourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-taint/pour", map[string]any{
		"target_plan": "gd-plan-1",
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	ids := append([]string{resp.Molecule.ID}, stepBeadIDValues(resp.Steps)...)
	for _, id := range ids {
		if len(ms.beadTaints[id]) != 0 {
			t.Errorf("bead %s: expected no taints, got %v", id, ms.beadTaints[id])
		}
	}
}

func stepBeadIDValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
