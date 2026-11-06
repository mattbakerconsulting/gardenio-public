package subscriber

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// assertLastEventID asserts w.LastEventID() directly. The watcher updates
// lastEventID BEFORE emitting on w.Events(), so a test that has already
// received the event observes the updated ID via the channel's
// happens-before edge — no polling needed.
func assertLastEventID(t *testing.T, w *SSEWatcher, want string) {
	t.Helper()
	if got := w.LastEventID(); got != want {
		t.Fatalf("expected last event ID %s, got %s", want, got)
	}
}

// makeAgentPayload builds a JSON SSE payload for an agent bead with the given ID.
func makeAgentPayload(beadID string) []byte {
	payload := map[string]any{
		"bead": map[string]any{
			"id":     beadID,
			"type":   "agent",
			"status": "in_progress",
			"fields": map[string]string{"project": "p", "role": "devops", "agent": "a1"},
		},
	}
	data, _ := json.Marshal(payload)
	return data
}

// TestSSEWatcher_ParsesAgentCreatedEvent verifies that an SSE "beads.bead.created"
// event for an agent bead is correctly translated to an AgentSpawn lifecycle event.
func TestSSEWatcher_ParsesAgentCreatedEvent(t *testing.T) {
	// Start an SSE server that sends one agent created event then closes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events/stream" {
			http.NotFound(w, r)
			return
		}

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flushing")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send an agent bead created event.
		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-abc123",
				"title":  "test agent",
				"type":   "agent",
				"status": "in_progress",
				"labels": []string{"gt:agent"},
				"fields": map[string]string{
					"project": "kbeads",
					"role":    "devops",
					"agent":   "worker-1",
					"mode":    "crew",
				},
				"assignee":   "human",
				"created_by": "human",
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:1\n")
		fmt.Fprintf(w, "event:beads.bead.created\n")
		fmt.Fprintf(w, "data:%s\n\n", data)
		flusher.Flush()

		// Give the watcher time to read, then close.
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Topics:        "beads.bead.*",
		Namespace:     "test-ns",
		CoopImage:     "test-image:latest",
		BeadsGRPCAddr: "localhost:9090",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != AgentSpawn {
			t.Fatalf("expected AgentSpawn, got %s", event.Type)
		}
		if event.Project != "kbeads" {
			t.Fatalf("expected project kbeads, got %s", event.Project)
		}
		if event.Role != "devops" {
			t.Fatalf("expected role devops, got %s", event.Role)
		}
		if event.AgentName != "worker-1" {
			t.Fatalf("expected agent worker-1, got %s", event.AgentName)
		}
		if event.BeadID != "gd-abc123" {
			t.Fatalf("expected bead ID gd-abc123, got %s", event.BeadID)
		}
		// buildEvent no longer auto-injects cfg.Namespace; a bead with no
		// explicit `namespace` field leaves it unset so the gVisor freeze path
		// can route runsc agents (bug ka-0000002448). namespaceFromEvent supplies
		// the cfg.Namespace fallback downstream.
		if _, ok := event.Metadata["namespace"]; ok {
			t.Fatalf("expected no namespace pin, got %s", event.Metadata["namespace"])
		}
		if event.Metadata["image"] != "test-image:latest" {
			t.Fatalf("expected image test-image:latest, got %s", event.Metadata["image"])
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}

	cancel()

	// Verify last event ID was tracked.
	assertLastEventID(t, w, "1")
}

