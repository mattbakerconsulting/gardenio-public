package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- CreateBead tests ---

func TestCreateBead_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody CreateBeadRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		resp := map[string]string{"id": "gd-new123"}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.CreateBead(context.Background(), CreateBeadRequest{
		Title:    "New task",
		Type:     "task",
		Priority: 2,
		Labels:   []string{"project:gardenio"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "gd-new123" {
		t.Errorf("expected id gd-new123, got %s", id)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/beads" {
		t.Errorf("expected path /v1/beads, got %s", gotPath)
	}
	if gotBody.Title != "New task" {
		t.Errorf("expected title 'New task', got %s", gotBody.Title)
	}
	if gotBody.Type != "task" {
		t.Errorf("expected type task, got %s", gotBody.Type)
	}
}

func TestCreateBead_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"missing title"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.CreateBead(context.Background(), CreateBeadRequest{Type: "task"})
	if err == nil {
		t.Fatal("expected error for 400 response")
	}
}

// --- DeleteBead tests ---

func TestDeleteBead_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.DeleteBead(context.Background(), "gd-del1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/v1/beads/gd-del1" {
		t.Errorf("expected path /v1/beads/gd-del1, got %s", gotPath)
	}
}

// --- UpdateBead tests ---

func TestUpdateBead_SendsOnlySetFields(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	title := "Updated title"
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBead(context.Background(), "gd-upd1", UpdateBeadRequest{
		Title: &title,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["title"] != "Updated title" {
		t.Errorf("expected title in body, got %v", gotBody)
	}
	if _, ok := gotBody["description"]; ok {
		t.Error("description should not be in body when not set")
	}
}

func TestUpdateBead_NoOpWhenEmpty(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBead(context.Background(), "gd-noop", UpdateBeadRequest{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if requestCount != 0 {
		t.Errorf("expected no requests for empty update, got %d", requestCount)
	}
}

func TestUpdateBead_AllFields(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	title := "T"
	desc := "D"
	assignee := "matt"
	status := "closed"
	notes := "N"
	priority := 1

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.UpdateBead(context.Background(), "gd-full", UpdateBeadRequest{
		Title:       &title,
		Description: &desc,
		Assignee:    &assignee,
		Status:      &status,
		Notes:       &notes,
		Priority:    &priority,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["title"] != "T" {
		t.Errorf("expected title=T, got %v", gotBody["title"])
	}
	if gotBody["assignee"] != "matt" {
		t.Errorf("expected assignee=matt, got %v", gotBody["assignee"])
	}
}

// --- AddDependency tests ---

func TestAddDependency_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.AddDependency(context.Background(), "gd-child", "gd-parent", "blocks", "matt-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "POST" {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/beads/gd-child/dependencies" {
		t.Errorf("expected path /v1/beads/gd-child/dependencies, got %s", gotPath)
	}
	if gotBody["depends_on_id"] != "gd-parent" {
		t.Errorf("expected depends_on_id=gd-parent, got %s", gotBody["depends_on_id"])
	}
	if gotBody["type"] != "blocks" {
		t.Errorf("expected type=blocks, got %s", gotBody["type"])
	}
	if gotBody["created_by"] != "matt-1" {
		t.Errorf("expected created_by=matt-1, got %s", gotBody["created_by"])
	}
}

func TestRemoveDependency_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath, gotDependsOn, gotType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotDependsOn = r.URL.Query().Get("depends_on_id")
		gotType = r.URL.Query().Get("type")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.RemoveDependency(context.Background(), "gd-doc", "gd-bundle", "parent-child")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotMethod != "DELETE" {
		t.Errorf("expected DELETE, got %s", gotMethod)
	}
	if gotPath != "/v1/beads/gd-doc/dependencies" {
		t.Errorf("expected path /v1/beads/gd-doc/dependencies, got %s", gotPath)
	}
	if gotDependsOn != "gd-bundle" {
		t.Errorf("expected depends_on_id=gd-bundle, got %s", gotDependsOn)
	}
	if gotType != "parent-child" {
		t.Errorf("expected type=parent-child, got %s", gotType)
	}
}

// TestRemoveDependency_OmitsEmptyType verifies an empty depType leaves the
// type query param off entirely (server removes the edge regardless of type).
func TestRemoveDependency_OmitsEmptyType(t *testing.T) {
	var hasType bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasType = r.URL.Query()["type"]
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if err := c.RemoveDependency(context.Background(), "gd-doc", "gd-bundle", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasType {
		t.Errorf("expected no type query param when depType is empty")
	}
}

// --- GetDependencies tests ---

func TestGetDependencies_ParsesResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/beads/gd-task1/dependencies" {
			t.Errorf("expected path /v1/beads/gd-task1/dependencies, got %s", r.URL.Path)
		}
		resp := map[string]any{
			"dependencies": []map[string]string{
				{"bead_id": "gd-task1", "depends_on_id": "gd-epic1", "type": "parent-child"},
				{"bead_id": "gd-task1", "depends_on_id": "gd-task0", "type": "blocks"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	deps, err := c.GetDependencies(context.Background(), "gd-task1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependencies, got %d", len(deps))
	}
	if deps[0].DependsOnID != "gd-epic1" || deps[0].Type != "parent-child" {
		t.Errorf("unexpected first dep: %+v", deps[0])
	}
}

// TestBatchGetDependencies_ForwardShape verifies the request URL (ids +
// direction params) and that empty per-ID slots in the response land in the
// returned map as nil — callers rely on map presence to mean "asked, no
// edges" rather than the daemon never knowing about the ID.
func TestBatchGetDependencies_ForwardShape(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.RequestURI()
		resp := map[string]any{
			"results": []map[string]any{
				{"id": "gd-A", "deps": []map[string]string{
					{"bead_id": "gd-A", "depends_on_id": "gd-A1", "type": "parent-child"},
					{"bead_id": "gd-A", "depends_on_id": "gd-A2", "type": "blocks"},
				}},
				{"id": "gd-B", "deps": []any{}},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetDependencies(context.Background(), []string{"gd-A", "gd-B"}, "forward")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotURL, "ids=gd-A%2Cgd-B") || !strings.Contains(gotURL, "direction=forward") {
		t.Errorf("unexpected request URL: %s", gotURL)
	}
	if len(out["gd-A"]) != 2 || out["gd-A"][0].DependsOnID != "gd-A1" {
		t.Errorf("gd-A deps: %+v", out["gd-A"])
	}
	if _, ok := out["gd-B"]; !ok {
		t.Errorf("gd-B should be present (with nil/empty slice), got map: %+v", out)
	}
	if out["gd-B"] != nil {
		t.Errorf("gd-B should map to nil (empty deps), got %+v", out["gd-B"])
	}
}

// TestBatchGetDependencies_ReverseDirection checks that direction=reverse is
// propagated and that returned rows preserve the daemon's reverse-direction
// shape (BeadID = dependent bead, DependsOnID = requested bead).
func TestBatchGetDependencies_ReverseDirection(t *testing.T) {
	var gotDirection string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotDirection = r.URL.Query().Get("direction")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"id": "gd-LEAF", "deps": []map[string]string{
					{"bead_id": "gd-PARENT", "depends_on_id": "gd-LEAF", "type": "parent-child"},
				}},
			},
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetDependencies(context.Background(), []string{"gd-LEAF"}, "reverse")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotDirection != "reverse" {
		t.Errorf("expected direction=reverse, got %q", gotDirection)
	}
	if got := out["gd-LEAF"]; len(got) != 1 || got[0].BeadID != "gd-PARENT" {
		t.Errorf("unexpected reverse-dep row: %+v", got)
	}
}

// TestBatchGetDependencies_Sharding ensures requests larger than the per-call
// cap are split into multiple round-trips and the per-shard results are
// merged into a single map.
func TestBatchGetDependencies_Sharding(t *testing.T) {
	// Shards are dispatched concurrently (ka-0000000376), so the call counter
	// is atomic — a plain int++ here is a data race the -race build catches.
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		shardIDs := strings.Split(r.URL.Query().Get("ids"), ",")
		results := make([]map[string]any, 0, len(shardIDs))
		for _, id := range shardIDs {
			results = append(results, map[string]any{
				"id":   id,
				"deps": []map[string]string{{"bead_id": id, "depends_on_id": id + "-dep", "type": "blocks"}},
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	ids := make([]string, BatchGetDependenciesMaxIDs+5)
	for i := range ids {
		ids[i] = fmt.Sprintf("gd-%03d", i)
	}
	out, err := c.BatchGetDependencies(context.Background(), ids, "forward")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 shard calls, got %d", got)
	}
	if len(out) != len(ids) {
		t.Errorf("expected %d entries, got %d", len(ids), len(out))
	}
}

func TestBatchGetDependencies_InvalidDirection(t *testing.T) {
	c := &Client{baseURL: "http://example", httpClient: http.DefaultClient}
	if _, err := c.BatchGetDependencies(context.Background(), []string{"gd-A"}, "sideways"); err == nil {
		t.Fatal("expected error for invalid direction")
	}
}

func TestBatchGetDependencies_EmptyIDsNoCall(t *testing.T) {
	c := &Client{baseURL: "http://example", httpClient: http.DefaultClient}
	out, err := c.BatchGetDependencies(context.Background(), nil, "forward")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty result, got %+v", out)
	}
}

func TestGetDependencies_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"dependencies": []any{}}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	deps, err := c.GetDependencies(context.Background(), "gd-nodeps")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deps) != 0 {
		t.Errorf("expected 0 dependencies, got %d", len(deps))
	}
}

// TestBatchGetChildrenByStatus_Shape verifies the request URL (parents param)
// and that the daemon's per-parent status histogram is returned verbatim,
// including an empty (childless) bucket that the daemon echoes back.
func TestBatchGetChildrenByStatus_Shape(t *testing.T) {
	var gotURL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.RequestURI()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"histograms": map[string]map[string]int{
				"gd-A": {"open": 3, "closed": 7},
				"gd-B": {},
			},
		})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	out, err := c.BatchGetChildrenByStatus(context.Background(), []string{"gd-A", "gd-B"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotURL, "parents=gd-A%2Cgd-B") {
		t.Errorf("unexpected request URL: %s", gotURL)
	}
	if out["gd-A"]["open"] != 3 || out["gd-A"]["closed"] != 7 {
		t.Errorf("gd-A histogram: %+v", out["gd-A"])
	}
	if bucket, ok := out["gd-B"]; !ok || len(bucket) != 0 {
		t.Errorf("gd-B should be present with an empty bucket, got ok=%v bucket=%+v", ok, bucket)
	}
}

// TestBatchGetChildrenByStatus_Sharding ensures requests larger than the
// per-call parent cap are split into multiple round-trips and merged.
func TestBatchGetChildrenByStatus_Sharding(t *testing.T) {
	// Shards are dispatched concurrently (ka-0000000376), so the call counter
	// is atomic — a plain int++ here is a data race the -race build catches.
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		shardIDs := strings.Split(r.URL.Query().Get("parents"), ",")
		hist := make(map[string]map[string]int, len(shardIDs))
		for _, id := range shardIDs {
			hist[id] = map[string]int{"open": 1}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"histograms": hist})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	ids := make([]string, BatchChildrenByStatusMaxParents+5)
	for i := range ids {
		ids[i] = fmt.Sprintf("gd-%03d", i)
	}
	out, err := c.BatchGetChildrenByStatus(context.Background(), ids)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 shard calls, got %d", got)
	}
	if len(out) != len(ids) {
		t.Errorf("expected %d entries, got %d", len(ids), len(out))
	}
}

func TestBatchGetChildrenByStatus_EmptyIDsNoCall(t *testing.T) {
	c := &Client{baseURL: "http://example", httpClient: http.DefaultClient}
	out, err := c.BatchGetChildrenByStatus(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty result, got %+v", out)
	}
}

// --- Health tests ---

func TestHealth_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/health" {
			t.Errorf("expected path /v1/health, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealth_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"database unreachable"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.Health(context.Background())
	if err == nil {
		t.Fatal("expected error for unhealthy server")
	}
	if !strings.Contains(err.Error(), "health check failed") {
		t.Errorf("expected health check error, got: %v", err)
	}
}

// --- ListBeadsFiltered tests ---

func TestListBeadsFiltered_AllQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{ID: "gd-1", Title: "Task 1", Type: "task", Status: "open"},
			},
			Total: 1,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	result, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		Types:            []string{"task", "bug"},
		Statuses:         []string{"open"},
		Labels:           []string{"project:gardenio"},
		Assignee:         "matt-1",
		Assignees:        []string{"agent-alice", "agent-bob"},
		Search:           "auth",
		Sort:             "priority",
		NoOpenDeps:       true,
		Parent:           "gd-Epic1",
		ExclusiveProject: "gardenio",
		Titles:           []string{"lands-pause", "a, b"},
		Limit:            20,
		Offset:           10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Total != 1 {
		t.Errorf("expected total=1, got %d", result.Total)
	}
	if len(result.Beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(result.Beads))
	}

	for _, expected := range []string{
		"type=task%2Cbug",
		"status=open",
		"labels=project%3Agardenio",
		"assignee=matt-1",
		"assignee_in=agent-alice%2Cagent-bob",
		"search=auth",
		"sort=priority",
		"no_open_deps=true",
		"parent=gd-Epic1",
		"exclusive_project=gardenio",
		"title=lands-pause&title=a%2C+b",
		"limit=20",
		"offset=10",
	} {
		if !strings.Contains(gotQuery, expected) {
			t.Errorf("expected query to contain %s, got %s", expected, gotQuery)
		}
	}
}

