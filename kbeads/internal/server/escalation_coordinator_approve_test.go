package server

import (
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

func requireUnapproved(t *testing.T, ms *mockStore, id string) {
	t.Helper()
	if v := escField(t, ms, id, "approved_by"); v != nil {
		t.Fatalf("%s: approved_by = %v after a refused approval", id, v)
	}
	if ms.beads[id].Status != model.StatusOpen {
		t.Fatalf("%s: status %s after a refused approval", id, ms.beads[id].Status)
	}
}

// Review Focus 2.
func TestCoordinatorApprove_RefusesOwnRequest(t *testing.T) {
	s, ms := newLaneTestServer()

	// The merge-desk reviewer requested this merge itself.
	seedRoutedEscalation(ms, "gd-own-rev", "gardenio", mergeDeskEscalation("gd-rev1"))
	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-own-rev", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reviewer approving its own request: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-own-rev")

	// The laptop approving an escalation it filed (created_by = its subject).
	ownF := mergeDeskEscalation("")
	ownF["held_by"] = "coordinator:laptop" // holding it, so only the self-approval rule refuses
	own := seedRoutedEscalation(ms, "gd-own-laptop", "gardenio", ownF)
	own.CreatedBy = "machine:gardenio-laptop-operator"
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-own-laptop", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("laptop approving its own escalation: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-own-laptop")
}

// Positive controls for TestCoordinatorApprove_RefusesOwnRequest: same lanes,
// a crew agent's request.
func TestCoordinatorApprove_MergeDeskAndLaptopApproveCrewMerge(t *testing.T) {
	s, ms := newLaneTestServer()

	seedRoutedEscalation(ms, "gd-crew-m1", "gardenio", mergeDeskEscalation("gd-crew1"))
	rec := callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-crew-m1", "approve", map[string]any{"approved_by": "forged@example.com"})
	if rec.Code != http.StatusOK {
		t.Fatalf("merge desk approve: got %d %s", rec.Code, rec.Body.String())
	}
	for k, want := range map[string]any{
		"approved_by":     "reviewer-a",
		"approval_method": "coordinator",
		"approved_as":     "coordinator:merge-desk",
		"held_by":         "coordinator:merge-desk",
	} {
		if got := escField(t, ms, "gd-crew-m1", k); got != want {
			t.Errorf("merge desk approve: %s = %v, want %v", k, got, want)
		}
	}
	if escField(t, ms, "gd-crew-m1", "approved_at") == nil {
		t.Error("approved_at not stamped")
	}
	if ms.beads["gd-crew-m1"].Status != model.StatusInProgress {
		t.Errorf("status = %s, want in_progress", ms.beads["gd-crew-m1"].Status)
	}

	laptopHeld := mergeDeskEscalation("gd-crew1")
	laptopHeld["held_by"] = "coordinator:laptop" // routed to the operators (D4 needs the hold)
	seedRoutedEscalation(ms, "gd-crew-m2", "gardenio", laptopHeld)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-crew-m2", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("laptop approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-crew-m2", "approved_as"); got != "coordinator:laptop" {
		t.Errorf("laptop approved_as = %v", got)
	}
	if got := escField(t, ms, "gd-crew-m2", "approved_by"); got != "machine:gardenio-laptop-operator" {
		t.Errorf("laptop approved_by = %v", got)
	}
	// D11: the approval moved held_by to execute_by (merge-desk), so the
	// audience follows it.
	if got := escField(t, ms, "gd-crew-m2", "audience"); got != "desk" {
		t.Errorf("audience after approval = %v, want desk (laneAudience(execute_by))", got)
	}
}

