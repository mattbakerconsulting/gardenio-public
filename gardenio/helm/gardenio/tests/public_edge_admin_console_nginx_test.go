package tests

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The gardenio realm admin console on auth.clonk.tv, in real nginx (owner
// decision gd-papvgFfwMf, admin impersonation V1): public-edge.yaml's
// auth.clonk.tv server block in front of a stub forwardauth and a stub
// Keycloak. The console and its admin API need both the /gardenio-admins edge
// sign-in and the console's own bearer, which must reach Keycloak untouched;
// the master realm and every other admin path stay 404 even for an admin.
//
// nginx comes from $EDGE_NGINX or PATH, as for the land door tests.

const (
	consoleAdminSession  = "_oauth2_proxy=signed-in-admin"
	consoleTenantSession = "_oauth2_proxy=signed-in-tenant"
	consoleBearer        = "Bearer console-admin-token"
	consoleRefreshCookie = "_oauth2_proxy=refreshed; Path=/; Secure; HttpOnly"
)

type consoleHarness struct {
	url      string
	keycloak *edgeStub
	fa       *edgeStub
}

func startAdminConsoleEdge(t *testing.T) *consoleHarness {
	t.Helper()
	bin := edgeNginxBinary(t)
	h := &consoleHarness{
		keycloak: newEdgeStub(t, func(r *http.Request) (int, string) { return http.StatusOK, "keycloak:" + r.URL.Path }),
	}
	// oauth2-proxy's /oauth2/auth: 202 for the admin cookie when the check
	// asks for /gardenio-admins, 403 for a signed-in non-admin, 401 with no
	// session. A bearer never signs in (the edge blanks Authorization).
	fa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.fa.mu.Lock()
		h.fa.reqs = append(h.fa.reqs, r.Clone(r.Context()))
		h.fa.mu.Unlock()
		cookie := r.Header.Get("Cookie")
		switch {
		case r.URL.Path != "/oauth2/auth", r.Header.Get("Authorization") != "":
			w.WriteHeader(http.StatusUnauthorized)
		case strings.Contains(cookie, consoleAdminSession) && r.URL.Query().Get("allowed_groups") == "/gardenio-admins":
			w.Header().Set("Set-Cookie", consoleRefreshCookie)
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(cookie, consoleAdminSession), strings.Contains(cookie, consoleTenantSession):
			w.WriteHeader(http.StatusForbidden)
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(fa.Close)
	h.fa = &edgeStub{srv: fa}

	edgeDoc := readEdgeFile(t, "public-edge.yaml")
	_, conf, ok := strings.Cut(edgeDoc, "  default.conf: |\n")
	if !ok {
		t.Fatal("public-edge.yaml: no default.conf")
	}
	conf, _, _ = strings.Cut(conf, "\n---")
	httpLevel := replaceAll(t, "public-edge http level", httpLevelOf(conf),
		// The harness sends far more than 10 requests a second from one address.
		"zone=auth_per_ip:10m rate=10r/s", "zone=auth_per_ip:10m rate=10000r/s",
	)
	port := freePort(t)
	server := replaceAll(t, "auth.clonk.tv block", edgeServer(t, edgeDoc, "auth.clonk.tv"),
		"listen 8443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d;", port),
		"http2 on;", "",
		"gardenio-forwardauth.gardenio.svc.cluster.local:4180", h.fa.addr(),
		"gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080", h.keycloak.addr(),
		"alias /usr/share/nginx/become/;", "alias "+becomePageDir(t, edgeDoc)+"/;",
	)
	h.url = runNginx(t, bin, httpLevel, server, port)
	return h
}

// becomePageDir writes the public-edge-become ConfigMap's files to a temp
// dir, as the pod's volume mount does, and returns the dir.
func becomePageDir(t *testing.T, edgeDoc string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range becomePageFiles(t, edgeDoc) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// becomePageFiles returns the data of public-edge.yaml's public-edge-become
// ConfigMap.
func becomePageFiles(t *testing.T, edgeDoc string) map[string]string {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(edgeDoc))
	for {
		var doc struct {
			Kind     string                `yaml:"kind"`
			Metadata struct{ Name string } `yaml:"metadata"`
			Data     map[string]string     `yaml:"data"`
		}
		if err := dec.Decode(&doc); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("public-edge.yaml: %v", err)
		}
		if doc.Kind == "ConfigMap" && doc.Metadata.Name == "public-edge-become" {
			return doc.Data
		}
	}
	t.Fatal("public-edge.yaml: no ConfigMap public-edge-become")
	return nil
}

