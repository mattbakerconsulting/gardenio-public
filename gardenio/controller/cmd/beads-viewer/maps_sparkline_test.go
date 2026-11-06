package main

import (
	"strconv"
	"strings"
	"testing"
)

// mkGlyph builds a gardenerRunGlyph fixture for the sparkline tests.
func mkGlyph(run int, outcome string, height int) gardenerRunGlyph {
	return gardenerRunGlyph{
		RunNumber: run,
		Outcome:   outcome,
		Height:    height,
		Title:     "Run #" + strconv.Itoa(run),
	}
}

// TestMapsSparkline_TooFewRuns: 0 or 1 run is not a line — HasData false so the
// card falls back to the discrete strip.
func TestMapsSparkline_TooFewRuns(t *testing.T) {
	if got := mapsSparkline(nil); got.HasData {
		t.Errorf("nil runs: HasData = true, want false")
	}
	if got := mapsSparkline([]gardenerRunGlyph{mkGlyph(1, "ok", 50)}); got.HasData {
		t.Errorf("1 run: HasData = true, want false")
	}
}

// TestMapsSparkline_Geometry asserts the polyline spans the full width, the
// first/last x sit on the edges, height maps to y (100 → top, 0 → bottom), and a
// dot is emitted per run carrying its outcome + title.
func TestMapsSparkline_Geometry(t *testing.T) {
	runs := []gardenerRunGlyph{
		mkGlyph(1, "flat", 0),  // bottom
		mkGlyph(2, "ok", 100),  // top
		mkGlyph(3, "fail", 50), // middle-ish
	}
	v := mapsSparkline(runs)
	if !v.HasData {
		t.Fatalf("HasData = false, want true")
	}
	if v.ViewW != mapsSparkViewW || v.ViewH != mapsSparkViewH {
		t.Errorf("viewBox = %dx%d, want %dx%d", v.ViewW, v.ViewH, mapsSparkViewW, mapsSparkViewH)
	}
	if len(v.Dots) != 3 {
		t.Fatalf("dots = %d, want 3", len(v.Dots))
	}

	// First x on the left edge (0), last x on the right edge (full width).
	if v.Dots[0].X != "0" {
		t.Errorf("first x = %q, want 0", v.Dots[0].X)
	}
	if v.Dots[2].X != fmtCoord(mapsSparkViewW) {
		t.Errorf("last x = %q, want %s", v.Dots[2].X, fmtCoord(mapsSparkViewW))
	}

	// Height 0 → floor (pad+usableH), height 100 → ceil (pad).
	topY := fmtCoord(float64(mapsSparkPad))
	floorY := fmtCoord(float64(mapsSparkViewH - mapsSparkPad))
	if v.Dots[0].Y != floorY {
		t.Errorf("height-0 y = %q, want %s (floor)", v.Dots[0].Y, floorY)
	}
	if v.Dots[1].Y != topY {
		t.Errorf("height-100 y = %q, want %s (ceil)", v.Dots[1].Y, topY)
	}

	// Outcome + title carried through onto the dots.
	if v.Dots[1].Outcome != "ok" || v.Dots[2].Outcome != "fail" {
		t.Errorf("dot outcomes wrong: %+v", v.Dots)
	}
	if v.Dots[0].Title != "Run #1" {
		t.Errorf("dot title = %q, want Run #1", v.Dots[0].Title)
	}

	// Points string has one vertex per run.
	if n := len(strings.Fields(v.Points)); n != 3 {
		t.Errorf("Points vertices = %d, want 3 (%q)", n, v.Points)
	}
}

// TestMapsSparkline_AreaClosesToFloor: the area polygon starts and ends on the
// baseline (bottom edge) so the soft fill sits under the line.
func TestMapsSparkline_AreaClosesToFloor(t *testing.T) {
	v := mapsSparkline([]gardenerRunGlyph{mkGlyph(1, "ok", 30), mkGlyph(2, "ok", 70)})
	floor := fmtCoord(float64(mapsSparkViewH))
	if !strings.HasPrefix(v.Area, "0,"+floor+" ") {
		t.Errorf("Area does not start on the floor at x=0: %q", v.Area)
	}
	if !strings.HasSuffix(v.Area, " "+fmtCoord(mapsSparkViewW)+","+floor) {
		t.Errorf("Area does not end on the floor at x=max: %q", v.Area)
	}
}

// TestMapsSparkline_HeightClamp: out-of-range heights are clamped into the box
// rather than overflowing the viewBox.
func TestMapsSparkline_HeightClamp(t *testing.T) {
	v := mapsSparkline([]gardenerRunGlyph{mkGlyph(1, "ok", -20), mkGlyph(2, "ok", 250)})
	floorY := fmtCoord(float64(mapsSparkViewH - mapsSparkPad))
	topY := fmtCoord(float64(mapsSparkPad))
	if v.Dots[0].Y != floorY {
		t.Errorf("negative height y = %q, want floor %s", v.Dots[0].Y, floorY)
	}
	if v.Dots[1].Y != topY {
		t.Errorf(">100 height y = %q, want ceil %s", v.Dots[1].Y, topY)
	}
}

// TestBuildMapsBoard_SparklineWired: the board projection populates the card's
// Sparkline from the row's full Runs slice while the discrete strip keeps its
// tail-N (they share the same source so they agree when toggled).
func TestBuildMapsBoard_SparklineWired(t *testing.T) {
	row := mkMapsRow("gd-cap", "spark cap")
	row.VerdictKind = "progress"
	for i := 1; i <= 10; i++ {
		row.Runs = append(row.Runs, mkGlyph(i, "ok", 10*i))
	}
	board := buildMapsBoard([]gardenerRowView{row}, "/base")
	c := board.Cards[0]
	if !c.Sparkline.HasData {
		t.Fatalf("Sparkline.HasData = false, want true")
	}
	// Sparkline draws the FULL trajectory; the strip is capped at the tail.
	if len(c.Sparkline.Dots) != 10 {
		t.Errorf("sparkline dots = %d, want 10 (full trajectory)", len(c.Sparkline.Dots))
	}
	if len(c.Runs) != mapsRecentRunStrip {
		t.Errorf("strip runs = %d, want %d (tail-N)", len(c.Runs), mapsRecentRunStrip)
	}
}
