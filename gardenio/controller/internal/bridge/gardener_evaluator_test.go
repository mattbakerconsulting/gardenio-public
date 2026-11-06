package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/progress"
)

// gardenerTrigger builds a minimal spawn_agent trigger with the supplied
// gardener spec for evalOne-level tests.
func gardenerTrigger(t *testing.T, cap GardenerSpec) (*mockTriggerDaemon, *TriggerEngine, *mockTriggerSource, TriggerDef) {
	t.Helper()
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-1",
		Title:         "gardener trig",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 1,
		LastChecked:   pastLastChecked(),
		Gardener:      &cap,
	}
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{
			"gd-cap-1": {{Key: "M1", Title: "match one"}},
		},
	}
	engine.RegisterSource(src)
	return daemon, engine, src, trig
}

// TestGardener_RunBudgetExhausted_ReArmsAndSkips: an exhausted run budget is a
// premature stop (bead ka-0000005353). The gardener skips the fire this cycle
// (no spawn) but is RE-ARMED rather than closed — it stays open with a
// refilled budget so it continues toward its unmet terminal_goal. A comment
// records the re-arm.
func TestGardener_RunBudgetExhausted_ReArmsAndSkips(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   2,
		RunsSpawned: 2,
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected no spawns when run_budget exhausted, got %d", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("premature run_budget exhaustion should re-arm, not close; got closedIDs=%v", daemon.closedIDs)
	}
	if !slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("expected gardener trigger to be commented on with the re-arm reason, got %v", daemon.commentedIDs)
	}
}

func TestGardener_OneSpawnAtATime_BlocksWhileRunning(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		OneSpawnAtATime: true,
		LastAgentID:     "gd-running-agent",
	})
	// Add a still-running agent bead.
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-running-agent", Type: "agent", Status: "in_progress",
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected no spawns while prior agent runs, got %d", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("one_spawn_at_a_time should not close the trigger")
	}
}

func TestGardener_OneSpawnAtATime_PassesWhenPriorAgentClosed(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		OneSpawnAtATime: true,
		LastAgentID:     "gd-done-agent",
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-done-agent", Type: "agent", Status: "closed",
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected one spawn after prior agent closed, got %d", len(daemon.spawnedNames))
	}
}

// TestGardener_StopSpec_MaxRunsMatch_ReArms verifies that an exhausted run
// budget (the canonical premature "out of runs" stop) re-arms the gardener
// rather than closing it, since terminal_goal is not declared done
// (bead ka-0000005353). The gardener stays open with a refilled budget.
func TestGardener_StopSpec_MaxRunsMatch_ReArms(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   1,
		RunsSpawned: 1,
		Stop:        json.RawMessage(stopJSON),
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if closed {
		t.Errorf("premature max_runs stop should re-arm, not close; got closedIDs=%v", daemon.closedIDs)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.ResumesWithoutProgress != 1 {
		t.Errorf("expected ResumesWithoutProgress=1 after re-arm, got %d", cap.ResumesWithoutProgress)
	}
	if cap.RunBudget <= 1 {
		t.Errorf("expected run budget refilled above RunsSpawned, got RunBudget=%d", cap.RunBudget)
	}
}

// TestGardener_StopSpec_GoalSatisfied_Closes verifies the master gate: when the
// gardener's freshest run declared goal_satisfied=="yes", the goal_satisfied
// loop_end kind is terminal and CLOSES the gardener (done means done — no
// resume). This is the only loop_end signal that should ever terminate a
// gardener (bead ka-0000005353).
func TestGardener_StopSpec_GoalSatisfied_Closes(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"goal_satisfied"}]}}`
	// A progress state whose latest run declared the goal met.
	progJSON := `{"runs":[{"agent_id":"a","progress_score":0,"goal_satisfied":"yes"}]}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:     5,
		RunsSpawned:   1,
		Stop:          json.RawMessage(stopJSON),
		ProgressState: json.RawMessage(progJSON),
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("expected goal_satisfied=yes to close the gardener (done means done), got closedIDs=%v", daemon.closedIDs)
	}
}

