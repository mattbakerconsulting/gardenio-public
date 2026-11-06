package tests

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Door D.18a (gd-WTyoLCSMUp), in a real nginx: the edge's api.clonk.tv server
// block, as public-edge.yaml has it, in front of three stubs. A JWT-shaped
// bearer still goes to forwardauth with the land's allowed_groups; a minted
// land token (opaque) goes to the daemon's GET /v1/me with X-Land-Door and
// nothing else that names an identity, and only this land's land principal
// reaches the land's upstream.
//
// nginx comes from $EDGE_NGINX or PATH. The checks.yml helm job installs it,
// and under GitHub Actions a missing nginx fails rather than skips, so the
// test cannot quietly stop running there.

// edgeNginxBinary returns the nginx to run, or skips.
func edgeNginxBinary(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("EDGE_NGINX"); p != "" {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("EDGE_NGINX=%s: %v", p, err)
		}
		return p
	}
	if p, err := exec.LookPath("nginx"); err == nil {
		return p
	}
	if os.Getenv("GITHUB_ACTIONS") == "true" {
		t.Fatal("no nginx: set EDGE_NGINX or put nginx on PATH (checks.yml helm job installs it)")
	}
	t.Skip("no nginx on PATH and EDGE_NGINX unset")
	return ""
}

// edgeStub is one upstream the edge talks to: it keeps every request and
// answers with reply.
type edgeStub struct {
	mu    sync.Mutex
	reqs  []*http.Request
	reply func(r *http.Request) (int, string)
	srv   *httptest.Server
}

func newEdgeStub(t *testing.T, reply func(r *http.Request) (int, string)) *edgeStub {
	t.Helper()
	s := &edgeStub{reply: reply}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, r.Clone(r.Context()))
		reply := s.reply
		s.mu.Unlock()
		code, body := reply(r)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *edgeStub) addr() string { return strings.TrimPrefix(s.srv.URL, "http://") }

func (s *edgeStub) take() []*http.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.reqs
	s.reqs = nil
	return out
}

func (s *edgeStub) setReply(reply func(r *http.Request) (int, string)) {
	s.mu.Lock()
	s.reply = reply
	s.mu.Unlock()
}

// Tokens the stubs know. JWT-shaped ones are forwardauth's, by the groups
// they hold; opaque ones are the daemon's land principals, by land ("" for a
// fleet or bridge token).
var (
	edgeJWTGroups = map[string][]string{
		"eyJhbGciOiJSUzI1NiJ9.eyJhY21lIjoxfQ.c2ln": {"/project-acme"},
		"eyJhbGciOiJSUzI1NiJ9.eyJiZXRhIjoxfQ.c2ln": {"/project-beta"},
	}
	edgeLandTokens = map[string]string{
		"0123456789abcdef0123456789abcdefacme": "acme",
		"0123456789abcdef0123456789abcdefbeta": "beta",
		"0123456789abcdef0123456789abcdefflee": "",
		"0123456789abcdef0123456789abcdefscop": "scopetest",
	}
)

const (
	edgeJWTAcme      = "eyJhbGciOiJSUzI1NiJ9.eyJhY21lIjoxfQ.c2ln"
	edgeLandTokAcme  = "0123456789abcdef0123456789abcdefacme"
	edgeLandTokBeta  = "0123456789abcdef0123456789abcdefbeta"
	edgeLandTokFleet = "0123456789abcdef0123456789abcdefflee"
	// scopetest has a landgate ($land_door_upstream).
	edgeLandTokScopetest = "0123456789abcdef0123456789abcdefscop"
)

// forwardauthReply is oauth2-proxy's /oauth2/auth with
// --skip-jwt-bearer-tokens: 202 for a known JWT in one of allowed_groups,
// 403 for one in none, 401 for anything else.
func forwardauthReply(r *http.Request) (int, string) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	groups, known := edgeJWTGroups[tok]
	if !ok || !known {
		return http.StatusUnauthorized, "unauthorized"
	}
	allowed := strings.Split(r.URL.Query().Get("allowed_groups"), ",")
	for _, g := range groups {
		if slices.Contains(allowed, g) {
			return http.StatusAccepted, ""
		}
	}
	return http.StatusForbidden, "forbidden"
}

