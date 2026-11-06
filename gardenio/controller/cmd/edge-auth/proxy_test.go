package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/edgeauth"
)

// upstreamRecorder is a tiny in-memory upstream that captures the first
// request it sees, including headers and the URL path the shim forwarded.
type upstreamRecorder struct {
	gotMethod  string
	gotPath    string
	gotHeaders http.Header
	gotBody    string
	status     int
	body       string
	// contentType, when non-empty, is set on the upstream response so tests
	// can assert the shim forwards it verbatim (single value) rather than
	// duplicating its own `application/json` onto it.
	contentType string
}

func (u *upstreamRecorder) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u.gotMethod = r.Method
		u.gotPath = r.URL.Path
		u.gotHeaders = r.Header.Clone()
		if r.Body != nil {
			b, _ := io.ReadAll(r.Body)
			u.gotBody = string(b)
		}
		if u.contentType != "" {
			w.Header().Set("Content-Type", u.contentType)
		}
		if u.status != 0 {
			w.WriteHeader(u.status)
		}
		_, _ = io.WriteString(w, u.body)
	}
}

func TestProxy_HappyPath_StampsTrustHeaders(t *testing.T) {
	const key = "test-hmac-key"
	base, upstream, fx := newProxyTestServerWithCapture(t, key)
	defer fx.close()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer good-pat")
	req.Header.Set("X-Auth-Request-Access-Token", "must-not-reach-daemon")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, body)
	}

	if upstream.gotPath != "/v1/beads" {
		t.Errorf("upstream path=%q, want /v1/beads (prefix stripped)", upstream.gotPath)
	}
	if got := upstream.gotHeaders.Get(headerOperator); got != "alice@example.com" {
		t.Errorf("X-Operator=%q, want alice@example.com", got)
	}
	if upstream.gotHeaders.Get(headerAuditID) == "" {
		t.Error("X-Auth-Audit-Id missing")
	}
	if upstream.gotHeaders.Get(headerTimestamp) == "" {
		t.Error("X-Edge-Auth-Ts missing")
	}
	sig := upstream.gotHeaders.Get(headerSignature)
	if sig == "" {
		t.Fatal("X-Edge-Auth-Sig missing")
	}
	// Authorization MUST NOT pass through — daemon authenticates on HMAC.
	if upstream.gotHeaders.Get("Authorization") != "" {
		t.Error("Authorization passed through to upstream")
	}
	if upstream.gotHeaders.Get("X-Auth-Request-Access-Token") != "" {
		t.Error("X-Auth-Request-Access-Token passed through to upstream")
	}

	// Verify the signature matches the documented HMAC-SHA256 message
	// shape: operator\naudit-id\npath\nts.
	op := upstream.gotHeaders.Get(headerOperator)
	auditID := upstream.gotHeaders.Get(headerAuditID)
	ts := upstream.gotHeaders.Get(headerTimestamp)
	expectMsg := op + "\n" + auditID + "\n" + upstream.gotPath + "\n" + ts
	m := hmac.New(sha256.New, []byte(key))
	_, _ = m.Write([]byte(expectMsg))
	expectSig := base64.StdEncoding.EncodeToString(m.Sum(nil))
	if sig != expectSig {
		t.Errorf("signature mismatch: got %s, want %s", sig, expectSig)
	}

	// ts should be the fixed-clock unix seconds we wired in.
	if ts != strconv.FormatInt(1_700_000_000, 10) {
		t.Errorf("ts=%q, want 1700000000", ts)
	}
}

