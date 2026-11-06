package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestHandleCosts_Empty(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "No accounts") {
		t.Errorf("expected empty-state copy, body=%s", body)
	}
	if len(f.listAllQ) == 0 {
		t.Fatal("expected ListBeadsFiltered to be called")
	}
	q := f.listAllQ[0]
	if got := q.Types; len(got) != 1 || got[0] != "account" {
		t.Errorf("types = %v, want [account]", got)
	}
	if !slices.Contains(q.Statuses, "open") || !slices.Contains(q.Statuses, "in_progress") {
		t.Errorf("statuses = %v, want open + in_progress", q.Statuses)
	}
}

func TestHandleCosts_Empty_RendersScaffolds(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Summary card renders unconditionally now.
		`class="costs-summary"`,
		"Today",
		"$0.00",
		// Empty-state explainer + action links.
		`class="costs-empty"`,
		"controller has not yet observed",
		"/health",
		"type=account",
		"/agents",
		// Planned-shape scaffold rows.
		"costs-scaffold-row",
		"Planned shape",
		"anthropic-prod-01",
		"5h: &mdash;%",
		"7d: &mdash;%",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty /costs page missing %q", want)
		}
	}
}

func TestHandleCosts_NonEmpty_RendersSummaryAndCards(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-acct-1",
				Title:     "anthropic-prod-01",
				Type:      "account",
				Status:    "open",
				UpdatedAt: now.Add(-2 * time.Minute),
				Fields: map[string]string{
					"provider":         "anthropic",
					"model":            "claude-opus-4-7",
					"status":           "active",
					"throttle_status":  "ok",
					"utilization_5h":   "0.42",
					"utilization_7d":   "0.18",
					"daily_cost_usd":   "12.34",
					"weekly_cost_usd":  "78.90",
					"total_cost_usd":   "234.56",
					"session_count":    "7",
					"probe_checked_at": now.Add(-3 * time.Minute).Format(time.RFC3339),
				},
			},
			{
				ID:        "gd-acct-2",
				Title:     "anthropic-prod-02",
				Type:      "account",
				Status:    "in_progress",
				UpdatedAt: now,
				Fields: map[string]string{
					"provider":                "anthropic",
					"model":                   "claude-sonnet-4-6",
					"status":                  "active",
					"throttle_status":         "throttled",
					"utilization_5h":          "0.95",
					"utilization_7d":          "0.71",
					"throttle_reset_at":       now.Add(2*time.Hour + 15*time.Minute).Format(time.RFC3339),
					"throttle_reset_5h_at":    now.Add(4*time.Hour + 5*time.Minute).Format(time.RFC3339),
					"throttle_reset_7d_at":    now.Add(6 * 24 * time.Hour).Format(time.RFC3339),
					"daily_cost_usd":          "5.00",
					"weekly_cost_usd":         "30.00",
					"total_cost_usd":          "120.00",
					"overage_status":          "disabled",
					"overage_disabled_reason": "spend cap exceeded",
				},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	for _, want := range []string{
		// Summary card.
		"Today",
		"$17.34", // grand total = 12.34 + 5.00
		"week",
		"$108.90",    // weekly = 78.90 + 30.00
		"<dt>active", // dl-shaped summary stats
		"<dd>2</dd>", // ActiveCount=2 and TotalCount=2
		"throttled",
		"<dt>sessions",
		"<dt>max 5h util",
		"<dt>max 7d util",
		// Month projection band (7d-avg basis) — present because WeeklyTotalUSD > 0.
		"month-so-far (7d-avg proj)",
		"projected month (7d-avg)",
		// By-provider rollup (both accounts share provider "anthropic")
		"By provider",
		// Model names still render on the account cards. The "By model" rollup
		// section is now sourced from the real-bill tag gauge (ka-0000006501) and
		// is hidden here because mkServer wires no metrics upstream — asserted
		// separately in TestHandleCosts_ByModelSection_*.
		"claude-opus-4-7",
		"claude-sonnet-4-6",
		// Top spenders panel.
		"Top spenders today",
		// Hottest-utilization panel.
		"Hottest utilization",
		// Throttled-now panel.
		"Throttled now",
		// Account cards.
		"anthropic-prod-01",
		"anthropic-prod-02",
		"anthropic",
		"claude-opus-4-7",
		"claude-sonnet-4-6",
		"$12.34",
		"$5.00",
		"$234.56",
		"5h: 42%",
		"7d: 71%",
		"resets in 2h",
		// Per-window countdowns: 5h ≈ 4h 4m, 7d ≈ 143h 59m. Match a stable
		// prefix to avoid races with elapsed-time math.
		"resets in 4h",
		"resets in 143h",
		// Compact bars rendered in Hottest util + Throttled now rollups.
		"cost-bar-compact",
		"overage: disabled",
		"spend cap exceeded",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("costs page missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "No accounts") {
		t.Errorf("unexpected empty-state when accounts present")
	}
	if strings.Contains(body, "Planned shape") {
		t.Errorf("unexpected planned-shape scaffold when accounts present")
	}
}

// TestHandleCosts_ExcludesE2EAccounts is the bead-side layer of the ka-0000005167
// fix at the handler level: E2E harness accounts must not appear as cards, in the
// summary totals, or in the Top spenders / Hottest / Throttled rollups — while a
// real account whose name legitimately begins with "e2e" (no hyphen) still does.
// Seeded across every harness shape, including the two that stamp NO e2e: label.
func TestHandleCosts_ExcludesE2EAccounts(t *testing.T) {
	now := time.Now()
	acct := func(id, title string, labels []string, createdBy, cost string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID: id, Title: title, Type: "account", Status: "open",
			Labels: labels, CreatedBy: createdBy, UpdatedAt: now,
			Fields: map[string]string{
				"provider": "anthropic", "status": "active", "throttle_status": "throttled",
				"utilization_5h": "0.99", "daily_cost_usd": cost, "total_cost_usd": cost,
			},
		}
	}
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// Real accounts — must render.
			acct("gd-real-1", "gardenio3", []string{"project:gardenio"}, "Matthew.Baker", "10.00"),
			// Real account that legitimately starts with "e2e" but no hyphen.
			acct("gd-real-2", "e2eflow-prod", []string{"project:gardenio"}, "Matthew.Baker", "7.00"),
			// E2E harness accounts — every naming convention, must all be dropped.
			acct("gd-e2e-1", "e2e-acct-primary-abc", []string{"project:e2e-test-abc"}, "e2e-test", "99.00"),
			acct("gd-e2e-2", "e2e-account-TAG", []string{"project:gardenio"}, "e2e-test", "99.00"),
			acct("gd-e2e-3", "probe-acct-999", []string{"e2e:claudeless"}, "", "99.00"),
			acct("gd-e2e-4", "e2e-cred-a-rot", []string{"e2e:cred-rot-1", "project:gardenio"}, "", "99.00"),
			acct("gd-e2e-5", "sneaky-acct", []string{"e2e:true"}, "", "99.00"),
			acct("gd-e2e-6", "shard-acct", nil, "e2e-rwx", "99.00"),
		},
		Total: 8,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Real accounts render.
	for _, want := range []string{"gardenio3", "e2eflow-prod"} {
		if !strings.Contains(body, want) {
			t.Errorf("real account %q missing from /costs; body=%s", want, body)
		}
	}
	// No E2E account leaks onto ANY surface (cards, Top spenders, Hottest, Throttled).
	for _, banned := range []string{
		"e2e-acct-primary-abc", "e2e-account-TAG", "probe-acct-999",
		"e2e-cred-a-rot", "sneaky-acct", "shard-acct",
	} {
		if strings.Contains(body, banned) {
			t.Errorf("E2E account %q leaked onto /costs", banned)
		}
	}
	// Totals count only the two real accounts ($10 + $7), not 6x$99 of test spend.
	if !strings.Contains(body, "$17.00") {
		t.Errorf("today total should be $17.00 (real accounts only); body=%s", body)
	}
	// Filtering must not trip the daemon-side truncation banner: Total(8) == len(Beads)(8).
	if strings.Contains(body, "Showing first") {
		t.Errorf("truncation banner must not appear when nothing was daemon-truncated; body=%s", body)
	}
}

