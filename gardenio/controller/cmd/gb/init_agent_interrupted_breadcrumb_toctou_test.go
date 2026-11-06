package main

import (
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// toctouMockDaemon extends the dedup mock with request-order tracking and
// reply-failure injection, to pin the ka-0000002080 mark-before-post ordering:
// the dedup marker PATCH must land BEFORE the breadcrumb POST, and a failed
// POST must roll the marker back.
type toctouMockDaemon struct {
	mu          sync.Mutex
	agentBeadID string
	fields      map[string]any
	order       []string // "patch" / "reply" in arrival order
	failReply   bool
	replyPosts  int
}

func (m *toctouMockDaemon) handler(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/slack/threads/reply":
			m.order = append(m.order, "reply")
			if m.failReply {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
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
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})

		case r.Method == http.MethodPatch && r.URL.Path == "/v1/beads/"+m.agentBeadID:
			m.order = append(m.order, "patch")
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
			_ = json.NewEncoder(w).Encode(map[string]any{"id": m.agentBeadID})

		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}
}

func (m *toctouMockDaemon) markerValue() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, _ := m.fields[interruptedBreadcrumbPostedField].(string)
	return v
}

func (m *toctouMockDaemon) firstPatchBeforeFirstReply() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ev := range m.order {
		switch ev {
		case "patch":
			return true
		case "reply":
			return false
		}
	}
	return false
}

func setupToctouEnv(t *testing.T, mock *toctouMockDaemon) {
	t.Helper()
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	withActor(t, "gardenio-thread-toctou")
	t.Setenv("KD_AGENT_ID", mock.agentBeadID)
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("SLACK_BRIDGE_URL", srv.URL)
	t.Setenv("SLACK_THREAD_CHANNEL", "C42")
	t.Setenv("SLACK_THREAD_TS", "111.222")
}

// TestInterruptedBreadcrumb_MarksBeforePosting pins the ka-0000002080 ordering
// fix: the dedup marker must be stamped BEFORE the breadcrumb POST, so a
// racing teardown of another pod incarnation that reads the bead after the
// stamp — but while the POST is still in flight — sees the marker set and
// stays silent. Under post-then-mark, a hard-kill between the two left the
// marker unset for every next incarnation (12 duplicate posts observed).
func TestInterruptedBreadcrumb_MarksBeforePosting(t *testing.T) {
	mock := &toctouMockDaemon{agentBeadID: "gd-thread-toctou", fields: map[string]any{}}
	setupToctouEnv(t, mock)

	closeAgentBeadBestEffort("context_cancelled")

	if mock.replyPosts != 1 {
		t.Fatalf("expected exactly 1 breadcrumb post, got %d", mock.replyPosts)
	}
	if !mock.firstPatchBeforeFirstReply() {
		t.Fatalf("dedup marker must be stamped BEFORE the breadcrumb POST (mark-before-post); observed order: %v", mock.order)
	}
	if mock.markerValue() == "" {
		t.Errorf("marker must remain stamped after a successful post")
	}
}

// TestInterruptedBreadcrumb_FailedPostRollsBackMarker verifies the trade-off
// guard on mark-before-post: when the POST fails (bridge down / non-200), the
// prematurely-stamped marker is rolled back so a later legitimate teardown of
// this same open cycle can still post the one breadcrumb the human should see.
func TestInterruptedBreadcrumb_FailedPostRollsBackMarker(t *testing.T) {
	mock := &toctouMockDaemon{
		agentBeadID: "gd-thread-toctou-fail",
		fields:      map[string]any{},
		failReply:   true,
	}
	setupToctouEnv(t, mock)

	closeAgentBeadBestEffort("context_cancelled")

	if mock.replyPosts != 0 {
		t.Fatalf("failing bridge must record no successful post, got %d", mock.replyPosts)
	}
	if got := mock.markerValue(); got != "" {
		t.Fatalf("marker must be rolled back after a failed post, got %q", got)
	}

	// The bridge recovers — the next teardown of the same open cycle posts.
	mock.mu.Lock()
	mock.failReply = false
	mock.mu.Unlock()

	closeAgentBeadBestEffort("context_cancelled")

	if mock.replyPosts != 1 {
		t.Fatalf("recovered bridge should get exactly 1 post, got %d", mock.replyPosts)
	}
	if mock.markerValue() == "" {
		t.Errorf("marker must be stamped after the successful post")
	}
}

// TestInterruptedBreadcrumb_NonThreadAgentNoDaemonWrites verifies the
// thread-bound gate in postInterruptedBreadcrumbOnce: a gardener/crew teardown
// (no SLACK_THREAD_* env) must not pay the stamp+rollback PATCH cycle on
// every context_cancelled exit.
func TestInterruptedBreadcrumb_NonThreadAgentNoDaemonWrites(t *testing.T) {
	mock := &toctouMockDaemon{agentBeadID: "gd-crew-no-thread", fields: map[string]any{}}
	setupToctouEnv(t, mock)
	// Not thread-bound: bridge URL present, no thread coordinates. The
	// resolveSlackEnv fallback then reads BOAT_AGENT_BEAD_ID, which
	// setupToctouEnv leaves empty — so it short-circuits without a lookup.
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")

	closeAgentBeadBestEffort("context_cancelled")

	if mock.replyPosts != 0 {
		t.Fatalf("non-thread agent must not post a breadcrumb, got %d", mock.replyPosts)
	}
	mock.mu.Lock()
	order := append([]string(nil), mock.order...)
	mock.mu.Unlock()
	for _, ev := range order {
		if ev == "patch" {
			t.Fatalf("non-thread agent must not PATCH the dedup marker; observed order: %v", order)
		}
	}
}
