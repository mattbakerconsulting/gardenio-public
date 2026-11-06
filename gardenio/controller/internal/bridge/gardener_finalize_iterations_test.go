package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/progress"
)

// gardenerFinalizeTrigger builds a subtree-mode (assign_bead) gardener trigger
// with NO source matches, so evalOne reaches the run-finalization reconcile
// hooks but does not fire (no spawn, no post-fire bookkeeping muddies the
// assertions). The iteration agent beads under test are appended to
// daemon.beads by the caller. The gardener carries a Progress spec (required for
// run finalization) and intentionally leaves LastAgentID empty — the exact
// shape of a concurrent subtree gardener whose lone pointer was overwritten
// (bug ka-0000006915).
func gardenerFinalizeTrigger(t *testing.T, cap GardenerSpec) (*mockTriggerDaemon, *TriggerEngine) {
	t.Helper()
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-f", Type: "trigger", Status: "open", Title: "subtree gardener"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-f",
		Title:         "subtree gardener",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 3, // concurrent: the failure mode this fix targets
		LastChecked:   pastLastChecked(),
		Gardener:      &cap,
	}
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{},
	}
	engine.RegisterSource(src)
	return daemon, engine
}

// closedIterationBead is a small helper building a closed iteration agent bead
// owned by gardener gd-cap-f via trigger_id (the modern bridge spawn path).
func closedIterationBead(id, agentState, wrapup string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"trigger_id":  "gd-cap-f",
			"agent_state": agentState,
			"wrapup":      wrapup,
		},
	}
}

// TestGardener_FinalizeFromIterations_ConcurrentSubtree is the core regression
// guard for bug ka-0000006915: a subtree-mode gardener with an EMPTY LastAgentID
// (its lone pointer was overwritten by concurrent fires) still records a
// per-run RunRecord for every closed iteration bead it owns. Before this fix,
// gardenerAgentTracker never ran (gated on LastAgentID != "") and
// ProgressState.Runs stayed empty — the gap that left /maps run glyphs blank.
func TestGardener_FinalizeFromIterations_ConcurrentSubtree(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
		Progress:   json.RawMessage(progressSpecWithSurface),
		// LastAgentID intentionally empty.
	})
	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-1", "done", `{"accomplishments":"shipped A","progress_score":2}`),
		closedIterationBead("gd-iter-2", "done", `{"accomplishments":"nothing moved"}`), // score 0
		closedIterationBead("gd-iter-3", "failed", `{"accomplishments":"crashed"}`),     // score 0
	)

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 3 {
		t.Fatalf("ProgressState.Runs: got %d, want 3 (one per closed iteration)", len(st.Runs))
	}
	// Oldest-first ordering: gd-iter-1 created first must be Runs[0].
	wantIDs := []string{"gd-iter-1", "gd-iter-2", "gd-iter-3"}
	for i, want := range wantIDs {
		if st.Runs[i].AgentID != want {
			t.Errorf("Runs[%d].AgentID: got %q, want %q", i, st.Runs[i].AgentID, want)
		}
	}
	if st.Runs[0].ProgressScore != 2 {
		t.Errorf("Runs[0].ProgressScore: got %d, want 2 (self-reported)", st.Runs[0].ProgressScore)
	}
	if st.Runs[1].ProgressScore != 0 || st.Runs[2].ProgressScore != 0 {
		t.Errorf("barren runs should score 0, got %d and %d", st.Runs[1].ProgressScore, st.Runs[2].ProgressScore)
	}
}

// TestGardener_FinalizeFromIterations_Idempotent verifies the finalizer credits
// each iteration at most once: an iteration already present in
// ProgressState.Runs (e.g. recorded earlier by gardenerAgentTracker's fast path,
// or by a prior cycle of this reconciler) is not double-counted.
func TestGardener_FinalizeFromIterations_Idempotent(t *testing.T) {
	// Seed ProgressState with gd-iter-1 already recorded.
	seed := progress.State{}
	seed.AppendRun(progress.RunRecord{AgentID: "gd-iter-1", ProgressScore: 1})
	seedJSON, err := seed.Marshal()
	if err != nil {
		t.Fatalf("seed marshal: %v", err)
	}

	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead:    "gd-epic-1",
		Progress:      json.RawMessage(progressSpecWithSurface),
		ProgressState: json.RawMessage(seedJSON),
	})
	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-1", "done", `{"progress_score":1}`), // already recorded
		closedIterationBead("gd-iter-2", "done", `{"progress_score":1}`), // new
	)

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 2 {
		t.Fatalf("ProgressState.Runs: got %d, want 2 (gd-iter-1 not double-counted)", len(st.Runs))
	}
	seenIter1 := 0
	for _, r := range st.Runs {
		if r.AgentID == "gd-iter-1" {
			seenIter1++
		}
	}
	if seenIter1 != 1 {
		t.Errorf("gd-iter-1 recorded %d times, want exactly 1", seenIter1)
	}
}

