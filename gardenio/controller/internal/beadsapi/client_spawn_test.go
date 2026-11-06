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

// spawnTestHandler returns an HTTP handler that serves both the dedup-check
// GET against the indexed by-name endpoint (404 — no existing agent) and the
// create POST with the given bead ID. All requests are appended to the
// requests slice.
func spawnTestHandler(beadID string, requests *[]spawnReq) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(body, &parsed)
		*requests = append(*requests, spawnReq{r.Method, r.URL.Path, parsed, r.URL.RawQuery})

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/") {
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
			return
		}
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": beadID})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

type spawnReq struct {
	method string
	path   string
	body   map[string]json.RawMessage
	// rawQuery is the request's URL query string. Needed to tell apart two
	// GETs that hit the same endpoint with the same task_id and differ only in
	// their status filter — the single-claim guard (active agents) and the
	// task-shared predecessor lookup (closed agents).
	rawQuery string
}

// findReq returns the first recorded request matching method+path, and whether
// one was found. Tests match on WHAT was requested rather than on a positional
// index: SpawnAgentResult's request sequence is not a stable contract — the
// task-shared predecessor lookup (ka-0000007863) legitimately inserts a GET
// mid-sequence for task-targeted spawns — and an index-based assertion turns
// every such addition into a spurious failure that says nothing about
// behavior.
func findReq(reqs []spawnReq, method, path string) (spawnReq, bool) {
	for _, r := range reqs {
		if r.method == method && r.path == path {
			return r, true
		}
	}
	return spawnReq{}, false
}

// findLabelReq returns the label-POST carrying the given label value.
func findLabelReq(reqs []spawnReq, beadID, label string) bool {
	for _, r := range reqs {
		if r.method != http.MethodPost || r.path != "/v1/beads/"+beadID+"/labels" {
			continue
		}
		var got string
		_ = json.Unmarshal(r.body["label"], &got)
		if got == label {
			return true
		}
	}
	return false
}

// --- SpawnAgent tests ---

