package server

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestAssembleDashboard_CostsCandidate covers the dashboard→catalog-id mapping
// and the assembled chrome for gardenio-costs-candidate = [fleet-spend-rate,
// agent-spend-over-time, spend-by-account, spend-by-model] (matching dashboard
// bead ka-0000005371). It pins the uid, title, graphTooltip, panel count/order,
// per-panel ids + grid layout, and the templating/time chrome.
func TestAssembleDashboard_CostsCandidate(t *testing.T) {
	d, err := AssembleDashboardByUID("gardenio-costs-candidate")
	if err != nil {
		t.Fatalf("AssembleDashboardByUID: %v", err)
	}

	if d.UID != "gardenio-costs-candidate" {
		t.Errorf("uid = %q, want gardenio-costs-candidate", d.UID)
	}
	if want := "Gardenio /costs candidate — Fleet spend rate + Per-agent cost"; d.Title != want {
		t.Errorf("title = %q, want %q", d.Title, want)
	}
	// graphTooltip=1 → shared crosshair, preserved from the committed model.
	if d.GraphTooltip != 1 {
		t.Errorf("graphTooltip = %d, want 1", d.GraphTooltip)
	}
	if d.SchemaVersion != grafanaSchemaVersion {
		t.Errorf("schemaVersion = %d, want %d", d.SchemaVersion, grafanaSchemaVersion)
	}
	if d.Refresh != "30s" {
		t.Errorf("refresh = %q, want 30s", d.Refresh)
	}
	if d.Time.From != "now-24h" || d.Time.To != "now" {
		t.Errorf("time = %+v, want now-24h..now", d.Time)
	}

	// Five panels, in catalog-id order: cost-per-agent-hour LEADS (the headline
	// unit economic, ka-0000006587), then fleet-spend-rate, agent-spend-over-time,
	// spend-by-account (the per-account breakdown, ka-0000004020), then
	// spend-by-model (the per-model 1h-rate breakdown, ka-0000003821).
	if len(d.Panels) != 5 {
		t.Fatalf("len(panels) = %d, want 5", len(d.Panels))
	}
	if d.Panels[0].Title != "Cost per agent-hour" {
		t.Errorf("panel[0] title = %q, want Cost per agent-hour", d.Panels[0].Title)
	}
	if d.Panels[1].Title != "Fleet spend rate" {
		t.Errorf("panel[1] title = %q, want Fleet spend rate", d.Panels[1].Title)
	}
	if d.Panels[2].Title != "Per-agent cost over time" {
		t.Errorf("panel[2] title = %q, want Per-agent cost over time", d.Panels[2].Title)
	}
	if d.Panels[3].Title != "Spend by account over time" {
		t.Errorf("panel[3] title = %q, want Spend by account over time", d.Panels[3].Title)
	}
	if d.Panels[4].Title != "Spend by model over time" {
		t.Errorf("panel[4] title = %q, want Spend by model over time", d.Panels[4].Title)
	}

	// Panel ids are 1-based and sequential. These ids are POSITIONAL, and the
	// /costs page embeds them by number (costsGrafanaPanels in
	// gardenio/controller/cmd/beads-viewer/costs.go) — so reordering CatalogIDs
	// renumbers the embeds and both sides must move together.
	for i, p := range d.Panels {
		if p.ID != i+1 {
			t.Errorf("panel[%d] id = %d, want %d", i, p.ID, i+1)
		}
	}

	// Full-width column, stacked top to bottom: each panel w=24,x=0; y of panel
	// n is the sum of prior heights.
	for i, p := range d.Panels {
		if p.GridPos.W != dashboardGridWidth {
			t.Errorf("panel[%d] w = %d, want %d", i, p.GridPos.W, dashboardGridWidth)
		}
		if p.GridPos.X != 0 {
			t.Errorf("panel[%d] x = %d, want 0", i, p.GridPos.X)
		}
	}
	if d.Panels[0].GridPos.Y != 0 {
		t.Errorf("panel[0] y = %d, want 0", d.Panels[0].GridPos.Y)
	}
	if want := d.Panels[0].GridPos.H; d.Panels[1].GridPos.Y != want {
		t.Errorf("panel[1] y = %d, want %d (stacked below panel[0])", d.Panels[1].GridPos.Y, want)
	}
	if want := d.Panels[0].GridPos.H + d.Panels[1].GridPos.H; d.Panels[2].GridPos.Y != want {
		t.Errorf("panel[2] y = %d, want %d (stacked below panel[1])", d.Panels[2].GridPos.Y, want)
	}
	if want := d.Panels[0].GridPos.H + d.Panels[1].GridPos.H + d.Panels[2].GridPos.H; d.Panels[3].GridPos.Y != want {
		t.Errorf("panel[3] y = %d, want %d (stacked below panel[2])", d.Panels[3].GridPos.Y, want)
	}

	// Templating carries exactly the DS_PROMETHEUS datasource variable that
	// every generated target references.
	if len(d.Templating.List) != 1 {
		t.Fatalf("len(templating) = %d, want 1", len(d.Templating.List))
	}
	if v := d.Templating.List[0]; v.Name != "DS_PROMETHEUS" || v.Type != "datasource" || v.Query != "prometheus" {
		t.Errorf("templating var = %+v, want DS_PROMETHEUS/datasource/prometheus", v)
	}

	// annotations.list and links are empty arrays, never null.
	if d.Annotations.List == nil {
		t.Error("annotations.list is nil, want empty array")
	}
	if d.Links == nil {
		t.Error("links is nil, want empty array")
	}
}