// TestGardener_FinalizeFromIterations_SkipsOpenIterations confirms only CLOSED
// iterations are finalized — a still-running agent must not record a (premature,
// score-0) RunRecord that would falsely advance no_progress_for.
func TestGardener_FinalizeFromIterations_SkipsOpenIterations(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
		Progress:   json.RawMessage(progressSpecWithSurface),
	})
	closed := closedIterationBead("gd-iter-closed", "done", `{"progress_score":1}`)
	running := closedIterationBead("gd-iter-running", "", "")
	running.Status = "in_progress"
	daemon.beads = append(daemon.beads, closed, running)

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 1 || st.Runs[0].AgentID != "gd-iter-closed" {
		t.Fatalf("Runs: got %+v, want only the closed iteration", st.Runs)
	}
}

// TestGardener_FinalizeFromIterations_SkipsNeverRanPhantoms is the sibling
// regression lock for bug ka-0000004058 on the iteration-driven finalizer. A
// closed iteration bead that never got a pod (agent_state=queued/expired/…) must
// NOT record a barren score-0 RunRecord — the same defect fixed in
// gardenerAgentTracker. A queue death made no progress it had a chance to make,
// so scoring it 0 would falsely advance no_progress_for toward a spurious stop.
func TestGardener_FinalizeFromIterations_SkipsNeverRanPhantoms(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
		Progress:   json.RawMessage(progressSpecWithSurface),
	})
	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-ran", "done", `{"progress_score":1}`),
		closedIterationBead("gd-iter-queued", "queued", ""),   // never got a pod
		closedIterationBead("gd-iter-expired", "expired", ""), // deferred past TTL
	)

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 1 || st.Runs[0].AgentID != "gd-iter-ran" {
		t.Fatalf("Runs: got %+v, want only the iteration that reached a pod (phantoms skipped)", st.Runs)
	}
}

// iterationBeadAt is closedIterationBead with an explicit CreatedAt, needed to
// exercise the RunsFinalizedAfter low-water mark (ka-0000003265).
func iterationBeadAt(id, agentState, wrapup string, createdAt time.Time) *beadsapi.BeadDetail {
	b := closedIterationBead(id, agentState, wrapup)
	b.CreatedAt = createdAt
	b.UpdatedAt = createdAt.Add(time.Minute)
	return b
}

// TestGardener_FinalizeFromIterations_HonorsRunsFinalizedAfter is the crux of the
// no_progress_for wedge fix (ka-0000003265): gardenerFinalizeRunsFromIterations
// must NOT re-credit iterations created before Gardener.RunsFinalizedAfter. This
// is what makes a no_progress_for re-arm's ProgressState reset DURABLE — without
// it, the immutable pre-reset zero tail is rebuilt every cycle and the gardener
// re-wedges.
func TestGardener_FinalizeFromIterations_HonorsRunsFinalizedAfter(t *testing.T) {
	baseline := time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead:         "gd-epic-1",
		Progress:           json.RawMessage(progressSpecWithSurface),
		RunsFinalizedAfter: baseline,
	})
	daemon.beads = append(daemon.beads,
		// Pre-baseline barren runs — the stale zero tail; must be SKIPPED.
		iterationBeadAt("gd-old-1", "done", `{"accomplishments":"nothing"}`, baseline.Add(-3*time.Hour)),
		iterationBeadAt("gd-old-2", "done", `{"accomplishments":"nothing"}`, baseline.Add(-2*time.Hour)),
		iterationBeadAt("gd-old-3", "done", `{"accomplishments":"nothing"}`, baseline.Add(-1*time.Hour)),
		// A post-baseline run — the fresh window; must be CREDITED.
		iterationBeadAt("gd-new-1", "done", `{"progress_score":5}`, baseline.Add(1*time.Hour)),
	)

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 1 || st.Runs[0].AgentID != "gd-new-1" {
		t.Fatalf("expected only the post-baseline run credited, got %+v", st.Runs)
	}
	if st.Runs[0].ProgressScore != 5 {
		t.Errorf("post-baseline run score: got %d, want 5", st.Runs[0].ProgressScore)
	}
}

