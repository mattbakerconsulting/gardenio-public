package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseEventParsed represents a single parsed SSE event from the stream.
type sseEventParsed struct {
	ID    string
	Event string
	Data  string
}

// sseReader reads SSE events from an HTTP response body using a bufio.Scanner.
// It sends parsed events to the returned channel and stops when the context is cancelled
// or the body is closed.
func sseReader(ctx context.Context, resp *http.Response) <-chan sseEventParsed {
	ch := make(chan sseEventParsed, 32)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(resp.Body)
		var current sseEventParsed
		for scanner.Scan() {
			select {
			case <-ctx.Done():
				return
			default:
			}

			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "id:"):
				current.ID = strings.TrimPrefix(line, "id:")
			case strings.HasPrefix(line, "event:"):
				current.Event = strings.TrimPrefix(line, "event:")
			case strings.HasPrefix(line, "data:"):
				current.Data = strings.TrimPrefix(line, "data:")
			case line == "":
				// Empty line marks end of SSE event block.
				if current.Event != "" || current.Data != "" {
					ch <- current
					current = sseEventParsed{}
				}
			}
		}
	}()
	return ch
}

// waitForEvent reads from the SSE event channel until an event with the given
// topic is received, or the timeout expires.
func waitForEvent(t *testing.T, ch <-chan sseEventParsed, topic string, timeout time.Duration) sseEventParsed {
	t.Helper()
	timer := time.After(timeout)
	for {
		select {
		case evt, ok := <-ch:
			if !ok {
				t.Fatalf("SSE channel closed before receiving event %q", topic)
			}
			if evt.Event == topic {
				return evt
			}
			// Keep reading; may receive other events first.
		case <-timer:
			t.Fatalf("timed out waiting for SSE event %q", topic)
		}
	}
}

// startSSEClient opens an SSE connection to the test server and returns a channel
// of parsed events plus a cancel function. The caller must call cancel when done.
func startSSEClient(t *testing.T, serverURL string, queryParams string) (<-chan sseEventParsed, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	url := serverURL + "/v1/events/stream"
	if queryParams != "" {
		url += "?" + queryParams
	}

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		t.Fatalf("failed to create SSE request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("failed to connect to SSE stream: %v", err)
	}

	if resp.Header.Get("Content-Type") != "text/event-stream" {
		resp.Body.Close()
		cancel()
		t.Fatalf("expected Content-Type=text/event-stream, got %q", resp.Header.Get("Content-Type"))
	}

	ch := sseReader(ctx, resp)

	// Return a wrapped cancel that also closes the body.
	cleanup := func() {
		cancel()
		resp.Body.Close()
	}

	return ch, cleanup
}

// startIntegrationServer creates a test server with a real TCP listener for
// integration tests, returning the server URL and HTTP handler for direct calls.
func startIntegrationServer(t *testing.T) (string, http.Handler, func()) {
	t.Helper()
	_, _, handler := newTestServer()
	ts := httptest.NewServer(handler)
	return ts.URL, handler, ts.Close
}

