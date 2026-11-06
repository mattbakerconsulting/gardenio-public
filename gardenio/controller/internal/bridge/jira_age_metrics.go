package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Per-issue AGE metrics for jira-bridge (follow-up to the JQL-count collector in
// jira_metrics.go; bead ka-0000000282). The count collector answers "how many
// issues match this JQL"; a grounded SLA-age / burn-down panel needs the AGE
// DISTRIBUTION of the matching issues — oldest-open age, age p50/p95, and the
// count breaching an N-day SLA. None of that is derivable from an
// approximate-count gauge, so this collector pages the matching issues (fields=
// created only) and republishes their ages as a SNAPSHOT histogram every cycle.
//
// Why a CONST histogram (prometheus.MustNewConstHistogram) rather than a
// promauto.Histogram whose Observe accumulates: the open-issue set is
// re-measured from scratch each cycle, so a cumulative histogram would double,
// triple, … count the SAME still-open issues on every scrape and the bucket
// counts would grow without bound. A const histogram rebuilt from the current
// snapshot each Collect is the correct "current distribution" semantics — the
// bucket counts track the live open set, not the number of times we scraped it.
//
// Registration: JiraAgeMetrics is a plain prometheus.Collector registered by the
// caller (jira-bridge main registers it on prometheus.DefaultRegisterer, the
// gatherer its :9093 /metrics handler serves). It registers NOTHING at package
// var-init, so importing this package into the controller / other bridges (which
// never run the collector) adds no phantom series — the promauto scrape-health
// vars below only emit a series once a query is actually scraped.

// ageBucketBoundsDays are the histogram bucket upper bounds in DAYS. Chosen to
// straddle the SLA horizons an on-call team reasons about (a day, a work-week,
// a fortnight, a month, a quarter, half a year, a year) so
// `gardenio_jira_issue_age_seconds_bucket{le="<Nd in seconds>"}` reads directly
// as "issues at most N days old" and (_count - that bucket) as "issues older
// than N days" — the SLA-breach count.
var ageBucketBoundsDays = []float64{1, 3, 7, 14, 30, 60, 90, 180, 365}

// ageBucketBoundsSeconds is ageBucketBoundsDays expressed in seconds, the unit
// the histogram observes (issue ages are seconds so `histogram_quantile` and the
// `s` Grafana unit line up). Sorted ascending for stable cumulative counting.
var ageBucketBoundsSeconds = daysToSeconds(ageBucketBoundsDays)

func daysToSeconds(days []float64) []float64 {
	out := make([]float64, len(days))
	for i, d := range days {
		out[i] = d * 86400
	}
	sort.Float64s(out)
	return out
}

var (
	// gardenio_jira_age_scrape_duration_seconds is the wall-clock duration of the
	// most recent age scrape (all pages) for each query. A gauge (last value):
	// one paged scrape per query per cycle, so the last duration is the signal.
	jiraAgeScrapeDurationSeconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_jira_age_scrape_duration_seconds",
			Help: "Wall-clock seconds of the most recent JIRA per-issue age scrape (all pages) for a named query.",
		},
		[]string{"query"},
	)

	// gardenio_jira_age_scrape_errors_total counts failed age scrapes per query
	// (HTTP error, transport error, context cancellation, or an HTTP 429 that
	// aborted paging), so a query whose age snapshot has gone stale is visible
	// even while its last good histogram lingers.
	jiraAgeScrapeErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_age_scrape_errors_total",
			Help: "Count of failed JIRA per-issue age scrapes for a named query.",
		},
		[]string{"query"},
	)

	// gardenio_jira_age_scrape_truncated is 1 while the most recent age scrape hit
	// its MaxIssues cap (the JQL matched more issues than were paged, so the
	// snapshot is a bounded prefix) and 0 once a later scrape fits under the cap.
	// A capped snapshot still grounds the low buckets but undercounts the tail,
	// so the panel can badge "partial" off this series.
	jiraAgeScrapeTruncated = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_jira_age_scrape_truncated",
			Help: "1 while the most recent per-issue age scrape for a named query hit its MaxIssues cap (snapshot is a bounded prefix), else 0.",
		},
		[]string{"query"},
	)
)

// defaultAgeMaxIssues caps how many issues one age query pages per cycle, a
// safety bound so a query that unexpectedly matches a huge set cannot page
// forever or blow the rate-limit budget. 2000 comfortably covers the shipped
// bounded queries (open bugs ~585, open high-priority ~600) with headroom.
const defaultAgeMaxIssues = 2000

