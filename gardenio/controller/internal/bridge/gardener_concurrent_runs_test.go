package bridge

import (
	"context"
	"log/slog"
	"slices"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Bug ka-0000003097: two gardeners ran concurrently off ONE schedule trigger. A
// late/catch-up schedule fire spawned a SECOND gardener from the same trigger
// while a prior agent from that trigger was still agent_state=working. The fire
// path gated on cooldown + open-children but NOT on "does this trigger already
// have a live agent (spawning/working)": one_spawn_at_a_time was unset and
// max_concurrent defaulted to 3, so nothing single-threaded the gardener loop.
//
// The fix adds a general single-live-instance default for gardeners
// (gardenerAtConcurrentRunsCap in gardener_evaluator.go): every gardener defaults
// to an effective cap of 1 live run per trigger, overridable via
// max_concurrent_runs, and it counts ALL live agents spawned by the trigger
// (spawned_by:trigger:<id>) rather than a single LastAgentID pointer.

// TestGardener_ConcurrentRuns_SecondTickDefersWhilePriorWorking is the
// acceptance-mandated scenario: drive two ticks with the first tick's agent left
// agent_state=working and assert only ONE spawn. The gardener does NOT set
// one_spawn_at_a_time and carries the default max_concurrent=3 — exactly the
// shape of the incident trigger gd-3Ub6rYSqUY — so before the fix the second
// tick would spawn a concurrent gardener.
func TestGardener_ConcurrentRuns_SecondTickDefersWhilePriorWorking(t *testing.T) {
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
		Project:       "gardenio",
		Role:          "gardener",
		Prompt:        "survey then dispatch",
		MaxConcurrent: 3, // the gb-gardener-start default — NOT single-threaded
		LastChecked:   pastLastChecked(),
		// one_spawn_at_a_time deliberately unset (the incident trigger's shape).
		Gardener: &GardenerSpec{RunBudget: 10, MaxRuns: 20},
	}
	// Two distinct cron windows fire across the two ticks — mirrors the incident's
	// catch-up key (2026-07-28T20:17) then on-time key (2026-07-29T20:17).
	src := &mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{
			"gd-cap-1": {{Key: "2026-07-28T20:17", Title: "catch-up window"}},
		},
	}
	engine.RegisterSource(src)

	// --- Tick 1: no live agent yet → the gardener fires once. ---
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	firstSpawns := len(daemon.spawnedNames)
	daemon.mu.Unlock()
	if firstSpawns != 1 {
		t.Fatalf("tick 1: expected exactly 1 spawn, got %d", firstSpawns)
	}

	// The first tick's agent is now live and still working (agent_state=working,
	// bead status=in_progress). The real SpawnAgent registers this bead; the mock
	// does not, so inject it to reproduce the incident's mid-cycle overlap.
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-working-agent", Type: "agent", Status: "in_progress",
		Fields: map[string]string{
			"spawned_by":  "trigger:gd-cap-1",
			"agent_state": "working",
		},
		UpdatedAt: time.Now().UTC(), // fresh → not lease-stale
	})
	daemon.mu.Unlock()

	// --- Tick 2: the late/catch-up fire for the next window. The prior agent is
	// still working, so the fire MUST defer — no second concurrent gardener. ---
	src.matches["gd-cap-1"] = []TriggerMatch{{Key: "2026-07-29T20:17", Title: "on-time window"}}
	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("tick 2: expected NO second spawn while prior agent works (total 1), got %d total spawns", len(daemon.spawnedNames))
	}
	// Deferral, not termination: the gardener trigger stays open for the next tick.
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("single-live-instance deferral must not close the gardener; got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_ConcurrentRuns_FiresWhenPriorClosed confirms the gate is a
// DEFERRAL, not a permanent stop: once the prior run terminates (bead closed),
// the next tick spawns again — sequential runs are fine, concurrent are the bug.
func TestGardener_ConcurrentRuns_FiresWhenPriorClosed(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig"},
			// A prior run that has already terminated (closed) — must NOT block.
			{ID: "gd-done-agent", Type: "agent", Status: "closed",
				Fields: map[string]string{"spawned_by": "trigger:gd-cap-1"}},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "gardener trig", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "gardenio", Role: "gardener",
		Prompt: "run", MaxConcurrent: 3, LastChecked: pastLastChecked(),
		Gardener: &GardenerSpec{RunBudget: 10, MaxRuns: 20},
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {{Key: "W1", Title: "window"}}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn when the only prior agent is closed, got %d", len(daemon.spawnedNames))
	}
}

