package server

import (
	"encoding/json"
	"testing"
)

// catalogDef fetches a ChartDef from the live catalog by id, failing the test
// if it is absent — so these tests track the real catalog (source of truth),
// not a hand-copied fixture.
func catalogDef(t *testing.T, id string) ChartDef {
	t.Helper()
	def, ok := chartCatalog()[id]
	if !ok {
		t.Fatalf("chart %q not in catalog", id)
	}
	return def
}

func findTarget(t *testing.T, p *GrafanaPanel, refID string) GrafanaTarget {
	t.Helper()
	for _, tg := range p.Targets {
		if tg.RefID == refID {
			return tg
		}
	}
	t.Fatalf("no target with refId %q (targets: %+v)", refID, p.Targets)
	return GrafanaTarget{}
}

// TestGeneratePanel_FleetSpendRate covers the 3-series, 2-axis case: a primary
// rolling spend line + a 1h companion on the SAME left axis + an active-agent
// overlay on the secondary RIGHT axis. It pins query parity (verbatim PromQL),
// refId ordering, axis placement overrides, and units/legends.
func TestGeneratePanel_FleetSpendRate(t *testing.T) {
	def := catalogDef(t, "fleet-spend-rate")

	p, err := GeneratePanel(def)
	if err != nil {
		t.Fatalf("GeneratePanel: %v", err)
	}

	// Panel-level mapping.
	if p.Type != "timeseries" {
		t.Errorf("type = %q, want timeseries", p.Type)
	}
	if p.Title != "Fleet spend rate" {
		t.Errorf("title = %q, want %q", p.Title, "Fleet spend rate")
	}
	// Subtitle "USD/hour, {{.Window}} rolling window" rendered with default 1d.
	if want := "USD/hour, 1d rolling window"; p.Description != want {
		t.Errorf("description = %q, want %q", p.Description, want)
	}
	// Mark line → line draw style, no stacking.
	if p.FieldConfig.Defaults.Custom.DrawStyle != "line" {
		t.Errorf("drawStyle = %q, want line", p.FieldConfig.Defaults.Custom.DrawStyle)
	}
	if p.FieldConfig.Defaults.Custom.Stacking != nil {
		t.Errorf("stacking = %+v, want nil for a line chart", p.FieldConfig.Defaults.Custom.Stacking)
	}
	// YFormat rate_per_hour → currencyUSD; YTitle → left axis label.
	if p.FieldConfig.Defaults.Unit != "currencyUSD" {
		t.Errorf("unit = %q, want currencyUSD", p.FieldConfig.Defaults.Unit)
	}
	if p.FieldConfig.Defaults.Custom.AxisPlacement != "left" {
		t.Errorf("primary axisPlacement = %q, want left", p.FieldConfig.Defaults.Custom.AxisPlacement)
	}
	if p.FieldConfig.Defaults.Custom.AxisLabel != "USD/hour" {
		t.Errorf("primary axisLabel = %q, want USD/hour", p.FieldConfig.Defaults.Custom.AxisLabel)
	}

	// Seven series → refIds A..G in catalog order: spend, spend (1h),
	// cost per agent, cost per agent (1h), active agents, agents (1h avg),
	// agents (24h avg). The last two are the rolling agent-count averages
	// (ka-0000006587).
	if len(p.Targets) != 7 {
		t.Fatalf("got %d targets, want 7: %+v", len(p.Targets), p.Targets)
	}
	a := findTarget(t, p, "A")
	b := findTarget(t, p, "B")
	c := findTarget(t, p, "C")
	d := findTarget(t, p, "D")
	e := findTarget(t, p, "E")
	f := findTarget(t, p, "F")
	g := findTarget(t, p, "G")

	// refId A = primary, verbatim PromQL (query parity), legend = PrimarySeriesName.
	// The max-by-dedup + [1d:5m] aligned subquery is the ka-0000000170 de-bump; the
	// account!~"(?i:e2e-.*|probe-acct-.*)" matcher is the ka-0000001436 E2E-account
	// exclusion (accountCostE2EExclusion), byte-identical to the controller's
	// costsRollupE2EAccountExclusion so the Grafana surface matches the /costs headline.
	if want := `sum(increase((max by (account, env) (gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)"}))[1d:5m])) / 24`; a.Expr != want {
		t.Errorf("refId A expr = %q, want %q", a.Expr, want)
	}
	if a.LegendFormat != "spend" {
		t.Errorf("refId A legend = %q, want spend", a.LegendFormat)
	}
	// refId B = 1h companion on the SAME left axis (no override), verbatim.
	if want := `sum(increase((max by (account, env) (gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)"}))[1h:5m])) / 1`; b.Expr != want {
		t.Errorf("refId B expr = %q, want %q", b.Expr, want)
	}
	if b.LegendFormat != "spend (1h)" {
		t.Errorf("refId B legend = %q, want %q", b.LegendFormat, "spend (1h)")
	}
	// refId C = cost per agent, {{.Window}} rolling (24h default), on the SAME
	// left USD/hour axis. Fleet rate numerator (identical to A) over the
	// window-averaged active-agent count (ka-0000003828). Selector dropped on the
	// denominator gauge (AllowedLabels filters it to account/env only).
	//
	// The denominator is the OUTER-subquery form avg_over_time(sum(...)[W:5m]),
	// NOT the per-series sum(avg_over_time(...[W])) this shipped with: agent series
	// are ephemeral, so the inner form summed the window's CUMULATIVE series
	// population (117x high at 24h) and drove this line 117x LOW. Pinned verbatim
	// here so the regression cannot come back silently (ka-0000006587).
	if want := `sum(increase((max by (account, env) (gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)"}))[1d:5m])) / 24 ` +
		`/ (avg_over_time(sum(max by (project, role, state)(gardenio_agents_active{state!~"prewarmed|idle|assigning|done|failed|exited|stopped"}))[1d:5m]) > 0)`; c.Expr != want {
		t.Errorf("refId C expr = %q, want %q", c.Expr, want)
	}
	if c.LegendFormat != "cost per agent" {
		t.Errorf("refId C legend = %q, want %q", c.LegendFormat, "cost per agent")
	}
	// refId D = cost per agent, fixed 1h rolling (the 1h companion to C, pairing
	// 1:1 with "spend (1h)"). Fixed [1h] numerator + 1h agent-count average.
	if want := `sum(increase((max by (account, env) (gardenio_account_cost_usd{account!~"(?i:e2e-.*|probe-acct-.*)"}))[1h:5m])) / 1 ` +
		`/ (avg_over_time(sum(max by (project, role, state)(gardenio_agents_active{state!~"prewarmed|idle|assigning|done|failed|exited|stopped"}))[1h:5m]) > 0)`; d.Expr != want {
		t.Errorf("refId D expr = %q, want %q", d.Expr, want)
	}
	if d.LegendFormat != "cost per agent (1h)" {
		t.Errorf("refId D legend = %q, want %q", d.LegendFormat, "cost per agent (1h)")
	}
	// refId E = active-agent overlay (selector dropped — gauge has no account/env
	// labels so the AllowedLabels filter yields an empty selector). Wrapped in
	// last_over_time(sum(...)[2m:30s]) to bridge controller-rollover scrape gaps
	// so the overlay does not dip to the floor for one sample (ka-0000006890).
	if want := `last_over_time(sum(gardenio_agents_active{state!~"prewarmed|idle|assigning|done|failed|exited|stopped"})[2m:30s])`; e.Expr != want {
		t.Errorf("refId E expr = %q, want %q", e.Expr, want)
	}
	if e.LegendFormat != "active agents" {
		t.Errorf("refId E legend = %q, want %q", e.LegendFormat, "active agents")
	}
	// refIds F/G = the 1h and 24h rolling agent-count averages (ka-0000006587).
	// F is byte-identical to refId C's and D's denominator by construction (all
	// three come from avgActiveAgentsOver), which is what lets the operator read
	// the ratio and its denominator off one panel.
	if want := `avg_over_time(sum(max by (project, role, state)(gardenio_agents_active{state!~"prewarmed|idle|assigning|done|failed|exited|stopped"}))[1h:5m])`; f.Expr != want {
		t.Errorf("refId F expr = %q, want %q", f.Expr, want)
	}
	if f.LegendFormat != "agents (1h avg)" {
		t.Errorf("refId F legend = %q, want %q", f.LegendFormat, "agents (1h avg)")
	}
	if want := `avg_over_time(sum(max by (project, role, state)(gardenio_agents_active{state!~"prewarmed|idle|assigning|done|failed|exited|stopped"}))[24h:5m])`; g.Expr != want {
		t.Errorf("refId G expr = %q, want %q", g.Expr, want)
	}
	if g.LegendFormat != "agents (24h avg)" {
		t.Errorf("refId G legend = %q, want %q", g.LegendFormat, "agents (24h avg)")
	}

	// THREE field overrides — one per YAxisIndex-1 series (active agents + both
	// rolling averages) → right axis, count→none unit, "agents" axis label. The 1h
	// companion + cost-per-agent series (all YAxisIndex 0) must NOT get one.
	if len(p.FieldConfig.Overrides) != 3 {
		t.Fatalf("got %d overrides, want 3: %+v", len(p.FieldConfig.Overrides), p.FieldConfig.Overrides)
	}
	wantOverrides := []string{"active agents", "agents (1h avg)", "agents (24h avg)"}
	for i, wantName := range wantOverrides {
		ov := p.FieldConfig.Overrides[i]
		if ov.Matcher.ID != "byName" || ov.Matcher.Options != wantName {
			t.Errorf("override[%d] matcher = %+v, want byName/%s", i, ov.Matcher, wantName)
		}
		props := map[string]any{}
		for _, pr := range ov.Properties {
			props[pr.ID] = pr.Value
		}
		if props["custom.axisPlacement"] != "right" {
			t.Errorf("override[%d] axisPlacement = %v, want right", i, props["custom.axisPlacement"])
		}
		if props["unit"] != "none" {
			t.Errorf("override[%d] unit = %v, want none (count)", i, props["unit"])
		}
		if props["custom.axisLabel"] != "agents" {
			t.Errorf("override[%d] axisLabel = %v, want agents", i, props["custom.axisLabel"])
		}
	}

	// The panel must marshal to valid JSON (grafana-bridge syncs it as-is).
	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("marshal panel: %v", err)
	}
}