// ageScrapePageSize is the per-page maxResults for the paged age search. 100 is
// the Jira Cloud page ceiling for /search/jql and keeps the page count (and so
// the request count) low for the bounded queries this collector runs.
const ageScrapePageSize = 100

// jiraCreatedLayouts are the timestamp layouts tried, in order, when parsing an
// issue's `created` field. Jira Cloud renders "2006-01-02T15:04:05.000-0700";
// RFC3339 is a defensive fallback for any variant that carries a colon in the
// zone offset. An issue whose created value parses under none is skipped (it
// contributes to neither the histogram nor the oldest gauge) rather than
// poisoning the distribution with a bogus age.
var jiraCreatedLayouts = []string{
	"2006-01-02T15:04:05.000-0700",
	"2006-01-02T15:04:05-0700",
	time.RFC3339,
}

// ageSnapshot is one query's most recent age distribution — the const-histogram
// inputs plus the oldest-age gauge value. buckets holds CUMULATIVE counts keyed
// by upper bound in seconds (le -> number of issues at most that many seconds
// old), the shape prometheus.MustNewConstHistogram consumes directly.
type ageSnapshot struct {
	project    string
	count      uint64
	sum        float64
	buckets    map[float64]uint64
	oldestSecs float64
}

// JiraAgeMetrics is the prometheus.Collector that republishes the latest
// per-query age snapshot as a const histogram + an oldest-age gauge on every
// scrape. The collector loop (JiraAgeCollector) calls set() once per cycle; the
// registry calls Collect() on each /metrics scrape. Decoupling the two means the
// exported distribution is always the freshest computed snapshot, independent of
// scrape timing.
type JiraAgeMetrics struct {
	mu         sync.RWMutex
	snapshots  map[string]ageSnapshot // keyed by query name
	ageDesc    *prometheus.Desc
	oldestDesc *prometheus.Desc
}

// NewJiraAgeMetrics builds the collector with its metric descriptors. It does
// not register itself — call Register (or hand it to prometheus.Registerer) so a
// bare import stays side-effect-free.
func NewJiraAgeMetrics() *JiraAgeMetrics {
	return &JiraAgeMetrics{
		snapshots: map[string]ageSnapshot{},
		ageDesc: prometheus.NewDesc(
			"gardenio_jira_issue_age_seconds",
			"Age (now - created) in seconds of each open JIRA issue matching a named query, as a snapshot histogram recomputed each scrape cycle. Drives oldest-age, age p50/p95 (histogram_quantile) and N-day SLA-breach counts (_count - _bucket{le=\"Nd\"}).",
			[]string{"query", "project"},
			nil,
		),
		oldestDesc: prometheus.NewDesc(
			"gardenio_jira_issue_oldest_age_seconds",
			"Age in seconds of the OLDEST open JIRA issue matching a named query (0 when the query matched no issues). Exposed directly because the single oldest value is not recoverable from histogram buckets.",
			[]string{"query", "project"},
			nil,
		),
	}
}

// Register adds the collector to reg. Convenience wrapper so callers need not
// import prometheus just to register.
func (m *JiraAgeMetrics) Register(reg prometheus.Registerer) error {
	return reg.Register(m)
}

// Describe implements prometheus.Collector.
func (m *JiraAgeMetrics) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.ageDesc
	ch <- m.oldestDesc
}

// Collect implements prometheus.Collector, emitting the current snapshot for
// every query as a const histogram plus its oldest-age gauge. Snapshots are
// keyed by query name so no (query, project) label set is emitted twice in one
// Collect (which would be a duplicate-metric error).
func (m *JiraAgeMetrics) Collect(ch chan<- prometheus.Metric) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for query, snap := range m.snapshots {
		ch <- prometheus.MustNewConstHistogram(
			m.ageDesc, snap.count, snap.sum, snap.buckets, query, snap.project,
		)
		ch <- prometheus.MustNewConstMetric(
			m.oldestDesc, prometheus.GaugeValue, snap.oldestSecs, query, snap.project,
		)
	}
}

// set stores the latest snapshot for a query, replacing any prior one.
func (m *JiraAgeMetrics) set(query string, snap ageSnapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snapshots[query] = snap
}

