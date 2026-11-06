package bridge

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// mockAgentNotifier records calls to NotifyAgentCrash, NotifyAgentDeath, NotifyAgentSpawn, NotifyAgentState, and NotifyAgentTaskUpdate.
type mockAgentNotifier struct {
	mu           sync.Mutex
	crashes      []BeadEvent
	deaths       []BeadEvent
	spawns       []BeadEvent
	stateChanges []BeadEvent
	taskUpdates  []string
	paused       []BeadEvent
}

func (m *mockAgentNotifier) NotifyAgentPaused(_ context.Context, bead BeadEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.paused = append(m.paused, bead)
}

func (m *mockAgentNotifier) NotifyAgentCrash(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.crashes = append(m.crashes, bead)
	return nil
}

func (m *mockAgentNotifier) NotifyAgentDeath(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deaths = append(m.deaths, bead)
	return nil
}

func (m *mockAgentNotifier) NotifyAgentSpawn(_ context.Context, bead BeadEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.spawns = append(m.spawns, bead)
}

func (m *mockAgentNotifier) NotifyAgentState(_ context.Context, bead BeadEvent) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stateChanges = append(m.stateChanges, bead)
}

func (m *mockAgentNotifier) NotifyAgentTaskUpdate(_ context.Context, agentName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.taskUpdates = append(m.taskUpdates, agentName)
}

func (m *mockAgentNotifier) getCrashes() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.crashes...)
}

func (m *mockAgentNotifier) getDeaths() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.deaths...)
}

func (m *mockAgentNotifier) getSpawns() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.spawns...)
}

func (m *mockAgentNotifier) getStateChanges() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.stateChanges...)
}

func (m *mockAgentNotifier) getTaskUpdates() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.taskUpdates...)
}

func TestAgents_HandleClosed_CrashNotification(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Non-agent bead should be ignored.
	nonAgent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-1",
		Type: "decision",
		Fields: map[string]string{
			"agent_state": "failed",
		},
	})
	a.handleClosed(context.Background(), nonAgent)
	if len(notif.getCrashes()) != 0 {
		t.Fatal("non-agent bead should not trigger crash notification")
	}

	// Agent bead closing with agent_state=done should be ignored.
	doneAgent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-1",
		Type:     "agent",
		Assignee: "gardenio/crew/test-bot",
		Fields: map[string]string{
			"agent_state": "done",
		},
	})
	a.handleClosed(context.Background(), doneAgent)
	if len(notif.getCrashes()) != 0 {
		t.Fatal("agent with state=done should not trigger crash notification")
	}

	// Agent bead closing with agent_state=failed should trigger notification.
	crashedAgent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-2",
		Type:     "agent",
		Title:    "crew-gardenio-crew-test-bot",
		Assignee: "gardenio/crew/test-bot",
		Fields: map[string]string{
			"agent_state": "failed",
			"pod_name":    "crew-gardenio-crew-test-bot-xyz",
		},
	})
	a.handleClosed(context.Background(), crashedAgent)

	crashes := notif.getCrashes()
	if len(crashes) != 1 {
		t.Fatalf("expected 1 crash notification, got %d", len(crashes))
	}
	if crashes[0].ID != "agent-2" {
		t.Errorf("expected bead ID agent-2, got %s", crashes[0].ID)
	}
	if crashes[0].Assignee != "gardenio/crew/test-bot" {
		t.Errorf("expected assignee gardenio/crew/test-bot, got %s", crashes[0].Assignee)
	}
}

func TestAgents_HandleUpdated_PodPhaseFailed(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Agent updated with pod_phase=failed should trigger notification.
	failedPod := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-3",
		Type:     "agent",
		Assignee: "gardenio/crew/worker-1",
		Fields: map[string]string{
			"agent_state": "working",
			"pod_phase":   "failed",
			"pod_name":    "crew-gardenio-crew-worker-1-abc",
		},
	})
	a.handleUpdated(context.Background(), failedPod)

	crashes := notif.getCrashes()
	if len(crashes) != 1 {
		t.Fatalf("expected 1 crash notification, got %d", len(crashes))
	}
	if crashes[0].ID != "agent-3" {
		t.Errorf("expected bead ID agent-3, got %s", crashes[0].ID)
	}
}

