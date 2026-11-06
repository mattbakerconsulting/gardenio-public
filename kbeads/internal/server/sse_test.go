package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func TestSSEHub_BroadcastAndReceive(t *testing.T) {
	hub := newSSEHub()

	client := hub.subscribe(nil, "", sseClientScope{}) // all topics
	defer hub.unsubscribe(client)

	hub.broadcast("beads.bead.created", []byte(`{"id":"gd-1"}`), "", "", "", nil, nil)

	select {
	case evt := <-client.ch:
		if evt.Topic != "beads.bead.created" {
			t.Fatalf("expected topic=%q, got %q", "beads.bead.created", evt.Topic)
		}
		if string(evt.Data) != `{"id":"gd-1"}` {
			t.Fatalf("expected data=%q, got %q", `{"id":"gd-1"}`, string(evt.Data))
		}
		if evt.ID != 1 {
			t.Fatalf("expected id=1, got %d", evt.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestSSEHub_TopicFiltering(t *testing.T) {
	hub := newSSEHub()

	// Client only wants bead events.
	client := hub.subscribe([]string{"beads.bead.*"}, "", sseClientScope{})
	defer hub.unsubscribe(client)

	hub.broadcast("beads.label.added", []byte(`{"label":"x"}`), "", "", "", nil, nil)
	hub.broadcast("beads.bead.created", []byte(`{"id":"gd-1"}`), "", "", "", nil, nil)

	select {
	case evt := <-client.ch:
		if evt.Topic != "beads.bead.created" {
			t.Fatalf("expected topic=%q, got %q", "beads.bead.created", evt.Topic)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}

	// Ensure no more events (label.added should have been filtered).
	select {
	case evt := <-client.ch:
		t.Fatalf("unexpected event: topic=%q", evt.Topic)
	case <-time.After(50 * time.Millisecond):
		// Good - no extra events.
	}
}

func TestSSEHub_MultipleTopicFilters(t *testing.T) {
	hub := newSSEHub()

	client := hub.subscribe([]string{"beads.bead.*", "beads.label.*"}, "", sseClientScope{})
	defer hub.unsubscribe(client)

	hub.broadcast("beads.bead.created", []byte(`{}`), "", "", "", nil, nil)
	hub.broadcast("beads.label.added", []byte(`{}`), "", "", "", nil, nil)
	hub.broadcast("beads.comment.added", []byte(`{}`), "", "", "", nil, nil) // should be filtered

	received := 0
	timeout := time.After(time.Second)
	for received < 2 {
		select {
		case <-client.ch:
			received++
		case <-timeout:
			t.Fatalf("expected 2 events, got %d", received)
		}
	}

	select {
	case <-client.ch:
		t.Fatal("unexpected third event")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSSEHub_Unsubscribe(t *testing.T) {
	hub := newSSEHub()

	client := hub.subscribe(nil, "", sseClientScope{})
	hub.unsubscribe(client)

	hub.broadcast("beads.bead.created", []byte(`{}`), "", "", "", nil, nil)

	select {
	case <-client.ch:
		t.Fatal("should not receive events after unsubscribe")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSSEHub_EventsSince(t *testing.T) {
	hub := newSSEHub()

	// Broadcast 5 events.
	for i := range 5 {
		hub.broadcast("beads.bead.created", []byte(`{"n":`+string(rune('0'+i))+`}`), "", "", "", nil, nil)
	}

	// Get events after ID 2 (should return IDs 3, 4, 5).
	evts := hub.eventsSince(2)
	if len(evts) != 3 {
		t.Fatalf("expected 3 events, got %d", len(evts))
	}
	if evts[0].ID != 3 || evts[1].ID != 4 || evts[2].ID != 5 {
		t.Fatalf("expected IDs [3,4,5], got [%d,%d,%d]", evts[0].ID, evts[1].ID, evts[2].ID)
	}
}

func TestSSEHub_EventsSince_Empty(t *testing.T) {
	hub := newSSEHub()
	evts := hub.eventsSince(0)
	if len(evts) != 0 {
		t.Fatalf("expected 0 events, got %d", len(evts))
	}
}

func TestSSEHub_EventsSince_AllNew(t *testing.T) {
	hub := newSSEHub()
	hub.broadcast("beads.bead.created", []byte(`{}`), "", "", "", nil, nil)
	hub.broadcast("beads.bead.updated", []byte(`{}`), "", "", "", nil, nil)

	evts := hub.eventsSince(0)
	if len(evts) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evts))
	}
}

func TestSSEHub_RingBufferWrap(t *testing.T) {
	hub := newSSEHub()

	// Fill the ring buffer and then some to force wrap.
	for range sseRingBufferSize + 100 {
		hub.broadcast("beads.bead.created", []byte(`{}`), "", "", "", nil, nil)
	}

	// The oldest event in the buffer should have ID = 101 (100 were evicted).
	evts := hub.eventsSince(0)
	if len(evts) != sseRingBufferSize {
		t.Fatalf("expected %d events, got %d", sseRingBufferSize, len(evts))
	}
	if evts[0].ID != 101 {
		t.Fatalf("expected oldest event ID=101, got %d", evts[0].ID)
	}
}

// TestSSEHub_EventsSinceReplayRace exercises the replay path the way
// handleEventStream does: eventsSince returns a slice of *sseEvent and the
// caller reads each event's fields (Topic, Fleet, ID, Data) OUTSIDE ringMu,
// concurrently with broadcast overwriting ring slots under ringMu.Lock.
//
// Before the fix, eventsSince returned &h.ring[idx] — pointers aliasing the
// ring backing array — so a concurrent broadcast's `h.ring[idx] = *evt`
// (sse.go) mutated the sseEvent (including the Data []byte header) while the
// consumer read it. Run under `go test -race`: this reproduces the data race
// on the old code and is clean with the value-copy fix.
func TestSSEHub_EventsSinceReplayRace(t *testing.T) {
	hub := newSSEHub()

	// Prime the ring so a replay has events to hand back.
	for range 64 {
		hub.broadcast("beads.bead.created", []byte(`{"fleet":"f0"}`), "", "", "", nil, nil)
	}

	var wg sync.WaitGroup

	// Writer: hammer broadcast to keep overwriting ring slots (enough to wrap
	// well past the slots any replay is holding).
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range sseRingBufferSize * 2 {
			payload := []byte(`{"fleet":"f` + string(rune('0'+(i%9))) + `"}`)
			hub.broadcast("beads.bead.updated", payload, "", "", "", nil, nil)
		}
	}()

	// Reader: repeatedly replay and consume the returned events exactly like
	// handleEventStream — read Topic/Fleet/ID/Data with no lock held.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 500 {
			for _, evt := range hub.eventsSince(0) {
				_ = evt.ID
				_ = evt.Topic
				_ = evt.Fleet
				// Force a read of the Data slice header + backing bytes,
				// mirroring writeSSEEvent's `data:%s` formatting.
				_ = len(evt.Data)
				if len(evt.Data) > 0 {
					_ = evt.Data[0]
				}
			}
		}
	}()

	wg.Wait()
}

func TestMatchTopicPattern(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		topic   string
		want    bool
	}{
		{"beads.bead.created", "beads.bead.created", true},
		{"beads.bead.created", "beads.bead.updated", false},
		{"beads.bead.*", "beads.bead.created", true},
		{"beads.bead.*", "beads.bead.updated", true},
		{"beads.bead.*", "beads.label.added", false},
		{"beads.>", "beads.bead.created", true},
		{"beads.>", "beads.label.added", true},
		{"beads.>", "other.topic", false},
		{"*.*.*", "beads.bead.created", true},
		{"*.*.*", "beads.bead", false},
	} {
		t.Run(tc.pattern+"_"+tc.topic, func(t *testing.T) {
			got := matchTopicPattern(tc.pattern, tc.topic)
			if got != tc.want {
				t.Fatalf("matchTopicPattern(%q, %q) = %v, want %v", tc.pattern, tc.topic, got, tc.want)
			}
		})
	}
}

func TestSSEHub_FleetFiltering(t *testing.T) {
	hub := newSSEHub()

	// Client only wants events from "qa-fleet".
	client := hub.subscribe(nil, "qa-fleet", sseClientScope{})
	defer hub.unsubscribe(client)

	// Broadcast events from different fleets.
	hub.broadcast("beads.bead.created", []byte(`{"bead":{"id":"gd-1","fleet":"qa-fleet"}}`), "", "", "", nil, nil)
	hub.broadcast("beads.bead.created", []byte(`{"bead":{"id":"gd-2","fleet":"prod"}}`), "", "", "", nil, nil)
	hub.broadcast("beads.label.added", []byte(`{"bead_id":"gd-3","label":"x","fleet":"qa-fleet"}`), "", "", "", nil, nil)

	// Should receive 2 events (qa-fleet only).
	received := 0
	timeout := time.After(time.Second)
	for received < 2 {
		select {
		case <-client.ch:
			received++
		case <-timeout:
			t.Fatalf("expected 2 events, got %d", received)
		}
	}

	// Should not receive the prod event.
	select {
	case evt := <-client.ch:
		t.Fatalf("unexpected event: %s", string(evt.Data))
	case <-time.After(50 * time.Millisecond):
	}
}

func TestSSEHub_FleetFiltering_EmptyMatchesAll(t *testing.T) {
	hub := newSSEHub()

	// Client with empty fleet should receive all events.
	client := hub.subscribe(nil, "", sseClientScope{})
	defer hub.unsubscribe(client)

	hub.broadcast("beads.bead.created", []byte(`{"bead":{"id":"gd-1","fleet":"qa-fleet"}}`), "", "", "", nil, nil)
	hub.broadcast("beads.bead.created", []byte(`{"bead":{"id":"gd-2","fleet":"prod"}}`), "", "", "", nil, nil)
	hub.broadcast("beads.bead.created", []byte(`{"id":"gd-3"}`), "", "", "", nil, nil) // no fleet

	received := 0
	timeout := time.After(time.Second)
	for received < 3 {
		select {
		case <-client.ch:
			received++
		case <-timeout:
			t.Fatalf("expected 3 events, got %d", received)
		}
	}
}

func TestExtractFleetFromPayload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		data  string
		fleet string
	}{
		{"top-level fleet", `{"bead_id":"gd-1","fleet":"qa-fleet"}`, "qa-fleet"},
		{"nested bead fleet", `{"bead":{"id":"gd-1","fleet":"prod"}}`, "prod"},
		{"top-level takes precedence", `{"fleet":"top","bead":{"fleet":"nested"}}`, "top"},
		{"no fleet", `{"id":"gd-1"}`, ""},
		{"empty payload", `{}`, ""},
		{"invalid json", `not-json`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := extractFleetFromPayload([]byte(tc.data))
			if got != tc.fleet {
				t.Errorf("extractFleetFromPayload(%s) = %q, want %q", tc.data, got, tc.fleet)
			}
		})
	}
}

