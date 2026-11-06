package tests

import (
	"strings"
	"testing"
)

// gd-yDXrcBd0Uq: gardenio-coopmux:9800 had no ingress policy on the k3s overlay
// (networkPolicies.enabled=false), so any pod could reach it. The coopmux ingress
// allowlist now renders from coopmux.networkPolicy.enabled, independent of the
// namespace default-deny layer.

const coopmuxNPTemplate = "templates/coopmux/networkpolicy.yaml"

// The policy must render with networkPolicies.enabled left false (k3s posture),
// isolate only the coopmux pod, and allow every verified caller.
func TestCoopmuxNetworkPolicy_RendersWithoutNamespaceDefaultDeny(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, coopmuxNPTemplate,
		"coopmux.enabled=true",
		"networkPolicies.enabled=false",
		"vmagent.enabled=true",
	)
	if !strings.Contains(out, "kind: NetworkPolicy") || !strings.Contains(out, "-coopmux-ingress") {
		t.Fatalf("coopmux ingress NetworkPolicy must render with networkPolicies.enabled=false, got:\n%s", out)
	}
	if !strings.Contains(out, "app.kubernetes.io/component: coopmux") {
		t.Errorf("policy must select the coopmux pod:\n%s", out)
	}
	if strings.Contains(out, "podSelector: {}") {
		t.Errorf("policy must not select every pod in the namespace:\n%s", out)
	}
	if strings.Contains(out, "Egress") {
		t.Errorf("ingress-only policy must not touch egress:\n%s", out)
	}
	for _, want := range []string{
		"app.kubernetes.io/component: agents",       // controller
		"gardenio.io/agent",                         // agent pods
		"app.kubernetes.io/component: beads-viewer", // terminal first-paint
		"app.kubernetes.io/component: vmagent",      // /metrics
		"app: viewer-authproxy",                     // mux.clonk.tv front door
		"app: ziti-host",                            // microVM ziti service host
		"port: 9800",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("coopmux ingress policy missing caller %q:\n%s", want, out)
		}
	}
}

// Nothing may be allowed from outside the namespace unless traefik is configured:
// a bare namespaceSelector / ipBlock / empty-from rule is the open door.
func TestCoopmuxNetworkPolicy_NoClusterWideAllow(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, coopmuxNPTemplate, "coopmux.enabled=true")
	for _, bad := range []string{"namespaceSelector", "ipBlock", "- {}", "from: []"} {
		if strings.Contains(out, bad) {
			t.Errorf("default policy must not contain %q (would admit unlisted pods):\n%s", bad, out)
		}
	}
}

func TestCoopmuxNetworkPolicy_TraefikOnlyWhenConfigured(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, coopmuxNPTemplate,
		"coopmux.enabled=true",
		"networkPolicies.traefik.namespaceSelector.kubernetes\\.io/metadata\\.name=traefik",
		"networkPolicies.traefik.podSelector.app\\.kubernetes\\.io/name=traefik",
	)
	if !strings.Contains(out, "namespaceSelector") || !strings.Contains(out, "app.kubernetes.io/name: traefik") {
		t.Errorf("configured traefik selector must be allowed:\n%s", out)
	}
}

func TestCoopmuxNetworkPolicy_EdgeAuthAndExtraFrom(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, coopmuxNPTemplate,
		"coopmux.enabled=true",
		"edgeAuth.enabled=true",
		"edgeAuth.dryRun=true",
		"coopmux.networkPolicy.extraFrom[0].app=custom-caller",
	)
	if !strings.Contains(out, "app.kubernetes.io/component: edge-auth") {
		t.Errorf("edge-auth proxies /mux to coopmux; it must be allowed when enabled:\n%s", out)
	}
	if !strings.Contains(out, "app: custom-caller") {
		t.Errorf("coopmux.networkPolicy.extraFrom must be honoured:\n%s", out)
	}
	off := helmTemplate(t, coopmuxNPTemplate, "coopmux.enabled=true")
	if strings.Contains(off, "edge-auth") {
		t.Errorf("edge-auth must not be allowed when edgeAuth is disabled:\n%s", off)
	}
}

func TestCoopmuxNetworkPolicy_OptOutAndCoopmuxDisabled(t *testing.T) {
	t.Parallel()
	// helm errors on --show-only for a template that renders nothing, so render
	// the full chart.
	out := helmTemplateFull(t, []string{"values-test.yaml"},
		"coopmux.enabled=true",
		"coopmux.networkPolicy.enabled=false",
	)
	if strings.Contains(out, "-coopmux-ingress") {
		t.Error("coopmux.networkPolicy.enabled=false must remove the policy")
	}
	out = helmTemplateFull(t, []string{"values-test.yaml"})
	if strings.Contains(out, "-coopmux-ingress") {
		t.Error("no coopmux policy when coopmux itself is disabled")
	}
}

// With the whole layer on, the policy must still render exactly once (it moved
// out of networkpolicies.yaml; a duplicate name would fail the install).
func TestCoopmuxNetworkPolicy_NoDuplicateWithNamespaceLayer(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"},
		"coopmux.enabled=true",
		"networkPolicies.enabled=true",
	)
	if n := strings.Count(out, "name: test-coopmux-ingress\n"); n != 1 {
		t.Errorf("expected exactly 1 coopmux-ingress policy, got %d", n)
	}
}