// TestListBeadsFiltered_ClosedAfter pins that ListBeadsQuery.ClosedAfter rides
// the wire as ?closed_after=<RFC3339> (opted-in only, zero value omits the
// param), mirroring UpdatedAfter/CreatedAfter. The gardener D2 throughput scorer
// depends on this window to count exactly the children closed inside the
// scoring window (task ka-0000003456).
func TestListBeadsFiltered_ClosedAfter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		set     bool
		wantHas bool
	}{
		{"set forwards the param", true, true},
		{"zero value omits the param", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				_ = json.NewEncoder(w).Encode(listBeadsResponse{Total: 0})
			}))
			defer srv.Close()

			q := ListBeadsQuery{Parent: "gd-epic", CountOnly: true}
			if tc.set {
				q.ClosedAfter = time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
			}
			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
			if _, err := c.ListBeadsFiltered(context.Background(), q); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			has := strings.Contains(gotQuery, "closed_after=2026-07-05T00%3A00%3A00Z")
			if has != tc.wantHas {
				t.Errorf("closed_after present=%v, want %v (query=%s)", has, tc.wantHas, gotQuery)
			}
		})
	}
}

// TestListBeadsFiltered_WithParent pins that ListBeadsQuery.WithParent rides the
// wire as ?with_parent=true (opted-in only), and that the daemon's parent_id /
// parent_title round-trip through beadJSON.toDetail into BeadDetail so a caller
// reading `gb ready --json` gets the parent-epic id per row (ka-0000006083).
func TestListBeadsFiltered_WithParent(t *testing.T) {
	for _, tc := range []struct {
		name       string
		withParent bool
		wantParam  bool
	}{
		{"opted in forwards the param", true, true},
		{"default omits the param", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				resp := listBeadsResponse{
					Beads: []beadJSON{
						{ID: "gd-leaf", Title: "leaf", Type: "task", Status: "open", ParentID: "gd-epic", ParentTitle: "Delivery Epic"},
						{ID: "gd-orphan", Title: "orphan", Type: "task", Status: "open"},
					},
					Total: 2,
				}
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
			result, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{WithParent: tc.withParent})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := strings.Contains(gotQuery, "with_parent=true"); got != tc.wantParam {
				t.Errorf("with_parent present=%v, want %v (query=%q)", got, tc.wantParam, gotQuery)
			}
			// The daemon fields must survive toDetail regardless of the flag —
			// the client never strips a field the daemon chose to send.
			byID := map[string]*BeadDetail{}
			for _, b := range result.Beads {
				byID[b.ID] = b
			}
			if got := byID["gd-leaf"]; got.ParentID != "gd-epic" || got.ParentTitle != "Delivery Epic" {
				t.Errorf("leaf parent not carried through toDetail: id=%q title=%q", got.ParentID, got.ParentTitle)
			}
			if got := byID["gd-orphan"]; got.ParentID != "" || got.ParentTitle != "" {
				t.Errorf("orphan must round-trip empty parent, got id=%q title=%q", got.ParentID, got.ParentTitle)
			}
		})
	}
}

