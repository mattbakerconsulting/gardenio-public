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

func TestAdviceNudge_HandleUpdated_NonAdvice_Ignored(t *testing.T) {
	a := &AdviceNudge{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	for _, typ := range []string{"task", "agent", "mail", "decision"} {
		data := marshalSSEBeadPayload(BeadEvent{
			ID:     "bd-1",
			Type:   typ,
			Labels: []string{"global"},
		})
		a.handleUpdated(context.Background(), data)
	}
	// No panic = pass; daemon was not called.
}

func TestAdviceNudge_HandleUpdated_ClosedAdvice_Ignored(t *testing.T) {
	a := &AdviceNudge{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-2",
		Type:   "advice",
		Status: "closed",
		Labels: []string{"global"},
	})
	a.handleUpdated(context.Background(), data)
	// No panic = pass; no agents were listed.
}

func TestAdviceNudge_HandleUpdated_GlobalAdvice_NudgesMatchingAgent(t *testing.T) {
	var nudgeMu sync.Mutex
	var nudgeMessages []string

	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeMu.Lock()
			nudgeMessages = append(nudgeMessages, body["message"])
			nudgeMu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockDaemon()
	// Running agent.
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{
		ID:   "test-agent",
		Type: "agent",
		Fields: map[string]string{
			"project":     "gardenio",
			"role":        "crew",
			"agent":       "test-agent",
			"agent_state": "working",
		},
		Notes: "coop_url: " + coopServer.URL,
	}
	// Dead agent (should not be nudged).
	daemon.beads["dead-agent"] = &beadsapi.BeadDetail{
		ID:   "dead-agent",
		Type: "agent",
		Fields: map[string]string{
			"project":     "gardenio",
			"role":        "crew",
			"agent":       "dead-agent",
			"agent_state": "done",
		},
		Notes: "coop_url: " + coopServer.URL,
	}

	a := &AdviceNudge{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "gd-advice-1",
		Type:   "advice",
		Title:  "Use feature flags for rollout",
		Status: "open",
		Labels: []string{"global"},
	})
	a.handleUpdated(context.Background(), data)

	time.Sleep(100 * time.Millisecond)
	nudgeMu.Lock()
	msgs := nudgeMessages
	nudgeMu.Unlock()

	if len(msgs) != 1 {
		t.Fatalf("expected 1 nudge (only running agent), got %d", len(msgs))
	}
	if msgs[0] == "" {
		t.Fatal("nudge message was empty")
	}
}

func TestAdviceNudge_HandleUpdated_ProjectScoped_OnlyMatchingAgent(t *testing.T) {
	var nudgeMu sync.Mutex
	var nudgeMessages []string

	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeMu.Lock()
			nudgeMessages = append(nudgeMessages, body["message"])
			nudgeMu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	daemon := newMockDaemon()
	// Gardenio agent.
	daemon.beads["gardenio-agent"] = &beadsapi.BeadDetail{
		ID:   "gardenio-agent",
		Type: "agent",
		Fields: map[string]string{
			"project":     "gardenio",
			"role":        "crew",
			"agent":       "gardenio-agent",
			"agent_state": "working",
		},
		Notes: "coop_url: " + coopServer.URL,
	}
	// Monorepo agent (different project).
	daemon.beads["mono-agent"] = &beadsapi.BeadDetail{
		ID:   "mono-agent",
		Type: "agent",
		Fields: map[string]string{
			"project":     "monorepo",
			"role":        "crew",
			"agent":       "mono-agent",
			"agent_state": "working",
		},
		Notes: "coop_url: " + coopServer.URL,
	}

	a := &AdviceNudge{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	// Advice scoped to project:gardenio only — a project-wide broadcast needs
	// the scope:project-broadcast opt-in since Phase 3 (ka-0000007790).
	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "gd-advice-2",
		Type:   "advice",
		Title:  "Gardenio-specific tip",
		Status: "open",
		Labels: []string{"project:gardenio", "scope:project-broadcast"},
	})
	a.handleUpdated(context.Background(), data)

	time.Sleep(100 * time.Millisecond)
	nudgeMu.Lock()
	msgs := nudgeMessages
	nudgeMu.Unlock()

	if len(msgs) != 1 {
		t.Fatalf("expected 1 nudge (only gardenio agent matches), got %d", len(msgs))
	}

	// The same advice WITHOUT the opt-in is a denied broadcast: gb prime
	// drops it, so the nudge path must not tell agents to refresh for it.
	nudgeMu.Lock()
	nudgeMessages = nil
	nudgeMu.Unlock()
	denied := marshalSSEBeadPayload(BeadEvent{
		ID:     "gd-advice-3",
		Type:   "advice",
		Title:  "Denied bare-project broadcast",
		Status: "open",
		Labels: []string{"project:gardenio"},
	})
	a.handleUpdated(context.Background(), denied)

	time.Sleep(100 * time.Millisecond)
	nudgeMu.Lock()
	msgs = nudgeMessages
	nudgeMu.Unlock()

	if len(msgs) != 0 {
		t.Fatalf("expected 0 nudges for a denied bare-project broadcast, got %d", len(msgs))
	}
}

func TestAdviceNudge_HandleUpdated_RateLimit(t *testing.T) {
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
	daemon.beads["agent-a"] = &beadsapi.BeadDetail{
		ID:   "agent-a",
		Type: "agent",
		Fields: map[string]string{
			"project":     "gardenio",
			"role":        "crew",
			"agent":       "agent-a",
			"agent_state": "working",
		},
		Notes: "coop_url: " + coopServer.URL,
	}

	a := &AdviceNudge{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "gd-advice-rl",
		Type:   "advice",
		Title:  "Rate-limited advice",
		Status: "open",
		Labels: []string{"global"},
	})

	// First update: nudge fires.
	a.handleUpdated(context.Background(), data)
	time.Sleep(50 * time.Millisecond)

	// Second update within TTL: should be suppressed.
	a.handleUpdated(context.Background(), data)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := nudgeCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 nudge (rate limited), got %d", count)
	}
}

func TestAdviceNudge_HandleUpdated_NoRunningAgents(t *testing.T) {
	daemon := newMockDaemon()
	// Only a dead agent.
	daemon.beads["dead-agent"] = &beadsapi.BeadDetail{
		ID:   "dead-agent",
		Type: "agent",
		Fields: map[string]string{
			"project":     "gardenio",
			"role":        "crew",
			"agent":       "dead-agent",
			"agent_state": "done",
		},
	}

	a := &AdviceNudge{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "gd-advice-3",
		Type:   "advice",
		Title:  "No one listening",
		Status: "open",
		Labels: []string{"global"},
	})
	a.handleUpdated(context.Background(), data)
	// No panic = pass; no agents to nudge.
}

func TestAdviceNudge_HandleUpdated_MalformedPayload(t *testing.T) {
	a := &AdviceNudge{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}
	a.handleUpdated(context.Background(), []byte("not-json"))
	// No panic = pass.
}

func TestAdviceNudge_ShouldNudge_CleanupExpired(t *testing.T) {
	a := &AdviceNudge{
		nudged: make(map[string]time.Time),
	}

	// Manually insert an expired entry.
	a.nudged["old-advice"] = time.Now().Add(-adviceNudgeTTL - time.Minute)

	// shouldNudge for a new bead should clean up the expired entry.
	if !a.shouldNudge("new-advice") {
		t.Fatal("expected shouldNudge to return true for new bead")
	}

	if _, exists := a.nudged["old-advice"]; exists {
		t.Error("expected expired entry to be cleaned up")
	}
}
