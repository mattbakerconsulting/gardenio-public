package server

// Tests for the ka-0000004779 spend-rate fix: the fleet-spend-rate chart on the
// LiteLLM account-cost gauge, the per-agent-over-time stacked chart with an
// "other" rollup, and the increase()/WindowHours rate form that makes the
// instant headline agree with the range chart.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFleetSpendRate_UsesAccountCostIncreaseOverHours guards that the fleet
// rate chart reads the LiteLLM account-cost gauge with increase()/WindowHours
// (not rate()*3600) — the form that yields the same value as an instant query
// and a range query, so the /costs headline and the chart's last point agree.
func TestFleetSpendRate_UsesAccountCostIncreaseOverHours(t *testing.T) {
	def := chartCatalog()["fleet-spend-rate"]
	if def.ID == "" {
		t.Fatal("fleet-spend-rate chart missing from catalog")
	}
	q := def.QueryTemplate
	if !strings.Contains(q, "gardenio_account_cost_usd") {
		t.Errorf("fleet-spend-rate must read the LiteLLM account-cost gauge:\n  %s", q)
	}
	if !strings.Contains(q, "increase(") || !strings.Contains(q, "{{.WindowHours}}") {
		t.Errorf("fleet-spend-rate must use increase()/WindowHours, not rate()*3600:\n  %s", q)
	}
	if strings.Contains(q, "rate(") {
		t.Errorf("fleet-spend-rate must not use rate() (instant!=range on counters):\n  %s", q)
	}
}

// TestFleetSpendRate_Has1hCompanionLine guards that the fleet-spend-rate chart
// draws a fixed 1-hour rolling spend line alongside the {{.Window}} primary,
// on the SAME left USD/hour axis (gd-Uvo8TZ8Kei). The operator asked to "show
// 1h rolling cost as well as 24h" — this is the chart counterpart to the
// /costs headline that already shows both windows.
func TestFleetSpendRate_Has1hCompanionLine(t *testing.T) {
	def := chartCatalog()["fleet-spend-rate"]
	if def.ID == "" {
		t.Fatal("fleet-spend-rate chart missing from catalog")
	}
	var oneH *ExtraSeriesDef
	for i := range def.ExtraSeries {
		if def.ExtraSeries[i].ValueField == "usd_per_hour_1h" {
			oneH = &def.ExtraSeries[i]
			break
		}
	}
	if oneH == nil {
		t.Fatalf("fleet-spend-rate must carry a 1h spend companion series "+
			"(value field usd_per_hour_1h); overlays = %+v", def.ExtraSeries)
	}
	q := oneH.QueryTemplate
	if !strings.Contains(q, "gardenio_account_cost_usd") {
		t.Errorf("1h companion must read the LiteLLM account-cost gauge:\n  %s", q)
	}
	// Fixed [1h:5m] aligned subquery window, independent of the ?window= param —
	// it is the always-1h line, so it must NOT template {{.Window}}. The :5m
	// subquery grid + max-by-dedup are the ka-0000000170 de-bump (per-pod gauge
	// dedup + per-step extrapolation removal).
	if !strings.Contains(q, "[1h:5m]") {
		t.Errorf("1h companion must use a fixed [1h:5m] aligned-subquery window:\n  %s", q)
	}
	if !strings.Contains(q, "max by (account, env)") {
		t.Errorf("1h companion must dedup per-pod gauge duplicates with max by (account, env):\n  %s", q)
	}
	if strings.Contains(q, "{{.Window}}") {
		t.Errorf("1h companion must NOT move with the ?window= param:\n  %s", q)
	}
	// increase()/divisor (not rate()*3600) keeps it instant==range stable, same
	// as the primary (ka-0000004779).
	if !strings.Contains(q, "increase(") || strings.Contains(q, "rate(") {
		t.Errorf("1h companion must use increase(), not rate():\n  %s", q)
	}
	// Drawn on the primary USD/hour axis (left), NOT the secondary agent-count
	// axis — it is a spend rate in the same units as the primary line.
	if oneH.YAxisIndex != 0 {
		t.Errorf("1h companion must bind to the primary USD/hour axis (YAxisIndex 0), got %d", oneH.YAxisIndex)
	}
	if oneH.YFormat != "rate_per_hour" {
		t.Errorf("1h companion YFormat = %q, want rate_per_hour", oneH.YFormat)
	}
	// It is fleet/account/env-scoped like the primary; the agent-gauge labels
	// must not leak into its selector.
	if oneH.AllowedLabels["project"] || oneH.AllowedLabels["role"] || oneH.AllowedLabels["state"] {
		t.Errorf("1h companion must not admit agent-gauge labels: %+v", oneH.AllowedLabels)
	}
}

