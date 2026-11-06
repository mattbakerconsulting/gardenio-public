package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSSEStream_IgnoresKeepalive(t *testing.T) {
	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send keepalive comment.
		fmt.Fprintf(w, ":keepalive\n\n")
		flusher.Flush()

		// Send real event after keepalive.
		fmt.Fprintf(w, "id:1\n")
		fmt.Fprintf(w, "event:beads.bead.created\n")
		fmt.Fprintf(w, "data:{\"bead\":{\"id\":\"test-1\",\"type\":\"mail\"}}\n")
		fmt.Fprintf(w, "\n")
		flusher.Flush()

		time.Sleep(200 * time.Millisecond)
	}))
	defer sseServer.Close()

	var mu sync.Mutex
	var received int

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: sseServer.URL,
		Logger:        slog.Default(),
	})
	stream.On("beads.bead.created", func(_ context.Context, _ []byte) {
		mu.Lock()
		received++
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		_ = stream.Start(ctx)
	}()

	deadline := time.After(1500 * time.Millisecond)
	for {
		mu.Lock()
		count := received
		mu.Unlock()
		if count >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for event after keepalive")
			return
		case <-time.After(10 * time.Millisecond):
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if received != 1 {
		t.Errorf("expected 1 event (keepalive should be ignored), got %d", received)
	}
}

func TestSSEStream_SendsLastEventID(t *testing.T) {
	var mu sync.Mutex
	var connectionCount int
	var lastEventIDs []string

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connectionCount++
		lastEventIDs = append(lastEventIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send one event with ID and close the connection to trigger reconnect.
		fmt.Fprintf(w, "id:42\n")
		fmt.Fprintf(w, "event:beads.bead.created\n")
		fmt.Fprintf(w, "data:{\"bead\":{\"id\":\"test-1\",\"type\":\"mail\"}}\n")
		fmt.Fprintf(w, "\n")
		flusher.Flush()

		// Close to trigger reconnect.
	}))
	defer sseServer.Close()

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: sseServer.URL,
		Logger:        slog.Default(),
	})
	stream.On("beads.bead.created", func(_ context.Context, _ []byte) {})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = stream.Start(ctx)
	}()

	// Wait for at least one reconnection to happen.
	deadline := time.After(2500 * time.Millisecond)
	for {
		mu.Lock()
		count := connectionCount
		mu.Unlock()
		if count >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for reconnection")
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()

	// First connection should have no Last-Event-ID.
	if lastEventIDs[0] != "" {
		t.Errorf("first connection should have empty Last-Event-ID, got %q", lastEventIDs[0])
	}
	// Second connection (reconnect) should send Last-Event-ID: 42.
	if lastEventIDs[1] != "42" {
		t.Errorf("reconnection should send Last-Event-ID 42, got %q", lastEventIDs[1])
	}
	// Verify the stream tracked lastID internally.
	if stream.LastID() != "42" {
		t.Errorf("expected lastID to be '42', got %q", stream.LastID())
	}
}

// TestSSEStream_ReconnectsOnDisconnect verifies that when the server closes
// mid-stream, the SSEStream reconnects and handlers continue receiving events
// from the new connection.
func TestSSEStream_ReconnectsOnDisconnect(t *testing.T) {
	var connCount atomic.Int32

	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connCount.Add(1)

		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Each connection sends one event with a unique ID, then closes.
		fmt.Fprintf(w, "id:%d\n", n)
		fmt.Fprintf(w, "event:beads.bead.created\n")
		fmt.Fprintf(w, "data:{\"bead\":{\"id\":\"conn%d\",\"type\":\"decision\"}}\n", n)
		fmt.Fprintf(w, "\n")
		flusher.Flush()
		time.Sleep(50 * time.Millisecond)
		// Return to close connection and trigger reconnect.
	}))
	defer sseServer.Close()

	var mu sync.Mutex
	var receivedIDs []string

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: sseServer.URL,
		Logger:        slog.Default(),
	})
	stream.On("beads.bead.created", func(_ context.Context, data []byte) {
		bead := ParseBeadEvent(data)
		if bead != nil {
			mu.Lock()
			receivedIDs = append(receivedIDs, bead.ID)
			mu.Unlock()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	go func() {
		_ = stream.Start(ctx)
	}()

	// Wait for events from at least two connections (proving reconnect worked).
	deadline := time.After(8 * time.Second)
	for {
		mu.Lock()
		count := len(receivedIDs)
		mu.Unlock()
		if count >= 2 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timed out waiting for reconnect events, got %d: %v", len(receivedIDs), receivedIDs)
			mu.Unlock()
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()

	if receivedIDs[0] != "conn1" {
		t.Errorf("expected first event from conn1, got %s", receivedIDs[0])
	}
	if receivedIDs[1] != "conn2" {
		t.Errorf("expected second event from conn2 (after reconnect), got %s", receivedIDs[1])
	}
	if connCount.Load() < 2 {
		t.Errorf("expected at least 2 server connections, got %d", connCount.Load())
	}
}

// TestSSEStream_MultipleHandlersSameTopic verifies that when two handlers are
// registered for the same topic, both handlers receive the event.
func TestSSEStream_MultipleHandlersSameTopic(t *testing.T) {
	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		fmt.Fprintf(w, "id:1\n")
		fmt.Fprintf(w, "event:beads.bead.created\n")
		fmt.Fprintf(w, "data:{\"bead\":{\"id\":\"multi-1\",\"type\":\"decision\"}}\n")
		fmt.Fprintf(w, "\n")
		flusher.Flush()

		time.Sleep(200 * time.Millisecond)
	}))
	defer sseServer.Close()

	var handler1Count, handler2Count atomic.Int32

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: sseServer.URL,
		Logger:        slog.Default(),
	})

	// Register two handlers for the same topic.
	stream.On("beads.bead.created", func(_ context.Context, data []byte) {
		handler1Count.Add(1)
	})
	stream.On("beads.bead.created", func(_ context.Context, data []byte) {
		handler2Count.Add(1)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = stream.Start(ctx)
	}()

	// Wait for both handlers to fire.
	deadline := time.After(2 * time.Second)
	for {
		if handler1Count.Load() >= 1 && handler2Count.Load() >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out: handler1=%d handler2=%d",
				handler1Count.Load(), handler2Count.Load())
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()

	if handler1Count.Load() != 1 {
		t.Errorf("expected handler1 called once, got %d", handler1Count.Load())
	}
	if handler2Count.Load() != 1 {
		t.Errorf("expected handler2 called once, got %d", handler2Count.Load())
	}
}

