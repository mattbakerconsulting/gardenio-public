package main

// Tests for the AVA-authenticated /grafana reverse proxy (Option C, decision
// ka-0000004242; epic ka-0000000746). Covers: identity stamping from the
// verified session, inbound identity-header stripping (anti-spoof), verbatim
// path + method forwarding (Grafana owns the /grafana subpath), degraded-mode
// 503 when no upstream is wired, fail-closed 401 when the request carries no
// identity, upstream 502, and the statusRecorder Hijack delegation that Grafana
// Live's WebSocket upgrade relies on.

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/oidcauth"
)

// newGrafanaProxyMux registers the /grafana routes exactly as server.go does,
// so the exact-path + subtree patterns and their any-method shape are
// exercised.
func newGrafanaProxyMux(srv *Server) *http.ServeMux {
	mux := http.NewServeMux()
	// Mirror server.go: GET-only exact route (methodless would conflict with the
	// GET /{id} catch-all), plus the any-method subtree.
	mux.HandleFunc("GET /grafana", srv.handleGrafanaProxy)
	mux.HandleFunc("/grafana/{path...}", srv.handleGrafanaProxy)
	return mux
}

func newGrafanaProxyServerForTest(upstream string) *Server {
	srv := &Server{logger: slog.Default()}
	srv.SetGrafanaProxy(upstream, slog.Default())
	return srv
}

// fakeGrafana records what path, method, and headers it was hit with and
// returns a canned body so tests can assert on forwarding fidelity.
type fakeGrafana struct {
	gotPath    string
	gotMethod  string
	gotWebauth string
	gotHeaders http.Header
	hits       int
	status     int
	body       string
}

func (f *fakeGrafana) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		f.gotPath = r.URL.Path
		f.gotMethod = r.Method
		f.gotWebauth = r.Header.Get(defaultGrafanaAuthProxyHeader)
		f.gotHeaders = r.Header.Clone()
		status := f.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		body := f.body
		if body == "" {
			body = "grafana-ok"
		}
		_, _ = io.WriteString(w, body)
	}
}

// authedRequest builds a request whose context carries the given verified
// email, mirroring what oidcauth.authBranch injects on the AVA path so the
// handler resolves identity via oidcauth.EmailFromRequest.
func authedRequest(method, target, email string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if email != "" {
		req = req.WithContext(oidcauth.ContextWithEmail(req.Context(), email))
	}
	return req
}

func TestGrafanaProxy_StampsVerifiedIdentity(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	mux := newGrafanaProxyMux(srv)

	req := authedRequest(http.MethodGet, "/grafana/d/abc123?kiosk", "matt@acme.example")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rr.Code, rr.Body.String())
	}
	if fake.hits != 1 {
		t.Fatalf("upstream hits: got %d want 1", fake.hits)
	}
	if fake.gotWebauth != "matt@acme.example" {
		t.Errorf("stamped %s: got %q want matt@acme.example", defaultGrafanaAuthProxyHeader, fake.gotWebauth)
	}
}

// The path is forwarded VERBATIM — Grafana owns the /grafana prefix via
// serve_from_sub_path, so the proxy must NOT strip it (that would 404 the SPA
// assets, ka-0000001440).
func TestGrafanaProxy_ForwardsPathVerbatim(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	mux := newGrafanaProxyMux(srv)

	for _, path := range []string{
		"/grafana",
		"/grafana/",
		"/grafana/public/build/app.js",
		"/grafana/api/dashboards/uid/abc",
		"/grafana/d/xyz/my-dash",
	} {
		req := authedRequest(http.MethodGet, path, "u@acme.example")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status got %d want 200", path, rr.Code)
		}
		if fake.gotPath != path {
			t.Errorf("path forwarding: got %q want %q (prefix must NOT be stripped)", fake.gotPath, path)
		}
	}
}

// Grafana's API uses more than GET; the routes carry no method restriction so
// POST/PUT/DELETE all reach the upstream.
func TestGrafanaProxy_ForwardsAllMethods(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	mux := newGrafanaProxyMux(srv)

	for _, m := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := authedRequest(m, "/grafana/api/dashboards/db", "u@acme.example")
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: status got %d want 200", m, rr.Code)
		}
		if fake.gotMethod != m {
			t.Errorf("method forwarding: got %q want %q", fake.gotMethod, m)
		}
	}
}

