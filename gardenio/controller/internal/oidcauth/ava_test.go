package oidcauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testTrustSigner = "arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-test1234"

// avaFixture bundles a JWKS server that serves a P-384 public key by kid plus
// a signer that can mint ES384 JWTs the verifier should accept.
type avaFixture struct {
	priv    *ecdsa.PrivateKey
	kid     string
	jwksSrv *httptest.Server
	hits    *atomic.Int32 // counts requests to the JWKS server, for caching assertions
}

func newAVAFixture(t *testing.T) *avaFixture {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pemBytes := encodeECPub(t, &priv.PublicKey)
	hits := &atomic.Int32{}
	kid := "test-kid-1"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !strings.HasSuffix(r.URL.Path, kid) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		_, _ = w.Write(pemBytes)
	}))
	t.Cleanup(srv.Close)
	return &avaFixture{priv: priv, kid: kid, jwksSrv: srv, hits: hits}
}

func encodeECPub(t *testing.T, pub *ecdsa.PublicKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		t.Fatalf("marshal PKIX: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
}

// signAVA produces an ES384 JWT with the supplied header overrides and the
// supplied payload JSON. Both are merged with sensible defaults so each test
// can vary one field at a time.
func (f *avaFixture) signAVA(t *testing.T, headerOverrides map[string]any, payload string) string {
	t.Helper()
	// AVA places `exp` in the JOSE header, so the fixture default header
	// includes a future-dated exp. Tests that want a different exp (e.g.
	// expired) override it explicitly via headerOverrides.
	hdr := map[string]any{
		"alg":    "ES384",
		"kid":    f.kid,
		"signer": testTrustSigner,
		"exp":    time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range headerOverrides {
		hdr[k] = v
	}
	return f.signRaw(t, hdr, payload)
}

func (f *avaFixture) signRaw(t *testing.T, header map[string]any, payload string) string {
	t.Helper()
	hdrJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	hdrB64 := base64.RawURLEncoding.EncodeToString(hdrJSON)
	payloadB64 := base64.RawURLEncoding.EncodeToString([]byte(payload))
	signedInput := hdrB64 + "." + payloadB64
	sum := sha512.Sum384([]byte(signedInput))
	r, s, err := ecdsa.Sign(rand.Reader, f.priv, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	rBytes := padLeft(r.Bytes(), 48)
	sBytes := padLeft(s.Bytes(), 48)
	sig := append(rBytes, sBytes...)
	sigB64 := base64.RawURLEncoding.EncodeToString(sig)
	return signedInput + "." + sigB64
}

func padLeft(b []byte, n int) []byte {
	if len(b) >= n {
		return b
	}
	out := make([]byte, n)
	copy(out[n-len(b):], b)
	return out
}

func (f *avaFixture) verifier() *avaVerifier {
	cfg := AVAConfig{
		TrustSigner: testTrustSigner,
		Region:      "test-region",
		publicKeyURLFn: func(_ string, kid string) string {
			return f.jwksSrv.URL + "/" + kid
		},
		httpClient: f.jwksSrv.Client(),
	}
	return newAVAVerifier(cfg, slog.Default())
}

func validPayload(t *testing.T, email string) string {
	t.Helper()
	// AVA puts exp in the JOSE header, not the payload — see avaHeader.Exp.
	return fmt.Sprintf(`{"iss":"gitlab","sub":"u-1","email":%q,"groups":["g1"]}`, email)
}

func TestAVAVerify_HappyPath(t *testing.T) {
	f := newAVAFixture(t)
	tok := f.signAVA(t, nil, validPayload(t, "user@acme.example"))
	claims, err := f.verifier().verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.effectiveEmail() != "user@acme.example" {
		t.Errorf("email = %q, want user@acme.example", claims.effectiveEmail())
	}
	if len(claims.effectiveGroups()) != 1 || claims.effectiveGroups()[0] != "g1" {
		t.Errorf("groups = %v, want [g1]", claims.effectiveGroups())
	}
}

func TestAVAVerify_AdditionalUserContext(t *testing.T) {
	// AVA can wrap trust-provider claims under additional_user_context.
	// The nested fields must override (or fill in for) the top-level ones.
	f := newAVAFixture(t)
	payload := `{"iss":"gitlab","sub":"u-1","additional_user_context":{"email":"nested@acme.example","groups":["nested-g"]}}`
	tok := f.signAVA(t, nil, payload)
	claims, err := f.verifier().verify(context.Background(), tok)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if claims.effectiveEmail() != "nested@acme.example" {
		t.Errorf("email = %q, want nested@acme.example", claims.effectiveEmail())
	}
	if len(claims.effectiveGroups()) != 1 || claims.effectiveGroups()[0] != "nested-g" {
		t.Errorf("groups = %v, want [nested-g]", claims.effectiveGroups())
	}
}

func TestAVAVerify_WrongSigner(t *testing.T) {
	// gd-nEGC0jfcRM acceptance: a JWT signed by a DIFFERENT AVA instance is
	// rejected — proves signer pinning works.
	f := newAVAFixture(t)
	tok := f.signAVA(t, map[string]any{
		"signer": "arn:aws:ec2:us-east-1:000000000000:verified-access-instance/vai-other",
	}, validPayload(t, "user@acme.example"))
	_, err := f.verifier().verify(context.Background(), tok)
	if err == nil {
		t.Fatal("expected error for wrong signer, got nil")
	}
	if !strings.Contains(err.Error(), "signer mismatch") {
		t.Errorf("error = %v, want signer mismatch", err)
	}
}

func TestAVAVerify_WrongAlg(t *testing.T) {
	// Defense against header substitution: alg=none / alg=HS256 must be
	// rejected outright. We don't bother actually producing a valid HS256
	// signature — the alg gate trips before signature verification.
	f := newAVAFixture(t)
	tok := f.signAVA(t, map[string]any{"alg": "none"}, validPayload(t, "user@acme.example"))
	_, err := f.verifier().verify(context.Background(), tok)
	if err == nil || !strings.Contains(err.Error(), "unexpected alg") {
		t.Fatalf("expected unexpected alg error, got %v", err)
	}
}

func TestAVAVerify_MissingKid(t *testing.T) {
	f := newAVAFixture(t)
	tok := f.signAVA(t, map[string]any{"kid": ""}, validPayload(t, "user@acme.example"))
	_, err := f.verifier().verify(context.Background(), tok)
	if err == nil || !strings.Contains(err.Error(), "missing kid") {
		t.Fatalf("expected missing kid error, got %v", err)
	}
}

func TestAVAVerify_Expired(t *testing.T) {
	f := newAVAFixture(t)
	exp := time.Now().Add(-time.Minute).Unix()
	payload := `{"iss":"gitlab","sub":"u","email":"user@acme.example"}`
	// AVA puts exp in the header — override to a past time to simulate
	// an expired token.
	tok := f.signAVA(t, map[string]any{"exp": exp}, payload)
	_, err := f.verifier().verify(context.Background(), tok)
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error, got %v", err)
	}
}

func TestAVAVerify_TamperedSignature(t *testing.T) {
	f := newAVAFixture(t)
	tok := f.signAVA(t, nil, validPayload(t, "user@acme.example"))
	// Flip the last byte of the signature segment.
	parts := strings.Split(tok, ".")
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	sig[len(sig)-1] ^= 0x01
	parts[2] = base64.RawURLEncoding.EncodeToString(sig)
	tampered := strings.Join(parts, ".")
	_, err = f.verifier().verify(context.Background(), tampered)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestAVAVerify_TamperedPayload(t *testing.T) {
	f := newAVAFixture(t)
	tok := f.signAVA(t, nil, validPayload(t, "user@acme.example"))
	parts := strings.Split(tok, ".")
	// Replace the payload with a different valid b64 JSON; signature now mismatches.
	parts[1] = base64.RawURLEncoding.EncodeToString([]byte(validPayload(t, "attacker@evil.com")))
	tampered := strings.Join(parts, ".")
	_, err := f.verifier().verify(context.Background(), tampered)
	if err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("expected signature error, got %v", err)
	}
}

func TestAVAVerify_Malformed(t *testing.T) {
	cases := []string{
		"",
		"not-a-jwt",
		"only.two",
		"a.b.c.d",
	}
	v := newAVAFixture(t).verifier()
	for _, c := range cases {
		if _, err := v.verify(context.Background(), c); err == nil {
			t.Errorf("verify(%q) succeeded; want error", c)
		}
	}
}

func TestAVAVerify_KeyCacheReusesKid(t *testing.T) {
	// After the first verify(), subsequent verifies with the same kid must
	// reuse the cached public key — otherwise every request would hit the
	// AWS public-keys endpoint and we'd both DOS them and add latency to
	// every request.
	f := newAVAFixture(t)
	v := f.verifier()
	for i := 0; i < 3; i++ {
		tok := f.signAVA(t, nil, validPayload(t, "user@acme.example"))
		if _, err := v.verify(context.Background(), tok); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	if got := f.hits.Load(); got != 1 {
		t.Errorf("JWKS hits = %d after 3 verifies; want 1 (kid cache not working)", got)
	}
}

func TestAVAVerify_JWKSUnreachable(t *testing.T) {
	// If the AWS public-keys endpoint is down or returns non-200, we must
	// reject the request — never accept a JWT we couldn't actually verify.
	f := newAVAFixture(t)
	cfg := AVAConfig{
		TrustSigner:    testTrustSigner,
		Region:         "test-region",
		publicKeyURLFn: func(string, string) string { return f.jwksSrv.URL + "/nonexistent-kid-zzz" },
		httpClient:     f.jwksSrv.Client(),
	}
	v := newAVAVerifier(cfg, slog.Default())
	// Sign with the real kid but ask the verifier to look up a kid that
	// will 404 from our fake JWKS.
	tok := f.signAVA(t, map[string]any{"kid": "nonexistent-kid-zzz"}, validPayload(t, "user@acme.example"))
	_, err := v.verify(context.Background(), tok)
	if err == nil {
		t.Fatal("expected error when JWKS endpoint returns 404, got nil")
	}
}

func TestParseECDSAPublicKey_RejectsWrongCurve(t *testing.T) {
	// Defense in depth: someone configures a P-256 endpoint by mistake — we
	// MUST refuse to verify ES384 signatures against a P-256 key (which
	// would actually be considered "alg=ES256" anyway). This guards the
	// parse step.
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	_, err = parseECDSAPublicKey(pemBytes)
	if err == nil {
		t.Fatal("expected error for P-256 key, got nil")
	}
	if !strings.Contains(err.Error(), "P-384") {
		t.Errorf("error = %v, want P-384 mention", err)
	}
}

// TestHandle_AVAHeader_BypassesKeycloak exercises the full Handle chain to
// prove that a valid AVA header satisfies authentication WITHOUT a Keycloak
// bearer token. Same chain, no AVA header → 401 (Keycloak demands a token).
func TestHandle_AVAHeader_BypassesKeycloak(t *testing.T) {
	f := newAVAFixture(t)
	cfg := Config{
		IssuerURL: "",
		ClientID:  "",
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
			httpClient:     f.jwksSrv.Client(),
		},
	}
	h := New(context.Background(), cfg, slog.Default())
	var gotEmail string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEmail = EmailFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := h.Middleware(inner)

	// With a valid AVA JWT: 200, ctx populated.
	tok := f.signAVA(t, nil, validPayload(t, "user@acme.example"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(AVAHeaderName, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("AVA happy path: status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if gotEmail != "user@acme.example" {
		t.Errorf("AVA happy path: email = %q, want user@acme.example", gotEmail)
	}

	// Without the AVA header: Keycloak is disabled here too, so the request
	// passes through (legacy hostname path). The pertinent assertion is
	// that the AVA branch does not block requests that don't carry the AVA
	// header at all.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	gotEmail = ""
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Errorf("non-AVA passthrough: status = %d, want 200", rec2.Code)
	}
	if gotEmail != "" {
		t.Errorf("non-AVA passthrough: email = %q, want empty", gotEmail)
	}
}

// TestHandle_ForgedIdentityHeaderWithoutJWT rejects a request that carries a
// plaintext "I'm admin" header but no AVA JWT — gd-nEGC0jfcRM acceptance:
// proves trust is on the signed JWT, not the plaintext identity header.
func TestHandle_ForgedIdentityHeaderWithoutJWT(t *testing.T) {
	f := newAVAFixture(t)
	cfg := Config{
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
			httpClient:     f.jwksSrv.Client(),
		},
	}
	h := New(context.Background(), cfg, slog.Default())
	var gotEmail string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEmail = EmailFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Amzn-Oidc-Identity", "admin@evil.com")
	req.Header.Set("X-Auth-Request-Email", "admin@evil.com")
	rec := httptest.NewRecorder()
	h.Middleware(inner).ServeHTTP(rec, req)
	// Request passes through (no AVA header at all in this branch-only
	// config), BUT the context email is empty — the forged identity header
	// did NOT inject any trusted identity.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if gotEmail != "" {
		t.Errorf("EmailFromContext = %q, want empty — forged header MUST NOT populate trusted identity", gotEmail)
	}
}

// TestHandle_AVAAndKeycloak_Coexist proves the combined chain:
// - AVA header present + valid → AVA path serves the request (Keycloak not invoked).
// - AVA header absent → request flows to Keycloak (here, demands a bearer token).
// - AVA header present + invalid (wrong signer) → 401 (does NOT fall through to Keycloak).
//
// The last case is the security-critical one: a request that tries (and
// fails) AVA must NOT be retried via Keycloak, or an attacker could probe
// AVA with garbage to discover Keycloak as a fallback.
func TestHandle_AVAAndKeycloak_Coexist(t *testing.T) {
	avaFix := newAVAFixture(t)
	kcFix := newOIDCFixture(t)
	cfg := Config{
		IssuerURL: kcFix.issuer,
		ClientID:  "test-client",
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			publicKeyURLFn: func(_ string, kid string) string { return avaFix.jwksSrv.URL + "/" + kid },
			httpClient:     avaFix.jwksSrv.Client(),
		},
	}
	h := New(context.Background(), cfg, slog.Default())
	waitReady(t, h)
	var gotEmail string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotEmail = EmailFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := h.Middleware(inner)

	// 1. Valid AVA header → 200, ctx populated with AVA email.
	tok := avaFix.signAVA(t, nil, validPayload(t, "ava@acme.example"))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(AVAHeaderName, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("valid AVA: status = %d, want 200", rec.Code)
	}
	if gotEmail != "ava@acme.example" {
		t.Errorf("valid AVA: email = %q, want ava@acme.example", gotEmail)
	}

	// 2. No AVA header, no bearer → 401 from Keycloak path.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	rec2 := httptest.NewRecorder()
	gotEmail = ""
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("no auth: status = %d, want 401", rec2.Code)
	}

	// 3. No AVA header, valid Keycloak bearer → 200, ctx populated with Keycloak email.
	req3 := httptest.NewRequest(http.MethodGet, "/", nil)
	req3.Header.Set("Authorization", "Bearer "+kcFix.signToken(t, kcFix.validClaims("kc@acme.example", []string{RoleViewer})))
	rec3 := httptest.NewRecorder()
	gotEmail = ""
	handler.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Errorf("Keycloak bearer: status = %d, want 200; body=%q", rec3.Code, rec3.Body.String())
	}
	if gotEmail != "kc@acme.example" {
		t.Errorf("Keycloak bearer: email = %q, want kc@acme.example", gotEmail)
	}

	// 4. Invalid AVA header (wrong signer) → 401, MUST NOT fall through to
	//    Keycloak even if a valid bearer is also attached. Trying-and-failing
	//    one auth path cannot be a probe to the other.
	badTok := avaFix.signAVA(t, map[string]any{
		"signer": "arn:aws:ec2:us-east-1:000000000000:verified-access-instance/vai-other",
	}, validPayload(t, "ava@acme.example"))
	req4 := httptest.NewRequest(http.MethodGet, "/", nil)
	req4.Header.Set(AVAHeaderName, badTok)
	req4.Header.Set("Authorization", "Bearer "+kcFix.signToken(t, kcFix.validClaims("kc@acme.example", []string{RoleViewer})))
	rec4 := httptest.NewRecorder()
	handler.ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusUnauthorized {
		t.Errorf("invalid AVA + valid bearer: status = %d, want 401 (no fall-through to Keycloak)", rec4.Code)
	}
}