// TestAgentSpendCumulative_ReadsRealBillLevel guards the re-point (ka-0000001663)
// of the former hourly-rate-rolling-window chart. Its old USD/hour RATE rode the
// dead at-close counter gardenio_agent_spend_usd_total (~1000x low), and that
// counter's real-bill replacement gardenio_agent_spend_logs_usd is a Reset+Set
// LEVEL gauge with no honest rate (increase()/rate()/deriv() read garbage; a
// windowed delta goes negative as agents age out; the ~48h lookback cannot honor
// 7d). So the chart is re-cast as the project/agent-scoped cumulative real-bill
// LEVEL: it reads the real-bill gauge as a deduped, gap-bridged last_over_time
// level, never increase() and never the dead counter.
func TestAgentSpendCumulative_ReadsRealBillLevel(t *testing.T) {
	def := chartCatalog()["agent-spend-cumulative"]
	if def.ID == "" {
		t.Fatal("agent-spend-cumulative chart missing from catalog")
	}
	q := def.QueryTemplate
	if !strings.Contains(q, "gardenio_agent_spend_logs_usd") {
		t.Errorf("agent-spend-cumulative must read the real-bill per-agent gauge:\n  %s", q)
	}
	if strings.Contains(q, "gardenio_agent_spend_usd_total") {
		t.Errorf("agent-spend-cumulative must NOT read the dead at-close counter (~1000x low):\n  %s", q)
	}
	// A Reset+Set gauge has no honest rate — the LEVEL is read with last_over_time,
	// never increase()/rate()/deriv().
	if !strings.Contains(q, "last_over_time(") {
		t.Errorf("agent-spend-cumulative must read the gauge LEVEL with last_over_time:\n  %s", q)
	}
	if strings.Contains(q, "increase(") || strings.Contains(q, "rate(") || strings.Contains(q, "deriv(") {
		t.Errorf("agent-spend-cumulative must not use increase()/rate()/deriv() on the Reset+Set gauge:\n  %s", q)
	}
	// Deduped by the gauge's full identity so an HA double-scrape can't inflate it.
	if !strings.Contains(q, "max by (agent, project, role, model, account)") {
		t.Errorf("agent-spend-cumulative must dedup the gauge by its full label set:\n  %s", q)
	}
	// It is a cumulative USD level now, not a USD/hour rate.
	if def.YFormat != "currency_usd" {
		t.Errorf("agent-spend-cumulative YFormat = %q, want currency_usd (it is a level, not a rate)", def.YFormat)
	}
}