// TestHandleEventStream_SSE tests the full HTTP SSE endpoint.
func TestHandleEventStream_SSE(t *testing.T) {
	srv, _, handler := newTestServer()

	// Start the SSE request in a goroutine.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v1/events/stream", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	// Give the handler time to register the subscription.
	time.Sleep(50 * time.Millisecond)

	// Broadcast an event.
	srv.sseHub.broadcast("beads.bead.created", []byte(`{"id":"gd-sse1"}`), "", "", "", nil, nil)

	// Give it time to be written.
	time.Sleep(50 * time.Millisecond)

	// Cancel the context to end the stream.
	cancel()
	<-done

	// Check response headers.
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("expected Content-Type=text/event-stream, got %q", ct)
	}

	// Parse the SSE output.
	body := rec.Body.String()
	if !strings.Contains(body, "event:beads.bead.created") {
		t.Fatalf("expected event:beads.bead.created in body, got:\n%s", body)
	}
	if !strings.Contains(body, `data:{"id":"gd-sse1"}`) {
		t.Fatalf("expected data with gd-sse1 in body, got:\n%s", body)
	}
	if !strings.Contains(body, "id:") {
		t.Fatalf("expected id: field in body, got:\n%s", body)
	}
}

// TestHandleEventStream_TopicFilter tests the ?topics= query param.
func TestHandleEventStream_TopicFilter(t *testing.T) {
	srv, _, handler := newTestServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v1/events/stream?topics=beads.label.*", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)

	// Broadcast a bead event (should be filtered) and a label event (should pass).
	srv.sseHub.broadcast("beads.bead.created", []byte(`{"id":"gd-1"}`), "", "", "", nil, nil)
	srv.sseHub.broadcast("beads.label.added", []byte(`{"label":"urgent"}`), "", "", "", nil, nil)

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if strings.Contains(body, "beads.bead.created") {
		t.Fatalf("expected bead event to be filtered out, got:\n%s", body)
	}
	if !strings.Contains(body, "beads.label.added") {
		t.Fatalf("expected label event in body, got:\n%s", body)
	}
}

