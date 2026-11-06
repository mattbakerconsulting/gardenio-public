package tests

import (
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// landgate v0 (Lands boundary gd-uYNbyW4a9Z §6 step 11, gd-ParwLpmgID): one
// transparent proxy per land that holds no credential, fenced so only the
// edge reaches it, and the crew -> edge half of the deny on every policy that
// selects agent pods.

const (
	landgateDeployTpl = "templates/landgate/deployment.yaml"
	landgateNPTpl     = "templates/landgate/networkpolicy.yaml"
)

func landgateOn(extra ...string) []string {
	return append([]string{
		"landgate.enabled=true",
		"landgate.lands={acme,beta}",
		"keycloak.enabled=true",
		"keycloak.oidcAudience=gardenio-traefik-forwardauth",
		"keycloak.realm.issuerURL=https://auth.clonk.tv/realms/gardenio",
	}, extra...)
}

// k8sDocs splits rendered output into objects of the given kind.
func k8sDocs(t *testing.T, out, kind string) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for raw := range strings.SplitSeq(out, "\n---\n") {
		var m map[string]any
		if err := yaml.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("unparseable document: %v\n%s", err, raw)
		}
		if m["kind"] == kind {
			docs = append(docs, m)
		}
	}
	return docs
}

func TestLandgate_OffByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "")
	if strings.Contains(out, "component: landgate") || strings.Contains(out, "-landgate-") {
		t.Fatal("landgate is off by default, but something of it rendered")
	}
}

func TestLandgate_OneGatePerLand(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", landgateDeployTpl, landgateOn()...)
	deps := k8sDocs(t, out, "Deployment")
	svcs := k8sDocs(t, out, "Service")
	if len(deps) != 2 || len(svcs) != 2 {
		t.Fatalf("want a Deployment and a Service per land, got %d and %d", len(deps), len(svcs))
	}
	for _, land := range []string{"acme", "beta"} {
		name := "test-landgate-" + land
		for _, want := range []string{
			"name: " + name + "\n",
			"name: LANDGATE_LAND\n              value: \"" + land + "\"",
			"value: \"http://test-beads:8080\"",
			"name: OIDC_ISSUER_URL\n              value: \"https://auth.clonk.tv/realms/gardenio\"",
			"name: OIDC_CLIENT_ID\n              value: \"gardenio-traefik-forwardauth\"",
			"gardenio.io/land: \"" + land + "\"",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q", land, want)
			}
		}
	}
}

// The whole point of v0: a compromised landgate reaches nothing beyond the
// bearers passing through it. No Secret, no token, no ServiceAccount token.
func TestLandgate_HoldsNoCredential(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", landgateDeployTpl, landgateOn("beads.tokenSecret=gardenio-beads-token")...)
	for _, bad := range []string{"secretKeyRef", "secretRef", "envFrom", "volumes:", "volumeMounts:", "TOKEN", "projected", "serviceAccountName"} {
		if strings.Contains(out, bad) {
			t.Errorf("landgate pod spec contains %q:\n%s", bad, out)
		}
	}
	if n := strings.Count(out, "automountServiceAccountToken: false"); n != 2 {
		t.Errorf("automountServiceAccountToken: false on %d of 2 gates", n)
	}
	if n := strings.Count(out, "readOnlyRootFilesystem: true"); n != 2 {
		t.Errorf("readOnlyRootFilesystem on %d of 2 gates", n)
	}
}

func TestLandgate_RefusesBadConfig(t *testing.T) {
	t.Parallel()
	for name, flags := range map[string][]string{
		"uppercase land": landgateOn("landgate.lands={Acme}"),
		"land with dot":  landgateOn("landgate.lands={a.b}"),
		"name too long":  landgateOn("landgate.lands={" + strings.Repeat("a", 50) + "}"),
		"no issuer":      {"landgate.enabled=true", "landgate.lands={acme}", "keycloak.oidcAudience=x"},
		"no audience":    {"landgate.enabled=true", "landgate.lands={acme}", "landgate.oidc.issuerURL=https://auth.example/realms/r"},
	} {
		if _, err := helmTemplateAllowFail(t, landgateDeployTpl, flags...); err == nil {
			t.Errorf("%s: rendered, want a fail", name)
		}
	}
}