// TestGardener_FinalizeFromIterations_NoProgressReArmStaysDurable is the
// end-to-end regression guard for the deadlock (ka-0000003265). Before the fix, a
// no_progress_for re-arm reset ProgressState but the NEXT reconcile rebuilt the
// same zero tail from the immutable iterations, re-tripping no_progress_for
// forever with last_checked frozen. With the RunsFinalizedAfter baseline the
// re-arm stamps, the subsequent finalize skips the stale iterations — so once the
// gardener has re-armed, a following cycle does NOT re-populate the barren window.
func TestGardener_FinalizeFromIterations_NoProgressReArmStaysDurable(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]}}`
	old := time.Now().Add(-2 * time.Hour)
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-f", Type: "trigger", Status: "open", Title: "subtree gardener"},
			iterationBeadAt("gd-b1", "done", `{"accomplishments":"nothing"}`, old),
			iterationBeadAt("gd-b2", "done", `{"accomplishments":"nothing"}`, old.Add(time.Minute)),
			iterationBeadAt("gd-b3", "done", `{"accomplishments":"nothing"}`, old.Add(2*time.Minute)),
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-f", Title: "subtree gardener", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "monorepo", Role: "crew", Prompt: "run",
		MaxConcurrent: 3, LastChecked: pastLastChecked(),
		Gardener: &GardenerSpec{
			AssignBead: "gd-epic-1",
			Progress:   json.RawMessage(progressSpecWithSurface),
			Stop:       json.RawMessage(stopJSON),
		},
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-f": {{Key: "M1", Title: "match one"}}},
	}
	engine.RegisterSource(src)

	// Cycle 1: finalize records 3 barren runs → no_progress_for trips → re-arm
	// resets ProgressState AND stamps RunsFinalizedAfter=now.
	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	if cap.RunsFinalizedAfter.IsZero() {
		t.Fatalf("expected the no_progress_for re-arm to stamp RunsFinalizedAfter")
	}
	// Feed the re-armed state back onto the trigger bead so the next load sees it.
	blob, err := json.Marshal(cap)
	if err != nil {
		t.Fatalf("marshal re-armed gardener: %v", err)
	}
	for _, b := range daemon.beads {
		if b.ID == "gd-cap-f" {
			if b.Fields == nil {
				b.Fields = map[string]string{}
			}
			b.Fields["gardener"] = string(blob)
		}
	}
	// Reset the recorded writes so we assert only on cycle 2.
	daemon.mu.Lock()
	daemon.updatedIDs = nil
	daemon.updatedFields = nil
	daemon.mu.Unlock()

	// Cycle 2: the stale barren iterations predate RunsFinalizedAfter, so finalize
	// must NOT rebuild the zero window. Feed the trigger through evalOne again.
	trig2 := trig
	c := cap
	trig2.Gardener = &c
	src.triggers = []TriggerDef{trig2}
	engine.Evaluate(context.Background())

	// After cycle 2, no gardener write should re-populate ProgressState.Runs with
	// the stale barren tail (the durable reset held).
	if maxRuns := maxProgressRunsAcrossUpdates(t, daemon, "gd-cap-f"); maxRuns != 0 {
		t.Fatalf("expected the stale barren window to stay cleared on cycle 2, got %d runs", maxRuns)
	}
}

// TestGardener_FinalizeFromIterations_NoProgressSpec_NoOp confirms gardeners
// WITHOUT a Progress spec do not accrue RunRecords from iterations — they don't
// use progress-aware stops, mirroring gardenerAgentTracker's Progress gate.
func TestGardener_FinalizeFromIterations_NoProgressSpec_NoOp(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
		// Progress intentionally empty.
	})
	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-1", "done", `{"progress_score":1}`),
	)

	engine.Evaluate(context.Background())

	// No gardener UpdateBeadFields write carrying a non-empty ProgressState
	// should have happened. Scan all gardener writes; ProgressState must stay
	// empty.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := range daemon.updatedFields {
		if daemon.updatedIDs[i] != "gd-cap-f" {
			continue
		}
		raw, ok := daemon.updatedFields[i]["gardener"]
		if !ok {
			continue
		}
		var cap GardenerSpec
		if err := json.Unmarshal([]byte(raw), &cap); err != nil {
			t.Fatalf("decode gardener: %v", err)
		}
		if len(cap.ProgressState) != 0 {
			t.Errorf("ProgressState should stay empty without a Progress spec, got %q", cap.ProgressState)
		}
	}
}

// TestGardener_FinalizeFromIterations_NoProgressForStillFires is the
// stop-logic safety guard the bead calls out as CRITICAL: once
// ProgressState.Runs is populated for a subtree gardener, the no_progress_for
// stop kind must still behave — neither stop prematurely (with too few runs)
// nor never stop (after enough barren runs).
//
// Here the gardener has a Progress spec, a no_progress_for{runs:3} loop_end, and
// 3 barren (score-0) closed iterations. After finalization records all 3 as
// zero-score runs, the pre-fire stop check must trip and route the gardener
// through resume-or-escalate (premature loop_end is re-armed, not closed —
// bead ka-0000005353). This proves the populated Runs flow correctly into the
// stop evaluator.
func TestGardener_FinalizeFromIterations_NoProgressForStillFires(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]}}`
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-f", Type: "trigger", Status: "open", Title: "subtree gardener"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-f",
		Title:         "subtree gardener",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 3,
		LastChecked:   pastLastChecked(),
		Gardener: &GardenerSpec{
			AssignBead: "gd-epic-1",
			Progress:   json.RawMessage(progressSpecWithSurface),
			Stop:       json.RawMessage(stopJSON),
		},
	}
	// A single match is available — but the no_progress_for stop must pre-empt
	// the fire once 3 barren runs are recorded.
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{"gd-cap-f": {{Key: "M1", Title: "match one"}}},
	}
	engine.RegisterSource(src)

	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-1", "done", `{"accomplishments":"nothing"}`),
		closedIterationBead("gd-iter-2", "done", `{"accomplishments":"nothing"}`),
		closedIterationBead("gd-iter-3", "done", `{"accomplishments":"nothing"}`),
	)

	engine.Evaluate(context.Background())

	// The finalize-iterations write must have recorded all three barren runs so
	// the no_progress_for evaluator can count them. gardenerSpecFromUpdate returns
	// the LAST gardener write, which (post-fix ka-0000002690) is the re-arm — and
	// a no_progress_for re-arm resets the window so the gardener can fire afresh.
	// So scan ALL gardener writes for the one that recorded the 3 barren runs.
	if maxRuns := maxProgressRunsAcrossUpdates(t, daemon, "gd-cap-f"); maxRuns != 3 {
		t.Fatalf("expected a gardener write recording 3 barren runs (finalize), got max %d", maxRuns)
	}
	// Final gardener state: the no_progress_for re-arm cleared the window so the
	// re-armed gardener gets a clean slate to fire and re-prove progress (bug
	// ka-0000002690; mirrors gardenerWakeFromDormant). Without this reset the same
	// barren window re-trips every reconcile and the gardener can never spawn again.
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 0 {
		t.Fatalf("expected no_progress_for re-arm to reset the progress window, got %d runs", len(st.Runs))
	}

	// no_progress_for is a premature loop_end → re-arm (comment), not close,
	// and the fire is skipped (no spawn).
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected no spawn once no_progress_for trips, got %d", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-f") {
		t.Errorf("premature no_progress_for should re-arm, not close; closedIDs=%v", daemon.closedIDs)
	}
	if !slices.Contains(daemon.commentedIDs, "gd-cap-f") {
		t.Errorf("expected re-arm comment on the gardener, got commentedIDs=%v", daemon.commentedIDs)
	}
}

