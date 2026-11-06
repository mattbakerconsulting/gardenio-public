package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// POST /garden/bead/{id}/adopt (meadow adoption, gd-6HQyB0jgmO) is a viewer write like POST /b/{id}/parent: the mux
// gates it on the operator role before gardenhub's handler runs, and the route class keeps scoped principals out.
func TestGardenAdoptRouteIsOperatorGated(t *testing.T) {
	if got := viewerRouteClasses["POST /garden/bead/{id}/adopt"]; got != routeAdminOnly {
		t.Fatalf("adopt route class = %d, want routeAdminOnly", got)
	}
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	// No daemon client: a request the gate lets through reaches the handler and answers 503.
	srv.garden = &gardenhub.Handler{Viewer: func(*http.Request) string { return "oidc:op@example.com" }}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	post := func(roles []string) int {
		req := httptest.NewRequest("POST", "/garden/bead/gd-loose/adopt", strings.NewReader("plan=gd-plan"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if roles != nil {
			req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), roles))
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post([]string{oidcauth.RoleViewer}); code != http.StatusForbidden {
		t.Errorf("a viewer adopting: %d, want 403 from the operator gate", code)
	}
	for _, role := range []string{oidcauth.RoleOperator, oidcauth.RoleAdmin} {
		if code := post([]string{role}); code != http.StatusServiceUnavailable {
			t.Errorf("%s adopting: %d, want the handler's 503 (the gate let it through)", role, code)
		}
	}
}

// POST /garden/gardener/{id}/wake (Wake / Restart from the farm, gd-3xrmzPMA1Y) is gated the same way: the operator role at
// the mux, the admin-only class, and gardenhub's own admin check (CanWake), which main.go wires to viewerCanAdmin, the
// gate gardener run-now (spawnGardenerRunNowCore) applies to a spawn.
func TestGardenWakeRouteIsOperatorGated(t *testing.T) {
	if got := viewerRouteClasses["POST /garden/gardener/{id}/wake"]; got != routeAdminOnly {
		t.Fatalf("wake route class = %d, want routeAdminOnly", got)
	}
	srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
	// No daemon client: a request the gate lets through reaches the handler and answers 503.
	srv.garden = &gardenhub.Handler{Viewer: func(*http.Request) string { return "oidc:op@example.com" }, CanWake: viewerCanAdmin}
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	post := func(roles []string) int {
		req := httptest.NewRequest("POST", "/garden/gardener/gd-agent/wake", nil)
		if roles != nil {
			req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), roles))
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post([]string{oidcauth.RoleViewer}); code != http.StatusForbidden {
		t.Errorf("a viewer restarting: %d, want 403 from the operator gate", code)
	}
	if code := post([]string{oidcauth.RoleAdmin}); code != http.StatusServiceUnavailable {
		t.Errorf("an admin restarting: %d, want the handler's 503 (the gate let it through)", code)
	}
}