func TestGardener_StopSpec_ManualForceLabel_Closes(t *testing.T) {
	stopJSON := `{"hard_stop":{"any":[{"kind":"manual_force","label":"halt-now"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		Stop: json.RawMessage(stopJSON),
	})
	// Apply the halt label to the gardener trigger bead itself.
	daemon.beads[0].Labels = append(daemon.beads[0].Labels, "halt-now")

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected hard_stop manual_force to skip fire, got %d spawns", len(daemon.spawnedNames))
	}
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("expected hard_stop manual_force to close trigger, got %v", daemon.closedIDs)
	}
}

// TestGardenerTerminate_DisarmsTrigger proves the source-side half of the
// ka-0000001423 fix: gardenerTerminate not only closes the gardener and stamps the
// terminate markers, it also disarms the trigger (enabled=false) so the retired
// lane presents as closed+DISABLED, not the closed+enabled phantom class the
// trigger-reviver re-surveys every 4h. The disarm MUST be a real JSON bool
// (enabled is boolean-typed — a string 400s the whole PATCH, ka-0000003627).
func TestGardenerTerminate_DisarmsTrigger(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig",
				Fields: map[string]string{"enabled": "true"}},
		},
	}
	trig := TriggerDef{ID: "gd-cap-1", Title: "gardener trig", Project: "monorepo"}

	gardenerTerminate(context.Background(), daemon, slog.Default(), trig,
		scheduler.StopSlotLoopEnd, scheduler.StopKindBeadQuery)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Fatalf("expected gardenerTerminate to close the gardener, got closedIDs=%v", daemon.closedIDs)
	}
	// A disarm write (enabled=false, JSON bool) must have landed.
	disarmed := false
	for i, id := range daemon.updatedIDs {
		if id != "gd-cap-1" || i >= len(daemon.updatedTyped) {
			continue
		}
		if v, ok := daemon.updatedTyped[i]["enabled"]; ok {
			b, isBool := v.(bool)
			if !isBool {
				t.Errorf("enabled must be written as a JSON bool, got %T (%v)", v, v)
			}
			if isBool && !b {
				disarmed = true
			}
		}
	}
	if !disarmed {
		t.Errorf("expected gardenerTerminate to disarm the trigger (enabled=false via UpdateBeadFieldsTyped), updates=%v", daemon.updatedTyped)
	}
}

// gardenerTriggerWithCreatedAt is gardenerTrigger with an explicit bead
// creation time, for exercising the wall_clock stop kind end-to-end through the
// fire path (ka-0000006967). CreatedAt is set on BOTH the daemon bead (so
// GetBead lookups agree) and the pre-built TriggerDef the mock source returns
// (LoadTriggers hands back the struct verbatim — the wall_clock evaluator reads
// trig.CreatedAt, which ParseTriggerDef populates in production).
func gardenerTriggerWithCreatedAt(t *testing.T, cap GardenerSpec, createdAt time.Time) (*mockTriggerDaemon, *TriggerEngine) {
	t.Helper()
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig", CreatedAt: createdAt},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-1",
		Title:         "gardener trig",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 1,
		LastChecked:   pastLastChecked(),
		CreatedAt:     createdAt,
		Gardener:      &cap,
	}
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{"gd-cap-1": {{Key: "M1", Title: "match one"}}},
	}
	engine.RegisterSource(src)
	return daemon, engine
}

// TestGardener_HardStopWallClock_ClosesWhenElapsed proves the CreatedAt wiring
// (ka-0000006967) end-to-end through the fire path: a hard_stop wall_clock now
// closes the gardener once the window measured from the bead's creation time has
// elapsed. Before the wiring GardenerStopContext.CreatedAt was always zero, so
// this was a silent no-op and the gardener never closed on wall_clock.
//
// The grandfather epoch is overridden to a fixed PAST instant so the "elapsed"
// direction is deterministic regardless of the real clock (the production epoch
// is a fixed rollout date). ScheduleWallClockGrandfatherEpochForTest is the
// scheduler package's test seam for this.
func TestGardener_HardStopWallClock_ClosesWhenElapsed(t *testing.T) {
	// Epoch 1000h in the past so a 168h window measured from it has elapsed.
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Now().Add(-1000*time.Hour).UTC())

	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`
	daemon, engine := gardenerTriggerWithCreatedAt(t, GardenerSpec{
		Stop: json.RawMessage(stopJSON),
	}, time.Now().Add(-200*time.Hour)) // 200h old, past both epoch and window

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("expected hard_stop wall_clock to skip fire, got %d spawns", len(daemon.spawnedNames))
	}
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("expected elapsed hard_stop wall_clock to close the gardener, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_HardStopWallClock_DoesNotCloseWithinWindow: a gardener created
// recently (well within its wall_clock) must NOT close. Epoch-independent — a
// 1h-old bead is under 168h regardless of the floor.
func TestGardener_HardStopWallClock_DoesNotCloseWithinWindow(t *testing.T) {
	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`
	daemon, engine := gardenerTriggerWithCreatedAt(t, GardenerSpec{
		Stop: json.RawMessage(stopJSON),
	}, time.Now().Add(-1*time.Hour)) // 1h old

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("gardener within its wall_clock window must not close, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_HardStopWallClock_GrandfathersAncientBead is the load-bearing
// safety guarantee (ka-0000006967): a gardener whose bead predates the wiring by
// far more than its wall_clock must NOT be retro-closed on the first reconcile
// after rollout. With the epoch pinned to "1h ago" (the just-after-rollout
// state), the floored window has not elapsed even for a 100-day-old bead.
func TestGardener_HardStopWallClock_GrandfathersAncientBead(t *testing.T) {
	// Epoch 1h in the past — simulate the first reconciles just after rollout.
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Now().Add(-1*time.Hour).UTC())

	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`
	daemon, engine := gardenerTriggerWithCreatedAt(t, GardenerSpec{
		Stop: json.RawMessage(stopJSON),
	}, time.Now().Add(-100*24*time.Hour)) // 100 days old, ancient in absolute terms

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("ancient pre-epoch gardener must be grandfathered (fresh window from epoch), not retro-closed; got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_WallClockTerminated_OperatorReopen_RevivesAndFires is the
// end-to-end acceptance test for bug ka-0000006465: a gardener that hard-stopped
// on wall_clock and is then reopened by an operator (the runbook's documented
// un-retire) must actually FIRE again — not fall into the reopen→re-close loop.
//
// This is the exact-match gd-3Ub6rYSqUY shape: a pre-epoch bead (floored to the
// grandfather epoch) whose 168h window elapsed long ago. Before the fix the
// operator-reopen normalization stamped every durable reset EXCEPT the wall_clock
// baseline, so gardenerPreFireCheck's stop gate re-measured wall_clock from
// max(created_at, epoch) — still expired — and re-closed the gardener on the very
// first reconcile after the reopen. The WallClockSince stamp (both revival paths)
// + the max(WallClockSince, created_at, epoch) baseline together make the revival
// deliverable: the reopened gardener measures wall_clock from the revive instant,
// is inside a fresh window, and fires.
func TestGardener_WallClockTerminated_OperatorReopen_RevivesAndFires(t *testing.T) {
	// Epoch 1000h in the past so a 168h window measured from it has elapsed —
	// the gd-3Ub6rYSqUY shape (bead older than the epoch, floored to it).
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Now().Add(-1000*time.Hour).UTC())

	// The gardener carries a bounded run_budget + hard_stop wall_clock ceiling.
	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`
	cap := GardenerSpec{
		RunBudget:   10,
		RunsSpawned: 3,
		Stop:        json.RawMessage(stopJSON),
		// WallClockSince intentionally zero here — this models the moment right
		// after an operator bead-level reopen, BEFORE the engine normalization
		// runs. The normalization must stamp it so the gardener survives the
		// pre-fire stop gate in the SAME cycle.
	}
	// A pre-epoch creation time (like gd-3Ub6rYSqUY's 2026-06-21).
	createdAt := time.Now().Add(-100 * 24 * time.Hour)

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "reopened wall_clock gardener", CreatedAt: createdAt},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-1",
		Title:         "reopened wall_clock gardener",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 1,
		LastChecked:   pastLastChecked(),
		CreatedAt:     createdAt,
		Gardener:      &cap,
		// The wall_clock terminate fingerprint on an OPEN bead: the operator
		// reopened it but the engine markers are still present.
		GardenerTerminatedSlot: scheduler.StopSlotHardStop,
		GardenerTerminatedKind: scheduler.StopKindWallClock,
	}
	engine.RegisterSource(&mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{"gd-cap-1": {{Key: "M1", Title: "match one"}}},
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Fatalf("wall_clock revival must not be reversed by an immediate re-close; got closedIDs=%v", daemon.closedIDs)
	}
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("reopened wall_clock gardener must fire this cycle (revival deliverable), got %d spawns", len(daemon.spawnedNames))
	}
}

func TestGardener_PostFire_BumpsRunsSpawnedAndStampsLastAgent(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget: 5,
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected one spawn, got %d", len(daemon.spawnedNames))
	}
	// Find the gardener UpdateBeadFields call — it carries the updated
	// gardener JSON. We just verify the engine called UpdateBeadFields on
	// the trigger bead (a `last_checked` update also happens, so we expect
	// at least two updates).
	count := 0
	for _, id := range daemon.updatedIDs {
		if id == "gd-cap-1" {
			count++
		}
	}
	if count < 2 {
		t.Errorf("expected ≥2 UpdateBeadFields on gardener bead (gardener+last_checked), got %d (updatedIDs=%v)", count, daemon.updatedIDs)
	}
}

// TestGardener_NonGardenerTriggerUnaffected ensures triggers without a Gardener
// spec fire normally and never write to the gardener post-fire path. This is
// the regression guard for source-only triggers (the common case).
func TestGardener_NonGardenerTriggerUnaffected(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-plain", Type: "trigger", Status: "open", Title: "plain trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	src := &mockTriggerSource{
		name: "mock",
		triggers: []TriggerDef{{
			ID: "gd-plain", Title: "plain trig", Source: "mock",
			Action: TriggerActionNotify, Project: "monorepo",
			MaxConcurrent: 1, LastChecked: pastLastChecked(),
			// Gardener: nil
		}},
		matches: map[string][]TriggerMatch{
			"gd-plain": {{Key: "M1", Title: "match one"}},
		},
	}
	engine.RegisterSource(src)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-plain") {
		t.Errorf("non-gardener trigger should never be closed by gardener path")
	}
	if slices.Contains(daemon.commentedIDs, "gd-plain") {
		t.Errorf("non-gardener trigger should never be commented on by gardener path")
	}
}

// gardenerSpecFromUpdate finds the most recent UpdateBeadFields call whose
// fields map carries a "gardener" key and returns the decoded GardenerSpec. Used
// by the agent-tracker tests to assert on the persisted bookkeeping counters
// without snapshotting the entire serialized JSON.
func gardenerSpecFromUpdate(t *testing.T, daemon *mockTriggerDaemon, beadID string) GardenerSpec {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := len(daemon.updatedFields) - 1; i >= 0; i-- {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		raw, ok := daemon.updatedFields[i]["gardener"]
		if !ok {
			continue
		}
		var cap GardenerSpec
		if err := json.Unmarshal([]byte(raw), &cap); err != nil {
			t.Fatalf("gardener JSON decode failed: %v (raw=%s)", err, raw)
		}
		return cap
	}
	t.Fatalf("no gardener UpdateBeadFields call for %s found (updates=%v)", beadID, daemon.updatedFields)
	return GardenerSpec{}
}

// maxProgressRunsAcrossUpdates returns the largest ProgressState.Runs count
// recorded across ALL gardener UpdateBeadFields writes for beadID. Used when a
// later write (e.g. a no_progress_for re-arm that resets the window, bug
// ka-0000002690) clears runs that an earlier write recorded — so asserting on
// only the final state would miss the intermediate finalize write.
func maxProgressRunsAcrossUpdates(t *testing.T, daemon *mockTriggerDaemon, beadID string) int {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	maxRuns := 0
	for i := range daemon.updatedFields {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		raw, ok := daemon.updatedFields[i]["gardener"]
		if !ok {
			continue
		}
		var cap GardenerSpec
		if err := json.Unmarshal([]byte(raw), &cap); err != nil {
			t.Fatalf("gardener JSON decode failed: %v (raw=%s)", err, raw)
		}
		st, err := progress.ParseState(string(cap.ProgressState))
		if err != nil {
			continue
		}
		if len(st.Runs) > maxRuns {
			maxRuns = len(st.Runs)
		}
	}
	return maxRuns
}

// gardenerTrackerTrigger builds a gardener trigger with no matches so evalOne
// returns after the agent-tracker runs (no fire path runs, no post-fire
// bookkeeping muddies the assertions). The agent bead under test is appended
// to daemon.beads by the caller.
func gardenerTrackerTrigger(t *testing.T, cap GardenerSpec) (*mockTriggerDaemon, *TriggerEngine) {
	t.Helper()
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-t", Type: "trigger", Status: "open", Title: "gardener tracker trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-t",
		Title:         "gardener tracker trig",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 1,
		LastChecked:   pastLastChecked(),
		Gardener:      &cap,
	}
	// Empty matches → evalOne reaches the agent-tracker call but does not
	// fire. gardenerPostFire never runs, so the only gardener UpdateBeadFields
	// call (if any) is the tracker's.
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{},
	}
	engine.RegisterSource(src)
	return daemon, engine
}

func TestGardener_AgentTracker_ClosedDone_BumpsRunsAndAppendsLog(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsSpawned:         3,
		RunsCompleted:       2,
		ConsecutiveFailures: 1, // a successful run resets this to 0
		LastAgentID:         "gd-agent-done",
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-done",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent_state":  "done",
			"close_reason": "wrapup_complete",
			"wrapup":       `{"accomplishments":"shipped X","handoff_notes":""}`,
		},
	})

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
	if cap.RunsCompleted != 3 {
		t.Errorf("RunsCompleted: got %d, want 3", cap.RunsCompleted)
	}
	if cap.ConsecutiveFailures != 0 {
		t.Errorf("ConsecutiveFailures: got %d, want 0 (done resets)", cap.ConsecutiveFailures)
	}
	if cap.LastCheckedAgent != "gd-agent-done" {
		t.Errorf("LastCheckedAgent: got %q, want gd-agent-done", cap.LastCheckedAgent)
	}

	runLog, err := scheduler.ParseRunLog(string(cap.RunLog))
	if err != nil {
		t.Fatalf("parse RunLog: %v", err)
	}
	if len(runLog) != 1 {
		t.Fatalf("RunLog len: got %d, want 1", len(runLog))
	}
	entry := runLog[0]
	if entry.Agent != "gd-agent-done" {
		t.Errorf("RunLog entry agent: got %q, want gd-agent-done", entry.Agent)
	}
	if entry.Outcome != "done" {
		t.Errorf("RunLog entry outcome: got %q, want done", entry.Outcome)
	}
	if entry.CloseReason != "wrapup_complete" {
		t.Errorf("RunLog entry close_reason: got %q, want wrapup_complete", entry.CloseReason)
	}
	if !strings.Contains(entry.WrapupSummary, "shipped X") {
		t.Errorf("RunLog entry wrapup_summary missing 'shipped X': got %q", entry.WrapupSummary)
	}
}

func TestGardener_AgentTracker_ClosedFailed_BumpsConsecutiveFailures(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted:       1,
		ConsecutiveFailures: 2,
		LastAgentID:         "gd-agent-failed",
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-failed",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent_state":  "failed",
			"close_reason": "timeout",
		},
	})

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
	if cap.RunsCompleted != 2 {
		t.Errorf("RunsCompleted: got %d, want 2", cap.RunsCompleted)
	}
	if cap.ConsecutiveFailures != 3 {
		t.Errorf("ConsecutiveFailures: got %d, want 3 (was 2, +1 on failure)", cap.ConsecutiveFailures)
	}
	if cap.LastCheckedAgent != "gd-agent-failed" {
		t.Errorf("LastCheckedAgent: got %q, want gd-agent-failed", cap.LastCheckedAgent)
	}

	runLog, err := scheduler.ParseRunLog(string(cap.RunLog))
	if err != nil {
		t.Fatalf("parse RunLog: %v", err)
	}
	if len(runLog) != 1 || runLog[0].Outcome != "failed" {
		t.Errorf("RunLog: got %+v, want one failed entry", runLog)
	}
}

// progressSpecWithSurface is a minimal non-zero progress spec: a single
// surface flag is enough for ParseSpec().IsZero() to be false, which is the
// gate gardenerDiscoverProgress / FinalizeRun check before recording runs.
const progressSpecWithSurface = `{"surface":{"in_prime":true}}`

// TestGardener_AgentTracker_FinalizesRunIntoProgressState verifies the tracker
// now records a per-run ProgressScore into ProgressState (via
// progress.FinalizeRun) when the gardener has a Progress spec. This is the
// missing primitive that lets no_progress_for judge a type=trigger gardener; a
// missing/zero wrap-up score must still record a run so a barren gardener
// eventually trips the stop.
func TestGardener_AgentTracker_FinalizesRunIntoProgressState(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted: 0,
		LastAgentID:   "gd-agent-barren",
		Progress:      json.RawMessage(progressSpecWithSurface),
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-barren",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent_state": "done",
			// No progress_score in the wrap-up → score-0 run recorded.
			"wrapup": `{"accomplishments":"poked around, nothing moved"}`,
		},
	})

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 1 {
		t.Fatalf("ProgressState.Runs: got %d, want 1 (a barren run must still be recorded)", len(st.Runs))
	}
	if st.Runs[0].ProgressScore != 0 {
		t.Errorf("ProgressScore: got %d, want 0 (no progress_score in wrap-up)", st.Runs[0].ProgressScore)
	}
	if st.Runs[0].AgentID != "gd-agent-barren" {
		t.Errorf("RunRecord AgentID: got %q, want gd-agent-barren", st.Runs[0].AgentID)
	}
}

// TestGardener_AgentTracker_NoProgressSpec_SkipsFinalize confirms gardeners
// WITHOUT a Progress spec do not accrue RunRecords — they don't use
// progress-aware stops, so the extra state would be dead weight.
func TestGardener_AgentTracker_NoProgressSpec_SkipsFinalize(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted: 0,
		LastAgentID:   "gd-agent-noprog",
		// Progress intentionally empty.
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-noprog",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{"agent_state": "done"},
	})

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
	if len(cap.ProgressState) != 0 {
		t.Errorf("ProgressState should stay empty without a Progress spec, got %q", cap.ProgressState)
	}
}

// TestGardener_AgentTracker_NeverRanPhantom_NotCountedAsCompleted is the
// regression lock for bug ka-0000004058. A gardener agent that never got a pod
// is closed in a PRE-POD state (queued/expired/spawning/…) — it consumed a fire
// slot but never ran. Crediting it as a completed run inflated runs_completed
// AND recorded a barren score-0 progress run that falsely advanced
// no_progress_for. The tracker must NOT bump RunsCompleted and must NOT record
// a RunRecord for such a phantom, while STILL appending a RunLog entry
// (visibility) and stamping LastCheckedAgent (dedup).
//
// The observed instance: acme-scif gardener ka-0000001682 run 88 = agent
// gd-QvCbErN5lW, closed at agent_state=queued without ever getting a pod.
func TestGardener_AgentTracker_NeverRanPhantom_NotCountedAsCompleted(t *testing.T) {
	// Each pre-pod state (plus the closed-while-deferred "expired" alias) must
	// be treated identically: a queue death, not a run.
	for _, state := range []string{"queued", "expired", "spawning", "assigning", "prewarmed", ""} {
		t.Run("state="+stateLabel(state), func(t *testing.T) {
			daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
				RunsSpawned:         88, // run was optimistically counted at fire
				RunsCompleted:       87,
				ConsecutiveFailures: 0,
				LastAgentID:         "gd-agent-phantom",
				Progress:            json.RawMessage(progressSpecWithSurface),
			})
			daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
				ID:     "gd-agent-phantom",
				Type:   "agent",
				Status: "closed",
				Fields: map[string]string{
					"agent_state":  state,
					"close_reason": "stale: queued for 22m without getting a pod slot",
					// No wrap-up: a phantom never ran, so there is none.
				},
			})

			engine.Evaluate(context.Background())

			cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
			if cap.RunsCompleted != 87 {
				t.Errorf("RunsCompleted: got %d, want 87 (a never-ran phantom is not a completed run)", cap.RunsCompleted)
			}
			if cap.ConsecutiveFailures != 0 {
				t.Errorf("ConsecutiveFailures: got %d, want 0 (a queue death is not a failure)", cap.ConsecutiveFailures)
			}
			// Dedup stamp must still advance so the phantom is processed once.
			if cap.LastCheckedAgent != "gd-agent-phantom" {
				t.Errorf("LastCheckedAgent: got %q, want gd-agent-phantom (must dedup even a skipped phantom)", cap.LastCheckedAgent)
			}
			// No barren RunRecord — this is the load-bearing half: a score-0 run
			// here would falsely advance no_progress_for toward a spurious stop.
			st, err := progress.ParseState(string(cap.ProgressState))
			if err != nil {
				t.Fatalf("parse progress state: %v", err)
			}
			if len(st.Runs) != 0 {
				t.Errorf("ProgressState.Runs: got %d, want 0 (a never-ran phantom must not record a barren run)", len(st.Runs))
			}
			// The phantom is still visible in the RunLog for operators.
			runLog, err := scheduler.ParseRunLog(string(cap.RunLog))
			if err != nil {
				t.Fatalf("parse RunLog: %v", err)
			}
			if len(runLog) != 1 || runLog[0].Agent != "gd-agent-phantom" {
				t.Errorf("RunLog: got %+v, want one entry for the phantom (visibility must be preserved)", runLog)
			}
		})
	}
}

// TestGardener_AgentTracker_RanAgent_StillCounted guards the other side of the
// ka-0000004058 fix: an agent that DID reach a pod (working→done/failed) is
// still credited as a completed run and still records a RunRecord. Without this
// the never-ran guard could over-reach and silently drop real runs.
func TestGardener_AgentTracker_RanAgent_StillCounted(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted: 5,
		LastAgentID:   "gd-agent-ran",
		Progress:      json.RawMessage(progressSpecWithSurface),
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-ran",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent_state": "done",
			"wrapup":      `{"accomplishments":"did work"}`,
		},
	})

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
	if cap.RunsCompleted != 6 {
		t.Errorf("RunsCompleted: got %d, want 6 (a run that reached a pod is a completed run)", cap.RunsCompleted)
	}
	st, err := progress.ParseState(string(cap.ProgressState))
	if err != nil {
		t.Fatalf("parse progress state: %v", err)
	}
	if len(st.Runs) != 1 {
		t.Errorf("ProgressState.Runs: got %d, want 1 (a real run must record a RunRecord)", len(st.Runs))
	}
}

// stateLabel renders an agent_state for a subtest name, mapping the empty
// (spawn-never-reached-spawning) state to a readable token.
func stateLabel(state string) string {
	if state == "" {
		return "empty"
	}
	return state
}

// gardenerTopLevelFieldFromUpdate returns the value of a registered top-level
// field (e.g. "runs_completed") from the most recent UpdateBeadFields call on
// beadID that also carries a "gardener" key. The mirror writes (B1,
// ka-0000002275) put the blob counters and the gardener blob in the SAME
// UpdateBeadFields map, so anchoring on the gardener-bearing update lets us
// assert that the top-level mirror agrees with the authoritative blob.
func gardenerTopLevelFieldFromUpdate(t *testing.T, daemon *mockTriggerDaemon, beadID, field string) (string, bool) {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := len(daemon.updatedFields) - 1; i >= 0; i-- {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		if _, ok := daemon.updatedFields[i]["gardener"]; !ok {
			continue
		}
		v, ok := daemon.updatedFields[i][field]
		return v, ok
	}
	return "", false
}

// TestGardener_AgentTracker_MirrorsCountersToTopLevelFields is the B1
// (ka-0000002275) invariant guard for the agent-tracker write: the blob
// counters must be mirrored up to the registered top-level fields in the same
// write, so external observers (gb gardener status, beads-viewer, declarative
// bead_query conditions) read live values instead of a dead 0. Anchored on the
// canonical-signal decision: last_checked is the liveness signal; these
// top-level counters are eventually-consistent convenience mirrors.
func TestGardener_AgentTracker_MirrorsCountersToTopLevelFields(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted:       2,
		ConsecutiveFailures: 1, // a successful run resets this to 0
		LastAgentID:         "gd-agent-done",
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-done",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{"agent_state": "done"},
	})

	engine.Evaluate(context.Background())

	// Blob is authoritative; top-level field must agree with it.
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
	if cap.RunsCompleted != 3 {
		t.Fatalf("blob RunsCompleted: got %d, want 3", cap.RunsCompleted)
	}
	if got, ok := gardenerTopLevelFieldFromUpdate(t, daemon, "gd-cap-t", "runs_completed"); !ok || got != "3" {
		t.Errorf("top-level runs_completed mirror: got %q (present=%v), want \"3\"", got, ok)
	}
	if got, ok := gardenerTopLevelFieldFromUpdate(t, daemon, "gd-cap-t", "consecutive_failures"); !ok || got != "0" {
		t.Errorf("top-level consecutive_failures mirror: got %q (present=%v), want \"0\" (done resets)", got, ok)
	}
}

// TestGardener_PostFire_MirrorsCountersAndStampsLastRun is the B1 invariant
// guard for the post-fire write: runs_spawned + last_agent_id are mirrored to
// the registered top-level fields and last_run is stamped, so last_run stops
// being null-by-construction on the type=trigger gardener path. last_checked
// remains the canonical liveness signal (asserted elsewhere); last_run is a
// best-effort convenience mirror that must be populated, not empty.
func TestGardener_PostFire_MirrorsCountersAndStampsLastRun(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.RunsSpawned != 1 {
		t.Fatalf("blob RunsSpawned: got %d, want 1", cap.RunsSpawned)
	}
	if got, ok := gardenerTopLevelFieldFromUpdate(t, daemon, "gd-cap-1", "runs_spawned"); !ok || got != "1" {
		t.Errorf("top-level runs_spawned mirror: got %q (present=%v), want \"1\"", got, ok)
	}
	if got, ok := gardenerTopLevelFieldFromUpdate(t, daemon, "gd-cap-1", "last_run"); !ok || strings.TrimSpace(got) == "" {
		t.Errorf("last_run must be stamped at fire time (not null-by-construction), got %q (present=%v)", got, ok)
	} else if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("last_run must be RFC3339, got %q: %v", got, err)
	}
	if got, ok := gardenerTopLevelFieldFromUpdate(t, daemon, "gd-cap-1", "last_agent_id"); !ok || got != cap.LastAgentID {
		t.Errorf("top-level last_agent_id mirror: got %q (present=%v), want %q", got, ok, cap.LastAgentID)
	}
}

// TestGardener_PostFire_SeedsLineageRoot asserts gardenerPostFire seeds the
// engine-owned LineageRoot from the FIRST iteration agent it records, then holds
// it constant across later fires (persistent-agents Phase 1, task ka-0000006931).
// LineageRoot is what lets every iteration stamp lineage_root_id = the chain root
// so the viewer groups a whole gardener life-thread by indexed lookup.
func TestGardener_PostFire_SeedsLineageRoot(t *testing.T) {
	// First fire: no prior root, so the recorded agent (mock returns gd-agent-1)
	// becomes the root.
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.LastAgentID != "gd-agent-1" {
		t.Fatalf("LastAgentID: got %q, want gd-agent-1", cap.LastAgentID)
	}
	if cap.LineageRoot != "gd-agent-1" {
		t.Errorf("first fire must seed LineageRoot to the first agent id; got %q, want gd-agent-1", cap.LineageRoot)
	}

	// A gardener that already has a LineageRoot keeps it (idempotent seed): the
	// root of the chain never moves off the first iteration.
	daemon2, engine2, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   5,
		RunsSpawned: 3,
		LineageRoot: "gd-root-original",
	})
	engine2.Evaluate(context.Background())
	cap2 := gardenerSpecFromUpdate(t, daemon2, "gd-cap-1")
	if cap2.LineageRoot != "gd-root-original" {
		t.Errorf("existing LineageRoot must be preserved, not overwritten; got %q, want gd-root-original", cap2.LineageRoot)
	}
}

// TestGardener_PostFire_RunsSpawnedIsIntegerTyped is the regression guard for
// ka-0000007976: gardenerPostFire must send the runs_spawned counter mirror as a
// JSON NUMBER, not a string. The kbeads type:trigger schema registers
// runs_spawned as integer, and validateFieldValue rejects a string for an
// integer field — failing the WHOLE fields PATCH (which bundles the gardener blob
// with the bumped RunsSpawned). The old code used UpdateBeadFields
// (map[string]string), so runs_spawned went out as "1" and every write was
// silently rejected: RunsSpawned stayed frozen at 0 fleet-wide, run_budget never
// tripped, and dormant_on_escalation was dead. This asserts the write goes
// through the typed path with an integer value so real kbeads validation passes.
func TestGardener_PostFire_RunsSpawnedIsIntegerTyped(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	var found bool
	for _, typed := range daemon.updatedTyped {
		v, ok := typed["runs_spawned"]
		if !ok {
			continue
		}
		found = true
		// UpdateBeadFieldsTyped preserves the caller's Go type. gardenerPostFire
		// must pass an int (JSON number), never a string, or kbeads rejects the
		// integer-typed field and drops the bundled gardener-blob write with it.
		switch v.(type) {
		case int, int64, float64:
			// integer/number — accepted by the schema.
		default:
			t.Fatalf("runs_spawned mirror must be a numeric type for the integer-typed schema field, got %T (%v)", v, v)
		}
	}
	if !found {
		t.Fatalf("gardenerPostFire did not mirror runs_spawned via UpdateBeadFieldsTyped; typed updates=%v", daemon.updatedTyped)
	}
}

// TestGardener_AgentTracker_CountersAreIntegerTyped is the regression guard for
// ka-0000004023, the sibling of ka-0000007976: gardenerAgentTracker must send the
// runs_completed and consecutive_failures counter mirrors as JSON NUMBERS, not
// strings. Both are integer-typed on the kbeads type:trigger schema, and
// validateFieldValue rejects a string for an integer field — failing the WHOLE
// fields PATCH (which bundles the gardener blob with the bumped counters). The old
// code used UpdateBeadFields (map[string]string), so the counters went out as
// "N" and every write was silently rejected: the RunsCompleted/ConsecutiveFailures
// top-level mirrors stayed dead and any hard_stop kind reading consecutive_failures
// via GardenerStopContext was starved. This asserts the write goes through the typed
// path with integer values so real kbeads validation passes.
func TestGardener_AgentTracker_CountersAreIntegerTyped(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted:       2,
		ConsecutiveFailures: 1,
		LastAgentID:         "gd-agent-failed",
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-failed",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent_state":  "failed",
			"close_reason": "timeout",
		},
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// The tracker must route the counter mirror through UpdateBeadFieldsTyped;
	// find the typed write that carries the gardener blob (the tracker's write).
	var found bool
	for _, typed := range daemon.updatedTyped {
		if _, hasGardener := typed["gardener"]; !hasGardener {
			continue
		}
		found = true
		for _, field := range []string{"runs_completed", "consecutive_failures"} {
			v, ok := typed[field]
			if !ok {
				t.Errorf("tracker typed write missing %s mirror; got %v", field, typed)
				continue
			}
			// UpdateBeadFieldsTyped preserves the caller's Go type. The tracker
			// must pass an int (JSON number), never a string, or kbeads rejects
			// the integer-typed field and drops the bundled gardener-blob write.
			switch v.(type) {
			case int, int64, float64:
				// integer/number — accepted by the schema.
			default:
				t.Errorf("%s mirror must be a numeric type for the integer-typed schema field, got %T (%v)", field, v, v)
			}
		}
	}
	if !found {
		t.Fatalf("gardenerAgentTracker did not mirror counters via UpdateBeadFieldsTyped; typed updates=%v", daemon.updatedTyped)
	}
}

func TestGardener_AgentTracker_AgentStillRunning_NoOp(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted:       1,
		ConsecutiveFailures: 0,
		LastAgentID:         "gd-agent-running",
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-running",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"agent_state": "running"},
	})

	engine.Evaluate(context.Background())

	// Tracker must not have written any "gardener" field update.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i, id := range daemon.updatedIDs {
		if id == "gd-cap-t" {
			if _, ok := daemon.updatedFields[i]["gardener"]; ok {
				t.Errorf("tracker should not persist gardener JSON for a running agent, got fields=%v", daemon.updatedFields[i])
			}
		}
	}
}

func TestGardener_AgentTracker_AlreadyChecked_NoOp(t *testing.T) {
	// Trigger declares LastCheckedAgent == LastAgentID — the tracker has
	// already processed this agent on a prior cycle and must not double-bump.
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted:    5,
		LastAgentID:      "gd-agent-done",
		LastCheckedAgent: "gd-agent-done",
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-done",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{"agent_state": "done"},
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i, id := range daemon.updatedIDs {
		if id == "gd-cap-t" {
			if _, ok := daemon.updatedFields[i]["gardener"]; ok {
				t.Errorf("tracker should not double-process an already-checked agent, got fields=%v", daemon.updatedFields[i])
			}
		}
	}
}

func TestGardener_AgentTracker_NoLastAgent_NoOp(t *testing.T) {
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsCompleted: 0,
		// LastAgentID intentionally empty.
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i, id := range daemon.updatedIDs {
		if id == "gd-cap-t" {
			if _, ok := daemon.updatedFields[i]["gardener"]; ok {
				t.Errorf("tracker should not write gardener JSON when LastAgentID is empty, got fields=%v", daemon.updatedFields[i])
			}
		}
	}
}

// TestGardener_AgentTracker_FeedsStopSpecConsecutiveFailures is the
// cross-cutting acceptance test from the bead: after the tracker bumps
// ConsecutiveFailures to the stop_spec threshold, the consecutive_failures
// loop_end kind must fire on the same evalOne cycle, closing the gardener
// trigger. This is the load-bearing G1 behavior the c1 task exists to unlock —
// without the tracker the counter never moves and the stop kind never trips.
func TestGardener_AgentTracker_FeedsStopSpecConsecutiveFailures(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"consecutive_failures","value":"3"}]}}`
	daemon, engine := gardenerTrackerTrigger(t, GardenerSpec{
		RunsSpawned:         3,
		RunsCompleted:       2,
		ConsecutiveFailures: 2, // tracker will bump to 3, matching threshold
		LastAgentID:         "gd-agent-failed3",
		Stop:                json.RawMessage(stopJSON),
	})
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     "gd-agent-failed3",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{"agent_state": "failed"},
	})

	engine.Evaluate(context.Background())

	// consecutive_failures is a *premature* stop kind (bead ka-0000005353):
	// a crashing gardener whose terminal_goal is unmet is RE-ARMED rather than
	// silently closed. With the default resume budget and no prior resumes,
	// the gardener must NOT be closed; it should re-arm (resumes_without_progress
	// bumped to 1, failure streak reset).
	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-t")
	daemon.mu.Unlock()
	if closed {
		t.Errorf("premature consecutive_failures stop should re-arm, not close; got closedIDs=%v", daemon.closedIDs)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-t")
	if cap.ResumesWithoutProgress != 1 {
		t.Errorf("expected ResumesWithoutProgress=1 after first re-arm, got %d", cap.ResumesWithoutProgress)
	}
	if cap.LastResumeReason != scheduler.StopKindConsecutiveFailures {
		t.Errorf("expected LastResumeReason=%q, got %q", scheduler.StopKindConsecutiveFailures, cap.LastResumeReason)
	}
}

// ── Cross-spawn credential-crashloop suppression (ka-0000006377) ───────────

// credRunLogJSON builds a gardener run_log field from a list of per-run
// close_reasons (oldest first), matching the shape gardenerAgentTracker writes.
func credRunLogJSON(t *testing.T, reasons ...string) json.RawMessage {
	t.Helper()
	var rl scheduler.RunLog
	for i, r := range reasons {
		rl.Append(scheduler.RunLogEntry{
			Agent:       "gd-agent-" + strconv.Itoa(i),
			Outcome:     "failed",
			CloseReason: r,
		})
	}
	raw, err := rl.Marshal()
	if err != nil {
		t.Fatalf("marshal run_log: %v", err)
	}
	return json.RawMessage(raw)
}

// TestGardenerCredCrashlooping unit-tests the streak predicate directly: only a
// trailing run of exactly credCrashloopThreshold not_logged_in closures trips
// it; any other close_reason in the trailing window (a run that DID log in)
// resets the streak, and fewer than the threshold recorded runs is never a
// crashloop.
func TestGardenerCredCrashlooping(t *testing.T) {
	cases := []struct {
		name    string
		reasons []string
		want    bool
	}{
		{"empty", nil, false},
		{"one_notlogged", []string{"not_logged_in"}, false},
		{"two_notlogged_below_threshold", []string{"not_logged_in", "not_logged_in"}, false},
		{"three_notlogged_trips", []string{"not_logged_in", "not_logged_in", "not_logged_in"}, true},
		{"trailing_three_after_recovery_trips", []string{"done", "not_logged_in", "not_logged_in", "not_logged_in"}, true},
		{"recent_recovery_resets", []string{"not_logged_in", "not_logged_in", "wrapup_complete"}, false},
		{"middle_other_reason_resets", []string{"not_logged_in", "timeout", "not_logged_in", "not_logged_in"}, false},
		{"other_failures_not_crashloop", []string{"timeout", "timeout", "timeout"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := GardenerSpec{RunLog: credRunLogJSON(t, tc.reasons...)}
			if got := gardenerCredCrashlooping(&cap); got != tc.want {
				t.Errorf("gardenerCredCrashlooping(%v) = %v, want %v", tc.reasons, got, tc.want)
			}
		})
	}
	if gardenerCredCrashlooping(nil) {
		t.Error("gardenerCredCrashlooping(nil) = true, want false")
	}
}

// TestGardener_CredCrashloop_EscalatesAndCloses is the cross-spawn acceptance
// test (ka-0000006377): a gardener whose last credCrashloopThreshold spawns ALL
// closed close_reason=not_logged_in must be ESCALATED (comment + stuck-escalate
// label + close) on the next pre-fire check instead of respawning yet another
// agent that cannot log in — the incident's 33-wake-up loop (ka-0000007394). It
// must NOT spawn on this cycle even though a match is pending.
func TestGardener_CredCrashloop_EscalatesAndCloses(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunsSpawned: 3,
		RunLog:      credRunLogJSON(t, "not_logged_in", "not_logged_in", "not_logged_in"),
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("expected credential-crashlooping gardener to be closed, got closedIDs=%v", daemon.closedIDs)
	}
	if !slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("expected an escalation comment on the gardener, got commentedIDs=%v", daemon.commentedIDs)
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("credential-crashlooping gardener must not spawn, got %d spawns", len(daemon.spawnedNames))
	}
	// The gardener must be flagged for the resumer watchdog / operator decision
	// via a REAL label (AddLabel), not a fields-blob write (ka-0000007275).
	labeled := false
	for _, l := range daemon.addedLabels {
		if l.BeadID == "gd-cap-1" && l.Label == gardenerStuckEscalateLabel {
			labeled = true
		}
	}
	if !labeled {
		t.Errorf("expected gardener flagged %q for the resumer watchdog, got labels=%v", gardenerStuckEscalateLabel, daemon.addedLabels)
	}
}

