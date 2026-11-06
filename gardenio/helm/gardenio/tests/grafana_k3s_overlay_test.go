package tests

import (
	"os/exec"
	"strings"
	"testing"
)

const k3sOverlay = "../../../deploy-values/gardenio-k3s.yaml"

// helmTemplateShowOnlyOrEmpty is helmTemplateShowOnlyWithValues for NEGATIVE
// assertions: helm exits non-zero with "could not find template" when
// --show-only matches nothing, which for these tests is the desired outcome
// (the template rendered empty), so it returns "" instead of failing. Any
// other helm error still fails the test.
func helmTemplateShowOnlyOrEmpty(t *testing.T, showOnly string, valuesFiles []string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t), "--show-only", showOnly}
	for _, f := range valuesFiles {
		args = append(args, "-f", chartDir(t)+"/"+f)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "could not find template") {
			return ""
		}
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// G5: readOnlyRootFilesystem is true and the /var/lib/grafana volume renders
// ONLY when persistence (or a plugin path) is enabled. Without it Grafana
// cannot create grafana.db. A bare grafana.enabled=true is not a bootable
// Grafana, and nothing in the chart says so out loud.
func TestGrafana_K3sOverlay_HasWritableDataDir(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "/var/lib/grafana") {
		t.Fatalf("grafana must mount /var/lib/grafana or it cannot write grafana.db:\n%s", out)
	}
	if !strings.Contains(out, "readOnlyRootFilesystem: true") {
		t.Error("the hardened root filesystem must stay on")
	}
}

func TestGrafana_K3sOverlay_RendersPVC(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/pvc.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "PersistentVolumeClaim") {
		t.Fatalf("expected a grafana PVC, got:\n%s", out)
	}
	if !strings.Contains(out, "ReadWriteOnce") {
		t.Error("expected an RWO claim")
	}
}

// G6: both adminPassword and adminPasswordSecret default to "", which renders
// NO GF_SECURITY_ADMIN_PASSWORD at all and leaves Grafana on built-in
// admin/admin. Behind ziti is still shipped.
func TestGrafana_K3sOverlay_AdminPasswordFromSecret(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/deployment.yaml", []string{k3sOverlay})
	if !strings.Contains(out, "GF_SECURITY_ADMIN_PASSWORD") {
		t.Fatalf("admin password env must render:\n%s", out)
	}
	if !strings.Contains(out, "admin-password") {
		t.Error("must read the exact secret key 'admin-password' (deployment.yaml:359)")
	}
	if strings.Contains(out, "GF_SECURITY_ADMIN_PASSWORD\n          value:") {
		t.Error("the password must come from secretKeyRef, never an inline value")
	}
}

// The datasource is already correct and already allowed by NetworkPolicy.
func TestGrafana_K3sOverlay_DatasourceIsVmsingle(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/grafana/configmap.yaml", []string{k3sOverlay})
	// The harness releases the chart as "test", so the in-chart Service renders
	// as test-vmsingle; in the cluster it is gardenio-vmsingle.
	if !strings.Contains(out, "url: http://test-vmsingle:8428") {
		t.Errorf("expected the in-chart vmsingle datasource, got:\n%s", out)
	}
}

// G1: no public route, ever.
func TestGrafana_K3sOverlay_RendersNoIngress(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyOrEmpty(t,
		"templates/grafana/ingressroute.yaml", []string{k3sOverlay})
	if strings.Contains(out, "kind: IngressRoute") {
		t.Errorf("grafana must render NO public route (spec G1), got:\n%s", out)
	}
}

// G7: no ESO on this cluster — an ExternalSecret here fails the whole install.
func TestGrafana_K3sOverlay_RendersNoExternalSecret(t *testing.T) {
	t.Parallel()
	for _, tpl := range []string{
		"templates/mcp-grafana-token-external-secret.yaml",
		"templates/grafana-bridge/token-external-secret.yaml",
	} {
		out := helmTemplateShowOnlyOrEmpty(t, tpl, []string{k3sOverlay})
		if strings.Contains(out, "kind: ExternalSecret") {
			t.Errorf("%s must not render on k3s (no ESO installed), got:\n%s", tpl, out)
		}
	}
}

// Without GRAFANA_BASE_URL the viewer's /charts hub shows the 26 dashboards
// with no way into Grafana. It is the public https name (gd-iS4Uxv9tfN), so the
// link from the https viewer is not mixed content.
func TestGrafana_K3sOverlay_ViewerLinksToGrafana(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t,
		"templates/beads-viewer/deployment.yaml", []string{k3sOverlay})
	want := "value: \"https://grafana.clonk.tv\""
	i := strings.Index(out, "name: GRAFANA_BASE_URL")
	if i < 0 || !strings.Contains(out[i:min(i+120, len(out))], want) {
		t.Fatalf("beads-viewer must get GRAFANA_BASE_URL=https://grafana.clonk.tv from the k3s overlay:\n%s", out)
	}
}