// TestListBeadsFiltered_Unassigned pins that ListBeadsQuery.Unassigned rides the
// wire as ?unassigned=true — the daemon param that renders as `assignee IS NULL
// OR assignee = <empty>` (ka-0000007209). It backs `gb ready --unassigned`.
//
// The false case is the load-bearing half: the param must be OMITTED when unset,
// because a stray unassigned=true would hide every CLAIMED bead from every
// existing ListBeadsFiltered caller in the controller and bridge.
func TestListBeadsFiltered_Unassigned(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unassigned bool
		wantParam  bool
	}{
		{"set emits the param", true, true},
		{"unset omits the param", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
			}))
			defer srv.Close()

			c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
			if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
				Statuses:   []string{"open"},
				Unassigned: tc.unassigned,
				Limit:      20,
			}); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if got := strings.Contains(gotQuery, "unassigned=true"); got != tc.wantParam {
				t.Errorf("query %q contains unassigned=true = %v, want %v", gotQuery, got, tc.wantParam)
			}
		})
	}
}

// TestListBeadsFiltered_ExcludeTypes pins that ExcludeTypes serializes as the
// comma-joined ?exclude_type= param the daemon parses into its server-side
// `type NOT IN (...)` clause. It backs `gb ready --exclude-type`, replacing a
// client-side `jq 'map(select(.type != …))'` post-filter (ka-0000007209).
func TestListBeadsFiltered_ExcludeTypes(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		Statuses:     []string{"open"},
		ExcludeTypes: []string{"epic", "molecule"},
		Limit:        20,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotQuery, "exclude_type=epic%2Cmolecule") {
		t.Errorf("query %q does not contain exclude_type=epic%%2Cmolecule", gotQuery)
	}
}

