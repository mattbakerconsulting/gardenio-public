package tests

import (
	"strings"
	"testing"
)

// grafana-llm-app plugin install + provisioning (C1, ka-0000004453, epic
// ka-0000002011 Phase A — OSS Grafana chat frontend, Path C).
//
// The chart additively installs the Apache-2.0, Grafana-signed grafana-llm-app
// and provisions it to use gardenio's in-cluster LiteLLM proxy as an
// OpenAI-compatible provider (no Grafana Cloud, no off-cluster LLM egress).
// These tests pin: (1) the feature is fully OFF by default (byte-additive),
// (2) the install env + provisioning + key-injection render when enabled,
// (3) a writable plugins dir exists under readOnlyRootFilesystem, and (4) the
// grafana-egress NetworkPolicy opens litellm + (gated) catalog egress — without
// which a default-deny-egress cluster silently breaks both install and chat.

// TestGrafanaLLM_DisabledByDefault — with grafana on but llm/plugins untouched,
// none of the new artifacts render. Guards the additive contract: an env that
// does not opt in is byte-identical to before this change.
func TestGrafanaLLM_DisabledByDefault(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	for _, needle := range []string{"GF_INSTALL_PLUGINS", "LITELLM_API_KEY", "provisioning-plugins"} {
		if strings.Contains(dep, needle) {
			t.Errorf("grafana deployment must not contain %q by default (additive contract, ka-0000004453)", needle)
		}
	}
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	if strings.Contains(cm, "grafana-llm-app") || strings.Contains(cm, "plugins.yaml") {
		t.Error("grafana provisioning configmap must not provision grafana-llm-app by default (ka-0000004453)")
	}
}

// TestGrafanaLLM_InstallEnvAndProvisioning — enabling the plugin renders the
// GF_INSTALL_PLUGINS env, the plugins-provisioning mount, and the configmap
// provisioning block pointed at the derived in-cluster LiteLLM URL.
func TestGrafanaLLM_InstallEnvAndProvisioning(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.plugins.install={grafana-llm-app}",
	)
	if !strings.Contains(dep, "GF_INSTALL_PLUGINS") || !strings.Contains(dep, "grafana-llm-app") {
		t.Error("GF_INSTALL_PLUGINS must list grafana-llm-app when plugins.install is set (ka-0000004453)")
	}
	if !strings.Contains(dep, "provisioning-plugins") ||
		!strings.Contains(dep, "/etc/grafana/provisioning/plugins") {
		t.Error("grafana-llm-app provisioning configmap must be mounted at /etc/grafana/provisioning/plugins (ka-0000004453)")
	}

	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
	)
	if !strings.Contains(cm, "plugins.yaml: |") {
		t.Fatal("provisioning configmap must render a plugins.yaml key when grafana.llm.enabled (ka-0000004453)")
	}
	if !strings.Contains(cm, "type: grafana-llm-app") {
		t.Error("plugins.yaml must provision the grafana-llm-app app")
	}
	// Default openaiURL derives the in-cluster LiteLLM Service (release name is
	// "test" → test-litellm:4000). Must be in-cluster (no off-cluster egress).
	if !strings.Contains(cm, "url: http://test-litellm:4000") {
		t.Errorf("plugins.yaml must point grafana-llm-app at the in-cluster LiteLLM Service; got:\n%s", cm)
	}
	if !strings.Contains(cm, "openAIKey: $LITELLM_API_KEY") {
		t.Error("plugins.yaml must reference the LITELLM_API_KEY env (Grafana provisioning $VAR interpolation)")
	}
}

// TestGrafanaLLM_KeyFromMasterSecretByDefault — the LITELLM_API_KEY env defaults
// to the litellm master-key Secret; an explicit grafana.llm.keySecret overrides
// it (so a dedicated scoped key from scripts/litellm-grafana-key.sh can be wired).
func TestGrafanaLLM_KeyFromMasterSecretByDefault(t *testing.T) {
	t.Parallel()
	def := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
	)
	if !strings.Contains(def, "LITELLM_API_KEY") || !strings.Contains(def, "test-litellm-master-key") {
		t.Errorf("LITELLM_API_KEY must default to the litellm master-key Secret; got:\n%s", def)
	}

	scoped := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.llm.keySecret=gardenio-grafana-llm-key",
	)
	if !strings.Contains(scoped, "name: gardenio-grafana-llm-key") {
		t.Error("grafana.llm.keySecret must override the key Secret name (ka-0000004453)")
	}
}

// TestGrafanaLLM_WritablePluginsDir — Grafana installs plugins under
// /var/lib/grafana/plugins, which must be writable despite
// readOnlyRootFilesystem=true. With persistence on, the data PVC covers it.
// With persistence OFF (ephemeral), a non-empty install list must still mount a
// writable /var/lib/grafana (emptyDir) — otherwise the boot-time install fails.
func TestGrafanaLLM_WritablePluginsDir(t *testing.T) {
	t.Parallel()
	// readOnlyRootFilesystem must remain true (we are not relaxing the rootfs).
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.plugins.install={grafana-llm-app}",
		"grafana.persistence.enabled=false",
	)
	if !strings.Contains(dep, "readOnlyRootFilesystem: true") {
		t.Error("readOnlyRootFilesystem must stay true — plugins live on a mount, not rootfs (ka-0000004453)")
	}
	if !strings.Contains(dep, "mountPath: /var/lib/grafana") {
		t.Error("a writable /var/lib/grafana must be mounted for the boot-time plugin install when persistence is off (ka-0000004453)")
	}
	// With persistence off, the data volume must be an emptyDir (no PVC claim).
	if strings.Contains(dep, "claimName") {
		t.Error("with persistence off, /var/lib/grafana must be an emptyDir, not a PVC claim")
	}
}

