package server

import (
	"fmt"
	"strconv"
)

// charts_grafana_panel.go — the deterministic catalog→Grafana generator.
//
// Epic ka-0000002763 (decision ka-0000007963, option `generate-from-catalog`):
// make the kbeads chart catalog the single source of truth and GENERATE the
// Grafana panel model from each ChartDef, killing the hand-sync between
// charts_catalog.go and the Grafana dashboard bead. A one-line catalog edit
// then propagates to the live Grafana panel automatically.
//
// This file is the head of that chain (task ka-0000000732): it maps ONE
// ChartDef to ONE Grafana timeseries panel. A later step (ka-0000006749) wraps
// N panels into a dashboard model keyed by uid and adds the drift guard that
// asserts generator(catalog) == committed model.
//
// Mapping (source of truth = ChartDef):
//   - QueryTemplate rendered with DefaultParams → target refId A; each
//     ExtraSeries.QueryTemplate → refId B, C, …. PromQL passes through VERBATIM
//     (same vmsingle query string the catalog runs), so the Grafana panel and
//     the inline ECharts/headless render hit identical queries — query parity
//     by construction.
//   - YAxisIndex → left/right axis: a series on axis 1 gets a byName field
//     override placing it on the right axis (matching the ECharts secondary-axis
//     binding, e.g. active-agents on the right).
//   - YFormat (rate_per_hour|currency_usd|count) → Grafana unit.
//   - YTitle → axis label; PrimarySeriesName / ExtraSeries.Name → legend names.
//   - Mark (line|area|bar) → drawStyle / fill / stacking.
//   - Title → panel title; Subtitle (rendered) → panel description.
//
// The generator is pure and deterministic: no clock, no network, no map
// iteration order leaks into the output (ExtraSeries is an ordered slice).

// grafanaPromDatasourceUID is the dashboard-variable reference every generated
// target/panel points at. It matches the committed dashboards
// (gardenio/helm/gardenio/dashboards/*.json), whose `DS_PROMETHEUS` templating
// variable selects the vmsingle prometheus datasource at view time.
const grafanaPromDatasourceUID = "${DS_PROMETHEUS}"

// grafanaPanelStep is the PromQL step the generator substitutes for the
// catalog's {{.Step}} placeholder. In the HTTP path Step is derived from the
// request's time range; the Grafana equivalent is the `$__interval` macro,
// which Grafana resolves per panel from the dashboard time range and panel
// width. Using the macro keeps the range-vector window adaptive exactly as the
// catalog's request-derived step is, preserving query shape parity.
const grafanaPanelStep = "$__interval"

// GrafanaDatasource is the {type,uid} datasource reference Grafana panels and
// targets carry.
type GrafanaDatasource struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

// GrafanaGridPos is a panel's position/size on the dashboard grid. The
// single-panel generator emits a default full-height half-width tile; the
// dashboard-wrapping step (ka-0000006749) repositions panels as it lays out the
// grid.
type GrafanaGridPos struct {
	H int `json:"h"`
	W int `json:"w"`
	X int `json:"x"`
	Y int `json:"y"`
}

// GrafanaTarget is one query (refId) on a panel. Expr is verbatim PromQL.
type GrafanaTarget struct {
	Expr         string `json:"expr"`
	LegendFormat string `json:"legendFormat"`
	RefID        string `json:"refId"`
}

// GrafanaStacking is the timeseries custom.stacking block. Mode "normal"
// produces a stacked (area) chart; "none" leaves series un-stacked.
type GrafanaStacking struct {
	Mode string `json:"mode"`
}

// GrafanaFieldCustom is the timeseries-specific custom field config: draw
// style, fill, line width, axis placement/label, and stacking.
type GrafanaFieldCustom struct {
	DrawStyle     string           `json:"drawStyle"`
	LineWidth     int              `json:"lineWidth"`
	FillOpacity   int              `json:"fillOpacity"`
	AxisPlacement string           `json:"axisPlacement,omitempty"`
	AxisLabel     string           `json:"axisLabel,omitempty"`
	Stacking      *GrafanaStacking `json:"stacking,omitempty"`
}

// GrafanaFieldDefaults is fieldConfig.defaults: the panel-wide unit/label that
// the primary (left-axis) series inherits.
type GrafanaFieldDefaults struct {
	Unit     string             `json:"unit,omitempty"`
	Decimals *int               `json:"decimals,omitempty"`
	Custom   GrafanaFieldCustom `json:"custom"`
}

