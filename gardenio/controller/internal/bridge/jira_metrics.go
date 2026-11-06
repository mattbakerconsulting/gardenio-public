package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// JQL-count metrics for jira-bridge (design bundle ka-0000005503, bead
// ka-0000003681). The collector runs configured JQL queries through the cheap
// /rest/api/3/search/approximate-count endpoint and publishes the counts as
// Prometheus gauges, plus per-query scrape-health series.
//
// Registered globally against the default registerer (promauto), matching the
// other bridge metrics (see gardener_resume_metrics.go). The jira-bridge serves
// /metrics from prometheus.DefaultGatherer, so these series are scrapeable; in
// the controller binary the collector never runs, so the families stay empty.
var (
	// gardenio_jira_issues is the approximate issue count for each configured
	// query. The project label records the JQL's project scope ("all" when the
	// query is unscoped) so faceted per-project series can coexist with
	// cross-project totals.
	jiraIssuesGauge = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_jira_issues",
			Help: "Approximate JIRA issue count for a named JQL query, labelled by query and project scope (project=all when the query spans all projects).",
		},
		[]string{"query", "project"},
	)

	// gardenio_jira_scrape_duration_seconds is the wall-clock duration of the
	// most recent approximate-count call for each query. A gauge (last value)
	// rather than a histogram: one cheap call per query per cycle, so the last
	// duration is the useful signal.
	jiraScrapeDurationSeconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_jira_scrape_duration_seconds",
			Help: "Wall-clock seconds of the most recent JIRA approximate-count call for a named query.",
		},
		[]string{"query"},
	)

	// gardenio_jira_scrape_errors_total counts failed approximate-count calls per
	// query (HTTP error, transport error, or context cancellation), so a query
	// that has gone stale is visible even while its last good gauge value lingers.
	jiraScrapeErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_scrape_errors_total",
			Help: "Count of failed JIRA approximate-count calls for a named query.",
		},
		[]string{"query"},
	)

	// gardenio_jira_rate_limited is 1 while a query is in HTTP 429 backoff and 0
	// once it next scrapes cleanly. The 1->0 edge is the RECOVERY signal — the
	// moment Jira's cooldown expired and normal throughput resumed — which is
	// otherwise invisible because 429s are folded into scrape_errors_total
	// alongside every other error. Rate-of-change on this gauge (or an alert on
	// sustained 1) pages while throttled; the drop back to 0 clears it.
	jiraRateLimitedGauge = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_jira_rate_limited",
			Help: "1 while a named JIRA query is backing off from an HTTP 429, 0 once it next scrapes cleanly (the 1->0 edge marks cooldown recovery).",
		},
		[]string{"query"},
	)

	// gardenio_jira_rate_limit_events_total counts 429 onsets per query — each
	// scrape that observed a fresh HTTP 429 and entered (or extended) backoff.
	// Paired with recoveries_total, a persistent events>recoveries gap means a
	// query is stuck throttled.
	jiraRateLimitEventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_rate_limit_events_total",
			Help: "Count of HTTP 429 rate-limit onsets for a named JIRA query (each scrape that entered backoff).",
		},
		[]string{"query"},
	)

	// gardenio_jira_rate_limit_recoveries_total counts observed recoveries per
	// query — each clean scrape that followed a 429 backoff (the 1->0 edge). This
	// is the durable, monotonic form of the rate_limited gauge's recovery edge,
	// so a recovery is still countable after the gauge has already fallen to 0.
	jiraRateLimitRecoveriesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_jira_rate_limit_recoveries_total",
			Help: "Count of observed rate-limit recoveries for a named JIRA query (a clean scrape following an HTTP 429 backoff).",
		},
		[]string{"query"},
	)

	// gardenio_jira_rate_limit_backoff_seconds is the backoff duration applied by
	// the most recent 429 for a query (the Retry-After hint, or the default
	// fallback when Jira sent none). It stays at its last value until the next
	// 429; use it to see how hard Jira is throttling, not as a live countdown.
	jiraRateLimitBackoffSeconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_jira_rate_limit_backoff_seconds",
			Help: "Backoff seconds applied by the most recent HTTP 429 for a named JIRA query (Retry-After, or the default fallback when absent).",
		},
		[]string{"query"},
	)
)

