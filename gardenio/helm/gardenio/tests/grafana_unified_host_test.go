package tests

import (
	"strings"
	"testing"
)

// Shared --set flags that bring up the unified host. Grafana participation is
// layered on per-test via the grafana.* and unifiedHost.paths.grafana flags.
func unifiedHostBase() []string {
	return []string{
		"traefik.enabled=true",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
	}
}

// --- Unified-host /grafana route ---

func TestUnifiedHost_GrafanaRoute_AbsentByDefault(t *testing.T) {
	t.Parallel()
	// grafana.enabled defaults false and paths.grafana defaults "" — no route.
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		unifiedHostBase()...,
	)
	if strings.Contains(out, "PathPrefix(`/grafana`)") {
		t.Error("grafana route should NOT render when grafana.enabled=false / paths.grafana empty")
	}
	if strings.Contains(out, "unified-host-grafana-stripprefix") {
		t.Error("grafana stripPrefix middleware should NOT render by default")
	}
}

func TestUnifiedHost_GrafanaRoute_AbsentWhenPathEmptyButEnabled(t *testing.T) {
	t.Parallel()
	// Enabling grafana alone is not enough — the path must be set too.
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(), "grafana.enabled=true")...,
	)
	if strings.Contains(out, "PathPrefix(`/grafana`)") {
		t.Error("grafana route should NOT render when paths.grafana is empty")
	}
}

func TestUnifiedHost_GrafanaRoute_AbsentWhenPathSetButDisabled(t *testing.T) {
	t.Parallel()
	// Setting the path but leaving grafana.enabled=false must not render.
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(), "unifiedHost.paths.grafana=/grafana")...,
	)
	if strings.Contains(out, "PathPrefix(`/grafana`)") {
		t.Error("grafana route should NOT render when grafana.enabled=false")
	}
}

func TestUnifiedHost_GrafanaRoute_PresentWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"grafana.enabled=true",
			"unifiedHost.paths.grafana=/grafana",
		)...,
	)
	if !strings.Contains(out, "PathPrefix(`/grafana`)") {
		t.Fatal("grafana route should render when grafana.enabled and paths.grafana set")
	}
	if !strings.Contains(out, "name: test-grafana") {
		t.Error("grafana route should point at the grafana Service (test-grafana)")
	}
	if !strings.Contains(out, "port: 3000") {
		t.Error("grafana route should target Grafana port 3000")
	}
	if !strings.Contains(out, "unified-host-grafana-stripprefix") {
		t.Error("grafana route should attach the grafana stripPrefix middleware")
	}
}

// Grafana is framed into the beads-viewer dashboard-bead page via a same-origin
// <iframe src="/grafana/d/<uid>?kiosk">. The strict security-headers middleware
// emits X-Frame-Options: DENY + CSP frame-ancestors 'none' at the EDGE, which
// refuses all framing and reproduces the operator "refused to connect" bug
// (ka-0000001147) even after Grafana itself drops X-Frame-Options
// (GF_SECURITY_ALLOW_EMBEDDING, ka-0000003842). The /grafana route must instead
// attach the iframe-allowed variant (SAMEORIGIN + frame-ancestors 'self') so the
// same-origin embed loads. (ka-0000006153)
func TestUnifiedHost_GrafanaRoute_UsesIframeSecurityHeaders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"grafana.enabled=true",
			"unifiedHost.paths.grafana=/grafana",
		)...,
	)
	idx := strings.Index(out, "PathPrefix(`/grafana`)")
	if idx < 0 {
		t.Fatal("grafana route missing")
	}
	// Scope the assertion to the grafana route's middleware block (up to the
	// next route / document separator) so we don't accidentally match the
	// strict middleware attached to OTHER routes (e.g. /beads).
	block := out[idx:]
	if end := strings.Index(block, "\n    - match:"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "test-security-headers-iframe") {
		t.Error("grafana route must attach the iframe-allowed security-headers middleware (SAMEORIGIN/frame-ancestors 'self') so the same-origin embed loads")
	}
	// The strict middleware (frameDeny / frame-ancestors 'none') must NOT be on
	// the grafana route — that is the header that refuses framing.
	if strings.Contains(block, "name: test-security-headers\n") {
		t.Error("grafana route must NOT attach the strict security-headers middleware (X-Frame-Options: DENY refuses the same-origin iframe)")
	}
}

