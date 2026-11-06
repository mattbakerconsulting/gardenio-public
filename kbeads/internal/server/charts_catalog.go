package server

import (
	"bytes"
	_ "embed"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	"go.yaml.in/yaml/v3"
)

// charts_catalog.go — the E5 chart catalog: types + the in-process registry of
// named charts, plus the PromQL/Vega-Lite assembly helpers the HTTP handlers
// (charts_catalog_http.go) consume.
//
// Plan-of-record: gardenio/plans/timeseries-platform.md §2.E5.
// Architecture:    gardenio/docs/research/timeseries-chart-backend-arch.md (R5).
//
// This catalog is also the SINGLE SOURCE OF TRUTH for the catalog-generated
// Grafana dashboards (e.g. /costs `gardenio-costs-candidate`): a ChartDef here is
// generated into the live Grafana model, so do NOT hand-edit that model in
// lockstep. Edit the ChartDef, regenerate the golden
// (`GB_UPDATE_GOLDEN=1 go test ./internal/server/ -run TestDashboardModel_Golden`),
// commit both, and `kd admin gen-dashboards` writes it to the live doc. Full
// workflow + drift guards: gardenio/docs/dashboard-sync.md
// ("Catalog-generated dashboards"). Epic ka-0000002763, decision ka-0000007963.
// Decision D1 (ka-0000001695): hybrid contract — `data` is generic long-format
// series-of-points (both web and iOS consume it directly); `spec` is a
// Vega-Lite v5 document the web renders and the server uses for the PNG/SVG
// fallback (E5.4). iOS reads `data` directly guided by `ios_hint`, falling back
// to PNG when a chart shape falls outside its native subset.
//
// The catalog is a small, repo-checked set of charts. The entries live in
// charts_catalog.yaml, embedded at build time and decoded strictly into ChartDef
// by chartCatalog() (gd-Wl6LuCnvlH; v1 held them as Go struct literals). The
// shared PromQL fragments stay Go constants below, which the YAML references
// as ${name}. TestChartCatalog_Golden pins every loaded field.

// ParamSpec describes one typed, caller-supplied parameter a chart accepts. It
// is surfaced verbatim in the catalog listing so a web/iOS client can build
// pickers without hard-coding each chart's params.
type ParamSpec struct {
	Type     string   `json:"type" yaml:"type"` // "string" | "string[]" | "enum" | "int"
	Required bool     `json:"required,omitempty" yaml:"required"`
	Default  any      `json:"default,omitempty" yaml:"default"`
	Enum     []string `json:"enum,omitempty" yaml:"enum"` // allowed values for type "enum"
	Min      int      `json:"min,omitempty" yaml:"min"`   // int bounds (inclusive)
	Max      int      `json:"max,omitempty" yaml:"max"`
	// Label, when set, names the VictoriaMetrics label this param filters on.
	// The handler builds the PromQL label selector from every present param
	// whose Label is non-empty; params without a Label (e.g. top_n, window) are
	// consumed by the chart logic rather than injected as a matcher.
	Label string `json:"-" yaml:"label"`
}

// IOSHint is the iOS adapter's native-rendering contract (R5 §2.4). It tells
// the Swift Charts client which shape to draw without parsing the Vega-Lite
// spec; SupportsNative=false routes iOS to the PNG fallback.
type IOSHint struct {
	Kind           string `json:"kind"` // "line-multi" | "line" | "bar" | "bar-stacked" | "area-stacked"
	XField         string `json:"x_field"`
	YField         string `json:"y_field"`
	SeriesField    string `json:"series_field,omitempty"`
	YFormat        string `json:"y_format,omitempty"` // "currency_usd" | "rate_per_hour" | "count" | ""
	SupportsNative bool   `json:"supports_native"`
	// YTitle is the primary axis label (left). Set for charts carrying Overlays
	// so the web renderer can title the axis; empty otherwise.
	YTitle string `json:"y_title,omitempty"`
	// PrimaryName names the primary series in the legend when Overlays is
	// present. Empty for single-series charts (no legend).
	PrimaryName string `json:"primary_name,omitempty"`
	// Overlays describes additional series drawn on the same time axis (the
	// multi-query extension). Each carries its own value field, format, and
	// y-axis index so the web renderer can place the active-agent count on a
	// secondary right-hand axis. Empty/absent for single-series charts.
	Overlays []IOSOverlay `json:"overlays,omitempty"`
}

