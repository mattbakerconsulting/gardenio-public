package oidcauth

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// gd-ddpTGbl4Te (follow-up to kbeads gd-rnklckVFKS): the email the middleware
// injects is the identity every controller surface keys on, so a token naming
// an email its issuer does not vouch for must not authenticate. These run the
// real go-oidc verifier against signed tokens from a mock issuer. Every
// refusal sits next to a positive control that still passes.

const testEntraTenant = "49e4de0b-c774-439a-b26e-4aaa1c944113"

// emailVerifiedClaims builds signed-token claims for the fixture issuer from a
// base set plus extra claims; a nil extra value leaves that claim out.
func (f *oidcFixture) emailVerifiedClaims(t *testing.T, extra map[string]any) string {
	t.Helper()
	claims := map[string]any{
		"iss": f.issuer,
		"aud": "test-client",
		"sub": "user-1",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		if v != nil {
			claims[k] = v
		}
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return f.signToken(t, string(raw))
}

// serveOIDC runs one request through a Handle for cfg and reports the status
// and the email the inner handler saw ("" when it was not reached).
func serveOIDC(t *testing.T, h *Handle, setup func(*http.Request)) (int, string) {
	t.Helper()
	var email string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		email = EmailFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	setup(req)
	rec := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec, req)
	return rec.Code, email
}

func bearer(tok string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

// A Keycloak/GitLab-style issuer (no pinned tenant, not a Microsoft host) must
// assert email_verified=true for any email it puts in the token.
func TestVerifyingMiddleware_StrictIssuerRequiresVerifiedEmail(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client"}, slog.Default())
	waitReady(t, h)
	const victim = "owner@acme.example"

	cases := []struct {
		name      string
		claims    map[string]any
		wantCode  int
		wantEmail string
	}{
		{"positive control: verified bool", map[string]any{"email": victim, "email_verified": true}, http.StatusOK, victim},
		{"positive control: verified string", map[string]any{"email": victim, "email_verified": "true"}, http.StatusOK, victim},
		{"positive control: service account username", map[string]any{"preferred_username": "service-account-gardenio-controller"}, http.StatusOK, "service-account-gardenio-controller"},
		{"unverified bool", map[string]any{"email": victim, "email_verified": false}, http.StatusUnauthorized, ""},
		{"unverified string", map[string]any{"email": victim, "email_verified": "false"}, http.StatusUnauthorized, ""},
		{"email_verified absent", map[string]any{"email": victim}, http.StatusUnauthorized, ""},
		{"email_verified null", map[string]any{"email": victim, "email_verified": json.RawMessage("null")}, http.StatusUnauthorized, ""},
		{"email_verified number", map[string]any{"email": victim, "email_verified": 1}, http.StatusUnauthorized, ""},
		{"email-shaped username, no email claim", map[string]any{"preferred_username": victim}, http.StatusUnauthorized, ""},
		{"email-shaped username, verified but no email claim", map[string]any{"preferred_username": victim, "email_verified": true}, http.StatusUnauthorized, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, email := serveOIDC(t, h, bearer(f.emailVerifiedClaims(t, tc.claims)))
			if code != tc.wantCode || email != tc.wantEmail {
				t.Fatalf("status %d email %q, want %d %q", code, email, tc.wantCode, tc.wantEmail)
			}
		})
	}
}

// The allowed-domain check is not proof of ownership: an unverified email in
// an allowed domain is still refused.
func TestVerifyingMiddleware_AllowedDomainStillRequiresVerifiedEmail(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client", AllowedEmailDomains: []string{"acme.example"}}, slog.Default())
	waitReady(t, h)

	if code, email := serveOIDC(t, h, bearer(f.emailVerifiedClaims(t, map[string]any{"email": "a@acme.example"}))); code != http.StatusUnauthorized {
		t.Fatalf("unverified email in an allowed domain: status %d email %q, want 401", code, email)
	}
	if code, email := serveOIDC(t, h, bearer(f.emailVerifiedClaims(t, map[string]any{"email": "a@acme.example", "email_verified": true}))); code != http.StatusOK || email != "a@acme.example" {
		t.Fatalf("positive control: verified email in an allowed domain: status %d email %q", code, email)
	}
	if code, _ := serveOIDC(t, h, bearer(f.emailVerifiedClaims(t, map[string]any{"email": "a@other.example", "email_verified": true}))); code != http.StatusForbidden {
		t.Fatalf("verified email outside the allowed domains: status %d, want 403", code)
	}
}