// TestGardener_CredCrashloop_TransientRecovers_DoesNotEscalate: a gardener whose
// most recent run recovered (logged in and closed with a non-not_logged_in
// reason) is NOT crashlooping even if earlier runs failed to log in. The gate
// must not trip — transient credential-provisioning lag that recovered on a
// later spawn is exactly what threshold 3 tolerates (ka-0000003924 calibration).
func TestGardener_CredCrashloop_TransientRecovers_DoesNotEscalate(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunsSpawned: 3,
		RunLog:      credRunLogJSON(t, "not_logged_in", "not_logged_in", "wrapup_complete"),
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("gardener whose latest run recovered must not be escalated/closed, got closedIDs=%v", daemon.closedIDs)
	}
	for _, l := range daemon.addedLabels {
		if l.BeadID == "gd-cap-1" && l.Label == gardenerStuckEscalateLabel {
			t.Errorf("recovered gardener must not be flagged %q, got labels=%v", gardenerStuckEscalateLabel, daemon.addedLabels)
		}
	}
}

// TestGardener_CredCrashloop_BelowThreshold_DoesNotEscalate: two consecutive
// not_logged_in closures are below credCrashloopThreshold, so the gate holds
// its fire — a single/double switch-time blip must not escalate.
func TestGardener_CredCrashloop_BelowThreshold_DoesNotEscalate(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunsSpawned: 2,
		RunLog:      credRunLogJSON(t, "not_logged_in", "not_logged_in"),
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("gardener below crashloop threshold must not be escalated/closed, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_PrematureStop_EscalatesWhenResumeBudgetExhausted verifies the
// other half of the resume contract: once a gardener has used up its resume
// budget on fruitless re-arms (ResumesWithoutProgress >= ResumeBudget) and its
// goal is still unmet, the next premature stop ESCALATES — closing the gardener
// and flagging it for the resumer watchdog — rather than re-arming forever.
// This is the advice ka-0000000974 bound: spin terminates at the operator.
func TestGardener_PrematureStop_EscalatesWhenResumeBudgetExhausted(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2, // budget already spent
		Stop:                   json.RawMessage(stopJSON),
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("expected gardener to be closed (escalated) once resume budget exhausted, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_PrematureStop_ResumeDisabled_Closes verifies the explicit opt-out:
// a gardener with ResumeBudget < 0 keeps the legacy behavior — a premature stop
// closes it immediately, no re-arm.
func TestGardener_PrematureStop_ResumeDisabled_Closes(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:    1,
		RunsSpawned:  1,
		ResumeBudget: -1, // resume disabled
		Stop:         json.RawMessage(stopJSON),
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("expected gardener with resume disabled to close on premature stop, got closedIDs=%v", daemon.closedIDs)
	}
}

// ── Dormant-on-escalation (bug ka-0000003082) ──────────────────────────────

// TestGardener_PrematureStop_FrontierWedgedOnEscalation_GoesDormant: a gardener
// whose resume budget is spent but whose frontier epic carries an OPEN operator
// escalation must NOT close (which would sever the cron wake-path and silently
// orphan it). It goes DORMANT instead: stays open + enabled, stamps
// DormantOnEscalation + DormantBlocker on the gardener block.
func TestGardener_PrematureStop_FrontierWedgedOnEscalation_GoesDormant(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2, // budget already spent
		AssignBead:             "gd-epic-1",
		Stop:                   json.RawMessage(stopJSON),
	})
	// Frontier: an OPEN epic with an OPEN operator escalation under it.
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-1", Type: "escalation", Status: "open", Title: "operator: seed secret"},
	)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	spawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if closed {
		t.Errorf("gardener wedged on open escalation must go dormant, not close; got closedIDs=%v", daemon.closedIDs)
	}
	if spawns != 0 {
		t.Errorf("dormant gardener must not spawn, got %d", spawns)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if !cap.DormantOnEscalation {
		t.Errorf("expected DormantOnEscalation=true, got %+v", cap)
	}
	if cap.DormantBlocker != "gd-esc-1" {
		t.Errorf("expected DormantBlocker=gd-esc-1, got %q", cap.DormantBlocker)
	}
}

// TestGardener_PrematureStop_FrontierViaLabel_GoesDormant: same as above but the
// gardener pins its epic via the gardener-epic:<id> label instead of AssignBead.
func TestGardener_PrematureStop_FrontierViaLabel_GoesDormant(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2,
		Stop:                   json.RawMessage(stopJSON),
	})
	daemon.beads[0].Labels = append(daemon.beads[0].Labels, "gardener-epic:gd-epic-9")
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-9", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-dec-9", Type: "decision", Status: "open", Title: "operator decision"},
	)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if closed {
		t.Errorf("label-pinned gardener wedged on open decision must go dormant, not close; got closedIDs=%v", daemon.closedIDs)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if !cap.DormantOnEscalation || cap.DormantBlocker != "gd-dec-9" {
		t.Errorf("expected dormant on gd-dec-9, got DormantOnEscalation=%v DormantBlocker=%q", cap.DormantOnEscalation, cap.DormantBlocker)
	}
}

