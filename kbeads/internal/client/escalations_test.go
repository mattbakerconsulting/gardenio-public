package client

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// TestHTTPClient_GetEscalation verifies the GET path and the two-view decode:
// the daemon returns the escalation-fields projection under `escalation` and
// the full bead under `issue`.
func TestHTTPClient_GetEscalation(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"escalation": {"operation_type": "aws_iam", "sensitivity": "secret"},
			"issue": {
				"id": "gd-egqpg0fc45",
				"type": "escalation",
				"kind": "data",
				"title": "rotate prod key",
				"status": "open",
				"priority": 1,
				"created_by": "agent-x",
				"created_at": "2026-01-10T08:00:00Z",
				"updated_at": "2026-01-10T08:00:00Z"
			}
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	d, err := c.GetEscalation(context.Background(), "gd-egqpg0fc45")
	if err != nil {
		t.Fatalf("GetEscalation() error = %v", err)
	}
	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/escalations/gd-egqpg0fc45" {
		t.Errorf("path = %q, want /v1/escalations/gd-egqpg0fc45", h.path)
	}
	if d.Issue == nil || d.Issue.ID != "gd-egqpg0fc45" {
		t.Fatalf("issue = %+v, want id gd-egqpg0fc45", d.Issue)
	}
	if d.Issue.CreatedBy != "agent-x" {
		t.Errorf("issue.CreatedBy = %q, want agent-x", d.Issue.CreatedBy)
	}
	if d.Escalation["operation_type"] != "aws_iam" {
		t.Errorf("escalation.operation_type = %v, want aws_iam", d.Escalation["operation_type"])
	}
}

