package tests

import (
	"strings"
	"testing"
)

// beads-viewer serves server-rendered htmx pages. htmx compiles hx-on /
// hx-trigger `[filter]` / hx-vals='js:{…}' attributes through `new Function()`,
// which the browser blocks under a CSP lacking script-src 'unsafe-eval'. That
// produced the EvalError reported on /plans (ka-0000006016). The fix scopes an
// eval-enabled CSP variant to the /beads route ONLY, leaving the strict no-eval
// default on the kd API, coopmux, nats, and jira routes.

// routeBlock returns the middleware region of a unified-host route, bounded to
// the next route match so assertions never leak into a sibling route's block.
func routeBlock(t *testing.T, out, pathPrefix string) string {
	t.Helper()
	marker := "PathPrefix(`" + pathPrefix + "`)"
	idx := strings.Index(out, marker)
	if idx < 0 {
		t.Fatalf("route %s missing from rendered output", pathPrefix)
	}
	block := out[idx:]
	if end := strings.Index(block, "\n    - match:"); end > 0 {
		block = block[:end]
	}
	return block
}

func TestSecurityHeaders_HtmxEvalMiddleware_HasUnsafeEvalButStrictFraming(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/security-headers.yaml",
		"traefik.enabled=true",
	)
	idx := strings.Index(out, "name: test-security-headers-htmx-eval")
	if idx < 0 {
		t.Fatal("expected the -security-headers-htmx-eval Middleware to render")
	}
	block := out[idx:]
	if end := strings.Index(block, "\n---"); end > 0 {
		block = block[:end]
	}
	// The whole point: script-src carries 'unsafe-eval' so htmx new Function() works.
	if !strings.Contains(block, "script-src 'self' 'unsafe-inline' 'unsafe-eval'") {
		t.Error("htmx-eval CSP must add 'unsafe-eval' to script-src")
	}
	// Framing stays strict — we must NOT weaken clickjacking protection the way
	// the -iframe variant does (SAMEORIGIN + frame-ancestors 'self').
	if !strings.Contains(block, "frameDeny: true") {
		t.Error("htmx-eval middleware must keep frameDeny: true (strict framing)")
	}
	if !strings.Contains(block, "frame-ancestors 'none'") {
		t.Error("htmx-eval CSP must keep frame-ancestors 'none'")
	}
	if strings.Contains(block, "frame-ancestors 'self'") || strings.Contains(block, "SAMEORIGIN") {
		t.Error("htmx-eval middleware must NOT relax framing like the -iframe variant")
	}
}

func TestSecurityHeaders_DefaultMiddleware_HasNoUnsafeEval(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/security-headers.yaml",
		"traefik.enabled=true",
	)
	// Scope to the default -security-headers Middleware object (up to the next
	// document separator) so the -iframe / -htmx-eval variants don't leak in.
	idx := strings.Index(out, "name: test-security-headers\n")
	if idx < 0 {
		t.Fatal("expected the default -security-headers Middleware to render")
	}
	block := out[idx:]
	if end := strings.Index(block, "\n---"); end > 0 {
		block = block[:end]
	}
	if strings.Contains(block, "unsafe-eval") {
		t.Error("default -security-headers CSP must NOT contain 'unsafe-eval' (strict, no-eval)")
	}
}

func TestUnifiedHost_BeadsRoute_UsesHtmxEvalSecurityHeaders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(), "beadsViewer.enabled=true")...,
	)
	block := routeBlock(t, out, "/beads")
	if !strings.Contains(block, "test-security-headers-htmx-eval") {
		t.Error("/beads route must attach the htmx-eval security-headers middleware (unsafe-eval for htmx new Function())")
	}
	// The strict default middleware must NOT be the one on /beads — that is the
	// no-eval header that reproduced the EvalError.
	if strings.Contains(block, "name: test-security-headers\n") {
		t.Error("/beads route must NOT attach the strict default -security-headers middleware")
	}
}

func TestUnifiedHost_NonBeadsRoutes_KeepStrictSecurityHeaders(t *testing.T) {
	t.Parallel()
	// The eval relaxation must be scoped to /beads. The kd daemon API and the
	// coopmux dashboard (neither uses htmx) must stay on the strict default CSP.
	// The kd route (/kd) is the most important: it fronts the beads daemon HTTP
	// API and must never be granted 'unsafe-eval'.
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"beadsViewer.enabled=true",
			"coopmux.enabled=true",
		)...,
	)
	for _, p := range []string{"/kd", "/mux"} {
		block := routeBlock(t, out, p)
		if !strings.Contains(block, "name: test-security-headers\n") {
			t.Errorf("%s route must attach the strict default -security-headers middleware", p)
		}
		if strings.Contains(block, "test-security-headers-htmx-eval") {
			t.Errorf("%s route must NOT attach the eval-enabled middleware (only /beads needs it)", p)
		}
	}
}