// TestProxy_StripsInboundOperatedBy pins the ka-0000002305 hardening: the shim
// strips a client-supplied X-Operated-By so the daemon derives operator
// attribution only from the HMAC-verified operator, never from a spoofable
// inbound header. Inert today (the edge path reads the resolved context value),
// filed as defense-in-depth against a future downstream that reads it raw.
func TestProxy_StripsInboundOperatedBy(t *testing.T) {
	base, upstream, fx := newProxyTestServerWithCapture(t, "test-hmac-key")
	defer fx.close()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	req.Header.Set("X-Operated-By", "oidc:attacker@evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d, body=%s", resp.StatusCode, body)
	}

	// The spoofed attribution header must not reach the upstream daemon.
	if got := upstream.gotHeaders.Get("X-Operated-By"); got != "" {
		t.Errorf("X-Operated-By=%q passed through to upstream, want it stripped", got)
	}
	// Sanity: the HMAC-verified operator still stamps X-Operator.
	if got := upstream.gotHeaders.Get(headerOperator); got != "alice@example.com" {
		t.Errorf("X-Operator=%q, want alice@example.com", got)
	}
}

// TestProxy_ForwardsUpstreamContentType is the regression guard for
// ka-0000007632: handleProxy sets `Content-Type: application/json` at the top
// of the handler for its deny/error paths. Before the fix it never cleared
// that value on the success path, and because httputil.ReverseProxy copies
// upstream headers with Header.Add (not Set), every proxied response carried
// a two-valued `Content-Type: application/json, <upstream>`. Browsers honour
// the first value, so an HTML upstream (the coopmux SPA served at /mux) was
// rendered as plain text — the "front door returns a bunch of JavaScript"
// report. This pins that the shim forwards the upstream's Content-Type as the
// SINGLE value on the response.
func TestProxy_ForwardsUpstreamContentType(t *testing.T) {
	base, upstream, fx := newProxyTestServerWithCapture(t, "k")
	defer fx.close()
	// Simulate the coopmux dashboard front door: an HTML body served as
	// text/html. The shim must not turn this into application/json.
	upstream.contentType = "text/html; charset=utf-8"
	upstream.body = "<!doctype html><html><body>dashboard</body></html>"

	req, _ := http.NewRequest("GET", base+"/kd/", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200", resp.StatusCode)
	}

	// The Go http client collapses a multi-valued Content-Type into a single
	// comma-joined string, so an un-cleared `application/json` would surface
	// here as "application/json, text/html; charset=utf-8".
	cts := resp.Header.Values("Content-Type")
	if len(cts) != 1 {
		t.Fatalf("Content-Type has %d values %v, want exactly 1 (upstream's)", len(cts), cts)
	}
	if got := cts[0]; got != "text/html; charset=utf-8" {
		t.Errorf("Content-Type=%q, want %q (upstream value, not the shim's application/json)", got, "text/html; charset=utf-8")
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("Content-Type %q still carries the shim's application/json", resp.Header.Get("Content-Type"))
	}
}

func TestProxy_ProxyNotConfigured_503(t *testing.T) {
	// HMAC key empty -> proxy disabled even if validator is good.
	gitlab := newStubGitLab(t)
	defer gitlab.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("upstream must not be reached")
		_, _ = w.Write([]byte("nope"))
	}))
	defer upstream.Close()

	cfg := &config{
		gitlabBaseURL:   gitlab.URL,
		gitlabAuthGroup: "gardenio",
		hmacKey:         "", // disables proxy
		upstreamKbeads:  upstream.URL,
	}
	srv := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status=%d, want 503", resp.StatusCode)
	}
}

func TestProxy_PreservesQueryAndBody(t *testing.T) {
	base, upstream, fx := newProxyTestServerWithCapture(t, "k")
	defer fx.close()

	req, _ := http.NewRequest("POST", base+"/kd/v1/beads?foo=bar", strings.NewReader(`{"x":1}`))
	req.Header.Set("Authorization", "Bearer good-pat")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if upstream.gotPath != "/v1/beads" {
		t.Errorf("path=%q, want /v1/beads", upstream.gotPath)
	}
	if upstream.gotBody != `{"x":1}` {
		t.Errorf("body=%q, want {\"x\":1}", upstream.gotBody)
	}
	if upstream.gotMethod != "POST" {
		t.Errorf("method=%q, want POST", upstream.gotMethod)
	}
	// SetURL inherits Target's query, but the in.URL.RawQuery is preserved
	// by httputil.ProxyRequest.SetURL since 1.20.
}