// TestHTTPClient_ListEscalations verifies status+limit are sent as query params
// and the enveloped `escalations` array decodes.
func TestHTTPClient_ListEscalations(t *testing.T) {
	h := &testHandler{
		responseBody: `{"escalations": [
			{"escalation": {"operation_type": "k8s_rbac"}, "issue": {"id": "gd-egqpg0fc45", "type": "escalation", "kind": "data", "title": "t", "status": "open", "priority": 0, "created_at": "2026-01-10T08:00:00Z", "updated_at": "2026-01-10T08:00:00Z"}}
		]}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	got, err := c.ListEscalations(context.Background(), ListEscalationsOptions{Status: "open,in_progress", Limit: 25})
	if err != nil {
		t.Fatalf("ListEscalations() error = %v", err)
	}
	if h.path != "/v1/escalations" {
		t.Errorf("path = %q, want /v1/escalations", h.path)
	}
	if !strings.Contains(h.query, "status=open") || !strings.Contains(h.query, "limit=25") {
		t.Errorf("query = %q, want status + limit params", h.query)
	}
	if len(got) != 1 || got[0].Issue == nil || got[0].Issue.ID != "gd-egqpg0fc45" {
		t.Fatalf("ListEscalations returned %+v, want one escalation gd-egqpg0fc45", got)
	}
}

// TestHTTPClient_ListEscalations_NoFilters omits both query params when status
// is empty and limit <= 0 (defer to daemon defaults).
func TestHTTPClient_ListEscalations_NoFilters(t *testing.T) {
	h := &testHandler{responseBody: `{"escalations": []}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.ListEscalations(context.Background(), ListEscalationsOptions{}); err != nil {
		t.Fatalf("ListEscalations() error = %v", err)
	}
	if h.query != "" {
		t.Errorf("query = %q, want empty (no status/limit)", h.query)
	}
}

// TestHTTPClient_ListEscalations_Filters sends operation_type, label and
// exclude_label as comma-joined query params (gd-CndLtkZ5h0).
func TestHTTPClient_ListEscalations_Filters(t *testing.T) {
	h := &testHandler{responseBody: `{"escalations": []}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.ListEscalations(context.Background(), ListEscalationsOptions{
		OperationTypes: []string{"github_pr_merge", "gitlab_mr_merge"},
		Labels:         []string{"merge-desk:draft"},
		ExcludeLabels:  []string{"merge-desk:seen", "merge-desk:close-denied"},
	}); err != nil {
		t.Fatalf("ListEscalations() error = %v", err)
	}
	q, err := url.ParseQuery(h.query)
	if err != nil {
		t.Fatalf("parse query %q: %v", h.query, err)
	}
	if got := q.Get("operation_type"); got != "github_pr_merge,gitlab_mr_merge" {
		t.Errorf("operation_type = %q", got)
	}
	if got := q.Get("label"); got != "merge-desk:draft" {
		t.Errorf("label = %q", got)
	}
	if got := q.Get("exclude_label"); got != "merge-desk:seen,merge-desk:close-denied" {
		t.Errorf("exclude_label = %q", got)
	}
}

// TestHTTPClient_ApproveEscalation POSTs the approve endpoint with the body.
func TestHTTPClient_ApproveEscalation(t *testing.T) {
	h := &testHandler{statusCode: http.StatusOK, responseBody: `{}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.ApproveEscalation(context.Background(), "gd-egqpg0fc45", ApproveEscalationRequest{
		ApprovedBy: "op", ApprovalMethod: "cli",
	})
	if err != nil {
		t.Fatalf("ApproveEscalation() error = %v", err)
	}
	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/escalations/gd-egqpg0fc45/approve" {
		t.Errorf("path = %q, want .../approve", h.path)
	}
	if !strings.Contains(h.body, `"approved_by":"op"`) {
		t.Errorf("body = %q, want approved_by=op", h.body)
	}
}

// TestHTTPClient_RejectEscalation POSTs the reject endpoint.
func TestHTTPClient_RejectEscalation(t *testing.T) {
	h := &testHandler{statusCode: http.StatusOK, responseBody: `{}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.RejectEscalation(context.Background(), "gd-egqpg0fc45", RejectEscalationRequest{
		RejectedBy: "op", Reason: "dup",
	})
	if err != nil {
		t.Fatalf("RejectEscalation() error = %v", err)
	}
	if h.path != "/v1/escalations/gd-egqpg0fc45/reject" {
		t.Errorf("path = %q, want .../reject", h.path)
	}
	if !strings.Contains(h.body, `"reason":"dup"`) {
		t.Errorf("body = %q, want reason=dup", h.body)
	}
}

// TestHTTPClient_CaptureEscalationResult POSTs the result endpoint.
func TestHTTPClient_CaptureEscalationResult(t *testing.T) {
	h := &testHandler{statusCode: http.StatusOK, responseBody: `{}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.CaptureEscalationResult(context.Background(), "gd-egqpg0fc45", CaptureEscalationResultRequest{
		ResultStatus: "success",
		Executor:     "op",
		Result:       map[string]string{"output": "done"},
	})
	if err != nil {
		t.Fatalf("CaptureEscalationResult() error = %v", err)
	}
	if h.path != "/v1/escalations/gd-egqpg0fc45/result" {
		t.Errorf("path = %q, want .../result", h.path)
	}
	if !strings.Contains(h.body, `"result_status":"success"`) {
		t.Errorf("body = %q, want result_status=success", h.body)
	}
}

// TestHTTPClient_ApproveEscalation_403 surfaces the operator-gated 403 as an
// *APIError so the CLI can render an actionable message. This is the wire-level
// half of the "agents cannot resolve escalations" acceptance criterion.
func TestHTTPClient_ApproveEscalation_403(t *testing.T) {
	h := &testHandler{statusCode: http.StatusForbidden, responseBody: `{"error": "forbidden: requires operator role"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.ApproveEscalation(context.Background(), "gd-egqpg0fc45", ApproveEscalationRequest{ApprovedBy: "agent"})
	if err == nil {
		t.Fatal("ApproveEscalation() with a 403 returned nil, want an error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 403 {
		t.Fatalf("error = %v, want an *APIError with StatusCode 403", err)
	}
}

func TestHTTPClient_ClaimEscalation(t *testing.T) {
	h := &testHandler{statusCode: http.StatusOK, responseBody: `{"id":"gd-egqpg0fc45","type":"escalation","status":"open","fields":{"claimed_by":"gd-a"}}`}
	c, srv := newTestClient(h)
	defer srv.Close()
	b, err := c.ClaimEscalation(context.Background(), "gd-egqpg0fc45")
	if err != nil {
		t.Fatalf("ClaimEscalation() error = %v", err)
	}
	if h.method != http.MethodPost || h.path != "/v1/escalations/gd-egqpg0fc45/claim" {
		t.Errorf("request = %s %s, want POST .../claim", h.method, h.path)
	}
	if b == nil || b.ID != "gd-egqpg0fc45" {
		t.Errorf("bead = %+v", b)
	}
}

func TestHTTPClient_HandUpEscalation(t *testing.T) {
	h := &testHandler{statusCode: http.StatusOK, responseBody: `{"id":"gd-egqpg0fc45","type":"escalation","status":"open"}`}
	c, srv := newTestClient(h)
	defer srv.Close()
	if _, err := c.HandUpEscalation(context.Background(), "gd-egqpg0fc45", HandUpEscalationRequest{Reason: "needs owner"}); err != nil {
		t.Fatalf("HandUpEscalation() error = %v", err)
	}
	if h.path != "/v1/escalations/gd-egqpg0fc45/hand-up" || !strings.Contains(h.body, `"reason":"needs owner"`) {
		t.Errorf("request = %s %s", h.path, h.body)
	}
}
