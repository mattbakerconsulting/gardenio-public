package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for gd-pUyCv4FSv5. Part A: approving a non-spawn escalation vets the
// replayed write against the APPROVER's role. Part B: the approval-relevant
// fields cannot be rewritten through the field endpoints or /result.

func adminRequest(h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	return bearerRequest(h, method, path, approverAdminTok, body)
}

func operatorRequest(h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	return bearerRequest(h, method, path, approverOperatorTok, body)
}

func bearerRequest(h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// A gardenio-operator approving a hand-filed write to an admin-only type gets
// 403 and the escalation stays open; an admin approves it.
func TestApproveEscalation_AdminOnlyTargetTypeNeedsAdminApprover(t *testing.T) {
	specs := map[string]map[string]any{
		"role":         {"type": "role", "title": "r", "fields": map[string]any{"name": "r", "privileged": false}},
		"team":         {"type": "team", "title": "t", "fields": map[string]any{"team": "t"}},
		"spawn_policy": {"type": "spawn_policy", "title": "sp", "fields": map[string]any{"role": "crew"}},
		"user":         {"type": "user", "title": "u", "fields": map[string]any{"email": "u@example.com"}},
		"config":       {"type": "config", "title": "cfg", "fields": map[string]any{"value": "{}"}},
		"project":      {"type": "project", "title": "p", "fields": map[string]any{"name": "p"}},
	}
	for name, spec := range specs {
		t.Run(name, func(t *testing.T) {
			_, ms, h := newSpawnApproverHarness(t)
			id := handFileEscalation(t, h, "create", spec)

			requireStatus(t, approveSpawnEscalation(h, id, approverOperatorTok), 403)
			if got := ms.beads[id].Status; got != "open" {
				t.Fatalf("refused approval must leave the escalation open, got %s", got)
			}
			requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 200)
		})
	}
}

// An update/close replay is vetted against the TARGET bead's type.
func TestApproveEscalation_UpdateAndCloseVetTargetType(t *testing.T) {
	for _, op := range []string{"update", "close"} {
		t.Run(op, func(t *testing.T) {
			_, ms, h := newSpawnApproverHarness(t)
			rec := adminRequest(h, "POST", "/v1/beads", map[string]any{
				"title": "team", "type": "team", "kind": "config", "fields": map[string]any{"team": "t"},
			})
			requireStatus(t, rec, 201)
			var team map[string]any
			decodeJSON(t, rec, &team)

			body := handFiledEscalationBody(op, map[string]any{"fields": map[string]any{"team": "t2"}},
				map[string]any{"target_bead_id": team["id"]})
			rec = requesterRequest(h, "POST", "/v1/beads", body)
			requireStatus(t, rec, 201)
			var esc map[string]any
			decodeJSON(t, rec, &esc)
			id := esc["id"].(string)

			requireStatus(t, approveSpawnEscalation(h, id, approverOperatorTok), 403)
			if got := ms.beads[id].Status; got != "open" {
				t.Fatalf("refused approval must leave the escalation open, got %s", got)
			}
			requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 200)
		})
	}
}

// Escalations that replay no write to a gated type still approve for an
// operator: a task create, a merge_window and a manual_operator_action with no
// operation_spec.
//
// The escalations are approved as ones filed before routing
// (unrouteEscalation): a routed one admits only the operator lane, an admin.
func TestApproveEscalation_NoGatedWriteStillApprovesForOperator(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)

	taskID := handFileEscalation(t, h, "create", map[string]any{"type": "task", "title": "plain task"})
	unrouteEscalation(t, ms, taskID)
	requireStatus(t, approveSpawnEscalation(h, taskID, approverOperatorTok), 200)

	for _, opType := range []string{"merge_window", "manual_operator_action"} {
		rec := requesterRequest(h, "POST", "/v1/beads", map[string]any{
			"title": opType, "type": "escalation", "kind": "data",
			"fields": map[string]any{"operation_type": opType, "operation_spec": map[string]any{"pr": "#1"}},
		})
		requireStatus(t, rec, 201)
		var esc map[string]any
		decodeJSON(t, rec, &esc)
		unrouteEscalation(t, ms, esc["id"].(string))
		requireStatus(t, approveSpawnEscalation(h, esc["id"].(string), approverOperatorTok), 200)
	}
}

func fileSpawnEscalationForFieldTests(t *testing.T) (*mockStore, http.Handler, string) {
	t.Helper()
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")
	return ms, h, esc.ID
}

func fieldsSnapshot(ms *mockStore, id string) string { return string(ms.beads[id].Fields) }

