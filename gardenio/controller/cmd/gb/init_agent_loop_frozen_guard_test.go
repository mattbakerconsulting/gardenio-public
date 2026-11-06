package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestCloseAgentBeadBestEffort_SkipsFrozenAwaitingThaw verifies the gb-side
// half of the resume-latency emitter fix (ka-0000002104, twin of the
// controller-side ka-0000007439): when the restart loop's SIGTERM teardown
// fires while the agent bead is OPEN at agent_state=stopping — i.e. the
// controller froze this agent and this pod's death IS the freeze's Pod A
// teardown — the teardown must NOT close the bead (which would destroy the
// frozen-awaiting-thaw signal the wake paths key on) and must NOT post an
// "interrupted" breadcrumb (the agent is suspended by design, not killed).
func TestCloseAgentBeadBestEffort_SkipsFrozenAwaitingThaw(t *testing.T) {
	const beadID = "gd-frozen-stopping"
	var mutations atomic.Int64 // any POST/PATCH — close, breadcrumb marker, comment
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
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := mutations.Load(); got != 0 {
		t.Fatalf("expected no bead mutations for a frozen-awaiting-thaw agent (bead must stay open at agent_state=stopping), got %d", got)
	}
}

// TestCloseAgentBeadBestEffort_ClosesOpenNonFrozenBead pins the control case:
// an OPEN bead in a normal live state (working) killed mid-turn IS still closed
// by the teardown path — the frozen guard keys strictly on agent_state=stopping
// and must not widen into skipping genuine involuntary deaths.
func TestCloseAgentBeadBestEffort_ClosesOpenNonFrozenBead(t *testing.T) {
	const beadID = "gd-open-working"
	var closePosts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+beadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     beadID,
				"type":   "agent",
				"status": "open",
				// Breadcrumb marker already set so the breadcrumb sub-path is
				// inert; this test pins only the close decision.
				"fields": map[string]string{
					"agent_state":                    "working",
					interruptedBreadcrumbPostedField: "true",
				},
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
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	closeAgentBeadBestEffort("context_cancelled")

	if got := closePosts.Load(); got != 1 {
		t.Fatalf("expected exactly 1 close POST for a non-frozen open bead (control), got %d", got)
	}
}
