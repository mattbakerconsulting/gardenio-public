package oidcauth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
)

// oidcFixture bundles a mock OIDC server with a signing key so tests can mint
// valid ID tokens and exercise verifyingMiddleware end to end.
type oidcFixture struct {
	srv    *httptest.Server
	priv   *rsa.PrivateKey
	issuer string
}

func newOIDCFixture(t *testing.T) *oidcFixture {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	oidcSrv := &oidctest.Server{
		PublicKeys: []oidctest.PublicKey{{
			PublicKey: priv.Public(),
			KeyID:     "test-key",
			Algorithm: oidc.RS256,
		}},
	}
	srv := httptest.NewServer(oidcSrv)
	oidcSrv.SetIssuer(srv.URL)
	t.Cleanup(srv.Close)
	return &oidcFixture{srv: srv, priv: priv, issuer: srv.URL}
}

// signToken mints a compact JWT signed with the fixture's key. Pass raw JSON
// so tests can control exactly what claims appear (including malformed ones).
func (f *oidcFixture) signToken(t *testing.T, claimsJSON string) string {
	t.Helper()
	return oidctest.SignIDToken(f.priv, "test-key", oidc.RS256, claimsJSON)
}

// validClaims returns legacy Keycloak-shaped claims that satisfy the standard
// checks (iss, aud, exp) with the supplied email (marked verified, as Keycloak's
// email scope does) and realm roles.
func (f *oidcFixture) validClaims(email string, roles []string) string {
	return f.validClaimsWithRoleClaims(email, "", `"realm_access": {"roles": %s}`, roles)
}

// validEntraClaims returns claims with app roles in the top-level roles claim
// (the Entra shape). The fixture issuer is not an Entra issuer, so like every
// other fixture the email carries email_verified; email_verified_test.go
// covers a real Entra token (pinned tenant, no email_verified).
func (f *oidcFixture) validEntraClaims(email string, roles []string) string {
	return f.validClaimsWithRoleClaims(email, "", `"roles": %s`, roles)
}

func (f *oidcFixture) validClaimsWithoutRoles(email string) string {
	return fmt.Sprintf(`{
		"iss": %q,
		"aud": "test-client",
		"sub": "user-1",
		"exp": %d,
		"email": %q,
		"email_verified": true
	}`, f.issuer, time.Now().Add(time.Hour).Unix(), email)
}

func (f *oidcFixture) validClaimsWithRoleClaims(email, standardClaimsOverride, roleClaimsFormat string, roles []string) string {
	exp := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	var b strings.Builder
	b.WriteString("[")
	for i, r := range roles {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q", r)
	}
	b.WriteString("]")
	rolesJSON := b.String()
	standardClaims := fmt.Sprintf(`
		"iss": %q,
		"aud": "test-client",
		"sub": "user-1",
		"exp": %s,`, f.issuer, exp)
	if standardClaimsOverride != "" {
		standardClaims = standardClaimsOverride
	}
	return fmt.Sprintf(`{
		%s
		"email": %q,
		"email_verified": true,
		%s
	}`, standardClaims, email, fmt.Sprintf(roleClaimsFormat, rolesJSON))
}

func TestNew_ProviderInitSucceeds_ReadyTrue(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client"}, slog.Default())
	waitReady(t, h)
}

