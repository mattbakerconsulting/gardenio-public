package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Postgres S3 backup staleness alerts (gd-Li0fBnupv2). Before these, nothing
// paged when the postgres S3 backup CronJob failed or never scheduled: on
// 2026-10-20 no backup succeeded for 16 h unseen. The contract enforced here:
//   - PostgresS3BackupStale / PostgresS3BackupUnmonitored render in gap-alerts
//     only when postgres.s3Backup is on, and honor their toggle,
//   - both select exactly this release's backup CronJob,
//   - pod-state-metrics watches cronjobs (flag, allowlist, namespaced batch
//     Role rule) only when postgres.s3Backup is on,
//   - promtool: 4h20m since the last success fires, 3h30m does not, and a
//     missing series fires Unmonitored after 4h.

var backupAlertArgs = append(append([]string{}, gapAlertsNotifierArgs...),
	"postgres.enabled=true",
	"postgres.s3Backup.enabled=true",
	"postgres.s3Backup.bucket=gardenio-hq-backups-123456789012",
)

func TestPostgresS3BackupAlert_OffWithoutS3Backup(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(out, "PostgresS3Backup") {
		t.Errorf("backup alerts must not render while postgres.s3Backup is off, got:\n%s", out)
	}
}

func TestPostgresS3BackupAlert_Renders(t *testing.T) {
	t.Parallel()
	out := helmTemplateNS(t, "gardenio-system", "templates/vmalert/configmap.yaml", backupAlertArgs...)
	sel := `kube_cronjob_status_last_successful_time{namespace="gardenio-system", cronjob="test-postgres-s3-backup"}`

	stale := alertBlock(t, out, "PostgresS3BackupStale")
	if !strings.Contains(stale, "time() - "+sel+"\n                > 14400") {
		t.Errorf("PostgresS3BackupStale must compare the age of this release's CronJob success to 14400 s, got:\n%s", stale)
	}
	if !strings.Contains(stale, `for: "15m"`) {
		t.Errorf("PostgresS3BackupStale must hold for 15m, got:\n%s", stale)
	}

	absent := alertBlock(t, out, "PostgresS3BackupUnmonitored")
	if !strings.Contains(absent, "absent("+sel+")") {
		t.Errorf("PostgresS3BackupUnmonitored must be absent() of the same selector, got:\n%s", absent)
	}
	if !strings.Contains(absent, `for: "4h"`) {
		t.Errorf("PostgresS3BackupUnmonitored must hold for 4h, got:\n%s", absent)
	}

	for _, block := range []string{stale, absent} {
		for _, want := range []string{`severity: "warn"`, "subsystem: backup", "#postgress3backupstale", "dashboard:", "bead:"} {
			if !strings.Contains(block, want) {
				t.Errorf("backup alert missing %q, got:\n%s", want, block)
			}
		}
	}

	off := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(backupAlertArgs, "vmalert.gapAlerts.postgresS3Backup.enabled=false")...)
	if strings.Contains(off, "PostgresS3Backup") {
		t.Errorf("postgresS3Backup.enabled=false must suppress both alerts, got:\n%s", off)
	}

	tuned := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(backupAlertArgs,
			"vmalert.gapAlerts.postgresS3Backup.staleSeconds=21600",
			"vmalert.gapAlerts.postgresS3Backup.severity=page")...)
	tunedStale := alertBlock(t, tuned, "PostgresS3BackupStale")
	if !strings.Contains(tunedStale, "> 21600") || !strings.Contains(tunedStale, "over 6h") {
		t.Errorf("staleSeconds must parameterize the expr and summary, got:\n%s", tunedStale)
	}
	if !strings.Contains(tunedStale, `severity: "page"`) {
		t.Errorf("severity must parameterize the label, got:\n%s", tunedStale)
	}
}

