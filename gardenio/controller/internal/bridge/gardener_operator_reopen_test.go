package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// reopenedGardenerTrigger builds a spawn_agent gardener trigger shaped like an
// out-of-band operator re-arm (bug ka-0000004191): the bead is OPEN but still
// carries the terminate/escalate markers, and the gardener blob carries the
// stale post-escalate counters (rwp at budget, exhausted run_budget, sticky
// no_progress_for resume marker) that made the bead-level reopen structurally
// insufficient — exactly the live shape of triage-intake ka-0000003783.
func reopenedGardenerTrigger(t *testing.T, cap GardenerSpec) (*mockTriggerDaemon, *TriggerEngine, TriggerDef) {
	t.Helper()
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "reopened gardener"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID:            "gd-cap-1",
		Title:         "reopened gardener",
		Source:        "mock",
		Action:        TriggerActionSpawnAgent,
		Project:       "monorepo",
		Role:          "crew",
		Prompt:        "run",
		MaxConcurrent: 1,
		LastChecked:   pastLastChecked(),
		Gardener:      &cap,
		// The out-of-band-reopen fingerprint: OPEN bead + terminate markers.
		GardenerTerminatedSlot: scheduler.StopSlotLoopEnd,
		GardenerTerminatedKind: scheduler.StopKindMaxRuns,
		GardenerStuckEscalated: true,
	}
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{
			"gd-cap-1": {{Key: "M1", Title: "match one"}},
		},
	}
	engine.RegisterSource(src)
	return daemon, engine, trig
}

// staleEscalatedGardener is the gardener-blob state an escalate-close leaves
// behind: resume budget spent, run_budget exhausted, no_progress_for resume
// marker stuck at the RunsSpawned snapshot, and a barren ProgressState tail.
func staleEscalatedGardener() GardenerSpec {
	staleProgress := `{"runs":[` +
		`{"agent_id":"gd-old-1","progress_score":0},` +
		`{"agent_id":"gd-old-2","progress_score":0},` +
		`{"agent_id":"gd-old-3","progress_score":0}]}`
	return GardenerSpec{
		RunBudget:              10,
		RunsSpawned:            10,
		ResumeBudget:           5,
		ResumesWithoutProgress: 5,
		LastResumeReason:       scheduler.StopKindMaxRuns,
		LastResumeRunsSpawned:  10,
		AssignBead:             "gd-epic-1",
		Progress:               json.RawMessage(progressSpecWithSurface),
		ProgressState:          json.RawMessage(staleProgress),
		Stop:                   json.RawMessage(`{"loop_end":{"any":[{"kind":"max_runs"},{"kind":"no_progress_for","runs":3}]}}`),
	}
}

// TestGardener_OperatorReopen_RearmsAndFires is the end-to-end acceptance test
// for bug ka-0000004191: an operator bead-level re-arm (reopen + enable, stale
// escalate counters intact) must actually make the gardener fire again in the
// SAME evaluation cycle. Before the fix the pre-fire gates read the stale
// counters — rwp already at budget — and escalate-CLOSED the gardener within
// one reconcile, silently reversing the operator's re-arm.
func TestGardener_OperatorReopen_RearmsAndFires(t *testing.T) {
	daemon, engine, _ := reopenedGardenerTrigger(t, staleEscalatedGardener())

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	spawned := len(daemon.spawnedNames)
	closed := slices.Contains(daemon.closedIDs, "gd-cap-1")
	daemon.mu.Unlock()

	if closed {
		t.Fatalf("operator re-arm must not be reversed by an escalate-close; got closedIDs containing gd-cap-1")
	}
	if spawned != 1 {
		t.Fatalf("re-armed gardener must fire this cycle, got %d spawns", spawned)
	}

	// The persisted gardener blob carries the full durable reset.
	cap := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if cap.ResumesWithoutProgress != 0 {
		t.Errorf("ResumesWithoutProgress: got %d, want 0", cap.ResumesWithoutProgress)
	}
	if cap.LastResumeReason != "" {
		t.Errorf("LastResumeReason: got %q, want cleared", cap.LastResumeReason)
	}
	if cap.LastResumeRunsSpawned != 0 {
		t.Errorf("LastResumeRunsSpawned: got %d, want 0", cap.LastResumeRunsSpawned)
	}
	if cap.RunsFinalizedAfter.IsZero() {
		t.Errorf("RunsFinalizedAfter must be stamped so finalize cannot rebuild the stale zero tail")
	}
	if cap.RunBudget <= 10 {
		t.Errorf("RunBudget must be topped up past RunsSpawned (10), got %d", cap.RunBudget)
	}

	// The terminate markers were cleared in a fields write so the
	// normalization is one-shot.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	markersCleared := false
	for i := range daemon.updatedIDs {
		if daemon.updatedIDs[i] != "gd-cap-1" {
			continue
		}
		f := daemon.updatedFields[i]
		slot, hasSlot := f["gardener_terminated_slot"]
		if hasSlot && slot == "" && f["gardener_terminated_kind"] == "" && f["gardener_stuck_escalated"] == "" {
			markersCleared = true
		}
	}
	if !markersCleared {
		t.Errorf("expected one fields write clearing all three terminate markers, got %v", daemon.updatedFields)
	}
	// An audit comment records the normalized re-arm.
	if !slices.Contains(daemon.commentedIDs, "gd-cap-1") {
		t.Errorf("expected an operator-reopen audit comment on gd-cap-1")
	}
}

