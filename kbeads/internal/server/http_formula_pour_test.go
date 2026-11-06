package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// pourFormulaFixture seeds a workflow formula with two dependent steps and
// one declared variable.
func pourFormulaFixture(t *testing.T, ms *mockStore) {
	t.Helper()
	fields, err := json.Marshal(map[string]any{
		"vars": []map[string]any{
			{"name": "component", "required": true},
		},
		"steps": []map[string]any{
			{"id": "design", "title": "Design {{component}}", "type": "task"},
			{"id": "implement", "title": "Implement {{component}}", "type": "task", "depends_on": []string{"design"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-1"] = &model.Bead{
		ID: "gd-formula-1", Kind: model.KindData, Type: "formula",
		Title: "Ship {{component}}", Status: model.StatusOpen, Fields: fields,
	}
}

func TestPourFormula_CreatesMoleculeAndSteps(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars": map[string]string{"component": "auth"},
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)

	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	if resp.Molecule.Title != "Ship auth" {
		t.Errorf("molecule title = %q, want %q", resp.Molecule.Title, "Ship auth")
	}
	if string(resp.Molecule.Type) != "molecule" {
		t.Errorf("molecule type = %q, want molecule", resp.Molecule.Type)
	}
	if resp.TotalSteps != 2 {
		t.Errorf("total_steps = %d, want 2", resp.TotalSteps)
	}
	if len(resp.Steps) != 2 {
		t.Fatalf("steps map = %v, want 2 entries", resp.Steps)
	}

	// The molecule bead records formula_id + applied_vars.
	mol := ms.beads[resp.Molecule.ID]
	if mol == nil {
		t.Fatal("molecule not persisted")
	}
	var molFields struct {
		FormulaID   string            `json:"formula_id"`
		AppliedVars map[string]string `json:"applied_vars"`
	}
	if err := json.Unmarshal(mol.Fields, &molFields); err != nil {
		t.Fatal(err)
	}
	if molFields.FormulaID != "gd-formula-1" {
		t.Errorf("formula_id = %q", molFields.FormulaID)
	}
	if molFields.AppliedVars["component"] != "auth" {
		t.Errorf("applied_vars = %v", molFields.AppliedVars)
	}

	// Step beads: titles substituted, parent-child dep to molecule, blocks dep
	// between implement → design.
	designID, implementID := resp.Steps["design"], resp.Steps["implement"]
	if ms.beads[designID].Title != "Design auth" {
		t.Errorf("design title = %q", ms.beads[designID].Title)
	}
	var foundParent, foundBlocks bool
	for _, d := range ms.deps[designID] {
		if d.Type == model.DepParentChild && d.DependsOnID == resp.Molecule.ID {
			foundParent = true
		}
	}
	for _, d := range ms.deps[implementID] {
		if d.Type == model.DepBlocks && d.DependsOnID == designID {
			foundBlocks = true
		}
	}
	if !foundParent {
		t.Error("design step missing parent-child dep to molecule")
	}
	if !foundBlocks {
		t.Error("implement step missing blocks dep on design")
	}

	// Step fields carry source tracing.
	var stepFields struct {
		SourceFormula string `json:"source_formula"`
		SourceStepID  string `json:"source_step_id"`
	}
	if err := json.Unmarshal(ms.beads[designID].Fields, &stepFields); err != nil {
		t.Fatal(err)
	}
	if stepFields.SourceFormula != "gd-formula-1" || stepFields.SourceStepID != "design" {
		t.Errorf("step tracing = %+v", stepFields)
	}
}

// TestPourFormula_StampsStepRoleDispatchOptIn pins the fix for ka-0000004970:
// a formula step carrying suggest_new_agent must land that field (as a JSON
// boolean) plus step_role + molecule_id on the poured step bead, so the
// controller's FormulaStepDispatcher (which hard-gates on suggest_new_agent)
// can spawn a role-matched agent for plan-poured molecules. Before the fix the
// HTTP pour dropped all three, so plan-poured molecules could never dispatch.
func TestPourFormula_StampsStepRoleDispatchOptIn(t *testing.T) {
	_, ms, h := newTestServer()

	fields, err := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{"id": "repro", "title": "Reproduce the bug", "type": "task"},
			{
				"id":                "fix",
				"title":             "Prove the fix",
				"type":              "task",
				"depends_on":        []string{"repro"},
				"roles":             []string{"monorepo-fix"},
				"suggest_new_agent": true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-optin"] = &model.Bead{
		ID: "gd-formula-optin", Kind: model.KindData, Type: "formula",
		Title: "Repro then fix", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-optin/pour", map[string]any{})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}

	// The opted-in step carries suggest_new_agent=true (JSON boolean, not the
	// string form a PATCH would 400 on), step_role resolved from roles[0], and
	// molecule_id scoping the dispatch.
	fixID := resp.Steps["fix"]
	if fixID == "" {
		t.Fatal("fix step not created")
	}
	var fixFields struct {
		SuggestNewAgent bool   `json:"suggest_new_agent"`
		StepRole        string `json:"step_role"`
		MoleculeID      string `json:"molecule_id"`
	}
	if err := json.Unmarshal(ms.beads[fixID].Fields, &fixFields); err != nil {
		t.Fatal(err)
	}
	if !fixFields.SuggestNewAgent {
		t.Errorf("fix step suggest_new_agent = false, want true (dispatcher hard-gates on this)")
	}
	if fixFields.StepRole != "monorepo-fix" {
		t.Errorf("fix step step_role = %q, want monorepo-fix", fixFields.StepRole)
	}
	if fixFields.MoleculeID != resp.Molecule.ID {
		t.Errorf("fix step molecule_id = %q, want %q", fixFields.MoleculeID, resp.Molecule.ID)
	}

	// A step that did NOT opt in must not carry suggest_new_agent — the field
	// is an explicit per-step opt-in, not a molecule-wide default. Its absence
	// (or false) keeps the opt-out step out of the dispatcher's candidate set.
	reproID := resp.Steps["repro"]
	var reproFields struct {
		SuggestNewAgent bool `json:"suggest_new_agent"`
	}
	if err := json.Unmarshal(ms.beads[reproID].Fields, &reproFields); err != nil {
		t.Fatal(err)
	}
	if reproFields.SuggestNewAgent {
		t.Errorf("repro step suggest_new_agent = true, want false (never opted in)")
	}
}

// TestPourFormula_ManualGateStepPoursWholeMolecule pins the fix for
// ka-0000006567. A step declaring a non-decision gate (manual/sync) stamps
// gate_type (+ timeout) onto the step bead but does NOT flip its type to
// "decision" — only a decision gate does — so the step lands on type:task.
// type:task did not declare gate_type/timeout, so createBead 400'd
// "gate_type: unknown field" and the pour aborted MID-MOLECULE, leaving a
// half-built molecule whose one dropped step was precisely the gate (the
// molecule silently lost its enforcement point). The gate is the LAST step
// here so a regression drops it and fails the total-steps/edge assertions,
// reproducing the partial-molecule failure mode rather than a clean error.
func TestPourFormula_ManualGateStepPoursWholeMolecule(t *testing.T) {
	_, ms, h := newTestServer()

	fields, err := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{"id": "build", "title": "Build it", "type": "task"},
			{
				"id":         "gardener-signoff",
				"title":      "Gardener sign-off",
				"depends_on": []string{"build"},
				"gate":       map[string]any{"type": "manual", "timeout": "72h"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-gate"] = &model.Bead{
		ID: "gd-formula-gate", Kind: model.KindData, Type: "formula",
		Title: "Build then gate", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-gate/pour", map[string]any{})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	// Both steps must exist — a mid-molecule 400 on the gate step would leave
	// total_steps=1 and no gardener-signoff entry (the exact partial-molecule bug).
	if resp.TotalSteps != 2 {
		t.Fatalf("total_steps = %d, want 2 (the gate step aborted the pour mid-molecule)", resp.TotalSteps)
	}
	gateID := resp.Steps["gardener-signoff"]
	if gateID == "" {
		t.Fatal("gardener-signoff gate step not created — pour dropped the molecule's enforcement point")
	}

	// A manual/sync gate keeps the default type=task (only a decision gate
	// flips to type=decision), and carries gate_type/timeout as stamped fields.
	if got := string(ms.beads[gateID].Type); got != "task" {
		t.Errorf("gate step type = %q, want task (manual gate does not flip to decision)", got)
	}
	var gateFields struct {
		GateType string `json:"gate_type"`
		Timeout  string `json:"timeout"`
	}
	if err := json.Unmarshal(ms.beads[gateID].Fields, &gateFields); err != nil {
		t.Fatal(err)
	}
	if gateFields.GateType != "manual" {
		t.Errorf("gate step gate_type = %q, want manual", gateFields.GateType)
	}
	if gateFields.Timeout != "72h" {
		t.Errorf("gate step timeout = %q, want 72h", gateFields.Timeout)
	}

	// The gate's blocking edge on its upstream step must survive too, so the
	// molecule is actually gated (not poured as a bag of independent steps).
	buildID := resp.Steps["build"]
	var foundBlocks bool
	for _, d := range ms.deps[gateID] {
		if d.Type == model.DepBlocks && d.DependsOnID == buildID {
			foundBlocks = true
		}
	}
	if !foundBlocks {
		t.Error("gate step missing blocks dep on its upstream step")
	}
}

func TestPourFormula_DryRunCreatesNothing(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":    map[string]string{"component": "auth"},
		"dry_run": true,
	})
	requireStatus(t, rec, 200)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if !resp.DryRun {
		t.Error("expected dry_run=true echoed")
	}
	if resp.Molecule != nil || len(resp.Steps) != 0 {
		t.Error("dry run must not create beads")
	}
	if len(resp.Plan) != 2 || resp.Plan[0].Title != "Design auth" {
		t.Errorf("plan = %+v", resp.Plan)
	}
	if len(ms.beads) != before {
		t.Errorf("bead count changed on dry run: %d → %d", before, len(ms.beads))
	}
}

func TestPourFormula_RequiredVarMissing(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{})
	requireStatus(t, rec, 400)
}

// TestPourFormula_DanglingDependsOn_Rejected is the daemon-side regression for
// ka-0000002771: a step whose depends_on names a non-existent step must 400
// BEFORE any bead is created, so the pour never produces an ungated molecule
// with the gating edge silently dropped.
func TestPourFormula_DanglingDependsOn_Rejected(t *testing.T) {
	_, ms, h := newTestServer()
	fields, err := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{"id": "investigate", "title": "Investigate", "type": "task"},
			// tag-reviewer gates on a gardener-signoff step that was never declared.
			{"id": "tag-reviewer", "title": "Tag reviewer", "type": "task", "depends_on": []string{"gardener-signoff"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-dangle"] = &model.Bead{
		ID: "gd-formula-dangle", Kind: model.KindData, Type: "formula",
		Title: "Gated", Status: model.StatusOpen, Fields: fields,
	}
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-dangle/pour", map[string]any{})
	requireStatus(t, rec, 400)
	if !strings.Contains(rec.Body.String(), "gardener-signoff") {
		t.Errorf("400 body should name the unresolvable ref, got: %s", rec.Body.String())
	}
	if len(ms.beads) != before {
		t.Errorf("no beads should be created on a rejected pour: %d → %d", before, len(ms.beads))
	}
}

// TestPourFormula_DanglingDependsOn_DryRunRejected proves the dry-run preview
// surfaces the same authoring bug, so a confirm-before-pour UI blocks it up
// front (ka-0000002771).
func TestPourFormula_DanglingDependsOn_DryRunRejected(t *testing.T) {
	_, ms, h := newTestServer()
	fields, err := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{"id": "a", "title": "A", "type": "task"},
			{"id": "b", "title": "B", "type": "task", "depends_on": []string{"missing"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-dangle2"] = &model.Bead{
		ID: "gd-formula-dangle2", Kind: model.KindData, Type: "formula",
		Title: "F", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-dangle2/pour", map[string]any{
		"dry_run": true,
	})
	requireStatus(t, rec, 400)
	if !strings.Contains(rec.Body.String(), "missing") {
		t.Errorf("dry-run 400 body should name the missing ref, got: %s", rec.Body.String())
	}
}

func TestPourFormula_NotAFormula(t *testing.T) {
	_, ms, h := newTestServer()
	ms.beads["gd-task-1"] = &model.Bead{
		ID: "gd-task-1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "not a formula", Status: model.StatusOpen,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-task-1/pour", map[string]any{})
	requireStatus(t, rec, 400)
}

func TestPourFormula_NotFound(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/formulas/gd-missing/pour", map[string]any{})
	requireStatus(t, rec, 404)
}

func TestPourFormula_DuplicateMoleculeConflict(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	molFields, _ := json.Marshal(map[string]any{
		"formula_id":   "gd-formula-1",
		"applied_vars": map[string]string{"component": "auth"},
	})
	ms.beads["gd-mol-existing"] = &model.Bead{
		ID: "gd-mol-existing", Kind: model.KindIssue, Type: "molecule",
		Title: "Ship auth", Status: model.StatusOpen, Fields: molFields,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars": map[string]string{"component": "auth"},
	})
	requireStatus(t, rec, 409)

	// force=true overrides the dedup guard.
	rec = doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":  map[string]string{"component": "auth"},
		"force": true,
	})
	requireStatus(t, rec, 201)
	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Warning == "" {
		t.Error("expected forced-duplicate warning")
	}
}

func TestPourFormula_UnresolvedPlaceholderVarRejected(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars": map[string]string{"component": "{{fire_time_iso}}"},
	})
	requireStatus(t, rec, 400)
}

