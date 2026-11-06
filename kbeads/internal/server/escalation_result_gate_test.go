package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func approvedMergeDesk(requester string) map[string]any {
	f := mergeDeskEscalation(requester)
	f["approved_by"] = "reviewer-a"
	f["approved_at"] = "2026-10-20T00:00:00Z"
	f["approval_method"] = "coordinator"
	f["approved_as"] = "coordinator:merge-desk"
	return f
}

func postResult(t *testing.T, s *BeadsServer, ctx context.Context, id, status string) int {
	t.Helper()
	return callEscalationVerb(t, ctx, s.handleCaptureEscalationResult, id, "result", map[string]any{"result_status": status, "executor": "test"}).Code
}

// CONTRACT ISSUE 23: with the flag unset nothing changes, so the merge desk's
// unapproved closes keep working until Task 14. The merge desk posts on its
// per-agent token (caller class agent). A fleet token with no escalation
// standing is refused by caller class whatever the flag says (P7 Task 9,
// gd-nPfSNHTkQQ); the shared static token it used to pin here is gone
// altogether (gd-ke6PC7bRto, TestRetiredStaticToken_RefusedOnHTTPAndGRPC).
func TestCaptureEscalationResult_GateOffUnchanged(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "")
	_, ms, h := newClassedTestServer(t)
	post := func(id, bearer string) *httptest.ResponseRecorder {
		t.Helper()
		return doBearerJSON(t, h, http.MethodPost, "/v1/escalations/"+id+"/result", bearer, map[string]any{"result_status": "success"}, nil)
	}
	seedRoutedEscalation(ms, "gd-off1", "gardenio", mergeDeskEscalation("gd-crew1"))
	if rec := post("gd-off1", testAgentToken); rec.Code != http.StatusOK {
		t.Fatalf("gate off, unapproved agent result: got %d %s, want 200 (today's behaviour)", rec.Code, rec.Body.String())
	}
	if ms.beads["gd-off1"].Status != model.StatusClosed {
		t.Fatalf("status = %s, want closed", ms.beads["gd-off1"].Status)
	}

	// A classless fleet token is refused by caller class, with the gate off too.
	seedRoutedEscalation(ms, "gd-off0", "gardenio", mergeDeskEscalation("gd-crew1"))
	if rec := post("gd-off0", testOtherFleetToken); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "caller class: other") {
		t.Fatalf("gate off, other-fleet result: got %d %s, want 403 caller class: other", rec.Code, rec.Body.String())
	}

	// The same agent request with the gate on is refused.
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	seedRoutedEscalation(ms, "gd-off2", "gardenio", mergeDeskEscalation("gd-crew1"))
	if rec := post("gd-off2", testAgentToken); rec.Code != http.StatusForbidden {
		t.Fatalf("gate on, unapproved agent result: got %d, want 403", rec.Code)
	}
	for _, id := range []string{"gd-off0", "gd-off2"} {
		if ms.beads[id].Status != model.StatusOpen {
			t.Fatalf("a refused result closed %s", id)
		}
	}
}