// TestHandleEventStream_FleetFilter tests the ?fleet= query param on SSE.
func TestHandleEventStream_FleetFilter(t *testing.T) {
	srv, _, handler := newTestServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v1/events/stream?fleet=qa-fleet", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)

	// Broadcast events from different fleets.
	srv.sseHub.broadcast("beads.bead.created", []byte(`{"bead":{"id":"gd-1","fleet":"qa-fleet"}}`), "", "", "", nil, nil)
	srv.sseHub.broadcast("beads.bead.created", []byte(`{"bead":{"id":"gd-2","fleet":"prod"}}`), "", "", "", nil, nil)

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "gd-1") {
		t.Fatalf("expected qa-fleet event (gd-1) in body, got:\n%s", body)
	}
	if strings.Contains(body, "gd-2") {
		t.Fatalf("expected prod event (gd-2) to be filtered out, got:\n%s", body)
	}
}

// TestHandleEventStream_LastEventID tests reconnection with Last-Event-ID.
func TestHandleEventStream_LastEventID(t *testing.T) {
	srv, _, handler := newTestServer()

	// Pre-broadcast 3 events before connecting.
	srv.sseHub.broadcast("beads.bead.created", []byte(`{"n":1}`), "", "", "", nil, nil)
	srv.sseHub.broadcast("beads.bead.updated", []byte(`{"n":2}`), "", "", "", nil, nil)
	srv.sseHub.broadcast("beads.bead.closed", []byte(`{"n":3}`), "", "", "", nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v1/events/stream", nil)
	req.Header.Set("Last-Event-ID", "1") // Should replay events 2 and 3.
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	// Should contain events 2 and 3 but not event 1.
	if strings.Contains(body, `data:{"n":1}`) {
		t.Fatalf("expected event 1 to be skipped, got:\n%s", body)
	}
	if !strings.Contains(body, `data:{"n":2}`) {
		t.Fatalf("expected event 2 in body, got:\n%s", body)
	}
	if !strings.Contains(body, `data:{"n":3}`) {
		t.Fatalf("expected event 3 in body, got:\n%s", body)
	}
}

// TestHandleEventStream_RecordAndPublish tests that recordAndPublish broadcasts to SSE.
func TestHandleEventStream_RecordAndPublish(t *testing.T) {
	srv, _, handler := newTestServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v1/events/stream", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)

	// Use recordAndPublish (which the HTTP handlers use) to emit an event.
	srv.recordAndPublish(context.Background(), events.TopicBeadCreated, "gd-sse-rp",
		"alice", events.BeadCreated{})

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "event:beads.bead.created") {
		t.Fatalf("expected SSE event from recordAndPublish, got:\n%s", body)
	}
}

