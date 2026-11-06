package main

// Tests for the /charts Observability Hub (epic ka-0000005362, W2 task
// ka-0000000773): the page enumerates every type=dashboard bead at request time
// and renders one card per dashboard, grouped by project, each with a live hero
// solo-panel Grafana embed reusing the AVA session under /grafana. The former
// hardcoded single-uid / fixed-5-panel slice is gone — see dashboard_hub.go.

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// dashboardHubFake seeds a fakeDaemon so ListBeadsFiltered(type=dashboard)
// returns the given dashboard beads, and each dashboard's model-doc child is
// reachable via revDeps + beads for the panel-count extraction.
func dashboardHubFake(dashboards []*beadsapi.BeadDetail, modelDocs map[string]*beadsapi.BeadDetail, revDeps map[string][]beadsapi.Dependency) *fakeDaemon {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: dashboards, Total: len(dashboards)}
	// Merge model-doc beads into the lookup map used by GetBead.
	for id, b := range modelDocs {
		f.beads[id] = b
	}
	if f.revDeps == nil {
		f.revDeps = map[string][]beadsapi.Dependency{}
	}
	for id, deps := range revDeps {
		f.revDeps[id] = deps
	}
	return f
}

func dashBead(id, title, project, uid string) *beadsapi.BeadDetail {
	var labels []string
	if project != "" {
		labels = []string{"project:" + project}
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "dashboard",
		Title:  title,
		Labels: labels,
		Fields: map[string]string{"dashboard_uid": uid, "title": title},
	}
}

func modelDoc(id, content string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "doc",
		Labels: []string{dashboardModelDocLabel},
		Fields: map[string]string{"content": content},
	}
}

func TestHandleCharts_EnumeratesDashboards(t *testing.T) {
	const grafanaURL = "https://gardenio.app.e2e.dev.acme.example/grafana"
	model := `{"panels":[{"id":1,"title":"A","type":"timeseries"},{"id":2,"title":"B","type":"stat"}]}`
	dashboards := []*beadsapi.BeadDetail{
		dashBead("gd-D1", "Fleet Live Activity", "gardenio", "gardenio-fleet-live"),
		dashBead("gd-D2", "Service Health RED", "monorepo", "monorepo-service-health"),
	}
	f := dashboardHubFake(dashboards,
		map[string]*beadsapi.BeadDetail{
			"gd-DOC1": modelDoc("gd-DOC1", model),
			"gd-DOC2": modelDoc("gd-DOC2", model),
		},
		map[string][]beadsapi.Dependency{
			"gd-D1": {{BeadID: "gd-DOC1", DependsOnID: "gd-D1", Type: "parent-child"}},
			"gd-D2": {{BeadID: "gd-DOC2", DependsOnID: "gd-D2", Type: "parent-child"}},
		},
	)
	// The "Charts" nav link is vibe-only after gb-ux #3 (ka-0000003340); render
	// as a vibe-mode viewer so the nav-link assertion below holds. No page
	// content is keyed on VibeMode, so the dashboard assertions are unaffected.
	f.userResp = vibeUserResp()
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = grafanaURL
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/charts", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// One hero iframe per enumerated dashboard.
	if got := strings.Count(body, `class="grafana-embed-frame"`); got != len(dashboards) {
		t.Errorf("hero embed count: got %d want %d", got, len(dashboards))
	}
	// Each dashboard's hero is a solo-panel render of panel 1 of its OWN uid —
	// proving the uid is enumerated per-bead, not a single hardcoded value.
	for _, d := range dashboards {
		uid := d.Fields["dashboard_uid"]
		soloSrc := grafanaURL + "/d-solo/" + uid + "?panelId=" + dashboardHubHeroPanelID + "&amp;kiosk"
		if !strings.Contains(body, soloSrc) {
			t.Errorf("dashboard %s: missing hero embed src %q", d.ID, soloSrc)
		}
		if !strings.Contains(body, d.Title) {
			t.Errorf("missing dashboard title %q", d.Title)
		}
	}
	// Grouped by project into sections (R1 relabel): both project names appear
	// as section headers.
	for _, want := range []string{"gardenio", "monorepo"} {
		if !strings.Contains(body, `class="charts-hub-section-title">`+want) {
			t.Errorf("missing section header for project %q", want)
		}
	}
	// Panel count surfaced from the model doc (2 viz panels each).
	if !strings.Contains(body, "2 panels") {
		t.Error("expected per-card panel count from model doc")
	}
	// The old hardcoded single-uid wiring must be gone.
	if strings.Contains(body, "dashboard <code>gardenio-charts-candidate</code>") {
		t.Error("old hardcoded single-dashboard copy still present")
	}
	// Nav link present so the tab is reachable.
	if !strings.Contains(body, `href="/charts"`) {
		t.Error("/charts nav link missing")
	}
}

