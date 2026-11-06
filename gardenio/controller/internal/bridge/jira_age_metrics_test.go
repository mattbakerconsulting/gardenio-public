package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeAgeSearcher is an injectable jiraSearcher for age-collector tests. Each JQL
// maps to a fixed list of pages (returned in order per pageToken chaining) or an
// error; calls records how many pages were fetched per JQL.
type fakeAgeSearcher struct {
	mu    sync.Mutex
	pages map[string][]*JiraSearchPage
	errs  map[string]error
	calls map[string]int
}

func newFakeAgeSearcher() *fakeAgeSearcher {
	return &fakeAgeSearcher{
		pages: map[string][]*JiraSearchPage{},
		errs:  map[string]error{},
		calls: map[string]int{},
	}
}

func (f *fakeAgeSearcher) SearchIssuesPage(_ context.Context, jql string, _ []string, _ int, pageToken string) (*JiraSearchPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.errs[jql]; err != nil {
		return nil, err
	}
	// pageToken "" means first page (index 0); otherwise the token IS the index.
	idx := 0
	if pageToken != "" {
		fmt.Sscanf(pageToken, "%d", &idx)
	}
	pages := f.pages[jql]
	if idx >= len(pages) {
		return &JiraSearchPage{}, nil
	}
	f.calls[jql]++
	return pages[idx], nil
}

// issueCreated builds a JiraIssue carrying just a created timestamp, the only
// field the age collector reads.
func issueCreated(created string) JiraIssue {
	return JiraIssue{Fields: JiraIssueFields{Created: created}}
}

// jiraTime renders a time in the Jira Cloud created layout the collector parses.
func jiraTime(t time.Time) string {
	return t.Format("2006-01-02T15:04:05.000-0700")
}

// gatherHistogram returns the *dto.Histogram for the given metric name +
// query/project labels from a collector, or nil if absent.
func gatherHistogram(t *testing.T, c prometheus.Collector, name, query, project string) *dto.Histogram {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelValue(m, "query") == query && labelValue(m, "project") == project {
				return m.GetHistogram()
			}
		}
	}
	return nil
}

