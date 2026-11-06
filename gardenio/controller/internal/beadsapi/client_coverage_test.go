package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

// --- helpers ---

// newTestClient returns a Client pointed at srv with no auth.
func newTestClient(srv *httptest.Server) *Client {
	return &Client{baseURL: srv.URL, httpClient: srv.Client(), sseClient: srv.Client()}
}

// --- list-kind endpoints returning beads ---

func TestListTaskBeads_ReturnsBeadDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "task" {
			t.Errorf("expected type=task, got %s", r.URL.Query().Get("type"))
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{{ID: "gd-a", Title: "a"}, {ID: "gd-b", Title: "b"}},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListTaskBeads(context.Background())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 2 || got[0].ID != "gd-a" {
		t.Fatalf("unexpected beads: %+v", got)
	}
}

func TestListTaskBeadsAll_IncludesClosed(t *testing.T) {
	var gotStatus string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStatus = r.URL.Query().Get("status")
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: []beadJSON{{ID: "gd-1"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListTaskBeadsAll(context.Background())
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if !strings.Contains(gotStatus, "closed") {
		t.Errorf("expected closed in status, got %s", gotStatus)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 bead, got %d", len(got))
	}
}

func TestListDecisionBeads_FiltersByType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "decision" {
			t.Errorf("expected type=decision, got %s", r.URL.Query().Get("type"))
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: []beadJSON{{ID: "gd-d"}}})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListDecisionBeads(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("unexpected: err=%v beads=%v", err, got)
	}
}

func TestListActiveAgentBeadDetails_ReturnsDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{{ID: "ag-1", Title: "agent-1"}},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListActiveAgentBeadDetails(context.Background())
	if err != nil || len(got) != 1 {
		t.Fatalf("unexpected: err=%v got=%v", err, got)
	}
	if got[0].ID != "ag-1" {
		t.Errorf("expected ag-1, got %s", got[0].ID)
	}
}

// --- ListAssignedTask ---

func TestListAssignedTask(t *testing.T) {
	tests := []struct {
		name   string
		beads  []beadJSON
		expect string // expected bead id ("" means nil)
	}{
		{"match", []beadJSON{{ID: "gd-task", Title: "t"}}, "gd-task"},
		{"empty", []beadJSON{}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("assignee") != "bot" {
					t.Errorf("expected assignee=bot, got %s", r.URL.Query().Get("assignee"))
				}
				if r.URL.Query().Get("kind") != "issue" {
					t.Errorf("expected kind=issue")
				}
				_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: tc.beads})
			}))
			defer srv.Close()

			c := newTestClient(srv)
			got, err := c.ListAssignedTask(context.Background(), "bot")
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if tc.expect == "" {
				if got != nil {
					t.Errorf("expected nil, got %+v", got)
				}
				return
			}
			if got == nil || got.ID != tc.expect {
				t.Errorf("expected %s, got %+v", tc.expect, got)
			}
		})
	}
}

// --- ResolveTicket ---

func TestResolveTicket_KdPrefix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/beads/gd-") {
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "gd-abc", Title: "found"})
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ResolveTicket(context.Background(), "gd-abc")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.ID != "gd-abc" {
		t.Errorf("expected gd-abc, got %s", got.ID)
	}
}

func TestResolveTicket_ByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("labels") == "jira:PE-1" {
			_ = json.NewEncoder(w).Encode(listBeadsResponse{
				Beads: []beadJSON{{ID: "gd-lbl", Title: "by-label"}},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ResolveTicket(context.Background(), "PE-1")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.ID != "gd-lbl" {
		t.Errorf("expected gd-lbl, got %s", got.ID)
	}
}

func TestResolveTicket_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(listBeadsResponse{Beads: nil})
	}))
	defer srv.Close()
	c := newTestClient(srv)
	_, err := c.ResolveTicket(context.Background(), "PE-missing")
	if err == nil {
		t.Fatal("expected error for missing ticket")
	}
}

// --- Update/request/mark helpers ---

