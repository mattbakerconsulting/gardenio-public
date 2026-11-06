package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for gd-4gdUwaWr6i: the spawn approver gate used to key on the
// escalation's requester-writable spawn_role field, so a requester could
// hand-file (or PATCH spawn_role off) an escalation whose operation_spec
// creates an infra-admin agent and have any operator approve it.

// requesterRequest sends a write as the requester: the shared static token a
// pod holds plus an operator header, so authMethod is static (not admin).
func requesterRequest(h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "gd-requester-agent")
	req.Header.Set("X-Operated-By", "slack:U0REQUESTER")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func handFiledEscalationBody(opType string, spec map[string]any, extra map[string]any) map[string]any {
	fields := map[string]any{"operation_type": opType, "operation_spec": spec}
	for k, v := range extra {
		fields[k] = v
	}
	return map[string]any{"title": "hand-filed", "type": "escalation", "kind": "data", "fields": fields}
}

func handFileEscalation(t *testing.T, h http.Handler, opType string, spec map[string]any) string {
	t.Helper()
	rec := requesterRequest(h, "POST", "/v1/beads", handFiledEscalationBody(opType, spec, nil))
	requireStatus(t, rec, 201)
	var created map[string]any
	decodeJSON(t, rec, &created)
	return created["id"].(string)
}

func agentSpec(role string) map[string]any {
	fields := map[string]any{"project": "gardenio", "agent": "evil-agent"}
	if role != "" {
		fields["role"] = role
	}
	return map[string]any{"title": "evil-agent", "type": "agent", "fields": fields}
}

// A hand-filed escalation (no spawn_role) creating an infra-admin agent is
// gated on the role in operation_spec: an unlisted operator gets 403, an
// admin approves.
func TestApproveSpawnEscalation_HandFiledWithoutSpawnRoleStillGated(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	id := handFileEscalation(t, h, "create", agentSpec("infra-admin"))

	requireStatus(t, approveSpawnEscalation(h, id, approverOperatorTok), 403)
	if got := ms.beads[id].Status; got != "open" {
		t.Fatalf("refused approval must leave the escalation open, got %s", got)
	}
	requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 200)
}

// The same holds when a rule names approvers: an unlisted admin is refused
// too, and the hand-filed escalation carries no stamped approvers.
func TestApproveSpawnEscalation_HandFiledHonoursRuleApprovers(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	seedSpawnPolicyBead(ms, "gd-sp-1", "infra-admin", []map[string]any{humanSpawnRule("oidc:owner@example.com")})
	id := handFileEscalation(t, h, "create", agentSpec("infra-admin"))

	requireStatus(t, approveSpawnEscalation(h, id, approverOperatorTok), 403)
	requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 403)
	requireStatus(t, approveSpawnEscalation(h, id, approverOwnerTok), 200)
}

// An agent create whose role cannot be resolved is refused outright, even for
// an admin, rather than replayed ungated.
func TestApproveSpawnEscalation_UnresolvableRoleRefused(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	for name, spec := range map[string]map[string]any{
		"no role":     agentSpec(""),
		"blank role":  agentSpec("   "),
		"role number": {"type": "agent", "title": "x", "fields": map[string]any{"role": 7}},
	} {
		t.Run(name, func(t *testing.T) {
			id := handFileEscalation(t, h, "create", spec)
			requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 403)
			if got := ms.beads[id].Status; got != "open" {
				t.Fatalf("escalation must stay open, got %s", got)
			}
		})
	}
}

// A role update in operation_spec is gated like a spawn, with no spawn_role.
func TestApproveSpawnEscalation_HandFiledRoleUpdateGated(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	id := handFileEscalation(t, h, "update", map[string]any{"fields": map[string]any{"role": "infra-admin"}})
	requireStatus(t, approveSpawnEscalation(h, id, approverOperatorTok), 403)
	requireStatus(t, approveSpawnEscalation(h, id, approverAdminTok), 200)
}

// A requester PATCHing spawn_role off a server-filed spawn escalation is
// refused, and approval is gated regardless.
func TestApproveSpawnEscalation_PatchRemovingSpawnRoleRefused(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")

	rec := requesterRequest(h, "PATCH", "/v1/beads/"+esc.ID, map[string]any{"fields": map[string]any{"spawn_role": nil}})
	requireStatus(t, rec, 400)

	// Even with the field gone from the store (direct write), approval is gated.
	var f map[string]json.RawMessage
	_ = json.Unmarshal(ms.beads[esc.ID].Fields, &f)
	delete(f, "spawn_role")
	ms.beads[esc.ID].Fields, _ = json.Marshal(f)
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOperatorTok), 403)
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverAdminTok), 200)
}

