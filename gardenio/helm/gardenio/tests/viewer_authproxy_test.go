package tests

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// Ziti is off (gd-xNMLrsgajy). The ziti listeners (8095, 8080, 8443, 8446,
// 8447) stamped a fixed operator email because the ziti identity was the
// credential; with no ziti that identity is gone, so one left behind would be
// an unauthenticated operator door for anything that can reach it. The proxy
// keeps only the two edge listeners: no fixed email, no TLS, no other port.
func TestViewerAuthproxy_OnlyEdgeListeners(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/viewer-authproxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	cfg, _, _ := strings.Cut(doc, "\n---\n")

	var listens []string
	for _, m := range regexp.MustCompile(`(?m)^\s*listen\s+([^;]+);`).FindAllStringSubmatch(cfg, -1) {
		listens = append(listens, m[1])
	}
	if !slices.Equal(listens, []string{"8097", "8448"}) {
		t.Errorf("nginx must listen on exactly 8097 and 8448, got %q", listens)
	}
	// A fixed email stamp; "" (a blanked header, as ^~ /w/ sends) is not one.
	if regexp.MustCompile(`X-Auth-Request-Email "[^"]`).MatchString(doc) {
		t.Error("viewer-authproxy.yaml stamps a fixed X-Auth-Request-Email (a ziti listener remnant)")
	}
	for _, bad := range []string{
		"ssl_certificate",
		"/etc/nginx/tls",
		"gardenio-front-door-tls",
		"gardenio-mux-front-door-tls",
		"app: ziti-host",
	} {
		if strings.Contains(doc, bad) {
			t.Errorf("viewer-authproxy.yaml still carries %q (a ziti listener remnant)", bad)
		}
	}
	// The Deployment and Service expose exactly the two edge ports, and the
	// probes check one of them (8095 is gone).
	for _, want := range []string{
		"- containerPort: 8097\n              name: edge-viewer\n            - containerPort: 8448\n              name: edge-mux\n          env:",
		"tcpSocket: { port: 8097 }",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("viewer-authproxy.yaml missing %q", want)
		}
	}
	for _, p := range []string{"8095", "8443", "8080", "8446", "8447"} {
		if regexp.MustCompile(`(?:containerPort|port|targetPort): ` + p + `\b`).MatchString(doc) {
			t.Errorf("viewer-authproxy.yaml still exposes ziti port %s", p)
		}
	}
}

// gd-C2Sl6ClxAY: a write (htmx/fetch/XHR) that arrives with a lapsed session was
// answered 302 /oauth2/start; the XHR followed it to the cross-origin IdP, could
// not read the response and the write was dropped with no error. Script-issued
// requests must get a plain 401 (which the page turns into a "session expired,
// your draft is kept" notice), while browser navigations keep the sign-in 302.
// The public edge is the only sign-in for gardenio.clonk.tv since the ziti front
// door went (gd-xNMLrsgajy).
func TestPublicEdge_ScriptedRequestsGet401NotRedirect(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/public-edge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)

	if !strings.Contains(doc, "map \"$http_hx_request:$http_sec_fetch_mode\" $edge_signin_scripted") {
		t.Error("missing the $edge_signin_scripted map keyed on HX-Request and Sec-Fetch-Mode")
	}
	_, viewer, _ := strings.Cut(doc, "server_name gardenio.clonk.tv;")
	viewer, _, _ = strings.Cut(viewer, "server_name mux.clonk.tv;")
	start := strings.Index(viewer, "location @signin {")
	if start < 0 {
		t.Fatal("no @signin location in the gardenio.clonk.tv block")
	}
	block := viewer[start:]
	block = block[:strings.Index(block, "\n      }\n")]
	i401 := strings.Index(block, "if ($edge_signin_scripted) { return 401; }")
	i302 := strings.Index(block, "return 302 https://$host/oauth2/start?rd=")
	if i401 < 0 || i302 < 0 || i401 > i302 {
		t.Errorf("@signin must 401 scripted requests before the navigation 302:\n%s", block)
	}
	// The 401 must come out of a named location that carries no error_page of its
	// own, or nginx would loop it back into @signin.
	if strings.Contains(block, "error_page") {
		t.Errorf("@signin must not declare error_page (would recurse):\n%s", block)
	}
}

// authproxyServerBlock returns the nginx server block that starts at listen.
func authproxyServerBlock(t *testing.T, doc, listen string) string {
	t.Helper()
	start := strings.Index(doc, listen)
	if start < 0 {
		t.Fatalf("no `%s` server block in viewer-authproxy.yaml", listen)
	}
	rest := doc[start+len(listen):]
	if end := strings.Index(rest, "listen "); end >= 0 {
		rest = rest[:end]
	} else if end := strings.Index(rest, "\n---\n"); end >= 0 {
		rest = rest[:end]
	}
	return listen + rest
}

