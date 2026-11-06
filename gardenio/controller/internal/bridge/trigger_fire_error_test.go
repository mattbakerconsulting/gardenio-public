package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// trigger_fire_error_test.go — regression coverage for the fire-error
// diagnostic seam (bug ka-0000002991). Before this, a fireAction that RAISED
// (e.g. spawn_agent's CreateBead 400s) was logged and dropped: last_run stayed
// null, consecutive_failures stayed 0, last_outcome was untouched. From the
// data plane a gardener that MATCHES EVERY CYCLE AND FAILS was
// byte-indistinguishable from one idle or gated (the exact trap of
// ka-0000004529, where two agents mis-diagnosed a hard spawn error as a silent
// decline). These tests pin that a fire error now (a) persists
// last_fire_error / last_fire_error_at, (b) increments the gardener's
// consecutive_failures via the authoritative blob so hard_stop engages, and
// (c) clears on the next successful fire.

// typedUpdateFor returns the most-recent UpdateBeadFieldsTyped payload the
// engine sent for beadID, plus whether any was recorded. Walks in reverse so
// the freshest write wins.
func typedUpdateFor(daemon *mockTriggerDaemon, beadID string) (map[string]any, bool) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := len(daemon.updatedIDs) - 1; i >= 0; i-- {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		if i < len(daemon.updatedTyped) && daemon.updatedTyped[i] != nil {
			if _, ok := daemon.updatedTyped[i]["last_fire_error"]; ok {
				return daemon.updatedTyped[i], true
			}
		}
	}
	return nil, false
}

// fieldValueFor returns the most-recent value the engine stamped for `field` on
// beadID via either UpdateBeadFields or UpdateBeadFieldsTyped (the mock records
// both into updatedFields as strings).
func fieldValueFor(daemon *mockTriggerDaemon, beadID, field string) (string, bool) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for i := len(daemon.updatedIDs) - 1; i >= 0; i-- {
		if daemon.updatedIDs[i] != beadID {
			continue
		}
		if v, ok := daemon.updatedFields[i][field]; ok {
			return v, true
		}
	}
	return "", false
}

// TestEvalOne_FireError_PersistsOnGardener: a gardener whose fireAction errors
// must land last_fire_error + last_fire_error_at AND bump the blob's
// consecutive_failures (mirrored as an integer) — the counter hard_stop reads.
func TestEvalOne_FireError_PersistsOnGardener(t *testing.T) {
	daemon, engine, _, _ := gardenerTrigger(t, GardenerSpec{RunBudget: 5})
	daemon.spawnErr = errors.New("HTTP 400: operator could not be resolved")

	engine.Evaluate(context.Background())

	typed, ok := typedUpdateFor(daemon, "gd-cap-1")
	if !ok {
		t.Fatalf("expected an UpdateBeadFieldsTyped carrying last_fire_error; typed updates=%v", daemon.updatedTyped)
	}

	// The error string is persisted.
	fireErr, _ := typed["last_fire_error"].(string)
	if !strings.Contains(fireErr, "operator could not be resolved") {
		t.Errorf("last_fire_error = %q, want it to contain the spawn error", fireErr)
	}
	if at, _ := typed["last_fire_error_at"].(string); at == "" {
		t.Error("last_fire_error_at must be stamped alongside last_fire_error")
	}

	// consecutive_failures must be a NUMERIC type (integer-typed schema field)
	// and equal to 1 after one failed fire — the value hard_stop reads.
	cf, ok := typed["consecutive_failures"]
	if !ok {
		t.Fatalf("consecutive_failures not mirrored on fire error; got %v", typed)
	}
	switch v := cf.(type) {
	case int:
		if v != 1 {
			t.Errorf("consecutive_failures = %d, want 1", v)
		}
	case int64:
		if v != 1 {
			t.Errorf("consecutive_failures = %d, want 1", v)
		}
	case float64:
		if v != 1 {
			t.Errorf("consecutive_failures = %v, want 1", v)
		}
	default:
		t.Fatalf("consecutive_failures must be a numeric type for the integer-typed schema field, got %T (%v)", cf, cf)
	}

	// The bumped counter must also be reflected in the mirrored gardener blob so
	// GardenerStopContext.ConsecutiveFailures (read from the blob) sees it.
	raw, _ := typed["gardener"].(string)
	var cap GardenerSpec
	if err := json.Unmarshal([]byte(raw), &cap); err != nil {
		t.Fatalf("gardener blob did not unmarshal: %v", err)
	}
	if cap.ConsecutiveFailures != 1 {
		t.Errorf("gardener blob ConsecutiveFailures = %d, want 1", cap.ConsecutiveFailures)
	}

	// And crucially the outcome must NAME the failure, not read like a decline.
	outcome, _ := lastOutcomeFor(daemon, "gd-cap-1")
	if !strings.Contains(outcome, "fire failed") {
		t.Errorf("last_outcome = %q, want it to name the fire failure", outcome)
	}
}

// TestEvalOne_FireError_NonGardenerStampsErrorOnly: a plain (non-gardener)
// spawn trigger that errors stamps last_fire_error but does NOT touch
// consecutive_failures — there is no stop-block consuming it.
func TestEvalOne_FireError_NonGardenerStampsErrorOnly(t *testing.T) {
	daemon := &mockTriggerDaemon{spawnErr: errors.New("boom: HTTP 500")}
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

	got, ok := fieldValueFor(daemon, "t1", "last_fire_error")
	if !ok {
		t.Fatal("expected a last_fire_error stamp, got none")
	}
	if !strings.Contains(got, "boom") {
		t.Errorf("last_fire_error = %q, want it to contain the spawn error", got)
	}
	// No consecutive_failures write for a non-gardener trigger.
	if _, ok := fieldValueFor(daemon, "t1", "consecutive_failures"); ok {
		t.Error("non-gardener trigger must not write consecutive_failures on a fire error")
	}
}

// TestEvalOne_FireError_ClearedOnNextSuccess: once a gardener fires cleanly, a
// prior last_fire_error is blanked so it stops masquerading as a live failure.
func TestEvalOne_FireError_ClearedOnNextSuccess(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-cap-1", Type: "trigger", Status: "open", Title: "gardener trig",
				// A prior failure is already recorded on the bead.
				Fields: map[string]string{"last_fire_error": "HTTP 400: stale failure"}},
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	cap := GardenerSpec{RunBudget: 5}
	trig := TriggerDef{
		ID: "gd-cap-1", Title: "gardener trig", Source: "mock",
		Action: TriggerActionSpawnAgent, Project: "monorepo", Role: "crew",
		Prompt: "run", MaxConcurrent: 1, LastChecked: pastLastChecked(),
		Gardener:      &cap,
		LastFireError: "HTTP 400: stale failure", // parsed from the bead field
	}
	src := &mockTriggerSource{
		name: "mock", triggers: []TriggerDef{trig},
		matches: map[string][]TriggerMatch{"gd-cap-1": {{Key: "M1", Title: "match one"}}},
	}
	engine.RegisterSource(src)
	engine.Evaluate(context.Background())

	got, ok := fieldValueFor(daemon, "gd-cap-1", "last_fire_error")
	if !ok {
		t.Fatal("expected a last_fire_error clear write on successful fire, got none")
	}
	if got != "" {
		t.Errorf("last_fire_error = %q after successful fire, want it cleared to \"\"", got)
	}
}
