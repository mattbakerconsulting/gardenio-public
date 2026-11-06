package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestMail_HandleCreated_NonMail_Ignored(t *testing.T) {
	m := &Mail{
		logger: slog.Default(),
	}

	// Non-mail bead should be ignored (no panic, no action).
	nonMail := marshalSSEBeadPayload(BeadEvent{
		ID:   "abc",
		Type: "agent",
	})
	m.handleCreated(context.Background(), nonMail)
	// No panic = pass.
}

func TestMail_HandleCreated_DoesNotDial(t *testing.T) {
	dialed := false
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
		w.WriteHeader(http.StatusOK)
	}))
	defer coopServer.Close()
	daemon := newMockDaemon()
	daemon.beads["crew-proj-devops-builder"] = &beadsapi.BeadDetail{ID: "crew-proj-devops-builder", Notes: "coop_url: " + coopServer.URL}
	m := &Mail{daemon: daemon, logger: slog.Default()}
	m.handleCreated(context.Background(), marshalSSEBeadPayload(BeadEvent{ID: "gd-m1", Type: "mail", Assignee: "crew-proj-devops-builder", Labels: []string{"delivery:interrupt"}}))
	if dialed {
		t.Fatal("the bridge mail watcher must no longer dial coop; the agent subscribes to its own mail")
	}
}

func TestMail_HandleCreated_QueueDelivery_NormalPriority_NoNudge(t *testing.T) {
	// Daemon should NOT be called for queue delivery + normal priority.
	daemon := newMockDaemon()

	m := &Mail{
		daemon: daemon,
		logger: slog.Default(),
	}

	event := marshalSSEBeadPayload(BeadEvent{
		ID:       "mail-3",
		Type:     "mail",
		Title:    "FYI update",
		Assignee: "crew-proj-devops-builder",
		Labels:   []string{"from:myproject/ops", "delivery:queue"},
		Priority: 2, // Normal priority.
	})
	m.handleCreated(context.Background(), event)

	if daemon.getGetCalls() != 0 {
		t.Fatalf("expected no daemon calls for queue+normal priority, got %d", daemon.getGetCalls())
	}
}

func TestMail_HandleCreated_NoAssignee_NoNudge(t *testing.T) {
	// Missing assignee should log warning but not panic.
	m := &Mail{
		logger: slog.Default(),
	}

	event := marshalSSEBeadPayload(BeadEvent{
		ID:       "mail-4",
		Type:     "mail",
		Title:    "Orphaned mail",
		Labels:   []string{"from:someone", "delivery:interrupt"},
		Priority: 0, // Critical but no assignee.
	})
	m.handleCreated(context.Background(), event)
	// No panic = pass.
}
