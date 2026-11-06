package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// testAudience is the OAuth client the test issuer's tokens are issued to.
const testAudience = "gardenio-test"

// testIssuer is a Keycloak-shaped OIDC issuer over TLS (oidcauth refuses a
// non-https issuer) that signs RS256 access tokens for any claims.
type testIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &testIssuer{key: key, kid: "test-key"}
	mux := http.NewServeMux()
	iss.srv = httptest.NewTLSServer(mux)
	t.Cleanup(iss.srv.Close)
	b64 := base64.RawURLEncoding.EncodeToString
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss.url(),
			"jwks_uri":                              iss.url() + "/certs",
			"authorization_endpoint":                iss.url() + "/auth",
			"token_endpoint":                        iss.url() + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/certs", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "alg": "RS256", "use": "sig", "kid": iss.kid,
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes()),
		}}})
	})
	return iss
}

func (iss *testIssuer) url() string { return iss.srv.URL }

// clientContext makes oidcauth's discovery and JWKS fetches trust the
// issuer's self-signed certificate.
func (iss *testIssuer) clientContext(ctx context.Context) context.Context {
	return oidc.ClientContext(ctx, iss.srv.Client())
}

// tenant mints a bearer for a Lands tenant: the project-member role and the
// given groups, minted for land (aud land-<land>; "" for no land audience).
func (iss *testIssuer) tenant(t *testing.T, land, email string, groups ...string) string {
	aud := []string{testAudience, "account"}
	if land != "" {
		aud = append(aud, landAudiencePrefix+land)
	}
	return iss.mint(t, map[string]any{
		"email":        email,
		"realm_access": map[string]any{"roles": []string{"gardenio-project-member"}},
		"groups":       groups,
		"aud":          aud,
	})
}

// mint signs a token with the standard claims plus extra (extra wins).
func (iss *testIssuer) mint(t *testing.T, extra map[string]any) string {
	t.Helper()
	now := time.Now().Unix()
	claims := map[string]any{
		"iss": iss.url(), "sub": "sub-" + time.Now().Format("150405.000000000"),
		"aud": []string{testAudience, "account"}, "azp": testAudience, "typ": "Bearer",
		"iat": now, "nbf": now - 5, "exp": now + 600,
		"email_verified": true,
	}
	for k, v := range extra {
		claims[k] = v
	}
	enc := func(v any) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	input := enc(map[string]string{"alg": "RS256", "typ": "JWT", "kid": iss.kid}) + "." + enc(claims)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, iss.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}