// A token handed over by ForwardAuth (X-Auth-Request-Access-Token) is refused
// the same way, and the email header beside it does not reach the context.
func TestVerifyingMiddleware_ForwardAuthUnverifiedEmailRejected(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client"}, slog.Default())
	waitReady(t, h)
	const victim = "owner@acme.example"
	forwardAuth := func(tok string) func(*http.Request) {
		return func(r *http.Request) {
			r.Header.Set("X-Auth-Request-Access-Token", tok)
			r.Header.Set("X-Auth-Request-Email", victim)
		}
	}

	for name, verified := range map[string]any{"absent": nil, "false": false} {
		t.Run(name, func(t *testing.T) {
			code, email := serveOIDC(t, h, forwardAuth(f.emailVerifiedClaims(t, map[string]any{"email": victim, "email_verified": verified})))
			if code != http.StatusUnauthorized || email != "" {
				t.Fatalf("status %d email %q, want 401 and no identity", code, email)
			}
		})
	}
	t.Run("positive control: verified", func(t *testing.T) {
		code, email := serveOIDC(t, h, forwardAuth(f.emailVerifiedClaims(t, map[string]any{"email": victim, "email_verified": true})))
		if code != http.StatusOK || email != victim {
			t.Fatalf("status %d email %q, want 200 %q", code, email, victim)
		}
	})
}

// Entra (a pinned tenant) never sends email_verified, so an Entra token still
// passes without it. An Entra token that does send it as not-true is refused.
func TestVerifyingMiddleware_EntraWithoutEmailVerified(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{
		IssuerURL:        f.issuer,
		ClientID:         "test-client",
		ExpectedTenantID: testEntraTenant,
		RequiredRole:     RoleViewer,
	}, slog.Default())
	waitReady(t, h)
	const user = "operator@acme.example"
	entra := func(extra map[string]any) string {
		claims := map[string]any{"tid": testEntraTenant, "roles": []string{RoleOperator}}
		for k, v := range extra {
			claims[k] = v
		}
		return f.emailVerifiedClaims(t, claims)
	}

	cases := []struct {
		name      string
		claims    map[string]any
		wantCode  int
		wantEmail string
	}{
		{"positive control: email, no email_verified", map[string]any{"email": user}, http.StatusOK, user},
		{"positive control: preferred_username only", map[string]any{"preferred_username": user}, http.StatusOK, user},
		{"positive control: email_verified true", map[string]any{"email": user, "email_verified": true}, http.StatusOK, user},
		{"email_verified false", map[string]any{"email": user, "email_verified": false}, http.StatusUnauthorized, ""},
		{"email_verified null", map[string]any{"email": user, "email_verified": json.RawMessage("null")}, http.StatusUnauthorized, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, email := serveOIDC(t, h, bearer(entra(tc.claims)))
			if code != tc.wantCode || email != tc.wantEmail {
				t.Fatalf("status %d email %q, want %d %q", code, email, tc.wantCode, tc.wantEmail)
			}
		})
	}
}