// TestHandleEventStream_MultipleClients verifies fan-out to multiple clients.
func TestHandleEventStream_MultipleClients(t *testing.T) {
	srv, _, handler := newTestServer()

	startClient := func() (*httptest.ResponseRecorder, context.CancelFunc, <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		req := httptest.NewRequest("GET", "/v1/events/stream", nil)
		req = req.WithContext(ctx)
		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			handler.ServeHTTP(rec, req)
		}()
		return rec, cancel, done
	}

	rec1, cancel1, done1 := startClient()
	defer cancel1()
	rec2, cancel2, done2 := startClient()
	defer cancel2()

	time.Sleep(50 * time.Millisecond)

	srv.sseHub.broadcast("beads.bead.created", []byte(`{"id":"gd-multi"}`), "", "", "", nil, nil)

	time.Sleep(50 * time.Millisecond)
	cancel1()
	cancel2()
	<-done1
	<-done2

	for i, rec := range []*httptest.ResponseRecorder{rec1, rec2} {
		body := rec.Body.String()
		if !strings.Contains(body, "beads.bead.created") {
			t.Fatalf("client %d: expected bead event, got:\n%s", i+1, body)
		}
	}
}

// --- read-scope gating (ADR docs/adr/taints.md §4.3/§5.1, ka-INalwzaYD9) --
//
// Before this fix, handleEventStream subscribed clients by topic + fleet
// only: a caller without a taint grant (or outside a token's project) still
// received every live event and Last-Event-ID replay entry for a
// tainted/out-of-scope bead. These tests pin the bug's acceptance criteria:
// a client without the grant never receives the event (live or replayed);
// a granted caller does; a project-bound fleet token cannot subscribe to
// another project's events.

