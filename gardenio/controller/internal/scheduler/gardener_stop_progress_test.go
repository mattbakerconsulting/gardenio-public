package scheduler

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/scheduler/progress"
)

// buildProgressState marshals a progress.State carrying the given per-run
// scores (oldest-first) into the opaque JSON that GardenerStopContext expects.
func buildProgressState(t *testing.T, scores ...int) string {
	t.Helper()
	var st progress.State
	for i, s := range scores {
		st.AppendRun(progress.RunRecord{
			AgentID:       string(rune('a' + i)),
			ProgressScore: s,
		})
	}
	raw, err := st.Marshal()
	if err != nil {
		t.Fatalf("marshal progress state: %v", err)
	}
	return raw
}

// TestGardenerStop_NoProgressFor exercises the type=trigger gardener path for the
// no_progress_for stop kind. This kind was previously hard-coded to not-met on
// the trigger path (the dead-code gap that let wedged gardeners run forever); it
// is now backed by ProgressState populated via progress.FinalizeRun.
func TestGardenerStop_NoProgressFor(t *testing.T) {
	entry := StopEntry{Kind: StopKindNoProgressFor, Runs: 3}

	cases := []struct {
		name   string
		scores []int
		want   bool
	}{
		{"three_zero_runs_fires", []int{0, 0, 0}, true},
		{"recent_progress_does_not_fire", []int{0, 0, 1}, false},
		{"older_progress_window_still_zero_fires", []int{1, 0, 0, 0}, true},
		{"not_enough_runs_to_judge", []int{0, 0}, false},
		{"no_runs_at_all", nil, false},
		{"mixed_below_threshold", []int{0, 1, 0}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx := GardenerStopContext{
				BeadID:        "gd-cap",
				ProgressState: buildProgressState(t, tc.scores...),
			}
			got := EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry)
			if got != tc.want {
				t.Fatalf("scores=%v: got %v, want %v", tc.scores, got, tc.want)
			}
		})
	}
}

// finalizeZeroRun appends a self-reported score-0 run spanning [start,end] to
// st via the real FinalizeRun path, so the event-aware rescue (bug
// ka-0000006914) runs exactly as it would in production.
func finalizeZeroRun(st *progress.State, agentID string, start, end int64) {
	progress.FinalizeRun(st, agentID, `{"progress_score":0}`, "done",
		time.Unix(start, 0), time.Unix(end, 0))
}

// TestGardenerStop_NoProgressFor_EventAware is the headline regression test for
// bug ka-0000006914: a gardener with N consecutive self-reported score-0 runs
// must NOT trip no_progress_for when a credited progress event (mr_merged /
// child_closed) landed inside one of those run windows. The barren-backstop and
// cancel_duplicates anti-gaming guard are exercised alongside it.
func TestGardenerStop_NoProgressFor_EventAware(t *testing.T) {
	entry := StopEntry{Kind: StopKindNoProgressFor, Runs: 3}

	t.Run("mr_merged_in_window_does_not_trip", func(t *testing.T) {
		// Three score-0 runs; an mr_merged event lands inside run #2's window
		// [200,300]. FinalizeRun lifts run #2 to score 1, so only 2 of the last
		// 3 runs are zero — below the window-of-3 threshold. Must NOT fire.
		var st progress.State
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindMRMerged, TargetID: "gd-MR1", OccurredAt: time.Unix(250, 0),
		})
		finalizeZeroRun(&st, "a", 100, 200)
		finalizeZeroRun(&st, "b", 200, 300)
		finalizeZeroRun(&st, "c", 300, 400)
		raw, err := st.Marshal()
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: raw}
		if EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
			t.Fatal("no_progress_for must NOT fire: an mr_merged event landed inside a run window")
		}
	})

	t.Run("child_closed_in_window_does_not_trip", func(t *testing.T) {
		var st progress.State
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindChildClosed, TargetID: "gd-C1", OccurredAt: time.Unix(150, 0),
		})
		finalizeZeroRun(&st, "a", 100, 200)
		finalizeZeroRun(&st, "b", 200, 300)
		finalizeZeroRun(&st, "c", 300, 400)
		raw, _ := st.Marshal()
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: raw}
		if EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
			t.Fatal("no_progress_for must NOT fire: a child_closed event landed inside a run window")
		}
	})

	t.Run("genuinely_barren_still_trips", func(t *testing.T) {
		// Three score-0 runs, no credited events anywhere — the wedged-gardener
		// backstop MUST still fire so a truly stuck gardener self-closes.
		var st progress.State
		finalizeZeroRun(&st, "a", 100, 200)
		finalizeZeroRun(&st, "b", 200, 300)
		finalizeZeroRun(&st, "c", 300, 400)
		raw, _ := st.Marshal()
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: raw}
		if !EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
			t.Fatal("no_progress_for MUST fire: three barren score-0 runs with no events")
		}
	})

	t.Run("cancel_duplicates_event_excluded_still_trips", func(t *testing.T) {
		// Anti-gaming guard (ka-0000006682): cancel_duplicates decisions never
		// enter state.Events (the evaluator filters them at discovery), so even
		// though a gardener re-raised a dismissed decision inside a run window,
		// no credited event exists to rescue the run — the barren backstop still
		// fires. We model "the evaluator filtered it" by recording NO
		// decision_resolved event for the cancelled decision; a single unrelated
		// out-of-window event is added to prove window scoping, not kind.
		var st progress.State
		// Out-of-window credited event (predates every run) — must not rescue.
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindDecisionResolved, TargetID: "gd-Dold", OccurredAt: time.Unix(10, 0),
		})
		finalizeZeroRun(&st, "a", 100, 200)
		finalizeZeroRun(&st, "b", 200, 300)
		finalizeZeroRun(&st, "c", 300, 400)
		raw, _ := st.Marshal()
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: raw}
		if !EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
			t.Fatal("no_progress_for MUST fire: cancel_duplicates decisions are excluded from state.Events, so no in-window event rescues these runs")
		}
	})
}

