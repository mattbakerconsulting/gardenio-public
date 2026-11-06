package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The three field-mutation endpoints — POST /v1/beads/{id}/increment,
// .../fields/{field}/append and .../fields/{field}/reorder — must enforce the
// same cross-project write-authz gate as their .../edit sibling: a token scoped
// to project A must not be able to mutate the fields of a bead in project B.
// Pre-fix these handlers skipped beadMatchesTokenProject entirely, so each of
// the "Denied" cases below returned 200 and mutated a foreign-project bead
// (gd-7o5WQSYCta).

func newProjectBead(t *testing.T, ms *mockStore, id, project string) {
	t.Helper()
	bead := &model.Bead{ID: id, Title: "Test", Type: "task", Kind: "issue", Status: model.StatusOpen}
	if err := ms.CreateBead(context.Background(), bead); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
	if err := ms.AddLabel(context.Background(), id, "project:"+project); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
}

func TestTokenProjectScope_IncrementBeadFields_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-inc-denied", "otherproject")

	body, _ := json.Marshal(map[string]float64{"counter": 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-inc-denied/increment", bytes.NewReader(body))
	req.SetPathValue("id", "bead-inc-denied")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	srv.handleIncrementBeadFields(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	// The foreign-project bead must not have been mutated.
	if got := ms.beads["bead-inc-denied"]; len(got.Fields) != 0 {
		t.Fatalf("expected fields untouched, got %s", got.Fields)
	}
}

func TestTokenProjectScope_IncrementBeadFields_Allowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-inc-ok", "myproject")

	body, _ := json.Marshal(map[string]float64{"counter": 2})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-inc-ok/increment", bytes.NewReader(body))
	req.SetPathValue("id", "bead-inc-ok")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	srv.handleIncrementBeadFields(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_AppendBeadArrayField_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-app-denied", "otherproject")

	body, _ := json.Marshal(appendArrayFieldRequest{Element: "phase-x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-app-denied/fields/phase_order/append", bytes.NewReader(body))
	req.SetPathValue("id", "bead-app-denied")
	req.SetPathValue("field", "phase_order")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	srv.handleAppendBeadArrayField(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["bead-app-denied"]; len(got.Fields) != 0 {
		t.Fatalf("expected fields untouched, got %s", got.Fields)
	}
}

func TestTokenProjectScope_AppendBeadArrayField_Allowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-app-ok", "myproject")

	body, _ := json.Marshal(appendArrayFieldRequest{Element: "phase-x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-app-ok/fields/phase_order/append", bytes.NewReader(body))
	req.SetPathValue("id", "bead-app-ok")
	req.SetPathValue("field", "phase_order")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	srv.handleAppendBeadArrayField(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestTokenProjectScope_ReorderBeadArrayField_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-reorder-denied", "otherproject")
	// Seed an existing array so a successful CAS would be observable.
	mockSetArrayField(ms.beads["bead-reorder-denied"], "phase_order", []string{"a", "b"})

	body, _ := json.Marshal(casArrayFieldRequest{Expected: []string{"a", "b"}, Next: []string{"b", "a"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-reorder-denied/fields/phase_order/reorder", bytes.NewReader(body))
	req.SetPathValue("id", "bead-reorder-denied")
	req.SetPathValue("field", "phase_order")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	srv.handleReorderBeadArrayField(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	// The foreign-project array must remain in its original order.
	if got := mockArrayField(ms.beads["bead-reorder-denied"], "phase_order"); got[0] != "a" || got[1] != "b" {
		t.Fatalf("expected array untouched [a b], got %v", got)
	}
}

func TestTokenProjectScope_ReorderBeadArrayField_Allowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-reorder-ok", "myproject")
	mockSetArrayField(ms.beads["bead-reorder-ok"], "phase_order", []string{"a", "b"})

	body, _ := json.Marshal(casArrayFieldRequest{Expected: []string{"a", "b"}, Next: []string{"b", "a"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-reorder-ok/fields/phase_order/reorder", bytes.NewReader(body))
	req.SetPathValue("id", "bead-reorder-ok")
	req.SetPathValue("field", "phase_order")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	srv.handleReorderBeadArrayField(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// An unscoped token (no project in context) must retain full access to any
// bead's field mutations — the gate only narrows a project-scoped token.
func TestTokenProjectScope_IncrementBeadFields_NoScope(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-inc-noscope", "otherproject")

	body, _ := json.Marshal(map[string]float64{"counter": 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-inc-noscope/increment", bytes.NewReader(body))
	req.SetPathValue("id", "bead-inc-noscope")
	// No WithTokenProject — unrestricted token.
	rec := httptest.NewRecorder()

	srv.handleIncrementBeadFields(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unscoped token, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// A project-scoped token targeting a missing bead must get 404, not 403 — the
// gate fetches the bead first and surfaces not-found before the scope check.
func TestTokenProjectScope_IncrementBeadFields_Missing(t *testing.T) {
	srv, _, _ := newTestServer()

	body, _ := json.Marshal(map[string]float64{"counter": 1})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads/bead-absent/increment", bytes.NewReader(body))
	req.SetPathValue("id", "bead-absent")
	req = req.WithContext(WithTokenProject(req.Context(), "myproject"))
	rec := httptest.NewRecorder()

	srv.handleIncrementBeadFields(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

// --- OIDC project-scope gate ---
//
// The same three endpoints must also enforce the OIDC-user project gate the
// canonical PATCH write path runs (oidcAllowedProjects / beadMatchesOIDCProjects):
// a browser-authenticated operator restricted to project A must not mutate a
// project-B bead's fields. Pre-fix (and after the token-only first cut) these
// handlers skipped the OIDC gate, so each "Denied" case below returned 200 and
// mutated a foreign-project bead (gd-7o5WQSYCta).

// oidcProjRequest builds a POST authenticated as an OIDC user restricted to
// `project`, carrying the given JSON body.
func oidcProjRequest(t *testing.T, ms *mockStore, project, path string, body any) *http.Request {
	t.Helper()
	makeUserBead(ms, "gd-user-fmwrite", "restricted-fmwrite@example.com", project, "crew")
	buf, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	ctx := oidcCtx("restricted-fmwrite@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")
	return req.WithContext(ctx)
}

func TestOIDCProjectScope_IncrementBeadFields_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-inc-oidc-denied", "otherproject")

	req := oidcProjRequest(t, ms, "myproject", "/v1/beads/bead-inc-oidc-denied/increment", map[string]float64{"counter": 1})
	req.SetPathValue("id", "bead-inc-oidc-denied")
	rec := httptest.NewRecorder()

	srv.handleIncrementBeadFields(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["bead-inc-oidc-denied"]; len(got.Fields) != 0 {
		t.Fatalf("expected fields untouched, got %s", got.Fields)
	}
}

func TestOIDCProjectScope_AppendBeadArrayField_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-app-oidc-denied", "otherproject")

	req := oidcProjRequest(t, ms, "myproject", "/v1/beads/bead-app-oidc-denied/fields/phase_order/append", map[string]string{"element": "evil"})
	req.SetPathValue("id", "bead-app-oidc-denied")
	req.SetPathValue("field", "phase_order")
	rec := httptest.NewRecorder()

	srv.handleAppendBeadArrayField(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["bead-app-oidc-denied"]; len(got.Fields) != 0 {
		t.Fatalf("expected fields untouched, got %s", got.Fields)
	}
}

func TestOIDCProjectScope_ReorderBeadArrayField_Denied(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-reo-oidc-denied", "otherproject")

	req := oidcProjRequest(t, ms, "myproject", "/v1/beads/bead-reo-oidc-denied/fields/phase_order/reorder",
		casArrayFieldRequest{Expected: []string{}, Next: []string{"evil"}})
	req.SetPathValue("id", "bead-reo-oidc-denied")
	req.SetPathValue("field", "phase_order")
	rec := httptest.NewRecorder()

	srv.handleReorderBeadArrayField(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing), got %d; body: %s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["bead-reo-oidc-denied"]; len(got.Fields) != 0 {
		t.Fatalf("expected fields untouched, got %s", got.Fields)
	}
}

// An in-scope OIDC operator is unaffected — the gate only narrows a restricted
// user, matching every other authz site.
func TestOIDCProjectScope_IncrementBeadFields_Allowed(t *testing.T) {
	srv, ms, _ := newTestServer()
	newProjectBead(t, ms, "bead-inc-oidc-ok", "myproject")

	req := oidcProjRequest(t, ms, "myproject", "/v1/beads/bead-inc-oidc-ok/increment", map[string]float64{"counter": 1})
	req.SetPathValue("id", "bead-inc-oidc-ok")
	rec := httptest.NewRecorder()

	srv.handleIncrementBeadFields(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}
