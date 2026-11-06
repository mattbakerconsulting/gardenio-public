package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
)

// TestHandleAgentInput_Success verifies the operator-text-write path forwards
// the form's `text` field to coopmux.SendInput and returns 204.
func TestHandleAgentInput_Success(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"last_session_id": "sess-xyz"},
	}
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"text": {"hello pty"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/input",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.inputCalls != 1 {
		t.Fatalf("expected 1 SendInput call, got %d", cm.inputCalls)
	}
	if cm.inputLastID != "sess-xyz" {
		t.Errorf("SendInput sessionID = %q, want sess-xyz", cm.inputLastID)
	}
	if cm.inputLastText != "hello pty" {
		t.Errorf("SendInput text = %q, want %q", cm.inputLastText, "hello pty")
	}
}

// TestHandleAgentInput_NoSession returns 409 when the agent bead has no
// last_session_id field — there is no live PTY to write to.
func TestHandleAgentInput_NoSession(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{},
	}
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"text": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/input",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
	if cm.inputCalls != 0 {
		t.Errorf("expected 0 SendInput calls, got %d", cm.inputCalls)
	}
}

// TestHandleAgentInput_NonAgent rejects POSTs to non-agent bead IDs.
func TestHandleAgentInput_NonAgent(t *testing.T) {
	f := newFake()
	f.beads["gd-task"] = &beadsapi.BeadDetail{
		ID:     "gd-task",
		Type:   "task",
		Status: "open",
	}
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	form := url.Values{"text": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-task/input",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestHandleAgentInput_EmptyText rejects empty text fields.
func TestHandleAgentInput_EmptyText(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"last_session_id": "sess-1"},
	}
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"text": {""}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/input",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if cm.inputCalls != 0 {
		t.Errorf("expected 0 SendInput calls, got %d", cm.inputCalls)
	}
}

// TestHandleAgentInput_TextTooLong rejects payloads above the per-write cap.
func TestHandleAgentInput_TextTooLong(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"last_session_id": "sess-1"},
	}
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"text": {strings.Repeat("a", maxAgentInputLen+1)}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/input",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
	if cm.inputCalls != 0 {
		t.Errorf("expected 0 SendInput calls, got %d", cm.inputCalls)
	}
}

// TestHandleAgentInput_SessionNotFoundUpstream maps coopmux's
// ErrSessionNotFound to a 404 surfaced to the operator.
func TestHandleAgentInput_SessionNotFoundUpstream(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"last_session_id": "sess-gone"},
	}
	cm := &fakeScreenClient{inputErr: coopmuxapi.ErrSessionNotFound}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"text": {"hello"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/input",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.inputCalls != 1 {
		t.Errorf("expected 1 SendInput call, got %d", cm.inputCalls)
	}
}