func TestPodStateMetrics_WatchesCronJobsForBackup(t *testing.T) {
	t.Parallel()
	const tmpl = "templates/monitoring/pod-state-metrics.yaml"

	def := helmTemplate(t, tmpl, "monitoring.podStateMetrics.enabled=true")
	if !strings.Contains(def, `"--resources=pods"`) {
		t.Errorf("default exporter must watch pods only, got:\n%s", def)
	}
	if strings.Contains(def, "batch") || strings.Contains(def, "kube_cronjob") {
		t.Errorf("default exporter must not get cronjob RBAC or metrics, got:\n%s", def)
	}

	on := helmTemplate(t, tmpl, "monitoring.podStateMetrics.enabled=true",
		"postgres.enabled=true", "postgres.s3Backup.enabled=true")
	if !strings.Contains(on, `"--resources=pods,cronjobs"`) {
		t.Errorf("exporter must watch cronjobs when postgres.s3Backup is on, got:\n%s", on)
	}
	if !strings.Contains(on, ",kube_cronjob_status_last_successful_time\"") {
		t.Errorf("allowlist must carry kube_cronjob_status_last_successful_time, got:\n%s", on)
	}
	if !strings.Contains(on, "apiGroups: [\"batch\"]\n    resources: [\"cronjobs\"]\n    verbs: [\"list\", \"watch\"]") {
		t.Errorf("Role must grant list/watch on batch cronjobs, got:\n%s", on)
	}
	if strings.Contains(on, "kind: ClusterRole") {
		t.Errorf("exporter RBAC must stay namespace-scoped, got:\n%s", on)
	}

	// The k3s shape: cronjobs only, one metric family, no pod RBAC.
	k3s := helmTemplate(t, tmpl, "monitoring.podStateMetrics.enabled=true",
		"monitoring.podStateMetrics.resourceScope=null",
		"monitoring.podStateMetrics.metricAllowlist[0]=kube_cronjob_status_last_successful_time",
		"postgres.enabled=true", "postgres.s3Backup.enabled=true")
	if !strings.Contains(k3s, `"--resources=cronjobs"`) {
		t.Errorf("an empty resourceScope must still watch the backup cronjob, got:\n%s", k3s)
	}
	if !strings.Contains(k3s, `"--metric-allowlist=kube_cronjob_status_last_successful_time"`) {
		t.Errorf("the allowlist must not duplicate the cronjob family, got:\n%s", k3s)
	}
	if strings.Contains(k3s, `resources: ["pods"]`) {
		t.Errorf("no pod RBAC when pods is not in resourceScope, got:\n%s", k3s)
	}

	_, err := helmTemplateAllowFail(t, tmpl, "monitoring.podStateMetrics.enabled=true",
		"monitoring.podStateMetrics.resourceScope=null")
	if err == nil {
		t.Errorf("an exporter with nothing to watch must fail the render")
	}
}

// TestPostgresS3BackupAlert_Fires feeds promtool synthetic series
// (postgres_s3_backup_rules_test.yaml). Only the two backup rules are extracted
// from gap-alerts.yaml, without annotations (promtool matches annotations
// exactly; TestPostgresS3BackupAlert_Renders covers them). Skips when
// helm/yq/promtool are not on PATH.
func TestPostgresS3BackupAlert_Fires(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool firing proof", bin)
		}
	}

	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "backup_rules.yaml")
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --namespace gardenio-system --show-only templates/vmalert/configmap.yaml "+
			"--set "+strings.Join(backupAlertArgs, ",")+" | "+
			`yq eval '.data."gap-alerts.yaml"' - | `+
			`yq eval '.groups[].rules |= map(select(.alert // "" | test("^PostgresS3Backup")) | del(.annotations)) | .groups |= map(select(.rules | length > 0))' -`)
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

	tmplBytes, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "postgres_s3_backup_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testPath := filepath.Join(tmp, "backup_test.yaml")
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

// The k3s overlay is the one environment with postgres.s3Backup on: it must
// arm both backup alerts and run the exporter on cronjobs alone, with no pod
// RBAC and no per-pod series.
func TestPostgresS3BackupAlert_K3sOverlay(t *testing.T) {
	t.Parallel()
	rules := helmTemplateShowOnlyWithValues(t, "templates/vmalert/configmap.yaml", []string{k3sOverlay})
	for _, alert := range []string{"PostgresS3BackupStale", "PostgresS3BackupUnmonitored"} {
		if !strings.Contains(alertBlock(t, rules, alert), `cronjob="test-postgres-s3-backup"`) {
			t.Errorf("k3s must arm %s on the backup CronJob", alert)
		}
	}

	ksm := helmTemplateShowOnlyWithValues(t, "templates/monitoring/pod-state-metrics.yaml", []string{k3sOverlay})
	for _, want := range []string{
		`"--resources=cronjobs"`,
		`"--metric-allowlist=kube_cronjob_status_last_successful_time"`,
		`resources: ["cronjobs"]`,
	} {
		if !strings.Contains(ksm, want) {
			t.Errorf("k3s pod-state-metrics missing %q, got:\n%s", want, ksm)
		}
	}
	if strings.Contains(ksm, `resources: ["pods"]`) {
		t.Errorf("k3s pod-state-metrics must not watch pods, got:\n%s", ksm)
	}
}
