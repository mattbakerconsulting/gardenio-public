package tests

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Lands Track B step 7.8: the gardenio.clonk.tv edge serves the Lands front of
// house. ^~ /lands/ admits invited people and the groups the viewer admits
// (gd-ycE484kLyk: GitHub sign-in is invite-only); the routePublic pages need
// no session; all of /lands/ is per-IP rate-limited, and so is auth.clonk.tv.
// The splash, /garden/splash and its two files, is a no-session page too
// (gd-hzRHm1KB5r), on the same rate limit.

func readEdgeFile(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../../gardenio-ziti/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// nginxBlock returns the body of the first `<head> {` block in text, braces
// matched, or "" when there is none.
func nginxBlock(text, head string) string {
	start := strings.Index(text, head+" {")
	if start < 0 {
		return ""
	}
	i := start + len(head) + 2
	for depth := 1; i < len(text) && depth > 0; i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
		}
	}
	return text[start+len(head)+2 : i-1]
}

// edgeServer returns the body of the 8443 server block serving host.
func edgeServer(t *testing.T, doc, host string) string {
	t.Helper()
	for _, m := range regexp.MustCompile(`(?m)^    server \{`).FindAllStringIndex(doc, -1) {
		body := nginxBlock(doc[m[0]:], "server")
		if strings.Contains(body, "listen 8443 ssl;") && strings.Contains(body, "server_name "+host+";") {
			return body
		}
	}
	t.Fatalf("no 8443 server block for %s", host)
	return ""
}

// viewerNoSession reads the viewer's noSessionPatterns, as edge location
// heads: "GET /lands/{$}" is "= /lands/", a trailing-slash pattern is a "^~"
// prefix, anything else is exact.
func viewerNoSession(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile("../../../controller/cmd/beads-viewer/lands.go")
	if err != nil {
		t.Fatal(err)
	}
	_, list, _ := strings.Cut(string(raw), "var noSessionPatterns = []string{")
	list, _, _ = strings.Cut(list, "\n}")
	var out []string
	for _, m := range regexp.MustCompile(`"GET (/[^"]*)"`).FindAllStringSubmatch(list, -1) {
		p := m[1]
		switch {
		case strings.HasSuffix(p, "{$}"):
			out = append(out, "= "+strings.TrimSuffix(p, "{$}"))
		case strings.HasSuffix(p, "/"):
			out = append(out, "^~ "+p)
		default:
			out = append(out, "= "+p)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no noSessionPatterns in beads-viewer/lands.go; did it move?")
	}
	slices.Sort(out)
	return out
}

// The no-session paths are listed three times: the viewer's routePublic
// noSessionPatterns, the edge's /lands/ and /garden/splash locations without
// auth_request, and viewer-authproxy's $bv_public map (the only paths it lets
// through with no identity). A path missing from the edge or the authproxy is
// a sign-in wall on a public page; one extra is a page served to strangers
// that the viewer does not mean to be public. Each edge one is GET-only,
// rate-limited and sends nothing of a session.
func TestPublicEdge_LandsPublicPathsMatchViewer(t *testing.T) {
	t.Parallel()
	want := viewerNoSession(t)
	viewer := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), "gardenio.clonk.tv")

	var edge []string
	for _, m := range regexp.MustCompile(`location ((?:=|\^~) /(?:lands/|garden/splash)\S*) \{`).FindAllStringSubmatch(viewer, -1) {
		head := m[1]
		body := nginxBlock(viewer, "location "+head)
		if strings.Contains(body, "auth_request") {
			continue
		}
		edge = append(edge, head)
		for _, line := range []string{
			"limit_req zone=front_door_per_ip burst=20 nodelay;",
			"limit_except GET { deny all; }",
			"proxy_pass http://$up_viewer;",
		} {
			if !strings.Contains(body, line) {
				t.Errorf("edge location %s missing %q", head, line)
			}
		}
		for _, h := range []string{"Cookie", "Authorization", "X-Auth-Request-Email", "X-Auth-Request-Access-Token", "X-Auth-Request-User", "X-Auth-Request-Preferred-Username", "X-Forwarded-Email"} {
			if !regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(h) + `\s+"";`).MatchString(body) {
				t.Errorf("edge location %s does not blank %s", head, h)
			}
		}
	}
	slices.Sort(edge)
	if !slices.Equal(edge, want) {
		t.Errorf("edge no-session locations = %v, want the viewer's noSessionPatterns %v", edge, want)
	}

	var ap []string
	apMap := nginxBlock(readEdgeFile(t, "viewer-authproxy.yaml"), "map $uri $bv_public")
	for line := range strings.SplitSeq(apMap, "\n") {
		f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), ";"))
		if len(f) != 2 || f[0] == "default" {
			continue
		}
		if f[1] != "1" {
			t.Errorf("$bv_public entry %q must map to 1", line)
		}
		if p, ok := strings.CutPrefix(f[0], "~^"); ok {
			ap = append(ap, "^~ "+p)
		} else {
			ap = append(ap, "= "+f[0])
		}
	}
	slices.Sort(ap)
	if !slices.Equal(ap, want) {
		t.Errorf("viewer-authproxy $bv_public = %v, want the viewer's noSessionPatterns %v", ap, want)
	}
}

