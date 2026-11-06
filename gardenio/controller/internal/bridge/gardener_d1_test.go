package bridge

import (
	"testing"
)

// A gardener that self-reported forward progress on every one of its recorded
// runs — the exact shape the 2026-08-26 scorecard mis-graded as D1=0 because it
// read run_log (score-less) or state.Events (empty for this wrapup_field-only
// spec) instead of progress_state.runs. It must now score a clean 5/5.
const d1HealthyBlob = `{
  "progress": {"events":[{"kind":"wrapup_field","field":"progress_score","op":">=","value":"1"}]},
  "progress_state": {"runs":[
    {"agent_id":"a1","progress_score":1,"agent_state":"done"},
    {"agent_id":"a2","progress_score":1,"agent_state":"done"},
    {"agent_id":"a3","progress_score":2,"agent_state":"done"},
    {"agent_id":"a4","progress_score":1,"agent_state":"done"}
  ]}
}`

// TestScoreGardenerD1_HealthySelfReported pins that a gardener whose runs all
// self-report progress_score>=1 scores D1=5 — never the false 0 the hand-scorer
// produced (task ka-0000004982).
func TestScoreGardenerD1_HealthySelfReported(t *testing.T) {
	v := ScoreGardenerD1Blob(d1HealthyBlob, 0)
	if !v.HasRuns {
		t.Fatalf("HasRuns = false, want true")
	}
	if v.Raw != D1MaxRaw {
		t.Fatalf("Raw = %d, want %d (yield %v, %d/%d runs)", v.Raw, D1MaxRaw, v.Yield, v.ProgressRuns, v.ScoredRuns)
	}
	if v.Yield != 1.0 {
		t.Errorf("Yield = %v, want 1.0", v.Yield)
	}
	if v.ProgressRuns != 4 || v.ScoredRuns != 4 {
		t.Errorf("progress/scored = %d/%d, want 4/4", v.ProgressRuns, v.ScoredRuns)
	}
	if v.SelfReported != 4 || v.EventCredited != 0 {
		t.Errorf("selfReported/eventCredited = %d/%d, want 4/0", v.SelfReported, v.EventCredited)
	}
	if v.Weighted != 30.0 {
		t.Errorf("Weighted = %v, want 30.0", v.Weighted)
	}
}

// TestScoreGardenerD1_EventCreditedRescue exercises the "OR credited" union: a
// score-0 run whose attribution interval contains a child_closed event must
// count as a progress run even though its self-reported score is 0 (the async
// delegating-gardener case). Two runs, one score-0 rescued by an event, one
// score-0 with no event → yield 1/2.
func TestScoreGardenerD1_EventCreditedRescue(t *testing.T) {
	// run r1 spans [10:00, next start 10:30); an mr_merged at 10:15 credits it.
	// run r2 starts 10:30, open-ended, no event → stays uncredited.
	blob := `{
  "progress_state": {
    "runs":[
      {"agent_id":"r1","progress_score":0,"started_at":"2026-08-26T10:00:00Z","closed_at":"2026-08-26T10:10:00Z","agent_state":"done"},
      {"agent_id":"r2","progress_score":0,"started_at":"2026-08-26T10:30:00Z","closed_at":"2026-08-26T10:40:00Z","agent_state":"done"}
    ],
    "events":[
      {"kind":"mr_merged","target_id":"gd-MR1","occurred_at":"2026-08-26T10:15:00Z"}
    ]
  }
}`
	v := ScoreGardenerD1Blob(blob, 0)
	if !v.HasRuns {
		t.Fatalf("HasRuns = false, want true")
	}
	if v.ProgressRuns != 1 || v.ScoredRuns != 2 {
		t.Fatalf("progress/scored = %d/%d, want 1/2", v.ProgressRuns, v.ScoredRuns)
	}
	if v.SelfReported != 0 || v.EventCredited != 1 {
		t.Errorf("selfReported/eventCredited = %d/%d, want 0/1", v.SelfReported, v.EventCredited)
	}
	if v.Yield != 0.5 {
		t.Errorf("Yield = %v, want 0.5", v.Yield)
	}
	// yield 0.5 falls in the 0.40–0.59 band → D1_raw 3.
	if v.Raw != 3 {
		t.Errorf("Raw = %d, want 3", v.Raw)
	}
}

