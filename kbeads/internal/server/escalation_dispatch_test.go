package server

import (
	"context"
	"net/http"
	"sync"
	"testing"
)

func TestEscalationDispatchFor(t *testing.T) {
	for _, c := range []struct {
		f    map[string]string
		want string
	}{
		{map[string]string{"operation_type": "github_pr_merge", "execution_target": "local"}, dispatchNone},
		{map[string]string{"operation_type": "create", "operation_spec": `{"title":"x"}`}, dispatchServer},
		{map[string]string{"operation_type": "create"}, dispatchNone},
		{map[string]string{"operation_type": "k8s_apply", "execution_target": "local"}, dispatchLocal},
		{map[string]string{"operation_type": "k8s_apply", "execution_target": "service_account"}, dispatchServiceAccount},
		{map[string]string{"operation_type": "manual_operator_action"}, dispatchNone},
		{map[string]string{"operation_type": "terraform_apply", "execution_target": "local"}, dispatchNone},
		{map[string]string{"operation_type": "aws_iam_update", "execution_target": "service_account"}, dispatchNone},
	} {
		if got := escalationDispatchFor(c.f); got != c.want {
			t.Errorf("%v: %q, want %q", c.f, got, c.want)
		}
	}
}

// Approve records execution_dispatch exactly once; two concurrent approves of a
// local escalation produce one claim and one 409.
func TestApprove_ClaimsOneDispatch(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedEscalation(t, ms, "gd-d1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "local"})
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = doBearerJSON(t, h, "POST", "/v1/escalations/gd-d1/approve", "owner@example.com", map[string]any{}, nil).Code
		}(i)
	}
	wg.Wait()
	if !((codes[0] == http.StatusOK && codes[1] == http.StatusConflict) || (codes[0] == http.StatusConflict && codes[1] == http.StatusOK)) {
		t.Fatalf("two approves: %v, want one 200 and one 409", codes)
	}
	if f := escFields(t, ms, "gd-d1"); f["execution_dispatch"] != "local" {
		t.Fatalf("execution_dispatch = %v", f["execution_dispatch"])
	}
}

// Review Focus 3: a server replay that failed can be approved again; the
// existing replay claim, not the dispatch claim, serialises the retry.
func TestApprove_ServerReplayFailureCanBeRetried(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedEscalation(t, ms, "gd-d2", "", map[string]any{"operation_type": "update", "operation_spec": map[string]any{"title": "x"}, "target_bead_id": "gd-missing"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-d2/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-d2"); f["execution_dispatch"] != "server" || f["result_status"] == "success" {
		t.Fatalf("first approve: %v", f)
	}
	if _, proceed, err := s.claimEscalationDispatch(context.Background(), ms.beads["gd-d2"]); err != nil || !proceed {
		t.Fatalf("re-approve of a server dispatch must proceed to replay: proceed=%v err=%v", proceed, err)
	}
}

// gb escalation approve --target: the override rides the approve request, is
// recorded with the approval, and chooses the dispatch.
func TestApprove_ExecutionTargetOverride(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedPrivilegedRole(t, ms, "k8s-live")
	seedEscalation(t, ms, "gd-d3", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "spawn_role": "k8s-live", "execution_script": "kubectl apply -f x"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-d3/approve", "owner@example.com", map[string]any{"execution_target": "server"}, nil), http.StatusBadRequest)
	if f := escFields(t, ms, "gd-d3"); f["approved_by"] != nil || f["execution_dispatch"] != nil {
		t.Fatalf("a refused override must write nothing: %v", f)
	}
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-d3/approve", "owner@example.com", map[string]any{"execution_target": "service_account"}, nil), http.StatusOK)
	f := escFields(t, ms, "gd-d3")
	if f["execution_target"] != "service_account" || f["execution_dispatch"] != "service_account" {
		t.Fatalf("override not recorded: target=%v dispatch=%v", f["execution_target"], f["execution_dispatch"])
	}
	// A later approve may not re-route it.
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-d3/approve", "owner@example.com", map[string]any{"execution_target": "local"}, nil), http.StatusConflict)
	if f := escFields(t, ms, "gd-d3"); f["execution_target"] != "service_account" {
		t.Fatalf("execution_target rewritten after dispatch: %v", f["execution_target"])
	}
}

// A verdict record dispatches to nothing; approving it again stays harmless.
func TestApprove_VerdictDispatchesNone(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedEscalation(t, ms, "gd-d4", "", map[string]any{"operation_type": "github_pr_merge", "execution_target": "local"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-d4/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-d4/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-d4"); f["execution_dispatch"] != "none" {
		t.Fatalf("execution_dispatch = %v, want none", f["execution_dispatch"])
	}
}
