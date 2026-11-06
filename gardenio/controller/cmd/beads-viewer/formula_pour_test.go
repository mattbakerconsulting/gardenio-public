package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// pourTestFormula seeds a workflow formula bead with one required var.
func pourTestFormula() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:    "gd-formula-1",
		Title: "Ship {{component}}",
		Type:  "formula",
		Kind:  "data",
		Fields: map[string]string{
			"steps": `[{"id":"design","title":"Design {{component}}"},{"id":"implement","title":"Implement {{component}}","depends_on":["design"]}]`,
			"vars":  `[{"name":"component","description":"Component name","required":true}]`,
		},
	}
}

// pourRequest builds an operator-authenticated request against srv.
func pourRequest(method, path string, form url.Values) *http.Request {
	var req *http.Request
	if form != nil {
		req = httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	ctx := oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator})
	req.Header.Set("X-Auth-Request-Email", "op@example.com")
	return req.WithContext(ctx)
}

func TestFormulaPourForm_RendersVars(t *testing.T) {
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-formula-1": pourTestFormula()}}
	mux := mkServer(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/formulas/gd-formula-1/pour", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"var_component", "Component name", "Pour", "Preview"} {
		if !strings.Contains(body, want) {
			t.Errorf("form missing %q", want)
		}
	}
}

func TestFormulaPourForm_NonOperatorSeesNoForm(t *testing.T) {
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-formula-1": pourTestFormula()}}
	mux := mkServer(t, fake)

	req := httptest.NewRequest("GET", "/formulas/gd-formula-1/pour", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "var_component") {
		t.Error("viewer-role request must not render the pour form inputs")
	}
}

func TestFormulaPour_SubmitRedirectsToMolecule(t *testing.T) {
	fake := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{"gd-formula-1": pourTestFormula()},
		pourResp: &beadsapi.PourFormulaResponse{
			Molecule:   &beadsapi.PourMolecule{ID: "gd-mol-9", Title: "Ship auth"},
			Steps:      map[string]string{"design": "gd-s1", "implement": "gd-s2"},
			TotalSteps: 2,
		},
	}
	mux := mkServer(t, fake)

	form := url.Values{"var_component": {"auth"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/formulas/gd-formula-1/pour", form))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d; body: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/gd-mol-9" {
		t.Errorf("redirect = %q, want /gd-mol-9", loc)
	}
	if fake.pourLastID != "gd-formula-1" {
		t.Errorf("poured formula = %q", fake.pourLastID)
	}
	if fake.pourLastReq.Vars["component"] != "auth" {
		t.Errorf("vars = %v", fake.pourLastReq.Vars)
	}
	if fake.pourLastReq.DryRun {
		t.Error("plain submit must not set dry_run")
	}
}

func TestFormulaPour_PreviewRunsDryRun(t *testing.T) {
	fake := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{"gd-formula-1": pourTestFormula()},
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

	form := url.Values{"var_component": {"auth"}, "preview": {"1"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/formulas/gd-formula-1/pour", form))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !fake.pourLastReq.DryRun {
		t.Error("preview must set dry_run on the daemon request")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Design auth") || !strings.Contains(body, "would create 1 molecule + 2 steps") {
		t.Errorf("preview plan not rendered; body: %.400s", body)
	}
}

func TestFormulaPour_DaemonErrorSurfacesMessage(t *testing.T) {
	fake := &fakeDaemon{
		beads:   map[string]*beadsapi.BeadDetail{"gd-formula-1": pourTestFormula()},
		pourErr: &beadsapi.APIError{StatusCode: 409, Message: "duplicate molecule detected: gd-mol-3"},
	}
	mux := mkServer(t, fake)

	form := url.Values{"var_component": {"auth"}}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("POST", "/formulas/gd-formula-1/pour", form))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "duplicate molecule detected") {
		t.Error("daemon error message not surfaced on the form")
	}
}

func TestFormulaPour_ViewerRoleForbidden(t *testing.T) {
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{"gd-formula-1": pourTestFormula()}}
	mux := mkServer(t, fake)

	form := url.Values{"var_component": {"auth"}}
	req := httptest.NewRequest("POST", "/formulas/gd-formula-1/pour", strings.NewReader(form.Encode()))
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

func TestFormulaPour_NotAFormula(t *testing.T) {
	fake := &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{
		"gd-task-1": {ID: "gd-task-1", Title: "task", Type: "task", Kind: "issue"},
	}}
	mux := mkServer(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/formulas/gd-task-1/pour", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestFormulasCatalog_PourActionOperatorOnly(t *testing.T) {
	fixture := formulaBeadFixture("gd-f1", "Deploy thing", "gardenio", map[string]string{
		"steps": `[{"id":"a","title":"A"}]`,
	})
	fake := &formulasFake{
		fakeDaemon:   &fakeDaemon{},
		formulasResp: &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{fixture}},
	}
	mux := mkServer(t, fake)

	// Operator sees the Pour… action.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, pourRequest("GET", "/formulas", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "/formulas/gd-f1/pour") {
		t.Error("operator catalog missing Pour action")
	}

	// Viewer role does not.
	req := httptest.NewRequest("GET", "/formulas", nil)
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "/formulas/gd-f1/pour") {
		t.Error("viewer catalog must not render Pour action")
	}
}