// TestHandleCosts_ByModelSection_RealBill is the end-to-end acceptance for
// Surface A (ka-0000006501): with a live metrics upstream, the "By model"
// section renders per-effective-model spend from the real-bill tag gauge — no
// single "unknown" bucket, and the effective model (a haiku row on an
// opus-default fleet) is present. The account beads carry NO `model` field on
// purpose (the prod field-absent case), proving the section no longer depends
// on it.
func TestHandleCosts_ByModelSection_RealBill(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-acct-1", Title: "litellm-prod", Type: "account", Status: "open",
			UpdatedAt: now,
			Fields: map[string]string{ // note: NO "model" field (prod case)
				"provider": "anthropic", "status": "active",
				"daily_cost_usd": "12.34", "total_cost_usd": "234.56",
				"litellm_key": "sk-a",
			},
		}},
		Total: 1,
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		// Only the per-model gauge queries need bodies; the headline-rate queries
		// tolerate an empty vector.
		if strings.Contains(q, `dim="model"`) {
			_, _ = w.Write([]byte(modelGaugeBody(map[string]string{
				"claude-opus-4-8": "30.00", "claude-haiku-4-5": "10.00",
			})))
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	t.Cleanup(upstream.Close)

	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"By model",
		"claude-opus-4-8",  // effective model rows from the real-bill gauge
		"claude-haiku-4-5", // the haiku split an opus-default account would hide
		"$30.00",
		"Real LiteLLM bill by effective model", // provenance note
	} {
		if !strings.Contains(body, want) {
			t.Errorf("By model section missing %q\n---\n%s", want, body)
		}
	}
	// The degraded account-field behavior must be gone: no synthesized "unknown"
	// model bucket even though the account bead has no `model` field.
	if strings.Contains(body, ">unknown<") {
		t.Error("By model section rendered an 'unknown' bucket — the fix must not")
	}
}

func TestHandleCosts_DaemonError_RendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load accounts") {
		t.Errorf("expected error banner, body=%s", w.Body.String())
	}
}

func TestHandleCosts_NavLink(t *testing.T) {
	// Costs is a vibe-only nav link after gb-ux #3 (ka-0000003340); render as a
	// vibe-mode viewer so the gated link is present.
	f := newFake()
	f.userResp = vibeUserResp()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), `href="/costs"`) {
		t.Errorf("expected /costs nav link in layout")
	}
}

// The /costs header links to the same charts in the garden's Costs panel (plan gd-gh5J0vVbJP step 2.3, gd-6vGD02rgka).
func TestHandleCosts_LinksToGardenCostsPanel(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/costs", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `<a class="meta" href="/garden?costs=fleet">See it in the garden →</a>`) {
		t.Errorf("expected the /costs header to link to /garden?costs=fleet")
	}
}

// After the inline-charts→Grafana swap (epic ka-0000006518, task ka-0000002791,
// decision ka-0000007043 accepted), the two former inline ECharts cards on
// /costs (fleet-spend-rate + agent-spend-over-time) are embedded Grafana solo
// panels (uid=gardenio-costs-candidate, panel=1..2) reusing the viewer session
// under /grafana. The cards render unconditionally (even with no accounts), and
// the inline ECharts wiring is gone.
func TestHandleCosts_RendersChartCard(t *testing.T) {
	const grafanaURL = "https://gardenio.app.e2e.dev.acme.example/grafana"
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = grafanaURL
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	body := w.Body.String()

	panels := costsGrafanaPanels()
	// One Grafana iframe per panel, each a solo-panel render at the scroll-free
	// embed height.
	if got := strings.Count(body, `class="grafana-embed-frame"`); got != len(panels) {
		t.Errorf("grafana embed count: got %d want %d", got, len(panels))
	}
	for _, p := range panels {
		// The solo-panel src carries panelId=N and the panel's source dashboard
		// uid (costsDashboardUID unless the panel overrides it; after ka-0000002699
		// removed the cross-dashboard settled-rollup embed, all remaining panels come
		// from costsDashboardUID). It is an HTML attribute value, so the '&' before
		// kiosk is entity-escaped.
		uid := p.DashboardUID
		if uid == "" {
			uid = costsDashboardUID
		}
		soloSrc := grafanaURL + "/d-solo/" + uid + "?panelId=" + p.PanelID + "&amp;kiosk"
		if !strings.Contains(body, soloSrc) {
			t.Errorf("panel %s: missing solo embed src %q", p.PanelID, soloSrc)
		}
		// Any template vars the panel declares must reach the embed src as
		// &var-<name>=<value> params so the scoped view actually applies.
		for _, v := range p.Vars {
			wantVar := "&amp;var-" + v.Name + "=" + v.Val
			if !strings.Contains(body, wantVar) {
				t.Errorf("panel %s: missing template var %q in embed src", p.PanelID, wantVar)
			}
		}
		// Each panel's heading must reach the DOM so an operator can read it
		// before the iframe paints.
		if !strings.Contains(body, p.Title) {
			t.Errorf("missing panel title %q", p.Title)
		}
	}
	// Both panels render at the Playwright-verified scroll-free height.
	if got := strings.Count(body, `height="`+costsEmbedHeight+`"`); got != len(panels) {
		t.Errorf("embed height %q count: got %d want %d", costsEmbedHeight, got, len(panels))
	}
	// The cost-chart card wrapper survives so the page layout is unchanged.
	if !strings.Contains(body, `class="costs-chart-card"`) {
		t.Error("cost page missing costs-chart-card wrapper")
	}
	// The inline ECharts wiring must be gone — no catalog-panel data attrs, no
	// ECharts scripts (all replaced by Grafana embeds).
	for _, gone := range []string{
		`data-chart-catalog="1"`,
		`data-chart-id="fleet-spend-rate"`,
		`data-chart-id="agent-spend-over-time"`,
		"/assets/static/echarts.min.js",
		"/assets/static/echarts_catalog.js",
		"/assets/static/echarts_bead.js",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("inline-ECharts artifact %q still present after Grafana swap", gone)
		}
	}
}

// When Grafana is unconfigured (GRAFANA_BASE_URL unset / grafanaBaseURL ""), the
// /costs chart cards degrade to the quiet link-card fallback rather than iframes
// pointed nowhere — same contract as /charts.
func TestHandleCosts_ChartsDegradeWithoutGrafana(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil, Total: 0}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = ""
	mux := edgeAuthMux(srv)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Code)
	}
	body := w.Body.String()

	if strings.Contains(body, `class="grafana-embed-frame"`) {
		t.Error("grafana iframe rendered with empty grafanaBaseURL — would point nowhere")
	}
	// One degraded "Grafana not configured" note per chart panel.
	if got := strings.Count(body, "Grafana not configured in this environment"); got != len(costsGrafanaPanels()) {
		t.Errorf("unconfigured fallback count: got %d want %d", got, len(costsGrafanaPanels()))
	}
}

// The Grafana panel set pins the /costs dashboard contract — the exact panel
// ids in render order. All panels come from the accepted candidate dashboard
// (gardenio-costs-candidate, ids matching its dashboard spec's CatalogIDs order
// in kbeads charts_grafana_dashboard.go): fleet-spend-rate, per-agent cost, and
// the spend-by-account breakdown (ka-0000004020). Two panels were removed and must
// not resurface: the spend-by-model iframe (ka-0000006501 — host dashboard never
// provisioned as a 4-panel dashboard, and its source resolved `model` from the
// account default; replaced by the on-page tag-gauge "By model" section), and the
// cross-dashboard "Settled cost — rollup by role" embed (ka-0000002699 — it read
// the dead at-close counter ~1000x low; superseded by the native real-bill Cost
// explorer, costs_rollup.go). Guarding exactly 3 panels here keeps both traps from
// silently returning. The PromQL itself lives in the Grafana dashboard models
// (synced from the type=dashboard beads), not here.
func TestCostsGrafanaPanels_Contract(t *testing.T) {
	panels := costsGrafanaPanels()
	type want struct {
		panelID string
		uid     string // "" => costsDashboardUID
		vars    int
	}
	// cost-per-agent-hour leads at panel 1 (ka-0000006587), shifting the other
	// three ids up by one — panel ids are positional in the kbeads dashboard spec
	// (dashboardCatalog CatalogIDs), so this list must track that order exactly.
	wantPanels := []want{
		{panelID: "1"},
		{panelID: "2"},
		{panelID: "3"},
		{panelID: "4"},
	}
	if len(panels) != len(wantPanels) {
		t.Fatalf("want %d grafana panels (cost/agent-hour, fleet rate, per-agent, per-account), got %d", len(wantPanels), len(panels))
	}
	// Panel 1 must be the unit-economic card: the operator asked for $/hour/agent
	// at the TOP of /costs, and it is the id the dashboard assigns it.
	if panels[0].Title != "Cost per agent-hour" {
		t.Errorf("panel[0] title = %q, want Cost per agent-hour to lead", panels[0].Title)
	}
	// Both removed panels must stay gone: the spend-by-model iframe (ka-0000006501)
	// and the dead-counter "Settled cost — rollup by role" embed pointing at the
	// retired rollup dashboard (ka-0000002699).
	for _, p := range panels {
		if p.Title == "Spend by model over time" {
			t.Error("spend-by-model Grafana panel should be removed (ka-0000006501); use the tag-gauge By model section instead")
		}
		if p.DashboardUID == "gb-cost-per-bead-rollup-preview" {
			t.Errorf("panel %q still embeds the retired settled-rollup dashboard (ka-0000002699)", p.PanelID)
		}
	}
	// Panel ids must be unique within each source dashboard (a solo embed is
	// keyed by uid+panelId; the rollup lives on a different uid so its id may
	// freely overlap the candidate's).
	seen := map[string]bool{}
	for i, p := range panels {
		w := wantPanels[i]
		if p.PanelID == "" {
			t.Error("grafana panel with empty panel id")
		}
		if p.PanelID != w.panelID {
			t.Errorf("panel %d: id %q, want %q (render order must match the dashboard)", i, p.PanelID, w.panelID)
		}
		if p.DashboardUID != w.uid {
			t.Errorf("panel %d (%q): DashboardUID %q, want %q", i, p.PanelID, p.DashboardUID, w.uid)
		}
		if len(p.Vars) != w.vars {
			t.Errorf("panel %d (%q): got %d template vars, want %d", i, p.PanelID, len(p.Vars), w.vars)
		}
		uid := p.DashboardUID
		if uid == "" {
			uid = costsDashboardUID
		}
		key := uid + "#" + p.PanelID
		if seen[key] {
			t.Errorf("duplicate grafana panel %q on dashboard %q", p.PanelID, uid)
		}
		seen[key] = true
		if p.Title == "" || p.Scope == "" {
			t.Errorf("panel %q: title/scope must be set for pre-render display", p.PanelID)
		}
	}
}

