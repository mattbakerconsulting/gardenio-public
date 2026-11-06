package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// threadSiblingsMux serves the two GET /v1/beads queries outputThreadSiblings
// issues: (1) lookupSlackThreadBead's label query (labels=slack-thread:{c}:{ts})
// resolves the head, and (2) the membership query (field=thread_id=<head>)
// returns siblings. It matches on the query string so a single mux answers both.
func threadSiblingsMux(t *testing.T, headID string, siblings []map[string]any) *httptest.Server {
	t.Helper()
	head := map[string]any{"id": headID, "title": "Slack thread head", "type": "task", "kind": "issue", "status": "open"}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/v1/beads" && strings.HasPrefix(q.Get("labels"), "slack-thread:"):
			// Head lookup by canonical label.
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{head}, "total": 1})
		case r.URL.Path == "/v1/beads":
			// Membership query. Assert the field predicate is the thread_id
			// equality on the resolved head.
			if got := q.Get("field"); got != "thread_id="+headID {
				t.Errorf("membership query field = %q, want thread_id=%s", got, headID)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": siblings, "total": len(siblings)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func sibBead(id, title, typ, status string) map[string]any {
	return map[string]any{"id": id, "title": title, "type": typ, "kind": "issue", "status": status}
}

// TestOutputThreadSiblings_ListsClosedSiblings is the core regression for
// ka-0000001160: a resumed thread agent must see a CLOSED answer bead a prior
// session filed in the same thread, so it does not re-derive it.
func TestOutputThreadSiblings_ListsClosedSiblings(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "C0BDD2EGZ7E")
	t.Setenv("SLACK_THREAD_TS", "1782515504.882099")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-self-agent")

	const head = "gd-61pi6kb3sx"
	siblings := []map[string]any{
		sibBead("gd-answer", "Answer: env var config location for GSK sandbox portal", "task", "closed"),
		sibBead("gd-self-agent", "the resuming agent itself", "agent", "in_progress"), // must be dropped
		sibBead(head, "the head itself", "task", "open"),                              // must be dropped
	}
	srv := threadSiblingsMux(t, head, siblings)
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputThreadSiblings(&buf)
	out := buf.String()

	if !strings.Contains(out, "Prior beads in THIS Slack thread") {
		t.Fatalf("expected sibling header, got:\n%s", out)
	}
	if !strings.Contains(out, "gd-answer") {
		t.Errorf("expected the closed answer sibling gd-answer to be listed, got:\n%s", out)
	}
	if !strings.Contains(out, "[task/closed]") {
		t.Errorf("expected closed status rendered for the answer, got:\n%s", out)
	}
	// The agent's own bead and the head must not appear as "prior work".
	if strings.Contains(out, "gd-self-agent") {
		t.Errorf("agent's own bead must be excluded from the sibling list, got:\n%s", out)
	}
	if strings.Contains(out, "the head itself") {
		t.Errorf("thread head must be excluded from the sibling list, got:\n%s", out)
	}
	// The copy-paste query must reference the resolved head via the field.
	if !strings.Contains(out, "kd list -f thread_id="+head) {
		t.Errorf("expected the kd list -f thread_id query hint, got:\n%s", out)
	}
}