// Only the edge namespace may open a connection to a landgate: no crew,
// agent, tenant or other in-cluster pod. Rendered whether or not
// networkPolicies.enabled is on.
func TestLandgate_IngressOnlyFromEdge(t *testing.T) {
	t.Parallel()
	for _, extra := range [][]string{nil, {"networkPolicies.enabled=true"}} {
		out := helmTemplateNS(t, "gardenio", landgateNPTpl, landgateOn(extra...)...)
		var ingress map[string]any
		for _, np := range k8sDocs(t, out, "NetworkPolicy") {
			if strings.HasSuffix(np["metadata"].(map[string]any)["name"].(string), "-landgate-ingress") {
				ingress = np
			}
		}
		if ingress == nil {
			t.Fatalf("no landgate ingress policy:\n%s", out)
		}
		spec := ingress["spec"].(map[string]any)
		rules := spec["ingress"].([]any)
		if len(rules) != 1 {
			t.Fatalf("want one ingress rule, got %v", rules)
		}
		from := rules[0].(map[string]any)["from"].([]any)
		if len(from) != 1 {
			t.Fatalf("want exactly the edge namespace as a peer, got %v", from)
		}
		peer := from[0].(map[string]any)
		if _, ok := peer["podSelector"]; ok {
			t.Errorf("edge peer must not carry a podSelector: %v", peer)
		}
		ns := peer["namespaceSelector"].(map[string]any)["matchLabels"].(map[string]any)
		if len(ns) != 1 || ns["kubernetes.io/metadata.name"] != "public-edge" {
			t.Errorf("ingress peer = %v, want only the public-edge namespace", ns)
		}
	}
}

// A crew pod is selected by imds-deny-agents in prod (networkPolicies off).
// With landgate on it must not admit the edge namespace or the edge CIDRs,
// and the general imds-deny, which admits everything, must not select agents.
func TestLandgate_CrewDenyOnProdOverlay(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, "templates/networkpolicies/imds-deny.yaml",
		[]string{"../../../deploy-values/gardenio-k3s.yaml"},
		// landgate.lands is replaced, so the overlay's verifier land must be too.
		landgateOn("landgate.crewDeny.edgeCIDRs={203.0.113.10/32,10.0.9.9/32,10.42.7.0/24}", "landgate.verifier.lands={acme}")...)
	var agents, general map[string]any
	for _, np := range k8sDocs(t, out, "NetworkPolicy") {
		md := np["metadata"].(map[string]any)
		if md["namespace"] != "gardenio" {
			continue
		}
		switch md["name"] {
		case "imds-deny-agents":
			agents = np
		case "imds-deny":
			general = np
		}
	}
	if agents == nil || general == nil {
		t.Fatalf("missing gardenio imds-deny policies:\n%s", out)
	}
	sel, _ := yaml.Marshal(general["spec"].(map[string]any)["podSelector"])
	if !strings.Contains(string(sel), "key: gardenio.io/agent") || !strings.Contains(string(sel), "operator: DoesNotExist") {
		t.Errorf("imds-deny still selects agent pods, which re-admits the edge by union:\n%s", sel)
	}
	egress, _ := yaml.Marshal(agents["spec"].(map[string]any)["egress"])
	e := string(egress)
	if strings.Contains(e, "namespaceSelector: {}") {
		t.Errorf("imds-deny-agents admits every namespace, the edge's included:\n%s", e)
	}
	if !regexp.MustCompile(`operator: NotIn\s+values:\s+- public-edge`).MatchString(e) {
		t.Errorf("imds-deny-agents must exclude the public-edge namespace:\n%s", e)
	}
	for _, c := range []string{"203.0.113.10/32", "10.0.9.9/32", "10.42.7.0/24", "169.254.169.254/32"} {
		if !strings.Contains(e, "- "+c) {
			t.Errorf("imds-deny-agents does not cut %s:\n%s", c, e)
		}
	}

	// Off (the rollback), the overlay renders exactly what it did before.
	off := helmTemplateShowOnlyWithValues(t, "templates/networkpolicies/imds-deny.yaml", []string{"../../../deploy-values/gardenio-k3s.yaml"},
		"landgate.enabled=false")
	if strings.Contains(off, "DoesNotExist") || strings.Contains(off, "NotIn\n                values: [\"public-edge\"]") {
		t.Errorf("landgate off changed imds-deny:\n%s", off)
	}
}