func TestUpdateBeadDescription_SendsPatch(t *testing.T) {
	var gotBody map[string]any
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.UpdateBeadDescription(context.Background(), "gd-x", "new desc"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/v1/beads/gd-x" {
		t.Errorf("unexpected request: %s %s", gotMethod, gotPath)
	}
	if gotBody["description"] != "new desc" {
		t.Errorf("expected description=new desc, got %+v", gotBody)
	}
}

func TestListAgentBeadsBySpawner(t *testing.T) {
	tests := []struct {
		name    string
		spawner string
	}{
		{"with_spawner", "gardener-1"},
		{"no_spawner", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotFields []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotFields = r.URL.Query()["field"]
				fields, _ := json.Marshal(map[string]string{
					"agent": "a1", "role": "crew", "project": "gb",
				})
				_ = json.NewEncoder(w).Encode(listBeadsResponse{
					Beads: []beadJSON{
						{ID: "ag-1", Fields: fields},
						{ID: "ag-skip"}, // empty fields → skipped (empty role/name)
					},
				})
			}))
			defer srv.Close()

			c := newTestClient(srv)
			got, err := c.ListAgentBeadsBySpawner(context.Background(), tc.spawner)
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if tc.spawner != "" {
				if len(gotFields) == 0 || !strings.Contains(gotFields[0], "spawned_by="+tc.spawner) {
					t.Errorf("expected spawned_by filter, got %v", gotFields)
				}
			}
			if len(got) != 1 || got[0].AgentName != "a1" {
				t.Errorf("unexpected result: %+v", got)
			}
		})
	}
}

func TestRequestAgentStop_SetsStopRequested(t *testing.T) {
	var lastPatchBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "ag-1", Fields: json.RawMessage(`{"agent_state":"working"}`)})
		case http.MethodPatch:
			_ = json.NewDecoder(r.Body).Decode(&lastPatchBody)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.RequestAgentStop(context.Background(), "ag-1"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	var fields map[string]any
	_ = json.Unmarshal(lastPatchBody["fields"], &fields)
	if fields["agent_state"] != "stop_requested" {
		t.Errorf("expected agent_state=stop_requested, got %+v", fields)
	}
}

func TestMarkAgentFailed_SetsFailureReason(t *testing.T) {
	var lastPatchBody map[string]json.RawMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "ag-1"})
		case http.MethodPatch:
			_ = json.NewDecoder(r.Body).Decode(&lastPatchBody)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.MarkAgentFailed(context.Background(), "ag-1", "DNS-1123"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	var fields map[string]any
	_ = json.Unmarshal(lastPatchBody["fields"], &fields)
	if fields["agent_state"] != "failed" || fields["failure_reason"] != "DNS-1123" {
		t.Errorf("expected agent_state=failed, failure_reason=DNS-1123, got %+v", fields)
	}
}

// --- Fleet tokens ---

func TestRegisterBridgeToken(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/fleet-tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.RegisterBridgeToken(context.Background(), "tok", "myfleet", "gardenio"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if gotBody["token"] != "tok" || gotBody["fleet"] != "myfleet" || gotBody["project"] != "gardenio" {
		t.Errorf("unexpected body: %+v", gotBody)
	}
}

func TestRevokeBridgeToken(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{"success", http.StatusNoContent, false},
		{"not_found_is_success", http.StatusNotFound, false},
		{"server_error_propagates", http.StatusInternalServerError, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete {
					t.Errorf("expected DELETE, got %s", r.Method)
				}
				w.WriteHeader(tc.status)
				if tc.status >= 400 {
					_, _ = w.Write([]byte(`{"error":"boom"}`))
				}
			}))
			defer srv.Close()

			c := newTestClient(srv)
			err := c.RevokeBridgeToken(context.Background(), "myfleet")
			if (err != nil) != tc.wantErr {
				t.Errorf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}

// --- RevokeUser ---

