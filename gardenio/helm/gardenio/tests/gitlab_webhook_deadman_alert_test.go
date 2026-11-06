package tests

import (
	"strings"
	"testing"
)

// GitLab inbound-webhook dark-hook deadman prevention alert (GAP-C of the non-AVA
// webhook carve-out ka-0000005632; runbook bug ka-0000000426). The GitLab twin of
// jira_webhook_deadman_alert_test.go: the gitlab-bridge publishes
// gardenio_gitlab_webhook_dark (0/1) and gardenio_gitlab_webhook_evaluations_total
// {result}; this rule turns them into a dark page + a monitor-deadman page, both
// with a runbook link to the carve-out task. Off by default; per-env overlays opt
// in.
//
// WHY THIS FILE EXISTS AT ALL (the whole point of the backfill, bead
// ka-0000005729). The template templates/gitlab-bridge/webhook-deadman-alert.yaml
// shipped with ZERO render coverage. The one test that LOOKS like it covers it —
// jira_webhook_deadman_alert_test.go — keys EVERY --set on
// jiraBridge.metrics.prometheusRule.webhookDeadman.*, while the GitLab template
// keys off gitlabBridge.*. So the gitlab template could be deleted outright and
// that jira test stays green — the classic silently-non-rendering-therefore-
// unfireable alert. TestGitLabWebhookDeadmanAlert_KeyedOffGitlabBridgeNotJira
// below encodes that name-match trap as a live regression guard, and the shell
// module's mutation test proves the divergence directly. NOTE: extractAlertExpr
// is shared with the jira sibling in this same package — do NOT redefine it here.

func TestGitLabWebhookDeadmanAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/gitlab-bridge/webhook-deadman-alert.yaml",
		"gitlabBridge.enabled=true",
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