// TestAgents_HandleUpdated_DoneSuppressesCrashOnLatePodPhase verifies the
// teardown race fix: when an agent closes cleanly (agent_state=done) and a
// later SSE update carries pod_phase=failed (pod SIGKILLed after its graceful
// period syncs late), we must NOT notify a crash — that path spams Slack with
// bogus "Agent crashed" alerts for agents that exited normally.
func TestAgents_HandleUpdated_DoneSuppressesCrashOnLatePodPhase(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// agent_state=done + pod_phase=failed → teardown race, suppress crash.
	raceEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-race",
		Type:     "agent",
		Assignee: "thread-1234567890-111111",
		Fields: map[string]string{
			"agent_state": "done",
			"pod_phase":   "failed",
		},
	})
	a.handleUpdated(context.Background(), raceEvent)
	if got := len(notif.getCrashes()); got != 0 {
		t.Fatalf("expected no crash notification when agent_state=done (teardown race), got %d", got)
	}

	// Same race but via the closed event.
	a.handleClosed(context.Background(), raceEvent)
	if got := len(notif.getCrashes()); got != 0 {
		t.Fatalf("expected no crash notification on close when agent_state=done, got %d", got)
	}
}

// TestAgents_OrphanReaperCleanupSuppressesCrash verifies that orphan reaper
// cleanups (close_reason="orphan: source task ... already closed") do NOT
// fire crash notifications. The stale reaper closes orphaned agent beads
// with agent_state=failed, but these are expected cleanups (decision-resume
// agents respawned after their parent task closed, scheduled-gardener agents
// whose schedule was disabled mid-flight) and should not be reported as
// crashes to Slack.
func TestAgents_OrphanReaperCleanupSuppressesCrash(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	orphanClosed := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-orphan",
		Type:     "agent",
		Assignee: "gardenio/crew/orphaned",
		Fields: map[string]string{
			"agent_state":  "failed",
			"close_reason": "orphan: source task gd-ABCDEF already closed",
		},
	})
	a.handleClosed(context.Background(), orphanClosed)
	if got := len(notif.getCrashes()); got != 0 {
		t.Fatalf("orphan reaper cleanup must not notify crash, got %d", got)
	}

	orphanUpdated := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-orphan-2",
		Type:     "agent",
		Assignee: "gardenio/crew/orphaned-2",
		Fields: map[string]string{
			"agent_state":  "failed",
			"close_reason": "orphan: source task gd-XYZ already closed",
		},
	})
	a.handleUpdated(context.Background(), orphanUpdated)
	if got := len(notif.getCrashes()); got != 0 {
		t.Fatalf("orphan reaper cleanup via update must not notify crash, got %d", got)
	}

	// Genuine liveness reap (stale: prefix) should still notify — these
	// represent real stuck/hung agents, not expected cleanups.
	staleClosed := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-stale",
		Type:     "agent",
		Assignee: "gardenio/crew/stuck",
		Fields: map[string]string{
			"agent_state":  "failed",
			"close_reason": "stale: no activity for 30m0s",
		},
	})
	a.handleClosed(context.Background(), staleClosed)
	if got := len(notif.getCrashes()); got != 1 {
		t.Fatalf("stale-prefixed reaper reap must still notify crash, got %d", got)
	}
}

func TestAgents_Deduplication(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	crashEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-4",
		Type:     "agent",
		Assignee: "gardenio/crew/bot-a",
		Fields: map[string]string{
			"agent_state": "failed",
		},
	})

	// First call: should notify.
	a.handleClosed(context.Background(), crashEvent)
	// Second call (e.g., from SSE reconnect): should be deduplicated.
	a.handleClosed(context.Background(), crashEvent)
	// Third call via updated handler: still deduplicated.
	a.handleUpdated(context.Background(), crashEvent)

	crashes := notif.getCrashes()
	if len(crashes) != 1 {
		t.Fatalf("expected exactly 1 crash notification (dedup), got %d", len(crashes))
	}
}

func TestAgents_NilNotifier(t *testing.T) {
	a := NewAgents(AgentsConfig{
		Notifier: nil,
		Logger:   slog.Default(),
	})

	crashEvent := marshalSSEBeadPayload(BeadEvent{
		ID:   "agent-5",
		Type: "agent",
		Fields: map[string]string{
			"agent_state": "failed",
		},
	})

	// Should not panic even with nil notifier.
	a.handleClosed(context.Background(), crashEvent)
}