func TestRevokeUser_POSTsAndParses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/admin/revoke-user" || r.Method != http.MethodPost {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		var req RevokeUserRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(RevokeUserResponse{
			UserID: "u1", Username: req.Username, SessionsRevoked: 3, UserDisabled: true,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	resp, err := c.RevokeUser(context.Background(), RevokeUserRequest{
		Username: "alice", Disable: true, Reason: "off-boarding",
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if resp.UserID != "u1" || resp.Username != "alice" || resp.SessionsRevoked != 3 || !resp.UserDisabled {
		t.Errorf("unexpected resp: %+v", resp)
	}
}

// --- ListConfigs ---

func TestListConfigs(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		wantQuery string
	}{
		{"with_namespace", "gardenio", "namespace=gardenio"},
		{"no_namespace", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.wantQuery != "" && !strings.Contains(r.URL.RawQuery, tc.wantQuery) {
					t.Errorf("expected %q in query, got %q", tc.wantQuery, r.URL.RawQuery)
				}
				_ = json.NewEncoder(w).Encode(struct {
					Configs []ConfigEntry `json:"configs"`
				}{Configs: []ConfigEntry{{Key: "k1", Value: json.RawMessage(`"v1"`)}}})
			}))
			defer srv.Close()

			c := newTestClient(srv)
			got, err := c.ListConfigs(context.Background(), tc.namespace)
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if len(got) != 1 || got[0].Key != "k1" {
				t.Errorf("unexpected: %+v", got)
			}
		})
	}
}

// --- Escalations ---

func TestGetEscalation_ParsesBothFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/escalations/") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(escalationDetailJSON{
			Escalation: &beadJSON{ID: "esc-1", Title: "esc"},
			Issue:      &beadJSON{ID: "gd-issue", Title: "issue"},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.GetEscalation(context.Background(), "esc-1")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Escalation == nil || got.Escalation.ID != "esc-1" {
		t.Errorf("expected esc-1, got %+v", got.Escalation)
	}
	if got.Issue == nil || got.Issue.ID != "gd-issue" {
		t.Errorf("expected gd-issue, got %+v", got.Issue)
	}
}

func TestGetEscalation_HandlesEmptyEscalation(t *testing.T) {
	// toDetail returns ed with nil Escalation when the inner ID is empty.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(escalationDetailJSON{
			Escalation: &beadJSON{},
			Issue:      nil,
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.GetEscalation(context.Background(), "esc-x")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Escalation != nil {
		t.Errorf("expected nil Escalation, got %+v", got.Escalation)
	}
}

func TestListEscalations(t *testing.T) {
	tests := []struct {
		name   string
		status string
		limit  int
	}{
		{"filters", "pending", 10},
		{"no_filters", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != "" && r.URL.Query().Get("status") != tc.status {
					t.Errorf("expected status=%s", tc.status)
				}
				_ = json.NewEncoder(w).Encode(struct {
					Escalations []escalationDetailJSON `json:"escalations"`
				}{Escalations: []escalationDetailJSON{
					{Escalation: &beadJSON{ID: "e1"}},
				}})
			}))
			defer srv.Close()

			c := newTestClient(srv)
			got, err := c.ListEscalations(context.Background(), tc.status, tc.limit)
			if err != nil || len(got) != 1 {
				t.Fatalf("unexpected: err=%v got=%v", err, got)
			}
		})
	}
}

func TestApproveAndRejectEscalation(t *testing.T) {
	var lastPath string
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&lastBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.ApproveEscalation(context.Background(), "e1", ApproveEscalationRequest{ApprovedBy: "alice"}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !strings.HasSuffix(lastPath, "/approve") || lastBody["approved_by"] != "alice" {
		t.Errorf("approve: unexpected path=%s body=%+v", lastPath, lastBody)
	}
	if err := c.RejectEscalation(context.Background(), "e1", RejectEscalationRequest{RejectedBy: "bob", Reason: "no"}); err != nil {
		t.Fatalf("reject: %v", err)
	}
	if !strings.HasSuffix(lastPath, "/reject") || lastBody["rejected_by"] != "bob" {
		t.Errorf("reject: unexpected path=%s body=%+v", lastPath, lastBody)
	}
}

func TestCaptureEscalationResult(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/result") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	err := c.CaptureEscalationResult(context.Background(), "e1", CaptureEscalationResultRequest{
		Executor: "bot", ResultStatus: "success",
	})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if gotBody["result_status"] != "success" || gotBody["executor"] != "bot" {
		t.Errorf("unexpected body: %+v", gotBody)
	}
}

