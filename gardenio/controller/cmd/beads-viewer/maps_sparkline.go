package main

import (
	"fmt"
	"strings"
)

// maps_sparkline.go — the verdict-over-time trajectory sparkline for a /maps
// card (task ka-0000001693, parent epic ka-0000006790).
//
// maps2 v1 rendered each card's recent runs as a DISCRETE bar strip: one glyph
// per run, capped at the tail-6 (mapsRecentRunStrip), height by score, colour by
// outcome. That reads "how did the last handful of runs go" but throws away the
// rest of the gardener's bounded trajectory and shows no continuous trend.
//
// This file projects the gardener's FULL bounded trajectory (up to
// gardenerTrajectorySparkRuns = 24 runs, oldest→newest — the same slice
// gardener_timeline.go / buildGardenerTrajectory builds and gardenerRowView.Runs
// carries) into a compact inline SVG polyline: one continuous line tracing run
// score over time, each vertex dotted in that run's outcome colour, the whole
// line tinted by the card's health VerdictKind. It is the default on the board;
// the discrete strip is kept behind a client-only "pills" toggle and is also the
// automatic fallback when there is too little history to draw a line (see
// HasData).
//
// mapsSparkline is pure and deterministic (no http, no daemon, no clock, no
// floats in the output): coordinates are formatted to one decimal so golden
// output stays stable. The template owns the SVG markup; this owns the geometry.

// mapsSparkViewW / mapsSparkViewH are the fixed SVG viewBox dimensions. The
// template draws the SVG with preserveAspectRatio="none" so it stretches to the
// strip's CSS box; the absolute numbers only set the coordinate space the points
// below are computed in.
const (
	mapsSparkViewW = 100
	mapsSparkViewH = 28
	// mapsSparkPad insets the line from the top/bottom edges so a full-height
	// (score==max) or floored (score==0) vertex still draws inside the box
	// rather than clipping at the border.
	mapsSparkPad = 4
)

// mapsSparkMinRuns is the fewest recorded runs that make a trajectory. A single
// point is not a line, so a gardener with 0 or 1 run renders the discrete strip /
// counters instead (HasData=false).
const mapsSparkMinRuns = 2

// mapsSparkView is the compact verdict-over-time sparkline drawn on a /maps card.
// Empty (HasData=false) when the gardener has fewer than mapsSparkMinRuns recorded
// runs — the card then falls back to the discrete strip / counters.
type mapsSparkView struct {
	HasData bool

	// Points is the SVG polyline points string ("x,y x,y …") for the trajectory
	// line, oldest→newest left→right, in the mapsSparkViewW × mapsSparkViewH box.
	Points string
	// Area is the same line closed down to the baseline (a polygon points string)
	// for the soft fill under the line.
	Area string
	// Dots marks each run's vertex, coloured by that run's outcome.
	Dots []mapsSparkDot

	ViewW int
	ViewH int
}

// mapsSparkDot is one vertex on the sparkline: a run, positioned at its line
// coordinate and coloured by its outcome (reusing the .gardener-spark-* palette).
type mapsSparkDot struct {
	X, Y    string // coordinates, formatted to one decimal for stable output
	Outcome string // "ok" | "fail" | "flat" — dot colour (gardener-spark-*)
	Title   string // hover tooltip (the run glyph's title)
}

// mapsSparkline projects a gardener's full bounded run trajectory (oldest→newest,
// as carried on gardenerRowView.Runs) into the sparkline view-model. With fewer
// than mapsSparkMinRuns runs it returns HasData=false so the card falls back to
// the discrete strip.
//
// The y-axis reuses each glyph's Height (0..100, already scaled relative to the
// gardener's busiest run by runGlyphHeight, with failed/zero runs floored to a
// visible stub) so the line and the discrete bars encode the same signal and the
// two displays agree when toggled. The x-axis spreads the runs evenly across the
// full width.
func mapsSparkline(runs []gardenerRunGlyph) mapsSparkView {
	n := len(runs)
	if n < mapsSparkMinRuns {
		return mapsSparkView{}
	}

	v := mapsSparkView{
		HasData: true,
		ViewW:   mapsSparkViewW,
		ViewH:   mapsSparkViewH,
		Dots:    make([]mapsSparkDot, 0, n),
	}

	usableH := float64(mapsSparkViewH - 2*mapsSparkPad)
	span := float64(n - 1) // n >= 2, so never divides by zero

	var line strings.Builder
	for i, g := range runs {
		x := float64(i) / span * float64(mapsSparkViewW)
		// Height 100 → top (y == pad); Height 0 → bottom (y == pad+usableH).
		h := g.Height
		if h < 0 {
			h = 0
		} else if h > 100 {
			h = 100
		}
		y := float64(mapsSparkPad) + (1-float64(h)/100)*usableH

		xs, ys := fmtCoord(x), fmtCoord(y)
		if i > 0 {
			line.WriteByte(' ')
		}
		line.WriteString(xs)
		line.WriteByte(',')
		line.WriteString(ys)

		v.Dots = append(v.Dots, mapsSparkDot{
			X:       xs,
			Y:       ys,
			Outcome: g.Outcome,
			Title:   g.Title,
		})
	}
	v.Points = line.String()

	// Area: the line closed down to the baseline (bottom edge) and back, so the
	// template can soft-fill beneath it. Anchor at the first/last x on the floor.
	firstX := v.Dots[0].X
	lastX := v.Dots[n-1].X
	floor := fmtCoord(float64(mapsSparkViewH))
	v.Area = firstX + "," + floor + " " + v.Points + " " + lastX + "," + floor

	return v
}

// fmtCoord formats an SVG coordinate to one decimal place, trimming a trailing
// ".0" so whole numbers stay compact ("50" not "50.0"). Deterministic output for
// golden tests.
func fmtCoord(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	return strings.TrimSuffix(s, ".0")
}
