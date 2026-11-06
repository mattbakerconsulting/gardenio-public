package tests

import (
	"strings"
	"testing"
)

// Latency & Cost Efficiency dashboard (feature ka-0000003414, GRADE 2026-08-20
// depth iteration; idea catalog
// gardenio/docs/grafana-grade/2026-08-20T07:23:00Z/idea-catalog.md).
//
// This board draws the catalog's live-but-undrawn latency histograms beside the
// cost/cache efficiency levers, so the two questions an operator asks about a
// slow, expensive fleet share one screen:
//   - #1 User-request latency heatmap (gardenio_user_request_latency_seconds):
//     the end-to-end tail as a distribution, not a smeared p95 line.
//   - #2 Reference-extract percentile fan
//     (gardenio_beads_references_extract_duration_seconds): is typed-mention
//     extraction keeping up as the bead graph grows?
//   - #3 Mention cold-respawn SLO (gardenio_mention_cold_respawn_seconds): @-mention
//     a dark agent → seconds until live, with a 45s SLO line.
//   - #4 Respawn phase breakdown (gardenio_slackbridge_respawn_phase_seconds):
//     which phase (dispatch vs boot) dominates thread-reply latency.
//   - #5 Account cache-efficiency leaderboard (cache_read / input tokens): the
//     prompt-cache cost lever, per account.
//   - #6 Pod-phase transition timeline
//     (gardenio_agent_pod_phase_transitions_total): where pods move in the
//     lifecycle over time, keyed on phase_from×phase_to.
//   - #7 Deferred-wait SLO fan (gardenio_agent_deferred_wait_seconds) with a 1h line.
//   - #8 Jira issue-age heatmap (gardenio_jira_issue_age_seconds).
//   - #9 Cost-per-bead-closed by project (spend / beads closed).
//   - #10 Compaction pressure vs median session duration (the context tax).
//   - #11 Operator-activity leaderboard (gardenio_beads_created_by_operator_total).
//   - #12 Upstream-error TRANSITION rate
//     (gardenio_agent_upstream_error_transitions_total): the smoother leading
//     indicator the catalog flags over the intermittent _active gauge.
//
// Every metric name below is the exact series the controller/kbeads emit and was
// verified to have live series in gardenio-vmsingle during the GRADE iteration.
// The dashboards/*.json files are the source of truth, baked into the
// grafana-bridge image (/dashboards); a rename on either the emitter or the board
// without updating the other is exactly the silent panel-drop these tests catch.

const latencyCostKey = "gardenio-latency-cost-efficiency.json"

func TestLatencyCostDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, latencyCostKey)
	if d.UID != "gardenio-latency-cost-efficiency" {
		t.Errorf("latency-cost board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("latency-cost board must have panels")
	}
}

// TestLatencyCostDashboard_ChartsAllTwelveSignals guards that every one of the
// twelve grounded catalog ideas the board exists to front stays charted.
func TestLatencyCostDashboard_ChartsAllTwelveSignals(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, latencyCostKey))

	wantMetric := []string{
		"gardenio_user_request_latency_seconds_bucket",              // #1
		"gardenio_beads_references_extract_duration_seconds_bucket", // #2
		"gardenio_mention_cold_respawn_seconds_bucket",              // #3
		"gardenio_slackbridge_respawn_phase_seconds_bucket",         // #4
		"gardenio_account_cache_read_tokens_current",                // #5
		"gardenio_account_input_tokens_current",                     // #5
		"gardenio_agent_pod_phase_transitions_total",                // #6
		"gardenio_agent_deferred_wait_seconds_bucket",               // #7
		"gardenio_jira_issue_age_seconds_bucket",                    // #8
		"gardenio_agent_spend_usd_total",                            // #9
		"gardenio_agent_compactions_current",                        // #10
		"gardenio_agent_session_duration_seconds_bucket",            // #10
		"gardenio_beads_created_by_operator_total",                  // #11
		"gardenio_agent_upstream_error_transitions_total",           // #12
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("latency-cost board must query %s somewhere; not found", m)
		}
	}
}