// GitHub sign-in is invite-only (owner ruling gd-hCMlZ5jnvB = b,
// gd-ycE484kLyk): ^~ /lands/ uses /oauth2/auth-lands, which admits exactly the
// viewer's list plus /invited, so every member the viewer admits still gets
// in and a GitHub registrant gets in once their email invite is claimed. No
// location on the viewer or the mux admits any signed-in user any more, but
// the demo door (TestPublicEdge_LandsDemoDoor).
// scripts/check-viewer-edge-groups.py holds the lists to deploy-values.
func TestPublicEdge_LandsInviteOnly(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	viewer := edgeServer(t, doc, "gardenio.clonk.tv")

	groupsOf := func(check string) []string {
		m := regexp.MustCompile(`proxy_pass http://\$up_fa/oauth2/auth\?allowed_groups=([^;\s]+);`).FindStringSubmatch(nginxBlock(viewer, "location = "+check))
		if m == nil {
			t.Fatalf("%s has no allowed_groups check", check)
		}
		out := strings.Split(strings.ReplaceAll(m[1], "%2F", "/"), ",")
		slices.Sort(out)
		return out
	}
	root, lands := groupsOf("/oauth2/auth"), groupsOf("/oauth2/auth-lands")
	if slices.Contains(root, "/invited") {
		t.Errorf("the viewer root check admits /invited: %v", root)
	}
	want := append(slices.Clone(root), "/invited")
	slices.Sort(want)
	if !slices.Equal(lands, want) {
		t.Errorf("/oauth2/auth-lands admits %v, want the viewer root's list plus /invited %v", lands, want)
	}
	for _, g := range []string{"/gardenio-admins", "/lands-tenants"} {
		if !slices.Contains(lands, g) {
			t.Errorf("/oauth2/auth-lands does not admit %s", g)
		}
	}
	check := nginxBlock(viewer, "location = /oauth2/auth-lands")
	for _, line := range []string{"internal;", `proxy_set_header Authorization     "";`} {
		if !strings.Contains(check, line) {
			t.Errorf("/oauth2/auth-lands missing %q", line)
		}
	}

	landsLoc := nginxBlock(viewer, "location ^~ /lands/")
	for _, line := range []string{
		"limit_req zone=front_door_per_ip burst=20 nodelay;",
		"auth_request /oauth2/auth-lands;",
		"error_page 401 = @signin;",
		"error_page 403 = @lands_need_invite;",
		"proxy_set_header X-Auth-Request-Email        $edge_email;",
		`proxy_set_header Authorization "";`,
		"proxy_pass http://$up_viewer;",
	} {
		if !strings.Contains(landsLoc, line) {
			t.Errorf("location ^~ /lands/ missing %q", line)
		}
	}
	// limit_req runs before the sign-in check (preaccess before access), but
	// keep it first in the block so it reads that way.
	if strings.Index(landsLoc, "limit_req ") > strings.Index(landsLoc, "auth_request ") {
		t.Error("location ^~ /lands/: limit_req must come before auth_request")
	}

	for _, host := range []string{"gardenio.clonk.tv", "mux.clonk.tv"} {
		body := edgeServer(t, doc, host)
		if strings.Contains(body, "auth-signed-in") {
			t.Errorf("%s still has the any-signed-in check", host)
		}
		uses := regexp.MustCompile(`auth_request /oauth2/auth-lands;`).FindAllStringIndex(body, -1)
		if want := map[string]int{"gardenio.clonk.tv": 1, "mux.clonk.tv": 0}[host]; len(uses) != want {
			t.Errorf("%s uses the /lands/ check %d times, want %d (only ^~ /lands/)", host, len(uses), want)
		}
	}
}

