package main

// W3 Observability Hub page tests (task gd-HHUBCXoyFm; epic ka-0000005362).
// W2 (charts_test.go) covers enumeration, grouping, the security/uid gate, and
// degraded-mode hero embeds. W3 adds, on top of that backend:
//
//   1. a per-card JSON-model panel-list fallback rendered on EVERY card, incl.
//      when GRAFANA_BASE_URL="" (auto-expanded then), so the board's shape is
//      always conveyed (design §3);
//   2. a sticky search + project-chip controls bar, with the chips derived from
//      the sections actually present (data-driven), and card data-attributes
//      the client filter keys off.
//
// These assert the rendered HTML carries those affordances and the ground-truth
// panel data (FLOOR rubric C1: the displayed panel list/count matches the
// model), not the client JS behavior (that is the Playwright CEILING gate).

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// modelWithRow is a Grafana model whose panels nest under a row — exercises the
// panel-list row-label branch and the row-excluded viz count (a row + 2 viz
// children → count 2, list shows the row label + both children).
const modelWithRow = `{"panels":[
  {"id":1,"type":"row","title":"Latency","panels":[
    {"id":2,"title":"p95 request","type":"timeseries"},
    {"id":3,"title":"p99 request","type":"timeseries"}
  ]}
]}`

// renderHubPage renders /charts for the given dashboards + grafana base and
// returns the response body, failing on a non-200.
func renderHubPage(t *testing.T, f *fakeDaemon, base string) string {
	t.Helper()
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = base
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	req := httptest.NewRequest("GET", "/charts", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%s", w.Code, w.Body.String())
	}
	return w.Body.String()
}

// Every card carries a JSON-model panel-list fallback that names each panel
// from the model — present even with Grafana configured (alongside the live
// hero embed), so the board's full shape is always one expand away.
func TestHubPage_PerCardPanelList(t *testing.T) {
	const grafanaURL = "https://gardenio.app.e2e.dev.acme.example/grafana"
	f := dashboardHubFake(
		[]*beadsapi.BeadDetail{dashBead("gd-D1", "Service Health RED", "monorepo", "monorepo-service-health")},
		map[string]*beadsapi.BeadDetail{"gd-DOC1": modelDoc("gd-DOC1", modelWithRow)},
		map[string][]beadsapi.Dependency{
			"gd-D1": {{BeadID: "gd-DOC1", DependsOnID: "gd-D1", Type: "parent-child"}},
		},
	)
	body := renderHubPage(t, f, grafanaURL)

	// The panel-list container + each panel title/type from the model.
	if !strings.Contains(body, `class="charts-hub-panel-list"`) {
		t.Error("per-card panel list not rendered")
	}
	for _, want := range []string{"p95 request", "p99 request", "Latency"} {
		if !strings.Contains(body, want) {
			t.Errorf("panel-list missing model panel %q", want)
		}
	}
	// Row header rendered via the row-label branch, not as a viz row.
	if !strings.Contains(body, "▸ Latency") {
		t.Error("row panel not rendered with row-label affordance")
	}
	// Viz count excludes the row: 2 children → "2 panels" in the header AND
	// "(2)" in the panel-list summary (FLOOR C1: count matches the model).
	if !strings.Contains(body, "2 panels") {
		t.Errorf("viz panel count wrong (rows must be excluded)\n%s", body)
	}
	// Hero embed still present (panel list is additive, not a replacement).
	if !strings.Contains(body, `class="grafana-embed-frame"`) {
		t.Error("hero embed missing — panel list must be additive to it")
	}
}