// IOSOverlay mirrors one ExtraSeriesDef for the rendering clients: enough for
// the web ECharts renderer (and a future iOS adapter) to draw the overlay
// series and bind it to the correct axis without re-deriving anything.
type IOSOverlay struct {
	Name       string `json:"name"`
	YField     string `json:"y_field"`
	YFormat    string `json:"y_format,omitempty"`
	YTitle     string `json:"y_title,omitempty"`
	YAxisIndex int    `json:"y_axis_index"`
}

// ChartDef is one catalog entry: everything needed to turn validated params +
// a time window into a ChartRender. It is the single source of truth shared by
// the catalog listing, the PromQL builder, the data reshaper, and the
// Vega-Lite spec builder.
type ChartDef struct {
	ID       string   `yaml:"id"`
	Title    string   `yaml:"title"`
	Subtitle string   `yaml:"subtitle"` // may reference {{.Window}} / {{.Step}} via renderTemplate
	Category string   `yaml:"category"`
	Surfaces []string `yaml:"surfaces"`

	Params        map[string]ParamSpec `yaml:"params"`
	DefaultParams map[string]any       `yaml:"default_params"`

	// QueryTemplate is a text/template rendering to a single PromQL string.
	// Context: {Selector, Step, Window} (see queryContext). This is the chart's
	// primary series.
	QueryTemplate string `yaml:"query_template"`

	// PrimarySeriesName names the primary series in the legend when the chart
	// carries ExtraSeries (multi-series). Ignored for single-series charts
	// (which have no legend). Empty falls back to the chart Title.
	PrimarySeriesName string `yaml:"primary_series_name"`

	// ExtraSeries are additional overlaid query series — the multi-query
	// extension (previously noted as "a future extension"). Each renders as its
	// own line; a series with YAxisIndex 1 binds to the secondary right-hand
	// axis. The agent-spend-cumulative chart uses one extra series to
	// overlay the active-agent count on the spend line. Empty for single-query
	// charts, which keep their exact prior behavior.
	ExtraSeries []ExtraSeriesDef `yaml:"extra_series"`

	// UsesWindow marks charts whose ?window= (1d/7d rolling) is meaningful. No
	// catalog chart currently varies its query by ?window= (the former
	// hourly-rate rolling chart is now a cumulative level — ka-0000001663), but
	// the flag is retained for charts that ignore it: window is still accepted
	// but does not change the query.
	UsesWindow bool `yaml:"uses_window"`

	// Data field naming (long format). XField is the timestamp column;
	// ValueField is the measured value; SeriesField names the series-key column
	// (empty => a single, unkeyed series).
	XField      string `yaml:"x_field"`
	ValueField  string `yaml:"value_field"`
	SeriesField string `yaml:"series_field"`
	// SeriesLabel is the VM label read off each returned series to fill
	// SeriesField (e.g. "agent", "type"). Empty for single-series charts.
	SeriesLabel string `yaml:"series_label"`

	// RollupOther, when true, summarizes every series past the top_n cap into a
	// single "other" series (summed per time bucket) instead of dropping it.
	// Keeps a stacked chart's total honest — the visible stack sums to the fleet
	// total rather than only the top-N. Requires a non-empty SeriesLabel and a
	// top_n param. When false (default), the long tail is simply dropped (the
	// prior topNSeries behavior).
	RollupOther bool `yaml:"rollup_other"`

	// Vega-Lite / iOS rendering hints.
	Mark    string `yaml:"mark"` // "line" | "bar" | "area"
	YTitle  string `yaml:"y_title"`
	YFormat string `yaml:"y_format"` // iOS y_format
	IOSKind string `yaml:"ios_kind"` // iOS hint kind
}

// ExtraSeriesDef is one overlaid query series on a multi-query chart. It is the
// minimal, targeted form of the multi-query extension: a second PromQL query
// whose single aggregate result is drawn as its own series, optionally bound to
// a secondary Y-axis. The selector is built from the chart's request params, so
// the overlay scopes to the same project/agent filter as the primary series —
// except that params whose VM label is absent on the extra series' metric are
// dropped (the agent-count gauge has no `agent` label, only project/role/state).
type ExtraSeriesDef struct {
	// Name is the legend label and series key for this overlay.
	Name string `yaml:"name"`
	// QueryTemplate renders against queryContext just like the primary query.
	QueryTemplate string `yaml:"query_template"`
	// ValueField is the long-format value column emitted for this series'
	// points (e.g. "agents"). Kept distinct from the primary ValueField so a
	// row's two measures never collide.
	ValueField string `yaml:"value_field"`
	// YFormat is the iOS/web y-axis format for this series ("count", etc.).
	YFormat string `yaml:"y_format"`
	// YTitle labels the (secondary) axis this series binds to.
	YTitle string `yaml:"y_title"`
	// YAxisIndex selects the axis: 0 = primary (left), 1 = secondary (right).
	YAxisIndex int `yaml:"y_axis_index"`
	// AllowedLabels, when non-empty, restricts which request-param VM labels are
	// injected into this series' selector. The agent-count gauge carries only
	// {project,role,state}, so an `agent=` param must not leak into its matcher.
	AllowedLabels map[string]bool `yaml:"allowed_labels"`
}

