package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// planPourFake seeds the plan epic + formula beads (for GetBead) and a formula
// list (for the picker). The base fakeDaemon routes a Types=[formula] list query
// to listResp, so seeding listResp is exactly what the picker reads.
func planPourFake(plan *beadsapi.BeadDetail, formulas ...*beadsapi.BeadDetail) *fakeDaemon {
	beads := map[string]*beadsapi.BeadDetail{plan.ID: plan}
	list := make([]*beadsapi.BeadDetail, 0, len(formulas))
	for _, f := range formulas {
		beads[f.ID] = f
		list = append(list, f)
	}
	// The shared fakeDaemon routes Types=[formula] queries to formulaResp (kept
	// separate from listResp so picker fixtures don't leak into other list
	// queries), so seed the picker's formulas there — this is the field the plan
	// pour picker's ListBeadsFiltered(Types=[formula]) call actually reads.
	return &fakeDaemon{
		beads:       beads,
		formulaResp: &beadsapi.ListBeadsResult{Beads: list, Total: len(list)},
	}
}

// planEpicFixture is a plan epic bead scoped to a project.
func planEpicFixture(id, project string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "epic",
		Status: "open",
		Title:  "A plan",
		Labels: []string{"plan", "project:" + project},
	}
}

// TestPlanDetail_RendersPourDisclosureForOperator verifies the "⚗ Pour a
// formula" disclosure shell renders in the plan cockpit footer for an operator
// (lazy-loading its picker) and is absent for a read-only viewer.
func TestPlanDetail_RendersPourDisclosureForOperator(t *testing.T) {
	mux := mkServer(t, seedPlansFake())

	// Operator sees the disclosure + its lazy-load hook.
	req := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	req.Header.Set("X-Auth-Request-Email", "op@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Pour a formula", `id="plan-pour"`, "/plans/gd-plan1/pours/new"} {
		if !strings.Contains(body, want) {
			t.Errorf("operator cockpit missing pour disclosure %q", want)
		}
	}

	// Read-only viewer: no disclosure (the footer's operator block is gated).
	vreq := httptest.NewRequest("GET", "/plans/gd-plan1", nil)
	vreq = vreq.WithContext(oidcauth.ContextWithRoles(vreq.Context(), []string{oidcauth.RoleViewer}))
	vrec := httptest.NewRecorder()
	mux.ServeHTTP(vrec, vreq)
	if strings.Contains(vrec.Body.String(), "Pour a formula") {
		t.Error("viewer cockpit must not render the pour disclosure")
	}
}

// TestPlanPourForm_PickerScopesToPlanProject verifies GET /plans/{id}/pours/new
// (no ?formula) renders the project-scoped formula <select>, and that the list
// query carried the plan's project label.
func TestPlanPourForm_PickerScopesToPlanProject(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	f := planPourFake(plan,
		formulaBeadFixture("gd-f1", "Security review", "gardenio", map[string]string{"steps": stepsJSON(6)}),
	)
	mux := mkServer(t, f)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/plans/gd-plan-1/pours/new", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"plan-pour-formula", "gd-f1", "Security review", "6 steps"} {
		if !strings.Contains(body, want) {
			t.Errorf("picker missing %q; body: %.400s", want, body)
		}
	}
	if f.listLastQ == nil {
		t.Fatal("no formula list query issued")
	}
	if !slices.Contains(f.listLastQ.Labels, "project:gardenio") {
		t.Errorf("picker query labels = %v, want project:gardenio scope", f.listLastQ.Labels)
	}
	if !slices.Contains(f.listLastQ.Types, "formula") {
		t.Errorf("picker query types = %v, want [formula]", f.listLastQ.Types)
	}
}

