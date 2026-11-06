package tests

import (
	"strings"
	"testing"
)

// Jira conversation-loop health dashboard (design-doc F-B, epic ka-0000007401 /
// task gd-DrDw84jy8R).
//
// The jira-bridge emits a full family of conversation-loop counters
// (gardenio_jira_mention_spawn_total, _spawn_ack_total, _reply_total,
// _followup_delivery_total, _followup_wake_{retries,failures}_total, the inbound
// comment-mirror retry/failure + DLQ families, and the webhook_dark deadman) for
// the mention → spawn → ack → reply → follow-up loop that IS the "respond to
// @gardenio conversationally" feature. Until this board those counters were
// charted in ZERO dashboards — jira-metrics.json is the separate JQL-count
// TICKET-ANALYTICS exporter (backlog / SLA-age / throughput), a different
// surface. So an operator had no single "is the Jira conversation loop healthy?"
// view and had to read pod logs (which agents cannot). This board is the
// operator-legibility control; these tests guard that it keeps charting the loop
// end to end.
//
// The dashboards/*.json files are the source of truth, baked into the
// grafana-bridge image (/dashboards) and backfilled into type=dashboard beads at
// runtime (see grafana_fleet_lifecycle_dashboard_test.go for the mechanism).

func TestJiraConversationHealthDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, "gardenio-jira-conversation-health.json")
	if d.UID != "gardenio-jira-conversation-health" {
		t.Errorf("jira-conversation-health board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("jira-conversation-health board must have panels")
	}
}

func TestJiraConversationHealthDashboard_ChartsTheWholeLoop(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, "gardenio-jira-conversation-health.json"))

	// Every leg of the conversational loop must be represented, or the board
	// stops being the single "is the loop healthy?" view it exists to be. Each
	// metric name is verified to exist as a registered counter/gauge in
	// controller/internal/bridge (a wrong name is a silent-empty panel).
	wantMetric := []string{
		// front half: mention → spawn → ack
		"gardenio_jira_mention_spawn_total",
		"gardenio_jira_spawn_ack_total",
		// reply path
		"gardenio_jira_reply_total",
		// follow-up delivery (warm/cold)
		"gardenio_jira_followup_delivery_total",
		// wake durability
		"gardenio_jira_followup_wake_retries_total",
		"gardenio_jira_followup_wake_failures_total",
		// inbound comment-mirror durability + DLQ
		"gardenio_jira_inbound_comment_write_retries_total",
		"gardenio_jira_inbound_comment_write_failures_total",
		"gardenio_jira_inbound_comment_dlq_depth",
		"gardenio_jira_inbound_comment_dlq_enqueued_total",
		"gardenio_jira_inbound_comment_dlq_replayed_total",
		"gardenio_jira_inbound_comment_dlq_dropped_total",
		// inbound deadman
		"gardenio_jira_webhook_dark",
		"gardenio_jira_webhook_deliveries_total",
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("jira-conversation-health board must query %s somewhere; not found", m)
		}
	}
}

func TestJiraConversationHealthDashboard_DarkGaugePinnedToComponent(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, "gardenio-jira-conversation-health.json"))

	// The dark gauge is registered on every bridge binary but only the
	// jira-bridge monitor updates it; an unfiltered query would fold in the other
	// bridges' always-zero series and could mask a real dark verdict at max().
	// Pin it to the owning component (the scrape-time relabel from the pod's
	// app.kubernetes.io/component label), matching the webhook-liveness board.
	want := `gardenio_jira_webhook_dark{component="jira-bridge"}`
	if !strings.Contains(exprs, want) {
		t.Errorf("dark-verdict panels must pin the gauge to the jira-bridge component; missing %q", want)
	}
}

func TestJiraConversationHealthDashboard_WarmHitRatioIsClampGuarded(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, "gardenio-jira-conversation-health.json"))

	// The follow-up warm-hit ratio is warm/(warm+cold). It MUST clamp the
	// denominator so an all-quiet window renders 0/1 = 0 instead of a divide-by-
	// zero NaN gap (mirrors the Slack warm-graft hit rate in resume-latency.json).
	if !strings.Contains(exprs, `path="warm"`) ||
		!strings.Contains(exprs, `clamp_min(sum(increase(gardenio_jira_followup_delivery_total{path=~"warm|cold"}`) {
		t.Error("warm-hit ratio must be warm / clamp_min(warm+cold, 1) to stay NaN-safe on a quiet window")
	}
}
