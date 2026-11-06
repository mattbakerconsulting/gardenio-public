package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// countingBeadDaemon is an httptest server that records how many times each
// distinct bead id was fetched via GET /v1/beads/<id>, and serves canned
// per-id bead JSON. Advice list queries return an empty set. It lets the tests
// below assert on the exact number of single-bead round-trips a prime render
// performs.
func countingBeadDaemon(t *testing.T, beads map[string]mockBead) (getCounts func(id string) int, cleanup func()) {
	t.Helper()

	var mu sync.Mutex
	counts := map[string]int{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// GetBead: GET /v1/beads/<id> (no query string).
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.URL.RawQuery == "" {
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			mu.Lock()
			counts[id]++
			mu.Unlock()
			if b, ok := beads[id]; ok {
				_ = json.NewEncoder(w).Encode(b)
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
			return
		}

		// Everything else (list queries, comments, deps) returns an empty list.
		resp := mockListResponse{Beads: nil, Total: 0}
		_ = json.NewEncoder(w).Encode(resp)
	}))

	oldDaemon := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}
	daemon = c

	getCounts = func(id string) int {
		mu.Lock()
		defer mu.Unlock()
		return counts[id]
	}
	cleanup = func() {
		daemon = oldDaemon
		srv.Close()
	}
	return getCounts, cleanup
}

// TestBuildPrimeBody_FetchesAgentBeadOnce is the regression lock for
// ka-0000002397: a single prime render must not re-fetch the same agent bead
// once per section. Before the request-scoped GetBead memo (armed via
// beginPrimeBeadCache) the agent bead was fetched ~8 times per `gb prime` —
// once each by lookupOperatorUserBead, isAutoAssignEnabled, isMemoryEligible,
// outputScheduledLoopContext, outputProgressBlock, outputPriorWorkOnBead,
// outputPriorWrapupOnResume AND inside advice-subscription enrichment — each
// returning the identical ~3.5 KB document. The memo collapses those to one GET
// per distinct bead. Fails (count > 1) if any section reverts to an un-memoized
// fetch (e.g. a client that no longer routes GetBead through the cache).
func TestBuildPrimeBody_FetchesAgentBeadOnce(t *testing.T) {
	const agentID = "gardenio/crews/cache-probe"
	const userBeadID = "gd-user01"

	getCounts, cleanup := countingBeadDaemon(t, map[string]mockBead{
		agentID: {
			ID:     agentID,
			Title:  "cache probe agent",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{
				"user_bead_id": userBeadID,
				"task_id":      "gd-task01",
				"schedule_id":  "gd-sched01",
			},
		},
		userBeadID: {
			ID:     userBeadID,
			Title:  "Operator",
			Type:   "user",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{"display_name": "Operator"},
		},
	})
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_AGENT_ID", agentID)
	t.Setenv("BOAT_TASK_ID", "") // let auto-assign path run isAutoAssignEnabled

	_, _, _ = buildPrimeBody(agentID, "crew")

	if got := getCounts(agentID); got != 1 {
		t.Errorf("agent bead %s fetched %d times in one prime render; want exactly 1 (per-render memo regressed)", agentID, got)
	}
	if got := getCounts(userBeadID); got != 1 {
		t.Errorf("operator user bead %s fetched %d times in one prime render; want exactly 1", userBeadID, got)
	}
}

// TestBuildSystemPromptBody_FetchesAgentBeadOnce locks the same invariant for
// the cacheable --system-prompt subset, which independently reads the agent bead
// from outputOperatorIdentity, outputScheduledLoopContext, outputPriorWorkOnBead,
// outputPriorWrapupOnResume and advice enrichment. When advice matches (as it
// does against a real catalog), advice enrichment adds a GetBead for a
// bead-ID-shaped KD_AGENT_ID; the memo must absorb it too.
func TestBuildSystemPromptBody_FetchesAgentBeadOnce(t *testing.T) {
	const agentID = "gd-agentbead01" // bead-ID-shaped: advice enrichment GetBeads it

	getCounts, cleanup := countingBeadDaemon(t, map[string]mockBead{
		agentID: {
			ID:     agentID,
			Title:  "sysprompt probe agent",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: map[string]string{"task_id": "gd-task02"},
		},
	})
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_AGENT_ID", agentID)

	var buf bytes.Buffer
	buildSystemPromptBody(&buf, agentID, "crew")

	if got := getCounts(agentID); got != 1 {
		t.Errorf("agent bead %s fetched %d times in one system-prompt render; want exactly 1", agentID, got)
	}
}

// TestGetBeadCache_ErrorsNotCached verifies a failed fetch is never cached: a
// transient daemon error on one section must not poison the rest of the render.
func TestGetBeadCache_ErrorsNotCached(t *testing.T) {
	var mu sync.Mutex
	attempts := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		fail := attempts == 1
		mu.Unlock()
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mockBead{ID: "gd-x", Title: "x", Type: "task", Kind: "issue", Status: "open"})
	}))
	defer srv.Close()

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}

	c.BeginGetBeadCache()
	defer c.EndGetBeadCache()

	if _, err := c.GetBead(t.Context(), "gd-x"); err == nil {
		t.Fatal("expected first fetch to error")
	}
	// Second call must retry the daemon (error was not cached) and succeed.
	b, err := c.GetBead(t.Context(), "gd-x")
	if err != nil || b == nil {
		t.Fatalf("expected second fetch to succeed after retry, got bead=%v err=%v", b, err)
	}
}

// TestGetBeadCache_DisarmedFetchesEveryTime verifies that with no armed cache
// (the default for every long-lived client — the controller, bridges),
// GetBead issues a live round-trip on every call.
func TestGetBeadCache_DisarmedFetchesEveryTime(t *testing.T) {
	const id = "gd-nocache"
	getCounts, cleanup := countingBeadDaemon(t, map[string]mockBead{
		id: {ID: id, Title: "t", Type: "task", Kind: "issue", Status: "open"},
	})
	defer cleanup()

	for i := 0; i < 3; i++ {
		if _, err := daemon.GetBead(t.Context(), id); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if got := getCounts(id); got != 3 {
		t.Errorf("with disarmed cache, got %d fetches; want 3 (no memoization)", got)
	}
}

// TestGetBeadCache_EndDisarms verifies EndGetBeadCache returns the client to
// live-fetch behavior: reads after End are not served from the memo.
func TestGetBeadCache_EndDisarms(t *testing.T) {
	const id = "gd-endcache"
	getCounts, cleanup := countingBeadDaemon(t, map[string]mockBead{
		id: {ID: id, Title: "t", Type: "task", Kind: "issue", Status: "open"},
	})
	defer cleanup()

	daemon.BeginGetBeadCache()
	_, _ = daemon.GetBead(t.Context(), id) // 1 live fetch, now cached
	_, _ = daemon.GetBead(t.Context(), id) // served from memo
	if got := getCounts(id); got != 1 {
		t.Fatalf("during armed cache got %d fetches; want 1", got)
	}
	daemon.EndGetBeadCache()
	_, _ = daemon.GetBead(t.Context(), id) // live again
	if got := getCounts(id); got != 2 {
		t.Errorf("after End got %d total fetches; want 2 (cache disarmed)", got)
	}
}