// beadsMeReply is the daemon's GET /v1/me behind landDoorMiddleware.
func beadsMeReply(r *http.Request) (int, string) {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/me" {
		return http.StatusMethodNotAllowed, "not GET /v1/me"
	}
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	land, known := edgeLandTokens[tok]
	door := r.Header.Values("X-Land-Door")
	switch {
	case !ok || !known:
		return http.StatusUnauthorized, `{"error":"invalid token"}`
	case len(door) != 1:
		return http.StatusOK, `{"note":"no land door: a door check must always name one"}`
	case land == "":
		return http.StatusForbidden, `{"error":"land door: a land bearer token is required"}`
	case land != door[0]:
		return http.StatusForbidden, `{"error":"land door: credential is for another land"}`
	}
	return http.StatusOK, `{"operator_id":"land:` + land + `","unrestricted":false,"lands":[{"name":"` + land + `","tier":"land"}]}`
}

// edgeHarness is a running nginx serving public-edge.yaml's api.clonk.tv
// block, with forwardauth, the daemon's /v1/me and the land door's upstream
// replaced by stubs.
type edgeHarness struct {
	url                string
	fa, me, land, gate *edgeStub
}

func startEdgeNginx(t *testing.T) *edgeHarness {
	t.Helper()
	bin := edgeNginxBinary(t)
	doc := readEdgeFile(t, "public-edge.yaml")
	_, conf, ok := strings.Cut(doc, "  default.conf: |\n")
	if !ok {
		t.Fatal("public-edge.yaml: no default.conf")
	}
	conf, _, _ = strings.Cut(conf, "\n---")
	// The http-level directives: everything before the first server block,
	// less TLS (the harness speaks plain HTTP) and the cluster resolver (every
	// stub is an IP literal).
	httpLevel := conf[:regexp.MustCompile(`(?m)^    server \{`).FindStringIndex(conf)[0]]
	var kept []string
	for line := range strings.SplitSeq(httpLevel, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ssl_") || strings.HasPrefix(trimmed, "resolver ") {
			continue
		}
		kept = append(kept, line)
	}
	httpLevel = strings.Join(kept, "\n")
	// The harness sends far more than 10 requests a second from one address.
	httpLevel = strings.Replace(httpLevel, "zone=api_per_ip:10m rate=10r/s", "zone=api_per_ip:10m rate=10000r/s", 1)

	h := &edgeHarness{
		fa:   newEdgeStub(t, forwardauthReply),
		me:   newEdgeStub(t, beadsMeReply),
		land: newEdgeStub(t, func(r *http.Request) (int, string) { return http.StatusOK, "upstream:" + r.URL.Path }),
		gate: newEdgeStub(t, func(r *http.Request) (int, string) { return http.StatusOK, "landgate:" + r.URL.Path }),
	}
	port := freePort(t)

	api := edgeServer(t, doc, "api.clonk.tv")
	for _, r := range []struct{ old, new string }{
		{"listen 8443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d;", port)},
		{"http2 on;", ""},
		// The land-token check asks the daemon directly; it gets its own stub
		// so a request to the land's upstream is never mistaken for it.
		{"set $up_beads_me gardenio-beads.gardenio.svc.cluster.local:8080;", "set $up_beads_me " + h.me.addr() + ";"},
		{"gardenio-forwardauth.gardenio.svc.cluster.local:4180", h.fa.addr()},
		{"gardenio-beads.gardenio.svc.cluster.local:8080", h.land.addr()},
	} {
		if !strings.Contains(api, r.old) {
			t.Fatalf("api.clonk.tv block has no %q", r.old)
		}
		api = strings.ReplaceAll(api, r.old, r.new)
	}
	httpLevel = strings.ReplaceAll(httpLevel, "gardenio-beads.gardenio.svc.cluster.local:8080", h.land.addr())
	httpLevel = regexp.MustCompile(`gardenio-landgate-[a-z0-9-]+\.gardenio\.svc\.cluster\.local:8080`).ReplaceAllString(httpLevel, h.gate.addr())

	h.url = runNginx(t, bin, httpLevel, api, port)
	return h
}

