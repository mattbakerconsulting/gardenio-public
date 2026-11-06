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

// Configurable four-eyes (gd-kvrZdjItZV): a human-mode spawn_policy rule may
// set four_eyes=true, which makes handleEscalation stamp requires_four_eyes on
// the escalation it files. Default is off, because a single-human org can never
// satisfy the stamp (gd-TXbrvA9E6F). Validation is at write time, so approval
// time never meets a stamp nobody else can satisfy.

func fourEyesHumanRule(approvers ...string) map[string]any {
	r := spawnPolicyHumanRule(map[string]any{})
	r["four_eyes"] = true
	if approvers != nil {
		r["approvers"] = approvers
	}
	return r
}

func TestValidateSpawnPolicyFields_FourEyes(t *testing.T) {
	tests := []struct {
		name    string
		rule    map[string]any
		wantErr string
	}{
		{"four_eyes with no approvers is refused", fourEyesHumanRule(), "four_eyes"},
		{"four_eyes with one approver is refused", fourEyesHumanRule("oidc:owner@example.com"), "four_eyes"},
		{"the same approver twice, bare and prefixed, is one", fourEyesHumanRule("owner@example.com", "OIDC:Owner@example.com"), "four_eyes"},
		{"a team entry is not a human approver", fourEyesHumanRule("oidc:owner@example.com", "team:gd-team-admins"), "four_eyes"},
		{"two distinct approvers are accepted", fourEyesHumanRule("oidc:owner@example.com", "oidc:other@example.com"), ""},
		{"four_eyes on an auto rule is refused", func() map[string]any {
			r := spawnPolicyAutoRule(map[string]any{}, 6, 1)
			r["four_eyes"] = true
			r["approvers"] = []string{"a@example.com", "b@example.com"}
			return r
		}(), "four_eyes"},
		{"control: a human rule without four_eyes needs no approvers", spawnPolicyHumanRule(map[string]any{}), ""},
		{"control: four_eyes=false with one approver is accepted", func() map[string]any {
			r := spawnPolicyHumanRule(map[string]any{})
			r["four_eyes"] = false
			r["approvers"] = []string{"oidc:owner@example.com"}
			return r
		}(), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason := validateSpawnPolicyFields(mustMarshal(map[string]any{
				"role": "infra", "rules": []map[string]any{tt.rule},
			}))
			if tt.wantErr == "" && reason != "" {
				t.Fatalf("expected no rejection, got %q", reason)
			}
			if tt.wantErr != "" && !strings.Contains(reason, tt.wantErr) {
				t.Fatalf("rejection should mention %q, got %q", tt.wantErr, reason)
			}
		})
	}
}

// TestCreateSpawnPolicyBead_FourEyesApproversMustBeDistinctOperators drives the
// create wiring: two approver aliases linked to ONE operator row are one human.
func TestCreateSpawnPolicyBead_FourEyesApproversMustBeDistinctOperators(t *testing.T) {
	ctx := WithRoles(ctxAsTestSubject(), []string{RoleAdmin})
	policy := func(approvers ...string) createBeadInput {
		return createBeadInput{
			Title: "infra spawn policy",
			Type:  "spawn_policy",
			Kind:  "config",
			Fields: mustMarshal(map[string]any{
				"role":  "infra",
				"rules": []map[string]any{fourEyesHumanRule(approvers...)},
			}),
		}
	}

	s, ms := newAutoCloseServer()
	ms.operatorAliases = map[string]int64{
		"slack\x00U0OWNER":          11,
		"oidc\x00owner@example.com": 11,
		"oidc\x00other@example.com": 12,
	}

	if _, err := s.createBead(ctx, policy("oidc:owner@example.com")); err == nil {
		t.Fatal("one approver must be refused at write")
	}
	_, err := s.createBead(ctx, policy("slack:U0OWNER", "oidc:owner@example.com"))
	if err == nil || !strings.Contains(err.Error(), "distinct operators") {
		t.Fatalf("two aliases of one operator must be refused as not distinct, got %v", err)
	}
	if _, err := s.createBead(ctx, policy("slack:U0OWNER", "oidc:other@example.com")); err != nil {
		t.Fatalf("two distinct operators must be accepted, got %v", err)
	}
}

// fourEyesApprovalServer wires an HTTP handler with two OIDC approvers. The
// owner (operator 11) is also the operator the escalation is filed under.
func fourEyesApprovalServer(t *testing.T, role string, rules []map[string]any) (http.Handler, *mockStore) {
	t.Helper()
	oidcMock := &escOIDCMock{tokens: map[string]escTokenInfo{
		"oidc-owner": {email: "owner@example.com", roles: []string{RoleAdmin}},
		"oidc-other": {email: "other@example.com", roles: []string{RoleAdmin}},
	}}
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetForwardAuthClients([]string{testBrowserClient})
	h := srv.NewHTTPHandler(oidcMock, withTestToken(nil, "admin-token"))
	makeRoleBead(ms, "gd-role-"+role, role, true)
	ms.operatorAliases = map[string]int64{
		"slack\x00U0OPERATOR2":      11,
		"oidc\x00owner@example.com": 11,
		"oidc\x00other@example.com": 12,
	}
	seedSpawnPolicyBead(ms, "gd-policy-"+role, role, rules)
	return h, ms
}

