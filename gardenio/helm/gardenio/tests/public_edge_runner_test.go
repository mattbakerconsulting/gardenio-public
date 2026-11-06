package tests

// The public runner listener on the k3s public edge (local-agents spec §4.3,
// plan gd-fwFefkDP4H, gd-T4eL6P5OtC): runners.clonk.tv in
// gardenio-ziti/public-edge.yaml, the nginx counterpart of
// templates/beads/runner-ingress.yaml. Held to kbeads' runnerAllowedPatterns
// (runner_auth.go) like the Traefik route is (runner_ingress_test.go).

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const runnerEdgeHost = "runners.clonk.tv"

// runnerEdgeRouteRegex is the $runner_route map entry for one
// runnerAllowedPatterns entry: METHOD, a space and the path, each mux
// wildcard one path segment and a {name...} wildcard the rest of the path.
func runnerEdgeRouteRegex(pattern string) string {
	method, path, _ := strings.Cut(pattern, " ")
	var b strings.Builder
	last := 0
	for _, loc := range runnerWildcard.FindAllStringIndex(path, -1) {
		b.WriteString(regexp.QuoteMeta(path[last:loc[0]]))
		if strings.HasSuffix(path[loc[0]:loc[1]], "...}") {
			b.WriteString(".+")
		} else {
			b.WriteString("[^/]+")
		}
		last = loc[1]
	}
	b.WriteString(regexp.QuoteMeta(path[last:]))
	return "^" + method + " " + b.String() + "$"
}

// runnerWildcard is a Go 1.22 mux wildcard, {name} or {name...}.
var runnerWildcard = regexp.MustCompile(`\{[^}]+\}`)

// runnerSampleURI is a request path a runnerAllowedPatterns path matches.
func runnerSampleURI(path string) string { return runnerWildcard.ReplaceAllString(path, "gd-1") }

// runnerEdgeRouteMap returns the regexes of the $runner_route map, which must
// default to 0 and set 1 on each entry.
func runnerEdgeRouteMap(t *testing.T, doc string) []string {
	t.Helper()
	m := nginxBlock(doc, `map "$request_method $uri" $runner_route`)
	if m == "" {
		t.Fatal(`public-edge.yaml: no map "$request_method $uri" $runner_route`)
	}
	var res []string
	for line := range strings.SplitSeq(m, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || line == "default 0;" {
			continue
		}
		e := regexp.MustCompile(`^"~(\^[^"]+\$)" 1;$`).FindStringSubmatch(line)
		if e == nil {
			t.Fatalf("$runner_route: unexpected line %q", line)
		}
		res = append(res, e[1])
	}
	if !strings.Contains(m, "default 0;") {
		t.Error("$runner_route must default to 0 (closed)")
	}
	return res
}

// The edge's allowlist is the daemon's, entry for entry.
func TestPublicEdge_RunnerRouteMapIsServerAllowlist(t *testing.T) {
	t.Parallel()
	got := runnerEdgeRouteMap(t, readEdgeFile(t, "public-edge.yaml"))
	var want []string
	for _, p := range serverRunnerPatterns(t) {
		want = append(want, runnerEdgeRouteRegex(p))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("$runner_route drifted from kbeads runnerAllowedPatterns\n got: %q\nwant: %q", got, want)
	}
	if r := runnerEdgeRouteRegex("POST /v1/runners/{id}/heartbeat"); r != `^POST /v1/runners/[^/]+/heartbeat$` {
		t.Errorf("runnerEdgeRouteRegex = %s", r)
	}
	if r := runnerEdgeRouteRegex("GET /v1/configs/{key...}"); r != `^GET /v1/configs/.+$` {
		t.Errorf("runnerEdgeRouteRegex = %s", r)
	}
}

