package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// stubHandler is a no-op gRPC handler used in interceptor tests.
func stubHandler(_ context.Context, _ any) (any, error) {
	return "ok", nil
}

func TestAuthInterceptor_Disabled(t *testing.T) {
	interceptor := AuthInterceptor(nil, nil)
	resp, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected 'ok', got %v", resp)
	}
}

func TestAuthInterceptor_HealthExempt(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), nil)
	// No metadata — should still pass for Health.
	resp, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/Health"}, stubHandler)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected 'ok', got %v", resp)
	}
}

func TestAuthInterceptor_MissingMetadata(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), nil)
	_, err := interceptor(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_MissingAuthHeader(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), nil)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("other", "value"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_WrongToken(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), nil)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer wrong"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_InvalidScheme(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), nil)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Basic secret"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_CorrectToken(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), nil)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret"))
	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected 'ok', got %v", resp)
	}
}

func TestAuthInterceptor_FleetResolution(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{"tok-qa": "qa-fleet"})

	t.Run("PrimaryToken_DefaultFleet", func(t *testing.T) {
		var capturedFleet string
		handler := func(ctx context.Context, _ any) (any, error) {
			capturedFleet = FleetFromContext(ctx)
			return "ok", nil
		}
		interceptor := AuthInterceptor(withTestToken(fc, "secret"), nil)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer secret"))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedFleet != "gardenio" {
			t.Fatalf("expected fleet 'gardenio', got %q", capturedFleet)
		}
	})

	t.Run("FleetMapToken", func(t *testing.T) {
		var capturedFleet string
		handler := func(ctx context.Context, _ any) (any, error) {
			capturedFleet = FleetFromContext(ctx)
			return "ok", nil
		}
		interceptor := AuthInterceptor(withTestToken(fc, "secret"), nil)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer tok-qa"))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if capturedFleet != "qa-fleet" {
			t.Fatalf("expected fleet 'qa-fleet', got %q", capturedFleet)
		}
	})

	t.Run("InvalidToken", func(t *testing.T) {
		interceptor := AuthInterceptor(withTestToken(fc, "secret"), nil)
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer unknown"))
		_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
		if err == nil {
			t.Fatal("expected error")
		}
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
		}
	})
}

// --- AuthMiddleware tests ---

