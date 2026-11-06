// gd-pLf5LSHDEh: actionSpawnAgent must be idempotent per (trigger, match). A
// re-fire of the same cron match that slips past the last_match skip guard in
// ScheduleTriggerSource.Evaluate (e.g. the last_match write raced with an
// engine restart) must find the agent bead already spawned for that match
// instead of creating a duplicate. See findExistingSpawnForMatch, and the
// restart-focused catch-up test in trigger_source_schedule_test.go
// (TestScheduleTriggerSource_Restart_DoesNotRefireLastMatch), which covers
// the primary defense (last_match); this file covers the second-layer
// defense that catches whatever slips past it.

package bridge

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestActionSpawnAgent_IdempotentPerTriggerMatch drives actionSpawnAgent
// directly for the SAME (trigger, match) twice — bypassing the engine-level
// last_match skip entirely, as a genuine race would — and asserts the second
// call finds the agent bead the first call already spawned rather than
// creating a second one.
func TestActionSpawnAgent_IdempotentPerTriggerMatch(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:      "t1",
		Title:   "Test Trigger",
		Source:  "schedule",
		Action:  TriggerActionSpawnAgent,
		Project: "gardenio",
		Role:    "crew",
		Prompt:  "Work on {{fire_time}}",
	}
	m := TriggerMatch{
		Key:          "2026-09-21T08:05",
		Title:        "08:05 fire",
		TemplateVars: map[string]string{},
	}

	firstID, err := engine.actionSpawnAgent(context.Background(), trig, m)
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}

	daemon.mu.Lock()
	if len(daemon.spawnedNames) != 1 {
		daemon.mu.Unlock()
		t.Fatalf("expected 1 SpawnAgent call after first fire, got %d", len(daemon.spawnedNames))
	}
	// Mirror what the real daemon does: the just-spawned agent bead persists
	// with the exact extraFields actionSpawnAgent stamped (spawned_by,
	// schedule_fire_key), so the SECOND call's idempotency lookup
	// (findExistingSpawnForMatch) can find it via ListBeadsFiltered. The mock's
	// SpawnAgent records the call but does not create a bead on its own.
	spawnedFields := daemon.spawnedFields[0]
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     firstID,
		Type:   "agent",
		Status: "open",
		Fields: spawnedFields,
	})
	daemon.mu.Unlock()

	secondID, err := engine.actionSpawnAgent(context.Background(), trig, m)
	if err != nil {
		t.Fatalf("second spawn: %v", err)
	}

	if secondID != firstID {
		t.Errorf("second call for the same (trigger, match) returned agent %q, want the existing agent %q", secondID, firstID)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Errorf("two actionSpawnAgent calls for the same (trigger, match) produced %d SpawnAgent calls, want exactly 1 (one agent bead)", len(daemon.spawnedNames))
	}
	if len(daemon.createdBeads) != 1 {
		t.Errorf("two actionSpawnAgent calls for the same (trigger, match) created %d task beads, want exactly 1", len(daemon.createdBeads))
	}
}

// TestActionSpawnAgent_DifferentMatch_NotDeduped is the negative control: two
// DIFFERENT matches for the same trigger must both spawn, so the idempotency
// guard is scoped to (trigger, match) and not just trigger.
func TestActionSpawnAgent_DifferentMatch_NotDeduped(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:      "t1",
		Title:   "Test Trigger",
		Source:  "schedule",
		Action:  TriggerActionSpawnAgent,
		Project: "gardenio",
		Role:    "crew",
		Prompt:  "Work on {{fire_time}}",
	}

	m1 := TriggerMatch{Key: "2026-09-21T08:05", Title: "08:05 fire", TemplateVars: map[string]string{}}
	firstID, err := engine.actionSpawnAgent(context.Background(), trig, m1)
	if err != nil {
		t.Fatalf("first spawn: %v", err)
	}

	daemon.mu.Lock()
	spawnedFields := daemon.spawnedFields[0]
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID:     firstID,
		Type:   "agent",
		Status: "open",
		Fields: spawnedFields,
	})
	daemon.mu.Unlock()

	m2 := TriggerMatch{Key: "2026-09-21T09:05", Title: "09:05 fire", TemplateVars: map[string]string{}}
	_, err = engine.actionSpawnAgent(context.Background(), trig, m2)
	if err != nil {
		t.Fatalf("second spawn (different match): %v", err)
	}

	// The mock's SpawnAgent hardcodes its returned ID (gd-agent-1) regardless
	// of the call count, so a real daemon's per-call-unique ID is not
	// observable here — the actual dedup signal is the SpawnAgent call count:
	// a second, distinct match must reach a real spawn call rather than be
	// short-circuited by findExistingSpawnForMatch.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 2 {
		t.Errorf("two distinct matches produced %d SpawnAgent calls, want 2", len(daemon.spawnedNames))
	}
}
