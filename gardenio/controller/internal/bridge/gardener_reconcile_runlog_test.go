package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/scheduler"
)

// runLogFromUpdate returns the run_log parsed off the LAST gardener
// UpdateBeadFields write for beadID. Fails the test if no gardener write happened.
func runLogFromUpdate(t *testing.T, daemon *mockTriggerDaemon, beadID string) scheduler.RunLog {
	t.Helper()
	cap := gardenerSpecFromUpdate(t, daemon, beadID)
	rl, err := scheduler.ParseRunLog(string(cap.RunLog))
	if err != nil {
		t.Fatalf("parse run_log: %v", err)
	}
	return rl
}

// TestGardener_ReconcileRunLog_ConcurrentDropHealed is the core regression guard
// for the audit finding on Phase 3b gardener ka-0000003669 (84 runs_completed but
// a 7-entry run_log): a concurrent gardener whose lone LastAgentID pointer is
// overwritten before predecessors close drops all but the last run from run_log.
// This reconciler must record a run_log entry for EVERY closed iteration bead the
// gardener owns — even though the gardener carries NO Progress spec (so the
// ProgressState finalizer never touches it).
func TestGardener_ReconcileRunLog_ConcurrentDropHealed(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
		// No Progress spec — the exact shape of Phase 3b. run_log must still fill.
		// LastAgentID intentionally empty (lone pointer overwritten).
	})
	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-1", "done", `{"accomplishments":"shipped A"}`),
		closedIterationBead("gd-iter-2", "done", `{"accomplishments":"shipped B"}`),
		closedIterationBead("gd-iter-3", "failed", `{"accomplishments":"crashed"}`),
	)

	engine.Evaluate(context.Background())

	rl := runLogFromUpdate(t, daemon, "gd-cap-f")
	if len(rl) != 3 {
		t.Fatalf("run_log: got %d entries, want 3 (one per closed iteration)", len(rl))
	}
	// Oldest-first ordering (newest-last): created order gd-iter-1..3.
	wantAgents := []string{"gd-iter-1", "gd-iter-2", "gd-iter-3"}
	for i, want := range wantAgents {
		if rl[i].Agent != want {
			t.Errorf("run_log[%d].Agent: got %q, want %q", i, rl[i].Agent, want)
		}
	}
	if rl[2].Outcome != "failed" {
		t.Errorf("run_log[2].Outcome: got %q, want failed", rl[2].Outcome)
	}
	if rl[0].WrapupSummary != "Wrap-up: shipped A" {
		t.Errorf("run_log[0].WrapupSummary: got %q, want %q", rl[0].WrapupSummary, "Wrap-up: shipped A")
	}
}

// TestGardener_ReconcileRunLog_DedupAgainstExisting confirms the reconciler credits
// each iteration at most once: an iteration already present in run_log (e.g.
// recorded earlier by gardenerAgentTracker's single-pointer fast path) is not
// double-appended. It composes with the fast path.
func TestGardener_ReconcileRunLog_DedupAgainstExisting(t *testing.T) {
	// Seed run_log with gd-iter-1 already present (as gardenerAgentTracker would).
	seed := scheduler.RunLog{}
	seed.Append(scheduler.RunLogEntry{Agent: "gd-iter-1", Outcome: "done"})
	seedJSON, err := seed.Marshal()
	if err != nil {
		t.Fatalf("seed marshal: %v", err)
	}

	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
		RunLog:     json.RawMessage(seedJSON),
	})
	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-1", "done", `{"accomplishments":"A"}`), // already recorded
		closedIterationBead("gd-iter-2", "done", `{"accomplishments":"B"}`), // new
	)

	engine.Evaluate(context.Background())

	rl := runLogFromUpdate(t, daemon, "gd-cap-f")
	if len(rl) != 2 {
		t.Fatalf("run_log: got %d, want 2 (gd-iter-1 not double-recorded)", len(rl))
	}
	seenIter1 := 0
	for _, e := range rl {
		if e.Agent == "gd-iter-1" {
			seenIter1++
		}
	}
	if seenIter1 != 1 {
		t.Errorf("gd-iter-1 recorded %d times, want exactly 1", seenIter1)
	}
}

// TestGardener_ReconcileRunLog_RecordsPhantoms confirms a never-ran phantom
// iteration (queued/expired — closed before it ever got a pod) IS recorded in
// run_log for visibility, mirroring gardenerAgentTracker (which appends its
// run_log entry too). This is the intentional divergence from the ProgressState
// finalizer, which SKIPS phantoms (scoring one 0 would falsely advance
// no_progress_for, bug ka-0000004058). run_log has no such hazard.
func TestGardener_ReconcileRunLog_RecordsPhantoms(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
	})
	daemon.beads = append(daemon.beads,
		closedIterationBead("gd-iter-ran", "done", `{"accomplishments":"ran"}`),
		closedIterationBead("gd-iter-queued", "queued", ""),   // never got a pod
		closedIterationBead("gd-iter-expired", "expired", ""), // deferred past TTL
	)

	engine.Evaluate(context.Background())

	rl := runLogFromUpdate(t, daemon, "gd-cap-f")
	if len(rl) != 3 {
		t.Fatalf("run_log: got %d, want 3 (phantoms recorded for visibility)", len(rl))
	}
	byAgent := map[string]scheduler.RunLogEntry{}
	for _, e := range rl {
		byAgent[e.Agent] = e
	}
	if byAgent["gd-iter-queued"].Outcome != "queued" {
		t.Errorf("queued phantom outcome: got %q, want queued", byAgent["gd-iter-queued"].Outcome)
	}
}

