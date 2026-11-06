package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fleet-lifecycle + cost-efficiency dashboard panels (bug ka-0000000045).
//
// Since ka-0000002799 the chart no longer renders dashboard ConfigMaps (they
// double-stored ~200KB per revision in the release-history Secret and overflowed
// the K8s 1 MiB limit, stranding prod — ka-0000003724). The dashboards/*.json
// files stay in the repo as the source of truth and are baked into the
// grafana-bridge image (/dashboards), which backfills them into type=dashboard
// beads and provisions them into live Grafana at runtime. These tests therefore
// guard the JSON CONTENT read straight from disk — the same bytes the bridge
// ships — that:
//   - the gardenio-fleet-lifecycle board carries its stable uid, and
//   - the five priority panels from the idea catalog are actually present with
//     their VM-grounded PromQL, and
//   - the cost-per-bead + operator-leaderboard panels landed on cost-overview.

// dashboardConfigMapData returns a map from the ConfigMap key the chart used to
// emit (before ka-0000002799 removed the templates) to the raw dashboard JSON,
// read directly from the dashboards/ source directory. The keys mirror exactly
// what templates/controller/grafana-dashboard-configmap.yaml used to publish so
// the content-guard tests below are unchanged.
func dashboardConfigMapData(t *testing.T) map[string]string {
	t.Helper()
	// key (former ConfigMap data key) -> file under dashboards/.
	keyToFile := map[string]string{
		"gardenio-cost-overview.json":  "cost-overview.json",
		"kbeads-embedding.json":        "kbeads-embedding.json",
		"kbeads-operated-by.json":      "operated-by.json",
		"gardenio-resume-latency.json": "resume-latency.json",
		// gitlab-dark-drift.json is PARKED (ka-0000005317 / decision ka-0000003993):
		// moved to dashboards/_parked/ so it drops out of the top-level *.json glob
		// (image bake, backfill, GRADE) until deploy-values/gitlab-dark/ is applied
		// and gitlab_dark_* is actually emitted. See dashboards/_parked/README.md.
		"gardenio-jira-metrics.json":             "jira-metrics.json",
		"gardenio-cicd-minutes.json":             "cicd-minutes.json",
		"gardenio-context-observability.json":    "context-observability.json",
		"gardenio-involuntary-deaths.json":       "involuntary-deaths.json",
		"gardenio-fleet-lifecycle.json":          "fleet-lifecycle.json",
		"gardenio-control-plane-liveness.json":   "control-plane-liveness.json",
		"gardenio-fleet-capacity.json":           "fleet-capacity.json",
		"gardenio-latency-cost-efficiency.json":  "latency-cost-efficiency.json",
		"gardenio-system-health.json":            "system-health.json",
		"gardenio-xcheck-trust.json":             "xcheck-trust.json",
		"gardenio-formula-step-dispatch.json":    "formula-step-dispatch.json",
		"gardenio-webhook-liveness.json":         "webhook-liveness.json",
		"gardenio-jira-conversation-health.json": "jira-conversation-health.json",
	}
	data := make(map[string]string, len(keyToFile))
	for key, file := range keyToFile {
		data[key] = readDashboardJSON(t, file)
	}
	return data
}

// readDashboardJSON reads one dashboard file from the dashboards/ source dir
// (the bytes baked into the grafana-bridge image at /dashboards).
func readDashboardJSON(t *testing.T, file string) string {
	t.Helper()
	path := filepath.Join(chartDir(t), "dashboards", file)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dashboard %s: %v", file, err)
	}
	return string(raw)
}

// dashboard is the minimal shape we assert against.
type dashboard struct {
	UID    string `json:"uid"`
	Title  string `json:"title"`
	Panels []struct {
		Type    string `json:"type"`
		Title   string `json:"title"`
		Targets []struct {
			Expr string `json:"expr"`
		} `json:"targets"`
	} `json:"panels"`
}

func parseDashboard(t *testing.T, data map[string]string, key string) dashboard {
	t.Helper()
	raw, ok := data[key]
	if !ok {
		t.Fatalf("dashboard ConfigMap is missing key %q; keys present: %v", key, keysOf(data))
	}
	var d dashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("embedded %s is not valid JSON (nindent likely broke it): %v", key, err)
	}
	return d
}