// D6(a): approved, and the caller is in execute_by.
func TestCaptureEscalationResult_GateA_ApprovedAndExecutor(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()

	seedRoutedEscalation(ms, "gd-a1", "gardenio", approvedMergeDesk("gd-crew1"))
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-a1", "success"); code != http.StatusForbidden {
		t.Fatalf("non-executor on an approved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, agentCtx("gd-rev1", "reviewer-a", "gardenio"), "gd-a1", "success"); code != http.StatusOK {
		t.Fatalf("executor on an approved escalation: got %d, want 200", code)
	}

	// Unapproved: even the executor is refused.
	seedRoutedEscalation(ms, "gd-a2", "gardenio", mergeDeskEscalation("gd-crew1"))
	if code := postResult(t, s, agentCtx("gd-rev1", "reviewer-a", "gardenio"), "gd-a2", "success"); code != http.StatusForbidden {
		t.Fatalf("executor on an unapproved escalation: got %d, want 403", code)
	}

	// Legacy (no execute_by) is executed by the operator (CONTRACT ISSUE 19).
	seedRoutedEscalation(ms, "gd-a3", "gardenio", map[string]any{"operation_type": "helm_deploy", "approved_by": "owner@example.com", "approved_at": "2026-10-20T00:00:00Z"})
	if code := postResult(t, s, agentCtx("gd-rev1", "reviewer-a", "gardenio"), "gd-a3", "success"); code != http.StatusForbidden {
		t.Fatalf("agent on a legacy approved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, operatorCtx("owner@example.com"), "gd-a3", "success"); code != http.StatusOK {
		t.Fatalf("operator on a legacy approved escalation: got %d, want 200", code)
	}
}

// D6(b): execute_by=server only takes the daemon's own result.
func TestCaptureEscalationResult_GateB_ServerExecuted(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	f := map[string]any{"operation_type": "close", "approvers": []string{"operator"}, "execute_by": "server", "held_by": "server", "approved_by": "owner@example.com", "approved_at": "2026-10-20T00:00:00Z"}
	seedRoutedEscalation(ms, "gd-b1", "gardenio", f)
	if code := postResult(t, s, operatorCtx("owner@example.com"), "gd-b1", "success"); code != http.StatusForbidden {
		t.Fatalf("operator result on a server-executed escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, withMachineWrite(operatorCtx("owner@example.com")), "gd-b1", "success"); code != http.StatusOK {
		t.Fatalf("daemon result on a server-executed escalation: got %d, want 200", code)
	}
}

// D6(c): the requester may withdraw with failure, and only with failure.
func TestCaptureEscalationResult_GateC_RequesterWithdrawal(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-c1", "gardenio", mergeDeskEscalation("gd-crew1"))
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-c1", "success"); code != http.StatusForbidden {
		t.Fatalf("requester success on an unapproved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, agentCtx("gd-crew9", "crew-z", "gardenio"), "gd-c1", "failure"); code != http.StatusForbidden {
		t.Fatalf("non-requester failure on an unapproved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-c1", "failure"); code != http.StatusOK {
		t.Fatalf("requester withdrawal: got %d, want 200", code)
	}
}

// D6(d): an allowlisted result machine (unused at rollout, CONTRACT ISSUE 21).
func TestCaptureEscalationResult_GateD_ResultMachine(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-d1", "gardenio", mergeDeskEscalation("gd-crew1"))
	if code := postResult(t, s, machineCtx("gardenio-gitlab-bridge"), "gd-d1", "success"); code != http.StatusForbidden {
		t.Fatalf("unlisted machine: got %d, want 403", code)
	}
	s.SetEscalationResultMachines([]string{"gardenio-gitlab-bridge"})
	if code := postResult(t, s, machineCtx("gardenio-gitlab-bridge"), "gd-d1", "success"); code != http.StatusOK {
		t.Fatalf("listed result machine: got %d, want 200", code)
	}
}

// D6(e) (gd-fs9lGoSLDd): the merge desk sends back an unapproved merge with a
// failure, while its lane holds it or after a hand-up (gd-0v8Ftju0z0). Nothing else widens: no approval is
// stamped and the requesting task stays open (failure never cascades).
func TestCaptureEscalationResult_GateE_DeskSendBack(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedWithdrawTask(ms, "gd-sbtask", "crew-a", "", model.StatusInProgress)
	desk, outsider := agentCtx("gd-rev1", "reviewer-a", "gardenio"), agentCtx("gd-crew9", "crew-z", "gardenio")
	t0, t1 := "2026-11-05T05:00:00Z", "2026-11-05T05:10:00Z"
	esc := func(kv ...string) map[string]any {
		f := mergeDeskEscalation("gd-crew1")
		f["requesting_task"], f["close_requesting_task"] = "gd-sbtask", true
		for i := 0; i+1 < len(kv); i += 2 {
			if f[kv[i]] = kv[i+1]; kv[i+1] == "" {
				delete(f, kv[i])
			}
		}
		return f
	}
	for i, c := range []struct {
		name   string
		ctx    context.Context
		f      map[string]any
		status string
		want   int
	}{
		{"desk fails a github_pr_merge", desk, esc(), "failure", http.StatusOK},
		{"desk fails a gitlab_mr_merge", desk, esc("operation_type", "gitlab_mr_merge"), "failure", http.StatusOK},
		{"desk posts success unapproved", desk, esc(), "success", http.StatusForbidden},
		{"desk fails a non-merge in its lane", desk, esc("operation_type", "helm_deploy"), "failure", http.StatusForbidden},
		{"agent outside the lane fails a merge", outsider, esc(), "failure", http.StatusForbidden},
		{"desk fails an execute_by=requester merge", desk, esc("execute_by", "requester", "held_by", "requester"), "failure", http.StatusForbidden},
		{"desk fails a merge routed to the operator", desk, esc("held_by", "operator"), "failure", http.StatusForbidden},
		{"desk fails a merge with no held_by", desk, esc("held_by", ""), "failure", http.StatusForbidden},
		{"desk fails a merge the laptop holds", desk, esc("held_by", "coordinator:laptop"), "failure", http.StatusForbidden},
		{"desk fails a merge handed up to the operator", desk, esc("held_by", "operator", "held_at", t0, "handed_up_at", t0), "failure", http.StatusOK},
		{"desk posts success on a handed-up merge", desk, esc("held_by", "operator", "held_at", t0, "handed_up_at", t0), "success", http.StatusForbidden},
		{"desk fails a handed-up merge then routed", desk, esc("held_by", "operator", "held_at", t1, "handed_up_at", t0), "failure", http.StatusForbidden},
		{"desk fails a handed-up merge executed by the operator", desk, esc("execute_by", "operator", "held_by", "operator", "held_at", t0, "handed_up_at", t0), "failure", http.StatusForbidden},
		{"outsider fails a handed-up merge", outsider, esc("held_by", "operator", "held_at", t0, "handed_up_at", t0), "failure", http.StatusForbidden},
	} {
		id := fmt.Sprintf("gd-sb%d", i)
		seedRoutedEscalation(ms, id, "gardenio", c.f)
		if code := postResult(t, s, c.ctx, id, c.status); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
		if closed := ms.beads[id].Status == model.StatusClosed; closed != (c.want == http.StatusOK) {
			t.Errorf("%s: closed = %v", c.name, closed)
		}
		if v := escField(t, ms, id, "approved_by"); v != nil {
			t.Errorf("%s: approved_by = %v", c.name, v)
		}
	}
	if st := ms.beads["gd-sbtask"].Status; st != model.StatusInProgress {
		t.Fatalf("requesting task status = %s after a send-back, want in_progress", st)
	}
}

// D6(e) after a real hand-up: applyHandUp stamps held_at and handed_up_at with
// one time (heldByHandUp), so the desk can still send back a merge it handed
// up for approval; a route afterwards takes that back (gd-0v8Ftju0z0).
func TestCaptureEscalationResult_GateE_DeskSendBackAfterHandUp(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	desk := agentCtx("gd-rev1", "reviewer-a", "gardenio")
	for _, id := range []string{"gd-hu1", "gd-hu2"} {
		seedRoutedEscalation(ms, id, "gardenio", mergeDeskEscalation("gd-crew1"))
		if rec := callEscalationVerb(t, desk, s.handleHandUpEscalation, id, "hand-up", map[string]any{"reason": "needs the owner"}); rec.Code != http.StatusOK {
			t.Fatalf("%s hand-up: got %d %s", id, rec.Code, rec.Body.String())
		}
	}
	if got := escField(t, ms, "gd-hu1", "held_by"); got != "operator" {
		t.Fatalf("held_by after hand-up = %v, want operator", got)
	}
	if code := postResult(t, s, desk, "gd-hu1", "failure"); code != http.StatusOK {
		t.Fatalf("desk failure after hand-up: got %d, want 200", code)
	}
	ms.beads["gd-hu2"].Fields = setFieldValue(ms.beads["gd-hu2"].Fields, "held_at", "2099-01-01T00:00:00Z")
	if code := postResult(t, s, desk, "gd-hu2", "failure"); code != http.StatusForbidden {
		t.Fatalf("desk failure after hand-up then route: got %d, want 403", code)
	}
}

// D6(g) (gd-UNcBYJ9TZP): an approved escalation its coordinator handed up to
// the operator is the operator's to finish, though execute_by still names the
// coordinator; once it is held there by anything but that hand-up, it is not.
func TestCaptureEscalationResult_GateG_OperatorFinishesHandUp(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	handedUp := func() map[string]any {
		return map[string]any{
			"operation_type": "kbeads_admin_write", "execute_by": "coordinator:cluster", "held_by": "operator",
			"approved_by": "owner@example.com", "approved_at": "2026-11-04T03:00:00Z",
			"handed_up_at": "2026-11-05T08:01:54Z", "held_at": "2026-11-05T08:01:54Z",
		}
	}
	seedRoutedEscalation(ms, "gd-g1", "gardenio", handedUp())
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-g1", "success"); code != http.StatusForbidden {
		t.Fatalf("agent on a handed-up approved escalation: got %d, want 403", code)
	}
	if code := postResult(t, s, operatorCtx("owner@example.com"), "gd-g1", "success"); code != http.StatusOK {
		t.Fatalf("operator on a handed-up approved escalation: got %d, want 200", code)
	}

	rerouted := handedUp()
	rerouted["held_at"] = "2026-11-05T09:00:00Z"
	seedRoutedEscalation(ms, "gd-g2", "gardenio", rerouted)
	if code := postResult(t, s, operatorCtx("owner@example.com"), "gd-g2", "success"); code != http.StatusForbidden {
		t.Fatalf("operator after a later re-route: got %d, want 403", code)
	}

	unapproved := handedUp()
	delete(unapproved, "approved_by")
	delete(unapproved, "approved_at")
	seedRoutedEscalation(ms, "gd-g3", "gardenio", unapproved)
	if code := postResult(t, s, operatorCtx("owner@example.com"), "gd-g3", "success"); code != http.StatusForbidden {
		t.Fatalf("operator on an unapproved handed-up escalation: got %d, want 403", code)
	}
}

// seedWithdrawTask seeds a task with assignee and the requester's agent bead
// spawned on it (task_id), the anchor callerMayWithdraw checks.
func seedWithdrawTask(ms *mockStore, taskID, assignee, requesterBead string, status model.Status) {
	ms.beads[taskID] = &model.Bead{ID: taskID, Kind: model.KindIssue, Type: model.TypeTask, Title: taskID, Status: status, Assignee: assignee, Labels: []string{"project:gardenio"}}
	ms.labels[taskID] = []string{"project:gardenio"}
	if requesterBead != "" {
		raw, _ := json.Marshal(map[string]any{"role": "crew", "project": "gardenio", "agent": "crew-gone", "task_id": taskID})
		ms.beads[requesterBead] = &model.Bead{ID: requesterBead, Kind: model.KindData, Type: model.BeadType("agent"), Title: requesterBead, Status: model.StatusClosed, Fields: raw}
	}
}

func withdrawEscalation(requester, task string) map[string]any {
	f := mergeDeskEscalation(requester)
	f["requesting_task"] = task
	return f
}

// D6(c) widened (gd-iCUeCw6UWP): the current assignee of the requesting task
// may withdraw, with failure only, once the requester is gone.
func TestCaptureEscalationResult_GateC_TaskAssigneeWithdrawal(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedAgent(ms, "gd-crew3", "crew", "gardenio")
	successor := agentCtx("gd-crew3", "crew-c", "gardenio")
	seedWithdrawTask(ms, "gd-task1", "crew-c", "gd-gone1", model.StatusInProgress)
	seedWithdrawTask(ms, "gd-task2", "crew-c", "", model.StatusInProgress)
	seedWithdrawTask(ms, "gd-task3", "crew-c", "gd-gone3", model.StatusClosed)
	seedWithdrawTask(ms, "gd-task4", "", "gd-gone4", model.StatusInProgress)
	seedWithdrawTask(ms, "gd-task5", "reviewer-a", "gd-gone5", model.StatusInProgress)
	seedWithdrawTask(ms, "gd-task6", "gd-crew3", "gd-gone6", model.StatusInProgress)

	staticAsAssignee := WithTokenProject(WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-c"}), "qa-fleet"), "gardenio")
	for i, c := range []struct {
		name   string
		ctx    context.Context
		f      map[string]any
		status string
		want   int
	}{
		{"assignee withdraws", successor, withdrawEscalation("gd-gone1", "gd-task1"), "failure", http.StatusOK},
		{"assignee recorded by bead id withdraws", successor, withdrawEscalation("gd-gone6", "gd-task6"), "failure", http.StatusOK},
		{"assignee posts success", successor, withdrawEscalation("gd-gone1", "gd-task1"), "success", http.StatusForbidden},
		{"unrelated agent posts failure", agentCtx("gd-crew9", "crew-z", "gardenio"), withdrawEscalation("gd-gone1", "gd-task1"), "failure", http.StatusForbidden},
		{"static token as the assignee", staticAsAssignee, withdrawEscalation("gd-gone1", "gd-task1"), "failure", http.StatusForbidden},
		{"assignee of a different task", agentCtx("gd-crew9", "crew-z", "gardenio"), withdrawEscalation("gd-gone1", "gd-task1"), "failure", http.StatusForbidden},
		// requesting_task is editable free text: pointing it at the caller's own
		// task does not help unless the requester was spawned on that task.
		{"requesting_task repointed at the caller's task", successor, withdrawEscalation("gd-gone1", "gd-task2"), "failure", http.StatusForbidden},
		{"requesting task closed", successor, withdrawEscalation("gd-gone3", "gd-task3"), "failure", http.StatusForbidden},
		{"requesting task has no assignee", successor, withdrawEscalation("gd-gone4", "gd-task4"), "failure", http.StatusForbidden},
		{"assignee is someone else", successor, withdrawEscalation("gd-gone5", "gd-task5"), "failure", http.StatusForbidden},
	} {
		id := fmt.Sprintf("gd-cw%d", i)
		seedRoutedEscalation(ms, id, "gardenio", c.f)
		if code := postResult(t, s, c.ctx, id, c.status); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
		if closed := ms.beads[id].Status == model.StatusClosed; closed != (c.want == http.StatusOK) {
			t.Errorf("%s: closed = %v", c.name, closed)
		}
	}

	// unverified requester: no widening.
	f := withdrawEscalation("gd-gone1", "gd-task1")
	f["requesting_agent_verified"] = "false"
	seedRoutedEscalation(ms, "gd-cwu", "gardenio", f)
	if code := postResult(t, s, successor, "gd-cwu", "failure"); code != http.StatusForbidden {
		t.Errorf("unverified requester: got %d, want 403", code)
	}
}

// requester is also an execute_by lane; the widening must not reach it, so a
// task assignee cannot post success on an approved execute_by=requester
// escalation while the original requester still can.
func TestCaptureEscalationResult_TaskAssigneeNotInRequesterExecuteLane(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedAgent(ms, "gd-crew3", "crew", "gardenio")
	seedWithdrawTask(ms, "gd-task1", "crew-c", "gd-crew1", model.StatusInProgress)
	approved := func() map[string]any {
		f := withdrawEscalation("gd-crew1", "gd-task1")
		f["approvers"] = []string{"operator"}
		f["execute_by"], f["held_by"] = "requester", "requester"
		f["approved_by"], f["approved_at"] = "owner@example.com", "2026-10-20T00:00:00Z"
		return f
	}
	seedRoutedEscalation(ms, "gd-er1", "gardenio", approved())
	if code := postResult(t, s, agentCtx("gd-crew3", "crew-c", "gardenio"), "gd-er1", "success"); code != http.StatusForbidden {
		t.Fatalf("task assignee success on execute_by=requester: got %d, want 403", code)
	}
	seedRoutedEscalation(ms, "gd-er2", "gardenio", approved())
	if code := postResult(t, s, agentCtx("gd-crew3", "crew-c", "gardenio"), "gd-er2", "failure"); code != http.StatusOK {
		t.Fatalf("task assignee withdrawal on execute_by=requester: got %d, want 200", code)
	}
	// Positive control: the original requester, dispatched as executor, may.
	f := approved()
	f["executor"] = "crew-a"
	seedRoutedEscalation(ms, "gd-er3", "gardenio", f)
	if code := postResult(t, s, agentCtx("gd-crew1", "crew-a", "gardenio"), "gd-er3", "success"); code != http.StatusOK {
		t.Fatalf("original requester success on execute_by=requester: got %d, want 200", code)
	}
}

// D6(a) needs authority, not presence (gd-JvbxHtR6bx, design gd-B2kAow1r1O
// step A): the owner's kd/gb login (gardenio-cli) records an approved
// operator-executed result and is stamped with its client, but still cannot
// approve, and a non-admin CLI token cannot record one.
func TestCaptureEscalationResult_GateA_AuthorityOnAnyClient(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "true")
	s, ms := newLaneTestServer()
	seedTenantLand(ms, "acme", nil)
	cli := func(email string, roles []string, groups ...string) context.Context {
		ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: email, ClientID: DeviceGrantClientID})
		return WithGroups(WithRoles(withAuthMethod(ctx, authMethodOIDC), roles), groups)
	}
	owner := cli("owner@example.com", []string{RoleAdmin})
	for i, c := range []struct {
		name     string
		ctx      context.Context
		approved bool
		want     int
	}{
		{"owner CLI, approved operator item", owner, true, http.StatusOK},
		{"owner CLI, unapproved operator item", owner, false, http.StatusForbidden},
		{"non-admin CLI, approved operator item", cli("op@example.com", []string{RoleOperator}), true, http.StatusForbidden},
		// Authority is an OIDC admin's: a service credential carrying RoleAdmin is not.
		{"fleet token with RoleAdmin, approved operator item", withAuthMethod(WithRoles(WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), "slack-bridge"), []string{RoleAdmin}), authMethodStatic), true, http.StatusForbidden},
		{"machine with RoleAdmin, approved operator item", machineCtx("gardenio-controller", RoleAdmin), true, http.StatusForbidden},
	} {
		id, f := fmt.Sprintf("gd-cli%d", i), map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "operator", "held_by": "operator"}
		if c.approved {
			f["approved_by"], f["approved_at"] = "owner@example.com", "2026-11-04T00:00:00Z"
		}
		seedRoutedEscalation(ms, id, "gardenio", f)
		if code := postResult(t, s, c.ctx, id, "success"); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		} else if got := escField(t, ms, id, "result_client"); c.want == http.StatusOK && got != DeviceGrantClientID {
			t.Errorf("%s: result_client = %v, want %s", c.name, got, DeviceGrantClientID)
		}
	}
	// land-owner:<land> is the owners group on any client. The result POST
	// itself is refused earlier for a scoped tenant principal (write-authz).
	land := laneRef{Kind: laneLandOwner, Name: "acme", Raw: "land-owner:acme"}
	for group, want := range map[string]bool{"/project-acme/owners": true, "/project-acme/members": false} {
		if in, err := s.callerHasLaneAuthority(cli("o@acme.test", []string{RoleProjectMember}, group), land, nil, escalationPolicy{}); err != nil || in != want {
			t.Errorf("land-owner authority for %s on gardenio-cli = %v, %v; want %v", group, in, err, want)
		}
	}
	seedRoutedEscalation(ms, "gd-cli-floor", "gardenio", map[string]any{"operation_type": "credential_rotation", "approvers": []string{"operator"}, "execute_by": "operator", "held_by": "operator"})
	if rec := callEscalationVerb(t, owner, s.handleApproveEscalation, "gd-cli-floor", "approve", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("owner CLI approving a floor item: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-cli-floor")
}

