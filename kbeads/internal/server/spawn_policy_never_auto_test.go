package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// The infra-admin role (plan gd-xiV5hyhPrG P6, gd-sAMar9tn5B) carries
// cluster-admin. It must be approved by a human on every spawn, so the
// spawn-policy auto path is closed structurally in two places:
//   1. write time: validateSpawnPolicyFields refuses an auto rule;
//   2. evaluation: evaluateSpawnEscalation never acts, even on a rule injected
//      past the write-time check;
// Approval is by an OIDC human through an approval-capable client. There is no
// requires_four_eyes stamp: the org has one human approver, so it could never
// be satisfied (gd-TXbrvA9E6F).
// Every "refused" case below is paired with the same shape for the ordinary
// role `infra`, which must still be accepted/auto-approved.

func TestNeverAutoApproveRoles_InfraAdminIsListedAndInfraIsNot(t *testing.T) {
	if !isNeverAutoApproveRole("infra-admin") {
		t.Fatal("infra-admin must be in neverAutoApproveRoles")
	}
	for _, role := range []string{"infra", "ha-drill-operator", "k8s-live", "", "infra-admin2", "INFRA-ADMIN"} {
		if isNeverAutoApproveRole(role) {
			t.Fatalf("role %q must not be in neverAutoApproveRoles", role)
		}
	}
}

func TestValidateSpawnPolicyFields_NeverAutoRoleRejectsAutoRule(t *testing.T) {
	autoRules := []map[string]any{spawnPolicyAutoRule(map[string]any{}, 6, 1)}
	humanRules := []map[string]any{{
		"when": map[string]any{}, "mode": "human",
		"approvers": []string{"oidc:matthewbaker1110@gmail.com"},
	}}
	mixed := []map[string]any{spawnPolicyHumanRule(map[string]any{"project": "a"}), spawnPolicyAutoRule(map[string]any{}, 6, 1)}

	tests := []struct {
		name    string
		role    string
		rules   []map[string]any
		wantErr bool
	}{
		{"infra-admin auto rule is refused", "infra-admin", autoRules, true},
		{"infra-admin auto rule later in the list is refused", "infra-admin", mixed, true},
		{"infra-admin human rule is accepted", "infra-admin", humanRules, false},
		{"control: infra auto rule is accepted", "infra", autoRules, false},
		{"control: infra human rule is accepted", "infra", humanRules, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := mustMarshal(map[string]any{"role": tt.role, "rules": tt.rules})
			reason := validateSpawnPolicyFields(raw)
			if tt.wantErr && reason == "" {
				t.Fatal("expected a rejection reason, got none")
			}
			if tt.wantErr && !strings.Contains(reason, "infra-admin") {
				t.Fatalf("rejection should name the role, got %q", reason)
			}
			if !tt.wantErr && reason != "" {
				t.Fatalf("expected no rejection, got %q", reason)
			}
		})
	}
}

// TestCreateSpawnPolicyBead_NeverAutoRoleAutoRuleRefusedAtWrite drives the
// create and update wiring in beads.go, not only the validator in isolation.
func TestCreateSpawnPolicyBead_NeverAutoRoleAutoRuleRefusedAtWrite(t *testing.T) {
	ctx := WithRoles(ctxAsTestSubject(), []string{RoleAdmin})
	autoPolicy := func(role string) createBeadInput {
		return createBeadInput{
			Title: role + " spawn policy",
			Type:  "spawn_policy",
			Kind:  "config",
			Fields: mustMarshal(map[string]any{
				"role":  role,
				"rules": []map[string]any{spawnPolicyAutoRule(map[string]any{}, 6, 1)},
			}),
		}
	}

	s, _ := newAutoCloseServer()
	_, err := s.createBead(ctx, autoPolicy("infra-admin"))
	if err == nil {
		t.Fatal("create of an infra-admin policy with an auto rule must be refused")
	}
	if !strings.Contains(err.Error(), "infra-admin") {
		t.Fatalf("refusal should be the never-auto guard, got %v", err)
	}
	if _, err := s.createBead(ctx, autoPolicy("infra")); err != nil {
		t.Fatalf("control: the same policy for role infra must be accepted, got %v", err)
	}
}