// TestGardenerStop_NoProgressFor_EmptyState confirms a gardener that has never
// finalized a run (empty ProgressState) cannot trip no_progress_for — a fresh
// gardener must not be declared wedged.
func TestGardenerStop_NoProgressFor_EmptyState(t *testing.T) {
	sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: ""}
	entry := StopEntry{Kind: StopKindNoProgressFor, Runs: 3}
	if EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
		t.Fatal("empty progress state should never fire no_progress_for")
	}
}

// buildTimedProgressState marshals a progress.State whose runs carry the given
// per-run scores (oldest-first), each stamped with a ClosedAt at `now - offset`
// where offset is drawn from offsetsAgo (same length as scores, in minutes).
// Lets the DURATION-based no_progress_for tests place runs inside/outside a
// time window deterministically relative to now.
func buildTimedProgressState(t *testing.T, now time.Time, scores []int, offsetsAgoMin []int) string {
	t.Helper()
	if len(scores) != len(offsetsAgoMin) {
		t.Fatalf("scores/offsets length mismatch: %d vs %d", len(scores), len(offsetsAgoMin))
	}
	var st progress.State
	for i, s := range scores {
		closed := now.Add(-time.Duration(offsetsAgoMin[i]) * time.Minute)
		st.AppendRun(progress.RunRecord{
			AgentID:       string(rune('a' + i)),
			StartedAt:     closed.Add(-5 * time.Minute),
			ClosedAt:      closed,
			ProgressScore: s,
		})
	}
	raw, err := st.Marshal()
	if err != nil {
		t.Fatalf("marshal timed progress state: %v", err)
	}
	return raw
}

// TestGardenerStop_NoProgressFor_Duration exercises the DURATION-based form
// (`no_progress_for: <minutes>` — ka-0000005141): fire when EVERY run that
// closed within the last <duration> scored zero, and never fire when the window
// is empty or contains a scored run.
func TestGardenerStop_NoProgressFor_Duration(t *testing.T) {
	now := time.Now()
	entry := StopEntry{Kind: StopKindNoProgressFor, Duration: "90m"} // Runs unset

	cases := []struct {
		name    string
		scores  []int
		offsets []int // minutes ago each run closed
		want    bool
	}{
		// Three barren runs all inside the 90m window → wedged.
		{"all_barren_in_window_fires", []int{0, 0, 0}, []int{80, 50, 10}, true},
		// A scored run inside the window defuses the wedge.
		{"scored_run_in_window_does_not_fire", []int{0, 1, 0}, []int{80, 50, 10}, false},
		// The only barren runs are OLDER than the window → not enough recent
		// evidence, must not fire.
		{"barren_runs_all_outside_window", []int{0, 0}, []int{200, 150}, false},
		// A barren run in-window plus an old scored run out-of-window still fires
		// (the out-of-window run is not counted).
		{"old_scored_ignored_recent_barren_fires", []int{1, 0, 0}, []int{300, 60, 20}, true},
		// No runs at all → not wedged.
		{"no_runs", nil, nil, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx := GardenerStopContext{
				BeadID:        "gd-cap",
				ProgressState: buildTimedProgressState(t, now, tc.scores, tc.offsets),
			}
			got := EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry)
			if got != tc.want {
				t.Fatalf("scores=%v offsets=%v: got %v, want %v", tc.scores, tc.offsets, got, tc.want)
			}
		})
	}
}

