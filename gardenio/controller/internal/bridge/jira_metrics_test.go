package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestApproximateCount_Parse confirms ApproximateCount POSTs the JQL to the
// approximate-count endpoint and decodes the {"count": <int>} body Phase 0
// confirmed live (gd-qaGG2JLq0r).
func TestApproximateCount_Parse(t *testing.T) {
	var gotMethod, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":503}`)
	}))
	defer srv.Close()

	client := NewJiraClient(JiraClientConfig{BaseURL: srv.URL, Email: "e@x", APIToken: "tok"})
	got, err := client.ApproximateCount(context.Background(), "issuetype = Bug AND statusCategory != Done")
	if err != nil {
		t.Fatalf("ApproximateCount: %v", err)
	}
	if got != 503 {
		t.Errorf("count = %d, want 503", got)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/rest/api/3/search/approximate-count" {
		t.Errorf("path = %q, want /rest/api/3/search/approximate-count", gotPath)
	}
	if !strings.Contains(gotBody, `"jql"`) || !strings.Contains(gotBody, "statusCategory") {
		t.Errorf("body = %q, want JQL payload", gotBody)
	}
}

// TestApproximateCount_ZeroCount confirms a 200 with count 0 is returned as a
// clean 0 (Phase 0 noted unknown field/enum names silently return count 0 / 200
// — not an error, so the collector still publishes a 0 gauge for them).
func TestApproximateCount_ZeroCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"count":0}`)
	}))
	defer srv.Close()

	client := NewJiraClient(JiraClientConfig{BaseURL: srv.URL})
	got, err := client.ApproximateCount(context.Background(), "issuetype = ZZZNonsense")
	if err != nil {
		t.Fatalf("ApproximateCount: %v", err)
	}
	if got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
}

// TestApproximateCount_SyntaxError400 confirms a JQL syntax error (HTTP 400) is
// surfaced as a *JiraAPIError carrying the status code, and that Error()
// preserves the legacy message format.
func TestApproximateCount_SyntaxError400(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"errorMessages":["Expecting either a value, list or function but got ..."]}`)
	}))
	defer srv.Close()

	client := NewJiraClient(JiraClientConfig{BaseURL: srv.URL})
	_, err := client.ApproximateCount(context.Background(), "issuetype = ")
	if err == nil {
		t.Fatal("expected error for syntax-error JQL")
	}
	apiErr, ok := asJiraAPIError(err)
	if !ok {
		t.Fatalf("error not a *JiraAPIError: %v", err)
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", apiErr.StatusCode)
	}
	if !strings.Contains(err.Error(), "returned 400") {
		t.Errorf("legacy error format not preserved: %q", err.Error())
	}
}

// TestParseRetryAfter checks the delta-seconds parsing the 429 backoff relies on.
func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"30":          30 * time.Second,
		"  45 ":       45 * time.Second,
		"0":           0,
		"-5":          0,
		"":            0,
		"Wed, 21 Oct": 0, // HTTP-date form not honored
		"notanumber":  0,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestJiraAPIError_RetryAfterParsed confirms doJSON populates RetryAfter from
