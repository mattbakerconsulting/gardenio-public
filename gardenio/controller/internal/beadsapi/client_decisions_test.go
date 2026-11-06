package beadsapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListDecisions_ParsesComplexFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/v1/decisions" {
			t.Errorf("expected path /v1/decisions, got %s", r.URL.Path)
		}
		// Simulate server response with options as a JSON array (not a string).
		resp := `{"decisions":[{
			"decision": {
				"id": "bd-dec1",
				"title": "Test decision",
				"type": "decision",
				"status": "open",
				"priority": 1,
				"fields": {
					"prompt": "Which approach?",
					"options": [
						{"id": "a", "label": "Fast", "short": "fast", "artifact_type": "plan"},
						{"id": "b", "label": "Safe", "short": "safe", "artifact_type": "report"}
					],
					"requested_by": "agent-1",
					"context": "{\"key\": \"value\"}"
				}
			},
			"issue": {
				"id": "bd-issue1",
				"title": "Parent issue",
				"type": "task",
				"status": "in_progress",
				"fields": {"project": "gardenio"}
			}
		}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	decisions, err := c.ListDecisions(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}

	dd := decisions[0]
	if dd.Decision == nil {
		t.Fatal("expected Decision to be non-nil")
	}
	if dd.Decision.ID != "bd-dec1" {
		t.Errorf("expected decision ID bd-dec1, got %s", dd.Decision.ID)
	}
	if dd.Decision.Fields["prompt"] != "Which approach?" {
		t.Errorf("expected prompt 'Which approach?', got %s", dd.Decision.Fields["prompt"])
	}
	// The options array should be re-marshaled to a JSON string.
	opts := dd.Decision.Fields["options"]
	if opts == "" {
		t.Fatal("expected options field to be set")
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(opts), &parsed); err != nil {
		t.Fatalf("options field should be valid JSON array: %v", err)
	}
	if len(parsed) != 2 {
		t.Errorf("expected 2 options, got %d", len(parsed))
	}

	// Issue should also be parsed.
	if dd.Issue == nil {
		t.Fatal("expected Issue to be non-nil")
	}
	if dd.Issue.ID != "bd-issue1" {
		t.Errorf("expected issue ID bd-issue1, got %s", dd.Issue.ID)
	}
}

func TestListDecisions_PassesQueryParams(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decisions":[]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	_, err := c.ListDecisions(context.Background(), "open,in_progress", 25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotQuery != "limit=25&status=open%2Cin_progress" {
		t.Errorf("unexpected query: %s", gotQuery)
	}
}

func TestListDecisions_EmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"decisions":[]}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	decisions, err := c.ListDecisions(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decisions) != 0 {
		t.Errorf("expected 0 decisions, got %d", len(decisions))
	}
}

func TestListDecisions_NilIssue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := `{"decisions":[{
			"decision": {
				"id": "bd-no-issue",
				"title": "Orphan decision",
				"type": "decision",
				"status": "open",
				"fields": {"prompt": "What now?"}
			}
		}]}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	decisions, err := c.ListDecisions(context.Background(), "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(decisions))
	}
	if decisions[0].Issue != nil {
		t.Error("expected Issue to be nil")
	}
}

func TestGetDecision_ParsesComplexFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions/bd-dec2" {
			t.Errorf("expected path /v1/decisions/bd-dec2, got %s", r.URL.Path)
		}
		resp := `{
			"decision": {
				"id": "bd-dec2",
				"title": "Get decision test",
				"type": "decision",
				"status": "open",
				"fields": {
					"prompt": "Pick one",
					"options": [{"id": "x", "label": "Option X"}]
				}
			}
		}`
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(resp))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	dd, err := c.GetDecision(context.Background(), "bd-dec2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dd.Decision == nil {
		t.Fatal("expected Decision to be non-nil")
	}
	if dd.Decision.ID != "bd-dec2" {
		t.Errorf("expected ID bd-dec2, got %s", dd.Decision.ID)
	}
	// options array should be re-marshaled to JSON string.
	opts := dd.Decision.Fields["options"]
	if opts == "" {
		t.Fatal("expected options field")
	}
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(opts), &parsed); err != nil {
		t.Fatalf("options should be valid JSON: %v", err)
	}
	if len(parsed) != 1 {
		t.Errorf("expected 1 option, got %d", len(parsed))
	}
}

func TestResolveDecision_SendsCorrectRequest(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody ResolveDecisionRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.ResolveDecision(context.Background(), "bd-res1", ResolveDecisionRequest{
		SelectedOption: "option-a",
		ResponseText:   "looks good",
		RespondedBy:    "human",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/decisions/bd-res1/resolve" {
		t.Errorf("expected path /v1/decisions/bd-res1/resolve, got %s", gotPath)
	}
	if gotBody.SelectedOption != "option-a" {
		t.Errorf("expected selected_option=option-a, got %s", gotBody.SelectedOption)
	}
	if gotBody.ResponseText != "looks good" {
		t.Errorf("expected response_text='looks good', got %s", gotBody.ResponseText)
	}
	if gotBody.RespondedBy != "human" {
		t.Errorf("expected responded_by=human, got %s", gotBody.RespondedBy)
	}
}

func TestResolveDecision_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"decision already resolved"}`))
	}))
	defer srv.Close()

	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}
	err := c.ResolveDecision(context.Background(), "dec-456", ResolveDecisionRequest{
		SelectedOption: "x",
		RespondedBy:    "bot",
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

func TestCancelDecision_SendsCorrectRequest(t *testing.T) {
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
	err := c.CancelDecision(context.Background(), "bd-cancel1", "not needed", "human")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("expected POST, got %s", gotMethod)
	}
	if gotPath != "/v1/decisions/bd-cancel1/cancel" {
		t.Errorf("expected path /v1/decisions/bd-cancel1/cancel, got %s", gotPath)
	}
	if gotBody["reason"] != "not needed" {
		t.Errorf("expected reason='not needed', got %s", gotBody["reason"])
	}
	if gotBody["canceled_by"] != "human" {
		t.Errorf("expected canceled_by=human, got %s", gotBody["canceled_by"])
	}
}