func gatherGaugeValue(t *testing.T, c prometheus.Collector, name, query, project string) (float64, bool) {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	if err := reg.Register(c); err != nil {
		t.Fatalf("register collector: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelValue(m, "query") == query && labelValue(m, "project") == project {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.GetLabel() {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}

// bucketCount returns the cumulative count for the bucket whose upper bound is
// leDays days (in seconds), or 0 if that bound is absent.
func bucketCount(h *dto.Histogram, leDays float64) uint64 {
	le := leDays * 86400
	for _, b := range h.GetBucket() {
		if b.GetUpperBound() == le {
			return b.GetCumulativeCount()
		}
	}
	return 0
}

// TestComputeAgeSnapshot_BucketsAndOldest verifies the snapshot's cumulative
// buckets, count, sum, and oldest age against a hand-computed fixture.
func TestComputeAgeSnapshot_BucketsAndOldest(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	issues := []JiraIssue{
		issueCreated(jiraTime(now.Add(-2 * 24 * time.Hour))),   // 2d
		issueCreated(jiraTime(now.Add(-10 * 24 * time.Hour))),  // 10d
		issueCreated(jiraTime(now.Add(-100 * 24 * time.Hour))), // 100d (oldest)
	}
	snap := computeAgeSnapshot(issues, "PE", now)

	if snap.count != 3 {
		t.Errorf("count = %d, want 3", snap.count)
	}
	if snap.project != "PE" {
		t.Errorf("project = %q, want PE", snap.project)
	}
	// oldest = 100d in seconds.
	if want := 100.0 * 86400; snap.oldestSecs != want {
		t.Errorf("oldestSecs = %v, want %v", snap.oldestSecs, want)
	}
	// sum = (2+10+100) days in seconds.
	if want := 112.0 * 86400; snap.sum != want {
		t.Errorf("sum = %v, want %v", snap.sum, want)
	}
	// Cumulative bucket checks: le=3d covers only the 2d issue.
	if got := snap.buckets[3*86400]; got != 1 {
		t.Errorf("bucket le=3d = %d, want 1 (only the 2d issue)", got)
	}
	// le=14d covers 2d + 10d.
	if got := snap.buckets[14*86400]; got != 2 {
		t.Errorf("bucket le=14d = %d, want 2 (2d + 10d)", got)
	}
	// le=180d covers all three.
	if got := snap.buckets[180*86400]; got != 3 {
		t.Errorf("bucket le=180d = %d, want 3 (all)", got)
	}
	// le=90d covers 2d + 10d but NOT 100d — the SLA-breach discriminator.
	if got := snap.buckets[90*86400]; got != 2 {
		t.Errorf("bucket le=90d = %d, want 2 (100d issue breaches 90d SLA)", got)
	}
}

// TestComputeAgeSnapshot_SkipsUnparseableAndClampsFuture confirms an
// unparseable created value is dropped and a future created (clock skew) clamps
// to age 0 rather than going negative.
func TestComputeAgeSnapshot_SkipsUnparseableAndClampsFuture(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	issues := []JiraIssue{
		issueCreated(""),                                     // empty -> skipped
		issueCreated("not-a-timestamp"),                      // garbage -> skipped
		issueCreated(jiraTime(now.Add(24 * time.Hour))),      // future -> age 0
		issueCreated(jiraTime(now.Add(-5 * 24 * time.Hour))), // 5d
	}
	snap := computeAgeSnapshot(issues, "", now)

	if snap.count != 2 {
		t.Errorf("count = %d, want 2 (two parseable)", snap.count)
	}
	if snap.project != "all" {
		t.Errorf("project = %q, want all (empty project defaults to all)", snap.project)
	}
	// oldest is the 5d issue; the future issue clamped to 0.
	if want := 5.0 * 86400; snap.oldestSecs != want {
		t.Errorf("oldestSecs = %v, want %v", snap.oldestSecs, want)
	}
	// The clamped-to-0 issue lands in the smallest bucket (le=1d).
	if got := snap.buckets[1*86400]; got != 1 {
		t.Errorf("bucket le=1d = %d, want 1 (future issue clamped to age 0)", got)
	}
}

// TestComputeAgeSnapshot_EmptyExposesAllBuckets confirms an empty issue set
// still yields a full, zeroed bucket set (a stable histogram shape) and oldest 0.
func TestComputeAgeSnapshot_EmptyExposesAllBuckets(t *testing.T) {
	snap := computeAgeSnapshot(nil, "PE", time.Now())
	if snap.count != 0 {
		t.Errorf("count = %d, want 0", snap.count)
	}
	if snap.oldestSecs != 0 {
		t.Errorf("oldestSecs = %v, want 0", snap.oldestSecs)
	}
	if len(snap.buckets) != len(ageBucketBoundsSeconds) {
		t.Errorf("buckets len = %d, want %d (all bounds seeded)", len(snap.buckets), len(ageBucketBoundsSeconds))
	}
	for _, b := range ageBucketBoundsSeconds {
		if snap.buckets[b] != 0 {
			t.Errorf("bucket le=%v = %d, want 0", b, snap.buckets[b])
		}
	}
}

// TestParseJiraCreated covers the layouts and rejection cases.
func TestParseJiraCreated(t *testing.T) {
	cases := map[string]bool{
		"2026-07-20T10:00:00.000+0000": true,  // Jira Cloud
		"2026-07-20T10:00:00+0000":     true,  // no millis
		"2026-07-20T10:00:00Z":         true,  // RFC3339
		"2026-07-20T10:00:00+00:00":    true,  // RFC3339 colon offset
		"":                             false, // empty
		"garbage":                      false, // unparseable
	}
	for in, wantOK := range cases {
		if _, ok := parseJiraCreated(in); ok != wantOK {
			t.Errorf("parseJiraCreated(%q) ok = %v, want %v", in, ok, wantOK)
		}
	}
}

// TestAgeMetrics_CollectEmitsHistogramAndOldest drives a full scrape through the
// collector and asserts the const histogram + oldest gauge are exposed with the
// expected values via the registry.
func TestAgeMetrics_CollectEmitsHistogramAndOldest(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fake := newFakeAgeSearcher()
	fake.pages["jql"] = []*JiraSearchPage{{Issues: []JiraIssue{
		issueCreated(jiraTime(now.Add(-2 * 24 * time.Hour))),
		issueCreated(jiraTime(now.Add(-50 * 24 * time.Hour))),
	}}}

	m := NewJiraAgeMetrics()
	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:    fake,
		Metrics: m,
		Clock:   func() time.Time { return now },
		Queries: []AgeQuery{{Name: "open_bugs_age", JQL: "jql", Project: "PE"}},
	})
	c.collectDue(context.Background())

	h := gatherHistogram(t, m, "gardenio_jira_issue_age_seconds", "open_bugs_age", "PE")
	if h == nil {
		t.Fatal("age histogram not exposed")
	}
	if h.GetSampleCount() != 2 {
		t.Errorf("histogram sample count = %d, want 2", h.GetSampleCount())
	}
	// le=7d covers only the 2d issue; le=90d covers both.
	if got := bucketCount(h, 7); got != 1 {
		t.Errorf("bucket le=7d = %d, want 1", got)
	}
	if got := bucketCount(h, 90); got != 2 {
		t.Errorf("bucket le=90d = %d, want 2", got)
	}

	oldest, ok := gatherGaugeValue(t, m, "gardenio_jira_issue_oldest_age_seconds", "open_bugs_age", "PE")
	if !ok {
		t.Fatal("oldest gauge not exposed")
	}
	if want := 50.0 * 86400; oldest != want {
		t.Errorf("oldest = %v, want %v", oldest, want)
	}
}

// TestAgeCollector_PagesUntilNoToken confirms the collector follows
// NextPageToken until it is empty, aggregating issues across pages.
func TestAgeCollector_PagesUntilNoToken(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fake := newFakeAgeSearcher()
	fake.pages["jql"] = []*JiraSearchPage{
		{Issues: []JiraIssue{issueCreated(jiraTime(now.Add(-1 * 24 * time.Hour)))}, NextPageToken: "1"},
		{Issues: []JiraIssue{issueCreated(jiraTime(now.Add(-2 * 24 * time.Hour)))}, NextPageToken: "2"},
		{Issues: []JiraIssue{issueCreated(jiraTime(now.Add(-3 * 24 * time.Hour)))}}, // no token -> last
	}

	m := NewJiraAgeMetrics()
	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:    fake,
		Metrics: m,
		Clock:   func() time.Time { return now },
		Queries: []AgeQuery{{Name: "q", JQL: "jql"}},
	})
	c.collectDue(context.Background())

	if got := fake.calls["jql"]; got != 3 {
		t.Errorf("page fetches = %d, want 3 (followed 2 tokens)", got)
	}
	h := gatherHistogram(t, m, "gardenio_jira_issue_age_seconds", "q", "all")
	if h == nil || h.GetSampleCount() != 3 {
		t.Fatalf("histogram sample count = %v, want 3 across pages", h.GetSampleCount())
	}
}