// defaultMetricsInterval is the base scrape cadence when none is configured.
// 120s keeps ~10 queries comfortably within the Jira Cloud points-based
// rate-limit budget (Phase 0 confirmed x-ratelimit-limit: 300 per window).
const defaultMetricsInterval = 120 * time.Second

// defaultRateLimitBackoff is the fallback pause applied after an HTTP 429 that
// carries no (or an unparseable) Retry-After header.
const defaultRateLimitBackoff = 60 * time.Second

// MetricQuery is one configured JQL-count metric.
type MetricQuery struct {
	Name    string // series label "query"; stable identifier (e.g. "open_bugs")
	Help    string // human description (carried in config; not a metric Help)
	JQL     string // the JQL passed to approximate-count
	Project string // series label "project"; "" is published as "all"
	// Interval optionally overrides the collector's base interval for this one
	// query (e.g. an expensive query scraped less often). 0 = use the default.
	Interval time.Duration
}

// jiraCounter is the slice of *JiraClient the collector needs. Tests inject a
// fake so the loop is exercised without a live Jira.
type jiraCounter interface {
	ApproximateCount(ctx context.Context, jql string) (int64, error)
}

// JiraMetricsConfig configures the periodic JQL-count collector.
type JiraMetricsConfig struct {
	Jira     jiraCounter
	Queries  []MetricQuery
	Interval time.Duration    // base scrape cadence; default 120s
	Logger   *slog.Logger     // default slog.Default()
	Clock    func() time.Time // injectable scheduling clock; default time.Now
}

// JiraMetricsCollector periodically scrapes JQL counts and publishes the
// gardenio_jira_* series. Each query runs at the collector's base interval
// unless it carries its own Interval override; an HTTP 429 backs the whole loop
// off for the Retry-After duration before the next cycle.
type JiraMetricsCollector struct {
	jira     jiraCounter
	queries  []MetricQuery
	interval time.Duration
	logger   *slog.Logger
	clock    func() time.Time
	lastRun  map[string]time.Time
	// rateLimited tracks whether each query is currently in 429 backoff, so a
	// clean scrape that follows a 429 can be recognized as a recovery (the
	// rate_limited gauge's 1->0 edge) rather than a plain success.
	rateLimited map[string]bool
}

// NewJiraMetricsCollector builds the collector. Defaults: Interval 120s,
// Logger slog.Default(), Clock time.Now.
func NewJiraMetricsCollector(cfg JiraMetricsConfig) *JiraMetricsCollector {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultMetricsInterval
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	return &JiraMetricsCollector{
		jira:        cfg.Jira,
		queries:     cfg.Queries,
		interval:    interval,
		logger:      logger,
		clock:       clock,
		lastRun:     make(map[string]time.Time, len(cfg.Queries)),
		rateLimited: make(map[string]bool, len(cfg.Queries)),
	}
}