// TestGardener_FinalizeFromIterations_NoProgressForNotYetEnough is the
// other half of the stop-logic safety guard: with FEWER barren runs than the
// no_progress_for window, the gardener must NOT stop (no premature-stop
// regression). Two barren runs against runs:3 ⇒ observed < window ⇒ the gardener
// fires normally.
func TestGardener_FinalizeFromIterations_NoProgressForNotYetEnough(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]}}`
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-f", Type: "trigger", Status: "open", Title: "subtree gardener"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-f",
		Title:         "subtree gardener",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 3,
		LastChecked:   pastLastChecked(),
		Gardener: &GardenerSpec{
			AssignBead: "gd-epic-1",
			Progress:   json.RawMessage(progressSpecWithSurface),
			Stop:       json.RawMessage(stopJSON),
		},
	}
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{"gd-cap-f": {{Key: "M1", Title: "match one"}}},
	}
	engine.RegisterSource(src)

	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-1", "done", `{"accomplishments":"nothing"}`),
		closedIterationBead("gd-iter-2", "done", `{"accomplishments":"nothing"}`),
	)

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-f")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 2 {
		t.Fatalf("ProgressState.Runs: got %d, want 2", len(st.Runs))
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("gardener should still fire with only 2/3 barren runs, got %d spawns", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-f") {
		t.Errorf("gardener must not stop with observed < window; closedIDs=%v", daemon.closedIDs)
	}
}
