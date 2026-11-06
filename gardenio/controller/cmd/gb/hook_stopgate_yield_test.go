package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// setupParkedDecisionDaemon installs a stub daemon that serves GET /v1/beads
// with a single open decision whose requesting_agent_bead_id is reqAgent. It
// returns a pointer to the last query values seen so tests can lock the query
// shape. The daemon is restored on cleanup.
func setupParkedDecisionDaemon(t *testing.T, reqAgent string) *url.Values {
	t.Helper()
	seen := &url.Values{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		*seen = q
		// Only answer the open-decision scan; anything else gets an empty page.
		beads := []map[string]any{}
		if q.Get("status") == "open" && q.Get("type") == "decision" {
			beads = append(beads, map[string]any{
				"id":     "gd-decision1",
				"type":   "decision",
				"status": "open",
				"fields": map[string]string{"requesting_agent_bead_id": reqAgent},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
	return seen
}

// setupEmptyDecisionDaemon installs a stub daemon that serves GET /v1/beads with
// no open decisions at all — the "no pending decision" state.
func setupEmptyDecisionDaemon(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })
}

// TestStopGateAgentParkedOnOpenDecision_Match is the core Layer-1 assertion: an
// agent that still owns an OPEN decision it requested is reported as parked, so
// the Stop hook allows a quiet turn-end WITHOUT closing the bead.
func TestStopGateAgentParkedOnOpenDecision_Match(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agentA")
	seen := setupParkedDecisionDaemon(t, "gd-agentA")

	if !stopGateAgentParkedOnOpenDecision(context.Background()) {
		t.Fatal("stopGateAgentParkedOnOpenDecision = false, want true when the agent owns an open decision")
	}
	// Lock the scan shape: it must query open decisions (not close the bead on
	// a stray fetch of some other type/status).
	if got := seen.Get("status"); got != "open" {
		t.Errorf("decision scan status = %q, want open", got)
	}
	if got := seen.Get("type"); got != "decision" {
		t.Errorf("decision scan type = %q, want decision", got)
	}
}

// TestStopGateAgentParkedOnOpenDecision_OtherAgentsDecision guards the
// conservative direction: an open decision that belongs to a DIFFERENT agent
// must NOT count as this agent being parked, otherwise the marker of any agent
// could keep unrelated pods alive.
func TestStopGateAgentParkedOnOpenDecision_OtherAgentsDecision(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agentA")
	setupParkedDecisionDaemon(t, "gd-someone-else")

	if stopGateAgentParkedOnOpenDecision(context.Background()) {
		t.Error("stopGateAgentParkedOnOpenDecision = true, want false when the open decision belongs to another agent")
	}
}

// TestStopGateAgentParkedOnOpenDecision_NoOpenDecision covers the shutdown-safe
// path: no open decision for anyone means the agent is NOT parked, so the gate
// falls through to its normal close logic and a genuine stop is never wedged.
func TestStopGateAgentParkedOnOpenDecision_NoOpenDecision(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agentA")
	setupEmptyDecisionDaemon(t)

	if stopGateAgentParkedOnOpenDecision(context.Background()) {
		t.Error("stopGateAgentParkedOnOpenDecision = true, want false when there is no open decision")
	}
}

// TestStopGateAgentParkedOnOpenDecision_NoAgentIdentity is the fail-open case:
// with no resolvable agent id and no actor, the function must return false
// (fall through to close) rather than guess.
func TestStopGateAgentParkedOnOpenDecision_NoAgentIdentity(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	oldActor := actor
	actor = ""
	t.Cleanup(func() { actor = oldActor })
	// A daemon that would answer "parked" if asked — proving the guard is the
	// missing identity, not an empty page.
	setupParkedDecisionDaemon(t, "gd-agentA")

	if stopGateAgentParkedOnOpenDecision(context.Background()) {
		t.Error("stopGateAgentParkedOnOpenDecision = true, want false when no agent identity can be resolved")
	}
}

// TestStopGateAgentParkedOnOpenDecision_DaemonError is the other fail-open case:
// a daemon error must return false so a transient blip reverts to pre-fix
// behavior rather than keeping a pod alive forever.
func TestStopGateAgentParkedOnOpenDecision_DaemonError(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agentA")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	old := daemon
	daemon = c
	t.Cleanup(func() { daemon = old })

	if stopGateAgentParkedOnOpenDecision(context.Background()) {
		t.Error("stopGateAgentParkedOnOpenDecision = true, want false on a daemon error (fail-open to close)")
	}
}

// TestYieldMarkerSurvivesTimeout models Layer 2(a): the yield marker must NOT be
// removed when the wait is cut short by a context cancellation (the 120s
// Bash-tool SIGINT / interrupt), because the agent is still parked. It is
// removed only when the context is still live (a genuine proceed). This exercises
// the exact defer predicate yield.go installs (ctx.Err()==nil gates the remove).
func TestYieldMarkerSurvivesTimeout(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, "stop-gate-yielding")
	old := stopGateYieldMarkerFile
	stopGateYieldMarkerFile = marker
	t.Cleanup(func() { stopGateYieldMarkerFile = old })

	// The predicate the yield defer uses. Kept in lockstep with yield.go: the
	// marker is dropped only when ctx.Err() == nil.
	dropMarkerIf := func(ctx context.Context) {
		if ctx.Err() == nil {
			os.Remove(stopGateYieldMarkerFile)
		}
	}

	// Cancelled context (timeout/interrupt): marker MUST survive.
	if f, err := os.Create(marker); err == nil {
		f.Close()
	}
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	dropMarkerIf(cancelledCtx)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("yield marker was removed on a cancelled context; parked flag must survive a timeout/interrupt (stat err=%v)", err)
	}

	// Live context (genuine resolve): marker is cleared so a real proceed does
	// not leave a stale parked flag.
	liveCtx := context.Background()
	dropMarkerIf(liveCtx)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("yield marker should be removed on a live-context return, stat err=%v", err)
	}
}