// queryContext is the substitution context for QueryTemplate. Selector is the
// pre-built label matcher body (no braces) assembled from the request's
// label-bearing params; Step and Window are PromQL durations.
type queryContext struct {
	Selector string
	Step     string
	Window   string
	// WindowHours is Window expressed as a number of hours ("24" for 1d, "168"
	// for 7d), for charts that average a windowed counter increase into a
	// per-hour rate: `sum(increase(metric[{{.Window}}]))/{{.WindowHours}}`.
	// This form is deliberately used in place of `rate()*3600` for the fleet
	// spend-rate charts (ka-0000004779): on a sparse/dense counter VictoriaMetrics
	// extrapolates rate() to the query edge on an INSTANT query but accumulates
	// per-step on a RANGE query, so the headline (instant) and the chart's last
	// range point disagree by multiples. increase()/hours is the same value
	// whether evaluated as instant or range, so headline == chart by construction.
	WindowHours string
	// TopN is the resolved top_n param, for charts that mention it in their
	// human-facing Subtitle (e.g. "top {{.TopN}} agents"). Not used in PromQL.
	TopN string
	// Rollup is the resolved roll-up dimension for charts whose primary query
	// groups by a caller-chosen label — the cost-per-bead chart rolls the spend
	// numerator up by agent | role | project | model. It substitutes into the
	// `sum by({{.Rollup}})(…)` clause and the human-facing Subtitle. Charts that
	// key on a fixed label never reference it.
	Rollup string
}

// realBillAgentLevel is the canonical PromQL fragment every per-agent SPEND
// chart reads: a deduped, gap-bridged instantaneous LEVEL of the real-bill
// per-agent spend gauge gardenio_agent_spend_logs_usd. It replaces the dead
// at-close counter gardenio_agent_spend_usd_total, which read ~1000x low (14 USD
// live across 1 series vs a ~$40k real bill — ka-0000001663, sibling /costs fix
// ka-0000002699). The gauge carries the SAME {agent,project,role,model,account}
// label set as the old counter, so it is a drop-in for every per-agent, per-role,
// per-project, and per-model breakdown the catalog draws (metrics/spend.go).
//
// SHAPE — why a LEVEL, not increase()/rate():
//
//   - gardenio_agent_spend_logs_usd is a Reset+Set GAUGE (SetSpendLogsCosts): each
//     ~30-min sweep Resets the whole vector then re-Sets every tracked agent's
//     ABSOLUTE lifetime figure (spendlogs_sweep.go). increase()/rate()/deriv() all
//     read the periodic Reset as a counter reset and return inflated garbage —
//     measured live: sum(increase(...[24h])) = ~$26k phantom vs a ~$9k real
//     tracked-population level; deriv() overshoots and goes negative as agents age
//     out. Only the instantaneous value is meaningful (same finding the merged
//     /costs fix documents in costs_rollup.go buildCostsRollupAgentGaugeQuery).
//   - `max by (agent, project, role, model, account)` collapses HA duplicates (two
//     controller replicas briefly co-publishing the same agent) so a doubled scrape
//     can't inflate the sum — matching the account/tag-gauge dedup discipline.
//   - The `[15m:5m]` last_over_time subquery bridges the gauge's between-sweep
//     ABSENCE. The sweep Resets then re-populates the vector each cycle, and the
//     gauge is intermittently absent at a raw instant (verified live: 0 series at
//     one instant, 879 series / ~$3k recovered by a 5-min-resolution look-back).
//     Since the catalog runs RANGE queries (QueryRange, per-step), a raw instant
//     would draw gappy lines; the 15-min bridge (> the 30-min-cadence scrape
//     jitter, < the sweep period) carries the last real level across the gap
//     without inventing a value — the range-query analogue of the /costs page's
//     single-instant read. Verified live: the bridged range series is gapless and
//     monotone.
//
// {{.Selector}} is injected inside the gauge's own label matcher so every existing
// chart param (project/agent/role/model) still scopes the series. The caller wraps
// this fragment in its own `sum by(<dim>)(...)` to pick the breakdown dimension.
const realBillAgentLevel = `last_over_time((max by (agent, project, role, model, account) ` +
	`(gardenio_agent_spend_logs_usd{{"{"}}{{.Selector}}{{"}"}}))[15m:5m])`

