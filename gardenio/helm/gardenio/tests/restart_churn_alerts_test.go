package tests

import (
	"strings"
	"testing"
)

// Agent restart churn and runtime-ceiling alerts (gd-5MTDWjwNjm, epic
// gd-aGiTndCXA4: bound agent restart churn). The chart renders one
// PrometheusRule with two alerts: AgentRestartChurn (too many coop
// sessions while still young) and AgentRuntimeCeiling (an agent of any role
// open past the max age). OFF by default; per-env overlays opt in.
//
// No CrewOOMKilled here — it already exists twice (templates/vpa-guardrails
// /alerts.yaml's CrewOOMKilled on the literal kube_pod_container_status_
// last_terminated_reason metric, and templates/vmalert/configmap.yaml's
// AgentOOMKilled role-scoped view of gardenio_agent_lifecycle_events_total);
// see restart-churn-alerts.yaml's top comment for why a third was skipped.

func TestRestartChurnAlerts_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/controller/restart-churn-alerts.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when restartChurn.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestRestartChurnAlerts_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/restart-churn-alerts.yaml",
		"agents.metrics.prometheusRule.restartChurn.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	for _, alert := range []string{
		"alert: AgentRestartChurn",
		"alert: AgentRuntimeCeiling",
	} {
		if !strings.Contains(out, alert) {
			t.Errorf("%s alert missing, got:\n%s", alert, out)
		}
	}
	if strings.Contains(out, "alert: CrewOOMKilled") {
		t.Errorf("CrewOOMKilled must not render here — it already exists in vpa-guardrails/alerts.yaml, got:\n%s", out)
	}
	// AgentRestartChurn must key off both source gauges and name the agent
	// + role in its annotations (acceptance criteria).
	for _, want := range []string{
		"gardenio_agent_sessions_current > 4",
		"gardenio_agent_age_seconds > 3600",
		"{{ $labels.agent }}",
		"{{ $labels.role }}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("AgentRestartChurn expr/annotations must contain %q, got:\n%s", want, out)
		}
	}
	// AgentRuntimeCeiling: default 6h ceiling (21600s), no role restriction.
	if !strings.Contains(out, "gardenio_agent_age_seconds > 21600") {
		t.Errorf("AgentRuntimeCeiling default 6h (21600s) threshold not in expr, got:\n%s", out)
	}
	// Defaults: 15m/10m for holds, warning severity.
	for _, want := range []string{
		`for: "15m"`,
		`for: "10m"`,
		`severity: "warning"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default %q not found, got:\n%s", want, out)
		}
	}
}

func TestRestartChurnAlerts_ThresholdsConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/restart-churn-alerts.yaml",
		"agents.metrics.prometheusRule.restartChurn.enabled=true",
		"agents.metrics.prometheusRule.restartChurn.sessionsThreshold=8",
		"agents.metrics.prometheusRule.restartChurn.minAgeSeconds=1800",
		"agents.metrics.prometheusRule.restartChurn.maxAgeSeconds=7200",
		"agents.metrics.prometheusRule.restartChurn.churnFor=30m",
		"agents.metrics.prometheusRule.restartChurn.ceilingFor=5m",
		"agents.metrics.prometheusRule.restartChurn.severity=page",
	)
	if !strings.Contains(out, "gardenio_agent_sessions_current > 8") {
		t.Errorf("sessionsThreshold override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_agent_age_seconds > 1800") {
		t.Errorf("minAgeSeconds override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_agent_age_seconds > 7200") {
		t.Errorf("maxAgeSeconds override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `for: "30m"`) {
		t.Errorf("churnFor override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `for: "5m"`) {
		t.Errorf("ceilingFor override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "page"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
}

func TestRestartChurnAlerts_RuleSelectorLabelsApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/restart-churn-alerts.yaml",
		"agents.metrics.prometheusRule.restartChurn.enabled=true",
		"agents.metrics.prometheusRule.restartChurn.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Errorf("prometheusRuleSelectorLabels must land on PrometheusRule metadata, got:\n%s", out)
	}
}
