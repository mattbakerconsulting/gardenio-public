// Package bridge — gardener D2 THROUGHPUT scorer + the D1/D2 consistency guard
// (task ka-0000003456, Phase 5 of the "formalize gardeners" epic ka-0000004384).
//
// The gardener effectiveness rubric (gardenio/docs/gardener-effectiveness-rubric.md)
// scores every live gardener on six dimensions. Two were already made
// machine-enforceable — D6 loop-hygiene (gardener_d6.go, ka-0000000452) and D1
// progress-yield (gardener_d1.go, ka-0000004982). This file completes the pair
// D1 depends on: D2 throughput, plus the MANDATORY D1/D2 consistency guard that
// ties them together.
//
// D2 (throughput, weight 20) is the rubric's independent cross-check on D1: it
// measures the SAME underlying fact — the epic's children closing — but from a
// different primitive (net epic closures over the window, not per-run credit).
// The rubric formula (§D2) is:
//
//	closed_in_window = children closed since last scoring
//	open_now         = open|in_progress|blocked children now
//	burn_rate        = closed_in_window / max(1, closed_in_window + open_now)
//	D2_raw           = clamp(round(5 × min(1, burn_rate / 0.15)), 0, 5)
//
// The guard (§2.1, load-bearing for the whole D1 design — see
// d1-progress-signal-design.md §2.2) is: **if D2 ≥ 4 then D1 must be ≥ 3.** A
// computed D1==0 alongside D2≥4 is definitionally a measurement bug in the D1
// count — a gardener whose children are actively closing is not "pure spin" — so
// the scorer clamps D1 up instead of emitting the false-negative grade. This was
// the exact two-runs-two-grades bug (ka-0000003502): a delegating gardener whose
// crew close children asynchronously read D1=0 one window and D1=100% the next
// with no config change. Until now the guard was PROSE the meta-gardener applied
// by eye; this makes it deterministic so no two runs disagree.
//
// Like the D1 and D6 scorers this is PURELY ADVISORY — it changes no engine
// fire-time behavior. It exists so the meta-gardener (and `gb gardener status`)
// computes the SAME D2 and applies the SAME guard the rubric documents, off the
// same graph facts, instead of re-deriving them by eye.
package bridge

// D2MaxRaw is the maximum raw D2 score — the 0–5 scale in the rubric §D2 grade
// formula. Score contribution = D2Raw/5 × 20 (D2's rubric weight is 20).
const D2MaxRaw = 5

// D2Weight is D2's weight in the 0–100 rubric total (§1: weights sum to 100).
const D2Weight = 20

// d2FullMarkBurnRate is the rubric §D2 target: closing ~15% of remaining work
// per scoring window earns full marks. The formula normalizes the observed
// burn_rate against this floor (min(1, burn_rate/0.15)) so a gardener clearing
// 15%+ of its open+closed frontier scores 5.
const d2FullMarkBurnRate = 0.15

// d1d2GuardMinD1 is the floor the D1/D2 consistency guard clamps D1_raw up to
// when D2 shows children actively closing (D2_raw >= d1d2GuardTripD2). The
// rubric §2.1 wording is "if D2 >= 4 then D1 must be >= 3".
const (
	d1d2GuardTripD2 = 4
	d1d2GuardMinD1  = 3
)

// D2Verdict is the throughput roll-up for one gardener: the 0–5 raw score, the
// burn rate it derived from, the closed/open frontier counts, and the weighted
// contribution to the rubric total.
type D2Verdict struct {
	// Raw is the 0–D2MaxRaw score from the rubric §D2 formula. This is the
	// "D2_raw" the rubric's Score = Σ(Di_raw/5 × weighti) formula consumes.
	Raw int `json:"raw"`
	// Weighted is Raw/5 × 20 — D2's contribution to the 0–100 rubric score,
	// rounded to two decimals.
	Weighted float64 `json:"weighted"`
	// BurnRate is closed_in_window / max(1, closed_in_window + open_now),
	// rounded to four decimals — the fraction of the epic's live frontier the
	// gardener cleared this window.
	BurnRate float64 `json:"burn_rate"`
	// ClosedInWindow is the count of the epic's children closed inside the
	// scoring window (the numerator).
	ClosedInWindow int `json:"closed_in_window"`
	// OpenNow is the count of open|in_progress|blocked children now (the rest
	// of the denominator).
	OpenNow int `json:"open_now"`
	// EpicDrained is true when the epic has NO open children AND closed none
	// this window — the rubric explicitly does NOT penalize this as a
	// throughput 0; it is a STOP CANDIDATE (§D2 / §4 / D6), not "pure spin". A
	// caller must not read Raw=0 with EpicDrained=true as a real throughput
	// failure.
	EpicDrained bool `json:"epic_drained"`
}