func TestBuildCostsSummary_HourlyRate(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	accts := []costAccountView{
		{DailyCostUSD: 24.00, WeeklyCostUSD: 100, IsActive: true},
		{DailyCostUSD: 12.00, WeeklyCostUSD: 50, IsActive: false, IsThrottled: true},
	}
	s := buildCostsSummary(accts, now)
	if s.GrandTotalUSD != 36.0 {
		t.Errorf("GrandTotal = %v, want 36", s.GrandTotalUSD)
	}
	if s.WeeklyTotalUSD != 150.0 {
		t.Errorf("WeeklyTotal = %v, want 150", s.WeeklyTotalUSD)
	}
	if s.ActiveCount != 1 {
		t.Errorf("ActiveCount = %d, want 1", s.ActiveCount)
	}
	if s.ThrottledCount != 1 {
		t.Errorf("ThrottledCount = %d, want 1", s.ThrottledCount)
	}
	// HourlyRateUSD is no longer derived from GrandTotal/hours here — it comes
	// from the rolling-window vmsingle query applied by the handler
	// (ka-0000002806). buildCostsSummary must leave it at the zero value /
	// unavailable so a misconfigured upstream renders "—" not a fake average.
	if s.HourlyRateUSD != 0 {
		t.Errorf("HourlyRateUSD = %v, want 0 (set by applyFleetHourlyRate, not buildCostsSummary)", s.HourlyRateUSD)
	}
	if s.HourlyRateAvailable {
		t.Error("HourlyRateAvailable = true, want false (no vmsingle in buildCostsSummary)")
	}
	if s.ProjectedTodayUSD != 0 {
		t.Errorf("ProjectedTodayUSD = %v, want 0 (derived from rolling rate, not here)", s.ProjectedTodayUSD)
	}
}

func TestBuildCostsSummary_DerivedFields(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	accts := []costAccountView{
		{DailyCostUSD: 24.00, WeeklyCostUSD: 100, TotalCostUSD: 500, SessionCount: 20, IsActive: true, HasSpend: true},
		{DailyCostUSD: 12.00, WeeklyCostUSD: 50, TotalCostUSD: 200, SessionCount: 10, IsActive: false, IsThrottled: true, HasOverage: true, HasSpend: true},
		{DailyCostUSD: 0, WeeklyCostUSD: 0},
	}
	s := buildCostsSummary(accts, now)
	if s.TotalCostAllUSD != 700 {
		t.Errorf("TotalCostAllUSD = %v, want 700", s.TotalCostAllUSD)
	}
	if s.TotalSessions != 30 {
		t.Errorf("TotalSessions = %d, want 30", s.TotalSessions)
	}
	if s.OverageCount != 1 {
		t.Errorf("OverageCount = %d, want 1", s.OverageCount)
	}
	if s.WithSpendCount != 2 {
		t.Errorf("WithSpendCount = %d, want 2", s.WithSpendCount)
	}
	if got, want := s.AvgPerActiveUSD, 36.0/1.0; got != want {
		t.Errorf("AvgPerActiveUSD = %v, want %v", got, want)
	}
	// F6: lifetime cost / lifetime sessions (700 / 30), not daily / lifetime.
	if got, want := s.CostPerSessionUSD, 700.0/30.0; got < want-0.001 || got > want+0.001 {
		t.Errorf("CostPerSessionUSD = %v, want ~%v", got, want)
	}
	if s.DayProgressPct != 50 {
		t.Errorf("DayProgressPct = %d, want 50", s.DayProgressPct)
	}
	// ProjectedTodayUSD is derived from the rolling-window rate by the handler
	// (applyFleetHourlyRate), not from buildCostsSummary; here it stays zero.
	if s.ProjectedTodayUSD != 0 {
		t.Errorf("ProjectedTodayUSD = %v, want 0 (set by applyFleetHourlyRate)", s.ProjectedTodayUSD)
	}
}

func TestBuildCostsProviderRollups(t *testing.T) {
	accts := []costAccountView{
		{Provider: "anthropic", DailyCostUSD: 10, WeeklyCostUSD: 50, TotalCostUSD: 100, Util7d: 0.4, Util7dHas: true},
		{Provider: "anthropic", DailyCostUSD: 5, WeeklyCostUSD: 25, TotalCostUSD: 60, IsThrottled: true},
		{Provider: "bedrock", DailyCostUSD: 20, WeeklyCostUSD: 80, TotalCostUSD: 200, Util7d: 0.8, Util7dHas: true},
		{Provider: "", DailyCostUSD: 1, WeeklyCostUSD: 7, TotalCostUSD: 15},
	}
	rollups := buildCostsProviderRollups(accts, 36.0)
	if len(rollups) != 3 {
		t.Fatalf("len(rollups) = %d, want 3", len(rollups))
	}
	// Sorted by daily desc: bedrock(20) > anthropic(15) > unknown(1)
	if rollups[0].Provider != "bedrock" {
		t.Errorf("rollups[0].Provider = %q, want bedrock", rollups[0].Provider)
	}
	if rollups[1].Provider != "anthropic" || rollups[1].AccountCount != 2 || rollups[1].ThrottledCount != 1 {
		t.Errorf("anthropic rollup = %+v", rollups[1])
	}
	if !rollups[1].AvgUtil7dHas || rollups[1].AvgUtil7dPct != 40 {
		t.Errorf("anthropic avg-util = (%v, %d), want (true, 40)", rollups[1].AvgUtil7dHas, rollups[1].AvgUtil7dPct)
	}
	if rollups[2].Provider != "unknown" {
		t.Errorf("rollups[2].Provider = %q, want unknown (empty bucket)", rollups[2].Provider)
	}
	if got := rollups[0].SharePct; got != 55 { // 20/36*100 = 55.55…
		t.Errorf("bedrock share = %d, want 55", got)
	}
}

func TestBuildCostsTopSpenders(t *testing.T) {
	accts := []costAccountView{
		{ID: "gd-a", Name: "alpha", DailyCostUSD: 1.0},
		{ID: "gd-b", Name: "bravo", DailyCostUSD: 5.0, IsThrottled: true},
		{ID: "gd-c", Name: "charlie", DailyCostUSD: 3.0},
		{ID: "gd-d", Name: "delta", DailyCostUSD: 0},
		{ID: "gd-e", Name: "echo", DailyCostUSD: 2.0},
	}
	top := buildCostsTopSpenders(accts, 11.0, 3)
	if len(top) != 3 {
		t.Fatalf("len(top) = %d, want 3", len(top))
	}
	if top[0].Name != "bravo" || top[1].Name != "charlie" || top[2].Name != "echo" {
		t.Errorf("top order = [%s %s %s], want [bravo charlie echo]", top[0].Name, top[1].Name, top[2].Name)
	}
	if top[0].DailyUSD != "$5.00" {
		t.Errorf("top[0].DailyUSD = %q, want $5.00", top[0].DailyUSD)
	}
	if !top[0].IsThrottled {
		t.Errorf("top[0].IsThrottled = false, want true")
	}
	if got := top[0].SharePct; got != 45 { // 5/11*100 = 45.45…
		t.Errorf("bravo share = %d, want 45", got)
	}
}

func TestBuildCostsSummary_MonthProjection(t *testing.T) {
	// Pick a date in the middle of April (30 days) so day-of-month and
	// days-in-month are stable across runs.
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	accts := []costAccountView{
		{DailyCostUSD: 10, WeeklyCostUSD: 70, IsActive: true, HasSpend: true},
		{DailyCostUSD: 4, WeeklyCostUSD: 28, IsActive: true, HasSpend: true},
	}
	s := buildCostsSummary(accts, now)
	if s.MonthDayOfMonth != 15 {
		t.Errorf("MonthDayOfMonth = %d, want 15", s.MonthDayOfMonth)
	}
	if s.MonthDaysInMonth != 30 {
		t.Errorf("MonthDaysInMonth = %d, want 30", s.MonthDaysInMonth)
	}
	if s.MonthProgressPct != 50 {
		t.Errorf("MonthProgressPct = %d, want 50", s.MonthProgressPct)
	}
	// WeeklyTotal=98, dailyAvg=14, MTD proj=14*15=210, ProjectedMonth=14*30=420.
	if got, want := s.MTDProjectionUSD, 210.0; got < want-0.01 || got > want+0.01 {
		t.Errorf("MTDProjectionUSD = %v, want %v", got, want)
	}
	if got, want := s.ProjectedMonthUSD, 420.0; got < want-0.01 || got > want+0.01 {
		t.Errorf("ProjectedMonthUSD = %v, want %v", got, want)
	}
}