// doHTTPJSON performs an HTTP request with an optional JSON body against a real server URL.
func doHTTPJSON(t *testing.T, method, url string, body any) *http.Response {
	t.Helper()
	var req *http.Request
	var err error
	if body != nil {
		b, _ := json.Marshal(body)
		req, err = http.NewRequest(method, url, strings.NewReader(string(b)))
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
	} else {
		req, err = http.NewRequest(method, url, nil)
		if err != nil {
			t.Fatalf("failed to create request: %v", err)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP request failed: %v", err)
	}
	return resp
}

// requireHTTPStatus asserts the response has the expected status code.
func requireHTTPStatus(t *testing.T, resp *http.Response, code int) {
	t.Helper()
	if resp.StatusCode != code {
		t.Fatalf("expected status %d, got %d", code, resp.StatusCode)
	}
}

// decodeHTTPJSON decodes the response body JSON into v.
func decodeHTTPJSON(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
}

// --- Integration Tests ---

func TestSSEIntegration_CreateBeadTriggersEvent(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Connect SSE client.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "")
	defer sseCancel()

	// Give the SSE subscription time to register.
	time.Sleep(50 * time.Millisecond)

	// Create a bead via HTTP.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Integration test bead", "type": "task", "created_by": "alice",
	})
	requireHTTPStatus(t, resp, 201)

	var createdBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &createdBead)
	if createdBead.ID == "" {
		t.Fatal("expected created bead to have an ID")
	}

	// Wait for the SSE event.
	evt := waitForEvent(t, sseEvents, "beads.bead.created", 2*time.Second)

	// Verify the event data contains the bead.
	var payload struct {
		Bead struct {
			ID string `json:"id"`
		} `json:"bead"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		t.Fatalf("failed to parse SSE data: %v", err)
	}
	if payload.Bead.ID != createdBead.ID {
		t.Fatalf("SSE event bead ID=%q does not match created bead ID=%q", payload.Bead.ID, createdBead.ID)
	}
	if evt.ID == "" {
		t.Fatal("expected SSE event to have a non-empty ID")
	}
}

func TestSSEIntegration_UpdateBeadTriggersEvent(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Pre-create a bead.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Bead to update", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var createdBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &createdBead)

	// Connect SSE client.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// Update the bead via PATCH.
	resp = doHTTPJSON(t, "PATCH", serverURL+"/v1/beads/"+createdBead.ID, map[string]any{
		"title": "Updated title",
	})
	requireHTTPStatus(t, resp, 200)

	// Wait for the SSE event.
	evt := waitForEvent(t, sseEvents, "beads.bead.updated", 2*time.Second)

	var payload struct {
		Bead struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"bead"`
		Changes map[string]any `json:"changes"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		t.Fatalf("failed to parse SSE data: %v", err)
	}
	if payload.Bead.ID != createdBead.ID {
		t.Fatalf("SSE event bead ID=%q does not match expected %q", payload.Bead.ID, createdBead.ID)
	}
	if payload.Bead.Title != "Updated title" {
		t.Fatalf("expected updated title in SSE event, got %q", payload.Bead.Title)
	}
	if _, ok := payload.Changes["title"]; !ok {
		t.Fatal("expected 'title' in changes map")
	}
}

func TestSSEIntegration_CloseBeadTriggersEvent(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Pre-create a bead.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Bead to close", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var createdBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &createdBead)

	// Connect SSE client.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// Close the bead. SECURITY (AUDIT §C3): closed_by in the body is now
	// ignored; actor is derived from the authenticated identity. The test
	// server's default-bearer middleware authenticates as defaultTestSubject,
	// which must win over the forged body value.
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+createdBead.ID+"/close", map[string]any{
		"closed_by": "evil-forged",
	})
	requireHTTPStatus(t, resp, 200)

	// Wait for the SSE event.
	evt := waitForEvent(t, sseEvents, "beads.bead.closed", 2*time.Second)

	var payload struct {
		Bead struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"bead"`
		ClosedBy string `json:"closed_by"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		t.Fatalf("failed to parse SSE data: %v", err)
	}
	if payload.Bead.ID != createdBead.ID {
		t.Fatalf("SSE event bead ID=%q does not match expected %q", payload.Bead.ID, createdBead.ID)
	}
	if payload.Bead.Status != "closed" {
		t.Fatalf("expected status=closed in SSE event, got %q", payload.Bead.Status)
	}
	// Server must have ignored the forged closed_by from the body and used
	// the authenticated subject instead.
	if payload.ClosedBy != defaultTestSubject {
		t.Fatalf("expected closed_by=%q (forged body value ignored), got %q",
			defaultTestSubject, payload.ClosedBy)
	}
}

func TestSSEIntegration_DeleteBeadTriggersEvent(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Pre-create a bead.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Bead to delete", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var createdBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &createdBead)

	// Connect SSE client.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// Delete the bead.
	resp = doHTTPJSON(t, "DELETE", serverURL+"/v1/beads/"+createdBead.ID, nil)
	requireHTTPStatus(t, resp, 204)
	resp.Body.Close()

	// Wait for the SSE event.
	evt := waitForEvent(t, sseEvents, "beads.bead.deleted", 2*time.Second)

	var payload struct {
		BeadID string `json:"bead_id"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		t.Fatalf("failed to parse SSE data: %v", err)
	}
	if payload.BeadID != createdBead.ID {
		t.Fatalf("SSE event bead_id=%q does not match expected %q", payload.BeadID, createdBead.ID)
	}
}

