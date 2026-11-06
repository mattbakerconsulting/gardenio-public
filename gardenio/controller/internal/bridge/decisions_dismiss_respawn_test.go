package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// findDecisionResumeSpawn returns the first decision-resume agent bead the mock
// recorded, or nil. Callers hold no lock — it takes the mock lock itself.
func findDecisionResumeSpawn(m *mockDaemon) *beadsapi.BeadDetail {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			return b
		}
	}
	return nil
}

// seedDismissDecisionFixture wires up the reproduction of ka-0000005665: a gate
// decision whose requesting agent has already stopped (agent_state=done, dead
// assignee) and which carries a requesting_task the agent parked on. It returns
// the closed SSE event for the decision, minus the `chosen` field so each test
// can set the terminal disposition (dismissed / _expired / a real answer).
func seedDismissDecisionFixture(daemon *mockDaemon, requestingTask string) *beadsapi.BeadDetail {
	reqAgent := &beadsapi.BeadDetail{
		ID:     "bd-req-agent",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":       "gardenio-standup-convergence-dead",
			"agent_state": "done",
			"project":     "gardenio",
			"task_id":     "gd-molecule",
			"role":        "crew",
		},
	}
	daemon.beads["gardenio-standup-convergence-dead"] = reqAgent
	daemon.beads["bd-req-agent"] = reqAgent
	if requestingTask != "" {
		daemon.beads[requestingTask] = &beadsapi.BeadDetail{
			ID:       requestingTask,
			Type:     "task",
			Status:   "open",
			Title:    "GATE — human approves the cross-post",
			Assignee: "gardenio-standup-convergence-dead",
		}
	}
	return reqAgent
}

func dismissDecisionEvent(requestingTask, chosen string) []byte {
	fields := map[string]string{
		"prompt":                   "Approve the cross-post to #daily-stand-up?",
		"chosen":                   chosen,
		"requesting_agent_bead_id": "bd-req-agent",
	}
	if requestingTask != "" {
		fields["requesting_task"] = requestingTask
	}
	return marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-gate-1",
		Type:     "decision",
		Assignee: "gardenio-standup-convergence-dead",
		Labels:   []string{"project:gardenio"},
		Fields:   fields,
	})
}

// TestDecisions_HandleClosed_DismissedWithRequestingTaskRespawns is the direct
// regression for ka-0000005665: an operator DISMISSING a gate decision that
// carries a requesting_task must respawn the parked (dead) requesting agent so
// it can dispose of its task — NOT silently strand it in_progress forever while
// its parent molecule stays blocked.
func TestDecisions_HandleClosed_DismissedWithRequestingTaskRespawns(t *testing.T) {
	daemon := newMockDaemon()
	seedDismissDecisionFixture(daemon, "gd-gate-task")

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	d.handleClosed(context.Background(), dismissDecisionEvent("gd-gate-task", "dismissed"))

	spawned := findDecisionResumeSpawn(daemon)
	if spawned == nil {
		t.Fatal("expected a decision-resume respawn for the DISMISSED gate decision with a parked requesting_task")
	}
	if got := spawned.Fields["decision_bead_id"]; got != "dec-gate-1" {
		t.Errorf("respawn decision_bead_id = %q, want dec-gate-1", got)
	}
	// The respawn prompt must tell the agent the gate was DECLINED and name the
	// parked task to dispose of — otherwise a naive respawn could re-file the
	// gate or proceed as if approved.
	prompt := spawned.Fields["prompt"]
	if !strings.Contains(prompt, "DISMISSED") {
		t.Errorf("respawn prompt must flag the dismissal; got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "gd-gate-task") {
		t.Errorf("respawn prompt must name the parked requesting task gd-gate-task; got:\n%s", prompt)
	}
}

// TestDecisions_HandleClosed_DismissedNoRequestingTaskNoRespawn preserves the
// prior behaviour: a plain dismissal with NO parked requesting_task has nothing
// to act on, so it must NOT spin up a pod (avoids churn on ordinary dismissals).
func TestDecisions_HandleClosed_DismissedNoRequestingTaskNoRespawn(t *testing.T) {
	daemon := newMockDaemon()
	seedDismissDecisionFixture(daemon, "")

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	d.handleClosed(context.Background(), dismissDecisionEvent("", "dismissed"))

	if spawned := findDecisionResumeSpawn(daemon); spawned != nil {
		t.Fatalf("dismissed decision with no requesting_task must NOT respawn; got agent bead %s", spawned.ID)
	}
}

// TestDecisions_HandleClosed_ExpiredWithRequestingTaskNoRespawn keeps the
// loop-avoidance guard for a pure TIMEOUT: _expired has no operator input, so
// respawning would just re-file the same decision and time out again. Even with
// a requesting_task present, _expired stays a no-respawn (the parked task is
// left for the intake/janitor sweep, not looped on).
func TestDecisions_HandleClosed_ExpiredWithRequestingTaskNoRespawn(t *testing.T) {
	daemon := newMockDaemon()
	seedDismissDecisionFixture(daemon, "gd-gate-task")

	d := NewDecisions(DecisionsConfig{
		Daemon:   daemon,
		Notifier: &mockNotifier{},
		Logger:   slog.Default(),
	})

	d.handleClosed(context.Background(), dismissDecisionEvent("gd-gate-task", "_expired"))

	if spawned := findDecisionResumeSpawn(daemon); spawned != nil {
		t.Fatalf("_expired decision must NOT respawn (loop-avoidance); got agent bead %s", spawned.ID)
	}
}

// TestDecisions_HandleClosed_RespawnCarriesWishPointer: a thread agent
// respawned for its resolved decision keeps its thread head (gd-AflFRvDw7Z),
// or its card edit upserts a second, top-level card bead.
func TestDecisions_HandleClosed_RespawnCarriesWishPointer(t *testing.T) {
	daemon := newMockDaemon()
	reqAgent := seedDismissDecisionFixture(daemon, "gd-gate-task")
	reqAgent.Fields["slack_thread_channel"] = "C0THREAD"
	reqAgent.Fields["slack_thread_ts"] = "1690000000.000100"
	reqAgent.Fields["slack_thread_bead_id"] = "gd-head-t"

	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})
	d.handleClosed(context.Background(), dismissDecisionEvent("gd-gate-task", "Approve"))

	spawned := findDecisionResumeSpawn(daemon)
	if spawned == nil {
		t.Fatal("expected a decision-resume respawn")
	}
	if spawned.Fields["thread_id"] != "gd-head-t" || spawned.Fields["slack_thread_bead_id"] != "gd-head-t" {
		t.Fatalf("respawn thread_id = %q, slack_thread_bead_id = %q, want gd-head-t for both",
			spawned.Fields["thread_id"], spawned.Fields["slack_thread_bead_id"])
	}
}