// TestGardener_Dormant_StillWedged_SkipsFireSilently: an already-dormant gardener
// whose blocker is still open skips the fire with no spawn, no close, and no
// gardener-block churn (no UpdateBeadFields) — this is what ends the watchdog
// reopen/re-close thrash.
func TestGardener_Dormant_StillWedged_SkipsFireSilently(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:           5,
		RunsSpawned:         1,
		AssignBead:          "gd-epic-1",
		DormantOnEscalation: true,
		DormantBlocker:      "gd-esc-1",
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-1", Type: "escalation", Status: "open", Title: "operator: seed secret"},
	)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("still-wedged dormant gardener must not spawn, got %d", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("still-wedged dormant gardener must not close, got %v", daemon.closedIDs)
	}
	for _, id := range daemon.updatedIDs {
		if id == "gd-cap-1" {
			t.Errorf("still-wedged dormant gardener must not churn its gardener block, got update for %s", id)
		}
	}
}

// TestGardener_Dormant_BlockerCleared_WakesAndResetsHistory: once the operator
// clears the blocker (no open escalation/decision remains under the still-open
// epic), the dormant gardener wakes — DormantOnEscalation clears and the stale
// no-progress history (ProgressState + ResumesWithoutProgress) resets so it does
// not instantly re-trip no_progress_for.
func TestGardener_Dormant_BlockerCleared_WakesAndResetsHistory(t *testing.T) {
	staleProgress := `{"runs":[{"agent_id":"a","progress_score":0},{"agent_id":"b","progress_score":0}]}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:              10,
		RunsSpawned:            1,
		ResumesWithoutProgress: 3,
		AssignBead:             "gd-epic-1",
		DormantOnEscalation:    true,
		DormantBlocker:         "gd-esc-1",
		ProgressState:          json.RawMessage(staleProgress),
	})
	// Epic still open, but the escalation is now CLOSED (operator cleared it),
	// so no open blocker remains under the frontier.
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "frontier epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-1", Type: "escalation", Status: "closed", Title: "operator: seed secret"},
	)

	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.DormantOnEscalation {
		t.Errorf("blocker cleared — gardener must wake (DormantOnEscalation=false), got true")
	}
	if cap.DormantBlocker != "" {
		t.Errorf("expected DormantBlocker cleared on wake, got %q", cap.DormantBlocker)
	}
	if len(cap.ProgressState) != 0 {
		t.Errorf("expected ProgressState reset on wake (fresh no-progress window), got %s", cap.ProgressState)
	}
	if cap.ResumesWithoutProgress != 0 {
		t.Errorf("expected ResumesWithoutProgress reset to 0 on wake, got %d", cap.ResumesWithoutProgress)
	}
}

// ── Resume budget spent: dormant on plan activity (gd-dNxyjFLGRJ) ──────────

// planActivityGardener builds a gardener whose resume budget is spent, driving
// an open plan gd-epic-1 with two open steps under it (parentage set, so the
// mock honours q.Parent).
func planActivityGardener(t *testing.T) (*mockTriggerDaemon, *TriggerEngine, TriggerDef) {
	t.Helper()
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, trig := gardenerTrigger(t, GardenerSpec{
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2,
		AssignBead:             "gd-epic-1",
		Stop:                   json.RawMessage(stopJSON),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "plan"},
		&beadsapi.BeadDetail{ID: "gd-step-1", Type: "task", Status: "open", Title: "step 1"},
		&beadsapi.BeadDetail{ID: "gd-step-2", Type: "task", Status: "in_progress", Title: "step 2"},
	)
	daemon.parentOf = map[string]string{"gd-step-1": "gd-epic-1", "gd-step-2": "gd-epic-1"}
	return daemon, engine, trig
}

// TestGardener_PrematureStop_PlanOpen_GoesDormantNotClosed: a plan gardener
// whose resume budget is spent with open steps and no operator blocker stays
// open, parks as resume_budget_spent with the plan's activity digest, keeps the
// stuck label, and spawns nothing.
func TestGardener_PrematureStop_PlanOpen_GoesDormantNotClosed(t *testing.T) {
	daemon, engine, _ := planActivityGardener(t)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	spawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if closed {
		t.Fatalf("resume-spent plan gardener must go dormant, not close; closedIDs=%v", daemon.closedIDs)
	}
	if spawns != 0 {
		t.Errorf("dormant gardener must not spawn, got %d", spawns)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if !cap.DormantOnEscalation || cap.DormantBlockerKind != gardenerDormantKindResumeBudgetSpent {
		t.Errorf("want dormant kind %q, got dormant=%v kind=%q", gardenerDormantKindResumeBudgetSpent, cap.DormantOnEscalation, cap.DormantBlockerKind)
	}
	if cap.DormantBlocker != "gd-epic-1" || cap.DormantActivityDigest == "" {
		t.Errorf("want blocker gd-epic-1 and a digest, got blocker=%q digest=%q", cap.DormantBlocker, cap.DormantActivityDigest)
	}
	if labels := daemon.labelsAddedFor("gd-cap-1"); !slices.Contains(labels, gardenerStuckEscalateLabel) {
		t.Errorf("dormant plan gardener keeps the %q label for visibility; got %v", gardenerStuckEscalateLabel, labels)
	}
}

// TestGardener_ResumeSpentDormant_PlanQuiet_StaysParked: with the plan
// unchanged the parked gardener skips silently: no spawn, no write.
func TestGardener_ResumeSpentDormant_PlanQuiet_StaysParked(t *testing.T) {
	daemon, engine, trig := planActivityGardener(t)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	daemon.updatedIDs, daemon.updatedFields = nil, nil
	daemon.mu.Unlock()
	skip, reason := gardenerPreFireCheck(context.Background(), daemon, slog.Default(), trig)
	if !skip || reason != gardenerSkipDormant {
		t.Fatalf("quiet plan: want skip %q, got skip=%v reason=%q", gardenerSkipDormant, skip, reason)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.updatedIDs) != 0 || slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("quiet plan: want no writes and no close, got updates=%v closed=%v", daemon.updatedIDs, daemon.closedIDs)
	}
}

// TestGardener_ResumeSpentDormant_PlanActivity_WakesWithFreshBudget: each kind
// of plan activity (a step closes, a step returns to ready, a step is added)
// wakes the parked gardener with a refilled run budget and a fresh resume
// budget, so the run_budget gate no longer trips.
func TestGardener_ResumeSpentDormant_PlanActivity_WakesWithFreshBudget(t *testing.T) {
	cases := map[string]func(d *mockTriggerDaemon){
		"step closed":        func(d *mockTriggerDaemon) { d.beads[2].Status = "closed" },
		"step back to ready": func(d *mockTriggerDaemon) { d.beads[3].Status = "open" },
		"step added": func(d *mockTriggerDaemon) {
			d.beads = append(d.beads, &beadsapi.BeadDetail{ID: "gd-step-3", Type: "task", Status: "open", Title: "step 3"})
			d.parentOf["gd-step-3"] = "gd-epic-1"
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			daemon, engine, trig := planActivityGardener(t)
			engine.Evaluate(context.Background())
			if !trig.Gardener.DormantOnEscalation {
				t.Fatalf("setup: gardener did not park")
			}

			daemon.mu.Lock()
			change(daemon)
			daemon.mu.Unlock()
			skip, reason := gardenerPreFireCheck(context.Background(), daemon, slog.Default(), trig)
			if skip && reason == gardenerSkipDormant {
				t.Fatalf("plan changed: gardener must wake, still skipped %q", reason)
			}
			if reason == gardenerSkipRunBudget {
				t.Errorf("woken gardener must have a refilled run budget, got skip %q", reason)
			}
			cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
			if cap.DormantOnEscalation || cap.DormantBlockerKind != "" || cap.DormantActivityDigest != "" {
				t.Errorf("want dormancy cleared, got dormant=%v kind=%q digest=%q", cap.DormantOnEscalation, cap.DormantBlockerKind, cap.DormantActivityDigest)
			}
			if cap.ResumesWithoutProgress != 0 || cap.RunBudget <= cap.RunsSpawned {
				t.Errorf("want fresh budgets, got resumes=%d run_budget=%d runs_spawned=%d", cap.ResumesWithoutProgress, cap.RunBudget, cap.RunsSpawned)
			}
			daemon.mu.Lock()
			defer daemon.mu.Unlock()
			if slices.Contains(daemon.closedIDs, "gd-cap-1") {
				t.Errorf("woken gardener must not close, got %v", daemon.closedIDs)
			}
		})
	}
}

// TestGardener_ResumeSpent_SubtreeReadFails_NeitherClosesNorWakes: a failed
// subtree read neither closes the gardener at park time nor wakes a parked one
// (a wake refills the run budget, so a flaky read must not buy spawns).
func TestGardener_ResumeSpent_SubtreeReadFails_NeitherClosesNorWakes(t *testing.T) {
	daemon, engine, trig := planActivityGardener(t)
	daemon.listBeadsErrForParent = map[string]error{"gd-epic-1": fmt.Errorf("simulated read failure")}

	engine.Evaluate(context.Background())
	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.listBeadsErrForParent = nil
	daemon.mu.Unlock()
	if closed || trig.Gardener.DormantOnEscalation {
		t.Fatalf("read failure at park: want open and not yet parked, got closed=%v dormant=%v", closed, trig.Gardener.DormantOnEscalation)
	}

	// Next tick parks; then a read failure must keep it parked.
	gardenerPreFireCheck(context.Background(), daemon, slog.Default(), trig)
	if !trig.Gardener.DormantOnEscalation {
		t.Fatalf("setup: gardener did not park on the retry")
	}
	daemon.mu.Lock()
	daemon.beads[2].Status = "closed"
	daemon.listBeadsErrForParent = map[string]error{"gd-epic-1": fmt.Errorf("simulated read failure")}
	daemon.mu.Unlock()
	if skip, reason := gardenerPreFireCheck(context.Background(), daemon, slog.Default(), trig); !skip || reason != gardenerSkipDormant {
		t.Errorf("read failure while parked: want skip %q, got skip=%v reason=%q", gardenerSkipDormant, skip, reason)
	}
}

// TestGardener_ResumeSpent_ReadFails_DoesNotClose: a failed read of the plan
// epic, or of the trigger bead a label-bound gardener resolves its epic from,
// leaves the gardener open with nothing written so the next tick retries; only
// an epic read back as closed lets the spent budget close it.
func TestGardener_ResumeSpent_ReadFails_DoesNotClose(t *testing.T) {
	spent := func() *GardenerSpec {
		return &GardenerSpec{ResumeBudget: 2, RunsSpawned: 5, ResumesWithoutProgress: 2, LastResumeRunsSpawned: 5}
	}
	cases := []struct {
		name  string
		beads []*beadsapi.BeadDetail
		cap   func() *GardenerSpec
	}{
		{
			name:  "epic read fails",
			beads: []*beadsapi.BeadDetail{{ID: "gd-cap-1", Type: "trigger", Status: "open"}},
			cap: func() *GardenerSpec {
				c := spent()
				c.AssignBead = "gd-epic-unreadable"
				return c
			},
		},
		{
			name:  "trigger read fails for a label-bound gardener",
			beads: nil,
			cap:   spent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := &mockTriggerDaemon{beads: tc.beads}
			trig := TriggerDef{ID: "gd-cap-1", Gardener: tc.cap()}

			gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindMaxRuns)

			daemon.mu.Lock()
			defer daemon.mu.Unlock()
			if slices.Contains(daemon.closedIDs, "gd-cap-1") {
				t.Errorf("a failed read must not close the gardener, got closedIDs=%v", daemon.closedIDs)
			}
			if len(daemon.updatedIDs) != 0 {
				t.Errorf("a failed read must write nothing, got updates for %v", daemon.updatedIDs)
			}
		})
	}

	// The positive case: an epic read back as closed still escalate-closes.
	daemon := &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
		{ID: "gd-cap-1", Type: "trigger", Status: "open"},
		{ID: "gd-epic-1", Type: "epic", Status: "closed"},
	}}
	c := spent()
	c.AssignBead = "gd-epic-1"
	gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), TriggerDef{ID: "gd-cap-1", Gardener: c}, scheduler.StopKindMaxRuns)
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("an epic read back closed leaves no plan to wait on: want close, got closedIDs=%v", daemon.closedIDs)
	}
}

// ── Legacy dormancy wakes on the operator_wedge predicate (bug gd-FUC635HJ2E) ──

// TestGardener_LegacyDormant_OwnerEscalationWithOpenTasks_Wakes: a legacy-dormant
// gardener (empty DormantBlockerKind) whose plan carries an owner-at-device
// escalation and a stale in_progress executor escalation, but also has open
// unblocked tasks, must wake and fire. It used to stay parked while ANY open
// escalation/decision sat in the subtree, which froze 9 of 13 plan gardeners.
func TestGardener_LegacyDormant_OwnerEscalationWithOpenTasks_Wakes(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:           50,
		RunsSpawned:         5,
		AssignBead:          "gd-epic-1",
		DormantOnEscalation: true,
		DormantBlocker:      "gd-esc-owner",
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "plan epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-owner", Type: "escalation", Status: "open", Title: "owner: register a passkey",
			Fields: map[string]string{"audience": "owner"}},
		&beadsapi.BeadDetail{ID: "gd-esc-stale", Type: "escalation", Status: "in_progress", Title: "merge an already-merged PR"},
		&beadsapi.BeadDetail{ID: "gd-task-1", Type: "task", Status: "open", Title: "blocked by stale escalation", Labels: []string{"project:monorepo"}},
		&beadsapi.BeadDetail{ID: "gd-task-2", Type: "task", Status: "open", Title: "actionable", Labels: []string{"project:monorepo"}},
		&beadsapi.BeadDetail{ID: "gd-task-3", Type: "task", Status: "open", Title: "actionable", Labels: []string{"project:monorepo"}},
	)
	daemon.parentOf = map[string]string{
		"gd-esc-owner": "gd-epic-1", "gd-esc-stale": "gd-epic-1",
		"gd-task-1": "gd-epic-1", "gd-task-2": "gd-epic-1", "gd-task-3": "gd-epic-1",
	}
	daemon.addedDeps = []triggerMockDep{
		{BeadID: "gd-task-1", DependsOnID: "gd-esc-stale", DepType: "blocks"},
	}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.DormantOnEscalation {
		t.Errorf("legacy-dormant gardener with actionable children must wake, stayed dormant on %q", cap.DormantBlocker)
	}
	if spawns != 1 {
		t.Errorf("woken gardener with actionable children must fire, got %d spawns", spawns)
	}
}

// TestGardener_LegacyDormant_OnlyOpenChildIsEscalation_StaysParked: the other
// half of the predicate. A legacy-dormant gardener whose only open child is the
// escalation itself has nothing to self-serve, so it stays parked: no spawn, no
// wake, no gardener-block churn.
func TestGardener_LegacyDormant_OnlyOpenChildIsEscalation_StaysParked(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:           50,
		RunsSpawned:         5,
		AssignBead:          "gd-epic-1",
		DormantOnEscalation: true,
		DormantBlocker:      "gd-esc-owner",
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "plan epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-owner", Type: "escalation", Status: "open", Title: "owner: register a passkey",
			Fields: map[string]string{"audience": "owner"}},
		&beadsapi.BeadDetail{ID: "gd-task-done", Type: "task", Status: "closed", Title: "done", Labels: []string{"project:monorepo"}},
	)
	daemon.parentOf = map[string]string{"gd-esc-owner": "gd-epic-1", "gd-task-done": "gd-epic-1"}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("gardener whose only open child is the escalation must not fire, got %d spawns", len(daemon.spawnedNames))
	}
	if slices.Contains(daemon.updatedIDs, "gd-cap-1") {
		t.Errorf("still-wedged gardener must stay parked without touching its gardener block, got an update")
	}
}

// TestGardener_PrematureStop_EscalationBesideActionableTasks_DoesNotPark: the
// park side matches the wake side. A gardener out of resume budget whose plan
// has an open escalation beside open unblocked tasks is not wedged on the
// operator, so it does not park behind the escalation; it parks on plan
// activity instead and stays open (gd-dNxyjFLGRJ).
func TestGardener_PrematureStop_EscalationBesideActionableTasks_DoesNotPark(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2,
		AssignBead:             "gd-epic-1",
		Stop:                   json.RawMessage(stopJSON),
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "plan epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-owner", Type: "escalation", Status: "open", Title: "owner: register a passkey"},
		&beadsapi.BeadDetail{ID: "gd-task-1", Type: "task", Status: "open", Title: "actionable", Labels: []string{"project:monorepo"}},
	)
	daemon.parentOf = map[string]string{"gd-esc-owner": "gd-epic-1", "gd-task-1": "gd-epic-1"}

	engine.Evaluate(context.Background())

	if cap := lastGardenerOrZero(daemon, "gd-cap-1"); cap.DormantBlockerKind != gardenerDormantKindResumeBudgetSpent || cap.DormantBlocker != "gd-epic-1" {
		t.Errorf("gardener with an actionable child must park on plan activity, not behind the escalation; got kind=%q blocker=%q",
			cap.DormantBlockerKind, cap.DormantBlocker)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("stuck plan gardener must stay open, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_Park_CommentsOnPlanEpic: when a gardener parks, the plan epic gets
// a comment naming the blocker, not just the trigger bead. The operator reads the
// plan; a park recorded only on the trigger went unnoticed for days.
func TestGardener_Park_CommentsOnPlanEpic(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:   50,
		RunsSpawned: 5,
		AssignBead:  "gd-epic-1",
	})
	daemon.beads = append(daemon.beads,
		&beadsapi.BeadDetail{ID: "gd-epic-1", Type: "epic", Status: "open", Title: "plan epic"},
		&beadsapi.BeadDetail{ID: "gd-esc-1", Type: "escalation", Status: "open", Title: "operator: seed secret"},
	)
	daemon.parentOf = map[string]string{"gd-esc-1": "gd-epic-1"}

	engine.Evaluate(context.Background())

	if cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1"); !cap.DormantOnEscalation {
		t.Fatalf("fully-wedged gardener must park, got %+v", cap)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.commentedIDs, "gd-epic-1") {
		t.Errorf("park must comment on the plan epic, got comments on %v", daemon.commentedIDs)
	}
}

// ── Standing (perpetual) gardener re-arm-in-place (bug ka-0000006241) ────────

// standingZeroProgressState is a ProgressState tail of 3 zero-score runs — the
// input that trips a no_progress_for{runs:3} loop_end. Used by the standing
// re-arm tests: after bug ka-0000004754 exempted the max_runs kind (pure churn
// for a perpetual gardener), no_progress_for is the premature stop that still
// legitimately routes a standing gardener through gardenerRearmStandingInPlace —
// and that re-arm DURABLY clears the progress window (RunsFinalizedAfter), so it
// is meaningful, not a treadmill.
const standingZeroProgressState = `{"runs":[` +
	`{"agent_id":"gd-old-1","progress_score":0},` +
	`{"agent_id":"gd-old-2","progress_score":0},` +
	`{"agent_id":"gd-old-3","progress_score":0}]}`

// TestGardener_StandingViaBlob_SpentBudget_ReArmsInPlace: a STANDING gardener
// (Standing:true in the blob) whose resume budget is spent with no open frontier
// blocker must NOT escalate + close (the give-up heuristic that reaped the live
// release-owner / delivery gardeners during quiet periods). It re-arms IN PLACE:
// stays open, refills run_budget, and RESETS ResumesWithoutProgress to 0 so it
// can never accumulate its way back to the give-up threshold. Driven via
// no_progress_for — the max_runs kind is now exempt for a standing gardener
// (ka-0000004754), so this uses the premature stop that still re-arms in place.
func TestGardener_StandingViaBlob_SpentBudget_ReArmsInPlace(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		Standing:               true,
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2, // budget already spent
		ProgressState:          json.RawMessage(standingZeroProgressState),
		Stop:                   json.RawMessage(stopJSON),
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	var flagged bool
	for _, l := range daemon.addedLabels {
		if l.BeadID == "gd-cap-1" && l.Label == gardenerStuckEscalateLabel {
			flagged = true
		}
	}
	daemon.mu.Unlock()
	if closed {
		t.Errorf("standing gardener with spent budget must re-arm in place, not close; got closedIDs=%v", daemon.closedIDs)
	}
	if flagged {
		t.Errorf("standing gardener must NOT be flagged %q (that is the give-up handle); got addedLabels=%v", gardenerStuckEscalateLabel, daemon.addedLabels)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.ResumesWithoutProgress != 0 {
		t.Errorf("standing re-arm must RESET ResumesWithoutProgress to 0 (perpetual), got %d", cap.ResumesWithoutProgress)
	}
	if cap.RunBudget <= cap.RunsSpawned {
		t.Errorf("standing re-arm must refill run_budget above RunsSpawned, got RunBudget=%d RunsSpawned=%d", cap.RunBudget, cap.RunsSpawned)
	}
}

// TestGardener_StandingViaLabel_SpentBudget_ReArmsInPlace: same as above but the
// gardener is recognized as standing via the perpetual:true label on the trigger
// bead (the live release-owner + delivery gardeners carry exactly this label, so
// the fix is retroactive with no blob edit).
func TestGardener_StandingViaLabel_SpentBudget_ReArmsInPlace(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]}}`
	daemon, engine, src, _ := gardenerTrigger(t, GardenerSpec{
		RunBudget:              1,
		RunsSpawned:            1,
		ResumeBudget:           2,
		ResumesWithoutProgress: 2,
		ProgressState:          json.RawMessage(standingZeroProgressState),
		Stop:                   json.RawMessage(stopJSON),
	})
	// Recognized as standing via the label, not the blob.
	src.triggers[0].Labels = []string{"project:gardenio", standingGardenerLabel}

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if closed {
		t.Errorf("label-recognized standing gardener must re-arm in place, not close; got closedIDs=%v", daemon.closedIDs)
	}
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.ResumesWithoutProgress != 0 {
		t.Errorf("standing re-arm must RESET ResumesWithoutProgress to 0, got %d", cap.ResumesWithoutProgress)
	}
}

