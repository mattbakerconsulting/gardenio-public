package main

// Gardener trajectory strip (task ka-0000003359, parent ka-0000001553).
//
// The gardener page renders run counters + a newest-first flat list of
// iteration cards, but conveys NO sense of trajectory: is the gardener winning,
// stalling, or stuck? This file parses the gardener's ProgressState
// (GardenerSpec.ProgressState — the rolling scheduler/progress.State blob) into
// human signals and a compact sparkline, plus a one-line health verdict, so the
// header reads as a narrative instead of a raw <pre> JSON dump.
//
// Inputs (best-effort, all optional):
//   - progress.State.Runs   — per-run closed summaries (ProgressScore, AgentState)
//   - progress.State.Events  — credited outcome events (mr_merged, child_closed, …)
//   - GardenerSpec counters    — RunsSpawned / RunsCompleted / ConsecutiveFailures,
//                               used as a legacy fallback when ProgressState is
//                               absent (older gardeners never wrote one).

import (
	"encoding/json"
	"fmt"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/progress"
)

// gardenerTrajectorySparkRuns caps how many runs the trajectory sparkline draws.
// The strip stays ~one row tall on a typical viewport at this width while still
// showing enough history to read the gardener's direction at a glance; older
// runs beyond this are summarized only in the aggregate counters.
const gardenerTrajectorySparkRuns = 24

// gardenerStuckFailureThreshold is the consecutive-failure count at which the
// verdict flips to "stuck". Two failures can be noise (a flaky run, a transient
// CI red); three in a row is a pattern worth flagging.
const gardenerStuckFailureThreshold = 3

// gardenerStalledMinRuns is the minimum number of recorded runs before a
// zero-outcome gardener is called "stalled" — below this it is just "starting
// up" and the operator should not read inactivity as failure.
const gardenerStalledMinRuns = 3

// gardenerTrajectory is the parsed, human-readable progress narrative rendered
// above the life-thread. Empty (HasData=false) when the gardener has neither a
// ProgressState nor any spec counters — the template suppresses the whole strip.
type gardenerTrajectory struct {
	HasData bool

	// Verdict is the one-line health call surfaced in the header:
	// "making progress" / "stalled" / "stuck" / "starting up".
	// VerdictKind is the CSS hook ("progress" | "stalled" | "stuck" | "neutral").
	// VerdictDetail is the parenthetical evidence ("12 runs, 4 with outcomes").
	Verdict       string
	VerdictKind   string
	VerdictDetail string

	// Runs is the per-run sparkline, oldest→newest (so the bar strip reads
	// left-to-right as the gardener's life advances). Bounded by
	// gardenerTrajectorySparkRuns; SparkOmitted is how many older runs were
	// dropped from the head so the template can label the truncation.
	Runs         []gardenerRunGlyph
	SparkOmitted int

	// Aggregate signals.
	TotalRuns int // runs recorded in ProgressState
	// OutcomeRuns is the count of runs that produced an outcome: a run that
	// closed `done` OR scored positive progress (failed runs never count).
	// A run completing cleanly IS an outcome — most done-via-gb-stop runs and
	// continual-audit/scorecard gardeners score 0 yet finish their work, so
	// counting only ProgressScore>0 mis-reads them as "0 outcomes" (ka-0000002776).
	OutcomeRuns int
	// ScoredRuns is the subset of OutcomeRuns that carried a positive
	// ProgressScore (explicit forward motion), used only to phrase the detail.
	ScoredRuns          int
	OutcomeRate         int // OutcomeRuns / TotalRuns, percent (0 when no runs)
	MRsMerged           int // distinct mr_merged events credited
	BeadsClosed         int // distinct child_closed events credited
	DecisionsResolved   int // distinct decision_resolved events credited
	ConsecutiveFailures int // trailing failed-run streak (spec-owned)
}

// gardenerRunGlyph is one bar in the trajectory sparkline. Outcome drives the
// bar color; Height (0..100) scales the bar to the busiest run so a relative
// trend is visible even when absolute scores are small.
type gardenerRunGlyph struct {
	RunNumber int
	Outcome   string // "ok" (scored>0) | "fail" (AgentState failed) | "flat" (closed, no score)
	Score     int
	Height    int
	Title     string
}

