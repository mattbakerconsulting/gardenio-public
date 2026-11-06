// Package bridge — gardener D6 LOOP-HYGIENE scorer (ka-0000000452).
//
// The gardener effectiveness rubric (gardenio/docs/gardener-effectiveness-rubric.md)
// scores every live gardener on six dimensions. Five of them (D1 progress-yield,
// D2 throughput, D3 compute-economy, D4 crew-health, D5 operator-burden) are
// RUNTIME signals — they need the gardener's run_log, its epic's child counts, the
// crew it dispatched, the decisions it raised. They can only be judged by the
// meta-gardener agent with a live view of the fleet.
//
// D6 (loop hygiene, weight 10) is different: it is a STATIC checklist scored
// against the gardener bead's OWN config — exactly the fields the structured-stop
// work (epic ka-0000003900) just made first-class on the `gardener` blob. Until
// now D6 was prose in the rubric that the meta-gardener eyeballed by hand, so two
// meta runs could score the same gardener differently and nothing pinned the
// definition. This file makes D6 MACHINE-ENFORCEABLE: one deterministic function
// that turns the rubric's five D6 checks into a 0–5 score plus a per-check
// breakdown, computed off the same decoded GardenerSpec the engine and the
// self-block classifier already use.
//
// It is the natural completion of the two-axis safety picture in
// gardener_selfblock.go. That file answers "does this gardener have an emergency
// backstop?" (ValidateGardenerBounds) and "does it have a progress-aware
// self-block?" (ClassifyGardenerSafety). D6 is the RUBRIC roll-up of those axes
// plus cadence sanity and a non-trivial goal — the score the meta-gardener feeds
// into the weighted grade. Because it reuses ClassifyGardenerSafety for the
// stop-condition checks, the D6 score can never disagree with the self-block /
// backstop verdicts `gb gardener status` renders.
//
// This is PURELY ADVISORY, like the self-block axis: nothing here changes the
// engine's fire-time behavior. It exists so the meta-gardener (and `gb gardener
// status`) computes the SAME D6 the rubric documents, deterministically, instead
// of re-deriving it by eye every run.
package bridge

import (
	"strings"

	"gardenio/controller/internal/scheduler"
)

// D6MaxRaw is the maximum raw D6 score — one point per rubric check. It matches
// the "0–5" scale in gardener-effectiveness-rubric.md §D6, so a caller can weight
// it identically to the doc (Score contribution = D6Raw/5 × 10).
const D6MaxRaw = 5

// D6Check is one row of the rubric's D6 checklist: a boolean the config either
// satisfies or does not, with a human-readable reason for the miss.
type D6Check struct {
	// Name is the stable slug the rubric uses for this check (e.g.
	// "non_trivial_terminal_goal"). Pinned by TestD6CheckNames_Doc so the meta
	// scorer and the rubric can never drift on the check vocabulary.
	Name string `json:"name"`
	// Pass is whether the gardener config satisfies this check.
	Pass bool `json:"pass"`
	// Reason explains a miss (empty when Pass). Present so `gb gardener status`
	// and the meta scorecard can name exactly which hygiene rule failed.
	Reason string `json:"reason,omitempty"`
}

// D6Verdict is the full loop-hygiene roll-up for one gardener: the 0–5 raw score,
// the per-check breakdown, and the weighted contribution to the rubric total.
type D6Verdict struct {
	// Raw is the number of passing checks, 0–D6MaxRaw. This is the "D6_raw" the
	// rubric's Score = Σ(Di_raw/5 × weighti) formula consumes.
	Raw int `json:"raw"`
	// Weighted is Raw/5 × 10 — D6's contribution to the 0–100 rubric score,
	// rounded to two decimals. Surfaced so a caller can sum dimensions directly.
	Weighted float64 `json:"weighted"`
	// Checks is the ordered per-check breakdown (rubric §D6 order).
	Checks []D6Check `json:"checks"`
}

// The rubric's default terminal goal shape — the "bare default" §D6 explicitly
// says does NOT earn the non-trivial-goal point. `gb gardener start` emits
// "All children of <epic> are closed." when the author gives no --terminal-goal;
// a gardener still carrying exactly that (modulo the epic id) has not declared a
// real goal. We detect it structurally rather than string-matching one epic id.
const (
	d6DefaultGoalPrefix = "all children of "
	d6DefaultGoalSuffix = " are closed."
)

// d6TrivialGoalMinWords is the floor below which a terminal_goal is treated as
// too thin to count as "non-trivial". The rubric wants a real declared goal, not
// a one-word placeholder; the default template is ~5 words, so anything shorter
// than this is certainly a stub. Kept deliberately low so it only catches
// genuine placeholders, never a terse-but-real standing mandate.
const d6TrivialGoalMinWords = 4

