package server

import (
	"context"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestRBACInterceptor_StaticTokenBypass asserts that static-token callers
// bypass RBAC below RoleAdmin (mirroring HTTP requireRole semantics) — the
// shared primary token every agent pod holds is routine machine traffic at
// RoleOperator and below.
func TestRBACInterceptor_StaticTokenBypass(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodStatic)
	resp, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"}, stubHandler)
	if err != nil {
		t.Fatalf("static token should bypass RBAC below RoleAdmin, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected 'ok', got %v", resp)
	}
}

// TestRBACInterceptor_StaticTokenDeniedAtAdminTier asserts that a static- or
// fleet-token caller is now DENIED at RoleAdmin, not bypassed (gd-ZjiSd9Kzio:
// the shared primary token was admin everywhere via this exact bypass).
func TestRBACInterceptor_StaticTokenDeniedAtAdminTier(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodStatic)
	_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/DeleteBead"}, stubHandler)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied for static token on admin RPC, got %v", err)
	}
}

// TestRBACInterceptor_NoAuthMethod asserts that contexts without an OIDC
// auth method (e.g. auth-disabled deployments) pass through.
func TestRBACInterceptor_NoAuthMethod(t *testing.T) {
	_, err := RBACInterceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"}, stubHandler)
	if err != nil {
		t.Fatalf("no auth method should bypass RBAC, got %v", err)
	}
}

// TestRBACInterceptor_AuthDisabledBypassesAdmin pins gd-ZjiSd9Kzio round 3
// item 2: authMethodNone (a daemon with no auth configured at all — no
// static token, no fleet tokens, no OIDC verifier) must bypass RBAC even at
// RoleAdmin RPCs — this is a dev/test mode where
// every route was always open, not a genuine credential to evaluate against
// min_role. Contrast with TestRBACInterceptor_StaticTokenDeniedAtAdminTier,
// which pins the opposite: a REAL static token against a daemon that DOES
// have auth configured is denied at RoleAdmin.
func TestRBACInterceptor_AuthDisabledBypassesAdmin(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodNone)
	_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/DeleteBead"}, stubHandler)
	if err != nil {
		t.Fatalf("auth-disabled daemon should bypass RBAC on admin RPC, got %v", err)
	}
}

// TestRBACInterceptor_OIDCInsufficientRole asserts an OIDC caller lacking the
// required role is rejected with PermissionDenied.
func TestRBACInterceptor_OIDCInsufficientRole(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleViewer})
	_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/DeleteBead"}, stubHandler)
	if err == nil {
		t.Fatal("expected PermissionDenied, got nil")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("expected PermissionDenied, got %v", status.Code(err))
	}
}

// TestRBACInterceptor_OIDCSufficientRole asserts an OIDC caller with a
// sufficient role is admitted.
func TestRBACInterceptor_OIDCSufficientRole(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleOperator})
	_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/CreateBead"}, stubHandler)
	if err != nil {
		t.Fatalf("operator should pass CreateBead, got %v", err)
	}
}

// TestRBACInterceptor_UnknownRPCFailsClosed asserts that RPCs not in the
// role map default to requiring admin.
func TestRBACInterceptor_UnknownRPCFailsClosed(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleOperator})
	_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/FutureRPC"}, stubHandler)
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("unknown RPC should default to admin-only, got %v", err)
	}
}

// TestRBACInterceptor_HealthExempt asserts the Health RPC has no role.
func TestRBACInterceptor_HealthExempt(t *testing.T) {
	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, nil)
	_, err := RBACInterceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/Health"}, stubHandler)
	if err != nil {
		t.Fatalf("Health should be exempt, got %v", err)
	}
}

// TestAuthInterceptor_OIDC_RoleExtractor asserts roles and auth method are
// injected into the gRPC context, matching AuthMiddleware behavior.
func TestAuthInterceptor_OIDC_RoleExtractor(t *testing.T) {
	verifier := &stubRoleExtractor{roles: []string{RoleOperator}}
	var capturedRoles []string
	var capturedMethod authMethod
	handler := func(ctx context.Context, _ any) (any, error) {
		capturedRoles = RolesFromContext(ctx)
		capturedMethod = authMethodFromContext(ctx)
		return "ok", nil
	}
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), verifier)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer oidc-jwt"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(capturedRoles) != 1 || capturedRoles[0] != RoleOperator {
		t.Fatalf("expected roles [%s], got %v", RoleOperator, capturedRoles)
	}
	if capturedMethod != authMethodOIDC {
		t.Fatalf("expected auth method %q, got %q", authMethodOIDC, capturedMethod)
	}
}

// TestAuthInterceptor_StaticTokenSetsStaticMethod asserts static-token auth
// does not set OIDC auth method, so RBAC bypass semantics apply. (Currently
// static token leaves authMethod unset; either unset or authMethodStatic is
// acceptable — both bypass RBAC.)
func TestAuthInterceptor_StaticTokenBypassesRBAC(t *testing.T) {
	var capturedCtx context.Context
	handler := func(ctx context.Context, _ any) (any, error) {
		capturedCtx = ctx
		return "ok", nil
	}
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), nil)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/DeleteBead"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if authMethodFromContext(capturedCtx) == authMethodOIDC {
		t.Fatalf("static token must not carry OIDC auth method")
	}
}

// TestNewGRPCServer_NoReflectionByDefault asserts reflection is NOT registered
// unless explicitly enabled (§C2 fix — reflection leaks schema).
func TestNewGRPCServer_NoReflectionByDefault(t *testing.T) {
	beadsServer := &BeadsServer{}
	srv := NewGRPCServer(beadsServer, nil, nil)
	// Reflection registers the "grpc.reflection.v1.ServerReflection" and v1alpha
	// services. Absence confirms the guard.
	info := srv.GetServiceInfo()
	for name := range info {
		if name == "grpc.reflection.v1.ServerReflection" || name == "grpc.reflection.v1alpha.ServerReflection" {
			t.Fatalf("reflection service %q registered by default — must be opt-in", name)
		}
	}
}

// TestNewGRPCServer_ReflectionEnabled asserts reflection registers when opted in.
func TestNewGRPCServer_ReflectionEnabled(t *testing.T) {
	beadsServer := &BeadsServer{}
	srv := NewGRPCServer(beadsServer, nil, nil, GRPCServerOptions{EnableReflection: true})
	info := srv.GetServiceInfo()
	found := false
	for name := range info {
		if name == "grpc.reflection.v1.ServerReflection" || name == "grpc.reflection.v1alpha.ServerReflection" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("reflection service not registered when EnableReflection=true")
	}
}