// realBillAgentLevelOffsetStep is realBillAgentLevel shifted back one {{.Step}}
// bucket. Subtracting it from realBillAgentLevel yields the real-bill spend that
// accrued WITHIN the step — the per-bucket FLOW the level gauge cannot express
// directly (increase() reads its Reset+Set garbage; see realBillAgentLevel). The
// caller clamps the difference to >= 0 (an agent aging out of the ~48h lookback
// would otherwise show a spurious negative) and divides by a per-bucket close
// count for the cost-per-bead ratio. `offset {{.Step}}` re-uses the same bridged
// level one bucket earlier, so both operands share the exact dedup + gap-bridge
// shape and the subtraction matches on the full {agent,...} label set.
const realBillAgentLevelOffsetStep = `last_over_time((max by (agent, project, role, model, account) ` +
	`(gardenio_agent_spend_logs_usd{{"{"}}{{.Selector}}{{"}"}}))[15m:5m] offset {{.Step}})`

// realBillTagFlow / realBillTagFlowOr are the per-{{.Rollup}}-bucket real-bill
// FLOW read from the LIFETIME per-dimension tag gauge gardenio_tag_cost_usd, with
// a self-healing fallback to the per-agent gauge — the numerator half of the
// cost-per-bead ratio when the roll-up dimension is one the tag gauge carries
// (role|project|model). It is the kbeads-chart twin of the /costs page rollup
// (costs_rollup.go buildCostsRollupTagGaugeQuery) and the Grafana "$rollup" dash
// panels 5/6 (uid gb-cost-per-bead-rollup-preview), kept the SAME self-healing
// `<tag-gauge> or <per-agent>` shape they proved live against vmsingle
// (ka-0000000478 → ka-0000002556, the deferred kbeads-catalog consumer).
//
// WHY THE TAG GAUGE, NOT the per-agent gauge, for role/project/model. Both are
// the real LiteLLM bill, but they cover different populations. The per-agent
// spend-logs gauge (realBillAgentLevel) only carries a series for agents whose
// bead was created inside the spend-logs sweep's ~48h lookback, so a
// sum-by-role/project/model over it is a RECENT-FLEET slice, not the lifetime
// bill. gardenio_tag_cost_usd is the lifetime real bill per (dim,value), synced
// from LiteLLM GET /spend/tags every ~5min (metrics/spend.go, tag_spend_sync.go)
// — the same source family as gardenio_account_cost_usd — so by-role/project/model
// finally reads the FULL lifetime bill, matching /costs and the account gauge.
//
// SHAPE, mirroring the account-cost charts and the proven /costs query:
//   - `max by (dim, value, env)` collapses any per-replica gauge duplicates
//     (HA), exactly the dedup the account gauge gets, then
//   - `label_replace(..., "{{.Rollup}}", "$1", "value", "(.*)")` copies the
//     gauge's shared `value` label onto a label NAMED after the roll-up dim, so
//     the outer `sum by({{.Rollup}})` groups on the same label name the per-agent
//     branch does — without this the two `or` arms key on different labels and
//     the fallback fails to line up (the "phantom Value series" bug ka-0000002838
//     fixed on the dashboard). dim="{{.Rollup}}" scopes to the requested
//     dimension; the roll-up value is the closed enum, injection-safe.
//   - `increase((...)[{{.Step}}:5m])` is the per-bucket FLOW. Unlike the per-agent
//     Reset+Set gauge (where increase() reads garbage), the tag gauge is a
//     lifetime-cumulative MONOTONIC series (tag totals only grow — SetTagCosts
//     re-Sets an ever-≥ value), so increase() is valid, and the [:5m] aligned
//     subquery de-bumps the extrapolation sawtooth like the account charts.
//
// The tag arm carries NO {{.Selector}} on purpose: /spend/tags reports
// per-single-tag totals, so the gauge cannot INTERSECT dimensions (no
// "role=gardener AND project=monorepo"). It is therefore FLEET-WIDE per
// dimension, and the chart's project/role/model/account filters scope only the
// per-agent fallback arm — byte-for-byte the behavior the Grafana "$rollup"
// panels 5/6 document ("the cross filters scope the per-agent fallback; the
// real-bill tag half is fleet-wide per dimension"). This MR matches that dash
// shape, not costs_rollup.go's interactive resolveRollupSource routing.
//
// realBillTagFlowOr wraps it with `or <per-agent flow>` so the chart SELF-HEALS:
// the tag gauge is empty until the tag-spend producer has accumulated tagged
// spend (backfill window after deploy), and PromQL's `or` yields the right arm
// only for the group keys the left arm did not produce — so during backfill the
// view falls back to the per-agent per-bucket flow (realBillAgentLevel minus its
// one-step offset, the same numerator the ratio used before this change) and
// auto-upgrades to the lifetime tag gauge the moment it fills. The fallback arm
// carries {{.Selector}} so project/agent/role/model params still scope it.
const realBillTagFlow = `increase((label_replace(max by (dim, value, env) ` +
	`(gardenio_tag_cost_usd{dim="{{.Rollup}}"}), "{{.Rollup}}", "$1", "value", "(.*)"))[{{.Step}}:5m])`

