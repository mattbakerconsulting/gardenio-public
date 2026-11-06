package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// The agent token's created_by is the agent name from the token subject; an
// X-Actor header cannot change it. X-Actor is not honored for any bearer,
// including the one that used to be the shared admin token.
func TestAgentToken_CreatedByIsAgentNameNotXActor(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	body := map[string]any{"title": "t", "type": "task", "labels": []string{"project:gardenio"}}

	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, body, map[string]string{"X-Actor": "impostor", "X-Operated-By": "oidc:alice@example.com"})
	requireStatus(t, rec, http.StatusCreated)
	var got map[string]any
	decodeJSON(t, rec, &got)
	if got["created_by"] != "crew-a" {
		t.Fatalf("agent token created_by = %v, want crew-a", got["created_by"])
	}

	rec = doBearerJSON(t, h, "POST", "/v1/beads", testStaticToken, body, map[string]string{"X-Actor": "static-actor", "X-Operated-By": "oidc:alice@example.com"})
	requireStatus(t, rec, http.StatusCreated)
	decodeJSON(t, rec, &got)
	if got["created_by"] != testBearerSubject {
		t.Fatalf("former admin bearer with X-Actor: created_by = %v, want %q (X-Actor ignored)", got["created_by"], testBearerSubject)
	}
}

// The agent branch honours X-Trigger-Bead and X-No-Auto-Parent (agent-pod
// headers); no other fleet token does.
func TestAgentToken_HonoursAgentPodHeaders(t *testing.T) {
	s, _, _ := newClassedTestServer(t)
	var trigger string
	var noParent bool
	capture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trigger = triggerBeadFromContext(r.Context())
		noParent = noAutoParentFromContext(r.Context())
	})
	mw := AuthMiddleware(withTestToken(s.fleetCfg, testStaticToken), &bearerSubjectVerifier{}, capture)

	for _, tc := range []struct {
		bearer       string
		wantHonoured bool
	}{
		{testAgentToken, true},
		{testStaticToken, false},     // the former admin bearer is a plain fleet token now
		{testOtherFleetToken, false}, // a non-agent fleet token does not get agent headers
	} {
		trigger, noParent = "", false
		req := httptest.NewRequest("GET", "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+tc.bearer)
		req.Header.Set("X-Trigger-Bead", "gd-trigger1")
		req.Header.Set("X-No-Auto-Parent", "1")
		mw.ServeHTTP(httptest.NewRecorder(), req)
		if (trigger == "gd-trigger1") != tc.wantHonoured || noParent != tc.wantHonoured {
			t.Errorf("bearer %s: trigger=%q noParent=%v, want honoured=%v", tc.bearer, trigger, noParent, tc.wantHonoured)
		}
	}
}

// agentNameFromContext recognises only agent-prefixed fleet identities.
func TestAgentNameFromContext(t *testing.T) {
	ctx := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "agent-gd-a1")
	if name, ok := agentNameFromContext(ctx); !ok || name != "crew-a" {
		t.Fatalf("agent fleet: got (%q,%v)", name, ok)
	}
	bridge := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}), "slack-bridge")
	if _, ok := agentNameFromContext(bridge); ok {
		t.Fatal("a bridge fleet identity is not an agent")
	}
	static := WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "crew-a"})
	if _, ok := agentNameFromContext(static); ok {
		t.Fatal("the static token is not an agent identity, even with an agent-looking X-Actor")
	}
}

// An escalation filed with an agent token gets requesting_agent stamped from
// the token's agent bead id (the key the bridge and the continuation gate
// share, not the agent name), whatever the body says, and marked verified.
func TestEscalationRequestingAgent_StampedForAgentToken(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	ms.beads["gd-a1"] = &model.Bead{ID: "gd-a1", Type: "agent", Fields: agentFields("gardenio", "crew")}
	rec := doBearerJSON(t, h, "POST", "/v1/beads", testAgentToken, map[string]any{
		"title": "esc", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"},
		"fields": map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}, "requesting_agent": "victim"},
	}, nil)
	requireStatus(t, rec, http.StatusCreated)
	var created map[string]any
	decodeJSON(t, rec, &created)
	var fields map[string]any
	_ = json.Unmarshal(ms.beads[created["id"].(string)].Fields, &fields)
	if fields["requesting_agent"] != "gd-a1" || fields["requesting_agent_verified"] != "true" {
		t.Fatalf("requesting_agent = %v verified = %v, want gd-a1 verified", fields["requesting_agent"], fields["requesting_agent_verified"])
	}
}

// Pins behaviour that matters once agents hold fleet tokens: no fleet token,
// agent or otherwise, can mint a bridge/fleet token. Only a machine principal
// with the token-mint capability can (machine_grants_test.go).
func TestAgentToken_CannotMintBridgeToken(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	body := map[string]any{"token": "minted-" + time.Now().Format("150405.000"), "bridge": "slack-bridge"}
	if rec := doBearerJSON(t, h, "POST", "/v1/bridge-tokens", testAgentToken, body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("agent token minting a bridge token: status %d, want 403", rec.Code)
	}
	// The former shared admin bearer is just a fleet token now: it mints nothing.
	body["token"] = "minted2-" + time.Now().Format("150405.000")
	if rec := doBearerJSON(t, h, "POST", "/v1/bridge-tokens", testStaticToken, body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("fleet token minting a service identity: status %d, want 403; body %s", rec.Code, rec.Body.String())
	}
	body["token"] = "minted3-" + time.Now().Format("150405.000")
	body["bridge"] = "qa-fleet"
	if rec := doBearerJSON(t, h, "POST", "/v1/bridge-tokens", testStaticToken, body, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("fleet token minting an ordinary fleet: status %d, want 403; body %s", rec.Code, rec.Body.String())
	}
}

// A header-less write by a per-agent token resolves its operator from the
// agent bead named by the bead id in its fleet name (agent-<bead id>), not by
// the agent name: names are not unique across projects. A non-agent fleet
// identity with the same subject does not.
func TestResolveOperatedByPure_AgentFleet_WalksAgentBead(t *testing.T) {
	srv := newChainWalkServer(&model.Bead{
		ID: "gd-a1", Type: "agent",
		Fields: []byte(`{"slack_user_id":"U0OPERATOR1"}`),
	})

	agentCtx := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "agent-gd-a1")
	value, source := srv.resolveOperatedByPure(agentCtx)
	if value != "slack:U0OPERATOR1" || source != operatedBySourceFallback {
		t.Fatalf("agent fleet: got (%q,%q), want (slack:U0OPERATOR1,%q)", value, source, operatedBySourceFallback)
	}

	bridgeCtx := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "crew-a"}), "slack-bridge")
	if value, _ := srv.resolveOperatedByPure(bridgeCtx); value == "slack:U0OPERATOR1" {
		t.Fatalf("non-agent fleet identity must not walk the agent bead, got %q", value)
	}
}
