package tests

import (
	"strings"
	"testing"
)

// T6a-viewer (ka-0000005900): unifiedHost.beadsForwardAuth gates ONLY the
// unified-host /beads route with the oauth2-proxy forwardauth (<release>-
// forwardauth) — the interactive-login edge coopmux/landing/roles already ride —
// so the beads-viewer can run in devAuthBypass (pod trusts the edge-stamped
// X-Auth-Request-Email) without exposing an un-authed route, and WITHOUT gating
// /kd or /mux (which carry their own daemon-side auth and would break under an
// interactive-login 302).

// beadsForwardAuthOn brings up the unified-host /beads route with the beads-only
// oauth2-proxy gate armed: beads route present + the middleware's gate
// (keycloak.enabled && keycloak.forwardAuth.enabled) satisfied.
func beadsForwardAuthOn() []string {
	return append(unifiedHostBase(),
		"beadsViewer.enabled=true",
		"unifiedHost.paths.beads=/beads",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"unifiedHost.beadsForwardAuth.enabled=true",
	)
}

// unifiedRouteBlock returns the single `- match:` route block whose match line
// contains the given PathPrefix, so a middleware assertion cannot bleed across
// routes.
func unifiedRouteBlock(out, pathPrefix string) string {
	for b := range strings.SplitSeq(out, "- match:") {
		if strings.Contains(b, "PathPrefix(`"+pathPrefix+"`)") {
			return b
		}
	}
	return ""
}

// Default off: the /beads route renders (viewer enabled, path set) but carries
// NO oauth2-proxy forwardauth — every non-opted-in overlay is unchanged.
func TestUnifiedHost_BeadsForwardAuth_AbsentByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"beadsViewer.enabled=true",
			"unifiedHost.paths.beads=/beads",
			"keycloak.enabled=true",
			"keycloak.forwardAuth.enabled=true",
		)...,
	)
	beads := unifiedRouteBlock(out, "/beads")
	if beads == "" {
		t.Fatalf("/beads route must render when beadsViewer.enabled + paths.beads set; got:\n%s", out)
	}
	if oauth2ProxyMiddlewareRE.MatchString(beads) {
		t.Errorf("/beads route must NOT carry the oauth2-proxy forwardauth when beadsForwardAuth is off (default); got:\n%s", beads)
	}
}

// Enabled: the /beads route carries <release>-forwardauth; /kd and /mux do not.
func TestUnifiedHost_BeadsForwardAuth_EnabledGatesBeadsOnly(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(beadsForwardAuthOn(),
			"coopmux.enabled=true",
			"unifiedHost.paths.kd=/kd",
			"unifiedHost.paths.mux=/mux",
		)...,
	)
	beads := unifiedRouteBlock(out, "/beads")
	if beads == "" {
		t.Fatalf("/beads route did not render; got:\n%s", out)
	}
	if !oauth2ProxyMiddlewareRE.MatchString(beads) {
		t.Errorf("/beads route must carry the oauth2-proxy forwardauth when beadsForwardAuth.enabled; got:\n%s", beads)
	}
	for _, other := range []string{"/kd", "/mux"} {
		blk := unifiedRouteBlock(out, other)
		if blk == "" {
			t.Fatalf("%s route did not render; got:\n%s", other, out)
		}
		if oauth2ProxyMiddlewareRE.MatchString(blk) {
			t.Errorf("%s route must NOT be gated by beadsForwardAuth (it carries daemon-side auth and would break under an interactive-login 302); got:\n%s", other, blk)
		}
	}
}

// Fail fast: beadsForwardAuth on but the oauth2-proxy middleware is not rendered
// (keycloak.forwardAuth off) would reference a non-existent Middleware → Traefik
// 500s every /beads request. The template must refuse at install time.
func TestUnifiedHost_BeadsForwardAuth_FailsWithoutForwardauthMiddleware(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, "templates/unified-host/ingressroute.yaml",
		"--set", "traefik.enabled=true",
		"--set", "unifiedHost.enabled=true",
		"--set", "unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"--set", "beadsViewer.enabled=true",
		"--set", "unifiedHost.paths.beads=/beads",
		"--set", "unifiedHost.beadsForwardAuth.enabled=true",
		"--set", "keycloak.forwardAuth.enabled=false",
	)
	if !strings.Contains(out, "unifiedHost.beadsForwardAuth.enabled is true but the oauth2-proxy forwardauth middleware is not rendered") {
		t.Errorf("expected the beadsForwardAuth-without-middleware fail-fast message; got:\n%s", out)
	}
}

// Fail fast: the /beads route cannot carry BOTH the AVA verifier
// (unifiedHost.forwardAuth) and the oauth2-proxy login gate (beadsForwardAuth) —
// they set overlapping X-Auth-Request-* headers from two verifiers.
func TestUnifiedHost_BeadsForwardAuth_FailsWithAvaForwardAuth(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, "templates/unified-host/ingressroute.yaml",
		"--set", "traefik.enabled=true",
		"--set", "unifiedHost.enabled=true",
		"--set", "unifiedHost.host=gardenio.app.e2e.dev.acme.example",
		"--set", "beadsViewer.enabled=true",
		"--set", "unifiedHost.paths.beads=/beads",
		"--set", "keycloak.enabled=true",
		"--set", "keycloak.forwardAuth.enabled=true",
		"--set", "unifiedHost.beadsForwardAuth.enabled=true",
		"--set", "unifiedHost.forwardAuth.enabled=true",
	)
	if !strings.Contains(out, "BOTH true") {
		t.Errorf("expected the AVA+oauth2-proxy double-auth fail-fast message; got:\n%s", out)
	}
}
