package bridge

import "testing"

// TestScoreGardenerD2_Bands pins the rubric §D2 burn-rate formula at the
// boundaries that matter: full marks at the 15% floor, graceful degradation
// below it, and the "closed nothing while work remains" genuine 0.
func TestScoreGardenerD2_Bands(t *testing.T) {
	cases := []struct {
		name    string
		closed  int
		open    int
		wantRaw int
	}{
		// burn 15% (3 of 20) → min(1, 1.0) → round(5) = 5.
		{"floor_15pct_full_marks", 3, 17, 5},
		// burn > 15% is capped at full marks, never over 5.
		{"above_floor_capped", 10, 5, 5},
		{"everything_closed", 8, 0, 5},
		// burn 7.5% (norm 0.5) → round(2.5) = 3 (half-up).
		{"half_floor", 3, 37, 3},
		// burn 3% (1 of 33, norm 0.2) → round(1.0) = 1.
		{"low_burn", 1, 32, 1},
		// burn ~1.6% (1 of 61, norm ~0.109) → round(0.546) = 1.
		{"barely_moving_rounds_to_1", 1, 60, 1},
		// closed nothing while 10 remain open → genuine throughput 0.
		{"closed_nothing_work_remains", 0, 10, 0},
	}
	for _, c := range cases {
		v := ScoreGardenerD2(c.closed, c.open)
		if v.Raw != c.wantRaw {
			t.Errorf("%s: ScoreGardenerD2(%d,%d).Raw = %d (burn %v), want %d",
				c.name, c.closed, c.open, v.Raw, v.BurnRate, c.wantRaw)
		}
		if v.EpicDrained {
			t.Errorf("%s: EpicDrained = true, want false (has a frontier)", c.name)
		}
	}
}

// TestScoreGardenerD2_EpicDrained pins the rubric's "no open children, closed
// none this window → NOT penalized, stop candidate" case: Raw 0 but
// EpicDrained=true so the guard and the readout treat it as no-signal, never a
// real throughput 0.
func TestScoreGardenerD2_EpicDrained(t *testing.T) {
	v := ScoreGardenerD2(0, 0)
	if !v.EpicDrained {
		t.Fatalf("EpicDrained = false, want true")
	}
	if v.Raw != 0 || v.Weighted != 0 {
		t.Errorf("got raw=%d weighted=%v, want 0/0", v.Raw, v.Weighted)
	}
}

// TestScoreGardenerD2_NegativeInputsFailSoft pins that garbage negative counts
// floor to 0 rather than producing a nonsense burn rate or a panic.
func TestScoreGardenerD2_NegativeInputsFailSoft(t *testing.T) {
	if v := ScoreGardenerD2(-5, -3); !v.EpicDrained || v.Raw != 0 {
		t.Errorf("ScoreGardenerD2(-5,-3) = %+v, want drained/0", v)
	}
	if v := ScoreGardenerD2(-1, 10); v.ClosedInWindow != 0 || v.Raw != 0 {
		t.Errorf("ScoreGardenerD2(-1,10) closed=%d raw=%d, want 0/0", v.ClosedInWindow, v.Raw)
	}
}

// TestScoreGardenerD2_Weighted pins the weighted contribution: Raw/5 × 20.
func TestScoreGardenerD2_Weighted(t *testing.T) {
	if v := ScoreGardenerD2(8, 0); v.Weighted != 20.0 {
		t.Errorf("full-marks Weighted = %v, want 20.0", v.Weighted)
	}
	if v := ScoreGardenerD2(3, 37); v.Weighted != 12.0 { // raw 3 → 3/5×20 = 12
		t.Errorf("raw-3 Weighted = %v, want 12.0", v.Weighted)
	}
}

// TestD2Constants_Doc pins the rubric §D2 / §2.1 constants so the code, the
// rubric doc, and the meta-scoring prompt can never silently drift on the
// weight, the full-mark burn floor, or the guard thresholds (mirrors
// TestD6CheckNames_Doc / TestD1RawFromYield_Bands).
func TestD2Constants_Doc(t *testing.T) {
	if D2MaxRaw != 5 {
		t.Errorf("D2MaxRaw = %d, want 5 (rubric §D2 grade is 0–5)", D2MaxRaw)
	}
	if D2Weight != 20 {
		t.Errorf("D2Weight = %d, want 20 (rubric §1 weight)", D2Weight)
	}
	if d2FullMarkBurnRate != 0.15 {
		t.Errorf("d2FullMarkBurnRate = %v, want 0.15 (rubric §D2: 15%%/window = full marks)", d2FullMarkBurnRate)
	}
	if d1d2GuardTripD2 != 4 || d1d2GuardMinD1 != 3 {
		t.Errorf("guard thresholds = trip@%d floor@%d, want trip@4 floor@3 (rubric §2.1: D2>=4 => D1>=3)",
			d1d2GuardTripD2, d1d2GuardMinD1)
	}
}
