package advice

import (
	"testing"
)

// densityFinding is a compact constructor for a directive-density finding used
// across the baseline tests.
func densityFinding(id, field string, count int) OverConstraintFinding {
	return OverConstraintFinding{
		ID: id, Field: field, Check: OverConstraintDirectiveDensity, DirectiveCount: count,
	}
}

// TestDiffOverConstraintBaseline_Classifies covers the three delta classes plus
// the pass-through of non-density findings.
func TestDiffOverConstraintBaseline_Classifies(t *testing.T) {
	base := OverConstraintBaseline{Version: 1, Entries: []OverConstraintBaselineEntry{
		{ID: "gd-floor10", DirectiveCount: 10},
		{ID: "gd-steady", DirectiveCount: 9},
	}}

	findings := []OverConstraintFinding{
		densityFinding("gd-floor10", "", 17), // climbed above floor 10
		densityFinding("gd-steady", "", 9),   // at floor -> same
		densityFinding("gd-steady", "", 8),   // below floor -> same (still not a regression)
		densityFinding("gd-brandnew", "", 6), // no baseline entry -> new
		// A non-density finding must pass through unclassified.
		{ID: "gd-floor10", Check: OverConstraintRulesOverJudgment, Rule: "no-exceptions"},
	}

	got := DiffOverConstraintBaseline(findings, base)

	want := []struct {
		delta BaselineDelta
		floor int
	}{
		{BaselineClimbed, 10},
		{BaselineSame, 9},
		{BaselineSame, 9},
		{BaselineNew, 0},
		{"", 0}, // rules-over-judgment: untouched
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].BaselineDelta != want[i].delta {
			t.Errorf("finding %d (%s): delta=%q want %q", i, got[i].ID, got[i].BaselineDelta, want[i].delta)
		}
		if got[i].BaselineFloor != want[i].floor {
			t.Errorf("finding %d (%s): floor=%d want %d", i, got[i].ID, got[i].BaselineFloor, want[i].floor)
		}
	}
}

// TestDiffOverConstraintBaseline_PerFieldKeying verifies the (id, field) compound
// key: two surfaces of the same bead have independent floors.
func TestDiffOverConstraintBaseline_PerFieldKeying(t *testing.T) {
	base := OverConstraintBaseline{Version: 1, Entries: []OverConstraintBaselineEntry{
		{ID: "gd-1", Field: "prompt", DirectiveCount: 5},
		{ID: "gd-1", Field: "terminal_goal", DirectiveCount: 12},
	}}
	findings := []OverConstraintFinding{
		densityFinding("gd-1", "prompt", 8),         // climbed vs 5
		densityFinding("gd-1", "terminal_goal", 12), // same vs 12
	}
	got := DiffOverConstraintBaseline(findings, base)
	if got[0].BaselineDelta != BaselineClimbed || got[0].BaselineFloor != 5 {
		t.Errorf("prompt surface: delta=%q floor=%d, want climbed/5", got[0].BaselineDelta, got[0].BaselineFloor)
	}
	if got[1].BaselineDelta != BaselineSame || got[1].BaselineFloor != 12 {
		t.Errorf("terminal_goal surface: delta=%q floor=%d, want same/12", got[1].BaselineDelta, got[1].BaselineFloor)
	}
}

// TestRatchetOverConstraintBaseline_FloorOnlyMovesDown is the core plateau-guard
// property: re-observing a surface ABOVE its floor keeps the floor, so the climb
// stays flagged; observing it BELOW the floor lowers the floor.
func TestRatchetOverConstraintBaseline_FloorOnlyMovesDown(t *testing.T) {
	prior := OverConstraintBaseline{Version: 1, Entries: []OverConstraintBaselineEntry{
		{ID: "gd-plateau", DirectiveCount: 10},
		{ID: "gd-improved", DirectiveCount: 10},
	}}
	findings := []OverConstraintFinding{
		densityFinding("gd-plateau", "", 17), // above floor -> floor stays 10
		densityFinding("gd-improved", "", 6), // below floor -> floor drops to 6
		densityFinding("gd-fresh", "", 8),    // new surface -> floor 8
	}
	next := RatchetOverConstraintBaseline(findings, prior)

	byID := map[string]int{}
	for _, e := range next.Entries {
		byID[e.ID] = e.DirectiveCount
	}
	if byID["gd-plateau"] != 10 {
		t.Errorf("plateau floor = %d, want 10 (floor must not rise to 17)", byID["gd-plateau"])
	}
	if byID["gd-improved"] != 6 {
		t.Errorf("improved floor = %d, want 6 (floor should drop)", byID["gd-improved"])
	}
	if byID["gd-fresh"] != 8 {
		t.Errorf("fresh floor = %d, want 8", byID["gd-fresh"])
	}
	if next.Version != baselineFormatVersion {
		t.Errorf("next.Version = %d, want %d", next.Version, baselineFormatVersion)
	}
}