// --- GetAgentRoster ---

func TestGetAgentRoster(t *testing.T) {
	tests := []struct {
		name           string
		threshold      int
		expectQueryVal string
	}{
		{"with_threshold", 120, "120"},
		{"zero_threshold", 0, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/agents/roster" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				if got := r.URL.Query().Get("stale_threshold_secs"); got != tc.expectQueryVal {
					t.Errorf("expected stale_threshold_secs=%s, got %s", tc.expectQueryVal, got)
				}
				_ = json.NewEncoder(w).Encode(AgentRosterResponse{
					Actors:         []RosterActor{{Actor: "a", TaskID: "t1"}},
					UnclaimedTasks: []UnclaimedTask{{ID: "u1"}},
				})
			}))
			defer srv.Close()

			c := newTestClient(srv)
			got, err := c.GetAgentRoster(context.Background(), tc.threshold)
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if len(got.Actors) != 1 || len(got.UnclaimedTasks) != 1 {
				t.Errorf("unexpected: %+v", got)
			}
		})
	}
}

// --- Search ---

func TestFindSimilar(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/similar") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("limit") == "" {
			t.Errorf("expected limit in query")
		}
		_ = json.NewEncoder(w).Encode(findSimilarWire{
			SourceID: "gd-src",
			Model:    "test-model",
			Results: []struct {
				Bead     *beadJSON `json:"bead"`
				Score    float64   `json:"score"`
				Distance float64   `json:"distance"`
			}{
				{Bead: &beadJSON{ID: "gd-n1"}, Score: 0.9, Distance: 0.1},
				{Bead: nil, Score: 0.5}, // should be filtered
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.FindSimilar(context.Background(), "gd-src", 0) // defaults to 10
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.SourceID != "gd-src" || got.Model != "test-model" {
		t.Errorf("unexpected meta: %+v", got)
	}
	if len(got.Results) != 1 || got.Results[0].Bead.ID != "gd-n1" {
		t.Errorf("unexpected results: %+v", got.Results)
	}
}

func TestSemanticSearch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/search/semantic" || r.Method != http.MethodPost {
			t.Errorf("unexpected: %s %s", r.Method, r.URL.Path)
		}
		var req SemanticSearchRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Query != "hello" {
			t.Errorf("expected query=hello, got %q", req.Query)
		}
		_ = json.NewEncoder(w).Encode(semanticSearchWire{
			Model: "m1",
			Results: []struct {
				Bead     *beadJSON `json:"bead"`
				Score    float64   `json:"score"`
				Distance float64   `json:"distance"`
			}{
				{Bead: &beadJSON{ID: "gd-1"}, Score: 0.95},
				{Bead: nil}, // filtered
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.SemanticSearch(context.Background(), SemanticSearchRequest{Query: "hello", Limit: 5})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if got.Model != "m1" || len(got.Results) != 1 || got.Results[0].Bead.ID != "gd-1" {
		t.Errorf("unexpected: %+v", got)
	}
}

// --- Devices ---

func TestListDevices(t *testing.T) {
	tests := []struct {
		name      string
		emails    []string
		platform  string
		wantQuery url.Values
	}{
		{"recipients_and_platform", []string{"a@b.c", "d+x@e.f"}, "ios",
			url.Values{"user_email": {"a@b.c", "d+x@e.f"}, "platform": {"ios"}}},
		{"email_only", []string{"a@b.c"}, "", url.Values{"user_email": {"a@b.c"}}},
		{"blank_emails_dropped", []string{""}, "ios", url.Values{"platform": {"ios"}}},
		{"none", nil, "", url.Values{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotQuery url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.Query()
				_ = json.NewEncoder(w).Encode(listDevicesResponse{
					Devices: []*Device{{Token: "tok", Platform: "ios"}},
				})
			}))
			defer srv.Close()

			c := newTestClient(srv)
			got, err := c.ListDevices(context.Background(), tc.emails, tc.platform)
			if err != nil {
				t.Fatalf("unexpected: %v", err)
			}
			if len(got) != 1 || got[0].Token != "tok" {
				t.Errorf("unexpected: %+v", got)
			}
			// The query must round-trip exactly: a "+" in an address left
			// unescaped would arrive as a space.
			if !reflect.DeepEqual(gotQuery, tc.wantQuery) {
				t.Errorf("query = %v, want %v", gotQuery, tc.wantQuery)
			}
		})
	}
}