// TestGrafanaLLM_EgressOpensLiteLLMAndCatalog — on a default-deny-egress cluster
// the grafana-egress policy otherwise allows only VictoriaMetrics + kube-DNS.
// Enabling the chat box must add a LiteLLM:4000 egress allow; enabling
// allowCatalogEgress must add an internet 443/80 allow (RFC1918+IMDS+CGNAT
// excluded). Without these the plugin cannot install or serve completions (R1).
func TestGrafanaLLM_EgressOpensLiteLLMAndCatalog(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.plugins.install={grafana-llm-app}",
		"grafana.plugins.allowCatalogEgress=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	egress := grafanaEgressBlock(t, out)
	if !strings.Contains(egress, "component: litellm") || !strings.Contains(egress, "port: 4000") {
		t.Errorf("grafana-egress must allow the in-cluster LiteLLM proxy when grafana.llm.enabled; got:\n%s", egress)
	}
	if !strings.Contains(egress, "cidr: 0.0.0.0/0") || !strings.Contains(egress, "port: 443") {
		t.Errorf("grafana-egress must allow internet 443 when allowCatalogEgress=true; got:\n%s", egress)
	}
	// IMDS + RFC1918 must be excluded so a Grafana SSRF cannot pivot to node IAM.
	for _, ex := range []string{"169.254.0.0/16", "10.0.0.0/8", "100.64.0.0/10"} {
		if !strings.Contains(egress, ex) {
			t.Errorf("catalog-egress allow must exclude %q (no node-IAM / cross-pod pivot)", ex)
		}
	}
}

// TestGrafanaLLM_EgressGatedOff — with the new gates off (but networkPolicies +
// default-deny on), the grafana-egress policy must NOT contain a LiteLLM or
// internet allow. Guards against silently widening Grafana's egress.
func TestGrafanaLLM_EgressGatedOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"grafana.enabled=true",
		"litellm.enabled=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	egress := grafanaEgressBlock(t, out)
	if strings.Contains(egress, "port: 4000") {
		t.Error("grafana-egress must NOT allow LiteLLM when grafana.llm.enabled is off")
	}
	if strings.Contains(egress, "cidr: 0.0.0.0/0") || strings.Contains(egress, "port: 443") {
		t.Error("grafana-egress must NOT allow internet egress when allowCatalogEgress is off")
	}
}

// TestGrafanaLLM_LitellmIngressAdmitsGrafana — NetworkPolicy is additive on
// BOTH sides: grafana-egress opening the sender path to litellm:4000 is not
// enough, because the litellm-ingress policy selects the litellm pod and so
// makes it default-deny-ingress except for its listed peers (agents +
// controller). Without a grafana peer there, the grafana-llm-app chat
// completion POST to litellm:4000 is dropped at the LiteLLM pod's ingress and
// the chat box returns no completion (ka-0000006203). This pins the grafana
// peer into litellm-ingress, gated on the same grafana.llm.enabled as the
// grafana-egress litellm allow.
func TestGrafanaLLM_LitellmIngressAdmitsGrafana(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"grafana.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	ingress := sectionByName(out, "test-litellm-ingress")
	if ingress == "" {
		t.Fatal("litellm-ingress NetworkPolicy must render when litellm.enabled")
	}
	// The agent + controller peers must still be present (no regression).
	if !strings.Contains(ingress, "key: gardenio.io/agent") ||
		!strings.Contains(ingress, "app.kubernetes.io/component: agents") {
		t.Errorf("litellm-ingress must keep the agent + controller peers; got:\n%s", ingress)
	}
	// And the grafana peer must be admitted on :4000 so the chat completion
	// path is not dropped at default-deny-ingress.
	if !strings.Contains(ingress, "app.kubernetes.io/component: grafana") {
		t.Errorf("litellm-ingress must admit the grafana pod when grafana.llm.enabled (ka-0000006203); got:\n%s", ingress)
	}
}

// TestGrafanaLLM_LitellmIngressGrafanaGatedOff — with the chat box off, the
// litellm-ingress policy must NOT admit grafana (no widening of the inference
// chokepoint when the feature is not opted into). Mirrors the egress gate test.
func TestGrafanaLLM_LitellmIngressGrafanaGatedOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"grafana.enabled=true",
		"litellm.enabled=true",
		"networkPolicies.enabled=true",
		"networkPolicies.defaultDenyEgress=true",
	)
	ingress := sectionByName(out, "test-litellm-ingress")
	if ingress == "" {
		t.Fatal("litellm-ingress NetworkPolicy must render when litellm.enabled")
	}
	if strings.Contains(ingress, "app.kubernetes.io/component: grafana") {
		t.Error("litellm-ingress must NOT admit grafana when grafana.llm.enabled is off (no chokepoint widening)")
	}
}

// grafanaEgressBlock returns the YAML of the *-grafana-egress NetworkPolicy
// document from a multi-doc helm render (the document between its "name:
// <rel>-grafana-egress" marker and the next "---" separator).
func grafanaEgressBlock(t *testing.T, rendered string) string {
	t.Helper()
	for d := range strings.SplitSeq(rendered, "\n---") {
		if strings.Contains(d, "-grafana-egress") && strings.Contains(d, "kind: NetworkPolicy") {
			return d
		}
	}
	t.Fatalf("no grafana-egress NetworkPolicy in render:\n%s", rendered)
	return ""
}