// TestGardener_StandingGardener_HardStopStillCloses: the standing exemption
// covers ONLY the no-progress give-up. A terminal hard_stop (consecutive
// failures over threshold) must still close a genuinely broken standing gardener
// — otherwise a crash-looping standing gardener would spin forever.
func TestGardener_StandingGardener_HardStopStillCloses(t *testing.T) {
	stopJSON := `{"hard_stop":{"any":[{"kind":"consecutive_failures","value":"3"}]},"loop_end":{"any":[{"kind":"max_runs"}]}}`
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		Standing:            true,
		RunBudget:           10,
		RunsSpawned:         1,
		ConsecutiveFailures: 3, // at the hard_stop threshold
		Stop:                json.RawMessage(stopJSON),
	})
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("standing gardener over consecutive_failures hard_stop must still close, got closedIDs=%v", daemon.closedIDs)
	}
}

// ── standing gardener designed wall_clock age-out (bug ka-0000004573) ───────
//
// A STANDING gardener hitting its DESIGNED wall_clock hard_stop ceiling (720h)
// is an EXPECTED, recurring lifecycle event, not a break. Closing it silently —
// with no auto-successor and no alert — took the release lane dark ~18h with 86
// undelivered commits (the v2->v3->v4 titles are a human-driven successor chain
// that only advances when an operator notices). The fix renews the wall_clock
// window IN PLACE: the SAME bead stays open + enabled with a fresh baseline, so
// the lane never goes dark on the ceiling. consecutive_failures hard_stop still
// closes a genuinely broken standing gardener (that test lives above).