func TestPourFormula_EphemeralWisp(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":      map[string]string{"component": "auth"},
		"ephemeral": true,
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	var molFields struct {
		Ephemeral bool `json:"ephemeral"`
	}
	if err := json.Unmarshal(ms.beads[resp.Molecule.ID].Fields, &molFields); err != nil {
		t.Fatal(err)
	}
	if !molFields.Ephemeral {
		t.Error("expected ephemeral=true on wisp molecule")
	}
}

func TestPourFormula_ExpansionFormulaRejected(t *testing.T) {
	_, ms, h := newTestServer()
	fields, _ := json.Marshal(map[string]any{
		"formula_type": "expansion",
		"steps":        []map[string]any{{"id": "a", "title": "A"}},
	})
	ms.beads["gd-exp-1"] = &model.Bead{
		ID: "gd-exp-1", Kind: model.KindData, Type: "formula",
		Title: "expansion", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-exp-1/pour", map[string]any{})
	requireStatus(t, rec, 400)
}

func TestPourFormula_LabelsMergedAndAttributionDerived(t *testing.T) {
	_, ms, h := newTestServerAuth()
	pourFormulaFixture(t, ms)
	ms.beads["gd-formula-1"].Labels = []string{"project:gardenio"}
	ms.labels["gd-formula-1"] = []string{"project:gardenio"}

	rec := doJSONAs(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":   map[string]string{"component": "auth"},
		"labels": []string{"kind:formula-ux"},
	}, "alice@example.com")
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	mol := ms.beads[resp.Molecule.ID]
	labels := map[string]bool{}
	for _, l := range mol.Labels {
		labels[l] = true
	}
	if !labels["project:gardenio"] || !labels["kind:formula-ux"] {
		t.Errorf("molecule labels = %v, want formula + request labels merged", mol.Labels)
	}
	// AUDIT §C3: CreatedBy derives from the authenticated identity.
	if mol.CreatedBy != "alice@example.com" {
		t.Errorf("molecule created_by = %q, want alice@example.com", mol.CreatedBy)
	}
}

