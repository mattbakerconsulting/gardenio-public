package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// The tests in this file extend the TriggerEngine.Evaluate / evalOne /
// fireAction coverage beyond the steady-state cases already exercised in
// triggers_test.go. They focus on three behaviors named on gd-A2I4dZI2vX that
// lacked dedicated assertions: the runtime bootstrap guard, the last_checked
// write-back on every eval outcome, and the extraFields stamped onto a
// spawn_agent fire.

// findFieldUpdate returns the last fields map recorded for a given bead ID, or
// nil if the bead was never updated. Used to assert last_checked write-back.
func findFieldUpdate(d *mockTriggerDaemon, beadID string) map[string]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out map[string]string
	for i, id := range d.updatedIDs {
		if id == beadID {
			out = d.updatedFields[i]
		}
	}
	return out
}

// TestTriggerEngine_BootstrapSeedsWithoutFiring verifies the runtime-bootstrap
// guard: a brand-new trigger (LastChecked zero, no in-memory seen state) seeds
// its seen set from the current matches WITHOUT firing any action, then stamps
// last_checked so it does not re-bootstrap on the next tick. This is the
// thundering-herd protection for triggers created after engine startup.
func TestTriggerEngine_BootstrapSeedsWithoutFiring(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			// LastChecked is the zero value → bootstrap path engages.
			{ID: "t-boot", Title: "Bootstrap", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10},
		},
		matches: map[string][]TriggerMatch{
			"t-boot": {{Key: "E1"}, {Key: "E2"}},
		},
		// No catch-up keys → bootstrap seeds from live matches.
	}
	engine.RegisterSource(source)

	fired := 0
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 0 {
		t.Errorf("bootstrap should not fire any action, fired %d", fired)
	}
	if fu := findFieldUpdate(daemon, "t-boot"); fu == nil || fu["last_checked"] == "" {
		t.Errorf("bootstrap should stamp last_checked, got %v", fu)
	}
	if tr, keys := engine.SeenMapSize(); tr != 1 || keys != 2 {
		t.Errorf("bootstrap should seed 2 keys for 1 trigger, got triggers=%d keys=%d", tr, keys)
	}

	// Second tick: the same matches are now seen → still no fire. A brand-new
	// match (E3) on a later tick should fire normally.
	source.matches["t-boot"] = []TriggerMatch{{Key: "E1"}, {Key: "E2"}, {Key: "E3"}}
	// LastChecked is unset on the in-memory TriggerDef, but the in-memory seen
	// state now exists, so evalOne skips bootstrap and treats E3 as new.
	engine.Evaluate(context.Background())

	if fired != 1 {
		t.Errorf("post-bootstrap new match E3 should fire exactly once, fired %d", fired)
	}
}

// TestTriggerEngine_BootstrapCatchUpKeysSeedFromPriorBeads verifies that when a
// freshly-created trigger already has prior beads (CatchUpKeys non-empty), the
// bootstrap seeds the seen set from those keys and does not stamp last_checked
// (it returns early, leaving the trigger to evaluate normally next tick).
func TestTriggerEngine_BootstrapCatchUpKeysSeedFromPriorBeads(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t-prior", Title: "Prior", Source: "test", Action: TriggerActionNotify, MaxConcurrent: 10},
		},
		matches: map[string][]TriggerMatch{
			"t-prior": {{Key: "P1"}},
		},
		keys: map[string][]string{
			"t-prior": {"P1"},
		},
	}
	engine.RegisterSource(source)

	fired := 0
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 0 {
		t.Errorf("bootstrap-from-prior-beads should not fire, fired %d", fired)
	}
	if tr, keys := engine.SeenMapSize(); tr != 1 || keys != 1 {
		t.Errorf("expected 1 trigger / 1 seeded key from prior beads, got triggers=%d keys=%d", tr, keys)
	}
	// The prior-beads branch returns false (does not suppress), so evalOne
	// continues to normal evaluation; P1 is now seen → no new matches → the
	// idle-tick last_checked write-back still fires.
	if fu := findFieldUpdate(daemon, "t-prior"); fu == nil || fu["last_checked"] == "" {
		t.Errorf("prior-beads bootstrap falls through to no-new-matches write-back, got %v", fu)
	}
}

