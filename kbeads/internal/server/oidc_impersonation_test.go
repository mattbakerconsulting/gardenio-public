package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"gardenio/kbeads/internal/model"
)

// The impersonator claim (impersonation.go) is read only from a token the
// daemon verified. These run the real go-oidc verifier against signed tokens.

const testTenantEmail = "tenant@example.test"

func impersonationClaims(extra map[string]any) map[string]any {
	claims := map[string]any{
		"azp":            testEdgeClient,
		"email":          testTenantEmail,
		"email_verified": true,
		"sid":            "kc-sid-1",
		"realm_access":   map[string]any{"roles": []string{RoleViewer, RoleOperator}},
	}
	for k, v := range extra {
		claims[k] = v
	}
	return claims
}

func TestOIDCVerifier_ExtractImpersonation(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		claims  map[string]any
		wantImp string
		wantSID string
	}{
		{"impersonated session", map[string]any{"impersonator": "owner", "impersonator_id": "u-1"}, "owner", "kc-sid-1"},
		{"own session", nil, "", "kc-sid-1"},
		{"empty claim", map[string]any{"impersonator": ""}, "", "kc-sid-1"},
		{"non-string claim", map[string]any{"impersonator": map[string]any{"name": "owner"}}, impersonatorUnreadable, "kc-sid-1"},
		// A malformed sid must not drop the impersonator.
		{"non-string sid", map[string]any{"impersonator": "owner", "sid": 42}, "owner", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tok := kc.mint(t, testPublicIssuer, testAudience, impersonationClaims(tc.claims))
			imp, sid, err := v.(ImpersonationExtractor).ExtractImpersonation(ctx, tok)
			if err != nil {
				t.Fatalf("ExtractImpersonation: %v", err)
			}
			if imp != tc.wantImp || sid != tc.wantSID {
				t.Fatalf("ExtractImpersonation = (%q, %q), want (%q, %q)", imp, sid, tc.wantImp, tc.wantSID)
			}
		})
	}
	t.Run("a token from another issuer is not read", func(t *testing.T) {
		tok := kc.mint(t, "https://evil.example/realms/x", testAudience, impersonationClaims(map[string]any{"impersonator": "owner"}))
		if imp, _, err := v.(ImpersonationExtractor).ExtractImpersonation(ctx, tok); err == nil {
			t.Fatalf("read impersonator %q from an unverified token", imp)
		}
	})
}

// The holder and the client role ceiling wrap the real verifier in
// production; both must pass ExtractImpersonation through.
func TestOIDCVerifierWrappers_ExtractImpersonation(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	tok := kc.mint(t, testPublicIssuer, testAudience, impersonationClaims(map[string]any{"impersonator": "owner"}))

	holder := NewOIDCVerifierHolder()
	if _, _, err := holder.ExtractImpersonation(context.Background(), tok); err == nil {
		t.Fatal("an empty holder must not extract")
	}
	ceiling, err := NewClientRoleCeilingVerifier(v, map[string]string{DeviceGrantClientID: RoleViewer})
	if err != nil {
		t.Fatal(err)
	}
	holder.Set(ceiling)
	for name, tv := range map[string]TokenVerifier{"holder": holder, "ceiling": ceiling} {
		iv, ok := tv.(ImpersonationExtractor)
		if !ok {
			t.Fatalf("%s does not implement ImpersonationExtractor", name)
		}
		imp, sid, err := iv.ExtractImpersonation(context.Background(), tok)
		if err != nil || imp != "owner" || sid != "kc-sid-1" {
			t.Fatalf("%s: ExtractImpersonation = (%q, %q, %v), want (owner, kc-sid-1, nil)", name, imp, sid, err)
		}
	}
}