// do sends one request to the edge as auth.clonk.tv with the given cookie
// ("" for none) and extra header pairs, and returns the response and body.
func (h *consoleHarness) do(t *testing.T, method, path, cookie string, extra ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.url+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "auth.clonk.tv"
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	for i := 0; i+1 < len(extra); i += 2 {
		req.Header.Set(extra[i], extra[i+1])
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

var consolePaths = []string{
	"/auth/admin/gardenio/console/",
	"/auth/admin/gardenio/console/whoami?currentRealm=gardenio",
	"/auth/admin/realms/gardenio",
	"/auth/admin/realms/gardenio/users/u-1",
	"/auth/admin/serverinfo",
	"/auth/admin/realms",
}

func TestAdminConsoleEdgeNginx_AdminReachesKeycloakWithItsBearer(t *testing.T) {
	t.Parallel()
	h := startAdminConsoleEdge(t)
	for _, path := range consolePaths {
		resp, body := h.do(t, "GET", path, consoleAdminSession, "Authorization", consoleBearer)
		if resp.StatusCode != http.StatusOK || !strings.HasPrefix(body, "keycloak:") {
			t.Errorf("admin GET %s: %d %q, want 200 from Keycloak", path, resp.StatusCode, body)
		}
		if got := resp.Header.Values("Set-Cookie"); !slices.Contains(got, consoleRefreshCookie) {
			t.Errorf("admin GET %s: the sign-in check's refreshed cookie is not passed on: %q", path, got)
		}
	}
	for _, r := range h.keycloak.take() {
		if got := r.Header.Get("Authorization"); got != consoleBearer {
			t.Errorf("Keycloak got Authorization %q on %s, want the console's bearer", got, r.URL.Path)
		}
		if got := r.Header.Get("X-Forwarded-Host"); got != "auth.clonk.tv" {
			t.Errorf("Keycloak got X-Forwarded-Host %q on %s", got, r.URL.Path)
		}
	}
	for _, r := range h.fa.take() {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("the sign-in check got the client's Authorization on %s", r.Header.Get("X-Forwarded-Uri"))
		}
	}

	// Impersonate is a POST on the realm's admin API.
	resp, _ := h.do(t, "POST", "/auth/admin/realms/gardenio/users/u-1/impersonation", consoleAdminSession, "Authorization", consoleBearer)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("admin POST impersonation: %d, want 200", resp.StatusCode)
	}
	if reqs := h.keycloak.take(); len(reqs) != 1 || reqs[0].Method != "POST" {
		t.Errorf("Keycloak did not get the impersonation POST: %d requests", len(reqs))
	}
}

func TestAdminConsoleEdgeNginx_NoAdminSessionNeverReachesKeycloak(t *testing.T) {
	t.Parallel()
	h := startAdminConsoleEdge(t)
	for _, path := range consolePaths {
		// A navigation with no session goes to the sign-in on this host.
		resp, _ := h.do(t, "GET", path, "", "Authorization", consoleBearer, "Sec-Fetch-Mode", "navigate")
		want := "https://auth.clonk.tv/oauth2/start?rd=http://auth.clonk.tv" + path
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
			t.Errorf("no session GET %s: %d %q, want 302 to %s", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
		// The console's own fetch gets a plain 401.
		if resp, _ := h.do(t, "GET", path, "", "Authorization", consoleBearer, "Sec-Fetch-Mode", "cors"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no session scripted GET %s: %d, want 401", path, resp.StatusCode)
		}
		// Signed in, not a gardenio admin.
		if resp, _ := h.do(t, "GET", path, consoleTenantSession, "Authorization", consoleBearer); resp.StatusCode != http.StatusForbidden {
			t.Errorf("non-admin GET %s: %d, want 403", path, resp.StatusCode)
		}
	}
	if reqs := h.keycloak.take(); len(reqs) != 0 {
		t.Errorf("Keycloak got %d requests with no admin session, first %s", len(reqs), reqs[0].URL.Path)
	}
}

func TestAdminConsoleEdgeNginx_MasterAndOtherAdminPathsStay404(t *testing.T) {
	t.Parallel()
	h := startAdminConsoleEdge(t)
	for _, path := range []string{
		"/auth/admin/",
		"/auth/admin",
		"/auth/admin/master/console/",
		"/auth/admin/master/console/whoami",
		"/auth/admin/realms/master",
		"/auth/admin/realms/master/users",
		"/auth/admin/realms/gardenio-other/users",
		"/auth/admin/realms/other",
		"/auth/admin/gardenio/consolex",
		"/auth/admin/gardenio/",
		"/auth//admin/realms/master/users",
		"/auth/admin/serverinfo/x",
		"/auth/realms/master/",
		"/gardenio/",
		"/gardenio/becomex",
		"/gardenio/become.js",
	} {
		resp, _ := h.do(t, "GET", path, consoleAdminSession, "Authorization", consoleBearer)
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("admin GET %s: %d, want 404", path, resp.StatusCode)
		}
	}
	for _, path := range []string{
		"/auth/admin/realms/gardenio/../master/users",
		"/auth/admin/realms/gardenio/%2e%2e/master/users",
		"/auth/admin/realms/gardenio%2F..%2Fmaster",
		"/gardenio/become/../../auth/admin/realms/master",
		"/gardenio/become/..data/become.js",
	} {
		resp, _ := h.do(t, "GET", path, consoleAdminSession, "Authorization", consoleBearer)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("admin GET %s: %d, want 400", path, resp.StatusCode)
		}
	}
	// The two endpoints outside the realm path are read-only: POST
	// /admin/realms would create a realm.
	for _, path := range []string{"/auth/admin/realms", "/auth/admin/serverinfo"} {
		if resp, _ := h.do(t, "POST", path, consoleAdminSession, "Authorization", consoleBearer); resp.StatusCode != http.StatusForbidden {
			t.Errorf("admin POST %s: %d, want 403", path, resp.StatusCode)
		}
	}
	if reqs := h.keycloak.take(); len(reqs) != 0 {
		t.Errorf("Keycloak got %d requests for closed paths, first %s %s", len(reqs), reqs[0].Method, reqs[0].URL.Path)
	}
}

