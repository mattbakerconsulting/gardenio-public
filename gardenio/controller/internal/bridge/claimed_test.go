package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"log/slog"
)

func TestClaimed_HandleUpdated_NonActionable_Ignored(t *testing.T) {
	c := &Claimed{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	for _, typ := range []string{"agent", "decision", "mail", "project", "report"} {
		data := marshalSSEBeadPayload(BeadEvent{
			ID:       "bd-1",
			Type:     typ,
			Assignee: "some-agent",
		})
		c.handleUpdated(context.Background(), data)
	}
	// No panic = pass; daemon was not called.
}

func TestClaimed_HandleUpdated_NoAssignee_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:   "bd-2",
		Type: "task",
		// No assignee.
	})
	c.handleUpdated(context.Background(), data)

	if daemon.getGetCalls() != 0 {
		t.Fatalf("expected no daemon calls for unassigned bead, got %d", daemon.getGetCalls())
	}
}

func TestClaimed_HandleUpdated_ClaimedTask_Nudges(t *testing.T) {
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

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-abc",
		Type:     "task",
		Title:    "Fix the bug",
		Assignee: "my-agent",
	})
	c.handleUpdated(context.Background(), data)

	time.Sleep(50 * time.Millisecond)
	nudgeMu.Lock()
	msg := nudgeMessage
	nudgeMu.Unlock()

	if msg == "" {
		t.Fatal("expected coop nudge for claimed task update, got none")
	}
	want := `Your claimed bead gd-abc "Fix the bug" was updated — run 'kd show gd-abc' to review`
	if msg != want {
		t.Errorf("unexpected nudge message:\n  got:  %s\n  want: %s", msg, want)
	}
}

// marshalSSEUpdatePayload builds a BeadUpdated SSE payload with the mutation
// actor at the wrapper level, mirroring the kbeads daemon's events.BeadUpdated.
func marshalSSEUpdatePayload(bead BeadEvent, actor string) []byte {
	data, _ := json.Marshal(map[string]any{"bead": bead, "actor": actor})
	return data
}

func TestClaimed_HandleUpdated_SelfActor_NoNudge(t *testing.T) {
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

	// Actor == assignee: the agent's own update (e.g. its own kd claim).
	data := marshalSSEUpdatePayload(BeadEvent{
		ID:       "gd-self",
		Type:     "task",
		Title:    "Self-claimed bead",
		Assignee: "my-agent",
	}, "my-agent")
	c.handleUpdated(context.Background(), data)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := nudgeCount
	mu.Unlock()

	if count != 0 {
		t.Errorf("expected no nudge for self-originated update, got %d", count)
	}

	// Self-updates must not consume the rate-limit slot: a later external
	// update to the same bead should still nudge.
	external := marshalSSEUpdatePayload(BeadEvent{
		ID:       "gd-self",
		Type:     "task",
		Title:    "Self-claimed bead",
		Assignee: "my-agent",
	}, "someone-else")
	c.handleUpdated(context.Background(), external)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count = nudgeCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 nudge for external update after self-update, got %d", count)
	}
}

func TestClaimed_HandleUpdated_SelfActor_CaseInsensitive_NoNudge(t *testing.T) {
	daemon := newMockDaemon()
	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEUpdatePayload(BeadEvent{
		ID:       "gd-self-case",
		Type:     "task",
		Title:    "Case-folded self update",
		Assignee: "My-Agent",
	}, "my-agent")
	c.handleUpdated(context.Background(), data)

	if daemon.getGetCalls() != 0 {
		t.Fatalf("expected no daemon calls for case-folded self update, got %d", daemon.getGetCalls())
	}
}

func TestClaimed_HandleUpdated_ExternalActor_Nudges(t *testing.T) {
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

	data := marshalSSEUpdatePayload(BeadEvent{
		ID:       "gd-external",
		Type:     "task",
		Title:    "Externally updated bead",
		Assignee: "my-agent",
	}, "other-agent")
	c.handleUpdated(context.Background(), data)

	time.Sleep(50 * time.Millisecond)
	nudgeMu.Lock()
	msg := nudgeMessage
	nudgeMu.Unlock()

	if msg == "" {
		t.Fatal("expected coop nudge for externally-updated claimed bead, got none")
	}
}

