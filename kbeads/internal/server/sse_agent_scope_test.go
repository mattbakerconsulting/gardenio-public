package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func recvOrTimeout(t *testing.T, c *sseClient) *sseEvent {
	t.Helper()
	select {
	case evt := <-c.ch:
		return evt
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return nil
	}
}

func assertNoEvent(t *testing.T, c *sseClient) {
	t.Helper()
	select {
	case evt := <-c.ch:
		t.Fatalf("unexpected event: topic=%q bead=%q", evt.Topic, evt.BeadID)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSSEHub_AgentScope_OwnBeadAndOwnMail(t *testing.T) {
	hub := newSSEHub()
	c := hub.subscribe(nil, "", sseClientScope{agentBeadID: "gd-agent1", agentName: "local-laptop"})
	defer hub.unsubscribe(c)

	hub.broadcast("beads.bead.updated", []byte(`{}`), "gd-agent1", "agent", "local-laptop", nil, nil)
	if evt := recvOrTimeout(t, c); evt.BeadID != "gd-agent1" {
		t.Fatalf("expected own agent bead event, got %q", evt.BeadID)
	}

	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-mail1", "mail", "local-laptop", nil, nil)
	if evt := recvOrTimeout(t, c); evt.BeadID != "gd-mail1" {
		t.Fatalf("expected own mail event, got %q", evt.BeadID)
	}
}

// Review Focus 1: a task assigned to the same name is not the agent's mail,
// and another agent's mail is never streamed.
func TestSSEHub_AgentScope_OnlyMailByAssignee(t *testing.T) {
	hub := newSSEHub()
	c := hub.subscribe(nil, "", sseClientScope{agentBeadID: "gd-agent1", agentName: "local-laptop"})
	defer hub.unsubscribe(c)

	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-task1", "task", "local-laptop", nil, nil)
	assertNoEvent(t, c)

	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-mail2", "mail", "crew-other", nil, nil)
	assertNoEvent(t, c)

	hub.broadcast("beads.bead.updated", []byte(`{}`), "gd-agent2", "agent", "crew-other", nil, nil)
	assertNoEvent(t, c)

	// A mail whose assignee could not be resolved fails closed.
	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-mail3", "mail", "", nil, nil)
	assertNoEvent(t, c)
}

// Positive control: an unscoped client still sees everything the old way.
func TestSSEHub_NoAgentScope_Unrestricted(t *testing.T) {
	hub := newSSEHub()
	c := hub.subscribe(nil, "", sseClientScope{})
	defer hub.unsubscribe(c)

	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-task1", "task", "someone", nil, nil)
	if evt := recvOrTimeout(t, c); evt.BeadID != "gd-task1" {
		t.Fatalf("unscoped client should receive every event, got %q", evt.BeadID)
	}
}

// Replay from Last-Event-ID applies the same agent gate as live delivery.
func TestSSEHub_AgentScope_ReplayFiltered(t *testing.T) {
	hub := newSSEHub()
	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-task1", "task", "local-laptop", nil, nil)
	hub.broadcast("beads.bead.created", []byte(`{}`), "gd-mail1", "mail", "local-laptop", nil, nil)
	c := hub.subscribe(nil, "", sseClientScope{agentBeadID: "gd-agent1", agentName: "local-laptop"})
	defer hub.unsubscribe(c)

	var got []string
	for _, evt := range hub.eventsSince(0) {
		if c.matchesScope(evt.BeadID, evt.BeadType, evt.Assignee, evt.Labels, evt.TaintIDs) {
			got = append(got, evt.BeadID)
		}
	}
	if len(got) != 1 || got[0] != "gd-mail1" {
		t.Fatalf("replay = %v, want [gd-mail1]", got)
	}
}

func TestHandleEventStream_AgentParam_UnknownBead404(t *testing.T) {
	srv, _, _ := newTestServer()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream?agent=gd-nope", nil)
	rec := httptest.NewRecorder()
	srv.handleEventStream(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unknown agent bead, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "agent bead not found") {
		t.Fatalf("expected agent bead not found error, got %s", rec.Body.String())
	}
}

func TestHandleEventStream_AgentParam_NonAgentBead404(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beadsMu.Lock()
	ms.beads["gd-task9"] = &model.Bead{ID: "gd-task9", Type: "task", Title: "a task", Status: "open"}
	ms.beadsMu.Unlock()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream?agent=gd-task9", nil)
	rec := httptest.NewRecorder()
	srv.handleEventStream(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for non-agent bead, got %d", rec.Code)
	}
}

// End to end through broadcastEvent: a local agent bead (type=task with the
// kd:agent label, as gb agent start --local registers it) streams its own
// bead and its own mail, and nothing else, with type and assignee resolved
// from the store at broadcast time.
func TestHandleEventStream_AgentParam_LocalAgentStreamsOwnMail(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.beadsMu.Lock()
	ms.beads["gd-local1"] = &model.Bead{ID: "gd-local1", Type: "task", Title: "local-laptop", Assignee: "local-laptop", Status: "open"}
	ms.beads["gd-mail-mine"] = &model.Bead{ID: "gd-mail-mine", Type: "mail", Title: "hi", Assignee: "local-laptop", Status: "open"}
	ms.beads["gd-mail-other"] = &model.Bead{ID: "gd-mail-other", Type: "mail", Title: "hi", Assignee: "crew-other", Status: "open"}
	ms.beads["gd-task-mine"] = &model.Bead{ID: "gd-task-mine", Type: "task", Title: "work", Assignee: "local-laptop", Status: "open"}
	ms.labels["gd-local1"] = []string{"kd:agent", "execution_target:local"}
	ms.beadsMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/v1/events/stream?agent=gd-local1", nil).WithContext(ctx)
	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)

	for _, id := range []string{"gd-mail-other", "gd-task-mine", "gd-mail-mine", "gd-local1"} {
		srv.broadcastEvent(context.Background(), "beads.bead.created", id, map[string]string{"id": id})
	}
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, body)
	}
	for _, want := range []string{`"id":"gd-mail-mine"`, `"id":"gd-local1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("stream missing %s; got:\n%s", want, body)
		}
	}
	for _, deny := range []string{"gd-mail-other", "gd-task-mine"} {
		if strings.Contains(body, deny) {
			t.Errorf("stream leaked %s; got:\n%s", deny, body)
		}
	}
}