// Review Focus 3, approve side.
func TestCoordinatorApprove_RefusesOperatorOnly(t *testing.T) {
	s, ms := newLaneTestServer()

	// A floor operation whose approvers were (somehow) widened to the laptop:
	// the floor is re-derived at approve time.
	f := mergeDeskEscalation("gd-crew1")
	f["operation_type"] = "credential_rotation"
	f["operator_only"] = false
	f["approvers"] = []string{"coordinator:laptop"}
	f["held_by"] = "coordinator:laptop"
	seedRoutedEscalation(ms, "gd-floor", "gardenio", f)
	rec := callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-floor", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on credential_rotation: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-floor")

	// A stamped operator_only=true is honoured even for a merge.
	g := mergeDeskEscalation("gd-crew1")
	g["operator_only"] = true
	g["held_by"] = "coordinator:laptop"
	seedRoutedEscalation(ms, "gd-oo", "gardenio", g)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-oo", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on operator_only=true: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-oo")

	// Positive control: the same merge fixture with operator_only=false.
	okF := mergeDeskEscalation("gd-crew1")
	okF["held_by"] = "coordinator:laptop"
	seedRoutedEscalation(ms, "gd-oo-ok", "gardenio", okF)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-oo-ok", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("coordinator on a plain merge: got %d %s, want 200", rec.Code, rec.Body.String())
	}

	// The operator can still approve the floor escalation (human path unchanged).
	rec = callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-oo", "approve", map[string]any{"approval_method": "web_ui"})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator on operator_only: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestCoordinatorApprove_RefusesGrantAndPrivilegedSpawn(t *testing.T) {
	s, ms := newLaneTestServer()
	makeRoleBead(ms, "gd-role-infra", "infra", true) // infra is not never-auto; its role bead makes it privileged
	lanes := func(op string, spec any) map[string]any {
		f := mergeDeskEscalation("gd-crew1")
		f["operation_type"] = op
		f["operation_spec"] = spec
		f["approvers"] = []string{"coordinator:laptop", "operator"}
		f["held_by"] = "coordinator:laptop"
		return f
	}
	seedRoutedEscalation(ms, "gd-grant", "gardenio", lanes("grant", map[string]any{"taint": "x"}))
	rec := callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-grant", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on grant: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	seedRoutedEscalation(ms, "gd-spawn", "gardenio", lanes("create", map[string]any{"type": "agent", "title": "a", "fields": map[string]any{"role": "infra"}}))
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-spawn", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("coordinator on a privileged spawn: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	// Positive control: the same lanes on a non-replayed operation.
	seedRoutedEscalation(ms, "gd-plain", "gardenio", lanes("helm_deploy", map[string]any{}))
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-plain", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("coordinator on helm_deploy: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestCoordinatorApprove_NonLaneAgentAndUnlistedMachineRefused(t *testing.T) {
	s, ms := newLaneTestServer()
	h := escalationAgentHandler(s, ms, nil)
	seedRoutedEscalation(ms, "gd-st", "gardenio", mergeDeskEscalation("gd-crew1"))
	// A crew agent on its own token is in no lane.
	rec := doAgentEscalationReq(t, h, http.MethodPost, "/v1/escalations/gd-st/approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-lane agent: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-st")

	// The laptop client without BEADS_ESCALATION_COORDINATOR_CLIENTS.
	s.SetEscalationCoordinatorClients(nil)
	rec = callEscalationVerb(t, machineCtx("gardenio-laptop-operator"), s.handleApproveEscalation, "gd-st", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unlisted machine: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-st")

	// Positive control: the same fixture, the reviewer on its own token.
	rec = callEscalationVerb(t, agentCtx("gd-rev1", "reviewer-a", "gardenio"), s.handleApproveEscalation, "gd-st", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("reviewer on its own token: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}

func TestHumanApprove_StampsHeldByExecuteBy(t *testing.T) {
	s, ms := newLaneTestServer()
	seedRoutedEscalation(ms, "gd-hx", "gardenio", map[string]any{"operation_type": "helm_deploy", "approvers": []string{"operator"}, "execute_by": "coordinator:laptop", "held_by": "operator", "claimed_by": "owner@example.com"})
	rec := callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-hx", "approve", map[string]any{"approval_method": "web_ui"})
	if rec.Code != http.StatusOK {
		t.Fatalf("human approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-hx", "held_by"); got != "coordinator:laptop" {
		t.Fatalf("held_by = %v, want execute_by coordinator:laptop", got)
	}
	if got := escField(t, ms, "gd-hx", "audience"); got != "operator" {
		t.Fatalf("audience = %v, want operator (laneAudience(coordinator:laptop), D11)", got)
	}
	if at, _ := escField(t, ms, "gd-hx", "held_at").(string); at == "" {
		t.Fatal("held_at not restamped when approval moved held_by")
	}
	if got := escField(t, ms, "gd-hx", "approval_method"); got != "web_ui" {
		t.Fatalf("approval_method = %v, want web_ui (human path unchanged)", got)
	}
	if got := escField(t, ms, "gd-hx", "approved_as"); got != nil {
		t.Fatalf("approved_as = %v on a human approval, want unset", got)
	}
	if got := escField(t, ms, "gd-hx", "claimed_by"); got != "" {
		t.Fatalf("claimed_by = %v, want cleared", got)
	}
	// Legacy (no execute_by): held_by is left alone.
	seedRoutedEscalation(ms, "gd-hy", "gardenio", map[string]any{"operation_type": "helm_deploy"})
	rec = callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-hy", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy human approve: got %d %s", rec.Code, rec.Body.String())
	}
	if got := escField(t, ms, "gd-hy", "held_by"); got != nil {
		t.Fatalf("legacy held_by = %v, want unset", got)
	}
	if got := escField(t, ms, "gd-hy", "audience"); got != nil {
		t.Fatalf("legacy audience = %v, want untouched", got)
	}
}

// The live human approve path re-derives the D3 floor like the can-approve
// dry run (Task 3 carry-forward): a gardenio-operator who is not in the
// operator lane may not approve a floor operation even when the approvers
// list names a team it is in.
func TestHumanApprove_FloorRederivedOnLivePath(t *testing.T) {
	s, ms := newLaneTestServer()
	floor := func() map[string]any {
		return map[string]any{"operation_type": "credential_rotation", "operator_only": false, "approvers": []string{"op@example.com", "operator"}}
	}
	seedRoutedEscalation(ms, "gd-hf", "gardenio", floor())
	rec := callEscalationVerb(t, humanCtx("op@example.com", []string{RoleOperator}), s.handleApproveEscalation, "gd-hf", "approve", map[string]any{})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin human on a floor operation: got %d %s, want 403", rec.Code, rec.Body.String())
	}
	requireUnapproved(t, ms, "gd-hf")

	// Positive control: the same fixture, an admin in the operator lane.
	rec = callEscalationVerb(t, operatorCtx("owner@example.com"), s.handleApproveEscalation, "gd-hf", "approve", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator on a floor operation: got %d %s, want 200", rec.Code, rec.Body.String())
	}
}
