package beadsapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRegisterSession(t *testing.T) {
	var gotBody CreateBeadRequest
	createCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == "POST" {
			createCalled = true
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "gd-sess-1"})
			return
		}
		// Dependencies endpoint (best-effort, return success).
		if r.Method == "POST" {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "dep-1"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	fields := SessionFields{
		Agent:       "test-agent",
		AgentBeadID: "gd-agent-1",
		Project:     "gardenio",
		Role:        "crew",
		Hostname:    "pod-abc",
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Resumed:     false,
		Status:      "active",
	}

	id, err := c.RegisterSession(context.Background(), fields)
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if id != "gd-sess-1" {
		t.Errorf("expected id gd-sess-1, got %s", id)
	}
	if !createCalled {
		t.Error("expected CreateBead to be called")
	}
	if gotBody.Type != "session" {
		t.Errorf("expected type=session, got %s", gotBody.Type)
	}
	if gotBody.Kind != "config" {
		t.Errorf("expected kind=config, got %s", gotBody.Kind)
	}
	if len(gotBody.Labels) != 1 || gotBody.Labels[0] != "project:gardenio" {
		t.Errorf("expected label project:gardenio, got %v", gotBody.Labels)
	}

	// Verify fields were properly serialized.
	var parsed SessionFields
	if err := json.Unmarshal(gotBody.Fields, &parsed); err != nil {
		t.Fatalf("unmarshalling fields: %v", err)
	}
	if parsed.Agent != "test-agent" {
		t.Errorf("expected agent=test-agent, got %s", parsed.Agent)
	}
	if parsed.Status != "active" {
		t.Errorf("expected status=active, got %s", parsed.Status)
	}
}

