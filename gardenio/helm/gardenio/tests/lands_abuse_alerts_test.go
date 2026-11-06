package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Lands abuse alerts (Track B spec §5.4, step 10.4, gd-l8ltROhREx): one
// PrometheusRule with five per-land alerts (spend velocity, purse wall hits,
// egress bytes, spawn refusals, an unusable untrusted-input taint,
// gd-Ms3cmrdW49). Off by default; per-env overlays opt in.

func TestLandsAbuseAlerts_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/lands-abuse-alerts.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when landsAbuse.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestLandsAbuseAlerts_EnabledRendersFiveAlerts(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/lands-abuse-alerts.yaml",
		"agents.metrics.prometheusRule.landsAbuse.enabled=true",
	)
	for _, want := range []string{
		"kind: PrometheusRule",
		"alert: LandSpendVelocityHigh",
		"alert: LandPurseWallHit",
		"alert: LandEgressHigh",
		"alert: LandSpawnRefusalsHigh",
		"alert: LandUntrustedInputUnusable",
		// Each alert is per land: spend from the land's own account, egress
		// from its namespace, refusals from the controller's counter.
		`gardenio_account_cost_usd{account=~"lands-.+"}`,
		`container_network_transmit_bytes_total{namespace=~"land-.+"}`,
		`gardenio_lands_spawn_refusals_total{code="purse_wall"}`,
		"sum by (land, reason) (increase(gardenio_lands_untrusted_input_unusable_total[15m])) > 0",
		// Defaults.
		"5 * max by (land) (label_replace(delta(gardenio_account_cost_usd{account=~\"lands-.+\"}[7d])",
		"> 2\n",
		"> 5000000000",
		"increase(gardenio_lands_spawn_refusals_total[1h])) > 10",
		`severity: "ticket"`,
		"subsystem: lands",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing, got:\n%s", want, out)
		}
	}
}

func TestLandsAbuseAlerts_Overrides(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/lands-abuse-alerts.yaml",
		"agents.metrics.prometheusRule.landsAbuse.enabled=true",
		"agents.metrics.prometheusRule.landsAbuse.severity=page",
		"agents.metrics.prometheusRule.landsAbuse.purseWall.severity=warn",
		"agents.metrics.prometheusRule.landsAbuse.spendVelocity.multiple=3",
		"agents.metrics.prometheusRule.landsAbuse.egress.bytesPerHour=1000000",
		"agents.metrics.prometheusRule.landsAbuse.spawnRefusals.perHour=4",
		"agents.metrics.prometheusRule.landsAbuse.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	for _, want := range []string{
		`severity: "page"`, `severity: "warn"`, "3 * max by (land) (label_replace(delta(gardenio_account_cost_usd{account=~\"lands-.+\"}[7d])", "> 1000000\n",
		"increase(gardenio_lands_spawn_refusals_total[1h])) > 4", "release: kube-prometheus-stack",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("%q missing, got:\n%s", want, out)
		}
	}
}

// The egress alert reads cadvisor's per-pod network counter, so the kubelet
// cadvisor scrape must keep it by default.
func TestLandsAbuseAlerts_CadvisorKeepsEgress(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/monitoring/kubelet-cadvisor-vmnodescrape.yaml",
		"monitoring.clusterMetrics.kubeletCadvisor.enabled=true",
	)
	if !strings.Contains(out, "container_network_transmit_bytes_total") {
		t.Errorf("the cadvisor keep-filter must retain container_network_transmit_bytes_total, got:\n%s", out)
	}
}

// TestLandsAbuseAlerts_Fire feeds promtool synthetic series for each alert
// (lands_abuse_rules_test.yaml): each fires for an abusive land and stays quiet
// for an ordinary one. Skips when helm/yq/promtool are not on PATH.
func TestLandsAbuseAlerts_Fire(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool firing proof", bin)
		}
	}
	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "lands_rules.yaml")
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/lands-abuse-alerts.yaml "+
			"--set agents.metrics.prometheusRule.landsAbuse.enabled=true | yq eval '.spec' -")
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render rules failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(rulesPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
	tmpl, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "lands_abuse_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testPath := filepath.Join(tmp, "lands_test.yaml")
	if err := os.WriteFile(testPath, []byte(strings.ReplaceAll(string(tmpl), "__RULES_FILE__", rulesPath)), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := exec.Command("promtool", "test", "rules", testPath).CombinedOutput()
	if err != nil {
		t.Fatalf("promtool firing proof failed: %v\n%s", err, res)
	}
	if !strings.Contains(string(res), "SUCCESS") {
		t.Errorf("expected promtool SUCCESS, got:\n%s", res)
	}
}