// buildGardenerTrajectory parses a gardener's ProgressState + spec counters into
// the trajectory view-model. It never errors: a malformed/empty ProgressState
// degrades to the spec-counter fallback (or to HasData=false when there is
// nothing at all to show), so the gardener page always renders.
func buildGardenerTrajectory(spec *bridge.GardenerSpec) gardenerTrajectory {
	if spec == nil {
		return gardenerTrajectory{}
	}

	state, _ := progress.ParseState(string(spec.ProgressState))
	t := gardenerTrajectory{
		ConsecutiveFailures: spec.ConsecutiveFailures,
		TotalRuns:           len(state.Runs),
	}

	// Outcome events (deduped already by the engine's AppendEvent).
	for _, e := range state.Events {
		switch e.Kind {
		case progress.EventKindMRMerged:
			t.MRsMerged++
		case progress.EventKindChildClosed:
			t.BeadsClosed++
		case progress.EventKindDecisionResolved:
			t.DecisionsResolved++
		}
	}

	// Per-run sparkline + outcome tally. Find the peak score first so bars
	// scale relative to the gardener's own busiest run. A run counts as an
	// outcome when it either scored progress OR simply closed `done` — a clean
	// completion is a real outcome, not a stall (ka-0000002776).
	maxScore := 0
	for _, r := range state.Runs {
		if r.ProgressScore > maxScore {
			maxScore = r.ProgressScore
		}
		if r.ProgressScore > 0 {
			t.ScoredRuns++
		}
		if runIsOutcome(r) {
			t.OutcomeRuns++
		}
	}
	if t.TotalRuns > 0 {
		t.OutcomeRate = t.OutcomeRuns * 100 / t.TotalRuns
	}

	// Sparkline: keep the most-recent gardenerTrajectorySparkRuns, oldest→newest.
	runs := state.Runs
	if len(runs) > gardenerTrajectorySparkRuns {
		t.SparkOmitted = len(runs) - gardenerTrajectorySparkRuns
		runs = runs[len(runs)-gardenerTrajectorySparkRuns:]
	}
	for i, r := range runs {
		t.Runs = append(t.Runs, gardenerRunGlyph{
			RunNumber: t.SparkOmitted + i + 1,
			Outcome:   runGlyphOutcome(r),
			Score:     r.ProgressScore,
			Height:    runGlyphHeight(r.ProgressScore, maxScore),
			Title:     runGlyphTitle(t.SparkOmitted+i+1, r),
		})
	}

	// HasData: render the strip when we have either parsed runs/events OR
	// spec counters worth narrating (legacy gardeners with no ProgressState).
	hasEvents := t.MRsMerged+t.BeadsClosed+t.DecisionsResolved > 0
	hasSpecActivity := spec.RunsSpawned > 0 || spec.RunsCompleted > 0 || spec.ConsecutiveFailures > 0
	t.HasData = t.TotalRuns > 0 || hasEvents || hasSpecActivity
	if !t.HasData {
		return t
	}

	// effectiveRuns prefers the ProgressState run count, falling back to the
	// spec's RunsCompleted so the verdict still reads for legacy gardeners.
	effectiveRuns := t.TotalRuns
	if effectiveRuns == 0 {
		effectiveRuns = spec.RunsCompleted
	}

	// run_log outcome signal (ka-0000003373): the trajectory verdict counts
	// outcomes ONLY from ProgressState (scored runs + credited events). But most
	// legacy gardeners never wrote a ProgressState, so they read as "0 outcomes →
	// stalled" even when every run completed cleanly. The per-run `run_log` is the
	// authoritative record of how each run actually ended — when ProgressState is
	// absent, a clean run_log (recent runs all `done`, no trailing failures) is a
	// real outcome signal, so treat it as one rather than flagging a healthy
	// gardener. We only consult it as a fallback: a present ProgressState is the
	// richer source and already drives the verdict above.
	hasCleanRunLog := t.TotalRuns == 0 && cleanRunLog(spec.RunLog)

	t.Verdict, t.VerdictKind, t.VerdictDetail = gardenerVerdict(t, effectiveRuns, hasEvents, hasCleanRunLog)
	return t
}

// cleanRunLog reports whether the gardener's run_log shows healthy completion:
// at least one recorded run, and every recorded run ended `done` (no failed or
// otherwise-non-done outcome). This is the fallback outcome signal used when a
// gardener never wrote a ProgressState — a gardener that has been quietly
// completing run after run is "making progress", not "stalled". A malformed or
// empty run_log returns false (no signal), so callers fall through to the
// existing ProgressState/spec-counter logic.
func cleanRunLog(raw json.RawMessage) bool {
	rl, err := scheduler.ParseRunLog(string(raw))
	if err != nil || len(rl) == 0 {
		return false
	}
	for _, e := range rl {
		// An empty Outcome predates outcome recording — treat as unknown, not
		// clean, so we don't manufacture a positive signal from a blank field.
		if e.Outcome != "done" {
			return false
		}
	}
	return true
}

// runIsOutcome reports whether a recorded run produced an outcome for the
// verdict tally. A run that closed `done` counts — completing cleanly is the
// outcome for done-via-gb-stop runs and continual-audit/scorecard gardeners that
// finish their work without ever scoring positive progress. A run that scored
// positive progress also counts (explicit forward motion). A `failed` run never
// counts, and an empty AgentState (predates state recording) only counts when it
// scored, so we never manufacture a positive signal from a blank field.
func runIsOutcome(r progress.RunRecord) bool {
	if r.AgentState == "failed" {
		return false
	}
	return r.AgentState == "done" || r.ProgressScore > 0
}