// TestGardenerStop_NoProgressFor_DispatchesButZeroMerges is the acceptance test
// for ka-0000005141 criterion #3: gardener-delivery must auto-stop after 3+
// consecutive cycles that DISPATCH crew but merge nothing. crew_dispatched is
// deliberately excluded from progress.CreditedProgressKinds, so a cycle whose
// only event is a crew_dispatched scores zero via FinalizeRun; three such cycles
// trip no_progress_for. The moment one cycle finally merges an MR, the wedge
// clears. Exercises both the count-based (`runs:3`) and duration-based forms
// against the SAME state so the two stay in agreement.
func TestGardenerStop_NoProgressFor_DispatchesButZeroMerges(t *testing.T) {
	// Three cycles: each dispatches crew (crew_dispatched event in-window) but
	// merges nothing. FinalizeRun must leave every run at score 0.
	barren := func() *progress.State {
		st := &progress.State{}
		// crew_dispatched events, one per cycle, landing inside each run window.
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindCrewDispatched, TargetID: "gd-crew1", OccurredAt: time.Unix(150, 0),
		})
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindCrewDispatched, TargetID: "gd-crew2", OccurredAt: time.Unix(250, 0),
		})
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindCrewDispatched, TargetID: "gd-crew3", OccurredAt: time.Unix(350, 0),
		})
		finalizeZeroRun(st, "a", 100, 200)
		finalizeZeroRun(st, "b", 200, 300)
		finalizeZeroRun(st, "c", 300, 400)
		return st
	}

	t.Run("runs_form_fires_after_3_dispatch_only_cycles", func(t *testing.T) {
		raw, _ := barren().Marshal()
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: raw}
		entry := StopEntry{Kind: StopKindNoProgressFor, Runs: 3}
		if !EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
			t.Fatal("no_progress_for(runs:3) MUST fire: three cycles dispatched crew but merged nothing (crew_dispatched is not credited progress)")
		}
	})

	t.Run("a_merge_in_any_cycle_clears_the_wedge", func(t *testing.T) {
		// Same three-cycle shape as barren(), but cycle #2 finally merges an MR
		// inside its window [200,300]. FinalizeRun rescues run #2 to a non-zero
		// score, so only 2 of the last 3 runs stay barren — below the runs:3
		// threshold, so the wedge must NOT fire.
		st := &progress.State{}
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindCrewDispatched, TargetID: "gd-crew1", OccurredAt: time.Unix(150, 0),
		})
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindMRMerged, TargetID: "gd-MRwin", OccurredAt: time.Unix(250, 0),
		})
		st.AppendEvent(progress.EventRecord{
			Kind: progress.EventKindCrewDispatched, TargetID: "gd-crew3", OccurredAt: time.Unix(350, 0),
		})
		finalizeZeroRun(st, "a", 100, 200)
		finalizeZeroRun(st, "b", 200, 300) // rescued to score 1 by the in-window mr_merged
		finalizeZeroRun(st, "c", 300, 400)
		raw, _ := st.Marshal()
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: raw}
		entry := StopEntry{Kind: StopKindNoProgressFor, Runs: 3}
		if EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
			t.Fatal("no_progress_for(runs:3) must NOT fire: cycle #2 merged an MR, so only 2 of 3 runs are barren")
		}
	})

	t.Run("duration_form_fires_on_dispatch_only_window", func(t *testing.T) {
		// Same shape, but timestamped relative to now so the duration scan sees
		// three barren runs inside a 2h window.
		now := time.Now()
		raw := buildTimedProgressState(t, now, []int{0, 0, 0}, []int{90, 60, 20})
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: raw}
		entry := StopEntry{Kind: StopKindNoProgressFor, Duration: "2h"}
		if !EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
			t.Fatal("no_progress_for(duration:2h) MUST fire: every run in the 2h window was a dispatch-only, zero-merge cycle")
		}
	})
}