func TestPourFormula_TriggeredByLineage(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	ms.beads["gd-src-1"] = &model.Bead{
		ID: "gd-src-1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "source", Status: model.StatusOpen,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":            map[string]string{"component": "auth"},
		"source_bead_ids": []string{"gd-src-1"},
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	var found bool
	for _, d := range ms.deps[resp.Molecule.ID] {
		if d.Type == model.DepTriggeredBy && d.DependsOnID == "gd-src-1" {
			found = true
		}
	}
	if !found {
		t.Error("expected triggered-by dep from molecule to source bead")
	}
}

// seedTargetPlan adds a plan epic (a type=epic bead carrying the `plan` label) so
// a pour can emplace a molecule into it as a phase (ka-0000004940).
func seedTargetPlan(ms *mockStore, id string) {
	ms.beads[id] = &model.Bead{
		ID: id, Kind: model.KindIssue, Type: model.TypeEpic,
		Title: "My plan", Status: model.StatusOpen,
	}
	// mockStore.GetBead sources labels from ms.labels, not the bead's own field.
	ms.labels[id] = []string{"plan"}
}

func TestPourFormula_TargetPlanEmplacesAsPhase(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":        map[string]string{"component": "auth"},
		"target_plan": "gd-plan-1",
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	molID := resp.Molecule.ID

	// The molecule is parented to the plan epic (parent-child dep).
	var parented bool
	for _, d := range ms.deps[molID] {
		if d.Type == model.DepParentChild && d.DependsOnID == "gd-plan-1" {
			parented = true
		}
	}
	if !parented {
		t.Errorf("molecule %s missing parent-child dep to plan gd-plan-1; deps=%+v", molID, ms.deps[molID])
	}

	// The plan's phase_order lists the molecule id — what makes it render as a phase.
	order := mockArrayField(ms.beads["gd-plan-1"], "phase_order")
	if len(order) != 1 || order[0] != molID {
		t.Errorf("plan phase_order = %v, want [%s]", order, molID)
	}
}

// TestPourFormula_TargetPlanAppendsToExistingPhaseOrder — emplacing into a plan
// that ALREADY has phases appends the new molecule to phase_order rather than
// clobbering the existing entries. This exercises the append-if-absent property
// the handler relies on (ka-0000000988): AppendBeadArrayField preserves prior
// phases, so a concurrent/prior phase add is never lost when a pour emplaces a
// new one (gd-gaQa1vLyel).
func TestPourFormula_TargetPlanAppendsToExistingPhaseOrder(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	// The plan already carries two phases before the pour.
	mockSetArrayField(ms.beads["gd-plan-1"], "phase_order", []string{"gd-phase-a", "gd-phase-b"})

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":        map[string]string{"component": "auth"},
		"target_plan": "gd-plan-1",
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	molID := resp.Molecule.ID

	// The new molecule is APPENDED — the two pre-existing phases are preserved and
	// still lead, with the poured molecule last.
	order := mockArrayField(ms.beads["gd-plan-1"], "phase_order")
	want := []string{"gd-phase-a", "gd-phase-b", molID}
	if len(order) != len(want) {
		t.Fatalf("plan phase_order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("phase_order[%d] = %q, want %q (full: %v)", i, order[i], want[i], order)
		}
	}
}

func TestPourFormula_TargetPlanNotFound(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":        map[string]string{"component": "auth"},
		"target_plan": "gd-nope",
	})
	requireStatus(t, rec, 400)
	if len(ms.beads) != before {
		t.Errorf("a bad target must create no beads: %d → %d", before, len(ms.beads))
	}
}

