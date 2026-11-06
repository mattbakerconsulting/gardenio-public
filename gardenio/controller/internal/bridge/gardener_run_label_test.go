package bridge

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Phase 5 of the captain->gardener rename (gd-3X8WRXmPZk) dropped the
// dual-written role:captain label. A gardener trigger authored before the rename
// still carries the top-level field role="captain", which fireAction passes to
// SpawnAgent as the spawn role — so its runs were created marked role:captain
// ONLY and the saturation gate (and every role:gardener reader) missed them.
// The fire path must stamp role:gardener on a gardener-fired run regardless of
// the spawn role.

func fireOnce(t *testing.T, daemon *mockTriggerDaemon, trig TriggerDef, gardener bool) {
	t.Helper()
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	trig.Source = "mock"
	trig.Action = TriggerActionSpawnAgent
	trig.Project = "gardenio"
	trig.LastChecked = pastLastChecked()
	if gardener {
		trig.Gardener = &GardenerSpec{RunBudget: 10, MaxRuns: 20}
	}
	engine.RegisterSource(&mockTriggerSource{
		name:     "mock",
		triggers: []TriggerDef{trig},
		matches:  map[string][]TriggerMatch{trig.ID: {{Key: "2026-10-14T05:47", Title: "window"}}},
	})
	engine.Evaluate(context.Background())
}

func newRunLabelDaemon() *mockTriggerDaemon {
	return &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{{ID: "gd-g1", Type: "trigger", Status: "open"}},
	}
}

func TestGardenerFire_LegacyCaptainRoleRunIsGardenerMarked(t *testing.T) {
	daemon := newRunLabelDaemon()
	fireOnce(t, daemon, TriggerDef{ID: "gd-g1", Title: "g", Role: "captain", Prompt: "go"}, true)

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawn, got %d", len(daemon.spawnedNames))
	}
	// The spawn role is deliberately unchanged (it drives pod role / authz).
	if daemon.spawnedRoles[0] != "captain" {
		t.Errorf("spawn role = %q, want it left as the trigger's role", daemon.spawnedRoles[0])
	}
	if got := daemon.labelsAddedFor("gd-agent-1"); !slices.Contains(got, "role:gardener") {
		t.Errorf("gardener-fired run labels = %v, want role:gardener", got)
	}
}

func TestGardenerFire_OtherSpawnRoleKeepsRoleAndGainsGardenerLabel(t *testing.T) {
	daemon := newRunLabelDaemon()
	fireOnce(t, daemon, TriggerDef{ID: "gd-g1", Title: "g", Role: "plan-gardener", Prompt: "go"}, true)

	if daemon.spawnedRoles[0] != "plan-gardener" {
		t.Errorf("spawn role = %q, want plan-gardener", daemon.spawnedRoles[0])
	}
	if got := daemon.labelsAddedFor("gd-agent-1"); !slices.Contains(got, "role:gardener") {
		t.Errorf("labels = %v, want role:gardener", got)
	}
}

func TestGardenerFire_GardenerSpawnRoleNotDoubleLabelled(t *testing.T) {
	daemon := newRunLabelDaemon()
	fireOnce(t, daemon, TriggerDef{ID: "gd-g1", Title: "g", Role: "gardener", Prompt: "go"}, true)

	// SpawnAgent itself stamps role:<spawn role>; the engine must not add a dup.
	if got := daemon.labelsAddedFor("gd-agent-1"); slices.Contains(got, "role:gardener") {
		t.Errorf("engine re-stamped role:gardener on a run already spawned as gardener: %v", got)
	}
}

func TestGardenerFire_LabelFailureDoesNotFailFire(t *testing.T) {
	daemon := newRunLabelDaemon()
	daemon.addLabelErr = errors.New("boom")
	fireOnce(t, daemon, TriggerDef{ID: "gd-g1", Title: "g", Role: "captain", Prompt: "go"}, true)
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("a role:gardener label failure must not fail the fire; spawns=%d", len(daemon.spawnedNames))
	}
}

func TestStampGardenerRunLabel_NonGardenerTriggerUnmarked(t *testing.T) {
	daemon := newRunLabelDaemon()
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	engine.stampGardenerRunLabel(context.Background(), TriggerDef{ID: "gd-g1", Role: "crew"}, "gd-agent-1") // Gardener == nil
	if got := daemon.labelsAddedFor("gd-agent-1"); len(got) != 0 {
		t.Errorf("non-gardener trigger run must not be gardener-marked: %v", got)
	}
}
