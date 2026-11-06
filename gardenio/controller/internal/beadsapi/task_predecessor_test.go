package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// predecessorTestServer stands up a fake daemon for the A1 stamping path
// (ka-0000007863). It answers the spawn dedup GET with a 404, returns the given
// closed-agent rows for the task_id predecessor query, and records the fields
// payload of the create POST so a test can assert what was stamped.
type predecessorTestServer struct {
	// predecessors are returned from the closed-agent-by-task_id list query.
	predecessors []map[string]any
	// createdFields captures fields on the agent-bead create.
	createdFields map[string]string
	// listQueries records the query string of every list call, so a test can
	// prove the lookup was (or was not) issued.
	listQueries []string
}

func (s *predecessorTestServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// Spawn dedup probe: no incumbent by that name.
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/") {
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
			return
		}
		// Predecessor lookup (and any other filtered list).
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			s.listQueries = append(s.listQueries, r.URL.RawQuery)
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": s.predecessors})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads" {
			var parsed map[string]json.RawMessage
			_ = json.Unmarshal(body, &parsed)
			var fields map[string]string
			_ = json.Unmarshal(parsed["fields"], &fields)
			s.createdFields = fields
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-new-agent"})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

func closedAgent(id string, created time.Time, extra map[string]string) map[string]any {
	fields := map[string]string{"agent": "prior-" + id, "task_id": "gd-task"}
	for k, v := range extra {
		fields[k] = v
	}
	return map[string]any{
		"id":         id,
		"type":       "agent",
		"status":     "closed",
		"fields":     fields,
		"created_at": created.Format(time.RFC3339Nano),
	}
}

// TestSpawnAgent_StampsTaskPredecessor is the core A1 property: a spawn onto a
// task a closed agent already worked carries a pointer back to that agent.
// Before this, all 770 such spawns per week carried none.
func TestSpawnAgent_StampsTaskPredecessor(t *testing.T) {
	now := time.Now()
	s := &predecessorTestServer{predecessors: []map[string]any{
		closedAgent("gd-older", now.Add(-2*time.Hour), nil),
		closedAgent("gd-newest", now.Add(-1*time.Hour), nil),
	}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "gd-task", "crew", ""); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	// The MOST RECENT closed occupant is the predecessor, regardless of the
	// order the daemon returned rows in.
	if got := s.createdFields["prior_agent_id"]; got != "gd-newest" {
		t.Errorf("prior_agent_id = %q, want gd-newest", got)
	}
	// No root on the predecessor, so the predecessor anchors the chain.
	if got := s.createdFields["lineage_root_id"]; got != "gd-newest" {
		t.Errorf("lineage_root_id = %q, want gd-newest", got)
	}

	// The lookup must ask for CLOSED agents on this task — a live incumbent is
	// not a predecessor (the single-claim guard handles that case).
	var found bool
	for _, q := range s.listQueries {
		v, _ := url.ParseQuery(q)
		if v.Get("status") == "closed" && strings.Contains(q, "gd-task") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a closed-status predecessor query, got queries: %v", s.listQueries)
	}
}

// TestSpawnAgent_InheritsLineageRoot: a chain that already has a root keeps it,
// so a third occupant points at the FIRST, not at its immediate predecessor.
// This is what makes lineage_root_id a groupable key rather than a linked list.
func TestSpawnAgent_InheritsLineageRoot(t *testing.T) {
	s := &predecessorTestServer{predecessors: []map[string]any{
		closedAgent("gd-second", time.Now(), map[string]string{"lineage_root_id": "gd-first"}),
	}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "gd-task", "crew", ""); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := s.createdFields["prior_agent_id"]; got != "gd-second" {
		t.Errorf("prior_agent_id = %q, want gd-second", got)
	}
	if got := s.createdFields["lineage_root_id"]; got != "gd-first" {
		t.Errorf("lineage_root_id = %q, want inherited gd-first", got)
	}
}

// TestSpawnAgent_ExplicitPriorAgentIDWins: the successor mint, gardener
// iteration and thread cold-remint each know the exact bead they continue.
// That is a stronger claim than "worked the same task at some point", so a
// caller-supplied pointer must never be overwritten.
func TestSpawnAgent_ExplicitPriorAgentIDWins(t *testing.T) {
	s := &predecessorTestServer{predecessors: []map[string]any{
		closedAgent("gd-task-occupant", time.Now(), nil),
	}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "gd-task", "crew", "",
		map[string]string{"prior_agent_id": "gd-explicit", "lineage_root_id": "gd-explicit-root"})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := s.createdFields["prior_agent_id"]; got != "gd-explicit" {
		t.Errorf("prior_agent_id = %q, want the caller's gd-explicit", got)
	}
	if got := s.createdFields["lineage_root_id"]; got != "gd-explicit-root" {
		t.Errorf("lineage_root_id = %q, want the caller's gd-explicit-root", got)
	}
}

// TestSpawnAgent_NoTaskSkipsLookup: a task-less spawn must not pay for a
// predecessor query it can never satisfy.
func TestSpawnAgent_NoTaskSkipsLookup(t *testing.T) {
	s := &predecessorTestServer{}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "crew", ""); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if got := s.createdFields["prior_agent_id"]; got != "" {
		t.Errorf("prior_agent_id = %q, want empty for a task-less spawn", got)
	}
	if len(s.listQueries) != 0 {
		t.Errorf("expected no predecessor lookup without a task, got %v", s.listQueries)
	}
}

// TestSpawnAgent_FirstOccupantStampsNothing: the common case. A task nobody has
// worked yields no pointer rather than a self-reference or an empty string
// field.
func TestSpawnAgent_FirstOccupantStampsNothing(t *testing.T) {
	s := &predecessorTestServer{predecessors: []map[string]any{}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "gd-task", "crew", ""); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if _, ok := s.createdFields["prior_agent_id"]; ok {
		t.Errorf("first occupant must carry no prior_agent_id, got %q", s.createdFields["prior_agent_id"])
	}
	if _, ok := s.createdFields["lineage_root_id"]; ok {
		t.Errorf("first occupant must carry no lineage_root_id, got %q", s.createdFields["lineage_root_id"])
	}
}

// TestSpawnAgent_StampsSeatKey pins Shape C (ka-0000007094): the position an
// agent occupies is recorded at spawn, so a recurring name becomes derivable
// instead of a coincidence visible only by eyeballing the roster.
func TestSpawnAgent_StampsSeatKey(t *testing.T) {
	s := &predecessorTestServer{}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.SpawnAgent(context.Background(), "gardenio-gardener-bugfix-dispatch-l369", "gardenio", "", "gardener", ""); err != nil {
		t.Fatalf("spawn: %v", err)
	}
	want := "gardenio/gardener/gardenio-gardener-bugfix-dispatch"
	if got := s.createdFields["seat_key"]; got != want {
		t.Errorf("seat_key = %q, want %q", got, want)
	}
}

// TestSpawnAgent_PredecessorLookupFailureIsNonFatal: the stamp is a nicety, the
// spawn is the work. A daemon hiccup on the lookup must never wedge a
// legitimate dispatch.
func TestSpawnAgent_PredecessorLookupFailureIsNonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/") {
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/beads" {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-new-agent"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "gd-task", "crew", "")
	if err != nil {
		t.Fatalf("a predecessor-lookup failure must not fail the spawn, got: %v", err)
	}
	if id != "gd-new-agent" {
		t.Errorf("agent id = %q, want gd-new-agent", id)
	}
}