func TestPourFormula_TargetPlanNotAPlan(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	// An epic WITHOUT the `plan` label is not a plan.
	ms.beads["gd-epic-1"] = &model.Bead{
		ID: "gd-epic-1", Kind: model.KindIssue, Type: model.TypeEpic,
		Title: "plain epic", Status: model.StatusOpen,
	}
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":        map[string]string{"component": "auth"},
		"target_plan": "gd-epic-1",
	})
	requireStatus(t, rec, 400)
	if len(ms.beads) != before {
		t.Errorf("a non-plan target must create no beads: %d → %d", before, len(ms.beads))
	}
}

func TestPourFormula_TargetPlanClosedRejected(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	ms.beads["gd-plan-1"].Status = model.StatusClosed // a completed plan
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":        map[string]string{"component": "auth"},
		"target_plan": "gd-plan-1",
	})
	requireStatus(t, rec, 409)
	if len(ms.beads) != before {
		t.Errorf("emplacing into a closed plan must create no beads: %d → %d", before, len(ms.beads))
	}
}

func TestPourFormula_TargetPlanIgnoredOnDryRun(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":        map[string]string{"component": "auth"},
		"target_plan": "gd-plan-1",
		"dry_run":     true,
	})
	requireStatus(t, rec, 200)

	// A dry run emplaces nothing: the plan's phase_order stays empty.
	if order := mockArrayField(ms.beads["gd-plan-1"], "phase_order"); len(order) != 0 {
		t.Errorf("dry run must not touch phase_order; got %v", order)
	}
}

