package tests

import (
	"strings"
	"testing"
)

// Ingestion-poller stall prevention alert (epic ka-0000005107 deliverable #4,
// task ka-0000001629). The poller publishes gardenio_jira_poller_stalled (0/1)
// and gardenio_jira_poller_polls_total{result} from the jira-bridge; this rule
// turns them into a stall page + a deadman page, both with a runbook link to
// the epic. Off by default; per-env overlays opt in.

func TestJiraPollerStallAlert_DefaultDisabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/jira-bridge/poller-stall-alert.yaml",
		"jiraBridge.enabled=true",
	)
	if err == nil {
		if strings.Contains(out, "kind: PrometheusRule") {
			t.Errorf("PrometheusRule must NOT render when pollerStall.enabled=false, got:\n%s", out)
		}
		return
	}
	if !strings.Contains(err.Error(), "could not find template") &&
		!strings.Contains(out, "could not find template") {
		t.Errorf("unexpected helm failure: %v\n%s", err, out)
	}
}

func TestJiraPollerStallAlert_EnabledRendersBothRules(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/poller-stall-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.pollerStall.enabled=true",
	)
	if !strings.Contains(out, "kind: PrometheusRule") {
		t.Fatalf("PrometheusRule must render when enabled=true, got:\n%s", out)
	}
	// Stall leg: reads the Go-computed 0/1 gauge as a trivial ==1 (the verdict
	// is folded into the gauge, so no fragile multi-term join).
	if !strings.Contains(out, "JiraPollerStalled") {
		t.Errorf("expected JiraPollerStalled alert, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_jira_poller_stalled") ||
		!strings.Contains(out, "== 1") {
		t.Errorf("stall expr must fire on the stalled gauge == 1, got:\n%s", out)
	}
	// Deadman leg: catches the loop not ticking at all — must use absent() so a
	// never-scraped / crash-before-first-scrape poller still pages.
	if !strings.Contains(out, "JiraPollerDeadman") {
		t.Errorf("expected JiraPollerDeadman alert, got:\n%s", out)
	}
	if !strings.Contains(out, "gardenio_jira_poller_polls_total") ||
		!strings.Contains(out, "absent(gardenio_jira_poller_polls_total)") {
		t.Errorf("deadman expr must guard the polls_total series with absent(), got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"page\"") {
		t.Errorf("default severity must page, got:\n%s", out)
	}
	// Both alerts must name the epic runbook bead under global.docsBaseURL.
	if strings.Count(out, "https://github.com/mattbakerconsulting/gardenio/blob/main/docs/user-guide/01-overview.md#beads (archived bead ka-0000005107)") < 2 {
		t.Errorf("both alerts must name archived runbook bead ka-0000005107 under docsBaseURL, got:\n%s", out)
	}
}

func TestJiraPollerStallAlert_ThresholdConfigurable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/jira-bridge/poller-stall-alert.yaml",
		"jiraBridge.enabled=true",
		"jiraBridge.metrics.prometheusRule.pollerStall.enabled=true",
		"jiraBridge.metrics.prometheusRule.pollerStall.window=45m",
		"jiraBridge.metrics.prometheusRule.pollerStall.for=20m",
		"jiraBridge.metrics.prometheusRule.pollerStall.severity=ticket",
	)
	if !strings.Contains(out, "gardenio_jira_poller_stalled[45m]") {
		t.Errorf("window must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "for: \"20m\"") {
		t.Errorf("for-hold must be configurable via values, got:\n%s", out)
	}
	if !strings.Contains(out, "severity: \"ticket\"") {
		t.Errorf("severity must be configurable via values, got:\n%s", out)
	}
}