// isTrivialTerminalGoal reports whether a terminal_goal is the bare default or an
// empty/stub placeholder — the shapes rubric §D6 does not reward. A goal is
// trivial when it is empty, shorter than d6TrivialGoalMinWords words, or matches
// the "All children of <X> are closed." default template `gb gardener start`
// emits with no --terminal-goal. Any author-written goal (a standing mandate, a
// specific completion condition) is non-trivial and passes.
func isTrivialTerminalGoal(goal string) bool {
	g := strings.TrimSpace(goal)
	if g == "" {
		return true
	}
	if len(strings.Fields(g)) < d6TrivialGoalMinWords {
		return true
	}
	lower := strings.ToLower(g)
	if strings.HasPrefix(lower, d6DefaultGoalPrefix) && strings.HasSuffix(lower, d6DefaultGoalSuffix) {
		return true
	}
	return false
}

// ScoreGardenerD6 computes the rubric's D6 loop-hygiene dimension for a decoded
// GardenerSpec and the gardener trigger bead's cron expression. It is the single
// deterministic answer any surveyor (the meta-gardener, `gb gardener status`, a
// future evaluator) must consult so no two runs disagree on D6.
//
// cron is the trigger bead's `cron` field (empty for a pure event-wake gardener);
// it is only consulted for the cadence-sanity check, which passes for any
// parseable cron and for the empty (event-driven) case.
//
// The five checks mirror gardener-effectiveness-rubric.md §D6 exactly:
//  1. non_trivial_terminal_goal — a real declared goal, not the bare default.
//  2. run_budget                — max_runs / run_budget > 0 (advice ka-0000000974).
//  3. intelligent_loop_end      — a progress-aware or bead_query/goal loop_end
//     kind (not wall-clock-only). Reuses the self-block classifier so it agrees
//     with `gb gardener status` by construction.
//  4. sane_cadence              — the cron parses, OR the gardener is pure
//     event-wake (no cron). A malformed cron that never fires is the shape §D6
//     dings; the finer "cadence matches the work's tempo" judgement needs live
//     epic-tempo context the static config lacks, so it stays a meta-gardener
//     judgement call — this check enforces the objective floor (a fireable cron).
//  5. not_wall_clock_only       — the gardener would stop on a REAL condition
//     (a progress-aware wedge detector: no_progress_for + a progress spec)
//     before its wall-clock backstop, rather than surviving ONLY because the
//     wall-clock has not elapsed. This is §D6's fifth check ("not currently
//     surviving only because of the wall-clock backstop"); it reuses the
//     self-block classifier's ProgressAware axis, so a gardener graded
//     `self-block: ok` earns the point and a `weak`/`none` one does not.
//
// A nil spec is not a gardener — every check fails and Raw is 0 (fail-closed,
// matching ClassifyGardenerSafety). The cron alone does not make a bead a gardener,
// so even a valid cadence does not earn a point without a loop spec to be
// hygienic about.
func ScoreGardenerD6(spec *GardenerSpec, cron string) D6Verdict {
	if spec == nil || !gardenerSpecHasLoopContent(spec) {
		// A nil spec, or a present-but-contentless spec (an empty `{}` / `null`
		// blob), is not a gardener worth scoring for loop hygiene — it declares no
		// goal, no budget, no stop, no progress. Fail-closed to 0 so the
		// "unmanaged" gardener the audit flags lines up with a D6 of 0, rather than
		// scoring a stray point because the trigger bead's cron happens to parse.
		return d6NoSpecVerdict()
	}

	safety := ClassifyGardenerSafety(spec)

	checks := []D6Check{
		d6GoalCheck(spec.TerminalGoal),
		d6RunBudgetCheck(safety),
		d6LoopEndCheck(safety),
		d6CadenceCheck(cron),
		d6NotWallClockOnlyCheck(safety),
	}

	raw := 0
	for _, c := range checks {
		if c.Pass {
			raw++
		}
	}
	// Weighted = raw/5 × 10, rounded to two decimals (D6's rubric weight is 10).
	weighted := float64(raw) / float64(D6MaxRaw) * 10.0
	weighted = float64(int(weighted*100+0.5)) / 100

	return D6Verdict{Raw: raw, Weighted: weighted, Checks: checks}
}