// The route-level test above guards that the /grafana route ATTACHES a
// middleware NAMED "-security-headers-iframe". But that name check passes even
// if the -iframe Middleware CR itself is silently edited back to strict framing
// (frameDeny / frame-ancestors 'none') — which is exactly the header that
// reproduced the "refused to connect" bug (ka-0000006153). The original blocker
// shipped undetected because verification ran through kubectl port-forward,
// bypassing Traefik so the edge-injected header was never seen; the E2E guard
// (tests/e2e/scripts/test-grafana-iframe.sh) still probes Grafana via
// port-forward and likewise cannot observe the edge middleware. This unit test
// closes that gap at the layer that can see it: it asserts the RENDERED VALUES
// of the -iframe Middleware CR are framing-permissive (SAMEORIGIN +
// frame-ancestors 'self') and that it carries NEITHER frameDeny NOR
// frame-ancestors 'none'. It mirrors the sibling guard on the -htmx-eval CR
// (security_headers_htmx_eval_test.go). A silent revert of the -iframe framing
// now fails here instead of only surfacing as a live "refused to connect".
func TestSecurityHeaders_IframeMiddleware_HasPermissiveFraming(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/security-headers.yaml",
		"traefik.enabled=true",
	)
	idx := strings.Index(out, "name: test-security-headers-iframe")
	if idx < 0 {
		t.Fatal("expected the -security-headers-iframe Middleware to render")
	}
	// Scope to this CR only (up to the next document separator) so the strict
	// default / htmx-eval variants can't satisfy the assertions.
	block := out[idx:]
	if end := strings.Index(block, "\n---"); end > 0 {
		block = block[:end]
	}
	// The whole point of the variant: SAMEORIGIN X-Frame-Options so the
	// same-origin iframe loads (Grafana framed into beads-viewer).
	if !strings.Contains(block, `customFrameOptionsValue: "SAMEORIGIN"`) {
		t.Error("iframe middleware must set customFrameOptionsValue: SAMEORIGIN so the same-origin embed loads")
	}
	if !strings.Contains(block, "frame-ancestors 'self'") {
		t.Error("iframe CSP must set frame-ancestors 'self' (permissive same-origin framing)")
	}
	// The strict framing directives that REFUSE the iframe must NOT be present —
	// this is the exact regression (frameDeny / frame-ancestors 'none') that
	// re-introduces "refused to connect" at the edge.
	if strings.Contains(block, "frameDeny: true") {
		t.Error("iframe middleware must NOT set frameDeny: true — X-Frame-Options: DENY refuses the same-origin iframe (ka-0000006153)")
	}
	if strings.Contains(block, "frame-ancestors 'none'") {
		t.Error("iframe CSP must NOT contain frame-ancestors 'none' — it refuses all framing (ka-0000006153)")
	}
}

func TestUnifiedHost_GrafanaRoute_StripPrefixMiddlewarePrefix(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"grafana.enabled=true",
			"unifiedHost.paths.grafana=/grafana",
		)...,
	)
	// The stripPrefix Middleware must carry the configured prefix.
	if !strings.Contains(out, "stripPrefix:") {
		t.Fatal("expected a stripPrefix middleware in the rendered output")
	}
	if !strings.Contains(out, "- /grafana") {
		t.Error("grafana stripPrefix middleware should strip the /grafana prefix")
	}
}