// The edge-only listeners (S4, gd-UqhR5d6qPt) trust the identity the public
// edge sends. They must never stamp a fixed email, must refuse a request with
// no identity, and the viewer one must carry the edge secret and the user's
// token (writes fail closed without a bearer).
func TestViewerAuthproxy_EdgeListeners(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/viewer-authproxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	viewer := authproxyServerBlock(t, doc, "listen 8097;")
	mux := authproxyServerBlock(t, doc, "listen 8448;")

	// The viewer listener lets only the Lands front-of-house paths through with
	// no email ($bv_no_identity; TestPublicEdge_LandsPublicPathsMatchViewer
	// pins which). The mux listener has no exception.
	if !strings.Contains(viewer, "if ($bv_no_identity) {\n          return 401;") {
		t.Error("8097 must return 401 when the edge sent no email off the front-of-house paths")
	}
	if !strings.Contains(doc, "map \"$bv_public:$http_x_auth_request_email\" $bv_no_identity {\n      default 0;\n      \"0:\"    1;\n    }") {
		t.Error("$bv_no_identity must be 1 exactly when the path is not public and the email is empty")
	}
	if !strings.Contains(mux, `if ($http_x_auth_request_email = "") {`+"\n          return 401;") {
		t.Error("8448 must return 401 when the edge sent no email")
	}
	for name, block := range map[string]string{"8097": viewer, "8448": mux} {
		if strings.Contains(block, "matthewbaker1110") || strings.Contains(block, "ssl_certificate") {
			t.Errorf("%s must not stamp a fixed email or terminate TLS", name)
		}
	}
	for _, want := range []string{
		"proxy_set_header X-Auth-Request-Email $http_x_auth_request_email;",
		`proxy_set_header X-Authproxy-Secret "${VIEWER_PROXY_SECRET}";`,
		"proxy_set_header Authorization $bv_edge_bearer;",
		`proxy_set_header X-Auth-Request-User "";`,
		`proxy_set_header X-Auth-Request-Preferred-Username "";`,
		`proxy_set_header X-Forwarded-Email "";`,
		"proxy_buffering off;",
		"gardenio-beads-viewer.gardenio.svc.cluster.local:8095",
	} {
		if !strings.Contains(viewer, want) {
			t.Errorf("8097 block missing %q", want)
		}
	}
	for _, want := range []string{
		"location ^~ /api/v1/credentials {\n        return 403;",
		`proxy_set_header Authorization "Bearer ${COOPMUX_TOKEN}";`,
		"proxy_set_header Upgrade    $http_upgrade;",
		"gardenio-coopmux.gardenio.svc.cluster.local:9800",
		// coopmux has no DAEMON_HMAC_KEY and trusts these as an operator identity.
		`proxy_set_header X-Operator "";`,
		`proxy_set_header X-Auth-Audit-Id "";`,
		`proxy_set_header X-Edge-Auth-Ts "";`,
		`proxy_set_header X-Edge-Auth-Sig "";`,
		`proxy_set_header X-Edge-Auth-Sig-V2 "";`,
		`proxy_set_header X-Edge-Auth-Version "";`,
		`proxy_set_header X-Edge-Auth-Mode "";`,
		`proxy_set_header X-Edge-Auth-Roles "";`,
	} {
		if !strings.Contains(mux, want) {
			t.Errorf("8448 block missing %q", want)
		}
	}
}

// The edge listeners are plain HTTP that trusts a header, so the NetworkPolicy
// is the only thing keeping a crew pod from being anyone it likes: it must have
// exactly one rule, admitting only the public-edge namespace on 8097/8448.
func TestViewerAuthproxy_NetworkPolicyFencesEdgeListeners(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/viewer-authproxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i := strings.Index(doc, "kind: NetworkPolicy")
	if i < 0 {
		t.Fatal("no NetworkPolicy in viewer-authproxy.yaml")
	}
	pol := doc[i:]
	if n := strings.Count(pol, "- from:"); n != 1 {
		t.Errorf("policy must have exactly one ingress rule (public-edge), got %d:\n%s", n, pol)
	}
	_, rule, ok := strings.Cut(pol, "- from:")
	if !ok || !strings.Contains(rule, "kubernetes.io/metadata.name: public-edge") {
		t.Fatalf("policy must admit the public-edge namespace:\n%s", pol)
	}
	if strings.Contains(rule, "podSelector") {
		t.Errorf("the rule must name only the public-edge namespace, no pod peers:\n%s", rule)
	}
	var ports []string
	for _, m := range regexp.MustCompile(`port: (\d+)`).FindAllStringSubmatch(rule, -1) {
		ports = append(ports, m[1])
	}
	if !slices.Equal(ports, []string{"8097", "8448"}) {
		t.Errorf("public-edge rule must admit exactly 8097 and 8448, got %q", ports)
	}
}