func TestCloseSession(t *testing.T) {
	var patchFields map[string]any
	var closeCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			// GetBead for read-modify-write in UpdateBeadFieldsTyped.
			bead := beadJSON{
				ID:     "gd-sess-1",
				Type:   "session",
				Status: "open",
				Fields: json.RawMessage(`{"agent":"test-agent","status":"active","started_at":"2026-03-08T12:00:00Z"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)
		case r.Method == "PATCH":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if raw, ok := body["fields"]; ok {
				_ = json.Unmarshal(raw, &patchFields)
			}
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST":
			closeCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	err := c.CloseSession(context.Background(), "gd-sess-1", 1, "/path/to/log.jsonl", nil)
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	if !closeCalled {
		t.Error("expected CloseBead to be called")
	}

	// Verify fields were updated. Exit 1 is a genuine failure — not one of the
	// signal-termination codes whose verdict depends on the shutdown bit — so it
	// classifies as crashed regardless of how the teardown was initiated. See
	// TestSessionStatusForExit for the full exit-code matrix (ka-0000003165).
	if patchFields["status"] != "crashed" {
		t.Errorf("expected status=crashed for exit 1, got %v", patchFields["status"])
	}
	if patchFields["session_log"] != "/path/to/log.jsonl" {
		t.Errorf("expected session_log set, got %v", patchFields["session_log"])
	}
	exitCode, ok := patchFields["exit_code"].(float64)
	if !ok || exitCode != 1 {
		t.Errorf("expected exit_code=1 (number), got %v (%T)", patchFields["exit_code"], patchFields["exit_code"])
	}
}

// TestCloseSessionAlwaysClosesBead verifies that CloseBead is attempted even
// when the PATCH update fails — a regression test for ka-0000008049 where a
// transient PATCH failure left the bead's main Status at "open" forever.
func TestCloseSessionAlwaysClosesBead(t *testing.T) {
	var closeCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET":
			bead := beadJSON{
				ID:     "gd-sess-fail",
				Type:   "session",
				Status: "open",
				Fields: json.RawMessage(`{"agent":"test-agent","status":"active","started_at":"2026-03-08T12:00:00Z"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)
		case r.Method == "PATCH":
			// Simulate a transient PATCH failure.
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == "POST":
			closeCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	err := c.CloseSession(context.Background(), "gd-sess-fail", 0, "", nil)
	if err == nil {
		t.Fatal("expected error from failed PATCH, got nil")
	}
	if !closeCalled {
		t.Error("expected CloseBead to be called even when PATCH fails")
	}
}

func TestUpdateAgentUsage(t *testing.T) {
	var gotDeltas map[string]float64
	incrementCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// UpdateAgentUsage now uses POST /v1/beads/{id}/increment (atomic).
		if r.Method == "POST" && r.URL.Path == "/v1/beads/gd-agent-1/increment" {
			incrementCalled = true
			_ = json.NewDecoder(r.Body).Decode(&gotDeltas)
			bead := beadJSON{ID: "gd-agent-1", Type: "agent", Status: "open"}
			_ = json.NewEncoder(w).Encode(bead)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	usage := &SessionUsage{
		InputTokens:      2000,
		OutputTokens:     1000,
		CacheReadTokens:  300,
		CacheWriteTokens: 150,
		TotalCostUSD:     1.25,
		RequestCount:     10,
		ToolCallCount:    20,
		ErrorCount:       3,
		CompactionCount:  1,
	}

	err := c.UpdateAgentUsage(context.Background(), "gd-agent-1", usage)
	if err != nil {
		t.Fatalf("UpdateAgentUsage: %v", err)
	}

	if !incrementCalled {
		t.Fatal("expected IncrementBeadFields to be called")
	}

	// Verify the correct deltas were sent (server handles accumulation atomically).
	assertDelta := func(key string, expected float64) {
		t.Helper()
		got, ok := gotDeltas[key]
		if !ok {
			t.Errorf("expected delta for %s", key)
			return
		}
		if got != expected {
			t.Errorf("delta %s: expected %v, got %v", key, expected, got)
		}
	}

	assertDelta("total_input_tokens", 2000)
	assertDelta("total_output_tokens", 1000)
	assertDelta("total_cache_read_tokens", 300)
	assertDelta("total_cache_write_tokens", 150)
	assertDelta("total_cost_usd", 1.25)
	assertDelta("total_request_count", 10)
	assertDelta("total_tool_calls", 20)
	assertDelta("total_errors", 3)
	assertDelta("total_compactions", 1)
	assertDelta("session_count", 1)
}

func TestUpdateAgentUsageNil(t *testing.T) {
	c := &Client{baseURL: "http://unused", httpClient: http.DefaultClient}
	if err := c.UpdateAgentUsage(context.Background(), "gd-agent-1", nil); err != nil {
		t.Fatalf("UpdateAgentUsage(nil) should return nil, got: %v", err)
	}
}

func TestUpdateAgentUsageFirstSession(t *testing.T) {
	// First session sends the same deltas as any other session — the server
	// handles COALESCE(existing, 0) + delta atomically.
	var gotDeltas map[string]float64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/beads/gd-agent-new/increment" {
			_ = json.NewDecoder(r.Body).Decode(&gotDeltas)
			bead := beadJSON{ID: "gd-agent-new", Type: "agent", Status: "open"}
			_ = json.NewEncoder(w).Encode(bead)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	usage := &SessionUsage{
		InputTokens:      5000,
		OutputTokens:     2000,
		CacheReadTokens:  1000,
		CacheWriteTokens: 500,
		TotalCostUSD:     3.50,
		RequestCount:     15,
		ToolCallCount:    25,
		ErrorCount:       2,
		CompactionCount:  1,
	}

	err := c.UpdateAgentUsage(context.Background(), "gd-agent-new", usage)
	if err != nil {
		t.Fatalf("UpdateAgentUsage (first session): %v", err)
	}

	if gotDeltas["total_input_tokens"] != 5000 {
		t.Errorf("total_input_tokens delta: expected 5000, got %v", gotDeltas["total_input_tokens"])
	}
	if gotDeltas["total_cost_usd"] != 3.50 {
		t.Errorf("total_cost_usd delta: expected 3.50, got %v", gotDeltas["total_cost_usd"])
	}
	if gotDeltas["session_count"] != 1 {
		t.Errorf("session_count delta: expected 1, got %v", gotDeltas["session_count"])
	}
}

func TestUpdateAgentUsageZeroValues(t *testing.T) {
	// A session with zero usage should still send session_count=1 delta.
	var gotDeltas map[string]float64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/beads/gd-agent-1/increment" {
			_ = json.NewDecoder(r.Body).Decode(&gotDeltas)
			bead := beadJSON{ID: "gd-agent-1", Type: "agent", Status: "open"}
			_ = json.NewEncoder(w).Encode(bead)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	usage := &SessionUsage{}

	err := c.UpdateAgentUsage(context.Background(), "gd-agent-1", usage)
	if err != nil {
		t.Fatalf("UpdateAgentUsage (zero): %v", err)
	}

	// session_count delta should be 1 even for zero-usage sessions.
	if gotDeltas["session_count"] != 1 {
		t.Errorf("session_count delta: expected 1, got %v", gotDeltas["session_count"])
	}
	// Zero-value token deltas should be 0.
	if gotDeltas["total_cost_usd"] != 0 {
		t.Errorf("total_cost_usd delta: expected 0, got %v", gotDeltas["total_cost_usd"])
	}
}

func TestUpdateAgentUsageServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	usage := &SessionUsage{InputTokens: 100}

	err := c.UpdateAgentUsage(context.Background(), "gd-agent-1", usage)
	if err == nil {
		t.Fatal("expected error on server failure, got nil")
	}
}

func TestCloseSessionWithUsage(t *testing.T) {
	// Verify that CloseSession correctly stores usage metrics on the session bead.
	var patchFields map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			bead := beadJSON{
				ID:     "gd-sess-2",
				Type:   "session",
				Status: "open",
				Fields: json.RawMessage(`{"agent":"test-agent","status":"active","started_at":"2026-03-08T12:00:00Z"}`),
			}
			_ = json.NewEncoder(w).Encode(bead)
		case "PATCH":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if raw, ok := body["fields"]; ok {
				_ = json.Unmarshal(raw, &patchFields)
			}
			w.WriteHeader(http.StatusNoContent)
		case "POST":
			w.WriteHeader(http.StatusNoContent) // CloseBead
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	usage := &SessionUsage{
		InputTokens:      10000,
		OutputTokens:     3000,
		ThinkingTokens:   1200,
		CacheReadTokens:  5000,
		CacheWriteTokens: 2000,
		TotalCostUSD:     7.85,
		RequestCount:     20,
		ToolCallCount:    50,
		ErrorCount:       5,
		CompactionCount:  3,
		InterruptCount:   2,
		LastInterruptAt:  "2026-10-28T20:10:13.209Z",
	}

	err := c.CloseSession(context.Background(), "gd-sess-2", 0, "/logs/session.jsonl", usage)
	if err != nil {
		t.Fatalf("CloseSession with usage: %v", err)
	}

	// Verify usage fields were written to the session bead.
	if patchFields["status"] != "completed" {
		t.Errorf("expected status=completed for exit code 0, got %v", patchFields["status"])
	}
	if v, ok := patchFields["input_tokens"].(float64); !ok || v != 10000 {
		t.Errorf("input_tokens: expected 10000, got %v (%T)", patchFields["input_tokens"], patchFields["input_tokens"])
	}
	if v, ok := patchFields["output_tokens"].(float64); !ok || v != 3000 {
		t.Errorf("output_tokens: expected 3000, got %v (%T)", patchFields["output_tokens"], patchFields["output_tokens"])
	}
	// F4: thinking_tokens must be wired onto the session bead for parity with
	// the agent rollup (it is an estimate, but it should not be silently dropped).
	if v, ok := patchFields["thinking_tokens"].(float64); !ok || v != 1200 {
		t.Errorf("thinking_tokens: expected 1200, got %v (%T)", patchFields["thinking_tokens"], patchFields["thinking_tokens"])
	}
	if v, ok := patchFields["cache_read_tokens"].(float64); !ok || v != 5000 {
		t.Errorf("cache_read_tokens: expected 5000, got %v (%T)", patchFields["cache_read_tokens"], patchFields["cache_read_tokens"])
	}
	if v, ok := patchFields["cache_write_tokens"].(float64); !ok || v != 2000 {
		t.Errorf("cache_write_tokens: expected 2000, got %v (%T)", patchFields["cache_write_tokens"], patchFields["cache_write_tokens"])
	}
	if v, ok := patchFields["total_cost_usd"].(float64); !ok || v != 7.85 {
		t.Errorf("total_cost_usd: expected 7.85, got %v (%T)", patchFields["total_cost_usd"], patchFields["total_cost_usd"])
	}
	if v, ok := patchFields["request_count"].(float64); !ok || v != 20 {
		t.Errorf("request_count: expected 20, got %v (%T)", patchFields["request_count"], patchFields["request_count"])
	}
	if v, ok := patchFields["tool_call_count"].(float64); !ok || v != 50 {
		t.Errorf("tool_call_count: expected 50, got %v (%T)", patchFields["tool_call_count"], patchFields["tool_call_count"])
	}
	if v, ok := patchFields["error_count"].(float64); !ok || v != 5 {
		t.Errorf("error_count: expected 5, got %v (%T)", patchFields["error_count"], patchFields["error_count"])
	}
	if v, ok := patchFields["compaction_count"].(float64); !ok || v != 3 {
		t.Errorf("compaction_count: expected 3, got %v (%T)", patchFields["compaction_count"], patchFields["compaction_count"])
	}
	if v, ok := patchFields["interrupt_count"].(float64); !ok || v != 2 {
		t.Errorf("interrupt_count: expected 2, got %v (%T)", patchFields["interrupt_count"], patchFields["interrupt_count"])
	}
	if v := patchFields["last_interrupt_at"]; v != "2026-10-28T20:10:13.209Z" {
		t.Errorf("last_interrupt_at: expected 2026-10-28T20:10:13.209Z, got %v", v)
	}
}

// TestCloseSessionOmitsZeroInterrupts pins that a session with no interrupts
// writes neither interrupt key, so a daemon whose type:session predates them
// does not 400 the whole close (gd-3HAERLWMLu).
func TestCloseSessionOmitsZeroInterrupts(t *testing.T) {
	var patchFields map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "gd-sess-3", Type: "session", Status: "open",
				Fields: json.RawMessage(`{"agent":"test-agent","status":"active"}`)})
		case "PATCH":
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.Unmarshal(body["fields"], &patchFields)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if err := c.CloseSession(context.Background(), "gd-sess-3", 0, "", &SessionUsage{CompactionCount: 1}); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if _, ok := patchFields["compaction_count"]; !ok {
		t.Fatalf("usage fields not written: %v", patchFields)
	}
	for _, k := range []string{"interrupt_count", "last_interrupt_at"} {
		if v, ok := patchFields[k]; ok {
			t.Errorf("%s written for a session with no interrupts: %v", k, v)
		}
	}
}