// TestGardener_ReconcileRunLog_SkipsOpenIterations confirms only CLOSED iterations
// are recorded — a still-running agent must not land a premature run_log entry.
func TestGardener_ReconcileRunLog_SkipsOpenIterations(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
	})
	closed := closedIterationBead("gd-iter-closed", "done", `{"accomplishments":"A"}`)
	running := closedIterationBead("gd-iter-running", "", "")
	running.Status = "in_progress"
	daemon.beads = append(daemon.beads, closed, running)

	engine.Evaluate(context.Background())

	rl := runLogFromUpdate(t, daemon, "gd-cap-f")
	if len(rl) != 1 || rl[0].Agent != "gd-iter-closed" {
		t.Fatalf("run_log: got %+v, want only the closed iteration", rl)
	}
}

// TestGardener_ReconcileRunLog_CapsAtMax confirms the rebuilt run_log honors the
// rolling MaxRunLogEntries bound — a gardener that has run far more than the cap
// keeps only the newest MaxRunLogEntries, matching RunLog.Append's behavior and
// the field-size budget.
func TestGardener_ReconcileRunLog_CapsAtMax(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
	})
	total := scheduler.MaxRunLogEntries + 20
	base := time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC)
	for i := range total {
		b := closedIterationBead(runLogIterID(i), "done", `{"accomplishments":"x"}`)
		b.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		b.UpdatedAt = b.CreatedAt.Add(30 * time.Second)
		daemon.beads = append(daemon.beads, b)
	}

	engine.Evaluate(context.Background())

	rl := runLogFromUpdate(t, daemon, "gd-cap-f")
	if len(rl) != scheduler.MaxRunLogEntries {
		t.Fatalf("run_log: got %d entries, want cap %d", len(rl), scheduler.MaxRunLogEntries)
	}
	// Newest-last: the final entry must be the last-created iteration, and the
	// oldest 20 must have been dropped.
	if got, want := rl[len(rl)-1].Agent, runLogIterID(total-1); got != want {
		t.Errorf("newest run_log entry: got %q, want %q", got, want)
	}
	if got, want := rl[0].Agent, runLogIterID(total-scheduler.MaxRunLogEntries); got != want {
		t.Errorf("oldest surviving run_log entry: got %q, want %q", got, want)
	}
	// DurationS is derived from CreatedAt→UpdatedAt (30s here).
	if rl[0].DurationS != 30 {
		t.Errorf("run_log[0].DurationS: got %d, want 30", rl[0].DurationS)
	}
}

// TestGardener_ReconcileRunLog_PastCap_SecondPassNoWrite is the regression guard
// for bug gd-YzUQBV7xa1: a gardener with more closed iterations than
// MaxRunLogEntries kept re-appending the ones that had fallen off the cap (they
// looked unrecorded), evicting others and rewriting the 25KB gardener blob on
// every evaluation. The first pass must keep the newest MaxRunLogEntries; a second
// pass over the same beads must make zero UpdateBeadFields calls.
func TestGardener_ReconcileRunLog_PastCap_SecondPassNoWrite(t *testing.T) {
	daemon, _ := gardenerFinalizeTrigger(t, GardenerSpec{AssignBead: "gd-epic-1"})
	total := 60
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i := range total {
		b := closedIterationBead(runLogIterID(i), "done", `{"accomplishments":"x"}`)
		b.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		b.UpdatedAt = b.CreatedAt.Add(30 * time.Second)
		daemon.beads = append(daemon.beads, b)
	}
	trig := TriggerDef{ID: "gd-cap-f", Gardener: &GardenerSpec{AssignBead: "gd-epic-1"}}
	logger := slog.Default()

	gardenerReconcileRunLogFromIterations(context.Background(), daemon, logger, trig)

	rl := runLogFromUpdate(t, daemon, "gd-cap-f")
	if len(rl) != scheduler.MaxRunLogEntries {
		t.Fatalf("run_log: got %d entries, want %d", len(rl), scheduler.MaxRunLogEntries)
	}
	if got, want := rl[0].Agent, runLogIterID(total-scheduler.MaxRunLogEntries); got != want {
		t.Errorf("oldest retained entry: got %q, want %q", got, want)
	}
	if got, want := rl[len(rl)-1].Agent, runLogIterID(total-1); got != want {
		t.Errorf("newest entry: got %q, want %q", got, want)
	}

	daemon.mu.Lock()
	writes := len(daemon.updatedIDs)
	daemon.mu.Unlock()

	// trig.Gardener.RunLog now holds the written log (mutated in place), exactly
	// what the next evaluation reads back off the bead.
	gardenerReconcileRunLogFromIterations(context.Background(), daemon, logger, trig)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if got := len(daemon.updatedIDs); got != writes {
		t.Fatalf("second pass wrote %d more time(s), want 0 (no-op once capped)", got-writes)
	}
}

// TestGardener_ReconcileRunLog_NoIterations_NoOp confirms a gardener with no closed
// iteration beads writes nothing (no spurious gardener PATCH, no empty run_log
// churn).
func TestGardener_ReconcileRunLog_NoIterations_NoOp(t *testing.T) {
	daemon, engine := gardenerFinalizeTrigger(t, GardenerSpec{
		AssignBead: "gd-epic-1",
	})
	// No iteration beads appended.

	engine.Evaluate(context.Background())

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
		if len(cap.RunLog) != 0 {
			t.Errorf("run_log should stay empty with no iterations, got %q", cap.RunLog)
		}
	}
}

// runLogIterID builds a stable iteration id for the cap test.
func runLogIterID(i int) string {
	return fmt.Sprintf("gd-rl-%03d", i)
}