// TestBuildCostsSummary_DayProgressUsesUTC pins ka-0000004628: the day/month
// progress captions must derive from the UTC day, matching the UTC-day-bucketed
// numerator (spend_sync.go:124-125), regardless of the pod's local zone. Before
// the fix, handleCosts fed a *local* time.Now() into these ratios, so a non-UTC
// TZ made "% of day elapsed" disagree with the UTC-day spend it paces.
func TestBuildCostsSummary_DayProgressUsesUTC(t *testing.T) {
	// One physical instant, expressed once in UTC and once in a non-UTC zone
	// that lands on a DIFFERENT calendar day/month. 2026-05-01 01:30 UTC is
	// 2026-04-30 18:30 in UTC-07 — different day (30 vs 1), different month
	// (Apr vs May), different local hour (18 vs 1). A correct implementation
	// must read the UTC clock (01:30 → ~6% of day) from both.
	utcNow := time.Date(2026, 5, 1, 1, 30, 0, 0, time.UTC)
	west := time.FixedZone("UTC-07", -7*3600)
	localNow := utcNow.In(west)

	accts := []costAccountView{{DailyCostUSD: 10, WeeklyCostUSD: 70, IsActive: true, HasSpend: true}}

	fromUTC := buildCostsSummary(accts, utcNow)
	fromLocal := buildCostsSummary(accts, localNow)

	// 01:30 UTC → 1.5/24 → 6%. If the local hour (18) leaked through it would
	// read 75%.
	if fromUTC.DayProgressPct != 6 {
		t.Errorf("DayProgressPct from UTC now = %d, want 6", fromUTC.DayProgressPct)
	}
	if fromLocal.DayProgressPct != fromUTC.DayProgressPct {
		t.Errorf("DayProgressPct differs by caller zone: local=%d utc=%d (must both be UTC-derived)",
			fromLocal.DayProgressPct, fromUTC.DayProgressPct)
	}
	// UTC day is May 1st; a leaked local zone would report April 30th.
	if fromLocal.MonthDayOfMonth != 1 {
		t.Errorf("MonthDayOfMonth from non-UTC now = %d, want 1 (UTC May 1st)", fromLocal.MonthDayOfMonth)
	}
	if fromLocal.MonthDaysInMonth != 31 {
		t.Errorf("MonthDaysInMonth from non-UTC now = %d, want 31 (UTC May)", fromLocal.MonthDaysInMonth)
	}
	if fromLocal.MonthDayOfMonth != fromUTC.MonthDayOfMonth || fromLocal.MonthDaysInMonth != fromUTC.MonthDaysInMonth {
		t.Errorf("month fields differ by caller zone: local=(%d/%d) utc=(%d/%d)",
			fromLocal.MonthDayOfMonth, fromLocal.MonthDaysInMonth, fromUTC.MonthDayOfMonth, fromUTC.MonthDaysInMonth)
	}
	// LastUpdated renders in UTC too (page framing is "UTC day").
	if fromLocal.LastUpdated != "01:30" {
		t.Errorf("LastUpdated from non-UTC now = %q, want %q (UTC clock)", fromLocal.LastUpdated, "01:30")
	}
}

func TestBuildCostsSummary_MaxUtil(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	accts := []costAccountView{
		{Util5h: 0.42, Util5hPct: 42, Util5hHas: true, Util7d: 0.18, Util7dPct: 18, Util7dHas: true},
		{Util5h: 0.95, Util5hPct: 95, Util5hHas: true, Util7d: 0.71, Util7dPct: 71, Util7dHas: true},
		{}, // no util signal — should not affect max
	}
	s := buildCostsSummary(accts, now)
	if s.MaxUtil5hPct != 95 {
		t.Errorf("MaxUtil5hPct = %d, want 95", s.MaxUtil5hPct)
	}
	if s.MaxUtil7dPct != 71 {
		t.Errorf("MaxUtil7dPct = %d, want 71", s.MaxUtil7dPct)
	}
}

// modelGaugeBody returns a per-model tag-gauge vector keyed on the shared
// `value` label (dim="model"), the shape gardenio_tag_cost_usd returns. Each
// entry maps a model name to its stringified USD value.
func modelGaugeBody(vals map[string]string) string {
	var parts []string
	for model, v := range vals {
		parts = append(parts, `{"metric":{"dim":"model","value":"`+model+
			`","env":"prod"},"value":[1700000000,"`+v+`"]}`)
	}
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		strings.Join(parts, ",") + `]}}`
}

// mkModelRollupServer wires a viewer to a fake vmsingle that returns per-window
// per-model gauge vectors, dispatched by inspecting the incoming query: the
// lifetime query has no increase(), the 24h/7d queries carry their window.
func mkModelRollupServer(t *testing.T, daily, weekly, total map[string]string) *Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(q, "[1d:"):
			_, _ = w.Write([]byte(modelGaugeBody(daily)))
		case strings.Contains(q, "[7d:"):
			_, _ = w.Write([]byte(modelGaugeBody(weekly)))
		default: // lifetime cumulative (no increase window)
			_, _ = w.Write([]byte(modelGaugeBody(total)))
		}
	}))
	t.Cleanup(upstream.Close)
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.SetMetricsProxy(upstream.URL)
	return srv
}

// TestBuildCostsModelRollups_RealBillSource is the ka-0000006501 fix: the
// on-page "By model" rollup is sourced from the real-bill tag gauge
// (gardenio_tag_cost_usd{dim="model"}) keyed by EFFECTIVE model, NOT the account
// bead's `model` field. It must NOT synthesize an "unknown" bucket, and it must
// sort by 24h spend descending with a self-relative share of the visible total.
func TestBuildCostsModelRollups_RealBillSource(t *testing.T) {
	srv := mkModelRollupServer(t,
		map[string]string{"claude-opus-4-8": "30", "claude-haiku-4-5": "10"},
		map[string]string{"claude-opus-4-8": "200", "claude-haiku-4-5": "70"},
		map[string]string{"claude-opus-4-8": "625.85", "claude-haiku-4-5": "180.15"},
	)
	rollups := srv.buildCostsModelRollups(context.Background())
	if len(rollups) != 2 {
		t.Fatalf("len(rollups) = %d, want 2", len(rollups))
	}
	// Sorted by 24h daily desc: opus(30) > haiku(10). No "unknown" bucket.
	if rollups[0].Model != "claude-opus-4-8" {
		t.Errorf("rollups[0].Model = %q, want claude-opus-4-8", rollups[0].Model)
	}
	if rollups[1].Model != "claude-haiku-4-5" {
		t.Errorf("rollups[1].Model = %q, want claude-haiku-4-5", rollups[1].Model)
	}
	for _, r := range rollups {
		if r.Model == "unknown" {
			t.Error("real-bill rollup must never synthesize an 'unknown' bucket")
		}
	}
	// Windowed + lifetime values flow through per model.
	if rollups[0].DailyUSD != 30 || rollups[0].WeeklyUSD != 200 || rollups[0].TotalUSD != 625.85 {
		t.Errorf("opus row = %+v, want daily 30 / week 200 / lifetime 625.85", rollups[0])
	}
	// Share is of the VISIBLE 24h total (30+10=40), always ≤100: opus 30/40=75.
	if rollups[0].SharePct != 75 {
		t.Errorf("opus SharePct = %d, want 75 (30/40)", rollups[0].SharePct)
	}
	if rollups[1].SharePct != 25 {
		t.Errorf("haiku SharePct = %d, want 25 (10/40)", rollups[1].SharePct)
	}
}

// TestBuildCostsModelRollups_EffectiveModelNotAccountDefault pins the acceptance
// criterion: a haiku role riding an opus-DEFAULT account lands in the haiku
// bucket, because the source is the corrected per-session `model:` tag, not the
// account bead's default. The account-bead fields are irrelevant here — the
// function never reads them — so the fixture only supplies the gauge.
func TestBuildCostsModelRollups_EffectiveModelNotAccountDefault(t *testing.T) {
	// Gauge reports two effective models even though (in prod) both could ride a
	// single opus-default account: the split proves attribution is per-effective-
	// model, not per-account-default.
	srv := mkModelRollupServer(t,
		map[string]string{"claude-opus-4-8": "40", "claude-haiku-4-5": "60"},
		map[string]string{"claude-opus-4-8": "40", "claude-haiku-4-5": "60"},
		map[string]string{"claude-opus-4-8": "40", "claude-haiku-4-5": "60"},
	)
	rollups := srv.buildCostsModelRollups(context.Background())
	byModel := map[string]float64{}
	for _, r := range rollups {
		byModel[r.Model] = r.DailyUSD
	}
	if byModel["claude-haiku-4-5"] != 60 {
		t.Errorf("haiku bucket = %v, want 60 (effective model, not folded into the opus default)", byModel["claude-haiku-4-5"])
	}
	if byModel["claude-opus-4-8"] != 40 {
		t.Errorf("opus bucket = %v, want 40", byModel["claude-opus-4-8"])
	}
}