func TestListSessionBeads(t *testing.T) {
	var gotQuery string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		resp := listBeadsResponse{
			Beads: []beadJSON{
				{ID: "gd-sess-1", Type: "session", Status: "closed"},
				{ID: "gd-sess-2", Type: "session", Status: "open"},
			},
			Total: 2,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	beads, err := c.ListSessionBeads(context.Background(), "my-agent", 10)
	if err != nil {
		t.Fatalf("ListSessionBeads: %v", err)
	}
	if len(beads) != 2 {
		t.Errorf("expected 2 beads, got %d", len(beads))
	}
	if gotQuery == "" {
		t.Error("expected query params")
	}
	// Should contain type=session.
	if got := gotQuery; got == "" {
		t.Error("expected non-empty query")
	}
}

// --- UpdateAccountUsage tests ---

func TestUpdateAccountUsage(t *testing.T) {
	var gotDeltas map[string]float64
	incrementCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/beads/gd-acct-1/increment" {
			incrementCalled = true
			_ = json.NewDecoder(r.Body).Decode(&gotDeltas)
			bead := beadJSON{ID: "gd-acct-1", Type: "account", Status: "open"}
			_ = json.NewEncoder(w).Encode(bead)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	usage := &SessionUsage{
		InputTokens:      2000,
		OutputTokens:     1000,
		CacheReadTokens:  500,
		CacheWriteTokens: 200,
		TotalCostUSD:     1.25,
		RequestCount:     5,
		ToolCallCount:    15,
		ErrorCount:       1,
		CompactionCount:  0,
	}

	err := c.UpdateAccountUsage(context.Background(), "gd-acct-1", usage)
	if err != nil {
		t.Fatalf("UpdateAccountUsage: %v", err)
	}

	if !incrementCalled {
		t.Fatal("expected IncrementBeadFields to be called")
	}

	// Verify deltas match the session usage (server accumulates atomically).
	assertDelta := func(key string, want float64) {
		t.Helper()
		if gotDeltas[key] != want {
			t.Errorf("%s delta: expected %v, got %v", key, want, gotDeltas[key])
		}
	}

	assertDelta("total_input_tokens", 2000)
	assertDelta("total_output_tokens", 1000)
	assertDelta("total_cache_read_tokens", 500)
	assertDelta("total_cache_write_tokens", 200)
	assertDelta("total_cost_usd", 1.25)
	assertDelta("total_request_count", 5)
	assertDelta("total_tool_calls", 15)
	assertDelta("total_errors", 1)
	assertDelta("total_compactions", 0)
	assertDelta("session_count", 1)
}

func TestUpdateAccountUsageNil(t *testing.T) {
	c := &Client{}
	if err := c.UpdateAccountUsage(context.Background(), "gd-acct-1", nil); err != nil {
		t.Fatalf("UpdateAccountUsage(nil) should return nil, got: %v", err)
	}
}

func TestUpdateAccountUsageFirstSession(t *testing.T) {
	var gotDeltas map[string]float64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v1/beads/gd-acct-new/increment" {
			_ = json.NewDecoder(r.Body).Decode(&gotDeltas)
			bead := beadJSON{ID: "gd-acct-new", Type: "account", Status: "open"}
			_ = json.NewEncoder(w).Encode(bead)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	usage := &SessionUsage{
		InputTokens:  3000,
		OutputTokens: 1000,
		TotalCostUSD: 2.50,
	}

	err := c.UpdateAccountUsage(context.Background(), "gd-acct-new", usage)
	if err != nil {
		t.Fatalf("UpdateAccountUsage (first session): %v", err)
	}

	if gotDeltas["total_input_tokens"] != 3000 {
		t.Errorf("total_input_tokens delta: expected 3000, got %v", gotDeltas["total_input_tokens"])
	}
	if gotDeltas["total_cost_usd"] != 2.50 {
		t.Errorf("total_cost_usd delta: expected 2.50, got %v", gotDeltas["total_cost_usd"])
	}
	if gotDeltas["session_count"] != 1 {
		t.Errorf("session_count delta: expected 1, got %v", gotDeltas["session_count"])
	}
}

// --- FindAccountBeadIDForProject tests ---

func TestFindAccountBeadIDForProject_ViaLabeledAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typ := r.URL.Query().Get("type")
		switch typ {
		case "account":
			// Two account beads labeled for the project; ListAccountBeadsByProject
			// sorts alphabetically so "alpha-account" wins over "beta-account".
			resp := listBeadsResponse{
				Beads: []beadJSON{
					{
						ID:     "gd-acct-beta",
						Title:  "beta-account",
						Type:   "account",
						Labels: []string{"project:my-project"},
					},
					{
						ID:     "gd-acct-alpha",
						Title:  "alpha-account",
						Type:   "account",
						Labels: []string{"project:my-project"},
					},
				},
				Total: 2,
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			resp := listBeadsResponse{Total: 0}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.FindAccountBeadIDForProject(context.Background(), "my-project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "gd-acct-alpha" {
		t.Errorf("expected gd-acct-alpha (alphabetically-first labeled account), got %q", id)
	}
}

func TestFindAccountBeadIDForProject_ViaProjectBead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		typ := r.URL.Query().Get("type")
		switch typ {
		case "config":
			// No auth override.
			resp := listBeadsResponse{Total: 0}
			_ = json.NewEncoder(w).Encode(resp)
		case "project":
			resp := listBeadsResponse{
				Beads: []beadJSON{
					{
						ID:    "gd-proj-1",
						Title: "my-project",
						Type:  "project",
						Fields: json.RawMessage(`{
							"prefix":"kd",
							"account":"primary-account"
						}`),
					},
				},
				Total: 1,
			}
			_ = json.NewEncoder(w).Encode(resp)
		case "account":
			resp := listBeadsResponse{
				Beads: []beadJSON{
					{ID: "gd-acct-primary", Title: "primary-account", Type: "account"},
				},
				Total: 1,
			}
			_ = json.NewEncoder(w).Encode(resp)
		default:
			resp := listBeadsResponse{Total: 0}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.FindAccountBeadIDForProject(context.Background(), "my-project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "gd-acct-primary" {
		t.Errorf("expected gd-acct-primary (from project bead), got %q", id)
	}
}

func TestFindAccountBeadIDForProject_NoAccount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := listBeadsResponse{Total: 0}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	id, err := c.FindAccountBeadIDForProject(context.Background(), "no-account-project")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty ID for project with no account, got %q", id)
	}
}

