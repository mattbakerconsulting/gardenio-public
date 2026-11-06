package tests

import (
	"strings"
	"testing"
)

// GARDENIO_DEFAULT_HOST wiring on the agent-controller deployment (ka-0000002401,
// R3 §4.4/§8.4). The controller forwards the chart-derived default gardenio host
// onto every agent pod so the in-pod gb CLI derives its zero-config
// daemon/viewer hosts from the unified host instead of the COMPILED default
// baked into the binary. The value comes from the gardenio.link.defaultGardenioHost
// helper, which applies R3's three-tier precedence (explicit override >
// unified-derived > "").
//
// This env var is COUPLED to the gated default-URL cutover (ka-0000004747): it
// is emitted ONLY when the helper yields a non-empty host, so with unifiedHost
// off and no explicit override the block is absent and gb keeps its compiled
// fallback — today's behaviour. These tests pin the inert default, the
// unified-derived value, and the explicit-override escape hatch.

const gardenioDefaultHostTpl = "templates/controller/deployment.yaml"

// TestGardenioDefaultHost_InertByDefault: with unifiedHost off and no explicit
// agents.defaultGardenioHost, the helper yields "" and the env var must NOT
// render — gb keeps its compiled default (gated cutover, ka-0000004747).
func TestGardenioDefaultHost_InertByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, gardenioDefaultHostTpl, "agents.enabled=true")
	if strings.Contains(out, "GARDENIO_DEFAULT_HOST") {
		t.Errorf("GARDENIO_DEFAULT_HOST must be absent by default (gated); got:\n%s", out)
	}
}

// TestGardenioDefaultHost_UnifiedDerived: flipping unifiedHost on derives the bare
// host (no scheme/path) from unifiedHost.host via the helper.
func TestGardenioDefaultHost_UnifiedDerived(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, gardenioDefaultHostTpl,
		"agents.enabled=true",
		"traefik.enabled=true",
		"unifiedHost.enabled=true",
		"unifiedHost.host=gardenio.app.e2e.dev.acme.example",
	)
	for _, want := range []string{
		"name: GARDENIO_DEFAULT_HOST",
		`value: "gardenio.app.e2e.dev.acme.example"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("unified-derived render must contain %q; got:\n%s", want, out)
		}
	}
}

// TestGardenioDefaultHost_ExplicitOverride: an explicit agents.defaultGardenioHost
// wins (tier-1) even with unifiedHost off, so an operator can correct gb's
// fallback host per-env WITHOUT enabling the unified cutover.
func TestGardenioDefaultHost_ExplicitOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, gardenioDefaultHostTpl,
		"agents.enabled=true",
		"agents.defaultGardenioHost=prod.gardenio.acme.example",
	)
	if !strings.Contains(out, "name: GARDENIO_DEFAULT_HOST") {
		t.Fatalf("explicit override must render the env key; got:\n%s", out)
	}
	if !strings.Contains(out, `value: "prod.gardenio.acme.example"`) {
		t.Errorf("explicit override must render verbatim; got:\n%s", out)
	}
}