func TestVerifyingMiddleware(t *testing.T) {
	f := newOIDCFixture(t)

	tests := []struct {
		name       string
		cfg        Config
		setupReq   func(*http.Request)
		wantStatus int
		wantEmail  string // expected EmailFromContext value in inner handler (empty = don't check)
		wantRoles  []string
	}{
		{
			name:       "missing token → 401",
			cfg:        Config{IssuerURL: f.issuer, ClientID: "test-client"},
			setupReq:   func(*http.Request) {},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "malformed token → 401",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client"},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer not-a-jwt")
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "wrong audience → 401",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client"},
			setupReq: func(r *http.Request) {
				tok := f.signToken(t, fmt.Sprintf(`{
					"iss": %q,
					"aud": "other-client",
					"sub": "u",
					"exp": %d,
					"email": "user@acme.example"
				}`, f.issuer, time.Now().Add(time.Hour).Unix()))
				r.Header.Set("Authorization", "Bearer "+tok)
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "legacy realm roles enrich context",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client"},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validClaims("user@acme.example", []string{RoleViewer})))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "user@acme.example",
			wantRoles:  []string{RoleViewer},
		},
		{
			name: "Entra top-level roles satisfy required role and enrich context",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: RoleOperator},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validEntraClaims("operator@acme.example", []string{RoleOperator})))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "operator@acme.example",
			wantRoles:  []string{RoleOperator},
		},
		{
			name: "Entra operator satisfies viewer floor",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: RoleViewer},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validEntraClaims("operator@acme.example", []string{RoleOperator})))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "operator@acme.example",
			wantRoles:  []string{RoleOperator},
		},
		{
			name: "Entra admin satisfies viewer floor",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: RoleViewer},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validEntraClaims("admin@acme.example", []string{RoleAdmin})))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "admin@acme.example",
			wantRoles:  []string{RoleAdmin},
		},
		{
			name: "missing roles fail required role",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: RoleViewer},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validClaimsWithoutRoles("user@acme.example")))
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "incorrect Entra role fails required role",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: RoleAdmin},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validEntraClaims("user@acme.example", []string{RoleViewer})))
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "required role missing → 403",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: "gardenio-admin"},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validClaims("user@acme.example", []string{RoleViewer})))
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "required role present → 200",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: "gardenio-admin"},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validClaims("user@acme.example", []string{"gardenio-admin"})))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "user@acme.example",
		},
		{
			name: "legacy custom required role preserves exact membership",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: "custom-realm-role"},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validClaims("user@acme.example", []string{"custom-realm-role"})))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "user@acme.example",
			wantRoles:  []string{"custom-realm-role"},
		},
		{
			name: "disallowed email domain → 403",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", AllowedEmailDomains: []string{"acme.example"}},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validClaims("user@evil.com", nil)))
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name: "allowed email domain → 200",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", AllowedEmailDomains: []string{"acme.example"}},
			setupReq: func(r *http.Request) {
				r.Header.Set("Authorization", "Bearer "+f.signToken(t,
					f.validClaims("user@acme.example", nil)))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "user@acme.example",
		},
		{
			name: "Entra preferred username is the verified email fallback",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", ExpectedTenantID: testEntraTenant, RequiredRole: RoleViewer, AllowedEmailDomains: []string{"acme.example"}},
			setupReq: func(r *http.Request) {
				claims := f.validClaimsWithRoleClaims(
					"",
					fmt.Sprintf(`
						"iss": %q,
						"aud": "test-client",
						"sub": "user-1",
						"exp": %d,
						"tid": %q,
						"preferred_username": "user@acme.example",`, f.issuer, time.Now().Add(time.Hour).Unix(), testEntraTenant),
					`"roles": %s`,
					[]string{RoleViewer},
				)
				r.Header.Set("Authorization", "Bearer "+f.signToken(t, claims))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "user@acme.example",
			wantRoles:  []string{RoleViewer},
		},
		{
			name: "valid Entra token via ForwardAuth header is verified and authorized",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client", RequiredRole: RoleViewer},
			setupReq: func(r *http.Request) {
				r.Header.Set("X-Auth-Request-Access-Token", f.signToken(t,
					f.validEntraClaims("user@acme.example", []string{RoleViewer})))
			},
			wantStatus: http.StatusOK,
			wantEmail:  "user@acme.example",
			wantRoles:  []string{RoleViewer},
		},
		{
			name: "ForwardAuth token with wrong audience is rejected",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client"},
			setupReq: func(r *http.Request) {
				claims := f.validClaimsWithRoleClaims(
					"user@acme.example",
					fmt.Sprintf(`
						"iss": %q,
						"aud": "other-client",
						"sub": "user-1",
						"exp": %d,`, f.issuer, time.Now().Add(time.Hour).Unix()),
					`"roles": %s`,
					[]string{RoleViewer},
				)
				r.Header.Set("X-Auth-Request-Access-Token", f.signToken(t, claims))
			},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name: "ForwardAuth token with wrong issuer is rejected",
			cfg:  Config{IssuerURL: f.issuer, ClientID: "test-client"},
			setupReq: func(r *http.Request) {
				claims := f.validClaimsWithRoleClaims(
					"user@acme.example",
					fmt.Sprintf(`
						"iss": "https://wrong-issuer.example",
						"aud": "test-client",
						"sub": "user-1",
						"exp": %d,`, time.Now().Add(time.Hour).Unix()),
					`"roles": %s`,
					[]string{RoleViewer},
				)
				r.Header.Set("X-Auth-Request-Access-Token", f.signToken(t, claims))
			},
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(context.Background(), tt.cfg, slog.Default())
			waitReady(t, h)

			var gotEmail string
			var gotRoles []string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotEmail = EmailFromContext(r.Context())
				gotRoles = RolesFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			tt.setupReq(req)
			rec := httptest.NewRecorder()
			h.Middleware(inner).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantEmail != "" && gotEmail != tt.wantEmail {
				t.Errorf("EmailFromContext = %q, want %q", gotEmail, tt.wantEmail)
			}
			if tt.wantRoles != nil {
				if len(gotRoles) != len(tt.wantRoles) {
					t.Errorf("RolesFromContext len = %d, want %d", len(gotRoles), len(tt.wantRoles))
				}
				for i, r := range tt.wantRoles {
					if i < len(gotRoles) && gotRoles[i] != r {
						t.Errorf("RolesFromContext[%d] = %q, want %q", i, gotRoles[i], r)
					}
				}
			}
		})
	}
}