// ScoreGardenerD6Blob is the raw-JSON convenience wrapper: it decodes the `gardener`
// blob (the artifact the engine persists) and scores D6. An empty or unparseable
// blob scores 0 across the board, the same fail-closed verdict the safety
// classifiers give.
func ScoreGardenerD6Blob(rawBlob, cron string) D6Verdict {
	spec, ok := decodeGardenerSpec(strings.TrimSpace(rawBlob))
	if !ok {
		return d6NoSpecVerdict()
	}
	return ScoreGardenerD6(spec, cron)
}

// gardenerSpecHasLoopContent reports whether a decoded spec carries ANY
// loop-defining content — a goal, a run budget, a stop block, or a progress spec.
// An `{}` / `null` blob decodes to a valid but contentless spec (decodeGardenerSpec
// returns ok=true); such a spec is not a gardener loop, so D6 fail-closes it to 0
// rather than crediting a stray cadence point. Runtime-only fields (RunsSpawned,
// LastAgentID, …) do NOT count — a blob seeded with only engine counters (the
// identity-only backfill shape, ka-0000007784) has no authored loop to be
// hygienic about.
func gardenerSpecHasLoopContent(spec *GardenerSpec) bool {
	if spec == nil {
		return false
	}
	if strings.TrimSpace(spec.TerminalGoal) != "" {
		return true
	}
	if spec.RunBudget > 0 || spec.MaxRuns > 0 {
		return true
	}
	if len(spec.Stop) > 0 && strings.TrimSpace(string(spec.Stop)) != "null" {
		return true
	}
	if gardenerHasProgressSpec(spec) {
		return true
	}
	return false
}

// d6NoSpecVerdict is the fail-closed 0/5 verdict for a bead with no gardener loop
// spec (empty/unparseable blob, or a nil spec). Every check fails with a "no
// gardener loop spec" reason so the breakdown is legible rather than an empty
// list — mirroring how ClassifyGardenerSafety reports the same shape.
func d6NoSpecVerdict() D6Verdict {
	const reason = "no gardener loop spec"
	checks := []D6Check{
		{Name: "non_trivial_terminal_goal", Reason: reason},
		{Name: "run_budget", Reason: reason},
		{Name: "intelligent_loop_end", Reason: reason},
		{Name: "sane_cadence", Reason: reason},
		{Name: "not_wall_clock_only", Reason: reason},
	}
	return D6Verdict{Raw: 0, Weighted: 0, Checks: checks}
}

func d6GoalCheck(goal string) D6Check {
	c := D6Check{Name: "non_trivial_terminal_goal"}
	if isTrivialTerminalGoal(goal) {
		c.Reason = "terminal_goal is empty or the bare default (\"All children of <epic> " +
			"are closed.\") — set a real declared goal so the loop has a meaningful done-state"
		return c
	}
	c.Pass = true
	return c
}

func d6RunBudgetCheck(safety GardenerSafetyVerdict) D6Check {
	c := D6Check{Name: "run_budget"}
	if safety.RunBudget <= 0 {
		c.Reason = "no run_budget / max_runs set (advice ka-0000000974) — the loop has no run ceiling"
		return c
	}
	c.Pass = true
	return c
}

func d6LoopEndCheck(safety GardenerSafetyVerdict) D6Check {
	c := D6Check{Name: "intelligent_loop_end"}
	for _, k := range safety.LoopEndKinds {
		if IntelligentLoopEndKinds[k] {
			c.Pass = true
			return c
		}
	}
	c.Reason = "no progress-aware or bead_query loop_end kind " +
		"(no_progress_for/bead_query/specific_bead_closed/max_runs) — the loop can only " +
		"end on the wall-clock backstop, never on real progress or goal completion"
	return c
}

func d6CadenceCheck(cron string) D6Check {
	c := D6Check{Name: "sane_cadence"}
	trimmed := strings.TrimSpace(cron)
	if trimmed == "" {
		// A pure event-wake gardener (no cron) has no cadence to mis-tune — it
		// fires only on real events. That is the healthiest cadence, so it
		// passes rather than being penalized for an absent cron.
		c.Pass = true
		return c
	}
	if _, err := scheduler.ParseCron(trimmed); err != nil {
		c.Reason = "cron does not parse (" + err.Error() + ") — the gardener either never " +
			"fires or fires on a malformed schedule"
		return c
	}
	c.Pass = true
	return c
}

func d6NotWallClockOnlyCheck(safety GardenerSafetyVerdict) D6Check {
	c := D6Check{Name: "not_wall_clock_only"}
	if !safety.ProgressAware {
		c.Reason = "no progress-aware wedge detector (no_progress_for + a progress spec) — " +
			"the gardener cannot stop on a stall and so survives only until its wall-clock " +
			"backstop elapses (rubric §D6: 'not surviving only because of the wall-clock backstop')"
		return c
	}
	c.Pass = true
	return c
}