// TestAgents_HandleCreated verifies that agent bead creation fires
// NotifyAgentSpawn for agent beads and is skipped for non-agent beads.
func TestAgents_HandleCreated(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Non-agent bead should be ignored.
	nonAgent := marshalSSEBeadPayload(BeadEvent{
		ID:   "dec-10",
		Type: "decision",
	})
	a.handleCreated(context.Background(), nonAgent)
	if len(notif.getSpawns()) != 0 {
		t.Fatal("non-agent bead should not trigger spawn notification")
	}

	// Agent bead creation should trigger spawn notification.
	agentBead := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-10",
		Type:     "agent",
		Title:    "crew-gardenio-crew-builder",
		Assignee: "gardenio/crew/builder",
	})
	a.handleCreated(context.Background(), agentBead)

	spawns := notif.getSpawns()
	if len(spawns) != 1 {
		t.Fatalf("expected 1 spawn notification, got %d", len(spawns))
	}
	if spawns[0].ID != "agent-10" {
		t.Errorf("expected bead ID agent-10, got %s", spawns[0].ID)
	}
	if spawns[0].Assignee != "gardenio/crew/builder" {
		t.Errorf("expected assignee gardenio/crew/builder, got %s", spawns[0].Assignee)
	}
}

// TestAgents_HandleClosed_NormalCompletion verifies that a normally completed
// agent bead (not failed) triggers NotifyAgentState with state "done".
func TestAgents_HandleClosed_NormalCompletion(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	doneAgent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-11",
		Type:     "agent",
		Assignee: "gardenio/crew/finisher",
		Fields: map[string]string{
			"agent_state": "done",
		},
	})
	a.handleClosed(context.Background(), doneAgent)

	if len(notif.getCrashes()) != 0 {
		t.Error("normal completion should not trigger crash notification")
	}
	changes := notif.getStateChanges()
	if len(changes) != 1 {
		t.Fatalf("expected 1 state change notification, got %d", len(changes))
	}
	if changes[0].Fields["agent_state"] != "done" {
		t.Errorf("expected agent_state=done, got %q", changes[0].Fields["agent_state"])
	}
}

// TestAgents_HandleClosed_NoState verifies that an agent bead closing without
// an explicit agent_state gets defaulted to "done".
func TestAgents_HandleClosed_NoState(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	closedAgent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-12",
		Type:     "agent",
		Assignee: "gardenio/crew/silent",
		Fields:   map[string]string{},
	})
	a.handleClosed(context.Background(), closedAgent)

	if len(notif.getCrashes()) != 0 {
		t.Error("normal close should not trigger crash notification")
	}
	changes := notif.getStateChanges()
	if len(changes) != 1 {
		t.Fatalf("expected 1 state change notification, got %d", len(changes))
	}
	if changes[0].Fields["agent_state"] != "done" {
		t.Errorf("expected agent_state defaulted to done, got %q", changes[0].Fields["agent_state"])
	}
}

// TestAgents_HandleUpdated_TaskClaim verifies that a non-agent bead becoming
// in_progress triggers NotifyAgentTaskUpdate for the assignee.
func TestAgents_HandleUpdated_TaskClaim(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Task bead claimed by an agent should trigger NotifyAgentTaskUpdate.
	claimedTask := marshalSSEBeadPayload(BeadEvent{
		ID:       "task-1",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "matt-1",
	})
	a.handleUpdated(context.Background(), claimedTask)

	updates := notif.getTaskUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 task update notification, got %d", len(updates))
	}
	if updates[0] != "matt-1" {
		t.Errorf("expected agentName=matt-1, got %s", updates[0])
	}

	// No state change or crash notifications should have fired.
	if len(notif.getCrashes()) != 0 {
		t.Error("task claim should not trigger crash notification")
	}
	if len(notif.getStateChanges()) != 0 {
		t.Error("task claim should not trigger state change notification")
	}
}

// TestAgents_HandleUpdated_TaskNoAssignee verifies that a non-agent bead update
// without an assignee does not trigger NotifyAgentTaskUpdate.
func TestAgents_HandleUpdated_TaskNoAssignee(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	unassigned := marshalSSEBeadPayload(BeadEvent{
		ID:     "task-2",
		Type:   "task",
		Status: "in_progress",
		// No assignee.
	})
	a.handleUpdated(context.Background(), unassigned)

	if len(notif.getTaskUpdates()) != 0 {
		t.Error("task with no assignee should not trigger task update notification")
	}
}