// AgeQuery is one configured per-issue age metric. The JQL should scope to the
// OPEN set whose age matters (e.g. `... AND statusCategory != Done`); resolved
// issues have a frozen age and would flatten the live distribution.
type AgeQuery struct {
	Name    string // series label "query"; stable identifier (e.g. "open_bugs_age")
	Help    string // human description (carried in config; not a metric Help)
	JQL     string // the JQL whose matching issues are aged
	Project string // series label "project"; "" is published as "all"
	// Interval optionally overrides the collector's base interval for this one
	// query (age scrapes are heavier than a count, so they typically run less
	// often). 0 = use the default.
	Interval time.Duration
	// MaxIssues caps how many matching issues are paged per cycle. 0 = the
	// package default (defaultAgeMaxIssues).
	MaxIssues int
}

// jiraSearcher is the slice of *JiraClient the age collector needs: a single
// paged JQL search returning the `created` field. Tests inject a fake.
type jiraSearcher interface {
	SearchIssuesPage(ctx context.Context, jql string, fields []string, maxResults int, pageToken string) (*JiraSearchPage, error)
}

// JiraAgeConfig configures the periodic per-issue age collector.
type JiraAgeConfig struct {
	Jira     jiraSearcher
	Metrics  *JiraAgeMetrics
	Queries  []AgeQuery
	Interval time.Duration    // base scrape cadence; default 600s
	Logger   *slog.Logger     // default slog.Default()
	Clock    func() time.Time // injectable "now" for age math; default time.Now
}

// defaultAgeInterval is the base age-scrape cadence when none is configured.
// 600s (10m) is deliberately slower than the count collector's 120s: an age
// scrape pages the whole matching set, so it is far more request-heavy, and age
// distributions drift slowly enough that 10-minute resolution is ample.
const defaultAgeInterval = 600 * time.Second

// JiraAgeCollector periodically pages each query's matching issues, computes the
// age distribution, and hands it to JiraAgeMetrics. It mirrors
// JiraMetricsCollector's scheduling (per-query intervals, 429-aware backoff) but
// pages issues instead of issuing a single approximate-count.
type JiraAgeCollector struct {
	jira     jiraSearcher
	metrics  *JiraAgeMetrics
	queries  []AgeQuery
	interval time.Duration
	logger   *slog.Logger
	clock    func() time.Time
	lastRun  map[string]time.Time
}

// NewJiraAgeCollector builds the collector. Defaults: Interval 600s, Logger
// slog.Default(), Clock time.Now, and a fresh JiraAgeMetrics if none is passed.
func NewJiraAgeCollector(cfg JiraAgeConfig) *JiraAgeCollector {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultAgeInterval
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	metrics := cfg.Metrics
	if metrics == nil {
		metrics = NewJiraAgeMetrics()
	}
	return &JiraAgeCollector{
		jira:     cfg.Jira,
		metrics:  metrics,
		queries:  cfg.Queries,
		interval: interval,
		logger:   logger,
		clock:    clock,
		lastRun:  make(map[string]time.Time, len(cfg.Queries)),
	}
}

// Run scrapes all due queries immediately, then on every tick until ctx is
// cancelled. A 429 Retry-After on any query stretches the next delay so the loop
// does not hammer a rate-limited Jira.
func (c *JiraAgeCollector) Run(ctx context.Context) error {
	base := c.baseTick()
	backoff := c.collectDue(ctx)
	timer := time.NewTimer(c.delay(base, backoff))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			backoff := c.collectDue(ctx)
			timer.Reset(c.delay(base, backoff))
		}
	}
}

// collectDue scrapes every query whose interval has elapsed since its last run
// and returns the longest Retry-After backoff seen this cycle (0 if none).
func (c *JiraAgeCollector) collectDue(ctx context.Context) time.Duration {
	now := c.clock()
	var maxBackoff time.Duration
	for _, q := range c.queries {
		if ctx.Err() != nil {
			return maxBackoff
		}
		if !c.due(q, now) {
			continue
		}
		if b := c.scrape(ctx, q); b > maxBackoff {
			maxBackoff = b
		}
		c.lastRun[q.Name] = now
	}
	return maxBackoff
}

