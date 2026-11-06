package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler/progress"
)

// TestGardener_ReconcileAsyncProgress_LiftsGapCloses is the end-to-end regression
// guard for the async-attribution gap (tune task ka-0000001254,
// gardener-effectiveness-rubric §D1). A delegating gardener dispatched crew and
// closed score-0 at its own wrap-up; the crew then closed a child bead in the
// gap AFTER that run's closed_at but BEFORE the next run started. That
// child_closed event lands in neither run's FinalizeRun [startedAt, closedAt]
// rescue window, so the run recorded a false score-0 (the First Mate symptom:
// D2=5 children closing, D1=1 progress yield). Once gardenerDiscoverProgress
// discovers the child into state.Events, gardenerReconcileAsyncProgress must lift
// the run that owns the interval the event fell in.
func TestGardener_ReconcileAsyncProgress_LiftsGapCloses(t *testing.T) {
	// A progress spec that discovers closed children under the epic AND opts into
	// a surface (so Spec.IsZero() is false and the hooks engage).
	progressSpec := `{"events":[{"kind":"child_closed","parent":"{{assign_bead}}"}],"surface":{"in_prime":true}}`

	// Seed ProgressState with two already-recorded score-0 delegating runs.
	// Run A: [100,150]. Run B: [300,350]. The gap (150,300) is where an async
	// child-close will land.
	seed := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "gd-run-a", StartedAt: time.Unix(100, 0), ClosedAt: time.Unix(150, 0), ProgressScore: 0, AgentState: "done"},
			{AgentID: "gd-run-b", StartedAt: time.Unix(300, 0), ClosedAt: time.Unix(350, 0), ProgressScore: 0, AgentState: "done"},
		},
	}
	seedJSON, err := seed.Marshal()
	if err != nil {
		t.Fatalf("seed marshal: %v", err)
	}

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "delegating gardener"},
		},
	}
	// Opt into subtree filtering so child_closed discovery is scoped to the epic.
	daemon.parentOf = map[string]string{"gd-child-gap": "gd-epic-1"}
	// The child closed at t=200 — in the gap after run A's closed_at (150) and
	// before run B started (300). closedOrNow reads ClosedAt for OccurredAt.
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-child-gap", Type: "task", Status: "closed",
		ClosedAt: time.Unix(200, 0).UTC().Format(time.RFC3339),
	})

	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "delegating gardener", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "monorepo", Role: "crew", Prompt: "run",
		MaxConcurrent: 3, LastChecked: pastLastChecked(),
		Gardener: &GardenerSpec{
			AssignBead:    "gd-epic-1",
			Progress:      json.RawMessage(progressSpec),
			ProgressState: json.RawMessage(seedJSON),
		},
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {{Key: "M1", Title: "match one"}}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	// The final gardener write must show run A lifted 0 -> 1 (it owns the interval
	// [100,300) the t=200 event fell in) and run B untouched (its interval
	// [300,+inf) does not contain the event).
	st := latestProgressStateWithLiftedRun(t, daemon, "gd-cap-1")
	if st == nil {
		t.Fatalf("no gardener write recorded the async-attributed lift")
	}
	var runA, runB *progress.RunRecord
	for i := range st.Runs {
		switch st.Runs[i].AgentID {
		case "gd-run-a":
			runA = &st.Runs[i]
		case "gd-run-b":
			runB = &st.Runs[i]
		}
	}
	if runA == nil || runB == nil {
		t.Fatalf("expected both seeded runs present, got %+v", st.Runs)
	}
	if runA.ProgressScore != 1 {
		t.Errorf("run A owns interval [100,300) containing the t=200 close; want score 1, got %d", runA.ProgressScore)
	}
	if runB.ProgressScore != 0 {
		t.Errorf("run B interval [300,+inf) has no event; want score 0, got %d", runB.ProgressScore)
	}
}

// TestGardener_ReconcileAsyncProgress_NoProgressSpec_NoOp confirms the hook is a
// no-op for gardeners without a Progress spec, mirroring the other reconcile
// hooks' Progress gate.
func TestGardener_ReconcileAsyncProgress_NoProgressSpec_NoOp(t *testing.T) {
	seed := progress.State{
		Runs: []progress.RunRecord{
			{AgentID: "gd-run-a", StartedAt: time.Unix(100, 0), ProgressScore: 0},
		},
	}
	seedJSON, err := seed.Marshal()
	if err != nil {
		t.Fatalf("seed marshal: %v", err)
	}
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "gardener", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "monorepo", Role: "crew", Prompt: "run",
		MaxConcurrent: 1, LastChecked: pastLastChecked(),
		Gardener: &GardenerSpec{
			// Progress intentionally empty.
			ProgressState: json.RawMessage(seedJSON),
		},
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {{Key: "M1", Title: "match one"}}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	// No gardener write should carry a score-lifted run.
	if st := latestProgressStateWithLiftedRun(t, daemon, "gd-cap-1"); st != nil {
		t.Errorf("hook must not lift scores without a Progress spec; got %+v", st.Runs)
	}
}

// latestProgressStateWithLiftedRun scans all gardener writes for kd and returns
// the parsed ProgressState from the last write whose runs include a positive
// ProgressScore, or nil if none does. Used to assert the reconcile hook credited
// a run without depending on write ordering across the reconcile hooks.
func latestProgressStateWithLiftedRun(t *testing.T, daemon *mockTriggerDaemon, id string) *progress.State {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var found *progress.State
	for i := range daemon.updatedFields {
		if daemon.updatedIDs[i] != id {
			continue
		}
		raw, ok := daemon.updatedFields[i]["gardener"]
		if !ok || raw == "" {
			continue
		}
		var cap GardenerSpec
		if err := json.Unmarshal([]byte(raw), &cap); err != nil {
			t.Fatalf("decode gardener write: %v", err)
		}
		st, err := progress.ParseState(string(cap.ProgressState))
		if err != nil {
			t.Fatalf("parse progress state: %v", err)
		}
		for _, r := range st.Runs {
			if r.ProgressScore > 0 {
				stCopy := st
				found = &stCopy
				break
			}
		}
	}
	return found
}