func TestSSEIntegration_TopicFilterOnlyReceivesMatching(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Pre-create a bead.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Filter test bead", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var createdBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &createdBead)

	// Connect SSE client that only wants beads.bead.created events.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "topics=beads.bead.created")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// Create a new bead (should produce beads.bead.created -- matching).
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Another bead", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var newBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &newBead)

	// Close the first bead (should produce beads.bead.closed -- NOT matching).
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+createdBead.ID+"/close", nil)
	requireHTTPStatus(t, resp, 200)
	resp.Body.Close()

	// Verify we receive the create event.
	evt := waitForEvent(t, sseEvents, "beads.bead.created", 2*time.Second)
	var payload struct {
		Bead struct {
			ID string `json:"id"`
		} `json:"bead"`
	}
	if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
		t.Fatalf("failed to parse SSE data: %v", err)
	}
	if payload.Bead.ID != newBead.ID {
		t.Fatalf("expected bead ID=%q in create event, got %q", newBead.ID, payload.Bead.ID)
	}

	// Verify no close event arrives within a short window.
	select {
	case extra := <-sseEvents:
		if extra.Event == "beads.bead.closed" {
			t.Fatalf("received unexpected beads.bead.closed event (should have been filtered)")
		}
		// Other events (like keepalive) are OK, but closed should not appear.
	case <-time.After(200 * time.Millisecond):
		// Good -- no unwanted events.
	}
}

func TestSSEIntegration_MultipleClientsReceiveSameEvents(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Connect two SSE clients.
	sseEvents1, sseCancel1 := startSSEClient(t, serverURL, "")
	defer sseCancel1()
	sseEvents2, sseCancel2 := startSSEClient(t, serverURL, "")
	defer sseCancel2()
	time.Sleep(50 * time.Millisecond)

	// Create a bead.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Multi-client test", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var createdBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &createdBead)

	// Both clients should receive the event.
	evt1 := waitForEvent(t, sseEvents1, "beads.bead.created", 2*time.Second)
	evt2 := waitForEvent(t, sseEvents2, "beads.bead.created", 2*time.Second)

	// Verify both received the same bead.
	for i, evt := range []sseEventParsed{evt1, evt2} {
		var payload struct {
			Bead struct {
				ID string `json:"id"`
			} `json:"bead"`
		}
		if err := json.Unmarshal([]byte(evt.Data), &payload); err != nil {
			t.Fatalf("client %d: failed to parse SSE data: %v", i+1, err)
		}
		if payload.Bead.ID != createdBead.ID {
			t.Fatalf("client %d: SSE event bead ID=%q does not match expected %q", i+1, payload.Bead.ID, createdBead.ID)
		}
	}

	// Both events should have the same sequence ID.
	if evt1.ID != evt2.ID {
		t.Fatalf("expected same event ID for both clients, got %q and %q", evt1.ID, evt2.ID)
	}
}

