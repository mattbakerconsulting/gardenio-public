package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/oidcauth"
)

// These tests lock the render-gate = route-gate parity fix for bug
// ka-0000005377: the /gardener/{id} Operate control bar and the GET
// /gardener/{id}/edit config form must render ONLY when the matching mutation
// POST would actually be accepted. Every gardener mutation POST rejects a
// request with no authenticated identity (email=="" → 401 Unauthenticated) on
// top of the operatorOnly route gate. Before the fix, an unauthenticated
// in-cluster request (no OIDC roles → viewerCanOperate true, no email) got the
// full lever bar + an editable GET /edit form, all of which 401 on submit —
// the exact "render-gate ≠ route-gate" dead-end the CDQR audit flagged.

// noAuthReq builds a request that carries neither an OIDC email header nor any
// realm roles — the shape of an unauthenticated in-cluster caller (the CDQR
// probe). viewerCanOperate returns true for it (RBAC is not enforced without
// roles), so it is exactly the case the email-parity gate must catch.
func noAuthReq(method, path string) *http.Request {
	return httptest.NewRequest(method, path, nil)
}

// TestGardenerPage_UnauthenticatedHidesControls pins the bug's headline
// symptom: an unauthenticated caller must NOT see the Operate bar or the
// Edit-config host, and must instead get a muted operator-only note — never a
// live-looking lever the POST would refuse.
func TestGardenerPage_UnauthenticatedHidesControls(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServerWithNow(t, f, gardenerPageNow)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, noAuthReq("GET", "/gardener/gd-cap"))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// None of the operator-only lever markers may leak to an unauthenticated
	// viewer (these all POST to routes the handler 401s without an email).
	for _, leaked := range []string{
		`id="gardener-controls"`,
		"/gardener/gd-cap/disable",
		"/gardener/gd-cap/run-now",
		"/gardener/gd-cap/cancel-run",
		"/gardener/gd-cap/retire",
		`id="gardener-config-host"`,
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("unauthenticated page leaked operator affordance %q (render-gate ≠ route-gate)", leaked)
		}
	}

	// The muted operator-only note stands in for the withheld levers so the
	// affordance is explained rather than silently missing.
	if !strings.Contains(body, "gardener-operator-note") {
		t.Errorf("unauthenticated page should render the muted operator-only note; body=%s", body)
	}
	// The read surface still renders — only the levers are withheld.
	if !strings.Contains(body, "gd-cap") {
		t.Errorf("unauthenticated page should still render the gardener body")
	}
}

// TestGardenerEditForm_UnauthenticatedGetsNote pins the second half of the fix:
// GET /gardener/{id}/edit must gate the editable form BODY on the same
// authed-operator parity, not just the route. An unauthenticated caller gets
// the friendly operator-only note (200, htmx-swappable) — never the editable
// terminal_goal / exit_condition / on_exit inputs a save would 401.
func TestGardenerEditForm_UnauthenticatedGetsNote(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, noAuthReq("GET", "/gardener/gd-cap/edit"))
	if w.Code != http.StatusOK {
		t.Fatalf("edit form (unauth): expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The editable form must NOT render to an unauthenticated caller.
	for _, leaked := range []string{
		`name="terminal_goal"`,
		`name="exit_condition"`,
		`name="on_exit"`,
		`name="stop_json"`,
		"/gardener/gd-cap/config",
	} {
		if strings.Contains(body, leaked) {
			t.Errorf("edit form leaked editable field %q to unauthenticated caller", leaked)
		}
	}
	// The friendly operator-only note stands in for the withheld form.
	if !strings.Contains(body, "operator role") {
		t.Errorf("edit form (unauth): expected operator-only note; body=%s", body)
	}
}

// TestGardenerRenderGate_MatchesRouteGate is the direct parity assertion: for a
// given viewer, the page renders the Operate bar IFF a mutation POST from that
// same viewer succeeds. It walks the three canonical viewer shapes and checks
// both sides agree, so a future refactor of either gate cannot let them drift.
func TestGardenerRenderGate_MatchesRouteGate(t *testing.T) {
	barMarker := `id="gardener-controls"`

	cases := []struct {
		name        string
		withEmail   bool
		roles       []string
		wantControl bool
	}{
		// Unauthenticated in-cluster caller: no email, no roles. viewerCanOperate
		// is true (no RBAC), but the POST 401s on the missing email — so NO bar.
		{"unauthenticated", false, nil, false},
		// Authenticated operator: email + operator role → POST succeeds → bar.
		{"authed operator", true, []string{oidcauth.RoleOperator}, true},
		// Authenticated viewer: email but under-privileged → POST 403 → no bar.
		{"authed viewer", true, []string{oidcauth.RoleViewer}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedGardenerOpsFake(t)
			mux := mkServerWithNow(t, f, gardenerPageNow)

			mkReq := func(method, path string) *http.Request {
				req := httptest.NewRequest(method, path, nil)
				if tc.withEmail {
					req.Header.Set("X-Auth-Request-Email", "op@example.com")
				}
				if tc.roles != nil {
					req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), tc.roles))
				}
				return req
			}

			// Render side.
			pw := httptest.NewRecorder()
			mux.ServeHTTP(pw, mkReq("GET", "/gardener/gd-cap"))
			if pw.Code != http.StatusOK {
				t.Fatalf("page: expected 200, got %d", pw.Code)
			}
			gotControl := strings.Contains(pw.Body.String(), barMarker)
			if gotControl != tc.wantControl {
				t.Errorf("render gate: control bar present=%v, want %v", gotControl, tc.wantControl)
			}

			// Route side: a disable POST must succeed IFF the bar rendered.
			rw := httptest.NewRecorder()
			mux.ServeHTTP(rw, mkReq("POST", "/gardener/gd-cap/disable"))
			routeOK := rw.Code == http.StatusOK
			if routeOK != tc.wantControl {
				t.Errorf("route gate: disable POST ok=%v (code %d), want ok=%v — render-gate ≠ route-gate",
					routeOK, rw.Code, tc.wantControl)
			}
		})
	}
}
