package tests

import (
	"strings"
	"testing"
)

// Plan ziti-to-keycloak S2a (gd-hRltljHGon): ingress NetworkPolicies so only
// the edge (plus named in-cluster callers, plus ziti-host) can reach the apps
// that have no auth of their own.

const edgeBackendsTemplate = "templates/networkpolicies/edge-backends.yaml"

func edgeBackendsOn() []string {
	return []string{
		"edgeBackends.enabled=true",
		"keycloak.enabled=true",
		"keycloak.forwardAuth.enabled=true",
		"keycloak.forwardAuth.oidcIssuerUrl=https://auth.example.test/auth/realms/gardenio",
		"grafana.enabled=true",
		"beadsViewer.enabled=true",
	}
}

func edgePolicyDocs(out string) map[string]string {
	docs := map[string]string{}
	for doc := range strings.SplitSeq(out, "\n---") {
		for _, n := range []string{"edge-forwardauth", "edge-grafana", "edge-beads-viewer"} {
			if strings.Contains(doc, "kind: NetworkPolicy") && strings.Contains(doc, "-"+n+"\n") {
				docs[n] = doc
			}
		}
	}
	return docs
}

// ingressFromBlock returns the text of the `from:` list of the policy (up to
// `ports:`), so assertions are scoped to peers.
func ingressFromBlock(doc string) string {
	_, rest, ok := strings.Cut(doc, "from:")
	if !ok {
		return ""
	}
	before, _, _ := strings.Cut(rest, "ports:")
	return before
}

func TestEdgeBackends_OffByDefaultRendersNothing(t *testing.T) {
	t.Parallel()
	// --show-only on a template that renders nothing makes helm exit non-zero;
	// either that or an empty render is "nothing rendered".
	out, _ := helmTemplateAllowFail(t, edgeBackendsTemplate, "keycloak.enabled=true", "grafana.enabled=true", "beadsViewer.enabled=true")
	if strings.Contains(out, "kind: NetworkPolicy") {
		t.Errorf("edgeBackends.enabled=false must render nothing:\n%s", out)
	}
}

func TestEdgeBackends_OnRendersThreePolicies(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, edgeBackendsTemplate, edgeBackendsOn()...)
	docs := edgePolicyDocs(out)
	if len(docs) != 3 {
		t.Fatalf("want 3 policies, got %d:\n%s", len(docs), out)
	}
	for name, doc := range docs {
		if !strings.Contains(doc, "policyTypes:\n    - Ingress") || strings.Contains(doc, "Egress") {
			t.Errorf("%s must be ingress-only:\n%s", name, doc)
		}
	}
}

func TestEdgeBackends_EveryPolicyAdmitsEdgeNamespaceNeverEverything(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, edgeBackendsTemplate, edgeBackendsOn()...)
	docs := edgePolicyDocs(out)
	// forwardauth and grafana admit the edge namespace; the viewer does not
	// (it is reached through viewer-authproxy / ziti-host).
	for _, n := range []string{"edge-forwardauth", "edge-grafana"} {
		if !strings.Contains(ingressFromBlock(docs[n]), `kubernetes.io/metadata.name: "public-edge"`) {
			t.Errorf("%s must admit namespace public-edge:\n%s", n, docs[n])
		}
	}
	for n, doc := range docs {
		// Negative control: an empty `from` or `{}` peer admits every pod.
		for _, bad := range []string{"from: []", "from:\n      ports:", "- {}", "podSelector: {}", "namespaceSelector: {}", "- from: {}"} {
			if strings.Contains(doc, bad) {
				t.Errorf("%s contains an admit-everything shape %q:\n%s", n, bad, doc)
			}
		}
		if !strings.Contains(doc, "ports:") {
			t.Errorf("%s must restrict ports:\n%s", n, doc)
		}
	}
}

func TestEdgeBackends_ForwardAuthOnlyEdgeNamespaceByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, edgeBackendsTemplate, edgeBackendsOn()...)
	from := ingressFromBlock(edgePolicyDocs(out)["edge-forwardauth"])
	if strings.Contains(from, "podSelector") {
		t.Errorf("forwardauth must admit ONLY the public-edge namespace by default:\n%s", from)
	}
	if !strings.Contains(edgePolicyDocs(out)["edge-forwardauth"], "port: 4180") {
		t.Error("forwardauth policy must target port 4180")
	}
}

func TestEdgeBackends_ForwardAuthCallersOptIn(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, edgeBackendsTemplate,
		append(edgeBackendsOn(), "edgeBackends.forwardauthCallers[0].matchLabels.app=viewer-authproxy")...)
	from := ingressFromBlock(edgePolicyDocs(out)["edge-forwardauth"])
	if !strings.Contains(from, "app: viewer-authproxy") {
		t.Errorf("forwardauthCallers entry must be admitted:\n%s", from)
	}
}