// TestAgeCollector_MaxIssuesTruncates confirms the MaxIssues cap bounds the
// paged set and raises the truncated gauge.
func TestAgeCollector_MaxIssuesTruncates(t *testing.T) {
	jiraAgeScrapeTruncated.Reset()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fake := newFakeAgeSearcher()
	// Two pages of 1 issue each, but MaxIssues=1 stops after the first.
	fake.pages["jql"] = []*JiraSearchPage{
		{Issues: []JiraIssue{issueCreated(jiraTime(now.Add(-1 * 24 * time.Hour)))}, NextPageToken: "1"},
		{Issues: []JiraIssue{issueCreated(jiraTime(now.Add(-2 * 24 * time.Hour)))}},
	}
	m := NewJiraAgeMetrics()
	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:    fake,
		Metrics: m,
		Clock:   func() time.Time { return now },
		Queries: []AgeQuery{{Name: "q", JQL: "jql", MaxIssues: 1}},
	})
	c.collectDue(context.Background())

	h := gatherHistogram(t, m, "gardenio_jira_issue_age_seconds", "q", "all")
	if h == nil || h.GetSampleCount() != 1 {
		t.Fatalf("histogram sample count = %v, want 1 (capped)", h.GetSampleCount())
	}
	if got := testutil.ToFloat64(jiraAgeScrapeTruncated.WithLabelValues("q")); got != 1 {
		t.Errorf("truncated{q} = %v, want 1", got)
	}
}