// TestGardener_OperatorReopen_FinalizeHonorsFreshBaseline proves the ordering
// contract: the normalization runs BEFORE gardenerFinalizeRunsFromIterations,
// so the stale barren iterations (which predate the freshly-stamped
// RunsFinalizedAfter) are NOT rebuilt into ProgressState in the same cycle —
// the wedge the bead names ("clearing progress_state.runs is futile").
func TestGardener_OperatorReopen_FinalizeHonorsFreshBaseline(t *testing.T) {
	cap := staleEscalatedGardener()
	// The operator also manually cleared progress_state (the observed, futile
	// recovery attempt) — the immutable closed iterations below are what
	// rebuilt it before the fix.
	cap.ProgressState = nil
	daemon, engine, _ := reopenedGardenerTrigger(t, cap)
	old := time.Now().Add(-2 * time.Hour)
	daemon.beads = append(daemon.beads,
		iterationBeadAt("gd-old-1", "done", `{"accomplishments":"nothing"}`, old),
		iterationBeadAt("gd-old-2", "done", `{"accomplishments":"nothing"}`, old.Add(time.Minute)),
		iterationBeadAt("gd-old-3", "done", `{"accomplishments":"nothing"}`, old.Add(2*time.Minute)),
	)
	// Point the iterations at this gardener (helper stamps gd-cap-f).
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			b.Fields["trigger_id"] = "gd-cap-1"
		}
	}

	engine.Evaluate(context.Background())

	if maxRuns := maxProgressRunsAcrossUpdates(t, daemon, "gd-cap-1"); maxRuns != 0 {
		t.Fatalf("stale pre-reopen iterations must not be re-finalized after the reset, got %d runs", maxRuns)
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Fatalf("no_progress_for must not re-trip from rebuilt history; gardener was closed")
	}
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("re-armed gardener must fire, got %d spawns", len(daemon.spawnedNames))
	}
}

// TestGardener_OperatorReopen_StampsWallClockSince is the bridge-level proof for
// bug ka-0000006465: the operator-reopen normalization — the very path the
// gardener-loop runbook tells operators to use to un-retire a gardener — must
// stamp the per-bead wall_clock baseline (WallClockSince) to now. Without it, a
// gardener that hard-stopped on wall_clock re-trips the same expired window on
// the next reconcile (the reopen→re-close loop), so the runbook's un-retire
// procedure is silently undeliverable for the wall_clock-terminated class.
func TestGardener_OperatorReopen_StampsWallClockSince(t *testing.T) {
	// Shape a gardener that was terminated on hard_stop wall_clock: the bead is
	// OPEN (operator reopened it) but still carries the terminate markers, and
	// the blob carries a bounded run_budget + hard_stop wall_clock ceiling.
	cap := GardenerSpec{
		RunBudget:   10,
		RunsSpawned: 3,
		Stop:        json.RawMessage(`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`),
	}
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "reopened wall_clock gardener"},
		},
	}
	trig := TriggerDef{
		ID: "gd-cap-1", Gardener: &cap,
		// The wall_clock terminate fingerprint: OPEN bead + hard_stop/wall_clock
		// markers (gardenerTerminate stamps slot=hard_stop kind=wall_clock).
		GardenerTerminatedSlot: scheduler.StopSlotHardStop,
		GardenerTerminatedKind: scheduler.StopKindWallClock,
	}

	before := time.Now()
	if skip := gardenerRearmFromOperatorReopen(context.Background(), daemon, slog.Default(), trig); skip {
		t.Fatalf("wall_clock terminate markers present — must normalize, not skip")
	}

	// In-memory gardener carries the fresh baseline (so THIS cycle's pre-fire
	// stop gate measures wall_clock from the revive instant, not the expired one).
	if trig.Gardener.WallClockSince.Before(before) {
		t.Errorf("in-memory WallClockSince not advanced: got %v, want >= %v",
			trig.Gardener.WallClockSince, before)
	}

	// Persisted blob carries the stamp durably.
	got := gardenerSpecFromUpdate(t, daemon, "gd-cap-1")
	if got.WallClockSince.Before(before) {
		t.Errorf("persisted WallClockSince not advanced: got %v, want >= %v",
			got.WallClockSince, before)
	}
}

// TestGardener_OperatorReopen_NoMarkers_NoOp: a healthy open gardener (no
// terminate markers) is untouched — no reset, no comment, normal fire path.
func TestGardener_OperatorReopen_NoMarkers_NoOp(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "healthy gardener"},
		},
	}
	cap := GardenerSpec{RunBudget: 10, RunsSpawned: 3, ResumesWithoutProgress: 2,
		LastResumeReason: scheduler.StopKindMaxRuns, LastResumeRunsSpawned: 2}
	trig := TriggerDef{ID: "gd-cap-1", Gardener: &cap}

	if skip := gardenerRearmFromOperatorReopen(context.Background(), daemon, slog.Default(), trig); skip {
		t.Fatalf("no markers — must not skip")
	}
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.updatedIDs) != 0 {
		t.Errorf("no markers — must not write, got updates on %v", daemon.updatedIDs)
	}
	if cap.ResumesWithoutProgress != 2 {
		t.Errorf("no markers — resume bookkeeping must be untouched, got rwp=%d", cap.ResumesWithoutProgress)
	}
}