// The verified claim reaches the AuthIdentity on both transports; a header
// naming an impersonator reaches nothing.
func TestAuth_ImpersonationIdentity(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	serve := func(tok string, hdr map[string]string) (int, AuthIdentity, string) {
		var seen AuthIdentity
		var stamp string
		h := authMiddleware(testEdgeClients, withTestToken(nil, "primary"), v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = AuthIdentityFromContext(r.Context())
			stamp = impersonatedByFromContext(r.Context())
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		for k, val := range hdr {
			req.Header.Set(k, val)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, seen, stamp
	}

	code, seen, stamp := serve(kc.mint(t, testPublicIssuer, testAudience, impersonationClaims(map[string]any{"impersonator": "owner"})), nil)
	if code != http.StatusNoContent || seen.Subject != testTenantEmail || seen.ImpersonatedBy != "owner" || seen.SessionID != "kc-sid-1" || stamp != "owner" {
		t.Fatalf("impersonated: status %d identity %+v stamp %q", code, seen, stamp)
	}
	own := kc.mint(t, testPublicIssuer, testAudience, impersonationClaims(nil))
	code, seen, stamp = serve(own, map[string]string{"X-Impersonated-By": "owner", "X-Impersonator": "owner", "Impersonator": "owner"})
	if code != http.StatusNoContent || seen.ImpersonatedBy != "" || stamp != "" {
		t.Fatalf("own session with headers: status %d identity %+v stamp %q", code, seen, stamp)
	}

	// A fleet token is never an impersonated session, whatever it says.
	code, _, stamp = serve("primary", map[string]string{"X-Impersonated-By": "owner"})
	if code != http.StatusNoContent || stamp != "" {
		t.Fatalf("fleet token: status %d stamp %q", code, stamp)
	}

	var grpcSeen AuthIdentity
	handler := func(ctx context.Context, _ any) (any, error) {
		grpcSeen = AuthIdentityFromContext(ctx)
		return "ok", nil
	}
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+kc.mint(t, testPublicIssuer, testAudience, impersonationClaims(map[string]any{"impersonator": "owner"}))))
	if _, err := AuthInterceptor(nil, v)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler); err != nil {
		t.Fatal(err)
	}
	if grpcSeen.ImpersonatedBy != "owner" {
		t.Fatalf("gRPC identity = %+v, want impersonated by owner", grpcSeen)
	}
}

// impersonatedByPatchDenial also guards beads with no type config, where
// field validation does not fold letter case.
func TestImpersonatedByPatchDenial(t *testing.T) {
	imp := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "ada@example.com", ImpersonatedBy: "owner"}), authMethodOIDC)
	own := withAuthMethod(WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "ada@example.com"}), authMethodOIDC)
	server := auditRecorderContext("owner")
	stored := []byte(`{"impersonated_by":"owner","x":1}`)
	for _, c := range []struct {
		name     string
		ctx      context.Context
		patch    string
		existing []byte
		refused  bool
	}{
		{"no key", own, `{"x":2}`, nil, false},
		{"own session sets it", own, `{"impersonated_by":"owner"}`, nil, true},
		{"own session, upper case", own, `{"IMPERSONATED_BY":"owner"}`, nil, true},
		{"own session echoes the stored value", own, `{"impersonated_by":"owner"}`, stored, false},
		{"own session echoes it in another case", own, `{"Impersonated_by":"owner"}`, stored, true},
		{"own session changes it", own, `{"impersonated_by":"mallory"}`, stored, true},
		{"own session clears it", own, `{"impersonated_by":null}`, stored, true},
		{"impersonated session names itself", imp, `{"impersonated_by":"owner"}`, nil, false},
		{"impersonated session names another", imp, `{"impersonated_by":"mallory"}`, nil, true},
		{"the server's own record", server, `{"impersonated_by":"owner"}`, nil, false},
		{"update names its own admin as author", imp, `{"created_impersonated_by":"owner"}`, nil, true},
		{"own session, created stamp upper case", own, `{"CREATED_IMPERSONATED_BY":"owner"}`, nil, true},
		{"own session echoes the stored created stamp", own, `{"created_impersonated_by":"owner"}`, []byte(`{"created_impersonated_by":"owner"}`), false},
	} {
		d := impersonatedByPatchDenial(c.ctx, "update", model.BeadType("untyped-thing"), []byte(c.patch), c.existing)
		if (d != nil) != c.refused {
			t.Errorf("%s: denial = %v, want refused=%v", c.name, d, c.refused)
		}
	}
}
