package tests

import (
	"strings"
	"testing"
)

// Control-plane liveness & starvation dashboard (gd-aXeoobL6lM, board added in
// ka-0000007265).
//
// This board fronts the failure modes that read GREEN on every other surface
// while work silently stops:
//   - #12 Probe-loop dead-man: the rate-limit probe RunLoop froze for ~24h on
//     2026-07-05 while the controller stayed alive, silently disabling both
//     exhausted-account auto-recovery paths (ka-0000001889). Charted via
//     gardenio_probe_loop_seconds_since_last_tick (freshness gauge) and
//     gardenio_probe_loop_ticks_total (the independent second liveness measure —
//     a flatlined counter is the alertable dead-loop signature).
//   - #4 Trigger starvation: a gardener/trigger wedged at the cooldown gate holds
//     its max_concurrent=1 lease forever — dark for days while enabled=true
//     reads healthy (advice ka-0000002229). Charted via
//     gardenio_trigger_cooldown_skip_streak (the pre-40h-of-missed-fires leading
//     signal) and gardenio_trigger_starved (the verdict).
//   - #1 Upstream-error blast radius: how many agents an Anthropic/upstream flap
//     took down, via gardenio_agent_upstream_error_active (current-state gauge)
//     and gardenio_agent_upstream_error_transitions_total (edge-triggered entries).
//   - #3 Prewarm-assign SLO: is the warm pool actually saving spin-up time, via
//     gardenio_prewarmed_assign_duration_seconds and gardenio_prewarmed_assigns_total.
//
// The dashboards/*.json files are the source of truth, baked into the
// grafana-bridge image (/dashboards) and backfilled into type=dashboard beads at
// runtime (see grafana_fleet_lifecycle_dashboard_test.go for the mechanism).
// Until this test the board — a P2 liveness surface — was the only liveness
// dashboard with no content guard, so a future JSON edit could silently drop a
// panel or fat-finger a metric name and no test would catch it. These tests
// guard that the board keeps its stable uid, valid JSON, and charts each of the
// four liveness signals it exists to front.

const controlPlaneLivenessKey = "gardenio-control-plane-liveness.json"

func TestControlPlaneLivenessDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, controlPlaneLivenessKey)
	if d.UID != "gardenio-control-plane-liveness" {
		t.Errorf("control-plane-liveness board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("control-plane-liveness board must have panels")
	}
}

// TestControlPlaneLivenessDashboard_ChartsAllFourSignals guards that every one
// of the four liveness signals the board exists to front stays charted. Each
// metric name is the exact series the controller emits (namespace "gardenio" +
// the Name in its *Opts); a rename on either side without updating the other is
// exactly the silent-drop this test catches.
func TestControlPlaneLivenessDashboard_ChartsAllFourSignals(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, controlPlaneLivenessKey))

	wantMetric := []string{
		// #12 Probe-loop dead-man — freshness gauge + independent tick counter.
		"gardenio_probe_loop_seconds_since_last_tick",
		"gardenio_probe_loop_ticks_total",
		// #4 Trigger starvation — leading skip-streak + the starved verdict.
		"gardenio_trigger_cooldown_skip_streak",
		"gardenio_trigger_starved",
		// #1 Upstream-error blast radius — current gauge + edge-triggered entries.
		"gardenio_agent_upstream_error_active",
		"gardenio_agent_upstream_error_transitions_total",
		// #3 Prewarm-assign SLO — latency histogram + outcome counter.
		"gardenio_prewarmed_assign_duration_seconds",
		"gardenio_prewarmed_assigns_total",
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("control-plane-liveness board must query %s somewhere; not found", m)
		}
	}
}

// TestControlPlaneLivenessDashboard_ProbeDeadManUsesBothMeasures guards the
// board's core design decision: the probe-loop liveness is measured two
// independent ways (advice ka-0000005924). The dead-man freshness gauge and the
// tick-rate counter must BOTH be present so a single broken derivation can't
// read healthy on the surviving one.
func TestControlPlaneLivenessDashboard_ProbeDeadManUsesBothMeasures(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, controlPlaneLivenessKey)

	var hasGauge, hasRate bool
	for _, p := range d.Panels {
		for _, tgt := range p.Targets {
			if strings.Contains(tgt.Expr, "gardenio_probe_loop_seconds_since_last_tick") {
				hasGauge = true
			}
			// The rate panel is the second, independent measure — a rate() over
			// the monotonic tick counter, not the freshness gauge.
			if strings.Contains(tgt.Expr, "rate(gardenio_probe_loop_ticks_total") {
				hasRate = true
			}
		}
	}
	if !hasGauge {
		t.Error("probe dead-man must chart the freshness gauge gardenio_probe_loop_seconds_since_last_tick")
	}
	if !hasRate {
		t.Error("probe liveness must include the independent second measure rate(gardenio_probe_loop_ticks_total[...])")
	}
}
