package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestClaimed_NudgeAgent_AgentNotFound(t *testing.T) {
	daemon := newMockDaemon()
	// No agent bead seeded — FindAgentBead will fail.

	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	// nudgeAgent should log error but not panic.
	c.nudgeAgent(context.Background(), BeadEvent{
		ID:       "gd-abc",
		Type:     "task",
		Title:    "Some task",
		Assignee: "nonexistent-agent",
	})
	// No panic = pass.
}

func TestClaimed_NudgeAgent_NoCoopURL(t *testing.T) {
	daemon := newMockDaemon()
	// Agent exists but has no coop_url in notes.
	daemon.beads["my-agent"] = &beadsapi.BeadDetail{
		ID:    "my-agent",
		Notes: "", // empty notes
	}

	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	c.nudgeAgent(context.Background(), BeadEvent{
		ID:       "gd-abc",
		Type:     "task",
		Title:    "Some task",
		Assignee: "my-agent",
	})
	// No panic = pass; should log error about missing coop_url.
}

func TestClaimed_NudgeAgent_Success(t *testing.T) {
	var nudgeMu sync.Mutex
	var nudgeMessage string

	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeMu.Lock()
			nudgeMessage = body["message"]
			nudgeMu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockDaemon()
	daemon.beads["my-agent"] = &beadsapi.BeadDetail{
		ID:    "my-agent",
		Notes: "coop_url: " + coopServer.URL,
	}

	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	c.nudgeAgent(context.Background(), BeadEvent{
		ID:       "gd-abc",
		Type:     "task",
		Title:    "Fix the bug",
		Assignee: "my-agent",
	})

	time.Sleep(50 * time.Millisecond)
	nudgeMu.Lock()
	msg := nudgeMessage
	nudgeMu.Unlock()

	if msg == "" {
		t.Fatal("expected nudge to be sent")
	}
	want := `Your claimed bead gd-abc "Fix the bug" was updated — run 'kd show gd-abc' to review`
	if msg != want {
		t.Errorf("unexpected nudge message:\n  got:  %s\n  want: %s", msg, want)
	}
}

func TestClaimed_HandleClosed_AgentNotFound(t *testing.T) {
	daemon := newMockDaemon()
	// No agent bead — FindAgentBead will fail.

	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-closed",
		Type:     "task",
		Title:    "Closed task",
		Assignee: "nonexistent-agent",
	})
	c.handleClosed(context.Background(), data)
	// Should not panic; logs error and returns.
}

func TestClaimed_HandleClosed_NoCoopURL(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["my-agent"] = &beadsapi.BeadDetail{
		ID:    "my-agent",
		Notes: "", // no coop_url
	}

	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-closed-2",
		Type:     "task",
		Title:    "Closed task",
		Assignee: "my-agent",
	})
	c.handleClosed(context.Background(), data)
	// Should not panic; logs warning about missing coop_url.
}

func TestClaimed_HandleClosed_CoopNudgeFails(t *testing.T) {
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return 500 to simulate failure.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer coopServer.Close()

	daemon := newMockDaemon()
	daemon.beads["my-agent"] = &beadsapi.BeadDetail{
		ID:    "my-agent",
		Notes: "coop_url: " + coopServer.URL,
	}

	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-closed-fail",
		Type:     "task",
		Title:    "Closed task",
		Assignee: "my-agent",
	})
	c.handleClosed(context.Background(), data)
	// Should not panic; logs error about nudge failure.
}

func TestClaimed_HandleClosed_RateLimitedSecondCall(t *testing.T) {
	nudgeCount := 0
	var mu sync.Mutex

	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			mu.Lock()
			nudgeCount++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockDaemon()
	daemon.beads["my-agent"] = &beadsapi.BeadDetail{
		ID:    "my-agent",
		Notes: "coop_url: " + coopServer.URL,
	}

	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-closed-rl",
		Type:     "task",
		Title:    "Rate limited close",
		Assignee: "my-agent",
	})

	// First call: nudge fires.
	c.handleClosed(context.Background(), data)
	time.Sleep(50 * time.Millisecond)

	// Second call within TTL: should be rate-limited.
	c.handleClosed(context.Background(), data)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := nudgeCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 nudge (rate limited), got %d", count)
	}
}

func TestClaimed_ShouldNudge_CleansUpExpired(t *testing.T) {
	c := &Claimed{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	// Manually insert an expired entry.
	c.nudgedMu.Lock()
	c.nudged["old-bead"] = time.Now().Add(-10 * time.Minute)
	c.nudgedMu.Unlock()

	// Call shouldNudge for a new bead — should clean up the expired entry.
	c.shouldNudge("new-bead")

	c.nudgedMu.Lock()
	_, oldExists := c.nudged["old-bead"]
	c.nudgedMu.Unlock()

	if oldExists {
		t.Error("expected expired entry to be cleaned up")
	}
}
