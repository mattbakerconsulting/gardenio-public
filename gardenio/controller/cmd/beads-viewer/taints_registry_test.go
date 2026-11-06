package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

func newTestServerForTaints(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	return NewServer(daemon, slog.Default(), "", "http://test", "", nil)
}

func intPtr(n int) *int { return &n }

func TestHandleTaintsRegistry_HappyPath(t *testing.T) {
	last := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	daemon := newFake()
	daemon.taintRegistryResp = []beadsapi.TaintRegistryEntry{
		{
			ID:                1,
			Name:              "acme-production",
			Description:       "Customer PII from prod.",
			OwnerTeam:         "data-platform",
			BeadCount:         intPtr(42),
			OpenScrubRequests: 3,
			LastScrubAt:       &last,
		},
		{
			ID:        2,
			Name:      "acme-scratch",
			OwnerTeam: "growth",
			BeadCount: nil, // no grant — must render as a dash, not 0 or an error
		},
	}

	srv := newTestServerForTaints(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/taints", nil)
	rec := httptest.NewRecorder()
	srv.handleTaintsRegistry(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		"acme-production",
		"data-platform",
		"Customer PII from prod.",
		"42",
		"acme-scratch",
		"growth",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected body to contain %q\n--- body ---\n%s", want, body)
		}
	}

	// acceptance criteria: a viewer without a grant sees the row with a dash
	// bead count, not an omitted row or an error.
	if !strings.Contains(body, `<td class="taint-count is-dash">—</td>`) {
		t.Errorf("expected a dashed bead-count cell for the ungranted taint\n--- body ---\n%s", body)
	}
	if strings.Contains(body, "Failed to load") {
		t.Errorf("unexpected error banner in body")
	}
}

// TestHandleTaintsRegistry_NewTaintButtonAdminGated verifies the "New taint"
// button is rendered only for admins — everyone with viewer role sees the
// list (ADR §9.1), but only admins see the create affordance.
func TestHandleTaintsRegistry_NewTaintButtonAdminGated(t *testing.T) {
	daemon := newFake()
	daemon.taintRegistryResp = []beadsapi.TaintRegistryEntry{
		{ID: 1, Name: "acme-production", OwnerTeam: "data-platform", BeadCount: intPtr(0)},
	}
	srv := newTestServerForTaints(t, daemon)

	cases := []struct {
		name     string
		roles    []string
		wantForm bool
	}{
		{"no roles (dev bypass / static token: daemon refuses admin)", nil, false},
		{"viewer only", []string{oidcauth.RoleViewer}, false},
		{"operator", []string{oidcauth.RoleOperator}, false},
		{"admin", []string{oidcauth.RoleAdmin}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/taints", nil)
			if tc.roles != nil {
				req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), tc.roles))
			}
			rec := httptest.NewRecorder()
			srv.handleTaintsRegistry(rec, req)

			got := strings.Contains(rec.Body.String(), "New taint")
			if got != tc.wantForm {
				t.Errorf("New-taint button present = %v, want %v", got, tc.wantForm)
			}
		})
	}
}

func TestHandleTaintsRegistry_DaemonError(t *testing.T) {
	daemon := newFake()
	daemon.taintRegistryErr = &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "registry down"}

	srv := newTestServerForTaints(t, daemon)
	req := httptest.NewRequest(http.MethodGet, "/taints", nil)
	rec := httptest.NewRecorder()
	srv.handleTaintsRegistry(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degrades to an error banner)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Failed to load") {
		t.Errorf("expected error banner in body:\n%s", rec.Body.String())
	}
}

func TestHandleCreateTaintSubmit_Success(t *testing.T) {
	daemon := newFake()
	srv := newTestServerForTaints(t, daemon)

	form := strings.NewReader("name=acme-new&owner_team=data-platform&preset=production")
	req := httptest.NewRequest(http.MethodPost, "/taints", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleCreateTaintSubmit(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("HX-Redirect"); got != "/taints" {
		t.Errorf("HX-Redirect = %q, want %q", got, "/taints")
	}
	if daemon.taintCreateReq == nil {
		t.Fatal("expected CreateTaint to be called")
	}
	if daemon.taintCreateReq.Name != "acme-new" || daemon.taintCreateReq.OwnerTeam != "data-platform" || daemon.taintCreateReq.Preset != "production" {
		t.Errorf("unexpected CreateTaint request: %+v", daemon.taintCreateReq)
	}
}

func TestHandleCreateTaintSubmit_MissingFields(t *testing.T) {
	daemon := newFake()
	srv := newTestServerForTaints(t, daemon)

	form := strings.NewReader("name=&owner_team=")
	req := httptest.NewRequest(http.MethodPost, "/taints", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleCreateTaintSubmit(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=\n%s", rec.Code, rec.Body.String())
	}
	if daemon.taintCreateReq != nil {
		t.Error("CreateTaint must not be called when required fields are missing")
	}
	if !strings.Contains(rec.Body.String(), "Name is required") {
		t.Errorf("expected a name-required error, got:\n%s", rec.Body.String())
	}
}

// TestHandleCreateTaintSubmit_NoForwardableBearer verifies the admin-facing
// error path when the client fails closed with
// ErrTaintCreateRequiresUserBearer (e.g. an AVA-routed caller with no
// forwardable OIDC token) — a clear message, not a raw daemon error.
func TestHandleCreateTaintSubmit_NoForwardableBearer(t *testing.T) {
	daemon := newFake()
	daemon.taintCreateErr = beadsapi.ErrTaintCreateRequiresUserBearer
	srv := newTestServerForTaints(t, daemon)

	form := strings.NewReader("name=acme-new&owner_team=data-platform&preset=production")
	req := httptest.NewRequest(http.MethodPost, "/taints", form)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.handleCreateTaintSubmit(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "requires signing in directly") {
		t.Errorf("expected the forwardable-token guidance message, got:\n%s", rec.Body.String())
	}
}

func TestHandleCreateTaintSheet_Renders(t *testing.T) {
	daemon := newFake()
	srv := newTestServerForTaints(t, daemon)

	req := httptest.NewRequest(http.MethodGet, "/taints/new", nil)
	rec := httptest.NewRecorder()
	srv.handleCreateTaintSheet(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=\n%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"New Taint", "production", "internal", "scratch"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("expected body to contain %q\n--- body ---\n%s", want, rec.Body.String())
		}
	}
}
