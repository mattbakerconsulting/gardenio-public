package bridge

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// A decision closed with neither an option nor a text answer was withdrawn by
// whoever filed it, not resolved (gd-VuYwGGYLLT): the bridge must not respawn the
// requesting agent on it, or an agent could wake a lane of its own choosing by
// filing a decision naming that lane's bead and closing it bare.

func bareCloseFixture() (*mockDaemon, *Decisions) {
	daemon := newMockDaemon()
	prior := &beadsapi.BeadDetail{
		ID: "bd-captain-1", Type: "agent",
		Fields: map[string]string{
			"agent": "gardenio-captain", "agent_state": "done",
			"project": "gardenio", "task_id": "gd-TASK1", "role": "captain",
		},
	}
	daemon.beads["gardenio-captain"] = prior
	daemon.beads["bd-captain-1"] = prior
	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})
	return daemon, d
}

func countDecisionResumes(daemon *mockDaemon) int {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	n := 0
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			n++
		}
	}
	return n
}

func TestDecisions_HandleClosed_BareCloseDoesNotRespawn(t *testing.T) {
	daemon, d := bareCloseFixture()
	ev := marshalSSEBeadPayload(BeadEvent{
		ID: "dec-bare-1", Type: "decision", Assignee: "gardenio-captain",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "hijack?",
			"requesting_agent_bead_id": "bd-captain-1",
		},
	})
	d.handleClosed(context.Background(), ev)
	if n := countDecisionResumes(daemon); n != 0 {
		t.Fatalf("a bare-closed decision must not respawn anyone, got %d spawn(s)", n)
	}
}

func TestDecisions_HandleClosed_BareCloseEventButStoredDecisionAnsweredRespawns(t *testing.T) {
	daemon, d := bareCloseFixture()
	// SSE close events can drop fields: the stored decision carries the answer.
	daemon.beads["dec-stripped-1"] = &beadsapi.BeadDetail{
		ID: "dec-stripped-1", Type: "decision",
		Fields: map[string]string{"prompt": "go?", "chosen": "yes", "requesting_agent_bead_id": "bd-captain-1"},
	}
	ev := marshalSSEBeadPayload(BeadEvent{
		ID: "dec-stripped-1", Type: "decision", Assignee: "gardenio-captain",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"requesting_agent_bead_id": "bd-captain-1"},
	})
	d.handleClosed(context.Background(), ev)
	if n := countDecisionResumes(daemon); n != 1 {
		t.Fatalf("an answered decision must still respawn, got %d spawn(s)", n)
	}
}

func TestDecisions_HandleClosed_FreeTextAnswerRespawns(t *testing.T) {
	daemon, d := bareCloseFixture()
	ev := marshalSSEBeadPayload(BeadEvent{
		ID: "dec-text-1", Type: "decision", Assignee: "gardenio-captain",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt": "go?", "response_text": "yes, ship it",
			"requesting_agent_bead_id": "bd-captain-1",
		},
	})
	d.handleClosed(context.Background(), ev)
	if n := countDecisionResumes(daemon); n != 1 {
		t.Fatalf("a free-text answer must respawn, got %d spawn(s)", n)
	}
}

// A decision a machine principal filed (the Lands Gate's approval decision,
// filed by machine:gardenio-beads-viewer) has no agent behind it: resolving it
// must not respawn a pod named after the client, whose reactor reads the
// answer itself.
func TestDecisions_HandleClosed_MachineFiledDecisionDoesNotRespawn(t *testing.T) {
	daemon, d := bareCloseFixture()
	ev := marshalSSEBeadPayload(BeadEvent{
		ID: "dec-gate-1", Type: "decision", CreatedBy: "machine:gardenio-beads-viewer",
		Labels: []string{"project:lands-intake"},
		Fields: map[string]string{
			"prompt": "Plot application: tomato-patch", "chosen": "approve",
			"requesting_task": "gd-app1",
		},
	})
	d.handleClosed(context.Background(), ev)
	if n := countDecisionResumes(daemon); n != 0 {
		t.Fatalf("a machine-filed decision must not respawn anyone, got %d spawn(s)", n)
	}
	d.RespawnForDecision(context.Background(), "machine:gardenio-beads-viewer", BeadEvent{ID: "dec-gate-1", Fields: map[string]string{"chosen": "approve"}}, "m")
	if n := countDecisionResumes(daemon); n != 0 {
		t.Fatalf("RespawnForDecision spawned for a machine filer: %d", n)
	}
}