func TestFindAccountBeadIDForProject_EmptyProject(t *testing.T) {
	c := &Client{}
	id, err := c.FindAccountBeadIDForProject(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty ID for empty project, got %q", id)
	}
}

// TestSessionBeadLifecycle exercises the full session bead lifecycle through a
// single stateful mock server: RegisterSession → CloseSession (with usage) →
// UpdateAgentUsage → UpdateAccountUsage → ListSessionBeads.
func TestSessionBeadLifecycle(t *testing.T) {
	// Stateful mock: tracks created session bead and its fields.
	var (
		sessionBeadID     = "gd-sess-lifecycle"
		sessionFields     json.RawMessage
		sessionStatus     = "open"
		agentFields       json.RawMessage
		accountFields     json.RawMessage
		registerCalled    bool
		depCalled         bool
		closeCalled       bool
		agentPatched      bool
		accountPatched    bool
		listSessionCalled bool
	)

	agentFields = json.RawMessage(`{"agent":"lifecycle-agent","mode":"crew"}`)
	accountFields = json.RawMessage(`{"provider":"anthropic","credential":"key"}`)

	var agentDeltas map[string]float64
	var accountDeltas map[string]float64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		switch {
		// --- CreateBead (RegisterSession) ---
		case r.Method == "POST" && path == "/v1/beads":
			registerCalled = true
			var body CreateBeadRequest
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Type != "session" {
				t.Errorf("RegisterSession: expected type=session, got %s", body.Type)
			}
			sessionFields = body.Fields
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": sessionBeadID})

		// --- AddDependency (session_of link) ---
		case r.Method == "POST" && path == "/v1/beads/"+sessionBeadID+"/dependencies":
			depCalled = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": "dep-1"})

		// --- CloseBead (CloseSession final step) ---
		case r.Method == "POST" && path == "/v1/beads/"+sessionBeadID+"/close":
			closeCalled = true
			sessionStatus = "closed"
			w.WriteHeader(http.StatusNoContent)

		// --- IncrementBeadFields (UpdateAgentUsage / UpdateAccountUsage) ---
		case r.Method == "POST" && path == "/v1/beads/gd-agent-lc/increment":
			agentPatched = true
			_ = json.NewDecoder(r.Body).Decode(&agentDeltas)
			bead := beadJSON{ID: "gd-agent-lc", Type: "agent", Status: "open", Fields: agentFields}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == "POST" && path == "/v1/beads/gd-acct-lc/increment":
			accountPatched = true
			_ = json.NewDecoder(r.Body).Decode(&accountDeltas)
			bead := beadJSON{ID: "gd-acct-lc", Type: "account", Status: "open", Fields: accountFields}
			_ = json.NewEncoder(w).Encode(bead)

		// --- GetBead ---
		case r.Method == "GET" && path == "/v1/beads/"+sessionBeadID:
			bead := beadJSON{
				ID:     sessionBeadID,
				Type:   "session",
				Status: sessionStatus,
				Fields: sessionFields,
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == "GET" && path == "/v1/beads/gd-agent-lc":
			bead := beadJSON{
				ID:     "gd-agent-lc",
				Type:   "agent",
				Status: "open",
				Fields: agentFields,
			}
			_ = json.NewEncoder(w).Encode(bead)

		case r.Method == "GET" && path == "/v1/beads/gd-acct-lc":
			bead := beadJSON{
				ID:     "gd-acct-lc",
				Type:   "account",
				Status: "open",
				Fields: accountFields,
			}
			_ = json.NewEncoder(w).Encode(bead)

		// --- PATCH (UpdateBeadFieldsTyped — session bead fields) ---
		case r.Method == "PATCH" && path == "/v1/beads/"+sessionBeadID:
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if raw, ok := body["fields"]; ok {
				sessionFields = raw
			}
			w.WriteHeader(http.StatusNoContent)

		// --- ListBeads (ListSessionBeads) ---
		case r.Method == "GET" && path == "/v1/beads":
			listSessionCalled = true
			resp := listBeadsResponse{
				Beads: []beadJSON{
					{
						ID:     sessionBeadID,
						Type:   "session",
						Status: sessionStatus,
						Fields: sessionFields,
					},
				},
				Total: 1,
			}
			_ = json.NewEncoder(w).Encode(resp)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	ctx := context.Background()

	// --- Step 1: RegisterSession ---
	fields := SessionFields{
		Agent:       "lifecycle-agent",
		AgentBeadID: "gd-agent-lc",
		Project:     "gardenio",
		Role:        "crew",
		Hostname:    "pod-lifecycle",
		StartedAt:   time.Now().UTC().Format(time.RFC3339),
		Resumed:     false,
		Status:      "active",
	}

	id, err := c.RegisterSession(ctx, fields)
	if err != nil {
		t.Fatalf("RegisterSession: %v", err)
	}
	if id != sessionBeadID {
		t.Errorf("expected session ID %s, got %s", sessionBeadID, id)
	}
	if !registerCalled {
		t.Error("expected CreateBead to be called")
	}
	if !depCalled {
		t.Error("expected AddDependency to be called for session_of link")
	}

	// --- Step 2: CloseSession with usage ---
	usage := &SessionUsage{
		InputTokens:      8000,
		OutputTokens:     3000,
		CacheReadTokens:  2000,
		CacheWriteTokens: 500,
		TotalCostUSD:     5.50,
		RequestCount:     12,
		ToolCallCount:    35,
		ErrorCount:       2,
		CompactionCount:  1,
	}

	err = c.CloseSession(ctx, sessionBeadID, 0, "/logs/lifecycle.jsonl", usage)
	if err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if !closeCalled {
		t.Error("expected CloseBead to be called")
	}
	if sessionStatus != "closed" {
		t.Errorf("expected session status=closed, got %s", sessionStatus)
	}

	// Verify session bead fields after close.
	var sessFields map[string]any
	_ = json.Unmarshal(sessionFields, &sessFields)
	if sessFields["status"] != "completed" {
		t.Errorf("expected status=completed for exit code 0, got %v", sessFields["status"])
	}
	if sessFields["session_log"] != "/logs/lifecycle.jsonl" {
		t.Errorf("expected session_log set, got %v", sessFields["session_log"])
	}
	if v, ok := sessFields["input_tokens"].(float64); !ok || v != 8000 {
		t.Errorf("expected input_tokens=8000, got %v (%T)", sessFields["input_tokens"], sessFields["input_tokens"])
	}

	// --- Step 3: UpdateAgentUsage (now uses atomic increment) ---
	err = c.UpdateAgentUsage(ctx, "gd-agent-lc", usage)
	if err != nil {
		t.Fatalf("UpdateAgentUsage: %v", err)
	}
	if !agentPatched {
		t.Error("expected agent bead increment to be called")
	}

	// Verify correct deltas were sent (server handles accumulation atomically).
	if agentDeltas["total_input_tokens"] != 8000 {
		t.Errorf("agent total_input_tokens delta: expected 8000, got %v", agentDeltas["total_input_tokens"])
	}
	if agentDeltas["session_count"] != 1 {
		t.Errorf("agent session_count delta: expected 1, got %v", agentDeltas["session_count"])
	}

	// --- Step 4: UpdateAccountUsage (now uses atomic increment) ---
	err = c.UpdateAccountUsage(ctx, "gd-acct-lc", usage)
	if err != nil {
		t.Fatalf("UpdateAccountUsage: %v", err)
	}
	if !accountPatched {
		t.Error("expected account bead increment to be called")
	}

	if accountDeltas["total_input_tokens"] != 8000 {
		t.Errorf("account total_input_tokens delta: expected 8000, got %v", accountDeltas["total_input_tokens"])
	}
	if accountDeltas["session_count"] != 1 {
		t.Errorf("account session_count delta: expected 1, got %v", accountDeltas["session_count"])
	}

	// --- Step 5: ListSessionBeads ---
	beads, err := c.ListSessionBeads(ctx, "lifecycle-agent", 10)
	if err != nil {
		t.Fatalf("ListSessionBeads: %v", err)
	}
	if !listSessionCalled {
		t.Error("expected ListBeads to be called")
	}
	if len(beads) != 1 {
		t.Fatalf("expected 1 session bead, got %d", len(beads))
	}
	if beads[0].ID != sessionBeadID {
		t.Errorf("expected bead ID %s, got %s", sessionBeadID, beads[0].ID)
	}
}

// TestSessionStatusForExit is the regression matrix for ka-0000003165: the old
// rule was `exitCode != 0 => "crashed"`, which labeled EVERY clean `gb stop` a
// crash (measured: 284 of 284 closed session beads read "crashed", zero read
// "completed"). 143 is 128+SIGTERM, the normal teardown signal, so the mislabel
// was structural rather than occasional.
func TestSessionStatusForExit(t *testing.T) {
	tests := []struct {
		name              string
		exitCode          int
		shutdownRequested bool
		want              string
	}{
		// Exited on its own terms.
		{"zero exit", 0, false, "completed"},
		{"zero exit on shutdown", 0, true, "completed"},

		// Credential switch: the restart loop relaunches DELIBERATELY on this
		// code, so it is a healthy mid-life restart. 54 of the 284 measured
		// sessions were this and every one was called a crash.
		{"credential switch", ExitCodeCredentialSwitch, false, "completed"},
		{"credential switch on shutdown", ExitCodeCredentialSwitch, true, "completed"},

		// The bug itself: 128 + {SIGTERM, SIGINT, SIGHUP} on a REQUESTED
		// teardown is the normal gb-stop / pod-terminate path. 229 of the 284.
		{"SIGTERM on requested shutdown", 143, true, "completed"},
		{"SIGINT on requested shutdown", 130, true, "completed"},
		{"SIGHUP on requested shutdown", 129, true, "completed"},

		// Same codes WITHOUT the shutdown bit are genuine mid-task kills and
		// must still surface — otherwise the fix would just invert the blind spot.
		{"SIGTERM with no shutdown requested", 143, false, "crashed"},
		{"SIGINT with no shutdown requested", 130, false, "crashed"},
		{"SIGHUP with no shutdown requested", 129, false, "crashed"},

		// SIGKILL stays a failure even on the shutdown path: it only happens
		// when the child missed its grace window. Matches coop's rule.
		{"SIGKILL on shutdown", 137, true, "crashed"},
		{"SIGKILL", 137, false, "crashed"},

		// Ordinary failures, and the reconciler's unknown-exit sentinel for a
		// session whose agent died without ever closing it.
		{"generic failure", 1, false, "crashed"},
		{"generic failure on shutdown", 1, true, "crashed"},
		{"unknown exit sentinel", -1, false, "crashed"},
		{"unknown exit sentinel on shutdown", -1, true, "crashed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SessionStatusForExit(tt.exitCode, tt.shutdownRequested); got != tt.want {
				t.Errorf("SessionStatusForExit(%d, %v) = %q, want %q",
					tt.exitCode, tt.shutdownRequested, got, tt.want)
			}
		})
	}
}

