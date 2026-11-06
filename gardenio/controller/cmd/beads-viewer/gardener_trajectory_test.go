package main

import (
	"encoding/json"
	"testing"
	"time"

	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/progress"
)

// mkProgressState marshals a progress.State into the raw JSON the gardener spec
// carries in its progress_state field.
func mkProgressState(t *testing.T, st progress.State) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal progress state: %v", err)
	}
	return raw
}

func TestBuildGardenerTrajectory_NilAndEmpty(t *testing.T) {
	if got := buildGardenerTrajectory(nil); got.HasData {
		t.Errorf("nil spec: HasData = true, want false")
	}
	// A spec with no progress + no counters has nothing to narrate.
	if got := buildGardenerTrajectory(&bridge.GardenerSpec{}); got.HasData {
		t.Errorf("empty spec: HasData = true, want false")
	}
}

func TestBuildGardenerTrajectory_MakingProgress(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "a1", ProgressScore: 2, AgentState: "done"},
			{AgentID: "a2", ProgressScore: 0, AgentState: "done"},
			{AgentID: "a3", ProgressScore: 5, AgentState: "done"},
		},
		Events: []progress.EventRecord{
			{Kind: progress.EventKindMRMerged, TargetID: "gd-mr1", OccurredAt: now},
			{Kind: progress.EventKindMRMerged, TargetID: "gd-mr2", OccurredAt: now},
			{Kind: progress.EventKindChildClosed, TargetID: "gd-c1", OccurredAt: now},
		},
	}
	spec := &bridge.GardenerSpec{
		RunsSpawned:   3,
		RunsCompleted: 3,
		ProgressState: mkProgressState(t, st),
	}

	tr := buildGardenerTrajectory(spec)
	if !tr.HasData {
		t.Fatalf("HasData = false, want true")
	}
	if tr.VerdictKind != "progress" || tr.Verdict != "making progress" {
		t.Errorf("verdict = %q/%q, want making progress/progress", tr.Verdict, tr.VerdictKind)
	}
	// All three runs closed `done`, so all three are outcomes (a clean
	// completion is an outcome — ka-0000002776); two of them also scored.
	if tr.TotalRuns != 3 || tr.OutcomeRuns != 3 {
		t.Errorf("runs = %d total / %d outcome, want 3/3", tr.TotalRuns, tr.OutcomeRuns)
	}
	if tr.ScoredRuns != 2 {
		t.Errorf("ScoredRuns = %d, want 2", tr.ScoredRuns)
	}
	if tr.OutcomeRate != 100 {
		t.Errorf("OutcomeRate = %d, want 100", tr.OutcomeRate)
	}
	if tr.MRsMerged != 2 || tr.BeadsClosed != 1 {
		t.Errorf("events = %d MR / %d bead, want 2/1", tr.MRsMerged, tr.BeadsClosed)
	}
	if len(tr.Runs) != 3 {
		t.Fatalf("spark runs = %d, want 3", len(tr.Runs))
	}
	// Tallest bar is the score-5 run; the score-0 run renders as the flat stub.
	if tr.Runs[2].Outcome != "ok" || tr.Runs[2].Height != 100 {
		t.Errorf("run3 glyph = %+v, want ok/100", tr.Runs[2])
	}
	if tr.Runs[1].Outcome != "flat" || tr.Runs[1].Height != 4 {
		t.Errorf("run2 glyph = %+v, want flat/4", tr.Runs[1])
	}
}

func TestBuildGardenerTrajectory_Stalled(t *testing.T) {
	// Enough runs to judge, every one zero-score AND none completed `done` (an
	// unknown/never-completing terminal state), no outcome events. This is a
	// genuine stall — the gardener is spinning without producing outcomes.
	// Contrast with all-`done` runs, which now count as outcomes (ka-0000002776).
	st := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "a1", ProgressScore: 0, AgentState: ""},
			{AgentID: "a2", ProgressScore: 0, AgentState: ""},
			{AgentID: "a3", ProgressScore: 0, AgentState: ""},
			{AgentID: "a4", ProgressScore: 0, AgentState: ""},
		},
	}
	spec := &bridge.GardenerSpec{RunsCompleted: 4, ProgressState: mkProgressState(t, st)}

	tr := buildGardenerTrajectory(spec)
	if tr.VerdictKind != "stalled" {
		t.Errorf("verdict kind = %q, want stalled", tr.VerdictKind)
	}
	if tr.OutcomeRuns != 0 {
		t.Errorf("OutcomeRuns = %d, want 0", tr.OutcomeRuns)
	}
}