// Run scrapes all due queries immediately, then on every tick until ctx is
// cancelled. A 429 Retry-After on any query stretches the next delay so the
// loop does not hammer a rate-limited Jira.
func (c *JiraMetricsCollector) Run(ctx context.Context) error {
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
func (c *JiraMetricsCollector) collectDue(ctx context.Context) time.Duration {
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

// scrape runs one query, publishes its duration and either the count gauge or
// an error increment, and returns a backoff duration when the call hit HTTP 429.
// It also maintains the rate-limit observability series: a fresh 429 marks the
// query rate-limited (onset), and the next clean scrape marks it recovered
// (the rate_limited gauge's 1->0 edge — cooldown expired, throughput resumed).
func (c *JiraMetricsCollector) scrape(ctx context.Context, q MetricQuery) time.Duration {
	start := time.Now()
	count, err := c.jira.ApproximateCount(ctx, q.JQL)
	jiraScrapeDurationSeconds.WithLabelValues(q.Name).Set(time.Since(start).Seconds())

	if err != nil {
		jiraScrapeErrorsTotal.WithLabelValues(q.Name).Inc()
		c.logger.Warn("jira-metrics: scrape failed", "query", q.Name, "jql", q.JQL, "error", err)
		if apiErr, ok := asJiraAPIError(err); ok && apiErr.StatusCode == http.StatusTooManyRequests {
			backoff := apiErr.RetryAfter
			if backoff <= 0 {
				backoff = defaultRateLimitBackoff
			}
			c.markRateLimited(q.Name, backoff)
			return backoff
		}
		// A non-429 error leaves the rate-limit state untouched: it neither
		// enters nor clears backoff, so a query already throttled stays flagged.
		return 0
	}

	jiraIssuesGauge.WithLabelValues(q.Name, projectLabel(q.Project)).Set(float64(count))
	c.markRecovered(q.Name)
	return 0
}

// markRateLimited records a fresh 429 for q: it counts the onset, publishes the
// backoff duration, raises the rate_limited gauge to 1, and logs. Called on
// every 429 (repeated 429s while already throttled re-arm the backoff and count
// again — a sustained throttle is a distinct signal from a single blip).
func (c *JiraMetricsCollector) markRateLimited(query string, backoff time.Duration) {
	jiraRateLimitEventsTotal.WithLabelValues(query).Inc()
	jiraRateLimitBackoffSeconds.WithLabelValues(query).Set(backoff.Seconds())
	jiraRateLimitedGauge.WithLabelValues(query).Set(1)
	c.rateLimited[query] = true
	c.logger.Warn("jira-metrics: rate limited, backing off", "query", query, "backoff", backoff)
}

// markRecovered clears q's rate-limit state on a clean scrape. When q was in
// backoff this is a genuine recovery (the 1->0 edge): it counts the recovery,
// drops the gauge to 0, and logs at INFO so the resume shows up in the bridge
// log next to the WARN that opened it. When q was never throttled it is a
// no-op beyond keeping the gauge pinned at 0.
func (c *JiraMetricsCollector) markRecovered(query string) {
	if c.rateLimited[query] {
		jiraRateLimitRecoveriesTotal.WithLabelValues(query).Inc()
		c.rateLimited[query] = false
		c.logger.Info("jira-metrics: rate-limit recovered, throughput resumed", "query", query)
	}
	jiraRateLimitedGauge.WithLabelValues(query).Set(0)
}

// due reports whether q should run at now: always on its first run, then once
// its effective interval has elapsed since the last run.
func (c *JiraMetricsCollector) due(q MetricQuery, now time.Time) bool {
	last, ok := c.lastRun[q.Name]
	if !ok {
		return true
	}
	return now.Sub(last) >= c.effInterval(q)
}

// effInterval is q's own Interval when set, else the collector's base interval.
func (c *JiraMetricsCollector) effInterval(q MetricQuery) time.Duration {
	if q.Interval > 0 {
		return q.Interval
	}
	return c.interval
}

// baseTick is the loop's wake cadence: the smallest effective interval across
// queries (so per-query overrides shorter than the base are honored), floored
// at 1s.
func (c *JiraMetricsCollector) baseTick() time.Duration {
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

// delay is the next timer duration: the base tick, or the 429 backoff when it
// is longer.
func (c *JiraMetricsCollector) delay(base, backoff time.Duration) time.Duration {
	if backoff > base {
		return backoff
	}
	return base
}

// projectLabel maps an empty project scope to the "all" series label so every
// gardenio_jira_issues series carries a non-empty project value.
func projectLabel(project string) string {
	if project == "" {
		return "all"
	}
	return project
}
