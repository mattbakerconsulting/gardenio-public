package tests

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// ka-0000003934: unified-host root ("/") redirect.
//
// The unified-host fanout only claims per-path prefixes (/beads, /kd, /mux,
// …); bare "/" has no matching route. On envs whose landing.ingress is off
// (new prod: prod.gardenio.acme.example routes the apex via AVA+NLB, landing off),
// hitting the bare host dead-ends. unifiedHost.rootRedirect adds a
// Host(`unifiedHost`) && Path(`/`) route → redirectRegex → the beads path,
// mirroring the coopmux "/" → "/mux" root redirect. Slack request from Matt
// Baker: "let's have prod.gardenio.acme.example redirect to the beads viewer".
//
// These tests lock in: (1) default OFF renders nothing (so envs serving a real
// landing page at the unified root are untouched); (2) enabled renders a
// Path(`/`) route + a redirectRegex middleware to /beads; (3) the route beats
// the landing catch-all; (4) target/priority/permanent are honored.

// rootRedirectRoutePriority pulls the priority off the Path(`/`) route block.
func rootRedirectRoutePriority(t *testing.T, out string) (int, bool) {
	t.Helper()
	prioRe := regexp.MustCompile(`(?m)^\s*priority:\s*(\d+)`)
	for b := range strings.SplitSeq(out, "- match:") {
		if !strings.Contains(b, "Path(`/`)") {
			continue
		}
		if m := prioRe.FindStringSubmatch(b); m != nil {
			n, _ := strconv.Atoi(m[1])
			return n, true
		}
		return -1, true
	}
	return 0, false
}

// Default: rootRedirect off → no Path(`/`) route, no root-redirect middleware.
// Envs that serve a landing page at the unified root must be unaffected.
func TestUnifiedHost_RootRedirect_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"unifiedHost.legacyRedirects.enabled=false",
			"beadsViewer.enabled=true",
		)...,
	)
	if strings.Contains(out, "Path(`/`)") {
		t.Error("rootRedirect defaults off — no Path(`/`) route should render")
	}
	if strings.Contains(out, "unified-host-root-redirect") {
		t.Error("rootRedirect defaults off — no root-redirect middleware should render")
	}
}

// Enabled: a Path(`/`) route + a redirectRegex middleware to the beads path.
func TestUnifiedHost_RootRedirect_Enabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"unifiedHost.legacyRedirects.enabled=false",
			"beadsViewer.enabled=true",
			"unifiedHost.rootRedirect.enabled=true",
		)...,
	)
	if !strings.Contains(out, "Path(`/`)") {
		t.Fatal("rootRedirect enabled — expected a Host(`…`) && Path(`/`) route")
	}
	if !strings.Contains(out, "name: test-unified-host-root-redirect") {
		t.Error("rootRedirect enabled — expected the root-redirect Middleware object")
	}
	// The redirect must target the beads path by default (empty target tracks
	// unifiedHost.paths.beads).
	if !strings.Contains(out, `replacement: "https://gardenio.app.e2e.dev.acme.example/beads"`) {
		t.Errorf("rootRedirect default target should be the beads path; render:\n%s", out)
	}
	// Anchored on the bare root (with optional trailing query) so it fires ONLY
	// on "/" — never the fanout paths — while still catching /?utm=... .
	if !strings.Contains(out, `regex: "^https?://gardenio.app.e2e.dev.acme.example/?(\\?.*)?$"`) {
		t.Errorf("rootRedirect regex should anchor on the bare root (query-tolerant); render:\n%s", out)
	}
	// Default is a 302 (permanent:false) so the target stays re-pointable.
	if !strings.Contains(out, "permanent: false") {
		t.Error("rootRedirect should default to a 302 (permanent: false)")
	}
}

// The Path(`/`) route must beat the landing Host()-catch-all (priority 10) so
// it deterministically wins the bare root instead of falling through to landing.
func TestUnifiedHost_RootRedirect_BeatsLanding(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"unifiedHost.legacyRedirects.enabled=false",
			"beadsViewer.enabled=true",
			"unifiedHost.rootRedirect.enabled=true",
		)...,
	)
	p, ok := rootRedirectRoutePriority(t, out)
	if !ok {
		t.Fatal("no Path(`/`) route rendered — cannot verify priority")
	}
	if p <= landingCatchAllPriority {
		t.Errorf("root-redirect route priority %d must be strictly greater than the landing catch-all (%d)", p, landingCatchAllPriority)
	}
}

// target / permanent / priority overrides all flow through.
func TestUnifiedHost_RootRedirect_Overrides(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/unified-host/ingressroute.yaml",
		append(unifiedHostBase(),
			"unifiedHost.legacyRedirects.enabled=false",
			"beadsViewer.enabled=true",
			"unifiedHost.rootRedirect.enabled=true",
			"unifiedHost.rootRedirect.target=/mux",
			"unifiedHost.rootRedirect.permanent=true",
			"unifiedHost.rootRedirect.priority=250",
		)...,
	)
	if !strings.Contains(out, `replacement: "https://gardenio.app.e2e.dev.acme.example/mux"`) {
		t.Errorf("rootRedirect.target override should re-point the replacement; render:\n%s", out)
	}
	if !strings.Contains(out, "permanent: true") {
		t.Error("rootRedirect.permanent=true should render a 301 (permanent: true)")
	}
	if p, ok := rootRedirectRoutePriority(t, out); !ok || p != 250 {
		t.Errorf("rootRedirect.priority=250 should flow through; got %d (found=%v)", p, ok)
	}
}
