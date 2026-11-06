package bridge

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestDecisions_HandleClosed_RespawnThawsFrozenAgent covers the Δ4/A2 thaw wiring
// (ka-0000000241): when the requesting agent was FROZEN on its yield (gb yield
// checkpointed the pod and left the agent bead OPEN at agent_state=stopping), a
// plain decision-resume respawn would cold-start — SpawnAgent's name-dedup treats
// the open stopping bead as ACTIVE and returns it without a new "created" event,
// so the controller never runs shouldThaw. The fix mirrors respawnThreadAgent:
// CLOSE the frozen bead so dedup no longer matches, then pin runtime_class=runsc
// on the fresh same-name spawn so its "created" event thaws the snapshot.
func TestDecisions_HandleClosed_RespawnThawsFrozenAgent(t *testing.T) {
	daemon := newMockDaemon()
	// enforceActiveStatusScoping so a stale FindAgentBead can't mask a missing
	// close: after the frozen bead is closed it must be invisible to lookups,
	// exactly as the real daemon's active-status query behaves.
	daemon.enforceActiveStatusScoping = true

	frozen := &beadsapi.BeadDetail{
		ID:     "bd-frozen-1",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":         "gardenio-crew-frozen",
			"agent_state":   "stopping", // written only by suspendAgent (freeze)
			"project":       "gardenio",
			"task_id":       "gd-task77",
			"role":          "crew",
			"runtime_class": "runsc",
		},
	}
	daemon.beads["gardenio-crew-frozen"] = frozen
	daemon.beads["bd-frozen-1"] = frozen

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-frozen-gate-1",
		Type:     "decision",
		Assignee: "gardenio-crew-frozen",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "Proceed with plan A or B?",
			"chosen":                   "A",
			"rationale":                "approved",
			"requesting_agent_bead_id": "bd-frozen-1",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	// The frozen bead must be closed so SpawnAgent's name-dedup won't return it.
	closedFrozen := false
	for _, c := range daemon.closed {
		if c.BeadID == "bd-frozen-1" {
			closedFrozen = true
		}
	}
	if !closedFrozen {
		t.Errorf("expected frozen agent bead bd-frozen-1 to be closed before respawn; closed=%+v", daemon.closed)
	}

	// The respawn must pin runtime_class=runsc so the fresh same-name bead's
	// "created" event passes shouldThaw and restores the snapshot.
	var spawned *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			spawned = b
			break
		}
	}
	if spawned == nil {
		t.Fatal("expected a new decision-resume agent bead to be spawned for thaw")
	}
	if got := spawned.Fields["runtime_class"]; got != "runsc" {
		t.Errorf("expected respawn to pin runtime_class=runsc for thaw, got %q", got)
	}
}

// TestDecisions_HandleClosed_RespawnFrozenPinsRunscWhenClassUnset verifies the
// belt-and-suspenders fallback: a frozen bead is definitionally runsc, so even if
// its runtime_class field was somehow unset the respawn still pins runsc (never
// leaving the thaw to a cold-start default).
func TestDecisions_HandleClosed_RespawnFrozenPinsRunscWhenClassUnset(t *testing.T) {
	daemon := newMockDaemon()
	frozen := &beadsapi.BeadDetail{
		ID:     "bd-frozen-2",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":       "gardenio-crew-frozen2",
			"agent_state": "stopping",
			"project":     "gardenio",
			"task_id":     "gd-task78",
			"role":        "crew",
			// runtime_class deliberately absent.
		},
	}
	daemon.beads["gardenio-crew-frozen2"] = frozen
	daemon.beads["bd-frozen-2"] = frozen

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-frozen-gate-2",
		Type:     "decision",
		Assignee: "gardenio-crew-frozen2",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "Go?",
			"chosen":                   "yes",
			"requesting_agent_bead_id": "bd-frozen-2",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var spawned *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			spawned = b
			break
		}
	}
	if spawned == nil {
		t.Fatal("expected a decision-resume respawn")
	}
	if got := spawned.Fields["runtime_class"]; got != "runsc" {
		t.Errorf("expected runtime_class=runsc fallback for a frozen bead with no class, got %q", got)
	}
}