// Static shape: only the console locations carry the console's bearer; the
// sign-in round trip and the sign-in check blank it, and the console
// locations check /gardenio-admins and nothing wider.
func TestPublicEdge_AuthHostConsoleSignInShape(t *testing.T) {
	t.Parallel()
	auth := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), "auth.clonk.tv")
	blank := regexp.MustCompile(`(?m)^\s*proxy_set_header Authorization\s+"";`)
	for _, loc := range []string{"location ^~ /oauth2/", "location = /oauth2/auth"} {
		body := nginxBlock(auth, loc)
		if body == "" {
			t.Fatalf("auth.clonk.tv: no %q", loc)
		}
		if !blank.MatchString(body) {
			t.Errorf("auth.clonk.tv %s does not blank the client Authorization header", loc)
		}
	}
	if !strings.Contains(nginxBlock(auth, "location = /oauth2/auth"), "/oauth2/auth?allowed_groups=%2Fgardenio-admins;") {
		t.Error("auth.clonk.tv's sign-in check must admit /gardenio-admins only")
	}
	if strings.Contains(auth, "/oauth2/auth-signed-in") {
		t.Error("auth.clonk.tv must not use the any-signed-in-user check")
	}
	n := 0
	for _, m := range regexp.MustCompile(`(?m)^\s*location ~ \^/auth/admin/.*\{$`).FindAllString(auth, -1) {
		n++
		body := nginxBlock(auth, strings.TrimSuffix(strings.TrimSpace(m), " {"))
		if !strings.Contains(body, "auth_request /oauth2/auth;") {
			t.Errorf("%s has no sign-in check", strings.TrimSpace(m))
		}
		if blank.MatchString(body) {
			t.Errorf("%s blanks Authorization: the console's bearer must reach Keycloak", strings.TrimSpace(m))
		}
	}
	if n != 2 {
		t.Errorf("auth.clonk.tv has %d /auth/admin/ locations, want 2 (console + realm API, read-only extras)", n)
	}
}

// The one-click Become page (gd-VIm9kMDswf): a static page on this host
// behind the same /gardenio-admins sign-in check. It reaches Keycloak only
// through the console locations above, by its own script.
var becomePaths = []string{
	"/gardenio/become/?email=tenant%40example.com",
	"/gardenio/become/",
	"/gardenio/become/index.html",
	"/gardenio/become/become.js",
	"/gardenio/become/become.css",
}