func TestSpawnAgent_SendsCorrectRequest(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(spawnTestHandler("bd-agent-42", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id != "bd-agent-42" {
		t.Errorf("expected id bd-agent-42, got %s", id)
	}

	// Expect four requests: GET /v1/beads (dedup), POST /v1/beads,
	// POST /v1/beads/{id}/labels (project), POST /v1/beads/{id}/labels (role:crew default).
	if len(requests) != 4 {
		t.Fatalf("expected 4 HTTP requests, got %d", len(requests))
	}

	// First request: dedup check.
	dedupReq := requests[0]
	if dedupReq.method != http.MethodGet {
		t.Errorf("expected GET for dedup, got %s", dedupReq.method)
	}

	// Second request: create agent bead.
	createReq := requests[1]
	if createReq.method != http.MethodPost {
		t.Errorf("expected POST, got %s", createReq.method)
	}
	if createReq.path != "/v1/beads" {
		t.Errorf("expected /v1/beads, got %s", createReq.path)
	}

	var beadType, beadTitle string
	_ = json.Unmarshal(createReq.body["type"], &beadType)
	_ = json.Unmarshal(createReq.body["title"], &beadTitle)
	if beadType != "agent" {
		t.Errorf("expected type=agent, got %s", beadType)
	}
	if beadTitle != "my-bot" {
		t.Errorf("expected title=my-bot, got %s", beadTitle)
	}

	var fields map[string]string
	_ = json.Unmarshal(createReq.body["fields"], &fields)
	if fields["agent"] != "my-bot" {
		t.Errorf("expected fields.agent=my-bot, got %s", fields["agent"])
	}
	if fields["project"] != "gardenio" {
		t.Errorf("expected fields.project=gardenio, got %s", fields["project"])
	}
	if fields["mode"] != "crew" {
		t.Errorf("expected fields.mode=crew, got %s", fields["mode"])
	}
	if fields["role"] != "crew" {
		t.Errorf("expected fields.role=crew (default), got %s", fields["role"])
	}

	// Third request: add project label.
	projectLabelReq := requests[2]
	if projectLabelReq.path != "/v1/beads/bd-agent-42/labels" {
		t.Errorf("expected label path /v1/beads/bd-agent-42/labels, got %s", projectLabelReq.path)
	}
	var projectLabel string
	_ = json.Unmarshal(projectLabelReq.body["label"], &projectLabel)
	if projectLabel != "project:gardenio" {
		t.Errorf("expected label=project:gardenio, got %s", projectLabel)
	}

	// Fourth request: add role:crew label (from defaulted role).
	roleLabelReq := requests[3]
	var roleLabel string
	_ = json.Unmarshal(roleLabelReq.body["label"], &roleLabel)
	if roleLabel != "role:crew" {
		t.Errorf("expected label=role:crew, got %s", roleLabel)
	}
}

func TestSpawnAgent_CustomRole(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(spawnTestHandler("bd-agent-77", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "gardener", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Skip dedup GET (index 0), check create POST (index 1).
	if len(requests) < 2 {
		t.Fatal("expected at least two requests")
	}
	var fields map[string]string
	createReq, ok := findReq(requests, http.MethodPost, "/v1/beads")
	if !ok {
		t.Fatalf("no create POST /v1/beads in %d requests", len(requests))
	}
	_ = json.Unmarshal(createReq.body["fields"], &fields)
	if fields["role"] != "gardener" {
		t.Errorf("expected fields.role=gardener, got %s", fields["role"])
	}

	// Verify role label is sent.
	foundRoleLabel := false
	for _, req := range requests[2:] {
		var label string
		_ = json.Unmarshal(req.body["label"], &label)
		if label == "role:gardener" {
			foundRoleLabel = true
		}
	}
	if !foundRoleLabel {
		t.Errorf("expected role:gardener label to be added")
	}
}

func TestSpawnAgent_PropagatesCreateError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// All requests fail — dedup check failure is best-effort, create fails.
		http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "bad-bot", "gardenio", "", "", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestSpawnAgent_WithTaskID_SetsDescriptionAndLinksDependency(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(spawnTestHandler("bd-agent-99", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "gd-task-123", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "bd-agent-99" {
		t.Errorf("expected id bd-agent-99, got %s", id)
	}

	// Assert on WHAT was requested, not on positions — see findReq.
	createReq, ok := findReq(requests, http.MethodPost, "/v1/beads")
	if !ok {
		t.Fatalf("no create POST /v1/beads in %d requests", len(requests))
	}
	var desc string
	_ = json.Unmarshal(createReq.body["description"], &desc)
	if desc != "Assigned to task: gd-task-123" {
		t.Errorf("expected description %q, got %q", "Assigned to task: gd-task-123", desc)
	}
	var fields map[string]string
	_ = json.Unmarshal(createReq.body["fields"], &fields)
	if fields["task_id"] != "gd-task-123" {
		t.Errorf("expected fields.task_id=gd-task-123, got %s", fields["task_id"])
	}

	if !findLabelReq(requests, "bd-agent-99", "project:gardenio") {
		t.Error("expected a project:gardenio label POST")
	}
	if !findLabelReq(requests, "bd-agent-99", "role:crew") {
		t.Error("expected a role:crew label POST (from the defaulted role)")
	}

	depReq, ok := findReq(requests, http.MethodPost, "/v1/beads/bd-agent-99/dependencies")
	if !ok {
		t.Fatalf("no dependency POST in %d requests", len(requests))
	}
	var dependsOn, depType string
	_ = json.Unmarshal(depReq.body["depends_on_id"], &dependsOn)
	_ = json.Unmarshal(depReq.body["type"], &depType)
	if dependsOn != "gd-task-123" {
		t.Errorf("expected depends_on_id=gd-task-123, got %s", dependsOn)
	}
	if depType != "assigned" {
		t.Errorf("expected dep type=assigned, got %s", depType)
	}
}

func TestSpawnAgent_WithTaskAndPrompt_SetsBothFields(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(spawnTestHandler("bd-agent-55", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "fix-auth-a7k", "gardenio", "gd-task-456", "", "fix the auth bug")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Index 1 is the create POST (index 0 is dedup GET).
	createReq, ok := findReq(requests, http.MethodPost, "/v1/beads")
	if !ok {
		t.Fatalf("no create POST /v1/beads in %d requests", len(requests))
	}
	var fields map[string]string
	_ = json.Unmarshal(createReq.body["fields"], &fields)

	// Both prompt and task_id should be in fields.
	if fields["prompt"] != "fix the auth bug" {
		t.Errorf("expected fields.prompt='fix the auth bug', got %q", fields["prompt"])
	}
	if fields["task_id"] != "gd-task-456" {
		t.Errorf("expected fields.task_id='gd-task-456', got %q", fields["task_id"])
	}

	// Description should reference the task (task takes precedence).
	var desc string
	_ = json.Unmarshal(createReq.body["description"], &desc)
	if desc != "Assigned to task: gd-task-456" {
		t.Errorf("expected description to reference task, got %q", desc)
	}
}

// --- Dedup tests ---

func TestSpawnAgent_Dedup_ReturnsExistingBead(t *testing.T) {
	var requests []spawnReq

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(body, &parsed)
		requests = append(requests, spawnReq{r.Method, r.URL.Path, parsed, r.URL.RawQuery})

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/") {
			// Return an existing active agent with matching name.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "bd-existing-agent",
				"title":  "my-bot",
				"type":   "agent",
				"status": "open",
				"fields": map[string]string{"agent": "my-bot", "project": "gardenio"},
			})
			return
		}
		// Should NOT reach create — fail loudly if it does.
		t.Error("unexpected POST to create bead — dedup should have prevented it")
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id != "bd-existing-agent" {
		t.Errorf("expected existing bead id bd-existing-agent, got %s", id)
	}

	// Only one request: the dedup GET. No POST.
	if len(requests) != 1 {
		t.Errorf("expected 1 request (dedup GET only), got %d", len(requests))
	}
}

func TestSpawnAgent_Dedup_IgnoresAgentsWithDifferentName(t *testing.T) {
	var requests []spawnReq

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var parsed map[string]json.RawMessage
		_ = json.Unmarshal(body, &parsed)
		requests = append(requests, spawnReq{r.Method, r.URL.Path, parsed, r.URL.RawQuery})

		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/") {
			// No active bead with the requested name → daemon returns 404.
			http.Error(w, `{"error":"agent not found"}`, http.StatusNotFound)
			return
		}
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "bd-new-agent"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id != "bd-new-agent" {
		t.Errorf("expected new bead id bd-new-agent, got %s", id)
	}
}

// --- extraFields / spawned_by identity tests ---

func TestSpawnAgent_ExtraFields_PropagatesSpawnedBy(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(spawnTestHandler("bd-agent-id1", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "", "",
		map[string]string{"spawned_by": "alice@example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Index 1 is the create POST (index 0 is dedup GET).
	createReq, ok := findReq(requests, http.MethodPost, "/v1/beads")
	if !ok {
		t.Fatalf("no create POST /v1/beads in %d requests", len(requests))
	}
	var fields map[string]string
	_ = json.Unmarshal(createReq.body["fields"], &fields)

	if fields["spawned_by"] != "alice@example.com" {
		t.Errorf("expected fields.spawned_by=alice@example.com, got %q", fields["spawned_by"])
	}
	// Standard fields should still be present.
	if fields["agent"] != "my-bot" {
		t.Errorf("expected fields.agent=my-bot, got %q", fields["agent"])
	}
}

func TestSpawnAgent_NoExtraFields_OmitsSpawnedBy(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(spawnTestHandler("bd-agent-id2", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	createReq, ok := findReq(requests, http.MethodPost, "/v1/beads")
	if !ok {
		t.Fatalf("no create POST /v1/beads in %d requests", len(requests))
	}
	var fields map[string]string
	_ = json.Unmarshal(createReq.body["fields"], &fields)

	if _, ok := fields["spawned_by"]; ok {
		t.Errorf("expected spawned_by to be absent when no extraFields, got %q", fields["spawned_by"])
	}
}

func TestSpawnAgent_ExtraFields_MergedWithStandard(t *testing.T) {
	var requests []spawnReq
	srv := httptest.NewServer(spawnTestHandler("bd-agent-id3", &requests))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "gd-task-789", "gardener", "run tests",
		map[string]string{"spawned_by": "bob@example.com"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	createReq, ok := findReq(requests, http.MethodPost, "/v1/beads")
	if !ok {
		t.Fatalf("no create POST /v1/beads in %d requests", len(requests))
	}
	var fields map[string]string
	_ = json.Unmarshal(createReq.body["fields"], &fields)

	// Extra field.
	if fields["spawned_by"] != "bob@example.com" {
		t.Errorf("spawned_by=%q, want bob@example.com", fields["spawned_by"])
	}
	// Standard fields should coexist.
	if fields["agent"] != "my-bot" {
		t.Errorf("agent=%q, want my-bot", fields["agent"])
	}
	if fields["role"] != "gardener" {
		t.Errorf("role=%q, want gardener", fields["role"])
	}
	if fields["task_id"] != "gd-task-789" {
		t.Errorf("task_id=%q, want gd-task-789", fields["task_id"])
	}
	if fields["prompt"] != "run tests" {
		t.Errorf("prompt=%q, want 'run tests'", fields["prompt"])
	}
}

func TestSpawnAgent_Dedup_ProceedsOnListError(t *testing.T) {
	callCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodGet {
			// Dedup check fails.
			http.Error(w, `{"error":"service unavailable"}`, http.StatusServiceUnavailable)
			return
		}
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "bd-created-anyway"})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.SpawnAgent(context.Background(), "my-bot", "gardenio", "", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id != "bd-created-anyway" {
		t.Errorf("expected id bd-created-anyway, got %s", id)
	}

	// Should have made both the failed GET and the successful POST.
	if callCount < 2 {
		t.Errorf("expected at least 2 requests, got %d", callCount)
	}
}
