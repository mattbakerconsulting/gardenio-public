package tests

import (
	"strings"
	"testing"
)

// Fleet Capacity & Placement dashboard (feature ka-0000000905, GRADE
// 2026-08-19 depth iteration; idea catalog
// gardenio/docs/grafana-grade/2026-08-19T07:23:00Z/idea-catalog.md).
//
// This board is the placement-side companion to control-plane-liveness. It
// answers one operator question — can the fleet PLACE the agents it wants to
// run right now, and is the reconciler that places them alive — by carrying the
// five grounded capacity/placement ideas from the catalog:
//   - #6 Pending-pod saturation (gardenio_agent_pods_pending): headline
//     "we can't place agents" signal + a project×role breakout.
//   - #4 Pod-phase flow (gardenio_agent_pod_phase_transitions_total): a
//     Pending→Running rate view + the full from→to transition matrix, so a
//     stuck-Pending backlog is visible before it is a capacity incident.
//   - #9 Node lifecycle churn (gardenio_node_lifecycle_events_total{reason}):
//     Karpenter/kubelet turning nodes over underneath the fleet — promoted from
//     an involuntary-deaths sub-panel to a first-class capacity view.
//   - #5 Reconcile dead-man (gardenio_reconcile_total): the reconciler that
//     drives placement MUST tick ~1/min; a flatlined counter is placement
//     silently stopping. Charted BOTH as a rate stat AND a by-result rate
//     timeseries — the two-independent-measures principle (advice ka-0000005924).
//   - #1 Compaction pressure leaderboard (gardenio_agent_compactions_current):
//     which agents are burning turns on context compaction and holding capacity;
//     no other board surfaces this PER-AGENT.
//
// Every metric name below is the exact series the controller emits and was
// verified to have live series in gardenio-vmsingle during the GRADE iteration.
// The dashboards/*.json files are the source of truth, baked into the
// grafana-bridge image (/dashboards) and backfilled into type=dashboard beads at
// runtime (see grafana_fleet_lifecycle_dashboard_test.go for the mechanism).
// A rename on either the emitter or the board without updating the other is
// exactly the silent panel-drop these tests catch.

const fleetCapacityKey = "gardenio-fleet-capacity.json"

func TestFleetCapacityDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, fleetCapacityKey)
	if d.UID != "gardenio-fleet-capacity" {
		t.Errorf("fleet-capacity board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("fleet-capacity board must have panels")
	}
}

// TestFleetCapacityDashboard_ChartsAllFiveSignals guards that every one of the
// five grounded capacity/placement signals the board exists to front stays
// charted.
func TestFleetCapacityDashboard_ChartsAllFiveSignals(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, fleetCapacityKey))

	wantMetric := []string{
		// #6 Pending-pod saturation.
		"gardenio_agent_pods_pending",
		// #4 Pod-phase flow.
		"gardenio_agent_pod_phase_transitions_total",
		// #9 Node lifecycle churn.
		"gardenio_node_lifecycle_events_total",
		// #5 Reconcile dead-man.
		"gardenio_reconcile_total",
		// #1 Compaction pressure leaderboard.
		"gardenio_agent_compactions_current",
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("fleet-capacity board must query %s somewhere; not found", m)
		}
	}
}

// TestFleetCapacityDashboard_ReconcileUsesBothMeasures guards the board's core
// liveness design decision: the reconcile-loop liveness is measured two
// independent ways (advice ka-0000005924) — a single-number rate stat AND a
// by-result rate timeseries — so a broken read on one surface can't leave the
// operator believing placement is alive on the other.
func TestFleetCapacityDashboard_ReconcileUsesBothMeasures(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, fleetCapacityKey)

	var hasStat, hasByResult bool
	for _, p := range d.Panels {
		for _, tgt := range p.Targets {
			if !strings.Contains(tgt.Expr, "gardenio_reconcile_total") {
				continue
			}
			// The by-result timeseries is the second, independent measure.
			if strings.Contains(tgt.Expr, "by (result)") {
				hasByResult = true
			} else {
				hasStat = true
			}
		}
	}
	if !hasStat {
		t.Error("reconcile dead-man must chart the aggregate rate as a headline stat")
	}
	if !hasByResult {
		t.Error("reconcile liveness must include the independent second measure: a by-result rate timeseries")
	}
}

// TestFleetCapacityDashboard_PhaseFlowUsesTransitionLabels guards that the
// pod-phase panels actually cut on the transition labels the controller emits
// (phase_to for the flow rate, phase_from×phase_to for the matrix). Aggregating
// these away would collapse the board's whole reason to exist — seeing where in
// the Pending→Running→Succeeded flow pods pile up.
func TestFleetCapacityDashboard_PhaseFlowUsesTransitionLabels(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, fleetCapacityKey))

	if !strings.Contains(exprs, "by (phase_to)") {
		t.Error("phase-flow panel must group by phase_to so entering-phase rates are legible")
	}
	if !strings.Contains(exprs, "phase_from") || !strings.Contains(exprs, "phase_to") {
		t.Error("phase transition matrix must retain phase_from and phase_to labels")
	}
}