// realBillTagFlowOr is realBillTagFlow already summed by the roll-up dim, OR'd
// with the per-agent per-bucket flow (its own summed clamp_min level-delta) so a
// missing group key self-heals to the per-agent gauge. See realBillTagFlow.
const realBillTagFlowOr = `sum by({{.Rollup}})(` + realBillTagFlow + `) or ` +
	`clamp_min(sum by({{.Rollup}})(` + realBillAgentLevel + `) - ` +
	`sum by({{.Rollup}})(` + realBillAgentLevelOffsetStep + `), 0)`

// accountCostE2EExclusion is the PromQL label matcher every account-cost chart
// carries to drop E2E test-account series from the by-account rollup and the
// fleet spend rate (ka-0000001436, sibling to the beads-viewer fix ka-0000005167).
//
// WHY IT IS NEEDED. The gardenio E2E suite seeds throwaway type=account beads
// (tests/e2e/scripts/test-*.sh) that flow through gardenio_account_cost_usd. The
// account series OUTLIVE the bead: the harness runs against a live
// deployment and the metric retains the series for the vmsingle retention window
// after the harness closes the bead, so the producer-side skip
// (metrics.collectAccounts, shipped in !6186) cannot clean the retention tail.
// This matcher does, on both the Grafana catalog surface AND the /costs headline
// it sits beside.
//
// STRING IDENTITY CONTRACT (the drift guard this bead was filed to close).
// kbeads and gardenio/controller are SEPARATE Go modules that do not cross-import,
// so this cannot be a single shared symbol. It is instead kept BYTE-IDENTICAL to
// the controller's costsRollupE2EAccountExclusion
// (gardenio/controller/cmd/beads-viewer/costs_rollup.go), which the viewer's
// by-account rollup, Accounts dropdown, and fleet-rate headline (costs_rate.go)
// all consume. TestAccountCostE2EExclusion_MatchesControllerConstant pins the
// exact literal here; the controller's costs_rate_test.go pins it there. If you
// change one you MUST change the other or those tests red.
//
//   - (?i:…) makes it CASE-INSENSITIVE to match the Go predicate
//     beadsapi.IsE2EAccountBead, which lowercases the title before prefix-matching.
//     PromQL matchers are case-sensitive, so without it "E2E-acct-…" would keep
//     its retention-tail series while the producer skipped it — silently defeating
//     the exclusion.
//   - Fully-anchored alternation (VM wraps each branch as ^(?:e2e-.*)$ etc.), so it
//     excludes exactly the two hyphen-anchored prefixes and never a real account
//     merely beginning with the letters "e2e" (e.g. e2eflow-prod).
//
// Do NOT propagate this to the bill-conservation x-check sweep
// (gardenio/controller/cmd/controller/xcheck_sweep.go): a side there is the WHOLE
// LiteLLM bill, so filtering the other side would push the agreement metric below
// its floor and fire a false cost-integrity alert (ka-0000001340).
const accountCostE2EExclusion = `account!~"(?i:e2e-.*|probe-acct-.*)"`