// TestSSEWatcher_ParsesAgentClosedEvent verifies that a closed event maps to AgentDone.
func TestSSEWatcher_ParsesAgentClosedEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-close1",
				"type":   "agent",
				"status": "closed",
				"fields": map[string]string{"project": "p", "role": "qa", "agent": "a1"},
			},
			"closed_by": "human",
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:42\nevent:beads.bead.closed\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != AgentDone {
			t.Fatalf("expected AgentDone, got %s", event.Type)
		}
		if event.BeadID != "gd-close1" {
			t.Fatalf("expected bead ID gd-close1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_SkipsNonAgentBeads verifies that non-agent beads are ignored.
func TestSSEWatcher_SkipsNonAgentBeads(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send a task bead (not agent).
		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-task1",
				"type":   "task",
				"status": "in_progress",
				"fields": map[string]string{"project": "p", "role": "devops", "agent": "a1"},
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:1\nevent:beads.bead.created\ndata:%s\n\n", data)
		flusher.Flush()

		// Then send an agent bead.
		payload2 := map[string]any{
			"bead": map[string]any{
				"id":     "gd-agent1",
				"type":   "agent",
				"status": "in_progress",
				"fields": map[string]string{"project": "p", "role": "devops", "agent": "a1"},
			},
		}
		data2, _ := json.Marshal(payload2)
		fmt.Fprintf(w, "id:2\nevent:beads.bead.created\ndata:%s\n\n", data2)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		// The first event should be the agent bead, not the task bead.
		if event.BeadID != "gd-agent1" {
			t.Fatalf("expected first event to be gd-agent1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_AgentStopOnUpdated verifies that updated events with
// agent_state=stopping map to AgentStop.
func TestSSEWatcher_AgentStopOnUpdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":          "gd-stop1",
				"type":        "agent",
				"status":      "in_progress",
				"agent_state": "stopping",
				"fields":      map[string]string{"project": "p", "role": "devops", "agent": "a1"},
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:5\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != AgentStop {
			t.Fatalf("expected AgentStop, got %s", event.Type)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_BudgetExceededOnUpdated verifies that updated events with
// agent_state=budget_exceeded map to BudgetExceeded.
func TestSSEWatcher_BudgetExceededOnUpdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":          "gd-budget1",
				"type":        "agent",
				"status":      "in_progress",
				"agent_state": "budget_exceeded",
				"fields":      map[string]string{"project": "myproj", "role": "crew", "agent": "worker-1"},
			},
			"changes": map[string]any{
				"agent_state": "budget_exceeded",
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:7\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != BudgetExceeded {
			t.Fatalf("expected BudgetExceeded, got %s", event.Type)
		}
		if event.Project != "myproj" {
			t.Fatalf("expected project myproj, got %s", event.Project)
		}
		if event.AgentName != "worker-1" {
			t.Fatalf("expected agent worker-1, got %s", event.AgentName)
		}
		if event.BeadID != "gd-budget1" {
			t.Fatalf("expected bead ID gd-budget1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_RateLimitedOnUpdated verifies that updated events with
// agent_state=rate_limited also map to BudgetExceeded (triggering auth failover).
func TestSSEWatcher_RateLimitedOnUpdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":          "gd-ratelim1",
				"type":        "agent",
				"status":      "in_progress",
				"agent_state": "rate_limited",
				"fields":      map[string]string{"project": "myproj", "role": "crew", "agent": "worker-2"},
			},
			"changes": map[string]any{
				"agent_state": "rate_limited",
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:8\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != BudgetExceeded {
			t.Fatalf("expected BudgetExceeded for rate_limited, got %s", event.Type)
		}
		if event.Project != "myproj" {
			t.Fatalf("expected project myproj, got %s", event.Project)
		}
		if event.AgentName != "worker-2" {
			t.Fatalf("expected agent worker-2, got %s", event.AgentName)
		}
		if event.BeadID != "gd-ratelim1" {
			t.Fatalf("expected bead ID gd-ratelim1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_KeepaliveIgnored verifies that keepalive comments don't break parsing.
func TestSSEWatcher_KeepaliveIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send keepalive comments before the actual event.
		fmt.Fprintf(w, ":keepalive\n\n")
		flusher.Flush()
		fmt.Fprintf(w, ":keepalive\n\n")
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-ka1",
				"type":   "agent",
				"status": "in_progress",
				"fields": map[string]string{"project": "p", "role": "devops", "agent": "a1"},
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:3\nevent:beads.bead.created\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.BeadID != "gd-ka1" {
			t.Fatalf("expected gd-ka1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_ConfigBeadUpdatedEmitsConfigUpdated verifies that an updated
// event for a config bead with a project label emits a ConfigUpdated lifecycle event.
func TestSSEWatcher_ConfigBeadUpdatedEmitsConfigUpdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-cfg1",
				"title":  "auth",
				"type":   "config",
				"status": "open",
				"labels": []string{"project:myproj"},
				"fields": map[string]string{},
			},
			"changes": map[string]any{
				"description": "updated account ref",
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:10\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != ConfigUpdated {
			t.Fatalf("expected ConfigUpdated, got %s", event.Type)
		}
		if event.Project != "myproj" {
			t.Fatalf("expected project myproj, got %s", event.Project)
		}
		if event.BeadID != "gd-cfg1" {
			t.Fatalf("expected bead ID gd-cfg1, got %s", event.BeadID)
		}
		if event.Metadata["title"] != "auth" {
			t.Fatalf("expected title auth, got %s", event.Metadata["title"])
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for config updated event")
	}
	cancel()
}

// TestSSEWatcher_ConfigBeadRoleScopedCarriesRole verifies that a role-scoped
// config bead's role: label is carried onto the ConfigUpdated event so the
// handler can restart only pods of that role (ka-0000002960).
func TestSSEWatcher_ConfigBeadRoleScopedCarriesRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-cfgrole",
				"title":  "claude-instructions",
				"type":   "config",
				"status": "open",
				"labels": []string{"claude-instructions", "project:myproj", "role:security-analyst"},
				"fields": map[string]string{},
			},
			"changes": map[string]any{"description": "authoring role"},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:11\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != ConfigUpdated {
			t.Fatalf("expected ConfigUpdated, got %s", event.Type)
		}
		if event.Project != "myproj" {
			t.Fatalf("expected project myproj, got %s", event.Project)
		}
		if event.Role != "security-analyst" {
			t.Fatalf("expected role security-analyst, got %q", event.Role)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for config updated event")
	}
	cancel()
}

// TestSSEWatcher_ConfigBeadNoRoleEmptyRole verifies that a config bead with no
// role: label emits a ConfigUpdated event with an empty Role, preserving the
// project-wide restart behavior (ka-0000002960).
func TestSSEWatcher_ConfigBeadNoRoleEmptyRole(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-cfgnorole",
				"title":  "claude-instructions",
				"type":   "config",
				"status": "open",
				"labels": []string{"claude-instructions", "project:myproj"},
				"fields": map[string]string{},
			},
			"changes": map[string]any{"description": "project-wide edit"},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:12\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != ConfigUpdated {
			t.Fatalf("expected ConfigUpdated, got %s", event.Type)
		}
		if event.Role != "" {
			t.Fatalf("expected empty role, got %q", event.Role)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for config updated event")
	}
	cancel()
}

// TestSSEWatcher_ConfigBeadNoProjectSkipped verifies that a config bead updated
// event without a project label is silently ignored.
func TestSSEWatcher_ConfigBeadNoProjectSkipped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Config bead with no project label — should be skipped.
		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-cfgnp",
				"title":  "auth",
				"type":   "config",
				"status": "open",
				"labels": []string{"global"},
				"fields": map[string]string{},
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:11\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()

		// Follow up with an agent bead to confirm we move past the config bead.
		payload2 := map[string]any{
			"bead": map[string]any{
				"id":     "gd-agent2",
				"type":   "agent",
				"status": "in_progress",
				"fields": map[string]string{"project": "p", "role": "devops", "agent": "a1"},
			},
		}
		data2, _ := json.Marshal(payload2)
		fmt.Fprintf(w, "id:12\nevent:beads.bead.created\ndata:%s\n\n", data2)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		// Should get the agent event, not the config event.
		if event.BeadID != "gd-agent2" {
			t.Fatalf("expected gd-agent2, got %s (type=%s)", event.BeadID, event.Type)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_ConfigCreatedIgnored verifies that config bead "created" events
// are not emitted (only "updated" triggers ConfigUpdated).
func TestSSEWatcher_ConfigCreatedIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Config bead created — should be ignored since type != agent.
		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-cfgcr",
				"title":  "auth",
				"type":   "config",
				"status": "open",
				"labels": []string{"project:myproj"},
				"fields": map[string]string{},
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:20\nevent:beads.bead.created\ndata:%s\n\n", data)
		flusher.Flush()

		// Follow with an agent bead.
		data2 := makeAgentPayload("gd-after-cfg")
		fmt.Fprintf(w, "id:21\nevent:beads.bead.created\ndata:%s\n\n", data2)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		// Should get the agent event, config created is not handled.
		if event.BeadID != "gd-after-cfg" {
			t.Fatalf("expected gd-after-cfg, got %s (type=%s)", event.BeadID, event.Type)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_AccountBeadUpdatedEmitsAccountUpdated verifies that an updated
// event for an account bead emits an AccountUpdated lifecycle event with
// throttle_status in the metadata.
func TestSSEWatcher_AccountBeadUpdatedEmitsAccountUpdated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-acct1",
				"title":  "my-account",
				"type":   "account",
				"status": "open",
				"labels": []string{},
				"fields": map[string]string{
					"credential":      "secret:my-secret",
					"provider":        "anthropic",
					"throttle_status": "rejected",
				},
			},
			"changes": map[string]any{
				"fields": "updated",
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:50\nevent:beads.bead.updated\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != AccountUpdated {
			t.Fatalf("expected AccountUpdated, got %s", event.Type)
		}
		if event.BeadID != "gd-acct1" {
			t.Fatalf("expected bead ID gd-acct1, got %s", event.BeadID)
		}
		if event.Metadata["title"] != "my-account" {
			t.Fatalf("expected title my-account, got %s", event.Metadata["title"])
		}
		if event.Metadata["throttle_status"] != "rejected" {
			t.Fatalf("expected throttle_status rejected, got %s", event.Metadata["throttle_status"])
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for account updated event")
	}
	cancel()
}

// TestSSEWatcher_SessionBeadClosedEmitsSessionDone verifies that a session
// bead close event maps to SessionDone with exit_code/project/role, and that
// it is metrics-only — no pod-operation event type (gd-5MTDWjwNjm).
func TestSSEWatcher_SessionBeadClosedEmitsSessionDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-session1",
				"type":   "session",
				"status": "closed",
				"labels": []string{},
				"fields": map[string]string{
					"project":   "gardenio",
					"role":      "crew",
					"exit_code": "143",
				},
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:60\nevent:beads.bead.closed\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.Type != SessionDone {
			t.Fatalf("expected SessionDone, got %s", event.Type)
		}
		if event.BeadID != "gd-session1" {
			t.Fatalf("expected bead ID gd-session1, got %s", event.BeadID)
		}
		if event.Project != "gardenio" {
			t.Fatalf("expected project gardenio, got %s", event.Project)
		}
		if event.Role != "crew" {
			t.Fatalf("expected role crew, got %s", event.Role)
		}
		if event.Metadata["exit_code"] != "143" {
			t.Fatalf("expected exit_code 143, got %s", event.Metadata["exit_code"])
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for session done event")
	}
	cancel()
}

// TestSSEWatcher_SessionBeadCreatedIgnored verifies that a session bead
// event other than "closed" (e.g. its initial creation) does not emit a
// SessionDone event.
func TestSSEWatcher_SessionBeadCreatedIgnored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		created := map[string]any{
			"bead": map[string]any{
				"id":     "gd-session2",
				"type":   "session",
				"status": "active",
				"fields": map[string]string{"project": "gardenio", "role": "crew"},
			},
		}
		data, _ := json.Marshal(created)
		fmt.Fprintf(w, "id:61\nevent:beads.bead.created\ndata:%s\n\n", data)
		flusher.Flush()

		// Followed by an agent bead so the test has a definite event to wait on.
		agent := map[string]any{
			"bead": map[string]any{
				"id":     "gd-agent2",
				"type":   "agent",
				"status": "in_progress",
				"fields": map[string]string{"project": "p", "role": "devops", "agent": "a1"},
			},
		}
		data2, _ := json.Marshal(agent)
		fmt.Fprintf(w, "id:62\nevent:beads.bead.created\ndata:%s\n\n", data2)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case event := <-w.Events():
		if event.BeadID != "gd-agent2" {
			t.Fatalf("expected first delivered event to be gd-agent2 (session create must be ignored), got %s (%s)", event.BeadID, event.Type)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event")
	}
	cancel()
}

// TestSSEWatcher_TopicFilterInURL verifies that topic filter is passed as query param.
func TestSSEWatcher_TopicFilterInURL(t *testing.T) {
	var receivedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.String()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Close immediately.
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Topics:        "beads.bead.*,beads.label.*",
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Wait for at least one connection attempt.
	time.Sleep(500 * time.Millisecond)
	cancel()

	expected := "/v1/events/stream?topics=beads.bead.*,beads.label.*"
	if receivedPath != expected {
		t.Fatalf("expected URL %q, got %q", expected, receivedPath)
	}
}

// TestSSEWatcher_StampsDarkPodMetadata verifies an agent bead carrying a dark
// marker label yields an AgentSpawn event whose Metadata[beadsapi.MetaDarkPod]
// is "true" — the signal the pod-spec builder reads to fence the Jira write
// token out of dark pods (ka-0000005242). A non-dark agent leaves it unset.
func TestSSEWatcher_StampsDarkPodMetadata(t *testing.T) {
	cases := []struct {
		name     string
		labels   []string
		wantDark bool
	}{
		{"dark labeled agent", []string{"role:crew", "monorepo-dark"}, true},
		{"plain agent", []string{"role:crew", "project:gardenio"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				flusher, _ := w.(http.Flusher)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				flusher.Flush()
				payload := map[string]any{
					"bead": map[string]any{
						"id":     "gd-dark1",
						"type":   "agent",
						"status": "in_progress",
						"labels": tc.labels,
						"fields": map[string]string{"project": "gardenio", "role": "crew", "agent": "a1"},
					},
				}
				data, _ := json.Marshal(payload)
				fmt.Fprintf(w, "id:1\nevent:beads.bead.created\ndata:%s\n\n", data)
				flusher.Flush()
				time.Sleep(100 * time.Millisecond)
			}))
			defer srv.Close()

			w := NewSSEWatcher(SSEConfig{BeadsHTTPAddr: srv.URL, Topics: "beads.bead.*"}, testLogger())
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go func() { _ = w.Start(ctx) }()

			select {
			case event := <-w.Events():
				got := event.Metadata["dark_pod"]
				if tc.wantDark && got != "true" {
					t.Fatalf("expected dark_pod=true, got %q", got)
				}
				if !tc.wantDark && got != "" {
					t.Fatalf("expected no dark_pod marker, got %q", got)
				}
			case <-ctx.Done():
				t.Fatal("timed out waiting for event")
			}
		})
	}
}

type stubTokenSource struct {
	tok string
	err error
}

func (s stubTokenSource) Token(context.Context) (string, error) { return s.tok, s.err }

// With a TokenSource (the controller's machine principal, gd-BDirD4F7Iq) the
// stream authenticates with the source's bearer and never the static Token.
func TestSSEWatcher_TokenSourceWinsOverStaticToken(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case got <- r.Header.Get("Authorization"):
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Token:         "static-token",
		TokenSource:   stubTokenSource{tok: "machine-token"},
	}, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	select {
	case h := <-got:
		if h != "Bearer machine-token" {
			t.Fatalf("Authorization = %q, want the machine token bearer", h)
		}
	case <-ctx.Done():
		t.Fatal("no SSE request seen")
	}
}

// A failing TokenSource fails the connect: it must not fall back to the static
// token, so a missing machine credential is loud, not a silent identity swap.
func TestSSEWatcher_TokenSourceFailureDoesNotFallBackToStaticToken(t *testing.T) {
	hit := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- r.Header.Get("Authorization"):
		default:
		}
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Token:         "static-token",
		TokenSource:   stubTokenSource{err: fmt.Errorf("auth_unavailable")},
	}, testLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = w.Start(ctx)

	select {
	case h := <-hit:
		t.Fatalf("daemon was contacted (Authorization %q) although the token source failed", h)
	default:
	}
}
