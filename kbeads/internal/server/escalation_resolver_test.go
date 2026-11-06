package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// rejected_by, closed_by and the audit actor are the operator's identity, not
// the body's. Bridges cannot reject (Task 9), so the Slack bridge's old web
// reject could not forge this either.
func TestReject_StampsApproverIdentity(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient}) // the operator rejects on the page
	seedEscalation(t, ms, "gd-rej1", "", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}})
	rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-rej1/reject", "owner@example.com",
		map[string]any{"rejected_by": "someone-else", "reason": "no"}, nil)
	requireStatus(t, rec, http.StatusOK)
	b, _ := ms.GetBead(context.Background(), "gd-rej1")
	if b.ClosedBy != "owner@example.com" {
		t.Fatalf("closed_by = %q, want owner@example.com", b.ClosedBy)
	}
	if f := escFields(t, ms, "gd-rej1"); f["rejected_by"] != "owner@example.com" {
		t.Fatalf("rejected_by = %v", f["rejected_by"])
	}
}

// executor is server-stamped; a forged body executor/execution_target is ignored.
func TestResult_ExecutorIsServerStamped(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-res1", "", map[string]any{"operation_type": "github_pr_merge", "execution_target": "local"})
	rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-res1/result", testBridgeToken,
		map[string]any{"result_status": "success", "executor": "forged", "execution_target": "service_account"}, nil)
	requireStatus(t, rec, http.StatusOK)
	f := escFields(t, ms, "gd-res1")
	if f["executor"] != "slack-bridge" || f["execution_target"] != "local" {
		t.Fatalf("executor=%v execution_target=%v; want slack-bridge, local", f["executor"], f["execution_target"])
	}
	b, _ := ms.GetBead(context.Background(), "gd-res1")
	if b.ClosedBy != "slack-bridge" {
		t.Fatalf("closed_by = %q", b.ClosedBy)
	}
}

// The reaper's render keys land from a bridge; anything else in fields is a
// 400, and only a bridge may send fields at all. Successor of
// TestCaptureEscalationResult_FieldsPassthrough.
func TestResult_BridgeRenderAllowlist(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-res2", "", map[string]any{"operation_type": "github_pr_merge"})
	ok := map[string]any{"result_status": "success", "fields": map[string]string{"reaped": "true", "reaped_by": "escalation-reaper", "mr_state": "merged", "approvals_left": "0"}}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-res2/result", testBridgeToken, ok, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-res2"); f["mr_state"] != "merged" || f["reaped"] != "true" {
		t.Fatalf("render keys did not land: %v", f)
	}
	for i, c := range []struct {
		name, bearer string
		fields       map[string]string
	}{
		{"bridge sends approved_by", testBridgeToken, map[string]string{"approved_by": "x"}},
		{"bridge sends executor", testBridgeToken, map[string]string{"executor": "x"}},
		{"bridge sends result_status", testBridgeToken, map[string]string{"result_status": "failure"}},
		// The server-owned impersonation stamps (impersonation.go) stay
		// closed even to the one class that may send fields.
		{"bridge sends impersonated_by", testBridgeToken, map[string]string{"impersonated_by": "owner"}},
		{"bridge sends Impersonated_By", testBridgeToken, map[string]string{"Impersonated_By": "owner"}},
		{"bridge sends created_impersonated_by", testBridgeToken, map[string]string{"created_impersonated_by": "owner"}},
		{"agent sends fields", testAgentToken, map[string]string{"reaped": "true"}},
	} {
		id := fmt.Sprintf("gd-res2x%d", i)
		seedEscalation(t, ms, id, "", map[string]any{"operation_type": "github_pr_merge", "requesting_agent": "crew-b"})
		rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/result", c.bearer,
			map[string]any{"result_status": "success", "fields": c.fields}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", c.name, rec.Code)
		}
		b, _ := ms.GetBead(context.Background(), id)
		if b.Status == model.StatusClosed {
			t.Errorf("%s: a refused result closed the escalation", c.name)
		}
		for k := range fieldsAsMap(b.Fields) {
			if isImpersonationField(k) {
				t.Errorf("%s: a refused result wrote %s", c.name, k)
			}
		}
	}
}

// Agents resolve a verdict record they did not request; for executable
// operations only the server-recorded executor may post the result.
func TestResult_AgentAdmission(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	post := func(id, bearer string) int {
		return doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/result", bearer, map[string]any{"result_status": "success"}, nil).Code
	}
	seedEscalation(t, ms, "gd-v1", "", map[string]any{"operation_type": "github_pr_merge", "requesting_agent": "crew-b"})
	if c := post("gd-v1", testAgentToken2); c != http.StatusForbidden {
		t.Errorf("requester resolving its own verdict: %d, want 403", c)
	}
	if c := post("gd-v1", testAgentToken); c != http.StatusOK {
		t.Errorf("positive control: non-requester verdict: %d, want 200", c)
	}
	seedEscalation(t, ms, "gd-x1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "executor": "crew-a"})
	if c := post("gd-x1", testAgentToken2); c != http.StatusForbidden {
		t.Errorf("non-executor agent on executable op: %d, want 403", c)
	}
	if c := post("gd-x1", testAgentToken); c != http.StatusOK {
		t.Errorf("positive control: recorded executor: %d, want 200", c)
	}
	seedEscalation(t, ms, "gd-x2", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}})
	if c := post("gd-x2", testAgentToken); c != http.StatusForbidden {
		t.Errorf("agent on undispatched executable op: %d, want 403", c)
	}
}

