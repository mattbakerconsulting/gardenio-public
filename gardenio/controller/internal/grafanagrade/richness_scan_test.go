package grafanagrade

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
)

// The fleet richness ratchet.
//
// The 2026-09-02 GRADE iteration measured Richness 6.69 across the fleet with
// 24 of 26 boards below the 8.0 bar (ka-0000008177): long single-type scrolls,
// zero distribution panels on 12 boards, thresholds on well under half the
// panels. The uplift that closed it is only worth shipping if it stays shipped,
// so this file gates two things:
//
//   - every board at or above the 8.0 bar (the audit's actual acceptance
//     criterion — TestRichnessScan_EveryBoardAtBar);
//   - no board drifting far below its recorded score (the snapshot in
//     testdata/richness_floor.json — TestRichnessScan_NoRegression).
//
// Why the drift gate has slack rather than pinning each score exactly: two of
// the six components (legend, threshold) are RATIOS over viz-panel count, so
// adding one honest panel can shave ~0.1 off a board that is already at the
// volume cap. An exact floor would red CI on a genuine improvement and teach
// the next agent to fight the rubric. driftTolerance absorbs that; gutting a
// board (dropping its quantile targets costs 1.5 outright) still fails loudly,
// and the bar gate is exact.
//
// Regenerate the snapshot after a deliberate, reviewed change:
//
//	GRAFANAGRADE_UPDATE_RICHNESS_FLOOR=1 go test ./internal/grafanagrade/ -run Richness
//
// The fleet table is reporting, not a gate:
//
//	go test -v ./internal/grafanagrade/ -run RichnessScan_FleetTable

const richnessFloorPath = "testdata/richness_floor.json"

// driftTolerance is how far below its recorded score a board may sit before
// the drift gate fails: enough to absorb ratio dilution from a few added
// panels (~0.1 each), far less than dropping a real feature (a board's whole
// quantile component is 1.5).
const driftTolerance = 0.5

func scoreFleet(t *testing.T) map[string]RichnessComponents {
	t.Helper()
	boards := loadDashboards(t)
	out := make(map[string]RichnessComponents, len(boards))
	for name, d := range boards {
		out[name] = ScoreRichness(name, d)
	}
	return out
}

