// Package bridge — gardener D1 PROGRESS-YIELD scorer (task ka-0000004982).
//
// The gardener effectiveness rubric scores every live gardener on six dimensions.
// D1 (progress yield, weight 30) is "the single most important signal": the
// fraction of a gardener's recent wake-ups that produced forward progress.
//
// D1 was documented prose the meta-gardener (a Haiku-pinned LLM) computed by
// hand each run — and it read a FALSE 0 for every scored gardener in the
// 2026-08-26 scorecard (ka-0000005712). Root cause (verified in ka-0000004982):
// the scorer looked for the per-run score in the WRONG place. The rubric's D1
// formula is
//
//	yield = progress_runs / scored_runs
//	  where a run is a progress_run when it self-reported progress_score >= 1
//	  OR the progress engine credited it (child_closed / mr_merged /
//	  decision_resolved).
//
// but the gardener's `run_log` entries carry NO progress_score (it is always
// null there); the real per-run score lives in
// `progress_state.runs[].progress_score`, and the credited-event fallback lives
// in `progress_state.events`. A scorer reading run_log — or reading only
// state.Events (empty for a wrapup_field-only spec) — sees 0 and mis-grades a
// healthy high-throughput gardener as "pure spin".
//
// This file makes D1 MACHINE-COMPUTED, deterministically, off the same
// progress_state blob the engine persists — exactly like gardener_d6.go did for
// D6. It reuses progress.State.ProgressYield, which implements the rubric's
// "OR credited" union (self-reported score OR a credited event in the run's
// attribution window), so `gb gardener status --json` and any surveyor read ONE
// definition of D1 that can never disagree with the run data.
//
// It is PURELY ADVISORY, like the D6 and self-block axes: nothing here changes
// the engine's fire-time behavior. It exists so the meta-gardener (and
// `gb gardener status`) computes the SAME D1 the rubric documents instead of
// re-deriving it by eye — and, critically, off the field that actually carries
// the score.
package bridge

import (
	"strings"

	"gardenio/controller/internal/scheduler/progress"
)

// D1MaxRaw is the maximum raw D1 score — the 0–5 scale in the rubric §D1 grade
// table. Score contribution = D1Raw/5 × 30 (D1's rubric weight is 30).
const D1MaxRaw = 5

// D1Weight is D1's weight in the 0–100 rubric total (§1: weights sum to 100).
const D1Weight = 30

// d1DefaultWindow is the rubric's default N — D1 scores the last N runs
// ("last N runs, default N=10"). ScoreGardenerD1 uses this when the caller
// passes window<=0.
const d1DefaultWindow = 10

// D1Verdict is the progress-yield roll-up for one gardener: the 0–5 raw score,
// the yield it derived from, the run/credit breakdown, and the weighted
// contribution to the rubric total.
type D1Verdict struct {
	// Raw is the 0–D1MaxRaw score from the rubric §D1 grade band table. This is
	// the "D1_raw" the rubric's Score = Σ(Di_raw/5 × weighti) formula consumes.
	Raw int `json:"raw"`
	// Weighted is Raw/5 × 30 — D1's contribution to the 0–100 rubric score,
	// rounded to two decimals.
	Weighted float64 `json:"weighted"`
	// Yield is progress_runs / scored_runs (0 when no runs), rounded to four
	// decimals. The value the grade band is read from.
	Yield float64 `json:"yield"`
	// ProgressRuns is the numerator: runs that made forward progress.
	ProgressRuns int `json:"progress_runs"`
	// ScoredRuns is the denominator: runs in the scoring window.
	ScoredRuns int `json:"scored_runs"`
	// SelfReported is the subset of ProgressRuns credited by progress_score>=1;
	// EventCredited is the subset credited ONLY by a window outcome event. They
	// sum to ProgressRuns and explain WHY D1 is non-zero (self-attested
	// throughput vs. async child closures) so the reading is legible.
	SelfReported  int `json:"self_reported"`
	EventCredited int `json:"event_credited"`
	// Window is the N applied (the count of runs considered, capped at what the
	// gardener actually recorded).
	Window int `json:"window"`
	// HasRuns is false when the gardener has recorded no runs at all — D1 is then
	// UNMEASURABLE (Raw 0, but not a "pure spin" verdict). A caller must treat
	// !HasRuns as "no signal yet", never as a real D1=0 grade.
	HasRuns bool `json:"has_runs"`
	// GuardApplied is true when the D1/D2 consistency guard (rubric §2.1) clamped
	// Raw up: the gardener's D2 showed children actively closing (D2_raw >= 4) but
	// the raw yield landed D1 below 3, which is definitionally a measurement bug
	// in the D1 count, so ApplyD1D2Guard floored Raw to 3. Set by ApplyD1D2Guard,
	// never by ScoreGardenerD1 itself (which has no D2 context). When true, Raw is
	// the guarded floor, not the raw-yield band — the readout names the guard so
	// the number is legible.
	GuardApplied bool `json:"guard_applied"`
}

