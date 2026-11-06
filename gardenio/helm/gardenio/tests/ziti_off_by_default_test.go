package tests

import (
	"strings"
	"testing"
)

// OpenZiti is off in the live stack (owner 2026-10-13: keep it as a
// possibility for configuration, do not use it today; plan ziti-to-keycloak,
// S9b gd-qstfURyZz9). The k3s overlay must therefore render nothing ziti-shaped
// from the gardenio chart; the manifests that remain live in the opt-in
// gardenio/helm/ziti chart, which tests/render_test.sh covers.
//
// What counts as "ziti-shaped" is deliberately narrow: the objects and env
// this chart renders only for the ziti access path. The overlay still names the
// `ziti` namespace in its NetworkPolicy/IMDS lists and a *.ziti Grafana URL;
// those are other tasks' to retire and are not asserted here.

const zitiOffOverlay = "../../../deploy-values/gardenio-k3s.yaml"

func TestZitiOffByDefault_LiveOverlayRendersNoZitiAccess(t *testing.T) {
	t.Parallel()
	out := helmTemplateFullNamespaced(t, "gardenio", []string{zitiOffOverlay})

	for _, forbidden := range []string{
		// kd/gb in-process dial settings must not be injected into any pod.
		"GARDENIO_ZITI_",
		// The controller's per-pod overlay access wiring (agents.zitiAccess).
		"ZITI_ACCESS_",
		"ZITI_TUNNELER_IMAGE",
		// The Lambda microVM broker env (agents.microvm.zitiBroker).
		"MICROVM_ZITI_",
		// Every object the zitiBrowser block renders.
		"templates/ziti-browser/",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("live overlay must not render %q (ziti is off by default)", forbidden)
		}
	}
}

// Positive controls: the assertions above only mean something if the same
// strings DO appear when the feature is switched on.
func TestZitiOffByDefault_ZitiAccessControlRendersControllerEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplateFullNamespaced(t, "gardenio", []string{zitiOffOverlay},
		"agents.zitiAccess.enabled=true",
		"agents.zitiAccess.brokerURL=http://x",
	)
	if !strings.Contains(out, "ZITI_ACCESS_BROKER_URL") {
		t.Fatal("agents.zitiAccess.enabled=true must render the controller's ZITI_ACCESS_BROKER_URL env")
	}
}

func TestZitiOffByDefault_ZitiBrowserControlRendersObjects(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/ziti-browser/ingressroute.yaml", zitiBrowserFlags()...)
	if !strings.Contains(out, "templates/ziti-browser/ingressroute.yaml") {
		t.Fatalf("zitiBrowser.enabled=true must render ziti-browser objects:\n%s", out)
	}
}