func TestSSEIntegration_DependencyAndLabelEvents(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Pre-create two beads for the dependency.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Bead A", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var beadA struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &beadA)

	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Bead B", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var beadB struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &beadB)

	// Connect SSE client.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// Add a dependency.
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+beadA.ID+"/dependencies", map[string]any{
		"depends_on_id": beadB.ID, "type": "blocks", "created_by": "carol",
	})
	requireHTTPStatus(t, resp, 201)
	resp.Body.Close()

	// Verify dependency event.
	depEvt := waitForEvent(t, sseEvents, "beads.dependency.added", 2*time.Second)
	var depPayload struct {
		Dependency struct {
			BeadID      string `json:"bead_id"`
			DependsOnID string `json:"depends_on_id"`
			Type        string `json:"type"`
		} `json:"dependency"`
	}
	if err := json.Unmarshal([]byte(depEvt.Data), &depPayload); err != nil {
		t.Fatalf("failed to parse dependency event data: %v", err)
	}
	if depPayload.Dependency.BeadID != beadA.ID {
		t.Fatalf("expected dependency bead_id=%q, got %q", beadA.ID, depPayload.Dependency.BeadID)
	}
	if depPayload.Dependency.DependsOnID != beadB.ID {
		t.Fatalf("expected dependency depends_on_id=%q, got %q", beadB.ID, depPayload.Dependency.DependsOnID)
	}

	// Add a label.
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+beadA.ID+"/labels", map[string]any{
		"label": "urgent",
	})
	requireHTTPStatus(t, resp, 201)
	resp.Body.Close()

	// Verify label event.
	labelEvt := waitForEvent(t, sseEvents, "beads.label.added", 2*time.Second)
	var labelPayload struct {
		BeadID string `json:"bead_id"`
		Label  string `json:"label"`
	}
	if err := json.Unmarshal([]byte(labelEvt.Data), &labelPayload); err != nil {
		t.Fatalf("failed to parse label event data: %v", err)
	}
	if labelPayload.BeadID != beadA.ID {
		t.Fatalf("expected label event bead_id=%q, got %q", beadA.ID, labelPayload.BeadID)
	}
	if labelPayload.Label != "urgent" {
		t.Fatalf("expected label=%q, got %q", "urgent", labelPayload.Label)
	}
}

func TestSSEIntegration_CommentEvent(t *testing.T) {
	serverURL, _, cleanup := startIntegrationServer(t)
	defer cleanup()

	// Pre-create a bead.
	resp := doHTTPJSON(t, "POST", serverURL+"/v1/beads", map[string]any{
		"title": "Bead for comments", "type": "task",
	})
	requireHTTPStatus(t, resp, 201)
	var createdBead struct {
		ID string `json:"id"`
	}
	decodeHTTPJSON(t, resp, &createdBead)

	// Connect SSE client.
	sseEvents, sseCancel := startSSEClient(t, serverURL, "")
	defer sseCancel()
	time.Sleep(50 * time.Millisecond)

	// Add a comment. SECURITY (AUDIT §C3): body "author" is ignored; actor
	// is derived from the authenticated identity (the default-bearer
	// middleware authenticates as defaultTestSubject).
	resp = doHTTPJSON(t, "POST", serverURL+"/v1/beads/"+createdBead.ID+"/comments", map[string]any{
		"author": "evil-forged",
		"text":   "This is an important comment",
	})
	requireHTTPStatus(t, resp, 201)
	resp.Body.Close()

	// Verify comment event.
	commentEvt := waitForEvent(t, sseEvents, "beads.comment.added", 2*time.Second)
	var payload struct {
		Comment struct {
			BeadID string `json:"bead_id"`
			Author string `json:"author"`
			Text   string `json:"text"`
		} `json:"comment"`
	}
	if err := json.Unmarshal([]byte(commentEvt.Data), &payload); err != nil {
		t.Fatalf("failed to parse comment event data: %v", err)
	}
	if payload.Comment.BeadID != createdBead.ID {
		t.Fatalf("expected comment bead_id=%q, got %q", createdBead.ID, payload.Comment.BeadID)
	}
	// Server must have ignored the forged author from the body and used
	// the authenticated subject instead.
	if payload.Comment.Author != defaultTestSubject {
		t.Fatalf("expected comment author=%q (forged body value ignored), got %q",
			defaultTestSubject, payload.Comment.Author)
	}
	if payload.Comment.Text != "This is an important comment" {
		t.Fatalf("expected comment text=%q, got %q", "This is an important comment", payload.Comment.Text)
	}
}