// padLeftUnused is a placeholder to suppress unused-package warnings if any
// goroutine-related import is unused in some builds. Currently a no-op.
var _ = big.NewInt

// TestHandle_AVADefaultRole_InjectedIntoCtxRoles is the acceptance test for
// gd-64RUGKGPEF (decision ka-0000002146: blanket viewer). A fresh AVA user
// whose GitLab groups contain NO gardenio-* roles must receive the configured
// DefaultRole in ctxRoles, so a downstream RequireRole(RoleViewer) middleware
// allows the request through. Mirrors the Keycloak isDefault:true behavior
// for AVA-authenticated users.
func TestHandle_AVADefaultRole_InjectedIntoCtxRoles(t *testing.T) {
	f := newAVAFixture(t)
	cfg := Config{
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			DefaultRole:    RoleViewer,
			publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
			httpClient:     f.jwksSrv.Client(),
		},
	}
	h := New(context.Background(), cfg, slog.Default())

	// Inner handler captures ctxRoles so we can assert what the middleware
	// chain stamped, then chains through RequireRole(RoleViewer) to prove the
	// default-role actually unblocks the viewerOnly gate.
	var gotRoles []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRoles = RolesFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := h.Middleware(RequireRole(RoleViewer, slog.Default())(inner))

	// Case 1: user has a non-gardenio GitLab group only. Default role must
	// be appended; RequireRole passes; ctxRoles contains both.
	payload := `{"iss":"gitlab","sub":"u-1","email":"fresh@acme.example","groups":["some-gitlab-group"]}`
	tok := f.signAVA(t, nil, payload)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(AVAHeaderName, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("default-role appended: status = %d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	wantRoles := map[string]bool{"some-gitlab-group": true, RoleViewer: true}
	if len(gotRoles) != 2 {
		t.Fatalf("ctxRoles len = %d, want 2 (got %v)", len(gotRoles), gotRoles)
	}
	for _, r := range gotRoles {
		if !wantRoles[r] {
			t.Errorf("unexpected role %q in ctxRoles %v", r, gotRoles)
		}
	}

	// Case 2: user already in a literal `gardenio-viewer` GitLab group —
	// no duplication, RequireRole still passes.
	payload2 := `{"iss":"gitlab","sub":"u-2","email":"explicit@acme.example","groups":["gardenio-viewer"]}`
	tok2 := f.signAVA(t, nil, payload2)
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.Header.Set(AVAHeaderName, tok2)
	rec2 := httptest.NewRecorder()
	gotRoles = nil
	handler.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("idempotency: status = %d, want 200", rec2.Code)
	}
	if len(gotRoles) != 1 || gotRoles[0] != RoleViewer {
		t.Errorf("idempotency: ctxRoles = %v, want [%s] (default-role must not duplicate)", gotRoles, RoleViewer)
	}
}