// runGlyphOutcome classifies one run for the sparkline color.
func runGlyphOutcome(r progress.RunRecord) string {
	switch {
	case r.AgentState == "failed":
		return "fail"
	case r.ProgressScore > 0:
		return "ok"
	default:
		return "flat"
	}
}

// runGlyphHeight scales a run's progress score to 0..100 relative to the
// busiest run. Floors a positive score at 12% so a small win is still visible
// next to a tall bar; a zero/failed run renders a 4% stub so the column is not
// invisible in the strip.
func runGlyphHeight(score, maxScore int) int {
	if score <= 0 || maxScore <= 0 {
		return 4
	}
	h := score * 100 / maxScore
	if h < 12 {
		return 12
	}
	return h
}

// runGlyphTitle composes the per-bar hover tooltip.
func runGlyphTitle(runNumber int, r progress.RunRecord) string {
	state := r.AgentState
	if state == "" {
		state = "closed"
	}
	if r.ProgressSummary != "" {
		return fmt.Sprintf("Run #%d · %s · score %d · %s", runNumber, state, r.ProgressScore, r.ProgressSummary)
	}
	return fmt.Sprintf("Run #%d · %s · score %d", runNumber, state, r.ProgressScore)
}

// gardenerVerdict computes the one-line health call. Priority order:
//
//  1. stuck   — consecutive failures have crossed the threshold (the most
//     actionable signal: the gardener is failing the same way repeatedly).
//  2. stalled — enough runs to judge, but zero outcomes: no run closed `done`,
//     no scored runs, no credited mr/bead/decision events, AND no clean
//     run_log. The gardener is spinning (its runs are failing or never
//     completing), not quietly succeeding.
//  3. starting up — has activity but too few runs to call stalled yet.
//  4. making progress — outcomes exist; the gardener is doing its job. A run
//     that closed `done` IS an outcome (ka-0000002776), so a gardener whose runs
//     all complete cleanly reads as making progress even without scored runs.
//
// hasCleanRunLog is the legacy fallback: a gardener with no ProgressState whose
// run_log shows every run completing cleanly counts as making progress, not
// stalled — its runs really did succeed, the verdict logic just never saw them
// because they weren't scored into a ProgressState (ka-0000003373).
func gardenerVerdict(t gardenerTrajectory, effectiveRuns int, hasEvents, hasCleanRunLog bool) (verdict, kind, detail string) {
	if t.ConsecutiveFailures >= gardenerStuckFailureThreshold {
		return "stuck",
			"stuck",
			fmt.Sprintf("%d consecutive failures", t.ConsecutiveFailures)
	}

	hasOutcome := t.OutcomeRuns > 0 || hasEvents
	if !hasOutcome {
		if hasCleanRunLog {
			return "making progress",
				"progress",
				fmt.Sprintf("%s completed cleanly", runsPhrase(effectiveRuns))
		}
		if effectiveRuns >= gardenerStalledMinRuns {
			return "stalled",
				"stalled",
				fmt.Sprintf("%s, 0 outcomes", runsPhrase(effectiveRuns))
		}
		return "starting up",
			"neutral",
			fmt.Sprintf("%s so far", runsPhrase(effectiveRuns))
	}

	return "making progress",
		"progress",
		gardenerProgressDetail(t)
}

// gardenerProgressDetail summarizes the positive signals for the "making
// progress" verdict, leading with the strongest evidence available.
func gardenerProgressDetail(t gardenerTrajectory) string {
	switch {
	case t.MRsMerged > 0 && t.BeadsClosed > 0:
		return fmt.Sprintf("%s merged, %s closed", mrPhrase(t.MRsMerged), beadsPhrase(t.BeadsClosed))
	case t.MRsMerged > 0:
		return fmt.Sprintf("%s merged", mrPhrase(t.MRsMerged))
	case t.BeadsClosed > 0:
		return fmt.Sprintf("%s closed", beadsPhrase(t.BeadsClosed))
	case t.DecisionsResolved > 0:
		return fmt.Sprintf("%d decision%s resolved", t.DecisionsResolved, plural(t.DecisionsResolved))
	case t.ScoredRuns > 0:
		return fmt.Sprintf("%d of %s produced output", t.ScoredRuns, runsPhrase(t.TotalRuns))
	default:
		// No scored runs and no credited events, but runs closed cleanly — the
		// outcome is the clean completion itself (scorecard/keep-fresh gardeners).
		return fmt.Sprintf("%s completed cleanly", runsPhrase(t.OutcomeRuns))
	}
}

func runsPhrase(n int) string  { return fmt.Sprintf("%d run%s", n, plural(n)) }
func mrPhrase(n int) string    { return fmt.Sprintf("%d MR%s", n, plural(n)) }
func beadsPhrase(n int) string { return fmt.Sprintf("%d bead%s", n, plural(n)) }

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