// TestListBeadsFiltered_Fields pins that the Fields map serializes as one
// repeated `field=key=value` query param per entry (Add, not Set, so multiple
// predicates compose). This is the Gardener Page's "all iterations of gardener
// X" lookup shape: Fields{"schedule_id": gardenerID}. The daemon splits each
// field= value on the first '=' (http_beads.go) and applies it as
// fields->>'key' = 'value' (queries.go), backed by migration 031's index.
func TestListBeadsFiltered_Fields(t *testing.T) {
	var gotValues url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotValues = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		Types:  []string{"agent"},
		Fields: map[string]string{"schedule_id": "gd-GardenerX", "project": "gardenio"},
		Sort:   "created_at",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Both predicates must arrive as separate `field=` entries (Add, not Set):
	// url.Values keeps them as a slice, so a Set would collapse to one.
	fields := gotValues["field"]
	if len(fields) != 2 {
		t.Fatalf("expected 2 field= params, got %d: %v", len(fields), fields)
	}
	want := map[string]bool{"schedule_id=gd-GardenerX": true, "project=gardenio": true}
	for _, f := range fields {
		if !want[f] {
			t.Errorf("unexpected field= value %q (want one of %v)", f, want)
		}
		delete(want, f)
	}
	if len(want) != 0 {
		t.Errorf("missing field= values: %v", want)
	}
}