// GrafanaMatcher selects fields an override applies to. The generator only uses
// byName matchers keyed on a series legend name.
type GrafanaMatcher struct {
	ID      string `json:"id"`
	Options string `json:"options"`
}

// GrafanaOverrideProperty is one {id,value} property set inside an override.
type GrafanaOverrideProperty struct {
	ID    string `json:"id"`
	Value any    `json:"value"`
}

// GrafanaFieldOverride is one fieldConfig.overrides entry: a matcher plus the
// properties to set on the matched field(s). Used to bind a secondary-axis
// series (YAxisIndex 1) to the right axis with its own unit/label.
type GrafanaFieldOverride struct {
	Matcher    GrafanaMatcher            `json:"matcher"`
	Properties []GrafanaOverrideProperty `json:"properties"`
}

// GrafanaFieldConfig is the panel fieldConfig: shared defaults + per-series
// overrides.
type GrafanaFieldConfig struct {
	Defaults  GrafanaFieldDefaults   `json:"defaults"`
	Overrides []GrafanaFieldOverride `json:"overrides"`
}

// GrafanaLegendOptions / GrafanaTooltipOptions / GrafanaPanelOptions mirror the
// committed timeseries panels' options block.
type GrafanaLegendOptions struct {
	DisplayMode string `json:"displayMode"`
	Placement   string `json:"placement"`
}

type GrafanaTooltipOptions struct {
	Mode string `json:"mode"`
	Sort string `json:"sort,omitempty"`
}

type GrafanaPanelOptions struct {
	Legend  GrafanaLegendOptions  `json:"legend"`
	Tooltip GrafanaTooltipOptions `json:"tooltip"`
}

// GrafanaPanel is one generated Grafana timeseries panel. It marshals to the
// same JSON shape as the panels in gardenio/helm/gardenio/dashboards/*.json, so
// grafana-bridge syncs it unchanged.
type GrafanaPanel struct {
	ID          int                 `json:"id,omitempty"`
	Type        string              `json:"type"`
	Title       string              `json:"title"`
	Description string              `json:"description,omitempty"`
	Datasource  GrafanaDatasource   `json:"datasource"`
	GridPos     GrafanaGridPos      `json:"gridPos"`
	FieldConfig GrafanaFieldConfig  `json:"fieldConfig"`
	Options     GrafanaPanelOptions `json:"options"`
	Targets     []GrafanaTarget     `json:"targets"`
}

// grafanaUnit maps a catalog YFormat to a Grafana unit string. The spend rate
// (USD/hour) reuses currencyUSD to match the committed "Cost Rate (USD/hour)"
// panel — the magnitude is carried by the YTitle axis label, as Grafana has no
// first-class "$/h" unit. The perf-RED formats (duration_seconds → "s",
// ratio → "percentunit") map to Grafana's auto-scaling units. Unknown/empty
// formats fall back to "short".
func grafanaUnit(yformat string) string {
	switch yformat {
	case "currency_usd", "rate_per_hour":
		return "currencyUSD"
	case "count":
		return "none"
	case "duration_seconds", "duration_s":
		// Latency panels (perf RED) carry seconds; Grafana's "s" unit
		// auto-scales to ms/µs so a 9.5ms p95 reads as "9.5 ms", not
		// "0.0095" (C3 unit-scale honesty).
		return "s"
	case "duration_ms":
		// Span-derived latency panels carry MILLISECONDS
		// (span_duration_milliseconds_*, W2b operation RED ka-0000001551).
		// Grafana's "ms" unit auto-scales to s/µs so a 6300ms p99 reads as
		// "6.3 s", a 4ms one as "4 ms" (C3 unit-scale honesty).
		return "ms"
	case "ratio":
		// Error-ratio panel is a 0..1 fraction; percentunit renders it as a
		// percentage with the axis carrying the scale.
		return "percentunit"
	default:
		return "short"
	}
}

// grafanaDecimals returns the fixed decimals for a unit, or nil to leave it
// auto. Currency reads cleaner pinned to 2 (matching the committed panels).
func grafanaDecimals(unit string) *int {
	if unit == "currencyUSD" {
		two := 2
		return &two
	}
	return nil
}