// TestPlanPourForm_ChosenFormulaRendersVars verifies GET .../pours/new?formula=<id>
// renders that formula's var inputs plus the Preview + Pour actions.
func TestPlanPourForm_ChosenFormulaRendersVars(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	formula := formulaBeadFixture("gd-f1", "Ship {{component}}", "gardenio", map[string]string{
		"steps": `[{"id":"design","title":"Design {{component}}"}]`,
		"vars":  `[{"name":"component","description":"Component name","required":true}]`,
	})
	f := planPourFake(plan, formula)
	mux := mkServer(t, f)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/plans/gd-plan-1/pours/new?formula=gd-f1", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"var_component", "Component name", "Preview", "Pour as phase"} {
		if !strings.Contains(body, want) {
			t.Errorf("form missing %q; body: %.500s", want, body)
		}
	}
}

// TestPlanPourForm_BlankReselectionRendersEmptyBody verifies re-selecting the
// blank picker option (GET .../pours/new?formula= with a present-but-empty
// value) renders just the empty body prompt into #plan-pour-body — NOT a second
// nested picker.
func TestPlanPourForm_BlankReselectionRendersEmptyBody(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	f := planPourFake(plan, formulaBeadFixture("gd-f1", "Ship it", "gardenio", map[string]string{"steps": stepsJSON(1)}))
	mux := mkServer(t, f)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/plans/gd-plan-1/pours/new?formula=", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "plan-pour-formula") {
		t.Error("blank re-selection must not render a nested picker <select>")
	}
	if !strings.Contains(body, "Pick a formula above") {
		t.Errorf("blank re-selection should render the empty body prompt; body: %.300s", body)
	}
}

// TestPlanPourForm_CrossProjectFormulaRejected verifies a formula outside the
// plan's project scope is not rendered (the daemon still enforces authz on the
// pour, but the read fragment fails closed too).
func TestPlanPourForm_CrossProjectFormulaRejected(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	other := formulaBeadFixture("gd-f9", "Other-project formula", "kbeads", map[string]string{"steps": stepsJSON(2)})
	f := planPourFake(plan, other)
	mux := mkServer(t, f)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/plans/gd-plan-1/pours/new?formula=gd-f9", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "var_") || !strings.Contains(body, "not in this plan") {
		t.Errorf("cross-project formula must not render its form; body: %.400s", body)
	}
}

// TestPlanPourForm_NonPlanIs404 verifies a pours/new against a non-epic bead
// 404s (only epics are plans), matching handlePlanDetail.
func TestPlanPourForm_NonPlanIs404(t *testing.T) {
	f := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-task-1": {ID: "gd-task-1", Type: "task", Kind: "issue", Title: "t"},
	}}
	mux := mkServer(t, f)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/plans/gd-task-1/pours/new", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestPlanPourForm_ViewerRoleForbidden verifies the read fragment is