// buildGoalState marshals a progress.State whose runs carry the given
// goal_satisfied values (oldest-first) so the goal_satisfied stop kind can be
// exercised. Scores are all 0 — goal_satisfied is independent of per-run
// progress score.
func buildGoalState(t *testing.T, goals ...string) string {
	t.Helper()
	var st progress.State
	for i, g := range goals {
		st.AppendRun(progress.RunRecord{
			AgentID:       string(rune('a' + i)),
			GoalSatisfied: g,
		})
	}
	raw, err := st.Marshal()
	if err != nil {
		t.Fatalf("marshal progress state: %v", err)
	}
	return raw
}

// TestGardenerStop_GoalSatisfied exercises the master-gate stop kind: it fires
// only when the freshest finalized run self-declared goal_satisfied=="yes"
// (bead ka-0000005353).
func TestGardenerStop_GoalSatisfied(t *testing.T) {
	entry := StopEntry{Kind: StopKindGoalSatisfied}

	cases := []struct {
		name  string
		goals []string
		want  bool
	}{
		{"latest_yes_fires", []string{"no", "yes"}, true},
		{"latest_yes_caps_case", []string{"no", "YES"}, true},
		{"latest_no_does_not_fire", []string{"yes", "no"}, false},
		{"latest_empty_does_not_fire", []string{"yes", ""}, false},
		{"no_runs_does_not_fire", nil, false},
		{"single_yes_fires", []string{"yes"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx := GardenerStopContext{
				BeadID:        "gd-cap",
				ProgressState: buildGoalState(t, tc.goals...),
			}
			got := EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry)
			if got != tc.want {
				t.Fatalf("goals=%v: got %v, want %v", tc.goals, got, tc.want)
			}
		})
	}
}

// TestStopKindIsTerminal locks the terminal/premature classification that
// drives the resume-or-escalate fork (bead ka-0000005353).
func TestStopKindIsTerminal(t *testing.T) {
	premature := []string{
		StopKindMaxRuns, StopKindWallClock, StopKindNoProgressFor, StopKindConsecutiveFailures,
		// mr_merged is a park-not-close marker (ka-0000007387): classified
		// premature so that if it ever reached the generic terminate/resume fork
		// (it should not — the dedicated gardenerBlockOnMR gate handles it first
		// and EvaluateGardenerStopEntry returns not-met) it re-arms rather than
		// silently CLOSING a gardener. This locks invariant 4.
		StopKindMRMerged,
	}
	terminal := []string{
		StopKindGoalSatisfied, StopKindManual, StopKindManualForce,
		StopKindSpecificBeadClosed, StopKindGatePresent, "some_unknown_kind",
	}
	for _, k := range premature {
		if stopKindIsTerminal(k) {
			t.Errorf("%q should be premature, classified terminal", k)
		}
	}
	for _, k := range terminal {
		if !stopKindIsTerminal(k) {
			t.Errorf("%q should be terminal, classified premature", k)
		}
	}
}