// markCustom derives the timeseries custom block (draw style, fill, stacking)
// from the catalog Mark. "area" is a STACKED area (every catalog area chart is
// IOSKind area-stacked) — Grafana draws this as a line with high fill opacity
// and stacking=normal, matching the committed "Token Usage Over Time" panel.
// "bar" is a STACKED bar (every catalog bar chart is IOSKind bar-stacked:
// spend-by-agent, agent-cost-per-bead) — drawStyle "bars" + stacking=normal +
// fillOpacity 80, matching the hand-authored "Spend per agent" panel. Anything
// else (incl. "line") is a plain line.
func markCustom(mark string) GrafanaFieldCustom {
	switch mark {
	case "area":
		return GrafanaFieldCustom{
			DrawStyle:   "line",
			LineWidth:   1,
			FillOpacity: 30,
			Stacking:    &GrafanaStacking{Mode: "normal"},
		}
	case "bar":
		return GrafanaFieldCustom{
			DrawStyle:   "bars",
			LineWidth:   1,
			FillOpacity: 80,
			Stacking:    &GrafanaStacking{Mode: "normal"},
		}
	default: // "line"
		return GrafanaFieldCustom{
			DrawStyle:   "line",
			LineWidth:   2,
			FillOpacity: 10,
		}
	}
}

// refID returns the Grafana target refId for the i-th series: A, B, C, ….
func refID(i int) string {
	return string(rune('A' + i))
}

// defaultWindowFor resolves the window the generator renders templates with:
// the chart's DefaultParams["window"] when present, else "1d". Mirrors the HTTP
// path's window defaulting (parseChartRequest), so the rendered PromQL matches.
func defaultWindowFor(def ChartDef) string {
	if w, ok := def.DefaultParams["window"].(string); ok && w != "" {
		return w
	}
	return "1d"
}

// defaultTopNStr resolves DefaultParams["top_n"] to a string for the Subtitle's
// {{.TopN}} placeholder; empty when the chart has no top_n default.
func defaultTopNStr(def ChartDef) string {
	switch v := def.DefaultParams["top_n"].(type) {
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case string:
		return v
	default:
		return ""
	}
}

// defaultRollupFor resolves the roll-up dimension the generator renders the
// query template with: DefaultParams["rollup"] when present, else the chart's
// SeriesLabel (so a non-rollup chart leaves {{.Rollup}} as its fixed series
// label and nothing else needs to change). Mirrors the HTTP path's rollup
// defaulting in parseChartRequest, so the generated PromQL matches the served
// default view.
func defaultRollupFor(def ChartDef) string {
	if r, ok := def.DefaultParams["rollup"].(string); ok && r != "" {
		return r
	}
	return def.SeriesLabel
}

// primaryLegend is the legend label for the primary target. A grouped query
// (SeriesField set) returns one series per label value, so the legend is the
// Grafana label-interpolation template `{{ <label> }}`; a single-aggregate
// primary uses PrimarySeriesName, falling back to the chart Title.
func primaryLegend(def ChartDef) string {
	if def.SeriesField != "" {
		label := def.SeriesLabel
		if label == "" {
			label = def.SeriesField
		}
		return fmt.Sprintf("{{ %s }}", label)
	}
	if def.PrimarySeriesName != "" {
		return def.PrimarySeriesName
	}
	return def.Title
}