// TestGardener_OperatorReopen_WriteFails_SkipsCycle: when the durable reset is
// needed but cannot persist, the helper reports skip=true so evalOne bails —
// running the pre-fire gates against the stale rwp would escalate-close the
// gardener and reverse the operator's re-arm.
func TestGardener_OperatorReopen_WriteFails_SkipsCycle(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "reopened gardener"},
		},
		updateFieldsErr: errors.New("daemon down"),
	}
	cap := staleEscalatedGardener()
	trig := TriggerDef{
		ID: "gd-cap-1", Gardener: &cap,
		GardenerTerminatedSlot: scheduler.StopSlotLoopEnd,
		GardenerTerminatedKind: scheduler.StopKindMaxRuns,
		GardenerStuckEscalated: true,
	}

	if skip := gardenerRearmFromOperatorReopen(context.Background(), daemon, slog.Default(), trig); !skip {
		t.Fatalf("persist failure on a needed reset must skip the cycle")
	}
	// In-memory gardener must be untouched (no half-applied reset).
	if cap.ResumesWithoutProgress != 5 || cap.LastResumeReason != scheduler.StopKindMaxRuns {
		t.Errorf("failed persist must not mutate in-memory state, got rwp=%d reason=%q",
			cap.ResumesWithoutProgress, cap.LastResumeReason)
	}
}

// TestGardener_OperatorReopen_BlockedByOpenDecision_StaysParked proves the
// evalOne ordering keeps the blocked_by gate ahead of the normalization: a
// reopened gardener parked behind an open decision is not normalized (markers
// intact, no reset write) until the operator clears the blocker.
func TestGardener_OperatorReopen_BlockedByOpenDecision_StaysParked(t *testing.T) {
	daemon, _, _ := reopenedGardenerTrigger(t, staleEscalatedGardener())
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-open-dec", Type: "decision", Status: "open", Title: "operator decides",
	})
	// Rebuild the source with BlockedBy set (RegisterSource holds a copy).
	engine2 := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "reopened gardener", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "monorepo", Role: "crew", Prompt: "run",
		MaxConcurrent: 1, LastChecked: pastLastChecked(),
		BlockedBy:              []string{"gd-open-dec"},
		Gardener:               func() *GardenerSpec { c := staleEscalatedGardener(); return &c }(),
		GardenerTerminatedSlot: scheduler.StopSlotLoopEnd,
		GardenerTerminatedKind: scheduler.StopKindMaxRuns,
		GardenerStuckEscalated: true,
	}
	engine2.RegisterSource(&mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {{Key: "M1", Title: "match one"}}},
	})

	engine2.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := range daemon.updatedIDs {
		if daemon.updatedIDs[i] == "gd-cap-1" {
			if _, ok := daemon.updatedFields[i]["gardener_terminated_slot"]; ok {
				t.Errorf("blocked_by an open decision — markers must stay intact, got clearing write %v",
					daemon.updatedFields[i])
			}
		}
	}
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("blocked_by an open decision — must not fire, got %d spawns", len(daemon.spawnedNames))
	}
}

// TestParseTriggerDef_TerminateMarkers verifies the marker fields round-trip
// from the bead onto TriggerDef — the load-bearing plumbing for the reopen
// fingerprint.
func TestParseTriggerDef_TerminateMarkers(t *testing.T) {
	bead := &beadsapi.BeadDetail{
		ID: "gd-t1", Type: "trigger", Status: "open",
		Fields: map[string]string{
			"gardener_terminated_slot": "loop_end",
			"gardener_terminated_kind": "max_runs",
			"gardener_stuck_escalated": "true",
		},
	}
	trig := ParseTriggerDef(bead, "schedule")
	if trig.GardenerTerminatedSlot != "loop_end" {
		t.Errorf("GardenerTerminatedSlot: got %q, want loop_end", trig.GardenerTerminatedSlot)
	}
	if trig.GardenerTerminatedKind != "max_runs" {
		t.Errorf("GardenerTerminatedKind: got %q, want max_runs", trig.GardenerTerminatedKind)
	}
	if !trig.GardenerStuckEscalated {
		t.Errorf("GardenerStuckEscalated: got false, want true")
	}

	clean := ParseTriggerDef(&beadsapi.BeadDetail{ID: "gd-t2", Type: "trigger", Status: "open",
		Fields: map[string]string{}}, "schedule")
	if clean.GardenerTerminatedSlot != "" || clean.GardenerTerminatedKind != "" || clean.GardenerStuckEscalated {
		t.Errorf("clean bead must carry zero markers, got %+v", clean)
	}
}