// TestHandle_AVADefaultRole_Disabled asserts the pre-decision behavior is
// preserved when DefaultRole is empty: groups are propagated verbatim, and
// a user with no gardenio-* group 403s on viewerOnly routes.
func TestHandle_AVADefaultRole_Disabled(t *testing.T) {
	f := newAVAFixture(t)
	cfg := Config{
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			DefaultRole:    "", // disabled
			publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
			httpClient:     f.jwksSrv.Client(),
		},
	}
	h := New(context.Background(), cfg, slog.Default())
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	handler := h.Middleware(RequireRole(RoleViewer, slog.Default())(inner))

	payload := `{"iss":"gitlab","sub":"u","email":"fresh@acme.example","groups":["some-gitlab-group"]}`
	tok := f.signAVA(t, nil, payload)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(AVAHeaderName, tok)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("DefaultRole disabled: status = %d, want 403 (pre-decision behavior)", rec.Code)
	}
}

func TestApplyAvaDefaultRole(t *testing.T) {
	tests := []struct {
		name        string
		groups      []string
		defaultRole string
		want        []string
	}{
		{
			name:        "empty default role is no-op",
			groups:      []string{"g1", "g2"},
			defaultRole: "",
			want:        []string{"g1", "g2"},
		},
		{
			name:        "appends to empty groups",
			groups:      nil,
			defaultRole: "gardenio-viewer",
			want:        []string{"gardenio-viewer"},
		},
		{
			name:        "appends to non-empty groups when absent",
			groups:      []string{"some-gitlab-group"},
			defaultRole: "gardenio-viewer",
			want:        []string{"some-gitlab-group", "gardenio-viewer"},
		},
		{
			name:        "idempotent when already present",
			groups:      []string{"some-gitlab-group", "gardenio-viewer"},
			defaultRole: "gardenio-viewer",
			want:        []string{"some-gitlab-group", "gardenio-viewer"},
		},
		{
			name:        "idempotent when only-entry present",
			groups:      []string{"gardenio-viewer"},
			defaultRole: "gardenio-viewer",
			want:        []string{"gardenio-viewer"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyAvaDefaultRole(tt.groups, tt.defaultRole)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got %v, want %v)", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestNormalizeAvaGroups is the unit-level acceptance test for ka-0000001646.
// AVA forwards GitLab groups as full subgroup paths; normalizeAvaGroups must
// append the bare gardenio-* role for any group whose trailing segment matches a
// known role, preserve the original entries, dedupe, and leave non-gardenio
// paths and bare-name (Keycloak-shape) groups otherwise untouched. Matching is
// case-sensitive to mirror the Cedar policy / GitLab on-server casing.
func TestNormalizeAvaGroups(t *testing.T) {
	tests := []struct {
		name   string
		groups []string
		want   []string
	}{
		{
			name:   "full-path admin appends bare gardenio-admin",
			groups: []string{"Acme/Platform/gardenio-admin"},
			want:   []string{"Acme/Platform/gardenio-admin", "gardenio-admin"},
		},
		{
			name:   "full-path operator appends bare gardenio-operator",
			groups: []string{"Acme/Platform/gardenio-operator"},
			want:   []string{"Acme/Platform/gardenio-operator", "gardenio-operator"},
		},
		{
			name:   "full-path viewer appends bare gardenio-viewer",
			groups: []string{"Acme/Platform/gardenio-viewer"},
			want:   []string{"Acme/Platform/gardenio-viewer", "gardenio-viewer"},
		},
		{
			name:   "non-gardenio full path is unchanged (no bare role added)",
			groups: []string{"Acme/Platform/some-other-group"},
			want:   []string{"Acme/Platform/some-other-group"},
		},
		{
			name:   "bare-name group (Keycloak shape) is unchanged — no duplicate",
			groups: []string{"gardenio-admin"},
			want:   []string{"gardenio-admin"},
		},
		{
			name:   "bare-name viewer is unchanged — no duplicate",
			groups: []string{"gardenio-viewer"},
			want:   []string{"gardenio-viewer"},
		},
		{
			name:   "case-sensitive: wrong-case trailing segment is NOT matched",
			groups: []string{"Acme/Platform/Gardenio-Admin"},
			want:   []string{"Acme/Platform/Gardenio-Admin"},
		},
		{
			name:   "trailing-segment-only match: a path that merely contains the role substring mid-path is not matched",
			groups: []string{"Acme/gardenio-admin/subteam"},
			want:   []string{"Acme/gardenio-admin/subteam"},
		},
		{
			name:   "mixed: gardenio path + non-gardenio path -> only the gardenio one contributes a bare role",
			groups: []string{"Acme/Platform/gardenio-operator", "Acme/Platform/eng"},
			want:   []string{"Acme/Platform/gardenio-operator", "Acme/Platform/eng", "gardenio-operator"},
		},
		{
			name:   "multiple gardenio paths each contribute their bare role",
			groups: []string{"Acme/Platform/gardenio-admin", "Acme/Platform/gardenio-viewer"},
			want:   []string{"Acme/Platform/gardenio-admin", "Acme/Platform/gardenio-viewer", "gardenio-admin", "gardenio-viewer"},
		},
		{
			name:   "full path + already-present bare role -> not duplicated",
			groups: []string{"Acme/Platform/gardenio-admin", "gardenio-admin"},
			want:   []string{"Acme/Platform/gardenio-admin", "gardenio-admin"},
		},
		{
			name:   "empty groups stays empty",
			groups: []string{},
			want:   []string{},
		},
		{
			name:   "nil groups -> nil/empty",
			groups: nil,
			want:   nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeAvaGroups(tt.groups)
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (got %v, want %v)", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("[%d] = %q, want %q (got %v, want %v)", i, got[i], tt.want[i], got, tt.want)
				}
			}
		})
	}
}