// startSSEStream runs srv.handleEventStream directly (bypassing the auth
// middleware chain, matching the direct-handler-call pattern used
// throughout this package's other read/write-authz tests, e.g.
// field_mutation_write_authz_test.go) so the request context's identity /
// token-project can be set precisely without fighting header-derived
// identity resolution.
func startSSEStream(srv *BeadsServer, req *http.Request) (*httptest.ResponseRecorder, <-chan struct{}) {
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.handleEventStream(rec, req)
	}()
	return rec, done
}

func TestHandleEventStream_TaintDenied_LiveEventNotDelivered(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	seedBeadTaints(ms, "gd-tainted", 1)

	ctx, cancel := context.WithCancel(asOIDC(context.Background(), "outsider@example.com"))
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)

	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-tainted", map[string]string{"id": "gd-tainted"})

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if body := rec.Body.String(); strings.Contains(body, "gd-tainted") {
		t.Fatalf("expected tainted bead event to be withheld from an ungranted caller, got:\n%s", body)
	}
}

func TestHandleEventStream_TaintGranted_LiveEventDelivered(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	ctx, cancel := context.WithCancel(asOIDC(context.Background(), "insider@example.com"))
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)

	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-tainted", map[string]string{"id": "gd-tainted"})

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if body := rec.Body.String(); !strings.Contains(body, "gd-tainted") {
		t.Fatalf("expected granted caller to receive the tainted bead event, got:\n%s", body)
	}
}

func TestHandleEventStream_TaintDenied_ReplayEventNotDelivered(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	seedBeadTaints(ms, "gd-tainted", 1)

	// Broadcast BEFORE the client connects so it lands only in the replay
	// buffer, not the live fan-out.
	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-tainted", map[string]string{"id": "gd-tainted"})

	ctx, cancel := context.WithCancel(asOIDC(context.Background(), "outsider@example.com"))
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", "0")

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if body := rec.Body.String(); strings.Contains(body, "gd-tainted") {
		t.Fatalf("expected tainted bead event to be withheld from replay for an ungranted caller, got:\n%s", body)
	}
}

func TestHandleEventStream_TaintGranted_ReplayEventDelivered(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-tainted", map[string]string{"id": "gd-tainted"})

	ctx, cancel := context.WithCancel(asOIDC(context.Background(), "insider@example.com"))
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", "0")

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if body := rec.Body.String(); !strings.Contains(body, "gd-tainted") {
		t.Fatalf("expected granted caller to receive the tainted bead event on replay, got:\n%s", body)
	}
}

func TestHandleEventStream_ProjectScope_ForeignProjectLiveEventNotDelivered(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.labels["gd-other-project"] = []string{"project:other"}

	ctx, cancel := context.WithCancel(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "acme-fleet"}))
	ctx = WithTokenProject(ctx, "acme")
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)

	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-other-project", map[string]string{"id": "gd-other-project"})

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if body := rec.Body.String(); strings.Contains(body, "gd-other-project") {
		t.Fatalf("expected a project-bound fleet token to be denied another project's event, got:\n%s", body)
	}
}

func TestHandleEventStream_ProjectScope_ForeignProjectReplayEventNotDelivered(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.labels["gd-other-project"] = []string{"project:other"}

	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-other-project", map[string]string{"id": "gd-other-project"})

	ctx, cancel := context.WithCancel(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "acme-fleet"}))
	ctx = WithTokenProject(ctx, "acme")
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)
	req.Header.Set("Last-Event-ID", "0")

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if body := rec.Body.String(); strings.Contains(body, "gd-other-project") {
		t.Fatalf("expected a project-bound fleet token to be denied another project's event on replay, got:\n%s", body)
	}
}

