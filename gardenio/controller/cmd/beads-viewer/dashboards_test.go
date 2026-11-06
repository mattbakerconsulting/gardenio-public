package main

// Tests for the /dashboards page (task ka-0000004892) and the engagement /
// security / uniqueness proxy routes it depends on. Covers: the page renders
// its shell + panel scaffolding + the dashboards driver script, and the new
// multi-segment proxy routes forward to the correct bounded daemon leaf (never
// an arbitrary path), reject an unknown panel with a 404, and degrade to 503
// when no daemon is wired.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDashboardsPage_RendersShell(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/dashboards", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	body := w.Body.String()
	// Panel scaffolding + both dashboard sections + the driver + the vendored
	// ECharts must all be present so the page is functional client-side.
	for _, want := range []string{
		`data-dash-panel="engagement"`,
		`data-dash-panel="security"`,
		`data-dash-chart="activity-surface"`,
		`data-dash-chart="activity-operators"`,
		`data-dash-chart="spend-operators"`,
		`data-dash-table="sessions"`,
		`data-dash-chart="authz"`,
		`data-dash-chart="decisions-lifecycle"`,
		`data-dash-chart="escalations"`,
		`data-dash-table="token-slo"`,
		`data-uniq-key="dau"`,
		`data-uniq-key="wau"`,
		`data-uniq-key="mau"`,
		"assets/static/echarts.min.js",
		"assets/static/dashboards.js",
		// The privacy/redaction contract is surfaced to the reader.
		"right to be forgotten",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboards page missing %q", want)
		}
	}
}

func TestDashboardsPage_NavLinkPresent(t *testing.T) {
	// Every full page carries the shared nav; the Dashboards link must be there
	// so the page is reachable.
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/dashboards", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `/dashboards"`) {
		t.Errorf("nav link to /dashboards not found")
	}
}

func TestDashboardsDriver_ServedStatic(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/dashboards.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("dashboards.js: got %d want 200", w.Code)
	}
	if w.Body.Len() < 2000 {
		t.Errorf("dashboards.js suspiciously small: %d bytes", w.Body.Len())
	}
}

// newDashProxyMux registers the engagement/security/uniqueness proxy routes
// exactly as server.go does so route-level constraints are exercised.
func newDashProxyMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/charts/uniqueness", srv.handleChartsUniqueness)
	mux.HandleFunc("GET /api/v1/charts/engagement/{panel}", srv.handleChartsEngagement)
	mux.HandleFunc("GET /api/v1/charts/security/{panel}", srv.handleChartsSecurity)
	mux.HandleFunc("GET /api/v1/charts/{chart_id}", srv.handleChartsRender)
	return mux
}

func TestDashProxy_ForwardsToBoundedLeaf(t *testing.T) {
	cases := []struct {
		reqPath      string
		wantUpstream string
	}{
		{"/api/v1/charts/uniqueness", "/v1/charts/uniqueness"},
		{"/api/v1/charts/engagement/activity", "/v1/charts/engagement/activity"},
		{"/api/v1/charts/engagement/spend", "/v1/charts/engagement/spend"},
		{"/api/v1/charts/engagement/sessions", "/v1/charts/engagement/sessions"},
		{"/api/v1/charts/security/authz", "/v1/charts/security/authz"},
		{"/api/v1/charts/security/decisions", "/v1/charts/security/decisions"},
		{"/api/v1/charts/security/token-slo", "/v1/charts/security/token-slo"},
	}
	for _, tc := range cases {
		fake := &fakeChartsDaemon{body: `{"ok":true}`, ct: "application/json"}
		upstream := httptest.NewServer(fake.handler())
		srv := newChartsProxyServerForTest(upstream.URL, "tok")
		mux := newDashProxyMux(srv)

		req := httptest.NewRequest(http.MethodGet, tc.reqPath+"?window=7d", nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("%s: status got %d want 200; body=%q", tc.reqPath, rr.Code, rr.Body.String())
		}
		if fake.gotPath != tc.wantUpstream {
			t.Errorf("%s: upstream path got %q want %q", tc.reqPath, fake.gotPath, tc.wantUpstream)
		}
		if !strings.Contains(fake.gotQuery, "window=7d") {
			t.Errorf("%s: window query not forwarded: %q", tc.reqPath, fake.gotQuery)
		}
		if fake.gotAuth != "Bearer tok" {
			t.Errorf("%s: daemon bearer not attached: %q", tc.reqPath, fake.gotAuth)
		}
		upstream.Close()
	}
}

func TestDashProxy_UnknownPanel_404(t *testing.T) {
	// A panel outside the allow-list must never be forwarded upstream.
	fake := &fakeChartsDaemon{body: `{}`, ct: "application/json"}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()
	srv := newChartsProxyServerForTest(upstream.URL, "tok")
	mux := newDashProxyMux(srv)

	for _, p := range []string{"/api/v1/charts/engagement/wat", "/api/v1/charts/security/nope"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status got %d want 404", p, rr.Code)
		}
		if fake.hits != 0 {
			t.Errorf("%s: unknown panel was forwarded upstream (hits=%d)", p, fake.hits)
		}
	}
}

func TestDashProxy_DegradedMode_503(t *testing.T) {
	srv := newChartsProxyServerForTest("", "") // no daemon wired
	mux := newDashProxyMux(srv)
	for _, p := range []string{
		"/api/v1/charts/uniqueness",
		"/api/v1/charts/engagement/activity",
		"/api/v1/charts/security/authz",
	} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status got %d want 503", p, rr.Code)
		}
	}
}