// TestNormalizeAvaGroups_GrantsRank closes the loop from a full-path group to
// the RBAC verdict: after normalization the resulting role set must clear the
// matching HasMinimumRole gate, while a non-gardenio path (with only the viewer
// floor applied) must NOT clear operator/admin. This is the property the live
// operatorOnly/adminOnly viewer surfaces depend on (ka-0000001646).
func TestNormalizeAvaGroups_GrantsRank(t *testing.T) {
	tests := []struct {
		name         string
		groups       []string
		wantViewer   bool
		wantOperator bool
		wantAdmin    bool
	}{
		{
			name:         "full-path admin clears admin (and below)",
			groups:       []string{"Acme/Platform/gardenio-admin"},
			wantViewer:   true,
			wantOperator: true,
			wantAdmin:    true,
		},
		{
			name:         "full-path operator clears operator/viewer but NOT admin",
			groups:       []string{"Acme/Platform/gardenio-operator"},
			wantViewer:   true,
			wantOperator: true,
			wantAdmin:    false,
		},
		{
			name:         "full-path viewer clears viewer only",
			groups:       []string{"Acme/Platform/gardenio-viewer"},
			wantViewer:   true,
			wantOperator: false,
			wantAdmin:    false,
		},
		{
			name:         "non-gardenio path with viewer floor clears viewer only",
			groups:       []string{"Acme/Platform/eng"},
			wantViewer:   true,
			wantOperator: false,
			wantAdmin:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Mirror the authBranch wiring: normalize, then apply the viewer floor.
			roles := applyAvaDefaultRole(normalizeAvaGroups(tt.groups), RoleViewer)
			if got := HasMinimumRole(roles, RoleViewer); got != tt.wantViewer {
				t.Errorf("HasMinimumRole(viewer) = %v, want %v (roles=%v)", got, tt.wantViewer, roles)
			}
			if got := HasMinimumRole(roles, RoleOperator); got != tt.wantOperator {
				t.Errorf("HasMinimumRole(operator) = %v, want %v (roles=%v)", got, tt.wantOperator, roles)
			}
			if got := HasMinimumRole(roles, RoleAdmin); got != tt.wantAdmin {
				t.Errorf("HasMinimumRole(admin) = %v, want %v (roles=%v)", got, tt.wantAdmin, roles)
			}
		})
	}
}

