package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testCookieName = "_gardenio_oauth2"

// fakeOauth2Proxy stands in for oauth2-proxy's /oauth2/auth subrequest
// endpoint. It accepts the session named testCookieName with value
// "valid-session" and 202s with the X-Auth-Request-* headers oauth2-proxy
// emits; anything else 401s. status/email overrides let individual tests
// drive the upstream-error and no-email branches.
type fakeOauth2Proxy struct {
	email      string
	groups     string
	forceCode  int    // when non-zero, always reply with this status
	gotCookie  string // captures the Cookie header the shim forwarded
	gotAuthHdr string // captures any Authorization header (must stay empty)
}

func (f *fakeOauth2Proxy) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.gotCookie = r.Header.Get("Cookie")
		f.gotAuthHdr = r.Header.Get("Authorization")
		if f.forceCode != 0 {
			w.WriteHeader(f.forceCode)
			return
		}
		if sessionCookieValue(r, testCookieName) != "valid-session" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		email := f.email
		if email == "" && f.email != "\x00" {
			email = "cookie-user@acme.example"
		}
		// f.email == "\x00" is the sentinel for "2xx but no email".
		if f.email != "\x00" {
			w.Header().Set("X-Auth-Request-Email", email)
			w.Header().Set("X-Auth-Request-User", email)
		}
		if f.groups != "" {
			w.Header().Set("X-Auth-Request-Groups", f.groups)
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// sessionCookieValue mirrors oauth2-proxy's base-cookie-first, then numbered
// chunk reconstruction closely enough for the verifier integration tests.
func sessionCookieValue(r *http.Request, name string) string {
	if c, err := r.Cookie(name); err == nil {
		return c.Value
	}
	var value strings.Builder
	for i := 0; ; i++ {
		c, err := r.Cookie(name + "_" + strconv.Itoa(i))
		if err != nil {
			break
		}
		value.WriteString(c.Value)
	}
	return value.String()
}

func newCookieVerifier(t *testing.T, authURL string) cookieVerifier {
	t.Helper()
	return newOauth2ProxyVerifier(&config{
		oauth2ProxyAuthURL:    authURL,
		oauth2ProxyCookieName: testCookieName,
	}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
}

func reqWithCookie(name, value string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/kd/v1/health", nil)
	if name != "" {
		r.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	return r
}

func TestOauth2ProxyVerifier_ValidSession(t *testing.T) {
	fake := &fakeOauth2Proxy{groups: "gardenio,viewers"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	v := newCookieVerifier(t, srv.URL)

	id, err := v.verify(context.Background(), reqWithCookie(testCookieName, "valid-session"))
	if err != nil {
		t.Fatalf("verify: unexpected error: %v", err)
	}
	if id.Email != "cookie-user@acme.example" {
		t.Errorf("Email=%q, want cookie-user@acme.example", id.Email)
	}
	if len(id.Groups) != 2 || id.Groups[0] != "gardenio" || id.Groups[1] != "viewers" {
		t.Errorf("Groups=%v, want [gardenio viewers]", id.Groups)
	}
	// The shim must forward the session cookie and never an Authorization header.
	if fake.gotCookie == "" {
		t.Error("oauth2-proxy did not receive the forwarded Cookie header")
	}
	if fake.gotAuthHdr != "" {
		t.Errorf("oauth2-proxy received Authorization=%q, want empty", fake.gotAuthHdr)
	}
}

func TestOauth2ProxyVerifier_ChunkedSession(t *testing.T) {
	fake := &fakeOauth2Proxy{groups: "gardenio-operator"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	v := newCookieVerifier(t, srv.URL)

	req := reqWithCookie(testCookieName+"_0", "valid-")
	req.AddCookie(&http.Cookie{Name: testCookieName + "_1", Value: "session"})
	id, err := v.verify(context.Background(), req)
	if err != nil {
		t.Fatalf("verify chunked session: unexpected error: %v", err)
	}
	if id.Email != "cookie-user@acme.example" {
		t.Errorf("Email=%q, want cookie-user@acme.example", id.Email)
	}
	for _, want := range []string{testCookieName + "_0=valid-", testCookieName + "_1=session"} {
		if !strings.Contains(fake.gotCookie, want) {
			t.Errorf("forwarded Cookie header %q missing %q", fake.gotCookie, want)
		}
	}
}

func TestHasCookie_NumberedChunksOnly(t *testing.T) {
	tests := []struct {
		name       string
		cookieName string
		want       bool
	}{
		{name: "base", cookieName: testCookieName, want: true},
		{name: "first chunk", cookieName: testCookieName + "_0", want: true},
		{name: "later chunk", cookieName: testCookieName + "_12", want: true},
		{name: "empty suffix", cookieName: testCookieName + "_"},
		{name: "non-numeric suffix", cookieName: testCookieName + "_x"},
		{name: "negative suffix", cookieName: testCookieName + "_-1"},
		{name: "lookalike prefix", cookieName: testCookieName + "2_0"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := reqWithCookie(tc.cookieName, "value")
			if got := hasCookie(req, testCookieName); got != tc.want {
				t.Errorf("hasCookie(%q)=%t, want %t", tc.cookieName, got, tc.want)
			}
		})
	}
}

func TestOauth2ProxyVerifier_NoCookie_Missing(t *testing.T) {
	// No subrequest should be made when the session cookie is absent.
	var hit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	v := newCookieVerifier(t, srv.URL)

	_, err := v.verify(context.Background(), reqWithCookie("", ""))
	if !errors.Is(err, errCookieMissing) {
		t.Errorf("err=%v, want errCookieMissing", err)
	}
	if hit {
		t.Error("oauth2-proxy was called despite no session cookie (wasteful network hop)")
	}
}

func TestOauth2ProxyVerifier_RejectedSession_Invalid(t *testing.T) {
	fake := &fakeOauth2Proxy{}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	v := newCookieVerifier(t, srv.URL)

	// Cookie present but value does not match → oauth2-proxy 401.
	_, err := v.verify(context.Background(), reqWithCookie(testCookieName, "stale-or-forged"))
	if !errors.Is(err, errCookieInvalid) {
		t.Errorf("err=%v, want errCookieInvalid", err)
	}
}

func TestOauth2ProxyVerifier_2xxNoEmail_Invalid(t *testing.T) {
	// A 202 with no X-Auth-Request-Email must fail closed, not stamp an empty
	// operator the daemon would attribute to an unauthenticated principal.
	fake := &fakeOauth2Proxy{email: "\x00"}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	v := newCookieVerifier(t, srv.URL)

	_, err := v.verify(context.Background(), reqWithCookie(testCookieName, "valid-session"))
	if !errors.Is(err, errCookieInvalid) {
		t.Errorf("err=%v, want errCookieInvalid (empty-email guard)", err)
	}
}

func TestOauth2ProxyVerifier_Upstream5xx_Retryable(t *testing.T) {
	fake := &fakeOauth2Proxy{forceCode: http.StatusInternalServerError}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()
	v := newCookieVerifier(t, srv.URL)

	_, err := v.verify(context.Background(), reqWithCookie(testCookieName, "valid-session"))
	if !errors.Is(err, errCookieUpstream) {
		t.Errorf("err=%v, want errCookieUpstream", err)
	}
}

func TestOauth2ProxyVerifier_Unreachable_Retryable(t *testing.T) {
	// Point at a closed port: the Do() call fails outright.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	v := newCookieVerifier(t, url)

	_, err := v.verify(context.Background(), reqWithCookie(testCookieName, "valid-session"))
	if !errors.Is(err, errCookieUpstream) {
		t.Errorf("err=%v, want errCookieUpstream", err)
	}
}

func TestOauth2ProxyVerifier_DisabledWhenURLEmpty(t *testing.T) {
	if v := newOauth2ProxyVerifier(&config{}, slog.New(slog.NewJSONHandler(io.Discard, nil))); v != nil {
		t.Error("newOauth2ProxyVerifier returned non-nil with empty oauth2ProxyAuthURL (cookie mode must ship dark)")
	}
}

// ── Integration through handleProxy ───────────────────────────────

// newCookieProxyShim wires a fake oauth2-proxy + a capturing upstream into a
// real edge-auth server with cookie mode enabled, returning the shim URL and
// the upstream recorder so tests can assert the HMAC envelope end-to-end.
func newCookieProxyShim(t *testing.T, fake *fakeOauth2Proxy) (string, *upstreamRecorder, func()) {
	return newCookieProxyShimWithRoleSource(t, fake, oauth2ProxyRoleSourceProviderGroups)
}

func newCookieProxyShimWithRoleSource(
	t *testing.T,
	fake *fakeOauth2Proxy,
	roleSource string,
) (string, *upstreamRecorder, func()) {
	t.Helper()
	oauth2 := httptest.NewServer(fake.handler())
	upstream := &upstreamRecorder{body: `{"ok":true}`}
	upstreamServer := httptest.NewServer(upstream.handler())

	cfg := &config{
		hmacKey:               "test-hmac-key",
		upstreamKbeads:        upstreamServer.URL,
		oauth2ProxyAuthURL:    oauth2.URL,
		oauth2ProxyCookieName: testCookieName,
		oauth2ProxyRoleSource: roleSource,
	}
	srv := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)

	return ts.URL, upstream, func() {
		ts.Close()
		upstreamServer.Close()
		oauth2.Close()
	}
}

func TestCookieProxy_ValidCookie_StampsEnvelopeAndReachesUpstream(t *testing.T) {
	fake := &fakeOauth2Proxy{}
	shimURL, upstream, cleanup := newCookieProxyShim(t, fake)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, shimURL+"/kd/v1/health", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: "valid-session"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (cookie should reach upstream)", resp.StatusCode)
	}

	// The upstream must receive the HMAC envelope, with the cookie identity as
	// X-Operator — and NOT the raw browser cookie or any X-Auth-Request-*.
	if got := upstream.gotHeaders.Get(headerOperator); got != "cookie-user@acme.example" {
		t.Errorf("upstream X-Operator=%q, want cookie-user@acme.example", got)
	}
	if upstream.gotHeaders.Get(headerSignature) == "" {
		t.Error("upstream did not receive X-Edge-Auth-Sig — cookie path must stamp the envelope")
	}
	if got := upstream.gotHeaders.Get("Cookie"); got != "" {
		t.Errorf("upstream received Cookie=%q, want it stripped", got)
	}
	if got := upstream.gotHeaders.Get("X-Auth-Request-Email"); got != "" {
		t.Errorf("upstream received X-Auth-Request-Email=%q, want it stripped", got)
	}
}

// TestCookieProxy_DefaultProviderGroups_EmitsV1Envelope pins operator decision
// ka-0000001641 ("additive"): a generic provider-group cookie (the Helm chart
// default role source) is a pre-existing path this MR does not change, so it
// signs a v1 envelope with NO roles even when the verified session carries
// recognized gardenio groups. Only an explicitly-configured entra-app-roles
// source (below) upgrades to v2. Downstream grants a verified v1 envelope its
// historical authority, so default cookie deployments keep working with
// enforce=true (ka-0000006523).
func TestCookieProxy_DefaultProviderGroups_EmitsV1Envelope(t *testing.T) {
	fake := &fakeOauth2Proxy{
		groups: "Acme/Platform/gardenio-admin, Acme/Platform/gardenio-viewer, unrelated",
	}
	shimURL, upstream, cleanup := newCookieProxyShim(t, fake)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, shimURL+"/kd/v1/beads", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: "valid-session"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	// v1 envelope: v1 signature present; no version, roles, or v2 signature.
	if got := upstream.gotHeaders.Get(headerSignature); got == "" {
		t.Error("X-Edge-Auth-Sig missing — cookie path must still stamp the v1 envelope")
	}
	if got := upstream.gotHeaders.Get(headerVersion); got != "" {
		t.Errorf("%s=%q, want empty (default provider-groups stays v1)", headerVersion, got)
	}
	if got := upstream.gotHeaders.Get(headerRoles); got != "" {
		t.Errorf("%s=%q, want empty (default provider-groups stays v1)", headerRoles, got)
	}
	if got := upstream.gotHeaders.Get(headerSignatureV2); got != "" {
		t.Errorf("%s=%q, want empty (default provider-groups must not sign v2)", headerSignatureV2, got)
	}
}

func TestCookieProxy_EntraAppRolesAcceptsBareOperatorWithoutPathEscalation(t *testing.T) {
	fake := &fakeOauth2Proxy{
		groups: "gardenio-operator, personal-project/gardenio-admin",
	}
	shimURL, upstream, cleanup := newCookieProxyShimWithRoleSource(
		t,
		fake,
		oauth2ProxyRoleSourceEntraAppRoles,
	)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodPost, shimURL+"/kd/v1/beads", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName + "_0", Value: "valid-"})
	req.AddCookie(&http.Cookie{Name: testCookieName + "_1", Value: "session"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}
	if got := upstream.gotHeaders.Get(headerVersion); got != "2" {
		t.Errorf("%s=%q, want 2", headerVersion, got)
	}
	if got := upstream.gotHeaders.Get(headerRoles); got != "gardenio-operator" {
		t.Errorf("%s=%q, want gardenio-operator", headerRoles, got)
	}
	if upstream.gotHeaders.Get(headerSignatureV2) == "" {
		t.Error("X-Edge-Auth-Sig-V2 missing for exact Entra app role")
	}
}

func TestCanonicalCookieGardenioRoles_UnknownSourceFailsClosed(t *testing.T) {
	got := canonicalCookieGardenioRoles(
		[]string{"gardenio-admin", "Acme/Platform/gardenio-admin"},
		"unknown-source",
	)
	if len(got) != 0 {
		t.Errorf("roles=%v, want none for unknown role source", got)
	}
}

func TestCookieProxy_InvalidCookie_401(t *testing.T) {
	fake := &fakeOauth2Proxy{}
	shimURL, upstream, cleanup := newCookieProxyShim(t, fake)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, shimURL+"/kd/v1/health", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: "forged"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401 for a rejected session", resp.StatusCode)
	}
	if upstream.gotMethod != "" {
		t.Error("upstream was reached despite an invalid cookie")
	}
}

func TestCookieProxy_UpstreamDown_502(t *testing.T) {
	fake := &fakeOauth2Proxy{forceCode: http.StatusBadGateway}
	shimURL, _, cleanup := newCookieProxyShim(t, fake)
	defer cleanup()

	req, _ := http.NewRequest(http.MethodGet, shimURL+"/kd/v1/health", nil)
	req.AddCookie(&http.Cookie{Name: testCookieName, Value: "valid-session"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status=%d, want 502 when oauth2-proxy is unreachable (retryable)", resp.StatusCode)
	}
}

func TestCookieProxy_NoCredential_401MissingToken(t *testing.T) {
	fake := &fakeOauth2Proxy{}
	shimURL, _, cleanup := newCookieProxyShim(t, fake)
	defer cleanup()

	// No cookie, no Bearer → the final default branch (missing_token).
	resp, err := http.Get(shimURL + "/kd/v1/health")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401 missing_token", resp.StatusCode)
	}
}