// Only a runner bearer counts: the prefix is the daemon's runnerTokenPrefix.
func TestPublicEdge_RunnerBearerShape(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../kbeads/internal/server/runners.go")
	if err != nil {
		t.Fatal(err)
	}
	pm := regexp.MustCompile(`runnerTokenPrefix\s*=\s*"([^"]+)"`).FindStringSubmatch(string(raw))
	if pm == nil {
		t.Fatal("runners.go: no runnerTokenPrefix")
	}
	m := nginxBlock(readEdgeFile(t, "public-edge.yaml"), "map $http_authorization $runner_bearer")
	want := "default 0;\n      \"~^Bearer " + pm[1] + "\" 1;"
	if strings.TrimSpace(m) != want {
		t.Fatalf("$runner_bearer: want exactly\n%s\ngot\n%s", want, strings.TrimSpace(m))
	}
	re := regexp.MustCompile(`^Bearer ` + pm[1])
	for h, want := range map[string]bool{
		"Bearer gr_0123456789abcdef":                  true,
		"Bearer gr_":                                  true,
		"Bearer eyJhbGciOiJSUzI1NiJ9.eyJ4IjoxfQ.c2ln": false,
		"Bearer 0123456789abcdef0123456789abcdef":     false,
		"Bearer  gr_0123":                             false,
		"bearer gr_0123":                              false,
		"Basic Z3JfMDEyMw==":                          false,
		"":                                            false,
	} {
		if got := re.MatchString(h); got != want {
			t.Errorf("$runner_bearer(%q) = %v, want %v", h, got, want)
		}
	}
}

// The host is served, certified and redirected from port 80 like every other.
func TestPublicEdge_RunnerHostListed(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	edgeServer(t, doc, runnerEdgeHost)
	dns := regexp.MustCompile(`dnsNames: \[([^\]]+)\]`).FindStringSubmatch(doc)
	if dns == nil || !slices.Contains(strings.Split(dns[1], ", "), runnerEdgeHost) {
		t.Errorf("%s missing from the edge Certificate's dnsNames", runnerEdgeHost)
	}
	redirect := regexp.MustCompile(`listen 8080;\s*\n\s*server_name ([^;]+);`).FindStringSubmatch(doc)
	if redirect == nil || !slices.Contains(strings.Fields(redirect[1]), runnerEdgeHost) {
		t.Errorf("%s missing from the port-80 redirect", runnerEdgeHost)
	}
	for _, zone := range []string{
		"limit_req_zone $binary_remote_addr zone=runner_per_ip:10m rate=20r/s;",
		"limit_req_zone $binary_remote_addr zone=runner_register_per_ip:10m rate=1r/m;",
	} {
		if !strings.Contains(doc, zone) {
			t.Errorf("public-edge.yaml missing %q", zone)
		}
	}
}

