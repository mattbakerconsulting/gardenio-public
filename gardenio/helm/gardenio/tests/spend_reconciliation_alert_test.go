package tests

import (
	"strings"
	"testing"
)

// E3 spend-as-counter reconciliation alert (ka-0000004289, plan §2.E3 v1
// acceptance). The chart renders a single PrometheusRule
// (SpendCounterDriftFromLiteLLM) that fires when the counter-derived 1h spend
// (increase(gardenio_agent_spend_usd_total[1h])) diverges from the LiteLLM
// /key/info backstop (delta(gardenio_account_cost_usd[1h])) by more than the
// configured drift threshold. OFF by default; per-env overlays opt in.

func TestSpendReconciliationAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/spend-reconciliation-alert.yaml")
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when spendReconciliation.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestSpendReconciliationAlert_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/spend-reconciliation-alert.yaml",
		"agents.metrics.prometheusRule.spendReconciliation.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: SpendCounterDriftFromLiteLLM") {
		t.Errorf("SpendCounterDriftFromLiteLLM alert missing, got:\n%s", out)
	}
	// The reconciliation predicate must reference BOTH the counter (E3.2) and the
	// LiteLLM backstop gauge (spendSync), grouped per account.
	for _, want := range []string{
		"increase(gardenio_agent_spend_usd_total[1h])",
		"delta(gardenio_account_cost_usd[1h])",
		"sum by (account)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expr must contain %q, got:\n%s", want, out)
		}
	}
	// Defaults: >1% drift, ticket severity.
	if !strings.Contains(out, "> 0.01") {
		t.Errorf("default drift threshold 0.01 not in expr, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "ticket"`) {
		t.Errorf("default severity must be ticket, got:\n%s", out)
	}
}

func TestSpendReconciliationAlert_ThresholdsConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/spend-reconciliation-alert.yaml",
		"agents.metrics.prometheusRule.spendReconciliation.enabled=true",
		"agents.metrics.prometheusRule.spendReconciliation.driftThreshold=0.05",
		"agents.metrics.prometheusRule.spendReconciliation.minSpendUSD=5",
		"agents.metrics.prometheusRule.spendReconciliation.severity=page",
	)
	if !strings.Contains(out, "> 0.05") {
		t.Errorf("driftThreshold override not applied, got:\n%s", out)
	}
	// minSpendUSD feeds both the clamp_min floor and the gating clause.
	if !strings.Contains(out, "clamp_min(sum by (account) (delta(gardenio_account_cost_usd[1h])), 5)") {
		t.Errorf("minSpendUSD override not applied to clamp_min, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "page"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
}

func TestSpendReconciliationAlert_RuleSelectorLabelsApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/spend-reconciliation-alert.yaml",
		"agents.metrics.prometheusRule.spendReconciliation.enabled=true",
		"agents.metrics.prometheusRule.spendReconciliation.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Errorf("prometheusRuleSelectorLabels must land on PrometheusRule metadata, got:\n%s", out)
	}
}
