package tests

import (
	"strings"
	"testing"
)

// Phase G — CI/CD compute-minutes (task ka-0000002378). Two guards:
//   1. the cicd-minutes Grafana dashboard is registered on the controller
//      grafana-dashboard ConfigMap and embeds as parseable JSON with its stable
//      uid, template vars, and the headline metric families; and
//   2. the gitlab-bridge deployment renders the CICD_* env only when the
//      collector is opted in, and keeps the first-party series independently
//      gated (dark until the group-owner token lands, escalation ka-0000006982).

func TestCICDMinutesDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, "gardenio-cicd-minutes.json")
	if d.UID != "cicd-minutes" {
		t.Errorf("cicd-minutes board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("cicd-minutes board must have panels")
	}
}

func TestCICDMinutesDashboard_HeadlineMetricsAndTemplating(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, "gardenio-cicd-minutes.json")
	exprs := allExprs(d)

	// Both series' headline families must be queried somewhere on the board:
	// the job-duration counters + histogram, and the first-party usage/quota
	// gauges (Phase R §5).
	wantMetric := []string{
		"gardenio_cicd_minutes_total",
		"gardenio_cicd_queued_seconds_total",
		"gardenio_cicd_job_duration_seconds_bucket",
		"gardenio_cicd_gitlab_minutes_used_current",
		"gardenio_cicd_gitlab_quota_total_current",
		"gardenio_cicd_gitlab_remaining_current",
		"gardenio_cicd_gitlab_exceeded_current",
		"gardenio_cicd_scrape_errors_total",
		"gardenio_cicd_last_success_timestamp_seconds",
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("cicd-minutes board must query %s somewhere; not found", m)
		}
	}
	// The wasted-CI panel must weight failed/canceled minutes.
	if !strings.Contains(exprs, `status=~\"failed|canceled\"`) && !strings.Contains(exprs, `status=~"failed|canceled"`) {
		t.Error("cicd-minutes board must have a wasted-CI panel filtering failed|canceled status")
	}
	// The per-job panel must expose the p95 tail via histogram_quantile.
	if !strings.Contains(exprs, "histogram_quantile(0.95") {
		t.Error("cicd-minutes board must compute per-job duration p95")
	}
}

// gitlabBridgeDeployment renders the gitlab-bridge Deployment with the given
// extra --set flags on top of the minimum needed to enable it.
func gitlabBridgeDeployment(t *testing.T, setFlags ...string) string {
	t.Helper()
	base := []string{
		"gitlabBridge.enabled=true",
		"gitlabBridge.gitlab.baseURL=https://gitlab.com",
		"gitlabBridge.gitlab.secretName=gitlab-token",
	}
	return helmTemplate(t, "templates/gitlab-bridge/deployment.yaml", append(base, setFlags...)...)
}

// The shipped default now enables the job-duration collector (ka-0000001422):
// jobDurationEnabled=true + projects=[monorepo, gardenio], running on the
// existing gitlab.secretName GAT. The first-party series stays independently
// gated OFF (dark until the group-owner token lands, escalation ka-0000006982).
func TestCICDMetricsEnv_DefaultOn(t *testing.T) {
	t.Parallel()
	out := gitlabBridgeDeployment(t)
	if !strings.Contains(out, "CICD_GITLAB_METRICS_ENABLED") {
		t.Error("job-duration collector must be enabled by default; CICD_GITLAB_METRICS_ENABLED not rendered")
	}
	if strings.Contains(out, "CICD_GITLAB_FIRSTPARTY_ENABLED") {
		t.Error("first-party series must stay OFF by default (independent gating), but it rendered")
	}
	for _, proj := range []string{"example-org/monorepo", "example-org/gardenio"} {
		if !strings.Contains(out, proj) {
			t.Errorf("default CICD_METRICS_PROJECTS must carry %q; not found", proj)
		}
	}
}

func TestCICDMetricsEnv_JobDurationOnly(t *testing.T) {
	t.Parallel()
	out := gitlabBridgeDeployment(t,
		"gitlabBridge.cicdMetrics.jobDurationEnabled=true",
		"gitlabBridge.cicdMetrics.projects={Acme/Platform/gardenio}",
	)
	if !strings.Contains(out, "CICD_GITLAB_METRICS_ENABLED") {
		t.Error("job-duration series must set CICD_GITLAB_METRICS_ENABLED when enabled")
	}
	if strings.Contains(out, "CICD_GITLAB_FIRSTPARTY_ENABLED") {
		t.Error("first-party series must stay OFF when only jobDurationEnabled is set (independent gating)")
	}
	if !strings.Contains(out, "Acme/Platform/gardenio") {
		t.Error("CICD_METRICS_PROJECTS must carry the configured project path")
	}
}

func TestCICDMetricsEnv_FirstPartyKnobs(t *testing.T) {
	t.Parallel()
	out := gitlabBridgeDeployment(t,
		"gitlabBridge.cicdMetrics.firstPartyEnabled=true",
		"gitlabBridge.cicdMetrics.firstPartyNamespaceID=gid://gitlab/Group/14331212",
		"gitlabBridge.cicdMetrics.firstPartyGroupPath=Acme",
	)
	for _, want := range []string{
		"CICD_GITLAB_FIRSTPARTY_ENABLED",
		"gid://gitlab/Group/14331212",
		"CICD_FIRSTPARTY_GROUP_PATH",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("first-party env must render %q when firstPartyEnabled, not found", want)
		}
	}
}
