package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"gardenio/controller/internal/glass"
	"gardenio/controller/internal/oidcauth"
)

const edgeBearerAudience = "gardenio-traefik-forwardauth"

// edgeBearerRealm is a stand-in Keycloak realm: a discovery + JWKS server and
// the key it signs access tokens with.
type edgeBearerRealm struct {
	issuer string
	priv   *rsa.PrivateKey
}

func newEdgeBearerRealm(t *testing.T) *edgeBearerRealm {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	s := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: priv.Public(), KeyID: "kc", Algorithm: oidc.RS256}}}
	srv := httptest.NewServer(s)
	s.SetIssuer(srv.URL)
	t.Cleanup(srv.Close)
	return &edgeBearerRealm{issuer: srv.URL, priv: priv}
}

// verifier starts an oidcauth.EdgeBearerVerifier on the realm and waits for
// its discovery.
func (k *edgeBearerRealm) verifier(t *testing.T) *oidcauth.EdgeBearerVerifier {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	v, err := oidcauth.NewEdgeBearerVerifier(ctx, oidcauth.EdgeBearerConfig{IssuerURL: k.issuer, Audience: edgeBearerAudience}, testLogger())
	if err != nil {
		t.Fatalf("NewEdgeBearerVerifier: %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); !v.Ready(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("edge bearer discovery did not succeed")
		}
	}
	return v
}

// token mints a Keycloak-shaped access token. Zero values take the valid
// defaults: this realm's issuer, an expiry an hour out, this realm's key.
type tokenOpts struct {
	issuer string
	email  string
	exp    time.Time
	key    *rsa.PrivateKey
	// impersonator, when set, is the token's impersonator claim (an impersonated session, one-click Become).
	impersonator string
	// github, when set, is the token's github_id and github_login claims (a GitHub sign-in).
	github string
}

func (k *edgeBearerRealm) token(o tokenOpts) string {
	if o.issuer == "" {
		o.issuer = k.issuer
	}
	if o.exp.IsZero() {
		o.exp = time.Now().Add(time.Hour)
	}
	if o.key == nil {
		o.key = k.priv
	}
	imp := ""
	if o.impersonator != "" {
		imp = fmt.Sprintf(`"impersonator": %q, "impersonator_id": "admin-sub",`, o.impersonator)
	}
	if o.github != "" {
		imp += fmt.Sprintf(`"github_id": 351190870, "github_login": %q,`, o.github)
	}
	claims := fmt.Sprintf(`{
		"iss": %q, "aud": [%q, "account"], "azp": %q, "sub": "owner-sub", "exp": %d,
		"email": %q, "email_verified": true, %s
		"realm_access": {"roles": ["gardenio-admin", "offline_access"]},
		"realm_roles": ["gardenio-operator"],
		"groups": ["/gardenio-admins"]
	}`, o.issuer, edgeBearerAudience, edgeBearerAudience, o.exp.Unix(), o.email, imp)
	return oidctest.SignIDToken(o.key, "kc", oidc.RS256, claims)
}

const edgeOwner = "owner@example.com"

// edgeRequest is a request as viewer-authproxy forwards it: the edge's email,
// the edge secret, and the user's access token as Authorization.
func edgeRequest(secret, email, bearer string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/garden/glass/gd-agent", nil)
	if secret != "" {
		r.Header.Set(devAuthProxySecretHeader, secret)
	}
	if email != "" {
		r.Header.Set("X-Auth-Request-Email", email)
	}
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	return r
}

// seen runs r through the dev-bypass middleware and returns the context the
// handler saw.
func seen(t *testing.T, proxySecret string, v edgeBearerVerifier, r *http.Request) context.Context {
	t.Helper()
	var got context.Context
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })
	devBypassMiddleware(testLogger(), proxySecret, v)(next).ServeHTTP(httptest.NewRecorder(), r)
	if got == nil {
		t.Fatal("handler not reached")
	}
	return got
}

