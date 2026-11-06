package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Open-PR health alerts (plan gd-3F4W22h2nT step 10, gd-Pt8ztuasTE). The PR
// sweeper's hourly count reaches vmsingle as the controller's gardenio_open_prs*
// gauges (internal/metrics/open_prs.go). The contract enforced here:
//   - OpenPROrphansHigh, OpenPROldestOverdue and OpenPRHealthStale render in
//     gap-alerts, honor their toggle and thresholds, and route as warn to the
//     default Slack receiver with a deep link to their System Health panel,
//   - promtool: 6 orphans fire and 5 do not; an oldest open PR of 49h fires and
//     47h does not; a count 4h10m old fires stale; no series fires nothing.

func TestOpenPRAlerts_Render(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)

	orphans := alertBlock(t, out, "OpenPROrphansHigh")
	if !strings.Contains(orphans, `max(gardenio_open_prs{state="orphaned"}) > 5`) {
		t.Errorf("OpenPROrphansHigh must fire above 5 orphans, got:\n%s", orphans)
	}
	oldest := alertBlock(t, out, "OpenPROldestOverdue")
	if !strings.Contains(oldest, `max(gardenio_open_pr_oldest_age_hours{state="open"}) > 48`) {
		t.Errorf("OpenPROldestOverdue must fire above 48h, got:\n%s", oldest)
	}
	stale := alertBlock(t, out, "OpenPRHealthStale")
	if !strings.Contains(stale, "time() - max(gardenio_open_pr_health_timestamp_seconds) > 14400") {
		t.Errorf("OpenPRHealthStale must fire 4h after the last count, got:\n%s", stale)
	}
	for name, block := range map[string]string{"OpenPROrphansHigh": orphans, "OpenPROldestOverdue": oldest, "OpenPRHealthStale": stale} {
		for _, want := range []string{`severity: "warn"`, "component: pr-sweeper", "subsystem: open-prs", "#openprorphanshigh", "gardenio-system-health?viewPanel=2"} {
			if !strings.Contains(block, want) {
				t.Errorf("%s missing %q, got:\n%s", name, want, block)
			}
		}
	}

	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(append([]string{}, gapAlertsNotifierArgs...), "vmalert.gapAlerts.openPRs.enabled=false")...)
	if strings.Contains(off, "OpenPR") {
		t.Errorf("openPRs.enabled=false must suppress the open-PR alerts, got:\n%s", off)
	}

	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(append([]string{}, gapAlertsNotifierArgs...),
			"vmalert.gapAlerts.openPRs.maxOrphans=10",
			"vmalert.gapAlerts.openPRs.maxOldestOpenHours=72",
			"vmalert.gapAlerts.openPRs.severity=page")...)
	if b := alertBlock(t, tuned, "OpenPROrphansHigh"); !strings.Contains(b, "> 10") || !strings.Contains(b, `severity: "page"`) {
		t.Errorf("maxOrphans and severity must parameterize OpenPROrphansHigh, got:\n%s", b)
	}
	if b := alertBlock(t, tuned, "OpenPROldestOverdue"); !strings.Contains(b, "> 72") {
		t.Errorf("maxOldestOpenHours must parameterize OpenPROldestOverdue, got:\n%s", b)
	}
}

// The k3s overlay runs vmalert with Alertmanager: it must arm the alerts.
func TestOpenPRAlerts_K3sOverlay(t *testing.T) {
	t.Parallel()
	rules := helmTemplateShowOnlyWithValues(t, "templates/vmalert/configmap.yaml", []string{k3sOverlay})
	for _, alert := range []string{"OpenPROrphansHigh", "OpenPROldestOverdue", "OpenPRHealthStale"} {
		alertBlock(t, rules, alert)
	}
}

// TestOpenPRAlerts_Fires is the seeded-value proof: promtool feeds synthetic
// gauges (open_pr_rules_test.yaml) to the three rules extracted from
// gap-alerts.yaml, without annotations (promtool matches annotations exactly;
// TestOpenPRAlerts_Render covers them). Skips when helm/yq/promtool are not on
// PATH.
func TestOpenPRAlerts_Fires(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool firing proof", bin)
		}
	}

	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "open_pr_rules.yaml")
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/vmalert/configmap.yaml "+
			"--set "+strings.Join(gapAlertsNotifierArgs, ",")+" | "+
			`yq eval '.data."gap-alerts.yaml"' - | `+
			`yq eval '.groups[].rules |= map(select(.alert // "" | test("^OpenPR")) | del(.annotations)) | .groups |= map(select(.rules | length > 0))' -`)
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render rules failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(rulesPath, out, 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := exec.Command("promtool", "check", "rules", rulesPath).CombinedOutput(); err != nil {
		t.Fatalf("promtool check rules failed: %v\n%s", err, res)
	}

	tmplBytes, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "open_pr_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testPath := filepath.Join(tmp, "open_pr_test.yaml")
	if err := os.WriteFile(testPath, []byte(strings.ReplaceAll(string(tmplBytes), "__RULES_FILE__", rulesPath)), 0o644); err != nil {
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
