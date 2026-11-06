package scheduler

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// fakeStopQueryDaemon implements GardenerStopDaemon for the never-started
// guard tests. ListBeadsFiltered returns a fixed Total so a bead_query
// loop_end with op "==" value "0" can be made to match (Total==0) or not
// (Total>0).
type fakeStopQueryDaemon struct {
	total int
	// parentStatus, when non-empty, is the status GetBead returns for the
	// loop_end bead_query's parent — used to exercise the closed-parent
	// override of the never-started guard.
	parentStatus string
}

func (f *fakeStopQueryDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Total: f.total}, nil
}

func (f *fakeStopQueryDaemon) GetBead(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	if f.parentStatus == "" {
		return nil, nil
	}
	return &beadsapi.BeadDetail{ID: "gd-epic", Status: f.parentStatus}, nil
}

func (f *fakeStopQueryDaemon) IsAncestor(_ context.Context, _, _ string) (bool, error) {
	return false, nil
}

// beadQueryGoalSpec builds a StopSpec whose loop_end is a single bead_query
// goal-completion check: "open|in_progress|blocked children of <parent> == 0".
func beadQueryGoalSpec(parent string) StopSpec {
	return StopSpec{
		LoopEnd: StopSlot{Any: []StopEntry{{
			Kind:     StopKindBeadQuery,
			Op:       "==",
			Value:    "0",
			Parent:   parent,
			Types:    []string{"task"},
			Statuses: []string{"open", "in_progress", "blocked"},
		}}},
	}
}

// TestGardenerStop_BeadQueryNeverStartedGuard is the regression lock for
// ka-0000005388: a bead_query goal-completion loop_end must NOT close the
// gardener when it matches at RunsSpawned==0. A zero-child count at 0 runs is
// indistinguishable from "the work graph was never linked" vs "instantly
// done", so the gardener stays open (the cron preflight's children>0 arm-time
// bar is mirrored onto goal evaluation).
func TestGardenerStop_BeadQueryNeverStartedGuard(t *testing.T) {
	t.Run("zero_runs_zero_children_suppressed", func(t *testing.T) {
		// Query matches (Total==0 satisfies "==0") but the gardener has
		// never fired — the never-started guard must suppress the close.
		d := &fakeStopQueryDaemon{total: 0}
		sctx := GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 0}
		match, fired := EvaluateGardenerStopSpec(context.Background(), d, nil, sctx, beadQueryGoalSpec("gd-epic"))
		if fired {
			t.Fatalf("bead_query at 0 runs must not close gardener, got match=%+v", match)
		}
	})

	t.Run("after_one_run_goal_met_closes", func(t *testing.T) {
		// Same zero-child query, but the gardener has fired at least once —
		// now a zero count is real evidence the goal is complete.
		d := &fakeStopQueryDaemon{total: 0}
		sctx := GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 1}
		match, fired := EvaluateGardenerStopSpec(context.Background(), d, nil, sctx, beadQueryGoalSpec("gd-epic"))
		if !fired {
			t.Fatal("bead_query goal-met after a run should close the gardener")
		}
		if match.Slot != StopSlotLoopEnd || match.Kind != StopKindBeadQuery {
			t.Fatalf("unexpected match: %+v", match)
		}
	})

	t.Run("zero_runs_open_children_does_not_match", func(t *testing.T) {
		// Children still open (Total>0) — the query itself does not match,
		// so the gardener stays open regardless of the guard.
		d := &fakeStopQueryDaemon{total: 3}
		sctx := GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 0}
		if _, fired := EvaluateGardenerStopSpec(context.Background(), d, nil, sctx, beadQueryGoalSpec("gd-epic")); fired {
			t.Fatal("bead_query with open children must not close the gardener")
		}
	})

	t.Run("zero_runs_closed_parent_overrides_guard_and_closes", func(t *testing.T) {
		// Matt's ask (thread ka-0000001290): a plan epic CLOSED before its
		// gardener ever fired must retire the gardener, not strand it open+
		// enabled. A closed parent resolves the guard's "never linked vs
		// instantly done" ambiguity — closed is unambiguous done — so the
		// guard is overridden even at RunsSpawned==0.
		d := &fakeStopQueryDaemon{total: 0, parentStatus: "closed"}
		sctx := GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 0}
		match, fired := EvaluateGardenerStopSpec(context.Background(), d, nil, sctx, beadQueryGoalSpec("gd-epic"))
		if !fired {
			t.Fatal("bead_query at 0 runs with a CLOSED parent must close the gardener")
		}
		if match.Slot != StopSlotLoopEnd || match.Kind != StopKindBeadQuery {
			t.Fatalf("unexpected match: %+v", match)
		}
	})

	t.Run("zero_runs_open_parent_still_suppressed", func(t *testing.T) {
		// An OPEN parent leaves the ambiguity intact — the guard still
		// suppresses the close at 0 runs (the closed-parent override is the
		// ONLY thing that lifts it).
		d := &fakeStopQueryDaemon{total: 0, parentStatus: "open"}
		sctx := GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 0}
		if _, fired := EvaluateGardenerStopSpec(context.Background(), d, nil, sctx, beadQueryGoalSpec("gd-epic")); fired {
			t.Fatal("bead_query at 0 runs with an OPEN parent must stay suppressed by the never-started guard")
		}
	})
}

// TestGardenerStop_NeverStartedGuardScopedToBeadQuery confirms the guard does
// not bleed into other loop_end kinds: a max_runs loop_end is a legitimate
// reason to close and is unaffected by RunsSpawned==0 (max_runs at 0 only
// fires with a zero budget, which is its own degenerate case, but the guard
// must not swallow a genuine terminal kind; ka-0000005388).
func TestGardenerStop_NeverStartedGuardScopedToBeadQuery(t *testing.T) {
	// wall_clock loop_end with an already-elapsed window should still close
	// even at 0 runs — the guard is bead_query-only.
	spec := StopSpec{LoopEnd: StopSlot{Any: []StopEntry{
		{Kind: StopKindConsecutiveFailures, Value: "1"},
	}}}
	sctx := GardenerStopContext{BeadID: "gd-cap", RunsSpawned: 0, ConsecutiveFailures: 2}
	match, fired := EvaluateGardenerStopSpec(context.Background(), &fakeStopQueryDaemon{}, nil, sctx, spec)
	if !fired {
		t.Fatal("consecutive_failures loop_end must still fire at 0 runs (guard is bead_query-only)")
	}
	if match.Kind != StopKindConsecutiveFailures {
		t.Fatalf("unexpected match kind: %+v", match)
	}
}