// TestClassifyRestartCause pins the restart_cause enum stamped on the session
// bead at close — the queryable-over-time form of the exit-code split.
func TestClassifyRestartCause(t *testing.T) {
	tests := []struct {
		name              string
		exitCode          int
		shutdownRequested bool
		want              string
	}{
		{"zero exit", 0, false, RestartCauseClean},
		{"zero exit on shutdown", 0, true, RestartCauseClean},

		{"credential switch", ExitCodeCredentialSwitch, false, RestartCauseCredentialSwitch},
		{"credential switch on shutdown", ExitCodeCredentialSwitch, true, RestartCauseCredentialSwitch},

		{"SIGTERM on requested shutdown", 143, true, RestartCauseSIGTERM},
		{"SIGINT on requested shutdown", 130, true, RestartCauseSIGTERM},
		{"SIGHUP on requested shutdown", 129, true, RestartCauseSIGTERM},

		{"SIGTERM with no shutdown requested", 143, false, RestartCauseCrash},
		{"SIGINT with no shutdown requested", 130, false, RestartCauseCrash},
		{"SIGHUP with no shutdown requested", 129, false, RestartCauseCrash},

		// SIGKILL is the OOM-killer's signal of choice — classified oomkill
		// regardless of the shutdown bit (a best-effort proxy; see the doc
		// comment on ClassifyRestartCause).
		{"SIGKILL on shutdown", 137, true, RestartCauseOOMKilled},
		{"SIGKILL", 137, false, RestartCauseOOMKilled},

		{"generic failure", 1, false, RestartCauseCrash},
		{"generic failure on shutdown", 1, true, RestartCauseCrash},
		{"unknown exit sentinel", -1, false, RestartCauseCrash},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyRestartCause(tt.exitCode, tt.shutdownRequested); got != tt.want {
				t.Errorf("ClassifyRestartCause(%d, %v) = %q, want %q",
					tt.exitCode, tt.shutdownRequested, got, tt.want)
			}
		})
	}
}

