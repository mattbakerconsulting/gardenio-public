package tests

import (
	"strings"
	"testing"
)

// Inbound-webhook liveness dashboard (GAP-E of the non-AVA webhook carve-out,
// ka-0000005632 / ka-0000000590).
//
// Inbound Jira/GitLab bridge webhooks are a named, permanent NON-AVA carve-out
// (operator decision, Matt 2026-06-03). The carve-out is accepted ONLY because
// it is compensated by structured logs AND receive-path metrics fronted on
// Grafana. Both bridges emit gardenio_{jira,gitlab}_webhook_deliveries_total plus
// a dark-hook deadman suite, but until this board those metrics were charted in
// zero dashboards — so the "metrics fronted via Grafana" claim was not yet
// literally true. This board makes it true, and these tests guard that the board
// keeps charting BOTH doors' deliveries-by-outcome AND the dark verdict.
//
// The dashboards/*.json files are the source of truth, baked into the
// grafana-bridge image (/dashboards) and backfilled into type=dashboard beads at
// runtime (see grafana_fleet_lifecycle_dashboard_test.go for the mechanism).

func TestWebhookLivenessDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, "gardenio-webhook-liveness.json")
	if d.UID != "gardenio-webhook-liveness" {
		t.Errorf("webhook-liveness board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("webhook-liveness board must have panels")
	}
}

func TestWebhookLivenessDashboard_ChartsBothDoorsDeliveriesAndDark(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, "gardenio-webhook-liveness.json"))

	// Both bridge doors' receive-path deliveries counter must be charted — this
	// is the whole point of the carve-out's compensating metric control. The
	// GitLab counter in particular closed the deliveries-parity gap (before
	// ka-0000005632 the GitLab door emitted no receive-path metric at all).
	wantMetric := []string{
		"gardenio_jira_webhook_deliveries_total",
		"gardenio_gitlab_webhook_deliveries_total",
		"gardenio_jira_webhook_dark",
		"gardenio_gitlab_webhook_dark",
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("webhook-liveness board must query %s somewhere; not found", m)
		}
	}

	// The dark gauges are registered on every bridge binary but only each hook's
	// home monitor updates its own; an unfiltered query would fold in the other
	// bridges' always-zero series and could mask a real dark verdict at max().
	// Guard that both dark queries are pinned to their owning component.
	//
	// WHICH `component` LABEL, AND FROM WHICH VICTORIAMETRICS (bug gd-tfR3nEQkZ6).
	// This board's targets all name datasource uid "victoriametrics", which the
	// chart provisions at templates/grafana/configmap.yaml -> the chart's OWN
	// vmsingle (`gardenio-vmsingle` in gardenio-system). That vmsingle is fed by
	// the chart's OWN vmagent, whose kubernetes-pods job relabels each pod's
	// `app.kubernetes.io/component` label into a `component` series label
	// (templates/vmagent/configmap.yaml). So on THIS datasource the dark series
	// are `{component="jira-bridge", job="kubernetes-pods", ...}` and the pins
	// below resolve — verified live 2026-09-02 through Grafana's own datasource
	// proxy: both exprs returned 0 (hooks alive), not empty.
	//
	// The cluster also runs an UNRELATED operator-managed VictoriaMetrics
	// (`vmsingle-vm-single` in the monitoring namespace) which scrapes the same
	// pods via VMServiceScrape and emits a DIFFERENT label set —
	// `{job="gardenio-jira-bridge", container="jira-bridge"}`, no `component` at
	// all. gd-tfR3nEQkZ6 was filed against that instance and concluded these
	// panels were dark; they are not. Do not "fix" these selectors to `job=` —
	// on the datasource the board actually reads, `job="gardenio-jira-bridge"`
	// matches the empty set and would take two working panels dark for real.
	//
	// TestDashboardComponentSelectorsNameRealChartComponents
	// (grafana_component_selector_guard_test.go) makes this checkable rather
	// than merely asserted: it cross-checks every `component=` selector on every
	// board against the `app.kubernetes.io/component` values the chart emits,
	// so a selector that can only match the empty set reds the build.
	for _, want := range []string{
		`gardenio_jira_webhook_dark{component="jira-bridge"}`,
		`gardenio_gitlab_webhook_dark{component="gitlab-bridge"}`,
	} {
		if !strings.Contains(exprs, want) {
			t.Errorf("webhook-liveness dark panels must pin the gauge to its owning bridge component; missing %q", want)
		}
	}
}