// TestPourFormula_TargetPlanCrossProjectForbidden — the fourth rejection branch
// of "reject a target that is not an open plan epic THE CALLER MAY OPERATE"
// (gd-USEEGEpCbw): a caller scoped to project X may not emplace a phase into a
// plan that lives in project Y. The formula itself lives in the caller's project
// so it clears the formula-level authz check (http_formula_pour.go:150); the
// target plan carries a DIFFERENT project label, so the target-plan authz check
// (http_formula_pour.go:291, beadMatchesTokenProject) fails it with 403 BEFORE
// any bead is created. Driven the token-project way token_project_test.go does —
// the handler called directly with a WithTokenProject context, the only way to
// exercise the scoped-token branch in a unit test.
func TestPourFormula_TargetPlanCrossProjectForbidden(t *testing.T) {
	s, ms, _ := newTestServer()
	pourFormulaFixture(t, ms)
	// The formula is in the caller's project → passes the formula-level authz.
	ms.labels["gd-formula-1"] = []string{"project:myproject"}
	// The target plan is a real, open plan epic — but in a DIFFERENT project the
	// token is not scoped to, so only the "may operate" check should reject it.
	seedTargetPlan(ms, "gd-plan-1")
	ms.labels["gd-plan-1"] = []string{"plan", "project:otherproject"}
	before := len(ms.beads)

	body, _ := json.Marshal(map[string]any{
		"vars":        map[string]string{"component": "auth"},
		"target_plan": "gd-plan-1",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/formulas/gd-formula-1/pour", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", "gd-formula-1")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	s.handlePourFormula(rec, req)

	requireStatus(t, rec, 403)
	if len(ms.beads) != before {
		t.Errorf("a cross-project target must create no beads: %d → %d", before, len(ms.beads))
	}
	// And nothing was emplaced into the plan the caller may not operate.
	if order := mockArrayField(ms.beads["gd-plan-1"], "phase_order"); len(order) != 0 {
		t.Errorf("a forbidden pour must not touch the plan's phase_order; got %v", order)
	}
}

// TestPourFormula_NoTargetLeavesMoleculeFreestanding — the "keep no-target
// behaviour byte-identical" half of gd-USEEGEpCbw. A pour with NO target_plan
// must behave exactly as it did before the emplacement feature existed: the
// molecule is created freestanding (molParent is "" so createBead wires NO
// parent-child edge to any plan/epic), and the phase_order append path is fully
// skipped (no bead gains a phase_order field). This pins the two observable
// side-effects the target-plan path would otherwise add, so a future change to
// the emplacement block can never silently leak into the no-target path.
func TestPourFormula_NoTargetLeavesMoleculeFreestanding(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars": map[string]string{"component": "auth"},
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	molID := resp.Molecule.ID

	// The molecule is freestanding: it carries NO parent-child edge (a target_plan
	// would have parented it under the plan epic). Its STEPS are parented to the
	// molecule, but the molecule itself has no parent — that is the byte-identical
	// pre-emplacement shape.
	for _, d := range ms.deps[molID] {
		if d.Type == model.DepParentChild {
			t.Errorf("no-target pour must leave the molecule freestanding; got parent-child dep %s → %s", molID, d.DependsOnID)
		}
	}

	// The emplacement append is skipped entirely: no bead gains a phase_order.
	for id, b := range ms.beads {
		if order := mockArrayField(b, "phase_order"); len(order) != 0 {
			t.Errorf("no-target pour must not write phase_order; bead %s has %v", id, order)
		}
	}
}

// TestPourFormula_TargetPhaseTitleOverridesMoleculeTitle — a non-blank
// target_phase_title names the poured molecule (ka-0000003236). Since the
// container's title is what renders as the plan phase title, this is the pour
// form's "Phase title" field.
func TestPourFormula_TargetPhaseTitleOverridesMoleculeTitle(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":               map[string]string{"component": "auth"},
		"target_plan":        "gd-plan-1",
		"target_phase_title": "Security review",
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	// The override replaces the var-substituted formula title ("Ship auth").
	if resp.Molecule.Title != "Security review" {
		t.Errorf("molecule title = %q, want %q (override)", resp.Molecule.Title, "Security review")
	}
	if got := ms.beads[resp.Molecule.ID].Title; got != "Security review" {
		t.Errorf("persisted molecule title = %q, want %q", got, "Security review")
	}
}

// TestPourFormula_BlankTargetPhaseTitleKeepsFormulaTitle — a blank or
// whitespace-only override is ignored, so the var-substituted formula title
// remains the default (ka-0000003236).
func TestPourFormula_BlankTargetPhaseTitleKeepsFormulaTitle(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":               map[string]string{"component": "auth"},
		"target_phase_title": "   ",
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	// Whitespace-only override is treated as absent: the formula title wins.
	if resp.Molecule.Title != "Ship auth" {
		t.Errorf("molecule title = %q, want %q (default formula title)", resp.Molecule.Title, "Ship auth")
	}
}

// ── Idempotency (ka-0000005875) ──────────────────────────────────────────────

// TestPourIdempotencyKey_Deterministic pins the key-derivation contract: same
// formula + same vars (regardless of map insertion order) within the same time
// bucket yield the SAME key, and any change to formula / vars / bucket yields a
// different one. This is what makes a double-submit collapse while a later
// re-pour stays distinct.
func TestPourIdempotencyKey_Deterministic(t *testing.T) {
	base := time.Unix(1_700_000_000, 0).UTC()

	k1 := pourIdempotencyKey("gd-formula-1", map[string]string{"a": "1", "b": "2"}, base)
	// Same content, different map literal (Go map order is unspecified) — the
	// derivation sorts keys, so the digest must be identical.
	k2 := pourIdempotencyKey("gd-formula-1", map[string]string{"b": "2", "a": "1"}, base)
	if k1 != k2 {
		t.Errorf("key not stable across var order: %q vs %q", k1, k2)
	}
	if !strings.HasPrefix(k1, "pour:gd-formula-1:") {
		t.Errorf("key %q missing pour:<formula>: prefix", k1)
	}

	// Different vars → different key.
	if got := pourIdempotencyKey("gd-formula-1", map[string]string{"a": "1", "b": "3"}, base); got == k1 {
		t.Error("different var values produced the same key")
	}
	// Different formula → different key.
	if got := pourIdempotencyKey("gd-formula-2", map[string]string{"a": "1", "b": "2"}, base); got == k1 {
		t.Error("different formula produced the same key")
	}
	// A later time bucket → different key (a re-pour after the window is fresh).
	if got := pourIdempotencyKey("gd-formula-1", map[string]string{"a": "1", "b": "2"}, base.Add(2*pourDedupWindow)); got == k1 {
		t.Error("a later time bucket produced the same key")
	}
	// Within the same bucket → same key.
	if got := pourIdempotencyKey("gd-formula-1", map[string]string{"a": "1", "b": "2"}, base.Add(pourDedupWindow/3)); got != k1 {
		t.Error("same time bucket produced a different key")
	}
}

// TestStepIdempotencyKey derives a per-step key from the molecule key, and
// returns empty when the molecule key is empty (force=true) so a forced pour's
// steps are all genuinely new.
func TestStepIdempotencyKey(t *testing.T) {
	if got := stepIdempotencyKey("pour:gd-f:abc", "design"); got != "pour:gd-f:abc:step:design" {
		t.Errorf("stepIdempotencyKey = %q, want pour:gd-f:abc:step:design", got)
	}
	if got := stepIdempotencyKey("", "design"); got != "" {
		t.Errorf("empty molecule key must yield empty step key, got %q", got)
	}
}

// TestPourFormula_StampsIdempotencyKeyOnMoleculeAndSteps — a real pour persists
// the derived idempotency key on the molecule and a derived per-step key on each
// step, so the migration-029 unique index and createBead's lookup-or-return can
// dedup a re-submit.
func TestPourFormula_StampsIdempotencyKeyOnMoleculeAndSteps(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars": map[string]string{"component": "auth"},
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)

	molKey := fieldValue(t, ms.beads[resp.Molecule.ID], idempotencyKeyField)
	if !strings.HasPrefix(molKey, "pour:gd-formula-1:") {
		t.Errorf("molecule idempotency_key = %q, want pour:gd-formula-1: prefix", molKey)
	}
	// Each step carries molKey:step:<stepID>.
	for stepID, beadID := range resp.Steps {
		got := fieldValue(t, ms.beads[beadID], idempotencyKeyField)
		want := molKey + ":step:" + stepID
		if got != want {
			t.Errorf("step %q idempotency_key = %q, want %q", stepID, got, want)
		}
	}
}