// With networkPolicies on, agent egress is an allowlist: the edge CIDRs are
// cut from the internet rules, and no rule names the edge namespace or a
// landgate.
func TestLandgate_CrewDenyWithNetworkPolicies(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", "templates/networkpolicies/networkpolicies.yaml",
		landgateOn("networkPolicies.enabled=true", "landgate.crewDeny.edgeCIDRs={203.0.113.10/32}",
			"jiraBridge.egress.atlassianCIDRs={192.0.2.0/24}")...)
	seen := map[string]bool{}
	for _, np := range k8sDocs(t, out, "NetworkPolicy") {
		name := np["metadata"].(map[string]any)["name"].(string)
		spec := np["spec"].(map[string]any)
		sel, _ := yaml.Marshal(spec["podSelector"])
		// Dark agents are selected by their gardenio.io/dark marker alone.
		if !strings.Contains(string(sel), "gardenio.io/agent") && !strings.Contains(string(sel), "gardenio.io/dark") {
			continue
		}
		egress, _ := yaml.Marshal(spec["egress"])
		e := string(egress)
		if strings.Contains(e, "public-edge") || strings.Contains(e, "landgate") {
			t.Errorf("%s lets agents reach the edge or a landgate:\n%s", name, e)
		}
		if strings.Contains(e, "cidr: 0.0.0.0/0") {
			seen[name] = true
			if !strings.Contains(e, "- 203.0.113.10/32") {
				t.Errorf("%s: internet egress does not cut the edge CIDR:\n%s", name, e)
			}
		}
	}
	for _, want := range []string{"test-agent-internet-egress", "test-dark-agent-egress"} {
		if !seen[want] {
			t.Errorf("%s did not render with an internet rule", want)
		}
	}
	if !strings.Contains(out, "app.kubernetes.io/component: landgate") {
		t.Error("beads ingress does not admit landgate")
	}
}

func TestLandgate_CrewDenyReachesLandNamespaces(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio", "templates/controller/deployment.yaml",
		landgateOn("agents.enabled=true", "landNamespaces.enabled=true", "landNamespaces.extraExceptCIDRs={10.42.0.0/16}",
			"landgate.crewDeny.edgeCIDRs={203.0.113.10/32,10.42.0.0/16}")...)
	if !strings.Contains(out, "name: LANDS_NAMESPACE_EXTRA_EXCEPT_CIDRS\n              value: \"10.42.0.0/16,203.0.113.10/32\"") {
		t.Errorf("tenant pods do not get the edge CIDRs cut:\n%s", landgateEnvLines(out, "LANDS_NAMESPACE"))
	}
}

func landgateEnvLines(out, sub string) string {
	var b strings.Builder
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.Contains(l, sub) {
			b.WriteString(l + "\n")
			if i+1 < len(lines) {
				b.WriteString(lines[i+1] + "\n")
			}
		}
	}
	return b.String()
}

// landgateVerifierPolicies returns the rendered land-door verifier policies
// (gd-Hq1PnUVOUC) by name.
func landgateVerifierPolicies(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	got := map[string]map[string]any{}
	for _, np := range k8sDocs(t, out, "NetworkPolicy") {
		name := np["metadata"].(map[string]any)["name"].(string)
		if strings.Contains(name, "-landgate-verifier-") {
			got[name] = np
		}
	}
	return got
}