func TestUnifiedHost_GrafanaRoute_ForwardAuthApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"grafana.enabled=true",
			"unifiedHost.paths.grafana=/grafana",
			"unifiedHost.forwardAuth.enabled=true",
		)...,
	)
	// Find the grafana route block and confirm it carries the forwardauth mw.
	idx := strings.Index(out, "PathPrefix(`/grafana`)")
	if idx < 0 {
		t.Fatal("grafana route missing")
	}
	block := out[idx:]
	if !strings.Contains(block[:min(len(block), 400)], "unified-host-forwardauth") {
		t.Error("grafana route should attach the unified forwardAuth middleware when enabled")
	}
}

func TestUnifiedHost_GrafanaRoute_ViaShim(t *testing.T) {
	t.Parallel()
	// When grafana is routed through the edge-auth shim, it points at the shim
	// Service, gets a priority, and gets NO stripPrefix (the shim strips).
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"grafana.enabled=true",
			"unifiedHost.paths.grafana=/grafana",
			"unifiedHost.viaShim={grafana}",
			"edgeAuth.enabled=true",
			"edgeAuth.host=cli.gardenio.app.e2e.dev.acme.example",
			// dryRun ships the shim surface dark — no HMAC key source needed
			// to render, which keeps this a pure template-shape assertion.
			"edgeAuth.dryRun=true",
		)...,
	)
	idx := strings.Index(out, "PathPrefix(`/grafana`)")
	if idx < 0 {
		t.Fatal("grafana route missing")
	}
	block := out[idx:min(len(out), idx+400)]
	if !strings.Contains(block, "edge-auth") {
		t.Error("viaShim grafana route should point at the edge-auth Service")
	}
	if strings.Contains(block, "grafana-stripprefix") {
		t.Error("viaShim grafana route should NOT attach a Traefik stripPrefix")
	}
}

// --- Grafana deployment subpath + auth-proxy ---

func TestGrafana_SubPath_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	for _, s := range []string{
		"GF_SERVER_ROOT_URL",
		"GF_SERVER_SERVE_FROM_SUB_PATH",
		"GF_AUTH_PROXY_ENABLED",
	} {
		if strings.Contains(out, s) {
			t.Errorf("%s should NOT be present by default (additive, opt-in)", s)
		}
	}
	// Probes stay at the root path when subpath serving is off.
	if !strings.Contains(out, "path: /api/health") {
		t.Error("health probe should default to /api/health")
	}
}

func TestGrafana_SubPath_RootURLDerivedFromUnifiedHost(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.serveFromSubPath=true",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"unifiedHost.paths.grafana=/grafana",
	)
	if !strings.Contains(out, "GF_SERVER_ROOT_URL") {
		t.Fatal("GF_SERVER_ROOT_URL should render when serveFromSubPath is on")
	}
	if !strings.Contains(out, "https://gardenio.app.e2e.dev.acme.example/grafana") {
		t.Error("ROOT_URL should derive from unifiedHost.host + paths.grafana")
	}
	if !strings.Contains(out, `GF_SERVER_SERVE_FROM_SUB_PATH`) {
		t.Error("GF_SERVER_SERVE_FROM_SUB_PATH should render")
	}
	// Probes follow the subpath so they pass once Grafana serves under it.
	if !strings.Contains(out, "path: /grafana/api/health") {
		t.Error("health probe should follow the subpath: /grafana/api/health")
	}
}

func TestGrafana_SubPath_ExplicitRootURLWins(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.serveFromSubPath=true",
		"grafana.rootURL=https://grafana.example.com/g",
		// Unified host present but explicit rootURL must take precedence.
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"unifiedHost.paths.grafana=/grafana",
	)
	if !strings.Contains(out, "https://grafana.example.com/g") {
		t.Error("explicit grafana.rootURL should win over the derived URL")
	}
	if strings.Contains(out, "https://gardenio.app.e2e.dev.acme.example/grafana") {
		t.Error("derived URL should not appear when rootURL is set explicitly")
	}
	if !strings.Contains(out, "path: /g/api/health") {
		t.Error("probe should use the explicit rootURL subpath: /g/api/health")
	}
}

