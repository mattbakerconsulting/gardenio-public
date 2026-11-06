package tests

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Grafana panels in the garden (plan gd-gh5J0vVbJP, step 1.3 gd-p8OsR6oRTm,
// docs/garden-grafana-panels.md). /costs and the garden frame a
// grafana.clonk.tv d-solo panel inside gardenio.clonk.tv. That works only
// while every knob below holds on the k3s layout; each test names the one it
// pins.

var allowEmbeddingEnv = regexp.MustCompile(`(?m)^\s*- name: GF_SECURITY_ALLOW_EMBEDDING\n\s*value: "true"$`)

// embedEnv returns the value rendered for env var name, or "" when absent.
func embedEnv(rendered, name string) string {
	m := regexp.MustCompile(`(?m)^\s*- name: ` + regexp.QuoteMeta(name) + `\n\s*value: "?([^"\n]*)"?$`).FindStringSubmatch(rendered)
	if m == nil {
		return ""
	}
	return m[1]
}

// Whenever the viewer is given a Grafana base URL (so /costs and the garden
// build iframes on it) and the in-chart Grafana renders, Grafana must allow
// embedding. Without GF_SECURITY_ALLOW_EMBEDDING it sends X-Frame-Options:
// deny and every panel reads "refused to connect" (ka-0000003842).
func TestGrafanaEmbedding_OnWheneverViewerFramesGrafana(t *testing.T) {
	t.Parallel()
	layouts := []struct {
		name   string
		values []string
		set    []string
	}{
		{name: "k3s overlay", values: []string{k3sOverlay}},
		{name: "chart default with an explicit base", set: []string{
			"grafana.enabled=true",
			"beadsViewer.enabled=true",
			"beadsViewer.grafanaBaseURL=https://grafana.example.test",
		}},
	}
	for _, l := range layouts {
		t.Run(l.name, func(t *testing.T) {
			viewer := helmTemplateShowOnlyWithValues(t, "templates/beads-viewer/deployment.yaml", l.values, l.set...)
			if embedEnv(viewer, "GRAFANA_BASE_URL") == "" {
				t.Fatalf("layout renders no GRAFANA_BASE_URL, so this check proves nothing:\n%s", viewer)
			}
			grafana := helmTemplateShowOnlyWithValues(t, "templates/grafana/deployment.yaml", l.values, l.set...)
			if !allowEmbeddingEnv.MatchString(grafana) {
				t.Errorf("beadsViewer.grafanaBaseURL is set but Grafana renders no GF_SECURITY_ALLOW_EMBEDDING=\"true\": " +
					"keep grafana.allowEmbedding true, or every /costs and garden panel is refused")
			}
		})
	}
}

// The k3s frame is cross-origin (grafana.clonk.tv inside gardenio.clonk.tv),
// so the viewer's base must be Grafana's own public root, not the viewer's
// /grafana proxy, which forwards a path k3s Grafana (served at /) does not
// serve.
func TestGrafanaEmbedding_K3sBaseIsGrafanaRoot(t *testing.T) {
	t.Parallel()
	viewer := helmTemplateShowOnlyWithValues(t, "templates/beads-viewer/deployment.yaml", []string{k3sOverlay})
	grafana := helmTemplateShowOnlyWithValues(t, "templates/grafana/deployment.yaml", []string{k3sOverlay})
	base := strings.TrimSuffix(embedEnv(viewer, "GRAFANA_BASE_URL"), "/")
	root := strings.TrimSuffix(embedEnv(grafana, "GF_SERVER_ROOT_URL"), "/")
	if base != "https://grafana.clonk.tv" || root != base {
		t.Errorf("GRAFANA_BASE_URL %q and GF_SERVER_ROOT_URL %q must both be https://grafana.clonk.tv", base, root)
	}
	if embedEnv(grafana, "GF_SERVER_SERVE_FROM_SUB_PATH") != "" {
		t.Error("k3s Grafana serves at its host root; a subpath breaks the grafana.clonk.tv frames")
	}
}

// The frame authenticates through the edge's header, never Grafana's own
// login: four frames each running an OAuth dance clobbered one another's state
// cookie and every panel showed the login page (2026-10-19). The whitelist is
// the k3s pod CIDR and the VPC, where the public-edge pod connects from.
func TestGrafanaEmbedding_K3sAuthProxyFromEdge(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, "templates/grafana/deployment.yaml", []string{k3sOverlay})
	for name, want := range map[string]string{
		"GF_AUTH_PROXY_ENABLED":         "true",
		"GF_AUTH_PROXY_HEADER_NAME":     "X-Auth-Request-Email",
		"GF_AUTH_PROXY_HEADER_PROPERTY": "email",
		"GF_AUTH_PROXY_WHITELIST":       "10.42.0.0/16,10.90.0.0/16",
	} {
		if got := embedEnv(out, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if embedEnv(out, "GF_AUTH_GENERIC_OAUTH_ENABLED") == "true" {
		t.Error("Grafana's own OIDC login must stay off while the frames use the auth-proxy header")
	}
	if embedEnv(out, "GF_SECURITY_CONTENT_SECURITY_POLICY") == "true" {
		t.Error("a Grafana CSP is not expected on k3s; check its frame-ancestors before turning it on")
	}
}

// The edge's grafana.clonk.tv sign-in cookie is host-only and SameSite=Lax.
// gardenio.clonk.tv and grafana.clonk.tv are the same site (clonk.tv), so the
// browser sends that cookie with the frame's requests, and the oauth2-proxy
// CSRF cookie survives a sign-in round trip inside the frame. SameSite=Strict
// would still pass same-site, but None is never needed and must not appear.
func TestGrafanaEmbedding_K3sEdgeCookieLaxHostOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, "templates/forwardauth/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "- --cookie-samesite=lax") {
		t.Error("forwardauth must pin --cookie-samesite=lax")
	}
	if strings.Contains(out, "--cookie-domain=") {
		t.Error("k3s forwardauth cookies are host-only (cookieDomain: \"\"); a shared domain changes who the grafana.clonk.tv cookie is sent to")
	}
	if !regexp.MustCompile(`--whitelist-domain=grafana\.clonk\.tv\b`).MatchString(out) {
		t.Error("grafana.clonk.tv must be a whitelisted rd= host, or the in-frame sign-in cannot return to the panel")
	}
}