func TestHandleEventStream_ProjectScope_OwnProjectDelivered(t *testing.T) {
	srv, ms, _ := newTestServer()
	ms.labels["gd-own-project"] = []string{"project:acme"}

	ctx, cancel := context.WithCancel(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "acme-fleet"}))
	ctx = WithTokenProject(ctx, "acme")
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)

	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-own-project", map[string]string{"id": "gd-own-project"})

	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	if body := rec.Body.String(); !strings.Contains(body, "gd-own-project") {
		t.Fatalf("expected a project-bound fleet token to receive its own project's event, got:\n%s", body)
	}
}

// TestHandleEventStream_TaintGateRefreshedMidStream proves a grant revoked
// after subscribe takes effect on the next refresh tick without requiring a
// reconnect (ka-INalwzaYD9's fix-shape) — and, symmetrically, a grant
// created mid-stream starts allowing the bead's events. The refresh interval
// is shrunk for the duration of the test so it doesn't sleep for the
// production 5s cadence.
func TestHandleEventStream_TaintGateRefreshedMidStream(t *testing.T) {
	prevInterval := sseTaintGateRefreshInterval
	sseTaintGateRefreshInterval = 20 * time.Millisecond
	defer func() { sseTaintGateRefreshInterval = prevInterval }()

	srv, ms, _ := newTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	grant := &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}
	if err := ms.GrantTaint(context.Background(), grant); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	ctx, cancel := context.WithCancel(asOIDC(context.Background(), "insider@example.com"))
	defer cancel()
	req := httptest.NewRequest("GET", "/v1/events/stream", nil).WithContext(ctx)

	rec, done := startSSEStream(srv, req)
	time.Sleep(50 * time.Millisecond)

	// Revoke the grant, wait for at least one refresh tick, then broadcast:
	// the now-ungranted caller must not receive it.
	if _, err := ms.RevokeTaint(context.Background(), 1, model.PrincipalOperator, "9"); err != nil {
		t.Fatalf("revoke grant: %v", err)
	}
	// The revoke handler's TopicTaintRevoked publish drops cached grant sets;
	// this test revokes in the store directly, so it does the same.
	srv.invalidateGrantSetsForEvent(events.TopicTaintRevoked, nil)
	time.Sleep(80 * time.Millisecond)
	srv.broadcastEvent(context.Background(), "beads.bead.created", "gd-tainted", map[string]string{"id": "revoked-mid-stream"})
	time.Sleep(50 * time.Millisecond)

	cancel()
	<-done

	if body := rec.Body.String(); strings.Contains(body, "revoked-mid-stream") {
		t.Fatalf("expected a grant revoked mid-stream to take effect on the next refresh tick, got:\n%s", body)
	}
}

// TestSSEEventFormat verifies the exact SSE wire format.
func TestSSEEventFormat(t *testing.T) {
	srv, _, handler := newTestServer()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req := httptest.NewRequest("GET", "/v1/events/stream", nil)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)
	srv.sseHub.broadcast("beads.bead.created", []byte(`{"id":"gd-fmt"}`), "", "", "", nil, nil)
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	// Parse SSE events from body.
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	var id, event, data string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id:") {
			id = strings.TrimPrefix(line, "id:")
		} else if strings.HasPrefix(line, "event:") {
			event = strings.TrimPrefix(line, "event:")
		} else if strings.HasPrefix(line, "data:") {
			data = strings.TrimPrefix(line, "data:")
		}
	}

	if id == "" {
		t.Fatal("expected non-empty id field")
	}
	if event != "beads.bead.created" {
		t.Fatalf("expected event=beads.bead.created, got %q", event)
	}
	if !json.Valid([]byte(data)) {
		t.Fatalf("expected valid JSON data, got %q", data)
	}
	if data != `{"id":"gd-fmt"}` {
		t.Fatalf("expected data=%q, got %q", `{"id":"gd-fmt"}`, data)
	}
}
