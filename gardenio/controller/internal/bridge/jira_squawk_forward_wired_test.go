package bridge

// Wired integration-style test for the JIRA squawk forwarder (feature F1, plan
// ka-0000007401). The unit tests in jira_squawk_forward_test.go call handleClosed
// directly; this file proves the missing link the F1 wiring closes — that
// RegisterHandlers actually attaches the forwarder's closed handler to a REAL
// SSEStream and that a `beads.bead.closed` squawk event dispatched through the
// stream reaches it and posts the JIRA comment. It mirrors the gitlab-bridge
// squawk-forwarder wiring and the SSEStream dispatch smoke test
// (TestSSEStream_MultipleHandlersSameTopic).

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestJiraSquawkForwarder_WiredOnSSEStream drives a closed squawk bead through a
// real SSEStream that the forwarder is registered on, and asserts the JIRA
// comment lands — the end-to-end proof that RegisterHandlers wires the closed
// handler (the exact wiring cmd/jira-bridge/main.go performs).
func TestJiraSquawkForwarder_WiredOnSSEStream(t *testing.T) {
	// One SSE event: a closed squawk from a PE-bound agent, then hold the
	// connection open so the stream does not reconnect-spam during the test.
	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		fmt.Fprintf(w, "id:1\n")
		fmt.Fprintf(w, "event:beads.bead.closed\n")
		fmt.Fprintf(w, `data:{"bead":{"id":"bd-squawk-wired","type":"message",`+
			`"labels":["squawk","from:test-agent"],`+
			`"fields":{"source_agent":"test-agent","text":"Wired squawk reaches the ticket."}}}`+"\n")
		fmt.Fprintf(w, "\n")
		flusher.Flush()

		time.Sleep(500 * time.Millisecond)
	}))
	defer sseServer.Close()

	daemon := &mockJiraSquawkDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"test-agent": {
				ID:     "bd-test-agent",
				Title:  "test-agent",
				Fields: map[string]string{"jira_key": "PE-4242"},
			},
		},
	}
	jira := &mockJiraSquawkJira{}
	f := newJiraSquawkForwarder(daemon, jira)

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: sseServer.URL,
		Logger:        jiraSquawkTestLogger(),
	})
	// The wiring under test: register the forwarder on the stream exactly as
	// cmd/jira-bridge/main.go does.
	f.RegisterHandlers(stream)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx) }()

	// Wait for the forwarded comment to arrive via the real dispatch path.
	deadline := time.After(2 * time.Second)
	for len(jira.getComments()) == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for squawk to be forwarded through the SSE stream — " +
				"RegisterHandlers did not wire the beads.bead.closed handler")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()

	comments := jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 forwarded JIRA comment, got %d", len(comments))
	}
	if comments[0].Key != "PE-4242" {
		t.Errorf("comment key = %q, want PE-4242", comments[0].Key)
	}
	if comments[0].Vis != CommentInternal {
		t.Errorf("comment visibility = %v, want internal (a squawk is an internal note)", comments[0].Vis)
	}
}

// TestJiraSquawkForwarder_RegisterHandlersDoesNotPanic guards the trivial
// contract — RegisterHandlers on a freshly-built stream must not panic — matching
// the parity checks other forwarders keep (e.g. TestThreadMessageForward_RegisterHandlers).
func TestJiraSquawkForwarder_RegisterHandlersDoesNotPanic(t *testing.T) {
	f := newJiraSquawkForwarder(&mockJiraSquawkDaemon{}, &mockJiraSquawkJira{})
	stream := NewSSEStream(SSEStreamConfig{BeadsHTTPAddr: "http://x", Logger: jiraSquawkTestLogger()})
	f.RegisterHandlers(stream) // must not panic; registers the closed handler
}