func TestProxy_DryRun_ShortCircuits(t *testing.T) {
	// Build a proxy server but flip dryRun on. Upstream MUST NOT be hit.
	const key = "test-hmac-key"
	gitlab := newStubGitLab(t)
	defer gitlab.Close()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("upstream must not be reached when dryRun=true")
		_, _ = w.Write([]byte("nope"))
	}))
	defer upstream.Close()

	cfg := &config{
		gitlabBaseURL:   gitlab.URL,
		gitlabAuthGroup: "gardenio",
		hmacKey:         key,
		upstreamKbeads:  upstream.URL,
		dryRun:          true,
	}
	srv := newServer(cfg, "test", slog.New(slog.NewJSONHandler(io.Discard, nil)))
	srv.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest("GET", ts.URL+"/kd/v1/beads", nil)
	req.Header.Set("Authorization", "Bearer good-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d, want 200 (synthetic)", resp.StatusCode)
	}
	if got := resp.Header.Get(headerDryRun); got != "true" {
		t.Errorf("X-Edge-Auth-Dry-Run=%q, want true", got)
	}
	if resp.Header.Get(headerAuditID) == "" {
		t.Error("X-Auth-Audit-Id missing on dry-run response")
	}
	// Even on dry-run, an invalid token must still 401 (we still validate).
	req2, _ := http.NewRequest("GET", ts.URL+"/kd/v1/beads", nil)
	req2.Header.Set("Authorization", "Bearer not-a-real-pat")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("dry-run invalid-token status=%d, want 401", resp2.StatusCode)
	}
}

func TestNewAuditID_ShapeAndUniqueness(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	a := newAuditID(now)
	b := newAuditID(now)
	if len(a) != 26 {
		t.Errorf("audit-id length=%d, want 26", len(a))
	}
	if a == b {
		t.Error("audit-ids collided across two calls")
	}
}

func TestSignEdgeAuth_StableAcrossCalls(t *testing.T) {
	key := []byte("shared")
	s1 := signEdgeAuth(key, "alice@example.com", "ABC", "/v1/beads", "1700000000")
	s2 := signEdgeAuth(key, "alice@example.com", "ABC", "/v1/beads", "1700000000")
	if s1 != s2 {
		t.Errorf("HMAC not deterministic: %s vs %s", s1, s2)
	}
	s3 := signEdgeAuth(key, "alice@example.com", "ABD", "/v1/beads", "1700000000")
	if s1 == s3 {
		t.Error("HMAC unchanged when audit-id changes")
	}
}

func TestSignEdgeAuthV2_BindsMethodModeAndCanonicalRoles(t *testing.T) {
	key := []byte("shared")
	base := signEdgeAuthV2(
		key,
		"alice@example.com",
		"entra",
		"gardenio-operator,gardenio-viewer",
		"ABC",
		http.MethodPost,
		"/v1/beads",
		"1700000000",
	)
	if got := signEdgeAuthV2(
		key,
		"alice@example.com",
		"entra",
		"gardenio-operator,gardenio-viewer",
		"ABC",
		http.MethodGet,
		"/v1/beads",
		"1700000000",
	); got == base {
		t.Fatal("v2 HMAC unchanged when method changes")
	}
	if got := signEdgeAuthV2(
		key,
		"alice@example.com",
		"pat",
		"gardenio-operator,gardenio-viewer",
		"ABC",
		http.MethodPost,
		"/v1/beads",
		"1700000000",
	); got == base {
		t.Fatal("v2 HMAC unchanged when auth mode changes")
	}

	// canonicalGardenioAppRoles is the ONLY role source that upgrades to a v2
	// envelope. It accepts exact bare app-role values and rejects group-path
	// claims (a full path is not a bare app role), which also means an
	// unrelated "*/gardenio-admin" path cannot escalate.
	appRoles := canonicalGardenioAppRoles([]string{
		"unrelated",
		"gardenio-viewer",
		"Acme/Platform/gardenio-operator",
		"gardenio-operator",
		"gardenio-viewer",
		"personal-project/gardenio-admin",
		"Acme/Platform/gardenio-admins",
	})
	if got := strings.Join(appRoles, ","); got != "gardenio-operator,gardenio-viewer" {
		t.Fatalf("canonical app roles=%q, want gardenio-operator,gardenio-viewer", got)
	}
}

