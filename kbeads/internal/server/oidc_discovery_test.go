package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	testPublicIssuer = "https://auth.example.test/auth/realms/gardenio"
	testAudience     = "gardenio-traefik-forwardauth"
)

// fakeKeycloak serves a discovery document whose issuer is the PUBLIC
// hostname (KC_HOSTNAME=https://auth.example.test/auth) from a listener that
// is reachable under a different, in-cluster-style URL, plus its JWKS. It
// mirrors Keycloak with hostname-backchannel-dynamic: public issuer, in-cluster
// jwks_uri.
type fakeKeycloak struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	signer jose.Signer
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	return startFakeKeycloak(t, httptest.NewServer)
}

// newFakeKeycloakTLS is newFakeKeycloak over https, with a certificate from
// httptest's private CA (the keycloak-runtime in-cluster port's shape).
func newFakeKeycloakTLS(t *testing.T) *fakeKeycloak {
	t.Helper()
	return startFakeKeycloak(t, httptest.NewTLSServer)
}

func startFakeKeycloak(t *testing.T, start func(http.Handler) *httptest.Server) *fakeKeycloak {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeKeycloak{key: key, signer: signer}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth/realms/gardenio/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                testPublicIssuer,
			"jwks_uri":                              f.srv.URL + "/auth/realms/gardenio/certs",
			"authorization_endpoint":                testPublicIssuer + "/protocol/openid-connect/auth",
			"token_endpoint":                        f.srv.URL + "/auth/realms/gardenio/protocol/openid-connect/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/auth/realms/gardenio/certs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"},
		}})
	})
	f.srv = start(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKeycloak) discoveryURL() string { return f.srv.URL + "/auth/realms/gardenio" }