// A client-supplied identity/auth-proxy header must be stripped before
// forwarding — the ONLY identity Grafana sees is the one this proxy stamps from
// the verified session. Otherwise a browser could impersonate another user.
func TestGrafanaProxy_StripsSpoofedHeaders(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	mux := newGrafanaProxyMux(srv)

	req := authedRequest(http.MethodGet, "/grafana/", "real@acme.example")
	// Attacker attempts to smuggle a forged identity in every stripped header.
	req.Header.Set(defaultGrafanaAuthProxyHeader, "admin@evil.test")
	req.Header.Set("X-Auth-Request-Email", "admin@evil.test")
	req.Header.Set("X-Auth-Request-User", "admin")
	req.Header.Set("X-Grafana-User", "admin@evil.test")
	req.Header.Set("X-WEBAUTH-GROUPS", "Admins")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	if fake.gotWebauth != "real@acme.example" {
		t.Errorf("identity: upstream saw %q, want real@acme.example (forged header not overwritten)", fake.gotWebauth)
	}
	for _, h := range []string{"X-Auth-Request-Email", "X-Auth-Request-User", "X-Grafana-User", "X-WEBAUTH-GROUPS"} {
		if got := fake.gotHeaders.Get(h); got != "" {
			t.Errorf("spoofed header %s reached upstream: got %q want empty", h, got)
		}
	}
}

// With GRAFANA_AUTH_PROXY_HEADER set (the chart passes grafana.auth.proxy.headerName,
// default X-Auth-Request-Email), the identity is stamped into that header, so it
// matches Grafana's GF_AUTH_PROXY_HEADER_NAME (gd-lHJot24kJu). Client copies of
// the configured name AND the default X-WEBAUTH-USER are still stripped.
func TestGrafanaProxy_StampsConfiguredHeader(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	srv.SetGrafanaAuthProxyHeader(" X-Auth-Request-Email ")
	mux := newGrafanaProxyMux(srv)

	req := authedRequest(http.MethodGet, "/grafana/", "real@acme.example")
	req.Header.Add("X-Auth-Request-Email", "admin@evil.test")
	req.Header.Add("X-Auth-Request-Email", "other@evil.test")
	req.Header.Set(defaultGrafanaAuthProxyHeader, "admin@evil.test")

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	if got := fake.gotHeaders.Values("X-Auth-Request-Email"); len(got) != 1 || got[0] != "real@acme.example" {
		t.Errorf("X-Auth-Request-Email: upstream saw %q, want exactly [real@acme.example]", got)
	}
	if got := fake.gotHeaders.Get(defaultGrafanaAuthProxyHeader); got != "" {
		t.Errorf("spoofed %s reached upstream: got %q want empty", defaultGrafanaAuthProxyHeader, got)
	}
}

// A configured name outside the static strip list is stripped too, and a blank
// configured name falls back to X-WEBAUTH-USER.
func TestGrafanaProxy_AuthHeaderNameCustomAndBlank(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	srv.SetGrafanaAuthProxyHeader("X-Custom-User")
	mux := newGrafanaProxyMux(srv)

	req := authedRequest(http.MethodGet, "/grafana/", "real@acme.example")
	req.Header.Add("X-Custom-User", "admin@evil.test")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if got := fake.gotHeaders.Values("X-Custom-User"); len(got) != 1 || got[0] != "real@acme.example" {
		t.Errorf("X-Custom-User: upstream saw %q, want exactly [real@acme.example]", got)
	}

	srv.SetGrafanaAuthProxyHeader("   ")
	if got := srv.grafanaAuthProxyHeader(); got != defaultGrafanaAuthProxyHeader {
		t.Errorf("blank name: got %q want %q", got, defaultGrafanaAuthProxyHeader)
	}
}

// No verified identity on the request (should not happen behind AVA, but fail
// closed rather than proxy an anonymous or spoofable user into Grafana).
func TestGrafanaProxy_UnauthenticatedFailsClosed(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	mux := newGrafanaProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/grafana/", nil) // no email in ctx or header
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated: got %d want 401", rr.Code)
	}
	if fake.hits != 0 {
		t.Errorf("upstream hit for unauthenticated request; got %d", fake.hits)
	}
}

// SECURITY: an X-Auth-Request-Email header with NO verified context identity
// must NOT authenticate the request. On the new-prod AVA edge that header is
// client-supplied (AVA injects the verified email into the request context, not
// this header, and does not strip a forged copy). If the proxy trusted the
// header a browser could log into Grafana as anyone by sending it. The handler
// resolves identity from the verified context only, so a header-only request
// fails closed with 401 and never reaches Grafana.
func TestGrafanaProxy_ForwardAuthEmailHeaderNotTrusted(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	mux := newGrafanaProxyMux(srv)

	req := httptest.NewRequest(http.MethodGet, "/grafana/", nil)
	req.Header.Set("X-Auth-Request-Email", "spoofed@evil.test") // no verified ctx identity
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Errorf("header-only identity: got %d want 401 (header must not authenticate)", rr.Code)
	}
	if fake.hits != 0 {
		t.Errorf("upstream hit on header-only spoof; got %d", fake.hits)
	}
}

