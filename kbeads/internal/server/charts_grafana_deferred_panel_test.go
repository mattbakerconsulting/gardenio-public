package server

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// This is the durable guard for the cost-overview "Deferred Agents" panel,
// which has now been "fixed" three times in three days, each pass swapping one
// false reading for another:
//
//   - ka-0000007164: `... or vector(0)` painted a confident green 0 over a
//     gauge that was not emitting at all — an UNGATED zero.
//   - ka-0000002930 (pass 1): removing the `or vector(0)` left a bare instant
//     read, which renders noValue ("counter not emitting") whenever the last
//     reconcile pass deferred nothing — i.e. in the healthy case.
//   - ka-0000002930 (pass 2): `last_over_time(...[$__range])` reports the last
//     deferral ANYWHERE in the window as the current count. Measured live
//     2026-09-02: rendered an orange "1" while the last reconcile pass deferred
//     0 and the newest sample was 57m old.
//
// The invariant that survives all three: gardenio_agents_deferred_total is a
// GaugeVec the reconciler Reset()s at the start of every pass and Inc()s once
// per deferred agent (gardenio/controller/internal/reconciler/metrics.go +
// reconciler.go). So an ABSENT series means "the last pass deferred nothing",
// not "the emitter is blind" — and the two are only distinguishable by a
// separate liveness signal. The panel must therefore read the gauge at the
// instant AND gate its zero on gardenio_reconcile_total actually advancing.
//
// Where it runs: same contract as charts_grafana_dashboard_uid_disjoint_test.go
// — the chart JSON lives in a sibling module reached by relative path, so the
// test SKIPS cleanly when the tree is absent (a partial checkout). CI checks out
// the whole monorepo, so the guard always runs there.
// TestDeferredAgentsPanel_DetectsRegressions is the anti-vacuity companion that
// drives the same checker with each historical bad shape, so this guard can
// never be silently green because the checker broke.

const costOverviewDashboardPath = "../../../gardenio/helm/gardenio/dashboards/cost-overview.json"

// deferredPanel is the slice of a Grafana stat panel this guard inspects.
type deferredTarget struct {
	Expr string `json:"expr"`
}

type deferredPanel struct {
	Title       string           `json:"title"`
	Targets     []deferredTarget `json:"targets"`
	FieldConfig struct {
		Defaults struct {
			NoValue string `json:"noValue"`
		} `json:"defaults"`
	} `json:"fieldConfig"`
	Panels []deferredPanel `json:"panels"`
}

// findPanel walks the (row-nested) panel tree for a panel by title.
func findPanel(panels []deferredPanel, title string) *deferredPanel {
	for i := range panels {
		if panels[i].Title == title {
			return &panels[i]
		}
		if p := findPanel(panels[i].Panels, title); p != nil {
			return p
		}
	}
	return nil
}

// checkDeferredPanel returns the reasons the panel violates the invariant, so
// the anti-vacuity companion can drive the same logic with synthetic input.
func checkDeferredPanel(p *deferredPanel) []string {
	var bad []string
	if len(p.Targets) != 1 {
		bad = append(bad, "expected exactly 1 target")
		return bad
	}
	expr := p.Targets[0].Expr

	if strings.Contains(expr, "vector(0)") {
		bad = append(bad, "expr uses `vector(0)`: an UNGATED zero paints green 0 over a blind gauge (ka-0000007164)")
	}
	if strings.Contains(expr, "last_over_time") {
		bad = append(bad, "expr uses `last_over_time`: resurrects the last deferral in the window as the current count (ka-0000002930)")
	}
	if !strings.Contains(expr, "gardenio_agents_deferred_total") {
		bad = append(bad, "expr no longer reads gardenio_agents_deferred_total")
	}
	if !strings.Contains(expr, "gardenio_reconcile_total") {
		bad = append(bad, "expr has no liveness gate: an empty gauge is indistinguishable from a dead emitter without gardenio_reconcile_total")
	}

	// noValue fires when the gate itself is missing, so it must not assert
	// anything about the deferred gauge's own health — that is the specific
	// claim ka-0000002930 found to be false.
	noValue := strings.ToLower(p.FieldConfig.Defaults.NoValue)
	for _, claim := range []string{"not emitting", "no samples", "no defer events"} {
		if strings.Contains(noValue, claim) {
			bad = append(bad, fmt.Sprintf(
				"noValue claims %q about the gauge, which it cannot know — noValue fires when the "+
					"liveness gate is missing, not when the gauge is idle (ka-0000002930)", claim))
		}
	}
	return bad
}

func TestDeferredAgentsPanel_ReadsInstantWithLivenessGate(t *testing.T) {
	raw, err := os.ReadFile(costOverviewDashboardPath)
	if os.IsNotExist(err) {
		t.Skipf("chart dashboard tree absent (%s) — sibling module not staged", costOverviewDashboardPath)
	}
	if err != nil {
		t.Fatalf("read %s: %v", costOverviewDashboardPath, err)
	}

	var board struct {
		Panels []deferredPanel `json:"panels"`
	}
	if err := json.Unmarshal(raw, &board); err != nil {
		t.Fatalf("unmarshal %s: %v", costOverviewDashboardPath, err)
	}

	p := findPanel(board.Panels, "Deferred Agents")
	if p == nil {
		t.Fatalf("no panel titled %q in %s", "Deferred Agents", costOverviewDashboardPath)
	}

	for _, reason := range checkDeferredPanel(p) {
		t.Errorf("Deferred Agents panel: %s\n  expr:    %s\n  noValue: %s",
			reason, p.Targets[0].Expr, p.FieldConfig.Defaults.NoValue)
	}
}

// TestDeferredAgentsPanel_DetectsRegressions is the anti-vacuity companion: it
// replays each historical bad shape through checkDeferredPanel and asserts the
// checker actually catches it. Without this, a broken checker would leave the
// guard above permanently, silently green.
func TestDeferredAgentsPanel_DetectsRegressions(t *testing.T) {
	panelWith := func(expr, noValue string) *deferredPanel {
		p := &deferredPanel{
			Title:   "Deferred Agents",
			Targets: []deferredTarget{{Expr: expr}},
		}
		p.FieldConfig.Defaults.NoValue = noValue
		return p
	}

	cases := []struct {
		name    string
		panel   *deferredPanel
		wantBad bool
	}{
		{
			name:    "ka-0000007164 ungated vector(0)",
			panel:   panelWith("sum(gardenio_agents_deferred_total) or vector(0)", "0"),
			wantBad: true,
		},
		{
			name:    "ka-0000002930 pass 1 bare instant read",
			panel:   panelWith("sum(gardenio_agents_deferred_total)", "stale — no samples in window (counter not emitting)"),
			wantBad: true,
		},
		{
			name:    "ka-0000002930 pass 2 windowed read",
			panel:   panelWith("sum(last_over_time(gardenio_agents_deferred_total[$__range]))", "no defer events in window"),
			wantBad: true,
		},
		{
			name:    "gate dropped",
			panel:   panelWith("sum(gardenio_agents_deferred_total)", "no reconcile signal (controller not reporting)"),
			wantBad: true,
		},
		{
			name: "current gated instant read",
			panel: panelWith(
				"sum(gardenio_agents_deferred_total) or (0 * (sum(increase(gardenio_reconcile_total[15m])) > 0))",
				"no reconcile signal (controller not reporting)"),
			wantBad: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bad := checkDeferredPanel(tc.panel)
			if got := len(bad) > 0; got != tc.wantBad {
				t.Fatalf("checkDeferredPanel wantBad=%v got=%v (reasons: %v)", tc.wantBad, got, bad)
			}
		})
	}
}
