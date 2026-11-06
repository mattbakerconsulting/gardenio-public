package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// trigger_last_outcome_test.go — regression coverage for the decline-reason
// diagnostic seam (bug ka-0000001513). Before this, a trigger evaluated every
// cycle but never firing left NO externally-visible reason — only last_checked
// advanced, identically across every decline cause. These tests pin that each
// evalOne exit stamps a distinguishing last_outcome on the trigger bead.

// lastOutcomeFor returns the most-recent last_outcome value the engine stamped
// on beadID via UpdateBeadFields, plus whether any was recorded. Walks the
// recorded calls in reverse so the freshest stamp wins.
func lastOutcomeFor(daemon *mockTriggerDaemon, beadID string) (string, bool) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := len(daemon.updatedIDs) - 1; i >= 0; i-- {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		if v, ok := daemon.updatedFields[i]["last_outcome"]; ok {
			return v, true
		}
	}
	return "", false
}

// TestEvalOne_Outcome_Fired: a trigger that spawns records a positive
// last_outcome=fired, so a gardener that DID run this cycle reads healthy rather
// than carrying a stale decline reason.
func TestEvalOne_Outcome_Fired(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID: "t1", Title: "Spawn", Source: "test",
			Action: TriggerActionSpawnAgent, Role: "crew", Project: "p",
			Prompt: "do work", MaxConcurrent: 10, LastChecked: pastLastChecked(),
		}},
		matches: map[string][]TriggerMatch{"t1": {{Key: "M1", Title: "m1"}}},
	}
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	got, ok := lastOutcomeFor(daemon, "t1")
	if !ok {
		t.Fatal("expected a last_outcome stamp, got none")
	}
	if want := "fired: spawned 1"; got != want {
		t.Errorf("last_outcome = %q, want %q", got, want)
	}
}

// TestEvalOne_Outcome_NoSourceMatches: the source returns zero matches — the
// most common dark case (a schedule gardener out of window / preflight-failed).
// The outcome must name "no source matches", not the seen-set case.
func TestEvalOne_Outcome_NoSourceMatches(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID: "t1", Title: "Idle", Source: "test",
			Action: TriggerActionSpawnAgent, Role: "crew", Project: "p",
			Prompt: "do work", MaxConcurrent: 10, LastChecked: pastLastChecked(),
		}},
		matches: map[string][]TriggerMatch{"t1": {}}, // zero matches
	}
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	got, ok := lastOutcomeFor(daemon, "t1")
	if !ok {
		t.Fatal("expected a last_outcome stamp, got none")
	}
	if want := "no source matches (out of window / preflight not met / already fired)"; got != want {
		t.Errorf("last_outcome = %q, want %q", got, want)
	}
}

// TestEvalOne_Outcome_SeenSetDedup: the source DID produce a match but the
// engine's in-memory seen-set already carries its key — the exact case the
// ka-0000001513 investigation had to rule out with a live cron experiment.
// The outcome must distinguish this from "no source matches".
func TestEvalOne_Outcome_SeenSetDedup(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID: "t1", Title: "Deduped", Source: "test",
			Action: TriggerActionSpawnAgent, Role: "crew", Project: "p",
			Prompt: "do work", MaxConcurrent: 10, LastChecked: pastLastChecked(),
		}},
		matches: map[string][]TriggerMatch{"t1": {{Key: "M1", Title: "m1"}}},
	}
	// Pre-seed the seen-set so M1 is already consumed — the fire window is spent.
	engine.SeedSeen("t1", "M1")
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	got, ok := lastOutcomeFor(daemon, "t1")
	if !ok {
		t.Fatal("expected a last_outcome stamp, got none")
	}
	if want := "all 1 match(es) deduped by seen-set (window already fired)"; got != want {
		t.Errorf("last_outcome = %q, want %q", got, want)
	}
	// And crucially it must NOT spawn.
	daemon.mu.Lock()
	spawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if spawns != 0 {
		t.Errorf("expected 0 spawns on deduped window, got %d", spawns)
	}
}

// TestEvalOne_Outcome_CooldownWritesNothing: a trigger inside its cooldown
// window is skipped with ZERO field writes — the cooldown-skip path is
// deliberately side-effect-free so the out-of-band-advance detector can
// attribute any last_checked movement to a foreign evaluator (ka-0000006237).
// Cooldown is not the dark-gardener case this bead targets, so it carries no
// last_outcome stamp.
func TestEvalOne_Outcome_CooldownWritesNothing(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{{
			ID: "t1", Title: "Cool", Source: "test",
			Action: TriggerActionSpawnAgent, Role: "crew", Project: "p",
			Prompt: "do work", MaxConcurrent: 10,
			CooldownMinutes: 60,
			// last_checked just now → still inside the 60m cooldown.
			LastChecked: time.Now().UTC(),
		}},
		matches: map[string][]TriggerMatch{"t1": {{Key: "M1", Title: "m1"}}},
	}
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.updatedIDs) != 0 {
		t.Errorf("cooldown-skipped trigger must not be stamped, got %v", daemon.updatedIDs)
	}
}

// TestStampCheckedOutcome_FoldsBothFields: the folded stamp writes last_checked
// AND last_outcome/last_outcome_at in ONE patch (zero extra round-trips over the
// pre-existing per-cycle last_checked write), and an empty outcome writes only
// last_checked — the plain stampLastChecked behavior.
func TestStampCheckedOutcome_FoldsBothFields(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	ctx := context.Background()

	if err := engine.stampCheckedOutcome(ctx, "t1", "fired: spawned 2"); err != nil {
		t.Fatalf("stampCheckedOutcome: %v", err)
	}
	daemon.mu.Lock()
	if len(daemon.updatedFields) != 1 {
		daemon.mu.Unlock()
		t.Fatalf("expected exactly 1 field patch, got %d", len(daemon.updatedFields))
	}
	f := daemon.updatedFields[0]
	daemon.mu.Unlock()
	if f["last_checked"] == "" {
		t.Error("folded patch must carry last_checked")
	}
	if f["last_outcome"] != "fired: spawned 2" {
		t.Errorf("last_outcome = %q, want %q", f["last_outcome"], "fired: spawned 2")
	}
	if f["last_outcome_at"] == "" {
		t.Error("folded patch must carry last_outcome_at")
	}

	// Empty outcome (the stampLastChecked shim) writes last_checked only.
	if err := engine.stampCheckedOutcome(ctx, "t2", ""); err != nil {
		t.Fatalf("stampCheckedOutcome(empty): %v", err)
	}
	if got, ok := lastOutcomeFor(daemon, "t2"); ok {
		t.Errorf("empty outcome must not stamp last_outcome, got %q", got)
	}
}