// freePort returns a loopback port nothing is listening on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// runNginx starts bin with httpLevel and one server block (which must listen
// on 127.0.0.1:port), waits until it accepts, stops it at cleanup, and returns
// its base URL.
func runNginx(t *testing.T, bin, httpLevel, server string, port int) string {
	t.Helper()
	dir := t.TempDir()
	cfg := fmt.Sprintf(`pid %[1]s/nginx.pid;
error_log %[1]s/error.log notice;
worker_processes 1;
events { worker_connections 64; }
http {
  access_log off;
  client_body_temp_path %[1]s/body;
  proxy_temp_path %[1]s/proxy;
  fastcgi_temp_path %[1]s/fastcgi;
  uwsgi_temp_path %[1]s/uwsgi;
  scgi_temp_path %[1]s/scgi;
%[2]s
    server {%[3]s}
}
`, dir, httpLevel, server)
	cfgPath := filepath.Join(dir, "nginx.conf")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "-p", dir, "-e", filepath.Join(dir, "error.log"), "-c", cfgPath, "-g", "daemon off;")
	out := &strings.Builder{}
	cmd.Stdout, cmd.Stderr = out, out
	// The workers inherit the output pipe; if one outlives the master, Wait
	// must not block on it forever.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// SIGTERM is nginx's fast shutdown: the master stops its workers.
		// SIGKILL would orphan them.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
		if t.Failed() {
			log, _ := os.ReadFile(filepath.Join(dir, "error.log"))
			t.Logf("nginx output:\n%s\nerror.log:\n%s", out, log)
		}
	})
	url := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := net.Dial("tcp", strings.TrimPrefix(url, "http://"))
		if err == nil {
			c.Close()
			break
		}
		if time.Now().After(deadline) || cmd.ProcessState != nil {
			log, _ := os.ReadFile(filepath.Join(dir, "error.log"))
			t.Fatalf("nginx did not start: %v\n%s\n%s", err, out, log)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return url
}

// forged is every identity header a caller might dress a request in; none
// may reach the land-token check or the land's upstream.
var forged = []string{
	"Cookie", "X-Auth-Request-Access-Token", "X-Actor", "X-Fleet", "X-System-Operator",
	"X-Owner-Override", "X-Operated-By", "X-Edge-Auth-Ts", "X-Edge-Auth-Sig", "X-Edge-Auth-Version",
	"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups",
}

