package server

import (
	"context"
	"testing"
)

func relayCtx(idType, subject, fleet, relayed string) context.Context {
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: idType, Subject: subject})
	if fleet != "" {
		ctx = WithFleet(ctx, fleet)
	}
	if relayed != "" {
		ctx = WithOperatedByHeader(ctx, relayed)
	}
	return ctx
}

// seedOperatorInTeam gives kind:subject an operator alias and makes it the one
// member of a type:team bead teamID.
func seedOperatorInTeam(t *testing.T, ms *mockStore, kind, subject, teamID string) {
	t.Helper()
	if ms.operatorAliases == nil {
		ms.operatorAliases = map[string]int64{}
	}
	ms.operatorAliases[kind+"\x00"+subject] = int64(len(ms.operatorAliases) + 1)
	seedTeamBead(ms, teamID, `{"team":"`+teamID+`","members":["`+kind+`:`+subject+`"]}`)
}

// Only a bridge fleet subject's relay counts for access control. The static
// token (held by the viewer, controller, coopmux and, before Task 2, every
// agent) and an agent token relay attribution only.
func TestTrustedRelayOperator(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	const owner = "oidc:owner@example.com"
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"bridge relays", relayCtx("fleet", "slack-bridge", "slack-bridge", owner), owner},
		{"rotator gitlab subject relays", relayCtx("fleet", "bridge:gitlab", "gitlab-bridge", owner), owner},
		{"rotator slack subject relays", relayCtx("fleet", "bridge:slack", "slack-bridge", owner), owner},
		{"unlisted rotator subject does not", relayCtx("fleet", "bridge:acme-scif", "acme-scif-bridge", owner), ""},
		{"static does not", relayCtx("static", "static", "", owner), ""},
		{"agent does not", relayCtx("fleet", "crew-a", "agent-gd-a1", owner), ""},
		{"agent named like a bridge does not", relayCtx("fleet", "slack-bridge", "agent-gd-a1", owner), ""},
		{"unlisted fleet does not", relayCtx("fleet", "qa-fleet", "qa-fleet", owner), ""},
		{"listed subject on a non-bridge fleet does not", relayCtx("fleet", "github-bridge", "qa", owner), ""},
		{"rotator subject on a non-bridge fleet does not", relayCtx("fleet", "bridge:slack", "qa", owner), ""},
		{"unlisted bridge-suffixed fleet does not", relayCtx("fleet", "sandbox-bridge", "sandbox-bridge", owner), ""},
		{"edge does not", relayCtx("edge", "owner@example.com", "", owner), ""},
	}
	for _, c := range cases {
		if got := s.trustedRelayOperator(c.ctx); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestSetBridgeSubjects(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	s.SetBridgeSubjects(nil)
	if !s.isBridgeSubject("slack-bridge") {
		t.Fatal("nil must keep the default list")
	}
	s.SetBridgeSubjects([]string{" qa-fleet ", ""})
	if s.isBridgeSubject("slack-bridge") || !s.isBridgeSubject("qa-fleet") || s.isBridgeSubject("") || s.isBridgeSubject("bridge:slack") {
		t.Fatalf("override not applied: %v", s.bridgeSubjects)
	}
}

// The relay no longer resolves teams for a static caller; positive control:
// the same relay from the bridge does.
func TestTeamsOf_RelayOnlyFromBridge(t *testing.T) {
	s, ms, _ := newClassedTestServer(t)
	seedOperatorInTeam(t, ms, "oidc", "owner@example.com", "gd-team1")
	static := relayCtx("static", "static", "", "oidc:owner@example.com")
	if teams, _ := s.TeamsOf(static); len(teams) != 0 {
		t.Fatalf("static relay resolved teams %v; want none", teams)
	}
	agent := relayCtx("fleet", "crew-a", "agent-gd-a1", "oidc:owner@example.com")
	if teams, _ := s.TeamsOf(agent); len(teams) != 0 {
		t.Fatalf("agent relay resolved teams %v; want none", teams)
	}
	bridge := relayCtx("fleet", "slack-bridge", "slack-bridge", "oidc:owner@example.com")
	if teams, _ := s.TeamsOf(bridge); len(teams) != 1 || teams[0] != "gd-team1" {
		t.Fatalf("positive control: bridge relay teams = %v, want [gd-team1]", teams)
	}
	rotated := relayCtx("fleet", "bridge:slack", "slack-bridge", "oidc:owner@example.com")
	if teams, _ := s.TeamsOf(rotated); len(teams) != 1 || teams[0] != "gd-team1" {
		t.Fatalf("rotator-minted bridge relay teams = %v, want [gd-team1]", teams)
	}
}

// grantsOfOperator and granterOperatorID take the relayed human only from a
// bridge.
func TestGrantResolution_RelayOnlyFromBridge(t *testing.T) {
	s, ms, _ := newClassedTestServer(t)
	seedOperatorInTeam(t, ms, "oidc", "owner@example.com", "gd-team1")
	static := relayCtx("static", "static", "", "oidc:owner@example.com")
	if _, ok := s.granterOperatorID(static); ok {
		t.Fatal("static relay resolved a granter")
	}
	bridge := relayCtx("fleet", "slack-bridge", "slack-bridge", "oidc:owner@example.com")
	if _, ok := s.granterOperatorID(bridge); !ok {
		t.Fatal("positive control: bridge relay resolved no granter")
	}
}

// decisionAttribution keeps the static caller's relayed human out of
// responded_by; the bridge's relay is kept.
func TestDecisionAttribution_RelayOnlyFromBridge(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	if got := s.decisionAttribution(relayCtx("static", "static", "", "oidc:owner@example.com"), ""); got != "static" {
		t.Fatalf("static relay: responded_by = %q, want static", got)
	}
	if got := s.decisionAttribution(relayCtx("fleet", "slack-bridge", "slack-bridge", "oidc:owner@example.com"), ""); got != "oidc:owner@example.com" {
		t.Fatalf("positive control: bridge relay responded_by = %q", got)
	}
	if got := s.decisionAttribution(relayCtx("fleet", "bridge:slack", "slack-bridge", "oidc:owner@example.com"), ""); got != "oidc:owner@example.com" {
		t.Fatalf("rotator-minted bridge relay: responded_by = %q", got)
	}
	if got := s.decisionAttribution(relayCtx("fleet", "slack-bridge", "slack-bridge", ""), ""); got != "slack-bridge" {
		t.Fatalf("bridge without relay: responded_by = %q, want slack-bridge", got)
	}
}

// Verified v1 edge (authMethodEdge) is not an approver; verified v2 edge
// (authMethodOIDC with roles) is.
func TestEscalationApprover_EdgeV1Refused_V2Allowed(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	v1 := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "owner@example.com"}), authMethodEdge)
	if _, ok := s.escalationApprover(v1); ok {
		t.Fatal("verified v1 edge must not approve")
	}
	v2 := WithRoles(withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "edge", Subject: "owner@example.com"}), authMethodOIDC), []string{RoleOperator})
	if got, ok := s.escalationApprover(v2); !ok || got != "owner@example.com" {
		t.Fatalf("positive control: verified v2 edge approver = (%q,%v)", got, ok)
	}
}