func (f *fakeKeycloak) mint(t *testing.T, iss, aud string, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	std := jwt.Claims{
		Issuer:   iss,
		Subject:  "user-1",
		Audience: jwt.Audience{aud},
		IssuedAt: jwt.NewNumericDate(now),
		Expiry:   jwt.NewNumericDate(now.Add(time.Hour)),
	}
	raw, err := jwt.Signed(f.signer).Claims(std).Claims(extra).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// The daemon must be able to validate tokens whose iss is the public issuer
// while fetching discovery over a different (in-cluster) URL.
func TestNewOIDCVerifierWithDiscovery_PublicIssuerInClusterDiscovery(t *testing.T) {
	kc := newFakeKeycloak(t)
	ctx := context.Background()

	// Without the override, discovery at the issuer URL cannot be reached, and
	// pointing the issuer at the in-cluster URL mismatches the document's issuer.
	if _, err := NewOIDCVerifierWithDiscovery(ctx, kc.discoveryURL(), "", testAudience); err == nil ||
		!strings.Contains(err.Error(), "did not match") {
		t.Fatalf("plain discovery against a differing document issuer: want issuer mismatch error, got %v", err)
	}

	v, err := NewOIDCVerifierWithDiscovery(ctx, testPublicIssuer, kc.discoveryURL(), testAudience)
	if err != nil {
		t.Fatalf("NewOIDCVerifierWithDiscovery: %v", err)
	}
	good := kc.mint(t, testPublicIssuer, testAudience, map[string]any{"email": "owner@example.test", "email_verified": true})
	if err := v.Verify(ctx, good); err != nil {
		t.Fatalf("Verify good token: %v", err)
	}
	// iss pinned to the configured issuer: the in-cluster URL is not accepted as iss.
	if err := v.Verify(ctx, kc.mint(t, kc.discoveryURL(), testAudience, nil)); err == nil {
		t.Fatal("token with in-cluster iss must be rejected")
	}
	// aud must include the configured audience.
	if err := v.Verify(ctx, kc.mint(t, testPublicIssuer, "account", nil)); err == nil {
		t.Fatal("token with the wrong audience must be rejected")
	}
}

// BEADS_OIDC_CA_FILE (gd-lYPZlslOM7): discovery and the JWKS fetch behind
// Verify both go over https to a private CA, trusted only through
// OIDCCAContext.
func TestOIDCCAContext_TrustsPrivateCAForDiscoveryAndJWKS(t *testing.T) {
	kc := newFakeKeycloakTLS(t)
	ctx := context.Background()
	if _, err := NewOIDCVerifierWithDiscovery(ctx, testPublicIssuer, kc.discoveryURL(), testAudience); err == nil ||
		!strings.Contains(err.Error(), "certificate") {
		t.Fatalf("without the CA: want a certificate error, got %v", err)
	}

	dir := t.TempDir()
	if _, err := OIDCCAContext(ctx, filepath.Join(dir, "absent")); err == nil {
		t.Fatal("missing CA file: want an error")
	}
	bad := filepath.Join(dir, "bad.crt")
	if err := os.WriteFile(bad, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OIDCCAContext(ctx, bad); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("non-PEM CA file: want an error, got %v", err)
	}
	if got, err := OIDCCAContext(ctx, ""); err != nil || got != ctx {
		t.Fatalf("empty CA file: want ctx unchanged, got (%v, %v)", got, err)
	}

	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: kc.srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	caCtx, err := OIDCCAContext(ctx, caFile)
	if err != nil {
		t.Fatalf("OIDCCAContext: %v", err)
	}
	v, err := NewOIDCVerifierWithDiscovery(caCtx, testPublicIssuer, kc.discoveryURL(), testAudience)
	if err != nil {
		t.Fatalf("NewOIDCVerifierWithDiscovery with the CA: %v", err)
	}
	// Verify fetches the JWKS over the same https listener: the provider must
	// have kept the CA-trusting client, whatever ctx Verify is given.
	good := kc.mint(t, testPublicIssuer, testAudience, map[string]any{"email": "owner@example.test", "email_verified": true})
	if err := v.Verify(context.Background(), good); err != nil {
		t.Fatalf("Verify over https: %v", err)
	}
}

// A discovery URL equal to the issuer is plain discovery (no override context).
func TestNewOIDCVerifierWithDiscovery_SameURLIsPlainDiscovery(t *testing.T) {
	kc := newFakeKeycloak(t)
	if _, err := NewOIDCVerifierWithDiscovery(context.Background(), kc.discoveryURL(), kc.discoveryURL(), testAudience); err == nil {
		t.Fatal("expected issuer mismatch: the fake serves the public issuer")
	}
}

// A browser login through the forwardauth client yields roles the daemon can
// read from whichever claim the token carries them in.
func TestOIDCVerifier_ExtractRoles_RealmClaims(t *testing.T) {
	kc := newFakeKeycloak(t)
	ctx := context.Background()
	v, err := NewOIDCVerifierWithDiscovery(ctx, testPublicIssuer, kc.discoveryURL(), testAudience)
	if err != nil {
		t.Fatal(err)
	}
	re := v.(RoleExtractor)

	cases := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{"realm_access only (access-token shape)", map[string]any{
			"realm_access": map[string]any{"roles": []string{"gardenio-admin", "offline_access"}},
		}, []string{"gardenio-admin", "offline_access"}},
		{"flat realm_roles only (ID-token shape from the client mapper)", map[string]any{
			"realm_roles": []string{"gardenio-admin", "uma_authorization"},
		}, []string{"gardenio-admin", "uma_authorization"}},
		{"both, merged and deduped", map[string]any{
			"realm_access": map[string]any{"roles": []string{"gardenio-admin"}},
			"realm_roles":  []string{"gardenio-admin", "viewer"},
		}, []string{"gardenio-admin", "viewer"}},
		{"neither", map[string]any{"email": "a@example.test", "email_verified": true}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := re.ExtractRoles(ctx, kc.mint(t, testPublicIssuer, testAudience, tc.claims))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("roles = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNewMultiOIDCVerifierWithDiscovery_PerIssuerOverride(t *testing.T) {
	kc := newFakeKeycloak(t)
	ctx := context.Background()
	v, err := NewMultiOIDCVerifierWithDiscovery(ctx, []string{testPublicIssuer},
		map[string]string{testPublicIssuer: kc.discoveryURL()}, testAudience)
	if err != nil {
		t.Fatalf("single issuer with override: %v", err)
	}
	if err := v.Verify(ctx, kc.mint(t, testPublicIssuer, testAudience, nil)); err != nil {
		t.Fatal(err)
	}
	// Without the override map the same issuer list fails discovery.
	if _, err := NewMultiOIDCVerifierWithDiscovery(ctx, []string{kc.discoveryURL()}, nil, testAudience); err == nil {
		t.Fatal("expected failure without override")
	}
}