// Subpath mode (bug ka-0000001440): when grafana.serveFromSubPath=true, Grafana
// owns the /grafana prefix end-to-end and expects /grafana/* to arrive INTACT,
// so the unified-host route must NOT attach the stripPrefix middleware — neither
// on the route nor as a rendered Middleware object. Stripping would feed the pod
// /api/... while it serves everything under /grafana/..., 404ing every route.
func TestUnifiedHost_GrafanaRoute_NoStripWhenServeFromSubPath(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"grafana.enabled=true",
			"unifiedHost.paths.grafana=/grafana",
			"grafana.serveFromSubPath=true",
		)...,
	)
	// The route must still exist...
	if !strings.Contains(out, "PathPrefix(`/grafana`)") {
		t.Fatal("grafana route should still render in subpath mode")
	}
	// ...but NO stripPrefix middleware (reference or definition) anywhere.
	if strings.Contains(out, "unified-host-grafana-stripprefix") {
		t.Error("grafana stripPrefix middleware must NOT render in subpath mode (Grafana owns /grafana intact)")
	}
}

func TestGrafana_AuthProxy_EnabledRendersHeaders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.auth.proxy.enabled=true",
	)
	want := map[string]string{
		"GF_AUTH_PROXY_ENABLED":         "true",
		"GF_AUTH_PROXY_HEADER_NAME":     "X-Auth-Request-Email",
		"GF_AUTH_PROXY_HEADER_PROPERTY": "username",
		"GF_AUTH_PROXY_AUTO_SIGN_UP":    "true",
		"GF_AUTH_PROXY_HEADERS":         "Groups:X-Auth-Request-Groups",
		"GF_USERS_AUTO_ASSIGN_ORG_ROLE": "Viewer",
		"GF_AUTH_ANONYMOUS_ENABLED":     "false",
	}
	for k, v := range want {
		if !strings.Contains(out, k) {
			t.Errorf("expected env %s when auth-proxy enabled", k)
		}
		if !strings.Contains(out, v) {
			t.Errorf("expected value %q (for %s) in rendered deployment", v, k)
		}
	}
}

func TestGrafana_AuthProxy_Whitelist(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.auth.proxy.enabled=true",
	)
	if !strings.Contains(out, "GF_AUTH_PROXY_WHITELIST") {
		t.Fatal("GF_AUTH_PROXY_WHITELIST should render so only in-cluster sources are trusted")
	}
	// Default whitelist covers RFC1918 private ranges (Traefik pod IP).
	if !strings.Contains(out, "10.0.0.0/8") {
		t.Error("default whitelist should include the 10.0.0.0/8 private range")
	}
}

func TestGrafana_AuthProxy_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	for _, s := range []string{
		"GF_AUTH_PROXY_ENABLED",
		"GF_AUTH_PROXY_HEADER_NAME",
		"GF_USERS_AUTO_ASSIGN_ORG_ROLE",
		"GF_AUTH_ANONYMOUS_ENABLED",
	} {
		if strings.Contains(out, s) {
			t.Errorf("%s should NOT render when auth.proxy.enabled=false", s)
		}
	}
}

func TestGrafana_AuthProxy_DisableLoginForm(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.auth.proxy.enabled=true",
		"grafana.auth.disableLoginForm=true",
	)
	if !strings.Contains(out, "GF_AUTH_DISABLE_LOGIN_FORM") {
		t.Fatal("GF_AUTH_DISABLE_LOGIN_FORM should render under auth-proxy")
	}
	// The break-glass admin form is hidden only when explicitly requested.
	idx := strings.Index(out, "GF_AUTH_DISABLE_LOGIN_FORM")
	if !strings.Contains(out[idx:min(len(out), idx+80)], `"true"`) {
		t.Error("GF_AUTH_DISABLE_LOGIN_FORM should be true when requested")
	}
}