// TestHandleAgentInput_CoopmuxDisabled returns 503 when the deployment is
// running without coopmux at all.
func TestHandleAgentInput_CoopmuxDisabled(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"last_session_id": "sess-1"},
	}
	cm := &fakeScreenClient{disabled: true}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"text": {"hi"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/input",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// TestHandleAgentControlClaim_RendersToolbar verifies the "Take control"
// fragment carries the input form pre-armed at /agents/{id}/input.
func TestHandleAgentControlClaim_RendersToolbar(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"last_session_id": "sess-1"},
	}
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/control/claim", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="agent-control"`,
		`agent-control-active`,
		`hx-post="/agents/gd-agt/input"`,
		`name="text"`,
		`agent-input-send`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("control-claim fragment missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandleAgentControlClaim_NonAgent rejects non-agent beads.
func TestHandleAgentControlClaim_NonAgent(t *testing.T) {
	f := newFake()
	f.beads["gd-task"] = &beadsapi.BeadDetail{
		ID:     "gd-task",
		Type:   "task",
		Status: "open",
	}
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodPost, "/agents/gd-task/control/claim", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// TestHandleAgentEvents_FiltersInSet feeds two SSE events through the daemon
// stream — one for a bead in the agent's filter set (the parent task) and
// one for an unrelated bead — and asserts only the in-set one is forwarded
// to the client.
func TestHandleAgentEvents_FiltersInSet(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"task_id": "gd-task-1"},
	}
	ch := make(chan beadsapi.SSEEvent, 4)
	f.eventStreamCh = ch
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	// Two events: one for the parent task (in-set), one for an unrelated bead.
	mustEnqueueEvt := func(topic, beadID, beadType, title string) {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"bead": map[string]any{"id": beadID, "type": beadType, "title": title, "status": "in_progress"},
		})
		ch <- beadsapi.SSEEvent{Event: topic, Data: payload}
	}
	mustEnqueueEvt("beads.bead.updated", "gd-task-1", "task", "parent task")
	mustEnqueueEvt("beads.bead.updated", "gd-other", "task", "unrelated")
	close(ch)

	server := httptest.NewServer(mux)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/agents/gd-agt/events", nil)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "event: updated") {
		t.Errorf("expected `event: updated` in stream, got: %s", got)
	}
	if !strings.Contains(got, "gd-task-1") {
		t.Errorf("expected parent task ID in stream, got: %s", got)
	}
	if strings.Contains(got, "gd-other") {
		t.Errorf("filter leak: unrelated bead surfaced in stream: %s", got)
	}
	if f.eventStreamCalls != 1 {
		t.Errorf("expected 1 EventStream call, got %d", f.eventStreamCalls)
	}
	if f.eventStreamLastTopics != "beads.bead.>" {
		t.Errorf("EventStream topics = %q, want beads.bead.>", f.eventStreamLastTopics)
	}
}

// TestHandleAgentEvents_NonAgent rejects requests targeting non-agent beads.
func TestHandleAgentEvents_NonAgent(t *testing.T) {
	f := newFake()
	f.beads["gd-task"] = &beadsapi.BeadDetail{
		ID:     "gd-task",
		Type:   "task",
		Status: "open",
	}
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodGet, "/agents/gd-task/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if f.eventStreamCalls != 0 {
		t.Errorf("expected 0 EventStream calls, got %d", f.eventStreamCalls)
	}
}

// TestAgentEventKind exercises the topic→kind mapping table.
func TestAgentEventKind(t *testing.T) {
	cases := []struct {
		topic, want string
	}{
		{"beads.bead.created", "created"},
		{"beads.bead.updated", "updated"},
		{"beads.bead.closed", "closed"},
		{"beads.bead.something_else", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := agentEventKind(c.topic); got != c.want {
			t.Errorf("agentEventKind(%q) = %q, want %q", c.topic, got, c.want)
		}
	}
}

// TestRenderAgentEventRow_Shape sanity-checks the event-row HTML so the SSE
// writer's data: line is well-formed.
func TestRenderAgentEventRow_Shape(t *testing.T) {
	now := time.Date(2026, 4, 27, 12, 30, 0, 0, time.UTC)
	row := renderAgentEventRow("/v",
		"closed",
		&agentEventBead{ID: "gd-x", Type: "task", Title: "ship it"},
		"alice",
		nil,
		now,
	)
	for _, want := range []string{
		`agent-event-closed`,
		`data-bead-id="gd-x"`,
		`data-bead-type="task"`,
		`href="/v/gd-x"`,
		`<code>gd-x</code>`,
		`ship it`,
		`agent-event-meta">by alice`,
	} {
		if !strings.Contains(row, want) {
			t.Errorf("event row missing %q\nrow: %s", want, row)
		}
	}
	if strings.Contains(row, "\n") {
		t.Errorf("event row contained a newline — SSE data: must be single-line:\n%s", row)
	}
}

// TestAgentInputSnippet tests the audit-log snippet helper.
func TestAgentInputSnippet(t *testing.T) {
	if got := agentInputSnippet("hi", 80); got != "hi" {
		t.Errorf("short snippet = %q, want hi", got)
	}
	if got := agentInputSnippet("a\nb\rc", 80); got != "a b c" {
		t.Errorf("newline-strip snippet = %q, want %q", got, "a b c")
	}
	long := strings.Repeat("x", 200)
	got := agentInputSnippet(long, 10)
	if !strings.HasSuffix(got, "…") || len(got) > 14 {
		t.Errorf("long snippet shape unexpected: %q", got)
	}
}

var _ = fmt.Sprintf // keep import even if unused