// TestPourFormula_DoubleSubmitReturnsExistingMolecule is the core anti-double-
// pour proof. The open-molecule dedup guard is a TOCTOU check (list → compare →
// 409): two concurrent submits both read "no duplicate" before either commits,
// so it cannot stop a race on its own. The idempotency key is the atomic
// backstop. Here we simulate the race's aftermath: the "winner" molecule already
// exists carrying the derived key, but with applied_vars the guard will NOT
// match (empty) — so the guard passes and the pour reaches createBead, where the
// key lookup-or-return finds the winner and returns it instead of pouring a
// second molecule + step tree.
func TestPourFormula_DoubleSubmitReturnsExistingMolecule(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	// Seed the concurrent winner: same derived key, non-matching applied_vars so
	// the open-molecule guard doesn't catch it (isolating the key's role).
	winnerKey := pourIdempotencyKey("gd-formula-1", map[string]string{"component": "auth"}, time.Now())
	winnerFields, _ := json.Marshal(map[string]any{
		"formula_id":        "gd-formula-1",
		"applied_vars":      map[string]string{}, // guard's MapsEqual won't match {component:auth}
		idempotencyKeyField: winnerKey,
	})
	ms.beads["gd-mol-winner"] = &model.Bead{
		ID: "gd-mol-winner", Kind: model.KindIssue, Type: "molecule",
		Title: "Ship auth", Status: model.StatusOpen, Fields: winnerFields,
	}
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars": map[string]string{"component": "auth"},
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil || resp.Molecule.ID != "gd-mol-winner" {
		t.Fatalf("double-submit created a NEW molecule instead of returning the winner: got %+v", resp.Molecule)
	}
	// No second molecule was poured. (createSteps still runs against the returned
	// molecule id, but the mock's create path is exercised by the count on the
	// molecule itself: the key must have short-circuited the molecule create.)
	if got := countBeadsOfType(ms, "molecule"); got != 1 {
		t.Errorf("molecule count = %d, want 1 (no double-pour)", got)
	}
	if len(ms.beads) < before {
		t.Errorf("bead count shrank unexpectedly: %d → %d", before, len(ms.beads))
	}
}

// TestPourFormula_ForceMintsNoIdempotencyKey — force=true means the caller
// deliberately wants a duplicate molecule (having just overridden the dedup
// guard), so the molecule and its steps carry NO idempotency key and insert
// unconditionally.
func TestPourFormula_ForceMintsNoIdempotencyKey(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)

	// An existing open molecule with identical vars trips the dedup guard; force
	// overrides it (mirrors TestPourFormula_DuplicateMoleculeConflict).
	molFields, _ := json.Marshal(map[string]any{
		"formula_id":   "gd-formula-1",
		"applied_vars": map[string]string{"component": "auth"},
	})
	ms.beads["gd-mol-existing"] = &model.Bead{
		ID: "gd-mol-existing", Kind: model.KindIssue, Type: "molecule",
		Title: "Ship auth", Status: model.StatusOpen, Fields: molFields,
	}

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":  map[string]string{"component": "auth"},
		"force": true,
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected a molecule from a forced pour")
	}
	if got := fieldValue(t, ms.beads[resp.Molecule.ID], idempotencyKeyField); got != "" {
		t.Errorf("forced pour stamped idempotency_key=%q, want empty", got)
	}
	for stepID, beadID := range resp.Steps {
		if got := fieldValue(t, ms.beads[beadID], idempotencyKeyField); got != "" {
			t.Errorf("forced pour step %q stamped idempotency_key=%q, want empty", stepID, got)
		}
	}
}