func TestEvaluateSpawnEscalation_NeverAutoRoleLeftOpenDespiteInjectedAutoRule(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	ctx := context.Background()

	for _, tc := range []struct {
		role       string
		wantStatus model.Status
	}{
		{"infra-admin", model.StatusOpen},
		{"infra", model.StatusClosed}, // control: same rule, ordinary role, auto-approved
	} {
		t.Run(tc.role, func(t *testing.T) {
			s, ms := newSpawnPolicyServer(t, tc.role)
			seedSpawnPolicyRequesterAgent(ms, "gd-captain-1", "gardenio-captain-1", "captain", "gardenio", "gardenio")
			// Written straight into the store: this bypasses
			// validateSpawnPolicyFields, as a bad SQL write or a rule that
			// predates the guard would.
			seedSpawnPolicyBead(ms, "gd-policy-"+tc.role, tc.role, []map[string]any{
				spawnPolicyAutoRule(map[string]any{}, 6, 2),
			})
			esc := seedSpawnEscalation(ms, "gd-esc-"+tc.role, "gardenio-captain-1", tc.role, "gardenio-"+tc.role+"-crew-1", "gardenio")

			s.evaluateSpawnEscalation(ctx, esc)

			if got := ms.beads["gd-esc-"+tc.role].Status; got != tc.wantStatus {
				t.Fatalf("escalation status = %s, want %s", got, tc.wantStatus)
			}
			var f map[string]any
			_ = json.Unmarshal(ms.beads["gd-esc-"+tc.role].Fields, &f)
			if tc.wantStatus == model.StatusOpen {
				if _, set := f["approved_by"]; set {
					t.Fatalf("a never-auto role escalation must not be approved, fields=%v", f)
				}
			} else if f["approved_by"] != spawnPolicyActor {
				t.Fatalf("control: approved_by = %v, want %q", f["approved_by"], spawnPolicyActor)
			}
		})
	}
}

// TestSweep_NeverAutoRoleEscalationNotAutoApprovedOrDenied runs the periodic
// sweep, the path that re-examines every open escalation each tick, with an
// auto rule and a no-match policy injected for infra-admin. Neither the
// approve nor the "no rule matched, deny and close" branch may fire.
func TestSweep_NeverAutoRoleEscalationNotAutoApprovedOrDenied(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	ctx := context.Background()
	s, ms := newSpawnPolicyServer(t, "infra-admin")
	makeRoleBead(ms, "gd-role-infra", "infra", true)
	seedSpawnPolicyRequesterAgent(ms, "gd-captain-1", "gardenio-captain-1", "captain", "gardenio", "gardenio")

	seedSpawnPolicyBead(ms, "gd-policy-infra-admin", "infra-admin", []map[string]any{
		spawnPolicyAutoRule(map[string]any{}, 6, 2),
	})
	// A second, no-match policy shape for the deny branch.
	seedSpawnPolicyBead(ms, "gd-policy-infra", "infra", []map[string]any{
		spawnPolicyAutoRule(map[string]any{"project": "someone-else"}, 6, 2),
	})
	seedSpawnEscalation(ms, "gd-esc-admin", "gardenio-captain-1", "infra-admin", "gardenio-admin-crew-1", "gardenio")
	seedSpawnEscalation(ms, "gd-esc-infra", "gardenio-captain-1", "infra", "gardenio-infra-crew-1", "gardenio")

	s.sweepPendingSpawnEscalations(ctx)

	if got := ms.beads["gd-esc-admin"].Status; got != model.StatusOpen {
		t.Fatalf("infra-admin escalation status after sweep = %s, want open", got)
	}
	// Control: the sweep did run, and denied the ordinary role's no-match request.
	if got := ms.beads["gd-esc-infra"].Status; got != model.StatusClosed {
		t.Fatalf("control: infra escalation status after sweep = %s, want closed (denied, no rule matched)", got)
	}
}

