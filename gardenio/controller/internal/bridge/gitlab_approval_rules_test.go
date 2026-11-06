package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGitLabClient_ListMergeRequestApprovalRules(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":1,"name":"AI-generated change review","approvals_required":1}]`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	rules, err := client.ListMergeRequestApprovalRules(context.Background(), "grp/repo", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "AI-generated change review" {
		t.Errorf("rules = %+v, want one named rule", rules)
	}
}

// MR-level approval rules are a Premium feature; on CE the endpoint 404s and
// must be treated as "no rules" rather than an error.
func TestGitLabClient_ListMergeRequestApprovalRules_CE404(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"404 Not Found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	rules, err := client.ListMergeRequestApprovalRules(context.Background(), "grp/repo", 7)
	if err != nil {
		t.Fatalf("CE 404 should not error, got: %v", err)
	}
	if rules != nil {
		t.Errorf("CE 404 should yield nil rules, got %+v", rules)
	}
}

func TestGitLabClient_CreateMergeRequestApprovalRule(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":5,"name":"AI-generated change review","approvals_required":2}`))
	}))
	defer server.Close()

	client := newTestGitLabClient(server.URL)
	rule, err := client.CreateMergeRequestApprovalRule(context.Background(), "grp/repo", 7, MRApprovalRuleSpec{
		Name:              "AI-generated change review",
		ApprovalsRequired: 2,
		GroupIDs:          []int{42},
		UserIDs:           []int{99},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rule.ID != 5 || rule.ApprovalsRequired != 2 {
		t.Errorf("rule = %+v, want id=5 approvals=2", rule)
	}
	if gotBody["name"] != "AI-generated change review" {
		t.Errorf("request name = %v", gotBody["name"])
	}
	if gotBody["approvals_required"].(float64) != 2 {
		t.Errorf("request approvals_required = %v, want 2", gotBody["approvals_required"])
	}
	if groups, ok := gotBody["group_ids"].([]any); !ok || len(groups) != 1 || groups[0].(float64) != 42 {
		t.Errorf("request group_ids = %v, want [42]", gotBody["group_ids"])
	}
}