// TestAssembleDashboard_QueryParity asserts the assembled lead panel carries
// the verbatim PromQL the catalog renders — query parity flows through the
// assembler unchanged from GeneratePanel. The fleet-spend-rate primary is
// pinned in charts_grafana_panel_test.go, so only cost-per-agent-hour is
// checked here (gd-cTYdroErdb).
func TestAssembleDashboard_QueryParity(t *testing.T) {
	d, err := AssembleDashboardByUID("gardenio-costs-candidate")
	if err != nil {
		t.Fatalf("AssembleDashboardByUID: %v", err)
	}
	// The leading panel's ratio must divide the SAME fleet 1h spend rate by the
	// OUTER-subquery agent-count average — the two halves that make it reconcile
	// with the fleet panel's "spend (1h)" and "agents (1h avg)" lines. Pinning the
	// full expr here is what stops a regression back to the per-series inner
	// avg_over_time form that read 117x wrong (ka-0000006587).
	ratio := findTarget(t, d.Panels[0], "A")
	if want := `sum(increase((max by (account, env) (gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)"}))[1h:5m])) / 1 ` +
		`/ (avg_over_time(sum(max by (project, role, state)(gardenio_agents_active{state!~"prewarmed|idle|assigning|done|failed|exited|stopped"}))[1h:5m]) > 0)`; ratio.Expr != want {
		t.Errorf("panel[0] refId A expr = %q, want %q", ratio.Expr, want)
	}
}