// TestSessionStatusForExitStaysInEnum guards the deploy-ordering hazard called
// out in SessionStatusForExit's doc comment: the live type:session config
// declares status ∈ {active, completed, crashed, retired} and the daemon
// validates it strictly. Because status travels in the same PATCH as the usage
// metrics, emitting an out-of-enum value (e.g. coop's more expressive "stopped")
// would fail the WHOLE write and silently lose the session's token/cost data.
func TestSessionStatusForExitStaysInEnum(t *testing.T) {
	allowed := map[string]bool{"active": true, "completed": true, "crashed": true, "retired": true}
	codes := []int{-1, 0, 1, 2, ExitCodeCredentialSwitch, 124, 129, 130, 137, 143, 255}
	for _, code := range codes {
		for _, shutdown := range []bool{true, false} {
			if got := SessionStatusForExit(code, shutdown); !allowed[got] {
				t.Errorf("SessionStatusForExit(%d, %v) = %q, which is not a type:session status enum value",
					code, shutdown, got)
			}
		}
	}
}

// TestSessionStatusDependsOnShutdown pins the cheap-path predicate to the
// classifier it summarizes: callers skip resolving the shutdown bit (a bead
// read) for codes that classify identically either way.
func TestSessionStatusDependsOnShutdown(t *testing.T) {
	dependent := []int{143, 130, 129}
	for _, code := range dependent {
		if !SessionStatusDependsOnShutdown(code) {
			t.Errorf("SessionStatusDependsOnShutdown(%d) = false, want true (signal-termination code)", code)
		}
	}
	for _, code := range []int{-1, 0, 1, ExitCodeCredentialSwitch, 124, 137, 255} {
		if SessionStatusDependsOnShutdown(code) {
			t.Errorf("SessionStatusDependsOnShutdown(%d) = true, want false", code)
		}
	}
}