// The public edge's viewer and mux blocks: sign-in gate, overwritten identity,
// no client bearer. mux.clonk.tv was held ziti-only (gd-1bizVpXrfk) until the
// owner answered publish (gd-jfXRKAn6Ez, S4b gd-0e6aSFhxM5).
func TestPublicEdge_ViewerAndMuxBlocks(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/public-edge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	upstreams := map[string]string{
		"gardenio.clonk.tv": "viewer-authproxy.gardenio.svc.cluster.local:8097",
		"mux.clonk.tv":      "viewer-authproxy.gardenio.svc.cluster.local:8448",
	}
	for _, host := range []string{"gardenio.clonk.tv", "mux.clonk.tv"} {
		start := strings.Index(doc, "server_name "+host+";")
		if start < 0 {
			t.Fatalf("no server block for %s", host)
		}
		block := doc[start:]
		if end := strings.Index(block[1:], "server_name "); end >= 0 {
			block = block[:end+1]
		}
		for _, want := range []string{
			"allowed_groups=%2Fgardenio-admins",
			"auth_request /oauth2/auth;",
			"proxy_set_header X-Auth-Request-Email $edge_email;",
			`proxy_set_header Authorization "";`,
			upstreams[host],
		} {
			if !strings.Contains(strings.Join(strings.Fields(block), " "), want) {
				t.Errorf("%s block missing %q", host, want)
			}
		}
		if !strings.Contains(doc[strings.Index(doc, "dnsNames:"):], host) {
			t.Errorf("%s missing from the Certificate", host)
		}
	}
	if !strings.Contains(doc, "viewer-authproxy.gardenio.svc.cluster.local:8097") {
		t.Error("edge must proxy the viewer to viewer-authproxy 8097")
	}
	_, mux, _ := strings.Cut(doc, "server_name mux.clonk.tv;")
	mux, _, _ = strings.Cut(mux, "server_name api.clonk.tv;")
	for _, want := range []string{
		"location ^~ /api/v1/credentials {", "return 403;", "proxy_set_header Upgrade    $http_upgrade;",
		// Cross-site WebSocket hijack guard: location / refuses a foreign Origin.
		"if ($mux_bad_origin) { return 403; }",
		// The block sets its own headers, so server-level ones do not inherit.
		"add_header X-Frame-Options DENY always;",
		`add_header Content-Security-Policy "frame-ancestors 'none'" always;`,
	} {
		if !strings.Contains(mux, want) {
			t.Errorf("mux block missing %q", want)
		}
	}
	// The origin map: no Origin and https://mux.clonk.tv pass, anything else is refused.
	_, originMap, _ := strings.Cut(doc, "map $http_origin $mux_bad_origin {")
	originMap, _, _ = strings.Cut(originMap, "}")
	for _, want := range []string{"default                0;", `"~."                   1;`, `"https://mux.clonk.tv" 0;`} {
		if !strings.Contains(originMap, want) {
			t.Errorf("$mux_bad_origin map missing %q", want)
		}
	}
	// Order matters: the origin check must precede auth_request in location /.
	loc := mux[strings.Index(mux, "location / {"):]
	if strings.Index(loc, "$mux_bad_origin") > strings.Index(loc, "auth_request /oauth2/auth;") {
		t.Error("mux origin check must run before auth_request")
	}
}