// TestAgents_HandleClosed_TaskBead verifies that closing a task bead assigned to
// an agent triggers NotifyAgentTaskUpdate so the card clears the completed task.
func TestAgents_HandleClosed_TaskBead(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Task bead closed by its agent assignee should trigger a card refresh.
	closedTask := marshalSSEBeadPayload(BeadEvent{
		ID:       "task-10",
		Type:     "task",
		Status:   "closed",
		Assignee: "matt-1",
	})
	a.handleClosed(context.Background(), closedTask)

	updates := notif.getTaskUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 task update notification on close, got %d", len(updates))
	}
	if updates[0] != "matt-1" {
		t.Errorf("expected agentName=matt-1, got %s", updates[0])
	}
	// No crash or state change notifications should have fired.
	if len(notif.getCrashes()) != 0 {
		t.Error("task close should not trigger crash notification")
	}
	if len(notif.getStateChanges()) != 0 {
		t.Error("task close should not trigger state change notification")
	}
}

// TestAgents_HandleClosed_TaskBeadNoAssignee verifies that closing an unassigned
// task bead does not trigger NotifyAgentTaskUpdate.
func TestAgents_HandleClosed_TaskBeadNoAssignee(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	closedTask := marshalSSEBeadPayload(BeadEvent{
		ID:     "task-11",
		Type:   "task",
		Status: "closed",
		// No Assignee.
	})
	a.handleClosed(context.Background(), closedTask)

	if len(notif.getTaskUpdates()) != 0 {
		t.Error("unassigned task close should not trigger task update notification")
	}
}

// TestAgents_HandleUpdated_TaskClose verifies that a task bead updated with
// status=closed triggers NotifyAgentTaskUpdate (defensive coverage).
func TestAgents_HandleUpdated_TaskClose(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	closedTask := marshalSSEBeadPayload(BeadEvent{
		ID:       "task-12",
		Type:     "bug",
		Status:   "closed",
		Assignee: "builder-1",
	})
	a.handleUpdated(context.Background(), closedTask)

	updates := notif.getTaskUpdates()
	if len(updates) != 1 {
		t.Fatalf("expected 1 task update notification for closed status, got %d", len(updates))
	}
	if updates[0] != "builder-1" {
		t.Errorf("expected agentName=builder-1, got %s", updates[0])
	}
}

// TestAgents_HandleUpdated_TaskUnassigned verifies that when a task bead's
// assignee is cleared (unclaimed), the previous assignee's card is refreshed.
func TestAgents_HandleUpdated_TaskUnassigned(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Step 1: Task claimed by matt-1 — should trigger card refresh and track assignee.
	claimedTask := marshalSSEBeadPayload(BeadEvent{
		ID:       "task-20",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "matt-1",
	})
	a.handleUpdated(context.Background(), claimedTask)
	if len(notif.getTaskUpdates()) != 1 {
		t.Fatalf("expected 1 task update after claim, got %d", len(notif.getTaskUpdates()))
	}

	// Step 2: Task unclaimed (assignee cleared) — should refresh matt-1's card.
	unclaimedTask := marshalSSEBeadPayload(BeadEvent{
		ID:     "task-20",
		Type:   "task",
		Status: "open",
		// No assignee — cleared.
	})
	a.handleUpdated(context.Background(), unclaimedTask)

	updates := notif.getTaskUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 task updates (claim + unclaim), got %d", len(updates))
	}
	if updates[1] != "matt-1" {
		t.Errorf("expected previous assignee matt-1 notified on unclaim, got %s", updates[1])
	}
}

// TestAgents_HandleClosed_TaskBeadPreviousAssignee verifies that closing a task
// bead with empty assignee still refreshes the previous assignee's card.
func TestAgents_HandleClosed_TaskBeadPreviousAssignee(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Step 1: Task claimed by builder-2 — track the assignee.
	claimedTask := marshalSSEBeadPayload(BeadEvent{
		ID:       "task-21",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "builder-2",
	})
	a.handleUpdated(context.Background(), claimedTask)

	// Step 2: Task closed with empty assignee — should still refresh builder-2's card.
	closedTask := marshalSSEBeadPayload(BeadEvent{
		ID:     "task-21",
		Type:   "task",
		Status: "closed",
		// Assignee cleared before close.
	})
	a.handleClosed(context.Background(), closedTask)

	updates := notif.getTaskUpdates()
	if len(updates) != 2 {
		t.Fatalf("expected 2 task updates (claim + close), got %d", len(updates))
	}
	if updates[1] != "builder-2" {
		t.Errorf("expected previous assignee builder-2 notified on close, got %s", updates[1])
	}
}