// TestBuildCostsModelRollups_LifetimeOnlyModelStillRenders: a model with only
// lifetime spend (nothing in the last 24h/7d windows) must still get a row, so
// the union of window keys — not just the 24h keys — drives the row set.
func TestBuildCostsModelRollups_LifetimeOnlyModelStillRenders(t *testing.T) {
	srv := mkModelRollupServer(t,
		map[string]string{"claude-opus-4-8": "30"},
		map[string]string{"claude-opus-4-8": "200"},
		map[string]string{"claude-opus-4-8": "625.85", "claude-fable-5": "12.00"},
	)
	rollups := srv.buildCostsModelRollups(context.Background())
	var fable *costsModelRollup
	for i := range rollups {
		if rollups[i].Model == "claude-fable-5" {
			fable = &rollups[i]
		}
	}
	if fable == nil {
		t.Fatal("lifetime-only model claude-fable-5 missing a row")
	}
	if fable.TotalUSD != 12.00 || fable.DailyUSD != 0 {
		t.Errorf("fable row = %+v, want lifetime 12.00 / daily 0", *fable)
	}
}

// TestBuildCostsModelRollups_NoUpstream returns nil (section hidden) rather than
// a fabricated or single-"unknown" table when no vmsingle upstream is wired —
// the same honest empty state the headline rate uses.
func TestBuildCostsModelRollups_NoUpstream(t *testing.T) {
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	if got := srv.buildCostsModelRollups(context.Background()); got != nil {
		t.Errorf("no-upstream rollup = %v, want nil (section hidden)", got)
	}
}

// TestBuildCostsModelRollups_NegativeFlooredAndEmptyKeyDropped guards the
// per-sample hardening: a transient negative (counter reset mid-window) floors
// to 0 and an empty model key is dropped rather than rendered as a blank row.
func TestBuildCostsModelRollups_NegativeFlooredAndEmptyKeyDropped(t *testing.T) {
	srv := mkModelRollupServer(t,
		map[string]string{"claude-opus-4-8": "-5", "": "9", "claude-haiku-4-5": "10"},
		map[string]string{"claude-opus-4-8": "0", "claude-haiku-4-5": "10"},
		map[string]string{"claude-opus-4-8": "100", "claude-haiku-4-5": "50"},
	)
	rollups := srv.buildCostsModelRollups(context.Background())
	for _, r := range rollups {
		if r.Model == "" {
			t.Error("empty model key should be dropped, not rendered as a blank row")
		}
		if r.DailyUSD < 0 {
			t.Errorf("model %q daily = %v, negative not floored", r.Model, r.DailyUSD)
		}
	}
	// opus's negative daily floored to 0, so haiku(10) now sorts first.
	if len(rollups) == 0 || rollups[0].Model != "claude-haiku-4-5" {
		t.Fatalf("want haiku first after opus floored to 0, got %+v", rollups)
	}
}

// TestCostsModelRollupQueries pins the PromQL shape: both the windowed and the
// lifetime query read the real-bill tag gauge with dim="model" and the HA dedup,
// and neither touches the at-close counter (that magnitude bug is ka-0000002699's
// lane).
func TestCostsModelRollupQueries(t *testing.T) {
	win := costsModelRollupWindowQuery("1d")
	for _, want := range []string{
		`sum by(value)(`,
		`increase((max by (dim, value, env) (` + costsRollupTagGaugeMetric + `{dim="model"}))[1d:5m]))`,
	} {
		if !strings.Contains(win, want) {
			t.Errorf("window query %q missing %q", win, want)
		}
	}
	for _, want := range []string{
		`sum by(value)(`,
		`max by (dim, value, env) (` + costsRollupTagGaugeMetric + `{dim="model"})`,
	} {
		if !strings.Contains(costsModelRollupTotalQuery, want) {
			t.Errorf("total query %q missing %q", costsModelRollupTotalQuery, want)
		}
	}
	// Neither may reference the at-close counter (the wrong-magnitude source).
	if strings.Contains(win, costsRollupMetric) || strings.Contains(costsModelRollupTotalQuery, costsRollupMetric) {
		t.Error("model rollup queries must not reference the at-close counter")
	}
}

func TestBuildCostsHottestUtilization(t *testing.T) {
	accts := []costAccountView{
		{ID: "gd-a", Name: "alpha", Util5hPct: 10, Util5hHas: true, Util7dPct: 5, Util7dHas: true},
		{ID: "gd-b", Name: "bravo", Util5hPct: 95, Util5hHas: true, Util7dPct: 71, Util7dHas: true, IsThrottled: true},
		{ID: "gd-c", Name: "charlie", Util5hPct: 50, Util5hHas: true, Util7dPct: 30, Util7dHas: true},
		{ID: "gd-d", Name: "delta"}, // no util — should drop
		{ID: "gd-e", Name: "echo", Util5hPct: 0, Util5hHas: true, Util7dPct: 0, Util7dHas: true}, // both zero — should drop
	}
	out := buildCostsHottestUtilization(accts, 5)
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	if out[0].Name != "bravo" || !out[0].IsThrottled {
		t.Errorf("out[0] = %+v, want bravo throttled", out[0])
	}
	if out[1].Name != "charlie" || out[2].Name != "alpha" {
		t.Errorf("order = [%s %s %s], want [bravo charlie alpha]", out[0].Name, out[1].Name, out[2].Name)
	}
}

func TestBuildCostsHottestUtilization_Limit(t *testing.T) {
	accts := make([]costAccountView, 0, 10)
	for i := 1; i <= 10; i++ {
		accts = append(accts, costAccountView{
			ID:        "gd-" + strconv.Itoa(i),
			Name:      "acct-" + strconv.Itoa(i),
			Util5hPct: i * 10, Util5hHas: true,
		})
	}
	out := buildCostsHottestUtilization(accts, 3)
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	// Highest 3 by util are acct-10, acct-9, acct-8.
	if out[0].Util5hPct != 100 || out[1].Util5hPct != 90 || out[2].Util5hPct != 80 {
		t.Errorf("util order = [%d %d %d], want [100 90 80]", out[0].Util5hPct, out[1].Util5hPct, out[2].Util5hPct)
	}
}

func TestDaysInMonth(t *testing.T) {
	cases := []struct {
		year  int
		month time.Month
		want  int
	}{
		{2026, time.January, 31},
		{2026, time.February, 28},
		{2024, time.February, 29}, // leap year
		{2026, time.April, 30},
		{2026, time.December, 31},
	}
	for _, c := range cases {
		if got := daysInMonth(c.year, c.month); got != c.want {
			t.Errorf("daysInMonth(%d, %v) = %d, want %d", c.year, c.month, got, c.want)
		}
	}
}

func TestBuildCostsThrottled(t *testing.T) {
	accts := []costAccountView{
		{ID: "gd-a", Name: "alpha", IsThrottled: false},
		{ID: "gd-b", Name: "bravo", IsThrottled: true, ThrottleLabel: "throttled", ThrottleReset: "resets in 1h", Util5hPct: 95, Util7dPct: 71},
	}
	out := buildCostsThrottled(accts)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].Name != "bravo" || out[0].Util5hPct != 95 {
		t.Errorf("throttled row = %+v", out[0])
	}
}

func TestBuildCostAccountView_PerWindowResets(t *testing.T) {
	now := time.Date(2026, 5, 15, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name             string
		fields           map[string]string
		wantReset5h      bool
		wantReset7d      bool
		wantBindingReset bool
	}{
		{
			name: "both windows parsed",
			fields: map[string]string{
				"throttle_reset_at":    now.Add(2 * time.Hour).Format(time.RFC3339),
				"throttle_reset_5h_at": now.Add(45 * time.Minute).Format(time.RFC3339),
				"throttle_reset_7d_at": now.Add(3 * 24 * time.Hour).Format(time.RFC3339),
			},
			wantReset5h:      true,
			wantReset7d:      true,
			wantBindingReset: true,
		},
		{
			name: "only 5h parsed",
			fields: map[string]string{
				"throttle_reset_5h_at": now.Add(20 * time.Minute).Format(time.RFC3339),
			},
			wantReset5h: true,
		},
		{
			name: "only 7d parsed",
			fields: map[string]string{
				"throttle_reset_7d_at": now.Add(6 * 24 * time.Hour).Format(time.RFC3339),
			},
			wantReset7d: true,
		},
		{
			name:   "neither parsed",
			fields: map[string]string{},
		},
		{
			name: "malformed timestamps are ignored",
			fields: map[string]string{
				"throttle_reset_5h_at": "not-a-date",
				"throttle_reset_7d_at": "",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := &beadsapi.BeadDetail{ID: "gd-x", Title: "acct", Fields: tc.fields}
			v := buildCostAccountView(b, now)
			if (v.Reset5hCountdown != "") != tc.wantReset5h {
				t.Errorf("Reset5hCountdown = %q, want present=%v", v.Reset5hCountdown, tc.wantReset5h)
			}
			if (v.Reset7dCountdown != "") != tc.wantReset7d {
				t.Errorf("Reset7dCountdown = %q, want present=%v", v.Reset7dCountdown, tc.wantReset7d)
			}
			if (v.ThrottleReset != "") != tc.wantBindingReset {
				t.Errorf("ThrottleReset = %q, want present=%v", v.ThrottleReset, tc.wantBindingReset)
			}
		})
	}
}