// TestWindowHours maps the rolling-window enum to its hour count.
func TestWindowHours(t *testing.T) {
	cases := map[string]string{"1d": "24", "7d": "168", "12h": "12", "": "24", "bogus": "24"}
	for in, want := range cases {
		if got := windowHours(in); got != want {
			t.Errorf("windowHours(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAgentSpendOverTime_RollupOther drives the new stacked per-agent chart end
// to end: with top_n=2 over three agents, the two largest are kept and the third
// is folded into a single "other" series whose per-bucket value equals the tail
// sum — so the stack still sums to the fleet total.
func TestAgentSpendOverTime_RollupOther(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// Three agents: big=10/20, mid=5/8, small=1/2. With top_n=2, small folds
	// into "other" (=1 then 2). reshapeLongFormat keys on the "agent" label.
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[%s,%s,%s]}}`,
		matrixSeries("agent", "big", start, step, "10", "20"),
		matrixSeries("agent", "mid", start, step, "5", "8"),
		matrixSeries("agent", "small", start, step, "1", "2"),
	)
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "gardenio_agent_spend_logs_usd", body: body}))

	def := chartCatalog()["agent-spend-over-time"]
	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Echo:    map[string]any{"top_n": 2},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	// Collect the per-agent series keys present in the data.
	seen := map[string]bool{}
	otherByTS := map[string]float64{}
	for _, row := range render.Data {
		ag, _ := row["agent"].(string)
		seen[ag] = true
		if ag == "other" {
			ts, _ := row["t"].(string)
			v, _ := row["spend_usd"].(float64)
			otherByTS[ts] = v
		}
	}
	if !seen["big"] || !seen["mid"] {
		t.Errorf("expected top-2 agents big+mid kept, got keys %v", seen)
	}
	if seen["small"] {
		t.Errorf("small should have been folded into other, not kept: %v", seen)
	}
	if !seen["other"] {
		t.Fatalf("expected an 'other' rollup series, got keys %v", seen)
	}
	// The folded tail is just `small` here: 1 then 2.
	if len(otherByTS) != 2 {
		t.Errorf("expected 2 'other' buckets, got %d (%v)", len(otherByTS), otherByTS)
	}
	for _, v := range otherByTS {
		if v != 1 && v != 2 {
			t.Errorf("unexpected 'other' bucket value %v (want 1 or 2)", v)
		}
	}
}

// TestSpendByAccount_Is1hRate guards the ka-0000007972 change: the
// spend-by-account chart is a per-account 1h-rolling spend RATE in USD/hour
// (like the fleet-spend-rate chart), NOT raw USD accumulated per {{.Step}}
// bucket. The fixed [1h] window makes the y-axis range-stable (invariant to
// dashboard zoom), and the increase()/1 form keeps it instant==range stable,
// so the per-account stack reconciles with the fleet "spend (1h)" line.
func TestSpendByAccount_Is1hRate(t *testing.T) {
	def := chartCatalog()["spend-by-account"]
	if def.ID == "" {
		t.Fatal("spend-by-account chart missing from catalog")
	}
	q := def.QueryTemplate
	if !strings.Contains(q, "gardenio_account_cost_usd") {
		t.Errorf("spend-by-account must read the LiteLLM account-cost gauge:\n  %s", q)
	}
	// Fixed [1h:5m] aligned-subquery window — the literal "1h rate" the operator
	// asked for. It must NOT plot raw USD per {{.Step}} bucket (the old,
	// zoom-dependent form). The :5m subquery grid + max-by-dedup are the
	// ka-0000000170 de-bump shared with the fleet-spend-rate chart.
	if !strings.Contains(q, "[1h:5m]") {
		t.Errorf("spend-by-account must use a fixed [1h:5m] aligned-subquery window:\n  %s", q)
	}
	if !strings.Contains(q, "max by (account, env)") {
		t.Errorf("spend-by-account must dedup per-pod gauge duplicates with max by (account, env):\n  %s", q)
	}
	if strings.Contains(q, "{{.Step}}") {
		t.Errorf("spend-by-account must NOT use the {{.Step}} bucket window — that "+
			"rescales with zoom; it must be a fixed 1h rate:\n  %s", q)
	}
	// increase()/1 (not rate()*3600) keeps the per-point value instant==range
	// stable, same discipline as the fleet rate (ka-0000004779).
	if !strings.Contains(q, "increase(") || strings.Contains(q, "rate(") {
		t.Errorf("spend-by-account must use increase(), not rate():\n  %s", q)
	}
	// Still broken out per credential account, with the long tail rolled up.
	if def.SeriesField != "account" || !def.RollupOther {
		t.Errorf("spend-by-account must group by(account) with RollupOther: SeriesField=%q RollupOther=%v",
			def.SeriesField, def.RollupOther)
	}
	// Rendered in USD/hour rate units, matching the fleet rate panel.
	if def.YFormat != "rate_per_hour" {
		t.Errorf("spend-by-account YFormat = %q, want rate_per_hour", def.YFormat)
	}
	if def.YTitle != "USD/hour" {
		t.Errorf("spend-by-account YTitle = %q, want USD/hour", def.YTitle)
	}
	if def.ValueField != "usd_per_hour" {
		t.Errorf("spend-by-account ValueField = %q, want usd_per_hour", def.ValueField)
	}
}

// TestSpendByAccount_RollupOther drives the rate chart end to end: with top_n=2
// over three accounts, the two largest are kept and the third folds into a
// single "other" series. Summing rates across the tail is still a rate, so the
// stacked total stays correct even though the values are USD/hour, not raw USD.
func TestSpendByAccount_RollupOther(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// Three accounts: big=10/20, mid=5/8, small=1/2. With top_n=2, small folds
	// into "other" (=1 then 2). reshapeLongFormat keys on the "account" label.
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[%s,%s,%s]}}`,
		matrixSeries("account", "big", start, step, "10", "20"),
		matrixSeries("account", "mid", start, step, "5", "8"),
		matrixSeries("account", "small", start, step, "1", "2"),
	)
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "gardenio_account_cost_usd", body: body}))

	def := chartCatalog()["spend-by-account"]
	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Echo:    map[string]any{"top_n": 2},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	seen := map[string]bool{}
	otherByTS := map[string]float64{}
	for _, row := range render.Data {
		acct, _ := row["account"].(string)
		seen[acct] = true
		if acct == "other" {
			ts, _ := row["t"].(string)
			// Values are emitted under the chart's ValueField (usd_per_hour).
			v, _ := row["usd_per_hour"].(float64)
			otherByTS[ts] = v
		}
	}
	if !seen["big"] || !seen["mid"] {
		t.Errorf("expected top-2 accounts big+mid kept, got keys %v", seen)
	}
	if seen["small"] {
		t.Errorf("small should have been folded into other, not kept: %v", seen)
	}
	if !seen["other"] {
		t.Fatalf("expected an 'other' rollup series, got keys %v", seen)
	}
	// The folded tail is just `small` here: 1 then 2.
	if len(otherByTS) != 2 {
		t.Errorf("expected 2 'other' buckets, got %d (%v)", len(otherByTS), otherByTS)
	}
	for _, v := range otherByTS {
		if v != 1 && v != 2 {
			t.Errorf("unexpected 'other' bucket value %v (want 1 or 2)", v)
		}
	}
}

