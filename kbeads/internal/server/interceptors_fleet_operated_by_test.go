package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// These tests pin the fleet-token bridge-attribution fix (ka-0000008252).
//
// The resolver resolveOperatedByPure has a `fleet` case written to honor
// X-Operated-By so the slack/jira/gitlab bridges (which authenticate with a
// FLEET token) can re-attribute the external-platform human to a kbeads
// operator — pinned by TestResolveAuditOperator_Fleet_HonorsHeader. But the
// resolver only sees the value if the auth interceptor first stamps it into
// ctx via applyOperatedByHeader. Before this fix that stamping happened ONLY
// inside `if id.Type == "static"`, so a fleet token's X-Operated-By header
// never reached the resolver and every bridge-spawned bead's operated_by
// column fell through to the fleet:slack-bridge sentinel — Matt's mention
// attribution disappeared. These tests guard the interceptor layer that the
// resolver test cannot reach (it injects the ctx value directly).

// fleetTok is a token mapped to a non-default fleet so the interceptor
// classifies the caller as AuthIdentity{Type:"fleet"} — the same shape the
// slack-bridge presents in production.
const fleetTok = "tok-bridge"

func newBridgeFleetConfig() *FleetConfig {
	return NewFleetConfig("gardenio", map[string]string{fleetTok: "slack-bridge"})
}

func TestAuthMiddleware_FleetToken_StampsOperatedByHeader(t *testing.T) {
	fc := newBridgeFleetConfig()

	t.Run("HumanHeaderStamped", func(t *testing.T) {
		var gotHeader string
		var gotType string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = operatedByHeaderFromContext(r.Context())
			gotType = AuthIdentityFromContext(r.Context()).Type
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+fleetTok)
		req.Header.Set("X-Operated-By", "oidc:matthew.baker@acme.example")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if gotType != "fleet" {
			t.Fatalf("auth type = %q; want fleet (precondition for this regression)", gotType)
		}
		if gotHeader != "oidc:matthew.baker@acme.example" {
			t.Fatalf("operatedByHeaderFromContext = %q; want oidc:matthew.baker@acme.example "+
				"(fleet token must stamp X-Operated-By so the resolver attributes the human)", gotHeader)
		}
	})

	t.Run("SlackHumanHeaderStamped", func(t *testing.T) {
		var gotHeader string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = operatedByHeaderFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+fleetTok)
		req.Header.Set("X-Operated-By", "slack:U0OPERATOR1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if gotHeader != "slack:U0OPERATOR1" {
			t.Fatalf("operatedByHeaderFromContext = %q; want slack:U0OPERATOR1", gotHeader)
		}
	})

	t.Run("NonHumanHeaderRejected", func(t *testing.T) {
		// sanitizeOperatedByHeader fail-closes on non-human shapes; a fleet
		// token cannot smuggle an agent-name / fleet-sentinel through the
		// header. The interceptor must end up with an empty stamped value so
		// the resolver falls back to the fleet sentinel (service-driven write).
		var gotHeader string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHeader = operatedByHeaderFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+fleetTok)
		req.Header.Set("X-Operated-By", "fleet:slack-bridge")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if gotHeader != "" {
			t.Fatalf("operatedByHeaderFromContext = %q; want empty (non-human header must be rejected)", gotHeader)
		}
	})

	t.Run("OwnerOverrideStamped", func(t *testing.T) {
		var gotOwner string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotOwner = ownerOverrideFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+fleetTok)
		req.Header.Set("X-Owner-Override", "oidc:matthew.baker@acme.example")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if gotOwner != "oidc:matthew.baker@acme.example" {
			t.Fatalf("ownerOverrideFromContext = %q; want oidc:matthew.baker@acme.example", gotOwner)
		}
	})
}