// TestLatencyCostDashboard_LatencyPanelsUseHistogramQuantile guards that the
// latency-SLO panels (#2, #3, #4, #7) compute percentiles over their bucket
// families rather than averaging — averaging a latency histogram hides exactly
// the tail these panels exist to surface.
func TestLatencyCostDashboard_LatencyPanelsUseHistogramQuantile(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, latencyCostKey))

	for _, want := range []string{
		"histogram_quantile(0.95, sum(rate(gardenio_beads_references_extract_duration_seconds_bucket",
		"histogram_quantile(0.95, sum(rate(gardenio_mention_cold_respawn_seconds_bucket",
		"histogram_quantile(0.90, sum(rate(gardenio_slackbridge_respawn_phase_seconds_bucket",
		"histogram_quantile(0.99, sum(rate(gardenio_agent_deferred_wait_seconds_bucket",
	} {
		if !strings.Contains(exprs, want) {
			t.Errorf("latency-cost board must compute a percentile tail; missing %q", want)
		}
	}
}

// TestLatencyCostDashboard_HeatmapsFormatByLe guards that the distribution
// heatmaps (#1 user-request, #8 jira-age) aggregate over the le bucket label and
// render as heatmaps — the shape that makes a widening tail visible as a drifting
// bright band.
func TestLatencyCostDashboard_HeatmapsFormatByLe(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, latencyCostKey)

	var userReqHeatmap, jiraAgeHeatmap bool
	for _, p := range d.Panels {
		if p.Type != "heatmap" {
			continue
		}
		for _, tgt := range p.Targets {
			if !strings.Contains(tgt.Expr, "by (le)") {
				continue
			}
			if strings.Contains(tgt.Expr, "gardenio_user_request_latency_seconds_bucket") {
				userReqHeatmap = true
			}
			if strings.Contains(tgt.Expr, "gardenio_jira_issue_age_seconds_bucket") {
				jiraAgeHeatmap = true
			}
		}
	}
	if !userReqHeatmap {
		t.Error("#1 user-request latency must render as a heatmap aggregating by (le)")
	}
	if !jiraAgeHeatmap {
		t.Error("#8 jira issue-age must render as a heatmap aggregating by (le)")
	}
}

// TestLatencyCostDashboard_CacheEfficiencyIsARatio guards the cost lever: #5 is a
// cache-read/input ratio (clamp_min'd against divide-by-zero), not a raw token
// count — the raw counts are already on account-capacity; the ratio is what makes
// it a cost lever.
func TestLatencyCostDashboard_CacheEfficiencyIsARatio(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, latencyCostKey))

	if !strings.Contains(exprs, "gardenio_account_cache_read_tokens_current") ||
		!strings.Contains(exprs, "clamp_min(sum(gardenio_account_input_tokens_current") {
		t.Error("#5 cache-efficiency must be a cache_read / clamp_min(input) ratio")
	}
}

// TestLatencyCostDashboard_CostPerBeadDividesByClosed guards the #9 efficiency
// ratio shape: spend rate over the closed-events rate, clamp_min'd so a project
// that closed nothing this window does not divide by zero.
func TestLatencyCostDashboard_CostPerBeadDividesByClosed(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, latencyCostKey))

	if !strings.Contains(exprs, "clamp_min(sum(rate(gardenio_beads_events_total{event=\"closed\"}[1h])) by (project),0.001)") {
		t.Error("#9 cost-per-bead must divide project spend by the clamp_min'd closed-events rate")
	}
}

// TestLatencyCostDashboard_UpstreamErrorUsesTransitionCounter guards catalog #12's
// core point: the leading indicator is the edge-triggered *_transitions_total
// counter (via rate), NOT the intermittent point-in-time *_active gauge.
func TestLatencyCostDashboard_UpstreamErrorUsesTransitionCounter(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, latencyCostKey))

	if !strings.Contains(exprs, "rate(gardenio_agent_upstream_error_transitions_total") {
		t.Error("#12 must rate() the transitions counter (the smoother leading indicator)")
	}
	if strings.Contains(exprs, "gardenio_agent_upstream_error_active") {
		t.Error("#12 must not read the intermittent _active gauge (catalog flags the transition counter as the leading indicator)")
	}
}
