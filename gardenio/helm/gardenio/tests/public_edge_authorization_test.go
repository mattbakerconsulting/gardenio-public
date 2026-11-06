package tests

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The cookie-session hosts of the raw public-edge manifest (grafana, dashboard, gardenio, mux)
// must blank the client's Authorization header on the sign-in check, the
// sign-in round trip and the proxied upstream: forwardauth runs
// --skip-jwt-bearer-tokens, so a client-sent bearer JWT would otherwise
// authenticate a request that has no cookie session (gd-YKKtrSwDCZ).
func TestPublicEdge_CookieSessionHostsBlankAuthorization(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "..", "gardenio-ziti", "public-edge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	blank := regexp.MustCompile(`(?m)^\s*proxy_set_header Authorization\s+"";`)
	for _, host := range []string{"grafana.clonk.tv", "dashboard.clonk.tv", "gardenio.clonk.tv", "mux.clonk.tv"} {
		_, rest, ok := strings.Cut(string(raw), "server_name "+host+";")
		if !ok {
			t.Fatalf("no server block for %s", host)
		}
		block, _, _ := strings.Cut(rest, "listen 8443 ssl;") // up to the next server block
		for _, loc := range []string{"location ^~ /oauth2/ {", "location = /oauth2/auth {", "location / {"} {
			_, body, ok := strings.Cut(block, loc)
			if !ok {
				t.Fatalf("%s: no %q", host, loc)
			}
			body, _, _ = strings.Cut(body, "\n      }\n")
			if !blank.MatchString(body) {
				t.Errorf("%s %s does not blank the client Authorization header", host, loc)
			}
		}
	}
}

// api.clonk.tv is bearer-only. forwardauth falls back to the shared
// _gardenio_oauth2 cookie session when the bearer is not a valid JWT, so a
// valid cookie plus a static token would pass the edge; the daemon honours
// X-Actor / X-Fleet / X-System-Operator / X-Owner-Override on that static
// path. The host must therefore blank the cookie (and the forwarded access
// token) on the sign-in check and the proxied upstream, blank the static-path
// headers upstream, and 404 both SSE streams (gd-FQJHYGAnjB, PR #423).
func TestPublicEdge_APIHostBlanksCookieAndStaticPathHeaders(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "..", "gardenio-ziti", "public-edge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "server_name api.clonk.tv;")
	if !ok {
		t.Fatal("no server block for api.clonk.tv")
	}
	block, _, _ := strings.Cut(rest, "\n    }\n---")
	blanked := func(body, header string) bool {
		return regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(header) + `\s+"";`).MatchString(body)
	}
	locBody := func(loc string) string {
		_, body, ok := strings.Cut(block, loc)
		if !ok {
			t.Fatalf("api.clonk.tv: no %q", loc)
		}
		body, _, _ = strings.Cut(body, "\n      }\n")
		return body
	}

	for _, h := range []string{"Cookie", "X-Auth-Request-Access-Token"} {
		if !blanked(locBody("location = /oauth2/auth {"), h) {
			t.Errorf("api.clonk.tv /oauth2/auth does not blank %s", h)
		}
	}
	upstream := []string{
		"Cookie", "X-Auth-Request-Access-Token",
		"X-Actor", "X-Fleet", "X-System-Operator", "X-Owner-Override",
		"X-Operated-By", "X-Edge-Auth-Ts", "X-Edge-Auth-Sig", "X-Edge-Auth-Version",
		"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups",
	}
	for _, loc := range []string{"location ^~ /v1/ {", "location = /health {"} {
		body := locBody(loc)
		for _, h := range upstream {
			if !blanked(body, h) {
				t.Errorf("api.clonk.tv %s does not blank %s", loc, h)
			}
		}
	}
	for _, path := range []string{"/v1/events/stream", "/v1/bus/events"} {
		if !regexp.MustCompile(`location = ` + regexp.QuoteMeta(path) + `\s+\{ return 404; \}`).MatchString(block) {
			t.Errorf("api.clonk.tv does not 404 %s", path)
		}
	}
}