// TestScoreGardenerD1_NoRunsUnmeasurable pins that a gardener with a spec but no
// recorded runs is UNMEASURABLE (HasRuns=false, Raw=0) — NOT a real "pure spin"
// D1=0 grade. A downstream scorer must treat this as "no signal", per the
// D1/D2 consistency guard.
func TestScoreGardenerD1_NoRunsUnmeasurable(t *testing.T) {
	blob := `{"progress":{"events":[{"kind":"wrapup_field","field":"progress_score"}]}}`
	v := ScoreGardenerD1Blob(blob, 0)
	if v.HasRuns {
		t.Fatalf("HasRuns = true, want false (no runs recorded)")
	}
	if v.Raw != 0 || v.ProgressRuns != 0 || v.ScoredRuns != 0 {
		t.Errorf("got raw=%d progress=%d scored=%d, want all 0", v.Raw, v.ProgressRuns, v.ScoredRuns)
	}
	if v.Window != d1DefaultWindow {
		t.Errorf("Window = %d, want default %d", v.Window, d1DefaultWindow)
	}
}

// TestScoreGardenerD1_EmptyAndNil pins the fail-soft shape for an empty/garbage
// blob and a nil spec — UNMEASURABLE, never a crash.
func TestScoreGardenerD1_EmptyAndNil(t *testing.T) {
	for _, blob := range []string{"", "   ", "not json", "{}", "null"} {
		v := ScoreGardenerD1Blob(blob, 0)
		if v.HasRuns || v.Raw != 0 {
			t.Errorf("blob %q: got HasRuns=%v Raw=%d, want false/0", blob, v.HasRuns, v.Raw)
		}
	}
	if v := ScoreGardenerD1(nil, 0); v.HasRuns || v.Raw != 0 {
		t.Errorf("nil spec: got HasRuns=%v Raw=%d, want false/0", v.HasRuns, v.Raw)
	}
}

// TestScoreGardenerD1_Window pins that only the last N runs are scored. Twelve
// runs, the oldest two score-0, the rest score 1: over the default window of 10
// all counted runs progressed → yield 1.0, and the two dropped score-0 runs do
// not drag it down.
func TestScoreGardenerD1_Window(t *testing.T) {
	runs := `{"agent_id":"z0","progress_score":0,"agent_state":"done"},{"agent_id":"z1","progress_score":0,"agent_state":"done"}`
	for i := 0; i < 10; i++ {
		runs += `,{"agent_id":"p","progress_score":1,"agent_state":"done"}`
	}
	blob := `{"progress_state":{"runs":[` + runs + `]}}`
	v := ScoreGardenerD1Blob(blob, 0)
	if v.ScoredRuns != 10 {
		t.Fatalf("ScoredRuns = %d, want 10 (windowed)", v.ScoredRuns)
	}
	if v.ProgressRuns != 10 || v.Yield != 1.0 || v.Raw != 5 {
		t.Errorf("got progress=%d yield=%v raw=%d, want 10/1.0/5", v.ProgressRuns, v.Yield, v.Raw)
	}
	// An explicit larger window pulls in the two dropped score-0 runs → 10/12.
	v = ScoreGardenerD1Blob(blob, 12)
	if v.ScoredRuns != 12 || v.ProgressRuns != 10 {
		t.Errorf("window=12: got %d/%d, want 10/12", v.ProgressRuns, v.ScoredRuns)
	}
}

// TestApplyD1D2Guard_DelegatorFalseZero reproduces the exact bug the whole
// epic exists to kill (ka-0000003502): a delegating gardener whose crew close
// the epic's children ASYNCHRONOUSLY, after each dispatch run's closed_at, so
// its runs all self-report progress_score 0 and no event lands in-window — a
// raw D1 of 0. But D2 sees the children closing (raw 5), so the mandatory
// D1/D2 consistency guard (rubric §2.1) must clamp D1 up to 3 and flag it,
// never emit the false "pure spin" 0.
func TestApplyD1D2Guard_DelegatorFalseZero(t *testing.T) {
	// Two dispatch runs, both honest score-0, no credited events in-window.
	blob := `{"progress_state":{"runs":[
		{"agent_id":"d1","progress_score":0,"agent_state":"done"},
		{"agent_id":"d2","progress_score":0,"agent_state":"done"}
	]}}`
	d1 := ScoreGardenerD1Blob(blob, 0)
	if d1.Raw != 0 || !d1.HasRuns {
		t.Fatalf("precondition: D1 raw=%d hasRuns=%v, want 0/true", d1.Raw, d1.HasRuns)
	}
	// D2: 6 children closed this window, 2 still open → burn 0.75 → raw 5.
	d2 := ScoreGardenerD2(6, 2)
	if d2.Raw < d1d2GuardTripD2 {
		t.Fatalf("precondition: D2 raw=%d, want >= %d", d2.Raw, d1d2GuardTripD2)
	}

	guarded := ApplyD1D2Guard(d1, d2)
	if !guarded.GuardApplied {
		t.Errorf("GuardApplied = false, want true")
	}
	if guarded.Raw != d1d2GuardMinD1 {
		t.Errorf("Raw = %d, want clamped to %d", guarded.Raw, d1d2GuardMinD1)
	}
	if guarded.Weighted != 18.0 { // 3/5 × 30
		t.Errorf("Weighted = %v, want 18.0 (3/5 × 30)", guarded.Weighted)
	}
	// The underlying yield/run counts are untouched — only the grade is floored.
	if guarded.ProgressRuns != 0 || guarded.ScoredRuns != 2 {
		t.Errorf("counts mutated by guard: progress/scored = %d/%d, want 0/2", guarded.ProgressRuns, guarded.ScoredRuns)
	}
}