func TestBuildGardenerTrajectory_AllDoneRunsAreProgress(t *testing.T) {
	// The ka-0000002776 regression: a gardener whose every run closed `done`
	// with zero ProgressScore and no credited events (scorecard/keep-fresh/
	// done-via-gb-stop gardeners) must read as making progress at 100% outcome
	// rate — NOT "stalled · 0 outcomes".
	st := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "a1", ProgressScore: 0, AgentState: "done"},
			{AgentID: "a2", ProgressScore: 0, AgentState: "done"},
			{AgentID: "a3", ProgressScore: 0, AgentState: "done"},
			{AgentID: "a4", ProgressScore: 0, AgentState: "done"},
			{AgentID: "a5", ProgressScore: 0, AgentState: "done"},
		},
	}
	spec := &bridge.GardenerSpec{RunsCompleted: 5, ProgressState: mkProgressState(t, st)}

	tr := buildGardenerTrajectory(spec)
	if tr.VerdictKind != "progress" || tr.Verdict != "making progress" {
		t.Errorf("verdict = %q/%q, want making progress/progress", tr.Verdict, tr.VerdictKind)
	}
	if tr.OutcomeRuns != 5 || tr.OutcomeRate != 100 {
		t.Errorf("outcomes = %d/%d%%, want 5/100", tr.OutcomeRuns, tr.OutcomeRate)
	}
	if tr.ScoredRuns != 0 {
		t.Errorf("ScoredRuns = %d, want 0 (none scored, all clean)", tr.ScoredRuns)
	}
}

func TestBuildGardenerTrajectory_FailedRunsNotOutcomes(t *testing.T) {
	// A `failed` run never counts as an outcome even at zero score. Enough
	// failed/blank runs with no scored runs and no events ⇒ stalled.
	st := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "a1", ProgressScore: 0, AgentState: "failed"},
			{AgentID: "a2", ProgressScore: 0, AgentState: "failed"},
			{AgentID: "a3", ProgressScore: 0, AgentState: "failed"},
		},
	}
	spec := &bridge.GardenerSpec{RunsCompleted: 3, ProgressState: mkProgressState(t, st)}

	tr := buildGardenerTrajectory(spec)
	if tr.OutcomeRuns != 0 {
		t.Errorf("OutcomeRuns = %d, want 0 (failed runs are not outcomes)", tr.OutcomeRuns)
	}
	if tr.VerdictKind != "stalled" {
		t.Errorf("verdict kind = %q, want stalled", tr.VerdictKind)
	}
}

func TestBuildGardenerTrajectory_Stuck(t *testing.T) {
	// Consecutive failures dominate even when there are some past outcomes.
	st := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "a1", ProgressScore: 3, AgentState: "done"},
			{AgentID: "a2", ProgressScore: 0, AgentState: "failed"},
		},
	}
	spec := &bridge.GardenerSpec{
		ConsecutiveFailures: 4,
		RunsCompleted:       2,
		ProgressState:       mkProgressState(t, st),
	}

	tr := buildGardenerTrajectory(spec)
	if tr.VerdictKind != "stuck" || tr.Verdict != "stuck" {
		t.Errorf("verdict = %q/%q, want stuck/stuck", tr.Verdict, tr.VerdictKind)
	}
	if tr.ConsecutiveFailures != 4 {
		t.Errorf("ConsecutiveFailures = %d, want 4", tr.ConsecutiveFailures)
	}
	// The failed run renders as a fail glyph.
	if tr.Runs[1].Outcome != "fail" {
		t.Errorf("run2 outcome = %q, want fail", tr.Runs[1].Outcome)
	}
}

func TestBuildGardenerTrajectory_StartingUp(t *testing.T) {
	// One run that neither scored nor completed `done` (blank terminal state) —
	// no outcome yet, and too few runs to call stalled.
	st := progress.State{
		Runs: []progress.RunRecord{{AgentID: "a1", ProgressScore: 0, AgentState: ""}},
	}
	spec := &bridge.GardenerSpec{RunsCompleted: 1, ProgressState: mkProgressState(t, st)}

	tr := buildGardenerTrajectory(spec)
	if tr.VerdictKind != "neutral" || tr.Verdict != "starting up" {
		t.Errorf("verdict = %q/%q, want starting up/neutral", tr.Verdict, tr.VerdictKind)
	}
}

func TestBuildGardenerTrajectory_LegacyCountersFallback(t *testing.T) {
	// No ProgressState at all — older gardeners. The spec counters alone still
	// drive HasData + a verdict.
	spec := &bridge.GardenerSpec{RunsSpawned: 5, RunsCompleted: 5}

	tr := buildGardenerTrajectory(spec)
	if !tr.HasData {
		t.Fatalf("HasData = false, want true (legacy counters)")
	}
	if tr.TotalRuns != 0 {
		t.Errorf("TotalRuns = %d, want 0 (no progress_state)", tr.TotalRuns)
	}
	// 5 completed runs, no recorded outcomes → stalled.
	if tr.VerdictKind != "stalled" {
		t.Errorf("verdict kind = %q, want stalled", tr.VerdictKind)
	}
	if len(tr.Runs) != 0 {
		t.Errorf("spark runs = %d, want 0 (no per-run records)", len(tr.Runs))
	}
}

