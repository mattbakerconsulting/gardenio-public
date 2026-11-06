package tests

import (
	"strings"
	"testing"
)

// G8: the chart renders NO dashboard ConfigMaps (~200KB per revision overflowed
// the 1 MiB release-Secret limit). The 26 dashboards are baked into the
// grafana-bridge image and reach Grafana as runtime ConfigMaps that the grafana
// sidecar watches. Both halves must be on in the k3s overlay or Grafana boots
// with zero dashboards and nothing says why. values.schema.json validates none
// of the grafana block (§2 item 8), so these assertions are the only guard
// against a typo'd key being silently ignored.

// The sidecar container is what loads the bridge's runtime ConfigMaps.
func TestGrafanaDashboards_K3sOverlay_RendersSidecar(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/deployment.yaml", []string{k3sOverlay})
	for _, want := range []string{
		"name: grafana-sc-dashboard",
		"grafana_dashboard", // the label the bridge stamps on its ConfigMaps
		"name: sidecar-dashboards",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("k3s overlay grafana Deployment must contain %q (sidecar delivery), got:\n%s", want, out)
		}
	}
}

// rbac.yaml is gated on sidecar.enabled, and the Deployment names that
// ServiceAccount unconditionally. Rendering one without the other is a dangling
// SA (pod FailedCreate) — or a sidecar that is forbidden to list ConfigMaps.
func TestGrafanaDashboards_K3sOverlay_RendersSidecarRBAC(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/rbac.yaml", []string{k3sOverlay})
	for _, kind := range []string{"kind: ServiceAccount", "kind: Role\n", "kind: RoleBinding"} {
		if !strings.Contains(out, kind) {
			t.Errorf("grafana rbac.yaml must render %q, got:\n%s", strings.TrimSpace(kind), out)
		}
	}
	if !strings.Contains(out, `"watch"`) {
		t.Errorf("the sidecar Role must grant watch on configmaps, got:\n%s", out)
	}
}

// Grafana only loads dashboards from a path a provisioning provider points at.
func TestGrafanaDashboards_K3sOverlay_ProvisionsSidecarFolder(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/configmap.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "dashboards.yaml: |") {
		t.Fatalf("grafana ConfigMap must carry the dashboards.yaml provisioning entry, got:\n%s", out)
	}
	if !strings.Contains(out, "path: /tmp/dashboards") {
		t.Errorf("dashboards.yaml must point at the sidecar folder /tmp/dashboards, got:\n%s", out)
	}
}

// The bridge is the producer of the runtime ConfigMaps. Without backfill it
// starts but imports nothing, so the sidecar has nothing to load.
func TestGrafanaDashboards_K3sOverlay_RendersBridgeWithBackfill(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana-bridge/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "app.kubernetes.io/component: grafana-bridge") {
		t.Fatalf("expected the grafana-bridge Deployment, got:\n%s", out)
	}
	if !strings.Contains(out, "name: GRAFANA_BACKFILL") {
		t.Errorf("bridge must run with backfill on (GRAFANA_BACKFILL), got:\n%s", out)
	}
	if !strings.Contains(out, `value: "/dashboards"`) {
		t.Errorf("bridge must read the baked-in /dashboards directory, got:\n%s", out)
	}
}

// The bridge creates the ConfigMaps, so it needs its own Role.
func TestGrafanaDashboards_K3sOverlay_RendersBridgeRBAC(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana-bridge/rbac.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "kind: Role\n") || !strings.Contains(out, `"create"`) {
		t.Errorf("bridge Role must be able to create configmaps, got:\n%s", out)
	}
}

// G7: no ESO on this cluster. The bridge's read-back token must not be
// externalSecret-synced; read-back stays off (grafana.url empty) so no token is
// needed at all.
func TestGrafanaDashboards_K3sOverlay_BridgeHasNoExternalSecretOrReadback(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyOrEmpty(t,
		"templates/grafana-bridge/token-external-secret.yaml", []string{k3sOverlay})
	if strings.Contains(out, "kind: ExternalSecret") {
		t.Errorf("bridge token ExternalSecret must not render on k3s (G7), got:\n%s", out)
	}
	dep := helmTemplateShowOnlyWithValues(t,
		"templates/grafana-bridge/deployment.yaml", []string{k3sOverlay})
	if strings.Contains(dep, "name: GRAFANA_URL") {
		t.Errorf("bridge read-back needs a token Secret we do not have; GRAFANA_URL must stay unset, got:\n%s", dep)
	}
}