// TestApplyD1D2Guard_NoTripCases pins that the guard is a NARROW backstop: it
// fires only when D2>=4 AND D1<3 AND D2 is not epic-drained. Every other case
// is a no-op, and the guard never LOWERS a healthy D1 (ka-0000003456).
func TestApplyD1D2Guard_NoTripCases(t *testing.T) {
	low := D1Verdict{Raw: 0, HasRuns: true, ScoredRuns: 3}
	healthy := D1Verdict{Raw: 5, Weighted: 30, Yield: 1, HasRuns: true, ProgressRuns: 3, ScoredRuns: 3}

	// D2 below the trip threshold → no clamp even though D1 is 0.
	if g := ApplyD1D2Guard(low, ScoreGardenerD2(1, 20)); g.GuardApplied || g.Raw != 0 {
		t.Errorf("D2 raw<4: got applied=%v raw=%d, want false/0", g.GuardApplied, g.Raw)
	}
	// D2 drained (no frontier) → no cross-check to assert, no clamp.
	if g := ApplyD1D2Guard(low, ScoreGardenerD2(0, 0)); g.GuardApplied || g.Raw != 0 {
		t.Errorf("D2 drained: got applied=%v raw=%d, want false/0", g.GuardApplied, g.Raw)
	}
	// D2>=4 but D1 already >=3 → no clamp, never lowers.
	if g := ApplyD1D2Guard(healthy, ScoreGardenerD2(8, 0)); g.GuardApplied || g.Raw != 5 {
		t.Errorf("healthy D1 with high D2: got applied=%v raw=%d, want false/5", g.GuardApplied, g.Raw)
	}
	// Boundary: D1 exactly at the floor (3) → not clamped, not flagged.
	atFloor := D1Verdict{Raw: 3, HasRuns: true}
	if g := ApplyD1D2Guard(atFloor, ScoreGardenerD2(8, 0)); g.GuardApplied || g.Raw != 3 {
		t.Errorf("D1 at floor: got applied=%v raw=%d, want false/3", g.GuardApplied, g.Raw)
	}
}

// TestApplyD1D2Guard_NoRunsButClosing pins the guard on the UNMEASURABLE-D1
// case: a gardener with D2>=4 but no recorded runs (has_runs=false) still gets
// clamped to 3 — children are closing, so it is emphatically not pure spin,
// even though its own run log is empty (a finalize/attribution gap)
// (ka-0000003456).
func TestApplyD1D2Guard_NoRunsButClosing(t *testing.T) {
	d1 := D1Verdict{HasRuns: false, Window: d1DefaultWindow}
	g := ApplyD1D2Guard(d1, ScoreGardenerD2(5, 1))
	if !g.GuardApplied || g.Raw != d1d2GuardMinD1 {
		t.Errorf("got applied=%v raw=%d, want true/%d", g.GuardApplied, g.Raw, d1d2GuardMinD1)
	}
}

// TestD1RawFromYield_Bands pins the rubric §D1 grade-band table exactly,
// including the boundary values where the round(5×yield) note disagrees.
func TestD1RawFromYield_Bands(t *testing.T) {
	cases := []struct {
		yield float64
		want  int
	}{
		{1.0, 5}, {0.80, 5}, {0.799, 4}, {0.60, 4}, {0.59, 3},
		{0.40, 3}, {0.39, 2}, {0.20, 2}, {0.19, 1}, {0.0001, 1}, {0, 0},
	}
	for _, c := range cases {
		if got := d1RawFromYield(c.yield); got != c.want {
			t.Errorf("d1RawFromYield(%v) = %d, want %d", c.yield, got, c.want)
		}
	}
}