// A non-admin cannot set or change the stamped fields at create or by PATCH.
func TestEscalationStampedFields_NonAdminRefused(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)

	stamped := map[string]any{
		"spawn_role":         "crew",
		"approvers":          []string{"oidc:operator@example.com"},
		"requires_four_eyes": false,
		"Spawn_Role":         "crew",
		"executor_mail_id":   "gd-forged-mail",
	}
	for k, v := range stamped {
		t.Run("create "+k, func(t *testing.T) {
			rec := requesterRequest(h, "POST", "/v1/beads",
				handFiledEscalationBody("create", agentSpec("infra-admin"), map[string]any{k: v}))
			requireStatus(t, rec, 400)
		})
	}

	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")
	patches := map[string]map[string]any{
		"approvers":          {"approvers": []string{"oidc:operator@example.com"}},
		"requires_four_eyes": {"requires_four_eyes": false},
		"spawn_role":         {"spawn_role": "crew"},
		"operation_spec":     {"operation_spec": agentSpec("crew")},
		"operation_type":     {"operation_type": "update"},
		"target_bead_id":     {"target_bead_id": "gd-other"},
		"executor_mail_id":   {"executor_mail_id": "gd-forged-mail"},
		"execution_target":   {"execution_target": "local"},
		"target_runner":      {"target_runner": "gd-other-runner"},
		"target_owner":       {"target_owner": "oidc:someone@example.com"},
		"requesting_runner":  {"requesting_runner": "gd-other-runner"},
	}
	for name, patch := range patches {
		t.Run("patch "+name, func(t *testing.T) {
			rec := requesterRequest(h, "PATCH", "/v1/beads/"+esc.ID, map[string]any{"fields": patch})
			requireStatus(t, rec, 400)
		})
	}

	// An unrelated field, and re-sending an unchanged stamped field, still work.
	requireStatus(t, requesterRequest(h, "PATCH", "/v1/beads/"+esc.ID,
		map[string]any{"fields": map[string]any{"context": "more detail"}}), 200)
}

// Not even a gardenio-admin may amend the stamped fields: spec D8 (P7 Task
// 11) freezes approvers once the escalation is filed.
func TestEscalationStampedFields_AdminPatchRefused(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")

	body, _ := json.Marshal(map[string]any{"fields": map[string]any{"approvers": []string{"oidc:owner@example.com"}}})
	req := httptest.NewRequest("PATCH", "/v1/beads/"+esc.ID, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+approverAdminTok)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 400)
}

// The server-stamped filing path (denial + X-Escalate) still stamps the
// fields for a non-admin requester.
func TestEscalationStampedFields_ServerFilingStillStamps(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	seedSpawnPolicyBead(ms, "gd-sp-1", "infra-admin", []map[string]any{humanSpawnRule("oidc:owner@example.com")})
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")
	var f map[string]json.RawMessage
	_ = json.Unmarshal(ms.beads[esc.ID].Fields, &f)
	if _, ok := f["spawn_role"]; !ok {
		t.Fatal("server filing must stamp spawn_role")
	}
	if got := escalationFieldList(t, ms, esc.ID, "approvers"); len(got) != 1 {
		t.Fatalf("server filing must stamp approvers, got %v", got)
	}
}

// four_eyes on the live rule is enforced even when the stamped flag is gone.
func TestApproveSpawnEscalation_FourEyesResolvedLive(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	rule := humanSpawnRule("oidc:owner@example.com", "oidc:admin@example.com")
	rule["four_eyes"] = true
	seedSpawnPolicyBead(ms, "gd-sp-1", "infra-admin", []map[string]any{rule})
	ms.operatorAliases = map[string]int64{
		"slack\x00U0OPERATOR2":      7,
		"oidc\x00owner@example.com": 7,
		"oidc\x00admin@example.com": 8,
	}
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")

	// Strip the stamped flag as a requester with direct store access would.
	var f map[string]json.RawMessage
	_ = json.Unmarshal(ms.beads[esc.ID].Fields, &f)
	delete(f, "requires_four_eyes")
	ms.beads[esc.ID].Fields, _ = json.Marshal(f)

	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOwnerTok), 403)
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverAdminTok), 200)
}

func TestEscalationSpawnTarget(t *testing.T) {
	cases := []struct {
		name      string
		f         spawnEscalationFields
		role      string
		spawn     bool
		resolvErr bool
	}{
		{"agent create", spawnEscalationFields{OperationType: "create", OperationSpec: mustMarshal(agentSpec("infra"))}, "infra", true, false},
		{"agent create ignores spawn_role", spawnEscalationFields{OperationType: "create", SpawnRole: "crew", OperationSpec: mustMarshal(agentSpec("infra"))}, "infra", true, false},
		{"agent create no role", spawnEscalationFields{OperationType: "create", OperationSpec: mustMarshal(agentSpec(""))}, "", true, true},
		{"non-agent create", spawnEscalationFields{OperationType: "create", OperationSpec: mustMarshal(map[string]any{"type": "task", "title": "x"})}, "", false, false},
		{"update with role", spawnEscalationFields{OperationType: "update", OperationSpec: mustMarshal(map[string]any{"fields": map[string]any{"role": "infra"}})}, "infra", true, false},
		{"update without role", spawnEscalationFields{OperationType: "update", OperationSpec: mustMarshal(map[string]any{"fields": map[string]any{"x": "y"}})}, "", false, false},
		{"garbage create spec", spawnEscalationFields{OperationType: "create", OperationSpec: json.RawMessage(`"str"`)}, "", true, true},
		{"close", spawnEscalationFields{OperationType: "close", OperationSpec: mustMarshal(agentSpec("infra"))}, "", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, spawn, err := escalationSpawnTarget(c.f)
			if role != c.role || spawn != c.spawn || (err != nil) != c.resolvErr {
				t.Fatalf("got (%q, %v, %v); want (%q, %v, err=%v)", role, spawn, err, c.role, c.spawn, c.resolvErr)
			}
		})
	}
}