func keysOf(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// allExprs concatenates every panel target expr so tests can assert a metric is
// queried somewhere on the board without pinning the exact panel index.
func allExprs(d dashboard) string {
	var b strings.Builder
	for _, p := range d.Panels {
		for _, tgt := range p.Targets {
			b.WriteString(tgt.Expr)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func TestFleetLifecycleDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, "gardenio-fleet-lifecycle.json")
	if d.UID != "gardenio-fleet-lifecycle" {
		t.Errorf("fleet-lifecycle board must carry its stable uid; got %q", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("fleet-lifecycle board must have panels")
	}
}

func TestFleetLifecycleDashboard_PriorityPanelsPresent(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	exprs := allExprs(parseDashboard(t, data, "gardenio-fleet-lifecycle.json"))

	// The idea-catalog priority panels (#1 session-duration, #3 authz-denials,
	// #4 freeze p95, #5 beads-events, #16/#17 llm/tool latency), each keyed on
	// its VM-grounded metric family.
	wantMetric := []string{
		"gardenio_agent_session_duration_seconds_bucket",
		"gardenio:beads_events:rate5m",
		"gardenio_authz_denials_total",
		"gardenio_freeze_latency_seconds_bucket",
		"gardenio_agent_llm_request_latency_seconds_bucket",
		"gardenio_agent_tool_call_latency_seconds_bucket",
	}
	for _, m := range wantMetric {
		if !strings.Contains(exprs, m) {
			t.Errorf("fleet-lifecycle board must query %s somewhere; not found", m)
		}
	}
	// The session-duration panel must expose the p95 tail via histogram_quantile.
	if !strings.Contains(exprs, "histogram_quantile(0.95, sum by (le, role) (rate(gardenio_agent_session_duration_seconds_bucket") {
		t.Error("fleet-lifecycle board must compute session-duration p95 by role")
	}
}

func TestCostOverviewDashboard_CostPerBeadAndOperatorLeaderboard(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, "gardenio-cost-overview.json")
	exprs := allExprs(d)

	// #12 cost-per-bead efficiency: spend / clamp_min(closed,1).
	if !strings.Contains(exprs, "clamp_min(sum(rate(gardenio_beads_events_total{event=\"closed\"") {
		t.Error("cost-overview must add the cost-per-bead efficiency ratio (spend / beads closed)")
	}
	// Guard against a broken splice: the board must still parse and keep a uid.
	if d.UID != "gardenio-cost-overview" {
		t.Errorf("cost-overview uid must be preserved; got %q", d.UID)
	}
}

// The attribution panels must window the real-bill tag gauge, NOT the settled
// operator counter (ka-0000004856): gardenio_operator_cost_usd_total is a
// per-{pod,project,model} counter whose series churn with every controller
// deploy, so every windowed aggregation over it (increase(), and per-series
// max-min alike) over-counted ~9x versus the bill — and 100% of its spend is
// operator_id="0", so a per-operator breakdown was one anonymous bar anyway.
// gardenio_tag_cost_usd is an absolute lifetime-cumulative value republished by
// every pod, so max by (value) dedups churn and a subquery max-min windows it
// honestly.
func TestCostOverviewDashboard_AttributionPanelsUseRealBillGauge(t *testing.T) {
	t.Parallel()
	data := dashboardConfigMapData(t)
	d := parseDashboard(t, data, "gardenio-cost-overview.json")
	exprs := allExprs(d)

	if strings.Contains(exprs, "gardenio_operator_cost_usd_total") {
		t.Error("cost-overview must not derive spend from gardenio_operator_cost_usd_total (churn-exposed per-pod counter, all operator_id=\"0\"; ka-0000004856)")
	}
	for _, want := range []string{
		"max by (value) (gardenio_tag_cost_usd{dim=\"project\", value=~\"$project\"}))[$__range:5m]",
		"max by (value) (gardenio_tag_cost_usd{dim=\"model\"}))[$__range:5m]",
	} {
		if !strings.Contains(exprs, want) {
			t.Errorf("cost-overview attribution panels must window the real-bill tag gauge; missing %q", want)
		}
	}
}