// TestDevBypassEdgeBearer_ValidTokenGivesRoles (gd-BmYmugDc4x): behind the
// edge, the forwarded Keycloak access token's realm roles reach the context,
// so the Glass watch tier lets the owner in.
func TestDevBypassEdgeBearer_ValidTokenGivesRoles(t *testing.T) {
	realm := newEdgeBearerRealm(t)
	v := realm.verifier(t)

	ctx := seen(t, edgeTestSecret, v, edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner})))

	if got := oidcauth.EmailFromContext(ctx); got != edgeOwner {
		t.Errorf("email = %q, want the edge's %q", got, edgeOwner)
	}
	roles := oidcauth.RolesFromContext(ctx)
	for _, want := range []string{"gardenio-admin", "gardenio-operator", "offline_access"} {
		if !slices.Contains(roles, want) {
			t.Errorf("roles = %v, missing %q (realm_access.roles and realm_roles are both read)", roles, want)
		}
	}
	if got := oidcauth.GroupsFromContext(ctx); !slices.Equal(got, []string{"/gardenio-admins"}) {
		t.Errorf("groups = %v, want the token's", got)
	}
	if oidcauth.OIDCVerifiedFromContext(ctx) {
		t.Error("the OIDC-verified mark is the OIDC middleware's alone; the bypass must not set it")
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	if who := glassIdentity(r); !glass.TierOperator.Allows(who.Roles) {
		t.Errorf("Glass watch tier refuses %v; the owner must be let in", who.Roles)
	}
}

// TestDevBypassEdgeBearer_GitHubAccount: behind the edge, a GitHub sign-in's
// github_id, github_login and sub reach the context, which /lands/github/install
// and /lands/apply need; without them every applicant read as "did not come
// through GitHub" (without sub, /lands/apply refused an applicant whose
// repository had just connected). A token with neither GitHub claim leaves
// both unset.
func TestDevBypassEdgeBearer_GitHubAccount(t *testing.T) {
	realm := newEdgeBearerRealm(t)
	v := realm.verifier(t)

	ctx := seen(t, edgeTestSecret, v, edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner, github: "fimbazland"})))
	if got := oidcauth.GitHubIDFromContext(ctx); got != 351190870 {
		t.Errorf("github id = %d, want the token's 351190870", got)
	}
	if got := oidcauth.GitHubLoginFromContext(ctx); got != "fimbazland" {
		t.Errorf("github login = %q, want the token's", got)
	}
	if got := oidcauth.SubjectFromContext(ctx); got != "owner-sub" {
		t.Errorf("subject = %q, want the token's owner-sub", got)
	}

	ctx = seen(t, edgeTestSecret, v, edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner})))
	if id, login := oidcauth.GitHubIDFromContext(ctx), oidcauth.GitHubLoginFromContext(ctx); id != 0 || login != "" {
		t.Errorf("github id, login = %d, %q on a token without the claims; want none", id, login)
	}

	ctx = seen(t, edgeTestSecret, v, edgeRequest(edgeTestSecret, "someone-else@example.com", realm.token(tokenOpts{email: edgeOwner, github: "fimbazland"})))
	if id, sub := oidcauth.GitHubIDFromContext(ctx), oidcauth.SubjectFromContext(ctx); id != 0 || sub != "" {
		t.Errorf("github id, subject = %d, %q when the token's email differs from the edge's; want none", id, sub)
	}
}