func TestDeleteDevice(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.DeleteDevice(context.Background(), "tok123"); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v1/devices/tok123" {
		t.Errorf("unexpected: %s %s", gotMethod, gotPath)
	}
}

// --- EventStream / BaseURL ---

func TestBaseURL_ReturnsConfiguredAddr(t *testing.T) {
	c, err := New(Config{HTTPAddr: "http://daemon:8080"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if c.BaseURL() != "http://daemon:8080" {
		t.Errorf("expected http://daemon:8080, got %s", c.BaseURL())
	}
}

func TestEventStream_ParsesEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/events/stream" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("topics") != "beads.>" {
			t.Errorf("expected topics=beads.>, got %s", r.URL.Query().Get("topics"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Two SSE events separated by blank lines.
		_, _ = io.WriteString(w, "event: bead.updated\ndata: {\"id\":\"gd-1\"}\n\nevent: bead.closed\ndata: {\"id\":\"gd-2\"}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer srv.Close()

	c := newTestClient(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ch, err := c.EventStream(ctx, "beads.>")
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	want := []string{"bead.updated", "bead.closed"}
	for i := range 2 {
		select {
		case ev, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed early at i=%d", i)
			}
			if ev.Event != want[i] {
				t.Errorf("i=%d expected event=%s, got %s", i, want[i], ev.Event)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
}

func TestEventStream_NonOKStatusReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.EventStream(context.Background(), "")
	if err == nil {
		t.Fatal("expected error on non-200 status")
	}
}

// --- ListOrphanedSessions / ReconcileSession ---

func TestListOrphanedSessions_FiltersByActiveAndAge(t *testing.T) {
	oldStart := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	recentStart := time.Now().UTC().Format(time.RFC3339)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server returns multiple sessions — the client filters.
		oldFields, _ := json.Marshal(map[string]string{"status": "active", "started_at": oldStart})
		recentFields, _ := json.Marshal(map[string]string{"status": "active", "started_at": recentStart})
		completedFields, _ := json.Marshal(map[string]string{"status": "completed", "started_at": oldStart})
		noStartFields, _ := json.Marshal(map[string]string{"status": "active"})
		badStartFields, _ := json.Marshal(map[string]string{"status": "active", "started_at": "not-a-date"})
		_ = json.NewEncoder(w).Encode(listBeadsResponse{
			Beads: []beadJSON{
				{ID: "old", Fields: oldFields},
				{ID: "recent", Fields: recentFields},
				{ID: "completed", Fields: completedFields},
				{ID: "nostart", Fields: noStartFields},
				{ID: "badstart", Fields: badStartFields},
			},
		})
	}))
	defer srv.Close()

	c := newTestClient(srv)
	got, err := c.ListOrphanedSessions(context.Background(), "", time.Hour)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 1 || got[0].ID != "old" {
		t.Errorf("expected [old], got %+v", got)
	}
}

func TestReconcileSession_ClosesWithoutUsage(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "s1"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	session := &BeadDetail{ID: "s1", Fields: map[string]string{}}
	cost, err := c.ReconcileSession(context.Background(), session, nil)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cost != 0 {
		t.Errorf("expected cost=0 with nil usage, got %v", cost)
	}
}

func TestReconcileSession_UpdatesAgentUsageWhenPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(beadJSON{ID: "s1"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := newTestClient(srv)
	session := &BeadDetail{
		ID:     "s1",
		Fields: map[string]string{"agent_bead_id": "ag-1"},
	}
	usage := &SessionUsage{TotalCostUSD: 1.23}
	cost, err := c.ReconcileSession(context.Background(), session, usage)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if cost != 1.23 {
		t.Errorf("expected cost=1.23, got %v", cost)
	}
}

// --- config_watcher options (WithRole, WithLogger, parseValue edge cases) ---

func TestWithRoleAndWithLogger_Apply(t *testing.T) {
	c := &Client{baseURL: "http://x"}
	w, err := NewConfigWatcher(c, "comp",
		WithRole("gardener"),
		WithLogger(nil), // no-op: nil logger is ignored
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if w.role != "gardener" {
		t.Errorf("expected role=gardener, got %q", w.role)
	}
	if w.logger == nil {
		t.Error("expected default logger to remain set when WithLogger(nil)")
	}
}

func TestParseValue_SupportsAllTypes(t *testing.T) {
	if v, err := parseValue[string]("hello"); err != nil || v != "hello" {
		t.Errorf("string: %v %v", v, err)
	}
	if v, err := parseValue[bool]("true"); err != nil || v != true {
		t.Errorf("bool: %v %v", v, err)
	}
	if v, err := parseValue[int]("42"); err != nil || v != 42 {
		t.Errorf("int: %v %v", v, err)
	}
	if v, err := parseValue[int64]("42"); err != nil || v != 42 {
		t.Errorf("int64: %v %v", v, err)
	}
	if v, err := parseValue[float64]("3.14"); err != nil || v != 3.14 {
		t.Errorf("float64: %v %v", v, err)
	}
	if v, err := parseValue[time.Duration]("5s"); err != nil || v != 5*time.Second {
		t.Errorf("duration: %v %v", v, err)
	}
	// Error paths.
	if _, err := parseValue[bool]("nope"); err == nil {
		t.Error("expected bool parse error")
	}
	if _, err := parseValue[int]("nope"); err == nil {
		t.Error("expected int parse error")
	}
	if _, err := parseValue[int64]("nope"); err == nil {
		t.Error("expected int64 parse error")
	}
	if _, err := parseValue[float64]("nope"); err == nil {
		t.Error("expected float64 parse error")
	}
	if _, err := parseValue[time.Duration]("nope"); err == nil {
		t.Error("expected duration parse error")
	}
}

func TestFormatCompiled_AllTypes(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{"hi", "hi"},
		{true, "true"},
		{42, "42"},
		{int64(42), "42"},
		{3.14, "3.14"},
		{5 * time.Second, "5s"},
	}
	for _, tc := range cases {
		if got := formatCompiled(tc.in); got != tc.want {
			t.Errorf("formatCompiled(%v)=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- setAuth coverage: TokenSource branch ---

type staticTokenSource string

func (s staticTokenSource) Token(context.Context) (string, error) { return string(s), nil }

type errorTokenSource struct{ err error }

func (e errorTokenSource) Token(context.Context) (string, error) { return "", e.err }

func TestSetAuth_TokenSourceUsed(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(beadJSON{ID: "ok"})
	}))
	defer srv.Close()

	c := &Client{
		baseURL:     srv.URL,
		httpClient:  srv.Client(),
		tokenSource: staticTokenSource("dyn-tok"),
	}
	_, _ = c.GetBead(context.Background(), "gd-any")
	if gotAuth != "Bearer dyn-tok" {
		t.Errorf("expected Bearer dyn-tok, got %q", gotAuth)
	}
}

func TestSetAuth_TokenSourceError_PropagatesFromDoJSON(t *testing.T) {
	c := &Client{
		baseURL:     "http://ignored",
		httpClient:  &http.Client{},
		tokenSource: errorTokenSource{err: io.EOF},
	}
	_, err := c.GetBead(context.Background(), "gd-x")
	if err == nil {
		t.Fatal("expected error when token source fails")
	}
}