// The Window (plan gd-pz87vf3xsB, PW.5): gardenio.clonk.tv serves ^~ /w/ with
// no sign-in, because the link /w/<token> is the credential and only kbeads
// judges it. That location must send nothing of a session, take only GET, sit
// behind the front door's per-IP request limit and a per-IP connection cap,
// and keep the token out of the error log. Every other location on the host
// keeps its sign-in check unless it is one of the known no-session ones.
// api.clonk.tv admits /v1/windows (mint, list, revoke on the caller's own
// bearer) through ^~ /v1/ and its sign-in check.
func TestPublicEdge_WindowLocation(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	if !strings.Contains(doc, "limit_conn_zone $binary_remote_addr zone=front_door_conn_per_ip:10m;") {
		t.Error("no front_door_conn_per_ip limit_conn_zone keyed on the client address")
	}
	viewer := edgeServer(t, doc, "gardenio.clonk.tv")

	w := nginxBlock(viewer, "location ^~ /w/")
	if w == "" {
		t.Fatal("gardenio.clonk.tv has no `location ^~ /w/`")
	}
	if strings.Contains(w, "auth_request") {
		t.Error("location ^~ /w/ must not run a sign-in check: the link is the credential")
	}
	for _, line := range []string{
		"limit_req zone=front_door_per_ip burst=10 nodelay;",
		"limit_req_status 429;",
		"limit_conn front_door_conn_per_ip 10;",
		"limit_conn_status 429;",
		"limit_except GET { deny all; }",
		"error_log /dev/stderr crit;",
		`add_header Referrer-Policy "no-referrer" always;`,
		`add_header X-Robots-Tag "noindex, nofollow" always;`,
		`add_header Strict-Transport-Security "max-age=31536000" always;`,
		"set $up_viewer viewer-authproxy.gardenio.svc.cluster.local:8097;",
		"proxy_pass http://$up_viewer;",
		// The live stream (PW.7): a buffered event stream would reach the
		// page only when the buffer filled.
		"proxy_buffering off;",
	} {
		if !strings.Contains(w, line) {
			t.Errorf("location ^~ /w/ missing %q", line)
		}
	}
	for _, h := range []string{"Cookie", "Authorization", "X-Auth-Request-Email", "X-Auth-Request-Access-Token", "X-Auth-Request-User", "X-Auth-Request-Preferred-Username", "X-Forwarded-Email"} {
		if !regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(h) + `\s+"";`).MatchString(w) {
			t.Errorf("location ^~ /w/ does not blank %s", h)
		}
	}
	if strings.Contains(w, "$edge_") || strings.Contains(w, "$http_") {
		t.Error("location ^~ /w/ must not forward any identity the edge or the client holds")
	}

	// Every location on the host without a sign-in check is one of these.
	open := map[string]bool{
		"^~ /oauth2/": true, "= /oauth2/auth": true, "= /oauth2/auth-lands": true, "= /oauth2/auth-lands-demo": true,
		"@signin": true, "@lands_front": true, "@lands_need_invite": true, "= /lands": true,
		"^~ /slack/link/": true, "^~ /w/": true,
	}
	for _, head := range viewerNoSession(t) {
		open[head] = true
	}
	for _, m := range regexp.MustCompile(`(?m)^      location (\S+(?: \S+)?) \{`).FindAllStringSubmatch(viewer, -1) {
		if !open[m[1]] && !strings.Contains(nginxBlock(viewer, "location "+m[1]), "auth_request /oauth2/auth") {
			t.Errorf("gardenio.clonk.tv location %s has no sign-in check and is not a known no-session location", m[1])
		}
	}

	// The token never reaches an access log: every server on the edge logs
	// the redacted format, and no other access_log is declared.
	checkRedactedLogs(t, "public-edge.yaml", doc)

	// api.clonk.tv: no 404 location swallows /v1/windows, so ^~ /v1/ (the
	// bearer-JWT sign-in check) carries it.
	api := edgeServer(t, doc, "api.clonk.tv")
	for _, m := range regexp.MustCompile(`location (=|\^~) (\S+)\s+\{ return 404; \}`).FindAllStringSubmatch(api, -1) {
		for _, p := range []string{"/v1/windows", "/v1/windows/gw-1"} {
			if (m[1] == "=" && p == m[2]) || (m[1] == "^~" && strings.HasPrefix(p, m[2])) {
				t.Errorf("api.clonk.tv `location %s %s` 404s %s", m[1], m[2], p)
			}
		}
	}
	if !strings.Contains(nginxBlock(api, "location ^~ /v1/"), "auth_request /oauth2/auth;") {
		t.Error("api.clonk.tv ^~ /v1/ must carry /v1/windows behind the sign-in check")
	}
}