func publicEdgeBlock(t *testing.T, host string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "..", "gardenio-ziti", "public-edge.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	_, rest, ok := strings.Cut(string(raw), "server_name "+host+";")
	if !ok {
		t.Fatalf("no server block for %s", host)
	}
	block, _, _ := strings.Cut(rest, "listen 8443 ssl;") // up to the next server block
	return block
}

var frameBlockingHeader = regexp.MustCompile(`(?mi)^\s*add_header\s+(X-Frame-Options|Content-Security-Policy)\b`)

// Nothing between the browser and the two documents may refuse the frame:
// no X-Frame-Options or CSP on the grafana.clonk.tv block (the framed side),
// the gardenio.clonk.tv block (whose frame-src would govern the parent), or
// viewer-authproxy behind it. The chart's Traefik security-headers middleware
// is not in the k3s path (TestGrafana_K3sOverlay_RendersNoIngress). mux.clonk.tv
// keeps its frame-ancestors 'none'; it is not checked here.
func TestGrafanaEmbedding_EdgeSetsNoFrameBlockingHeaders(t *testing.T) {
	t.Parallel()
	for _, host := range []string{"grafana.clonk.tv", "gardenio.clonk.tv"} {
		if m := frameBlockingHeader.FindString(publicEdgeBlock(t, host)); m != "" {
			t.Errorf("%s block sets %q, which can refuse the garden's Grafana frames", host, strings.TrimSpace(m))
		}
	}
	raw, err := os.ReadFile(filepath.Join(chartDir(t), "..", "gardenio-ziti", "viewer-authproxy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if m := frameBlockingHeader.FindString(string(raw)); m != "" {
		t.Errorf("viewer-authproxy sets %q, which can refuse the garden's Grafana frames", strings.TrimSpace(m))
	}
}

// A grafana_session_expiry cookie left from Grafana's own OIDC login makes
// Grafana 11.4's frontend rotate a token the auth-proxy user does not have,
// get 401 and reload, forever, in every frame (gd-JXjqvnKU8u). The
// grafana.clonk.tv location / expires both session cookies on every response,
// and, having its own add_header, restates the server's HSTS. Auth proxy must
// stay tokenless, or the edge would delete a live session.
func TestGrafanaEmbedding_EdgeExpiresStaleGrafanaSession(t *testing.T) {
	t.Parallel()
	block := publicEdgeBlock(t, "grafana.clonk.tv")
	_, body, ok := strings.Cut(block, "location / {")
	if !ok {
		t.Fatal("grafana.clonk.tv: no location /")
	}
	body, _, _ = strings.Cut(body, "\n      }\n")
	for _, name := range []string{"grafana_session", "grafana_session_expiry"} {
		re := regexp.MustCompile(`(?m)^\s*add_header Set-Cookie "` + name + `=; Path=/; Max-Age=0;[^"]*Secure; SameSite=Lax" always;`)
		if !re.MatchString(body) {
			t.Errorf("grafana.clonk.tv location / must expire %s (Path=/, Max-Age=0, always): a stale one reload-loops every Grafana frame", name)
		}
	}
	if !regexp.MustCompile(`(?m)^\s*add_header Strict-Transport-Security "max-age=31536000" always;`).MatchString(body) {
		t.Error("grafana.clonk.tv location / sets add_header, so it must restate Strict-Transport-Security (nginx does not inherit it)")
	}
	grafana := helmTemplateShowOnlyWithValues(t, "templates/grafana/deployment.yaml", []string{k3sOverlay})
	if v := embedEnv(grafana, "GF_AUTH_PROXY_ENABLE_LOGIN_TOKEN"); v == "true" {
		t.Error("Grafana auth proxy issues login tokens: the edge's cookie expiry would now delete live sessions; drop it from public-edge.yaml")
	}
}

// grafana.auth.defaultOrgRole is Admin on k3s: every user Grafana creates from
// the edge header is a Grafana Admin over one VictoriaMetrics with no tenant
// cut. That is safe only while the grafana.clonk.tv block admits
// /gardenio-admins and nobody else. gardenio.clonk.tv admits tenants
// (/lands-tenants, /project-*), so a copy of its group list here would make
// every tenant a Grafana Admin. Change defaultOrgRole before this test.
func TestGrafanaEmbedding_GrafanaHostAdmitsAdminsOnly(t *testing.T) {
	t.Parallel()
	grafana := helmTemplateShowOnlyWithValues(t, "templates/grafana/deployment.yaml", []string{k3sOverlay})
	if embedEnv(grafana, "GF_USERS_AUTO_ASSIGN_ORG_ROLE") != "Admin" {
		t.Skip("defaultOrgRole is no longer Admin; revisit this guard and docs/garden-grafana-panels.md")
	}
	block := publicEdgeBlock(t, "grafana.clonk.tv")
	groups := regexp.MustCompile(`allowed_groups=([^;\s]*)`).FindAllStringSubmatch(block, -1)
	if len(groups) == 0 {
		t.Fatal("grafana.clonk.tv runs no allowed_groups check: with defaultOrgRole Admin, any signed-in realm user would be a Grafana Admin")
	}
	for _, g := range groups {
		if g[1] != "%2Fgardenio-admins" {
			t.Errorf("grafana.clonk.tv admits %q: with defaultOrgRole Admin only /gardenio-admins may reach Grafana", g[1])
		}
	}
	for _, loc := range []string{"location / {"} {
		_, body, ok := strings.Cut(block, loc)
		if !ok {
			t.Fatalf("grafana.clonk.tv: no %q", loc)
		}
		body, _, _ = strings.Cut(body, "\n      }\n")
		if !strings.Contains(body, "auth_request /oauth2/auth;") {
			t.Error("grafana.clonk.tv location / must run the admins-only auth_request")
		}
		if !regexp.MustCompile(`(?m)^\s*proxy_set_header X-Auth-Request-Email \$edge_email;`).MatchString(body) {
			t.Error("grafana.clonk.tv must overwrite X-Auth-Request-Email with the edge's identity")
		}
	}
}
