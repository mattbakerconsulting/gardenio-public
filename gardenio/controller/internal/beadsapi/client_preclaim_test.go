package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// preClaimHandler serves the SpawnAgent request chain and records every
// request so a test can assert whether — and how — the target task bead was
// pre-claimed. The name-dedup GET 404s (no existing agent). The single-claim
// GET (/v1/beads with a task_id field filter) returns no live agent. Create
// POST returns createID. Labels / dependency / pre-claim PATCH all 200.
func preClaimHandler(t *testing.T, createID string, requests *[]spawnReq) http.Handler {
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
			// Single-claim guard query — no live agent holds the task.
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": createID})
		default:
			w.WriteHeader(http.StatusOK)
		}
	})
}

// findPreClaim returns the PATCH request against the target task bead, or nil
// if the spawn did not pre-claim it.
func findPreClaim(requests []spawnReq, taskID string) *spawnReq {
	for i := range requests {
		if requests[i].method == http.MethodPatch && requests[i].path == "/v1/beads/"+taskID {
			return &requests[i]
		}
	}
	return nil
}

// TestSpawnAgent_PreClaim_StampsTargetInProgress asserts that a single-claim
// spawn synchronously stamps the TARGET task bead status=in_progress +
// assignee=agentName, closing the open+unassigned dispatch-race window
// (ka-0000002756). The stamp must write exactly what `kd claim` writes so the
// crew's boot claim is idempotent.
func TestSpawnAgent_PreClaim_StampsTargetInProgress(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(preClaimHandler(t, "bd-agent-1", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "fix-bot", "gardenio", "gd-bug-7", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-agent-1" {
		t.Fatalf("expected id bd-agent-1, got %s", id)
	}

	pc := findPreClaim(requests, "gd-bug-7")
	if pc == nil {
		t.Fatal("expected a pre-claim PATCH against the target task bead, found none")
	}
	var status, assignee string
	_ = json.Unmarshal(pc.body["status"], &status)
	_ = json.Unmarshal(pc.body["assignee"], &assignee)
	if status != "in_progress" {
		t.Errorf("expected pre-claim status=in_progress, got %q", status)
	}
	if assignee != "fix-bot" {
		t.Errorf("expected pre-claim assignee=fix-bot (== agent name), got %q", assignee)
	}
}

// TestSpawnAgent_PreClaim_SkippedWithoutFlag asserts the target task bead is
// NOT pre-claimed when the single_claim_task opt-in is absent — fan-out and
// AssignBead-pinned gardeners, which spawn without the flag, must leave the
// target's status/assignee untouched.
func TestSpawnAgent_PreClaim_SkippedWithoutFlag(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(preClaimHandler(t, "bd-agent-2", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "fanout-bot", "gardenio", "gd-bug-8", "crew", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc := findPreClaim(requests, "gd-bug-8"); pc != nil {
		t.Errorf("expected NO pre-claim PATCH without single_claim_task, got %+v", pc.body)
	}
}

// TestSpawnAgent_PreClaim_NoTaskIDNoStamp asserts an ad-hoc spawn (no task
// targeted) never pre-claims, even with the flag set — there is nothing to
// pre-claim.
func TestSpawnAgent_PreClaim_NoTaskIDNoStamp(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(preClaimHandler(t, "bd-agent-3", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "adhoc-bot", "gardenio", "", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, req := range requests {
		if req.method == http.MethodPatch {
			t.Errorf("expected no PATCH for a task-less spawn, got PATCH %s", req.path)
		}
	}
}

// TestSpawnAgent_PreClaim_FailsOpen asserts a pre-claim PATCH failure does not
// fail the spawn: the agent bead is still created and returned, matching the
// best-effort posture of every other post-create step (labels, dependency).
func TestSpawnAgent_PreClaim_FailsOpen(t *testing.T) {
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
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "bd-agent-4"})
		case r.Method == http.MethodPatch:
			// Pre-claim stamp fails — spawn must still succeed.
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "failopen-bot", "gardenio", "gd-bug-9", "crew", "",
		map[string]string{"single_claim_task": "1"})
	if err != nil {
		t.Fatalf("expected spawn to succeed despite pre-claim failure, got error: %v", err)
	}
	if id != "bd-agent-4" {
		t.Errorf("expected agent id bd-agent-4 returned despite pre-claim failure, got %s", id)
	}
	if findPreClaim(requests, "gd-bug-9") == nil {
		t.Error("expected a pre-claim PATCH attempt against the target task bead")
	}
}