// The land-door verifier (gd-Hq1PnUVOUC) is the one exception to "only the
// edge reaches a landgate": agent pods of the listed roles, in this namespace
// only, reach the listed lands' landgates and no other. Off unless listed.
func TestLandgate_VerifierAdmitsListedRolesToListedLands(t *testing.T) {
	t.Parallel()
	if got := landgateVerifierPolicies(t, helmTemplateNS(t, "gardenio", landgateNPTpl, landgateOn()...)); len(got) != 0 {
		t.Fatalf("verifier policies render with no verifier lands: %v", got)
	}
	out := helmTemplateNS(t, "gardenio", landgateNPTpl,
		landgateOn("landgate.verifier.lands={acme}", "landgate.verifier.roles={crew}")...)
	got := landgateVerifierPolicies(t, out)
	np, ok := got["test-landgate-verifier-acme"]
	if !ok || len(got) != 1 {
		t.Fatalf("want exactly the acme verifier policy (no egress half with networkPolicies off), got %v", got)
	}
	spec := np["spec"].(map[string]any)
	sel, _ := yaml.Marshal(spec["podSelector"])
	if !strings.Contains(string(sel), "gardenio.io/land: acme") || !strings.Contains(string(sel), "app.kubernetes.io/component: landgate") {
		t.Errorf("verifier policy must select acme's landgate only:\n%s", sel)
	}
	rules, _ := yaml.Marshal(spec["ingress"])
	r := string(rules)
	if strings.Contains(r, "namespaceSelector") || strings.Contains(r, "ipBlock") {
		t.Errorf("verifier peers must be pods in this namespace only:\n%s", r)
	}
	if !regexp.MustCompile(`key: gardenio.io/role\s+operator: In\s+values:\s+- crew\n`).MatchString(r) ||
		!regexp.MustCompile(`key: gardenio.io/agent\s+operator: Exists`).MatchString(r) {
		t.Errorf("verifier peers must be agent pods of role crew:\n%s", r)
	}
	if !strings.Contains(r, "port: 8080") {
		t.Errorf("verifier rule must name port 8080:\n%s", r)
	}

	// With networkPolicies on, agent egress is an allowlist, so the egress
	// half names the listed lands' landgates and nothing else.
	on := landgateVerifierPolicies(t, helmTemplateNS(t, "gardenio", landgateNPTpl,
		landgateOn("networkPolicies.enabled=true", "landgate.verifier.lands={acme}", "landgate.verifier.roles={crew}")...))
	eg, ok := on["test-landgate-verifier-egress"]
	if !ok {
		t.Fatalf("no verifier egress policy with networkPolicies on: %v", on)
	}
	e, _ := yaml.Marshal(eg["spec"].(map[string]any)["egress"])
	if !regexp.MustCompile(`key: gardenio.io/land\s+operator: In\s+values:\s+- acme\n`).MatchString(string(e)) ||
		strings.Contains(string(e), "beta") || strings.Contains(string(e), "ipBlock") || strings.Contains(string(e), "namespaceSelector") {
		t.Errorf("verifier egress must reach acme's landgate only:\n%s", e)
	}
}

func TestLandgate_VerifierRefusesBadConfig(t *testing.T) {
	t.Parallel()
	for name, flags := range map[string][]string{
		"land without a landgate": landgateOn("landgate.verifier.lands={zeta}", "landgate.verifier.roles={crew}"),
		"no roles":                landgateOn("landgate.verifier.lands={acme}"),
	} {
		if _, err := helmTemplateAllowFail(t, landgateNPTpl, flags...); err == nil {
			t.Errorf("%s: rendered, want a fail", name)
		}
	}
}

// The k3s overlay admits crew pods to scopetest's landgate, and the edge
// fence stays as it was.
func TestLandgate_VerifierOnProdOverlay(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, landgateNPTpl, []string{"../../../deploy-values/gardenio-k3s.yaml"})
	got := landgateVerifierPolicies(t, out)
	np, ok := got["test-landgate-verifier-scopetest"]
	if !ok || len(got) != 1 {
		t.Fatalf("want the scopetest verifier policy alone, got %v", got)
	}
	rules, _ := yaml.Marshal(np["spec"].(map[string]any)["ingress"])
	if !regexp.MustCompile(`values:\s+- crew\n`).MatchString(string(rules)) {
		t.Errorf("k3s verifier must admit role crew only:\n%s", rules)
	}
}