// operator-gated at the route layer (a viewer never sees the pour form).
func TestPlanPourForm_ViewerRoleForbidden(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	f := planPourFake(plan, formulaBeadFixture("gd-f1", "Deploy", "gardenio", map[string]string{"steps": stepsJSON(1)}))
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan-1/pours/new", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// TestPlanPour_HTMXPreviewRendersStepPlanFragment verifies the cockpit Preview
// (an HTMX form POST with preview=1) re-renders the plan_pour_body fragment with
// the dry-run step plan inline — the Preview centerpiece (ka-0000001848) — and
// that the daemon request was a dry run so nothing was written.
func TestPlanPour_HTMXPreviewRendersStepPlanFragment(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	formula := formulaBeadFixture("gd-f1", "Ship {{component}}", "gardenio", map[string]string{
		"vars": `[{"name":"component","required":true}]`,
	})
	f := planPourFake(plan, formula)
	f.pourResp = &beadsapi.PourFormulaResponse{
		Plan: []beadsapi.PourPlanStep{
			{ID: "design", Title: "Design auth", Type: "task"},
			{ID: "implement", Title: "Implement auth", Type: "task", DependsOn: []string{"design"}},
		},
		TotalSteps: 2,
		DryRun:     true,
	}
	mux := mkServer(t, f)

	form := url.Values{"formula": {"gd-f1"}, "var_component": {"auth"}, "preview": {"1"}}
	req := pourRequest("POST", "/plans/gd-plan-1/pours", form)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if !f.pourLastReq.DryRun {
		t.Error("preview must set dry_run on the daemon request")
	}
	if f.pourLastReq.TargetPlan != "gd-plan-1" {
		t.Errorf("target_plan = %q, want gd-plan-1", f.pourLastReq.TargetPlan)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Design auth") || !strings.Contains(body, "would create 1 molecule + 2 steps") {
		t.Errorf("preview step plan not rendered; body: %.500s", body)
	}
	// A fragment, not a full page redirect.
	if loc := rec.Header().Get("HX-Redirect"); loc != "" {
		t.Errorf("preview must not redirect, got HX-Redirect=%q", loc)
	}
	// Var value sticky across the preview re-render.
	if !strings.Contains(body, `value="auth"`) {
		t.Errorf("submitted var value not sticky; body: %.500s", body)
	}
}

// TestPlanPour_HTMXPourRedirectsToCockpit verifies the cockpit Pour (an HTMX
// form POST without preview) emplaces via the daemon and drives a full-page
// HX-Redirect to the refreshed cockpit rather than swapping cockpit HTML into
// the disclosure body.
func TestPlanPour_HTMXPourRedirectsToCockpit(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	formula := formulaBeadFixture("gd-f1", "Ship it", "gardenio", nil)
	f := planPourFake(plan, formula)
	f.pourResp = &beadsapi.PourFormulaResponse{
		Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-9", Title: "Ship it"},
		TotalSteps: 1,
	}
	mux := mkServer(t, f)

	form := url.Values{"formula": {"gd-f1"}}
	req := pourRequest("POST", "/plans/gd-plan-1/pours", form)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if f.pourLastReq.DryRun {
		t.Error("a plain pour must not set dry_run")
	}
	if loc := rec.Header().Get("HX-Redirect"); loc != "/plans/gd-plan-1" {
		t.Errorf("HX-Redirect = %q, want /plans/gd-plan-1", loc)
	}
}

// TestPlanPour_HTMXDaemonErrorRerendersBody verifies a daemon error on an HTMX
// pour re-renders the disclosure body with the daemon's own message (so the
// operator can fix + retry in place) rather than 502ing.
func TestPlanPour_HTMXDaemonErrorRerendersBody(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	formula := formulaBeadFixture("gd-f1", "Ship {{component}}", "gardenio", map[string]string{
		"vars": `[{"name":"component","required":true}]`,
	})
	f := planPourFake(plan, formula)
	f.pourErr = &beadsapi.APIError{StatusCode: 409, Message: "duplicate molecule detected: gd-mol-3"}
	mux := mkServer(t, f)

	form := url.Values{"formula": {"gd-f1"}, "var_component": {"auth"}}
	req := pourRequest("POST", "/plans/gd-plan-1/pours", form)
	req.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "duplicate molecule detected") {
		t.Errorf("daemon error not surfaced in fragment; body: %.500s", body)
	}
	// The form re-renders so the operator can retry, with the var value sticky.
	if !strings.Contains(body, "var_component") || !strings.Contains(body, `value="auth"`) {
		t.Errorf("error re-render must keep the form + sticky value; body: %.500s", body)
	}
}

// TestPlanPour_JSONDryRunUnchanged guards the pre-existing JSON contract: a
// non-HTMX dry run still returns the step plan as JSON (programmatic callers).
func TestPlanPour_JSONDryRunUnchanged(t *testing.T) {
	plan := planEpicFixture("gd-plan-1", "gardenio")
	f := planPourFake(plan, formulaBeadFixture("gd-f1", "Ship it", "gardenio", nil))
	f.pourResp = &beadsapi.PourFormulaResponse{
		Plan:       []beadsapi.PourPlanStep{{ID: "a", Title: "A", Type: "task"}},
		TotalSteps: 1,
		DryRun:     true,
	}
	mux := mkServer(t, f)

	form := url.Values{"formula": {"gd-f1"}, "preview": {"1"}}
	// No HX-Request header → the JSON contract path.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("non-HTMX dry run must stay JSON, got Content-Type %q", ct)
	}
}
