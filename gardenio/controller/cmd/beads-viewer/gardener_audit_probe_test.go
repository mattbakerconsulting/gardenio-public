package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// The audit-probe dry-run contract (bug ka-0000007079): a request carrying the
// X-Gardenio-Audit-Probe header reaches every gardener mutation handler through
// the SAME auth gates as a real click (operatorOnly route + email != "" 401),
// but the handler MUST return the would-succeed status (200) WITHOUT performing
// any state change. This lets a CDQR operability audit confirm "the route exists
// and an operator would be accepted" against a REAL prod gardener without the
// probe disabling/retiring/running it — the exact blast radius that closed live
// prod gardener ka-0000003719 in the 2026-08-25T07:47:00Z iteration.

// probeReq builds an operator-scoped POST that also declares itself an audit
// probe. Operator identity is present (mirrors the incident: the operate BAR
// screenshot needs X-Auth-Request-Email set), so ONLY the dry-run guard stands
// between the probe and a live mutation.
func probeReq(method, path string, form url.Values) *http.Request {
	req := opsReq(method, path, form)
	req.Header.Set(auditProbeHeader, "1")
	return req
}

// assertNoMutation fails if the fake daemon recorded ANY write — no create
// (run-now spawn), no close (cancel-run / retire), no field update
// (enable/disable/pause/config).
func assertNoMutation(t *testing.T, f *fakeDaemon, label string) {
	t.Helper()
	if f.createCalls != 0 {
		t.Errorf("%s: audit probe must not create beads, got %d CreateBead calls", label, f.createCalls)
	}
	if f.closeCalls != 0 {
		t.Errorf("%s: audit probe must not close beads, got %d CloseBeadAs calls", label, f.closeCalls)
	}
	if f.updateFieldsCalls != 0 {
		t.Errorf("%s: audit probe must not update fields, got %d UpdateBeadFieldsTyped calls", label, f.updateFieldsCalls)
	}
}

// assertProbeAck fails unless the response is the 200 dry-run acknowledgement
// (the X-Gardenio-Audit-Probe echo distinguishes it from a real 200 mutation).
func assertProbeAck(t *testing.T, w *httptest.ResponseRecorder, label string) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("%s: audit probe expected 200, got %d body=%s", label, w.Code, w.Body.String())
	}
	if got := w.Header().Get(auditProbeHeader); got != "acknowledged" {
		t.Errorf("%s: expected %s response echo 'acknowledged', got %q", label, auditProbeHeader, got)
	}
	if !strings.Contains(w.Body.String(), "no state change performed") {
		t.Errorf("%s: dry-run body should note no state change; got %q", label, w.Body.String())
	}
}

// TestGardenerAuditProbe_DryRunNoMutation is the core regression lock: every
// destructive lifecycle route, hit WITH operator identity AND the audit-probe
// header, returns 200 and mutates NOTHING.
func TestGardenerAuditProbe_DryRunNoMutation(t *testing.T) {
	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"disable", "/gardener/gd-cap/disable", nil},
		{"enable", "/gardener/gd-cap/enable", nil},
		{"run-now", "/gardener/gd-cap/run-now", nil},
		{"cancel-run", "/gardener/gd-cap/cancel-run", nil},
		{"retire", "/gardener/gd-cap/retire", nil},
		{"pause-toggle", "/gardener/gd-cap/pause-toggle", nil},
		{"mode", "/gardener/gd-cap/mode", url.Values{"mode": {"event"}}},
		{"config", "/gardener/gd-cap/config", url.Values{"terminal_goal": {"HIJACKED"}}},
		{"successor", "/gardener/gd-cap/successor", url.Values{"terminal_goal": {"heir"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := seedGardenerOpsFake(t)
			// run-now would otherwise create; give it a create response so a
			// missing guard produces a real spawn we can catch.
			f.createResp = "gd-rNEW"
			mux := mkServer(t, f)

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, probeReq("POST", tc.path, tc.form))

			assertProbeAck(t, w, tc.name)
			assertNoMutation(t, f, tc.name)
			// The gardener trigger bead must survive every probe intact.
			if f.beads["gd-cap"].Status != "open" {
				t.Errorf("%s: gardener bead status changed to %q under a probe", tc.name, f.beads["gd-cap"].Status)
			}
		})
	}
}

// TestGardenerAuditProbe_HeaderDoesNotBypassAuth locks that the dry-run header is
// NOT an auth bypass: a request with the probe header but NO operator identity
// still gets the authoritative 401 from each handler's email!="" gate. A 401
// already proves the route is implemented+gated — that is all a VIEWER-scoped
// probe needs, and it means the header can never leak information a bare
// unauthenticated POST wouldn't.
func TestGardenerAuditProbe_HeaderDoesNotBypassAuth(t *testing.T) {
	paths := []string{
		"/gardener/gd-cap/disable",
		"/gardener/gd-cap/enable",
		"/gardener/gd-cap/run-now",
		"/gardener/gd-cap/cancel-run",
		"/gardener/gd-cap/retire",
		"/gardener/gd-cap/pause-toggle",
		"/gardener/gd-cap/config",
		"/gardener/gd-cap/successor",
	}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			f := seedGardenerOpsFake(t)
			mux := mkServer(t, f)

			// No X-Auth-Request-Email → EmailFromContext == "" → 401.
			req := httptest.NewRequest("POST", path, nil)
			req.Header.Set(auditProbeHeader, "1")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s: probe without operator identity expected 401, got %d", path, w.Code)
			}
			assertNoMutation(t, f, path)
		})
	}
}

// TestGardenerAuditProbe_AbsentHeaderStillMutates guards that the dry-run path is
// opt-in ONLY: without the header, an operator POST performs the real mutation
// exactly as before. This is the negative control that proves the probe test
// above is testing the guard, not a broken handler.
func TestGardenerAuditProbe_AbsentHeaderStillMutates(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/disable", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("disable (no probe header): expected 200, got %d", w.Code)
	}
	if f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("disable (no probe header): expected real mutation enabled=false, got %v", f.updateFieldsLastVal["enabled"])
	}
}
