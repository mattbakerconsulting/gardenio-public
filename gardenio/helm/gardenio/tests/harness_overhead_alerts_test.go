package tests

import (
	"strings"
	"testing"
)

// Gardenio harness tax per tool call (gd-RpflV68KX1, plan
// gardenio/plans/agent-tool-latency-attribution.md P3, epic gd-Oc3GVm9gc6).
// The chart renders one PrometheusRule with a single alert,
// HarnessOverheadP95High, keyed off gardenio_tool_harness_overhead_seconds.
// OFF by default; per-env overlays opt in.

func TestHarnessOverheadAlerts_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/controller/harness-overhead-alerts.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when harnessOverhead.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestHarnessOverheadAlerts_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/harness-overhead-alerts.yaml",
		"agents.metrics.prometheusRule.harnessOverhead.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: HarnessOverheadP95High") {
		t.Errorf("HarnessOverheadP95High alert missing, got:\n%s", out)
	}
	for _, want := range []string{
		"gardenio_tool_harness_overhead_seconds_bucket",
		"sum by (le, phase)",
		"> 1",
		`for: "10m"`,
		`severity: "ticket"`,
		"{{ $labels.phase }}",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expr/annotations must contain %q, got:\n%s", want, out)
		}
	}
}

func TestHarnessOverheadAlerts_ThresholdsConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/harness-overhead-alerts.yaml",
		"agents.metrics.prometheusRule.harnessOverhead.enabled=true",
		"agents.metrics.prometheusRule.harnessOverhead.p95ThresholdSeconds=2.5",
		"agents.metrics.prometheusRule.harnessOverhead.for=5m",
		"agents.metrics.prometheusRule.harnessOverhead.severity=page",
	)
	if !strings.Contains(out, "> 2.5") {
		t.Errorf("p95ThresholdSeconds override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `for: "5m"`) {
		t.Errorf("for override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "page"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
}

func TestHarnessOverheadAlerts_RuleSelectorLabelsApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/harness-overhead-alerts.yaml",
		"agents.metrics.prometheusRule.harnessOverhead.enabled=true",
		"agents.metrics.prometheusRule.harnessOverhead.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Errorf("prometheusRuleSelectorLabels must land on PrometheusRule metadata, got:\n%s", out)
	}
}