func TestClaimed_HandleUpdated_EmptyActor_Nudges(t *testing.T) {
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

	// Empty actor (auth disabled / anonymous): keep current behavior — nudge.
	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-anon",
		Type:     "task",
		Title:    "Anonymously updated bead",
		Assignee: "my-agent",
	})
	c.handleUpdated(context.Background(), data)

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := nudgeCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 nudge for empty-actor update, got %d", count)
	}
}

func TestClaimed_HandleUpdated_RateLimit_SecondNudgeSuppressed(t *testing.T) {
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
		ID:       "gd-ratelimit",
		Type:     "task",
		Title:    "Rate-limited bead",
		Assignee: "my-agent",
	})

	// First update: nudge fires.
	c.handleUpdated(context.Background(), data)
	time.Sleep(50 * time.Millisecond)

	// Second update within TTL: nudge should be suppressed.
	c.handleUpdated(context.Background(), data)
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := nudgeCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 nudge (rate limited), got %d", count)
	}
}

func TestClaimed_HandleUpdated_DifferentBeads_BothNudge(t *testing.T) {
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

	// Two different beads — each should get its own nudge.
	for _, id := range []string{"gd-bead-1", "gd-bead-2"} {
		data := marshalSSEBeadPayload(BeadEvent{
			ID:       id,
			Type:     "feature",
			Title:    "Bead " + id,
			Assignee: "my-agent",
		})
		c.handleUpdated(context.Background(), data)
	}

	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	count := nudgeCount
	mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 nudges for 2 different beads, got %d", count)
	}
}

func TestClaimed_HandleUpdated_MalformedPayload_NoAction(t *testing.T) {
	c := &Claimed{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}
	c.handleUpdated(context.Background(), []byte("not-json"))
	// No panic = pass.
}

func TestClaimed_HandleClosed_NonActionable_Ignored(t *testing.T) {
	c := &Claimed{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	for _, typ := range []string{"agent", "decision", "mail", "project", "report"} {
		data := marshalSSEBeadPayload(BeadEvent{
			ID:       "bd-1",
			Type:     typ,
			Assignee: "some-agent",
		})
		c.handleClosed(context.Background(), data)
	}
	// No panic = pass; daemon was not called.
}

func TestClaimed_HandleClosed_NoAssignee_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	c := &Claimed{
		daemon: daemon,
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:   "bd-closed-no-assignee",
		Type: "task",
		// No assignee.
	})
	c.handleClosed(context.Background(), data)

	if daemon.getGetCalls() != 0 {
		t.Fatalf("expected no daemon calls for unassigned closed bead, got %d", daemon.getGetCalls())
	}
}

func TestClaimed_HandleClosed_ClaimedTask_NudgesCheckpoint(t *testing.T) {
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

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-xyz",
		Type:     "task",
		Title:    "Finished work",
		Assignee: "my-agent",
	})
	c.handleClosed(context.Background(), data)

	time.Sleep(50 * time.Millisecond)
	nudgeMu.Lock()
	msg := nudgeMessage
	nudgeMu.Unlock()

	if msg == "" {
		t.Fatal("expected coop nudge for claimed task closure, got none")
	}
	want := `Your claimed bead gd-xyz "Finished work" was closed — if all work is done, wrap up and call gb stop; only create a gb decision if you are blocked on a human choice. If you ALREADY filed a decision and are only waiting on the operator, gb stop too — the decision persists as your re-entry handle and auto-respawns you when it resolves; keep gb yield for a decision you need answered to finish THIS turn`
	if msg != want {
		t.Errorf("unexpected nudge message:\n  got:  %s\n  want: %s", msg, want)
	}
}

func TestClaimed_HandleClosed_MalformedPayload_NoAction(t *testing.T) {
	c := &Claimed{
		logger: slog.Default(),
		nudged: make(map[string]time.Time),
	}
	c.handleClosed(context.Background(), []byte("not-json"))
	// No panic = pass.
}