// TestOutputThreadSiblings_NotThreadAgent verifies the block is a no-op for a
// non-thread agent (no SLACK_THREAD_* env) — it must never emit and never even
// call the daemon.
func TestOutputThreadSiblings_NotThreadAgent(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	// A genuine non-thread agent has no coords in env AND no agent bead to fall
	// back to. Clearing the bead-id env vars keeps resolveThreadCoords short-
	// circuiting before any daemon call — so this still asserts the no-query
	// invariant for a truly non-thread agent under the prewarmed fallback.
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"beads":[],"total":0}`))
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputThreadSiblings(&buf)

	if buf.Len() != 0 {
		t.Errorf("expected no output for a non-thread agent, got:\n%s", buf.String())
	}
	if called {
		t.Error("outputThreadSiblings must not query the daemon for a non-thread agent")
	}
}

// TestOutputThreadSiblings_NoSiblings verifies that a thread whose only member
// is the resuming agent itself renders nothing (no empty header).
func TestOutputThreadSiblings_NoSiblings(t *testing.T) {
	t.Setenv("SLACK_THREAD_CHANNEL", "C-solo")
	t.Setenv("SLACK_THREAD_TS", "1.1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-only-agent")

	const head = "gd-solohead"
	siblings := []map[string]any{
		sibBead("gd-only-agent", "the resuming agent itself", "agent", "in_progress"),
	}
	srv := threadSiblingsMux(t, head, siblings)
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputThreadSiblings(&buf)

	if buf.Len() != 0 {
		t.Errorf("expected no output when the only member is the agent itself, got:\n%s", buf.String())
	}
}

// threadSiblingsMuxWithBead extends threadSiblingsMux to also serve the agent
// bead GET (/v1/beads/{agentID}) that resolveThreadCoords issues when the
// SLACK_THREAD_* env vars are empty — i.e. the prewarmed (pool-assigned) pod
// path. The agent bead carries the coords the controller stamped at assign time.
func threadSiblingsMuxWithBead(t *testing.T, agentID, channel, ts, headID string, siblings []map[string]any) *httptest.Server {
	t.Helper()
	head := map[string]any{"id": headID, "title": "Slack thread head", "type": "task", "kind": "issue", "status": "open"}
	agentBead := map[string]any{
		"id": agentID, "title": agentID, "type": "agent", "status": "in_progress",
		"fields": map[string]string{"slack_thread_channel": channel, "slack_thread_ts": ts},
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/v1/beads/"+agentID:
			_ = json.NewEncoder(w).Encode(agentBead)
		case r.URL.Path == "/v1/beads" && strings.HasPrefix(q.Get("labels"), "slack-thread:"):
			// Assert the label was built from the bead-recovered coords.
			if want := "slack-thread:" + channel + ":" + ts; q.Get("labels") != want {
				t.Errorf("head-lookup label = %q, want %q", q.Get("labels"), want)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []map[string]any{head}, "total": 1})
		case r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": siblings, "total": len(siblings)})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestOutputThreadSiblings_PrewarmedBeadFallback is the core regression for this
// task: a PREWARMED pod boots with SLACK_THREAD_* baked empty and must still
// recover its thread siblings via the agent-bead coords fallback. Before the
// fix, the env-only isThreadSpawnedAgent() gate no-op'd this path entirely.
func TestOutputThreadSiblings_PrewarmedBeadFallback(t *testing.T) {
	// Prewarmed pod: env coords empty, only the agent bead carries them.
	t.Setenv("SLACK_THREAD_CHANNEL", "")
	t.Setenv("SLACK_THREAD_TS", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-prewarm-agent")

	const head = "gd-prewarmhead"
	siblings := []map[string]any{
		sibBead("gd-prior-answer", "Answer filed by an earlier session", "task", "closed"),
		sibBead("gd-prewarm-agent", "the resuming agent itself", "agent", "in_progress"), // dropped
	}
	srv := threadSiblingsMuxWithBead(t, "gd-prewarm-agent", "C-PREWARM", "999.888", head, siblings)
	defer srv.Close()
	swapDaemon(t, srv.URL)

	var buf bytes.Buffer
	outputThreadSiblings(&buf)
	out := buf.String()

	if !strings.Contains(out, "Prior beads in THIS Slack thread") {
		t.Fatalf("expected sibling header on prewarmed fallback, got:\n%s", out)
	}
	if !strings.Contains(out, "gd-prior-answer") {
		t.Errorf("expected prior sibling recovered via bead fallback, got:\n%s", out)
	}
	if strings.Contains(out, "gd-prewarm-agent") {
		t.Errorf("agent's own bead must be excluded, got:\n%s", out)
	}
}
