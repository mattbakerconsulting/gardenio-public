package tests

import (
	"strings"
	"testing"
)

// Inbound-webhook deadman prevention alert (bug ka-0000000426, parent epic
// ka-0000007401). The jira-bridge publishes gardenio_jira_webhook_dark (0/1) and
// gardenio_jira_webhook_evaluations_total{result}; this rule turns them into a
// dark page + a monitor-deadman page, both with a runbook link to the bug.
// Sibling of jira_poller_stall_alert_test.go / jira_trigger_starved_alert_test.go.
// Off by default; per-env overlays opt in.

func TestJiraWebhookDeadmanAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/jira-bridge/webhook-deadman-alert.yaml",
		"jiraBridge.enabled=true",
	)
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when webhookDeadman.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestJiraWebhookDeadmanAlert_EnabledRendersBothRules(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/webhook-deadman-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	// Dark leg: reads the Go-computed 0/1 gauge as a trivial ==1 (the verdict is
	// folded into the gauge, so no fragile multi-term join).
	if !strings.Contains(out, "JiraWebhookDark") {
		t.Errorf("expected JiraWebhookDark alert, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_jira_webhook_dark") ||
		!strings.Contains(out, "== 1") {
		t.Errorf("dark expr must fire on the dark gauge == 1, got:\n%s", out)
	}
	// Monitor-deadman leg: catches the monitor not evaluating at all — must use
	// absent() so a never-scraped / crash-before-first-scrape monitor still pages.
	if !strings.Contains(out, "JiraWebhookMonitorDeadman") {
		t.Errorf("expected JiraWebhookMonitorDeadman alert, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_jira_webhook_evaluations_total") ||
		!strings.Contains(out, "absent(gardenio_jira_webhook_evaluations_total)") {
		t.Errorf("deadman expr must guard the evaluations_total series with absent(), got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"page\"") {
		t.Errorf("default severity must page, got:\n%s", out)
	}
	// Both alerts must name the bug runbook bead under global.docsBaseURL.
	if strings.Count(out, "https://github.com/mattbakerconsulting/gardenio/blob/main/docs/user-guide/01-overview.md#beads (archived bead ka-0000000426)") < 2 {
		t.Errorf("both alerts must name archived runbook bead ka-0000000426 under docsBaseURL, got:\n%s", out)
	}
}

// THE CORE GUARD (bead deliverable #4): the rule must not be suppressible by an
// absent series. The verdict is folded into a single set-per-cycle gauge, so the
// dark expr is a trivial ==1 with NO `and`-joined second term whose absence
// could silently drop the page (the trigger-starved backlogQuery footgun). And
// the deadman leg's second failure shape is covered by absent(), not by a bare
// increase() that a never-written series would leave un-fireable.
func TestJiraWebhookDeadmanAlert_NotSuppressibleByAbsentSeries(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/webhook-deadman-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	)
	// The dark expr must be a self-contained gauge==1 — it must NOT `and`-join a
	// second series (e.g. the JQL-count collector's gardenio_jira_issues) whose
	// absence on envs without JIRA_METRICS_QUERIES would silently suppress the
	// page. The Go monitor already folds the upstream-activity confirmation into
	// the gauge, so the rule never joins.
	if strings.Contains(out, "gardenio_jira_issues") {
		t.Errorf("dark expr must not join a collector series that could be absent, got:\n%s", out)
	}
	// Isolate the dark alert's expr block and assert it carries no `and` join.
	darkExpr := extractAlertExpr(out, "JiraWebhookDark")
	if darkExpr == "" {
		t.Fatalf("could not isolate JiraWebhookDark expr from:\n%s", out)
	}
	if strings.Contains(darkExpr, " and ") || strings.Contains(darkExpr, "\nand") {
		t.Errorf("dark expr must be a folded gauge==1 with no `and`-joined term an absent series could suppress, got expr:\n%s", darkExpr)
	}
	// The deadman leg must fire on an absent heartbeat series, not merely on
	// increase()==0 (which a never-written series evaluates to no-data, not 0).
	deadmanExpr := extractAlertExpr(out, "JiraWebhookMonitorDeadman")
	if !strings.Contains(deadmanExpr, "absent(gardenio_jira_webhook_evaluations_total)") {
		t.Errorf("deadman expr must OR in absent() so a never-scraped monitor still pages, got expr:\n%s", deadmanExpr)
	}
}

func TestJiraWebhookDeadmanAlert_Configurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/webhook-deadman-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.window=45m",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.for=20m",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.deadmanWindow=30m",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.severity=ticket",
	)
	if !strings.Contains(out, "gardenio_jira_webhook_dark[45m]") {
		t.Errorf("window must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "for: \"20m\"") {
		t.Errorf("for-hold must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_jira_webhook_evaluations_total[30m]") {
		t.Errorf("deadman window must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"ticket\"") {
		t.Errorf("severity must be configurable via values, got:\n%s", out)
	}
}

// extractAlertExpr returns the text of the `expr: |` block for the named alert,
// up to the next `for:` key at the rule's indentation. Good enough to assert on
// a single alert's expression in a multi-alert rendered rule.
func extractAlertExpr(rendered, alertName string) string {
	marker := "- alert: " + alertName
	i := strings.Index(rendered, marker)
	if i < 0 {
		return ""
	}
	rest := rendered[i:]
	_, afterExpr, ok := strings.Cut(rest, "expr:")
	if !ok {
		return ""
	}
	beforeFor, _, ok := strings.Cut(afterExpr, "for:")
	if !ok {
		return afterExpr
	}
	return beforeFor
}