func TestBecomePageEdgeNginx_AdminGetsThePage(t *testing.T) {
	t.Parallel()
	h := startAdminConsoleEdge(t)
	want := map[string]string{
		"/gardenio/become/?email=tenant%40example.com": "text/html",
		"/gardenio/become/":                            "text/html",
		"/gardenio/become/index.html":                  "text/html",
		"/gardenio/become/become.js":                   "text/javascript",
		"/gardenio/become/become.css":                  "text/css",
	}
	for _, path := range becomePaths {
		// Opened from /garden (same site), and the return from Keycloak.
		for _, site := range []string{"same-site", "same-origin", "none", ""} {
			resp, body := h.do(t, "GET", path, consoleAdminSession, "Sec-Fetch-Site", site)
			if resp.StatusCode != http.StatusOK {
				t.Errorf("admin GET %s (Sec-Fetch-Site %q): %d, want 200", path, site, resp.StatusCode)
				continue
			}
			if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, want[path]) {
				t.Errorf("admin GET %s: Content-Type %q, want %s", path, got, want[path])
			}
			if strings.HasPrefix(want[path], "text/html") && !strings.Contains(body, `<script src="become.js" defer></script>`) {
				t.Errorf("admin GET %s: not the Become page:\n%s", path, body)
			}
			if got := resp.Header.Values("Set-Cookie"); !slices.Contains(got, consoleRefreshCookie) {
				t.Errorf("admin GET %s: the sign-in check's refreshed cookie is not passed on: %q", path, got)
			}
			csp := resp.Header.Get("Content-Security-Policy")
			for _, d := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
				if !strings.Contains(csp, d) {
					t.Errorf("admin GET %s: CSP %q lacks %s", path, csp, d)
				}
			}
			if resp.Header.Get("Cache-Control") != "no-store" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
				resp.Header.Get("Strict-Transport-Security") == "" {
				t.Errorf("admin GET %s: headers %v", path, resp.Header)
			}
		}
	}
	// The page and its files only: anything else under the prefix is 404.
	for _, path := range []string{"/gardenio/become/nope.js", "/gardenio/become/x/", "/gardenio/become/default.conf"} {
		if resp, _ := h.do(t, "GET", path, consoleAdminSession); resp.StatusCode != http.StatusNotFound {
			t.Errorf("admin GET %s: %d, want 404", path, resp.StatusCode)
		}
	}
	// No trailing slash: to the page, with the query kept.
	resp, _ := h.do(t, "GET", "/gardenio/become?email=tenant%40example.com", consoleAdminSession)
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "https://auth.clonk.tv/gardenio/become/?email=tenant%40example.com" {
		t.Errorf("GET /gardenio/become: %d %q, want 301 to the page", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Read-only.
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if resp, _ := h.do(t, method, "/gardenio/become/", consoleAdminSession); resp.StatusCode != http.StatusForbidden {
			t.Errorf("admin %s /gardenio/become/: %d, want 403", method, resp.StatusCode)
		}
	}
	if reqs := h.keycloak.take(); len(reqs) != 0 {
		t.Errorf("the page itself sent %d requests to Keycloak, first %s", len(reqs), reqs[0].URL.Path)
	}
}

func TestBecomePageEdgeNginx_NoAdminSessionGetsNoPage(t *testing.T) {
	t.Parallel()
	h := startAdminConsoleEdge(t)
	for _, path := range becomePaths {
		resp, _ := h.do(t, "GET", path, "", "Sec-Fetch-Mode", "navigate")
		want := "https://auth.clonk.tv/oauth2/start?rd=http://auth.clonk.tv" + path
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != want {
			t.Errorf("no session GET %s: %d %q, want 302 to %s", path, resp.StatusCode, resp.Header.Get("Location"), want)
		}
		if resp, _ := h.do(t, "GET", path, "", "Sec-Fetch-Mode", "cors"); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("no session scripted GET %s: %d, want 401", path, resp.StatusCode)
		}
		if resp, body := h.do(t, "GET", path, consoleTenantSession); resp.StatusCode != http.StatusForbidden || strings.Contains(body, "become.js") {
			t.Errorf("non-admin GET %s: %d, want 403 and no page", path, resp.StatusCode)
		}
	}
	if reqs := h.keycloak.take(); len(reqs) != 0 {
		t.Errorf("Keycloak got %d requests with no admin session", len(reqs))
	}
}