// standingWallClockTrigger builds a standing gardener whose ONLY stop is a
// wall_clock hard_stop, aged past both the grandfather epoch and its window so
// the wall_clock kind fires. Recognized as standing via Standing:true.
func standingWallClockTrigger(t *testing.T, cap GardenerSpec, createdAt time.Time) (*mockTriggerDaemon, *TriggerEngine) {
	t.Helper()
	cap.Standing = true
	return gardenerTriggerWithCreatedAt(t, cap, createdAt)
}

// TestGardener_StandingWallClock_RenewsInPlace_NotClose is the core regression
// for bug ka-0000004573: a standing gardener that ages out on its designed 720h
// wall_clock hard_stop must NOT close (going the lane dark) — it renews the
// window in place, staying open + enabled, and records the renewal.
func TestGardener_StandingWallClock_RenewsInPlace_NotClose(t *testing.T) {
	// Epoch 1000h in the past so a 720h window measured from it has elapsed for
	// a bead created 800h ago (past both epoch and window).
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Now().Add(-1000*time.Hour).UTC())

	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"720h"}]}}`
	daemon, engine := standingWallClockTrigger(t, GardenerSpec{
		RunBudget:   555,
		RunsSpawned: 1771, // far past the run-count ceiling, like the live lane
		Stop:        json.RawMessage(stopJSON),
	}, time.Now().Add(-800*time.Hour))

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	commented := slices.Contains(daemon.commentedIDs, "gd-cap-1")
	spawned := len(daemon.spawnedNames)
	daemon.mu.Unlock()

	if closed {
		t.Errorf("standing gardener aging out on its DESIGNED wall_clock hard_stop must NOT close (that takes the lane dark, ka-0000004573); got closedIDs=%v", daemon.closedIDs)
	}
	if !commented {
		t.Errorf("wall_clock renewal must be commented on the bead (the operator-visible signal); got commentedIDs=%v", daemon.commentedIDs)
	}
	// The gardener skips the fire THIS cycle (the stop matched) but is renewed, not
	// closed — so it fires on the next reconcile with a fresh window.
	if spawned != 0 {
		t.Errorf("expected no spawn on the tick the wall_clock stop matched, got %d", spawned)
	}
}

// TestGardener_StandingWallClock_RenewAdvancesBaselineAndRefills asserts the
// renewal is durable: WallClockSince is advanced to ~now (a genuinely fresh
// window) and RunBudget is refilled above RunsSpawned so the gardener does not
// immediately re-stop on max_runs.
func TestGardener_StandingWallClock_RenewAdvancesBaselineAndRefills(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Now().Add(-1000*time.Hour).UTC())

	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"720h"}]}}`
	daemon, engine := standingWallClockTrigger(t, GardenerSpec{
		RunBudget:   555,
		RunsSpawned: 1771,
		Stop:        json.RawMessage(stopJSON),
	}, time.Now().Add(-800*time.Hour))

	before := time.Now()
	engine.Evaluate(context.Background())

	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.WallClockSince.Before(before) {
		t.Errorf("renewal must advance WallClockSince to ~now (fresh window), got %s (before=%s)", cap.WallClockSince, before)
	}
	if cap.RunBudget <= cap.RunsSpawned {
		t.Errorf("renewal must refill run_budget above RunsSpawned, got RunBudget=%d RunsSpawned=%d", cap.RunBudget, cap.RunsSpawned)
	}
}

