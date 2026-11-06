package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// Tests for gd-5o6PxGfSa7: spawn_policy rule approvers used to be
// informational and nothing stamped them on the escalation, so any signed-in
// gardenio-operator could approve an infra-admin (cluster-admin) spawn.

const (
	approverAdminTok    = "oidc-admin"
	approverOperatorTok = "oidc-operator"
	approverOwnerTok    = "oidc-owner"
)

func newSpawnApproverHarness(t *testing.T) (*BeadsServer, *mockStore, http.Handler) {
	t.Helper()
	oidcMock := &escOIDCMock{tokens: map[string]escTokenInfo{
		approverAdminTok:    {email: "admin@example.com", roles: []string{RoleAdmin}},
		approverOperatorTok: {email: "operator@example.com", roles: []string{RoleOperator}},
		approverOwnerTok:    {email: "owner@example.com", roles: []string{RoleAdmin}},
	}}
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetForwardAuthClients([]string{testBrowserClient})
	return srv, ms, srv.NewHTTPHandler(oidcMock, withTestToken(nil, "admin-token"))
}

func approveSpawnEscalation(h http.Handler, id, token string) *httptest.ResponseRecorder {
	req, _ := http.NewRequest("POST", "/v1/escalations/"+id+"/approve", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func humanSpawnRule(approvers ...string) map[string]any {
	return map[string]any{"when": map[string]any{}, "mode": "human", "approvers": approvers}
}

func escalationFieldList(t *testing.T, ms *mockStore, id, key string) []string {
	t.Helper()
	var f map[string]json.RawMessage
	_ = json.Unmarshal(ms.beads[id].Fields, &f)
	var out []string
	_ = json.Unmarshal(f[key], &out)
	return out
}

// A privileged spawn with no approvers-bearing rule defaults to admin-only.
func TestApproveSpawnEscalation_PrivilegedDefaultsToAdminOnly(t *testing.T) {
	for _, role := range []string{"infra-admin", "infra", "claimlayer-infra"} {
		t.Run(role, func(t *testing.T) {
			_, ms, h := newSpawnApproverHarness(t)
			makeRoleBead(ms, "gd-role-"+role, role, true)
			esc := fileSpawnEscalationViaHTTP(t, h, ms, role)

			requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOperatorTok), 403)
			if got := ms.beads[esc.ID].Status; got != "open" {
				t.Fatalf("refused approval must leave the escalation open, got %s", got)
			}
			requireStatus(t, approveSpawnEscalation(h, esc.ID, approverAdminTok), 200)
		})
	}
}

// A rule's approvers are copied onto the escalation and enforced: the listed
// owner (an operator-role human) approves, an unlisted operator and an
// unlisted admin do not. Privileged roles also need the approver to be an
// admin (operator review of PR #374, B1), so the owner here is an admin.
func TestApproveSpawnEscalation_RuleApproversEnforced(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	seedSpawnPolicyBead(ms, "gd-sp-1", "infra-admin", []map[string]any{humanSpawnRule("oidc:owner@example.com")})
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")

	if got := escalationFieldList(t, ms, esc.ID, "approvers"); len(got) != 1 || got[0] != "oidc:owner@example.com" {
		t.Fatalf("rule approvers should be stamped on the escalation, got %v", got)
	}
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOperatorTok), 403)
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverAdminTok), 403)
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOwnerTok), 200)
}

// The enforcement does not depend on the stamped field: an operator who
// PATCHes approvers off the escalation bead is still refused.
func TestApproveSpawnEscalation_StrippedApproversStillEnforced(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	seedSpawnPolicyBead(ms, "gd-sp-1", "infra-admin", []map[string]any{humanSpawnRule("oidc:owner@example.com")})
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")

	var f map[string]json.RawMessage
	_ = json.Unmarshal(ms.beads[esc.ID].Fields, &f)
	delete(f, "approvers")
	ms.beads[esc.ID].Fields, _ = json.Marshal(f)

	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOperatorTok), 403)
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverAdminTok), 403)
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOwnerTok), 200)
}

// Approvers resolve through operator aliases (slack:/oidc: linked to one row)
// and team: entries, not only the literal identity.
func TestApproveSpawnEscalation_ApproversResolveAliasesAndTeams(t *testing.T) {
	t.Run("alias", func(t *testing.T) {
		_, ms, h := newSpawnApproverHarness(t)
		makeRoleBead(ms, "gd-role-infra", "infra", true)
		seedSpawnPolicyBead(ms, "gd-sp-1", "infra", []map[string]any{humanSpawnRule("slack:U0OWNER")})
		ms.operatorAliases = map[string]int64{
			"slack\x00U0OWNER":          7,
			"oidc\x00owner@example.com": 7,
			"oidc\x00admin@example.com": 8,
		}
		esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra")
		requireStatus(t, approveSpawnEscalation(h, esc.ID, approverAdminTok), 403)
		requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOwnerTok), 200)
	})
	t.Run("team", func(t *testing.T) {
		_, ms, h := newSpawnApproverHarness(t)
		makeRoleBead(ms, "gd-role-infra", "infra", true)
		seedSpawnPolicyBead(ms, "gd-sp-1", "infra", []map[string]any{humanSpawnRule("team:gd-team-owners")})
		seedTeamBead(ms, "gd-team-owners", `{"team":"owners","members":["oidc:owner@example.com"]}`)
		ms.operatorAliases = map[string]int64{
			"oidc\x00owner@example.com": 7,
			"oidc\x00admin@example.com": 8,
		}
		esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra")
		requireStatus(t, approveSpawnEscalation(h, esc.ID, approverAdminTok), 403)
		requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOwnerTok), 200)
	})
}

// A non-privileged role under no approvers-bearing rule is unchanged: an
// operator approves it.
func TestApproveSpawnEscalation_UnprivilegedRoleUnchanged(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-special", "special", false)
	// An unprivileged role needs a gate to escalate at all; mark it
	// bypass-free but file the escalation directly.
	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
		"spawn_role":     "special",
	})
	ms.beads["gd-esc-unpriv"] = &model.Bead{
		ID: "gd-esc-unpriv", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U999",
	}
	requireStatus(t, approveSpawnEscalation(h, "gd-esc-unpriv", approverOperatorTok), 200)
}