// accountCostGaugeSelector is the account-cost gauge (gardenio_account_cost_usd)
// with accountCostE2EExclusion ALWAYS applied, plus the chart's own label params
// ({{.Selector}}) appended when present. It is the single templated fragment every
// account-cost chart embeds inside its `max by (account, env)(…)` dedup, so the
// exclusion cannot be forgotten on one chart and drift from the others. Renders to
// `gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)"}` with no params,
// or `…,{{.Selector}}}` when the request scopes account/env.
const accountCostGaugeSelector = `gardenio_account_cost_usd{{"{"}}` + accountCostE2EExclusion +
	`{{if .Selector}},{{.Selector}}{{end}}{{"}"}}`

// activeAgentStates is the state exclusion every "producing agent" count shares:
// drop the states that burn no spend (prewarmed/idle pool agents, terminal
// states). It is EXCLUSION-based, not an allowlist, so a newly-added active state
// still counts. Kept as one constant so the count overlay and the per-agent-cost
// denominator can never drift apart — a divergence there silently changes the
// ratio's meaning.
const activeAgentStates = `state!~"prewarmed|idle|assigning|done|failed|exited|stopped"`

// activeAgentsInstant is the deduped INSTANTANEOUS count of producing agents.
//
// `max by (project, role, state)` collapses per-POD duplicates. Metrics
// collection runs on EVERY controller replica (metricsCollector.Start is outside
// leader election), so during a controller rollover two pods briefly co-report
// the same {project,role,state} cell and a bare sum() double-counts. Measured
// live 2026-08-03: 26 distinct pods reported the gauge over 24h, and the bare vs
// deduped 24h peak differed (25 vs 20 agents) — so the dedup is load-bearing on
// the peak, even though the two agree at a quiet instant (5 vs 5).
const activeAgentsInstant = `sum(max by (project, role, state)(gardenio_agents_active{` + activeAgentStates + `}))`

// avgActiveAgentsOver builds the WINDOW-AVERAGE producing-agent count over the
// given PromQL window — the denominator of every dollars-per-hour-per-agent
// ratio. It is an OUTER subquery over the already-summed instant vector:
//
//	avg_over_time( sum(<gauge>)[W:5m] )
//
// THE BUG THIS FIXES (ka-0000006587). The natural-looking INNER form
// `sum(avg_over_time(gauge[W]))` is WRONG by a large, window-dependent factor,
// and it is what the "cost per agent" lines shipped with. avg_over_time applied
// PER-SERIES averages each {project,role,state} series over only the samples
// that series has, then sums across ALL series that existed anywhere in the
// window. Agent series are highly EPHEMERAL — one per role/project/state cell,
// churning as agents come and go — so the sum runs over the window's whole
// CUMULATIVE series population rather than the count concurrent at any instant.
//
// Measured live 2026-08-03 (3 series present at the instant; 18 distinct series
// over 1h; 470 over 24h):
//
//	inner sum(avg_over_time(...[1h]))   = 20.5   "agents"  (true ~ 5.05)   4x high
//	inner sum(avg_over_time(...[24h]))  = 603.9  "agents"  (true ~ 5.16) 117x high
//	outer avg_over_time(sum(...)[1h:5m])=  5.05  agents  ✓
//	outer avg_over_time(sum(...)[24h:5m])= 5.37  agents  ✓
//
// Because the denominator was inflated, the shipped ratio read LOW: $0.115/hr/agent
// on the 24h line against a true ~$13.2. The error grows with the window (more
// churn accumulates), so the 24h and 1h lines were wrong by different factors and
// could not be compared with each other — the defect that motivated this fix.
//
// Cross-check (two independent derivations, per advice ka-0000005924): the outer
// form agrees with an agent-seconds integral computed a different way —
// avg_over_time = 5.145 vs sum_over_time(...)*60/86400 = 5.066 over 24h (~1.5%,
// the residual being grid alignment). The inner form matches neither.
//
// GRID = 5m, NOT 1m (memory, verified live). A 1m grid OOMs vmsingle at a 7d
// display range: VM must materialize the inner series × grid points and 422s with
// "not enough memory … returns 20168000 data points across 1000 time series …
// requested memory: 323688000 bytes; total available: 80530636". The 5m grid
// (= the metric's real scrape/refresh cadence) evaluates the same 7d range in
// well under a second and returns a gapless series, and the two grids agree to
// ~1% at 24h (5.149 vs 5.301). So 5m is both the cheap and the honest choice.
func avgActiveAgentsOver(window string) string {
	return `avg_over_time(` + activeAgentsInstant + `[` + window + `:5m])`
}

