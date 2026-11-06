package tests

import (
	"strings"
	"testing"
)

// DarkSpendOverThreshold (R6-FU6c, ka-0000007149; pre-autonomy guardrails
// epic ka-0000005876) — the OBSERVE half of the R6-FU6 cost guardrail for the
// autonomous (dark) fleet, authored as a vmalert ConfigMap rule key (the
// fireable-on-prod path, mirroring the rest of the gap-alerts pack) rather than
// a PrometheusRule CRD, which on new prod is converted but never evaluated.
//
// The alert is keyed to litellm-cost-tracking: it reads
// gardenio_agent_spend_usd_total — the per-turn LiteLLM spend counter the
// controller tag_spend_sync daemon feeds from LiteLLM spend logs and the
// litellm-cost-tracking E2E module validates — and pages when the dark fleet's
// trailing-1h aggregate spend rate crosses the operator-set USD/hour threshold.
//
// It ships OFF by default (dark-fleet spend is only in this vmsingle once the
// dark controller's metrics are federated), so these tests pin BOTH the
// default-off contract and the enabled render shape + values wiring.

var darkSpendEnabledArgs = append(append([]string{}, gapAlertsNotifierArgs...),
	"vmalert.gapAlerts.darkSpend.enabled=true")

func TestDarkSpend_OffByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(out, "alert: DarkSpendOverThreshold") {
		t.Errorf("DarkSpendOverThreshold must be OFF by default (dark spend not yet federated), got:\n%s", out)
	}
}

func TestDarkSpend_RendersWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", darkSpendEnabledArgs...)
	if !strings.Contains(out, "alert: DarkSpendOverThreshold") {
		t.Fatalf("DarkSpendOverThreshold must render when darkSpend.enabled=true, got:\n%s", out)
	}
	// Must live inside the FIREABLE gap-alerts key (the path that evaluates on
	// prod), not merely somewhere in the ConfigMap.
	gap := out[strings.Index(out, "gap-alerts.yaml"):]
	if !strings.Contains(gap, "alert: DarkSpendOverThreshold") {
		t.Errorf("DarkSpendOverThreshold must render inside the fireable gap-alerts key, got:\n%s", gap)
	}
}

func TestDarkSpend_KeyedToLitellmCostTracking(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", darkSpendEnabledArgs...)
	block := alertBlock(t, out, "DarkSpendOverThreshold")

	// Keyed to the litellm-cost-tracking spend counter, aggregated as a
	// trailing-1h USD/hour rate — the same quantity the dark-spend dashboard
	// and the gardenio:agent_cost_usd:rate1h recording rule read.
	if !strings.Contains(block, "increase(gardenio_agent_spend_usd_total") {
		t.Errorf("expr must read the litellm-cost-tracking spend counter gardenio_agent_spend_usd_total, got:\n%s", block)
	}
	// Scoped to the dark universe via the default project regex.
	if !strings.Contains(block, `project=~"monorepo-dark"`) {
		t.Errorf("expr must scope to the dark project (monorepo-dark) by default, got:\n%s", block)
	}
	// Operator-set threshold (default 25 USD/hour).
	if !strings.Contains(block, "> 25") {
		t.Errorf("expr must compare against the default 25 USD/hour threshold, got:\n%s", block)
	}
	// The dark universe is labeled so downstream routing can distinguish it.
	if !strings.Contains(block, "universe: dark") {
		t.Errorf("alert must carry the universe: dark label, got:\n%s", block)
	}
	// Deep-links its companion board.
	if !strings.Contains(block, "/d/gardenio-dark-spend/") {
		t.Errorf("dashboard annotation must deep-link the gardenio-dark-spend board, got:\n%s", block)
	}
}

func TestDarkSpend_ThresholdAndProjectOverridable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(append([]string{}, darkSpendEnabledArgs...),
			"vmalert.gapAlerts.darkSpend.threshold=100",
			"vmalert.gapAlerts.darkSpend.projectRegex=monorepo-dark|other-lab")...)
	block := alertBlock(t, out, "DarkSpendOverThreshold")
	if !strings.Contains(block, "> 100") {
		t.Errorf("custom threshold 100 must flow into the expr, got:\n%s", block)
	}
	if !strings.Contains(block, `project=~"monorepo-dark|other-lab"`) {
		t.Errorf("custom projectRegex must flow into the expr, got:\n%s", block)
	}
}