// TestSpendByModel_ReadsRealBillLevel guards the per-model panel (ka-0000003821,
// "spend per model, haiku vs opus") after the re-point (ka-0000001663). It was a
// per-MODEL USD/hour RATE via increase() on the dead at-close counter
// gardenio_agent_spend_usd_total (~1000x low). Its real-bill replacement
// gardenio_agent_spend_logs_usd — the only real-bill spend series carrying a
// `model` label — is a Reset+Set LEVEL gauge on which increase() reads garbage,
// so the chart is re-cast as the per-model cumulative real-bill LEVEL. The
// account-cost gauge the fleet/account panels read has only {account,env}, so it
// still cannot break out by model; that is why this source (not account_cost) is
// used, and why the stack reconciles with the per-agent panel.
func TestSpendByModel_ReadsRealBillLevel(t *testing.T) {
	def := chartCatalog()["spend-by-model"]
	if def.ID == "" {
		t.Fatal("spend-by-model chart missing from catalog")
	}
	q := def.QueryTemplate
	// MUST read the per-agent real-bill gauge (the only real-bill spend metric
	// with a model label), NOT the dead counter, NOT the account-cost gauge.
	if !strings.Contains(q, "gardenio_agent_spend_logs_usd") {
		t.Errorf("spend-by-model must read the per-agent real-bill gauge "+
			"(the only real-bill spend metric with a model label):\n  %s", q)
	}
	if strings.Contains(q, "gardenio_agent_spend_usd_total") {
		t.Errorf("spend-by-model must NOT read the dead at-close counter (~1000x low):\n  %s", q)
	}
	if strings.Contains(q, "gardenio_account_cost_usd") {
		t.Errorf("spend-by-model must NOT read the account-cost gauge — it has no "+
			"model label, so a per-model breakdown is impossible from it:\n  %s", q)
	}
	// Grouped by(model), so haiku/opus are distinct stacked series.
	if !strings.Contains(q, "by(model)") {
		t.Errorf("spend-by-model must aggregate by(model):\n  %s", q)
	}
	// A Reset+Set gauge has no honest rate — the LEVEL is read with last_over_time,
	// never increase()/rate().
	if !strings.Contains(q, "last_over_time(") {
		t.Errorf("spend-by-model must read the gauge LEVEL with last_over_time:\n  %s", q)
	}
	if strings.Contains(q, "increase(") || strings.Contains(q, "rate(") {
		t.Errorf("spend-by-model must not use increase()/rate() on the Reset+Set gauge:\n  %s", q)
	}
	// Broken out per model, long tail rolled up (model cardinality is tiny so
	// "other" rarely appears, but the rollup keeps the stack total correct).
	if def.SeriesField != "model" || !def.RollupOther {
		t.Errorf("spend-by-model must group by(model) with RollupOther: SeriesField=%q RollupOther=%v",
			def.SeriesField, def.RollupOther)
	}
	// Cumulative USD level units now, not a USD/hour rate.
	if def.YFormat != "currency_usd" {
		t.Errorf("spend-by-model YFormat = %q, want currency_usd (it is a level, not a rate)", def.YFormat)
	}
	if def.YTitle != "USD" {
		t.Errorf("spend-by-model YTitle = %q, want USD", def.YTitle)
	}
	if def.ValueField != "spend_usd" {
		t.Errorf("spend-by-model ValueField = %q, want spend_usd", def.ValueField)
	}
}