// windowHours maps a PromQL rolling-window duration to its length in hours as a
// bare string, for the increase()/hours rate form. Only the enum values the
// rolling charts accept ("1d"/"7d") are mapped; anything else falls back to a
// best-effort parse and defaults to "24" so a misconfigured window never emits
// a divide-by-zero or empty divisor into the PromQL.
func windowHours(window string) string {
	switch window {
	case "1d":
		return "24"
	case "7d":
		return "168"
	}
	if d, err := time.ParseDuration(window); err == nil && d >= time.Hour {
		return strconv.FormatFloat(d.Hours(), 'f', -1, 64)
	}
	return "24"
}

// chartCatalog is the registry of v1 charts, built once. The five charter charts
// match the E5 charter (ka-0000001540): spend-by-agent, beads-events-rate,
// agent-cost-per-bead, bead-status-flow, and the operator's
// agent-spend-cumulative (ka-0000001797; formerly hourly-rate-rolling-window,
// re-cast to a cumulative real-bill level — ka-0000001663).
//
// The spend / beads-events metrics (E3 ka-0000000692, E4 ka-0000001152) may
// not be emitting yet; the range queries then return empty series and
// the charts render as honest empty time series rather than erroring.
//
// The YAML is decoded once (about 8ms); each call returns fresh copies, so a
// caller can still change what it gets without touching the shared catalog.
func chartCatalog() map[string]ChartDef {
	defs, err := loadedChartCatalog()
	if err != nil {
		// The YAML is embedded at build time and pinned by
		// TestChartCatalog_Golden, so this cannot happen in a tested build.
		// Serve no charts rather than crash the daemon.
		slog.Error("chart catalog: load charts_catalog.yaml", "err", err)
		return map[string]ChartDef{}
	}
	out := make(map[string]ChartDef, len(defs))
	for _, d := range defs {
		out[d.ID] = d.clone()
	}
	return out
}

var loadedChartCatalog = sync.OnceValues(func() ([]ChartDef, error) {
	return loadChartCatalog(chartCatalogYAML)
})

// clone deep-copies the def's slices and maps (keeping nil as nil). A new
// slice or map field on ChartDef, ParamSpec or ExtraSeriesDef must be copied
// here too.
func (d ChartDef) clone() ChartDef {
	d.Surfaces = slices.Clone(d.Surfaces)
	if d.Params != nil {
		params := make(map[string]ParamSpec, len(d.Params))
		for k, p := range d.Params {
			p.Enum = slices.Clone(p.Enum)
			params[k] = p
		}
		d.Params = params
	}
	d.DefaultParams = maps.Clone(d.DefaultParams)
	if d.ExtraSeries != nil {
		d.ExtraSeries = slices.Clone(d.ExtraSeries)
		for i := range d.ExtraSeries {
			d.ExtraSeries[i].AllowedLabels = maps.Clone(d.ExtraSeries[i].AllowedLabels)
		}
	}
	return d
}

// chartCatalogYAML is the chart catalog: one entry per ChartDef, keys in the
// fields' yaml tags. Each query_template may reference the shared PromQL
// fragments above as ${name}, or ${avgActiveAgentsOver:<window>}; the loader
// expands them, so the fragments stay single-sourced in Go.
//
//go:embed charts_catalog.yaml
var chartCatalogYAML []byte

// chartQueryFragments are the Go-held PromQL fragments a catalog
// query_template may reference as ${name}.
var chartQueryFragments = map[string]string{
	"realBillAgentLevel":       realBillAgentLevel,
	"realBillTagFlowOr":        realBillTagFlowOr,
	"accountCostGaugeSelector": accountCostGaugeSelector,
	"activeAgentsInstant":      activeAgentsInstant,
}

// chartQueryFragmentFuncs are the parameterised fragments a catalog
// query_template may reference as ${name:arg}.
var chartQueryFragmentFuncs = map[string]func(string) string{
	"avgActiveAgentsOver": avgActiveAgentsOver,
}

// chartFragmentRef matches ${name} and ${name:arg}. The arg may hold
// {{...}} template actions (e.g. ${avgActiveAgentsOver:{{.Window}}}), so
// braces are allowed inside it only as whole {{...}} pairs.
var chartFragmentRef = regexp.MustCompile(`\$\{([A-Za-z][A-Za-z0-9]*)(?::((?:\{\{[^}]*\}\}|[^{}])*))?\}`)