// The field endpoints refuse approval-relevant fields on an escalation for a
// non-admin caller, in any case, and leave the bead untouched.
func TestEscalationFieldEndpoints_ImmutableForNonAdmin(t *testing.T) {
	ms, h, id := fileSpawnEscalationForFieldTests(t)
	before := fieldsSnapshot(ms, id)

	for _, field := range []string{"approvers", "Approvers", "spawn_role", "requires_four_eyes", "operation_spec", "OPERATION_TYPE", "target_bead_id"} {
		t.Run(field, func(t *testing.T) {
			base := "/v1/beads/" + id + "/fields/" + field
			requireStatus(t, requesterRequest(h, "POST", base+"/edit",
				map[string]any{"op": "append", "element": "x"}), 400)
			requireStatus(t, requesterRequest(h, "POST", base+"/edit",
				map[string]any{"op": "set", "key": "k", "value": "v"}), 400)
			requireStatus(t, requesterRequest(h, "POST", base+"/append",
				map[string]any{"element": "team:gd-evil"}), 400)
			requireStatus(t, requesterRequest(h, "POST", base+"/reorder",
				map[string]any{"expected": []string{}, "next": []string{"x"}}), 400)
		})
	}
	requireStatus(t, requesterRequest(h, "POST", "/v1/beads/"+id+"/increment",
		map[string]any{"deltas": map[string]any{"requires_four_eyes": 1}}), 400)

	if after := fieldsSnapshot(ms, id); after != before {
		t.Fatalf("refused writes must not change the escalation\nbefore: %s\nafter:  %s", before, after)
	}
}

// A field that is not approval-relevant is still writable through the endpoints.
func TestEscalationFieldEndpoints_OtherFieldsStillWritable(t *testing.T) {
	_, h, id := fileSpawnEscalationForFieldTests(t)
	rec := requesterRequest(h, "POST", "/v1/beads/"+id+"/fields/notes_list/append", map[string]any{"element": "x"})
	if rec.Code == 400 {
		t.Fatalf("unrelated field must not be refused by the escalation guard: %s", rec.Body.String())
	}
}

// Not even an admin may edit a filing field through the field endpoints:
// spec D8 (P7 Task 11) freezes approvers for every caller.
func TestEscalationFieldEndpoints_AdminRefusedFilingField(t *testing.T) {
	_, h, id := fileSpawnEscalationForFieldTests(t)
	rec := adminRequest(h, "POST", "/v1/beads/"+id+"/fields/approvers/append", map[string]any{"element": "team:gd-t"})
	if rec.Code != 400 {
		t.Fatalf("admin append to approvers: got %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// /result must not merge approval-relevant fields, in any case, and must not
// close the escalation when it refuses. The caller is an OIDC operator: P7
// Task 9 refuses the static token on /result before the field check runs, and
// an admin is exempt from the field check.
func TestEscalationResult_ApprovalFieldsRefused(t *testing.T) {
	ms, h, id := fileSpawnEscalationForFieldTests(t)
	before := fieldsSnapshot(ms, id)

	for _, key := range []string{"approvers", "Operation_Spec", "spawn_role", "requires_four_eyes", "operation_type", "target_bead_id"} {
		t.Run(key, func(t *testing.T) {
			rec := operatorRequest(h, "POST", "/v1/escalations/"+id+"/result", map[string]any{
				"result_status": "success",
				"fields":        map[string]string{key: "x"},
			})
			requireStatus(t, rec, 400)
		})
	}
	if after := fieldsSnapshot(ms, id); after != before {
		t.Fatalf("refused /result must not change the escalation\nbefore: %s\nafter:  %s", before, after)
	}
	if got := ms.beads[id].Status; got != "open" {
		t.Fatalf("refused /result must leave the escalation open, got %s", got)
	}

	// Ordinary result capture is unaffected (an operator sends no fields:
	// only a bridge may, P7 Task 10).
	requireStatus(t, operatorRequest(h, "POST", "/v1/escalations/"+id+"/result", map[string]any{
		"result_status": "success", "executor": "ops",
	}), 200)
}

// PATCH refuses ANY case variant of a stamped key, whichever way the variants
// collide, so the outcome does not depend on map iteration order.
func TestEscalationPatch_CaseVariantsRefused(t *testing.T) {
	ms, h, id := fileSpawnEscalationForFieldTests(t)
	before := fieldsSnapshot(ms, id)
	for i := 0; i < 20; i++ {
		rec := requesterRequest(h, "PATCH", "/v1/beads/"+id, map[string]any{"fields": map[string]any{
			"Approvers": []string{"oidc:operator@example.com"},
			"APPROVERS": []string{"oidc:operator@example.com"},
		}})
		requireStatus(t, rec, 400)
	}
	if after := fieldsSnapshot(ms, id); after != before {
		t.Fatalf("refused PATCH must not change the escalation\nbefore: %s\nafter:  %s", before, after)
	}
}