// TestGardener_ConcurrentRuns_OverrideHonored verifies max_concurrent_runs raises
// the effective cap: with the override set to 2 and one prior agent live, a
// second concurrent run is allowed (the escape hatch for a gardener that
// genuinely wants bounded fan-out).
func TestGardener_ConcurrentRuns_OverrideHonored(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig"},
			// One prior agent live (working).
			{ID: "gd-live-1", Type: "agent", Status: "in_progress",
				Fields:    map[string]string{"spawned_by": "trigger:gd-cap-1", "agent_state": "working"},
				UpdatedAt: time.Now().UTC()},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "gardener trig", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "gardenio", Role: "gardener",
		Prompt: "run", MaxConcurrent: 3, MaxConcurrentRuns: 2, LastChecked: pastLastChecked(),
		Gardener: &GardenerSpec{RunBudget: 10, MaxRuns: 20},
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {{Key: "W1", Title: "window"}}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("with max_concurrent_runs=2 and 1 prior live agent, expected 1 new spawn, got %d", len(daemon.spawnedNames))
	}
}

// TestGardener_ConcurrentRuns_StaleHolderReleasesGate verifies the gate honors the
// lease-staleness bound: a prior agent whose updated_at is well past the lease
// threshold no longer counts, so the gardener resumes firing rather than being
// wedged forever by a hung run (symmetric with the one_spawn / max_concurrent
// gates, bugs ka-0000001095 / ka-0000002023).
func TestGardener_ConcurrentRuns_StaleHolderReleasesGate(t *testing.T) {
	stale := time.Now().UTC().Add(-6 * time.Hour) // past the 3h default lease bound
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig"},
			{ID: "gd-hung-agent", Type: "agent", Status: "in_progress",
				Fields:    map[string]string{"spawned_by": "trigger:gd-cap-1", "agent_state": "working"},
				CreatedAt: stale, UpdatedAt: stale},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "gardener trig", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "gardenio", Role: "gardener",
		Prompt: "run", MaxConcurrent: 3, LastChecked: pastLastChecked(),
		Gardener: &GardenerSpec{RunBudget: 10, MaxRuns: 20},
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {{Key: "W1", Title: "window"}}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("a hung prior agent past the lease bound must not wedge the gardener; expected 1 spawn, got %d", len(daemon.spawnedNames))
	}
}

// TestGardenerAtConcurrentRunsCap_NonGardenerIgnored confirms the gate is a no-op
// for a non-gardener trigger (Gardener==nil) — those keep the legacy
// max_concurrent live-lease behavior and are unaffected by this fix.
func TestGardenerAtConcurrentRunsCap_NonGardenerIgnored(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-live-1", Type: "agent", Status: "in_progress",
				Fields: map[string]string{"spawned_by": "trigger:gd-plain"}},
		},
	}
	trig := TriggerDef{ID: "gd-plain", Action: TriggerActionSpawnAgent} // Gardener == nil
	if gardenerAtConcurrentRunsCap(context.Background(), daemon, slog.Default(), trig, time.Now()) {
		t.Errorf("non-gardener trigger must never be gated by the gardener concurrent-runs cap")
	}
}

// TestParseTriggerDef_MaxConcurrentRuns confirms max_concurrent_runs parses off
// the trigger bead field, defaults to 0 (unset) when absent, and that
// gardenerMaxConcurrentRuns then resolves the unset value to the default cap of 1.
func TestParseTriggerDef_MaxConcurrentRuns(t *testing.T) {
	set := ParseTriggerDef(&beadsapi.BeadDetail{
		ID: "gd-a", Type: "trigger",
		Fields: map[string]string{"max_concurrent_runs": "2"},
	}, "schedule")
	if set.MaxConcurrentRuns != 2 {
		t.Errorf("max_concurrent_runs=2 should parse to 2, got %d", set.MaxConcurrentRuns)
	}
	if got := gardenerMaxConcurrentRuns(set); got != 2 {
		t.Errorf("gardenerMaxConcurrentRuns with explicit 2 = %d, want 2", got)
	}

	unset := ParseTriggerDef(&beadsapi.BeadDetail{
		ID: "gd-b", Type: "trigger", Fields: map[string]string{},
	}, "schedule")
	if unset.MaxConcurrentRuns != 0 {
		t.Errorf("absent max_concurrent_runs should parse to 0 (unset), got %d", unset.MaxConcurrentRuns)
	}
	if got := gardenerMaxConcurrentRuns(unset); got != 1 {
		t.Errorf("gardenerMaxConcurrentRuns with unset value = %d, want default 1", got)
	}
}

