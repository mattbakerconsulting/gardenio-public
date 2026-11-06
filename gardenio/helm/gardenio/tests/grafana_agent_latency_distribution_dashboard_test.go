package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Agent Latency & Distribution dashboard (feature ka-0000001465).
//
// Draws the five dense _bucket histogram families the GRADE audit
// (2026-08-16) flagged as the biggest Depth gap — LLM request, tool-call,
// session-duration, deferred-wait, and semantic-search latency — as a
// distribution HEATMAP paired with a p50/p90/p99 percentile FAN each, plus the
// live authz-denial signal (idea #6). These tests guard the JSON content read
// straight from the dashboards/ source dir (the same bytes the grafana-bridge
// image ships and backfills into a type=dashboard bead), mirroring the
// fleet-lifecycle content guards.

// latencyDistDashboard parses the shipped board via the shared helpers.
func latencyDistDashboard(t *testing.T) dashboard {
	t.Helper()
	raw := readDashboardJSON(t, "agent-latency-distribution.json")
	return parseDashboard(t, map[string]string{"agent-latency-distribution.json": raw},
		"agent-latency-distribution.json")
}

func TestAgentLatencyDistributionDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	d := latencyDistDashboard(t)
	if d.UID != "gardenio-agent-latency-distribution" {
		t.Errorf("latency-distribution board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("latency-distribution board must have panels")
	}
}

// Every one of the five _bucket families must be queried somewhere on the board
// — this is the acceptance criterion "test with real session data" pinned to the
// exact VM-grounded metric names the idea catalog verified live.
func TestAgentLatencyDistributionDashboard_AllFiveBucketFamiliesPresent(t *testing.T) {
	t.Parallel()
	exprs := allExprs(latencyDistDashboard(t))
	for _, m := range []string{
		"gardenio_agent_llm_request_latency_seconds_bucket",
		"gardenio_agent_tool_call_latency_seconds_bucket",
		"gardenio_agent_session_duration_seconds_bucket",
		"gardenio_agent_deferred_wait_seconds_bucket",
		"gardenio_beads_semantic_search_duration_seconds_bucket",
	} {
		if !strings.Contains(exprs, m) {
			t.Errorf("latency-distribution board must query %s somewhere; not found", m)
		}
	}
	// Idea #6: the live authz-denial signal no other board draws.
	if !strings.Contains(exprs, "gardenio_authz_denials_total") {
		t.Error("latency-distribution board must draw the authz-denial signal (idea #6)")
	}
}

// Acceptance criterion #1: a heatmap of the latency distribution over time
// (colour = latency density). Every _bucket family that carries a real
// time-bucketed distribution must be drawn as a heatmap using the standard
// sum-by-le rate expression.
func TestAgentLatencyDistributionDashboard_Heatmaps(t *testing.T) {
	t.Parallel()
	d := latencyDistDashboard(t)
	heatmapMetrics := map[string]bool{
		"gardenio_agent_llm_request_latency_seconds_bucket":      false,
		"gardenio_agent_tool_call_latency_seconds_bucket":        false,
		"gardenio_agent_session_duration_seconds_bucket":         false,
		"gardenio_beads_semantic_search_duration_seconds_bucket": false,
	}
	nHeatmaps := 0
	for _, p := range d.Panels {
		if p.Type != "heatmap" {
			continue
		}
		nHeatmaps++
		for _, tgt := range p.Targets {
			for m := range heatmapMetrics {
				if strings.Contains(tgt.Expr, "sum by (le) (rate("+m) {
					heatmapMetrics[m] = true
				}
			}
		}
	}
	if nHeatmaps < 4 {
		t.Errorf("board must have >=4 latency heatmaps; got %d", nHeatmaps)
	}
	for m, ok := range heatmapMetrics {
		if !ok {
			t.Errorf("no heatmap draws the sum-by-le rate distribution of %s", m)
		}
	}
}

// Acceptance criterion #2: percentile-fan (ribbon) with transparent p50/p90/p99
// bands. Each fan must compute all three quantiles via histogram_quantile, and
// the ribbon effect must be wired via custom.fillBelowTo (p90→p50, p99→p90).
func TestAgentLatencyDistributionDashboard_PercentileFans(t *testing.T) {
	t.Parallel()
	raw := readDashboardJSON(t, "agent-latency-distribution.json")
	for _, want := range []string{
		"histogram_quantile(0.5,",
		"histogram_quantile(0.9,",
		"histogram_quantile(0.99,",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("percentile fans must compute %s ...; not found", want)
		}
	}
	// The transparent ribbon between bands is produced by fillBelowTo overrides
	// referencing the lower percentile series by legend name.
	if !strings.Contains(raw, "fillBelowTo") {
		t.Error("percentile fans must use custom.fillBelowTo to paint the transparent ribbon bands")
	}
	if !strings.Contains(raw, `"p50"`) || !strings.Contains(raw, `"p90"`) || !strings.Contains(raw, `"p99"`) {
		t.Error("percentile fans must name p50/p90/p99 series (used by the fillBelowTo ribbon)")
	}

	// At least four fan panels — one per bucket family that supports quantiles.
	d := latencyDistDashboard(t)
	nFans := 0
	for _, p := range d.Panels {
		if p.Type != "timeseries" {
			continue
		}
		var has50, has90, has99 bool
		for _, tgt := range p.Targets {
			if strings.Contains(tgt.Expr, "histogram_quantile(0.5,") {
				has50 = true
			}
			if strings.Contains(tgt.Expr, "histogram_quantile(0.9,") {
				has90 = true
			}
			if strings.Contains(tgt.Expr, "histogram_quantile(0.99,") {
				has99 = true
			}
		}
		if has50 && has90 && has99 {
			nFans++
		}
	}
	if nFans < 4 {
		t.Errorf("board must have >=4 p50/p90/p99 percentile-fan panels; got %d", nFans)
	}
}