func TestRequiresEmailVerifiedClaim(t *testing.T) {
	cases := []struct {
		cfg  Config
		want bool
	}{
		{Config{IssuerURL: "https://auth.gardenio.example/realms/gardenio"}, true},
		{Config{IssuerURL: "https://gitlab.com"}, true},
		{Config{IssuerURL: "https://login.microsoftonline.com/" + testEntraTenant + "/v2.0"}, false},
		{Config{IssuerURL: "https://sts.windows.net/" + testEntraTenant + "/"}, false},
		{Config{IssuerURL: "https://LOGIN.microsoftonline.com:443/" + testEntraTenant + "/v2.0"}, false},
		{Config{IssuerURL: "https://login.microsoftonline.us/" + testEntraTenant + "/v2.0"}, false},
		{Config{IssuerURL: "https://login.microsoftonline.com.evil.example/" + testEntraTenant + "/v2.0"}, true},
		{Config{IssuerURL: "https://evil.example/login.microsoftonline.com/v2.0"}, true},
		{Config{IssuerURL: "https://auth.gardenio.example/realms/gardenio", ExpectedTenantID: testEntraTenant}, false},
		{Config{IssuerURL: "://not a url"}, true},
	}
	for _, tc := range cases {
		name := fmt.Sprintf("%s tid=%q", tc.cfg.IssuerURL, tc.cfg.ExpectedTenantID)
		if got := requiresEmailVerifiedClaim(tc.cfg); got != tc.want {
			t.Errorf("%s: requiresEmailVerifiedClaim = %v, want %v", name, got, tc.want)
		}
	}
}

// AVA forwards whatever the trust provider returned. An email_verified that is
// present and not true (top-level or nested) is refused; one that is absent is
// not, since AVA is not known to always forward it.
func TestAVAVerify_EmailVerified(t *testing.T) {
	f := newAVAFixture(t)
	v := f.verifier()
	cases := []struct {
		name    string
		payload string
		wantOK  bool
	}{
		{"positive control: absent", `{"iss":"gitlab","sub":"u-1","email":"u@acme.example"}`, true},
		{"positive control: true", `{"iss":"gitlab","sub":"u-1","email":"u@acme.example","email_verified":true}`, true},
		{"positive control: nested true", `{"iss":"gitlab","sub":"u-1","additional_user_context":{"email":"u@acme.example","email_verified":true}}`, true},
		{"false", `{"iss":"gitlab","sub":"u-1","email":"u@acme.example","email_verified":false}`, false},
		{"null", `{"iss":"gitlab","sub":"u-1","email":"u@acme.example","email_verified":null}`, false},
		{"nested false", `{"iss":"gitlab","sub":"u-1","additional_user_context":{"email":"u@acme.example","email_verified":false}}`, false},
		{"nested true, top-level false", `{"iss":"gitlab","sub":"u-1","email":"u@acme.example","email_verified":false,"additional_user_context":{"email":"u@acme.example","email_verified":true}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := v.verify(context.Background(), f.signAVA(t, nil, tc.payload))
			if tc.wantOK && (err != nil || claims.effectiveEmail() != "u@acme.example") {
				t.Fatalf("verify: claims %v err %v, want u@acme.example", claims, err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("accepted an AVA token whose email is not verified (email %q)", claims.effectiveEmail())
			}
		})
	}
}

// Through the Handle, an AVA header with an unverified email is a 401 and does
// not fall through to the Keycloak path.
func TestHandle_AVAUnverifiedEmailRejected(t *testing.T) {
	f := newAVAFixture(t)
	h := New(context.Background(), Config{AVA: AVAConfig{
		TrustSigner:    testTrustSigner,
		Region:         "test-region",
		publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
		httpClient:     f.jwksSrv.Client(),
	}}, slog.Default())
	ava := func(payload string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set(AVAHeaderName, f.signAVA(t, nil, payload)) }
	}

	if code, email := serveOIDC(t, h, ava(`{"iss":"gitlab","sub":"u-1","email":"owner@acme.example","email_verified":false}`)); code != http.StatusUnauthorized || email != "" {
		t.Fatalf("unverified AVA email: status %d email %q, want 401 and no identity", code, email)
	}
	if code, email := serveOIDC(t, h, ava(`{"iss":"gitlab","sub":"u-1","email":"owner@acme.example","email_verified":true}`)); code != http.StatusOK || email != "owner@acme.example" {
		t.Fatalf("positive control: verified AVA email: status %d email %q", code, email)
	}
}