// TestHandleEscalation_NeverStampsFourEyes: a never-auto spawn escalation is
// not stamped requires_four_eyes (gd-TXbrvA9E6F). The ordinary role is the
// control that the fixture really files a spawn escalation.
func TestHandleEscalation_NeverStampsFourEyes(t *testing.T) {
	for _, role := range []string{"infra-admin", "infra"} {
		t.Run(role, func(t *testing.T) {
			h, ms := newSpawnAuthzTestServer(&stubOIDCVerifier{})
			makeRoleBead(ms, "gd-role-"+role, role, true)

			esc := fileSpawnEscalationViaHTTP(t, h, ms, role)
			var fields map[string]any
			if err := json.Unmarshal(esc.Fields, &fields); err != nil {
				t.Fatalf("unmarshal escalation fields: %v", err)
			}
			if fields["spawn_role"] != role {
				t.Fatalf("spawn_role = %v, want %q (the fixture did not escalate the role under test)", fields["spawn_role"], role)
			}
			if _, set := fields["requires_four_eyes"]; set {
				t.Fatalf("requires_four_eyes must not be stamped, fields=%v", fields)
			}
			if escalationRequiresFourEyes(esc) {
				t.Fatalf("escalationRequiresFourEyes = true, want false (fields=%v)", fields)
			}
		})
	}
}

// fileSpawnEscalationViaHTTP files a privileged-spawn escalation the way an
// agent pod does: a static-token create of an agent bead with X-Escalate,
// attributed to the laptop operator through X-Operated-By.
func fileSpawnEscalationViaHTTP(t *testing.T, h http.Handler, ms *mockStore, role string) *model.Bead {
	t.Helper()
	body, _ := json.Marshal(agentCreateBody("gardenio", role))
	req, _ := http.NewRequest("POST", "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer admin-token")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR2")
	req.Header.Set("X-Escalate", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	requireStatus(t, rec, 202)

	var resp EscalationResponse
	decodeJSON(t, rec, &resp)
	esc, _ := ms.GetBead(context.Background(), resp.EscalationID)
	if esc == nil {
		t.Fatal("escalation bead should exist in store")
	}
	return esc
}

// TestApproveEscalation_OwnerApprovesAgentFiledNeverAutoEscalation: the filer
// of an agent-filed escalation and the approver are the same operator (the
// owner's aliases), which a four-eyes stamp would 403 (gd-vjjLRTIy68). With no
// stamp the owner's approval through an approval-capable client succeeds.
func TestApproveEscalation_OwnerApprovesAgentFiledNeverAutoEscalation(t *testing.T) {
	const ownerTok = "oidc-owner"
	oidcMock := &escOIDCMock{tokens: map[string]escTokenInfo{
		ownerTok: {email: "owner@example.com", roles: []string{RoleAdmin}},
	}}
	for _, role := range []string{"infra-admin", "infra"} {
		t.Run(role, func(t *testing.T) {
			ms := newMockStore()
			srv := NewBeadsServer(ms, &events.NoopPublisher{})
			srv.userProvisioner = nil
			srv.SetForwardAuthClients([]string{testBrowserClient})
			h := srv.NewHTTPHandler(oidcMock, withTestToken(nil, "admin-token"))
			makeRoleBead(ms, "gd-role-"+role, role, true)
			ms.operatorAliases = map[string]int64{
				"slack\x00U0OPERATOR2":      11,
				"oidc\x00owner@example.com": 11,
			}

			esc := fileSpawnEscalationViaHTTP(t, h, ms, role)

			req, _ := http.NewRequest("POST", "/v1/escalations/"+esc.ID+"/approve", bytes.NewReader([]byte("{}")))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+ownerTok)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			requireStatus(t, rec, 200)
			var f map[string]any
			_ = json.Unmarshal(ms.beads[esc.ID].Fields, &f)
			if f["approved_by"] != "owner@example.com" {
				t.Fatalf("approved_by = %v, want the owner (fields=%v)", f["approved_by"], f)
			}
		})
	}
}