// TestAgeCollector_ErrorIncrementsCounterAndKeepsPriorSnapshot confirms a scrape
// error bumps the error counter and does NOT overwrite a prior good snapshot.
func TestAgeCollector_ErrorIncrementsCounterAndKeepsPriorSnapshot(t *testing.T) {
	jiraAgeScrapeErrorsTotal.Reset()
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fake := newFakeAgeSearcher()
	fake.pages["jql"] = []*JiraSearchPage{{Issues: []JiraIssue{
		issueCreated(jiraTime(now.Add(-30 * 24 * time.Hour))),
	}}}
	m := NewJiraAgeMetrics()
	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:    fake,
		Metrics: m,
		Clock:   func() time.Time { return now },
		Queries: []AgeQuery{{Name: "q", JQL: "jql"}},
	})
	// Cycle 1: good snapshot.
	c.scrape(context.Background(), AgeQuery{Name: "q", JQL: "jql"})
	if h := gatherHistogram(t, m, "gardenio_jira_issue_age_seconds", "q", "all"); h == nil || h.GetSampleCount() != 1 {
		t.Fatalf("cycle1 sample count = %v, want 1", h.GetSampleCount())
	}

	// Cycle 2: query errors — counter bumps, prior snapshot preserved.
	fake.mu.Lock()
	fake.errs["jql"] = errors.New("boom")
	fake.mu.Unlock()
	c.scrape(context.Background(), AgeQuery{Name: "q", JQL: "jql"})

	if got := testutil.ToFloat64(jiraAgeScrapeErrorsTotal.WithLabelValues("q")); got != 1 {
		t.Errorf("age_scrape_errors{q} = %v, want 1", got)
	}
	if h := gatherHistogram(t, m, "gardenio_jira_issue_age_seconds", "q", "all"); h == nil || h.GetSampleCount() != 1 {
		t.Errorf("cycle2 sample count = %v, want still 1 (prior snapshot kept)", h.GetSampleCount())
	}
}

// TestAgeCollector_429Backoff confirms a 429 with Retry-After is returned as a
// backoff and does not publish a snapshot.
func TestAgeCollector_429Backoff(t *testing.T) {
	fake := newFakeAgeSearcher()
	fake.errs["jql"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 22 * time.Second}
	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:    fake,
		Queries: []AgeQuery{{Name: "q", JQL: "jql"}},
	})
	backoff := c.scrape(context.Background(), AgeQuery{Name: "q", JQL: "jql"})
	if backoff != 22*time.Second {
		t.Errorf("backoff = %v, want 22s (Retry-After)", backoff)
	}
}

// TestAgeCollector_429BackoffDefault confirms a 429 with no Retry-After falls
// back to the default backoff.
func TestAgeCollector_429BackoffDefault(t *testing.T) {
	fake := newFakeAgeSearcher()
	fake.errs["jql"] = &JiraAPIError{StatusCode: http.StatusTooManyRequests}
	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:    fake,
		Queries: []AgeQuery{{Name: "q", JQL: "jql"}},
	})
	if backoff := c.scrape(context.Background(), AgeQuery{Name: "q", JQL: "jql"}); backoff != defaultRateLimitBackoff {
		t.Errorf("backoff = %v, want default %v", backoff, defaultRateLimitBackoff)
	}
}

// TestAgeCollector_DueRespectsPerQueryInterval mirrors the count collector's
// scheduling test: a query with a longer interval is not re-run early.
func TestAgeCollector_DueRespectsPerQueryInterval(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	fake := newFakeAgeSearcher()
	fake.pages["a"] = []*JiraSearchPage{{Issues: []JiraIssue{issueCreated(jiraTime(now))}}}
	fake.pages["b"] = []*JiraSearchPage{{Issues: []JiraIssue{issueCreated(jiraTime(now))}}}

	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:     fake,
		Interval: 600 * time.Second,
		Clock:    func() time.Time { return now },
		Queries: []AgeQuery{
			{Name: "fast", JQL: "a"},
			{Name: "slow", JQL: "b", Interval: 3600 * time.Second},
		},
	})
	c.collectDue(context.Background())
	if fake.calls["a"] != 1 || fake.calls["b"] != 1 {
		t.Fatalf("first pass: a=%d b=%d, want 1/1", fake.calls["a"], fake.calls["b"])
	}

	now = now.Add(700 * time.Second) // fast due (>600s), slow not (<3600s)
	c.collectDue(context.Background())
	if fake.calls["a"] != 2 {
		t.Errorf("fast calls after 700s = %d, want 2", fake.calls["a"])
	}
	if fake.calls["b"] != 1 {
		t.Errorf("slow calls after 700s = %d, want 1 (interval not elapsed)", fake.calls["b"])
	}
}