func TestVerifyingMiddleware_EntraDelegatedContract(t *testing.T) {
	f := newOIDCFixture(t)
	const (
		tenantID = "49e4de0b-c774-439a-b26e-4aaa1c944113"
		piidID   = "04e2ec54-c730-4079-83a3-38401054c997"
	)

	cfg := Config{
		IssuerURL:                f.issuer,
		ClientID:                 "test-client",
		ExpectedTenantID:         tenantID,
		RequiredRole:             RoleViewer,
		RequiredScopes:           []string{"user_impersonation"},
		AllowedAuthorizedParties: []string{piidID},
		AllowedEmailDomains:      []string{"acme.example"},
	}

	claims := func(overrides string) string {
		t.Helper()
		return fmt.Sprintf(`{
			"iss": %q,
			"aud": "test-client",
			"sub": "user-1",
			"exp": %d,
			"tid": %q,
			"scp": "openid user_impersonation profile",
			"azp": %q,
			"preferred_username": "operator@acme.example",
			"roles": ["gardenio-operator"]
			%s
		}`, f.issuer, time.Now().Add(time.Hour).Unix(), tenantID, piidID, overrides)
	}

	tests := []struct {
		name       string
		claims     string
		wantStatus int
	}{
		{name: "valid delegated token", claims: claims(""), wantStatus: http.StatusOK},
		{
			name:       "missing scope",
			claims:     strings.Replace(claims(""), `"scp": "openid user_impersonation profile",`, "", 1),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "scope substring is not accepted",
			claims:     strings.Replace(claims(""), "openid user_impersonation profile", "openid user_impersonation_extra profile", 1),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong authorized party",
			claims:     strings.Replace(claims(""), piidID, "11111111-1111-1111-1111-111111111111", 1),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "wrong tenant",
			claims:     strings.Replace(claims(""), tenantID, "22222222-2222-2222-2222-222222222222", 1),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "missing gardenio role",
			claims:     strings.Replace(claims(""), `"roles": ["gardenio-operator"]`, `"roles": ["unrelated"]`, 1),
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "preferred username must be an allowed email",
			claims:     strings.Replace(claims(""), "operator@acme.example", "operator@example.com", 1),
			wantStatus: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := New(context.Background(), cfg, slog.Default())
			waitReady(t, h)

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+f.signToken(t, tt.claims))
			rec := httptest.NewRecorder()
			h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got := EmailFromContext(r.Context()); got != "operator@acme.example" {
					t.Errorf("email=%q, want operator@acme.example", got)
				}
				if got := RolesFromContext(r.Context()); len(got) != 1 || got[0] != RoleOperator {
					t.Errorf("roles=%v, want [%s]", got, RoleOperator)
				}
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status=%d, want %d (body=%q)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestContainsAllScopes(t *testing.T) {
	tests := []struct {
		name     string
		claim    string
		required []string
		want     bool
	}{
		{name: "disabled", required: nil, want: true},
		{name: "exact", claim: "openid user_impersonation profile", required: []string{"user_impersonation"}, want: true},
		{name: "multiple", claim: "scope.two scope.one", required: []string{"scope.one", "scope.two"}, want: true},
		{name: "missing", claim: "openid profile", required: []string{"user_impersonation"}, want: false},
		{name: "substring", claim: "user_impersonation_extra", required: []string{"user_impersonation"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := containsAllScopes(tt.claim, tt.required); got != tt.want {
				t.Errorf("containsAllScopes(%q, %v)=%v, want %v", tt.claim, tt.required, got, tt.want)
			}
		})
	}
}

func TestOIDCClaimsEffectiveRoles(t *testing.T) {
	entraRoles := []string{RoleOperator}
	emptyEntraRoles := []string{}

	tests := []struct {
		name   string
		claims oidcClaims
		want   []string
	}{
		{
			name: "Entra top-level roles take precedence",
			claims: oidcClaims{
				Roles:       &entraRoles,
				RealmAccess: realmAccessClaims{Roles: []string{RoleAdmin}},
			},
			want: []string{RoleOperator},
		},
		{
			name: "legacy realm roles are fallback when top-level claim is absent",
			claims: oidcClaims{
				RealmAccess: realmAccessClaims{Roles: []string{RoleViewer}},
			},
			want: []string{RoleViewer},
		},
		{
			name: "explicit empty Entra roles do not fall back",
			claims: oidcClaims{
				Roles:       &emptyEntraRoles,
				RealmAccess: realmAccessClaims{Roles: []string{RoleAdmin}},
			},
			want: []string{},
		},
		{
			name:   "missing roles returns nil",
			claims: oidcClaims{},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.claims.effectiveRoles()
			if len(got) != len(tt.want) {
				t.Fatalf("effectiveRoles() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("effectiveRoles()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestEmailFromContext(t *testing.T) {
	tests := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"empty context", context.Background(), ""},
		{"with email", context.WithValue(context.Background(), ctxEmail, "user@example.com"), "user@example.com"},
		{"wrong type in context", context.WithValue(context.Background(), ctxEmail, 42), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EmailFromContext(tt.ctx); got != tt.want {
				t.Errorf("EmailFromContext = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleMiddleware_PassesThroughWhenDisabled(t *testing.T) {
	// Handle.Middleware wraps h.middleware; the empty-issuer path makes it a
	// passthrough. Covers the 1-line method directly (vs. the package-level
	// Middleware() helper exercised elsewhere).
	h := New(context.Background(), Config{IssuerURL: ""}, slog.Default())
	called := false
	h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !called {
		t.Error("inner handler not invoked by passthrough Handle.Middleware")
	}
}

// TestVerifyingMiddleware_MarksOIDCVerified: a request that passes the OIDC
// verifier is marked verified, role or no role (a Lands applicant holds none),
// and nothing else marks it.
func TestVerifyingMiddleware_MarksOIDCVerified(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client"}, slog.Default())
	waitReady(t, h)
	var got bool
	handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = OIDCVerifiedFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+f.signToken(t, f.validClaimsWithoutRoles("stranger@example.com")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !got {
		t.Fatalf("roleless verified token: status %d, verified=%v; want 200 and true", rec.Code, got)
	}
	if OIDCVerifiedFromContext(context.Background()) {
		t.Fatal("a bare context reports OIDC-verified")
	}
	if !OIDCVerifiedFromContext(ContextWithOIDCVerified(context.Background())) {
		t.Fatal("ContextWithOIDCVerified does not mark the context")
	}
}

// TestVerifyingMiddleware_GitHubID: the github_id claim (Keycloak's GitHub IdP
// mapper) reaches the context as a number or a numeric string; anything else,
// or no claim, is 0.
func TestVerifyingMiddleware_GitHubID(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client"}, slog.Default())
	waitReady(t, h)
	for name, tc := range map[string]struct {
		claim any
		want  int64
	}{
		"number":   {4242, 4242},
		"string":   {"4242", 4242},
		"absent":   {nil, 0},
		"garbage":  {"octocat", 0},
		"negative": {-1, 0},
		"float":    {42.5, 0},
	} {
		t.Run(name, func(t *testing.T) {
			var got int64 = -99
			handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = GitHubIDFromContext(r.Context())
			}))
			claims := f.validClaimsWithoutRoles("stranger@example.com")
			if tc.claim != nil {
				raw, _ := json.Marshal(tc.claim)
				claims = strings.Replace(claims, `"sub":`, `"github_id": `+string(raw)+`, "sub":`, 1)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+f.signToken(t, claims))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || got != tc.want {
				t.Fatalf("status %d, github_id %d; want 200 and %d", rec.Code, got, tc.want)
			}
		})
	}
	if GitHubIDFromContext(ContextWithGitHubID(context.Background(), 7)) != 7 {
		t.Fatal("ContextWithGitHubID does not carry the id")
	}
}

// TestVerifyingMiddleware_GitHubLogin: the github_login claim (Keycloak's
// GitHub IdP mapper, gd-0o4Ggf9ngP) reaches the context when it is a valid
// GitHub username; anything else, or no claim, is "".
func TestVerifyingMiddleware_GitHubLogin(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client"}, slog.Default())
	waitReady(t, h)
	for name, tc := range map[string]struct {
		claim any
		want  string
	}{
		"login":          {"octocat", "octocat"},
		"hyphenated":     {"mona-lisa-2", "mona-lisa-2"},
		"39 characters":  {strings.Repeat("a", 39), strings.Repeat("a", 39)},
		"absent":         {nil, ""},
		"40 characters":  {strings.Repeat("a", 40), ""},
		"leading hyphen": {"-octocat", ""},
		"double hyphen":  {"octo--cat", ""},
		"markup":         {"<b>octo</b>", ""},
		"number":         {4242, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got := "unset"
			handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = GitHubLoginFromContext(r.Context())
			}))
			claims := f.validClaimsWithoutRoles("stranger@example.com")
			if tc.claim != nil {
				raw, _ := json.Marshal(tc.claim)
				claims = strings.Replace(claims, `"sub":`, `"github_login": `+string(raw)+`, "sub":`, 1)
			}
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Authorization", "Bearer "+f.signToken(t, claims))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || got != tc.want {
				t.Fatalf("status %d, github_login %q; want 200 and %q", rec.Code, got, tc.want)
			}
		})
	}
	if GitHubLoginFromContext(ContextWithGitHubLogin(context.Background(), "octocat")) != "octocat" {
		t.Fatal("ContextWithGitHubLogin does not carry the login")
	}
}

// TestVerifyingMiddleware_Subject: the verified token's sub claim reaches the
// context; a request with no verified token carries none.
func TestVerifyingMiddleware_Subject(t *testing.T) {
	f := newOIDCFixture(t)
	h := New(context.Background(), Config{IssuerURL: f.issuer, ClientID: "test-client"}, slog.Default())
	waitReady(t, h)
	got := "unset"
	handler := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = SubjectFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+f.signToken(t, f.validClaimsWithoutRoles("stranger@example.com")))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || got != "user-1" {
		t.Fatalf("status %d, sub %q; want 200 and user-1", rec.Code, got)
	}
	if SubjectFromContext(context.Background()) != "" {
		t.Fatal("an empty context carries a subject")
	}
	if SubjectFromContext(ContextWithSubject(context.Background(), "s")) != "s" {
		t.Fatal("ContextWithSubject does not carry the subject")
	}
}
