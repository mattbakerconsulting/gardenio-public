package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// Slack assistant-pane silent-delivery smoke check (ka-0000000892 NEXT STEP #3:
// "add a prod-log smoke check that handleAssistantMessage fires").
//
// The chart renders a ServiceMonitor + VMServiceScrape that scrape the
// slack-bridge :9093 /metrics endpoint (where gardenio_assistant_event_total is
// exported) and a PrometheusRule (AssistantPaneSilent) that fires when panes
// are being opened (event="thread_started" rising) while zero typed messages
// are handled (event="message" flat). All three are OFF by default; per-env
// overlays opt in.

func TestAssistantPaneAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/slack-bridge/assistant-pane-alert.yaml",
		"slackBridge.enabled=true",
	)
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when assistantPane.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestAssistantPaneAlert_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/assistant-pane-alert.yaml",
		"slackBridge.enabled=true",
		"slackBridge.metrics.prometheusRule.assistantPane.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "alert: AssistantPaneSilent") {
		t.Errorf("AssistantPaneSilent alert missing, got:\n%s", out)
	}
	// The predicate must reference BOTH the opens signal (thread_started) and
	// the delivery signal (message) over the same window — that is the whole
	// "lifecycle fine, message.im dropped" detection.
	for _, want := range []string{
		`gardenio_assistant_event_total{event="thread_started"}`,
		`gardenio_assistant_event_total{event="message"}`,
		"== 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expr must contain %q, got:\n%s", want, out)
		}
	}
	// Defaults (ka-0000000664): >= 1 opens floor over a 24h window, page
	// severity, 2h hold. The 24h/minOpens=1 tuning is what makes the alert
	// practically fireable at prod's ~0.3 pane-opens/day — a 1h/>=3 floor was
	// effectively unfireable (matched zero samples over 30d). The chart default
	// severity stays "page"; per-env overlays (prod) drop it to "ticket".
	if !strings.Contains(out, ">= 1") {
		t.Errorf("default minOpens floor (>= 1) not in expr, got:\n%s", out)
	}
	if !strings.Contains(out, "[24h]") {
		t.Errorf("default 24h window not in expr, got:\n%s", out)
	}
	// Guard against a regression back to the unfireable 1h/>=3 tuning.
	if strings.Contains(out, "[1h]") {
		t.Errorf("expr must NOT use the unfireable 1h window at ~0.3 opens/day, got:\n%s", out)
	}
	if strings.Contains(out, ">= 3") {
		t.Errorf("expr must NOT use the unfireable >= 3 opens floor at ~0.3 opens/day, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "page"`) {
		t.Errorf("default severity must be page, got:\n%s", out)
	}
	if !strings.Contains(out, `for: "2h"`) {
		t.Errorf("default for-hold must be 2h, got:\n%s", out)
	}
}

func TestAssistantPaneAlert_ThresholdsConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/assistant-pane-alert.yaml",
		"slackBridge.enabled=true",
		"slackBridge.metrics.prometheusRule.assistantPane.enabled=true",
		"slackBridge.metrics.prometheusRule.assistantPane.minOpens=10",
		"slackBridge.metrics.prometheusRule.assistantPane.window=6h",
		"slackBridge.metrics.prometheusRule.assistantPane.severity=ticket",
		"slackBridge.metrics.prometheusRule.assistantPane.for=30m",
	)
	if !strings.Contains(out, ">= 10") {
		t.Errorf("minOpens override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, "[6h]") {
		t.Errorf("window override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `severity: "ticket"`) {
		t.Errorf("severity override not applied, got:\n%s", out)
	}
	if !strings.Contains(out, `for: "30m"`) {
		t.Errorf("for override not applied, got:\n%s", out)
	}
}

func TestAssistantPaneAlert_RuleSelectorLabelsApplied(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/assistant-pane-alert.yaml",
		"slackBridge.enabled=true",
		"slackBridge.metrics.prometheusRule.assistantPane.enabled=true",
		"slackBridge.metrics.prometheusRule.assistantPane.prometheusRuleSelectorLabels.release=kube-prometheus-stack",
	)
	if !strings.Contains(out, "release: kube-prometheus-stack") {
		t.Errorf("prometheusRuleSelectorLabels must land on PrometheusRule metadata, got:\n%s", out)
	}
}

// The alert is only useful if something scrapes the metric it reads. Guard the
// scrape resources too: a ServiceMonitor (prometheus-operator) and a
// VMServiceScrape (vm-operator), both selecting the slack-bridge service and
// hitting the `metrics` port (9093).

func TestSlackBridgeServiceMonitor_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/slack-bridge/servicemonitor.yaml",
		"slackBridge.enabled=true",
	)
	if err == nil && strings.Contains(out, "kind: ServiceMonitor") {
		t.Errorf("ServiceMonitor must NOT render when serviceMonitor.enabled=false, got:\n%s", out)
	}
}