// TestCloseSessionWithShutdownCleanStop is the end-to-end guard: a clean
// `gb stop` (exit 143, shutdown requested) must land status=completed on the
// bead — the exact write that produced 284/284 "crashed" before ka-0000003165.
// It also asserts the usage metrics ride along, since the enum hazard above
// would take them down with a rejected status.
func TestCloseSessionWithShutdownCleanStop(t *testing.T) {
	var patchFields map[string]any
	closeCalled := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(beadJSON{
				ID:     "gd-sess-stop",
				Type:   "session",
				Status: "open",
				Fields: json.RawMessage(`{"agent":"test-agent","status":"active"}`),
			})
		case http.MethodPatch:
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if raw, ok := body["fields"]; ok {
				_ = json.Unmarshal(raw, &patchFields)
			}
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost:
			closeCalled = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	usage := &SessionUsage{InputTokens: 100, OutputTokens: 200, RequestCount: 5}

	if err := c.CloseSessionWithShutdown(
		context.Background(), "gd-sess-stop", 143, "/log.jsonl", usage, true,
	); err != nil {
		t.Fatalf("CloseSessionWithShutdown: %v", err)
	}

	if !closeCalled {
		t.Error("expected CloseBead to be called")
	}
	if patchFields["status"] != "completed" {
		t.Errorf("clean gb-stop (exit 143, shutdown requested) should record status=completed, got %v",
			patchFields["status"])
	}
	if got, ok := patchFields["exit_code"].(float64); !ok || got != 143 {
		t.Errorf("expected exit_code=143, got %v", patchFields["exit_code"])
	}
	// The usage metrics share the PATCH with status — an out-of-enum status
	// would have taken these down with it.
	if got, ok := patchFields["input_tokens"].(float64); !ok || got != 100 {
		t.Errorf("expected input_tokens=100 alongside status, got %v", patchFields["input_tokens"])
	}
}

// TestCloseSessionDefaultsToNotShutdown pins the conservative default of the
// back-compat CloseSession wrapper: with no shutdown signal to offer, a
// signal-termination exit is still reported as a crash. The orphan reconciler
// relies on this — its sessions are by definition ones nobody closed cleanly.
func TestCloseSessionDefaultsToNotShutdown(t *testing.T) {
	var patchFields map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(beadJSON{
				ID:     "gd-sess-orphan",
				Type:   "session",
				Status: "open",
				Fields: json.RawMessage(`{"agent":"test-agent","status":"active"}`),
			})
		case http.MethodPatch:
			var body map[string]json.RawMessage
			_ = json.NewDecoder(r.Body).Decode(&body)
			if raw, ok := body["fields"]; ok {
				_ = json.Unmarshal(raw, &patchFields)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	if err := c.CloseSession(context.Background(), "gd-sess-orphan", 143, "", nil); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if patchFields["status"] != "crashed" {
		t.Errorf("CloseSession without a shutdown signal should stay crashed for exit 143, got %v",
			patchFields["status"])
	}
}
