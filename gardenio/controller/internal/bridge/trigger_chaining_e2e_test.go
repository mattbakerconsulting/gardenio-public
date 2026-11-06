package bridge

import (
	"context"
	"log/slog"
	"testing"
)

// TestE2E_BeadStateChaining_EpicClosedSpawnsNextGardener is the end-to-end
// proof of the choreographed gardener-chaining substrate (C0, ka-0000004826):
// a `trigger:bead` watching a specific epic for `closed` fires a `spawn_agent`
// action when that epic closes — i.e. "gardener B wakes when gardener A's epic
// closes" works on the REAL TriggerEngine, not just in isolation.
//
// Wire path exercised:
//
//	beads.bead.closed SSE event (for the watched epic)
//	  → BeadStateTriggerSource.Evaluate (event_types=closed + id-condition match)
//	  → TriggerEngine.EvaluateSource → evalOne → actionSpawnAgent
//	  → daemon.SpawnAgent (the next gardener)
//
// BeadStateTriggerSource is registered & live in prod trigger-engine/main.go
// (commit 6c18f1344, on main since 2026-05-10), so this choreography is not
// hypothetical — it fires today. This test pins that contract.
func TestE2E_BeadStateChaining_EpicClosedSpawnsNextGardener(t *testing.T) {
	const (
		watchedEpic = "gd-gardener-a-epic"
		triggerID   = "gd-trig-chain-on-close"
		project     = "gardenio"
	)

	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	// Gardener A's epic closes — the SSE substrate delivers a bead.closed event.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID:     watchedEpic,
				Type:   "epic",
				Title:  "Gardener A epic",
				Status: "closed",
				Labels: []string{"project:" + project},
			},
		},
	}
	src.mu.Unlock()

	// The chaining trigger: when THIS epic closes, spawn gardener B. The
	// id-condition is what makes it a directed A→B chain rather than a
	// fire-on-any-close — gardener B does not wake when an unrelated epic closes.
	daemon.mu.Lock()
	daemon.beads = triggerBeads(
		triggerBead(triggerID, "Chain: spawn gardener B when gardener A epic closes", "trigger:bead", map[string]string{
			"action":      "spawn_agent",
			"event_types": "closed",
			"conditions":  `[{"field":"id","op":"==","value":"` + watchedEpic + `"}]`,
			"role":        "gardener",
			"project":     project,
			"prompt":      "Gardener A epic {{bead_id}} closed — begin phase B.",
		}),
	)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// THE proof: the engine spawned the next gardener off the close event.
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected exactly 1 chained spawn on epic close, got %d (names=%v)",
			len(daemon.spawnedNames), daemon.spawnedNames)
	}
	if got, want := daemon.spawnedRoles[0], "gardener"; got != want {
		t.Errorf("chained spawn role = %q, want %q", got, want)
	}
	if got, want := daemon.lastPrompt, "Gardener A epic "+watchedEpic+" closed — begin phase B."; got != want {
		t.Errorf("chained spawn prompt = %q, want %q", got, want)
	}
	// The spawned tracking task carries triggered-by lineage back to the
	// trigger definition AND the source (the closed epic) — so the chain is
	// auditable in the bead graph.
	wantTargets := map[string]bool{triggerID: false, watchedEpic: false}
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			continue
		}
		if _, ok := wantTargets[d.DependsOnID]; ok {
			wantTargets[d.DependsOnID] = true
		}
	}
	for target, seen := range wantTargets {
		if !seen {
			t.Errorf("missing triggered-by lineage edge to %q (deps=%+v)", target, daemon.addedDeps)
		}
	}
}

// TestE2E_BeadStateChaining_UnrelatedEpicCloseDoesNotSpawn is the negative
// half of the chaining contract: a close event for an epic the trigger does
// NOT watch must not fire the spawn. Without this, the id-condition above
// could be a no-op and every close would spuriously wake gardener B.
func TestE2E_BeadStateChaining_UnrelatedEpicCloseDoesNotSpawn(t *testing.T) {
	const (
		watchedEpic = "gd-gardener-a-epic"
		triggerID   = "gd-trig-chain-on-close"
		project     = "gardenio"
	)

	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID:     "gd-some-other-epic",
				Type:   "epic",
				Title:  "Unrelated epic",
				Status: "closed",
				Labels: []string{"project:" + project},
			},
		},
	}
	src.mu.Unlock()

	daemon.mu.Lock()
	daemon.beads = triggerBeads(
		triggerBead(triggerID, "Chain: spawn gardener B when gardener A epic closes", "trigger:bead", map[string]string{
			"action":      "spawn_agent",
			"event_types": "closed",
			"conditions":  `[{"field":"id","op":"==","value":"` + watchedEpic + `"}]`,
			"role":        "gardener",
			"project":     project,
			"prompt":      "Gardener A epic {{bead_id}} closed — begin phase B.",
		}),
	)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 0 {
		t.Fatalf("unrelated epic close must NOT chain-spawn, got %d spawns (names=%v)",
			len(daemon.spawnedNames), daemon.spawnedNames)
	}
}