// TestGeneratePanel_AgentSpendOverTime covers the stacked-bar / top-N case: a
// single grouped primary series drawn as chonky stacked bars (operator request
// ka-0000004162, matching the by-role rollup panel), legend interpolated from
// the series label, top_n surfaced in the description.
func TestGeneratePanel_AgentSpendOverTime(t *testing.T) {
	def := catalogDef(t, "agent-spend-over-time")

	p, err := GeneratePanel(def)
	if err != nil {
		t.Fatalf("GeneratePanel: %v", err)
	}

	if p.Title != "Per-agent cost over time" {
		t.Errorf("title = %q", p.Title)
	}
	// Subtitle "cumulative real-bill USD, top {{.TopN}} agents" → default top_n
	// (8). This is where the topk/top-N intent surfaces; the PromQL itself stays
	// verbatim (top-N is applied post-query by the catalog reshaper, not in
	// PromQL), preserving query parity.
	if want := "cumulative real-bill USD, top 8 agents"; p.Description != want {
		t.Errorf("description = %q, want %q", p.Description, want)
	}

	// Mark bar → chonky stacked bars: bars draw style, stacking normal, filled.
	cu := p.FieldConfig.Defaults.Custom
	if cu.DrawStyle != "bars" {
		t.Errorf("drawStyle = %q, want bars (chonky stacked bars)", cu.DrawStyle)
	}
	if cu.Stacking == nil || cu.Stacking.Mode != "normal" {
		t.Errorf("stacking = %+v, want normal", cu.Stacking)
	}
	if cu.FillOpacity == 0 {
		t.Errorf("fillOpacity = 0, want a filled bar")
	}

	// YFormat currency_usd → currencyUSD unit + 2 decimals; YTitle → axis label.
	if p.FieldConfig.Defaults.Unit != "currencyUSD" {
		t.Errorf("unit = %q, want currencyUSD", p.FieldConfig.Defaults.Unit)
	}
	if p.FieldConfig.Defaults.Decimals == nil || *p.FieldConfig.Defaults.Decimals != 2 {
		t.Errorf("decimals = %v, want 2", p.FieldConfig.Defaults.Decimals)
	}
	if cu.AxisLabel != "USD" {
		t.Errorf("axisLabel = %q, want USD", cu.AxisLabel)
	}

	// One grouped primary series, refId A, label-interpolated legend. No extra
	// series → no overrides. The PromQL is wrapped in topk(8, …) for the Grafana
	// surface (ka-0000002132): this is a RollupOther chart, so the HTTP/iOS path
	// caps it to top_n via topNSeriesWithOther, and GeneratePanel bakes the SAME
	// cap into the query — otherwise the ~hundreds of per-agent series render as
	// illegible spaghetti with an off-panel legend. The inner grouped expr is
	// unchanged; topk(8, …) wraps it verbatim.
	if len(p.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(p.Targets), p.Targets)
	}
	a := findTarget(t, p, "A")
	if want := `topk(8, sum by(agent)(last_over_time((max by (agent, project, role, model, account) (gardenio_agent_spend_logs_usd{}))[15m:5m])))`; a.Expr != want {
		t.Errorf("refId A expr = %q, want %q", a.Expr, want)
	}
	if a.LegendFormat != "{{ agent }}" {
		t.Errorf("refId A legend = %q, want {{ agent }}", a.LegendFormat)
	}
	if len(p.FieldConfig.Overrides) != 0 {
		t.Errorf("got %d overrides, want 0 for a single-series chart", len(p.FieldConfig.Overrides))
	}

	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("marshal panel: %v", err)
	}
}