// The server block: three locations, no sign-in check, no redirect. Each
// forwarding location 404s what it does not serve before the rate limit and
// the proxy, stamps X-Gardenio-Listener, passes the caller's bearer and
// blanks every other header that names an identity.
func TestPublicEdge_RunnerServer(t *testing.T) {
	t.Parallel()
	srv := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), runnerEdgeHost)
	var heads []string
	for _, m := range regexp.MustCompile(`(?m)^      (location [^{]+) \{`).FindAllStringSubmatch(srv, -1) {
		heads = append(heads, m[1])
	}
	if want := []string{"location = /v1/runners/register", "location ^~ /v1/", "location /"}; !slices.Equal(heads, want) {
		t.Errorf("runners.clonk.tv locations = %q, want %q", heads, want)
	}
	for _, bad := range []string{"auth_request", "error_page", "allowed_groups", "return 30"} {
		if strings.Contains(srv, bad) {
			t.Errorf("runners.clonk.tv must not carry %q: the runner token is the only credential", bad)
		}
	}
	if !strings.Contains(srv, `if ($public_edge_bad_path) { return 400; }`) {
		t.Error("runners.clonk.tv must refuse traversal and ambiguous encodings like every edge host")
	}
	if got := strings.TrimSpace(nginxBlock(srv, "location /")); got != "return 404;" {
		t.Errorf("runners.clonk.tv location / = %q, want return 404;", got)
	}

	for head, order := range map[string][]string{
		"location = /v1/runners/register": {
			"if ($request_method != POST) { return 404; }",
			"limit_req zone=runner_register_per_ip burst=9 nodelay;",
			"proxy_pass http://$up_runner_beads;",
		},
		"location ^~ /v1/": {
			"if ($runner_route = 0) { return 404; }",
			"if ($runner_bearer = 0) { return 404; }",
			"limit_req zone=runner_per_ip burst=40 nodelay;",
			"proxy_pass http://$up_runner_beads;",
		},
	} {
		body := nginxBlock(srv, head)
		last := -1
		for _, line := range order {
			i := strings.Index(body, line)
			if i < 0 {
				t.Errorf("%s missing %q", head, line)
				continue
			}
			if i < last {
				t.Errorf("%s: %q is out of order (want %v)", head, line, order)
			}
			last = i
		}
		for _, line := range []string{
			"set $up_runner_beads gardenio-beads.gardenio.svc.cluster.local:8080;",
			"limit_req_status 429;",
			"proxy_set_header X-Forwarded-For   $remote_addr;",
		} {
			if !strings.Contains(body, line) {
				t.Errorf("%s missing %q", head, line)
			}
		}
		if !regexp.MustCompile(`(?m)^\s*proxy_set_header X-Gardenio-Listener runner;`).MatchString(body) {
			t.Errorf("%s must set X-Gardenio-Listener: runner", head)
		}
		if regexp.MustCompile(`(?m)^\s*proxy_set_header Authorization\s`).MatchString(body) {
			t.Errorf("%s must pass the runner's bearer through to the daemon", head)
		}
		for _, h := range append(slices.Clone(forged), "X-Land-Door") {
			if !regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(h) + `\s+"";`).MatchString(body) {
				t.Errorf("%s does not blank %s", head, h)
			}
		}
	}
	if !strings.Contains(nginxBlock(srv, "location ^~ /v1/"), "proxy_buffering off;") {
		t.Error("the runner routes carry the event stream (SSE): proxy_buffering must be off")
	}
}

// Which requests runners.clonk.tv forwards, by nginx's location choice (an
// exact match, then the ^~ prefix, then /) and the two maps.
func TestPublicEdge_RunnerRoutes(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	var allow []*regexp.Regexp
	for _, r := range runnerEdgeRouteMap(t, doc) {
		allow = append(allow, regexp.MustCompile(r))
	}
	bearer := regexp.MustCompile(`^Bearer gr_`)
	forwards := func(method, uri, authorization string) bool {
		switch {
		case uri == "/v1/runners/register":
			return method == http.MethodPost
		case strings.HasPrefix(uri, "/v1/"):
			return bearer.MatchString(authorization) &&
				slices.ContainsFunc(allow, func(re *regexp.Regexp) bool { return re.MatchString(method + " " + uri) })
		}
		return false
	}
	const tok = "Bearer gr_0123456789abcdef"
	for _, p := range serverRunnerPatterns(t) {
		method, path, _ := strings.Cut(p, " ")
		uri := runnerSampleURI(path)
		if !forwards(method, uri, tok) {
			t.Errorf("%s %s with a runner token is not forwarded", method, uri)
		}
		if forwards(method, uri, "Bearer eyJhbGciOiJSUzI1NiJ9.eyJ4IjoxfQ.c2ln") || forwards(method, uri, "") {
			t.Errorf("%s %s is forwarded without a runner token", method, uri)
		}
	}
	if !forwards(http.MethodPost, "/v1/runners/register", "") {
		t.Error("POST /v1/runners/register (no bearer) is not forwarded")
	}
	for _, c := range [][2]string{
		// Method-pinned: the right path with another method.
		{"GET", "/v1/runners/register"}, {"GET", "/v1/runners/r-1/heartbeat"}, {"DELETE", "/v1/beads/gd-1"},
		{"PUT", "/v1/beads/gd-1"}, {"POST", "/v1/events/stream"}, {"GET", "/v1/hooks/emit"},
		{"HEAD", "/v1/beads"}, {"GET", "/v1/escalations/gd-1/result"},
		// Off the allowlist: the operator's runner routes, tokens, admin and the rest.
		{"POST", "/v1/runners/invite"}, {"POST", "/v1/runners/r-1/revoke"}, {"GET", "/v1/runners"},
		{"GET", "/v1/whoami"}, {"GET", "/v1/bus/events"}, {"GET", "/v1/decisions/gd-1"},
		{"PUT", "/v1/configs/type:x"}, {"DELETE", "/v1/configs/type:x"}, {"GET", "/v1/agents-by-name:batch"},
		{"POST", "/v1/escalations/gd-1/approve"}, {"POST", "/v1/escalations/gd-1/reject"},
		{"POST", "/v1/decisions/gd-1/cancel"}, {"GET", "/v1/agents/roster"},
		{"POST", "/v1/bridge-tokens"}, {"POST", "/v1/fleet-tokens"}, {"POST", "/v1/auth/refresh"},
		{"POST", "/v1/admin/revoke-token"}, {"POST", "/v1/hooks/execute"}, {"POST", "/v1/embeddings/upsert"},
		{"POST", "/v1/escalations"}, {"POST", "/v1/decisions/gd-1/resolve"}, {"GET", "/v1/beads/gd-1/events"},
		// One segment per wildcard, no trailing slash, no prefix match.
		{"GET", "/v1/beads/"}, {"GET", "/v1/beads/gd-1/"}, {"GET", "/v1/beads/gd-1/x/y"},
		{"GET", "/v1/events/stream/"}, {"POST", "/v1/runners/r-1/heartbeat/x"},
		{"GET", "/v1/configs/"}, {"GET", "/v1/agents-by-name/a/b"}, {"GET", "/v1/me/x"},
		// Not /v1/.
		{"GET", "/"}, {"GET", "/health"}, {"GET", "/metrics"}, {"GET", "/v2/beads"},
	} {
		if forwards(c[0], c[1], tok) {
			t.Errorf("%s %s is forwarded, want 404", c[0], c[1])
		}
	}
}

