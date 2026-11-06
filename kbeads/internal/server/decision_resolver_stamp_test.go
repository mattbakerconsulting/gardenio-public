package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The resolver stamp (gd-ekhj3MJjEa): handleResolveDecision records who
// resolved and whether they were a gardenio-admin human, and no caller can
// write either field.

func withRoles(mutate func(*http.Request), roles ...string) func(*http.Request) {
	return func(r *http.Request) {
		mutate(r)
		*r = *r.WithContext(WithRoles(r.Context(), roles))
	}
}

// withMethod stamps the auth method the request's auth path pairs with its
// identity (authMethodOIDC for an OIDC bearer or a verified v2 edge envelope,
// authMethodMachine for a machine principal, authMethodStatic for an HTTP
// static or fleet token). Without it a fixture is a caller no request path
// builds, which the ownership gate and the grant table read differently.
func withMethod(mutate func(*http.Request), m authMethod) func(*http.Request) {
	return func(r *http.Request) {
		mutate(r)
		*r = *r.WithContext(withAuthMethod(r.Context(), m))
	}
}

func TestDecisionResolve_StampsResolver(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate      func(*http.Request)
		wantSubject string
		wantAdmin   bool
	}{
		"oidc admin":    {withRoles(identityMutator(AuthIdentity{Type: "oidc", Subject: "owner@example.com"}, ""), RoleAdmin), "owner@example.com", true},
		"oidc operator": {withRoles(identityMutator(AuthIdentity{Type: "oidc", Subject: "op@example.com"}, ""), RoleOperator), "op@example.com", false},
		"edge admin":    {withRoles(identityMutator(AuthIdentity{Type: "edge", Subject: "oidc:owner@example.com"}, ""), RoleAdmin), "oidc:owner@example.com", true},
		// A machine principal is never admin here, even with the role in context.
		"machine": {withRoles(identityMutator(AuthIdentity{Type: "machine", Subject: "machine:gardenio-controller"}, ""), RoleAdmin), "machine:gardenio-controller", false},
		// A bridge token relaying a human in X-Operated-By records the bridge,
		// never the relayed human and never admin. (Slack clicks reach the
		// daemon as an edge-auth envelope, the "edge admin" case.)
		"bridge relaying": {withRoles(identityMutator(AuthIdentity{Type: "fleet", Subject: "bridge:slack"}, "slack-bridge"), RoleAdmin), "bridge:slack", false},
	} {
		t.Run(name, func(t *testing.T) {
			ms, h := resolutionAuthzFixture(t, "gardenio")
			rec := postJSON(h, "/v1/decisions/gd-dec-target/resolve", map[string]any{"selected_option": "y"}, tc.mutate)
			if rec.Code != http.StatusOK {
				t.Fatalf("resolve = %d; body: %s", rec.Code, rec.Body.String())
			}
			var f map[string]any
			if err := json.Unmarshal(ms.beads["gd-dec-target"].Fields, &f); err != nil {
				t.Fatal(err)
			}
			if f[decisionResolverSubjectField] != tc.wantSubject {
				t.Errorf("resolver_subject = %v, want %q", f[decisionResolverSubjectField], tc.wantSubject)
			}
			if f[decisionResolverAdminField] != tc.wantAdmin {
				t.Errorf("resolver_admin = %v, want %v", f[decisionResolverAdminField], tc.wantAdmin)
			}
		})
	}
}