// mkRunLog marshals a scheduler.RunLog into the raw JSON the gardener spec
// carries in its run_log field.
func mkRunLog(t *testing.T, rl scheduler.RunLog) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(rl)
	if err != nil {
		t.Fatalf("marshal run log: %v", err)
	}
	return raw
}

func TestBuildGardenerTrajectory_CleanRunLogIsProgress(t *testing.T) {
	// Legacy gardener: no ProgressState, but a run_log full of clean `done`
	// outcomes. It must read as making progress, not stalled (ka-0000003373).
	spec := &bridge.GardenerSpec{
		RunsSpawned:   137,
		RunsCompleted: 137,
		RunLog: mkRunLog(t, scheduler.RunLog{
			{Agent: "a1", Outcome: "done"},
			{Agent: "a2", Outcome: "done"},
			{Agent: "a3", Outcome: "done"},
		}),
	}

	tr := buildGardenerTrajectory(spec)
	if tr.VerdictKind != "progress" || tr.Verdict != "making progress" {
		t.Errorf("verdict = %q/%q, want making progress/progress", tr.Verdict, tr.VerdictKind)
	}
}

func TestBuildGardenerTrajectory_FailedRunLogStillStalled(t *testing.T) {
	// A run_log with a non-done outcome is NOT a clean signal — the gardener
	// falls through to the stalled verdict (zero outcomes, enough runs).
	spec := &bridge.GardenerSpec{
		RunsCompleted: 5,
		RunLog: mkRunLog(t, scheduler.RunLog{
			{Agent: "a1", Outcome: "done"},
			{Agent: "a2", Outcome: "failed"},
			{Agent: "a3", Outcome: "done"},
		}),
	}

	tr := buildGardenerTrajectory(spec)
	if tr.VerdictKind != "stalled" {
		t.Errorf("verdict kind = %q, want stalled (run_log not clean)", tr.VerdictKind)
	}
}

func TestBuildGardenerTrajectory_ProgressStateWinsOverRunLog(t *testing.T) {
	// When a ProgressState IS present, it drives the verdict — the run_log
	// fallback is not consulted, so a zero-outcome ProgressState (no scored
	// runs, none completed `done`) still reads stalled even if the run_log
	// looks clean.
	st := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "a1", ProgressScore: 0, AgentState: ""},
			{AgentID: "a2", ProgressScore: 0, AgentState: ""},
			{AgentID: "a3", ProgressScore: 0, AgentState: ""},
		},
	}
	spec := &bridge.GardenerSpec{
		RunsCompleted: 3,
		ProgressState: mkProgressState(t, st),
		RunLog: mkRunLog(t, scheduler.RunLog{
			{Agent: "a1", Outcome: "done"},
			{Agent: "a2", Outcome: "done"},
			{Agent: "a3", Outcome: "done"},
		}),
	}

	tr := buildGardenerTrajectory(spec)
	if tr.VerdictKind != "stalled" {
		t.Errorf("verdict kind = %q, want stalled (ProgressState present, run_log ignored)", tr.VerdictKind)
	}
}

func TestBuildGardenerTrajectory_MalformedProgressDegrades(t *testing.T) {
	// Garbage progress_state must not panic or error — it degrades to the spec
	// counters.
	spec := &bridge.GardenerSpec{
		RunsCompleted: 2,
		ProgressState: json.RawMessage(`{not valid json`),
	}
	tr := buildGardenerTrajectory(spec)
	if tr.TotalRuns != 0 {
		t.Errorf("TotalRuns = %d, want 0 on malformed input", tr.TotalRuns)
	}
	if !tr.HasData {
		t.Errorf("HasData = false; spec counters should still narrate")
	}
}

func TestBuildGardenerTrajectory_SparkTruncation(t *testing.T) {
	// More runs than the spark cap: keep the most-recent window, count the rest.
	runs := make([]progress.RunRecord, gardenerTrajectorySparkRuns+5)
	for i := range runs {
		runs[i] = progress.RunRecord{ProgressScore: 1, AgentState: "done"}
	}
	spec := &bridge.GardenerSpec{
		RunsCompleted: len(runs),
		ProgressState: mkProgressState(t, progress.State{Runs: runs}),
	}

	tr := buildGardenerTrajectory(spec)
	if len(tr.Runs) != gardenerTrajectorySparkRuns {
		t.Errorf("spark len = %d, want %d", len(tr.Runs), gardenerTrajectorySparkRuns)
	}
	if tr.SparkOmitted != 5 {
		t.Errorf("SparkOmitted = %d, want 5", tr.SparkOmitted)
	}
	// Run numbers stay absolute across the truncation: first shown bar is #6.
	if tr.Runs[0].RunNumber != 6 {
		t.Errorf("first shown run number = %d, want 6", tr.Runs[0].RunNumber)
	}
	if tr.TotalRuns != gardenerTrajectorySparkRuns+5 {
		t.Errorf("TotalRuns = %d, want %d", tr.TotalRuns, gardenerTrajectorySparkRuns+5)
	}
}