// GeneratePanel deterministically maps one catalog ChartDef to one Grafana
// timeseries panel. It renders QueryTemplate (and each ExtraSeries template)
// against the chart's default params, assigns refIds A,B,C…, maps YFormat→unit,
// YTitle→axis label, Mark→draw style/fill, and emits a right-axis field
// override for every secondary-axis (YAxisIndex 1) series. The returned panel's
// gridPos/ID are placeholders the dashboard-wrapping step overrides.
func GeneratePanel(def ChartDef) (*GrafanaPanel, error) {
	window := defaultWindowFor(def)
	qctx := queryContext{
		Selector:    buildSelector(def, nil),
		Step:        grafanaPanelStep,
		Window:      window,
		WindowHours: windowHours(window),
		TopN:        defaultTopNStr(def),
		Rollup:      defaultRollupFor(def),
	}

	// Primary target → refId A. PromQL is rendered verbatim.
	primaryExpr, err := renderTemplate(def.QueryTemplate, qctx)
	if err != nil {
		return nil, fmt.Errorf("render primary query for %s: %w", def.ID, err)
	}

	// Top-N cap for the Grafana surface (ka-0000002132). A RollupOther chart is
	// capped to its top_n highest series on the HTTP/iOS path by
	// topNSeriesWithOther — but the Grafana panel has NO post-query hook, so
	// GeneratePanel would otherwise plot the FULL grouped result. For the
	// per-agent spend chart that is hundreds of per-agent series stacked into
	// illegible spaghetti with an off-panel legend (the bug). Bake the SAME cap
	// the HTTP path applies into the PromQL as `topk(N, <expr>)` so the panel is
	// legible on every surface. topk ranks per step; the primary series are
	// cumulative real-bill LEVELS, so the kept set is stable across the range
	// rather than flickering. Low-cardinality rollups (account/model/role) hold
	// fewer than N series so topk is a no-op there — it only bites the
	// high-cardinality agent rollup, exactly where the cardinality lives. The
	// honest fleet TOTAL is not lost: it is read off the adjacent
	// "Fleet spend rate" panel, which sums the whole bill (the per-agent panel
	// is a top-spenders breakdown, not a reconciliation surface).
	if def.RollupOther && def.SeriesField != "" {
		if topN := defaultTopNStr(def); topN != "" {
			primaryExpr = fmt.Sprintf("topk(%s, %s)", topN, primaryExpr)
		}
	}

	targets := []GrafanaTarget{{
		Expr:         primaryExpr,
		LegendFormat: primaryLegend(def),
		RefID:        refID(0),
	}}

	// Extra series → refId B, C, …. Each renders against the same context with
	// its own AllowedLabels selector (so an overlay scopes to the labels its
	// metric actually carries — the active-agent gauge has no `agent` label).
	var overrides []GrafanaFieldOverride
	for i, ex := range def.ExtraSeries {
		exCtx := qctx
		exCtx.Selector = buildSelectorFiltered(def, nil, ex.AllowedLabels)
		expr, err := renderTemplate(ex.QueryTemplate, exCtx)
		if err != nil {
			return nil, fmt.Errorf("render extra series %q for %s: %w", ex.Name, def.ID, err)
		}
		targets = append(targets, GrafanaTarget{
			Expr:         expr,
			LegendFormat: ex.Name,
			RefID:        refID(i + 1),
		})

		// YAxisIndex 1 → right axis. Bind by the series' legend name and carry
		// its own unit + axis label so the secondary axis reads correctly
		// (e.g. active-agents: count/none on the right, labelled "agents").
		if ex.YAxisIndex == 1 {
			props := []GrafanaOverrideProperty{
				{ID: "custom.axisPlacement", Value: "right"},
				{ID: "unit", Value: grafanaUnit(ex.YFormat)},
			}
			if ex.YTitle != "" {
				props = append(props, GrafanaOverrideProperty{ID: "custom.axisLabel", Value: ex.YTitle})
			}
			overrides = append(overrides, GrafanaFieldOverride{
				Matcher:    GrafanaMatcher{ID: "byName", Options: ex.Name},
				Properties: props,
			})
		}
	}

	// Subtitle → description (rendered for {{.Step}}/{{.Window}}/{{.TopN}}). A
	// render error degrades to an empty description rather than failing the
	// whole panel — the description is cosmetic.
	description, _ := renderTemplate(def.Subtitle, qctx)

	unit := grafanaUnit(def.YFormat)
	custom := markCustom(def.Mark)
	custom.AxisPlacement = "left"
	custom.AxisLabel = def.YTitle

	return &GrafanaPanel{
		Type:        "timeseries",
		Title:       def.Title,
		Description: description,
		Datasource:  GrafanaDatasource{Type: "prometheus", UID: grafanaPromDatasourceUID},
		GridPos:     GrafanaGridPos{H: 8, W: 12, X: 0, Y: 0},
		FieldConfig: GrafanaFieldConfig{
			Defaults: GrafanaFieldDefaults{
				Unit:     unit,
				Decimals: grafanaDecimals(unit),
				Custom:   custom,
			},
			Overrides: overrides,
		},
		Options: GrafanaPanelOptions{
			Legend:  GrafanaLegendOptions{DisplayMode: "list", Placement: "bottom"},
			Tooltip: GrafanaTooltipOptions{Mode: "multi", Sort: "desc"},
		},
		Targets: targets,
	}, nil
}
