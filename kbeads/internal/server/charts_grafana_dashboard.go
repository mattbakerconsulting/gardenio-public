package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
)

// charts_grafana_dashboard.go — the deterministic dashboard ASSEMBLER.
//
// Epic ka-0000002763 (decision ka-0000007963, option `generate-from-catalog`):
// make the kbeads chart catalog the single source of truth and GENERATE the
// Grafana dashboard model from it, killing the hand-sync between
// charts_catalog.go and the committed dashboard bead.
//
// This file is the second link of that chain (task ka-0000006749). The first
// link, GeneratePanel (charts_grafana_panel.go, task ka-0000000732), maps ONE
// ChartDef to ONE Grafana timeseries panel. Here we wrap N of those generated
// panels into a full dashboard model keyed by uid: we look the chart ids up in
// the catalog, generate each panel, lay them out on the grid (assigning panel
// ids + gridPos), and surround them with the dashboard chrome (annotations,
// templating, time, tags, graphTooltip, …) that the committed model
// (dashboard-model bead ka-0000000972, dashboard bead ka-0000005371) already
// carries. A later step (ka-0000004040) adds the drift guard that asserts
// AssembleDashboardByUID(uid) == the committed model.
//
// Determinism: the assembler is pure (no clock/network/map-iteration leak —
// panels come from the ordered CatalogIDs slice and every chrome field is a
// struct, so JSON field order is canonical). MarshalDashboard emits byte-stable
// indented JSON so the future drift test is a deterministic byte comparison.

// dashboardGridWidth is the full Grafana grid width (24 columns). The assembler
// lays panels out as a single full-width column stacked top to bottom — the
// layout the committed /costs candidate uses (both panels w=24, stacked at
// y=0 and y=8).
const dashboardGridWidth = 24

// grafanaSchemaVersion is the dashboard schemaVersion the committed models
// declare. Pinned here so generated dashboards stay on the same schema as the
// hand-authored ones grafana-bridge already syncs.
const grafanaSchemaVersion = 39

// GrafanaAnnotations is the dashboard `annotations` block. The generated
// dashboards carry no built-in annotation queries, so List is always the empty
// list (never null — Grafana expects an array).
type GrafanaAnnotations struct {
	List []any `json:"list"`
}

// GrafanaTemplateVar is one dashboard templating variable. The only variable
// the generated dashboards declare is the DS_PROMETHEUS datasource picker that
// every generated target/panel references via ${DS_PROMETHEUS}
// (grafanaPromDatasourceUID).
type GrafanaTemplateVar struct {
	Current    struct{} `json:"current"`
	Hide       int      `json:"hide"`
	IncludeAll bool     `json:"includeAll"`
	Name       string   `json:"name"`
	Query      string   `json:"query"`
	Type       string   `json:"type"`
}

// GrafanaTemplating is the dashboard `templating` block.
type GrafanaTemplating struct {
	List []GrafanaTemplateVar `json:"list"`
}

// GrafanaTime is the dashboard default time range (`time` block).
type GrafanaTime struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GrafanaDashboard is a full Grafana dashboard model. Field order matches the
// committed dashboards' canonical key order so MarshalDashboard's output reads
// identically to the hand-authored JSON and stays byte-stable across runs.
type GrafanaDashboard struct {
	Annotations          GrafanaAnnotations `json:"annotations"`
	Editable             bool               `json:"editable"`
	FiscalYearStartMonth int                `json:"fiscalYearStartMonth"`
	GraphTooltip         int                `json:"graphTooltip"`
	Links                []any              `json:"links"`
	Panels               []*GrafanaPanel    `json:"panels"`
	Refresh              string             `json:"refresh"`
	SchemaVersion        int                `json:"schemaVersion"`
	Tags                 []string           `json:"tags"`
	Templating           GrafanaTemplating  `json:"templating"`
	Time                 GrafanaTime        `json:"time"`
	Timepicker           struct{}           `json:"timepicker"`
	Timezone             string             `json:"timezone"`
	Title                string             `json:"title"`
	UID                  string             `json:"uid"`
	Version              int                `json:"version"`
}

// DashboardSpec declares one assembled dashboard: its uid + non-panel chrome
// (title, tags, default time range, refresh) plus the ORDERED list of catalog
// chart ids whose generated panels fill it. This is the dashboard→catalog-id
// mapping — the single place that says "gardenio-costs-candidate is
// [fleet-spend-rate, agent-spend-over-time]" (matching dashboard bead
// ka-0000005371). Panels render in CatalogIDs order, top to bottom.
type DashboardSpec struct {
	UID        string
	Title      string
	Tags       []string
	Refresh    string
	Time       GrafanaTime
	CatalogIDs []string
}

