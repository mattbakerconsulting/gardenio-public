package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestCallerClassFromContext(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	oidcOp := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "owner@example.com"}), authMethodOIDC), []string{RoleOperator})
	oidcViewer := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "v@example.com"}), authMethodOIDC), []string{RoleViewer})
	scoped := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "m@example.com"}), authMethodOIDC), []string{RoleProjectMember})
	edgeV2 := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "owner@example.com"}), authMethodOIDC), []string{RoleOperator})
	edgeV1 := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "owner@example.com"}), authMethodEdge)
	// No auth path mints Type "static" any more (gd-ke6PC7bRto); a stray one
	// still resolves nothing.
	static := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "crew-a"}), authMethodStatic)
	machine := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "machine", Subject: "machine:gardenio-controller"}), authMethodMachine)
	agent := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), authMethodStatic), "agent-gd-a1")
	bridge := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), authMethodStatic), "slack-bridge")
	rotatedBridge := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "bridge:jira"}), authMethodStatic), "jira")
	agentClaimsBridge := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "bridge:slack"}), authMethodStatic), "agent-gd-a1")
	other := WithFleet(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "qa-fleet"}), authMethodStatic), "qa-fleet")
	window := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "window", Subject: windowSubjectPrefix + "gd-w1"}), authMethodWindow), []string{RoleOperator})
	none := withAuthMethod(context.Background(), authMethodNone)
	for name, c := range map[string]struct {
		ctx  context.Context
		want callerClass
	}{
		"oidc operator":       {oidcOp, classOperator},
		"oidc viewer":         {oidcViewer, classOther},
		"oidc project member": {scoped, classOperator},
		"edge v2 operator":    {edgeV2, classOperator},
		"edge v1":             {edgeV1, classOther},
		"legacy static":       {static, classOther},
		"machine":             {machine, classMachine},
		"agent":               {agent, classAgent},
		"bridge":              {bridge, classBridge},
		"rotated bridge":      {rotatedBridge, classBridge},
		"agent claims bridge": {agentClaimsBridge, classAgent},
		"other fleet":         {other, classOther},
		"runner":              {grantRunnerCtx("oidc:owner@example.com"), classRunner},
		"land principal":      {grantLandCtx("pet"), classOther},
		"window principal":    {window, classOther},
		"auth disabled":       {none, classNone},
	} {
		if got := s.callerClassFromContext(c.ctx); got != c.want {
			t.Errorf("%s: class %q, want %q", name, got, c.want)
		}
	}
}

// Doc 11 defect 1: /reject and /result were open to every static-token pod.
// The static token itself is gone (TestRetiredStaticToken_RefusedOnHTTPAndGRPC);
// testStaticToken here is a privileged fleet token with no escalation class.
// Positive controls: an operator rejects, a bridge posts a result, an agent
// posts a result but cannot reject.
func TestRejectResult_CallerClasses(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient}) // the operator rejects on the page
	cases := []struct {
		name, bearer, verb string
		want               int
	}{
		{"privileged fleet reject", testStaticToken, "reject", http.StatusForbidden},
		{"agent reject", testAgentToken, "reject", http.StatusForbidden},
		{"bridge reject", testBridgeToken, "reject", http.StatusForbidden},
		{"unlisted fleet reject", testOtherFleetToken, "reject", http.StatusForbidden},
		{"privileged fleet result", testStaticToken, "result", http.StatusForbidden},
		{"unlisted fleet result", testOtherFleetToken, "result", http.StatusForbidden},
		{"operator reject", "owner@example.com", "reject", http.StatusOK},
		{"operator result", "owner@example.com", "result", http.StatusOK},
		{"bridge result", testBridgeToken, "result", http.StatusOK},
		{"agent result", testAgentToken, "result", http.StatusOK},
	}
	for i, c := range cases {
		id := fmt.Sprintf("gd-esc%d", i)
		fields := map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}}
		if c.name == "agent result" {
			// Task 10 narrows which agent: the recorded executor may.
			fields["executor"] = "crew-a"
		}
		seedEscalation(t, ms, id, "", fields)
		body := map[string]any{"reason": "no"}
		if c.verb == "result" {
			body = map[string]any{"result_status": "success"}
		}
		rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/"+c.verb, c.bearer, body, nil)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d; body %s", c.name, rec.Code, c.want, rec.Body.String())
		}
		if c.want == http.StatusForbidden {
			if st := escFields(t, ms, id); st["result_status"] != nil || st["rejected_by"] != nil {
				t.Errorf("%s: refused call still wrote fields %v", c.name, st)
			}
		}
	}
}

// An agent token passes the class gate on /result (the handler narrows which
// agent, Task 10); it is never refused here for its class. On /reject it is.
func TestRejectResult_AgentPassesClassGateOnResultOnly(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	for verb, refusedByClass := range map[string]bool{"result": false, "reject": true} {
		id := "gd-escagent-" + verb
		seedEscalation(t, ms, id, "", map[string]any{"operation_type": "manual_operator_action"})
		rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/"+verb, testAgentToken, map[string]any{"result_status": "success", "reason": "no"}, nil)
		if got := strings.Contains(rec.Body.String(), "caller class: agent"); got != refusedByClass {
			t.Errorf("%s: refused by class gate = %v, want %v; status %d body %s", verb, got, refusedByClass, rec.Code, rec.Body.String())
		}
	}
}

// A runner (gr_ bearer) posts a result for its project's escalation; reject is
// off its route allowlist, so it never reaches the class gate there.
func TestRejectResult_RunnerResultAdmitted(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	rec := doRunnerBearer(h, http.MethodPost, "/v1/escalations/gd-e1/result", token, `{"result_status":"success"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("runner result: status %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if escFields(t, ms, "gd-e1")["result_status"] != "success" {
		t.Fatal("runner result not recorded")
	}
	if rec := doRunnerBearer(h, http.MethodPost, "/v1/escalations/gd-e1/reject", token, `{"reason":"no"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("runner reject: status %d, want 403", rec.Code)
	}
}

// Positive control (spec D7 with the D6 gate on): an agent token passes the
// class gate on /result but is still refused a result it has no standing for.
// A legacy escalation (no execute_by) is executed by the operator lane, so the
// agent gets 403 and the escalation stays open. (The operator lane's own
// result is TestCaptureEscalationResult_GateA_ApprovedAndExecutor.)
func TestRejectResult_AgentRefusedResultWithoutStanding(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-escstand", "", map[string]any{
		"operation_type": "helm_deploy", "approved_by": "owner@example.com", "approved_at": "2026-10-20T00:00:00Z",
	})
	body := map[string]any{"result_status": "success"}
	rec := doBearerJSON(t, h, "POST", "/v1/escalations/gd-escstand/result", testAgentToken, body, nil)
	if rec.Code != http.StatusForbidden || strings.Contains(rec.Body.String(), "caller class:") {
		t.Fatalf("agent result without standing: status %d body %s, want a D6 403 (not a class refusal)", rec.Code, rec.Body.String())
	}
	if ms.beads["gd-escstand"].Status != model.StatusOpen {
		t.Fatal("a refused agent result closed the escalation")
	}
}