// The demo land's door (owner decision gd-T37nCeWMhq = open): = /lands/demo
// and = /lands/accept (the terms the door wants accepted first) admit any
// signed-in account through /oauth2/auth-lands-demo, a check with no
// allowed_groups, so a stranger can take a demo pass. They proxy like
// ^~ /lands/, and no other location on the viewer or the mux uses the check.
func TestPublicEdge_LandsDemoDoor(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	viewer := edgeServer(t, doc, "gardenio.clonk.tv")

	check := nginxBlock(viewer, "location = /oauth2/auth-lands-demo")
	for _, line := range []string{"internal;", "proxy_pass http://$up_fa/oauth2/auth;", `proxy_set_header Authorization     "";`} {
		if !strings.Contains(check, line) {
			t.Errorf("/oauth2/auth-lands-demo missing %q", line)
		}
	}
	if strings.Contains(check, "allowed_groups") {
		t.Error("/oauth2/auth-lands-demo must admit any signed-in account (no allowed_groups)")
	}

	for _, path := range []string{"/lands/demo", "/lands/accept"} {
		loc := nginxBlock(viewer, "location = "+path)
		for _, line := range []string{
			"limit_req zone=front_door_per_ip burst=20 nodelay;",
			"limit_except GET POST { deny all; }",
			"auth_request /oauth2/auth-lands-demo;",
			"error_page 401 = @signin;",
			"proxy_set_header X-Auth-Request-Email        $edge_email;",
			`proxy_set_header Authorization "";`,
			"proxy_pass http://$up_viewer;",
		} {
			if !strings.Contains(loc, line) {
				t.Errorf("location = %s missing %q", path, line)
			}
		}
		if strings.Index(loc, "limit_req ") > strings.Index(loc, "auth_request ") {
			t.Errorf("location = %s: limit_req must come before auth_request", path)
		}
	}

	for _, host := range []string{"gardenio.clonk.tv", "mux.clonk.tv"} {
		uses := regexp.MustCompile(`auth_request /oauth2/auth-lands-demo;`).FindAllStringIndex(edgeServer(t, doc, host), -1)
		if want := map[string]int{"gardenio.clonk.tv": 2, "mux.clonk.tv": 0}[host]; len(uses) != want {
			t.Errorf("%s uses the demo check %d times, want %d (= /lands/demo and = /lands/accept)", host, len(uses), want)
		}
	}
}

// Signed in but not invited: a navigation gets a plain "You need an invite"
// page with a 403, not a redirect (a sign-in redirect would come straight
// back); a script gets the bare 403; the named location declares no
// error_page (it would loop) and sends nothing to the viewer.
func TestPublicEdge_LandsNeedInvitePage(t *testing.T) {
	t.Parallel()
	viewer := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), "gardenio.clonk.tv")
	page := nginxBlock(viewer, "location @lands_need_invite")
	i403 := strings.Index(page, "if ($edge_signin_scripted) { return 403; }")
	iPage := strings.Index(page, "return 403 '<!doctype html>")
	if i403 < 0 || iPage < 0 || i403 > iPage {
		t.Fatalf("@lands_need_invite must 403 scripted requests before the page:\n%s", page)
	}
	for _, want := range []string{"default_type text/html;", "You need an invite", "invite-only", `href="/oauth2/sign_out?rd=/lands/"`, `add_header Cache-Control "no-store" always;`} {
		if !strings.Contains(page, want) {
			t.Errorf("@lands_need_invite missing %q", want)
		}
	}
	for _, bad := range []string{"error_page", "proxy_pass", "return 302", "auth_request"} {
		if strings.Contains(page, bad) {
			t.Errorf("@lands_need_invite must not %s", bad)
		}
	}
	// The page is one quoted nginx string: no single quote inside it, and
	// nothing nginx would read as a variable.
	body := page[iPage+len("return 403 '") : strings.LastIndex(page, "';")]
	if strings.ContainsAny(body, "'$") {
		t.Errorf("the page body holds a quote or $:\n%s", body)
	}
}