// A local runner's result resolves to its owner (local-agents spec §4.2 D6),
// so result_client names the runner: the record never reads as the person's
// own login (P10.D gd-4LKA42orC2). An OIDC result keeps its OAuth client.
func TestCaptureEscalationResult_RunnerStampsResultClient(t *testing.T) {
	t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", "")
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-runres", "gardenio", map[string]any{"operation_type": "helm_deploy", "execution_target": "local"})
	ctx := WithRunnerProjects(runnerCallerCtx("gd-runner1", "oidc:owner@example.com"), []string{"gardenio"})
	if code := postResult(t, s, ctx, "gd-runres", "success"); code != http.StatusOK {
		t.Fatalf("runner result: got %d, want 200", code)
	}
	if got := escField(t, ms, "gd-runres", "result_client"); got != "runner:gd-runner1" {
		t.Errorf("result_client = %v, want runner:gd-runner1", got)
	}
}

// gd-mlVNJxZ1yC: a later plan-gardener run withdraws the infra spawn ask an
// earlier run on its seat filed (gd-iYvyVsUaYy, gd-Tf7ViuofOq got 403), and
// the filer withdraws its own server-executed ask, with the D6 gate off or on.
func TestCaptureEscalationResult_SeatSuccessorWithdrawal(t *testing.T) {
	const seat = "gardenio/plan-gardener/gardenio-plan-gardener-for-gd-plan1"
	seatAgent := func(ms *mockStore, id, seatKey string, status model.Status) {
		raw, _ := json.Marshal(map[string]any{"role": "plan-gardener", "project": "gardenio", "seat_key": seatKey, "task_id": "gd-plan1"})
		ms.beads[id] = &model.Bead{ID: id, Kind: model.KindConfig, Type: model.BeadType("agent"), Title: id, Status: status, Fields: raw, Labels: []string{"project:gardenio"}}
		ms.labels[id] = []string{"project:gardenio"}
	}
	spawnAsk := func(requester string, kv ...string) map[string]any {
		f := map[string]any{
			"operation_type": "create", "spawn_role": "infra", "policy_rule": "escalation#3",
			"approvers":  []string{"oidc:owner@example.com"},
			"execute_by": "server", "held_by": "server", "operator_only": false,
			"requesting_agent": requester, "requesting_agent_verified": "true",
			"requesting_task": "gd-plan1", "requesting_plan": "gd-plan1",
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if f[kv[i]] = kv[i+1]; kv[i+1] == "" {
				delete(f, kv[i])
			}
		}
		return f
	}
	for _, gate := range []string{"false", "true"} {
		t.Run("gate="+gate, func(t *testing.T) {
			t.Setenv("BEADS_ESCALATION_RESULT_REQUIRES_APPROVAL", gate)
			s, ms := newLaneTestServer()
			seedWithdrawTask(ms, "gd-plan1", "", "", model.StatusOpen)
			seatAgent(ms, "gd-gone1", seat, model.StatusClosed)
			seatAgent(ms, "gd-filer", seat, model.StatusOpen)
			seatAgent(ms, "gd-succ1", seat, model.StatusOpen)
			seatAgent(ms, "gd-other", "gardenio/plan-gardener/gardenio-plan-gardener-for-gd-plan2", model.StatusOpen)
			seatAgent(ms, "gd-noseat", "", model.StatusClosed)
			successor := agentCtx("gd-succ1", "gardenio-plan-gardener-for-gd-plan1-9p4k", "gardenio")
			for i, c := range []struct {
				name   string
				ctx    context.Context
				f      map[string]any
				status string
				want   int
			}{
				{"seat successor withdraws", successor, spawnAsk("gd-gone1"), "failure", http.StatusOK},
				{"filer withdraws its server-executed ask", agentCtx("gd-filer", "gardenio-plan-gardener-for-gd-plan1-ravi", "gardenio"), spawnAsk("gd-filer"), "failure", http.StatusOK},
				{"seat successor posts success", successor, spawnAsk("gd-gone1"), "success", http.StatusForbidden},
				{"seat successor on an approved ask", successor, spawnAsk("gd-gone1", "approved_by", "owner@example.com", "approved_at", "2026-11-05T03:26:58Z"), "failure", http.StatusForbidden},
				{"seat successor, unverified requester", successor, spawnAsk("gd-gone1", "requesting_agent_verified", "false"), "failure", http.StatusForbidden},
				{"agent on another seat", agentCtx("gd-other", "gardenio-plan-gardener-for-gd-plan2-x1y2", "gardenio"), spawnAsk("gd-gone1"), "failure", http.StatusForbidden},
				// seat_key is an ordinary agent-bead field: the token subject must occupy it too.
				{"seat_key copied, subject off the seat", agentCtx("gd-succ1", "crew-z", "gardenio"), spawnAsk("gd-gone1"), "failure", http.StatusForbidden},
				{"requester has no seat", successor, spawnAsk("gd-noseat"), "failure", http.StatusForbidden},
			} {
				id := fmt.Sprintf("gd-ss%d", i)
				seedRoutedEscalation(ms, id, "gardenio", c.f)
				if code := postResult(t, s, c.ctx, id, c.status); code != c.want {
					t.Errorf("%s: got %d, want %d", c.name, code, c.want)
				}
				if closed := ms.beads[id].Status == model.StatusClosed; closed != (c.want == http.StatusOK) {
					t.Errorf("%s: closed = %v", c.name, closed)
				}
			}
		})
	}
}