// TestGardener_NonStandingWallClock_StillCloses guards the scope of the
// exemption: a NON-standing gardener's wall_clock is a real terminal ceiling and
// must still close. Only perpetual gardeners renew in place.
func TestGardener_NonStandingWallClock_StillCloses(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Now().Add(-1000*time.Hour).UTC())

	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"720h"}]}}`
	// NOT standing (Standing defaults false, no perpetual label).
	daemon, engine := gardenerTriggerWithCreatedAt(t, GardenerSpec{
		RunBudget:   10,
		RunsSpawned: 1,
		Stop:        json.RawMessage(stopJSON),
	}, time.Now().Add(-800*time.Hour))

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("non-standing gardener over its wall_clock hard_stop must still close, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_StandingWallClock_RenewThenFires is the end-to-end proof the lane
// recovers rather than going dark: the first tick past the wall_clock ceiling
// RENEWS in place (skip + no close, WallClockSince advanced to now); the next
// tick then measures a fresh 720h window from now — which has NOT elapsed — so
// the gardener fires normally instead of re-stopping. The pre-fix behavior closed
// the gardener on the first matching tick, ending the lane.
func TestGardener_StandingWallClock_RenewThenFires(t *testing.T) {
	scheduler.SetWallClockGrandfatherEpochForTest(t, time.Now().Add(-1000*time.Hour).UTC())

	stopJSON := `{"hard_stop":{"any":[{"kind":"wall_clock","duration":"720h"}]}}`
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "standing gardener", CreatedAt: time.Now().Add(-800 * time.Hour)}},
	}
	trig := TriggerDef{
		ID:        "gd-cap-1",
		Project:   "gardenio",
		Action:    TriggerActionSpawnAgent,
		Labels:    []string{"project:gardenio", standingGardenerLabel},
		CreatedAt: time.Now().Add(-800 * time.Hour),
		Gardener: &GardenerSpec{
			RunBudget:   555,
			RunsSpawned: 1771,
			Stop:        json.RawMessage(stopJSON),
		},
	}

	// Tick 1: wall_clock ceiling matches → renew in place (skip fire, no close).
	skip, reason := gardenerPreFireCheck(context.Background(), daemon, nil, trig)
	if !skip || reason != gardenerSkipStopMatched {
		t.Fatalf("tick 1: expected the wall_clock stop to match and skip the fire, got skip=%v reason=%q", skip, reason)
	}

	// Tick 2: the renewal advanced WallClockSince to now, so a fresh 720h window
	// has NOT elapsed — the stop no longer matches and the gardener proceeds to
	// fire (no skip, no close).
	skip2, reason2 := gardenerPreFireCheck(context.Background(), daemon, nil, trig)
	if skip2 && reason2 == gardenerSkipStopMatched {
		t.Errorf("tick 2: after renewal the fresh wall_clock window must NOT re-match; got skip=%v reason=%q", skip2, reason2)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("standing gardener must NEVER close on its designed wall_clock (ka-0000004573), got closedIDs=%v", daemon.closedIDs)
	}
}

// ── perpetual run_budget churn (bug ka-0000004754) ─────────────────────────

// TestGardener_Standing_RunBudgetGate1_ExemptNoChurn is the direct-gate (gate 1)
// regression for bug ka-0000004754. A standing gardener sitting AT the run_budget
// ceiling (runs_spawned >= run_budget) must be EXEMPT from the run_budget gate:
// the gate must NOT skip the fire, must NOT comment, and must NOT re-arm — the
// gardener falls through to fire like any un-throttled gardener. Before the fix
// the gate tripped every cycle, routing to gardenerRearmStandingInPlace which
// appended an identical re-arm comment on every lap (33+ on a prod lane).
func TestGardener_Standing_RunBudgetGate1_ExemptNoChurn(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "standing gardener"}},
	}
	trig := TriggerDef{
		ID:      "gd-cap-1",
		Project: "gardenio",
		Action:  TriggerActionSpawnAgent,
		Labels:  []string{"project:gardenio", standingGardenerLabel},
		Gardener: &GardenerSpec{
			// AT the ceiling; NO stop spec, so ONLY gate 1 is in play.
			RunBudget:   350,
			RunsSpawned: 350,
		},
	}

	skip, reason := gardenerPreFireCheck(context.Background(), daemon, nil, trig)
	if skip {
		t.Errorf("standing gardener at run_budget ceiling must NOT skip the fire (gate 1 exempt), got skip=true reason=%q", reason)
	}
	if reason == gardenerSkipRunBudget {
		t.Errorf("standing gardener must not trip the run_budget gate (ka-0000004754), got reason=%q", reason)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("standing gardener at run_budget ceiling must NOT churn a re-arm comment, got commentedIDs=%v", daemon.commentedIDs)
	}
	if len(daemon.updatedIDs) != 0 {
		t.Errorf("standing gardener at run_budget ceiling must NOT re-arm (no field writes), got updatedIDs=%v", daemon.updatedIDs)
	}
}

// TestGardener_Standing_MaxRunsGate3_ExemptNoChurn is the stop-spec (gate 3)
// regression. The LIVE standing gardeners declare exactly
// loop_end:{max_runs} + hard_stop:{consecutive_failures,wall_clock} — so even
// with gate 1 exempt, the run-count ceiling would still trip through the
// declarative stop spec's max_runs kind and re-arm-churn there. Assert the
// stop-spec max_runs is a no-op for a standing gardener: no skip, no comment.
func TestGardener_Standing_MaxRunsGate3_ExemptNoChurn(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]},"hard_stop":{"any":[{"kind":"consecutive_failures","value":"5"},{"kind":"wall_clock","duration":"720h"}]}}`
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "standing gardener"}},
	}
	trig := TriggerDef{
		ID:      "gd-cap-1",
		Project: "gardenio",
		Action:  TriggerActionSpawnAgent,
		Labels:  []string{"project:gardenio", standingGardenerLabel},
		Gardener: &GardenerSpec{
			RunBudget:   350,
			RunsSpawned: 350, // AT the ceiling
			Stop:        json.RawMessage(stopJSON),
		},
	}

	skip, reason := gardenerPreFireCheck(context.Background(), daemon, nil, trig)
	if skip && reason == gardenerSkipStopMatched {
		t.Errorf("standing gardener must NOT trip loop_end:max_runs via the stop spec (gate 3, ka-0000004754), got skip=true reason=%q", reason)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("standing gardener at max_runs ceiling must NOT churn a re-arm comment via gate 3, got commentedIDs=%v", daemon.commentedIDs)
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("standing gardener at max_runs ceiling must NOT close, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_Standing_ManyTicksAtCeiling_NoCommentTreadmill is the end-to-end
// proof the churn TREADMILL is dead (bug ka-0000004754). It runs the pre-fire
// gate many times against a standing gardener pinned at the run-count ceiling —
// exactly the ~30s-floor reconcile loop that walked ka-0000001510 to 33 re-arm
// comments — and asserts ZERO comments accumulate across all the ticks. The
// pre-fix behavior would have appended one comment per tick.
func TestGardener_Standing_ManyTicksAtCeiling_NoCommentTreadmill(t *testing.T) {
	stopJSON := `{"loop_end":{"any":[{"kind":"max_runs"}]},"hard_stop":{"any":[{"kind":"consecutive_failures","value":"5"},{"kind":"wall_clock","duration":"720h"}]}}`
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "standing gardener"}},
	}
	trig := TriggerDef{
		ID:      "gd-cap-1",
		Project: "gardenio",
		Action:  TriggerActionSpawnAgent,
		Labels:  []string{"project:gardenio", standingGardenerLabel},
		Gardener: &GardenerSpec{
			RunBudget:   350,
			RunsSpawned: 350,
			Stop:        json.RawMessage(stopJSON),
		},
	}

	const ticks = 40 // more than the 33 comments observed on the prod lane
	for range ticks {
		gardenerPreFireCheck(context.Background(), daemon, nil, trig)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	commentCount := 0
	for _, id := range daemon.commentedIDs {
		if id == "gd-cap-1" {
			commentCount++
		}
	}
	if commentCount != 0 {
		t.Errorf("standing gardener at ceiling must NOT accumulate re-arm comments across reconcile ticks (ka-0000004754 treadmill); got %d comments over %d ticks", commentCount, ticks)
	}
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("standing gardener must never close at the run-count ceiling, got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_NonStanding_RunBudgetGate_StillReArms guards the blast radius of
// the exemption (ka-0000004754): a NON-standing, epic-bounded gardener at the
// run_budget ceiling must keep the existing resume-then-escalate behavior —
// skip the fire, re-arm, and comment. The exemption is scoped strictly to the
// standing flag (ka-0000006241 must not regress).
func TestGardener_NonStanding_RunBudgetGate_StillReArms(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "epic-bounded gardener"}},
	}
	trig := TriggerDef{
		ID:      "gd-cap-1",
		Project: "gardenio",
		Action:  TriggerActionSpawnAgent,
		Labels:  []string{"project:gardenio"}, // NOT perpetual
		Gardener: &GardenerSpec{
			RunBudget:    2,
			RunsSpawned:  2,
			ResumeBudget: 5, // budget available -> re-arm, not escalate
		},
	}

	skip, reason := gardenerPreFireCheck(context.Background(), daemon, nil, trig)
	if !skip || reason != gardenerSkipRunBudget {
		t.Fatalf("non-standing gardener at run_budget ceiling must trip the gate, got skip=%v reason=%q", skip, reason)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("non-standing gardener re-arm must still comment, got commentedIDs=%v", daemon.commentedIDs)
	}
}

