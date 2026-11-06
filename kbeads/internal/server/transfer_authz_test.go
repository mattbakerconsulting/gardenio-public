package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-i6GZX5jzHM: POST /v1/beads/{id}/transfer moves a bead, with its deps,
// labels, comments and events, to another fleet. It was requireRole(RoleOperator)
// — open to the shared static token and every fleet token — and never ran the
// bead's write_policy. Only an OIDC gardenio-admin session may transfer now.

const (
	transferStaticToken = "transfer-primary-static"
	transferFleetToken  = "transfer-bridge-fleet"
	transferAdmin       = "admin@example.com"
	transferOperator    = "operator@example.com"
)

// newTransferTestServer wires the full HTTP auth chain with a primary static
// token, one fleet token and an OIDC verifier granting per-subject roles.
func newTransferTestServer(t *testing.T) (*mockStore, http.Handler) {
	t.Helper()
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	s.userProvisioner = nil
	fc := NewFleetConfig("default", map[string]string{transferFleetToken: "alpha"})
	verifier := &taintTestVerifier{roles: map[string][]string{
		transferAdmin:    {RoleAdmin},
		transferOperator: {RoleOperator},
	}}
	ms.beads["gd-xfer"] = &model.Bead{ID: "gd-xfer", Title: "Transfer me", Type: "task", Status: model.StatusOpen, Fleet: "alpha"}
	return ms, s.NewHTTPHandler(verifier, withTestToken(fc, transferStaticToken))
}

func TestTransferBead_HTTPRefusesStaticFleetAndNonAdmin(t *testing.T) {
	for _, c := range []struct {
		name, bearer string
	}{
		{"static token", transferStaticToken},
		{"fleet token", transferFleetToken},
		{"oidc operator", transferOperator},
	} {
		t.Run(c.name, func(t *testing.T) {
			ms, h := newTransferTestServer(t)
			rec := doJSONAs(t, h, "POST", "/v1/beads/gd-xfer/transfer", map[string]string{"to_fleet": "beta"}, c.bearer)
			requireStatus(t, rec, http.StatusForbidden)
			if got := ms.beads["gd-xfer"].Fleet; got != "alpha" {
				t.Fatalf("refused transfer still moved the bead to fleet %q", got)
			}
		})
	}
}

func TestTransferBead_HTTPRefusesBeforeRevealingBead(t *testing.T) {
	_, h := newTransferTestServer(t)
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-nope/transfer", map[string]string{"to_fleet": "beta"}, transferStaticToken)
	requireStatus(t, rec, http.StatusForbidden)
}

func TestTransferBead_HTTPOIDCAdminAllowed(t *testing.T) {
	ms, h := newTransferTestServer(t)
	rec := doJSONAs(t, h, "POST", "/v1/beads/gd-xfer/transfer", map[string]string{"to_fleet": "beta"}, transferAdmin)
	requireStatus(t, rec, http.StatusOK)
	if got := ms.beads["gd-xfer"].Fleet; got != "beta" {
		t.Fatalf("admin transfer left the bead in fleet %q, want beta", got)
	}
}