func TestSlackBridgeServiceMonitor_EnabledRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/servicemonitor.yaml",
		"slackBridge.enabled=true",
		"slackBridge.metrics.serviceMonitor.enabled=true",
	)
	if !strings.Contains(out, "kind: ServiceMonitor") {
		t.Fatalf("ServiceMonitor must render when enabled=true, got:\n%s", out)
	}
	for _, want := range []string{
		"app.kubernetes.io/component: slack-bridge",
		"port: metrics",
		"path: /metrics",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ServiceMonitor must contain %q, got:\n%s", want, out)
		}
	}
}

// ── vmalert ConfigMap rule path (ka-0000004766) ──────────────────────────────
//
// On a VictoriaMetrics-operator cluster the PrometheusRule CRD above is
// INERT: the VM operator converts it to a VMRule but no VMAlert evaluates it
// (see the gap-alerts header in templates/vmalert/configmap.yaml). The fireable path is the SAME
// rule (shared define gardenio.slackBridge.assistantPaneSilentRule — zero
// drift) rendered as an assistant-pane-alert.yaml key in the vmalert rules
// ConfigMap, gated on assistantPane.enabled AND a wired notifier (alerting
// rules without a notifier crashloop vmalert — same contract as gap-alerts).

func TestAssistantPaneAlert_VmalertKeyRendersWhenNotifierWired(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		append(gapAlertsNotifierArgs,
			"slackBridge.metrics.prometheusRule.assistantPane.enabled=true",
			"slackBridge.metrics.prometheusRule.assistantPane.severity=ticket",
		)...)
	if !strings.Contains(out, "assistant-pane-alert.yaml") {
		t.Fatalf("assistant-pane-alert.yaml key must render when enabled + notifier wired, got:\n%s", out)
	}
	pane := out[strings.Index(out, "assistant-pane-alert.yaml"):]
	if !strings.Contains(pane, "alert: AssistantPaneSilent") {
		t.Errorf("AssistantPaneSilent must render on the vmalert path, got:\n%s", pane)
	}
	// The shared define carries the fireable tuning (ka-0000000664) verbatim.
	for _, want := range []string{
		`sum(increase(gardenio_assistant_event_total{event="thread_started"}[24h])) >= 1`,
		`sum(increase(gardenio_assistant_event_total{event="message"}[24h])) == 0`,
		`severity: "ticket"`,
		`for: "2h"`,
	} {
		if !strings.Contains(pane, want) {
			t.Errorf("vmalert-path rule must contain %q, got:\n%s", want, pane)
		}
	}
}

func TestAssistantPaneAlert_VmalertKeyAbsentWithoutNotifier(t *testing.T) {
	t.Parallel()
	// Recording-only vmalert (no notifier) must stay clean even with the pane
	// alert enabled — vmalert crashloops on alerting rules without a notifier.
	out := helmTemplate(t, "templates/vmalert/configmap.yaml",
		"vmalert.enabled=true",
		"vmsingle.enabled=true",
		"slackBridge.metrics.prometheusRule.assistantPane.enabled=true",
	)
	if strings.Contains(out, "assistant-pane-alert.yaml") ||
		strings.Contains(out, "alert: AssistantPaneSilent") {
		t.Errorf("assistant-pane rule must NOT render without a notifier, got:\n%s", out)
	}
}

func TestAssistantPaneAlert_VmalertKeyDefaultOff(t *testing.T) {
	t.Parallel()
	// Notifier wired but assistantPane.enabled left at the chart default
	// (false): the pane key must not appear (per-env overlays opt in).
	out := helmTemplate(t, "templates/vmalert/configmap.yaml", gapAlertsNotifierArgs...)
	if strings.Contains(out, "assistant-pane-alert.yaml") {
		t.Errorf("assistant-pane key must NOT render at the chart default, got:\n%s", out)
	}
}

// helmTemplateShowOnlyWithValues renders a single template with one or more
// values files layered in (helm `-f`), so overlay-specific assertions see only
// that template's output — not every rule in the chart.
func helmTemplateShowOnlyWithValues(t *testing.T, showOnly string, valuesFiles []string, setFlags ...string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t), "--show-only", showOnly}
	for _, f := range valuesFiles {
		args = append(args, "-f", chartDir(t)+"/"+f)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

func TestSlackBridgeVMServiceScrape_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/slack-bridge/vmservicescrape.yaml",
		"slackBridge.enabled=true",
	)
	if err == nil && strings.Contains(out, "kind: VMServiceScrape") {
		t.Errorf("VMServiceScrape must NOT render when victoriaMetrics.enabled=false, got:\n%s", out)
	}
}

func TestSlackBridgeVMServiceScrape_EnabledRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/vmservicescrape.yaml",
		"slackBridge.enabled=true",
		"monitoring.victoriaMetrics.enabled=true",
	)
	if !strings.Contains(out, "kind: VMServiceScrape") {
		t.Fatalf("VMServiceScrape must render when victoriaMetrics enabled, got:\n%s", out)
	}
	for _, want := range []string{
		"app.kubernetes.io/component: slack-bridge",
		"port: metrics",
		"path: /metrics",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("VMServiceScrape must contain %q, got:\n%s", want, out)
		}
	}
}
