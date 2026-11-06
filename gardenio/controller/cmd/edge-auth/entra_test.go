package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/edgeauth"
)

type stubEntraVerifier struct {
	id      entraIdentity
	err     error
	isReady bool
	calls   int
}

func (s *stubEntraVerifier) verify(_ context.Context, _ *http.Request) (entraIdentity, error) {
	s.calls++
	return s.id, s.err
}

func (s *stubEntraVerifier) ready() bool {
	return s.isReady
}

type countingValidator struct {
	id    *edgeauth.Identity
	err   error
	calls int
	token string
}

func (v *countingValidator) Validate(_ context.Context, token string) (*edgeauth.Identity, error) {
	v.calls++
	v.token = token
	return v.id, v.err
}

func jwtWithIssuer(issuer string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"` + issuer + `"}`))
	return header + "." + payload + ".unsigned"
}

func newEntraProxyTestServer(t *testing.T, entra entraVerifier, validator edgeauth.Validator, mutate ...func(*config)) (string, *upstreamRecorder, func()) {
	t.Helper()
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	upstream := &upstreamRecorder{body: `{"ok":true}`}
	upstreamServer := httptest.NewServer(upstream.handler())
	up, err := newProxyUpstream("kbeads", "/kd/", upstreamServer.URL, logger)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config{
		entraIssuerURL: "https://login.microsoftonline.com/tenant/v2.0",
		hmacKey:        "test-hmac-key",
	}
	for _, m := range mutate {
		m(cfg)
	}
	srv := &server{
		cfg:           cfg,
		version:       "test",
		logger:        logger,
		validator:     validator,
		entraVerifier: entra,
		upstreams:     []*proxyUpstream{up},
		now:           func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	srv.audit = newAuditSink(io.Discard, srv.now)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	return ts.URL, upstream, func() {
		ts.Close()
		upstreamServer.Close()
	}
}

func TestProxy_EntraBearerPrecedesPATAndUsesExistingHMACEnvelope(t *testing.T) {
	entra := &stubEntraVerifier{
		id:      entraIdentity{Email: "operator@acme.example", Roles: []string{"gardenio-operator"}},
		isReady: true,
	}
	pat := &countingValidator{id: &edgeauth.Identity{Email: "pat@example.com"}}
	base, upstream, cleanup := newEntraProxyTestServer(t, entra, pat)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, base+"/kd/v1/beads", bytes.NewBufferString(`{"title":"test"}`))
	req.Header.Set("Authorization", "Bearer "+jwtWithIssuer("https://login.microsoftonline.com/tenant/v2.0"))
	req.Header.Set("Cookie", "_gardenio_ziti_oauth2=forged")
	req.Header.Set("X-Auth-Request-Email", "forged@example.com")
	req.Header.Set("X-Auth-Request-Access-Token", "forged")
	req.Header.Set(headerVersion, "99")
	req.Header.Set(headerAuthMode, "forged")
	req.Header.Set(headerRoles, "gardenio-admin")
	req.Header.Set(headerLegacyRoles, "gardenio-admin")
	req.Header.Set(headerSignatureV2, "forged")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, body)
	}
	if entra.calls != 1 {
		t.Fatalf("Entra verifier calls=%d, want 1", entra.calls)
	}
	if pat.calls != 0 {
		t.Fatalf("PAT validator calls=%d, want 0", pat.calls)
	}
	if got := upstream.gotHeaders.Get(headerOperator); got != "operator@acme.example" {
		t.Errorf("X-Operator=%q, want operator@acme.example", got)
	}
	for _, header := range []string{
		"Authorization",
		"Cookie",
		"X-Auth-Request-Email",
		"X-Auth-Request-Access-Token",
	} {
		if got := upstream.gotHeaders.Get(header); got != "" {
			t.Errorf("upstream %s=%q, want stripped", header, got)
		}
	}
	if upstream.gotHeaders.Get(headerSignature) == "" {
		t.Error("legacy X-Edge-Auth-Sig compatibility envelope missing")
	}
	if upstream.gotHeaders.Get(headerSignatureV2) == "" {
		t.Error("v2 X-Edge-Auth-Sig-V2 envelope missing")
	}
	if got := upstream.gotHeaders.Get(headerVersion); got != "2" {
		t.Errorf("%s=%q, want 2", headerVersion, got)
	}
	if got := upstream.gotHeaders.Get(headerAuthMode); got != auditAuthModeEntra {
		t.Errorf("%s=%q, want %q", headerAuthMode, got, auditAuthModeEntra)
	}
	if got := upstream.gotHeaders.Get(headerRoles); got != "gardenio-operator" {
		t.Errorf("%s=%q, want gardenio-operator", headerRoles, got)
	}
	if got := upstream.gotHeaders.Get(headerLegacyRoles); got != "" {
		t.Errorf("%s=%q, want stripped", headerLegacyRoles, got)
	}
	ts := upstream.gotHeaders.Get(headerTimestamp)
	auditID := upstream.gotHeaders.Get(headerAuditID)
	wantLegacySig := signEdgeAuth(
		[]byte("test-hmac-key"),
		"operator@acme.example",
		auditID,
		"/v1/beads",
		ts,
	)
	if got := upstream.gotHeaders.Get(headerSignature); got != wantLegacySig {
		t.Errorf("legacy signature=%q, want %q", got, wantLegacySig)
	}
	wantSig := signEdgeAuthV2(
		[]byte("test-hmac-key"),
		"operator@acme.example",
		auditAuthModeEntra,
		"gardenio-operator",
		auditID,
		http.MethodPost,
		"/v1/beads",
		ts,
	)
	if got := upstream.gotHeaders.Get(headerSignatureV2); got != wantSig {
		t.Errorf("v2 signature=%q, want %q", got, wantSig)
	}
}

// With EDGE_AUTH_HMAC_KEY_V2 set, the v2 envelope is signed with it and the
// v1 compatibility signature keeps the shared key, so a holder of the v1 key
// alone cannot mint a v2 envelope (gd-dMof1PW1oJ).
func TestProxy_V2EnvelopeSignedWithV2Key(t *testing.T) {
	entra := &stubEntraVerifier{
		id:      entraIdentity{Email: "operator@acme.example", Roles: []string{"gardenio-operator"}},
		isReady: true,
	}
	base, upstream, cleanup := newEntraProxyTestServer(t, entra, &countingValidator{},
		func(c *config) { c.hmacKeyV2 = "test-hmac-key-v2" })
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, base+"/kd/v1/beads", bytes.NewBufferString(`{"title":"test"}`))
	req.Header.Set("Authorization", "Bearer "+jwtWithIssuer("https://login.microsoftonline.com/tenant/v2.0"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	ts := upstream.gotHeaders.Get(headerTimestamp)
	auditID := upstream.gotHeaders.Get(headerAuditID)
	v2 := func(key string) string {
		return signEdgeAuthV2([]byte(key), "operator@acme.example", auditAuthModeEntra, "gardenio-operator",
			auditID, http.MethodPost, "/v1/beads", ts)
	}
	if got := upstream.gotHeaders.Get(headerSignatureV2); got != v2("test-hmac-key-v2") || got == v2("test-hmac-key") {
		t.Errorf("v2 signature=%q, want it signed with the v2 key", got)
	}
	if got, want := upstream.gotHeaders.Get(headerSignature),
		signEdgeAuth([]byte("test-hmac-key"), "operator@acme.example", auditID, "/v1/beads", ts); got != want {
		t.Errorf("legacy signature=%q, want it signed with the shared key %q", got, want)
	}
}

func TestProxy_ClaimedEntraFailureNeverFallsThroughToPAT(t *testing.T) {
	entra := &stubEntraVerifier{err: errEntraInvalid, isReady: true}
	pat := &countingValidator{id: &edgeauth.Identity{Email: "pat@example.com"}}
	base, _, cleanup := newEntraProxyTestServer(t, entra, pat)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, base+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+jwtWithIssuer("https://login.microsoftonline.com/tenant/v2.0"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d, want 401", resp.StatusCode)
	}
	if pat.calls != 0 {
		t.Fatalf("PAT validator calls=%d after claimed Entra failure, want 0", pat.calls)
	}
}

func TestProxy_ConfiguredButUnavailableEntraNeverFallsThroughToPAT(t *testing.T) {
	pat := &countingValidator{id: &edgeauth.Identity{Email: "pat@example.com"}}
	base, _, cleanup := newEntraProxyTestServer(t, nil, pat)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, base+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer "+jwtWithIssuer("https://login.microsoftonline.com/tenant/v2.0"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status=%d, want 503", resp.StatusCode)
	}
	if pat.calls != 0 {
		t.Fatalf("PAT validator calls=%d after unavailable Entra configuration, want 0", pat.calls)
	}
}

func TestProxy_OpaqueBearerAndTokenSchemeRemainPAT(t *testing.T) {
	for _, authorization := range []string{"Bearer glpat-example", "Token glpat-example"} {
		t.Run(authorization[:5], func(t *testing.T) {
			entra := &stubEntraVerifier{id: entraIdentity{Email: "entra@acme.example"}, isReady: true}
			pat := &countingValidator{id: &edgeauth.Identity{Email: "pat@example.com", Name: "PAT User"}}
			base, upstream, cleanup := newEntraProxyTestServer(t, entra, pat)
			defer cleanup()

			req, _ := http.NewRequest(http.MethodGet, base+"/kd/v1/beads", nil)
			req.Header.Set("Authorization", authorization)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want 200", resp.StatusCode)
			}
			if entra.calls != 0 {
				t.Fatalf("Entra verifier calls=%d, want 0", entra.calls)
			}
			if pat.calls != 1 || pat.token != "glpat-example" {
				t.Fatalf("PAT calls=%d token=%q, want 1/glpat-example", pat.calls, pat.token)
			}
			if got := upstream.gotHeaders.Get(headerOperator); got != "pat@example.com" {
				t.Errorf("X-Operator=%q, want pat@example.com", got)
			}
		})
	}
}

func TestEntraForwardauth_Dispositions(t *testing.T) {
	tests := []struct {
		name string
		v    entraVerifier
		want int
	}{
		{name: "not configured", v: nil, want: http.StatusNotFound},
		{name: "valid", v: &stubEntraVerifier{id: entraIdentity{Email: "operator@acme.example"}, isReady: true}, want: http.StatusAccepted},
		{name: "missing", v: &stubEntraVerifier{err: errEntraMissing, isReady: true}, want: http.StatusUnauthorized},
		{name: "invalid", v: &stubEntraVerifier{err: errEntraInvalid, isReady: true}, want: http.StatusUnauthorized},
		{name: "forbidden", v: &stubEntraVerifier{err: errEntraForbidden, isReady: true}, want: http.StatusForbidden},
		{name: "unavailable", v: &stubEntraVerifier{err: errEntraUnavailable}, want: http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
			srv := &server{
				cfg:           &config{},
				version:       "test",
				logger:        logger,
				entraVerifier: tt.v,
				now:           time.Now,
			}
			srv.audit = newAuditSink(io.Discard, time.Now)
			mux := http.NewServeMux()
			srv.RegisterRoutes(mux)

			req := httptest.NewRequest(http.MethodGet, "/forwardauth/entra", nil)
			req.Header.Set("Authorization", "Bearer token")
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tt.want {
				t.Errorf("status=%d, want %d (body=%q)", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusAccepted && rec.Header().Get("Authorization") != "" {
				t.Error("ForwardAuth response must not replace the original Authorization header")
			}
		})
	}
}

func TestReady_EntraOnlyTracksProviderReadiness(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ready bool
		want  bool
	}{
		{name: "pending", ready: false, want: false},
		{name: "ready", ready: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &server{
				cfg: &config{
					patDisabled:    true,
					entraIssuerURL: "https://login.microsoftonline.com/tenant/v2.0",
				},
				entraVerifier: &stubEntraVerifier{
					isReady: tc.ready,
				},
			}
			got, _ := srv.ready()
			if got != tc.want {
				t.Errorf("ready()=%v, want %v", got, tc.want)
			}
		})
	}
}

func TestReady_EntraReadinessGatesConfiguredPAT(t *testing.T) {
	srv := &server{
		cfg:           &config{entraIssuerURL: "https://login.microsoftonline.com/tenant/v2.0"},
		validator:     &countingValidator{},
		entraVerifier: &stubEntraVerifier{isReady: false},
	}
	if got, _ := srv.ready(); got {
		t.Error("ready()=true while configured Entra discovery is pending")
	}

	srv.entraVerifier = nil
	if got, _ := srv.ready(); got {
		t.Error("ready()=true while configured Entra verifier construction failed")
	}
}