// A dashboard bead with a missing/invalid uid is the security gate AND the
// enumeration predicate: it must be skipped entirely so grafanaembed.HTML is
// never handed an unvalidated uid (design Flag B / VictoriaTraces, W4 backfill).
func TestHandleCharts_SkipsInvalidUID(t *testing.T) {
	const grafanaURL = "https://gardenio.app.e2e.dev.acme.example/grafana"
	dashboards := []*beadsapi.BeadDetail{
		dashBead("gd-OK", "Valid Board", "gardenio", "valid-uid"),
		dashBead("gd-BAD", "Injection Attempt", "gardenio", "bad/uid?evil=1"), // slash + query → fails dashboardUIDRe
		dashBead("gd-EMPTY", "No UID Board", "gardenio", ""),                  // no uid at all
	}
	f := dashboardHubFake(dashboards, nil, nil)
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = grafanaURL
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/charts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Only the valid board renders an embed.
	if got := strings.Count(body, `class="grafana-embed-frame"`); got != 1 {
		t.Errorf("hero embed count: got %d want 1 (only the valid-uid board)", got)
	}
	if !strings.Contains(body, "Valid Board") {
		t.Error("valid board missing")
	}
	// The malformed uid must never reach the DOM in any iframe src form.
	if strings.Contains(body, "bad/uid") {
		t.Error("invalid uid leaked into page — security gate breached")
	}
	if strings.Contains(body, "d-solo/bad") {
		t.Error("invalid-uid dashboard produced an embed")
	}
}

// When Grafana is unconfigured (GRAFANA_BASE_URL unset), dashboards still
// enumerate but the hero embeds degrade to the quiet link-card fallback rather
// than iframes pointed nowhere, and the header "Open in Grafana ↗" is hidden.
func TestHandleCharts_DegradesWithoutGrafana(t *testing.T) {
	dashboards := []*beadsapi.BeadDetail{
		dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live"),
	}
	f := dashboardHubFake(dashboards, nil, nil)
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = ""
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/charts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if strings.Contains(body, `class="grafana-embed-frame"`) {
		t.Error("grafana iframe rendered with empty GRAFANA_BASE_URL — would point nowhere")
	}
	// The dashboard still enumerates (title + degraded card present).
	if !strings.Contains(body, "Fleet Live") {
		t.Error("dashboard title missing in degraded mode")
	}
	if got := strings.Count(body, "Grafana not configured in this environment"); got != len(dashboards) {
		t.Errorf("unconfigured fallback count: got %d want %d", got, len(dashboards))
	}
}

// An empty fleet (no type=dashboard beads) renders the empty-state, not a crash.
func TestHandleCharts_EmptyFleet(t *testing.T) {
	f := dashboardHubFake(nil, nil, nil)
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = "https://g.example/grafana"
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/charts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No dashboards filed yet") {
		t.Error("expected empty-state copy when no dashboards exist")
	}
	if strings.Contains(body, `class="grafana-embed-frame"`) {
		t.Error("no embeds expected on an empty fleet")
	}
}

// The "Open in Grafana ↗" header link renders only when grafanaBaseURL is set,
// and points at that URL with target=_blank (task ka-0000006706).
func TestHandleCharts_GrafanaLink(t *testing.T) {
	const grafanaURL = "https://gardenio.app.e2e.dev.acme.example/grafana"

	render := func(t *testing.T, base string) string {
		t.Helper()
		f := dashboardHubFake([]*beadsapi.BeadDetail{
			dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live"),
		}, nil, nil)
		srv := NewServer(f, slog.Default(), "", "", "", nil)
		srv.grafanaBaseURL = base
		mux := edgeAuthMux(srv)
		req := httptest.NewRequest("GET", "/charts", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	t.Run("present when configured", func(t *testing.T) {
		body := render(t, grafanaURL)
		if !strings.Contains(body, `class="charts-grafana-link"`) {
			t.Error("grafana link not rendered when GRAFANA_BASE_URL set")
		}
		if !strings.Contains(body, `href="`+grafanaURL+`"`) {
			t.Errorf("grafana link missing href %q", grafanaURL)
		}
		if !strings.Contains(body, `target="_blank"`) {
			t.Error("grafana link should open in a new tab")
		}
	})

	t.Run("hidden when unset", func(t *testing.T) {
		body := render(t, "")
		if strings.Contains(body, `class="charts-grafana-link"`) {
			t.Error("grafana link rendered with empty GRAFANA_BASE_URL — would be a dead link")
		}
	})
}