// The handler gates itself: requireRole passes a verified v1 edge envelope and
// an auth-disabled daemon, and a protocol adapter could dispatch to the
// handler directly. Driven here without any route wrapper.
func TestTransferBead_HandlerGate(t *testing.T) {
	edgeV2 := func(roles ...string) context.Context {
		ctx := withAuthMethod(context.Background(), authMethodOIDC)
		ctx = WithRoles(ctx, roles)
		return WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "oidc:ada@example.com"})
	}
	land := WithRoles(withAuthMethod(context.Background(), authMethodLand), []string{RoleAdmin})
	land = WithAuthIdentity(land, AuthIdentity{Type: "land", Subject: "land:alpha"})

	cases := []struct {
		name  string
		ctx   context.Context
		allow bool
	}{
		{"static", machineUserCtx(), false},
		{"fleet", fleetUserCtx(), false},
		{"edge v1", WithAuthIdentity(withAuthMethod(context.Background(), authMethodEdge), AuthIdentity{Type: "edge", Subject: "slack:U0TESTER1"}), false},
		{"edge v2 operator", edgeV2(RoleViewer, RoleOperator), false},
		{"edge burn-in viewer", edgeV2(RoleViewer), false},
		{"machine with admin role", machineCtx("gardenio-controller", RoleAdmin), false},
		{"land carrying admin role", land, false},
		{"oidc operator", oidcCtx(transferOperator, []string{RoleOperator}), false},
		{"oidc admin", oidcCtx(transferAdmin, []string{RoleAdmin}), true},
		{"edge v2 admin", edgeV2(RoleAdmin), true},
		{"auth disabled", withAuthMethod(context.Background(), authMethodNone), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ms := newMockStore()
			s := NewBeadsServer(ms, &events.NoopPublisher{})
			s.userProvisioner = nil
			ms.beads["gd-xfer"] = &model.Bead{ID: "gd-xfer", Title: "Transfer me", Type: "task", Status: model.StatusOpen, Fleet: "alpha", CreatedBy: transferAdmin}
			mux := http.NewServeMux()
			mux.HandleFunc("POST /v1/beads/{id}/transfer", s.handleTransferBead)

			req := oidcCtxReq("POST", "/v1/beads/gd-xfer/transfer", map[string]string{"to_fleet": "beta"}, "", nil).WithContext(c.ctx)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if c.allow {
				requireStatus(t, rec, http.StatusOK)
				if got := ms.beads["gd-xfer"].Fleet; got != "beta" {
					t.Fatalf("allowed transfer left the bead in fleet %q", got)
				}
				return
			}
			requireStatus(t, rec, http.StatusForbidden)
			if got := ms.beads["gd-xfer"].Fleet; got != "alpha" {
				t.Fatalf("refused transfer still moved the bead to fleet %q", got)
			}
		})
	}
}

// There is no gRPC transfer RPC (GRPCClient.TransferBead returns an error), so
// gRPC never reaches handleTransferBead. A transfer RPC added later must carry
// callerMayTransferBead; this fails until it is reviewed here.
func TestTransferBead_NoGRPCRoute(t *testing.T) {
	for _, m := range beadsv1.BeadsService_ServiceDesc.Methods {
		if strings.Contains(strings.ToLower(m.MethodName), "transfer") {
			t.Errorf("gRPC method %s moves beads between fleets: gate it with callerMayTransferBead and add its refusal cases here", m.MethodName)
		}
	}

}

// A verified v1 edge envelope no longer skips write_policy (write_authz.go):
// it is evaluated like the static token, so an admin-gated type refuses it
// unless the pair is in machineAllowedWrites.
func TestAuthorizeWrite_EdgeV1_EvaluatedLikeStatic(t *testing.T) {
	srv, _, _ := testCtx(t)
	edge := withAuthMethod(context.Background(), authMethodEdge)

	d := srv.authorizeWrite(edge, "create", "team", nil)
	if d == nil {
		t.Fatal("verified v1 edge envelope must not bypass write_policy on admin-gated type team")
	}
	if d.RequiredRole != RoleAdmin {
		t.Errorf("RequiredRole=%q, want %q", d.RequiredRole, RoleAdmin)
	}
	if staticD := srv.authorizeWrite(staticCtx(), "create", "team", nil); staticD == nil || staticD.RequiredRole != d.RequiredRole {
		t.Errorf("edge v1 and static token must get the same answer on create/team: edge=%+v static=%+v", d, staticD)
	}
	if d := srv.authorizeWrite(edge, "create", "report", nil); d != nil {
		t.Errorf("machine-allowlisted create/report should still pass for edge v1 like static, got %+v", d)
	}
	if d := srv.authorizeWrite(edge, "create", "task", nil); d != nil {
		t.Errorf("type with no write_policy should pass for edge v1, got %+v", d)
	}
}

// The same refusal on the gRPC path: CreateBead runs authorizeWrite, and the
// Connect shim carries a v1 edge context to it.
func TestCreateBeadGRPC_EdgeV1_AdminGatedTypeDenied(t *testing.T) {
	srv, _, _ := testCtx(t)
	ctx := withAuthMethod(context.Background(), authMethodEdge)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "edge", Subject: "slack:U0TESTER1"})
	ctx = WithOperatedByHeader(ctx, "slack:U0TESTER1")

	_, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{Title: "edge rig", Type: "rig"})
	requireCode(t, err, codes.PermissionDenied)
}