// Acceptance criterion #4 (deferred-wait SLO, idea #4): the deferred-wait panel
// must expose p95 against an SLO threshold line so an operator can read whether
// the wait is within budget.
func TestAgentLatencyDistributionDashboard_DeferredWaitSLO(t *testing.T) {
	t.Parallel()
	raw := readDashboardJSON(t, "agent-latency-distribution.json")
	if !strings.Contains(raw, "histogram_quantile(0.95, sum by (le) (rate(gardenio_agent_deferred_wait_seconds_bucket") {
		t.Error("deferred-wait SLO panel must compute p95 over gardenio_agent_deferred_wait_seconds_bucket")
	}
	// The SLO panel carries a dashed threshold step (30-min resume SLO = 1800s).
	if !strings.Contains(raw, `"thresholdsStyle":{"mode":"dashed"}`) {
		t.Error("deferred-wait SLO panel must draw a dashed SLO threshold line")
	}
}

// The deferred-wait row queries gardenio_agent_deferred_wait_seconds_bucket,
// which has zero series in gardenio VM (verified live 2026-08-18): the emitter is
// wired (reconciler observes wait time on a pool_exhausted deferral) but no
// deferral has ever fired, so the fleet has always had pod capacity. Both
// deferred-wait panels must therefore carry an honest noValue explainer — a bare
// "No data" leaves an operator unable to tell whether the SLO is met,
// un-instrumented, or broken. This mirrors the dark-board pattern
// (fleet-lifecycle freeze panels, vpa-right-sizing, eks boards).
func TestAgentLatencyDistributionDashboard_DeferredWaitHonestNoValue(t *testing.T) {
	t.Parallel()
	raw := readDashboardJSON(t, "agent-latency-distribution.json")

	// Both deferred-wait panels (p95-vs-SLO and the p50/p90/p99 fan) must carry a
	// noValue explainer; a bare grafana "No data" is indistinguishable from a
	// broken query. Count the occurrences so a regression that drops one is caught.
	if n := strings.Count(raw, `"noValue":`); n < 2 {
		t.Errorf("both deferred-wait panels must carry an honest noValue annotation; found %d noValue field(s)", n)
	}
	// The explainer must state the emitter is wired-but-idle (the verified truth:
	// gardenio_agent_deferred_wait_seconds is emitted on a pool_exhausted deferral,
	// which has never fired), not merely "no data".
	if !strings.Contains(raw, "emitter is wired") {
		t.Error("deferred-wait noValue must honestly explain the emitter is wired but no deferral has fired (mirrors the fleet-lifecycle freeze / vpa dark-board pattern)")
	}
}

// P3 acceptance (gd-RpflV68KX1): "board panel with overhead p50/p95 per hook
// command". One panel must plot both quantiles per phase (= per hook command)
// over gardenio_tool_harness_overhead_seconds_bucket, with the 1s alert line.
func TestAgentLatencyDistributionDashboard_HarnessOverheadP50P95PerPhase(t *testing.T) {
	t.Parallel()
	var doc struct {
		Panels []struct {
			Title       string `json:"title"`
			Type        string `json:"type"`
			Description string `json:"description"`
			FieldConfig struct {
				Defaults struct {
					NoValue    string `json:"noValue"`
					Thresholds struct {
						Steps []struct {
							Value *float64 `json:"value"`
						} `json:"steps"`
					} `json:"thresholds"`
				} `json:"defaults"`
			} `json:"fieldConfig"`
			Targets []struct {
				Expr         string `json:"expr"`
				LegendFormat string `json:"legendFormat"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(readDashboardJSON(t, "agent-latency-distribution.json")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, p := range doc.Panels {
		if p.Title != "Harness overhead per phase (p50 / p95)" {
			continue
		}
		if p.Type != "timeseries" {
			t.Errorf("panel type = %q, want timeseries", p.Type)
		}
		wantQ := map[string]string{
			// Split per harness (gd-0D8aGbRwYp): claude | codex | unknown.
			"histogram_quantile(0.5, sum by (le, phase, harness) (rate(gardenio_tool_harness_overhead_seconds_bucket{harness=~\"$harness\"}":  "{{phase}} · {{harness}} p50",
			"histogram_quantile(0.95, sum by (le, phase, harness) (rate(gardenio_tool_harness_overhead_seconds_bucket{harness=~\"$harness\"}": "{{phase}} · {{harness}} p95",
		}
		for expr, legend := range wantQ {
			ok := false
			for _, tg := range p.Targets {
				if strings.HasPrefix(tg.Expr, expr) && tg.LegendFormat == legend {
					ok = true
				}
			}
			if !ok {
				t.Errorf("panel needs a target starting %q with legend %q; got %+v", expr, legend, p.Targets)
			}
		}
		red := false
		for _, st := range p.FieldConfig.Defaults.Thresholds.Steps {
			if st.Value != nil && *st.Value == 1 {
				red = true
			}
		}
		if !red {
			t.Error("panel must carry a threshold step at 1s (the HarnessOverheadP95High gate)")
		}
		for _, phase := range []string{"relay", "bus_publish", "drain_queue"} {
			if !strings.Contains(p.Description, phase) {
				t.Errorf("description must explain phase %q", phase)
			}
		}
		if strings.Contains(p.FieldConfig.Defaults.NoValue, "not yet deployed") {
			t.Error("noValue still claims P3 is undeployed")
		}
		return
	}
	t.Fatal("board must carry the P3 harness-overhead p50/p95-per-phase panel")
}