// scrape pages one query's matching issues, publishes the age snapshot + scrape
// duration, and returns a backoff duration when the call hit HTTP 429. A partial
// page set from a mid-paging error still publishes what was gathered so the
// snapshot degrades gracefully rather than vanishing.
func (c *JiraAgeCollector) scrape(ctx context.Context, q AgeQuery) time.Duration {
	start := time.Now()
	issues, truncated, err := c.pageIssues(ctx, q)
	jiraAgeScrapeDurationSeconds.WithLabelValues(q.Name).Set(time.Since(start).Seconds())

	if err != nil {
		jiraAgeScrapeErrorsTotal.WithLabelValues(q.Name).Inc()
		c.logger.Warn("jira-age: scrape failed", "query", q.Name, "jql", q.JQL, "error", err)
		if apiErr, ok := asJiraAPIError(err); ok && apiErr.StatusCode == http.StatusTooManyRequests {
			backoff := apiErr.RetryAfter
			if backoff <= 0 {
				backoff = defaultRateLimitBackoff
			}
			// A 429 mid-paging leaves a partial or empty set; do not overwrite a
			// good prior snapshot with the partial — just back off and retry.
			return backoff
		}
		// Non-429 error with no issues gathered: keep the last good snapshot.
		if len(issues) == 0 {
			return 0
		}
	}

	snap := computeAgeSnapshot(issues, q.Project, c.clock())
	c.metrics.set(q.Name, snap)
	if truncated {
		jiraAgeScrapeTruncated.WithLabelValues(q.Name).Set(1)
	} else {
		jiraAgeScrapeTruncated.WithLabelValues(q.Name).Set(0)
	}
	return 0
}

// pageIssues walks the JQL result set (created field only) up to the query's
// MaxIssues cap, returning the gathered issues, whether the cap truncated the
// set, and any error. On error it returns the issues gathered so far so a
// partial snapshot can still be published for a non-429 failure.
func (c *JiraAgeCollector) pageIssues(ctx context.Context, q AgeQuery) ([]JiraIssue, bool, error) {
	maxIssues := q.MaxIssues
	if maxIssues <= 0 {
		maxIssues = defaultAgeMaxIssues
	}
	var issues []JiraIssue
	token := ""
	for {
		if ctx.Err() != nil {
			return issues, false, ctx.Err()
		}
		page, err := c.jira.SearchIssuesPage(ctx, q.JQL, []string{"created"}, ageScrapePageSize, token)
		if err != nil {
			return issues, false, err
		}
		issues = append(issues, page.Issues...)
		if len(issues) >= maxIssues {
			return issues[:maxIssues], true, nil
		}
		if page.NextPageToken == "" {
			return issues, false, nil
		}
		token = page.NextPageToken
	}
}

// computeAgeSnapshot turns a set of issues into an ageSnapshot at instant now.
// Ages are clamped at 0 (a created timestamp in the future — clock skew — must
// not produce a negative age). buckets is seeded with every bound so the
// histogram always exposes a full, stable set of buckets even at count 0.
func computeAgeSnapshot(issues []JiraIssue, project string, now time.Time) ageSnapshot {
	buckets := make(map[float64]uint64, len(ageBucketBoundsSeconds))
	for _, b := range ageBucketBoundsSeconds {
		buckets[b] = 0
	}
	snap := ageSnapshot{project: projectLabel(project), buckets: buckets}
	for _, iss := range issues {
		created, ok := parseJiraCreated(iss.Fields.Created)
		if !ok {
			continue
		}
		age := now.Sub(created).Seconds()
		if age < 0 {
			age = 0
		}
		snap.count++
		snap.sum += age
		if age > snap.oldestSecs {
			snap.oldestSecs = age
		}
		for _, b := range ageBucketBoundsSeconds {
			if age <= b {
				buckets[b]++
			}
		}
	}
	return snap
}

// parseJiraCreated parses an issue `created` timestamp under the known Jira
// layouts, reporting whether any matched. An empty or unparseable value returns
// (zero, false) so the caller skips the issue.
func parseJiraCreated(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range jiraCreatedLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// due reports whether q should run at now: always on its first run, then once
// its effective interval has elapsed since the last run.
func (c *JiraAgeCollector) due(q AgeQuery, now time.Time) bool {
	last, ok := c.lastRun[q.Name]
	if !ok {
		return true
	}
	return now.Sub(last) >= c.effInterval(q)
}

// effInterval is q's own Interval when set, else the collector's base interval.
func (c *JiraAgeCollector) effInterval(q AgeQuery) time.Duration {
	if q.Interval > 0 {
		return q.Interval
	}
	return c.interval
}

// baseTick is the loop's wake cadence: the smallest effective interval across
// queries (so per-query overrides shorter than the base are honored), floored
// at 1s.
func (c *JiraAgeCollector) baseTick() time.Duration {
	smallest := c.interval
	for _, q := range c.queries {
		if q.Interval > 0 && q.Interval < smallest {
			smallest = q.Interval
		}
	}
	if smallest < time.Second {
		smallest = time.Second
	}
	return smallest
}

// delay is the next timer duration: the base tick, or the 429 backoff when it is
// longer.
func (c *JiraAgeCollector) delay(base, backoff time.Duration) time.Duration {
	if backoff > base {
		return backoff
	}
	return base
}
