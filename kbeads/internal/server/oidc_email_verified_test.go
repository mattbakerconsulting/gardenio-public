package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
)

// gd-rnklckVFKS: the email claim is the subject kbeads keys an operator,
// spawn permissions, team membership and approver matching on, so a token
// that names an email its issuer does not mark verified must not
// authenticate. These run the real go-oidc verifier against signed tokens.

func newEmailVerifiedVerifier(t *testing.T, allowedDomains ...string) (*fakeKeycloak, TokenVerifier) {
	t.Helper()
	kc := newFakeKeycloak(t)
	v, err := NewOIDCVerifierWithDiscovery(context.Background(), testPublicIssuer, kc.discoveryURL(), testAudience, allowedDomains...)
	if err != nil {
		t.Fatal(err)
	}
	return kc, v
}

func TestOIDCVerifier_RequiresVerifiedEmail(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	ctx := context.Background()
	const victim = "owner@example.test"

	cases := []struct {
		name      string
		claims    map[string]any
		wantOK    bool
		wantEmail string
	}{
		{"verified bool", map[string]any{"email": victim, "email_verified": true}, true, victim},
		{"verified string", map[string]any{"email": victim, "email_verified": "true"}, true, victim},
		{"no email claim", map[string]any{"preferred_username": "someone"}, true, ""},
		{"unverified bool", map[string]any{"email": victim, "email_verified": false}, false, ""},
		{"unverified string", map[string]any{"email": victim, "email_verified": "false"}, false, ""},
		{"email_verified absent", map[string]any{"email": victim}, false, ""},
		{"email_verified null", map[string]any{"email": victim, "email_verified": nil}, false, ""},
		{"email_verified number", map[string]any{"email": victim, "email_verified": 1}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims := map[string]any{
				"azp":          "gardenio-cli",
				"groups":       []string{"/own-group"},
				"realm_access": map[string]any{"roles": []string{RoleViewer}},
			}
			for k, val := range tc.claims {
				claims[k] = val
			}
			tok := kc.mint(t, testPublicIssuer, testAudience, claims)

			verifyErr := v.Verify(ctx, tok)
			email, idErr := v.(IdentityVerifier).VerifyAndIdentify(ctx, tok)
			_, rolesErr := v.(RoleExtractor).ExtractRoles(ctx, tok)
			_, groupsErr := v.(GroupExtractor).ExtractGroups(ctx, tok)
			_, azpErr := v.(ClientExtractor).ExtractClientID(ctx, tok)
			errs := map[string]error{
				"Verify": verifyErr, "VerifyAndIdentify": idErr, "ExtractRoles": rolesErr,
				"ExtractGroups": groupsErr, "ExtractClientID": azpErr,
			}
			for method, err := range errs {
				if tc.wantOK && err != nil {
					t.Errorf("%s: unexpected error %v", method, err)
				}
				if !tc.wantOK && err == nil {
					t.Errorf("%s: accepted a token whose email is not verified", method)
				}
			}
			if email != tc.wantEmail {
				t.Errorf("VerifyAndIdentify email = %q, want %q", email, tc.wantEmail)
			}
		})
	}
}

// The allowed-domain check alone is not proof of ownership: an unverified
// email in an allowed domain is still refused.
func TestOIDCVerifier_AllowedDomainStillRequiresVerifiedEmail(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t, "example.test")
	ctx := context.Background()
	if err := v.Verify(ctx, kc.mint(t, testPublicIssuer, testAudience, map[string]any{"email": "a@example.test"})); err == nil {
		t.Fatal("unverified email in an allowed domain must be refused")
	}
	if err := v.Verify(ctx, kc.mint(t, testPublicIssuer, testAudience, map[string]any{"email": "a@example.test", "email_verified": true})); err != nil {
		t.Fatalf("verified email in an allowed domain: %v", err)
	}
	if err := v.Verify(ctx, kc.mint(t, testPublicIssuer, testAudience, map[string]any{"email": "a@other.test", "email_verified": true})); err == nil {
		t.Fatal("verified email outside the allowed domains must be refused")
	}
}

// Through the HTTP auth middleware an unverified-email token is a 401, and
// the ForwardAuth email header cannot stand in for the email the token was
// refused for, even when the token was minted for the edge client.
func TestAuthMiddleware_UnverifiedEmailRejected(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	const victim = "owner@example.test"
	mint := func(verified any) string {
		claims := map[string]any{
			"azp":          testEdgeClient,
			"email":        victim,
			"realm_access": map[string]any{"roles": []string{RoleViewer, RoleOperator}},
		}
		if verified != nil {
			claims["email_verified"] = verified
		}
		return kc.mint(t, testPublicIssuer, testAudience, claims)
	}
	serve := func(tok string) (int, AuthIdentity) {
		var seen AuthIdentity
		h := authMiddleware(testEdgeClients, withTestToken(nil, "primary"), v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			seen = AuthIdentityFromContext(r.Context())
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodGet, "/v1/beads", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("X-Auth-Request-Email", victim)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code, seen
	}

	for name, verified := range map[string]any{"absent": nil, "false": false} {
		t.Run(name, func(t *testing.T) {
			code, seen := serve(mint(verified))
			if code != http.StatusUnauthorized {
				t.Fatalf("status = %d (subject %q), want 401", code, seen.Subject)
			}
		})
	}
	t.Run("positive control: verified", func(t *testing.T) {
		code, seen := serve(mint(true))
		if code != http.StatusNoContent || seen.Subject != victim {
			t.Fatalf("status = %d subject = %q, want 204 %q", code, seen.Subject, victim)
		}
	})
}

func TestAuthInterceptor_UnverifiedEmailRejected(t *testing.T) {
	kc, v := newEmailVerifiedVerifier(t)
	call := func(verified bool) (string, error) {
		tok := kc.mint(t, testPublicIssuer, testAudience, map[string]any{
			"email":          "owner@example.test",
			"email_verified": verified,
			"realm_access":   map[string]any{"roles": []string{RoleViewer}},
		})
		var subject string
		handler := func(ctx context.Context, _ any) (any, error) {
			subject = AuthIdentityFromContext(ctx).Subject
			return "ok", nil
		}
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+tok))
		_, err := AuthInterceptor(withTestToken(nil, "secret"), v)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: "/beads.v1.BeadsService/ListBeads"}, handler)
		return subject, err
	}
	if subject, err := call(false); err == nil {
		t.Fatalf("unverified email authenticated as %q", subject)
	}
	if subject, err := call(true); err != nil || subject != "owner@example.test" {
		t.Fatalf("verified: subject = %q err = %v", subject, err)
	}
}
