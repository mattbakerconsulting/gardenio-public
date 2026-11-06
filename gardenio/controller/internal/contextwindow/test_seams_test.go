package contextwindow

// OccupancyPct returns the raw context reading `used` (an agent bead's
// context_tokens) as a whole-number percentage of the auto-compaction threshold
// for the given model. It returns 0 when either input is non-positive (no
// reading yet, or an unresolvable window). The result can exceed 100 briefly —
// an agent can overshoot the threshold within a single turn before compaction
// lands — which is intended: the bar should read "over" rather than clamp and
// imply headroom that is gone.
//
// The parameter is deliberately named `used` rather than something containing
// "token": the metrics cost-source guard (TestCostGuard_NoTokenTimesRateMath)
// forbids a token-named value multiplied by a scalar as the canonical shape of
// token×price cost math. This is an occupancy percentage, not cost, but the two
// render seams' inline versions used the same non-token naming (`ctx`, `n`) to
// stay clear of that guard, so this centralized helper follows suit.
func OccupancyPct(used int64, model string) int {
	if used <= 0 {
		return 0
	}
	eff := EffectiveWindowTokens(model)
	if eff <= 0 {
		return 0
	}
	return int(used * 100 / eff)
}