func (h *edgeHarness) do(t *testing.T, method, path, authorization string, dress bool) int {
	t.Helper()
	req, err := http.NewRequest(method, h.url+path, strings.NewReader(`{"title":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "api.clonk.tv"
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if dress {
		for _, name := range forged {
			req.Header.Set(name, "forged")
		}
		req.Header.Set("X-Land-Door", "beta")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// The land door's two forwarding locations: the door and the land feed (D.9).
var edgeLandDoorPaths = []struct{ method, path string }{
	{http.MethodGet, "/land/acme/v1/beads?limit=1"},
	{http.MethodPost, "/land/acme/v1/beads"},
	{http.MethodGet, "/land/acme/v1/me"},
	{http.MethodGet, "/land/acme/v1/lands/v1/lands/acme/events"},
}

func TestPublicEdgeNginx_LandTokenAdmittedByTheDaemonCheck(t *testing.T) {
	t.Parallel()
	h := startEdgeNginx(t)
	for _, p := range edgeLandDoorPaths {
		if code := h.do(t, p.method, p.path, "Bearer "+edgeLandTokAcme, true); code != http.StatusOK {
			t.Fatalf("%s %s with acme's land token: got %d, want 200", p.method, p.path, code)
		}
		if n := len(h.fa.take()); n != 0 {
			t.Errorf("%s %s: a land token reached forwardauth %d times", p.method, p.path, n)
		}
		checks := h.me.take()
		if len(checks) != 1 {
			t.Fatalf("%s %s: the daemon was asked %d times, want 1", p.method, p.path, len(checks))
		}
		chk := checks[0]
		if chk.Method != http.MethodGet || chk.URL.RequestURI() != "/v1/me" {
			t.Errorf("%s %s: check = %s %s, want GET /v1/me", p.method, p.path, chk.Method, chk.URL.RequestURI())
		}
		if got := chk.Header.Get("Authorization"); got != "Bearer "+edgeLandTokAcme {
			t.Errorf("%s %s: check Authorization = %q", p.method, p.path, got)
		}
		if got := chk.Header.Values("X-Land-Door"); len(got) != 1 || got[0] != "acme" {
			t.Errorf("%s %s: check X-Land-Door = %q, want [acme] (the path's, not the caller's)", p.method, p.path, got)
		}
		if chk.ContentLength > 0 || chk.Header.Get("Content-Length") != "" {
			t.Errorf("%s %s: the check carried a body", p.method, p.path)
		}
		for name, vs := range chk.Header {
			if slices.Contains(vs, "forged") {
				t.Errorf("%s %s: the check carried %s", p.method, p.path, name)
			}
		}
		reqs := h.land.take()
		if len(reqs) != 1 {
			t.Fatalf("%s %s: the land's upstream saw %d requests, want 1", p.method, p.path, len(reqs))
		}
		r := reqs[0]
		if r.Method != p.method || !strings.HasPrefix(r.URL.Path, "/v1/") {
			t.Errorf("%s %s: forwarded %s %s", p.method, p.path, r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+edgeLandTokAcme {
			t.Errorf("%s %s: forwarded Authorization = %q, want the caller's bearer", p.method, p.path, got)
		}
		if got := r.Header.Values("X-Land-Door"); len(got) != 1 || got[0] != "acme" {
			t.Errorf("%s %s: forwarded X-Land-Door = %q", p.method, p.path, got)
		}
		for name, vs := range r.Header {
			if slices.Contains(vs, "forged") {
				t.Errorf("%s %s: the land's upstream received %s", p.method, p.path, name)
			}
		}
	}
	// Nothing is cached: a revoked token is refused on the very next request.
	h.me.setReply(func(*http.Request) (int, string) { return http.StatusUnauthorized, `{"error":"token revoked"}` })
	if code := h.do(t, http.MethodGet, "/land/acme/v1/beads", "Bearer "+edgeLandTokAcme, false); code != http.StatusUnauthorized {
		t.Errorf("revoked land token: got %d, want 401", code)
	}
	if n := len(h.land.take()); n != 0 {
		t.Errorf("a revoked land token reached the land's upstream")
	}
}

func TestPublicEdgeNginx_LandTokenRefusals(t *testing.T) {
	t.Parallel()
	h := startEdgeNginx(t)
	for _, c := range []struct {
		name, path, authorization string
		want                      int
	}{
		{"another land's land token", "/land/acme/v1/beads", "Bearer " + edgeLandTokBeta, http.StatusForbidden},
		{"acme's land token at beta's door", "/land/beta/v1/beads", "Bearer " + edgeLandTokAcme, http.StatusForbidden},
		{"a fleet or bridge token", "/land/acme/v1/beads", "Bearer " + edgeLandTokFleet, http.StatusForbidden},
		{"an unknown opaque token", "/land/acme/v1/beads", "Bearer garbage-token_0123", http.StatusUnauthorized},
		{"another land's land token on the feed", "/land/acme/v1/lands/v1/lands/acme/events", "Bearer " + edgeLandTokBeta, http.StatusForbidden},
		{"an unknown token on the feed", "/land/acme/v1/lands/v1/lands/acme/events", "Bearer nope", http.StatusUnauthorized},
	} {
		if code := h.do(t, http.MethodGet, c.path, c.authorization, true); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
		if n := len(h.me.take()); n != 1 {
			t.Errorf("%s: the daemon was asked %d times, want 1", c.name, n)
		}
		if n := len(h.fa.take()); n != 0 {
			t.Errorf("%s: an opaque token reached forwardauth", c.name)
		}
		if n := len(h.land.take()); n != 0 {
			t.Errorf("%s: reached the land's upstream", c.name)
		}
	}
	// A daemon that answers anything but 2xx, 401 or 403 admits nobody.
	h.me.setReply(func(*http.Request) (int, string) { return http.StatusInternalServerError, "boom" })
	if code := h.do(t, http.MethodGet, "/land/acme/v1/beads", "Bearer "+edgeLandTokAcme, false); code != http.StatusInternalServerError {
		t.Errorf("daemon 500: got %d, want 500", code)
	}
	if n := len(h.land.take()); n != 0 {
		t.Error("a daemon 500 let the request through")
	}
}

// A JWT, or anything that is not one base64url segment, takes forwardauth
// exactly as before D.18a, with the land's allowed_groups.
func TestPublicEdgeNginx_JWTStillTakesForwardauth(t *testing.T) {
	t.Parallel()
	h := startEdgeNginx(t)
	for _, p := range edgeLandDoorPaths {
		if code := h.do(t, p.method, p.path, "Bearer "+edgeJWTAcme, false); code != http.StatusOK {
			t.Fatalf("%s %s with acme's JWT: got %d, want 200", p.method, p.path, code)
		}
		fa := h.fa.take()
		if len(fa) != 1 {
			t.Fatalf("%s %s: forwardauth saw %d requests, want 1", p.method, p.path, len(fa))
		}
		want := "/project-acme,/project-acme/owners,/project-acme/guests"
		if got := fa[0].URL.Query().Get("allowed_groups"); got != want {
			t.Errorf("%s %s: allowed_groups = %q, want %q", p.method, p.path, got, want)
		}
		if n := len(h.me.take()); n != 0 {
			t.Errorf("%s %s: a JWT reached the land-token check", p.method, p.path)
		}
		h.land.take()
	}
	for _, c := range []struct {
		name, authorization string
		want                int
	}{
		{"another land's JWT", "Bearer eyJhbGciOiJSUzI1NiJ9.eyJiZXRhIjoxfQ.c2ln", http.StatusForbidden},
		{"a malformed JWT-shaped string", "Bearer aaa.bbb.ccc", http.StatusUnauthorized},
		{"a dotted non-JWT", "Bearer a.b", http.StatusUnauthorized},
		{"a lowercase scheme", "bearer " + edgeLandTokAcme, http.StatusUnauthorized},
		{"two spaces", "Bearer  " + edgeLandTokAcme, http.StatusUnauthorized},
		{"base64 padding", "Bearer YWNtZQ==", http.StatusUnauthorized},
		{"basic auth", "Basic YTpi", http.StatusUnauthorized},
		{"no credential", "", http.StatusUnauthorized},
	} {
		if code := h.do(t, http.MethodGet, "/land/acme/v1/beads", c.authorization, true); code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, code, c.want)
		}
		if n := len(h.fa.take()); n != 1 {
			t.Errorf("%s: forwardauth saw %d requests, want 1", c.name, n)
		}
		if n := len(h.me.take()); n != 0 {
			t.Errorf("%s: reached the land-token check", c.name)
		}
		if n := len(h.land.take()); n != 0 {
			t.Errorf("%s: reached the land's upstream", c.name)
		}
	}
}

// The land-token check is internal, and a land with a landgate still has its
// token checked by the daemon itself, not by its landgate.
func TestPublicEdgeNginx_LandTokenCheckIsInternalAndIndependentOfLandgate(t *testing.T) {
	t.Parallel()
	h := startEdgeNginx(t)
	for _, p := range []string{"/oauth2/auth-land-token", "/oauth2/auth-land"} {
		if code := h.do(t, http.MethodGet, p, "Bearer "+edgeLandTokAcme, false); code != http.StatusNotFound {
			t.Errorf("%s from outside: got %d, want 404", p, code)
		}
	}
	if code := h.do(t, http.MethodGet, "/land/scopetest/v1/beads", "Bearer "+edgeLandTokScopetest, false); code != http.StatusOK {
		t.Fatalf("scopetest's land token: got %d, want 200", code)
	}
	if n := len(h.me.take()); n != 1 {
		t.Errorf("the daemon was asked %d times, want 1", n)
	}
	if reqs := h.gate.take(); len(reqs) != 1 || reqs[0].URL.Path != "/v1/beads" {
		t.Errorf("scopetest's landgate saw %d requests, want the one forwarded request", len(reqs))
	}
	if n := len(h.land.take()); n != 0 {
		t.Errorf("scopetest's request went to the daemon's upstream, not its landgate")
	}
}
