package tests

import (
	"encoding/json"
	"strings"
	"testing"
)

// Jira-metrics dashboard — SLA age & burn-down guards (task ka-0000000282).
//
// The jira-metrics board's count panels are grounded in the approximate-count
// gauge gardenio_jira_issues{query,project}. Those cannot answer "how OLD are the
// open issues" — only "how many". This bead added a per-issue age export from
// the jira-bridge (gardenio_jira_issue_age_seconds snapshot histogram +
// gardenio_jira_issue_oldest_age_seconds gauge) and a "SLA age & burn-down" row
// that consumes it. These guards keep that row honest: it must actually query
// the age series, and the SLA-breach panels must use the _count − _bucket{le=…}
// shape (not a count gauge dressed up as an age), so the panels stay grounded in
// the per-issue data the exporter publishes.
func jiraMetricsDashboard(t *testing.T) dashboard {
	t.Helper()
	raw := readDashboardJSON(t, "jira-metrics.json")
	var d dashboard
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("jira-metrics.json is not valid JSON: %v", err)
	}
	return d
}

func TestJiraMetricsDashboard_RegisteredAndValidJSON(t *testing.T) {
	t.Parallel()
	d := jiraMetricsDashboard(t)
	if d.UID != "jira-metrics" {
		t.Errorf("jira-metrics board must carry its stable uid %q; got %q", "jira-metrics", d.UID)
	}
	if len(d.Panels) == 0 {
		t.Fatal("jira-metrics board must have panels")
	}
}

// The SLA-age row must query BOTH new age series — the snapshot histogram and
// the oldest-age gauge — or the age export it depends on is dead weight.
func TestJiraMetricsDashboard_QueriesAgeSeries(t *testing.T) {
	t.Parallel()
	d := jiraMetricsDashboard(t)
	exprs := allExprs(d)
	for _, want := range []string{
		"gardenio_jira_issue_age_seconds",
		"gardenio_jira_issue_oldest_age_seconds",
	} {
		if !strings.Contains(exprs, want) {
			t.Errorf("jira-metrics board must query %q (the per-issue age export); not found in any panel target", want)
		}
	}
}

// The SLA-breach counts must be derived from the histogram as
// _count − _bucket{le=…}, and age percentiles via histogram_quantile over the
// buckets — the grounded shapes. A count gauge (gardenio_jira_issues) can never
// answer an age question, so assert the age panels lean on the histogram, not a
// count series masquerading as an age.
func TestJiraMetricsDashboard_SLAAgeUsesHistogramShapes(t *testing.T) {
	t.Parallel()
	d := jiraMetricsDashboard(t)
	exprs := allExprs(d)
	if !strings.Contains(exprs, "histogram_quantile(") {
		t.Error("expected an age-percentile panel using histogram_quantile over gardenio_jira_issue_age_seconds buckets")
	}
	if !strings.Contains(exprs, "gardenio_jira_issue_age_seconds_bucket") ||
		!strings.Contains(exprs, "gardenio_jira_issue_age_seconds_count") {
		t.Error("SLA-breach panels must use _count − _bucket{le=…} over the age histogram (grounded breach count)")
	}
}

// Every age panel target must still bind the Project template variable, so the
// SLA-age row slices by project exactly like the count panels above it.
func TestJiraMetricsDashboard_AgePanelsBindProjectVar(t *testing.T) {
	t.Parallel()
	d := jiraMetricsDashboard(t)
	for _, p := range d.Panels {
		for _, tgt := range p.Targets {
			if !strings.Contains(tgt.Expr, "gardenio_jira_issue_age") &&
				!strings.Contains(tgt.Expr, "gardenio_jira_issue_oldest_age") {
				continue
			}
			if !strings.Contains(tgt.Expr, "project=~\"$project\"") {
				t.Errorf("age panel %q target does not bind the Project var ($project): %q", p.Title, tgt.Expr)
			}
		}
	}
}