func TestSignEdgeAuthV2_CrossLanguageKnownVector(t *testing.T) {
	got := signEdgeAuthV2(
		[]byte("shared-secret"),
		"operator@acme.example",
		"entra",
		"gardenio-operator,gardenio-viewer",
		"audit-123",
		http.MethodPost,
		"/v1/beads",
		"1700000000",
	)
	const want = "KNAEp9aRG8u2NQXkBgNsyD/R3X7l6RdnkI+CFL7mkBY="
	if got != want {
		t.Fatalf("v2 wire vector=%q, want %q", got, want)
	}
}

// TestProxyEnabled_RequiresAuthBackendHmacAndUpstream pins the matrix of
// configurations that flip proxyEnabled. The two auth backends (PAT
// validator vs AVA verifier) are interchangeable — either one is
// sufficient — but the HMAC key and at least one upstream are mandatory.
//
// This matters because AVA-only prod deployments (epic ka-0000005862)
// leave s.validator nil. Before the ka-0000004294 patch the shim 503'd
// every /v1/... request in that posture even when the AVA verifier was
// correctly configured; this test pins the post-patch behaviour so a
// future refactor cannot silently revert it.
func TestProxyEnabled_RequiresAuthBackendHmacAndUpstream(t *testing.T) {
	stubUp := &proxyUpstream{name: "kbeads", prefix: "/kd/"}
	stubAva := &stubVerifier{id: avaIdentity{Email: "x@y"}}
	stubVal := &countingValidator{}

	cases := []struct {
		name      string
		validator edgeauth.Validator
		ava       avaVerifier
		entra     entraVerifier
		hmacKey   string
		upstreams []*proxyUpstream
		want      bool
	}{
		{"all-empty", nil, nil, nil, "", nil, false},
		{"pat-only-no-hmac", stubVal, nil, nil, "", []*proxyUpstream{stubUp}, false},
		{"pat-only-no-upstreams", stubVal, nil, nil, "k", nil, false},
		{"pat-only-complete", stubVal, nil, nil, "k", []*proxyUpstream{stubUp}, true},
		{"ava-only-no-hmac", nil, stubAva, nil, "", []*proxyUpstream{stubUp}, false},
		{"ava-only-no-upstreams", nil, stubAva, nil, "k", nil, false},
		{"ava-only-complete", nil, stubAva, nil, "k", []*proxyUpstream{stubUp}, true},
		{"entra-only-complete", nil, nil, &stubEntraVerifier{isReady: true}, "k", []*proxyUpstream{stubUp}, true},
		{"all-backends-complete", stubVal, stubAva, &stubEntraVerifier{isReady: true}, "k", []*proxyUpstream{stubUp}, true},
		{"no-auth-backend-but-other-wiring-set", nil, nil, nil, "k", []*proxyUpstream{stubUp}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &server{
				cfg:           &config{hmacKey: tc.hmacKey},
				validator:     tc.validator,
				avaVerifier:   tc.ava,
				entraVerifier: tc.entra,
				upstreams:     tc.upstreams,
			}
			if got := s.proxyEnabled(); got != tc.want {
				t.Errorf("proxyEnabled()=%v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildUpstreams_NatsRoutePrecedesKbeads is the regression guard for
// ka-0000006000: the realtime NATS WebSocket channel (/kd/nats) must resolve
// to the NATS upstream, not the kbeads daemon. matchUpstream is
// first-match-by-order, so /kd/nats/ has to be registered BEFORE /kd/.
func TestBuildUpstreams_NatsRoutePrecedesKbeads(t *testing.T) {
	lg := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config{
		upstreamNats:    "http://gardenio-nats:8443",
		upstreamKbeads:  "http://gardenio-beads:8080",
		upstreamCoopmux: "http://gardenio-coopmux:9800",
	}
	ups, err := buildUpstreams(cfg, lg)
	if err != nil {
		t.Fatalf("buildUpstreams: %v", err)
	}
	if len(ups) != 3 {
		t.Fatalf("got %d upstreams, want 3", len(ups))
	}
	// Registration order must put nats before kbeads.
	if ups[0].name != "nats" || ups[1].name != "kbeads" {
		t.Fatalf("order = [%s, %s, ...], want nats before kbeads", ups[0].name, ups[1].name)
	}
	if ups[0].prefix != "/kd/nats/" {
		t.Fatalf("nats prefix = %q, want /kd/nats/", ups[0].prefix)
	}

	s := &server{upstreams: ups}
	for _, tc := range []struct{ path, want string }{
		{"/kd/nats", "nats"},       // exact prefix (trailing slash trimmed)
		{"/kd/nats/sub", "nats"},   // sub-path under nats
		{"/kd/v1/beads", "kbeads"}, // daemon HTTP API still routes to kbeads
		{"/kd/", "kbeads"},         // kd root → kbeads
		{"/mux/sessions", "coopmux"},
	} {
		got := s.matchUpstream(tc.path)
		if got == nil {
			t.Errorf("matchUpstream(%q) = nil, want %s", tc.path, tc.want)
			continue
		}
		if got.name != tc.want {
			t.Errorf("matchUpstream(%q) = %s, want %s", tc.path, got.name, tc.want)
		}
	}
}

// TestBuildUpstreams_NatsUnsetFallsThroughToKbeads documents the pre-fix
// (broken) behaviour: with no NATS upstream URL, /kd/nats is NOT registered
// and falls through to kbeads — exactly the misroute that 502s
// gb agent start --local. Guards against the URL silently going unset.
func TestBuildUpstreams_NatsUnsetFallsThroughToKbeads(t *testing.T) {
	lg := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config{upstreamKbeads: "http://gardenio-beads:8080"}
	ups, err := buildUpstreams(cfg, lg)
	if err != nil {
		t.Fatalf("buildUpstreams: %v", err)
	}
	for _, up := range ups {
		if up.name == "nats" {
			t.Fatal("nats upstream registered despite empty EDGE_AUTH_UPSTREAM_NATS")
		}
	}
	s := &server{upstreams: ups}
	if got := s.matchUpstream("/kd/nats"); got == nil || got.name != "kbeads" {
		t.Fatalf("with nats unset, /kd/nats should fall through to kbeads, got %v", got)
	}
}

func TestNewProxyUpstream_Validation(t *testing.T) {
	lg := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cases := []struct {
		name, prefix, url string
		wantErr           bool
	}{
		{"kbeads", "kd", "http://k", true},         // prefix missing leading slash
		{"kbeads", "/kd", "://nope", true},         // unparseable URL
		{"kbeads", "/kd", "http://", true},         // missing host
		{"kbeads", "/kd/", "http://k:8080", false}, // valid
		{"kbeads", "/kd", "http://k:8080/", false}, // normalised
		{"", "/kd", "http://k", true},              // missing name
	}
	for i, tc := range cases {
		_, err := newProxyUpstream(tc.name, tc.prefix, tc.url, lg)
		if (err != nil) != tc.wantErr {
			t.Errorf("case %d: err=%v wantErr=%v", i, err, tc.wantErr)
		}
	}
}
