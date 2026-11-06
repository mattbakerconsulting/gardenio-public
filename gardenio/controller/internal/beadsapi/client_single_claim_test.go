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
)

// singleClaimHandler serves the SpawnAgent request chain for single-claim
// tests. The name-dedup GET (/v1/agents-by-name/) always 404s (no existing
// agent by name). The single-claim GET (/v1/beads?...field=task_id=...)
// returns the supplied agent rows. A create POST returns createID. Every
// request is recorded.
func singleClaimHandler(t *testing.T, createID string, taskAgents []map[string]any, requests *[]spawnReq) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(body, &parsed)
		*requests = append(*requests, spawnReq{r.Method, r.URL.Path, parsed, r.URL.RawQuery})

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"beads": taskAgents,
				"total": len(taskAgents),
			})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": createID})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

// agentRow builds a minimal type=agent bead JSON row for the single-claim GET.
func agentRow(id, agentState string) map[string]any {
	return map[string]any{
		"id":     id,
		"title":  id,
		"type":   "agent",
		"status": "in_progress",
		"fields": map[string]string{"agent_state": agentState, "task_id": "gd-step-1"},
	}
}

func didCreate(requests []spawnReq) bool {
	for _, req := range requests {
		if req.method == http.MethodPost && req.path == "/v1/beads" {
			return true
		}
	}
	return false
}

// singleClaimGetCount counts the single-claim guard's own lookups.
//
// It must NOT count every GET /v1/beads: SpawnAgentResult also issues a
// task-shared PREDECESSOR lookup (ka-0000007863) against the same endpoint and
// the same task_id, differing only in the status filter — the guard asks for
// ACTIVE agents (is someone on this now?), the predecessor lookup asks for
// CLOSED ones (has someone been on this before?). Counting both conflates two
// unrelated queries and made these tests report a phantom "2 single-claim GETs".
func singleClaimGetCount(requests []spawnReq) int {
	n := 0
	for _, req := range requests {
		if req.method != http.MethodGet || req.path != "/v1/beads" {
			continue
		}
		if strings.Contains(req.rawQuery, "status=closed") {
			continue // the predecessor lookup, not the guard
		}
		n++
	}
	return n
}

