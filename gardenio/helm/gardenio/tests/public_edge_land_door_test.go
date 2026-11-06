package tests

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Door D.2 (gd-h8nQExxbkD): api.clonk.tv serves /land/<name>/v1/* as the
// daemon's /v1/* for the members of that one land. The caller's bearer goes
// through; the sign-in check admits /project-<name> and its /owners and
// /guests only; SSE, token, admin, hook and embedding routes are 404 there.

const landDoorLocation = `location ~ "^/land/[a-z][a-z0-9-]{0,62}/v1/"`

// landDoorName is the edge's $land_door_name map as Go: the land a raw
// request line names, or "" when it names none.
func landDoorName(t *testing.T, doc, requestURI string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*"~(\^/land/[^"]+)"\s+\$land_door_n;`).FindStringSubmatch(nginxBlock(doc, "map $request_uri $land_door_name"))
	if m == nil {
		t.Fatal("public-edge.yaml: no `map $request_uri $land_door_name` entry capturing land_door_n")
	}
	re := regexp.MustCompile(m[1])
	sm := re.FindStringSubmatch(requestURI)
	if sm == nil {
		return ""
	}
	return sm[re.SubexpIndex("land_door_n")]
}

// The land name the edge accepts is the contract's LandName, in the map and
// in every land-door location, so a name the contract allows always has a
// door and a name it refuses never reaches forwardauth's allowed_groups.
func TestPublicEdge_LandDoorNameIsContractLandName(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../../../lands/contract/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "\n    LandName:\n")
	if !ok {
		t.Fatal("lands/contract/openapi.yaml: no LandName schema")
	}
	m := regexp.MustCompile(`pattern: '\^([^$']+)\$'`).FindStringSubmatch(rest)
	if m == nil {
		t.Fatal("LandName has no anchored pattern")
	}
	name := m[1]

	doc := readEdgeFile(t, "public-edge.yaml")
	if want := `"~^/land/(?<land_door_n>` + name + `)/v1/" $land_door_n;`; !strings.Contains(nginxBlock(doc, "map $request_uri $land_door_name"), want) {
		t.Errorf("$land_door_name map must read the contract's LandName %s: want %s", name, want)
	}
	api := edgeServer(t, doc, "api.clonk.tv")
	for _, loc := range regexp.MustCompile(`location ~ "\^/land/([^/"]+)/`).FindAllStringSubmatch(api, -1) {
		if loc[1] != name {
			t.Errorf("api.clonk.tv land location names lands with %s, want the contract's %s", loc[1], name)
		}
	}
}

// The land a request names decides the groups, and only it: a bearer for
// another land, a land whose name is a prefix or extension of this one, an
// admin and a plain tenant are all refused; an encoded or doubled prefix
// names no land.
func TestPublicEdge_LandDoorCrossLandBearer(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	check := nginxBlock(edgeServer(t, doc, "api.clonk.tv"), "location = /oauth2/auth-land")
	m := regexp.MustCompile(`allowed_groups=([^;\s]+);`).FindStringSubmatch(check)
	if m == nil {
		t.Fatal("/oauth2/auth-land has no allowed_groups")
	}
	admits := func(requestURI string, groups ...string) bool {
		land := landDoorName(t, doc, requestURI)
		if land == "" {
			t.Fatalf("%s names no land", requestURI)
		}
		list := strings.ReplaceAll(strings.ReplaceAll(m[1], "$land_door_name", land), "%2F", "/")
		for a := range strings.SplitSeq(list, ",") {
			if slices.Contains(groups, a) {
				return true
			}
		}
		return false
	}

	for _, g := range []string{"/project-acme", "/project-acme/owners", "/project-acme/guests"} {
		if !admits("/land/acme/v1/beads?limit=1", g) {
			t.Errorf("/land/acme refuses %s", g)
		}
	}
	for _, c := range []struct {
		uri    string
		groups []string
	}{
		{"/land/other/v1/beads", []string{"/project-acme", "/project-acme/owners", "/project-acme/guests"}},
		{"/land/acme/v1/beads", []string{"/project-other", "/project-other/owners"}},
		{"/land/acm/v1/beads", []string{"/project-acme"}},
		{"/land/acme2/v1/beads", []string{"/project-acme"}},
		{"/land/acme/v1/beads", []string{"/project-acme-x/owners", "/project-acm"}},
		{"/land/acme/v1/beads", []string{"/gardenio-admins", "/lands-tenants"}},
		{"/land/acme/v1/beads?project=other", []string{"/project-other"}},
	} {
		if admits(c.uri, c.groups...) {
			t.Errorf("%s admits a bearer holding only %v", c.uri, c.groups)
		}
	}

	for _, uri := range []string{
		"/land/%61cme/v1/beads", "/land//acme/v1/beads", "//land/acme/v1/beads",
		"/land/Acme/v1/beads", "/land/1acme/v1/beads", "/land/a,b/v1/beads",
		"/land/acme%2Cother/v1/beads", "/land/acme/v2/beads", "/v1/beads",
	} {
		if land := landDoorName(t, doc, uri); land != "" {
			t.Errorf("%s names land %q, want none (the door 404s it)", uri, land)
		}
	}
}

// The land door's sign-in check is bearer-only, like api.clonk.tv's own:
// no cookie and no forwarded access token reach forwardauth, and the client's
// Authorization does (it is what is being checked).
func TestPublicEdge_LandDoorSignInCheck(t *testing.T) {
	t.Parallel()
	api := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), "api.clonk.tv")
	check := nginxBlock(api, "location = /oauth2/auth-land")
	for _, line := range []string{
		"internal;",
		`if ($land_door_name = "") { return 403; }`,
		"proxy_pass http://$up_fa/oauth2/auth?allowed_groups=%2Fproject-$land_door_name,%2Fproject-$land_door_name%2Fowners,%2Fproject-$land_door_name%2Fguests;",
		"proxy_pass_request_body off;",
		"proxy_set_header X-Forwarded-Uri   $request_uri;",
		`proxy_set_header Cookie                      "";`,
		`proxy_set_header X-Auth-Request-Access-Token "";`,
	} {
		if !strings.Contains(check, line) {
			t.Errorf("/oauth2/auth-land missing %q", line)
		}
	}
	if regexp.MustCompile(`(?m)^\s*proxy_set_header Authorization\s`).MatchString(check) {
		t.Error("/oauth2/auth-land must pass the client's bearer to forwardauth")
	}
	// D.18a: a minted land token branches to the daemon's check, after the
	// empty-land guard and before anything is sent to forwardauth.
	order := []string{
		`if ($land_door_name = "") { return 403; }`,
		"if ($land_door_land_token) { rewrite ^ /oauth2/auth-land-token last; }",
		"proxy_pass http://$up_fa/oauth2/auth?",
	}
	last := -1
	for _, line := range order {
		i := strings.Index(check, line)
		if i < 0 || i < last {
			t.Errorf("/oauth2/auth-land: want %v in this order", order)
			break
		}
		last = i
	}
}

// landTokenMap returns the regex of the $land_door_land_token map's one
// entry, which must default to 0.
func landTokenMap(t *testing.T, doc string) string {
	t.Helper()
	m := nginxBlock(doc, "map $http_authorization $land_door_land_token")
	if m == "" {
		t.Fatal("public-edge.yaml: no map $http_authorization $land_door_land_token")
	}
	var entries []string
	for line := range strings.SplitSeq(m, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") && line != "default 0;" {
			entries = append(entries, line)
		}
	}
	if !strings.Contains(m, "default 0;") || len(entries) != 1 {
		t.Fatalf("$land_door_land_token: want `default 0;` and one entry, got %q", entries)
	}
	e := regexp.MustCompile(`^"~(\^[^"]+\$)" 1;$`).FindStringSubmatch(entries[0])
	if e == nil {
		t.Fatalf("$land_door_land_token: unexpected entry %q", entries[0])
	}
	return e[1]
}

// Door D.18a (gd-WTyoLCSMUp): which Authorization headers count as a minted
// land token. Only one base64url segment does; it has no dot, so no JWT can
// take the daemon's check and skip forwardauth's group check.
func TestPublicEdge_LandTokenBearerShape(t *testing.T) {
	t.Parallel()
	re := regexp.MustCompile(landTokenMap(t, readEdgeFile(t, "public-edge.yaml")))
	for h, want := range map[string]bool{
		"Bearer 0123456789abcdef0123456789abcdef":     true,
		"Bearer land_tok-0123":                        true,
		"Bearer eyJhbGciOiJSUzI1NiJ9.eyJ4IjoxfQ.c2ln": false,
		"Bearer aaa.bbb.ccc":                          false,
		"Bearer a.b":                                  false,
		"Bearer abc.":                                 false,
		"bearer 0123456789abcdef":                     false,
		"Bearer  0123456789abcdef":                    false,
		"Bearer 0123456789abcdef ":                    false,
		"Bearer YWNtZQ==":                             false,
		"Bearer a+b/c":                                false,
		"Bearer ":                                     false,
		"Basic YTpi":                                  false,
		"":                                            false,
	} {
		if got := re.MatchString(h); got != want {
			t.Errorf("$land_door_land_token(%q) = %v, want %v", h, got, want)
		}
	}
}

// The land-token check (D.18a): internal, asks the daemon directly (never
// $land_door_upstream, so the edge's check does not depend on the land's
// landgate) with GET /v1/me, no body, the caller's bearer, the path's land
// door, and none of the identity headers the land door blanks.
func TestPublicEdge_LandTokenCheck(t *testing.T) {
	t.Parallel()
	api := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), "api.clonk.tv")
	check := nginxBlock(api, "location = /oauth2/auth-land-token")
	if check == "" {
		t.Fatal("api.clonk.tv has no `location = /oauth2/auth-land-token`")
	}
	for _, line := range []string{
		"internal;",
		`if ($land_door_name = "") { return 403; }`,
		"set $up_beads_me gardenio-beads.gardenio.svc.cluster.local:8080;",
		"proxy_pass http://$up_beads_me/v1/me;",
		"proxy_method GET;",
		"proxy_pass_request_body off;",
		`proxy_set_header Content-Length    "";`,
	} {
		if !strings.Contains(check, line) {
			t.Errorf("/oauth2/auth-land-token missing %q", line)
		}
	}
	if strings.Contains(check, "$land_door_upstream") || strings.Contains(check, "landgate") {
		t.Error("/oauth2/auth-land-token must ask the daemon, not the land's landgate")
	}
	assertLandDoorHeaders(t, "/oauth2/auth-land-token", check)
	// Only /oauth2/auth-land reaches it: no location auth_requests it.
	if strings.Contains(api, "auth_request /oauth2/auth-land-token") {
		t.Error("a location auth_requests /oauth2/auth-land-token directly; it is reached only through /oauth2/auth-land")
	}
}

// landDoorAllowlist reads the land door's route allowlist (Door D.18), the
// one list kbeads and landgate enforce too: "METHOD /v1/pattern" per line.
func landDoorAllowlist(t *testing.T) [][2]string {
	t.Helper()
	raw, err := os.ReadFile("../../../../kbeads/internal/server/land_door_routes.txt")
	if err != nil {
		t.Fatal(err)
	}
	var routes [][2]string
	for line := range strings.SplitSeq(string(raw), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if f := strings.Fields(line); len(f) == 2 {
			routes = append(routes, [2]string{f[0], f[1]})
		} else if len(f) != 0 {
			t.Fatalf("land_door_routes.txt: bad line %q", line)
		}
	}
	if len(routes) < 40 {
		t.Fatalf("land_door_routes.txt: only %d routes", len(routes))
	}
	return routes
}

// landDoorRouteRegex is the $land_door_route map entry for one listed route:
// METHOD, a space and the door path, each mux wildcard one path segment.
func landDoorRouteRegex(method, pattern string) string {
	var b strings.Builder
	last := 0
	for _, m := range regexp.MustCompile(`\{[^}]+\}`).FindAllStringIndex(pattern, -1) {
		b.WriteString(regexp.QuoteMeta(pattern[last:m[0]]))
		if strings.HasSuffix(pattern[m[0]:m[1]], "...}") {
			b.WriteString(`.+`)
		} else {
			b.WriteString(`[^/]+`)
		}
		last = m[1]
	}
	b.WriteString(regexp.QuoteMeta(pattern[last:]))
	return "^" + method + " /land/[^/]+" + b.String() + "$"
}

// landDoorRouteMap returns the regexes of the $land_door_route map, which
// must default to 0 and set 1 on each entry.
func landDoorRouteMap(t *testing.T, doc string) []string {
	t.Helper()
	m := nginxBlock(doc, `map "$request_method $uri" $land_door_route`)
	if m == "" {
		t.Fatal(`public-edge.yaml: no map "$request_method $uri" $land_door_route`)
	}
	var res []string
	for line := range strings.SplitSeq(m, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || line == "default 0;" {
			continue
		}
		e := regexp.MustCompile(`^"~(\^[^"]+\$)" 1;$`).FindStringSubmatch(line)
		if e == nil {
			t.Fatalf("$land_door_route: unexpected line %q", line)
		}
		res = append(res, e[1])
	}
	if !strings.Contains(m, "default 0;") {
		t.Error("$land_door_route must default to 0 (closed)")
	}
	return res
}

// The edge's allowlist is the list, entry for entry.
func TestPublicEdge_LandDoorRouteMapIsTheList(t *testing.T) {
	t.Parallel()
	got := landDoorRouteMap(t, readEdgeFile(t, "public-edge.yaml"))
	var want []string
	for _, r := range landDoorAllowlist(t) {
		want = append(want, landDoorRouteRegex(r[0], r[1]))
	}
	slices.Sort(got)
	slices.Sort(want)
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("$land_door_route lacks %q (from land_door_routes.txt)", w)
		}
	}
	for _, g := range got {
		if !slices.Contains(want, g) {
			t.Errorf("$land_door_route has %q, which land_door_routes.txt does not list", g)
		}
	}
	if r := landDoorRouteRegex("GET", "/v1/beads/{id}/labels/{label}"); r != `^GET /land/[^/]+/v1/beads/[^/]+/labels/[^/]+$` {
		t.Errorf("landDoorRouteRegex = %s", r)
	}
}

// Which routes the land door forwards. nginx tries the regex locations inside
// ^~ /land/ in order and takes the first match; anything none matches gets
// the outer return 404. A forwarding location then 404s a request whose
// method and path are not on $land_door_route. The paths are the decoded,
// slash-merged $uri nginx matches on, which is also what the daemon routes on.
func TestPublicEdge_LandDoorRoutes(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	api := edgeServer(t, doc, "api.clonk.tv")
	land := nginxBlock(api, "location ^~ /land/")
	if land == "" {
		t.Fatal("api.clonk.tv has no `location ^~ /land/`")
	}
	var allow []*regexp.Regexp
	for _, r := range landDoorRouteMap(t, doc) {
		allow = append(allow, regexp.MustCompile(r))
	}
	type loc struct {
		re      *regexp.Regexp
		forward bool
	}
	var locs []loc
	for _, m := range regexp.MustCompile(`location ~ "([^"]+)" \{`).FindAllStringSubmatchIndex(land, -1) {
		head := land[m[0] : m[1]-2]
		body := nginxBlock(land[m[0]:], head)
		forward := strings.Contains(body, "proxy_pass ")
		locs = append(locs, loc{regexp.MustCompile(land[m[2]:m[3]]), forward})
		if !forward && strings.TrimSpace(body) != "return 404;" {
			t.Errorf("%s neither forwards nor returns 404:\n%s", head, body)
		}
		// The allowlist check comes before the sign-in check and the
		// rewrite, so it sees the door path and refuses before forwardauth.
		if forward {
			gate := strings.Index(body, `if ($land_door_route = 0) { return 404; }`)
			if gate < 0 || gate > strings.Index(body, "auth_request ") || gate > strings.Index(body, "rewrite ") {
				t.Errorf("%s must 404 a route off $land_door_route before auth_request and rewrite", head)
			}
		}
	}
	if len(locs) == 0 {
		t.Fatal("no regex locations inside ^~ /land/")
	}
	// The outer fallback, after the nested locations.
	if outer := regexp.MustCompile(`(?s)\}\s*return 404;\s*$`); !outer.MatchString(land) {
		t.Error("location ^~ /land/ must end in return 404 for every path its locations do not match")
	}
	forwards := func(method, uri string) bool {
		for _, l := range locs {
			if l.re.MatchString(uri) {
				if !l.forward {
					return false
				}
				return slices.ContainsFunc(allow, func(re *regexp.Regexp) bool { return re.MatchString(method + " " + uri) })
			}
		}
		return false
	}

	for _, c := range [][2]string{
		{"GET", "/v1/beads"}, {"GET", "/v1/beads/gd-1"}, {"GET", "/v1/beads/gd-1/comments"},
		{"POST", "/v1/beads/gd-1/comments"}, {"POST", "/v1/beads/gd-1/close"}, {"PATCH", "/v1/beads/gd-1"},
		{"POST", "/v1/beads"}, {"DELETE", "/v1/beads/gd-1/labels/x"}, {"GET", "/v1/me"},
		{"GET", "/v1/agents-by-name/a"}, {"GET", "/v1/lands/v1/lands/acme/events"},
		{"GET", "/v1/lands/v1/lands/acme/whoami"}, {"POST", "/v1/lands/v1/lands/acme/escalations"},
		{"POST", "/v1/windows"}, {"GET", "/v1/windows"}, {"DELETE", "/v1/windows/gd-1"}, {"GET", "/v1/windows/preview"},
	} {
		if !forwards(c[0], "/land/acme"+c[1]) {
			t.Errorf("%s /land/acme%s is not forwarded", c[0], c[1])
		}
	}
	for _, c := range [][2]string{
		// Off the allowlist: not the agent loop's, not the Lands API's.
		{"GET", "/v1/events"}, {"GET", "/v1/whoami"}, {"GET", "/v1/ready"}, {"GET", "/v1/authz"},
		{"GET", "/v1/beads/gd-1/events"}, {"POST", "/v1/decisions/gd-1/resolve"}, {"GET", "/v1/escalations"},
		{"GET", "/v1/users/me/terms"}, {"GET", "/v1/lands/acme/members"}, {"GET", "/v1/configs"},
		{"POST", "/v1/agents/gd-1/gates/decision/satisfy"}, {"DELETE", "/v1/beads/gd-1"},
		{"PUT", "/v1/beads/gd-1"}, {"GET", "/v1/beads/gd-1/x/y"}, {"GET", "/v1/beads/"},
		{"POST", "/v1/beads/gd-1/transfer"}, {"GET", "/v1/lands/v1/lands/acme/nope"},
		// A window bearer's view and its live stream are read in-cluster,
		// never through a door.
		{"GET", "/v1/windows/self/view"}, {"GET", "/v1/windows/self/live"}, {"GET", "/v1/windows/gd-1"}, {"DELETE", "/v1/windows/gd-1/x"},
		{"POST", "/v1/windows/preview"}, {"GET", "/v1/windows/preview/x"},
		// The daemon's SSE: long-lived, outlive the access token. The land
		// feed is the map's, above.
		{"GET", "/v1/events/stream"}, {"GET", "/v1/events/stream/"}, {"GET", "/v1/bus/events"}, {"GET", "/v1/bus/events/x"},
		// Token issuing and management.
		{"POST", "/v1/bridge-tokens"}, {"GET", "/v1/bridge-tokens/x"}, {"POST", "/v1/fleet-tokens"}, {"GET", "/v1/fleet-tokens/x"},
		{"POST", "/v1/auth/refresh"}, {"GET", "/v1/auth"},
		// Admin.
		{"POST", "/v1/admin/revoke-token"}, {"POST", "/v1/admin/revoke-user"}, {"GET", "/v1/admin/"}, {"GET", "/v1/admin"},
		// Machine-only.
		{"POST", "/v1/hooks/execute"}, {"POST", "/v1/hooks/emit"}, {"POST", "/v1/embeddings/upsert"},
	} {
		if forwards(c[0], "/land/acme"+c[1]) {
			t.Errorf("%s /land/acme%s is forwarded, want 404", c[0], c[1])
		}
	}
	for _, uri := range []string{
		"/land/", "/land/acme", "/land/acme/", "/land/acme/health", "/land/acme/metrics",
		"/land/acme/v2/beads", "/land/Acme/v1/beads", "/land/1acme/v1/beads",
		"/land/" + strings.Repeat("a", 64) + "/v1/beads",
	} {
		if forwards("GET", uri) {
			t.Errorf("%s is forwarded, want 404", uri)
		}
	}
	if !forwards("GET", "/land/"+strings.Repeat("a", 63)+"/v1/beads") {
		t.Error("a 63-character land name has no door")
	}
}

// The forwarding location: the raw and decoded paths must name the same land,
// the per-IP limit runs before the sign-in check, the path loses its
// /land/<name> prefix, the caller's bearer reaches the daemon and nothing
// else that names an identity does.
func TestPublicEdge_LandDoorForwarding(t *testing.T) {
	t.Parallel()
	api := edgeServer(t, readEdgeFile(t, "public-edge.yaml"), "api.clonk.tv")
	door := nginxBlock(nginxBlock(api, "location ^~ /land/"), landDoorLocation)
	if door == "" {
		t.Fatalf("no %s inside location ^~ /land/", landDoorLocation)
	}
	order := []string{
		`if ($land_door_name = "") { return 404; }`,
		"limit_req zone=api_per_ip burst=20 nodelay;",
		"auth_request /oauth2/auth-land;",
		"set $up_land $land_door_upstream;",
		"rewrite ^/land/[^/]+(/v1/.*)$ $1 break;",
		"proxy_pass http://$up_land;",
	}
	last := -1
	for _, line := range order {
		i := strings.Index(door, line)
		if i < 0 {
			t.Errorf("land door missing %q", line)
			continue
		}
		if i < last {
			t.Errorf("land door: %q is out of order (want %v)", line, order)
		}
		last = i
	}
	if strings.Contains(door, "error_page") {
		t.Error("land door must not redirect: an API client gets the 401 or 403 as is")
	}
	assertLandDoorHeaders(t, "land door", door)
	rw := regexp.MustCompile(`^/land/[^/]+(/v1/.*)$`)
	for in, want := range map[string]string{
		"/land/acme/v1/beads":          "/v1/beads",
		"/land/acme/v1/beads/gd-1/x/y": "/v1/beads/gd-1/x/y",
		"/land/acme/v1/":               "/v1/",
	} {
		if got := rw.ReplaceAllString(in, "$1"); got != want {
			t.Errorf("rewrite %s = %s, want %s", in, got, want)
		}
	}
}

// assertLandDoorHeaders: the caller's bearer reaches the daemon and nothing
// else that names an identity does.
func assertLandDoorHeaders(t *testing.T, what, door string) {
	t.Helper()
	if regexp.MustCompile(`(?m)^\s*proxy_set_header Authorization\s`).MatchString(door) {
		t.Errorf("%s must pass the caller's bearer through to the daemon", what)
	}
	for _, h := range []string{
		"Cookie", "X-Auth-Request-Access-Token",
		"X-Actor", "X-Fleet", "X-System-Operator", "X-Owner-Override",
		"X-Operated-By", "X-Edge-Auth-Ts", "X-Edge-Auth-Sig", "X-Edge-Auth-Version",
		"X-Auth-Request-User", "X-Auth-Request-Email", "X-Auth-Request-Groups",
		// The agent-pod family kbeads honours for a per-agent token
		// (gd-Utbyy4xJRX).
		"X-Trigger-Bead", "X-User-Bead-Id", "X-No-Owner-Default", "X-No-Auto-Parent",
		"X-No-Auto-Inherit", "X-Force-Parent-Edge",
	} {
		if !regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(h) + `\s+"";`).MatchString(door) {
			t.Errorf("%s does not blank %s", what, h)
		}
	}
	// X-Land-Door-Check marks the edge's own admission check, which the
	// daemon leaves out of the rate limit: set on that check alone, blanked
	// wherever a caller is forwarded.
	check := `proxy_set_header X-Land-Door-Check       "";`
	if what == "/oauth2/auth-land-token" {
		check = "proxy_set_header X-Land-Door-Check 1;"
	}
	if !strings.Contains(door, check) {
		t.Errorf("%s: want %q", what, check)
	}
	// The door names its land to kbeads and landgate, which then require a
	// token minted for exactly that land (gd-eAfptUBh9Z). The value is the
	// edge's own capture, never the client's header.
	if !regexp.MustCompile(`(?m)^\s*proxy_set_header X-Land-Door\s+\$land_door_name;`).MatchString(door) {
		t.Errorf("%s must set X-Land-Door to $land_door_name", what)
	}
}

const landFeedLocation = `location ~ "^/land/[a-z][a-z0-9-]{0,62}/v1/lands/v1/lands/[a-z][a-z0-9-]{0,62}/events$"`

// The land feed (D.9 gd-Z9VVGjvzBk): forwarded like the door, ahead of it
// (nginx takes the first regex location that matches), with no proxy
// buffering. No limit is keyed on the land: limit_req and limit_conn run
// before auth_request whatever their order, so a stranger's garbage tokens
// would spend the land's share. kbeads holds the per-land limits.
func TestPublicEdge_LandFeed(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	if regexp.MustCompile(`limit_(req|conn)_zone \$land_door_name`).MatchString(doc) {
		t.Error("a limit keyed on the land runs before the sign-in check; keep per-land limits in kbeads")
	}
	land := nginxBlock(edgeServer(t, doc, "api.clonk.tv"), "location ^~ /land/")
	feed := nginxBlock(land, landFeedLocation)
	if feed == "" {
		t.Fatalf("no %s inside location ^~ /land/", landFeedLocation)
	}
	if strings.Index(land, landFeedLocation) > strings.Index(land, landDoorLocation) {
		t.Error("the land feed location must come before the land door's, or the door serves the feed buffered")
	}
	order := []string{
		`if ($land_door_name = "") { return 404; }`,
		"limit_req zone=api_per_ip burst=20 nodelay;",
		"auth_request /oauth2/auth-land;",
		"set $up_land $land_door_upstream;",
		"rewrite ^/land/[^/]+(/v1/.*)$ $1 break;",
		"proxy_pass http://$up_land;",
		"proxy_buffering off;",
	}
	last := -1
	for _, line := range order {
		i := strings.Index(feed, line)
		if i < 0 {
			t.Errorf("land feed missing %q", line)
			continue
		}
		if i < last {
			t.Errorf("land feed: %q is out of order (want %v)", line, order)
		}
		last = i
	}
	assertLandDoorHeaders(t, "land feed", feed)

	re := regexp.MustCompile(`^/land/[a-z][a-z0-9-]{0,62}/v1/lands/v1/lands/[a-z][a-z0-9-]{0,62}/events$`)
	for uri, want := range map[string]bool{
		"/land/acme/v1/lands/v1/lands/acme/events":  true,
		"/land/acme/v1/lands/v1/lands/acme/events/": false,
		"/land/acme/v1/lands/v1/lands/acme/eventsx": false,
		"/land/acme/v1/lands/v1/lands/acme/tickets": false,
		"/land/acme/v1/events/stream":               false,
		"/land/Acme/v1/lands/v1/lands/acme/events":  false,
	} {
		if got := re.MatchString(uri); got != want {
			t.Errorf("land feed location matches %s = %v, want %v", uri, got, want)
		}
	}
}

// Where the door forwards a land (landgate v0, gd-ParwLpmgID): the daemon by
// default, and a land named in the map only to its own landgate, so no edit
// can send one land's members through another land's gate or off the cluster.
func TestPublicEdge_LandDoorUpstreamMap(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "public-edge.yaml")
	m := regexp.MustCompile(`(?s)map \$land_door_name \$land_door_upstream \{(.*?)\n    \}`).FindStringSubmatch(doc)
	if m == nil {
		t.Fatal("public-edge.yaml: no `map $land_door_name $land_door_upstream`")
	}
	entry := regexp.MustCompile(`^([a-z][a-z0-9-]{0,62}) gardenio-landgate-([a-z][a-z0-9-]{0,62})\.gardenio\.svc\.cluster\.local:8080;$`)
	sawDefault := false
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == "default gardenio-beads.gardenio.svc.cluster.local:8080;" {
			sawDefault = true
			continue
		}
		e := entry.FindStringSubmatch(line)
		if e == nil || e[1] != e[2] {
			t.Errorf("land door upstream entry %q: want `<name> gardenio-landgate-<name>.gardenio.svc.cluster.local:8080;`", line)
		}
	}
	if !sawDefault {
		t.Error("the land door upstream map must default to the daemon")
	}
}
