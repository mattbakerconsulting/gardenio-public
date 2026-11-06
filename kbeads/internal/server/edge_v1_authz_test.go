package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"gardenio/kbeads/internal/edgeauth"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-AvSKOUyqRz retired decision ka-0000001641: a verified v1 edge envelope
// (authMethodEdge) carries no roles, so it is non-role-bearing like the static
// token. Operator-level routes still admit it; every RoleAdmin route and every
// admin-only field gate refuses it. The auth-disabled bypass (authMethodNone)
// is unchanged and is the control for each site.

func v1EdgeCtx() context.Context {
	ctx := withAuthMethod(context.Background(), authMethodEdge)
	return WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "slack:U0TESTER1"})
}

func TestEdgeV1_RouteGates(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }
	serve := func(mw func(string, http.HandlerFunc) http.HandlerFunc, role string, ctx context.Context) int {
		rec := httptest.NewRecorder()
		mw(role, ok)(rec, httptest.NewRequest(http.MethodDelete, "/v1/beads/x-1", nil).WithContext(ctx))
		return rec.Code
	}
	none := withAuthMethod(context.Background(), authMethodNone)
	for wrap, mw := range map[string]func(string, http.HandlerFunc) http.HandlerFunc{
		"requireRole": requireRole, "requireRoleMachineAllowed": requireRoleMachineAllowed,
	} {
		if got := serve(mw, RoleAdmin, v1EdgeCtx()); got != http.StatusForbidden {
			t.Errorf("%s(admin) v1 edge = %d, want 403", wrap, got)
		}
		if got := serve(mw, RoleAdmin, none); got != http.StatusOK {
			t.Errorf("%s(admin) auth disabled = %d, want 200", wrap, got)
		}
	}
	if got := serve(requireRole, RoleOperator, v1EdgeCtx()); got != http.StatusOK {
		t.Errorf("requireRole(operator) v1 edge = %d, want 200", got)
	}
}

func TestEdgeV1_RBACInterceptors(t *testing.T) {
	unary := func(rpc string) error {
		_, err := RBACInterceptor(v1EdgeCtx(), nil, &grpc.UnaryServerInfo{FullMethod: rpc}, stubHandler)
		return err
	}
	stream := func(rpc string) error {
		ss := &wrappedServerStream{ctx: v1EdgeCtx()}
		return RBACStreamInterceptor(nil, ss, &grpc.StreamServerInfo{FullMethod: rpc}, func(any, grpc.ServerStream) error { return nil })
	}
	for name, call := range map[string]func(string) error{"unary": unary, "stream": stream} {
		for _, rpc := range []string{"/beads.v1.BeadsService/DeleteBead", "/beads.v1.BeadsService/DeleteConfig", "/beads.v1.BeadsService/Unclassified"} {
			if err := call(rpc); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s %s: err = %v, want PermissionDenied", name, rpc, err)
			}
		}
		if err := call("/beads.v1.BeadsService/CreateBead"); err != nil {
			t.Errorf("%s CreateBead: err = %v, want admitted (operator RPC)", name, err)
		}
	}
}

// Through the real middleware: a verified v1 envelope reaches an admin route
// and is refused there.
func TestEdgeAuthMiddleware_VerifiedV1RefusedOnAdminRoute(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	ts := strconv.FormatInt(now.Unix(), 10)
	const path = "/v1/beads/x-1"
	called := false
	h := EdgeAuthMiddleware(
		edgeauth.Config{HMACKey: []byte(testEdgeAuthKey), Enforce: true, Now: func() time.Time { return now }},
		requireRole(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
			called = true
			w.WriteHeader(http.StatusNoContent)
		}),
		http.NotFoundHandler(),
	)
	r := httptest.NewRequest(http.MethodDelete, path, nil)
	r.Header.Set(edgeauth.HeaderOperator, "alice@example.com")
	r.Header.Set(edgeauth.HeaderAuditID, "audit-v1-admin")
	r.Header.Set(edgeauth.HeaderTimestamp, ts)
	r.Header.Set(edgeauth.HeaderSignature, signEdge(t, "alice@example.com", "audit-v1-admin", path, ts))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden || called {
		t.Fatalf("status=%d called=%v, want 403 and handler not reached; body=%s", w.Code, called, w.Body.String())
	}
}

