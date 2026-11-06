package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"
)

// A project-less spawn writes an agent bead the controller's credential guard
// then refuses permanently (spawn_credential_guard.go:70-77), leaving an
// orphan agent bead nobody reaps. Refuse before the write (gd-s7taAUxCD7).
func TestActionSpawnAgent_EmptyProjectRefused(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:      "gd-trig-noproj",
		Title:   "project-less trigger",
		Source:  "schedule",
		Action:  TriggerActionSpawnAgent,
		Project: "",
		Role:    "crew",
		Prompt:  "do the thing",
	}

	_, err := engine.actionSpawnAgent(context.Background(), trig, TriggerMatch{Key: "k1"})
	if err == nil {
		t.Fatal("a project-less spawn_agent must be refused, got nil")
	}
	if !strings.Contains(err.Error(), "gd-trig-noproj") || !strings.Contains(err.Error(), "project") {
		t.Errorf("the error must name the trigger and the reason, got %v", err)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("no agent may be spawned, got %d SpawnAgent calls", len(daemon.spawnedNames))
	}
	if len(daemon.createdBeads) != 0 {
		t.Errorf("no task bead may be written, got %d CreateBead calls", len(daemon.createdBeads))
	}
}

func TestActionSpawnAgent_WithProjectStillSpawns(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:      "gd-trig-ok",
		Title:   "normal trigger",
		Source:  "schedule",
		Action:  TriggerActionSpawnAgent,
		Project: "gardenio",
		Role:    "crew",
		Prompt:  "do the thing",
	}

	agentID, err := engine.actionSpawnAgent(context.Background(), trig, TriggerMatch{Key: "k1"})
	if err != nil {
		t.Fatalf("a project-scoped spawn must still work, got %v", err)
	}
	if agentID == "" {
		t.Error("expected an agent id")
	}
}

func TestActionApplyFormula_EmptyProjectRefused(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	trig := TriggerDef{
		ID:        "gd-trig-formula",
		Title:     "formula trigger",
		Source:    "schedule",
		Action:    TriggerActionApplyFormula,
		Project:   "",
		FormulaID: "gd-formula-1",
		Prompt:    "pour it",
	}

	_, err := engine.actionApplyFormula(context.Background(), trig, TriggerMatch{Key: "k1"})
	if err == nil {
		t.Fatal("a project-less formula pour must be refused before the driver spawn, got nil")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Errorf("the error must name the reason, got %v", err)
	}

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.spawnedNames) != 0 {
		t.Errorf("no driver may be spawned, got %d SpawnAgent calls", len(daemon.spawnedNames))
	}
	if len(daemon.createdBeads) != 0 {
		t.Errorf("no molecule step bead may be written, got %d CreateBead calls", len(daemon.createdBeads))
	}
}
