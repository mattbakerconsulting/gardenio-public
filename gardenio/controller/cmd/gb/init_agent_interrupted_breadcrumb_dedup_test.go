package main

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// dedupMockDaemon is a stateful in-memory agent bead + Slack bridge, covering
// the endpoints the breadcrumb dedup path touches:
//   - GET   /v1/beads/<agentBead>                     (GetBead: alreadyClosed + marker + tool-call guards)
//   - PATCH /v1/beads/<agentBead>                     (UpdateBeadFields: stamp / clear the marker)
//   - GET   /v1/beads?status=in_progress&kind=issue   (ListAssignedTask: no claim)
//   - POST  /v1/beads/<agentBead>/close               (CloseBead)
//   - POST  /api/slack/threads/reply                  (the Slack bridge breadcrumb)
//
// PATCH mutates the in-memory field map so a subsequent GetBead observes the
// marker exactly as the real additive-merge daemon would — that's what makes
// the "second teardown does not repost" assertion meaningful.
type dedupMockDaemon struct {
	mu          sync.Mutex
	agentBeadID string
	fields      map[string]any
	replyPosts  int
	closePosts  int
}

func (m *dedupMockDaemon) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/slack/threads/reply":
			m.replyPosts++
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(slackReplyResult{OK: true, Timestamp: "1.2"})

		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+m.agentBeadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     m.agentBeadID,
				"type":   "agent",
				"status": "open",
				"fields": m.fields,
			})

		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// ListAssignedTask — no claim held.
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})

		case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/"+m.agentBeadID:
			body, _ := io.ReadAll(r.Body)
			var patch struct {
				Fields json.RawMessage `json:"fields"`
			}
			_ = json.Unmarshal(body, &patch)
			var incoming map[string]any
			_ = json.Unmarshal(patch.Fields, &incoming)
			maps.Copy(m.fields, incoming)
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+m.agentBeadID+"/close":
			m.closePosts++
			_ = json.NewEncoder(w).Encode(map[string]any{"id": m.agentBeadID})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}
}

func (m *dedupMockDaemon) markerValue() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, _ := m.fields[interruptedBreadcrumbPostedField].(string)
	return v
}

func (m *dedupMockDaemon) counts() (replies, closes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.replyPosts, m.closePosts
}

// TestInterruptedBreadcrumb_DedupsAcrossSuccessiveTeardowns is the core fix for
// ka-0000002138: a thread agent whose still-open bead is SIGTERM'd and
// cold-resumed many times in a row must post the "I was interrupted" breadcrumb
// at most ONCE per open interruption cycle — not once per teardown (the bug: one
// thread got 20 identical posts).
func TestInterruptedBreadcrumb_DedupsAcrossSuccessiveTeardowns(t *testing.T) {
	const beadID = "gd-thread-interrupted"
	mock := &dedupMockDaemon{
		agentBeadID: beadID,
		fields:      map[string]any{},
	}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-thread-xyz")

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	// Thread-bound: the breadcrumb path targets the same bridge server.
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C42")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	// Simulate 5 successive involuntary teardowns of the SAME still-open bead.
	for range 5 {
		closeAgentBeadBestEffort("context_cancelled")
	}

	replies, closes := mock.counts()
	if replies != 1 {
		t.Fatalf("expected exactly 1 breadcrumb across 5 teardowns, got %d", replies)
	}
	if closes != 5 {
		t.Errorf("expected 5 close attempts (one per teardown), got %d", closes)
	}
	if mock.markerValue() == "" {
		t.Errorf("expected the dedup marker to be stamped after the first post")
	}
}

// TestInterruptedBreadcrumb_ClearedMarkerAllowsRepost verifies the marker is not
// a permanent gag: once a resumed session works again the marker is cleared, and
// a genuinely NEW interruption in that fresh work cycle posts one more time.
func TestInterruptedBreadcrumb_ClearedMarkerAllowsRepost(t *testing.T) {
	const beadID = "gd-thread-recycled"
	mock := &dedupMockDaemon{
		agentBeadID: beadID,
		fields:      map[string]any{},
	}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-thread-recycled")

	t.Setenv("KD_AGENT_ID", beadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C42")
	t.Setenv("SLACK_THREAD_TS", "111.222")

	// First interruption cycle: posts once and stamps the marker.
	closeAgentBeadBestEffort("context_cancelled")
	closeAgentBeadBestEffort("context_cancelled")
	if replies, _ := mock.counts(); replies != 1 {
		t.Fatalf("first cycle should post exactly once, got %d", replies)
	}

	// The agent resumes and works again — the loop clears the marker.
	clearInterruptedBreadcrumbMarker(context.Background(), beadID)
	if mock.markerValue() != "" {
		t.Fatalf("marker should be cleared after a productive resume, got %q", mock.markerValue())
	}

	// A brand-new interruption in the fresh work cycle posts once more.
	closeAgentBeadBestEffort("context_cancelled")
	if replies, _ := mock.counts(); replies != 2 {
		t.Fatalf("a new interruption after a productive resume should post once more (total 2), got %d", replies)
	}
}

// TestMarkInterruptedBreadcrumbPosted_StampsMarker verifies the stamp helper
// writes a non-empty timestamp into the dedup field.
func TestMarkInterruptedBreadcrumbPosted_StampsMarker(t *testing.T) {
	const beadID = "gd-thread-stamp"
	mock := &dedupMockDaemon{agentBeadID: beadID, fields: map[string]any{}}
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)

	markInterruptedBreadcrumbPosted(context.Background(), beadID)

	if got := mock.markerValue(); got == "" {
		t.Errorf("expected a non-empty marker timestamp, got empty")
	}
}

// TestClearInterruptedBreadcrumbMarker_NoOpWhenAlreadyClear verifies the clear
// helper does not issue a PATCH (no daemon write / SSE churn) when the marker is
// already absent — the common case on every productive session.
func TestClearInterruptedBreadcrumbMarker_NoOpWhenAlreadyClear(t *testing.T) {
	const beadID = "gd-thread-noop"
	var patches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": beadID, "type": "agent", "status": "open",
				"fields": map[string]any{}, // marker absent
			})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			patches++
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)

	clearInterruptedBreadcrumbMarker(context.Background(), beadID)

	if patches != 0 {
		t.Errorf("clearing an already-clear marker must not PATCH, got %d PATCH calls", patches)
	}
}