// dashboardCatalog is the registry of generated dashboards, keyed by uid. It is
// the dashboard-level counterpart to chartCatalog(): the catalog of charts is
// the source of truth for panel content, and this is the source of truth for
// which charts compose which dashboard and the chrome around them.
//
// Ownership (authoritative — stated here once; ka-0000003225). Every uid in this
// map is CATALOG-GENERATED and BRIDGE-PROVISIONED, and is NOT chart-bundle-owned:
//   - catalog-generated: the live grafana:dashboard-model doc is (re)written from
//     this catalog by `kd admin gen-dashboards`, so a catalog edit here is the
//     source of truth — but only propagates after that command runs;
//   - bridge-provisioned: the grafana-bridge renders the model doc into a
//     bridge-managed grafana_dashboard ConfigMap (managed-by=grafana-bridge);
//   - NOT chart-bundle-owned: no JSON for these uids is baked into the Helm chart
//     bundle ConfigMap (helm/gardenio/dashboards/), so ChartOwnsDashboard is false.
//     That is exactly what grafana_configmap_test.go asserts for
//     gardenio-costs-candidate ("bead-only, no chart bundle CM"); "bead-only" there
//     means "not chart-owned", which is consistent with catalog-generated — the
//     two descriptions are the same fact, not a contradiction.
//
// Because catalog-generated propagation is manual (gen-dashboards), the live doc
// can silently diverge from this catalog (a panel added here but never
// regenerated — the gardenio-costs-candidate drift, ka-0000003225). The
// `gen-dashboards --check` drift guard exists to catch that; keep it wired as a
// scheduled alert / CI job.
func dashboardCatalog() map[string]DashboardSpec {
	specs := []DashboardSpec{
		{
			// /costs candidate — the embedded cost panels on /costs, generated
			// from their catalog ids. Preserves the committed uid + chrome
			// (ka-0000005371 / ka-0000000972): graphTooltip=1 (shared crosshair),
			// 30s refresh, now-24h range, the costs-candidate tag set. Panel 3
			// (spend-by-account) is the per-account spend breakdown (ka-0000004020),
			// stacked below the fleet-rate + per-agent panels. Panel 4
			// (spend-by-model) is the per-model 1h-rate breakdown
			// (ka-0000003821, haiku vs opus), stacked below them.
			UID:     "gardenio-costs-candidate",
			Title:   "Gardenio /costs candidate — Fleet spend rate + Per-agent cost",
			Tags:    []string{"gardenio", "cost", "agents", "costs-candidate"},
			Refresh: "30s",
			Time:    GrafanaTime{From: "now-24h", To: "now"},
			// cost-per-agent-hour leads (ka-0000006587, operator request): the unit
			// economic ($/hour/agent) is the headline number, so it sits ABOVE the
			// absolute fleet rate. Panel ids are positional (AssembleDashboard assigns
			// i+1), so inserting at the front SHIFTS every later panel's id by one —
			// the /costs page's iframe panel ids (costsGrafanaPanels in
			// gardenio/controller/cmd/beads-viewer/costs.go) are updated to match in the
			// same change. Keep the two in lockstep or the embeds render the wrong panel.
			CatalogIDs: []string{"cost-per-agent-hour", "fleet-spend-rate", "agent-spend-over-time", "spend-by-account", "spend-by-model"},
		},
		{
			// perf-traces-red — W2b parity board (ka-0000001551, epic ka-0000000756).
			// Composes the span-derived RED catalog charts into ONE generated
			// Grafana dashboard so the beads-embed primitive (`:::grafana
			// gardenio-perf-traces-red panel=N`) can drop any of them into a bead
			// description/comment/bundle. This is the "beads-embedded graphs"
			// half of W2b: the ACME distributed-traces dashboard's best features
			// (W2a inventory ka-0000003866), ported as catalog panels rather than
			// a standalone hand-authored Grafana board.
			//
			// UID is `gardenio-perf-traces-red`, NOT `gardenio-perf-traces` (bug
			// ka-0000002712). `gardenio-perf-traces` is already owned by the
			// chart-shipped, chart-bundle-baked JSON at
			// gardenio/helm/gardenio/dashboards/perf-traces.json (the Jaeger
			// trace-search + VM RED board, title "Gardenio — perf traces
			// (VictoriaTraces)"), which the grafana-bridge backfills as a
			// type=dashboard bead and whose ownership the bridge tests assert
			// (ChartOwnsDashboard=true, grafana_configmap_test.go /
			// grafana_sync_test.go). Every uid in THIS catalog is NOT
			// chart-bundle-owned (the ownership contract stated in this file's
			// header) — so this board must not reuse that uid, or the two
			// provisioning paths fight over one Grafana dashboard:
			// `gen-dashboards` would overwrite the trace-search board's model doc
			// with these six timeseries panels, and GrafanaSync would RemoveOwn
			// its shadow. The `-red` suffix keeps both boards. The disjointness
			// invariant is pinned by TestDashboardUIDs_DisjointFromChartBundle.
			//
			// Panel order (positional ids assigned by AssembleDashboard): the
			// per-SERVICE RED strip first (the overview an operator reads first —
			// which service is hot), then the per-OPERATION topk drill-downs
			// (which endpoint within it). Every panel is source="perf"-pinned by
			// its catalog query, so this board never mixes dev/prod spans.
			//
			// Live embed provisioning: assembling this spec is offline-tested
			// (TestDashboardCatalog_EveryUIDAssembles); a live `:::grafana`
			// embed additionally needs a type=dashboard bead carrying
			// dashboard_uid=gardenio-perf-traces-red plus a `kd admin gen-dashboards`
			// run to write+sync its model doc — the same operational step every
			// catalog-generated board takes (charts_grafana_dashboard.go header).
			UID:     "gardenio-perf-traces-red",
			Title:   "Gardenio perf traces — span-derived RED (service + operation)",
			Tags:    []string{"gardenio", "perf", "traces", "red", "spanmetrics"},
			Refresh: "30s",
			Time:    GrafanaTime{From: "now-6h", To: "now"},
			CatalogIDs: []string{
				// per-service RED overview
				"perf-span-request-rate", "perf-span-error-ratio", "perf-span-latency-quantiles",
				// per-operation (span_name) topk drill-downs
				"perf-span-top-operations", "perf-span-operation-errors", "perf-span-slowest-operations",
			},
		},
	}
	out := make(map[string]DashboardSpec, len(specs))
	for _, s := range specs {
		out[s.UID] = s
	}
	return out
}