func sortedNames(scores map[string]RichnessComponents) []string {
	names := make([]string, 0, len(scores))
	for n := range scores {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// TestRichnessScan_NoRegression is the ratchet.
func TestRichnessScan_NoRegression(t *testing.T) {
	scores := scoreFleet(t)

	if os.Getenv("GRAFANAGRADE_UPDATE_RICHNESS_FLOOR") == "1" {
		floors := map[string]float64{}
		for n, c := range scores {
			floors[n] = round2(c.Total)
		}
		raw, err := json.MarshalIndent(floors, "", "  ")
		if err != nil {
			t.Fatalf("marshal floors: %v", err)
		}
		if err := os.WriteFile(richnessFloorPath, append(raw, '\n'), 0o644); err != nil {
			t.Fatalf("write %s: %v", richnessFloorPath, err)
		}
		t.Logf("rewrote %s with %d boards", richnessFloorPath, len(floors))
		return
	}

	raw, err := os.ReadFile(richnessFloorPath)
	if err != nil {
		t.Fatalf("read %s: %v", richnessFloorPath, err)
	}
	var floors map[string]float64
	if err := json.Unmarshal(raw, &floors); err != nil {
		t.Fatalf("parse %s: %v", richnessFloorPath, err)
	}

	for _, n := range sortedNames(scores) {
		c := scores[n]
		floor, ok := floors[n]
		if !ok {
			t.Errorf("%s: no richness score recorded (scores %.2f) — regenerate the snapshot", n, c.Total)
			continue
		}
		switch {
		case c.Total < floor-driftTolerance:
			t.Errorf("%s: richness regressed %.2f -> %.2f, more than the %.2f drift tolerance (weakest: %s)\n  %s",
				n, floor, c.Total, driftTolerance, c.Weakest(), describe(c))
		case c.Total < floor-0.005:
			t.Logf("%s: richness drifted %.2f -> %.2f, within tolerance (weakest: %s)", n, floor, c.Total, c.Weakest())
		}
	}
	for n := range floors {
		if _, ok := scores[n]; !ok {
			t.Logf("stale snapshot entry for removed board %s — prune it", n)
		}
	}
}

// TestRichnessScan_EveryBoardAtBar is the exact gate: the uplift put all 26
// boards at or above the bar, and a new board joins the fleet at the same
// standard rather than dragging the mean back down.
func TestRichnessScan_EveryBoardAtBar(t *testing.T) {
	scores := scoreFleet(t)
	for _, n := range sortedNames(scores) {
		c := scores[n]
		if c.Total < RichnessBar-0.005 {
			t.Errorf("%s: richness %.2f is below the %.1f bar (weakest: %s)\n  %s",
				n, c.Total, RichnessBar, c.Weakest(), describe(c))
		}
	}
}

// TestRichnessScan_FleetTable is reporting, not a gate: `go test -v -run
// RichnessScan_FleetTable ./internal/grafanagrade/` prints the same table the
// GRADE iteration publishes, so a change can be measured before it is shipped.
func TestRichnessScan_FleetTable(t *testing.T) {
	scores := scoreFleet(t)
	var sum float64
	var thin []string
	for _, n := range sortedNames(scores) {
		c := scores[n]
		sum += c.Total
		mark := " "
		if c.Thin() {
			mark = "*"
			thin = append(thin, n)
		}
		t.Logf("%s %-38s %5.2f  %s", mark, n, c.Total, describe(c))
	}
	t.Logf("fleet mean richness %.2f over %d boards; %d below the %.1f bar",
		sum/float64(len(scores)), len(scores), len(thin), RichnessBar)
}

// TestRichnessScan_ScorerMatchesAudit pins the saturation curve to the numbers
// the 2026-09-02 iteration published, using that iteration's own measurements
// as the fixture. If the curve is ever retuned, this is the test that says so
// out loud rather than letting the floors drift under a changed rubric.
func TestRichnessScan_ScorerMatchesAudit(t *testing.T) {
	// cost-overview as measured 2026-09-02: 23 viz panels, 6 types, 0 quantile
	// targets, 2 legend-calc panels, 7 thresholded, 0 text panels, no board
	// description -> 5.13 (verdicts.json).
	c := RichnessComponents{VizPanels: 23, PanelTypes: []string{"a", "b", "c", "d", "e", "f"},
		Quantile: 0, LegendCalcs: 2, Thresholded: 7, TextPanels: 0, BoardDesc: false}
	got := recompute(c)
	if round2(got.Total) != 5.13 {
		t.Errorf("audit fixture cost-overview: got %.2f, iteration published 5.13\n  %s", got.Total, describe(got))
	}

	// context-observability, the fleet's best board: 22 viz, 4 types, 11
	// quantile targets, 13 legend calcs, 9 thresholded, 0 text, board
	// description present -> 8.95.
	c = RichnessComponents{VizPanels: 22, PanelTypes: []string{"a", "b", "c", "d"},
		Quantile: 11, LegendCalcs: 13, Thresholded: 9, TextPanels: 0, BoardDesc: true}
	got = recompute(c)
	if round2(got.Total) != 8.95 {
		t.Errorf("audit fixture context-observability: got %.2f, iteration published 8.95\n  %s", got.Total, describe(got))
	}
}

// recompute runs raw counts back through the saturation curve, so the fixture
// test exercises the same arithmetic ScoreRichness does without needing a
// dashboard file.
func recompute(c RichnessComponents) RichnessComponents {
	d := &Dashboard{Description: map[bool]string{true: "x", false: ""}[c.BoardDesc]}
	for i := 0; i < c.VizPanels; i++ {
		p := Panel{Type: c.PanelTypes[i%len(c.PanelTypes)]}
		if i < c.LegendCalcs {
			p.Options.Legend.Calcs = []string{"lastNotNull"}
		}
		if i < c.Thresholded {
			v := 1.0
			p.FieldConfig.Defaults.Thresholds.Steps = append(p.FieldConfig.Defaults.Thresholds.Steps,
				struct {
					Value *float64 `json:"value"`
				}{Value: &v})
		}
		if i < c.Quantile {
			p.Targets = []Target{{Expr: "histogram_quantile(0.95, x)"}}
		}
		d.Panels = append(d.Panels, p)
	}
	for i := 0; i < c.TextPanels; i++ {
		d.Panels = append(d.Panels, Panel{Type: "text"})
	}
	return ScoreRichness("fixture", d)
}

func describe(c RichnessComponents) string {
	return fmt.Sprintf("%d viz / %d types / %d pctl / %d legend-calc / %d thresholded / %d text / desc=%v",
		c.VizPanels, len(c.PanelTypes), c.Quantile, c.LegendCalcs, c.Thresholded, c.TextPanels, c.BoardDesc)
}

func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}