// A signed-in user with no admitted group who navigates anywhere else is sent
// to /lands/ (Track B 2.2 step 4); a script still gets the 403, and the named
// location declares no error_page (it would loop).
func TestPublicEdge_UnadmittedGoToLands(t *testing.T) {
	t.Parallel()
	viewer := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), "gardenio.clonk.tv")
	if !strings.Contains(nginxBlock(viewer, "location /"), "error_page 403 = @lands_front;") {
		t.Error("gardenio.clonk.tv location / must send an auth_request 403 to @lands_front")
	}
	front := nginxBlock(viewer, "location @lands_front")
	i403 := strings.Index(front, "if ($edge_signin_scripted) { return 403; }")
	i302 := strings.Index(front, "return 302 https://$host/lands/;")
	if i403 < 0 || i302 < 0 || i403 > i302 {
		t.Errorf("@lands_front must 403 scripted requests before the navigation 302:\n%s", front)
	}
	if strings.Contains(front, "error_page") {
		t.Error("@lands_front must not declare error_page")
	}
}

// auth.clonk.tv is per-IP limited on every location, except that the
// back-channel endpoints the cluster calls through it map to an empty key
// (not counted): forwardauth's redeem and refresh, discovery and JWKS.
func TestPublicEdge_AuthHostRateLimited(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	if !strings.Contains(doc, "limit_req_zone $auth_limit_key zone=auth_per_ip:10m rate=10r/s;") {
		t.Error("no auth_per_ip zone keyed on $auth_limit_key")
	}
	if !strings.Contains(doc, "limit_req_zone $binary_remote_addr zone=front_door_per_ip:10m rate=5r/s;") {
		t.Error("no front_door_per_ip zone")
	}
	keyMap := nginxBlock(doc, "map $uri $auth_limit_key")
	if !regexp.MustCompile(`(?m)^\s*default\s+\$binary_remote_addr;`).MatchString(keyMap) {
		t.Error("$auth_limit_key must default to the client address")
	}
	for _, path := range []string{
		"/auth/realms/gardenio/protocol/openid-connect/token",
		"/auth/realms/gardenio/protocol/openid-connect/certs",
		"/auth/realms/gardenio/.well-known/openid-configuration",
	} {
		if !mapExempts(keyMap, path) {
			t.Errorf("$auth_limit_key does not exempt %s", path)
		}
	}
	for _, path := range []string{
		"/auth/realms/gardenio/protocol/openid-connect/auth",
		"/auth/realms/gardenio/login-actions/authenticate",
		"/auth/realms/gardenio/broker/github/endpoint",
		"/auth/resources/x/login/gardenio/css/gardenio.css",
	} {
		if mapExempts(keyMap, path) {
			t.Errorf("$auth_limit_key exempts the browser path %s", path)
		}
	}

	auth := edgeServer(t, doc, "auth.clonk.tv")
	server, _, _ := strings.Cut(auth, "location ")
	if !strings.Contains(server, "limit_req zone=auth_per_ip burst=40 nodelay;") || !strings.Contains(server, "limit_req_status 429;") {
		t.Error("auth.clonk.tv must set limit_req auth_per_ip at server level, before its locations")
	}
}

// mapExempts reports whether a regex entry of an nginx map maps path to "".
func mapExempts(m, path string) bool {
	for _, mm := range regexp.MustCompile(`(?m)^\s*"~([^"]+)"\s+"";`).FindAllStringSubmatch(m, -1) {
		if regexp.MustCompile(mm[1]).MatchString(path) {
			return true
		}
	}
	return false
}
