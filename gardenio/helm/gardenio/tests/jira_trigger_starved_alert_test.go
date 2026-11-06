package tests

import (
	"strings"
	"testing"
)

// Trigger cooldown-starvation SYMPTOM alert (Alert B of epic ka-0000005107,
// task ka-0000000052). The TriggerEngine publishes
// gardenio_trigger_starved{source,trigger} from the jira-bridge; this rule
// turns it into a page with a runbook link to the epic.

func TestJiraTriggerStarvedAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/jira-bridge/trigger-starved-alert.yaml",
		"jiraBridge.enabled=true",
	)
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when triggerStarved.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestJiraTriggerStarvedAlert_EnabledRendersRule(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/trigger-starved-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.triggerStarved.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	if !strings.Contains(out, "JiraTriggerStarved") {
		t.Errorf("expected JiraTriggerStarved alert, got:\n%s", out)
	}
	if !strings.Contains(out, `gardenio_trigger_starved{source="jira"}`) {
		t.Errorf("alert expr must read the engine-side starved gauge scoped to the jira source, got:\n%s", out)
	}
	// The engine gauge already folds in the per-trigger ApproximateCount>0
	// check, so the fleet-level gardenio_jira_issues join must be OFF by
	// default — an absent collector series in an `and` would silently
	// suppress the page on envs without JIRA_METRICS_QUERIES.
	if strings.Contains(out, "gardenio_jira_issues{query=") {
		t.Errorf("backlog join must not render by default (backlogQuery empty), got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"page\"") {
		t.Errorf("default severity must page, got:\n%s", out)
	}
	// The runbook must name the epic bead under global.docsBaseURL.
	if !strings.Contains(out, "https://github.com/mattbakerconsulting/gardenio/blob/main/docs/user-guide/01-overview.md#beads (archived bead ka-0000005107)") {
		t.Errorf("runbook_url must name archived epic ka-0000005107 under docsBaseURL, got:\n%s", out)
	}
}

func TestJiraTriggerStarvedAlert_BacklogJoinOptIn(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/trigger-starved-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.triggerStarved.enabled=true",
		"jiraBridge.metrics.prometheusRule.triggerStarved.backlogQuery=backlog",
	)
	if !strings.Contains(out, `gardenio_jira_issues{query="backlog"}`) {
		t.Errorf("backlogQuery opt-in must join on the named gardenio_jira_issues series, got:\n%s", out)
	}
}