// AssembleDashboard wraps the generated panels for spec.CatalogIDs into a full
// dashboard model. Each catalog id is looked up in chartCatalog(), turned into
// a panel by GeneratePanel, assigned a 1-based panel id, and laid out as a
// full-width tile stacked below the previous one (preserving each panel's
// generated height). The panels are then surrounded by the dashboard chrome
// from spec. Returns an error if spec references an unknown chart id or a panel
// fails to generate. Pure and deterministic.
func AssembleDashboard(spec DashboardSpec) (*GrafanaDashboard, error) {
	catalog := chartCatalog()
	panels := make([]*GrafanaPanel, 0, len(spec.CatalogIDs))
	y := 0
	for i, id := range spec.CatalogIDs {
		def, ok := catalog[id]
		if !ok {
			return nil, fmt.Errorf("dashboard %q references unknown catalog chart %q", spec.UID, id)
		}
		p, err := GeneratePanel(def)
		if err != nil {
			return nil, fmt.Errorf("generate panel %q for dashboard %q: %w", id, spec.UID, err)
		}
		p.ID = i + 1
		// Single full-width column, stacked top to bottom. Keep the generated
		// height; override width/origin so the grid is deterministic regardless
		// of the per-panel placeholder GeneratePanel emits.
		p.GridPos = GrafanaGridPos{H: p.GridPos.H, W: dashboardGridWidth, X: 0, Y: y}
		y += p.GridPos.H
		panels = append(panels, p)
	}

	return &GrafanaDashboard{
		Annotations:          GrafanaAnnotations{List: []any{}},
		Editable:             true,
		FiscalYearStartMonth: 0,
		GraphTooltip:         1,
		Links:                []any{},
		Panels:               panels,
		Refresh:              spec.Refresh,
		SchemaVersion:        grafanaSchemaVersion,
		Tags:                 spec.Tags,
		Templating: GrafanaTemplating{List: []GrafanaTemplateVar{{
			Hide:       0,
			IncludeAll: false,
			Name:       "DS_PROMETHEUS",
			Query:      "prometheus",
			Type:       "datasource",
		}}},
		Time:     spec.Time,
		Timezone: "",
		Title:    spec.Title,
		UID:      spec.UID,
		Version:  1,
	}, nil
}

// DashboardUIDs returns the uids of every dashboard the catalog generates,
// sorted for deterministic iteration. It is the exported entry point the
// live-sync wiring (task ka-0000005728, `kd admin gen-dashboards`) walks to
// regenerate every catalog-backed dashboard model from a single command.
func DashboardUIDs() []string {
	cat := dashboardCatalog()
	uids := make([]string, 0, len(cat))
	for uid := range cat {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	return uids
}

// AssembleDashboardByUID looks up the dashboard spec for uid in
// dashboardCatalog() and assembles it. Returns an error for an unknown uid.
func AssembleDashboardByUID(uid string) (*GrafanaDashboard, error) {
	spec, ok := dashboardCatalog()[uid]
	if !ok {
		return nil, fmt.Errorf("no dashboard with uid %q", uid)
	}
	return AssembleDashboard(spec)
}

// MarshalDashboard renders a dashboard to byte-stable, 2-space-indented JSON.
// HTML escaping is disabled so PromQL operators and the em-dash in titles pass
// through verbatim; struct field order is canonical, so the output is
// deterministic and suitable for the drift test's byte comparison (ka-0000004040).
// No trailing newline (json.Encoder appends one; we trim it).
func MarshalDashboard(d *GrafanaDashboard) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, fmt.Errorf("marshal dashboard %q: %w", d.UID, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