func TestGitLabWebhookDeadmanAlert_EnabledRendersBothRules(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gitlab-bridge/webhook-deadman-alert.yaml",
		"gitlabBridge.enabled=true",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	// Dark leg: reads the Go-computed 0/1 gauge as a trivial ==1 (the verdict is
	// folded into the gauge, so no fragile multi-term join).
	if !strings.Contains(out, "GitLabWebhookDark") {
		t.Errorf("expected GitLabWebhookDark alert, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_gitlab_webhook_dark") ||
		!strings.Contains(out, "== 1") {
		t.Errorf("dark expr must fire on the dark gauge == 1, got:\n%s", out)
	}
	// Monitor-deadman leg: catches the monitor not evaluating at all — must use
	// absent() so a never-scraped / crash-before-first-scrape monitor still pages.
	if !strings.Contains(out, "GitLabWebhookMonitorDeadman") {
		t.Errorf("expected GitLabWebhookMonitorDeadman alert, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_gitlab_webhook_evaluations_total") ||
		!strings.Contains(out, "absent(gardenio_gitlab_webhook_evaluations_total)") {
		t.Errorf("deadman expr must guard the evaluations_total series with absent(), got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"page\"") {
		t.Errorf("default severity must page, got:\n%s", out)
	}
	// Both alerts must name the carve-out runbook bead under global.docsBaseURL
	// (the GitLab door points at the carve-out task, not the Jira bug).
	if strings.Count(out, "https://github.com/mattbakerconsulting/gardenio/blob/main/docs/user-guide/01-overview.md#beads (archived bead ka-0000005632)") < 2 {
		t.Errorf("both alerts must name archived runbook bead ka-0000005632 under docsBaseURL, got:\n%s", out)
	}
}

// THE CORE GUARD (mirrors the jira sibling): the rule must not be suppressible by
// an absent series. The verdict is folded into a single set-per-cycle gauge, so
// the dark expr is a trivial ==1 with NO `and`-joined second term whose absence
// could silently drop the page. And the deadman leg's second failure shape is
// covered by absent(), not by a bare increase() that a never-written series would
// leave un-fireable.
func TestGitLabWebhookDeadmanAlert_NotSuppressibleByAbsentSeries(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gitlab-bridge/webhook-deadman-alert.yaml",
		"gitlabBridge.enabled=true",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	)
	// The dark expr must be a self-contained gauge==1 — it must NOT `and`-join the
	// upstream-matches context series (gardenio_gitlab_webhook_upstream_matches),
	// whose absence on envs without a group scope would silently suppress the page.
	// The Go monitor already folds the upstream-activity confirmation into the
	// gauge, so the rule never joins.
	if strings.Contains(out, "max_over_time(gardenio_gitlab_webhook_dark") &&
		strings.Contains(out, "gardenio_gitlab_webhook_upstream_matches[") {
		t.Errorf("dark expr must not join the upstream_matches collector series that could be absent, got:\n%s", out)
	}
	// Isolate the dark alert's expr block and assert it carries no `and` join.
	darkExpr := extractAlertExpr(out, "GitLabWebhookDark")
	if darkExpr == "" {
		t.Fatalf("could not isolate GitLabWebhookDark expr from:\n%s", out)
	}
	if strings.Contains(darkExpr, " and ") || strings.Contains(darkExpr, "\nand") {
		t.Errorf("dark expr must be a folded gauge==1 with no `and`-joined term an absent series could suppress, got expr:\n%s", darkExpr)
	}
	// The deadman leg must fire on an absent heartbeat series, not merely on
	// increase()==0 (which a never-written series evaluates to no-data, not 0).
	deadmanExpr := extractAlertExpr(out, "GitLabWebhookMonitorDeadman")
	if !strings.Contains(deadmanExpr, "absent(gardenio_gitlab_webhook_evaluations_total)") {
		t.Errorf("deadman expr must OR in absent() so a never-scraped monitor still pages, got expr:\n%s", deadmanExpr)
	}
}

func TestGitLabWebhookDeadmanAlert_Configurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gitlab-bridge/webhook-deadman-alert.yaml",
		"gitlabBridge.enabled=true",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.window=45m",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.for=20m",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.deadmanWindow=30m",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.deadmanFor=25m",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.severity=ticket",
	)
	if !strings.Contains(out, "gardenio_gitlab_webhook_dark[45m]") {
		t.Errorf("window must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "for: \"20m\"") {
		t.Errorf("dark for-hold must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_gitlab_webhook_evaluations_total[30m]") {
		t.Errorf("deadman window must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "for: \"25m\"") {
		t.Errorf("deadman for-hold must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"ticket\"") {
		t.Errorf("severity must be configurable via values, got:\n%s", out)
	}
}

// THE NAME-MATCH TRAP, encoded as a live regression guard (bead ka-0000005729).
// This is the failure that let the template ship uncovered: a reader assumes the
// jira webhookDeadman test covers the gitlab rule because the config path LOOKS
// identical. It is not — the two templates key off DIFFERENT top-level values
// blocks. This test pins that divergence in BOTH directions so a future refactor
// that accidentally cross-wires the keys (or deletes one template) reddens:
//
//   - Enabling ONLY the jiraBridge key must NOT render the gitlab rule.
//   - Enabling ONLY the gitlabBridge key must NOT render the jira rule.
//
// Together with TestGitLabWebhookDeadmanAlert_DefaultDisabled (gitlab key absent
// ⇒ nothing renders) this makes the gitlab template un-deletable-while-green: the
// mutation test in the shell module deletes the template and shows THIS file goes
// red while the jira file stays green.
func TestGitLabWebhookDeadmanAlert_KeyedOffGitlabBridgeNotJira(t *testing.T) {
	t.Parallel()
	// Enabling ONLY the jira key must not conjure the gitlab rule.
	jiraOnly, err := helmTemplateAllowFail(t, "templates/gitlab-bridge/webhook-deadman-alert.yaml",
		"gitlabBridge.enabled=true",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	)
	if err == nil && strings.Contains(jiraOnly, "kind: PrometheusRule") {
		t.Errorf("gitlab rule must NOT render off the jiraBridge webhookDeadman key — that name-match is the whole gap, got:\n%s", jiraOnly)
	}

	// Enabling ONLY the gitlab key must not conjure the jira rule.
	gitlabOnly, err := helmTemplateAllowFail(t, "templates/jira-bridge/webhook-deadman-alert.yaml",
		"jiraBridge.enabled=true",
		"gitlabBridge.enabled=true",
		"gitlabBridge.metrics.prometheusRule.webhookDeadman.enabled=true",
	)
	if err == nil && strings.Contains(gitlabOnly, "kind: PrometheusRule") {
		t.Errorf("jira rule must NOT render off the gitlabBridge webhookDeadman key, got:\n%s", gitlabOnly)
	}
}