// TestLivenessSweep_SkipsGardenerAtConcurrentRunsCap locks in the load-bearing
// "no-stamp set == liveness skip set" invariant for the new gate: a gardener that
// deferred its fire because a prior run is still live freezes last_checked
// without being dark, so the liveness sweep MUST NOT flag it (bug ka-0000003097).
// Mirrors TestLivenessSweep_SkipsOneSpawnWithRunningAgent for the general,
// default-on gate.
func TestLivenessSweep_SkipsGardenerAtConcurrentRunsCap(t *testing.T) {
	gardener := gardenerBead("gd-mcr-1", "single-instance gardener", rfc3339Ago(10*time.Hour),
		map[string]string{"gardener": `{"name":"si"}`}) // no one_spawn_at_a_time; default cap 1
	agent := &beadsapi.BeadDetail{
		ID: "gd-live-agent", Type: "agent", Status: "in_progress", Title: "live prior run",
		Fields:    map[string]string{"spawned_by": "trigger:gd-mcr-1", "agent_state": "working"},
		UpdatedAt: time.Now().UTC(),
	}
	engine, _, rec := livenessEngine(t, true, false, gardener, agent)

	engine.SweepGardenerLiveness(context.Background())

	if rec.count() != 0 {
		t.Fatalf("a gardener deferring on the single-live-instance gate must not alert, got %d", rec.count())
	}
}

// TestGardener_ConcurrentRuns_InCycleBurstDefersExtras is the acceptance repro for
// bug ka-0000003387: an IDLE gardener (0 live) that gets a BURST of >=2 matching
// events in ONE drain cycle must still spawn only ONE agent. Before the fix the
// per-match fire loop was bounded by trig.MaxConcurrent (default 3), and the
// once-per-cycle gardenerPreFireCheck gate saw 0 live and passed — so all 3
// matches spawned concurrent gardeners in a single cycle, defeating the documented
// single-live-instance cap of 1 via the in-cycle path. The loop must instead bind
// to gardenerMaxConcurrentRuns (default 1).
func TestGardener_ConcurrentRuns_InCycleBurstDefersExtras(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "gardener trig", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "gardenio", Role: "gardener",
		Prompt: "dispatch", MaxConcurrent: 3, LastChecked: pastLastChecked(),
		// one_spawn_at_a_time unset; default effective cap 1.
		Gardener: &GardenerSpec{RunBudget: 10, MaxRuns: 20},
	}
	// Three distinct matching events land in ONE drain cycle (e.g. three MRs
	// opened near-simultaneously for a trigger:multi release/delivery gardener).
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {
			{Key: "evt-1", Title: "MR 1"},
			{Key: "evt-2", Title: "MR 2"},
			{Key: "evt-3", Title: "MR 3"},
		}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("in-cycle burst of 3 matches on a cap-1 gardener must spawn exactly 1, got %d", len(daemon.spawnedNames))
	}
	// Deferral, not termination: the gardener trigger stays open for the next tick.
	if slices.Contains(daemon.closedIDs, "gd-cap-1") {
		t.Errorf("in-cycle deferral must not close the gardener; got closedIDs=%v", daemon.closedIDs)
	}
}

// TestGardener_ConcurrentRuns_InCycleBurstOverrideFansOut confirms the in-cycle
// bound is the EFFECTIVE gardener cap, not a hard 1: with max_concurrent_runs=2 an
// idle gardener hit by a 3-event burst spawns exactly 2 (the bounded fan-out
// escape hatch), deferring the third.
func TestGardener_ConcurrentRuns_InCycleBurstOverrideFansOut(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "gardener trig", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "gardenio", Role: "gardener",
		Prompt: "dispatch", MaxConcurrent: 3, MaxConcurrentRuns: 2, LastChecked: pastLastChecked(),
		Gardener: &GardenerSpec{RunBudget: 10, MaxRuns: 20},
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {
			{Key: "evt-1", Title: "MR 1"},
			{Key: "evt-2", Title: "MR 2"},
			{Key: "evt-3", Title: "MR 3"},
		}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 2 {
		t.Fatalf("in-cycle burst with max_concurrent_runs=2 must spawn exactly 2, got %d", len(daemon.spawnedNames))
	}
}

// TestNonGardener_MaxConcurrentRuns_HonoredInCycle covers the runner-up half of
// ka-0000003387: max_concurrent_runs was documented as "honored verbatim for both
// shapes" but was DEAD for non-gardener spawn_agent triggers — the generic fire
// loop always bound to MaxConcurrent (default 3) and gardenerAtConcurrentRunsCap
// returns false when Gardener==nil. A non-gardener trigger with
// max_concurrent_runs=1 hit by a 3-event burst must now spawn exactly 1.
func TestNonGardener_MaxConcurrentRuns_HonoredInCycle(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-plain", Type: "trigger", Status: "open", Title: "plain spawn trig"},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig := TriggerDef{
		ID: "gd-plain", Title: "plain spawn trig", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "gardenio", Role: "crew",
		Prompt: "work", MaxConcurrent: 3, MaxConcurrentRuns: 1, LastChecked: pastLastChecked(),
		// Gardener == nil — a generic spawn_agent trigger.
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-plain": {
			{Key: "evt-1", Title: "e1"},
			{Key: "evt-2", Title: "e2"},
			{Key: "evt-3", Title: "e3"},
		}},
	}
	engine.RegisterSource(src)

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("non-gardener max_concurrent_runs=1 must bound an in-cycle burst to 1 spawn, got %d", len(daemon.spawnedNames))
	}
}