func approveAs(h http.Handler, token, escID string) *httptest.ResponseRecorder {
	req, _ := http.NewRequest("POST", "/v1/escalations/"+escID+"/approve", bytes.NewReader([]byte("{}")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Default off: a human rule without four_eyes stamps nothing, so the owner
// (the requester's own operator) can approve an agent-filed escalation.
func TestFourEyes_DefaultOffOwnerCanApprove(t *testing.T) {
	for _, role := range []string{"infra", "infra-admin"} {
		t.Run(role, func(t *testing.T) {
			h, ms := fourEyesApprovalServer(t, role, []map[string]any{
				spawnPolicyHumanRule(map[string]any{}),
			})
			esc := fileSpawnEscalationViaHTTP(t, h, ms, role)
			if escalationRequiresFourEyes(esc) {
				t.Fatalf("a rule without four_eyes must not stamp requires_four_eyes, fields=%s", esc.Fields)
			}
			requireStatus(t, approveAs(h, "oidc-owner", esc.ID), 200)
		})
	}
}

// four_eyes=true with two approvers: the requester's operator is refused, the
// other approver succeeds.
func TestFourEyes_RequesterOperatorRefusedOtherApproverSucceeds(t *testing.T) {
	h, ms := fourEyesApprovalServer(t, "infra", []map[string]any{
		fourEyesHumanRule("oidc:owner@example.com", "oidc:other@example.com"),
	})
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra")
	if !escalationRequiresFourEyes(esc) {
		t.Fatalf("a four_eyes rule must stamp requires_four_eyes, fields=%s", esc.Fields)
	}

	requireStatus(t, approveAs(h, "oidc-owner", esc.ID), 403)
	if got := ms.beads[esc.ID].Status; got != model.StatusOpen {
		t.Fatalf("escalation status after the refused approval = %s, want open", got)
	}

	requireStatus(t, approveAs(h, "oidc-other", esc.ID), 200)
	var f map[string]any
	_ = json.Unmarshal(ms.beads[esc.ID].Fields, &f)
	if f["approved_by"] != "other@example.com" {
		t.Fatalf("approved_by = %v, want other@example.com (fields=%v)", f["approved_by"], f)
	}
}

// The stamp follows the FIRST matching rule: a four_eyes rule scoped to another
// project does not stamp this project's escalation.
func TestFourEyes_OnlyTheMatchingRuleStamps(t *testing.T) {
	scoped := fourEyesHumanRule("oidc:owner@example.com", "oidc:other@example.com")
	scoped["when"] = map[string]any{"project": "someone-else"}
	h, ms := fourEyesApprovalServer(t, "infra", []map[string]any{
		scoped,
		spawnPolicyHumanRule(map[string]any{}),
	})
	esc := fileSpawnEscalationViaHTTP(t, h, ms, "infra")
	if escalationRequiresFourEyes(esc) {
		t.Fatalf("a non-matching four_eyes rule must not stamp, fields=%s", esc.Fields)
	}
}

// Never-auto stays code-level and independent of four_eyes: with four_eyes
// false (or absent) on every rule, an injected auto rule still never approves
// infra-admin, and the control role is approved.
func TestFourEyes_FalseStillNeverAutoApprovesInfraAdmin(t *testing.T) {
	t.Setenv("BEADS_SPAWN_POLICY_ENABLED", "1")
	ctx := context.Background()
	for _, tc := range []struct {
		role       string
		wantStatus model.Status
	}{
		{"infra-admin", model.StatusOpen},
		{"infra", model.StatusClosed}, // control
	} {
		t.Run(tc.role, func(t *testing.T) {
			s, ms := newSpawnPolicyServer(t, tc.role)
			seedSpawnPolicyRequesterAgent(ms, "gd-captain-1", "gardenio-captain-1", "captain", "gardenio", "gardenio")
			auto := spawnPolicyAutoRule(map[string]any{}, 6, 2)
			auto["four_eyes"] = false
			seedSpawnPolicyBead(ms, "gd-policy-"+tc.role, tc.role, []map[string]any{auto})
			seedSpawnEscalation(ms, "gd-esc-"+tc.role, "gardenio-captain-1", tc.role, "gardenio-"+tc.role+"-crew-1", "gardenio")

			s.evaluateSpawnEscalation(ctx, ms.beads["gd-esc-"+tc.role])

			if got := ms.beads["gd-esc-"+tc.role].Status; got != tc.wantStatus {
				t.Fatalf("escalation status = %s, want %s", got, tc.wantStatus)
			}
		})
	}
}