// TestAgeCollector_BaseTickAndDelay covers the wake-cadence helpers: baseTick
// picks the smallest per-query interval (floored at 1s) and delay returns the
// larger of the base tick and a 429 backoff.
func TestAgeCollector_BaseTickAndDelay(t *testing.T) {
	c := NewJiraAgeCollector(JiraAgeConfig{
		Interval: 600 * time.Second,
		Queries: []AgeQuery{
			{Name: "a", JQL: "a"},
			{Name: "b", JQL: "b", Interval: 120 * time.Second}, // smaller override wins
		},
	})
	if got := c.baseTick(); got != 120*time.Second {
		t.Errorf("baseTick = %v, want 120s (smallest per-query interval)", got)
	}

	// A sub-second base is floored at 1s.
	c2 := NewJiraAgeCollector(JiraAgeConfig{
		Interval: 600 * time.Second,
		Queries:  []AgeQuery{{Name: "z", JQL: "z", Interval: 10 * time.Millisecond}},
	})
	if got := c2.baseTick(); got != time.Second {
		t.Errorf("baseTick = %v, want 1s floor", got)
	}

	// delay: base when backoff is smaller, backoff when larger.
	if got := c.delay(120*time.Second, 30*time.Second); got != 120*time.Second {
		t.Errorf("delay(120s,30s) = %v, want 120s (base)", got)
	}
	if got := c.delay(120*time.Second, 300*time.Second); got != 300*time.Second {
		t.Errorf("delay(120s,300s) = %v, want 300s (backoff)", got)
	}
}

// TestAgeCollector_RunScrapesThenHonorsContext confirms Run scrapes the due
// queries on entry and returns ctx.Err() when the context is cancelled, without
// hanging. A cancelled-after-first-cycle context exercises the loop's exit arm.
func TestAgeCollector_RunScrapesThenHonorsContext(t *testing.T) {
	now := time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC)
	fake := newFakeAgeSearcher()
	fake.pages["jql"] = []*JiraSearchPage{{Issues: []JiraIssue{issueCreated(jiraTime(now.Add(-3 * 24 * time.Hour)))}}}
	m := NewJiraAgeMetrics()
	c := NewJiraAgeCollector(JiraAgeConfig{
		Jira:     fake,
		Metrics:  m,
		Interval: time.Hour, // long, so the loop parks on the timer after the first scrape
		Clock:    func() time.Time { return now },
		Queries:  []AgeQuery{{Name: "q", JQL: "jql"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// The first scrape runs synchronously on Run entry (collectDue before the
	// timer). Wait until the snapshot lands, then cancel to unblock the loop.
	deadline := time.After(2 * time.Second)
	for {
		if h := gatherHistogram(t, m, "gardenio_jira_issue_age_seconds", "q", "all"); h != nil && h.GetSampleCount() == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("first scrape did not publish a snapshot within 2s")
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Run returned nil, want context cancellation error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestAgeMetrics_RegisterOnDefaultRegistry proves JiraAgeMetrics registers onto a
// registry and its Describe advertises both descriptors — the shape the
// jira-bridge relies on when registering against DefaultRegisterer.
func TestAgeMetrics_RegisterOnDefaultRegistry(t *testing.T) {
	m := NewJiraAgeMetrics()
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Seed one snapshot so the families materialize on gather.
	m.set("q", computeAgeSnapshot([]JiraIssue{issueCreated("2026-01-01T00:00:00.000+0000")}, "PE", time.Now()))
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	names := map[string]bool{}
	for _, mf := range mfs {
		names[mf.GetName()] = true
	}
	for _, want := range []string{
		"gardenio_jira_issue_age_seconds",
		"gardenio_jira_issue_oldest_age_seconds",
	} {
		if !names[want] {
			t.Errorf("metric family %q not exposed", want)
		}
	}
}
