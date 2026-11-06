package bridge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// TestRateLimitRecovery_OverMetricsEndpoint drives one query through
// throttle -> recovery and scrapes the real promhttp handler over the DEFAULT
// gatherer — the same gatherer jira-bridge serves on :9093 — asserting the
// rate-limit onset and recovery series appear in the exposition body exactly as
// an operator or an alert rule would see them. This is the end-to-end guard for
// the rate-limit recovery monitoring added in ka-0000002481: the unit tests
// prove the collector sets the series, this proves they serialize through
// /metrics.
func TestRateLimitRecovery_OverMetricsEndpoint(t *testing.T) {
	jiraRateLimitedGauge.Reset()
	jiraRateLimitEventsTotal.Reset()
	jiraRateLimitRecoveriesTotal.Reset()
	jiraRateLimitBackoffSeconds.Reset()

	srv := httptest.NewServer(promhttp.HandlerFor(prometheus.DefaultGatherer, promhttp.HandlerOpts{}))
	defer srv.Close()
	scrapeMetrics := func() string {
		resp, err := http.Get(srv.URL)
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read /metrics: %v", err)
		}
		return string(b)
	}

	fake := newFakeCounter()
	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "open_bugs", JQL: "q", Project: "PE"}},
	})
	q := MetricQuery{Name: "open_bugs", JQL: "q", Project: "PE"}

	// t0: throttled by a 429 carrying Retry-After 30s.
	fake.errs["q"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 30 * time.Second}
	c.scrape(context.Background(), q)

	onset := scrapeMetrics()
	for _, want := range []string{
		`gardenio_jira_rate_limited{query="open_bugs"} 1`,
		`gardenio_jira_rate_limit_events_total{query="open_bugs"} 1`,
		`gardenio_jira_rate_limit_backoff_seconds{query="open_bugs"} 30`,
	} {
		if !strings.Contains(onset, want) {
			t.Errorf("onset /metrics missing %q\n---\n%s", want, rateLimitLines(onset))
		}
	}

	// t1: cooldown expired — a clean scrape resumes throughput and fires the
	// recovery edge (rate_limited 1->0, recoveries_total +1).
	fake.mu.Lock()
	delete(fake.errs, "q")
	fake.counts["q"] = 561
	fake.mu.Unlock()
	c.scrape(context.Background(), q)

	recovered := scrapeMetrics()
	for _, want := range []string{
		`gardenio_jira_rate_limited{query="open_bugs"} 0`,
		`gardenio_jira_rate_limit_recoveries_total{query="open_bugs"} 1`,
	} {
		if !strings.Contains(recovered, want) {
			t.Errorf("recovered /metrics missing %q\n---\n%s", want, rateLimitLines(recovered))
		}
	}
}

// rateLimitLines extracts the gardenio_jira_rate_limit* sample lines from an
// exposition body for readable failure output.
func rateLimitLines(body string) string {
	var b strings.Builder
	for ln := range strings.SplitSeq(body, "\n") {
		if strings.Contains(ln, "gardenio_jira_rate_limit") && !strings.HasPrefix(ln, "#") {
			b.WriteString(ln)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