// checkRedactedLogs holds one nginx manifest to the Window's log redaction
// (PW-D9): $log_uri writes /w/- for the token, the "redacted" format logs it
// in place of the request line, and every server block sets that access log
// (the image's nginx.conf logs the full request line at http level, which a
// server-level access_log replaces).
func checkRedactedLogs(t *testing.T, name, doc string) {
	t.Helper()
	logURI := nginxBlock(doc, "map $uri $log_uri")
	if !regexp.MustCompile(`(?m)^\s*default\s+\$request_uri;`).MatchString(logURI) ||
		!strings.Contains(logURI, `"~^/w/[^/]*(?<log_window_rest>/[^?]*)?$" /w/-$log_window_rest;`) {
		t.Errorf("%s: $log_uri must map /w/<token> to /w/- and default to $request_uri:\n%s", name, logURI)
	}
	format := regexp.MustCompile(`(?s)log_format redacted (.*?);\n`).FindStringSubmatch(doc)
	if format == nil {
		t.Fatalf("%s: no log_format redacted", name)
	}
	if !strings.Contains(format[1], "$log_uri") || !strings.Contains(format[1], "$log_referer") {
		t.Errorf("%s: log_format redacted must log $log_uri and $log_referer", name)
	}
	for _, v := range []string{"$request ", `$request"`, "$request_uri", "$uri", "$args", "$query_string", "$http_referer"} {
		if strings.Contains(format[1], v) {
			t.Errorf("%s: log_format redacted logs %s, which carries the token", name, v)
		}
	}
	servers := regexp.MustCompile(`(?m)^    server \{`).FindAllStringIndex(doc, -1)
	for _, s := range servers {
		if !strings.Contains(nginxBlock(doc[s[0]:], "server"), "\n      access_log /dev/stdout redacted;\n") {
			t.Errorf("%s: server at offset %d does not log the redacted format", name, s[0])
		}
	}
	if n := len(regexp.MustCompile(`(?m)^\s*access_log\s`).FindAllString(doc, -1)); n != len(servers) {
		t.Errorf("%s: %d access_log directives for %d servers; each server sets exactly the redacted one", name, n, len(servers))
	}
}