// TestListBeadsFiltered_FieldsIn: FieldsIn sends one field_in=key=a,b,c per
// key, and none for a key with no values (gd-OYKXtzN9kr).
func TestListBeadsFiltered_FieldsIn(t *testing.T) {
	var gotValues url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotValues = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		FieldsIn: map[string][]string{"channel_id": {"gd-c1", "gd-c2"}, "thread_id": nil},
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := gotValues["field_in"]; len(got) != 1 || got[0] != "channel_id=gd-c1,gd-c2" {
		t.Errorf("field_in = %v, want [channel_id=gd-c1,gd-c2]", got)
	}
}

// TestListBeadsFiltered_UpdatedBefore pins the index health-badge query shape
// (ka-0000002729): UpdatedBefore serializes as ?updated_before=<RFC3339 UTC>,
// combined with CountOnly + a status filter for a cheap per-project stale count.
func TestListBeadsFiltered_UpdatedBefore(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 7})
	}))
	defer srv.Close()

	cutoff := time.Date(2026, 6, 22, 12, 0, 0, 0, time.UTC)
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	result, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		Statuses:      []string{"in_progress"},
		Labels:        []string{"project:gardenio"},
		UpdatedBefore: cutoff,
		CountOnly:     true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Total != 7 {
		t.Errorf("expected total=7, got %d", result.Total)
	}
	if got := gotQuery.Get("updated_before"); got != "2026-06-22T12:00:00Z" {
		t.Errorf("updated_before = %q, want 2026-06-22T12:00:00Z", got)
	}
	if got := gotQuery.Get("count_only"); got != "true" {
		t.Errorf("count_only = %q, want true", got)
	}
}