// TestSSEStream_MalformedJSON verifies that when the server sends an event with
// malformed JSON in the data: field, handlers are not called for that event, the
// stream does not crash, and subsequent valid events are still delivered.
func TestSSEStream_MalformedJSON(t *testing.T) {
	sseServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming not supported", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send event with malformed JSON data.
		fmt.Fprintf(w, "id:1\n")
		fmt.Fprintf(w, "event:beads.bead.created\n")
		fmt.Fprintf(w, "data:{this is not valid JSON!!!\n")
		fmt.Fprintf(w, "\n")
		flusher.Flush()

		// Send a valid event after the bad one.
		fmt.Fprintf(w, "id:2\n")
		fmt.Fprintf(w, "event:beads.bead.created\n")
		fmt.Fprintf(w, "data:{\"bead\":{\"id\":\"good-1\",\"type\":\"decision\"}}\n")
		fmt.Fprintf(w, "\n")
		flusher.Flush()

		time.Sleep(200 * time.Millisecond)
	}))
	defer sseServer.Close()

	var mu sync.Mutex
	var receivedData []string

	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: sseServer.URL,
		Logger:        slog.Default(),
	})

	// The bridge SSEStream dispatches raw bytes to handlers -- it does not
	// parse JSON itself. Both the malformed and good payloads will be delivered.
	// The handler is responsible for parsing. We verify the stream keeps running
	// and delivers both.
	stream.On("beads.bead.created", func(_ context.Context, data []byte) {
		mu.Lock()
		receivedData = append(receivedData, string(data))
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() {
		_ = stream.Start(ctx)
	}()

	// Wait for at least 2 events (both the bad and good data are dispatched).
	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		count := len(receivedData)
		mu.Unlock()
		if count >= 2 {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("timed out waiting for events, got %d: %v", len(receivedData), receivedData)
			mu.Unlock()
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()

	mu.Lock()
	defer mu.Unlock()

	// First payload is malformed -- ParseBeadEvent should return nil.
	if ParseBeadEvent([]byte(receivedData[0])) != nil {
		t.Errorf("expected ParseBeadEvent to return nil for malformed data, got non-nil")
	}

	// Second payload is valid.
	bead := ParseBeadEvent([]byte(receivedData[1]))
	if bead == nil {
		t.Fatal("expected ParseBeadEvent to return non-nil for valid data")
	}
	if bead.ID != "good-1" {
		t.Errorf("expected bead ID good-1, got %s", bead.ID)
	}
}

// TestSSEStream_ParsesOversizedDataLine verifies the bridge SSE consumer
// parses an event whose `data:` line exceeds any fixed bufio.Scanner cap.
// Regression test for the slack-bridge variant of gd-hWOJwtNzsA: the bridge
// previously used bufio.Scanner with a 256 KiB ceiling, so any oversized
// bead event tripped "token too long" without advancing Last-Event-ID — the
// reconnect loop replayed the same offending event forever, dropping every
// subsequent decision/mail/agent event for the duration of the loop.
func TestSSEStream_ParsesOversizedDataLine(t *testing.T) {
	// 5 MiB payload comfortably exceeds bufio.MaxScanTokenSize (64 KiB), the
	// prior 256 KiB explicit cap, and a generous 4 MiB cap — proving the
	// consumer no longer has a fixed line-length ceiling.
	bigBlob := make([]byte, 5*1024*1024)
	for i := range bigBlob {
		bigBlob[i] = 'x'
	}
	payload := map[string]any{
		"bead": map[string]any{
			"id":     "gd-big",
			"title":  string(bigBlob),
			"type":   "decision",
			"status": "open",
		},
	}
	data, _ := json.Marshal(payload)
	if len(data) <= 4*1024*1024 {
		t.Fatalf("test fixture must exceed 4 MiB; got %d", len(data))
	}

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only serve the first connection — closing afterwards lets the
		// stream reconnect harmlessly while the test reads the event.
		if requests.Add(1) > 1 {
			time.Sleep(200 * time.Millisecond)
			return
		}
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		fmt.Fprintf(w, "id:42\nevent:beads.bead.created\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	gotEvent := make(chan *BeadEvent, 1)
	stream := NewSSEStream(SSEStreamConfig{
		BeadsHTTPAddr: srv.URL,
		Logger:        slog.Default(),
	})
	stream.On("beads.bead.created", func(_ context.Context, raw []byte) {
		select {
		case gotEvent <- ParseBeadEvent(raw):
		default:
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = stream.Start(ctx) }()

	select {
	case event := <-gotEvent:
		if event == nil {
			t.Fatal("expected non-nil parsed bead event")
		}
		if event.ID != "gd-big" {
			t.Fatalf("expected gd-big, got %s", event.ID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for oversized SSE event — consumer likely tripped a line-length cap")
	}

	if got := stream.LastID(); got != "42" {
		t.Fatalf("expected last event ID 42, got %s", got)
	}
}