func TestBuildCostsThrottled_PropagatesResets(t *testing.T) {
	accts := []costAccountView{
		{
			ID: "gd-b", Name: "bravo", IsThrottled: true, ThrottleLabel: "throttled",
			Util5hPct: 95, Util5hHas: true,
			Util7dPct: 71, Util7dHas: true,
			Reset5hCountdown: "resets in 45m",
			Reset7dCountdown: "resets in 3d",
		},
	}
	out := buildCostsThrottled(accts)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].Reset5hCountdown != "resets in 45m" || out[0].Reset7dCountdown != "resets in 3d" {
		t.Errorf("countdowns = (%q, %q), want (resets in 45m, resets in 3d)", out[0].Reset5hCountdown, out[0].Reset7dCountdown)
	}
	if !out[0].Util5hHas || !out[0].Util7dHas {
		t.Errorf("util has flags = (%v, %v), want (true, true)", out[0].Util5hHas, out[0].Util7dHas)
	}
}

func TestBuildCostsHottestUtilization_PropagatesResets(t *testing.T) {
	accts := []costAccountView{
		{
			ID: "gd-b", Name: "bravo",
			Util5hPct: 95, Util5hHas: true,
			Util7dPct: 71, Util7dHas: true,
			Reset5hCountdown: "resets in 45m",
			Reset7dCountdown: "resets in 3d",
		},
	}
	out := buildCostsHottestUtilization(accts, 5)
	if len(out) != 1 {
		t.Fatalf("len = %d, want 1", len(out))
	}
	if out[0].Reset5hCountdown != "resets in 45m" || out[0].Reset7dCountdown != "resets in 3d" {
		t.Errorf("countdowns = (%q, %q)", out[0].Reset5hCountdown, out[0].Reset7dCountdown)
	}
	if !out[0].Util5hHas || !out[0].Util7dHas {
		t.Errorf("util has flags = (%v, %v)", out[0].Util5hHas, out[0].Util7dHas)
	}
}

func TestCostAccountView_CostPerSession(t *testing.T) {
	// F6: lifetime cost / lifetime sessions, not daily / lifetime.
	if got := (costAccountView{TotalCostUSD: 10, SessionCount: 5}).CostPerSession(); got != 2.0 {
		t.Errorf("CostPerSession = %v, want 2.0", got)
	}
	if got := (costAccountView{TotalCostUSD: 10, SessionCount: 0}).CostPerSession(); got != 0 {
		t.Errorf("CostPerSession with 0 sessions = %v, want 0", got)
	}
}

// TestBuildCostAccountView_NoLiteLLMSpend locks the headline I2 guarantee
// (epic ka-0000000758, design §5): an account bead with no LiteLLM-sourced
// cost fields yields $0 with HasSpend=false — never a fabricated estimate.
// The viewer has no fallback price table and performs no token×rate math, so
// absent cost fields MUST surface as zero spend, not a computed number.
func TestBuildCostAccountView_NoLiteLLMSpend(t *testing.T) {
	now := time.Date(2026, 6, 6, 12, 0, 0, 0, time.UTC)

	// Account bead carrying usage/utilization signal but NO cost fields —
	// the controller never wrote daily/weekly/total_cost_usd (no LiteLLM spend).
	b := &beadsapi.BeadDetail{
		ID:    "gd-nospend",
		Title: "anthropic-idle-01",
		Fields: map[string]string{
			"provider":            "anthropic",
			"model":               "claude-opus-4-8",
			"status":              "active",
			"session_count":       "3",
			"total_output_tokens": "12000",
			"utilization_5h":      "0.4",
		},
	}
	v := buildCostAccountView(b, now)

	if v.DailyCostUSD != 0 || v.WeeklyCostUSD != 0 || v.TotalCostUSD != 0 {
		t.Errorf("expected all cost fields 0 when absent, got daily=%v weekly=%v total=%v",
			v.DailyCostUSD, v.WeeklyCostUSD, v.TotalCostUSD)
	}
	if v.SinceResetSpend != 0 {
		t.Errorf("SinceResetSpend = %v, want 0 (no spend, no baseline)", v.SinceResetSpend)
	}
	if v.HasSpend {
		t.Error("HasSpend = true with no LiteLLM cost fields; want false (no fabricated spend)")
	}
	// Usage signal must still flow — absence of *cost* must not zero out usage.
	if !v.HasUsage || v.SessionCount != 3 {
		t.Errorf("usage signal lost: HasUsage=%v SessionCount=%d", v.HasUsage, v.SessionCount)
	}
}

func TestBuildCostAccountView_CarriesLiteLLMKey(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	b := &beadsapi.BeadDetail{
		ID:    "gd-key",
		Title: "litellm-bedrock",
		Fields: map[string]string{
			"litellm_key":    "sk-shared-abc",
			"total_cost_usd": "786.15",
		},
	}
	v := buildCostAccountView(b, now)
	if v.LiteLLMKey != "sk-shared-abc" {
		t.Errorf("LiteLLMKey = %q, want sk-shared-abc", v.LiteLLMKey)
	}
	// A cost-duplicate is only marked by markCostDuplicates over the full set;
	// a freshly-built view is never a duplicate on its own.
	if v.CostDuplicate {
		t.Error("CostDuplicate = true on a standalone view; want false")
	}
}

func TestMarkCostDuplicates(t *testing.T) {
	// glm-bedrock + litellm-bedrock share one key; anthropic-prod-01 is alone on
	// a distinct key; no-key has an empty key and must never be deduped.
	accounts := []costAccountView{
		{Name: "anthropic-prod-01", LiteLLMKey: "sk-solo"},
		{Name: "glm-bedrock", LiteLLMKey: "sk-shared"},
		{Name: "litellm-bedrock", LiteLLMKey: "sk-shared"},
		{Name: "no-key-a", LiteLLMKey: ""},
		{Name: "no-key-b", LiteLLMKey: ""},
	}
	markCostDuplicates(accounts)

	dup := map[string]bool{}
	for _, a := range accounts {
		dup[a.Name] = a.CostDuplicate
	}
	// Owner is the lexicographically-first name on the shared key: glm-bedrock
	// < litellm-bedrock, so glm-bedrock owns and litellm-bedrock is the dup.
	if dup["glm-bedrock"] {
		t.Error("glm-bedrock marked duplicate; want owner (lexicographically first)")
	}
	if !dup["litellm-bedrock"] {
		t.Error("litellm-bedrock not marked duplicate; want duplicate")
	}
	// Solo-key and empty-key accounts are never deduped.
	for _, name := range []string{"anthropic-prod-01", "no-key-a", "no-key-b"} {
		if dup[name] {
			t.Errorf("%s marked duplicate; want kept (solo or empty key)", name)
		}
	}
}

func TestMarkCostDuplicates_DeterministicRegardlessOfOrder(t *testing.T) {
	// The owner is the min NAME, not the first in slice order — reversing the
	// input must not change which account owns the shared key's cost.
	forward := []costAccountView{
		{Name: "glm-bedrock", LiteLLMKey: "sk-shared"},
		{Name: "litellm-bedrock", LiteLLMKey: "sk-shared"},
	}
	reverse := []costAccountView{
		{Name: "litellm-bedrock", LiteLLMKey: "sk-shared"},
		{Name: "glm-bedrock", LiteLLMKey: "sk-shared"},
	}
	markCostDuplicates(forward)
	markCostDuplicates(reverse)

	ownerOf := func(accts []costAccountView) string {
		for _, a := range accts {
			if !a.CostDuplicate {
				return a.Name
			}
		}
		return ""
	}
	if got := ownerOf(forward); got != "glm-bedrock" {
		t.Errorf("forward owner = %q, want glm-bedrock", got)
	}
	if got := ownerOf(reverse); got != "glm-bedrock" {
		t.Errorf("reverse owner = %q, want glm-bedrock", got)
	}
}