// ScoreGardenerD2 computes the rubric's D2 throughput dimension from the two
// graph counts the meta-gardener scoring window defines: the number of the
// gardener's epic children that CLOSED inside the window, and the number OPEN
// now. It is the single deterministic answer any surveyor (the meta-gardener,
// `gb gardener status`, a future evaluator) consults so no two runs disagree on
// D2 or on the D1/D2 guard it feeds.
//
// Negative inputs are floored to 0 (fail-soft, matching the other scorers). A
// fully drained epic (closedInWindow==0 && openNow==0) returns Raw=0 with
// EpicDrained=true — the rubric's "not penalized here, stop candidate" case —
// so a caller can distinguish it from a genuine "closed nothing while work
// remains" D2=0.
func ScoreGardenerD2(closedInWindow, openNow int) D2Verdict {
	if closedInWindow < 0 {
		closedInWindow = 0
	}
	if openNow < 0 {
		openNow = 0
	}

	if closedInWindow == 0 && openNow == 0 {
		// No frontier at all: the epic has drained. Per rubric §D2 this is NOT a
		// throughput failure — it is a stop candidate. Report Raw 0 but flag it
		// so the caller (and the D1/D2 guard) treat it as "no signal", never as
		// a "pure spin" 0.
		return D2Verdict{EpicDrained: true}
	}

	// burn_rate = closed / max(1, closed + open). The max(1, …) guard is
	// redundant here (we already returned on the all-zero case) but mirrors the
	// rubric formula verbatim so the code and the doc read identically.
	denom := closedInWindow + openNow
	if denom < 1 {
		denom = 1
	}
	burn := float64(closedInWindow) / float64(denom)

	// D2_raw = clamp(round(5 × min(1, burn/0.15)), 0, 5).
	norm := burn / d2FullMarkBurnRate
	if norm > 1 {
		norm = 1
	}
	raw := roundHalfUp(5 * norm)
	if raw < 0 {
		raw = 0
	}
	if raw > D2MaxRaw {
		raw = D2MaxRaw
	}

	return D2Verdict{
		Raw:            raw,
		Weighted:       round2(float64(raw) / float64(D2MaxRaw) * float64(D2Weight)),
		BurnRate:       round4(burn),
		ClosedInWindow: closedInWindow,
		OpenNow:        openNow,
	}
}

// ApplyD1D2Guard enforces the rubric's mandatory D1/D2 consistency guard (§2.1)
// on an already-computed D1 verdict: if D2 shows children actively closing
// (d2Raw >= 4) but D1 landed below 3, the D1 count is definitionally a
// measurement bug (a gardener whose children are closing is not spinning), so
// D1_raw is clamped up to 3, its weighted contribution recomputed, and
// GuardApplied set so the readout explains WHY D1 is not the low grade the raw
// yield implied.
//
// The guard is a no-op when d2Raw < 4, when D1 is already >= 3, or when D2 is
// EpicDrained (no frontier — no cross-check to assert). It never LOWERS D1: the
// rubric guard is a floor against false negatives, not a ceiling. Returns the
// (possibly clamped) verdict; the input is not mutated.
func ApplyD1D2Guard(d1 D1Verdict, d2 D2Verdict) D1Verdict {
	if d2.EpicDrained || d2.Raw < d1d2GuardTripD2 || d1.Raw >= d1d2GuardMinD1 {
		return d1
	}
	d1.Raw = d1d2GuardMinD1
	d1.Weighted = round2(float64(d1.Raw) / float64(D1MaxRaw) * float64(D1Weight))
	d1.GuardApplied = true
	return d1
}

// roundHalfUp rounds a non-negative float to the nearest integer, halves up.
// Kept package-local (not a float→float rounder like round2/round4) so the D2
// band math reads as the rubric's round(…) does.
func roundHalfUp(f float64) int {
	return int(f + 0.5)
}
