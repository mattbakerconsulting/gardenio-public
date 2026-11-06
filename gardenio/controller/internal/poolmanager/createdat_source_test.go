package poolmanager

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"

	"gardenio/controller/internal/beadsapi"
)

// prodMockDaemon mirrors a real daemon list response: the bead-creation time is
// delivered as the TOP-LEVEL `created_at` JSON field (which beadsapi maps onto
// AgentBead.CreatedAt), and createPrewarmedAgent never writes a `created_at`
// FIELD — so `fields` carries none. This is the shape production actually
// serves; the older test fixtures hand-stamp created_at into `fields`, which
// masked ka-0000007925 (listPrewarmedAgents read the always-absent field and
// left every member at a zero CreatedAt).
func prodMockDaemon(t *testing.T, agents []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			beads := make([]map[string]any, len(agents))
			for i, a := range agents {
				beads[i] = map[string]any{
					"id":         a["id"],
					"title":      a["title"],
					"type":       "agent",
					"status":     "open",
					"created_at": a["created_at"], // top-level -> AgentBead.CreatedAt
					"fields":     a["fields"],     // NO created_at inside
				}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
		case r.Method == http.MethodGet:
			for _, a := range agents {
				if "/v1/beads/"+a["id"].(string) == r.URL.Path {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": a["id"], "title": a["title"], "type": "agent",
						"status": "open", "created_at": a["created_at"], "fields": a["fields"],
					})
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
		case r.Method == http.MethodPatch:
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestListPrewarmedAgents_CreatedAtFromDaemonTimestamp is the regression guard
// for ka-0000007925. It feeds beads whose age lives ONLY in the daemon's
// top-level created_at (as in production), with none in the fields map, and
// asserts the resolved prewarmedAgent.CreatedAt is non-zero and matches. Before
// the fix listPrewarmedAgents read parseTime(Metadata["created_at"]) alone, so
// this was the zero time and the assertion fails.
func TestListPrewarmedAgents_CreatedAtFromDaemonTimestamp(t *testing.T) {
	old := time.Now().Add(-20 * time.Minute).UTC().Truncate(time.Second)
	newer := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	agents := []map[string]any{
		{
			"id": "gd-old", "title": "prewarmed-old",
			"created_at": old.Format(time.RFC3339),
			"fields": map[string]string{
				"agent": "prewarmed-old", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
		{
			"id": "gd-new", "title": "prewarmed-new",
			"created_at": newer.Format(time.RFC3339),
			"fields": map[string]string{
				"agent": "prewarmed-new", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	srv := prodMockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	got, err := pm.listPrewarmedAgents(context.Background(), false)
	if err != nil {
		t.Fatalf("listPrewarmedAgents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 members, got %d", len(got))
	}
	byID := map[string]prewarmedAgent{}
	for _, a := range got {
		byID[a.ID] = a
	}
	for id, want := range map[string]time.Time{"gd-old": old, "gd-new": newer} {
		a, ok := byID[id]
		if !ok {
			t.Fatalf("missing member %s", id)
		}
		if a.CreatedAt.IsZero() {
			t.Errorf("%s: CreatedAt is zero — age resolved from never-written fields[created_at] instead of the daemon timestamp (ka-0000007925)", id)
		}
		if !a.CreatedAt.Equal(want) {
			t.Errorf("%s: CreatedAt = %s, want %s", id, a.CreatedAt.UTC(), want)
		}
	}
}

// TestAssignPrewarmed_PicksOldest_ProdTimestampShape proves the downstream
// FIFO-oldest pick works when age comes only from the daemon timestamp (the
// production shape). Pre-fix every candidate had a zero CreatedAt, so the pick
// loop's `if a.CreatedAt.IsZero() { continue }` never advanced past
// candidates[0] and the pick degraded to daemon list order — here gd-new (listed
// first) would be returned instead of the truly-oldest gd-old.
func TestAssignPrewarmed_PicksOldest_ProdTimestampShape(t *testing.T) {
	old := time.Now().Add(-20 * time.Minute).UTC().Truncate(time.Second)
	newer := time.Now().Add(-5 * time.Minute).UTC().Truncate(time.Second)
	// gd-new is listed FIRST so a broken (list-order) pick returns it, not gd-old.
	agents := []map[string]any{
		{
			"id": "gd-new", "title": "prewarmed-new",
			"created_at": newer.Format(time.RFC3339),
			"fields": map[string]string{
				"agent": "prewarmed-new", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
		{
			"id": "gd-old", "title": "prewarmed-old",
			"created_at": old.Format(time.RFC3339),
			"fields": map[string]string{
				"agent": "prewarmed-old", "agent_state": "prewarmed",
				"mode": "crew", "role": "thread", "project": "gardenio",
			},
		},
	}
	srv := prodMockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Channel:  "C-test",
		ThreadTS: "1234.5678",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed: %v", err)
	}
	if result.BeadID != "gd-old" {
		t.Errorf("expected FIFO-oldest gd-old, got %s (pick degraded to list order — CreatedAt not sourced from daemon timestamp, ka-0000007925)", result.BeadID)
	}
}