// the response header on a 429.
func TestJiraAPIError_RetryAfterParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"errorMessages":["rate limited"]}`)
	}))
	defer srv.Close()

	client := NewJiraClient(JiraClientConfig{BaseURL: srv.URL})
	_, err := client.ApproximateCount(context.Background(), "statusCategory != Done")
	apiErr, ok := asJiraAPIError(err)
	if !ok {
		t.Fatalf("error not a *JiraAPIError: %v", err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", apiErr.StatusCode)
	}
	if apiErr.RetryAfter != 17*time.Second {
		t.Errorf("RetryAfter = %v, want 17s", apiErr.RetryAfter)
	}
}

// fakeCounter is an injectable jiraCounter for collector tests. Each query name
// maps to a fixed result or error; a counter records how many times it ran.
type fakeCounter struct {
	mu     sync.Mutex
	counts map[string]int64
	errs   map[string]error
	calls  map[string]int
}

func newFakeCounter() *fakeCounter {
	return &fakeCounter{
		counts: map[string]int64{},
		errs:   map[string]error{},
		calls:  map[string]int{},
	}
}

func (f *fakeCounter) ApproximateCount(_ context.Context, jql string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[jql]++
	if err := f.errs[jql]; err != nil {
		return 0, err
	}
	return f.counts[jql], nil
}

func (f *fakeCounter) callCount(jql string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[jql]
}

// TestCollector_PublishesExpectedSeries runs one collection cycle and asserts
// the gardenio_jira_issues and gardenio_jira_scrape_* families carry the expected
// series, including the project=all default for an unscoped query.
func TestCollector_PublishesExpectedSeries(t *testing.T) {
	jiraIssuesGauge.Reset()
	jiraScrapeDurationSeconds.Reset()
	jiraScrapeErrorsTotal.Reset()

	fake := newFakeCounter()
	fake.counts["issuetype = Bug AND statusCategory != Done"] = 498
	fake.counts["project = PE AND statusCategory != Done"] = 4141

	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira: fake,
		Queries: []MetricQuery{
			{Name: "open_bugs", JQL: "issuetype = Bug AND statusCategory != Done"},
			{Name: "by_project", JQL: "project = PE AND statusCategory != Done", Project: "PE"},
		},
	})

	// collectDue runs every due query exactly once on the first pass.
	c.collectDue(context.Background())

	if got := testutil.ToFloat64(jiraIssuesGauge.WithLabelValues("open_bugs", "all")); got != 498 {
		t.Errorf("gardenio_jira_issues{open_bugs,all} = %v, want 498", got)
	}
	if got := testutil.ToFloat64(jiraIssuesGauge.WithLabelValues("by_project", "PE")); got != 4141 {
		t.Errorf("gardenio_jira_issues{by_project,PE} = %v, want 4141", got)
	}

	// Both queries published a duration series; neither incremented errors.
	if n := testutil.CollectAndCount(jiraScrapeDurationSeconds); n != 2 {
		t.Errorf("scrape_duration series count = %d, want 2", n)
	}
	if got := testutil.ToFloat64(jiraScrapeErrorsTotal.WithLabelValues("open_bugs")); got != 0 {
		t.Errorf("scrape_errors{open_bugs} = %v, want 0", got)
	}
}

// TestCollector_ErrorIncrementsCounter confirms a failing query bumps
// gardenio_jira_scrape_errors_total and leaves the issues gauge unset.
func TestCollector_ErrorIncrementsCounter(t *testing.T) {
	jiraIssuesGauge.Reset()
	jiraScrapeErrorsTotal.Reset()

	fake := newFakeCounter()
	fake.errs["bad jql"] = errors.New("boom")

	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "broken", JQL: "bad jql"}},
	})
	c.collectDue(context.Background())

	if got := testutil.ToFloat64(jiraScrapeErrorsTotal.WithLabelValues("broken")); got != 1 {
		t.Errorf("scrape_errors{broken} = %v, want 1", got)
	}
	if n := testutil.CollectAndCount(jiraIssuesGauge); n != 0 {
		t.Errorf("issues gauge should have no series on error, got %d", n)
	}
}

// TestCollector_429Backoff confirms a 429 with Retry-After is reported as a
// backoff from scrape(), and that the error path still increments the error
// counter.
func TestCollector_429Backoff(t *testing.T) {
	jiraScrapeErrorsTotal.Reset()

	fake := newFakeCounter()
	fake.errs["rate limited jql"] = &JiraAPIError{
		Method:     http.MethodPost,
		Path:       "/rest/api/3/search/approximate-count",
		StatusCode: http.StatusTooManyRequests,
		RetryAfter: 25 * time.Second,
	}

	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "rl", JQL: "rate limited jql"}},
	})

	backoff := c.scrape(context.Background(), MetricQuery{Name: "rl", JQL: "rate limited jql"})
	if backoff != 25*time.Second {
		t.Errorf("backoff = %v, want 25s (from Retry-After)", backoff)
	}
	if got := testutil.ToFloat64(jiraScrapeErrorsTotal.WithLabelValues("rl")); got != 1 {
		t.Errorf("scrape_errors{rl} = %v, want 1", got)
	}
}

// TestCollector_429BackoffDefault confirms a 429 with no Retry-After falls back
// to the default backoff.
func TestCollector_429BackoffDefault(t *testing.T) {
	fake := newFakeCounter()
	fake.errs["rl jql"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests}

	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "rl", JQL: "rl jql"}},
	})
	if backoff := c.scrape(context.Background(), MetricQuery{Name: "rl", JQL: "rl jql"}); backoff != defaultRateLimitBackoff {
		t.Errorf("backoff = %v, want default %v", backoff, defaultRateLimitBackoff)
	}
}

// TestCollector_RateLimitOnsetMetrics confirms a 429 raises the rate_limited
// gauge to 1, counts the onset event, and records the backoff seconds — the
// onset half of the recovery-monitoring surface (task ka-0000002481).
func TestCollector_RateLimitOnsetMetrics(t *testing.T) {
	jiraRateLimitedGauge.Reset()
	jiraRateLimitEventsTotal.Reset()
	jiraRateLimitBackoffSeconds.Reset()
	jiraRateLimitRecoveriesTotal.Reset()

	fake := newFakeCounter()
	fake.errs["rl jql"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 25 * time.Second}

	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "rl", JQL: "rl jql"}},
	})
	c.scrape(context.Background(), MetricQuery{Name: "rl", JQL: "rl jql"})

	if got := testutil.ToFloat64(jiraRateLimitedGauge.WithLabelValues("rl")); got != 1 {
		t.Errorf("rate_limited{rl} = %v, want 1 while throttled", got)
	}
	if got := testutil.ToFloat64(jiraRateLimitEventsTotal.WithLabelValues("rl")); got != 1 {
		t.Errorf("rate_limit_events_total{rl} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(jiraRateLimitBackoffSeconds.WithLabelValues("rl")); got != 25 {
		t.Errorf("rate_limit_backoff_seconds{rl} = %v, want 25 (Retry-After)", got)
	}
	// No recovery has happened yet.
	if got := testutil.ToFloat64(jiraRateLimitRecoveriesTotal.WithLabelValues("rl")); got != 0 {
		t.Errorf("rate_limit_recoveries_total{rl} = %v, want 0 before recovery", got)
	}
}

// TestCollector_RateLimitRecoveryEdge is the core recovery-monitoring assertion:
// after a 429 backoff, the next clean scrape drops the rate_limited gauge to 0
// and increments the recoveries counter exactly once (the 1->0 edge = cooldown
// expired, throughput resumed).
func TestCollector_RateLimitRecoveryEdge(t *testing.T) {
	jiraRateLimitedGauge.Reset()
	jiraRateLimitEventsTotal.Reset()
	jiraRateLimitRecoveriesTotal.Reset()

	fake := newFakeCounter()
	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "rl", JQL: "q"}},
	})
	q := MetricQuery{Name: "rl", JQL: "q"}

	// Cycle 1: 429 — throttled.
	fake.errs["q"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 10 * time.Second}
	c.scrape(context.Background(), q)
	if got := testutil.ToFloat64(jiraRateLimitedGauge.WithLabelValues("rl")); got != 1 {
		t.Fatalf("after 429: rate_limited = %v, want 1", got)
	}

	// Cycle 2: clean — recovery edge fires.
	fake.mu.Lock()
	delete(fake.errs, "q")
	fake.counts["q"] = 42
	fake.mu.Unlock()
	c.scrape(context.Background(), q)

	if got := testutil.ToFloat64(jiraRateLimitedGauge.WithLabelValues("rl")); got != 0 {
		t.Errorf("after recovery: rate_limited = %v, want 0", got)
	}
	if got := testutil.ToFloat64(jiraRateLimitRecoveriesTotal.WithLabelValues("rl")); got != 1 {
		t.Errorf("recoveries_total = %v, want 1 (one 1->0 edge)", got)
	}
	// The count gauge published on recovery proves throughput resumed.
	if got := testutil.ToFloat64(jiraIssuesGauge.WithLabelValues("rl", "all")); got != 42 {
		t.Errorf("issues{rl,all} = %v, want 42 after recovery", got)
	}

	// Cycle 3: another clean scrape does NOT double-count the recovery.
	c.scrape(context.Background(), q)
	if got := testutil.ToFloat64(jiraRateLimitRecoveriesTotal.WithLabelValues("rl")); got != 1 {
		t.Errorf("recoveries_total after 2nd clean scrape = %v, want still 1 (edge, not level)", got)
	}
}

// TestCollector_RateLimitSustainedThenRecover confirms repeated 429s while
// already throttled keep counting onsets (a sustained throttle is louder than a
// single blip) and only a clean scrape yields exactly one recovery.
func TestCollector_RateLimitSustainedThenRecover(t *testing.T) {
	jiraRateLimitedGauge.Reset()
	jiraRateLimitEventsTotal.Reset()
	jiraRateLimitRecoveriesTotal.Reset()

	fake := newFakeCounter()
	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "rl", JQL: "q"}},
	})
	q := MetricQuery{Name: "rl", JQL: "q"}

	fake.errs["q"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests}
	c.scrape(context.Background(), q)
	c.scrape(context.Background(), q)
	c.scrape(context.Background(), q)
	if got := testutil.ToFloat64(jiraRateLimitEventsTotal.WithLabelValues("rl")); got != 3 {
		t.Errorf("events_total after 3 sustained 429s = %v, want 3", got)
	}
	if got := testutil.ToFloat64(jiraRateLimitRecoveriesTotal.WithLabelValues("rl")); got != 0 {
		t.Errorf("recoveries_total while still throttled = %v, want 0", got)
	}

	fake.mu.Lock()
	delete(fake.errs, "q")
	fake.mu.Unlock()
	c.scrape(context.Background(), q)
	if got := testutil.ToFloat64(jiraRateLimitRecoveriesTotal.WithLabelValues("rl")); got != 1 {
		t.Errorf("recoveries_total after clearing = %v, want exactly 1", got)
	}
}

// TestCollector_NonRateLimitErrorPreservesThrottleState confirms a non-429 error
// during backoff does NOT falsely clear the rate-limit state — only a genuine
// clean scrape counts as recovery, so a transient 500 mid-throttle can't
// manufacture a phantom recovery edge.
func TestCollector_NonRateLimitErrorPreservesThrottleState(t *testing.T) {
	jiraRateLimitedGauge.Reset()
	jiraRateLimitRecoveriesTotal.Reset()

	fake := newFakeCounter()
	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:    fake,
		Queries: []MetricQuery{{Name: "rl", JQL: "q"}},
	})
	q := MetricQuery{Name: "rl", JQL: "q"}

	fake.errs["q"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests}
	c.scrape(context.Background(), q) // throttled

	fake.mu.Lock()
	fake.errs["q"] = &JiraAPIError{StatusCode: http.StatusInternalServerError} // transient 500
	fake.mu.Unlock()
	c.scrape(context.Background(), q)

	if got := testutil.ToFloat64(jiraRateLimitedGauge.WithLabelValues("rl")); got != 1 {
		t.Errorf("rate_limited after mid-throttle 500 = %v, want still 1", got)
	}
	if got := testutil.ToFloat64(jiraRateLimitRecoveriesTotal.WithLabelValues("rl")); got != 0 {
		t.Errorf("recoveries_total after mid-throttle 500 = %v, want 0 (no phantom recovery)", got)
	}
}

// TestCollector_RateLimitSeriesOnDefaultRegistry proves the new rate-limit
// families are registered on the DEFAULT registry (via promauto), the same
// gatherer the :9093 /metrics handler serves.
func TestCollector_RateLimitSeriesOnDefaultRegistry(t *testing.T) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"gardenio_jira_rate_limited",
		"gardenio_jira_rate_limit_events_total",
		"gardenio_jira_rate_limit_recoveries_total",
		"gardenio_jira_rate_limit_backoff_seconds",
	} {
		if !names[want] {
			t.Errorf("metric family %q not on the default registry (would never be scraped via /metrics)", want)
		}
	}
}

// TestCollector_DueRespectsPerQueryInterval confirms a query with its own
// longer interval is not re-run before that interval elapses, while a
// default-interval query is. A controllable clock drives the due() check.
func TestCollector_DueRespectsPerQueryInterval(t *testing.T) {
	jiraIssuesGauge.Reset()

	now := time.Unix(1_000_000, 0)
	fake := newFakeCounter()
	fake.counts["a"] = 1
	fake.counts["b"] = 2

	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira:     fake,
		Interval: 120 * time.Second,
		Clock:    func() time.Time { return now },
		Queries: []MetricQuery{
			{Name: "fast", JQL: "a"},                              // default 120s
			{Name: "slow", JQL: "b", Interval: 600 * time.Second}, // 10m override
		},
	})

	c.collectDue(context.Background()) // first pass: both run
	if got := fake.callCount("a"); got != 1 {
		t.Fatalf("fast first-pass calls = %d, want 1", got)
	}
	if got := fake.callCount("b"); got != 1 {
		t.Fatalf("slow first-pass calls = %d, want 1", got)
	}

	// 130s later: fast is due (>120s), slow is not (<600s).
	now = now.Add(130 * time.Second)
	c.collectDue(context.Background())
	if got := fake.callCount("a"); got != 2 {
		t.Errorf("fast calls after 130s = %d, want 2", got)
	}
	if got := fake.callCount("b"); got != 1 {
		t.Errorf("slow calls after 130s = %d, want 1 (interval not elapsed)", got)
	}
}

// TestCollector_OnDefaultRegistry proves the gardenio_jira_* families are
// registered on the DEFAULT registry (via promauto), the same gatherer the
// :9093 /metrics handler serves — a private registry would increment but never
// be scraped.
func TestCollector_OnDefaultRegistry(t *testing.T) {
	jiraIssuesGauge.Reset()
	jiraScrapeErrorsTotal.Reset()

	fake := newFakeCounter()
	fake.counts["q"] = 7
	fake.errs["e"] = errors.New("boom")
	c := NewJiraMetricsCollector(JiraMetricsConfig{
		Jira: fake,
		Queries: []MetricQuery{
			{Name: "ok", JQL: "q"},
			{Name: "bad", JQL: "e"},
		},
	})
	c.collectDue(context.Background())

	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather default registry: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"gardenio_jira_issues",
		"gardenio_jira_scrape_duration_seconds",
		"gardenio_jira_scrape_errors_total",
	} {
		if !names[want] {
			t.Errorf("metric family %q not on the default registry (would never be scraped via /metrics)", want)
		}
	}
}
