package tests

import (
	"strings"
	"testing"
)

// gd-6vUzU7Fhpy: coopmux and the controller must read the same land token key
// (COOP_MUX_LAND_TOKEN_KEY_FILE) from the one chart-owned Secret, or a land's
// coopmux bearer minted by the controller never verifies at coopmux.

const landTokenKeyEnv = "name: COOP_MUX_LAND_TOKEN_KEY_FILE\n              value: /var/run/secrets/coopmux-land-token-key/key"

func TestCoopmuxLandTokenKey_Enabled(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{"templates/coopmux/deployment.yaml", "templates/controller/deployment.yaml"} {
		out := helmTemplate(t, tmpl,
			"agents.enabled=true",
			"coopmux.enabled=true",
			"coopmux.landTokens.enabled=true",
		)
		if !strings.Contains(out, landTokenKeyEnv) {
			t.Errorf("%s: missing COOP_MUX_LAND_TOKEN_KEY_FILE env", tmpl)
		}
		if !strings.Contains(out, "mountPath: /var/run/secrets/coopmux-land-token-key") {
			t.Errorf("%s: missing land token key mount", tmpl)
		}
		if !strings.Contains(out, "-coopmux-land-token-key\n            defaultMode: 0440") {
			t.Errorf("%s: missing land token key volume", tmpl)
		}
	}

	out := helmTemplate(t, "templates/coopmux/land-token-key-secret.yaml",
		"coopmux.landTokens.enabled=true",
	)
	for _, want := range []string{"-coopmux-land-token-key\n  namespace:", `"helm.sh/resource-policy": keep`, "  key: "} {
		if !strings.Contains(out, want) {
			t.Errorf("land token key Secret: missing %q", want)
		}
	}
}

func TestCoopmuxLandTokenKey_DisabledByDefault(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{"templates/coopmux/deployment.yaml", "templates/controller/deployment.yaml"} {
		out := helmTemplate(t, tmpl, "agents.enabled=true", "coopmux.enabled=true")
		if strings.Contains(out, "coopmux-land-token-key") || strings.Contains(out, "COOP_MUX_LAND_TOKEN_KEY_FILE") {
			t.Errorf("%s: land token key wired while coopmux.landTokens.enabled is off", tmpl)
		}
	}
}
