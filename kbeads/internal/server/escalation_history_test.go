package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Each merged field gets a mutation_history row with the old and new value
// (P7 Task 15).
func TestApprove_RecordsMutationHistory(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedEscalation(t, ms, "gd-h1", "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
	before := len(ms.mutations)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-h1/approve", "owner@example.com", map[string]any{"approval_method": "cli"}, nil), http.StatusOK)
	got := map[string]string{}
	for _, m := range ms.mutations[before:] {
		if m.BeadID == "gd-h1" {
			got[m.Field] = m.NewValue
		}
	}
	for _, k := range []string{"fields.approved_by", "fields.approved_at", "fields.approval_method", "fields.execution_dispatch"} {
		if _, ok := got[k]; !ok {
			t.Errorf("no mutation row for %s (rows: %v)", k, got)
		}
	}
	if got["fields.approval_method"] != `"web_ui"` {
		t.Errorf("approval_method row = %s, want derived \"web_ui\" (body said cli)", got["fields.approval_method"])
	}
	if m := escFields(t, ms, "gd-h1")["approval_method"]; m != "web_ui" {
		t.Errorf("approval_method on bead = %v, want web_ui", m)
	}
}

// A body approval_method the daemon never accepted (the Commons sends
// "commons") no longer 400s: the value is ignored, not validated.
func TestApprove_IgnoresUnknownBodyApprovalMethod(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedEscalation(t, ms, "gd-h4", "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-h4/approve", "owner@example.com", map[string]any{"approval_method": "anything"}, nil), http.StatusOK)
	if m := escFields(t, ms, "gd-h4")["approval_method"]; m != "web_ui" {
		t.Errorf("approval_method = %v, want web_ui", m)
	}
}

func TestApprovalMethodFor(t *testing.T) {
	if got := approvalMethodFor(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "o@example.com"})); got != "edge" {
		t.Errorf("edge identity: %q, want edge", got)
	}
	if got := approvalMethodFor(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "o@example.com"})); got != "web_ui" {
		t.Errorf("oidc identity: %q, want web_ui", got)
	}
}

func TestResultAndReject_RecordMutationHistory(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient}) // the operator rejects on the page
	seedEscalation(t, ms, "gd-h2", "", map[string]any{"operation_type": "github_pr_merge"})
	seedEscalation(t, ms, "gd-h3", "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
	before := len(ms.mutations)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-h2/result", "owner@example.com", map[string]any{"result_status": "success"}, nil), http.StatusOK)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-h3/reject", "owner@example.com", map[string]any{"reason": "no"}, nil), http.StatusOK)
	seen := map[string]bool{}
	for _, m := range ms.mutations[before:] {
		seen[m.BeadID+" "+m.Field] = true
	}
	for _, k := range []string{"gd-h2 fields.result_status", "gd-h2 fields.executor", "gd-h3 fields.rejected_by", "gd-h3 fields.result_status"} {
		if !seen[k] {
			t.Errorf("missing mutation row %q", k)
		}
	}
}

// An escalation the daemon would dispatch needs a non-empty operation_spec;
// a verdict record or a manual request does not (gb sends {} by default),
// and neither does a close, which names its bead in target_bead_id.
func TestEscalationCreate_OperationSpecMustBeRealWhenExecutable(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedRunnerAndAgent(ms, "gd-la4", "laptop")
	seedEscalation(t, ms, "gd-x", "", map[string]any{"operation_type": "manual_operator_action"})
	refused := func(name string, fields map[string]any) {
		t.Helper()
		rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(fields), nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "operation_spec") {
			t.Fatalf("%s: %d %s, want 400 naming operation_spec", name, rec.Code, rec.Body.String())
		}
	}
	refused("local with {} spec", map[string]any{
		"operation_type": "k8s_apply", "operation_spec": map[string]any{}, "execution_target": "local", "target_runner": "gd-la4",
	})
	refused("update replay with {} spec", map[string]any{
		"operation_type": "update", "operation_spec": map[string]any{}, "target_bead_id": "gd-x",
	})
	refused("update replay with a non-object spec", map[string]any{
		"operation_type": "update", "operation_spec": "yes", "target_bead_id": "gd-x",
	})
	for _, f := range []map[string]any{
		{"operation_type": "github_pr_merge", "operation_spec": map[string]any{}},
		{"operation_type": "manual_operator_action", "operation_spec": map[string]any{}},
		{"operation_type": "close", "operation_spec": map[string]any{}, "target_bead_id": "gd-x"},
		{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "local", "target_runner": "gd-la4"},
	} {
		if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(f), nil); rec.Code != http.StatusCreated {
			t.Fatalf("positive control %v: %d %s, want 201", f, rec.Code, rec.Body.String())
		}
	}
}