// TestGeneratePanel_CostPerBead covers the chonky cost-per-bead panel
// (ka-0000003269): a roll-up STACKED-BAR chart whose primary query is the
// group_left() fleet-denominator form. It pins the verified PromQL, the
// stacked-bar custom block (drawStyle bars + stacking normal + fillOpacity 80,
// mirroring "Spend per agent"), the default roll-up dimension in the legend +
// `sum by()`, and the currency unit.
func TestGeneratePanel_CostPerBead(t *testing.T) {
	def := catalogDef(t, "agent-cost-per-bead")

	// Catalog source-of-truth: the chonky-bars shape.
	if def.Mark != "bar" {
		t.Errorf("Mark = %q, want bar", def.Mark)
	}
	if def.IOSKind != "bar-stacked" {
		t.Errorf("IOSKind = %q, want bar-stacked", def.IOSKind)
	}
	if !def.RollupOther {
		t.Errorf("RollupOther = false, want true (long tail folds into 'other' so the stack stays honest)")
	}

	p, err := GeneratePanel(def)
	if err != nil {
		t.Fatalf("GeneratePanel: %v", err)
	}
	if p.Title != "Cost per bead closed" {
		t.Errorf("title = %q, want Cost per bead closed", p.Title)
	}

	// Stacked bars: drawStyle bars, stacking normal, filled at 80 (matches the
	// hand-authored "Spend per agent" panel).
	cu := p.FieldConfig.Defaults.Custom
	if cu.DrawStyle != "bars" {
		t.Errorf("drawStyle = %q, want bars", cu.DrawStyle)
	}
	if cu.Stacking == nil || cu.Stacking.Mode != "normal" {
		t.Errorf("stacking = %+v, want normal", cu.Stacking)
	}
	if cu.FillOpacity != 80 {
		t.Errorf("fillOpacity = %d, want 80", cu.FillOpacity)
	}
	// currency_usd → currencyUSD with 2 decimals; YTitle → axis label.
	if p.FieldConfig.Defaults.Unit != "currencyUSD" {
		t.Errorf("unit = %q, want currencyUSD", p.FieldConfig.Defaults.Unit)
	}
	if cu.AxisLabel != "USD / bead" {
		t.Errorf("axisLabel = %q, want USD / bead", cu.AxisLabel)
	}

	// One grouped primary series, refId A, group_left() PromQL rendered at the
	// default roll-up (role), legend interpolated from that label.
	//
	// NUMERATOR (ka-0000002556): the by-{role,project,model} spend numerator is
	// the LIFETIME real-bill tag gauge gardenio_tag_cost_usd{dim=<rollup>}, keyed
	// onto a label named after the roll-up dim via label_replace(value->rollup),
	// OR'd with the per-agent per-bucket flow so a missing key (backfill window,
	// or rollup=agent which the tag gauge excludes) self-heals to the per-agent
	// gauge — the same self-healing shape the /costs rollup + Grafana panels 5/6
	// use. The whole `or` is PARENTHESIZED so PromQL's looser-binding `or` stays
	// the numerator both arms of the fleet-scalar ratio share; the denominator and
	// its on() group_left() broadcast are unchanged.
	//
	// The whole ratio is then wrapped in topk(10, …) for the Grafana surface
	// (ka-0000002132): this is a RollupOther chart, so GeneratePanel bakes the
	// same top_n cap the HTTP/iOS path applies. On the default role rollup
	// (≤ a handful of series) topk(10, …) is a no-op; it only bites the
	// high-cardinality agent rollup — matching topNSeriesWithOther exactly.
	if len(p.Targets) != 1 {
		t.Fatalf("got %d targets, want 1: %+v", len(p.Targets), p.Targets)
	}
	a := findTarget(t, p, "A")
	want := `topk(10, (sum by(role)(increase((label_replace(max by (dim, value, env) (gardenio_tag_cost_usd{dim="role"}), "role", "$1", "value", "(.*)"))[$__interval:5m])) or ` +
		`clamp_min(sum by(role)(last_over_time((max by (agent, project, role, model, account) (gardenio_agent_spend_logs_usd{}))[15m:5m])) - ` +
		`sum by(role)(last_over_time((max by (agent, project, role, model, account) (gardenio_agent_spend_logs_usd{}))[15m:5m] offset $__interval)), 0)) ` +
		`/ on() group_left() (sum(increase(gardenio_beads_events_total{event="closed"}[$__interval])) > 0))`
	if a.Expr != want {
		t.Errorf("refId A expr =\n  %q\nwant\n  %q", a.Expr, want)
	}
	if a.LegendFormat != "{{ role }}" {
		t.Errorf("refId A legend = %q, want {{ role }}", a.LegendFormat)
	}
	// Subtitle surfaces the top_n + roll-up dimension.
	if want := "USD per closed bead — top 10 by role"; p.Description != want {
		t.Errorf("description = %q, want %q", p.Description, want)
	}

	if _, err := json.Marshal(p); err != nil {
		t.Fatalf("marshal panel: %v", err)
	}
}

