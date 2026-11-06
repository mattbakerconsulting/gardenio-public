package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// staleRosterWireBead is the minimal daemon wire shape the fake returns for the
// per-assignee agent-bead lookup resolveStaleAssigneeAgents issues.
type staleRosterWireBead struct {
	ID        string            `json:"id"`
	Type      string            `json:"type"`
	Status    string            `json:"status"`
	Fields    map[string]string `json:"fields"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// newsStaleRosterServer wires a fake daemon whose /v1/beads returns, for an
// agent-bead query filtered by `field=agent=<name>`, the row registered under
// that name. It records every distinct `field` param it saw so a test can assert
// which assignees actually triggered a targeted lookup (and that resolved ones
// did NOT). Any other query returns an empty result.
func newsStaleRosterServer(t *testing.T, byAgent map[string]staleRosterWireBead, seen *[]string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/beads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var rows []staleRosterWireBead
		if q.Get("type") == "agent" && q.Get("status") == "open" {
			for _, f := range q["field"] {
				if seen != nil {
					*seen = append(*seen, f)
				}
				// f is "agent=<name>"; match against the registered rows.
				const prefix = "agent="
				if len(f) > len(prefix) && f[:len(prefix)] == prefix {
					if b, ok := byAgent[f[len(prefix):]]; ok {
						rows = append(rows, b)
					}
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": rows, "total": len(rows)})
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

// TestResolveStaleAssigneeAgents_RecoversTruncatedLiveAgent is the regression for
// ka-0000007979: a live agent whose bead fell outside the capped roster fetch is
// absent from the roster, so its in-progress bead is misclassified stale. The
// targeted per-assignee lookup must re-resolve it as alive so the caller can
// re-split it out of the stale bucket.
func TestResolveStaleAssigneeAgents_RecoversTruncatedLiveAgent(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-30 * time.Second) // within the freshness window

	var seen []string
	newsStaleRosterServer(t, map[string]staleRosterWireBead{
		"thread-live": {
			ID:     "gd-agentlive",
			Type:   "agent",
			Status: "open",
			// working + fresh + recent progress == alive under newsAliveState.
			Fields:    map[string]string{"agent": "thread-live", "agent_state": "working", "last_progress_at": fresh.Format(time.RFC3339)},
			UpdatedAt: fresh,
		},
	}, &seen)

	// Stale bucket: one bead whose assignee is the truncated-but-live agent.
	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-victim", Assignee: "thread-live", Status: "in_progress"},
	}
	// Empty roster simulates the agent having been dropped past the fetch cap.
	extra := resolveStaleAssigneeAgents(context.Background(), staleIP, nil, now, newsAliveFreshnessWindow)

	if len(extra) != 1 {
		t.Fatalf("expected 1 recovered roster row, got %d (%+v)", len(extra), extra)
	}
	if extra[0].Name != "thread-live" {
		t.Fatalf("recovered row name = %q, want thread-live", extra[0].Name)
	}
	if !extra[0].Alive {
		t.Fatalf("recovered agent must be Alive (it is working+fresh)")
	}
	// The recovered row must fold into the active set so a re-split rescues the bead.
	active, _ := rosterDerived(extra)
	if !active["thread-live"] {
		t.Fatalf("recovered agent must be in the active set")
	}
	_, staleAfter, _ := splitActiveStale(staleIP, active)
	if len(staleAfter) != 0 {
		t.Fatalf("victim bead must leave the stale bucket after backfill, got %d stale", len(staleAfter))
	}
}

// TestResolveStaleAssigneeAgents_SkipsResolvedAssignee proves the backfill does
// NO daemon round-trip for a stale assignee already present in the roster (any
// liveness) — that assignee was already classified from ground truth, so a
// genuinely-dead agent is not needlessly re-fetched.
func TestResolveStaleAssigneeAgents_SkipsResolvedAssignee(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	var seen []string
	newsStaleRosterServer(t, map[string]staleRosterWireBead{}, &seen)

	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-dead", Assignee: "gone-agent", Status: "in_progress"},
	}
	// Roster already carries the assignee (resolved, but not alive).
	roster := []newsAgentInfo{{Name: "gone-agent", Alive: false}}

	extra := resolveStaleAssigneeAgents(context.Background(), staleIP, roster, now, newsAliveFreshnessWindow)
	if len(extra) != 0 {
		t.Fatalf("resolved assignee must not be backfilled, got %+v", extra)
	}
	if len(seen) != 0 {
		t.Fatalf("no per-assignee lookup should fire for a resolved assignee, saw %v", seen)
	}
}

// TestResolveStaleAssigneeAgents_DeadAgentStaysStale proves a genuinely-gone
// assignee (its bead absent from the daemon) yields no roster row, so the stale
// classification is preserved — the backfill only rescues the false positives.
func TestResolveStaleAssigneeAgents_DeadAgentStaysStale(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	var seen []string
	newsStaleRosterServer(t, map[string]staleRosterWireBead{}, &seen) // no rows for anyone

	staleIP := []*beadsapi.BeadDetail{
		{ID: "gd-victim", Assignee: "truly-gone", Status: "in_progress"},
	}
	extra := resolveStaleAssigneeAgents(context.Background(), staleIP, nil, now, newsAliveFreshnessWindow)
	if len(extra) != 0 {
		t.Fatalf("a gone agent must not be recovered, got %+v", extra)
	}
	if len(seen) != 1 || seen[0] != "agent=truly-gone" {
		t.Fatalf("expected exactly one targeted lookup for agent=truly-gone, saw %v", seen)
	}
}

// TestResolveStaleAssigneeAgents_NilDaemonSafe proves the helper degrades to a
// no-op rather than panicking when the daemon client is unset.
func TestResolveStaleAssigneeAgents_NilDaemonSafe(t *testing.T) {
	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })

	staleIP := []*beadsapi.BeadDetail{{ID: "gd-x", Assignee: "someone"}}
	if extra := resolveStaleAssigneeAgents(context.Background(), staleIP, nil, time.Now(), newsAliveFreshnessWindow); extra != nil {
		t.Fatalf("nil daemon must yield nil, got %+v", extra)
	}
}