// ── target_position: the "target cell" generalization (ka-0000001001) ────────
//
// A pour into a plan appends its phase to the END of phase_order by default;
// target_position emplaces it at a chosen slot instead. These tests pin the
// four contract points: mid-slot insert shifts the tail right, position 0 makes
// it the first phase, an out-of-range slot is rejected atomically (no beads
// created), and a position without a plan is a caller error.

// TestPourFormula_TargetPositionInsertsAtSlot — a mid-slot position splices the
// poured molecule between existing phases rather than appending it.
func TestPourFormula_TargetPositionInsertsAtSlot(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	mockSetArrayField(ms.beads["gd-plan-1"], "phase_order", []string{"gd-phase-a", "gd-phase-b"})

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":            map[string]string{"component": "auth"},
		"target_plan":     "gd-plan-1",
		"target_position": 1,
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	if resp.Molecule == nil {
		t.Fatal("expected molecule in response")
	}
	molID := resp.Molecule.ID

	// Emplaced at slot 1: between phase-a and phase-b, tail shifted right.
	order := mockArrayField(ms.beads["gd-plan-1"], "phase_order")
	want := []string{"gd-phase-a", molID, "gd-phase-b"}
	if len(order) != len(want) {
		t.Fatalf("plan phase_order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("phase_order[%d] = %q, want %q (full: %v)", i, order[i], want[i], order)
		}
	}
}

// TestPourFormula_TargetPositionZeroInsertsFirst — position 0 makes the poured
// phase the plan's first.
func TestPourFormula_TargetPositionZeroInsertsFirst(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	mockSetArrayField(ms.beads["gd-plan-1"], "phase_order", []string{"gd-phase-a", "gd-phase-b"})

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":            map[string]string{"component": "auth"},
		"target_plan":     "gd-plan-1",
		"target_position": 0,
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	molID := resp.Molecule.ID

	order := mockArrayField(ms.beads["gd-plan-1"], "phase_order")
	want := []string{molID, "gd-phase-a", "gd-phase-b"}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("phase_order = %v, want %v", order, want)
		}
	}
}

// TestPourFormula_TargetPositionAtLenAppends — a position equal to the current
// phase count is exactly a tail append (the general subsumes the default).
func TestPourFormula_TargetPositionAtLenAppends(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	mockSetArrayField(ms.beads["gd-plan-1"], "phase_order", []string{"gd-phase-a", "gd-phase-b"})

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":            map[string]string{"component": "auth"},
		"target_plan":     "gd-plan-1",
		"target_position": 2, // == len(phase_order)
	})
	requireStatus(t, rec, 201)

	var resp pourFormulaResponse
	decodeJSON(t, rec, &resp)
	molID := resp.Molecule.ID

	order := mockArrayField(ms.beads["gd-plan-1"], "phase_order")
	want := []string{"gd-phase-a", "gd-phase-b", molID}
	for i := range want {
		if i >= len(order) || order[i] != want[i] {
			t.Fatalf("phase_order = %v, want %v", order, want)
		}
	}
}

// TestPourFormula_TargetPositionOutOfRange — a slot past the end is rejected
// (400) BEFORE any beads are created, so a stale UI index never lands the phase
// somewhere unexpected.
func TestPourFormula_TargetPositionOutOfRange(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	mockSetArrayField(ms.beads["gd-plan-1"], "phase_order", []string{"gd-phase-a"})
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":            map[string]string{"component": "auth"},
		"target_plan":     "gd-plan-1",
		"target_position": 5, // len is 1, so valid positions are 0..1
	})
	requireStatus(t, rec, 400)
	if len(ms.beads) != before {
		t.Errorf("an out-of-range position must create no beads: %d → %d", before, len(ms.beads))
	}
	// The pre-existing phase_order is untouched.
	if order := mockArrayField(ms.beads["gd-plan-1"], "phase_order"); len(order) != 1 || order[0] != "gd-phase-a" {
		t.Errorf("rejected pour must not touch phase_order; got %v", order)
	}
}

// TestPourFormula_TargetPositionWithoutPlan — a position with no target_plan has
// nowhere to land; reject it up front (400, no beads).
func TestPourFormula_TargetPositionWithoutPlan(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	before := len(ms.beads)

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":            map[string]string{"component": "auth"},
		"target_position": 0,
	})
	requireStatus(t, rec, 400)
	if len(ms.beads) != before {
		t.Errorf("target_position without target_plan must create no beads: %d → %d", before, len(ms.beads))
	}
}

// TestPourFormula_TargetPositionIgnoredOnDryRun — a dry run emplaces nothing, so
// a position never touches phase_order even when supplied.
func TestPourFormula_TargetPositionIgnoredOnDryRun(t *testing.T) {
	_, ms, h := newTestServer()
	pourFormulaFixture(t, ms)
	seedTargetPlan(ms, "gd-plan-1")
	mockSetArrayField(ms.beads["gd-plan-1"], "phase_order", []string{"gd-phase-a"})

	rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
		"vars":            map[string]string{"component": "auth"},
		"target_plan":     "gd-plan-1",
		"target_position": 0,
		"dry_run":         true,
	})
	requireStatus(t, rec, 200)
	if order := mockArrayField(ms.beads["gd-plan-1"], "phase_order"); len(order) != 1 || order[0] != "gd-phase-a" {
		t.Errorf("dry run must not touch phase_order; got %v", order)
	}
}