// TestMarshalDashboard_ByteStable asserts MarshalDashboard is deterministic:
// two marshals of the same uid produce identical bytes (the property the drift
// test ka-0000004040 relies on), the output is valid JSON, HTML is not escaped
// (PromQL/`—` pass through), and there is no trailing newline.
func TestMarshalDashboard_ByteStable(t *testing.T) {
	d1, err := AssembleDashboardByUID("gardenio-costs-candidate")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	d2, err := AssembleDashboardByUID("gardenio-costs-candidate")
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	b1, err := MarshalDashboard(d1)
	if err != nil {
		t.Fatalf("marshal d1: %v", err)
	}
	b2, err := MarshalDashboard(d2)
	if err != nil {
		t.Fatalf("marshal d2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Error("MarshalDashboard is not byte-stable across runs")
	}

	// Valid JSON.
	var sink map[string]any
	if err := json.Unmarshal(b1, &sink); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	// HTML not escaped: PromQL `!~`/quotes and the em-dash survive verbatim.
	if bytes.Contains(b1, []byte(`<`)) || bytes.Contains(b1, []byte(`&`)) {
		t.Error("output has HTML-escaped sequences; want SetEscapeHTML(false)")
	}
	if !bytes.Contains(b1, []byte("—")) {
		t.Error("em-dash in title was escaped/dropped")
	}

	// No trailing newline.
	if bytes.HasSuffix(b1, []byte("\n")) {
		t.Error("output has a trailing newline")
	}
}

// TestAssembleDashboardByUID_Unknown returns an error (not a panic) for an
// unknown uid.
func TestAssembleDashboardByUID_Unknown(t *testing.T) {
	if _, err := AssembleDashboardByUID("does-not-exist"); err == nil {
		t.Error("AssembleDashboardByUID(unknown) = nil error, want error")
	}
}

// TestAssembleDashboard_UnknownChart surfaces a clear error when a spec
// references a catalog id that is not in the chart catalog.
func TestAssembleDashboard_UnknownChart(t *testing.T) {
	spec := DashboardSpec{
		UID:        "bad",
		Title:      "bad",
		CatalogIDs: []string{"fleet-spend-rate", "no-such-chart"},
	}
	if _, err := AssembleDashboard(spec); err == nil {
		t.Error("AssembleDashboard with unknown chart = nil error, want error")
	}
}

// TestDashboardCatalog_EveryUIDAssembles is the offline half of the catalog↔live
// drift guard (ka-0000003225 / ka-0000004040). It asserts every uid the catalog
// declares assembles cleanly and marshals to byte-stable JSON — so a catalog
// edit that references an unknown chart id, or otherwise breaks assembly, reds
// CI here rather than only failing at `kd admin gen-dashboards` runtime against
// the live daemon. (The live half — asserting the live model doc matches this
// assembly — is `kd admin gen-dashboards --check`, which needs a running daemon
// and so runs as a scheduled alert / CI job, not a unit test.)
func TestDashboardCatalog_EveryUIDAssembles(t *testing.T) {
	uids := DashboardUIDs()
	if len(uids) == 0 {
		t.Fatal("DashboardUIDs() is empty — the catalog declares no dashboards")
	}
	for _, uid := range uids {
		d, err := AssembleDashboardByUID(uid)
		if err != nil {
			t.Errorf("AssembleDashboardByUID(%q): %v", uid, err)
			continue
		}
		if len(d.Panels) == 0 {
			t.Errorf("dashboard %q assembled with zero panels", uid)
		}
		// Panel ids are 1-based and dense, matching AssembleDashboard's layout.
		for i, p := range d.Panels {
			if p.ID != i+1 {
				t.Errorf("dashboard %q panel[%d] id = %d, want %d", uid, i, p.ID, i+1)
			}
		}
		if _, err := MarshalDashboard(d); err != nil {
			t.Errorf("MarshalDashboard(%q): %v", uid, err)
		}
	}
}

// TestDashboardCatalog_CostsCandidateHasSpendByModel pins the specific drift the
// bug fixed (ka-0000003225): the catalog must declare the spend-by-model panel
// for gardenio-costs-candidate so consumers embedding panelId=4 get a real panel,
// not an empty iframe. This is the offline anchor for the panel that the live
// model doc had dropped.
func TestDashboardCatalog_CostsCandidateHasSpendByModel(t *testing.T) {
	spec, ok := dashboardCatalog()["gardenio-costs-candidate"]
	if !ok {
		t.Fatal("gardenio-costs-candidate missing from dashboardCatalog()")
	}
	var hasSpendByModel bool
	for _, id := range spec.CatalogIDs {
		if id == "spend-by-model" {
			hasSpendByModel = true
		}
	}
	if !hasSpendByModel {
		t.Errorf("gardenio-costs-candidate CatalogIDs %v missing spend-by-model", spec.CatalogIDs)
	}
	if got := len(spec.CatalogIDs); got != 5 {
		t.Errorf("gardenio-costs-candidate has %d catalog ids, want 5 (spend-by-model is panel 5)", got)
	}
	// cost-per-agent-hour must LEAD: the operator asked for the unit economic at
	// the top of /costs (ka-0000006587), and panel ids are positional.
	if spec.CatalogIDs[0] != "cost-per-agent-hour" {
		t.Errorf("CatalogIDs[0] = %q, want cost-per-agent-hour to lead", spec.CatalogIDs[0])
	}
}

// TestDashboardCatalog_PerfTracesParityBoard pins the W2b parity dashboard
// (ka-0000001551): the perf-traces-red board must compose the six span-derived
// RED catalog charts — the three per-service RED panels followed by the three
// per-operation (span_name) topk drill-downs — so `:::grafana
// gardenio-perf-traces-red panel=N` can embed any of them in a bead. It assembles
// cleanly and lays every panel out with a dense 1-based id (the offline half of
// the embed contract; the live half is a dashboard bead + `kd admin
// gen-dashboards`).
//
// The uid is `gardenio-perf-traces-red`, NOT `gardenio-perf-traces` — the latter
// is the chart-shipped VictoriaTraces board (bug ka-0000002712); disjointness is
// enforced by TestDashboardUIDs_DisjointFromChartBundle below.
func TestDashboardCatalog_PerfTracesParityBoard(t *testing.T) {
	const uid = "gardenio-perf-traces-red"
	spec, ok := dashboardCatalog()[uid]
	if !ok {
		t.Fatalf("%s missing from dashboardCatalog()", uid)
	}
	want := []string{
		"perf-span-request-rate", "perf-span-error-ratio", "perf-span-latency-quantiles",
		"perf-span-top-operations", "perf-span-operation-errors", "perf-span-slowest-operations",
	}
	if len(spec.CatalogIDs) != len(want) {
		t.Fatalf("%s has %d catalog ids, want %d: %v", uid, len(spec.CatalogIDs), len(want), spec.CatalogIDs)
	}
	for i, id := range want {
		if spec.CatalogIDs[i] != id {
			t.Errorf("CatalogIDs[%d] = %q, want %q (per-service RED first, per-operation topk after)", i, spec.CatalogIDs[i], id)
		}
	}

	// Assembles to a full model with a panel per catalog id, dense 1-based ids.
	d, err := AssembleDashboardByUID(uid)
	if err != nil {
		t.Fatalf("AssembleDashboardByUID(%s): %v", uid, err)
	}
	if d.UID != uid {
		t.Errorf("assembled uid = %q, want %q", d.UID, uid)
	}
	if len(d.Panels) != len(want) {
		t.Fatalf("assembled %d panels, want %d", len(d.Panels), len(want))
	}
	for i, p := range d.Panels {
		if p.ID != i+1 {
			t.Errorf("panel[%d] id = %d, want %d", i, p.ID, i+1)
		}
	}
	// Byte-stable marshal (the property the drift guard relies on).
	if _, err := MarshalDashboard(d); err != nil {
		t.Errorf("MarshalDashboard(%s): %v", uid, err)
	}
}