// runnerEdgeHarness is a running nginx serving public-edge.yaml's
// runners.clonk.tv block with the daemon replaced by a stub.
type runnerEdgeHarness struct {
	url    string
	daemon *edgeStub
}

func startRunnerEdgeNginx(t *testing.T) *runnerEdgeHarness {
	t.Helper()
	bin := edgeNginxBinary(t)
	doc := readEdgeFile(t, "public-edge.yaml")
	_, conf, ok := strings.Cut(doc, "  default.conf: |\n")
	if !ok {
		t.Fatal("public-edge.yaml: no default.conf")
	}
	conf, _, _ = strings.Cut(conf, "\n---")
	// The http-level directives less TLS and the cluster resolver, as the
	// land door harness does. The runner-route limit is lifted (the harness
	// sends more than 40 requests at once); the register limit is kept.
	httpLevel := conf[:regexp.MustCompile(`(?m)^    server \{`).FindStringIndex(conf)[0]]
	var kept []string
	for line := range strings.SplitSeq(httpLevel, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ssl_") || strings.HasPrefix(trimmed, "resolver ") {
			continue
		}
		kept = append(kept, line)
	}
	httpLevel = strings.Replace(strings.Join(kept, "\n"), "zone=runner_per_ip:10m rate=20r/s", "zone=runner_per_ip:10m rate=10000r/s", 1)

	h := &runnerEdgeHarness{daemon: newEdgeStub(t, func(r *http.Request) (int, string) { return http.StatusOK, "daemon:" + r.URL.Path })}
	port := freePort(t)
	srv := edgeServer(t, doc, runnerEdgeHost)
	for _, r := range []struct{ old, new string }{
		{"listen 8443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d;", port)},
		{"http2 on;", ""},
		{"gardenio-beads.gardenio.svc.cluster.local:8080", h.daemon.addr()},
	} {
		if !strings.Contains(srv, r.old) {
			t.Fatalf("%s block has no %q", runnerEdgeHost, r.old)
		}
		srv = strings.ReplaceAll(srv, r.old, r.new)
	}
	h.url = runNginx(t, bin, httpLevel, srv, port)
	return h
}