func TestSpawnAgent_SingleClaim_LiveAgentBlocksDuplicate(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(singleClaimHandler(t, "bd-new",
		[]map[string]any{agentRow("bd-incumbent", "working")}, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "dup-bot", "gardenio", "gd-step-1", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-incumbent" {
		t.Errorf("expected existing live agent bd-incumbent, got %s", id)
	}
	if didCreate(requests) {
		t.Error("expected NO create POST — single-claim should have blocked the duplicate spawn")
	}
}

func TestSpawnAgent_SingleClaim_TerminalAgentAllowsRespawn(t *testing.T) {
	for _, state := range []string{"done", "failed"} {
		t.Run(state, func(t *testing.T) {
			var requests []spawnReq
			srv := httptest.NewServer(singleClaimHandler(t, "bd-fresh",
				[]map[string]any{agentRow("bd-dead", state)}, &requests))
			defer srv.Close()

			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
			id, err := c.SpawnAgent(context.Background(), "reclaim-bot", "gardenio", "gd-step-1", "crew", "",
				map[string]string{"single_claim_task": "1"})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if id != "bd-fresh" {
				t.Errorf("expected fresh spawn bd-fresh (incumbent %s is terminal), got %s", state, id)
			}
			if !didCreate(requests) {
				t.Error("expected a create POST — a terminal incumbent must not block reclaim")
			}
		})
	}
}

func TestSpawnAgent_SingleClaim_NoLiveAgentSpawnsNormally(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(singleClaimHandler(t, "bd-new", nil, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "first-bot", "gardenio", "gd-step-1", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-new" {
		t.Errorf("expected new spawn bd-new, got %s", id)
	}
	if singleClaimGetCount(requests) != 1 {
		t.Errorf("expected exactly 1 single-claim GET, got %d", singleClaimGetCount(requests))
	}
	if !didCreate(requests) {
		t.Error("expected a create POST when no live agent holds the task")
	}
}

func TestSpawnAgent_SingleClaim_FlagOmittedSkipsGuard(t *testing.T) {
	var requests []spawnReq
	// If the guard ran it would find a live agent and block; since the flag is
	// omitted, the guard must NOT query and the spawn proceeds.
	srv := httptest.NewServer(singleClaimHandler(t, "bd-new",
		[]map[string]any{agentRow("bd-incumbent", "working")}, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "fanout-bot", "gardenio", "gd-step-1", "crew", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-new" {
		t.Errorf("expected new spawn bd-new (guard disabled), got %s", id)
	}
	if singleClaimGetCount(requests) != 0 {
		t.Errorf("expected 0 single-claim GETs when flag omitted, got %d", singleClaimGetCount(requests))
	}
	if !didCreate(requests) {
		t.Error("expected a create POST when the single-claim flag is omitted")
	}
}

func TestSpawnAgent_SingleClaim_NoTaskIDSkipsGuard(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(singleClaimHandler(t, "bd-new",
		[]map[string]any{agentRow("bd-incumbent", "working")}, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	// Flag set but no task targeted → nothing to single-claim on.
	_, err := c.SpawnAgent(context.Background(), "adhoc-bot", "gardenio", "", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if singleClaimGetCount(requests) != 0 {
		t.Errorf("expected 0 single-claim GETs with empty taskID, got %d", singleClaimGetCount(requests))
	}
	if !didCreate(requests) {
		t.Error("expected a create POST when no task is targeted")
	}
}

func TestSpawnAgent_SingleClaim_FlagNotPersisted(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(singleClaimHandler(t, "bd-new", nil, &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "first-bot", "gardenio", "gd-step-1", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var createReq *spawnReq
	for i := range requests {
		if requests[i].method == http.MethodPost && requests[i].path == "/v1/beads" {
			createReq = &requests[i]
			break
		}
	}
	if createReq == nil {
		t.Fatal("expected a create POST")
	}
	var fields map[string]string
	_ = json.Unmarshal(createReq.body["fields"], &fields)
	if _, ok := fields["single_claim_task"]; ok {
		t.Error("single_claim_task is a control flag and must NOT be persisted on the agent bead")
	}
	if fields["task_id"] != "gd-step-1" {
		t.Errorf("expected fields.task_id=gd-step-1, got %q", fields["task_id"])
	}
	// The opt-in the controller's claim at pod create reads (gd-Yo28zKgkGx).
	if fields[FieldClaimTaskAtSpawn] != "true" {
		t.Errorf("expected fields.%s=true, got %q", FieldClaimTaskAtSpawn, fields[FieldClaimTaskAtSpawn])
	}
}

func TestSpawnAgent_SingleClaim_QueryErrorFailsOpen(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(body, &parsed)
		requests = append(requests, spawnReq{r.Method, r.URL.Path, parsed, r.URL.RawQuery})

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Single-claim query fails — guard must fail open and proceed.
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "bd-failopen"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "failopen-bot", "gardenio", "gd-step-1", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-failopen" {
		t.Errorf("expected fail-open spawn bd-failopen, got %s", id)
	}
	if !didCreate(requests) {
		t.Error("expected a create POST — a single-claim query error must fail open")
	}
}

// TestSpawnAgent_SingleClaim_GuardQueriesByTaskID asserts the guard scopes its
// lookup to type=agent + the target task_id field filter, so it cannot be
// tripped by unrelated agents (ka-0000004654).
func TestSpawnAgent_SingleClaim_GuardQueriesByTaskID(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			gotQuery = r.URL.Query()
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "bd-new"})
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "scoped-bot", "gardenio", "gd-step-42", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := gotQuery.Get("type"); got != "agent" {
		t.Errorf("expected type=agent filter, got %q", got)
	}
	foundField := false
	for _, f := range gotQuery["field"] {
		if f == "task_id=gd-step-42" {
			foundField = true
		}
	}
	if !foundField {
		t.Errorf("expected field=task_id=gd-step-42 filter, got %v", gotQuery["field"])
	}
}
