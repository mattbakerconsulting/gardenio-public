package tests

import (
	"strings"
	"testing"
)

// mcp-grafana-write is the WRITE-CAPABLE mcp-grafana instance the
// gardenio-assistant-app plugin backend reverse-proxies the chat box's MCP
// traffic to (ka-0000000293). These tests pin the security envelope that makes
// it safe: a DISTINCT component label (so it is never selected by the agent
// read-only ingress/egress), a curated --enabled-tools allow-list with NO
// --disable-write, no env SA token (header auth), and NetworkPolicies that admit
// the grafana pod ONLY (never agents) and lock its egress to Grafana + DNS.

// writeFlags is the realistic enabled config: the write sidecar is meaningful
// only alongside the branded chat box, which requires the assistant + llm combo.
var writeFlags = []string{
	"grafana.enabled=true",
	"grafana.assistant.enabled=true",
	"grafana.llm.enabled=true",
	"litellm.enabled=true",
	"grafana.plugins.install={grafana-llm-app}",
	"mcpGrafanaWrite.enabled=true",
}

// TestMcpGrafanaWrite_DisabledByDefault — additive contract: with the write
// instance untouched, its templates render nothing (helm --show-only errors with
// "could not find template" when the whole file is gated off — the same signal
// other default-off component tests assert on).
func TestMcpGrafanaWrite_DisabledByDefault(t *testing.T) {
	t.Parallel()
	dep, derr := helmTemplateAllowFail(t, "templates/mcp-grafana-write/deployment.yaml",
		"grafana.enabled=true",
	)
	if derr == nil && strings.Contains(dep, "mcp-grafana-write") {
		t.Errorf("mcp-grafana-write deployment must not render by default (additive contract, ka-0000000293); got:\n%s", dep)
	}
	svc, serr := helmTemplateAllowFail(t, "templates/mcp-grafana-write/service.yaml",
		"grafana.enabled=true",
	)
	if serr == nil && strings.Contains(svc, "mcp-grafana-write") {
		t.Errorf("mcp-grafana-write service must not render by default (ka-0000000293); got:\n%s", svc)
	}
}

// TestMcpGrafanaWrite_DeploymentEnvelope — the write deployment must run
// streamable-HTTP with the curated --enabled-tools allow-list, must NOT carry
// --disable-write (that is the whole point — the read sidecar keeps it), and
// must carry NO env service-account token (identity is supplied per-request by
// the plugin backend's X-Grafana-API-Key header).
func TestMcpGrafanaWrite_DeploymentEnvelope(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/mcp-grafana-write/deployment.yaml", writeFlags...)

	if !strings.Contains(dep, "--enabled-tools") {
		t.Error("write sidecar must pass --enabled-tools (curated family allow-list, ka-0000000293)")
	}
	if !strings.Contains(dep, "search,datasource,prometheus,navigation,rendering,dashboard,folder") {
		t.Errorf("write sidecar --enabled-tools must be the curated read+dashboard+folder list (no admin/alerting), ka-0000000293; got:\n%s", dep)
	}
	if strings.Contains(dep, "--disable-write") {
		t.Errorf("write sidecar must NOT pass --disable-write — it is the write-capable instance (ka-0000000293); got:\n%s", dep)
	}
	// No env SA token: the plugin backend authenticates per-request. A static
	// GRAFANA_SERVICE_ACCOUNT_TOKEN env would mean a long-lived token in a Secret;
	// the default config deliberately omits it.
	if strings.Contains(dep, "GRAFANA_SERVICE_ACCOUNT_TOKEN") {
		t.Errorf("write sidecar must NOT inject an env GRAFANA_SERVICE_ACCOUNT_TOKEN by default (header auth, ka-0000000293); got:\n%s", dep)
	}
	// Hardened pod (matches the read sidecar): non-root, read-only rootfs, drop all.
	for _, needle := range []string{"runAsNonRoot: true", "readOnlyRootFilesystem: true"} {
		if !strings.Contains(dep, needle) {
			t.Errorf("write sidecar pod must be hardened (%q), ka-0000000293; got:\n%s", needle, dep)
		}
	}
}

// TestMcpGrafanaWrite_DistinctComponentLabel — the highest-consequence detail:
// the write instance MUST carry a DISTINCT component label so the agent
// read-only ingress (which selects component: mcp-grafana) never admits it and
// agent-egress never reaches it. A shared label would silently widen the agent
// read-only contract into write.
func TestMcpGrafanaWrite_DistinctComponentLabel(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/mcp-grafana-write/deployment.yaml", writeFlags...)
	if !strings.Contains(dep, "app.kubernetes.io/component: mcp-grafana-write") {
		t.Errorf("write deployment must use the DISTINCT component label mcp-grafana-write (ka-0000000293); got:\n%s", dep)
	}
	// It must NOT carry the read sidecar's bare component label as a selector.
	if strings.Contains(dep, "app.kubernetes.io/component: mcp-grafana\n") {
		t.Errorf("write deployment must NOT reuse the read sidecar's component label (would merge selectors, ka-0000000293); got:\n%s", dep)
	}
}

