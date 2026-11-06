package tests

import (
	"strings"
	"testing"
)

// VictoriaLogs datasource + pre-baked plugin seam (ka-0000007948).
//
// The LogsQL dashboards (dashboards/edge-traffic-geomap.json) query the
// in-chart VictoriaLogs (vlsingle) store through a VictoriaLogs Grafana
// datasource, backed by the commercial-signed victoriametrics-logs-datasource
// plugin. Because PROD runs with grafana.plugins.allowCatalogEgress:false under
// default-deny-egress, the plugin is PRE-BAKED into a gardenio artifact image and
// seeded by a copy-only initContainer (mirroring grafana.llm.prebake /
// grafana.assistant) — never catalog-installed, so no 0.0.0.0/0:443 egress hole
// is opened (ka-0000007541).
//
// These tests pin: (1) the feature is fully OFF by default (byte-additive),
// (2) all seam artifacts render when enabled (initContainer, emptyDir volume,
// volumeMount at the plugin subdir, a writable /var/lib/grafana), (3) the
// datasource is provisioned pointing at the in-chart vlsingle Service under the
// stable `victorialogs` uid the dashboard references, (4) the seed adds no
// off-cluster egress (cp-only), and (5) the grafana-egress NetworkPolicy carries
// the :9428 allow ONLY when the in-chart store is also enabled.

const vlDatasourcePluginID = "victoriametrics-logs-datasource"

// vlDatasourceFlags is the realistic enabled config: the datasource only
// produces signal against the in-chart VictoriaLogs store, so it is enabled
// alongside vlsingle (the normal Phase-2 posture).
var vlDatasourceFlags = []string{
	"grafana.enabled=true",
	"grafana.victoriaLogsDatasource.enabled=true",
	"vlsingle.enabled=true",
}

// TestGrafanaVLDatasource_DisabledByDefault — with grafana on but the datasource
// untouched, none of the new artifacts render. Guards the additive contract: an
// env that does not opt in is byte-identical to before this change. This matters
// especially because the VictoriaLogs stack is inert-but-ready until an env
// enables it.
func TestGrafanaVLDatasource_DisabledByDefault(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml",
		"grafana.enabled=true",
	)
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
	)
	for _, needle := range []string{
		"seed-vl-datasource-plugin",
		"vl-datasource-plugin",
		"grafana-vl-datasource-plugin",
	} {
		if strings.Contains(dep, needle) {
			t.Errorf("grafana deployment must not contain %q by default (additive contract, ka-0000007948)", needle)
		}
	}
	if strings.Contains(cm, vlDatasourcePluginID) || strings.Contains(cm, "uid: victorialogs") {
		t.Errorf("grafana configmap must not provision the VictoriaLogs datasource by default (additive contract, ka-0000007948)")
	}
}

// TestGrafanaVLDatasource_RendersSeedArtifacts — enabling the datasource renders
// the seed initContainer (pointed at the gardenio-owned artifact image), the
// emptyDir volume + its mount at the plugin's own subdir, and a writable
// /var/lib/grafana (so the seeded dir + backend binary have a writable parent
// under readOnlyRootFilesystem when persistence is off).
func TestGrafanaVLDatasource_RendersSeedArtifacts(t *testing.T) {
	t.Parallel()
	dep := helmTemplate(t, "templates/grafana/deployment.yaml", vlDatasourceFlags...)

	// (1) initContainer present and pointed at the gardenio-owned artifact image.
	if !strings.Contains(dep, "initContainers") || !strings.Contains(dep, "name: seed-vl-datasource-plugin") {
		t.Errorf("seed-vl-datasource-plugin initContainer must render when grafana.victoriaLogsDatasource.enabled (ka-0000007948); got:\n%s", dep)
	}
	if !strings.Contains(dep, "/grafana-vl-datasource-plugin:") {
		t.Error("seed initContainer must use the gardenio-owned grafana-vl-datasource-plugin image (ka-0000007948)")
	}

	// (2) copy-only, no-network seed: the seed script must assert the dist is
	// present (plugin.json + MANIFEST.txt + the amd64 backend binary) and then
	// `cp` it — never fetch. Any runtime fetch would violate the
	// zero-off-cluster-egress guarantee that justifies pre-baking.
	for _, fetch := range []string{"curl", "wget", "npm ", "apt-get", "pip "} {
		// Scope to the seed container's exec body: the whole Deployment mentions
		// wget in the image-renderer probe, so assert on the seed script region.
		if seedScript := vlSeedScript(t, dep); strings.Contains(seedScript, fetch) {
			t.Errorf("seed-vl-datasource-plugin must be cp-only (no %q) — the build happens in CI, the runtime seed adds zero off-cluster egress (ka-0000007948); script:\n%s", fetch, seedScript)
		}
	}
	if seedScript := vlSeedScript(t, dep); !strings.Contains(seedScript, "cp -r /plugin/. /mnt/plugin/") {
		t.Errorf("seed-vl-datasource-plugin must copy the dist with cp -r (ka-0000007948); script:\n%s", vlSeedScript(t, dep))
	}
	if seedScript := vlSeedScript(t, dep); !strings.Contains(seedScript, "victoriametrics_logs_backend_plugin_linux_amd64") {
		t.Errorf("seed-vl-datasource-plugin must assert the amd64 backend binary is present — it is a BACKEND datasource Grafana spawns (ka-0000007948); script:\n%s", seedScript)
	}

	// (3) volume + mount at the plugin's own subdir under the plugins path.
	if !strings.Contains(dep, "mountPath: /var/lib/grafana/plugins/"+vlDatasourcePluginID) {
		t.Errorf("plugin dist must mount at /var/lib/grafana/plugins/%s (ka-0000007948); got:\n%s", vlDatasourcePluginID, dep)
	}
	if !strings.Contains(dep, "name: vl-datasource-plugin") {
		t.Error("a vl-datasource-plugin volume must be declared (ka-0000007948)")
	}

	// (4) a writable /var/lib/grafana must be backed even when persistence is off,
	// so the nested plugin dir + the backend binary have a real writable parent.
	if !strings.Contains(dep, "name: data") {
		t.Error("a writable data volume must back /var/lib/grafana when the VL datasource is seeded and persistence is off (ka-0000007948)")
	}
}