func TestAuthInterceptor_FleetToken_StampsOperatedByHeader(t *testing.T) {
	fc := newBridgeFleetConfig()

	t.Run("HumanHeaderStamped", func(t *testing.T) {
		var gotHeader string
		var gotType string
		handler := func(ctx context.Context, _ any) (any, error) {
			gotHeader = operatedByHeaderFromContext(ctx)
			gotType = AuthIdentityFromContext(ctx).Type
			return "ok", nil
		}
		interceptor := AuthInterceptor(withTestToken(fc, "secret"), nil)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"authorization", "Bearer "+fleetTok,
			"x-operated-by", "oidc:matthew.baker@acme.example",
		))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"}, handler)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotType != "fleet" {
			t.Fatalf("auth type = %q; want fleet (precondition for this regression)", gotType)
		}
		if gotHeader != "oidc:matthew.baker@acme.example" {
			t.Fatalf("operatedByHeaderFromContext = %q; want oidc:matthew.baker@acme.example", gotHeader)
		}
	})

	t.Run("NonHumanHeaderRejected", func(t *testing.T) {
		var gotHeader string
		handler := func(ctx context.Context, _ any) (any, error) {
			gotHeader = operatedByHeaderFromContext(ctx)
			return "ok", nil
		}
		interceptor := AuthInterceptor(withTestToken(fc, "secret"), nil)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(
			"authorization", "Bearer "+fleetTok,
			"x-operated-by", "fleet:slack-bridge",
		))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"}, handler)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if gotHeader != "" {
			t.Fatalf("operatedByHeaderFromContext = %q; want empty (non-human header must be rejected)", gotHeader)
		}
	})
}

// TestFleetToken_CreateBead_PersistsHumanOperatedBy is the end-to-end guard
// that would have caught the original bug (ka-0000008252): a fleet-token
// CreateBead carrying X-Operated-By for the human must persist the human as
// the authoritative operated_by COLUMN — not the fleet:slack-bridge sentinel.
//
// It drives the request through the real gRPC AuthInterceptor (so header
// stamping happens exactly as in production), then through CreateBead, and
// asserts the persisted field='*' create mutation row's OperatedBy is the
// human. Before the fix the interceptor only stamped X-Operated-By for static
// tokens, the resolver's fleet case never saw the header, and this row carried
// fleet:slack-bridge — the symptom Matt reported. The complementary
// TestResolveAuditOperator_Fleet_HonorsHeader only covers the resolver in
// isolation (it injects the ctx value directly), so it could pass while the
// interceptor silently dropped the header.
func TestFleetToken_CreateBead_PersistsHumanOperatedBy(t *testing.T) {
	const adminToken = "admin-secret"
	fc := newBridgeFleetConfig()

	srv, ms, _ := newTestServer()
	srv.SetFleetAuth(fc)
	interceptor := AuthInterceptor(withTestToken(fc, adminToken), nil)

	const human = "oidc:matthew.baker@acme.example"
	md := metadata.Pairs(
		"authorization", "Bearer "+fleetTok,
		"x-operated-by", human,
	)
	ctx := metadata.NewIncomingContext(context.Background(), md)

	var beadID string
	handler := func(ctx context.Context, req any) (any, error) {
		// Precondition: the interceptor classified us as a fleet caller.
		if got := AuthIdentityFromContext(ctx).Type; got != "fleet" {
			t.Fatalf("auth type = %q; want fleet", got)
		}
		resp, err := srv.CreateBead(ctx, req.(*beadsv1.CreateBeadRequest))
		if err != nil {
			return nil, err
		}
		beadID = resp.Bead.Id
		return resp, nil
	}

	_, err := interceptor(ctx,
		&beadsv1.CreateBeadRequest{Title: "bridge-spawned thread task", Type: "task"},
		&grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"},
		handler,
	)
	if err != nil {
		t.Fatalf("CreateBead via fleet token: %v", err)
	}

	row := findMutation(ms, beadID, model.MutationTypeCreate, "*")
	if row == nil {
		t.Fatalf("missing field='*' create mutation row for %s", beadID)
	}
	if row.OperatedBy != human {
		t.Fatalf("persisted operated_by = %q; want %q "+
			"(fleet-token bridge spawn must attribute the human creator, not the fleet sentinel)",
			row.OperatedBy, human)
	}
}
