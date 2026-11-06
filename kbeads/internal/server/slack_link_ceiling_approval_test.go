package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/events"
)

// Operator review of PR #374, B1: the owner approving through the capped
// gardenio-slack-link client must NOT be able to approve a privileged spawn,
// even when a spawn_policy rule names them as approver.

const slackLinkOwnerTok = "oidc-slack-owner"

// slackLinkAzpVerifier answers azp = gardenio-slack-link for slackLinkOwnerTok
// and the browser client for every other token.
type slackLinkAzpVerifier struct{ *escOIDCMock }

func (v slackLinkAzpVerifier) ExtractClientID(ctx context.Context, raw string) (string, error) {
	if raw == slackLinkOwnerTok {
		return SlackLinkClientID, nil
	}
	return v.escOIDCMock.ExtractClientID(ctx, raw)
}

func newSlackLinkApprovalHarness(t *testing.T) (*mockStore, http.Handler) {
	t.Helper()
	inner := slackLinkAzpVerifier{&escOIDCMock{tokens: map[string]escTokenInfo{
		// The same person, an admin in Keycloak, through the two clients.
		approverOwnerTok:  {email: "owner@example.com", roles: []string{RoleAdmin}},
		slackLinkOwnerTok: {email: "owner@example.com", roles: []string{RoleAdmin}},
	}}}
	capped, err := NewClientRoleCeilingVerifier(inner, map[string]string{SlackLinkClientID: RoleOperator})
	if err != nil {
		t.Fatal(err)
	}
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetForwardAuthClients([]string{testBrowserClient, SlackLinkClientID})
	return ms, srv.NewHTTPHandler(capped, withTestToken(nil, "admin-token"))
}

func TestSlackLinkToken_CannotApprovePrivilegedSpawn(t *testing.T) {
	for _, role := range []string{"infra-admin", "infra", "claimlayer-infra"} {
		t.Run(role+"/rule names the owner", func(t *testing.T) {
			ms, h := newSlackLinkApprovalHarness(t)
			makeRoleBead(ms, "gd-role-"+role, role, true)
			seedSpawnPolicyBead(ms, "gd-sp-1", role, []map[string]any{humanSpawnRule("oidc:owner@example.com")})
			esc := fileSpawnEscalationViaHTTP(t, h, ms, role)

			requireStatus(t, approveSpawnEscalation(h, esc.ID, slackLinkOwnerTok), 403)
			if got := ms.beads[esc.ID].Status; got != "open" {
				t.Fatalf("refused approval must leave the escalation open, got %s", got)
			}
			// The same person through the uncapped browser client still can.
			requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOwnerTok), 200)
		})
		t.Run(role+"/no rule", func(t *testing.T) {
			ms, h := newSlackLinkApprovalHarness(t)
			makeRoleBead(ms, "gd-role-"+role, role, true)
			esc := fileSpawnEscalationViaHTTP(t, h, ms, role)
			requireStatus(t, approveSpawnEscalation(h, esc.ID, slackLinkOwnerTok), 403)
		})
	}
}

// A listed approver who is not an admin cannot approve a privileged spawn
// through any client.
func TestApproveSpawnEscalation_ListedNonAdminCannotApprovePrivileged(t *testing.T) {
	_, ms, h := newSpawnApproverHarness(t)
	makeRoleBead(ms, "gd-role-infra-admin", "infra-admin", true)
	seedSpawnPolicyBead(ms, "gd-sp-1", "infra-admin", []map[string]any{humanSpawnRule("oidc:operator@example.com")})
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra-admin")
	requireStatus(t, approveSpawnEscalation(h, esc.ID, approverOperatorTok), 403)
}

// An unprivileged spawn filed before routing is still approvable through the
// Slack door. A routed one is not: the door is capped at gardenio-operator,
// and D2's operator lane is gardenio-admin (CONTRACT ISSUE 6).
func TestSlackLinkToken_CanApproveUnprivilegedSpawn(t *testing.T) {
	ms, h := newSlackLinkApprovalHarness(t)
	makeRoleBead(ms, "gd-role-crew", "crew", false)
	id := handFileEscalation(t, h, "create", agentSpec("crew"))
	requireStatus(t, approveSpawnEscalation(h, id, slackLinkOwnerTok), 403)
	unrouteEscalation(t, ms, id)
	requireStatus(t, approveSpawnEscalation(h, id, slackLinkOwnerTok), 200)
}

// B2: only a token the gardenio-slack-link sign-in issued may write the bind.
func TestLinkSlackIdentity_RefusesOtherClients(t *testing.T) {
	for _, azp := range []string{"", "gardenio-traefik-forwardauth", "gardenio-cli", "gardenio-slack-link-evil"} {
		t.Run("azp="+azp, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedLinkUserBead(ms, "u-owner", "oidc:owner@example.com", map[string]any{"email": "owner@example.com"})
			rec := httptest.NewRecorder()
			s.handleLinkSlackIdentity(rec, slackLinkRequest(http.MethodPost, "owner@example.com", azp,
				map[string]any{"slack_user_id": "UVICTIM"}))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if f := slackLinkFields(t, ms, "u-owner"); f["slack_id"] != nil || f["identity_link"] != nil {
				t.Fatalf("a refused link must not write the bead: %v", f)
			}
		})
	}
}