// sharedKeyAccounts reproduces the ka-0000003446 scenario: two distinct account
// beads (different models) share one litellm_key, so spend_sync stamped the SAME
// per-key total onto both. A third account is alone on its own key. All figures
// mirror the real litellm-bedrock/glm-bedrock beads (one $761 bill counted once).
func sharedKeyAccounts() []costAccountView {
	shared := costAccountView{
		DailyCostUSD: 150.0, WeeklyCostUSD: 227.0, TotalCostUSD: 761.0,
		SessionCount: 4, Provider: "anthropic", IsActive: true, HasSpend: true,
		LiteLLMKey: "sk-shared",
	}
	glm := shared
	glm.Name, glm.Model = "glm-bedrock", "bedrock-glm-5"
	litellm := shared
	litellm.Name, litellm.Model = "litellm-bedrock", "bedrock-claude-opus-4-7"
	solo := costAccountView{
		Name: "anthropic-prod-01", Model: "claude-opus-4-8", Provider: "anthropic",
		DailyCostUSD: 50.0, WeeklyCostUSD: 100.0, TotalCostUSD: 300.0,
		SessionCount: 6, IsActive: true, HasSpend: true, LiteLLMKey: "sk-solo",
	}
	accounts := []costAccountView{glm, litellm, solo}
	markCostDuplicates(accounts)
	return accounts
}

func TestSharedKey_SummaryBooksBillOnce(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	s := buildCostsSummary(sharedKeyAccounts(), now)

	// Money: shared $150 daily counted once + solo $50 = $200 (NOT $350).
	if s.GrandTotalUSD != 200.0 {
		t.Errorf("GrandTotalUSD = %v, want 200 (shared daily booked once)", s.GrandTotalUSD)
	}
	if s.WeeklyTotalUSD != 327.0 { // 227 + 100
		t.Errorf("WeeklyTotalUSD = %v, want 327", s.WeeklyTotalUSD)
	}
	if s.TotalCostAllUSD != 1061.0 { // 761 + 300
		t.Errorf("TotalCostAllUSD = %v, want 1061", s.TotalCostAllUSD)
	}
	// Counts stay per-account: all three beads have their own sessions/active.
	if s.TotalSessions != 14 { // 4 + 4 + 6
		t.Errorf("TotalSessions = %d, want 14 (per-account, not deduped)", s.TotalSessions)
	}
	if s.ActiveCount != 3 {
		t.Errorf("ActiveCount = %d, want 3 (per-account, not deduped)", s.ActiveCount)
	}
	if s.TotalCount != 3 {
		t.Errorf("TotalCount = %d, want 3", s.TotalCount)
	}
}

func TestSharedKey_RollupsReconcile_NoShareOver100(t *testing.T) {
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	accounts := sharedKeyAccounts()
	s := buildCostsSummary(accounts, now)

	providers := buildCostsProviderRollups(accounts, s.GrandTotalUSD)
	top := buildCostsTopSpenders(accounts, s.GrandTotalUSD, 5)
	// The "By model" rollup is no longer account-sourced (ka-0000006501): it
	// reads the real-bill gardenio_tag_cost_usd{dim="model"} gauge, so the shared-
	// litellm_key account dedup no longer applies to it. Its own reconciliation
	// is covered by TestBuildCostsModelRollups_* below.

	// Provider rollup: single "anthropic" bucket = full deduped grand total.
	var provDaily float64
	var provShare int
	for _, p := range providers {
		provDaily += p.DailyUSD
		provShare += p.SharePct
		if p.SharePct > 100 {
			t.Errorf("provider %q SharePct = %d > 100", p.Provider, p.SharePct)
		}
	}
	if provDaily != s.GrandTotalUSD {
		t.Errorf("sum(provider daily) = %v, want GrandTotal %v", provDaily, s.GrandTotalUSD)
	}

	// Top spenders: the duplicate must NOT appear as its own row (would
	// double-list the shared bill and double its SharePct).
	var topShare int
	seen := map[string]bool{}
	for _, ts := range top {
		seen[ts.Name] = true
		topShare += ts.SharePct
		if ts.SharePct > 100 {
			t.Errorf("top spender %q SharePct = %d > 100", ts.Name, ts.SharePct)
		}
	}
	if seen["litellm-bedrock"] {
		t.Error("cost-duplicate litellm-bedrock listed in top spenders; want skipped")
	}
	if !seen["glm-bedrock"] || !seen["anthropic-prod-01"] {
		t.Error("owner and solo accounts missing from top spenders")
	}
	if topShare > 100 {
		t.Errorf("sum(top spender share) = %d > 100 (double-count)", topShare)
	}
}

func TestSharedKey_PerAccountCardsUnchanged(t *testing.T) {
	// The dedup flag must not alter the per-account cost the CARD renders: the
	// duplicate keeps its real bill so the operator still sees each bead's cost.
	accounts := sharedKeyAccounts()
	var dup *costAccountView
	for i := range accounts {
		if accounts[i].Name == "litellm-bedrock" {
			dup = &accounts[i]
		}
	}
	if dup == nil {
		t.Fatal("litellm-bedrock not present")
	}
	if !dup.CostDuplicate {
		t.Fatal("litellm-bedrock should be a cost-duplicate in this fixture")
	}
	if dup.DailyCostUSD != 150.0 || dup.TotalCostUSD != 761.0 {
		t.Errorf("per-account card cost mutated: daily=%v total=%v, want 150 and 761",
			dup.DailyCostUSD, dup.TotalCostUSD)
	}
}

func TestHandleCosts_SharedKey_EndToEnd(t *testing.T) {
	now := time.Now()
	f := newFake()
	// Reproduces ka-0000003446: two beads with different models share one key,
	// each stamped with the identical per-key total by spend_sync.
	mkAcct := func(id, title, model, key, daily, weekly, total string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID: id, Title: title, Type: "account", Status: "open", UpdatedAt: now,
			Fields: map[string]string{
				"provider": "anthropic", "model": model, "status": "active",
				"litellm_key": key, "daily_cost_usd": daily,
				"weekly_cost_usd": weekly, "total_cost_usd": total,
				"session_count": "4",
			},
		}
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkAcct("gd-glm", "glm-bedrock", "bedrock-glm-5", "sk-shared", "150.00", "227.00", "761.00"),
			mkAcct("gd-litellm", "litellm-bedrock", "bedrock-claude-opus-4-7", "sk-shared", "150.00", "227.00", "761.00"),
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// Fleet "Today" total is the shared $150 booked once — NOT $300.
	if !strings.Contains(body, "$150.00") {
		t.Errorf("expected deduped grand total $150.00 in body")
	}
	if strings.Contains(body, "$300.00") {
		t.Errorf("shared key double-counted: $300.00 present in body")
	}
	// Both account cards still render (per-account cards unchanged).
	for _, want := range []string{"glm-bedrock", "litellm-bedrock"} {
		if !strings.Contains(body, want) {
			t.Errorf("account card %q missing from body", want)
		}
	}
}

// TestBuildCostAccountView_SubscriptionWindows pins parsing of the new
// bill-against-window fields + the billing_type default (ka-0000003343).
func TestBuildCostAccountView_SubscriptionWindows(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	t.Run("subscription with both windows", func(t *testing.T) {
		b := &beadsapi.BeadDetail{ID: "gd-s", Title: "sub", Fields: map[string]string{
			"spend_5h_window_usd":  "2.50",
			"spend_7d_window_usd":  "40.00",
			"representative_claim": "seven_day",
			// billing_type intentionally omitted → defaults to subscription.
		}}
		v := buildCostAccountView(b, now)
		if v.BillingType != accountBillingSubscription {
			t.Errorf("BillingType = %q, want subscription (default)", v.BillingType)
		}
		if !v.Bill5hHas || v.Bill5hUSD != 2.50 {
			t.Errorf("Bill5h = (%v, %v), want (true, 2.50)", v.Bill5hHas, v.Bill5hUSD)
		}
		if !v.Bill7dHas || v.Bill7dUSD != 40.00 {
			t.Errorf("Bill7d = (%v, %v), want (true, 40.00)", v.Bill7dHas, v.Bill7dUSD)
		}
		if v.RepresentativeClaim != representativeClaimSevenDay {
			t.Errorf("RepresentativeClaim = %q, want seven_day", v.RepresentativeClaim)
		}
	})

	t.Run("metered account, no window fields", func(t *testing.T) {
		b := &beadsapi.BeadDetail{ID: "gd-m", Title: "met", Fields: map[string]string{
			"billing_type":   "metered",
			"daily_cost_usd": "9.00",
		}}
		v := buildCostAccountView(b, now)
		if v.BillingType != accountBillingMetered {
			t.Errorf("BillingType = %q, want metered", v.BillingType)
		}
		if v.Bill5hHas || v.Bill7dHas {
			t.Errorf("metered account must not carry window bills: 5h=%v 7d=%v", v.Bill5hHas, v.Bill7dHas)
		}
	})
}

