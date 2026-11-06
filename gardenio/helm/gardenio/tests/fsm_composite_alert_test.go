package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Composite FSM alert (ka-0000006124, plan §5.7 / FSM_CORRELATION_CONTRACT §5a).
//
// The chart renders a single PrometheusRule (FSMAgentOOMWithECSRestart) that
// fires when a gardenio agent is OOM-killed AND an ECS task restart is observed
// in the same (env, cluster, service) window. It is OFF by default; per-env
// overlays opt in. It is a plain monitoring.coreos.com/v1 PrometheusRule routed
// by the EXISTING ACME kube-prometheus-stack Alertmanager — the chart does NOT
// stand up a second Alertmanager.

func TestFSMCompositeAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	// With fsmCompositeAlert.enabled=false (the default), the template renders
	// nothing — helm exits non-zero with "could not find template" when
	// --show-only points at an all-empty template.
	out, err := helmTemplateAllowFail(t, "templates/fsm-composite-alert.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when fsmCompositeAlert.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestFSMCompositeAlert_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/fsm-composite-alert.yaml",
		"fsmCompositeAlert.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when fsmCompositeAlert.enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: FSMAgentOOMWithECSRestart") {
		t.Errorf("FSMAgentOOMWithECSRestart alert missing, got:\n%s", out)
	}
	// The composite predicate must reference BOTH estates' signals and the
	// gardenio-owned pivot.
	for _, want := range []string{
		`gardenio_agent_lifecycle_events_total{event="oom_killed"}`,
		"aws_ecs_task_restart_total",
		"gardenio_resource_mapping",
		"and on (env, cluster, service)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expr must contain %q, got:\n%s", want, out)
		}
	}
	if !strings.Contains(out, `severity: "page"`) {
		t.Errorf("default severity must be page, got:\n%s", out)
	}
}

func TestFSMCompositeAlert_DeepLinksAndS3Annotations(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/fsm-composite-alert.yaml",
		"fsmCompositeAlert.enabled=true",
		"fsmCompositeAlert.grafanaBaseURL=https://gardenio.app.e2e.dev.acme.example/grafana",
		"agents.s3.bucket=gardenio-sessions",
	)
	// Deep links to BOTH dashboards (acceptance: deep links to BOTH dashboards).
	if !strings.Contains(out, "dashboard_agent_url:") || !strings.Contains(out, "/d/gardenio-cost-overview?") {
		t.Errorf("agent dashboard deep link missing, got:\n%s", out)
	}
	if !strings.Contains(out, "dashboard_acme_url:") || !strings.Contains(out, "/d/fsm-correlation?") {
		t.Errorf("ACME dashboard deep link missing, got:\n%s", out)
	}
	// Session-recording S3 key (acceptance: the session-recording S3 key).
	if !strings.Contains(out, "session_recording_s3:") {
		t.Errorf("session_recording_s3 annotation missing, got:\n%s", out)
	}
	if !strings.Contains(out, "s3://gardenio-sessions/coop/sessions/") {
		t.Errorf("S3 key must use the configured bucket/prefix, got:\n%s", out)
	}
}

func TestFSMCompositeAlert_S3DefaultsFromAgentsS3(t *testing.T) {
	t.Parallel()
	// When fsmCompositeAlert.s3 is unset, the S3 key falls back to agents.s3.
	out := helmTemplate(t, "templates/fsm-composite-alert.yaml",
		"fsmCompositeAlert.enabled=true",
		"agents.s3.bucket=other-bucket",
		"agents.s3.prefix=tenants/abc/sessions",
	)
	if !strings.Contains(out, "s3://other-bucket/tenants/abc/sessions/") {
		t.Errorf("S3 key must default from agents.s3, got:\n%s", out)
	}
}

func TestFSMCompositeAlert_SeverityConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/fsm-composite-alert.yaml",
		"fsmCompositeAlert.enabled=true",
		"fsmCompositeAlert.severity=ticket",
	)
	if !strings.Contains(out, `severity: "ticket"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
}

func TestFSMCompositeAlert_RuleSelectorLabelsApplied(t *testing.T) {
	t.Parallel()
	// The existing kube-prometheus-stack ruleSelector commonly matches on a
	// release label; prometheusRuleSelectorLabels must land on the CR metadata
	// so the rule is discovered (and routed by the existing Alertmanager).
	out := helmTemplate(t, "templates/fsm-composite-alert.yaml",
		"fsmCompositeAlert.enabled=true",
		"fsmCompositeAlert.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Errorf("prometheusRuleSelectorLabels must land on PrometheusRule metadata, got:\n%s", out)
	}
}

// TestFSMCompositeAlert_SyntheticOOMRestartFires is the acceptance proof: it
// renders the alert rule and feeds promtool synthetic series for an OOM and an
// ECS task restart in the same (env,cluster,service) window, asserting the
// composite alert fires (with both deep links + the S3 key resolved) and that
// the OOM-only / restart-only / unmapped-service cases do NOT fire. The full
// promtool unit-test cases live in fsm_composite_alert_rules_test.yaml.
//
// Skips gracefully when helm/yq/promtool are not on PATH (e.g. a bare `go test`
// without the chart toolchain) — the helm-rendered render tests above still run.
func TestFSMCompositeAlert_SyntheticOOMRestartFires(t *testing.T) {
	t.Parallel()
	for _, bin := range []string{"helm", "yq", "promtool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH; skipping promtool firing proof", bin)
		}
	}

	tmp := t.TempDir()
	rulesPath := filepath.Join(tmp, "fsm_rules.yaml")

	// Render the rule spec to a plain Prometheus rules file via helm | yq.
	render := exec.Command("bash", "-c",
		"helm template test "+chartDir(t)+" --show-only templates/fsm-composite-alert.yaml "+
			"--set fsmCompositeAlert.enabled=true "+
			"--set fsmCompositeAlert.grafanaBaseURL=https://gardenio.app.e2e.dev.acme.example/grafana "+
			"--set agents.s3.bucket=gardenio-sessions | yq eval '.spec' -")
	out, err := render.CombinedOutput()
	if err != nil {
		t.Fatalf("render rules failed: %v\n%s", err, out)
	}
	if err := os.WriteFile(rulesPath, out, 0o644); err != nil {
		t.Fatal(err)
	}

	// Build the promtool test file by pointing rule_files at the rendered spec.
	tmplBytes, err := os.ReadFile(filepath.Join(chartDir(t), "tests", "fsm_composite_alert_rules_test.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	testYAML := strings.ReplaceAll(string(tmplBytes), "__RULES_FILE__", rulesPath)
	testPath := filepath.Join(tmp, "fsm_test.yaml")
	if err := os.WriteFile(testPath, []byte(testYAML), 0o644); err != nil {
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