// TestDevBypassEdgeBearer_NoRoles: every way the token or the edge proof can
// fall short leaves the request with no roles at all.
func TestDevBypassEdgeBearer_NoRoles(t *testing.T) {
	realm := newEdgeBearerRealm(t)
	v := realm.verifier(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}

	cases := []struct {
		name        string
		proxySecret string
		req         *http.Request
		wantEmail   string
	}{
		{
			name:        "expired token",
			proxySecret: edgeTestSecret,
			req:         edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner, exp: time.Now().Add(-time.Minute)})),
			wantEmail:   edgeOwner,
		},
		{
			name:        "wrong issuer",
			proxySecret: edgeTestSecret,
			req:         edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner, issuer: "https://evil.example/realms/gardenio"})),
			wantEmail:   edgeOwner,
		},
		{
			name:        "bad signature",
			proxySecret: edgeTestSecret,
			req:         edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: edgeOwner, key: other})),
			wantEmail:   edgeOwner,
		},
		{
			name:        "missing edge secret",
			proxySecret: edgeTestSecret,
			req:         edgeRequest("", edgeOwner, realm.token(tokenOpts{email: edgeOwner})),
			wantEmail:   "",
		},
		{
			name:        "wrong edge secret",
			proxySecret: edgeTestSecret,
			req:         edgeRequest("not-the-secret", edgeOwner, realm.token(tokenOpts{email: edgeOwner})),
			wantEmail:   "",
		},
		{
			name:        "token email differs from the edge's",
			proxySecret: edgeTestSecret,
			req:         edgeRequest(edgeTestSecret, edgeOwner, realm.token(tokenOpts{email: "someone-else@example.com"})),
			wantEmail:   edgeOwner,
		},
		{
			name:        "no edge email",
			proxySecret: edgeTestSecret,
			req:         edgeRequest(edgeTestSecret, "", realm.token(tokenOpts{email: edgeOwner})),
			wantEmail:   "",
		},
		{
			name:        "no edge secret configured",
			proxySecret: "",
			req:         edgeRequest("", edgeOwner, realm.token(tokenOpts{email: edgeOwner})),
			wantEmail:   edgeOwner,
		},
		{
			name:        "not a JWT",
			proxySecret: edgeTestSecret,
			req:         edgeRequest(edgeTestSecret, edgeOwner, "opaque-token"),
			wantEmail:   edgeOwner,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := seen(t, tc.proxySecret, v, tc.req)
			if roles := oidcauth.RolesFromContext(ctx); len(roles) != 0 {
				t.Errorf("roles = %v, want none", roles)
			}
			if groups := oidcauth.GroupsFromContext(ctx); len(groups) != 0 {
				t.Errorf("groups = %v, want none", groups)
			}
			if got := oidcauth.EmailFromContext(ctx); got != tc.wantEmail {
				t.Errorf("email = %q, want %q", got, tc.wantEmail)
			}
		})
	}
}

// TestDevBypassEdgeBearer_NoBearerUnchanged: with no bearer the bypass does
// what it did before: the edge's email, no roles.
func TestDevBypassEdgeBearer_NoBearerUnchanged(t *testing.T) {
	realm := newEdgeBearerRealm(t)
	for name, v := range map[string]edgeBearerVerifier{"verifier": realm.verifier(t), "no verifier": nil} {
		t.Run(name, func(t *testing.T) {
			ctx := seen(t, edgeTestSecret, v, edgeRequest(edgeTestSecret, edgeOwner, ""))
			if got := oidcauth.EmailFromContext(ctx); got != edgeOwner {
				t.Errorf("email = %q, want %q", got, edgeOwner)
			}
			if roles := oidcauth.RolesFromContext(ctx); len(roles) != 0 {
				t.Errorf("roles = %v, want none", roles)
			}
		})
	}
}

// TestNewDevAuthEdgeBearer_Config: the verifier is built only with an issuer,
// an audience and an edge secret.
func TestNewDevAuthEdgeBearer_Config(t *testing.T) {
	realm := newEdgeBearerRealm(t)
	full := oidcauth.EdgeBearerConfig{IssuerURL: realm.issuer, Audience: edgeBearerAudience}
	cases := []struct {
		name   string
		cfg    config
		wantOn bool
	}{
		{"unset", config{devAuthProxySecret: edgeTestSecret}, false},
		{"issuer without audience", config{devAuthProxySecret: edgeTestSecret, devAuthEdgeBearer: oidcauth.EdgeBearerConfig{IssuerURL: realm.issuer}}, false},
		{"no edge secret", config{devAuthEdgeBearer: full}, false},
		{"unreadable CA file", config{devAuthProxySecret: edgeTestSecret, devAuthEdgeBearer: oidcauth.EdgeBearerConfig{IssuerURL: realm.issuer, Audience: edgeBearerAudience, CAFile: t.TempDir() + "/missing.crt"}}, false},
		{"configured", config{devAuthProxySecret: edgeTestSecret, devAuthEdgeBearer: full}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			v := newDevAuthEdgeBearer(ctx, &tc.cfg, testLogger())
			if (v != nil) != tc.wantOn {
				t.Errorf("verifier built = %v, want %v", v != nil, tc.wantOn)
			}
		})
	}
}