// TestAgents_HandleUpdated_TaskReassigned verifies that when a task is
// reassigned from one agent to another, the previous agent's card is refreshed.
func TestAgents_HandleUpdated_TaskReassigned(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	// Step 1: Task claimed by matt-1.
	claimedTask := marshalSSEBeadPayload(BeadEvent{
		ID:       "task-22",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "matt-1",
	})
	a.handleUpdated(context.Background(), claimedTask)

	// Step 2: Task reassigned to matt-2 — should notify matt-2 (new) AND matt-1 (previous).
	reassignedTask := marshalSSEBeadPayload(BeadEvent{
		ID:       "task-22",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "matt-2",
	})
	a.handleUpdated(context.Background(), reassignedTask)

	updates := notif.getTaskUpdates()
	// 3 updates: matt-1 (claim), matt-2 (new assignee), matt-1 (previous assignee refresh).
	if len(updates) != 3 {
		t.Fatalf("expected 3 task updates (claim + new assignee + previous assignee), got %d: %v", len(updates), updates)
	}
	if updates[0] != "matt-1" {
		t.Errorf("expected matt-1 notified on claim, got %s", updates[0])
	}
	if updates[1] != "matt-2" {
		t.Errorf("expected matt-2 notified on reassign, got %s", updates[1])
	}
	if updates[2] != "matt-1" {
		t.Errorf("expected matt-1 (previous assignee) notified on reassign, got %s", updates[2])
	}

	// Verify tracking was updated: unassigning should now notify matt-2, not matt-1.
	unclaimedTask := marshalSSEBeadPayload(BeadEvent{
		ID:     "task-22",
		Type:   "task",
		Status: "open",
	})
	a.handleUpdated(context.Background(), unclaimedTask)

	updates = notif.getTaskUpdates()
	if len(updates) != 4 {
		t.Fatalf("expected 4 task updates, got %d: %v", len(updates), updates)
	}
	if updates[3] != "matt-2" {
		t.Errorf("expected matt-2 (last assignee) notified on unclaim, got %s", updates[3])
	}
}

// TestAgents_HandleClosed_WorkingState verifies that an agent bead closing with
// agent_state="working" gets overridden to "done" (not left as stale "working").
func TestAgents_HandleClosed_WorkingState(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	workingAgent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-13",
		Type:     "agent",
		Assignee: "gardenio/crew/busy-bot",
		Fields: map[string]string{
			"agent_state": "working",
		},
	})
	a.handleClosed(context.Background(), workingAgent)

	if len(notif.getCrashes()) != 0 {
		t.Error("working agent closing should not trigger crash notification")
	}
	changes := notif.getStateChanges()
	if len(changes) != 1 {
		t.Fatalf("expected 1 state change notification, got %d", len(changes))
	}
	if changes[0].Fields["agent_state"] != "done" {
		t.Errorf("expected agent_state overridden to done, got %q", changes[0].Fields["agent_state"])
	}
}

// TestAgents_HandleUpdated_StateChange verifies that non-crash state changes
// (e.g. spawning→working) trigger NotifyAgentState, not NotifyAgentCrash.
func TestAgents_HandleUpdated_StateChange(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{
		Notifier: notif,
		Logger:   slog.Default(),
	})

	workingAgent := marshalSSEBeadPayload(BeadEvent{
		ID:       "agent-6",
		Type:     "agent",
		Assignee: "gardenio/crew/runner",
		Fields: map[string]string{
			"agent_state": "working",
			"pod_phase":   "running",
		},
	})
	a.handleUpdated(context.Background(), workingAgent)

	if len(notif.getCrashes()) != 0 {
		t.Error("working state should not trigger crash notification")
	}
	changes := notif.getStateChanges()
	if len(changes) != 1 {
		t.Fatalf("expected 1 state change notification, got %d", len(changes))
	}
	if changes[0].Fields["agent_state"] != "working" {
		t.Errorf("expected agent_state=working, got %q", changes[0].Fields["agent_state"])
	}
}

// Agent card, wrapup, and imageTag tests are in agents_card_test.go.