// viewer-authproxy injects the coopmux bearer on mux.clonk.tv. Once
// agents.perAgentTokens splits the agent-facing coopmux token off kd-beads-token
// (coopmux.coopmuxTokenSecret), coopmux only accepts the split token; an authproxy
// still reading kd-beads-token returns 401 on every request. Pin the manifest to
// the overlay so the two cannot drift. (gd-kLRIbndGNp)
func TestViewerAuthproxy_CoopmuxTokenMatchesOverlay(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/viewer-authproxy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	i := strings.Index(doc, "- name: COOPMUX_TOKEN")
	if i < 0 {
		t.Fatal("viewer-authproxy.yaml has no COOPMUX_TOKEN env")
	}
	block := doc[i:min(len(doc), i+250)]
	if strings.Contains(block, "kd-beads-token") {
		t.Fatal("COOPMUX_TOKEN must not come from kd-beads-token: coopmux's agent-facing token is split from it")
	}

	overlay, err := os.ReadFile("../../../../deploy-values/gardenio-k3s.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, line := range strings.Split(string(overlay), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "coopmuxTokenSecret:"); ok {
			want = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	if want == "" {
		t.Fatal("deploy-values/gardenio-k3s.yaml sets no coopmux.coopmuxTokenSecret")
	}
	if !strings.Contains(block, "name: "+want) {
		t.Fatalf("COOPMUX_TOKEN must come from %q (the overlay's coopmux.coopmuxTokenSecret); block:\n%s", want, block)
	}
}

// Land roles are exclusive (gd-9aCcG1zCCY, spec §6.4): an owner or guest is a
// direct member of /project-<land>/owners or /guests only, and Keycloak's
// full-path groups claim lists direct memberships only. oauth2-proxy matches
// exact strings, so every sign-in check that admits a land's parent group must
// also admit both role subgroups, or owners and guests get a 403 at the edge
// (gd-A9avO8cXBr). scripts/check-viewer-edge-groups.py --write writes them.
func TestViewerEdges_AdmitLandRoleSubgroups(t *testing.T) {
	t.Parallel()
	groupsRe := regexp.MustCompile(`allowed_groups=([^;&\s]*)`)
	parentRe := regexp.MustCompile(`^/project-[a-z0-9-]+$`)
	for _, file := range []string{"public-edge.yaml"} {
		raw, err := os.ReadFile("../../gardenio-ziti/" + file)
		if err != nil {
			t.Fatal(err)
		}
		parents := 0
		for _, m := range groupsRe.FindAllStringSubmatch(string(raw), -1) {
			list := strings.Split(strings.ReplaceAll(m[1], "%2F", "/"), ",")
			for _, g := range list {
				if !parentRe.MatchString(g) {
					continue
				}
				parents++
				for _, sub := range []string{g + "/owners", g + "/guests"} {
					if !slices.Contains(list, sub) {
						t.Errorf("%s: allowed_groups=%s admits %s but not %s", file, m[1], g, sub)
					}
				}
			}
		}
		if parents == 0 {
			t.Errorf("%s: no sign-in check admits a /project-<land> group; the viewer check is missing", file)
		}
	}
}

// The Window (plan gd-pz87vf3xsB, PW.5): the 8097 listener's ^~ /w/ forwards
// with no identity at all (the link is the credential, judged by kbeads), so
// the no-identity 401 lives in location / only. /w/ still proves the edge
// with the secret, takes only GET, never forwards an email, token, bearer or
// cookie, keeps the token out of the error log, and both listeners log the
// redacted format.
func TestViewerAuthproxy_WindowLocation(t *testing.T) {
	t.Parallel()
	doc := readEdgeFile(t, "viewer-authproxy.yaml")
	viewer := authproxyServerBlock(t, doc, "listen 8097;")

	if n := strings.Count(viewer, "return 401;"); n != 1 {
		t.Errorf("8097 has %d `return 401;`, want exactly the one in location /", n)
	}
	if !strings.Contains(nginxBlock(viewer, "location /"), "if ($bv_no_identity) {\n          return 401;") {
		t.Error("8097 location / must keep the no-identity 401")
	}

	w := nginxBlock(viewer, "location ^~ /w/")
	if w == "" {
		t.Fatal("8097 has no `location ^~ /w/`")
	}
	for _, line := range []string{
		"limit_except GET { deny all; }",
		"error_log /dev/stderr crit;",
		"proxy_pass http://gardenio-beads-viewer.gardenio.svc.cluster.local:8095;",
		`proxy_set_header X-Authproxy-Secret "${VIEWER_PROXY_SECRET}";`,
	} {
		if !strings.Contains(w, line) {
			t.Errorf("8097 location ^~ /w/ missing %q", line)
		}
	}
	for _, h := range []string{"X-Auth-Request-Email", "X-Auth-Request-Access-Token", "Authorization", "Cookie", "X-Auth-Request-User", "X-Auth-Request-Preferred-Username", "X-Forwarded-Email"} {
		if !regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(h) + `\s+"";`).MatchString(w) {
			t.Errorf("8097 location ^~ /w/ does not blank %s", h)
		}
	}
	for _, v := range []string{"$http_x_auth_request_email", "$http_x_auth_request_access_token", "$bv_edge_bearer", "$http_cookie", "$http_authorization", "return 401"} {
		if strings.Contains(w, v) {
			t.Errorf("8097 location ^~ /w/ uses %s: it must forward no identity and refuse nothing for want of one", v)
		}
	}
	if strings.Contains(authproxyServerBlock(t, doc, "listen 8448;"), "/w/") {
		t.Error("the mux listener has no Window")
	}

	checkRedactedLogs(t, "viewer-authproxy.yaml", doc)
}