func TestDecisionResolverStamp_NoClientWrite(t *testing.T) {
	stamp := map[string]any{decisionResolverSubjectField: "oidc:owner@example.com", decisionResolverAdminField: true, decisionResolverClientField: DeviceGrantClientID}
	for name, ctx := range map[string]context.Context{
		"oidc admin": oidcCtx("owner@example.com", []string{RoleAdmin}),
		"bridge":     slackBridgeMachineCtx(),
		"agent":      agentFleetCtx("gd-crew", "crew-a"),
	} {
		t.Run("create/"+name, func(t *testing.T) {
			f := createDecision(t, ctx, stamp)
			if f[decisionResolverSubjectField] != "" || f[decisionResolverAdminField] != "" || f[decisionResolverClientField] != "" {
				t.Errorf("a created decision kept a resolver stamp: %v", f)
			}
		})
	}
	dec := &model.Bead{Type: model.TypeDecision}
	for _, key := range []string{decisionResolverSubjectField, decisionResolverAdminField, decisionResolverClientField} {
		if d := denyDecisionStampedKeyWrite("close", dec, []string{"chosen", key}); d == nil {
			t.Errorf("writing %s on a field endpoint or close must be denied", key)
		}
	}
	prior := json.RawMessage(`{"prompt":"x","resolver_subject":"op@example.com","resolver_admin":false}`)
	for name, tc := range map[string]struct {
		patched string
		wantErr bool
	}{
		"unchanged echo":               {`{"prompt":"y","resolver_subject":"op@example.com","resolver_admin":false}`, false},
		"admin flipped":                {`{"prompt":"x","resolver_subject":"op@example.com","resolver_admin":true}`, true},
		"subject repointed":            {`{"prompt":"x","resolver_subject":"oidc:owner@example.com","resolver_admin":false}`, true},
		"answer edit leaves the stamp": {`{"prompt":"x","resolver_subject":"op@example.com","resolver_admin":false,"chosen":"y"}`, false},
	} {
		t.Run("patch/"+name, func(t *testing.T) {
			err := checkDecisionRequestingAgentImmutable(&model.Bead{Type: model.TypeDecision, Fields: json.RawMessage(tc.patched)}, prior)
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	if err := checkDecisionRequestingAgentImmutable(&model.Bead{Type: model.TypeDecision, Fields: json.RawMessage(`{"prompt":"x","resolver_admin":true}`)}, json.RawMessage(`{"prompt":"x"}`)); err == nil {
		t.Error("a PATCH must not add resolver_admin to an unstamped decision")
	}
}

// The Slack bridge answers through a generic close carrying chosen, and a
// PATCH or a field endpoint can change the answer: each re-stamps the resolver
// from the caller.
func TestDecisionAnswerWrites_RestampResolver(t *testing.T) {
	// A verified v2 edge envelope: edge_auth.go stamps it as authMethodOIDC.
	admin := withMethod(withRoles(identityMutator(AuthIdentity{Type: "edge", Subject: "oidc:owner@example.com"}, ""), RoleAdmin), authMethodOIDC)
	machine := withMethod(identityMutator(AuthIdentity{Type: "machine", Subject: "machine:gardenio-controller"}, ""), authMethodMachine)
	stamp := func(t *testing.T, ms *mockStore) (any, any) {
		t.Helper()
		f := fieldsAsMap(ms.beads["gd-dec-own"].Fields)
		return f[decisionResolverSubjectField], f[decisionResolverAdminField]
	}
	t.Run("close with chosen", func(t *testing.T) {
		ms, h, _ := decisionFilerFixture(t)
		rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{"chosen": "y"}, admin)
		if rec.Code != http.StatusOK {
			t.Fatalf("close = %d; body: %s", rec.Code, rec.Body.String())
		}
		if sub, adm := stamp(t, ms); sub != "oidc:owner@example.com" || adm != true {
			t.Fatalf("stamp = %v / %v", sub, adm)
		}
	})
	t.Run("close with a forged stamp", func(t *testing.T) {
		ms, h, _ := decisionFilerFixture(t)
		rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{"chosen": "y", decisionResolverAdminField: true}, machine)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("close = %d, want 403", rec.Code)
		}
		if _, adm := stamp(t, ms); adm != nil {
			t.Fatalf("stamp written: %v", adm)
		}
	})
	t.Run("close without an answer leaves no stamp", func(t *testing.T) {
		ms, h, _ := decisionFilerFixture(t)
		if rec := doFilerJSON(h, "POST", "/v1/beads/gd-dec-own/close", map[string]any{"rationale": "withdrawn"}, admin); rec.Code != http.StatusOK {
			t.Fatalf("close = %d; body: %s", rec.Code, rec.Body.String())
		}
		if sub, _ := stamp(t, ms); sub != nil {
			t.Fatalf("stamped %v on a close that answered nothing", sub)
		}
	})
	t.Run("patch re-stamps", func(t *testing.T) {
		ms, h, _ := decisionFilerFixture(t)
		ms.beads["gd-dec-own"].Fields = json.RawMessage(`{"prompt":"Deploy?","chosen":"deny","resolver_subject":"owner@example.com","resolver_admin":true}`)
		rec := doFilerJSON(h, "PATCH", "/v1/beads/gd-dec-own", map[string]any{"fields": map[string]any{"chosen": "approve"}}, machine)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch = %d; body: %s", rec.Code, rec.Body.String())
		}
		if sub, adm := stamp(t, ms); sub != "machine:gardenio-controller" || adm != false {
			t.Fatalf("after a machine changed the answer: %v / %v", sub, adm)
		}
	})
}
