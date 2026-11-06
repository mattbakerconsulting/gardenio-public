package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestCloseAgentBeadOnExit_SkipsFrozenAwaitingThaw verifies the stop-gate
// close path honors the frozen-awaiting-thaw signal (ka-0000002104): when a
// Stop-hook cooldown/escape-hatch firing lands after `gb stop` suspended the
// agent (bead OPEN at agent_state=stopping, controller checkpointing the pod),
// closeAgentBeadOnExit must leave the bead untouched — closing it would
// destroy the thaw-target signal exactly like the init_agent_loop teardown
// clobber did.
func TestCloseAgentBeadOnExit_SkipsFrozenAwaitingThaw(t *testing.T) {
	const beadID = "gd-frozen-hook"
	var mutations atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{"agent_state": "stopping"},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		default:
			mutations.Add(1)
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)

	closeAgentBeadOnExit("cooldown")

	if got := mutations.Load(); got != 0 {
		t.Fatalf("expected no bead mutations for a frozen-awaiting-thaw agent on the stop-gate path, got %d", got)
	}
}

// TestCloseAgentBeadOnExit_ClosesNonFrozenBead pins the control case: the
// stop-gate close still fires for an ordinary open bead — the frozen guard
// keys strictly on agent_state=stopping.
func TestCloseAgentBeadOnExit_ClosesNonFrozenBead(t *testing.T) {
	const beadID = "gd-open-hook"
	var closePosts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{"agent_state": "working"},
			})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads/"+beadID+"/close":
			closePosts.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": beadID})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })

	t.Setenv("KD_AGENT_ID", beadID)

	closeAgentBeadOnExit("cooldown")

	if got := closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST for a non-frozen open bead (control), got %d", got)
	}
}