// pourNestedFormulaFixture seeds a formula with a NESTED step tree (a parent
// step carrying `children`) so the pour exercises the recursive createSteps
// descent in http_formula_pour.go — the born-open assertion below must hold at
// every depth, not just the top-level steps.
func pourNestedFormulaFixture(t *testing.T, ms *mockStore) {
	t.Helper()
	fields, err := json.Marshal(map[string]any{
		"vars": []map[string]any{
			{"name": "component", "required": true},
		},
		"steps": []map[string]any{
			{
				"id":    "parent",
				"title": "Parent for {{component}}",
				"type":  "task",
				"children": []map[string]any{
					{"id": "child-a", "title": "Child A {{component}}", "type": "task"},
					{"id": "child-b", "title": "Child B {{component}}", "type": "task", "depends_on": []string{"child-a"}},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ms.beads["gd-formula-nested"] = &model.Bead{
		ID: "gd-formula-nested", Kind: model.KindData, Type: "formula",
		Title: "Ship {{component}}", Status: model.StatusOpen, Fields: fields,
	}
}

// assertBornReadyClean asserts the born-honest contract for one poured bead:
// it must be status=open with an EMPTY assignee, and (for a step/molecule) carry
// the formula-scaffolding fields (formula_id for the molecule, source_formula for
// a step) that let `gb news` recognize and drop it. This is the exact shape the
// UPDATE-path invariant (ka-0000006063) protects on the way IN; here we pin the
// CREATE path so a pour can never mint the polluting state in the first place.
func assertBornReadyClean(t *testing.T, b *model.Bead, wantScaffoldField string) {
	t.Helper()
	if b == nil {
		t.Fatal("assertBornReadyClean: nil bead")
	}
	if b.Status != model.StatusOpen {
		t.Errorf("poured bead %s born status=%q, want %q — an eager in_progress pollutes gb ready/news (ka-0000006063)", b.ID, b.Status, model.StatusOpen)
	}
	// The pollution the precedent fixed is specifically in_progress WITHOUT an
	// assignee, which gb news reports as an orphan. A born-open bead has no
	// assignee, and that is correct — it becomes one only at atomic claim.
	if strings.TrimSpace(b.Assignee) != "" {
		t.Errorf("poured bead %s born with assignee=%q, want empty (assignee is set only at claim)", b.ID, b.Assignee)
	}
	if wantScaffoldField != "" && fieldValue(t, b, wantScaffoldField) == "" {
		t.Errorf("poured bead %s missing %q field — gb news isFormulaScaffolding relies on it to drop the bead", b.ID, wantScaffoldField)
	}
}

// TestPourFormula_StepsBornOpenNotPollutingReadyNews is the CONFIRM/status-honesty
// regression for gd-1b4xryWExs (precedent ka-0000006063). Emplacing a formula pour
// into a plan creates a molecule phase plus its step beads. Those beads must be
// born status=open with NO assignee — never eagerly in_progress-without-assignee,
// the exact state that once polluted `gb ready`/`gb news`. The UPDATE path is
// already guarded (TestGRPCUpdateBead_InProgressRequiresAssignee); this pins the
// pour CREATE path, recursively, so no future edit to createSteps can reintroduce
// the regression. It also asserts each bead carries the scaffolding field
// (formula_id / source_formula) that `gb news` uses to drop poured beads outright.
func TestPourFormula_StepsBornOpenNotPollutingReadyNews(t *testing.T) {
	// Both a freestanding pour and a plan-emplaced pour go through the same
	// createBead/createSteps path; run both so the plan target can never diverge.
	t.Run("Freestanding", func(t *testing.T) {
		_, ms, h := newTestServer()
		pourFormulaFixture(t, ms)

		rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-1/pour", map[string]any{
			"vars": map[string]string{"component": "auth"},
		})
		requireStatus(t, rec, 201)
		var resp pourFormulaResponse
		decodeJSON(t, rec, &resp)

		assertBornReadyClean(t, ms.beads[resp.Molecule.ID], "formula_id")
		if len(resp.Steps) == 0 {
			t.Fatal("expected steps to be created")
		}
		for stepID, beadID := range resp.Steps {
			b := ms.beads[beadID]
			if b == nil {
				t.Fatalf("step %q (%s) not persisted", stepID, beadID)
			}
			assertBornReadyClean(t, b, "source_formula")
		}
	})

	t.Run("EmplacedIntoPlanIncludingNestedSteps", func(t *testing.T) {
		_, ms, h := newTestServer()
		pourNestedFormulaFixture(t, ms)
		seedTargetPlan(ms, "gd-plan-1")

		rec := doJSON(t, h, "POST", "/v1/formulas/gd-formula-nested/pour", map[string]any{
			"vars":        map[string]string{"component": "auth"},
			"target_plan": "gd-plan-1",
		})
		requireStatus(t, rec, 201)
		var resp pourFormulaResponse
		decodeJSON(t, rec, &resp)

		// The molecule phase itself must be born-clean: it is listed in the plan's
		// phase_order (renders as a phase) but must not appear as orphaned work.
		assertBornReadyClean(t, ms.beads[resp.Molecule.ID], "formula_id")

		// Every step at EVERY depth (the parent step + both nested children) must
		// be born-clean. resp.Steps is flattened across the recursion.
		wantSteps := []string{"parent", "child-a", "child-b"}
		for _, id := range wantSteps {
			beadID, ok := resp.Steps[id]
			if !ok {
				t.Fatalf("expected step %q in response steps %v", id, resp.Steps)
			}
			assertBornReadyClean(t, ms.beads[beadID], "source_formula")
		}
	})
}

// fieldValue reads a single string field off a bead's Fields JSON, or "" if
// absent. Test helper for asserting the stamped idempotency key.
func fieldValue(t *testing.T, b *model.Bead, key string) string {
	t.Helper()
	if b == nil || len(b.Fields) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(b.Fields, &m); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}