// TestGardenerIsStanding covers the discriminator directly: blob flag OR label.
func TestGardenerIsStanding(t *testing.T) {
	cases := []struct {
		name string
		trig TriggerDef
		want bool
	}{
		{"neither", TriggerDef{Gardener: &GardenerSpec{}}, false},
		{"blob flag", TriggerDef{Gardener: &GardenerSpec{Standing: true}}, true},
		{"label only", TriggerDef{Gardener: &GardenerSpec{}, Labels: []string{"role:gardener-delivery", standingGardenerLabel}}, true},
		{"nil gardener + label", TriggerDef{Labels: []string{standingGardenerLabel}}, true},
		{"nil gardener no label", TriggerDef{}, false},
		{"unrelated labels", TriggerDef{Gardener: &GardenerSpec{}, Labels: []string{"project:gardenio", "trigger:multi"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gardenerIsStanding(tc.trig); got != tc.want {
				t.Errorf("gardenerIsStanding(%+v) = %v, want %v", tc.trig, got, tc.want)
			}
			// The exported wrapper (bug ka-0000006238) MUST agree with the
			// unexported discriminator on every input — it is the single source
			// of truth beads-viewer consults to suppress the budget-burn meter.
			if got := GardenerIsStanding(tc.trig); got != tc.want {
				t.Errorf("GardenerIsStanding(%+v) = %v, want %v", tc.trig, got, tc.want)
			}
		})
	}
}

// ── rwp gating on a real run (bug ka-0000002690) ───────────────────────────

// noFieldsLabelWrites asserts NO UpdateBeadFields/Typed call for beadID carried
// a "labels" key. That shape puts "labels" inside the custom-fields blob, which
// the closed type:trigger schema rejects with 400 "unknown field" — the exact
// silent failure that made every escalated gardener invisible to the resumer
// watchdog (bug ka-0000007275). Labels must go through AddLabel instead.
func noFieldsLabelWrites(t *testing.T, daemon *mockTriggerDaemon, beadID string) {
	t.Helper()
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := range daemon.updatedIDs {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		if _, ok := daemon.updatedFields[i]["labels"]; ok {
			t.Errorf("labels must NEVER be written through the fields PATCH (schema rejects the whole write, ka-0000007275); got fields=%v", daemon.updatedFields[i])
		}
	}
}

// TestGardener_ResumeOrEscalate_BareReconcileDoesNotClimbRWP is the core
// regression for bug ka-0000002690 (SHARPER FINDING): the engine calls
// gardenerResumeOrEscalate from the pre-fire stop gate on EVERY reconcile tick a
// premature stop matches (~30s floor). A sticky condition (no_progress_for, with
// no new run between ticks) must NOT bump ResumesWithoutProgress on each bare
// re-evaluation — otherwise a re-armed gardener burns its whole resume budget in
// minutes and re-escalates before the next cron tick can spawn a real agent.
//
// Drives gardenerResumeOrEscalate directly across many bare reconciles (RunsSpawned
// fixed at the snapshot, i.e. no run completed since the last counted resume) and
// asserts rwp stays put.
func TestGardener_ResumeOrEscalate_BareReconcileDoesNotClimbRWP(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener"},
		},
	}
	// Already re-armed once: rwp=1, snapshot taken at RunsSpawned=3, budget has
	// been refilled. The gardener has not fired again (RunsSpawned still 3).
	cap := &GardenerSpec{
		ResumeBudget:           5,
		RunsSpawned:            3,
		RunBudget:              13,
		ResumesWithoutProgress: 1,
		LastResumeReason:       scheduler.StopKindNoProgressFor,
		LastResumeRunsSpawned:  3,
	}
	trig := TriggerDef{ID: "gd-cap-1", Gardener: cap}

	// 20 bare reconciles, no new run in between.
	for range 20 {
		gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindNoProgressFor)
	}

	if cap.ResumesWithoutProgress != 1 {
		t.Fatalf("bare reconciles must not climb ResumesWithoutProgress; got %d, want 1", cap.ResumesWithoutProgress)
	}
	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	commented := slices.Contains(daemon.commentedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if closed {
		t.Errorf("bare reconciles must not escalate-close the gardener; got closedIDs containing gd-cap-1")
	}
	if commented {
		t.Errorf("bare reconciles must not churn a re-arm comment every tick; got a comment on gd-cap-1")
	}
}

// TestGardener_ResumeOrEscalate_NewRunCountsAsFruitlessResume is the
// complement: when a REAL run has completed since the last resume (RunsSpawned
// advanced past LastResumeRunsSpawned) and it still produced no progress, the
// resume IS counted — ResumesWithoutProgress climbs by exactly one and the
// snapshot advances. This proves the gate credits genuine fruitless runs.
func TestGardener_ResumeOrEscalate_NewRunCountsAsFruitlessResume(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener"},
		},
	}
	// Re-armed once at RunsSpawned=3; the gardener has since fired two more real
	// runs (RunsSpawned=5) that produced no progress.
	cap := &GardenerSpec{
		ResumeBudget:           5,
		RunsSpawned:            5,
		RunBudget:              13,
		ResumesWithoutProgress: 1,
		LastResumeReason:       scheduler.StopKindNoProgressFor,
		LastResumeRunsSpawned:  3,
	}
	trig := TriggerDef{ID: "gd-cap-1", Gardener: cap}

	gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindNoProgressFor)

	if cap.ResumesWithoutProgress != 2 {
		t.Fatalf("a real run with no progress must count one fruitless resume; got rwp=%d, want 2", cap.ResumesWithoutProgress)
	}
	if cap.LastResumeRunsSpawned != 5 {
		t.Errorf("snapshot must advance to current RunsSpawned on a counted resume; got %d, want 5", cap.LastResumeRunsSpawned)
	}
	// And a SUBSEQUENT bare reconcile (no further run) must not climb again.
	gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindNoProgressFor)
	if cap.ResumesWithoutProgress != 2 {
		t.Fatalf("bare reconcile after a counted resume must not climb rwp; got %d, want 2", cap.ResumesWithoutProgress)
	}
}

// TestGardener_ResumeOrEscalate_BudgetSpentEscalatesImmediately: the escalate
// decision is NOT gated on a new run. Once the budget is genuinely spent
// (rwp>=budget), the very next premature stop escalates — closing the gardener,
// stamping gardener_stuck_escalated=true AND applying the gardener-stuck-escalate
// label (Gap 1: the label is what the resumer watchdog enumerates by). This is
// the explicit label-coverage test the bead asks for.
func TestGardener_ResumeOrEscalate_BudgetSpentEscalatesImmediately(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener", Labels: []string{"project:monorepo"}},
		},
	}
	cap := &GardenerSpec{
		ResumeBudget:           2,
		RunsSpawned:            5,
		ResumesWithoutProgress: 2, // budget already spent
		LastResumeRunsSpawned:  5, // no new run since last resume — must NOT block escalation
	}
	trig := TriggerDef{ID: "gd-cap-1", Gardener: cap}

	gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindMaxRuns)

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()
	if !closed {
		t.Fatalf("budget spent must escalate-close immediately even with no new run; closedIDs missing gd-cap-1")
	}
	if labels := daemon.labelsAddedFor("gd-cap-1"); !slices.Contains(labels, gardenerStuckEscalateLabel) {
		t.Errorf("escalate must apply the %q label via AddLabel for the resumer watchdog; got labels=%v", gardenerStuckEscalateLabel, labels)
	}
	// The label must land as a REAL label, never as a "labels" custom field —
	// the type:trigger schema rejects that whole PATCH (ka-0000007275).
	noFieldsLabelWrites(t, daemon, "gd-cap-1")
}

// TestGardener_Escalate_LabelWriteFails_DoesNotClose is the acceptance test for
// bug ka-0000007275: an escalate-closed gardener must ALWAYS be discoverable by
// the resumer watchdog. When the gardener-stuck-escalate label write fails, the
// escalate must NOT close the gardener — closing without the label strands it
// invisibly (the merge-lane incident: ka-0000001510 closed with
// gardener_stuck_escalated=true but no label and no comment, so no operator
// decision was ever opened and the lane was dark for 3 days). Leaving the
// gardener open lets the next reconcile tick retry the whole escalate.
func TestGardener_Escalate_LabelWriteFails_DoesNotClose(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener", Labels: []string{"project:monorepo"}},
		},
		addLabelErr: fmt.Errorf("simulated label endpoint failure"),
	}
	cap := &GardenerSpec{
		ResumeBudget:           2,
		RunsSpawned:            5,
		ResumesWithoutProgress: 2, // budget spent — escalate branch
		LastResumeRunsSpawned:  5,
	}
	trig := TriggerDef{ID: "gd-cap-1", Gardener: cap}

	gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindMaxRuns)

	daemon.mu.Lock()
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.addLabelErr = nil // label endpoint recovers for the retry below
	daemon.mu.Unlock()
	if closed {
		t.Fatalf("label write failed — gardener must stay OPEN for the next reconcile to retry the escalate, not close into the void")
	}

	// And the retry works: with the endpoint recovered, the next premature stop
	// escalate-closes WITH the label.
	gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindMaxRuns)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if !slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("retry after label endpoint recovered must escalate-close")
	}
	labeled := false
	for _, l := range daemon.addedLabels {
		if l.BeadID == "gd-cap-1" && l.Label == gardenerStuckEscalateLabel {
			labeled = true
		}
	}
	if !labeled {
		t.Errorf("retry must stamp %q via AddLabel; got %v", gardenerStuckEscalateLabel, daemon.addedLabels)
	}
}

// TestGardener_CredCrashloop_LabelWriteFails_DoesNotClose mirrors the
// label-before-close contract on the credential-crashloop escalate path
// (ka-0000007275): a failed label write leaves the gardener open (the crashloop
// gate keeps suppressing spawns, so nothing fires meanwhile) and the next
// reconcile retries the escalate.
func TestGardener_CredCrashloop_LabelWriteFails_DoesNotClose(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{
		RunsSpawned: 3,
		RunLog:      credRunLogJSON(t, "not_logged_in", "not_logged_in", "not_logged_in"),
	})
	daemon.mu.Lock()
	daemon.addLabelErr = fmt.Errorf("simulated label endpoint failure")
	daemon.mu.Unlock()

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("label write failed — cred-crashloop escalate must not close the gardener into the void")
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("cred-crashloop gate must still suppress the spawn while escalate retries, got %d spawns", len(daemon.spawnedNames))
	}
}

// TestGardener_FirstReArm_StampsResumeSnapshot: the FIRST re-arm on a freshly
// stuck gardener (LastResumeReason empty) must take the counting branch — it
// records the resume and stamps LastResumeRunsSpawned so subsequent bare
// reconciles are recognised as "no new run". Guards against the bare-reconcile
// guard accidentally swallowing the legitimate first resume.
func TestGardener_FirstReArm_StampsResumeSnapshot(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener"},
		},
	}
	cap := &GardenerSpec{
		ResumeBudget: 5,
		RunsSpawned:  4,
		RunBudget:    4, // exhausted
	}
	trig := TriggerDef{ID: "gd-cap-1", Gardener: cap}

	gardenerResumeOrEscalate(context.Background(), daemon, slog.Default(), trig, scheduler.StopKindMaxRuns)

	if cap.ResumesWithoutProgress != 1 {
		t.Fatalf("first re-arm must count one resume; got rwp=%d, want 1", cap.ResumesWithoutProgress)
	}
	if cap.LastResumeRunsSpawned != 4 {
		t.Errorf("first re-arm must stamp the resume snapshot; got %d, want 4", cap.LastResumeRunsSpawned)
	}
	if cap.RunBudget <= cap.RunsSpawned {
		t.Errorf("first re-arm must refill run budget above RunsSpawned; got RunBudget=%d RunsSpawned=%d", cap.RunBudget, cap.RunsSpawned)
	}
}