// TestGeneratePanel_Deterministic asserts repeated generation is byte-identical
// — the generator must be pure (no clock, no map-order leak) so a drift guard
// can compare generator(catalog) against a committed model.
func TestGeneratePanel_Deterministic(t *testing.T) {
	for _, id := range []string{"fleet-spend-rate", "agent-spend-over-time"} {
		def := catalogDef(t, id)
		p1, err := GeneratePanel(def)
		if err != nil {
			t.Fatalf("%s: GeneratePanel #1: %v", id, err)
		}
		p2, err := GeneratePanel(def)
		if err != nil {
			t.Fatalf("%s: GeneratePanel #2: %v", id, err)
		}
		j1, _ := json.Marshal(p1)
		j2, _ := json.Marshal(p2)
		if string(j1) != string(j2) {
			t.Errorf("%s: non-deterministic output:\n#1 %s\n#2 %s", id, j1, j2)
		}
	}
}

// TestGrafanaUnit pins the YFormat→unit mapping the task enumerates.
func TestGrafanaUnit(t *testing.T) {
	cases := map[string]string{
		"rate_per_hour":    "currencyUSD",
		"currency_usd":     "currencyUSD",
		"count":            "none",
		"rate":             "short",
		"duration_seconds": "s",
		"duration_s":       "s",
		"duration_ms":      "ms",
		"ratio":            "percentunit",
		"":                 "short",
	}
	for in, want := range cases {
		if got := grafanaUnit(in); got != want {
			t.Errorf("grafanaUnit(%q) = %q, want %q", in, got, want)
		}
	}
}
