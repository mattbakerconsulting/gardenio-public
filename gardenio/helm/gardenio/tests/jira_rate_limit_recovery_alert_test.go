package tests

import (
	"strings"
	"testing"
)

// JIRA rate-limit recovery monitor alert (task ka-0000002481, tuning of the FSM
// Jira Triage gardener ka-0000005655). The JQL-count collector publishes
// gardenio_jira_rate_limited (0/1), gardenio_jira_rate_limit_events_total,
// gardenio_jira_rate_limit_recoveries_total, and gardenio_jira_rate_limit_backoff_seconds
// from the jira-bridge :9093 endpoint; this rule pages while a query is stuck
// throttled and auto-resolves on the recovery edge. Off by default; per-env
// overlays opt in.

func TestJiraRateLimitRecoveryAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/jira-bridge/rate-limit-recovery-alert.yaml",
		"jiraBridge.enabled=true",
	)
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when rateLimitRecovery.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestJiraRateLimitRecoveryAlert_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/rate-limit-recovery-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.rateLimitRecovery.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "JiraRateLimitStuck") {
		t.Errorf("expected JiraRateLimitStuck alert, got:\n%s", out)
	}
	// The verdict is folded into the 0/1 gauge, so the expr reads the
	// rate_limited gauge as a max_over_time == 1 — auto-resolving on the
	// recovery edge (a clean scrape dropping the gauge to 0).
	if !strings.Contains(out, "gardenio_jira_rate_limited") ||
		!strings.Contains(out, "== 1") {
		t.Errorf("stuck expr must fire on the rate_limited gauge == 1, got:\n%s", out)
	}
	// Default severity is ticket (a stuck JQL-count query is degraded telemetry,
	// not a customer-facing outage).
	if !strings.Contains(out, "severity: \"ticket\"") {
		t.Errorf("default severity must be ticket, got:\n%s", out)
	}
	// The alert must name its runbook (the task bead) under global.docsBaseURL.
	if !strings.Contains(out, "https://github.com/mattbakerconsulting/gardenio/blob/main/docs/user-guide/01-overview.md#beads (archived bead ka-0000002481)") {
		t.Errorf("alert must name archived runbook bead ka-0000002481 under docsBaseURL, got:\n%s", out)
	}
}

func TestJiraRateLimitRecoveryAlert_ThresholdConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/rate-limit-recovery-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.rateLimitRecovery.enabled=true",
		"jiraBridge.metrics.prometheusRule.rateLimitRecovery.window=30m",
		"jiraBridge.metrics.prometheusRule.rateLimitRecovery.for=20m",
		"jiraBridge.metrics.prometheusRule.rateLimitRecovery.severity=page",
	)
	if !strings.Contains(out, "gardenio_jira_rate_limited[30m]") {
		t.Errorf("window must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "for: \"20m\"") {
		t.Errorf("for-hold must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"page\"") {
		t.Errorf("severity must be configurable via values, got:\n%s", out)
	}
}
