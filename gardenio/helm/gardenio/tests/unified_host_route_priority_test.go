package tests

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ka-0000005784: harden unified-host route priorities.
//
// The landing IngressRoute is a Host(`unifiedHost`) CATCH-ALL pinned at an
// explicit priority 10 with the oauth2-proxy forwardauth middleware
// (templates/landing/ingressroute.yaml). A unified-host per-path route whose
// priority is left unset falls back to Traefik's rule-length ordering, which is
// NOT guaranteed to outrank that explicit 10. The shim routes (/kd,/mux) were
// shadow-303'd to oauth2-proxy under exactly this gap (BUG#1, ka-0000004502) and
// only viaShim routes were bumped then; the browser surfaces (/beads,/jira,
// /nats,/grafana) reached edge-auth by rule-length luck alone.
//
// These tests lock in the defense-in-depth fix: EVERY per-path unified-host
// route carries an explicit priority strictly greater than the landing
// catch-all's 10, in both the default (forwardauth+strip) and viaShim modes, so
// ordering is deterministic-by-design rather than rule-length luck.

// landingCatchAllPriority is the explicit priority pinned on the landing
// Host(`unifiedHost`) catch-all in templates/landing/ingressroute.yaml. Every
// per-path route must beat it.
const landingCatchAllPriority = 10

// perPathPrefixes are the six web surfaces fanned out under the unified host.
var perPathPrefixes = []string{"/beads", "/kd", "/mux", "/nats", "/jira", "/grafana"}

// unifiedHostAllPaths brings up every per-path route (default mode: forwardauth
// + strip-prefix, no shim). legacyRedirects are disabled to keep the render to
// just the fanout IngressRoute (the legacy-redirect blocks need per-app ingress
// dicts that are irrelevant to route priority).
func unifiedHostAllPaths() []string {
	return append(unifiedHostBase(),
		"unifiedHost.legacyRedirects.enabled=false",
		"beadsViewer.enabled=true",
		"coopmux.enabled=true",
		"jiraBridge.enabled=true",
		"grafana.enabled=true",
		"unifiedHost.paths.grafana=/grafana",
		"beads.ingress.natsWebsocketRoute.enabled=true",
	)
}

// routePriorities parses the rendered IngressRoute and returns, for each per-path
// route, the explicit priority found on it. A route present in the render but
// missing a priority: line maps to -1 so the caller can flag the regression.
func routePriorities(t *testing.T, out string) map[string]int {
	t.Helper()
	got := map[string]int{}
	// Split on the route separator so each "- match:" block is scanned in
	// isolation and a priority never leaks across route boundaries.
	blocks := strings.Split(out, "- match:")
	prioRe := regexp.MustCompile(`(?m)^\s*priority:\s*(\d+)`)
	for _, b := range blocks {
		for _, pfx := range perPathPrefixes {
			if !strings.Contains(b, "PathPrefix(`"+pfx+"`)") {
				continue
			}
			if m := prioRe.FindStringSubmatch(b); m != nil {
				n, _ := strconv.Atoi(m[1])
				got[pfx] = n
			} else {
				got[pfx] = -1
			}
		}
	}
	return got
}

func assertAllRoutesBeatLanding(t *testing.T, out string) {
	t.Helper()
	got := routePriorities(t, out)
	for _, pfx := range perPathPrefixes {
		p, ok := got[pfx]
		if !ok {
			t.Errorf("unified-host route %s did not render — cannot verify priority", pfx)
			continue
		}
		if p < 0 {
			t.Errorf("unified-host route %s has NO explicit priority — it would fall back to rule-length ordering and could shadow-303 to oauth2-proxy (ka-0000005784)", pfx)
			continue
		}
		if p <= landingCatchAllPriority {
			t.Errorf("unified-host route %s priority %d does not beat the landing catch-all (%d); it must be strictly greater to deterministically outrank it (ka-0000005784)", pfx, p, landingCatchAllPriority)
		}
	}
}

// Default mode: forwardauth + strip-prefix on every route, no shim. Every
// per-path route must still carry an explicit priority > landing(10).
func TestUnifiedHost_AllRoutes_BeatLanding_Default(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		unifiedHostAllPaths()...,
	)
	assertAllRoutesBeatLanding(t, out)
}

// viaShim mode: every route points at the edge-auth shim. Priorities must be
// stamped identically — the fix is not conditional on viaShim participation.
func TestUnifiedHost_AllRoutes_BeatLanding_ViaShim(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostAllPaths(),
			"edgeAuth.enabled=true",
			"edgeAuth.dryRun=true",
			"unifiedHost.viaShim={beads,kd,mux,nats,jira,grafana}",
		)...,
	)
	assertAllRoutesBeatLanding(t, out)
}

// The browser surfaces specifically — /beads,/jira,/nats,/grafana — are the ones
// that previously reached edge-auth by rule-length luck (viaShim /kd,/mux were
// already stamped by ka-0000004502). Assert they now carry explicit priorities
// even when NO path is routed through the shim, which is the whole point of the
// hardening.
func TestUnifiedHost_BrowserSurfaces_HaveExplicitPriority_NoShim(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		unifiedHostAllPaths()...,
	)
	got := routePriorities(t, out)
	for _, pfx := range []string{"/beads", "/jira", "/nats", "/grafana"} {
		if got[pfx] <= landingCatchAllPriority {
			t.Errorf("browser surface %s must carry an explicit priority > %d even with an empty viaShim list, so it never relies on rule-length ordering to beat the landing catch-all (ka-0000005784); got %d", pfx, landingCatchAllPriority, got[pfx])
		}
	}
}

// The priority is operator-tunable via unifiedHost.routePriority and must flow
// through to every per-path route, not just a subset.
func TestUnifiedHost_RoutePriority_Configurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostAllPaths(), "unifiedHost.routePriority=250")...,
	)
	got := routePriorities(t, out)
	for _, pfx := range perPathPrefixes {
		if got[pfx] != 250 {
			t.Errorf("unified-host route %s should honor unifiedHost.routePriority=250; got %d", pfx, got[pfx])
		}
	}
}

// The landing catch-all's priority is the baseline this whole invariant is
// measured against. If someone bumps it, landingCatchAllPriority (and the
// hardening rationale) must be revisited — so pin the rendered value.
func TestLanding_CatchAll_PriorityIsBaseline(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/landing/ingressroute.yaml",
		"traefik.enabled=true",
		"landing.enabled=true",
		"landing.ingress.enabled=true",
		"landing.ingress.host=gardenio.app.e2e.dev.acme.example",
	)
	if !strings.Contains(out, "priority: 10") {
		t.Fatalf("landing catch-all is expected to render priority: %d — if this changed, revisit landingCatchAllPriority and the unified-host route-priority hardening (ka-0000005784). Rendered:\n%s", landingCatchAllPriority, out)
	}
}