// The requester may withdraw its own escalation (failure, the D6(c)
// withdrawal), but not report it a success.
func TestResult_RequesterMayWithdraw(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-w1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "requesting_agent": "crew-b"})
	if rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-w1/result", testAgentToken2, map[string]any{"result_status": "success"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("requester reporting its own executable op a success: %d, want 403", rec.Code)
	}
	rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-w1/result", testAgentToken2, map[string]any{"result_status": "failure"}, nil)
	requireStatus(t, rec, http.StatusOK)
	if b, _ := ms.GetBead(context.Background(), "gd-w1"); b.ClosedBy != "crew-b" {
		t.Fatalf("closed_by = %q, want crew-b", b.ClosedBy)
	}
}

// An agent spawned by the requester counts as the requester (spec Q8).
func TestResult_AgentSpawnedByRequesterRefused(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	raw, _ := json.Marshal(map[string]string{"agent": "crew-a", "spawned_by": "crew-b"})
	if err := ms.CreateBead(context.Background(), &model.Bead{ID: "crew-a", Title: "crew-a", Type: "agent", Kind: model.KindData, Status: model.StatusOpen, Fields: raw}); err != nil {
		t.Fatal(err)
	}
	seedEscalation(t, ms, "gd-v2", "", map[string]any{"operation_type": "doc_review", "requesting_agent": "crew-b"})
	if rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-v2/result", testAgentToken, map[string]any{"result_status": "success"}, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("agent spawned by the requester: %d, want 403", rec.Code)
	}
	seedEscalation(t, ms, "gd-v3", "", map[string]any{"operation_type": "doc_review", "requesting_agent": "crew-z"})
	if rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-v3/result", testAgentToken, map[string]any{"result_status": "success"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("positive control: same agent, unrelated requester: %d, want 200", rec.Code)
	}
}

func TestEscalationOperationClasses(t *testing.T) {
	for _, c := range []struct {
		op                     string
		verdict, replay, unfit bool
	}{
		{"github_pr_merge", true, false, false},
		{" GitLab_MR_Merge ", true, false, false},
		{"doc_review", true, false, false},
		{"create", false, true, false},
		{"GRANT", false, true, false},
		{"terraform_apply", false, false, true},
		{"aws_iam_grant", false, false, true},
		{"git_push_public", false, false, true},
		{"coopmux_account_sync", false, false, true},
		{"k8s_apply", false, false, false},
	} {
		if got := isVerdictOperation(c.op); got != c.verdict {
			t.Errorf("isVerdictOperation(%q) = %v", c.op, got)
		}
		if got := isServerReplayOperation(c.op); got != c.replay {
			t.Errorf("isServerReplayOperation(%q) = %v", c.op, got)
		}
		if got := isReplayUnfitOperation(c.op); got != c.unfit {
			t.Errorf("isReplayUnfitOperation(%q) = %v", c.op, got)
		}
	}
}

// The current assignee of the requesting task may withdraw too (gd-iCUeCw6UWP),
// with failure only, and only on its own per-agent token.
func TestResult_RequestingTaskAssigneeMayWithdraw(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedWithdrawTask(ms, "gd-t1", "crew-a", "gd-gone", model.StatusInProgress)
	seedWithdrawTask(ms, "gd-t2", "crew-b", "", model.StatusInProgress)
	esc := func(id, task string) {
		seedEscalation(t, ms, id, "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1},
			"requesting_agent": "gd-gone", "requesting_agent_verified": "true", "requesting_task": task})
	}
	post := func(id, bearer, status string, headers map[string]string) int {
		return doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/result", bearer, map[string]any{"result_status": status}, headers).Code
	}
	esc("gd-sw1", "gd-t1")
	if c := post("gd-sw1", testAgentToken, "success", nil); c != http.StatusForbidden {
		t.Errorf("task assignee reporting success: %d, want 403", c)
	}
	if c := post("gd-sw1", testAgentToken2, "failure", nil); c != http.StatusForbidden {
		t.Errorf("assignee of a different task withdrawing: %d, want 403", c)
	}
	if c := post("gd-sw1", testStaticToken, "failure", map[string]string{"X-Actor": "crew-a"}); c != http.StatusForbidden {
		t.Errorf("static token as the assignee withdrawing: %d, want 403", c)
	}
	if c := post("gd-sw1", testAgentToken, "failure", nil); c != http.StatusOK {
		t.Fatalf("task assignee withdrawing: %d, want 200", c)
	}
	if b, _ := ms.GetBead(context.Background(), "gd-sw1"); b.ClosedBy != "crew-a" {
		t.Fatalf("closed_by = %q, want crew-a", b.ClosedBy)
	}
	// requesting_task repointed at crew-b's own task: the requester was not
	// spawned on it, so no widening.
	esc("gd-sw2", "gd-t2")
	if c := post("gd-sw2", testAgentToken2, "failure", nil); c != http.StatusForbidden {
		t.Errorf("requesting_task repointed at the caller's task: %d, want 403", c)
	}
}