// loadChartCatalog decodes the catalog YAML strictly (an unknown key is an
// error) and expands the fragment references in every query template.
func loadChartCatalog(data []byte) ([]ChartDef, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var defs []ChartDef
	if err := dec.Decode(&defs); err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(defs))
	for i := range defs {
		d := &defs[i]
		if d.ID == "" || seen[d.ID] {
			return nil, fmt.Errorf("entry %d: missing or duplicate id %q", i, d.ID)
		}
		seen[d.ID] = true
		var err error
		if d.QueryTemplate, err = expandChartFragments(d.QueryTemplate); err != nil {
			return nil, fmt.Errorf("%s: %w", d.ID, err)
		}
		for j := range d.ExtraSeries {
			es := &d.ExtraSeries[j]
			if es.QueryTemplate, err = expandChartFragments(es.QueryTemplate); err != nil {
				return nil, fmt.Errorf("%s extra series %q: %w", d.ID, es.Name, err)
			}
		}
	}
	return defs, nil
}

func expandChartFragments(q string) (string, error) {
	var bad error
	out := chartFragmentRef.ReplaceAllStringFunc(q, func(ref string) string {
		m := chartFragmentRef.FindStringSubmatch(ref)
		name, hasArg := m[1], strings.Contains(ref, ":")
		if hasArg {
			if fn, ok := chartQueryFragmentFuncs[name]; ok {
				return fn(m[2])
			}
		} else if frag, ok := chartQueryFragments[name]; ok {
			return frag
		}
		bad = fmt.Errorf("unknown query fragment %s", ref)
		return ref
	})
	if bad != nil {
		return "", bad
	}
	if strings.Contains(out, "${") {
		return "", fmt.Errorf("unexpanded fragment reference in %q", q)
	}
	return out, nil
}

// renderTemplate renders a text/template string against the given context,
// returning the rendered string. Used for both the PromQL QueryTemplate and the
// human-facing Subtitle. A template parse/exec error is returned to the caller
// rather than panicked so a malformed catalog entry surfaces as a 500 on that
// one chart, not a daemon crash.
func renderTemplate(tmpl string, ctx any) (string, error) {
	t, err := template.New("chart").Parse(tmpl)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, ctx); err != nil {
		return "", fmt.Errorf("execute template: %w", err)
	}
	return buf.String(), nil
}

// buildSelector assembles a PromQL label-matcher body (no surrounding braces)
// from the request's label-bearing params. For a "string[]" param with
// multiple values it emits a regex alternation (label=~"a|b"); for a single
// value it emits an exact match (label="a"). Keys are emitted in sorted order
// so the resulting PromQL — and therefore the cache key built from it — is
// deterministic. Values are sanitized to a conservative charset so a param
// cannot smuggle a quote/brace and break out of the matcher.
func buildSelector(def ChartDef, params map[string][]string) string {
	return buildSelectorFiltered(def, params, nil)
}

// buildSelectorFiltered is buildSelector with an optional allowlist of VM
// labels: when allowed is non-nil, only params whose Label is in allowed are
// injected. This lets an overlay series scope to the labels its metric actually
// carries (e.g. the active-agent gauge has no `agent` label) while still
// honoring the shared project/role filter. A nil allowlist admits every
// label-bearing param, preserving the primary query's behavior.
func buildSelectorFiltered(def ChartDef, params map[string][]string, allowed map[string]bool) string {
	var parts []string
	keys := make([]string, 0, len(def.Params))
	for name := range def.Params {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		spec := def.Params[name]
		if spec.Label == "" {
			continue
		}
		if allowed != nil && !allowed[spec.Label] {
			continue
		}
		vals := sanitizeLabelValues(params[name])
		switch len(vals) {
		case 0:
			continue
		case 1:
			parts = append(parts, fmt.Sprintf("%s=%q", spec.Label, vals[0]))
		default:
			parts = append(parts, fmt.Sprintf("%s=~%q", spec.Label, strings.Join(vals, "|")))
		}
	}
	return strings.Join(parts, ",")
}

// sanitizeLabelValues drops empty entries and strips any character outside a
// conservative identifier/path charset, so a PromQL label value built from
// user input cannot contain a quote, brace, backslash, or regex
// metacharacter. This is defense-in-depth on top of the curated-catalog model
// (we never accept raw PromQL); chart labels in this system are agent names,
// project slugs, model ids, and event types — all within this charset.
func sanitizeLabelValues(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		var b strings.Builder
		for _, r := range v {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case r == '-' || r == '_' || r == '.' || r == '/' || r == ':':
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			out = append(out, b.String())
		}
	}
	return out
}