// TestMcpGrafanaWrite_IngressGrafanaOnly — the write instance is reachable ONLY
// by the grafana pod (the plugin backend). The ingress policy must select the
// grafana component and must NOT admit agents.
func TestMcpGrafanaWrite_IngressGrafanaOnly(t *testing.T) {
	t.Parallel()
	np := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		append(writeFlags, "networkPolicies.enabled=true")...)

	// Find the write-ingress policy block, scoped from its name marker to the
	// next YAML document separator (--- ) so we read exactly this one policy.
	const marker = "name: test-mcp-grafana-write-ingress"
	idx := strings.Index(np, marker)
	if idx < 0 {
		t.Fatalf("mcp-grafana-write-ingress NetworkPolicy must render when mcpGrafanaWrite.enabled (ka-0000000293); got:\n%s", np)
	}
	block := np[idx:]
	if next := strings.Index(block, "\n---"); next >= 0 {
		block = block[:next]
	}
	if !strings.Contains(block, "app.kubernetes.io/component: grafana") {
		t.Errorf("write-ingress must admit the grafana pod (the plugin backend), ka-0000000293; got block:\n%s", block)
	}
	// The agent pod selector must NOT appear in the write ingress (that is the
	// read sidecar's allowance, not the write one).
	if strings.Contains(block, "gardenio.io/agent") || strings.Contains(block, "agentPodSelector") {
		t.Errorf("write-ingress must NOT admit agent pods (read-only contract preserved), ka-0000000293; got block:\n%s", block)
	}
}

// TestMcpGrafanaWrite_EgressLockedToGrafanaAndDNS — the write instance's egress
// must reach ONLY the in-cluster Grafana + kube-dns. No internet, no litellm.
func TestMcpGrafanaWrite_EgressLockedToGrafanaAndDNS(t *testing.T) {
	t.Parallel()
	np := helmTemplate(t, "templates/networkpolicies/default-deny-egress.yaml",
		append(writeFlags,
			"networkPolicies.enabled=true",
			"networkPolicies.defaultDenyEgress=true",
		)...)

	const marker = "name: test-mcp-grafana-write-egress"
	idx := strings.Index(np, marker)
	if idx < 0 {
		t.Fatalf("mcp-grafana-write-egress NetworkPolicy must render under default-deny + mcpGrafanaWrite.enabled (ka-0000000293); got:\n%s", np)
	}
	block := np[idx:]
	if next := strings.Index(block, "\n---"); next >= 0 {
		block = block[:next]
	}
	if !strings.Contains(block, "app.kubernetes.io/component: grafana") {
		t.Errorf("write-egress must allow the in-cluster Grafana API, ka-0000000293; got block:\n%s", block)
	}
	// No 0.0.0.0/0 internet egress in the write block.
	if strings.Contains(block, "0.0.0.0/0") {
		t.Errorf("write-egress must NOT allow internet egress (zero off-cluster egress), ka-0000000293; got block:\n%s", block)
	}
}

// TestMcpGrafanaWrite_AssistantJsonDataRepointed — when the write instance is on,
// the assistant plugin's jsonData must carry mcpUpstreamURL (the write Service)
// + grafanaInClusterURL (the in-cluster Grafana), so the plugin backend proxies
// MCP to the in-cluster sidecar instead of the embedded edge-bound server.
func TestMcpGrafanaWrite_AssistantJsonDataRepointed(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", writeFlags...)
	if !strings.Contains(cm, "mcpUpstreamURL: http://test-mcp-grafana-write:8000") {
		t.Errorf("assistant jsonData must point mcpUpstreamURL at the write Service when mcpGrafanaWrite.enabled (ka-0000000293); got:\n%s", cm)
	}
	if !strings.Contains(cm, "grafanaInClusterURL:") {
		t.Errorf("assistant jsonData must carry grafanaInClusterURL (forwarded as X-Grafana-URL) when mcpGrafanaWrite.enabled (ka-0000000293); got:\n%s", cm)
	}
	// The in-cluster URL must NOT be the external host — that is the exact value
	// that breaks the embedded server.
	if strings.Contains(cm, "grafanaInClusterURL: \"https://") {
		t.Errorf("grafanaInClusterURL must be an in-cluster (http://...:3000) URL, not the external https host (ka-0000000293); got:\n%s", cm)
	}
}

// TestMcpGrafanaWrite_JsonDataAbsentWhenWriteOff — with the assistant on but the
// write instance OFF, the repoint jsonData keys must NOT render (else the chat
// box would point at a non-existent Service).
func TestMcpGrafanaWrite_JsonDataAbsentWhenWriteOff(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.assistant.enabled=true",
		"grafana.llm.enabled=true",
		"litellm.enabled=true",
		"grafana.plugins.install={grafana-llm-app}",
	)
	if strings.Contains(cm, "mcpUpstreamURL") {
		t.Errorf("mcpUpstreamURL must NOT render when mcpGrafanaWrite is disabled (ka-0000000293); got:\n%s", cm)
	}
}