// TestListBeadsFiltered_UpdatedBeforeOmittedWhenZero pins that a zero
// UpdatedBefore is not serialized (no filter), mirroring UpdatedAfter.
func TestListBeadsFiltered_UpdatedBeforeOmittedWhenZero(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{Statuses: []string{"open"}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := gotQuery["updated_before"]; ok {
		t.Errorf("updated_before should be omitted for zero time, got %q", gotQuery.Get("updated_before"))
	}
}

func TestListBeadsFiltered_EmptyQuery(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		resp := listBeadsResponse{Beads: nil, Total: 0}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	result, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Total != 0 {
		t.Errorf("expected total=0, got %d", result.Total)
	}
	if gotPath != "/v1/beads" {
		t.Errorf("expected path /v1/beads, got %s", gotPath)
	}
}

// TestListBeadsFiltered_OperatedByAliases pins that the client serializes
// OperatedByAliases as the `operated_by_alias_in` query param and never sends
// the legacy `operated_by` / `operated_by_is_null` params (dropped in daemon
// migration 024). Regression guard for ka-0000003586.
func TestListBeadsFiltered_OperatedByAliases(t *testing.T) {
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		OperatedByAliases: []string{"oidc:matt@acme.example", "slack:U0OPERATOR1"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(gotRawQuery, "operated_by_alias_in=") {
		t.Errorf("expected operated_by_alias_in= in query, got %q", gotRawQuery)
	}
	// Comma is url-encoded by url.Values.Encode as %2C; check both joined values appear.
	for _, want := range []string{"oidc%3Amatt%40acme.example", "slack%3AU0OPERATOR1"} {
		if !strings.Contains(gotRawQuery, want) {
			t.Errorf("expected encoded alias %q in query, got %q", want, gotRawQuery)
		}
	}
	// Regression: must not send the legacy params that the daemon ignores.
	if strings.Contains(gotRawQuery, "operated_by=") {
		t.Errorf("legacy operated_by= must not be sent, got %q", gotRawQuery)
	}
	if strings.Contains(gotRawQuery, "operated_by_is_null=") {
		t.Errorf("legacy operated_by_is_null= must not be sent, got %q", gotRawQuery)
	}
}

// TestListBeadsFiltered_OperatedByID_NotInAliasParam pins that OperatedByID
// (operators.id FK, used by /inbox) goes out as `operated_by_id=<int>` and
// is independent of the alias filter — both can be set on the same query.
func TestListBeadsFiltered_OperatedByID_NotInAliasParam(t *testing.T) {
	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListBeadsFiltered(context.Background(), ListBeadsQuery{
		OperatedByID: 42,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(gotRawQuery, "operated_by_id=42") {
		t.Errorf("expected operated_by_id=42 in query, got %q", gotRawQuery)
	}
	if strings.Contains(gotRawQuery, "operated_by_alias_in=") {
		t.Errorf("OperatedByID must not populate operated_by_alias_in, got %q", gotRawQuery)
	}
}

// --- ListAllBeadsFiltered: the paged exhaustion walk (bug ka-0000001936) ---

// TestListAllBeadsFiltered_PagesPastCapAndKeepsBeadBeyondFirstPage is the core
// regression guard for ka-0000001936: a single capped fetch drops any matching
// bead that sorts past the cap boundary. This mock serves a FULL first page
// (exactly listAllPagesPageSize rows) so the walk must follow offset to a second
// page, where an mr_url-bearing bead lives. The walk must surface it.
//
// The concrete field of the real bug: ka-0000003829 (an open task with mr_url)
// sorted past the Limit:500 boundary, so `gb mr list` never saw it. Here the
// stand-in "gd-beyond-cap" plays that role.
func TestListAllBeadsFiltered_PagesPastCapAndKeepsBeadBeyondFirstPage(t *testing.T) {
	const beyond = "gd-beyond-cap"
	var offsets []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		offsets = append(offsets, q.Get("offset"))
		off, _ := strconv.Atoi(q.Get("offset"))
		// Total is 1 more than a full page: page 0 (offset 0) returns a full
		// page, page 1 (offset=pageSize) returns the single trailing bead.
		total := listAllPagesPageSize + 1
		var beads []beadJSON
		switch off {
		case 0:
			beads = make([]beadJSON, listAllPagesPageSize)
			for i := range beads {
				beads[i] = beadJSON{ID: fmt.Sprintf("gd-p0-%d", i), Type: "task", Status: "open"}
			}
		case listAllPagesPageSize:
			beads = []beadJSON{{
				ID: beyond, Type: "task", Status: "open",
				Fields: json.RawMessage(`{"mr_url":"https://gitlab/x/-/merge_requests/9"}`),
			}}
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: total})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	res, err := c.ListAllBeadsFiltered(context.Background(), ListBeadsQuery{
		Types:    []string{"task", "bug", "feature", "epic"},
		Statuses: []string{"open", "in_progress"},
		Fields:   map[string]string{"mr_url": "*"},
	})
	if err != nil {
		t.Fatalf("ListAllBeadsFiltered: %v", err)
	}
	if got, want := len(res.Beads), listAllPagesPageSize+1; got != want {
		t.Fatalf("returned %d beads, want %d (full first page + trailing bead)", got, want)
	}
	// The bead past the cap boundary must be present — the whole point.
	var found bool
	for _, b := range res.Beads {
		if b.ID == beyond {
			found = true
			if b.Fields["mr_url"] == "" {
				t.Errorf("%s came back without its mr_url field", beyond)
			}
		}
	}
	if !found {
		t.Errorf("bead beyond the first-page cap (%s) was dropped — the exact ka-0000001936 defect", beyond)
	}
	// It must have actually paged: page 0 sends no offset param (client omits
	// offset=0), page 1 sends offset=pageSize.
	if len(offsets) < 2 || offsets[0] != "" || offsets[1] != strconv.Itoa(listAllPagesPageSize) {
		t.Errorf("expected offsets [\"\", %d, ...], got %v", listAllPagesPageSize, offsets)
	}
}

// TestListAllBeadsFiltered_ForwardsFieldPresenceAndStableSort pins that the walk
// pushes the caller's field filter server-side (so the daemon caps the RELEVANT
// population, not a superset) and pins a stable created_at sort so page
// boundaries do not skip/repeat rows mid-walk.
func TestListAllBeadsFiltered_ForwardsFieldPresenceAndStableSort(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		// Short page → single round-trip, walk exits.
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil, Total: 0})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if _, err := c.ListAllBeadsFiltered(context.Background(), ListBeadsQuery{
		Types:  []string{"task"},
		Fields: map[string]string{"mr_url": "*"},
	}); err != nil {
		t.Fatalf("ListAllBeadsFiltered: %v", err)
	}
	for _, want := range []string{
		"field=mr_url%3D%2A", // field=mr_url=* url-encoded
		"sort=created_at",
		"limit=" + strconv.Itoa(listAllPagesPageSize),
		"no_total=true", // only the last, short page's total is used
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("expected query to contain %q, got %q", want, gotQuery)
		}
	}
}