// TestSpendByModel_RollupOther drives the per-model rate chart end to end: with
// top_n=2 over three models (the live haiku/opus-7/opus-8 shape), the two
// largest are kept and the third folds into a single "other" series. Mirrors
// TestSpendByAccount_RollupOther but keyed on the `model` label and reading the
// per-agent spend counter.
func TestSpendByModel_RollupOther(t *testing.T) {
	start := int64(1700000000)
	step := int64(3600)
	// Three models: opus=10/20, haiku=5/8, oldopus=1/2. With top_n=2, oldopus
	// folds into "other" (=1 then 2). reshapeLongFormat keys on "model".
	body := fmt.Sprintf(`{"status":"success","data":{"resultType":"matrix","result":[%s,%s,%s]}}`,
		matrixSeries("model", "claude-opus-4-8-oauth", start, step, "10", "20"),
		matrixSeries("model", "anthropic/claude-haiku-4-5", start, step, "5", "8"),
		matrixSeries("model", "bedrock-claude-opus-4-7", start, step, "1", "2"),
	)
	s := &BeadsServer{}
	s.SetVMClient(newVMRangeStub(t, vmRule{contains: "gardenio_agent_spend_logs_usd", body: body}))

	def := chartCatalog()["spend-by-model"]
	req := &chartRequest{
		Since:   time.Unix(start, 0).UTC(),
		Until:   time.Unix(start+2*step, 0).UTC(),
		Step:    time.Hour,
		Echo:    map[string]any{"top_n": 2},
		rawVals: map[string][]string{},
	}
	render, err := s.computeChartRender(context.Background(), def, req, time.Unix(start+2*step, 0).UTC())
	if err != nil {
		t.Fatalf("computeChartRender: %v", err)
	}

	seen := map[string]bool{}
	otherByTS := map[string]float64{}
	for _, row := range render.Data {
		model, _ := row["model"].(string)
		seen[model] = true
		if model == "other" {
			ts, _ := row["t"].(string)
			v, _ := row["spend_usd"].(float64)
			otherByTS[ts] = v
		}
	}
	if !seen["claude-opus-4-8-oauth"] || !seen["anthropic/claude-haiku-4-5"] {
		t.Errorf("expected top-2 models (opus-8 + haiku) kept, got keys %v", seen)
	}
	if seen["bedrock-claude-opus-4-7"] {
		t.Errorf("oldopus should have been folded into other, not kept: %v", seen)
	}
	if !seen["other"] {
		t.Fatalf("expected an 'other' rollup series, got keys %v", seen)
	}
	if len(otherByTS) != 2 {
		t.Errorf("expected 2 'other' buckets, got %d (%v)", len(otherByTS), otherByTS)
	}
	for _, v := range otherByTS {
		if v != 1 && v != 2 {
			t.Errorf("unexpected 'other' bucket value %v (want 1 or 2)", v)
		}
	}
}