// TestTriggerEngine_LastCheckedWriteBackOnNoNewMatches verifies that an
// already-bootstrapped trigger that evaluates to zero new matches still writes
// last_checked back — the engine advances the cooldown clock even on idle
// ticks so a steady stream of already-seen matches doesn't re-evaluate every
// 30s indefinitely.
func TestTriggerEngine_LastCheckedWriteBackOnNoNewMatches(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			// pastLastChecked bypasses bootstrap → steady-state path.
			{ID: "t-idle", Title: "Idle", Source: "test", Action: TriggerActionNotify,
				MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t-idle": nil, // zero matches
		},
	}
	engine.RegisterSource(source)
	engine.notifier = func(_ context.Context, _, _ string, _ string) {}

	engine.Evaluate(context.Background())

	fu := findFieldUpdate(daemon, "t-idle")
	if fu == nil || fu["last_checked"] == "" {
		t.Errorf("no-new-matches path must write last_checked, got %v", fu)
	}
	// The stamped timestamp should be parseable RFC3339 in UTC.
	if ts, err := time.Parse(time.RFC3339, fu["last_checked"]); err != nil {
		t.Errorf("last_checked not RFC3339: %v", err)
	} else if time.Since(ts) > time.Minute {
		t.Errorf("last_checked should be ~now, got %v", ts)
	}
}

// TestTriggerEngine_LastCheckedWriteBackAfterFire verifies the write-back on
// the fire path: after firing at least one action the engine stamps a fresh
// last_checked so the per-trigger cooldown begins from the fire time.
func TestTriggerEngine_LastCheckedWriteBackAfterFire(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t-fire", Title: "Fire", Source: "test", Action: TriggerActionNotify,
				MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t-fire": {{Key: "F1"}},
		},
	}
	engine.RegisterSource(source)

	fired := 0
	engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }

	engine.Evaluate(context.Background())

	if fired != 1 {
		t.Fatalf("expected 1 fire, got %d", fired)
	}
	if fu := findFieldUpdate(daemon, "t-fire"); fu == nil || fu["last_checked"] == "" {
		t.Errorf("fire path must write last_checked, got %v", fu)
	}
}

// TestTriggerEngine_SpawnAgentExtraFields asserts the extraFields the engine
// stamps onto a spawn_agent fire: trigger_id / trigger_title / source /
// spawned_by, plus the sanitized per-source match-key field. These fields are
// what gb prime and the agent card key off, and a missing/hyphenated key was
// the ka-0000000883 bug — so pin them explicitly.
func TestTriggerEngine_SpawnAgentExtraFields(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	source := &mockTriggerSource{
		// Multi-word source name → field key must be sanitized to underscores.
		name: "schedule-mirror",
		triggers: []TriggerDef{
			{ID: "t-spawn", Title: "Spawn FX", Source: "schedule-mirror",
				Action: TriggerActionSpawnAgent, Role: "crew", Project: "myproj",
				Prompt: "Do {{k}}", MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t-spawn": {{Key: "SM-7", TemplateVars: map[string]string{"{{k}}": "SM-7"}}},
		},
	}
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedFields) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(daemon.spawnedFields))
	}
	f := daemon.spawnedFields[0]
	want := map[string]string{
		"trigger_id":    "t-spawn",
		"trigger_title": "Spawn FX",
		"source":        "schedule-mirror",
		"spawned_by":    "trigger:t-spawn",
	}
	for k, v := range want {
		if f[k] != v {
			t.Errorf("extraFields[%q] = %q, want %q", k, f[k], v)
		}
	}
	// triggerSourceFieldKey("schedule-mirror") → "schedule_mirror_key".
	if f["schedule_mirror_key"] != "SM-7" {
		t.Errorf("expected sanitized match-key field schedule_mirror_key=SM-7, got fields %v", f)
	}
	// The spawned task bead is the per-fire created bead, not the trigger.
	if len(daemon.spawnedTaskIDs) != 1 || daemon.spawnedTaskIDs[0] == "" {
		t.Errorf("expected a non-empty per-fire task id, got %v", daemon.spawnedTaskIDs)
	}
	if daemon.lastPrompt != "Do SM-7" {
		t.Errorf("expanded prompt = %q, want %q", daemon.lastPrompt, "Do SM-7")
	}
}

// TestTriggerEngine_SpawnAgentEmptyPromptFallback verifies the default prompt
// the engine synthesizes when a spawn_agent trigger has no Prompt template:
// "Work on <key>: <title>".
func TestTriggerEngine_SpawnAgentEmptyPromptFallback(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})

	source := &mockTriggerSource{
		name: "test",
		triggers: []TriggerDef{
			{ID: "t-np", Title: "No Prompt", Source: "test", Action: TriggerActionSpawnAgent,
				Role: "crew", Project: "p", MaxConcurrent: 10, LastChecked: pastLastChecked()},
		},
		matches: map[string][]TriggerMatch{
			"t-np": {{Key: "NP-1", Title: "fix it"}},
		},
	}
	engine.RegisterSource(source)
	engine.Evaluate(context.Background())

	if daemon.lastPrompt != "Work on NP-1: fix it" {
		t.Errorf("fallback prompt = %q, want %q", daemon.lastPrompt, "Work on NP-1: fix it")
	}
}