// ScoreGardenerD1 computes the rubric's D1 progress-yield dimension for a decoded
// GardenerSpec over the last `window` runs (window<=0 => d1DefaultWindow). It is
// the single deterministic answer any surveyor (the meta-gardener, `gb gardener
// status`, a future evaluator) consults so no two runs disagree on D1.
//
// A nil spec, or a spec with no recorded runs, yields HasRuns=false and Raw=0 —
// UNMEASURABLE, not "pure spin". The rubric's D1/D2 consistency guard still
// applies downstream: a gardener whose D2 shows children closing but whose D1 is
// unmeasurable here is graded on D2/D3/D4, never handed a D1-driven C/D/F.
func ScoreGardenerD1(spec *GardenerSpec, window int) D1Verdict {
	if window <= 0 {
		window = d1DefaultWindow
	}
	if spec == nil || len(spec.ProgressState) == 0 {
		return D1Verdict{Window: window}
	}
	state, err := progress.ParseState(string(spec.ProgressState))
	if err != nil || len(state.Runs) == 0 {
		return D1Verdict{Window: window}
	}

	progressRuns, scoredRuns, selfReported, eventCredited := state.ProgressYield(window)
	if scoredRuns == 0 {
		return D1Verdict{Window: window}
	}

	yield := float64(progressRuns) / float64(scoredRuns)
	v := D1Verdict{
		Yield:         round4(yield),
		ProgressRuns:  progressRuns,
		ScoredRuns:    scoredRuns,
		SelfReported:  selfReported,
		EventCredited: eventCredited,
		Window:        scoredRuns,
		HasRuns:       true,
	}
	v.Raw = d1RawFromYield(yield)
	// Weighted = raw/5 × 30, rounded to two decimals (D1's rubric weight is 30).
	v.Weighted = round2(float64(v.Raw) / float64(D1MaxRaw) * float64(D1Weight))
	return v
}

// ScoreGardenerD1Blob is the raw-JSON convenience wrapper: it decodes the
// `gardener` blob (the artifact the engine persists) and scores D1. An empty or
// unparseable blob yields the UNMEASURABLE verdict (HasRuns=false, Raw=0), the
// same fail-soft shape the safety classifiers give.
func ScoreGardenerD1Blob(rawBlob string, window int) D1Verdict {
	spec, ok := decodeGardenerSpec(strings.TrimSpace(rawBlob))
	if !ok {
		if window <= 0 {
			window = d1DefaultWindow
		}
		return D1Verdict{Window: window}
	}
	return ScoreGardenerD1(spec, window)
}

// d1RawFromYield maps a progress yield to the rubric §D1 grade band. The bands
// are the AUTHORITY (they are what the meta-scorer reads and the D1/D2
// consistency guard references); the rubric's round(5×yield) note is their
// origin but disagrees at the boundaries (round(5×0.8)=4, band says 5), so the
// table wins:
//
//	yield >= 0.80        -> 5  (almost every wake does something)
//	0.60 <= yield < 0.80 -> 4  (healthy)
//	0.40 <= yield < 0.60 -> 3  (half its wakes wasted)
//	0.20 <= yield < 0.40 -> 2  (mostly spinning)
//	yield >  0           -> 1  (barely moving)
//	yield == 0           -> 0  (pure spin — stop/redesign candidate)
func d1RawFromYield(yield float64) int {
	switch {
	case yield >= 0.80:
		return 5
	case yield >= 0.60:
		return 4
	case yield >= 0.40:
		return 3
	case yield >= 0.20:
		return 2
	case yield > 0:
		return 1
	default:
		return 0
	}
}

// round2 rounds to two decimals (half-up).
func round2(f float64) float64 {
	return float64(int(f*100+0.5)) / 100
}

// round4 rounds to four decimals (half-up).
func round4(f float64) float64 {
	return float64(int(f*10000+0.5)) / 10000
}