func TestEdgeBackends_ZitiHostSelectorHonoured(t *testing.T) {
	t.Parallel()
	on := helmTemplate(t, edgeBackendsTemplate, edgeBackendsOn()...)
	for _, n := range []string{"edge-grafana", "edge-beads-viewer"} {
		if !strings.Contains(edgePolicyDocs(on)[n], "app: ziti-host") {
			t.Errorf("%s must admit app: ziti-host while zitiHostSelector is set", n)
		}
	}
	off := helmTemplate(t, edgeBackendsTemplate, append(edgeBackendsOn(), "edgeBackends.zitiHostSelector=null")...)
	for n, doc := range edgePolicyDocs(off) {
		if strings.Contains(doc, "ziti-host") {
			t.Errorf("%s must not mention ziti-host when zitiHostSelector is empty:\n%s", n, doc)
		}
	}
}

func TestEdgeBackends_ViewerPeers(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, edgeBackendsTemplate, edgeBackendsOn()...)
	from := ingressFromBlock(edgePolicyDocs(out)["edge-beads-viewer"])
	if !strings.Contains(from, "app: viewer-authproxy") {
		t.Errorf("viewer policy must admit viewer-authproxy:\n%s", from)
	}
	if strings.Contains(from, "namespaceSelector") {
		t.Errorf("viewer policy must not admit a whole namespace:\n%s", from)
	}
}

func TestEdgeBackends_CustomCallers(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, edgeBackendsTemplate,
		append(edgeBackendsOn(),
			"edgeBackends.grafanaCallers[0].matchLabels.role=probe",
			"edgeBackends.viewerCallers[0].matchLabels.role=viewer-client",
		)...)
	docs := edgePolicyDocs(out)
	if g := ingressFromBlock(docs["edge-grafana"]); !strings.Contains(g, "role: probe") || strings.Contains(g, "mcp-grafana") {
		t.Errorf("grafanaCallers override not honoured:\n%s", g)
	}
	if v := ingressFromBlock(docs["edge-beads-viewer"]); !strings.Contains(v, "role: viewer-client") || strings.Contains(v, "gardenio.io/agent") {
		t.Errorf("viewerCallers override not honoured:\n%s", v)
	}
}

// gd-2TXOKAXFcQ: with auth.proxy on, Grafana's port trusts the identity header,
// so ziti-host (whose clients could set that header) must not be a peer there.
// The viewer policy keeps ziti-host until S9c (gd-xNMLrsgajy).
func TestEdgeBackends_GrafanaZitiHost(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		flags    []string
		wantPeer bool
	}{
		{"auth.proxy on", []string{"grafana.auth.proxy.enabled=true"}, false},
		{"auth.proxy on, knob forced true", []string{"grafana.auth.proxy.enabled=true", "edgeBackends.grafanaZitiHost=true"}, false},
		{"auth.proxy off, knob default", nil, true},
		{"auth.proxy off, knob false", []string{"edgeBackends.grafanaZitiHost=false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := helmTemplate(t, edgeBackendsTemplate, append(edgeBackendsOn(), tc.flags...)...)
			docs := edgePolicyDocs(out)
			grafana := ingressFromBlock(docs["edge-grafana"])
			if got := strings.Contains(grafana, "app: ziti-host"); got != tc.wantPeer {
				t.Errorf("edge-grafana ziti-host peer = %v, want %v:\n%s", got, tc.wantPeer, grafana)
			}
			// The rest of the port-3000 peers are untouched either way.
			for _, want := range []string{`kubernetes.io/metadata.name: "public-edge"`, "app.kubernetes.io/component: beads-viewer"} {
				if !strings.Contains(grafana, want) {
					t.Errorf("edge-grafana lost peer %q:\n%s", want, grafana)
				}
			}
			if v := ingressFromBlock(docs["edge-beads-viewer"]); !strings.Contains(v, "app: ziti-host") {
				t.Errorf("edge-beads-viewer must still admit ziti-host:\n%s", v)
			}
		})
	}
}

// Ziti is off on k3s (S9c, gd-xNMLrsgajy): no ziti-host peer on any fence the
// overlay renders, and viewer-authproxy (no sign-in check of its own any more)
// is not a forwardauth caller. The overlay must use null, not {}, for
// zitiHostSelector: helm merges a {} into the default {app: ziti-host} and the
// peer survives.
func TestEdgeBackends_K3sOverlayAdmitsNoZitiHost(t *testing.T) {
	t.Parallel()
	for _, tpl := range []string{edgeBackendsTemplate, "templates/coopmux/networkpolicy.yaml"} {
		out := helmTemplateShowOnlyWithValues(t, tpl, []string{k3sOverlay})
		if strings.Contains(out, "ziti-host") {
			t.Errorf("%s admits ziti-host on the k3s overlay:\n%s", tpl, out)
		}
	}
	fa := edgePolicyDocs(helmTemplateShowOnlyWithValues(t, edgeBackendsTemplate, []string{k3sOverlay}))["edge-forwardauth"]
	if fa == "" {
		t.Fatal("overlay renders no edge-forwardauth policy")
	}
	if strings.Contains(fa, "viewer-authproxy") {
		t.Errorf("edge-forwardauth must admit only the public-edge namespace:\n%s", fa)
	}
	mux := helmTemplateShowOnlyWithValues(t, "templates/coopmux/networkpolicy.yaml", []string{k3sOverlay})
	if !strings.Contains(mux, "app: viewer-authproxy") {
		t.Error("coopmux must still admit viewer-authproxy (the mux.clonk.tv proxy)")
	}
}
