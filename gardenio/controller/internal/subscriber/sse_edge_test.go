package subscriber

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSSEWatcher_LastEventIDSentOnReconnect verifies that the Last-Event-ID
// header is sent when reconnecting.
func TestSSEWatcher_LastEventIDSentOnReconnect(t *testing.T) {
	requestCount := 0
	lastEventIDSeen := ""

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		lastEventIDSeen = r.Header.Get("Last-Event-ID")

		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		if requestCount == 1 {
			// First connection: send an event then close.
			payload := map[string]any{
				"bead": map[string]any{
					"id":     "gd-r1",
					"type":   "agent",
					"status": "in_progress",
					"fields": map[string]string{"project": "p", "role": "devops", "agent": "a1"},
				},
			}
			data, _ := json.Marshal(payload)
			fmt.Fprintf(w, "id:99\nevent:beads.bead.created\ndata:%s\n\n", data)
			flusher.Flush()
			time.Sleep(50 * time.Millisecond)
			return // close connection to trigger reconnect
		}

		// Second connection: verify Last-Event-ID and send another event.
		payload := map[string]any{
			"bead": map[string]any{
				"id":     "gd-r2",
				"type":   "agent",
				"status": "in_progress",
				"fields": map[string]string{"project": "p", "role": "devops", "agent": "a2"},
			},
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "id:100\nevent:beads.bead.created\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Read first event.
	select {
	case event := <-w.Events():
		if event.BeadID != "gd-r1" {
			t.Fatalf("expected gd-r1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for first event")
	}

	// Read second event (after reconnect).
	select {
	case event := <-w.Events():
		if event.BeadID != "gd-r2" {
			t.Fatalf("expected gd-r2, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for second event")
	}

	cancel()

	// Verify the Last-Event-ID was sent on reconnection.
	if lastEventIDSeen != "99" {
		t.Fatalf("expected Last-Event-ID 99 on reconnect, got %q", lastEventIDSeen)
	}
}

// TestSSEWatcher_ReconnectsOnServerClose verifies that the watcher reconnects
// after the server closes the connection, and continues receiving events from
// the new connection.
func TestSSEWatcher_ReconnectsOnServerClose(t *testing.T) {
	var connCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connCount.Add(1)
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Each connection sends one event with a unique bead ID, then closes.
		beadID := fmt.Sprintf("gd-conn%d", n)
		data := makeAgentPayload(beadID)
		fmt.Fprintf(w, "id:%d\nevent:beads.bead.created\ndata:%s\n\n", n, data)
		flusher.Flush()
		time.Sleep(50 * time.Millisecond)
		// Return to close the connection and trigger reconnect.
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Read event from first connection.
	select {
	case event := <-w.Events():
		if event.BeadID != "gd-conn1" {
			t.Fatalf("expected gd-conn1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for first event")
	}

	// Read event from second connection (after automatic reconnect).
	select {
	case event := <-w.Events():
		if event.BeadID != "gd-conn2" {
			t.Fatalf("expected gd-conn2, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for second event (reconnect)")
	}

	cancel()

	// Verify that at least two connections were made.
	if connCount.Load() < 2 {
		t.Fatalf("expected at least 2 connections, got %d", connCount.Load())
	}
}

// TestSSEWatcher_ServerReturns500 verifies that the watcher retries with backoff
// when the server returns HTTP 500. The second request should arrive after a delay.
func TestSSEWatcher_ServerReturns500(t *testing.T) {
	var requestTimes []time.Time
	var connCount atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := connCount.Add(1)
		requestTimes = append(requestTimes, time.Now())

		if n == 1 {
			// First request: return 500.
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		// Second request: return valid SSE with an event.
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		data := makeAgentPayload("gd-after500")
		fmt.Fprintf(w, "id:1\nevent:beads.bead.created\ndata:%s\n\n", data)
		flusher.Flush()
		time.Sleep(100 * time.Millisecond)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx) }()

	// Should eventually receive the event from the second (successful) connection.
	select {
	case event := <-w.Events():
		if event.BeadID != "gd-after500" {
			t.Fatalf("expected gd-after500, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event after 500 retry")
	}

	cancel()

	// Verify backoff: second request should arrive at least 900ms after first
	// (initial backoff is 1s).
	if len(requestTimes) < 2 {
		t.Fatalf("expected at least 2 requests, got %d", len(requestTimes))
	}
	delay := requestTimes[1].Sub(requestTimes[0])
	if delay < 900*time.Millisecond {
		t.Fatalf("expected backoff of at least 900ms, got %s", delay)
	}
}

// TestSSEWatcher_MalformedSSEData verifies that corrupt JSON in the data: field
// does not crash the watcher and that subsequent good events are still delivered.
func TestSSEWatcher_MalformedSSEData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send a malformed event (corrupt JSON).
		fmt.Fprintf(w, "id:1\nevent:beads.bead.created\ndata:{corrupt json!!! not valid\n\n")
		flusher.Flush()

		// Send a valid event after the bad one.
		data := makeAgentPayload("gd-good1")
		fmt.Fprintf(w, "id:2\nevent:beads.bead.created\ndata:%s\n\n", data)
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

	// The malformed event should be skipped; we should get the good event.
	select {
	case event := <-w.Events():
		if event.BeadID != "gd-good1" {
			t.Fatalf("expected gd-good1, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out: watcher may have crashed on malformed JSON")
	}

	// Verify the last event ID advanced past the bad event.
	assertLastEventID(t, w, "2")

	cancel()
}

// TestSSEWatcher_ContextCancellation verifies that canceling the context during
// a reconnection backoff causes the watcher to stop promptly without hanging.
func TestSSEWatcher_ContextCancellation(t *testing.T) {
	// Server always returns 500 to force the watcher into repeated backoff.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	w := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- w.Start(ctx)
	}()

	// Let the watcher make at least one failed attempt and enter backoff.
	time.Sleep(500 * time.Millisecond)

	// Cancel during backoff.
	cancel()

	// Watcher should stop within a short time (well before a full 1s backoff cycle).
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected non-nil error from canceled watcher")
		}
		// The error should wrap context.Canceled.
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop promptly after context cancellation")
	}
}

// TestSSEWatcher_MissingEventField verifies that an SSE message with data: but
// no event: line is handled gracefully (skipped, no crash) and doesn't block
// subsequent valid events.
func TestSSEWatcher_MissingEventField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, _ := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		// Send an SSE message with data but no event: line.
		data := makeAgentPayload("gd-noevent")
		fmt.Fprintf(w, "id:1\ndata:%s\n\n", data)
		flusher.Flush()

		// Send a proper event after.
		data2 := makeAgentPayload("gd-proper")
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

	// The missing-event message should be skipped; we should get the proper one.
	select {
	case event := <-w.Events():
		if event.BeadID != "gd-proper" {
			t.Fatalf("expected gd-proper, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for event after missing-event-field message")
	}

	// Verify lastEventID still advanced through the no-event message.
	assertLastEventID(t, w, "2")

	cancel()
}

// TestSSEWatcher_ParsesOversizedDataLine verifies that the consumer parses a
// single SSE event whose `data:` line is larger than the legacy bufio.Scanner
// MaxScanTokenSize (64 KiB) and the prior 256 KiB explicit cap. Regression
// test for gd-hWOJwtNzsA — the controller used bufio.Scanner with a fixed
// buffer cap, so any oversized event tripped "token too long" and the watcher
// reconnected without advancing Last-Event-ID, replaying forever.
func TestSSEWatcher_ParsesOversizedDataLine(t *testing.T) {
	// Build a payload whose JSON serialization is well past 64 KiB and
	// also past the prior 4 MiB explicit cap on the bufio.Scanner buffer.
	// 5 MiB of payload comfortably exceeds the legacy
	// bufio.MaxScanTokenSize (64 KiB), the original 256 KiB explicit
	// cap, and the bumped 4 MiB cap — proving the consumer no longer
	// has a fixed line-length ceiling.
	bigBlob := make([]byte, 5*1024*1024)
	for i := range bigBlob {
		bigBlob[i] = 'x'
	}
	payload := map[string]any{
		"bead": map[string]any{
			"id":     "gd-big",
			"title":  string(bigBlob),
			"type":   "agent",
			"status": "in_progress",
			"fields": map[string]string{
				"project": "p",
				"role":    "devops",
				"agent":   "big-agent",
			},
		},
	}
	data, _ := json.Marshal(payload)
	if len(data) <= 4*1024*1024 {
		t.Fatalf("test fixture must exceed 4 MiB; got %d", len(data))
	}

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only serve the first connection — closing afterwards lets the
		// watcher reconnect harmlessly while the test reads the event.
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

	watcher := NewSSEWatcher(SSEConfig{
		BeadsHTTPAddr: srv.URL,
		Namespace:     "ns",
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = watcher.Start(ctx) }()

	select {
	case event := <-watcher.Events():
		if event.BeadID != "gd-big" {
			t.Fatalf("expected gd-big, got %s", event.BeadID)
		}
	case <-ctx.Done():
		t.Fatal("timed out waiting for oversized SSE event — consumer likely tripped a line-length cap")
	}

	assertLastEventID(t, watcher, "42")

	cancel()
}