// A Become link opened from another site does not run: it gets a page that
// asks the admin to continue (a same-origin click) or go back. The return
// from Keycloak carries no query and is let through, since a GitHub sign-in
// on the way makes it cross-site. Any query counts as a start, so a
// percent-encoded or recased name (?%65mail=, which become.js's
// URLSearchParams decodes to email) is caught too.
func TestBecomePageEdgeNginx_CrossSiteStartAsksFirst(t *testing.T) {
	t.Parallel()
	h := startAdminConsoleEdge(t)
	for _, path := range []string{
		"/gardenio/become/?email=tenant%40example.com",
		"/gardenio/become/index.html?email=x",
		"/gardenio/become/?%65mail=t@example.com",
		"/gardenio/become/?EMAIL=t@example.com",
	} {
		resp, body := h.do(t, "GET", path, consoleAdminSession, "Sec-Fetch-Site", "cross-site")
		if resp.StatusCode != http.StatusForbidden || strings.Contains(body, "become.js") ||
			!strings.Contains(body, `<a href="">Continue</a>`) || !strings.Contains(body, `href="https://gardenio.clonk.tv/garden"`) {
			t.Errorf("cross-site GET %s: %d, want 403 with Continue and /garden links:\n%s", path, resp.StatusCode, body)
		}
		if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
			t.Errorf("cross-site GET %s: Content-Type %q", path, resp.Header.Get("Content-Type"))
		}
	}
	if reqs := h.fa.take(); len(reqs) != 0 {
		t.Errorf("a cross-site start reached the sign-in check %d times", len(reqs))
	}
	if resp, _ := h.do(t, "GET", "/gardenio/become/", consoleAdminSession, "Sec-Fetch-Site", "cross-site"); resp.StatusCode != http.StatusOK {
		t.Errorf("cross-site return from sign-in: %d, want 200", resp.StatusCode)
	}
}

// A 429 from auth_per_ip on the page is a page with a way back, not nginx's
// bare error.
func TestBecomePageEdgeNginx_RateLimitedIsAPage(t *testing.T) {
	t.Parallel()
	bin := edgeNginxBinary(t)
	edgeDoc := readEdgeFile(t, "public-edge.yaml")
	_, conf, _ := strings.Cut(edgeDoc, "  default.conf: |\n")
	conf, _, _ = strings.Cut(conf, "\n---")
	httpLevel := replaceAll(t, "public-edge http level", httpLevelOf(conf),
		"zone=auth_per_ip:10m rate=10r/s", "zone=auth_per_ip:10m rate=1r/m",
	)
	port := freePort(t)
	server := replaceAll(t, "auth.clonk.tv block", edgeServer(t, edgeDoc, "auth.clonk.tv"),
		"listen 8443 ssl;", fmt.Sprintf("listen 127.0.0.1:%d;", port),
		"http2 on;", "",
		"limit_req zone=auth_per_ip burst=40 nodelay;", "limit_req zone=auth_per_ip nodelay;",
		"alias /usr/share/nginx/become/;", "alias "+becomePageDir(t, edgeDoc)+"/;",
	)
	h := &consoleHarness{url: runNginx(t, bin, httpLevel, server, port)}
	// The first request takes the one allowed (the stub-less sign-in check
	// fails it); a return such as /__edge_canary's would not count.
	h.do(t, "GET", "/gardenio/become/", consoleAdminSession)
	resp, body := h.do(t, "GET", "/gardenio/become/?email=tenant%40example.com", consoleAdminSession)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(body, "Too many requests") ||
		!strings.Contains(body, `href="https://gardenio.clonk.tv/garden"`) {
		t.Errorf("rate-limited GET: %d, want 429 with a way back:\n%s", resp.StatusCode, body)
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Errorf("rate-limited GET: Content-Type %q", resp.Header.Get("Content-Type"))
	}
}

// Static shape: the page's location has the admin sign-in check, and the
// pod mounts the page's ConfigMap where the location reads it.
func TestPublicEdge_BecomePageShape(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	auth := edgeServer(t, doc, "auth.clonk.tv")
	loc := nginxBlock(auth, "location ^~ /gardenio/become/")
	if loc == "" {
		t.Fatal("auth.clonk.tv: no location ^~ /gardenio/become/")
	}
	for _, want := range []string{"auth_request /oauth2/auth;", "error_page 401 = @signin;", "limit_except GET HEAD { deny all; }", "alias /usr/share/nginx/become/;"} {
		if !strings.Contains(loc, want) {
			t.Errorf("/gardenio/become/ lacks %q", want)
		}
	}
	if strings.Contains(loc, "proxy_pass") {
		t.Error("/gardenio/become/ must serve files, not proxy")
	}
	if !strings.Contains(doc, "- {name: become, mountPath: /usr/share/nginx/become, readOnly: true}") ||
		!strings.Contains(doc, "- name: become\n          configMap: {name: public-edge-become}") {
		t.Error("the public-edge pod must mount ConfigMap public-edge-become at /usr/share/nginx/become")
	}
	files := becomePageFiles(t, doc)
	for _, name := range []string{"index.html", "become.js", "become.css"} {
		if files[name] == "" {
			t.Errorf("public-edge-become has no %s", name)
		}
	}
	// No inline script or style: the CSP allows neither.
	if regexp.MustCompile(`<script>|<style|\sstyle=|\son[a-z]+=`).MatchString(files["index.html"]) {
		t.Error("index.html has inline script or style, which the page's CSP blocks")
	}
}