// TestBuildCostsSummary_SubscriptionHeadline verifies the fleet headline sums
// only subscription-account window spend, excludes metered, and picks the
// binding window per representative_claim (ka-0000003343).
func TestBuildCostsSummary_SubscriptionHeadline(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)

	t.Run("five_hour binding, metered excluded", func(t *testing.T) {
		accts := []costAccountView{
			{BillingType: accountBillingSubscription, RepresentativeClaim: "five_hour",
				Bill5hUSD: 3.0, Bill5hHas: true, Bill7dUSD: 30.0, Bill7dHas: true, DailyCostUSD: 5},
			{BillingType: accountBillingSubscription, RepresentativeClaim: "five_hour",
				Bill5hUSD: 1.0, Bill5hHas: true, Bill7dUSD: 10.0, Bill7dHas: true, DailyCostUSD: 2},
			// Metered account: contributes to GrandTotal but NOT the subscription headline.
			{BillingType: accountBillingMetered, Bill5hUSD: 99, Bill5hHas: true, DailyCostUSD: 7},
		}
		s := buildCostsSummary(accts, now)
		if s.SubscriptionCount != 2 {
			t.Errorf("SubscriptionCount = %d, want 2 (metered excluded)", s.SubscriptionCount)
		}
		if s.PrimaryBindsSevenDay {
			t.Error("PrimaryBindsSevenDay = true, want false (all report five_hour)")
		}
		if s.Bill5hUSD != 4.0 {
			t.Errorf("Bill5hUSD = %v, want 4.0 (3+1, metered 99 excluded)", s.Bill5hUSD)
		}
		if s.Bill7dUSD != 40.0 {
			t.Errorf("Bill7dUSD = %v, want 40.0", s.Bill7dUSD)
		}
		if got := s.PrimaryBillUSD(); got != 4.0 {
			t.Errorf("PrimaryBillUSD = %v, want 4.0 (5h binds)", got)
		}
		if got := s.SecondaryBillUSD(); got != 40.0 {
			t.Errorf("SecondaryBillUSD = %v, want 40.0", got)
		}
		if got := s.PrimaryBillLabel(); got != "bill vs 5h limit" {
			t.Errorf("PrimaryBillLabel = %q", got)
		}
	})

	t.Run("seven_day binding when any account reports it", func(t *testing.T) {
		accts := []costAccountView{
			{BillingType: accountBillingSubscription, RepresentativeClaim: "five_hour",
				Bill5hUSD: 3.0, Bill5hHas: true, Bill7dUSD: 30.0, Bill7dHas: true},
			{BillingType: accountBillingSubscription, RepresentativeClaim: "seven_day",
				Bill5hUSD: 1.0, Bill5hHas: true, Bill7dUSD: 90.0, Bill7dHas: true},
		}
		s := buildCostsSummary(accts, now)
		if !s.PrimaryBindsSevenDay {
			t.Error("PrimaryBindsSevenDay = false, want true (one account binds seven_day)")
		}
		if got := s.PrimaryBillUSD(); got != 120.0 {
			t.Errorf("PrimaryBillUSD = %v, want 120.0 (7d binds: 30+90)", got)
		}
		if got := s.PrimaryBillLabel(); got != "bill vs week (7d limit)" {
			t.Errorf("PrimaryBillLabel = %q, want week", got)
		}
	})

	t.Run("7d binds but only 5h data → headline falls back to 5h", func(t *testing.T) {
		accts := []costAccountView{
			// representative_claim says seven_day, but no 7d figure was written
			// (probe race). Headline must not show a misleading $0.00 for 7d.
			{BillingType: accountBillingSubscription, RepresentativeClaim: "seven_day",
				Bill5hUSD: 2.0, Bill5hHas: true},
		}
		s := buildCostsSummary(accts, now)
		if s.PrimaryBindsSevenDay {
			t.Error("PrimaryBindsSevenDay = true, want false (no 7d data → fall back to 5h)")
		}
		if s.PrimaryBillUSD() != 2.0 {
			t.Errorf("PrimaryBillUSD = %v, want 2.0", s.PrimaryBillUSD())
		}
		if s.SecondaryBillHas() {
			t.Error("SecondaryBillHas = true, want false (no 7d figure)")
		}
	})

	t.Run("all metered → no subscription headline", func(t *testing.T) {
		accts := []costAccountView{
			{BillingType: accountBillingMetered, DailyCostUSD: 5, Bill5hUSD: 1, Bill5hHas: true},
		}
		s := buildCostsSummary(accts, now)
		if s.SubscriptionCount != 0 {
			t.Errorf("SubscriptionCount = %d, want 0 (metered-only fleet)", s.SubscriptionCount)
		}
		if s.Bill5hHas || s.Bill7dHas {
			t.Error("metered-only fleet must not set Bill*Has")
		}
	})

	t.Run("cost-duplicate window spend not double-booked", func(t *testing.T) {
		accts := []costAccountView{
			{Name: "a", LiteLLMKey: "sk-shared", BillingType: accountBillingSubscription,
				RepresentativeClaim: "five_hour", Bill5hUSD: 3.0, Bill5hHas: true},
			{Name: "b", LiteLLMKey: "sk-shared", BillingType: accountBillingSubscription,
				RepresentativeClaim: "five_hour", Bill5hUSD: 3.0, Bill5hHas: true},
		}
		markCostDuplicates(accts)
		s := buildCostsSummary(accts, now)
		if s.Bill5hUSD != 3.0 {
			t.Errorf("Bill5hUSD = %v, want 3.0 (shared key booked once, not 6.0)", s.Bill5hUSD)
		}
		if s.SubscriptionCount != 1 {
			t.Errorf("SubscriptionCount = %d, want 1 (dupe excluded)", s.SubscriptionCount)
		}
	})
}

// TestBuildCostsSummary_BillWindowXCheck pins the measure-twice containment
// check: 5h window ⊆ 7d window ⇒ bill-5h ≤ bill-7d, else a counted violation
// (ka-0000003343, advice ka-0000005924).
func TestBuildCostsSummary_BillWindowXCheck(t *testing.T) {
	now := time.Date(2026, 4, 25, 12, 0, 0, 0, time.UTC)
	accts := []costAccountView{
		// healthy: 5h ≤ 7d
		{BillingType: accountBillingSubscription, Bill5hHas: true, Bill5hUSD: 2.0, Bill7dHas: true, Bill7dUSD: 20.0},
		// violation: 5h > 7d (stale 7d reset)
		{BillingType: accountBillingSubscription, Bill5hHas: true, Bill5hUSD: 9.0, Bill7dHas: true, Bill7dUSD: 3.0},
		// only 5h present → not checkable, not counted
		{BillingType: accountBillingSubscription, Bill5hHas: true, Bill5hUSD: 5.0},
		// within tolerance → not a violation
		{BillingType: accountBillingSubscription, Bill5hHas: true, Bill5hUSD: 4.005, Bill7dHas: true, Bill7dUSD: 4.0},
	}
	s := buildCostsSummary(accts, now)
	if s.BillXCheckAccounts != 3 {
		t.Errorf("BillXCheckAccounts = %d, want 3 (both-present accounts)", s.BillXCheckAccounts)
	}
	if s.BillXCheckViolations != 1 {
		t.Errorf("BillXCheckViolations = %d, want 1 (the 9>3 account)", s.BillXCheckViolations)
	}
}

// TestHandleCosts_SubscriptionHeadline is the end-to-end render guard for the
// subscription headline (ka-0000003343): an account carrying the reset-aligned
// window bills must show "bill vs 5h limit" as the headline and demote the
// calendar-day figure to an explicit "UTC day" line — never the old "Today"
// 2rem number masquerading as the bill.
func TestHandleCosts_SubscriptionHeadline(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-sub-1",
				Title:  "anthropic-prod-01",
				Type:   "account",
				Status: "open",
				Fields: map[string]string{
					"provider":             "anthropic",
					"status":               "active",
					"utilization_5h":       "0.42",
					"utilization_7d":       "0.18",
					"representative_claim": "five_hour",
					"throttle_reset_5h_at": now.Add(3 * time.Hour).Format(time.RFC3339),
					"throttle_reset_7d_at": now.Add(5 * 24 * time.Hour).Format(time.RFC3339),
					"daily_cost_usd":       "12.00",
					"weekly_cost_usd":      "78.00",
					"total_cost_usd":       "234.00",
					"spend_5h_window_usd":  "3.50",
					"spend_7d_window_usd":  "44.00",
				},
			},
		},
		Total: 1,
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"bill vs 5h limit",     // primary headline label (five_hour binds)
		"$3.50",                // the 5h-window bill as the headline number
		"bill vs week (7d limit)", // secondary label
		"$44.00",               // 7d-window bill
		"UTC day",              // demoted calendar-day line
		"$12.00",               // calendar-day figure still shown (demoted)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("subscription headline missing %q", want)
		}
	}
	// The calendar-day figure must NOT be the sole 2rem headline anymore: the
	// primary total is the 5h bill.
	if strings.Contains(body, `costs-summary-total">$12.00`) {
		t.Errorf("calendar-day figure is still the 2rem headline — should be demoted")
	}
}