// TestDecisions_HandleClosed_RespawnNonFrozenNoRuntimePin is the guard: a NORMAL
// (non-frozen, agent_state=done) decision-resume must NOT be over-pinned to runsc
// and must NOT close the agent bead — it keeps inheriting the role/project/cluster
// runtime_class default exactly as before Δ4/A2.
func TestDecisions_HandleClosed_RespawnNonFrozenNoRuntimePin(t *testing.T) {
	daemon := newMockDaemon()
	done := &beadsapi.BeadDetail{
		ID:     "bd-done-1",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":       "gardenio-crew-done",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-task79",
			"role":        "crew",
		},
	}
	daemon.beads["gardenio-crew-done"] = done
	daemon.beads["bd-done-1"] = done

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-done-gate-1",
		Type:     "decision",
		Assignee: "gardenio-crew-done",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "Go?",
			"chosen":                   "yes",
			"requesting_agent_bead_id": "bd-done-1",
		},
	})
	d.handleClosed(context.Background(), closedEvent)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	var spawned *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			spawned = b
			break
		}
	}
	if spawned == nil {
		t.Fatal("expected a decision-resume respawn for the dead agent")
	}
	if got, ok := spawned.Fields["runtime_class"]; ok {
		t.Errorf("expected NO runtime_class pin for a non-frozen respawn, got %q", got)
	}
	// The dead agent bead must not be closed by the freeze/thaw path (it was
	// already terminal; the freeze branch is guarded on agent_state=stopping).
	for _, c := range daemon.closed {
		if c.BeadID == "bd-done-1" {
			t.Errorf("non-frozen respawn must not close the agent bead bd-done-1")
		}
	}
}

func frozenDecisionFixture(project string) (*mockDaemon, *Decisions, []byte) {
	daemon := newMockDaemon()
	frozen := &beadsapi.BeadDetail{
		ID:     "bd-frozen-9",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":         "gardenio-captain-frozen",
			"agent_state":   "stopping",
			"project":       project,
			"role":          "captain",
			"runtime_class": "runsc",
		},
	}
	daemon.beads["gardenio-captain-frozen"] = frozen
	daemon.beads["bd-frozen-9"] = frozen
	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})
	ev := marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-frozen-gate-9",
		Type:     "decision",
		Assignee: "gardenio-captain-frozen",
		Labels:   []string{"project:" + project},
		Fields: map[string]string{
			"prompt":                   "Go?",
			"chosen":                   "yes",
			"requesting_agent_bead_id": "bd-frozen-9",
		},
	})
	return daemon, d, ev
}

// gd-VuYwGGYLLT fix C: the frozen bead is closed so the respawn's name dedup
// misses, but a respawn the daemon then refuses must not leave the agent behind
// a closed bead with its snapshot stranded. The bead is put back.
func TestDecisions_HandleClosed_FrozenBeadReopenedWhenRespawnFails(t *testing.T) {
	daemon, d, ev := frozenDecisionFixture("gardenio")
	daemon.spawnAgentErr = errors.New("403 spawn denied: role captain is privileged")

	d.handleClosed(context.Background(), ev)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.closed) != 1 || daemon.closed[0].BeadID != "bd-frozen-9" {
		t.Fatalf("expected the frozen bead to be closed once before the create, got %+v", daemon.closed)
	}
	frozen := daemon.beads["bd-frozen-9"]
	if frozen.Status != "open" {
		t.Errorf("failed respawn must reopen the frozen bead, status=%q", frozen.Status)
	}
	if frozen.Fields["agent_state"] != "stopping" {
		t.Errorf("reopened bead must keep agent_state=stopping so a retry still thaws, got %q", frozen.Fields["agent_state"])
	}
}

// A respawn that is skipped before it ever tries to create (the e2e fixture
// project) must not close the frozen bead at all.
func TestDecisions_HandleClosed_FrozenBeadUntouchedWhenRespawnSkipped(t *testing.T) {
	daemon, d, ev := frozenDecisionFixture(e2eTestProject)

	d.handleClosed(context.Background(), ev)

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if len(daemon.closed) != 0 {
		t.Errorf("a skipped respawn must not close the frozen bead, closed=%+v", daemon.closed)
	}
	if got := daemon.beads["bd-frozen-9"].Status; got != "open" {
		t.Errorf("frozen bead status = %q, want open", got)
	}
}
