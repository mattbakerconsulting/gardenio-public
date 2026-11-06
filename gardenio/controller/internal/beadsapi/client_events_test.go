package beadsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEventStreamScoped_SendsAgentAndLastEventID_ParsesID pins the agent
// scope query, the Last-Event-ID replay header, and the parsed SSE id: line
// the mail subscriber resumes from (gd-qVFbBLLrpu).
func TestEventStreamScoped_SendsAgentAndLastEventID_ParsesID(t *testing.T) {
	var gotAgent, gotLast, gotTopics string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAgent = r.URL.Query().Get("agent")
		gotTopics = r.URL.Query().Get("topics")
		gotLast = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("id:42\nevent:beads.bead.created\ndata:{\"bead\":{\"id\":\"gd-m1\"}}\n\n"))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), sseClient: srv.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := c.EventStreamScoped(ctx, "beads.bead.created", "gd-agent1", "41")
	if err != nil {
		t.Fatalf("EventStreamScoped: %v", err)
	}
	evt, ok := <-ch
	if !ok {
		t.Fatal("stream closed before first event")
	}
	if evt.ID != "42" || evt.Event != "beads.bead.created" || string(evt.Data) != `{"bead":{"id":"gd-m1"}}` {
		t.Fatalf("got id=%q event=%q data=%s", evt.ID, evt.Event, evt.Data)
	}
	if gotAgent != "gd-agent1" || gotLast != "41" || gotTopics != "beads.bead.created" {
		t.Fatalf("query/header not forwarded: agent=%q last=%q topics=%q", gotAgent, gotLast, gotTopics)
	}
}

// TestEventStream_UnscopedSendsNoAgentOrCursor: the plain EventStream keeps
// its old request shape.
func TestEventStream_UnscopedSendsNoAgentOrCursor(t *testing.T) {
	var hasAgent bool
	var gotLast string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasAgent = r.URL.Query()["agent"]
		gotLast = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event:beads.bead.created\ndata:{}\n\n"))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client(), sseClient: srv.Client()}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := c.EventStream(ctx, "beads.bead.created")
	if err != nil {
		t.Fatalf("EventStream: %v", err)
	}
	evt := <-ch
	if evt.ID != "" {
		t.Errorf("ID = %q, want empty when the server sent no id: line", evt.ID)
	}
	if hasAgent || gotLast != "" {
		t.Errorf("unscoped stream sent agent=%v Last-Event-ID=%q", hasAgent, gotLast)
	}
}