func TestAuthMiddleware_NoHeader(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_WrongToken(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_InvalidScheme(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Basic secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_CorrectToken(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_HealthExempt(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_Disabled(t *testing.T) {
	handler := AuthMiddleware(nil, nil, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_FleetResolution(t *testing.T) {
	fc := NewFleetConfig("gardenio", map[string]string{"tok-qa": "qa-fleet"})

	t.Run("PrimaryToken_DefaultFleet", func(t *testing.T) {
		var capturedFleet string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedFleet = FleetFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if capturedFleet != "gardenio" {
			t.Fatalf("expected fleet 'gardenio', got %q", capturedFleet)
		}
	})

	t.Run("FleetMapToken", func(t *testing.T) {
		var capturedFleet string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedFleet = FleetFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer tok-qa")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if capturedFleet != "qa-fleet" {
			t.Fatalf("expected fleet 'qa-fleet', got %q", capturedFleet)
		}
	})

	t.Run("XFleet_Ignored", func(t *testing.T) {
		var capturedFleet string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedFleet = FleetFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("X-Fleet", "qa-fleet")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if capturedFleet != "gardenio" {
			t.Fatalf("X-Fleet must be ignored (the fleet comes from the credential); got %q", capturedFleet)
		}
	})

	t.Run("XFleet_NonAdmin_Ignored", func(t *testing.T) {
		var capturedFleet string
		handler := AuthMiddleware(withTestToken(fc, "secret"), nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedFleet = FleetFromContext(r.Context())
			w.WriteHeader(http.StatusOK)
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer tok-qa")
		req.Header.Set("X-Fleet", "gardenio")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		// Non-admin token: X-Fleet should be ignored, fleet stays as resolved from token.
		if capturedFleet != "qa-fleet" {
			t.Fatalf("expected fleet 'qa-fleet' (X-Fleet ignored), got %q", capturedFleet)
		}
	})
}

// --- RBAC tests ---

// TestRPCMinRole_CoversEveryDescriptorMethod walks the actual gRPC service
// descriptor (not a hand-maintained list) and fails if any BeadsService RPC
// lacks an explicit rpcMinRole entry. rpcMinRole's own doc comment says
// missing entries fail closed to RoleAdmin, which sounds safe but silently
// denies a static/fleet-token machine caller that was supposed to be at
// RoleViewer or RoleOperator — exactly what happened to
// GetChildrenStatusHistogram, ListMentionedBy, ListMentions, MyWorkSummary,
// and Whoami in the gd-ZjiSd9Kzio round-3 merge-desk review: the reviewer had
// to notice the gap by hand. This test makes that gap a build failure instead
// of a review finding.
func TestRPCMinRole_CoversEveryDescriptorMethod(t *testing.T) {
	for _, m := range beadsv1.BeadsService_ServiceDesc.Methods {
		full := "/" + beadsv1.BeadsService_ServiceDesc.ServiceName + "/" + m.MethodName
		if _, known := rpcMinRole[full]; !known {
			t.Errorf("rpcMinRole is missing an entry for %s — it will default to RoleAdmin, "+
				"silently denying any static/fleet-token caller of this RPC (gd-ZjiSd9Kzio); "+
				"add an explicit entry (even RoleAdmin) so the omission is a deliberate choice",
				full)
		}
	}
}

// TestRPCMinRole_NoStaleEntries is the converse of the descriptor walk above:
// every key in rpcMinRole must name a real RPC on the current descriptor, so
// a renamed or removed RPC can't leave a dead entry that masks a fresh
// fail-closed gap under the old name.
func TestRPCMinRole_NoStaleEntries(t *testing.T) {
	known := make(map[string]bool, len(beadsv1.BeadsService_ServiceDesc.Methods))
	for _, m := range beadsv1.BeadsService_ServiceDesc.Methods {
		known["/"+beadsv1.BeadsService_ServiceDesc.ServiceName+"/"+m.MethodName] = true
	}
	for full := range rpcMinRole {
		if !known[full] {
			t.Errorf("rpcMinRole has a stale entry for %s — no such RPC on BeadsService_ServiceDesc", full)
		}
	}
}

func TestHasMinimumRole(t *testing.T) {
	tests := []struct {
		name     string
		roles    []string
		required string
		want     bool
	}{
		{"admin has admin", []string{RoleAdmin}, RoleAdmin, true},
		{"admin has operator", []string{RoleAdmin}, RoleOperator, true},
		{"admin has viewer", []string{RoleAdmin}, RoleViewer, true},
		{"operator has operator", []string{RoleOperator}, RoleOperator, true},
		{"operator has viewer", []string{RoleOperator}, RoleViewer, true},
		{"operator lacks admin", []string{RoleOperator}, RoleAdmin, false},
		{"viewer has viewer", []string{RoleViewer}, RoleViewer, true},
		{"viewer lacks operator", []string{RoleViewer}, RoleOperator, false},
		{"viewer lacks admin", []string{RoleViewer}, RoleAdmin, false},
		{"no roles", []string{}, RoleViewer, false},
		{"nil roles", nil, RoleViewer, false},
		{"unknown required", []string{RoleAdmin}, "gardenio-superadmin", false},
		{"unrelated role", []string{"other-role"}, RoleViewer, false},
		{"mixed uses highest", []string{"other", RoleOperator}, RoleOperator, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasMinimumRole(tt.roles, tt.required)
			if got != tt.want {
				t.Errorf("hasMinimumRole(%v, %q) = %v, want %v", tt.roles, tt.required, got, tt.want)
			}
		})
	}
}

func TestRequireRole_StaticAuthBypassBelowAdmin(t *testing.T) {
	// Static token auth (non-OIDC) still bypasses RBAC below RoleAdmin —
	// routine machine traffic (bead create/update, comments, deps) depends
	// on this.
	handler := requireRole(RoleOperator, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// No auth method in context — behaves like static/internal caller.
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for static auth below RoleAdmin, got %d", rec.Code)
	}
}

func TestRequireRole_StaticAuthDeniedAtAdmin(t *testing.T) {
	// gd-ZjiSd9Kzio: static/fleet token auth is no longer bypassed at
	// RoleAdmin — the shared primary token was admin everywhere via this
	// exact bypass.
	handler := requireRole(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// No auth method in context — behaves like static/internal caller.
	req := httptest.NewRequest(http.MethodGet, "/v1/configs/test", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for static auth on admin route, got %d", rec.Code)
	}
}

func TestRequireRole_StaticMethodExplicitDeniedAtAdmin(t *testing.T) {
	handler := requireRole(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	req := httptest.NewRequest(http.MethodGet, "/v1/configs/test", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for explicit static auth on admin route, got %d", rec.Code)
	}
}

// TestRequireRole_AuthDisabledBypassesAdmin pins gd-ZjiSd9Kzio round 3 item
// 2: a daemon with NO auth configured at all (no static token, no fleet
// tokens, no OIDC verifier — authMethodNone, stamped by AuthMiddleware's
// auth-disabled branch) must still bypass RoleAdmin routes, exactly as every
// route was open before gd-ZjiSd9Kzio. Distinguishes this dev/test mode from
// a genuine static/fleet token presented against a daemon that DOES have
// auth configured (TestRequireRole_StaticAuthDeniedAtAdmin /
// TestRequireRole_StaticMethodExplicitDeniedAtAdmin), which is the real
// admin-everywhere bypass this task closes.
func TestRequireRole_AuthDisabledBypassesAdmin(t *testing.T) {
	handler := requireRole(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodNone)
	req := httptest.NewRequest(http.MethodGet, "/v1/configs/test", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for auth-disabled daemon on admin route, got %d", rec.Code)
	}
}

func TestRequireRoleMachineAllowed_StaticAuthBypassesAtAdmin(t *testing.T) {
	// requireRoleMachineAllowed is the narrow, enumerated exception for
	// routes with a confirmed genuine non-OIDC machine caller (bridge/fleet
	// tokens, operator alias sync) — unlike requireRole, it keeps the
	// pre-gd-ZjiSd9Kzio bypass at RoleAdmin too.
	handler := requireRoleMachineAllowed(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	req := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for static auth on machine-allowed admin route, got %d", rec.Code)
	}
}

func TestRequireRoleMachineAllowed_OIDCStillRoleChecked(t *testing.T) {
	handler := requireRoleMachineAllowed(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleOperator})
	req := httptest.NewRequest(http.MethodPost, "/v1/bridge-tokens", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for insufficient OIDC role on machine-allowed route, got %d", rec.Code)
	}
}

func TestRequireRoleMachineAllowed_ConfigSetBypassesAtAdmin(t *testing.T) {
	// PUT /v1/configs/{key} (SetConfig) is machine-allowed for the
	// beads-viewer e2e seed script, a genuine static-token caller
	// (gd-ZjiSd9Kzio round-4 re-inventory) — see the doc comment above
	// requireRoleMachineAllowed.
	handler := requireRoleMachineAllowed(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	req := httptest.NewRequest(http.MethodPut, "/v1/configs/type:widget-instance", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for static auth on machine-allowed PUT /v1/configs, got %d", rec.Code)
	}
}

func TestRequireRoleMachineAllowed_ConfigSetOIDCStillRoleChecked(t *testing.T) {
	handler := requireRoleMachineAllowed(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleOperator})
	req := httptest.NewRequest(http.MethodPut, "/v1/configs/type:widget-instance", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for insufficient OIDC role on PUT /v1/configs, got %d", rec.Code)
	}
}

func TestRequireRole_DeleteConfigStillDeniedAtAdminForStatic(t *testing.T) {
	// DELETE /v1/configs/{key} has no confirmed non-OIDC caller and stays on
	// requireRole (deny static at RoleAdmin) — only SetConfig moved to the
	// machine-allowed path.
	handler := requireRole(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodStatic)
	req := httptest.NewRequest(http.MethodDelete, "/v1/configs/type:widget-instance", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for static auth on DELETE /v1/configs, got %d", rec.Code)
	}
}

func TestRequireRole_OIDCAllowsSufficientRole(t *testing.T) {
	handler := requireRole(RoleOperator, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleAdmin})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when admin accesses operator route, got %d", rec.Code)
	}
}

func TestRequireRole_OIDCDeniesInsufficientRole(t *testing.T) {
	handler := requireRole(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleViewer})
	req := httptest.NewRequest(http.MethodDelete, "/v1/beads/123", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when viewer accesses admin route, got %d", rec.Code)
	}
}

func TestRequireRole_OIDCExactRole(t *testing.T) {
	handler := requireRole(RoleViewer, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	ctx := withAuthMethod(context.Background(), authMethodOIDC)
	ctx = WithRoles(ctx, []string{RoleViewer})
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 when viewer accesses viewer route, got %d", rec.Code)
	}
}

// --- OIDC Token Verifier tests ---

// stubVerifier implements TokenVerifier for testing.
type stubVerifier struct {
	err error
}

func (s *stubVerifier) Verify(_ context.Context, _ string) error {
	return s.err
}

// stubRoleExtractor implements RoleExtractor for testing RBAC integration.
type stubRoleExtractor struct {
	roles []string
	err   error
}

func (s *stubRoleExtractor) Verify(_ context.Context, _ string) error {
	return s.err
}

func (s *stubRoleExtractor) ExtractRoles(_ context.Context, _ string) ([]string, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.roles, nil
}

func TestAuthMiddleware_OIDC_ValidToken(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), &stubVerifier{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_OIDC_InvalidToken(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), &stubVerifier{err: fmt.Errorf("invalid JWT")}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer bad-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_OIDC_StaticTokenTakesPrecedence(t *testing.T) {
	// When the static token matches, OIDC should not be consulted.
	handler := AuthMiddleware(withTestToken(nil, "secret"), &stubVerifier{err: fmt.Errorf("should not be called")}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 (static token), got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_OIDC_ForwardAuthHeader(t *testing.T) {
	handler := AuthMiddleware(withTestToken(nil, "secret"), &stubVerifier{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("X-Auth-Request-Access-Token", "forwarded-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 via ForwardAuth header, got %d; body: %s", rec.Code, rec.Body.String())
	}
}

func TestAuthMiddleware_OIDC_DefaultFleet(t *testing.T) {
	fc := NewFleetConfig("myfleet", nil)
	var capturedFleet string
	handler := AuthMiddleware(withTestToken(fc, "secret"), &stubVerifier{}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedFleet = FleetFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if capturedFleet != "myfleet" {
		t.Fatalf("expected fleet 'myfleet' (default), got %q", capturedFleet)
	}
}

func TestAuthMiddleware_OIDC_OnlyMode(t *testing.T) {
	// No static token, no fleet tokens — OIDC-only auth mode.
	handler := AuthMiddleware(nil, &stubVerifier{}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// Valid OIDC token should pass.
	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 in OIDC-only mode, got %d; body: %s", rec.Code, rec.Body.String())
	}

	// Missing token should fail.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", rec2.Code)
	}
}

// --- OIDC AuthInterceptor tests ---

func TestAuthInterceptor_OIDC_ValidToken(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), &stubVerifier{})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer oidc-jwt"))
	resp, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if resp != "ok" {
		t.Fatalf("expected 'ok', got %v", resp)
	}
}

func TestAuthInterceptor_OIDC_InvalidToken(t *testing.T) {
	interceptor := AuthInterceptor(withTestToken(nil, "secret"), &stubVerifier{err: fmt.Errorf("bad jwt")})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer bad-jwt"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, stubHandler)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("expected Unauthenticated, got %v", status.Code(err))
	}
}

func TestAuthInterceptor_OIDC_DefaultFleet(t *testing.T) {
	fc := NewFleetConfig("myfleet", nil)
	var capturedFleet string
	handler := func(ctx context.Context, _ any) (any, error) {
		capturedFleet = FleetFromContext(ctx)
		return "ok", nil
	}
	interceptor := AuthInterceptor(withTestToken(fc, "secret"), &stubVerifier{})
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer oidc-jwt"))
	_, err := interceptor(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if capturedFleet != "myfleet" {
		t.Fatalf("expected fleet 'myfleet' (default), got %q", capturedFleet)
	}
}

// --- RoleExtractor integration tests ---

func TestAuthMiddleware_RoleExtractor_InjectsRoles(t *testing.T) {
	verifier := &stubRoleExtractor{roles: []string{RoleOperator}}
	var capturedRoles []string
	var capturedMethod authMethod
	handler := AuthMiddleware(withTestToken(nil, "secret"), verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedRoles = RolesFromContext(r.Context())
		capturedMethod = authMethodFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if len(capturedRoles) != 1 || capturedRoles[0] != RoleOperator {
		t.Fatalf("expected roles [%s], got %v", RoleOperator, capturedRoles)
	}
	if capturedMethod != authMethodOIDC {
		t.Fatalf("expected auth method %q, got %q", authMethodOIDC, capturedMethod)
	}
}

func TestAuthMiddleware_RoleExtractor_StaticTokenBypass(t *testing.T) {
	// Static token match should not go through RoleExtractor.
	verifier := &stubRoleExtractor{roles: []string{RoleViewer}}
	var capturedMethod authMethod
	handler := AuthMiddleware(withTestToken(nil, "secret"), verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = authMethodFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if capturedMethod != authMethodStatic {
		t.Fatalf("expected auth method %q for static token, got %q", authMethodStatic, capturedMethod)
	}
}

func TestAuthMiddleware_RoleExtractor_WithFleet(t *testing.T) {
	fc := NewFleetConfig("myfleet", nil)
	verifier := &stubRoleExtractor{roles: []string{RoleAdmin}}
	var capturedFleet string
	handler := AuthMiddleware(withTestToken(fc, "secret"), verifier, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedFleet = FleetFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer oidc-jwt")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if capturedFleet != "myfleet" {
		t.Fatalf("expected fleet 'myfleet', got %q", capturedFleet)
	}
}

func TestAuthMiddleware_RBAC_EndToEnd(t *testing.T) {
	// Full integration: RoleExtractor → AuthMiddleware → requireRole enforcement.
	verifier := &stubRoleExtractor{roles: []string{RoleViewer}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/beads", requireRole(RoleViewer, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux.HandleFunc("DELETE /v1/beads/{id}", requireRole(RoleAdmin, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	handler := AuthMiddleware(withTestToken(nil, "secret"), verifier, mux)

	t.Run("ViewerCanRead", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer oidc-jwt")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for viewer on GET /v1/beads, got %d", rec.Code)
		}
	})

	t.Run("ViewerCannotDelete", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodDelete, "/v1/beads/123", nil)
		req.Header.Set("Authorization", "Bearer oidc-jwt")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for viewer on DELETE /v1/beads, got %d", rec.Code)
		}
	})

	t.Run("StaticTokenDeniedAtAdminRoute", func(t *testing.T) {
		// gd-ZjiSd9Kzio: the shared primary token no longer bypasses RBAC at
		// RoleAdmin — it was admin everywhere via this exact bypass.
		req := httptest.NewRequest(http.MethodDelete, "/v1/beads/123", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for static token on DELETE /v1/beads, got %d", rec.Code)
		}
	})

	t.Run("StaticTokenBypassesRBACBelowAdmin", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for static token on GET /v1/beads (RoleViewer route), got %d", rec.Code)
		}
	})
}