// TestListAllBeadsFiltered_EmptyLastPageTotal pins the NoTotal edge: when the
// match set is an exact multiple of the page size, the last page is empty and
// the daemon reports total -1 (not counted); the walk's Total is then the rows
// it read, never -1.
func TestListAllBeadsFiltered_EmptyLastPageTotal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		if q.Get("offset") != "" {
			_ = json.NewEncoder(w).Encode(listBeadsResponse{Total: -1})
			return
		}
		beads := make([]beadJSON, listAllPagesPageSize)
		for i := range beads {
			beads[i] = beadJSON{ID: fmt.Sprintf("gd-p0-%d", i), Type: "task", Status: "open"}
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: -1})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	res, err := c.ListAllBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"task"}})
	if err != nil {
		t.Fatalf("ListAllBeadsFiltered: %v", err)
	}
	if len(res.Beads) != listAllPagesPageSize || res.Total != listAllPagesPageSize {
		t.Fatalf("got %d beads, Total %d; want %d and %d", len(res.Beads), res.Total, listAllPagesPageSize, listAllPagesPageSize)
	}
}

// TestListBeadsResult_MayHaveMore pins the truncation test a NoTotal caller
// uses: a full uncounted page (-1) and a counted overflow may have more; a
// short page, an exact count and a nil result do not.
func TestListBeadsResult_MayHaveMore(t *testing.T) {
	two := []*BeadDetail{{ID: "a"}, {ID: "b"}}
	for _, tc := range []struct {
		name string
		res  *ListBeadsResult
		want bool
	}{
		{"nil", nil, false},
		{"uncounted full page", &ListBeadsResult{Beads: two, Total: -1}, true},
		{"counted overflow", &ListBeadsResult{Beads: two, Total: 5}, true},
		{"exact", &ListBeadsResult{Beads: two, Total: 2}, false},
		{"empty", &ListBeadsResult{}, false},
	} {
		if got := tc.res.MayHaveMore(); got != tc.want {
			t.Errorf("%s: MayHaveMore() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestListAllBeadsFiltered_RejectsCallerPaging guards the contract: the walk
// owns Limit/Offset. A caller that sets either would re-cap every page and make
// the loop misread exhaustion, so it is a hard error rather than a silent
// mis-page.
func TestListAllBeadsFiltered_RejectsCallerPaging(t *testing.T) {
	c := &Client{baseURL: "http://unused", httpClient: http.DefaultClient}
	for _, q := range []ListBeadsQuery{
		{Types: []string{"task"}, Limit: 100},
		{Types: []string{"task"}, Offset: 50},
	} {
		if _, err := c.ListAllBeadsFiltered(context.Background(), q); err == nil {
			t.Errorf("expected error for caller-set paging %+v, got nil", q)
		}
	}
}

// TestListAllBeadsFiltered_DedupesAcrossPages pins that a bead appearing on two
// pages (a concurrent insert can shift a boundary and repeat a row) is returned
// once, not double-counted.
func TestListAllBeadsFiltered_DedupesAcrossPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		off, _ := strconv.Atoi(q.Get("offset"))
		total := listAllPagesPageSize + 1
		var beads []beadJSON
		switch off {
		case 0:
			beads = make([]beadJSON, listAllPagesPageSize)
			for i := range beads {
				beads[i] = beadJSON{ID: fmt.Sprintf("gd-p0-%d", i), Type: "task", Status: "open"}
			}
		case listAllPagesPageSize:
			// Repeats the last row of page 0 (gd-p0-<last>) plus one new row.
			beads = []beadJSON{
				{ID: fmt.Sprintf("gd-p0-%d", listAllPagesPageSize-1), Type: "task", Status: "open"},
				{ID: "gd-new", Type: "task", Status: "open"},
			}
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: beads, Total: total})
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	res, err := c.ListAllBeadsFiltered(context.Background(), ListBeadsQuery{Types: []string{"task"}})
	if err != nil {
		t.Fatalf("ListAllBeadsFiltered: %v", err)
	}
	// full page + 1 genuinely-new row; the repeated row is dropped.
	if got, want := len(res.Beads), listAllPagesPageSize+1; got != want {
		t.Fatalf("returned %d beads, want %d (dedup should drop the repeated row)", got, want)
	}
	seen := map[string]int{}
	for _, b := range res.Beads {
		seen[b.ID]++
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("bead %s returned %d times, want 1", id, n)
		}
	}
}