// TestRatchetPlateauStaysFlaggedAcrossRuns is the end-to-end property the whole
// change exists for: a surface that climbs above its floor and then plateaus must
// keep reading CLIMBED on every subsequent run, not silently become "same" — the
// exact failure mode ka-0000001510 hit (13->16->17 then flat, above its floor of
// 10, invisible to a night-over-night diff).
func TestRatchetPlateauStaysFlaggedAcrossRuns(t *testing.T) {
	// Start from the remediated floor of 10 (as D1 left ka-0000001510).
	base := OverConstraintBaseline{Version: 1, Entries: []OverConstraintBaselineEntry{
		{ID: "gd-HX", DirectiveCount: 10},
	}}

	// Night 1: climbed to 16.
	night1 := []OverConstraintFinding{densityFinding("gd-HX", "", 16)}
	diff1 := DiffOverConstraintBaseline(night1, base)
	if diff1[0].BaselineDelta != BaselineClimbed {
		t.Fatalf("night1 delta = %q, want climbed", diff1[0].BaselineDelta)
	}
	base = RatchetOverConstraintBaseline(diff1, base)

	// Night 2: plateaued at 17 (still above floor). A naive night-over-night diff
	// (16 vs 17) would call this a tiny move; the baseline floor (10) correctly
	// keeps it a regression.
	night2 := []OverConstraintFinding{densityFinding("gd-HX", "", 17)}
	diff2 := DiffOverConstraintBaseline(night2, base)
	if diff2[0].BaselineDelta != BaselineClimbed || diff2[0].BaselineFloor != 10 {
		t.Fatalf("night2 delta=%q floor=%d, want climbed/10 (plateau must stay flagged)", diff2[0].BaselineDelta, diff2[0].BaselineFloor)
	}
	base = RatchetOverConstraintBaseline(diff2, base)

	// Night 3: flat again at 17 — STILL a regression against the persisted floor.
	night3 := []OverConstraintFinding{densityFinding("gd-HX", "", 17)}
	diff3 := DiffOverConstraintBaseline(night3, base)
	if diff3[0].BaselineDelta != BaselineClimbed {
		t.Fatalf("night3 delta = %q, want climbed (flat-above-floor is still a regression)", diff3[0].BaselineDelta)
	}

	// Night 4: actually re-remediated below the floor -> now 'same', floor drops.
	night4 := []OverConstraintFinding{densityFinding("gd-HX", "", 9)}
	diff4 := DiffOverConstraintBaseline(night4, base)
	if diff4[0].BaselineDelta != BaselineSame {
		t.Fatalf("night4 delta = %q, want same (re-remediated below floor)", diff4[0].BaselineDelta)
	}
	base = RatchetOverConstraintBaseline(diff4, base)
	if got := baselineFloorFor(base, "gd-HX", ""); got != 9 {
		t.Fatalf("post-remediation floor = %d, want 9", got)
	}
}

// TestRatchetDropsSurfacesNoLongerFlagged verifies a surface that falls out of the
// WARN set entirely is removed from the next baseline (so a later re-entry reads
// NEW, not "same" against a stale low floor).
func TestRatchetDropsSurfacesNoLongerFlagged(t *testing.T) {
	prior := OverConstraintBaseline{Version: 1, Entries: []OverConstraintBaselineEntry{
		{ID: "gd-gone", DirectiveCount: 8},
		{ID: "gd-here", DirectiveCount: 9},
	}}
	// gd-gone is no longer among the findings (re-remediated below the density
	// threshold entirely); only gd-here remains.
	findings := []OverConstraintFinding{densityFinding("gd-here", "", 9)}
	next := RatchetOverConstraintBaseline(findings, prior)
	for _, e := range next.Entries {
		if e.ID == "gd-gone" {
			t.Errorf("gd-gone should have been dropped from the baseline (no longer a finding)")
		}
	}
	if len(next.Entries) != 1 || next.Entries[0].ID != "gd-here" {
		t.Errorf("next baseline = %+v, want only gd-here", next.Entries)
	}
}

// TestLoadOverConstraintBaseline_Roundtrip covers marshal -> load and the
// empty-input fail-open path.
func TestLoadOverConstraintBaseline_Roundtrip(t *testing.T) {
	orig := OverConstraintBaseline{Version: 1, Entries: []OverConstraintBaselineEntry{
		{ID: "gd-a", Field: "prompt", Title: "A", DirectiveCount: 7},
	}}
	blob, err := MarshalOverConstraintBaseline(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got, err := LoadOverConstraintBaseline(blob)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got.Entries) != 1 || got.Entries[0].ID != "gd-a" || got.Entries[0].DirectiveCount != 7 {
		t.Errorf("roundtrip lost data: %+v", got)
	}

	// Empty input is a zero baseline, not an error.
	empty, err := LoadOverConstraintBaseline(nil)
	if err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if len(empty.Entries) != 0 {
		t.Errorf("empty baseline has %d entries, want 0", len(empty.Entries))
	}

	// Garbage input is an error (caller surfaces it rather than silently
	// treating a corrupt file as an empty floor).
	if _, err := LoadOverConstraintBaseline([]byte("{not json")); err == nil {
		t.Error("expected error on malformed baseline JSON")
	}
}

// TestHasBaselineRegression covers the --strict --baseline gate condition.
func TestHasBaselineRegression(t *testing.T) {
	same := []OverConstraintFinding{{BaselineDelta: BaselineSame}}
	if HasBaselineRegression(same) {
		t.Error("all-same findings should not be a regression")
	}
	climbed := []OverConstraintFinding{{BaselineDelta: BaselineSame}, {BaselineDelta: BaselineClimbed}}
	if !HasBaselineRegression(climbed) {
		t.Error("a climbed finding is a regression")
	}
	fresh := []OverConstraintFinding{{BaselineDelta: BaselineNew}}
	if !HasBaselineRegression(fresh) {
		t.Error("a new finding is a regression")
	}
}

// baselineFloorFor is a test helper that reads a surface's recorded floor.
func baselineFloorFor(b OverConstraintBaseline, id, field string) int {
	for _, e := range b.Entries {
		if e.ID == id && e.Field == field {
			return e.DirectiveCount
		}
	}
	return -1
}