// clonk.tv is the public front door (gd-gQ8hZLd4AQ): the splash from the
// chart's landing Service, with no sign-in. It is certified and redirected
// from port 80 like every other host; it forwards only GET and HEAD (405
// otherwise) under the front door's per-IP limit, refuses the bad paths every
// host refuses, and sends nothing of the client's session. www.clonk.tv only
// redirects to it.
func TestPublicEdge_ClonkTV(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	dns := regexp.MustCompile(`dnsNames: \[([^\]]+)\]`).FindStringSubmatch(doc)
	redirect := regexp.MustCompile(`listen 8080;\s*\n\s*server_name ([^;]+);`).FindStringSubmatch(doc)
	for _, host := range []string{"clonk.tv", "www.clonk.tv"} {
		if dns == nil || !slices.Contains(strings.Split(dns[1], ", "), host) {
			t.Errorf("%s missing from the edge Certificate's dnsNames", host)
		}
		if redirect == nil || !slices.Contains(strings.Fields(redirect[1]), host) {
			t.Errorf("%s missing from the port-80 redirect", host)
		}
	}

	apex := edgeServer(t, doc, "clonk.tv")
	for _, line := range []string{
		`add_header Strict-Transport-Security "max-age=31536000" always;`,
		"if ($public_edge_bad_path) { return 400; }",
		"if ($request_method !~ ^(GET|HEAD)$) { return 405; }",
	} {
		if !strings.Contains(apex, line) {
			t.Errorf("clonk.tv missing %q", line)
		}
	}
	if heads := regexp.MustCompile(`(?m)^      location (\S+(?: \S+)?) \{`).FindAllStringSubmatch(apex, -1); len(heads) != 1 || heads[0][1] != "/" {
		t.Errorf("clonk.tv locations = %q, want only /", heads)
	}
	loc := nginxBlock(apex, "location /")
	for _, line := range []string{
		"limit_req zone=front_door_per_ip burst=20 nodelay;",
		"limit_req_status 429;",
		"set $up_landing gardenio-landing.gardenio.svc.cluster.local:80;",
		"proxy_pass http://$up_landing;",
	} {
		if !strings.Contains(loc, line) {
			t.Errorf("clonk.tv location / missing %q", line)
		}
	}
	for _, h := range []string{"Cookie", "Authorization", "X-Auth-Request-Email", "X-Auth-Request-Access-Token", "X-Auth-Request-User", "X-Auth-Request-Preferred-Username", "X-Forwarded-Email"} {
		if !regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(h) + `\s+"";`).MatchString(loc) {
			t.Errorf("clonk.tv location / does not blank %s", h)
		}
	}
	if strings.Contains(apex, "auth_request") || strings.Contains(apex, "$edge_") {
		t.Error("clonk.tv must not run a sign-in check or forward an edge identity: the splash holds no session")
	}

	www := edgeServer(t, doc, "www.clonk.tv")
	if got := strings.TrimSpace(nginxBlock(www, "location /")); got != "return 301 https://clonk.tv$request_uri;" {
		t.Errorf("www.clonk.tv location / = %q, want return 301 https://clonk.tv$request_uri;", got)
	}
	if strings.Contains(www, "proxy_pass") {
		t.Error("www.clonk.tv must only redirect")
	}

	// Through a real nginx, the landing Service replaced by a stub.
	bin := edgeNginxBinary(t)
	landing := newEdgeStub(t, func(r *http.Request) (int, string) { return http.StatusOK, "splash" })
	apexURL := runEdgeServer(t, bin, doc, "clonk.tv", "gardenio-landing.gardenio.svc.cluster.local:80", landing.addr())
	wwwURL := runEdgeServer(t, bin, doc, "www.clonk.tv", "", "")
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	send := func(base, host, method, path string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiJ9.eyJ4IjoxfQ.c2ln")
		for _, name := range forged {
			req.Header.Set(name, "forged")
		}
		resp, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp
	}

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range []string{"/", "/three.module.min.js?v=0123abcd"} {
			resp := send(apexURL, "clonk.tv", method, path)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("%s clonk.tv%s = %d, want 200", method, path, resp.StatusCode)
			}
			if resp.Header.Get("Strict-Transport-Security") == "" {
				t.Errorf("%s clonk.tv%s has no HSTS", method, path)
			}
		}
	}
	for _, r := range landing.take() {
		for _, h := range []string{"Cookie", "Authorization", "X-Auth-Request-Email", "X-Auth-Request-Access-Token", "X-Auth-Request-User"} {
			if v := r.Header.Get(h); v != "" {
				t.Errorf("%s %s reached the landing with %s: %q", r.Method, r.URL, h, v)
			}
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions} {
		if code := send(apexURL, "clonk.tv", method, "/").StatusCode; code != http.StatusMethodNotAllowed {
			t.Errorf("%s clonk.tv/ = %d, want 405", method, code)
		}
	}
	if code := send(apexURL, "clonk.tv", http.MethodGet, "/a%2f..%2fb").StatusCode; code != http.StatusBadRequest {
		t.Errorf("GET clonk.tv/a%%2f..%%2fb = %d, want 400", code)
	}
	if reqs := landing.take(); len(reqs) != 0 {
		t.Errorf("%d refused requests reached the landing", len(reqs))
	}

	resp := send(wwwURL, "www.clonk.tv", http.MethodGet, "/x?y=1")
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://clonk.tv/x?y=1" {
		t.Errorf("GET www.clonk.tv/x?y=1 = %d Location %q, want 301 https://clonk.tv/x?y=1", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// runEdgeServer runs public-edge.yaml's 8443 server block for host in nginx,
// plain HTTP on a free port, with the http-level directives less TLS and the
// cluster resolver, and upstream (when not "") pointed at stub. It returns the
// base URL.
func runEdgeServer(t *testing.T, bin, doc, host, upstream, stub string) string {
	t.Helper()
	_, conf, ok := strings.Cut(doc, "  default.conf: |\n")
	if !ok {
		t.Fatal("public-edge.yaml: no default.conf")
	}
	conf, _, _ = strings.Cut(conf, "\n---")
	var kept []string
	for line := range strings.SplitSeq(conf[:regexp.MustCompile(`(?m)^    server \{`).FindStringIndex(conf)[0]], "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "ssl_") || strings.HasPrefix(trimmed, "resolver ") {
			continue
		}
		kept = append(kept, line)
	}
	port := freePort(t)
	srv := edgeServer(t, doc, host)
	repl := []struct{ old, new string }{
		{"listen 8443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d;", port)},
		{"http2 on;", ""},
	}
	if upstream != "" {
		repl = append(repl, struct{ old, new string }{upstream, stub})
	}
	for _, r := range repl {
		if !strings.Contains(srv, r.old) {
			t.Fatalf("%s block has no %q", host, r.old)
		}
		srv = strings.ReplaceAll(srv, r.old, r.new)
	}
	return runNginx(t, bin, strings.Join(kept, "\n"), srv, port)
}