// When a verified context identity IS present, an ATTACKER-supplied
// X-Auth-Request-Email in the same request must not override it — the verified
// context wins and the spoofed header is stripped before forwarding.
func TestGrafanaProxy_ContextIdentityBeatsHeaderSpoof(t *testing.T) {
	fake := &fakeGrafana{}
	upstream := httptest.NewServer(fake.handler())
	defer upstream.Close()

	srv := newGrafanaProxyServerForTest(upstream.URL)
	mux := newGrafanaProxyMux(srv)

	req := authedRequest(http.MethodGet, "/grafana/", "real@acme.example")
	req.Header.Set("X-Auth-Request-Email", "spoofed@evil.test")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200", rr.Code)
	}
	if fake.gotWebauth != "real@acme.example" {
		t.Errorf("identity: upstream saw %q, want real@acme.example (verified ctx must win)", fake.gotWebauth)
	}
	if got := fake.gotHeaders.Get("X-Auth-Request-Email"); got != "" {
		t.Errorf("spoofed X-Auth-Request-Email leaked to upstream: got %q want empty", got)
	}
}

func TestGrafanaProxy_DegradedNoUpstream503(t *testing.T) {
	srv := newGrafanaProxyServerForTest("") // no upstream wired
	mux := newGrafanaProxyMux(srv)

	req := authedRequest(http.MethodGet, "/grafana/", "u@acme.example")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("no upstream: got %d want 503", rr.Code)
	}
}

func TestGrafanaProxy_UpstreamUnreachable502(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed := upstream.URL
	upstream.Close()

	srv := newGrafanaProxyServerForTest(closed)
	mux := newGrafanaProxyMux(srv)

	req := authedRequest(http.MethodGet, "/grafana/", "u@acme.example")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Errorf("unreachable upstream: got %d want 502", rr.Code)
	}
}

// A malformed GRAFANA_UPSTREAM_URL leaves the proxy in degraded mode rather
// than panicking a later request.
func TestSetGrafanaProxy_InvalidURLDegrades(t *testing.T) {
	srv := &Server{logger: slog.Default()}
	srv.SetGrafanaProxy("://not-a-url", slog.Default())
	if srv.grafanaProxy != nil {
		t.Errorf("invalid upstream should leave grafanaProxy nil")
	}

	// A bare path with no scheme/host is also rejected.
	srv.SetGrafanaProxy("gardenio-grafana:3000", slog.Default())
	if srv.grafanaProxy != nil {
		t.Errorf("scheme-less upstream should leave grafanaProxy nil")
	}

	// A well-formed URL wires the proxy and trims a trailing slash.
	srv.SetGrafanaProxy("http://gardenio-grafana:3000/", slog.Default())
	if srv.grafanaProxy == nil {
		t.Fatalf("valid upstream should wire the proxy")
	}
	if strings.HasSuffix(srv.grafanaUpstream, "/") {
		t.Errorf("SetGrafanaProxy left trailing slash: %q", srv.grafanaUpstream)
	}
}

// statusRecorder must delegate Hijack to the underlying writer so Grafana
// Live's WebSocket upgrade (httputil.ReverseProxy → http.Hijacker) works
// through the telemetry wrapper.
func TestStatusRecorder_HijackDelegates(t *testing.T) {
	// A ResponseWriter that IS a Hijacker: the recorder must delegate to it.
	hj := &hijackableRecorder{ResponseRecorder: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: hj}
	if _, ok := any(rec).(http.Hijacker); !ok {
		t.Fatalf("statusRecorder does not implement http.Hijacker")
	}
	if _, _, err := rec.Hijack(); err != nil {
		t.Errorf("Hijack delegation: got err %v want nil", err)
	}
	if !hj.hijacked {
		t.Errorf("underlying Hijacker was not called")
	}

	// A ResponseWriter that is NOT a Hijacker: the recorder returns a clean
	// error rather than panicking.
	plain := &statusRecorder{ResponseWriter: httptest.NewRecorder()}
	if _, _, err := plain.Hijack(); err != http.ErrNotSupported {
		t.Errorf("non-hijacker: got err %v want http.ErrNotSupported", err)
	}
}

// hijackableRecorder is an httptest.ResponseRecorder that also satisfies
// http.Hijacker, recording whether Hijack was invoked.
type hijackableRecorder struct {
	*httptest.ResponseRecorder
	hijacked bool
}

func (h *hijackableRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h.hijacked = true
	c1, _ := net.Pipe()
	return c1, bufio.NewReadWriter(bufio.NewReader(c1), bufio.NewWriter(c1)), nil
}