// One refusal per admin field gate, with authMethodNone as the control.
func TestEdgeV1_FieldGates(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	agent := &model.Bead{ID: "a-1", Type: "agent", Fields: json.RawMessage(`{"project":"claimlayer"}`)}
	channel := &model.Bead{ID: "ch-1", Type: typeChannel}
	gate := &model.Bead{ID: "gd-gate", Title: landsPauseGateTitle, Type: "gate", Status: model.StatusOpen}
	escalation := &model.Bead{ID: "e-1", Type: model.TypeEscalation, Fields: json.RawMessage(
		`{"operation_type":"create","operation_spec":{"type":"role","title":"r","fields":{"name":"r"}}}`)}

	// refused reports, per gate, whether the caller is refused.
	refused := func(ctx context.Context) map[string]bool {
		replay, err := s.replayWritePolicyDenial(ctx, escalation)
		if err != nil {
			t.Fatalf("replayWritePolicyDenial: %v", err)
		}
		return map[string]bool{
			"denyAgentIdentityFields":      s.denyAgentIdentityFields(ctx, "update", "agent", []string{"service_account"}) != nil,
			"authorizeChannelBindingKey":   s.authorizeChannelBindingKeyWrite(ctx, "update", channel, "slack_channel_id") != nil,
			"authorizeSetConfigKey":        authorizeSetConfigKey(ctx, "bridge_token:slack") != nil,
			"callerMayApproveAsAdmin":      !callerMayApproveAsAdmin(ctx),
			"escalationFieldsExempt":       !escalationFieldsExempt(ctx),
			"replayWritePolicyDenial":      replay != "",
			"denyUserIdentityFields":       s.denyUserIdentityFields(ctx, "update", []string{"slack_id"}) != nil,
			"authorizeSetConfig":           s.authorizeSetConfig(ctx, &model.Config{Key: "type:user", Value: json.RawMessage(`{}`)}) != nil,
			"authorizeAgentProjectUpdate":  authorizeAgentProjectUpdate(ctx, agent, json.RawMessage(`{"project":"gardenio"}`)) != nil,
			"denyAgentRoleKeyWrite":        s.denyAgentRoleKeyWrite(ctx, "update", []string{agentRoleField}) != nil,
			"isAdminCaller":                !isAdminCaller(ctx),
			"killSwitchWriter":             !killSwitchWriter(ctx),
			"authorizeKillSwitchLifecycle": s.authorizeKillSwitchLifecycle(ctx, "close", gate, nil, nil) != nil,
		}
	}
	for gate, denied := range refused(v1EdgeCtx()) {
		if !denied {
			t.Errorf("%s admitted a v1 edge envelope", gate)
		}
	}
	for gate, denied := range refused(withAuthMethod(context.Background(), authMethodNone)) {
		if denied {
			t.Errorf("%s refused an auth-disabled daemon", gate)
		}
	}
}

// escalationApprover takes an edge identity only from a verified v2 envelope
// carrying at least gardenio-operator (gd-dMof1PW1oJ). A verified v1 envelope
// names any operator its signer likes, and a burn-in-trusted envelope that did
// not verify is pinned to viewer: neither approves, so neither can satisfy an
// approvers list or a four-eyes check under someone else's name.
func TestEscalationApprover_EdgeOnlyVerifiedV2(t *testing.T) {
	srv := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
	edge := func(method authMethod, roles ...string) context.Context {
		ctx := WithRoles(withAuthMethod(context.Background(), method), roles)
		return WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "owner@example.com"})
	}
	for name, c := range map[string]struct {
		ctx  context.Context
		want bool
	}{
		"verified v1":          {v1EdgeCtx(), false},
		"burn-in viewer":       {edge(authMethodOIDC, RoleViewer), false},
		"v2 without roles":     {edge(authMethodOIDC), false},
		"verified v2 operator": {edge(authMethodOIDC, RoleOperator), true},
		"verified v2 admin":    {edge(authMethodOIDC, RoleAdmin), true},
	} {
		got, ok := srv.escalationApprover(c.ctx)
		if ok != c.want {
			t.Errorf("%s: escalationApprover ok = %v (%q), want %v", name, ok, got, c.want)
		}
	}
}