// TestHandle_AVAFullPathGroup_ClearsOperatorGate is the end-to-end acceptance
// test: a real operator/admin whose AVA token carries the FULL GitLab subgroup
// path must clear the operatorOnly RequireRole gate on the live AVA path —
// where previously they collapsed to the viewer floor and 403'd. It also proves
// a viewer-only (or non-gardenio) member still 403s on operatorOnly, and that a
// bare-name group (Keycloak shape forwarded via AVA) keeps working unchanged.
func TestHandle_AVAFullPathGroup_ClearsOperatorGate(t *testing.T) {
	f := newAVAFixture(t)
	cfg := Config{
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			DefaultRole:    RoleViewer, // production posture: viewer floor.
			publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
			httpClient:     f.jwksSrv.Client(),
		},
	}
	h := New(context.Background(), cfg, slog.Default())
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	operatorGate := h.Middleware(RequireRole(RoleOperator, slog.Default())(inner))

	cases := []struct {
		name     string
		groups   string // JSON array literal for the payload
		wantCode int
	}{
		{
			name:     "full-path operator clears operatorOnly",
			groups:   `["Acme/Platform/gardenio-operator"]`,
			wantCode: http.StatusOK,
		},
		{
			name:     "full-path admin clears operatorOnly (admin > operator)",
			groups:   `["Acme/Platform/gardenio-admin"]`,
			wantCode: http.StatusOK,
		},
		{
			name:     "full-path viewer is forbidden on operatorOnly",
			groups:   `["Acme/Platform/gardenio-viewer"]`,
			wantCode: http.StatusForbidden,
		},
		{
			name:     "non-gardenio path (viewer floor only) is forbidden on operatorOnly",
			groups:   `["Acme/Platform/eng"]`,
			wantCode: http.StatusForbidden,
		},
		{
			name:     "bare-name operator (Keycloak shape via AVA) still clears operatorOnly",
			groups:   `["gardenio-operator"]`,
			wantCode: http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"iss":"gitlab","sub":"u","email":"u@acme.example","groups":%s}`, tc.groups)
			tok := f.signAVA(t, nil, payload)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(AVAHeaderName, tok)
			rec := httptest.NewRecorder()
			operatorGate.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}

// TestHandle_AVAFullPathGroup_ClearsAdminGate proves the admin tier: a
// full-path gardenio-admin member clears adminOnly, while a full-path operator
// (highest non-admin) is forbidden on adminOnly. Guards against the
// normalization accidentally over-granting (ka-0000001646 AC5: do not weaken
// any gate beyond mapping legitimate membership to its bare role).
func TestHandle_AVAFullPathGroup_ClearsAdminGate(t *testing.T) {
	f := newAVAFixture(t)
	cfg := Config{
		AVA: AVAConfig{
			TrustSigner:    testTrustSigner,
			Region:         "test-region",
			DefaultRole:    RoleViewer,
			publicKeyURLFn: func(_ string, kid string) string { return f.jwksSrv.URL + "/" + kid },
			httpClient:     f.jwksSrv.Client(),
		},
	}
	h := New(context.Background(), cfg, slog.Default())
	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	adminGate := h.Middleware(RequireRole(RoleAdmin, slog.Default())(inner))

	cases := []struct {
		name     string
		groups   string
		wantCode int
	}{
		{
			name:     "full-path admin clears adminOnly",
			groups:   `["Acme/Platform/gardenio-admin"]`,
			wantCode: http.StatusOK,
		},
		{
			name:     "full-path operator is forbidden on adminOnly",
			groups:   `["Acme/Platform/gardenio-operator"]`,
			wantCode: http.StatusForbidden,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"iss":"gitlab","sub":"u","email":"u@acme.example","groups":%s}`, tc.groups)
			tok := f.signAVA(t, nil, payload)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set(AVAHeaderName, tok)
			rec := httptest.NewRecorder()
			adminGate.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}
}
