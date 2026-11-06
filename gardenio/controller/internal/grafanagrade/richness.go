package grafanagrade

import (
	"sort"
	"strings"
)

// Richness is the second GRADE detector: a static, per-board decomposition of
// the audit's Richness dimension (bug ka-0000008177, epic ka-0000008066).
//
// Where the empty-branch detector asks "does this panel assert something it
// cannot know?", this one asks the cheaper structural question the 2026-09-02
// iteration measured 24 of 26 boards failing: "can an operator find the signal
// on this board at all?" It scores the same six components the iteration's
// scorer used (docs/grafana-grade/2026-09-02T07:23:00Z/build_verdicts.py), so
// a score here is directly comparable to the number in that REPORT.md.
//
// Every component SATURATES at what a genuinely good board of this kind does,
// not at a theoretical maximum — a rubric nothing can pass measures the rubric,
// not the fleet. The saturation points are the audit's, unchanged:
//
//	volume     12 viz panels
//	diversity  4 distinct panel types
//	analysis   3 quantile targets (distribution, not just means)
//	legend     legend calcs on 60% of viz panels
//	threshold  thresholds on 50% of viz panels
//	narrative  one text panel + a board description
//
// It is deliberately structural. It cannot see whether a panel is BEAUTIFUL or
// whether its query is TRUE — Aesthetics is scored off renders and Grounded off
// a VictoriaMetrics replay, neither of which belongs in a unit test. What it
// can do is stop the fleet silently sliding back into long single-type scrolls
// with no distribution and no thresholds, which is what the ratchet in
// richness_scan_test.go is for.

// RichnessComponents is the measured decomposition behind a board's score.
// Counts are raw observations; the six float fields are those counts run
// through the saturation curve, and Total is their sum, clamped to 10.
type RichnessComponents struct {
	Dashboard string `json:"dashboard"`

	// Raw counts.
	VizPanels   int      `json:"viz_panels"`
	PanelTypes  []string `json:"panel_types"`
	Quantile    int      `json:"quantile_targets"`
	LegendCalcs int      `json:"legend_calc_panels"`
	Thresholded int      `json:"thresholded_panels"`
	TextPanels  int      `json:"text_panels"`
	BoardDesc   bool     `json:"board_description"`

	// Scored components.
	Volume    float64 `json:"volume"`
	Diversity float64 `json:"diversity"`
	Analysis  float64 `json:"analysis"`
	Legend    float64 `json:"legend"`
	Threshold float64 `json:"threshold"`
	Narrative float64 `json:"narrative"`

	Total float64 `json:"total"`
}

// RichnessBar is the audit's "good" line. A board at or above it is not thin.
const RichnessBar = 8.0

// quantileFns are the PromQL forms that plot a distribution rather than a
// central tendency. `quantile_over_time` and `quantile(` cover the gauge and
// aggregation-operator spellings; `histogram_quantile` the classic bucket one.
var quantileFns = []string{"histogram_quantile(", "quantile_over_time(", "quantile("}

func hasQuantile(expr string) bool {
	e := strings.ToLower(expr)
	for _, fn := range quantileFns {
		if strings.Contains(e, fn) {
			return true
		}
	}
	return false
}

// isViz reports whether a panel counts toward the board's visible surface.
// Rows are chrome and text panels are narrative — both are scored separately.
func isViz(p *Panel) bool {
	switch strings.ToLower(p.Type) {
	case "row", "text", "":
		return false
	}
	return true
}

// thresholded reports whether the panel carries at least one threshold step
// with a value. Grafana always writes a `base` step with a null value, so the
// bare presence of a thresholds block proves nothing.
func thresholded(p *Panel) bool {
	for _, s := range p.FieldConfig.Defaults.Thresholds.Steps {
		if s.Value != nil {
			return true
		}
	}
	return false
}

// ScoreRichness decomposes one dashboard. name is the file name, used only for
// reporting.
func ScoreRichness(name string, d *Dashboard) RichnessComponents {
	c := RichnessComponents{Dashboard: name, BoardDesc: strings.TrimSpace(d.Description) != ""}
	types := map[string]bool{}

	var walk func([]Panel)
	walk = func(ps []Panel) {
		for i := range ps {
			p := &ps[i]
			switch {
			case strings.EqualFold(p.Type, "row"):
				walk(p.Panels)
				continue
			case strings.EqualFold(p.Type, "text"):
				c.TextPanels++
			case isViz(p):
				c.VizPanels++
				types[strings.ToLower(p.Type)] = true
				if len(p.Options.Legend.Calcs) > 0 {
					c.LegendCalcs++
				}
				if thresholded(p) {
					c.Thresholded++
				}
			}
			for _, t := range p.Targets {
				if !t.Hide && hasQuantile(t.Expr) {
					c.Quantile++
				}
			}
			walk(p.Panels)
		}
	}
	walk(d.Panels)

	for t := range types {
		c.PanelTypes = append(c.PanelTypes, t)
	}
	sort.Strings(c.PanelTypes)

	n := float64(c.VizPanels)
	if n < 1 {
		n = 1
	}
	c.Volume = min(2.0, float64(c.VizPanels)/12.0*2.0)
	c.Diversity = min(2.0, float64(len(c.PanelTypes))/4.0*2.0)
	c.Analysis = min(1.5, float64(c.Quantile)/3.0*1.5)
	c.Legend = min(1.5, float64(c.LegendCalcs)/(n*0.6)*1.5)
	c.Threshold = min(1.5, float64(c.Thresholded)/(n*0.5)*1.5)
	narrative := float64(c.TextPanels) * 0.75
	if c.BoardDesc {
		narrative += 0.75
	}
	c.Narrative = min(1.5, narrative)

	c.Total = c.Volume + c.Diversity + c.Analysis + c.Legend + c.Threshold + c.Narrative
	if c.Total > 10 {
		c.Total = 10
	}
	return c
}

// Thin reports whether the board is below the audit's bar.
func (c RichnessComponents) Thin() bool { return c.Total < RichnessBar }

// Weakest names the component furthest from its own saturation point — the
// cheapest next fix for this board.
func (c RichnessComponents) Weakest() string {
	gaps := []struct {
		name string
		gap  float64
	}{
		{"volume (more panels)", 2.0 - c.Volume},
		{"diversity (more panel types)", 2.0 - c.Diversity},
		{"analysis (p50/p95 distribution)", 1.5 - c.Analysis},
		{"legend (legend calcs)", 1.5 - c.Legend},
		{"threshold (is this bad?)", 1.5 - c.Threshold},
		{"narrative (text panel / board description)", 1.5 - c.Narrative},
	}
	sort.SliceStable(gaps, func(i, j int) bool { return gaps[i].gap > gaps[j].gap })
	return gaps[0].name
}
