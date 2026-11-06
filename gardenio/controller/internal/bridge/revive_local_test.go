package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"
)

// seedLaptopRequester mirrors seedDismissDecisionFixture, but the requesting
// agent is a laptop agent under a runner whose session has ended: gb closed
// its bead at exit, so the nudge fails and the caller reaches the revival
// path, which must write mail rather than spawn a pod (spec §7, D5).
func seedLaptopRequester(daemon *mockDaemon, requestingTask string) *beadsapi.BeadDetail {
	reqAgent := &beadsapi.BeadDetail{
		ID:     "bd-local-agent",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":            "local-laptop",
			"agent_state":      "done",
			"execution_target": "laptop",
			"runtime_id":       "gd-runner-1",
			"project":          "gardenio",
			"task_id":          requestingTask,
			"role":             "local",
		},
	}
	daemon.beads["local-laptop"] = reqAgent
	daemon.beads["bd-local-agent"] = reqAgent
	if requestingTask != "" {
		daemon.beads[requestingTask] = &beadsapi.BeadDetail{
			ID: requestingTask, Type: "task", Status: "open",
			Title: "write the P4 plan", Assignee: "local-laptop",
		}
	}
	return reqAgent
}

func laptopDecisionEvent(requestingTask, chosen string) []byte {
	fields := map[string]string{
		"prompt":                   "P4 spec review: approve or request changes?",
		"chosen":                   chosen,
		"requesting_agent_bead_id": "bd-local-agent",
	}
	if requestingTask != "" {
		fields["requesting_task"] = requestingTask
	}
	return marshalSSEBeadPayload(BeadEvent{
		ID: "dec-local-1", Type: "decision", Assignee: "local-laptop",
		Labels: []string{"project:gardenio"}, Fields: fields,
	})
}

// spawnsWithSource counts the agent beads the mock's SpawnAgent recorded with
// the given spawn_source.
func spawnsWithSource(m *mockDaemon, source string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == source {
			n++
		}
	}
	return n
}

func TestDecisions_HandleClosed_LaptopRequester_SendsMailNeverSpawns(t *testing.T) {
	daemon := newMockDaemon()
	seedLaptopRequester(daemon, "gd-plan-task")
	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})

	d.handleClosed(context.Background(), laptopDecisionEvent("gd-plan-task", "approve"))

	if spawned := findDecisionResumeSpawn(daemon); spawned != nil {
		t.Fatalf("a laptop agent must never be respawned as a pod; got spawn %q", spawned.ID)
	}
	mails := mailsTo(daemon, "local-laptop")
	if len(mails) != 1 {
		t.Fatalf("expected one interrupt mail to local-laptop carrying the decision outcome, got %d", len(mails))
	}
	mail := mails[0]
	if !strings.Contains(mail.Description, "dec-local-1") || !strings.Contains(mail.Description, "approve") {
		t.Errorf("mail must name the decision and the chosen option; got:\n%s", mail.Description)
	}
	if !hasLabel(mail.Labels, mailproto.LabelDeliveryInterrupt) {
		t.Errorf("revival mail must be delivery:interrupt; labels=%v", mail.Labels)
	}
	if got := daemon.beads["bd-local-agent"].Status; got != "closed" {
		t.Errorf("the revival path must not touch the laptop agent bead, got status %q", got)
	}
}

// A redelivered close event hits the decision dedup and writes no second mail.
func TestDecisions_HandleClosed_LaptopRequester_RedeliveryOneMail(t *testing.T) {
	daemon := newMockDaemon()
	seedLaptopRequester(daemon, "gd-plan-task")
	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})

	d.handleClosed(context.Background(), laptopDecisionEvent("gd-plan-task", "approve"))
	d.handleClosed(context.Background(), laptopDecisionEvent("gd-plan-task", "approve"))

	if n := len(mailsTo(daemon, "local-laptop")); n != 1 {
		t.Fatalf("want one mail across two deliveries of the close event, got %d", n)
	}
}

func TestDecisions_HandleClosed_PodRequester_StillSpawns(t *testing.T) {
	daemon := newMockDaemon()
	seedDismissDecisionFixture(daemon, "gd-gate-task")
	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})

	d.handleClosed(context.Background(), dismissDecisionEvent("gd-gate-task", "approve"))

	if findDecisionResumeSpawn(daemon) == nil {
		t.Fatal("a pod requester keeps the existing decision-resume respawn")
	}
}

func TestEscalations_Respawn_LaptopRequester_SendsMailNeverSpawns(t *testing.T) {
	daemon := newMockDaemon()
	agent := seedLaptopRequester(daemon, "gd-plan-task")
	e := NewEscalations(EscalationsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})
	ev := BeadEvent{
		ID: "esc-1", Type: "escalation", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"requesting_task": "gd-plan-task", "result_status": "success"},
	}

	e.respawnRequestingAgent(context.Background(), "local-laptop", ev, agent, "esc-1 executed: success")

	if n := spawnsWithSource(daemon, "escalation-resume"); n != 0 {
		t.Fatalf("escalation follow-through must not spawn a pod for a laptop agent; spawns=%d", n)
	}
	mails := mailsTo(daemon, "local-laptop")
	if len(mails) != 1 || !strings.Contains(mails[0].Description, "esc-1 executed: success") {
		t.Fatalf("expected one interrupt mail to local-laptop carrying the escalation result, got %+v", mails)
	}
}

// execution_target=local is what the escalation bridge stamps on the fleet
// pods it spawns for a local-fallback escalation (gd-SzPJDNTBXS). Such an
// executor is a pod, so its follow-through still spawns.
func TestEscalations_Respawn_FleetLocalExecutor_StillSpawns(t *testing.T) {
	daemon := newMockDaemon()
	agent := seedLaptopRequester(daemon, "gd-plan-task")
	agent.Fields["execution_target"] = "local"
	e := NewEscalations(EscalationsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})
	ev := BeadEvent{
		ID: "esc-2", Type: "escalation", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"requesting_task": "gd-plan-task", "result_status": "success"},
	}

	e.respawnRequestingAgent(context.Background(), "local-laptop", ev, agent, "esc-2 executed: success")

	if n := spawnsWithSource(daemon, "escalation-resume"); n != 1 {
		t.Fatalf("a fleet executor with execution_target=local keeps its follow-through respawn; spawns=%d", n)
	}
	if n := len(mailsTo(daemon, "local-laptop")); n != 0 {
		t.Fatalf("no revival mail for a pod agent, got %d", n)
	}
}

// The laptop label alone marks a laptop agent too (gb runner join stamps both).
func TestReviveLocalAgentByMail_LabelOnly(t *testing.T) {
	daemon := newMockDaemon()
	agent := &beadsapi.BeadDetail{
		ID: "bd-l", Title: "label-laptop", Status: "closed",
		Labels: []string{"execution_target:laptop"},
		Fields: map[string]string{},
	}
	if !reviveLocalAgentByMail(context.Background(), daemon, slog.Default(), agent, "why", "what") {
		t.Fatal("a bead labelled execution_target:laptop is a laptop agent")
	}
	mails := mailsTo(daemon, "label-laptop")
	if len(mails) != 1 || mails[0].Description != "why\n\nwhat" {
		t.Fatalf("want one mail to the bead title with body reason+blank line+message, got %+v", mails)
	}
	if reviveLocalAgentByMail(context.Background(), daemon, slog.Default(), nil, "why", "what") {
		t.Fatal("no agent bead is not a laptop agent")
	}
}