func (h *runnerEdgeHarness) do(t *testing.T, method, path, authorization string) int {
	t.Helper()
	req, err := http.NewRequest(method, h.url+path, strings.NewReader(`{"invite_code":"x","hostname":"h"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = runnerEdgeHost
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	for _, name := range forged {
		req.Header.Set(name, "forged")
	}
	req.Header.Set("X-Land-Door", "forged")
	req.Header.Set("X-Gardenio-Listener", "forged")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

// In a real nginx: every allowlisted route with a runner token reaches the
// daemon with the token, the listener header (the edge's, not the caller's)
// and nothing forged; anything else is 404 and never reaches it.
func TestPublicEdgeNginx_RunnerListener(t *testing.T) {
	t.Parallel()
	h := startRunnerEdgeNginx(t)
	const tok = "Bearer gr_0123456789abcdef"
	check := func(what, method, path, authorization string) {
		t.Helper()
		reqs := h.daemon.take()
		if len(reqs) != 1 {
			t.Fatalf("%s: the daemon saw %d requests, want 1", what, len(reqs))
		}
		r := reqs[0]
		if r.Method != method || r.URL.Path != path {
			t.Errorf("%s: forwarded %s %s", what, r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != authorization {
			t.Errorf("%s: forwarded Authorization = %q, want %q", what, got, authorization)
		}
		if got := r.Header.Values("X-Gardenio-Listener"); len(got) != 1 || got[0] != "runner" {
			t.Errorf("%s: forwarded X-Gardenio-Listener = %q, want [runner]", what, got)
		}
		for name, vs := range r.Header {
			if slices.Contains(vs, "forged") {
				t.Errorf("%s: the daemon received %s", what, name)
			}
		}
	}
	for _, p := range serverRunnerPatterns(t) {
		method, path, _ := strings.Cut(p, " ")
		uri := runnerSampleURI(path)
		if code := h.do(t, method, uri, tok); code != http.StatusOK {
			t.Fatalf("%s %s with a runner token: got %d, want 200", method, uri, code)
		}
		check(p, method, uri, tok)
	}
	for _, c := range []struct{ method, path, authorization string }{
		{http.MethodGet, "/v1/beads", "Bearer eyJhbGciOiJSUzI1NiJ9.eyJ4IjoxfQ.c2ln"},
		{http.MethodGet, "/v1/beads", "Bearer 0123456789abcdef0123456789abcdef"},
		{http.MethodGet, "/v1/beads", ""},
		{http.MethodDelete, "/v1/beads/gd-1", tok},
		{http.MethodPost, "/v1/runners/invite", tok},
		{http.MethodPost, "/v1/runners/gd-1/revoke", tok},
		{http.MethodPut, "/v1/configs/type:x", tok},
		{http.MethodPost, "/v1/escalations/gd-1/approve", tok},
		{http.MethodGet, "/v1/beads/gd-1/x", tok},
		{http.MethodGet, "//v1/bridge-tokens", tok},
		{http.MethodGet, "/v1/runners/register", tok},
		{http.MethodGet, "/health", tok},
		{http.MethodGet, "/", ""},
	} {
		if code := h.do(t, c.method, c.path, c.authorization); code != http.StatusNotFound {
			t.Errorf("%s %s (%q): got %d, want 404", c.method, c.path, c.authorization, code)
		}
	}
	if code := h.do(t, http.MethodGet, "/v1/%2e%2e/admin", tok); code != http.StatusBadRequest {
		t.Errorf("an encoded traversal: got %d, want 400", code)
	}
	if n := len(h.daemon.take()); n != 0 {
		t.Errorf("%d refused requests reached the daemon", n)
	}

	// Registration: no bearer, POST only, ten at once from one address.
	for i := range 10 {
		if code := h.do(t, http.MethodPost, "/v1/runners/register", ""); code != http.StatusOK {
			t.Fatalf("registration %d: got %d, want 200", i+1, code)
		}
		check("register", http.MethodPost, "/v1/runners/register", "")
	}
	if code := h.do(t, http.MethodPost, "/v1/runners/register", ""); code != http.StatusTooManyRequests {
		t.Errorf("the 11th registration at once: got %d, want 429", code)
	}
	if n := len(h.daemon.take()); n != 0 {
		t.Error("a rate-limited registration reached the daemon")
	}
}
