package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Task 3b (plan ka-0000005794 Surface A): the "+ New plan" affordance + the
// actionable empty state. These tests pin the operator-gated create form onto
// /plans — the goal textarea + project selector that POST the 3a create verb
// (POST /plans) — and prove the read-only viewer never sees it.

// operatorReq builds a GET /plans request authenticated AND role-scoped as an
// operator, so the .IsOperator branches (viewerCanOperate) render.
func operatorPlansReq(target, email string) *http.Request {
	req := plansIndexReq(target, email)
	return req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleOperator}))
}

// TestPlansIndex_NewPlanDisclosureForOperator proves an operator viewing a
// POPULATED /plans sees the "+ New plan" disclosure whose form POSTs the 3a
// create verb with the goal + project fields.
func TestPlansIndex_NewPlanDisclosureForOperator(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPlansReq("/plans?operated_by=", "op@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "+ New plan") {
		t.Errorf("operator should see the '+ New plan' affordance above the list")
	}
	// The form is a native browser POST to the operator-gated 3a create verb.
	if !strings.Contains(body, `method="post" action="/plans"`) {
		t.Errorf("new-plan form must POST the 3a create verb at /plans; body:\n%s", body)
	}
	if !strings.Contains(body, `name="goal"`) {
		t.Errorf("new-plan form must carry the goal textarea")
	}
	if !strings.Contains(body, `name="project"`) {
		t.Errorf("new-plan form must carry the project selector")
	}
	// maxlength mirrors the server's maxPlanGoalLen guard so the client caps
	// before the request round-trips.
	if !strings.Contains(body, `maxlength="2000"`) {
		t.Errorf("goal textarea should cap at 2000 chars (maxPlanGoalLen)")
	}
}

// TestPlansIndex_NoNewPlanFormForViewer proves a read-only viewer sees NEITHER
// the disclosure nor a create form — the POST /plans route is operatorOnly, so
// the affordance must be too.
func TestPlansIndex_NoNewPlanFormForViewer(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	req := plansIndexReq("/plans?operated_by=", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "+ New plan") {
		t.Errorf("a read-only viewer must not see the '+ New plan' affordance")
	}
	if strings.Contains(body, `action="/plans"`) {
		t.Errorf("a read-only viewer must not see a create form posting to /plans")
	}
}

// TestPlansIndex_EmptyStateOperatorHasCreateForm proves the actionable empty
// state: a blank /plans for an operator replaces the old "mint an epic
// elsewhere" copy with the in-page create form (always-open), so a cold-start
// plan can be originated in one action.
func TestPlansIndex_EmptyStateOperatorHasCreateForm(t *testing.T) {
	f := newFake() // no hotEpicsResp → the epic query returns empty → empty state.
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"kbeads":   {Name: "kbeads"},
	}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, operatorPlansReq("/plans?operated_by=", "op@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Start a plan from a one-line goal") {
		t.Errorf("operator empty state should show the actionable create hero")
	}
	if !strings.Contains(body, `method="post" action="/plans"`) {
		t.Errorf("operator empty state must embed the create form; body:\n%s", body)
	}
	// The old out-of-viewer copy is struck through, not the primary CTA.
	if !strings.Contains(body, "the old empty state pushed you out of the viewer") {
		t.Errorf("empty state should retire the old 'mint elsewhere' copy as a strikethrough")
	}
	// The project selector is seeded from the daemon's project beads.
	if !strings.Contains(body, `<option value="kbeads"`) {
		t.Errorf("project selector should list the daemon's project beads (kbeads); body:\n%s", body)
	}
}

// TestPlansIndex_EmptyStateViewerNoForm proves a read-only viewer on a blank
// /plans keeps a plain no-plans message and gets NO create form.
func TestPlansIndex_EmptyStateViewerNoForm(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	req := plansIndexReq("/plans?operated_by=", "viewer@acme.example")
	req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), []string{oidcauth.RoleViewer}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `action="/plans"`) {
		t.Errorf("a read-only viewer must not get a create form on the empty state")
	}
	if !strings.Contains(body, "No plans yet") {
		t.Errorf("a read-only viewer should still see a no-plans message")
	}
}

// TestPlansIndex_NewPlanProjectDefaultsToFilter proves the project selector
// pre-selects the active ?project= scope, and folds it in even when the daemon's
// project list doesn't include it — so a scoped /plans always has a matching
// <option selected>.
func TestPlansIndex_NewPlanProjectDefaultsToFilter(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{"gardenio": {Name: "gardenio"}}
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	// coop is NOT in projectsResp; the active filter must still round-trip.
	mux.ServeHTTP(w, operatorPlansReq("/plans?project=coop&operated_by=", "op@acme.example"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<option value="coop" selected>`) {
		t.Errorf("project selector should pre-select the active ?project= filter; body:\n%s", body)
	}
}

// TestPlanProjectOptions_DefaultAndDedup unit-tests the selector's option list:
// gardenio is always present, blanks are dropped, the active scope is folded in,
// and there are no duplicates.
func TestPlanProjectOptions_DefaultAndDedup(t *testing.T) {
	f := newFake()
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"kbeads":   {Name: "kbeads"},
		"":         {Name: ""}, // blank must be dropped
	}
	srv := NewServer(f, testLogger(), "", "", "", nil)

	opts := srv.planProjectOptions(t.Context(), "coop")
	seen := map[string]int{}
	for _, o := range opts {
		if o == "" {
			t.Errorf("blank project name must be dropped, got %v", opts)
		}
		seen[o]++
	}
	for _, want := range []string{"gardenio", "kbeads", "coop"} {
		if seen[want] == 0 {
			t.Errorf("expected %q in options %v", want, opts)
		}
	}
	for name, n := range seen {
		if n > 1 {
			t.Errorf("project %q appears %d times; options must be deduped: %v", name, n, opts)
		}
	}
}

// TestPlanProjectOptions_DaemonErrorDegrades proves a daemon error degrades to
// the default rather than blocking the page.
func TestPlanProjectOptions_DaemonErrorDegrades(t *testing.T) {
	f := newFake()
	f.projectsErr = errFake("boom")
	srv := NewServer(f, testLogger(), "", "", "", nil)

	opts := srv.planProjectOptions(t.Context(), "")
	if len(opts) != 1 || opts[0] != "gardenio" {
		t.Errorf("daemon error should degrade to just [gardenio], got %v", opts)
	}
}