// With Grafana unconfigured the hero degrades to a link card AND the panel-list
// <details> is auto-expanded (open) so the card still conveys the board shape.
func TestHubPage_PanelListAutoOpenWhenNoGrafana(t *testing.T) {
	f := dashboardHubFake(
		[]*beadsapi.BeadDetail{dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live")},
		map[string]*beadsapi.BeadDetail{"gd-DOC1": modelDoc("gd-DOC1", modelWithRow)},
		map[string][]beadsapi.Dependency{
			"gd-D1": {{BeadID: "gd-DOC1", DependsOnID: "gd-D1", Type: "parent-child"}},
		},
	)
	body := renderHubPage(t, f, "") // no Grafana

	if strings.Contains(body, `class="grafana-embed-frame"`) {
		t.Error("iframe rendered with empty GRAFANA_BASE_URL")
	}
	// The panel list must be auto-expanded so a no-Grafana env still reads the
	// board's contents without a click.
	if !strings.Contains(body, `<details class="charts-hub-panels" open>`) {
		t.Errorf("panel list should be auto-open when Grafana is unconfigured\n%s", body)
	}
	for _, want := range []string{"p95 request", "p99 request"} {
		if !strings.Contains(body, want) {
			t.Errorf("panel-list missing %q in degraded mode", want)
		}
	}
}

// A dashboard whose model is missing/unreadable shows the panel-list's empty
// state with the degradation reason, never a crash or a blank card (R2).
func TestHubPage_PanelListEmptyStateOnModelErr(t *testing.T) {
	// No model doc wired → buildDashboardView sets ModelErr "no model attached".
	f := dashboardHubFake(
		[]*beadsapi.BeadDetail{dashBead("gd-D1", "No Model Board", "gardenio", "no-model-uid")},
		nil, nil,
	)
	body := renderHubPage(t, f, "https://g.example/grafana")
	if !strings.Contains(body, "Panel list unavailable") {
		t.Errorf("expected panel-list empty state for a model-less board\n%s", body)
	}
	if !strings.Contains(body, "no model attached") {
		t.Error("expected the degradation reason surfaced in the empty state")
	}
}

// The sticky controls bar renders: a search input, an "All" chip, and one chip
// per project section present — chips are data-driven, so a project appears
// with no code change (G3).
func TestHubPage_SearchAndProjectChips(t *testing.T) {
	const grafanaURL = "https://gardenio.app.e2e.dev.acme.example/grafana"
	f := dashboardHubFake(
		[]*beadsapi.BeadDetail{
			dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live"),
			dashBead("gd-D2", "Service Health", "monorepo", "monorepo-service-health"),
		},
		nil, nil,
	)
	body := renderHubPage(t, f, grafanaURL)

	if !strings.Contains(body, `id="charts-hub-search"`) {
		t.Error("search input missing from controls bar")
	}
	if !strings.Contains(body, `class="charts-hub-controls"`) {
		t.Error("sticky controls bar missing")
	}
	// "All" chip + a chip per present project.
	if !strings.Contains(body, `data-project=""`) {
		t.Error(`"All" chip (data-project="") missing`)
	}
	for _, proj := range []string{"gardenio", "monorepo"} {
		if !strings.Contains(body, `class="charts-hub-chip" data-project="`+proj+`"`) {
			t.Errorf("project chip for %q missing", proj)
		}
	}
	// Cards carry the data-attributes the client filter keys off.
	for _, want := range []string{
		`data-title="Fleet Live"`, `data-uid="gardenio-fleet-live"`,
		`data-title="Service Health"`, `data-uid="monorepo-service-health"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("card filter attribute missing: %s", want)
		}
	}
}

// An empty fleet renders neither the controls bar nor the no-results sentinel —
// only the "file a dashboard" empty state (no dead filter UI).
func TestHubPage_NoControlsWhenEmpty(t *testing.T) {
	f := dashboardHubFake(nil, nil, nil)
	body := renderHubPage(t, f, "https://g.example/grafana")
	if strings.Contains(body, `class="charts-hub-controls"`) {
		t.Error("controls bar should not render on an empty fleet")
	}
	if !strings.Contains(body, "No dashboards filed yet") {
		t.Error("empty-state copy missing")
	}
}

// TestHubPage_GridUsesMinmaxZero is the static guardrail for bug ka-0000004897
// (W5 GRADE audit gd-ijw8zaKfyL): the card grid MUST use minmax(0, 1fr) tracks,
// never a bare 1fr.
//
// A bare `1fr` grid track floors at min-content. A hub card's min-content width
// is its longest white-space:nowrap panel name — the monorepo boards carry
// ~400px titles ("Monorepo — Distributed Traces / Error Correlation"). On a
// narrow (mobile) viewport that floor refuses to shrink, so the card overflows
// the screen (measured: scrollWidth 633px on a 390px viewport the instant a
// panel list expands) and the `.charts-hub-panel-name` text-overflow:ellipsis
// never engages. `minmax(0, 1fr)` lets the track shrink below min-content so
// the names truncate and the grid stays in-frame at every viewport.
//
// This asserts on the rendered <style>, so a future edit that reverts the grid
// to a bare 1fr (re-introducing the mobile overflow) fails the build.
func TestHubPage_GridUsesMinmaxZero(t *testing.T) {
	f := dashboardHubFake(
		[]*beadsapi.BeadDetail{dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live")},
		nil, nil,
	)
	body := renderHubPage(t, f, "https://gardenio.app.e2e.dev.acme.example/grafana")

	// The .charts-grid rule must declare both the single-column base and the
	// two-column breakpoint with minmax(0, …) tracks. We scope to the
	// .charts-grid declaration so other pages' shared-CSS grids in the full
	// layout don't interfere.
	grid := sliceAfter(body, ".charts-grid {")
	if grid == "" {
		t.Fatal(".charts-grid rule not found in rendered page")
	}
	if !strings.Contains(grid, "grid-template-columns: minmax(0, 1fr);") {
		t.Error(".charts-grid base track is not minmax(0, 1fr) — a bare 1fr track " +
			"floors at min-content and overflows narrow viewports (bug ka-0000004897)")
	}
	if !strings.Contains(grid, "grid-template-columns: minmax(0, 1fr) minmax(0, 1fr);") {
		t.Error(".charts-grid two-column breakpoint is not minmax(0, 1fr) minmax(0, 1fr) " +
			"(bug ka-0000004897); use minmax(0,…) so cards can shrink below min-content")
	}
}

// sliceAfter returns the substring of s starting at marker (inclusive), or ""
// if marker is absent. Lets a test scope an assertion to one CSS rule's body.
func sliceAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	return s[i:]
}