// TestGrafanaVLDatasource_Provisioned — the datasource is provisioned into
// Grafana under the stable `victorialogs` uid the dashboard references, pointed
// at the in-chart vlsingle Service (release-relative), with the seeded plugin
// type. A rename of the uid or type breaks the dashboard's datasource binding.
func TestGrafanaVLDatasource_Provisioned(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml", vlDatasourceFlags...)
	if !strings.Contains(cm, "datasources.yaml: |") {
		t.Fatalf("provisioning configmap must render datasources.yaml; got:\n%s", cm)
	}
	for _, needle := range []string{
		"name: VictoriaLogs",
		"type: victoriametrics-logs-datasource",
		"uid: victorialogs",
	} {
		if !strings.Contains(cm, needle) {
			t.Errorf("VictoriaLogs datasource must provision %q (the dashboard binds this uid+type, ka-0000007948); got:\n%s", needle, cm)
		}
	}
	// URL derives from the in-chart vlsingle Service on the standard :9428 select
	// port (release-relative — correct on prod `gardenio` AND the
	// gardenio-e2e-* namespaces). The helm test harness uses release name `test`,
	// so the derived Service is `test-victoria-logs`.
	if !strings.Contains(cm, "url: http://test-victoria-logs:9428") {
		t.Errorf("VictoriaLogs datasource URL must derive from the in-chart vlsingle Service :9428, release-relative (ka-0000007948); got:\n%s", cm)
	}
}

// TestGrafanaVLDatasource_URLOverride — an explicit url wins over the vlsingle
// derivation, so an operator can point Grafana at an out-of-chart store.
func TestGrafanaVLDatasource_URLOverride(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/grafana/configmap.yaml",
		"grafana.enabled=true",
		"grafana.victoriaLogsDatasource.enabled=true",
		"grafana.victoriaLogsDatasource.url=http://vl.example.internal:9428",
	)
	if !strings.Contains(cm, "url: http://vl.example.internal:9428") {
		t.Errorf("explicit grafana.victoriaLogsDatasource.url must win over the vlsingle derivation (ka-0000007948); got:\n%s", cm)
	}
}

// TestGrafanaVLDatasource_EgressAllow — the grafana-egress NetworkPolicy carries
// the :9428 allow to vlsingle when BOTH the datasource and the in-chart store are
// enabled, and NOT when only the datasource is on (an out-of-chart store carries
// its own egress). The vlsingle-ingress grafana peer is already present (shipped
// with vlsingle), so this egress rule is the missing half of the reachability.
func TestGrafanaVLDatasource_EgressAllow(t *testing.T) {
	t.Parallel()
	// Both enabled -> the egress allow renders.
	np := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"grafana.enabled=true",
		"grafana.victoriaLogsDatasource.enabled=true",
		"vlsingle.enabled=true",
		"networkPolicies.enabled=true",
	)
	egress := grafanaEgressBlock(t, np)
	if !strings.Contains(egress, "port: 9428") {
		t.Errorf("grafana-egress must allow :9428 to vlsingle when the VL datasource + in-chart store are enabled (ka-0000007948); grafana-egress block:\n%s", egress)
	}

	// Datasource on but no in-chart store -> no :9428 egress allow (the store is
	// out-of-chart and carries its own egress).
	npNoStore := helmTemplate(t, "templates/networkpolicies/networkpolicies.yaml",
		"grafana.enabled=true",
		"grafana.victoriaLogsDatasource.enabled=true",
		"vlsingle.enabled=false",
		"networkPolicies.enabled=true",
	)
	if strings.Contains(grafanaEgressBlock(t, npNoStore), "port: 9428") {
		t.Errorf("grafana-egress must NOT allow :9428 when vlsingle is disabled — an out-of-chart store carries its own egress (ka-0000007948)")
	}
}

// vlSeedScript returns the exec body of the seed-vl-datasource-plugin
// initContainer, so egress/cp assertions bind to the actual script rather than
// the whole Deployment (which mentions wget in the renderer probe).
func vlSeedScript(t *testing.T, dep string) string {
	t.Helper()
	const marker = "name: seed-vl-datasource-plugin"
	i := strings.Index(dep, marker)
	if i < 0 {
		t.Fatalf("seed-vl-datasource-plugin container not found in deployment:\n%s", dep)
	}
	rest := dep[i:]
	// The seed container is followed by the next container/section; bound the
	// slice at the next "- name: " sibling or the volumes section, whichever is
	// first, so the script region is scoped tightly.
	end := len(rest)
	for _, stop := range []string{"\n        - name:", "\n      containers:", "\n      volumes:"} {
		if j := strings.Index(rest[len(marker):], stop); j >= 0 && len(marker)+j < end {
			end = len(marker) + j
		}
	}
	return rest[:end]
}