// TestEvaluateGardenerStopEntry_MRMerged_AlwaysNotMet locks invariant 4
// (ka-0000007387): the generic stop evaluator must ALWAYS report the mr_merged
// kind as not-met, so it can never route through the ordinary loop_end
// close/resume path. The dedicated gardenerBlockOnMR pre-fire gate — not this
// evaluator — owns the MR-merge park decision. Verified regardless of the loop
// slot the entry lands in.
func TestEvaluateGardenerStopEntry_MRMerged_AlwaysNotMet(t *testing.T) {
	sctx := GardenerStopContext{BeadID: "gd-cap", Project: "monorepo"}
	entry := StopEntry{Kind: StopKindMRMerged, Parent: "gd-epic-1"}
	if EvaluateGardenerStopEntry(context.Background(), nil, nil, sctx, entry) {
		t.Fatalf("mr_merged must always evaluate not-met in the generic stop evaluator (invariant 4)")
	}
	// And it must not fire through the full spec evaluator either.
	spec, err := ParseStopSpec(`{"loop_end":{"any":[{"kind":"mr_merged","parent":"gd-epic-1"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, fired := EvaluateGardenerStopSpec(context.Background(), nil, nil, sctx, spec); fired {
		t.Fatalf("a loop_end containing only mr_merged must not fire the generic stop spec (invariant 4)")
	}
}

// TestEvaluateGardenerStopSpec_PrematureFlag verifies the StopMatch.Premature
// flag is set correctly per slot+kind: premature only for premature loop_end
// kinds; never for terminal loop_end or hard_stop.
func TestEvaluateGardenerStopSpec_PrematureFlag(t *testing.T) {
	t.Run("premature_loop_end_max_runs", func(t *testing.T) {
		spec, err := ParseStopSpec(`{"loop_end":{"any":[{"kind":"max_runs"}]}}`)
		if err != nil {
			t.Fatal(err)
		}
		sctx := GardenerStopContext{BeadID: "gd-cap", RunBudget: 1, RunsSpawned: 1}
		m, fired := EvaluateGardenerStopSpec(context.Background(), nil, nil, sctx, spec)
		if !fired || m.Slot != StopSlotLoopEnd || !m.Premature {
			t.Fatalf("expected fired loop_end premature match, got fired=%v match=%+v", fired, m)
		}
	})
	t.Run("terminal_loop_end_goal_satisfied", func(t *testing.T) {
		spec, err := ParseStopSpec(`{"loop_end":{"any":[{"kind":"goal_satisfied"}]}}`)
		if err != nil {
			t.Fatal(err)
		}
		sctx := GardenerStopContext{BeadID: "gd-cap", ProgressState: buildGoalState(t, "yes")}
		m, fired := EvaluateGardenerStopSpec(context.Background(), nil, nil, sctx, spec)
		if !fired || m.Slot != StopSlotLoopEnd || m.Premature {
			t.Fatalf("expected fired loop_end terminal (non-premature) match, got fired=%v match=%+v", fired, m)
		}
	})
}

// TestEvaluateGardenerStopEntry_MaxRuns_StandingExempt is the scheduler-level
// regression for bug ka-0000004754. A STANDING (perpetual) gardener must NOT
// trip the max_runs kind even when RunsSpawned >= RunBudget: a run-count
// ceiling can never legitimately terminate a gardener with no terminal epic, so
// tripping it only churns an identical re-arm comment on every lap (33+ observed
// on a prod lane). A non-standing gardener at the same ceiling still fires — the
// exemption is scoped strictly to the standing flag.
func TestEvaluateGardenerStopEntry_MaxRuns_StandingExempt(t *testing.T) {
	entry := StopEntry{Kind: StopKindMaxRuns}

	// Non-standing gardener at the ceiling: max_runs fires (unchanged behavior).
	nonStanding := GardenerStopContext{BeadID: "gd-cap", RunBudget: 5, RunsSpawned: 5}
	if !EvaluateGardenerStopEntry(context.Background(), nil, nil, nonStanding, entry) {
		t.Fatalf("non-standing gardener at run-count ceiling must trip max_runs")
	}

	// Standing gardener at (and past) the ceiling: max_runs is a no-op.
	standing := GardenerStopContext{BeadID: "gd-cap", RunBudget: 5, RunsSpawned: 500, IsStanding: true}
	if EvaluateGardenerStopEntry(context.Background(), nil, nil, standing, entry) {
		t.Fatalf("standing gardener must be EXEMPT from the max_runs kind (ka-0000004754); got fired")
	}

	// And it must not fire through the full spec evaluator either — the live
	// standing gardeners declare exactly loop_end:{max_runs}.
	spec, err := ParseStopSpec(`{"loop_end":{"any":[{"kind":"max_runs"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, fired := EvaluateGardenerStopSpec(context.Background(), nil, nil, standing, spec); fired {
		t.Fatalf("a loop_end of only max_runs must not fire for a standing gardener (ka-0000004754)")
	}
	// The hard_stop brakes remain live for a standing gardener: consecutive_failures
	// over threshold still fires terminally.
	brakeSpec, err := ParseStopSpec(`{"hard_stop":{"any":[{"kind":"consecutive_failures","value":"3"}]},"loop_end":{"any":[{"kind":"max_runs"}]}}`)
	if err != nil {
		t.Fatal(err)
	}
	standingBroken := GardenerStopContext{BeadID: "gd-cap", RunBudget: 5, RunsSpawned: 500, ConsecutiveFailures: 3, IsStanding: true}
	m, fired := EvaluateGardenerStopSpec(context.Background(), nil, nil, standingBroken, brakeSpec)
	if !fired || m.Slot != StopSlotHardStop {
		t.Fatalf("standing gardener must still trip the consecutive_failures hard_stop, got fired=%v match=%+v", fired, m)
	}
}
