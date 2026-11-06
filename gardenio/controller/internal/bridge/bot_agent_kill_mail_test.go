package bridge

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestMain shrinks the control:kill wait for the whole package: every
// non-force kill in a test would otherwise wait killWaitMax for an agent that
// has no subscriber to stop it.
func TestMain(m *testing.M) {
	killWaitPoll = 2 * time.Millisecond
	killWaitMax = 20 * time.Millisecond
	os.Exit(m.Run())
}

func TestKillAgent_SendsControlKillMailThenCloses(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "working"}}
	b := &Bot{daemon: daemon, logger: slog.Default(), agentCards: map[string]MessageRef{}}
	origPoll, origMax := killWaitPoll, killWaitMax
	killWaitPoll = 5 * time.Millisecond
	killWaitMax = 50 * time.Millisecond
	defer func() { killWaitPoll, killWaitMax = origPoll, origMax }()

	if err := b.killAgent(context.Background(), "crew-x", false); err != nil {
		t.Fatalf("killAgent: %v", err)
	}
	req := daemon.lastCreateBead()
	if req.Type != "mail" || req.Assignee != "crew-x" || !hasLabel(req.Labels, "control:kill") {
		t.Fatalf("expected control:kill mail to crew-x, got %+v", req)
	}
	if len(daemon.closed) != 1 || daemon.closed[0].BeadID != "gd-a1" {
		t.Fatalf("agent bead should be closed after the wait, got %+v", daemon.closed)
	}
	if len(daemon.closedMails) != 1 {
		t.Fatalf("an unread kill mail must be closed so the next session does not drain it, got %v", daemon.closedMails)
	}
}

func TestKillAgent_DeadAgent_SkipsMail(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "done"}}
	b := &Bot{daemon: daemon, logger: slog.Default(), agentCards: map[string]MessageRef{}}
	if err := b.killAgent(context.Background(), "crew-x", false); err != nil {
		t.Fatalf("killAgent: %v", err)
	}
	if len(daemon.mails) != 0 {
		t.Fatal("a dead agent has no session to stop; no kill mail")
	}
}

func TestKillAgent_Force_SkipsMail(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open"}
	b := &Bot{daemon: daemon, logger: slog.Default(), agentCards: map[string]MessageRef{}}
	if err := b.killAgent(context.Background(), "crew-x", true); err != nil {
		t.Fatalf("killAgent --force: %v", err)
	}
	if len(daemon.mails) != 0 {
		t.Fatal("--force must not send mail")
	}
}

// TestKillAgent_WaitReadsTheBeadID: the wait for the agent to stop reads the
// agent bead by id, so it ends as soon as that bead reads dead.
func TestKillAgent_WaitReadsTheBeadID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["crew-x"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "working"}}
	daemon.beads["gd-a1"] = &beadsapi.BeadDetail{ID: "gd-a1", Title: "crew-x", Status: "open", Fields: map[string]string{"agent_state": "done"}}
	b := &Bot{daemon: daemon, logger: slog.Default(), agentCards: map[string]MessageRef{}}
	origPoll, origMax := killWaitPoll, killWaitMax
	killWaitPoll = 5 * time.Millisecond
	killWaitMax = 10 * time.Second
	defer func() { killWaitPoll, killWaitMax = origPoll, origMax }()

	start := time.Now()
	if err := b.killAgent(context.Background(), "crew-x", false); err != nil {
		t.Fatalf("killAgent: %v", err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("the wait should end once gd-a1 reads dead, took %s", took)
	}
}
