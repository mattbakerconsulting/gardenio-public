package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// TestPlanPour_FormSubmitEmplacesAndRedirects verifies a browser form submit
// pours with target_plan = the URL plan id and 303s to the plan cockpit.
func TestPlanPour_FormSubmitEmplacesAndRedirects(t *testing.T) {
	fake := &fakeDaemon{
		pourResp: &beadsapi.PourFormulaResponse{
			Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-9", Title: "Ship auth"},
			Steps:      map[string]string{"design": "gd-s1"},
			TotalSteps: 1,
		},
	}
	mux := mkServer(t, fake)

	form := url.Values{"formula": {"gd-formula-1"}, "var_component": {"auth"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/plans/gd-plan-1" {
		t.Errorf("redirect = %q, want /plans/gd-plan-1", loc)
	}
	if fake.pourLastID != "gd-formula-1" {
		t.Errorf("poured formula = %q, want gd-formula-1", fake.pourLastID)
	}
	if fake.pourLastReq.TargetPlan != "gd-plan-1" {
		t.Errorf("target_plan = %q, want gd-plan-1 (the URL plan id)", fake.pourLastReq.TargetPlan)
	}
	if fake.pourLastReq.Vars["component"] != "auth" {
		t.Errorf("vars = %v, want component=auth", fake.pourLastReq.Vars)
	}
	if fake.pourLastReq.DryRun {
		t.Error("a plain submit must not set dry_run")
	}
}

// TestPlanPour_JSONSubmitReturnsMoleculeID verifies a JSON caller gets the
// emplaced molecule id back (the field "for the MR comment").
func TestPlanPour_JSONSubmitReturnsMoleculeID(t *testing.T) {
	fake := &fakeDaemon{
		pourResp: &beadsapi.PourFormulaResponse{
			Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-42", Title: "Ship auth"},
			Steps:      map[string]string{"design": "gd-s1", "implement": "gd-s2"},
			TotalSteps: 2,
		},
	}
	mux := mkServer(t, fake)

	body := `{"formula":"gd-formula-1","vars":{"component":"auth"}}`
	req := httptest.NewRequest("POST", "/plans/gd-plan-1/pours", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	var got planPourResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	if got.Molecule != "gd-mol-42" {
		t.Errorf("molecule = %q, want gd-mol-42", got.Molecule)
	}
	if got.Plan != "gd-plan-1" || got.Formula != "gd-formula-1" {
		t.Errorf("plan/formula echo = %q/%q", got.Plan, got.Formula)
	}
	if got.TotalSteps != 2 {
		t.Errorf("total_steps = %d, want 2", got.TotalSteps)
	}
	if fake.pourLastReq.TargetPlan != "gd-plan-1" {
		t.Errorf("target_plan = %q, want gd-plan-1", fake.pourLastReq.TargetPlan)
	}
	if fake.pourLastReq.DryRun {
		t.Error("a JSON submit without dry_run must not set dry_run")
	}
}

// TestPlanPour_DryRunReturnsStepPlanJSON verifies a preview submit runs a dry-run
// pour and returns the step plan as JSON without a molecule id.
func TestPlanPour_DryRunReturnsStepPlanJSON(t *testing.T) {
	fake := &fakeDaemon{
		pourResp: &beadsapi.PourFormulaResponse{
			Plan: []beadsapi.PourPlanStep{
				{ID: "design", Title: "Design auth", Type: "task"},
				{ID: "implement", Title: "Implement auth", Type: "task", DependsOn: []string{"design"}},
			},
			TotalSteps: 2,
			DryRun:     true,
		},
	}
	mux := mkServer(t, fake)

	form := url.Values{"formula": {"gd-formula-1"}, "var_component": {"auth"}, "preview": {"1"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if !fake.pourLastReq.DryRun {
		t.Error("preview must set dry_run on the daemon request")
	}
	var got planPourResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	if !got.DryRun {
		t.Error("response must echo dry_run=true")
	}
	if got.Molecule != "" {
		t.Errorf("dry run must not report a molecule id, got %q", got.Molecule)
	}
	if len(got.StepPlan) != 2 || got.StepPlan[0].Title != "Design auth" {
		t.Errorf("step_plan not returned: %+v", got.StepPlan)
	}
}

// TestPlanPour_DaemonErrorSurfacesStatusAndMessage verifies the daemon's error
// status + message pass through (e.g. the duplicate-molecule 409 guard, which
// lives entirely in the daemon).
func TestPlanPour_DaemonErrorSurfacesStatusAndMessage(t *testing.T) {
	fake := &fakeDaemon{
		pourErr: &beadsapi.APIError{StatusCode: 409, Message: "duplicate molecule detected: gd-mol-3"},
	}
	mux := mkServer(t, fake)

	body := `{"formula":"gd-formula-1"}`
	req := httptest.NewRequest("POST", "/plans/gd-plan-1/pours", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (daemon status passed through)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "duplicate molecule detected") {
		t.Errorf("daemon error message not surfaced: %s", rec.Body.String())
	}
}

// TestPlanPour_MissingFormulaIsBadRequest verifies a pour with no formula id is
// rejected before touching the daemon.
func TestPlanPour_MissingFormulaIsBadRequest(t *testing.T) {
	fake := &fakeDaemon{}
	mux := mkServer(t, fake)

	form := url.Values{"var_component": {"auth"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if fake.pourCalls != 0 {
		t.Error("a formula-less pour must not reach the daemon")
	}
}

// TestPlanPour_ViewerRoleForbidden verifies the route-layer operator gate blocks
// a viewer-role POST before any daemon write.
func TestPlanPour_ViewerRoleForbidden(t *testing.T) {
	fake := &fakeDaemon{}
	mux := mkServer(t, fake)

	form := url.Values{"formula": {"gd-formula-1"}}
	req := httptest.NewRequest("POST", "/plans/gd-plan-1/pours", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if fake.pourCalls != 0 {
		t.Error("viewer-role POST must not reach the daemon")
	}
}

// TestPlanPour_PassesEphemeralAndForce verifies the ephemeral/force knobs pass
// straight through to the daemon pour request.
func TestPlanPour_PassesEphemeralAndForce(t *testing.T) {
	fake := &fakeDaemon{
		pourResp: &beadsapi.PourFormulaResponse{
			Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-9"},
			TotalSteps: 1,
		},
	}
	mux := mkServer(t, fake)

	form := url.Values{"formula": {"gd-formula-1"}, "ephemeral": {"1"}, "force": {"1"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if !fake.pourLastReq.Ephemeral {
		t.Error("ephemeral=1 must pass through as Ephemeral")
	}
	if !fake.pourLastReq.Force {
		t.Error("force=1 must pass through as Force")
	}
}

// TestPlanPour_FormPassesPosition — a `position` form field passes through as
// TargetPosition so the browser can emplace a phase at a chosen slot
// (ka-0000001001).
func TestPlanPour_FormPassesPosition(t *testing.T) {
	fake := &fakeDaemon{
		pourResp: &beadsapi.PourFormulaResponse{
			Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-9"},
			TotalSteps: 1,
		},
	}
	mux := mkServer(t, fake)

	form := url.Values{"formula": {"gd-formula-1"}, "position": {"2"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if fake.pourLastReq.TargetPosition == nil {
		t.Fatal("position=2 must pass through as a non-nil TargetPosition")
	}
	if *fake.pourLastReq.TargetPosition != 2 {
		t.Errorf("TargetPosition = %d, want 2", *fake.pourLastReq.TargetPosition)
	}
}

// TestPlanPour_FormOmittedPositionIsNilAppend — an absent `position` field
// leaves TargetPosition nil, so the daemon tail-appends (default, unchanged).
func TestPlanPour_FormOmittedPositionIsNilAppend(t *testing.T) {
	fake := &fakeDaemon{
		pourResp: &beadsapi.PourFormulaResponse{
			Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-9"},
			TotalSteps: 1,
		},
	}
	mux := mkServer(t, fake)

	form := url.Values{"formula": {"gd-formula-1"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if fake.pourLastReq.TargetPosition != nil {
		t.Errorf("omitted position must leave TargetPosition nil, got %d", *fake.pourLastReq.TargetPosition)
	}
}

// TestPlanPour_JSONPassesPosition — a JSON `position` passes through as
// TargetPosition too (the programmatic path).
func TestPlanPour_JSONPassesPosition(t *testing.T) {
	fake := &fakeDaemon{
		pourResp: &beadsapi.PourFormulaResponse{
			Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-9"},
			TotalSteps: 1,
		},
	}
	mux := mkServer(t, fake)

	body := `{"formula":"gd-formula-1","position":0}`
	req := httptest.NewRequest("POST", "/plans/gd-plan-1/pours", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if fake.pourLastReq.TargetPosition == nil || *fake.pourLastReq.TargetPosition != 0 {
		t.Fatalf("JSON position:0 must pass through as TargetPosition=0, got %v", fake.pourLastReq.TargetPosition)
	}
}

// TestPlanPour_MalformedPositionIsBadRequest — a non-integer `position` form
// value is a caller mistake surfaced as 400, never silently dropped (which would
// pour to the wrong slot).
func TestPlanPour_MalformedPositionIsBadRequest(t *testing.T) {
	fake := &fakeDaemon{}
	mux := mkServer(t, fake)

	form := url.Values{"formula": {"gd-formula-1"}, "position": {"abc"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/plans/gd-plan-1/pours", form))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}
