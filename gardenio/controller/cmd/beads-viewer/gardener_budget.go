package main

// Gardener budget-burn surfacing (task ka-0000007105). The operator's #1 question
// about a running gardener is "when/why does it stop, and how close is it?" The
// stop CONDITIONS are already surfaced legibly (StopSummary / Legible.Stops); this
// file surfaces the run-budget BURN — how many of the gardener's budgeted runs are
// already spent — as a first-class progress meter instead of a pair of plain
// numbers buried in the stats dl.
//
// The engine gates spawns on `RunsSpawned >= RunBudget` (gardener_evaluator.go
// gardenerPreFireCheck gate 1): RunBudget is the effective run ceiling and
// RunsSpawned is the lifetime spawn tally. So the burn ratio RunsSpawned/RunBudget
// is exactly "how close is this gardener to its run ceiling", the number the meter
// visualizes.

// gardenerBudgetBurn is the parsed, render-ready run-budget burn for the meter.
// HasData gates rendering: it is false for a gardener with no run_budget (an
// unlimited gardener) AND for a STANDING gardener even when it carries a run_budget
// (bug ka-0000006238). A standing gardener's run_budget is refilled every ceiling
// hit by the re-arm path and is NEVER an enforced ceiling — the engine exempts a
// standing gardener from both the run_budget gate (gardener_evaluator.go gate 1)
// and the max_runs loop_end kind (gate 3), leaving only the hard_stop brakes
// (consecutive_failures, wall_clock) as real bounds. So a burn bar on a standing
// gardener — often reading RunsSpawned >> RunBudget, "at budget", "over" — is a
// FALSE "about to stop on max_runs / runaway" signal, exactly the misread that
// generated a phantom-runaway investigation (ka-0000006238). For such a gardener
// the meter is suppressed and a "standing (unbounded)" chip renders in its place.
type gardenerBudgetBurn struct {
	HasData bool

	Spawned int // RunsSpawned — lifetime runs the engine has spawned
	Budget  int // RunBudget — the effective run ceiling the spawn gate enforces

	// Pct is Spawned/Budget as a whole-number percentage, CLAMPED to [0,100]
	// for the bar width. A gardener that was resumed (run budget refilled) can
	// briefly carry Spawned > Budget between the gate trip and the refill; the
	// clamp keeps the bar from overflowing while Over records the true overrun.
	Pct int
	// Remaining is Budget-Spawned, floored at 0 — runs left before the ceiling.
	Remaining int
	// Over is true when Spawned >= Budget: the run ceiling is reached/exceeded,
	// so the gardener is at its budget (about to stop on max_runs unless it is
	// standing or gets resumed). Drives the "at budget" styling on the meter.
	Over bool

	// Level buckets the burn for styling: "ok" (<75%), "warn" (75–99%),
	// "over" (>=100%). Kept as a string so the template maps it straight to a
	// CSS modifier class without arithmetic in the view layer.
	Level string
}

// BudgetBurn derives the run-budget burn meter from the gardener's engine
// counters. Returns a zero-value (HasData=false) struct — the template suppresses
// the meter — when the gardener carries no run_budget ceiling to burn against.
func (v gardenerView) BudgetBurn() gardenerBudgetBurn {
	// No ceiling to burn against: an unlimited gardener (no run_budget), OR a
	// standing gardener whose run_budget is non-enforced accounting the engine
	// exempts it from (bug ka-0000006238). Suppress the meter in both cases.
	if v.RunBudget <= 0 || v.IsStanding {
		return gardenerBudgetBurn{}
	}
	b := gardenerBudgetBurn{
		HasData: true,
		Spawned: v.RunsSpawned,
		Budget:  v.RunBudget,
	}
	if b.Spawned < 0 {
		b.Spawned = 0
	}
	b.Remaining = b.Budget - b.Spawned
	if b.Remaining < 0 {
		b.Remaining = 0
	}
	b.Over = b.Spawned >= b.Budget

	pct := b.Spawned * 100 / b.Budget
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	b.Pct = pct

	switch {
	case b.Over:
		b.Level = "over"
	case pct >= 75:
		b.Level = "warn"
	default:
		b.Level = "ok"
	}
	return b
}
