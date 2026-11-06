package bridge

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// CI/CD compute-minutes metrics for gitlab-bridge (Phase G, task ka-0000002378;
// design docs/research/cicd-minutes-metrics.md, amendment decision
// ka-0000001083). The collector polls GitLab for two complementary series and
// publishes them as gardenio_cicd_* Prometheus metrics on the default registry,
// scraped from the bridge's existing :9092 /metrics endpoint:
//
//   - COMPLEMENTARY (job-duration): per-pipeline job durations summed into
//     gardenio_cicd_minutes_total, a compute-minutes counter, plus a queued
//     counter, a runs counter, and a per-job duration histogram. Runs on the
//     existing gitlab-bridge Group Access Token — lights up immediately.
//
//   - PRIMARY (first-party): GitLab's own billed compute-minute usage + quota
//     via the GraphQL ciMinutesUsage series and the REST group quota, published
//     as gardenio_cicd_gitlab_minutes_* gauges (used / quota / remaining /
//     exceeded, + per-project/subgroup). NEEDS a group-owner token (escalation
//     ka-0000006982); the bridge GAT 404s it, so this series stays dark (its
//     scrape errors increment but the job-duration series keeps flowing) until
//     the token lands. Ported from Shannon's acme-helm-chart !440.
//
// Registered globally against the default registerer (promauto), matching the
// other bridge metrics (jira_metrics.go, gardener_resume_metrics.go). Cardinality
// is bounded by design: no raw pipeline / job ids ever become label values (the
// dedup seen-set holds them in memory only); ref is coarsened to a bounded
// ref-class; job_name/runner_type facet only the histogram.
var (
	// gardenio_cicd_minutes_total is the cumulative billed compute MINUTES per
	// counted pipeline (Σ job.duration / 60). A counter — CI minutes are a
	// monotonically accumulating event total, rate()-able for minutes/day. The
	// collector's dedup seen-set keeps increments idempotent across polls and
	// restarts.
	cicdMinutesTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_cicd_minutes_total",
			Help: "Cumulative CI/CD compute minutes billed, summed from per-job durations, labelled by provider, project, ref-class, and terminal pipeline status.",
		},
		[]string{"provider", "project", "ref", "status"},
	)

	// gardenio_cicd_runs_total counts counted pipelines — the denominator for
	// "avg minutes per run" and failure-rate panels.
	cicdRunsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_cicd_runs_total",
			Help: "Count of counted CI/CD pipelines, labelled by provider, project, ref-class, and terminal status.",
		},
		[]string{"provider", "project", "ref", "status"},
	)

	// gardenio_cicd_queued_seconds_total is the cumulative queue/wait seconds
	// (Σ job.queued_duration) — the "wait vs run" split.
	cicdQueuedSecondsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_cicd_queued_seconds_total",
			Help: "Cumulative CI/CD queue/wait seconds, summed from per-job queued_duration, labelled by provider and project.",
		},
		[]string{"provider", "project"},
	)

	// gardenio_cicd_job_duration_seconds is the per-job duration distribution
	// (GitLab only). Faceted by a BOUNDED job_name set and runner_type; gardenio
	// CI has ~dozens of stable stage/job names, so this is safe here.
	cicdJobDurationSeconds = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gardenio_cicd_job_duration_seconds",
			Help:    "Per-job CI/CD duration distribution (seconds), labelled by provider, project, bounded job_name, and runner_type.",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600},
		},
		[]string{"provider", "project", "job_name", "runner_type"},
	)

	// ── First-party GitLab usage/quota gauges (dark until the token lands) ──

	// gardenio_cicd_gitlab_minutes_used is the namespace compute minutes used per
	// month (GraphQL ciMinutesUsage). month="current" is the live month.
	cicdGitLabMinutesUsed = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_gitlab_minutes_used",
			Help: "First-party GitLab compute minutes used by the namespace in a month (GraphQL ciMinutesUsage).",
		},
		[]string{"namespace", "month"},
	)

	// gardenio_cicd_gitlab_minutes_used_current is the current-month namespace
	// usage (the budget-tracking headline).
	cicdGitLabMinutesUsedCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_gitlab_minutes_used_current",
			Help: "First-party GitLab compute minutes used by the namespace in the current month.",
		},
		[]string{"namespace"},
	)

	// gardenio_cicd_gitlab_quota_total_current is base+additional quota ceiling.
	cicdGitLabQuotaTotalCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_gitlab_quota_total_current",
			Help: "First-party GitLab total (base + additional) compute-minute quota for the namespace, current month.",
		},
		[]string{"namespace"},
	)

	// gardenio_cicd_gitlab_remaining_current is quota_total − used (floored at 0).
	cicdGitLabRemainingCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_gitlab_remaining_current",
			Help: "First-party GitLab compute minutes remaining after current-month usage (0 when exhausted or unlimited).",
		},
		[]string{"namespace"},
	)

	// gardenio_cicd_gitlab_exceeded_current is 1 when used >= total quota, else 0.
	cicdGitLabExceededCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_gitlab_exceeded_current",
			Help: "First-party GitLab quota-exceeded flag: 1 when current-month usage is at/above the total quota, else 0.",
		},
		[]string{"namespace"},
	)

	// gardenio_cicd_gitlab_project_minutes_used_current is per-project current-
	// month usage. project_path is bounded (the group's project set).
	cicdGitLabProjectMinutesCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_gitlab_project_minutes_used_current",
			Help: "First-party GitLab compute minutes used by a project in the current month.",
		},
		[]string{"namespace", "project_path"},
	)

	// gardenio_cicd_gitlab_subgroup_minutes_used_current is per-subgroup rollup.
	cicdGitLabSubgroupMinutesCurrent = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_gitlab_subgroup_minutes_used_current",
			Help: "First-party GitLab compute minutes used by a subgroup and its descendant projects in the current month.",
		},
		[]string{"namespace", "subgroup_path"},
	)

	// ── Scrape-health (per provider) ──

	// gardenio_cicd_scrape_duration_seconds is the wall-clock of the most recent
	// poll cycle per provider ("gitlab" job-duration, "gitlab-firstparty").
	cicdScrapeDurationSeconds = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_scrape_duration_seconds",
			Help: "Wall-clock seconds of the most recent CI/CD-minutes poll cycle per provider/series.",
		},
		[]string{"provider"},
	)

	// gardenio_cicd_scrape_errors_total counts failed API calls per provider so a
	// stalled series is visible while its last-good counters linger.
	cicdScrapeErrorsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_cicd_scrape_errors_total",
			Help: "Count of failed CI/CD-minutes API calls per provider/series.",
		},
		[]string{"provider"},
	)

	// gardenio_cicd_last_success_timestamp_seconds is the unix time of the last
	// fully-successful cycle per provider — drives a "collector stale" alert.
	cicdLastSuccessTimestamp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "gardenio_cicd_last_success_timestamp_seconds",
			Help: "Unix time of the last fully-successful CI/CD-minutes poll per provider/series.",
		},
		[]string{"provider"},
	)
)

// provider label values for the scrape-health + counter series.
const (
	cicdProviderGitLab           = "gitlab"            // job-duration series
	cicdProviderGitLabFirstParty = "gitlab-firstparty" // GraphQL usage/quota series
)

// Default collector knobs. Mirrors the JIRA_METRICS_* defaults in the
// design doc §4.
const (
	defaultCICDMetricsInterval = 120 * time.Second
	defaultCICDMetricsWindow   = 60 * time.Minute
	defaultCICDLookbackMonths  = 12
)

// cicdPipelineLister is the slice of *GitLabClient the job-duration collector
// needs. Tests inject a fake so the loop runs without a live GitLab.
type cicdPipelineLister interface {
	ListPipelines(ctx context.Context, projectPath, ref, status string, updatedAfter time.Time) ([]GitLabPipeline, error)
	ListPipelineJobsByPath(ctx context.Context, projectPath string, pipelineID int) ([]GitLabJob, error)
}

// cicdUsageSource is the slice of *GitLabClient the first-party collector needs.
type cicdUsageSource interface {
	GetCIMinutesUsage(ctx context.Context, namespaceID string, lookbackMonths int) ([]GitLabCIMinutesMonth, error)
	GetGroupComputeQuota(ctx context.Context, groupPath string) (GitLabComputeQuota, error)
}

// CICDMetricsConfig configures the periodic CI/CD-minutes collector.
type CICDMetricsConfig struct {
	// Pipelines/Usage are the GitLab surfaces. Either may be nil to disable that
	// series independently (job-duration proceeds even if first-party is off).
	Pipelines cicdPipelineLister
	Usage     cicdUsageSource

	// Projects are the GitLab project paths whose pipelines are polled for the
	// job-duration series (e.g. "example-org/gardenio").
	Projects []string

	// GitLabJobDurationEnabled / GitLabFirstPartyEnabled gate each series.
	GitLabJobDurationEnabled bool
	GitLabFirstPartyEnabled  bool

	// FirstPartyNamespaceID is the GraphQL global namespace id for the usage
	// query (e.g. "gid://gitlab/Group/14331212").
	FirstPartyNamespaceID string
	// FirstPartyGroupPath is the group path for the REST quota query (e.g.
	// "Acme").
	FirstPartyGroupPath string
	// LookbackMonths caps the usage history returned (default 12).
	LookbackMonths int

	Interval time.Duration    // base poll cadence; default 120s
	Window   time.Duration    // pipeline updated_after lookback; default 60m
	Logger   *slog.Logger     // default slog.Default()
	Clock    func() time.Time // injectable clock; default time.Now
}

// CICDMetricsCollector periodically polls GitLab and publishes the gardenio_cicd_*
// series. Both series run on the same interval; each is independently gated and
// its failures are isolated (a first-party GraphQL 404 does not stop the
// job-duration series).
type CICDMetricsCollector struct {
	pipelines cicdPipelineLister
	usage     cicdUsageSource

	projects            []string
	jobDurationEnabled  bool
	firstPartyEnabled   bool
	firstPartyNamespace string
	firstPartyGroupPath string
	lookbackMonths      int

	interval time.Duration
	window   time.Duration
	logger   *slog.Logger
	clock    func() time.Time

	// seenPipelines is the dedup set of already-counted pipeline ids, keyed
	// "project#id" -> the clock time the entry was last observed, so a
	// pipeline's job durations are added to the counter exactly once across
	// polls. The value is a timestamp (not struct{}) so stale keys can be
	// pruned: a pipeline older than the window is never re-listed by
	// ListPipelines(updated_after=start-window) and so can never be
	// double-counted, so entries untouched for longer than the retention
	// grace are safe to drop. Without this the map would grow unbounded for
	// the process lifetime (GitLab pipeline ids are monotonic), a slow heap
	// leak in the long-lived gitlab-bridge.
	seenPipelines map[string]time.Time
}

// NewCICDMetricsCollector builds the collector with defaults.
func NewCICDMetricsCollector(cfg CICDMetricsConfig) *CICDMetricsCollector {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := cfg.Interval
	if interval <= 0 {
		interval = defaultCICDMetricsInterval
	}
	window := cfg.Window
	if window <= 0 {
		window = defaultCICDMetricsWindow
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	lookback := cfg.LookbackMonths
	if lookback <= 0 {
		lookback = defaultCICDLookbackMonths
	}
	return &CICDMetricsCollector{
		pipelines:           cfg.Pipelines,
		usage:               cfg.Usage,
		projects:            cfg.Projects,
		jobDurationEnabled:  cfg.GitLabJobDurationEnabled,
		firstPartyEnabled:   cfg.GitLabFirstPartyEnabled,
		firstPartyNamespace: cfg.FirstPartyNamespaceID,
		firstPartyGroupPath: cfg.FirstPartyGroupPath,
		lookbackMonths:      lookback,
		interval:            interval,
		window:              window,
		logger:              logger,
		clock:               clock,
		seenPipelines:       map[string]time.Time{},
	}
}

// seenPipelineRetention is how long a counted-pipeline dedup entry is kept
// after it was last observed. It only needs to exceed the poll window (a
// pipeline older than the window is never re-listed and so can never be
// double-counted); 2×window gives ample slack against clock skew and a
// missed poll while still bounding the map to roughly one lookback window of
// pipelines instead of every pipeline ever seen.
func (c *CICDMetricsCollector) seenPipelineRetention() time.Duration {
	return 2 * c.window
}

// Run collects once immediately, then on every interval tick until ctx is
// cancelled.
func (c *CICDMetricsCollector) Run(ctx context.Context) error {
	c.collectOnce(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			c.collectOnce(ctx)
		}
	}
}

// collectOnce runs both enabled series for one cycle. Each series is isolated:
// a failure in one increments only its scrape-error counter and does not stop
// the other.
func (c *CICDMetricsCollector) collectOnce(ctx context.Context) {
	if c.jobDurationEnabled && c.pipelines != nil {
		c.collectJobDuration(ctx)
	}
	if c.firstPartyEnabled && c.usage != nil {
		c.collectFirstParty(ctx)
	}
}

// collectJobDuration polls each tracked project's recently-updated pipelines,
// and for each newly-finished pipeline not yet counted, sums its job durations
// into the compute-minutes counter and observes per-job histograms.
func (c *CICDMetricsCollector) collectJobDuration(ctx context.Context) {
	start := c.clock()
	since := start.Add(-c.window)
	var failed bool

	for _, project := range c.projects {
		if ctx.Err() != nil {
			return
		}
		pipelines, err := c.pipelines.ListPipelines(ctx, project, "", "", since)
		if err != nil {
			cicdScrapeErrorsTotal.WithLabelValues(cicdProviderGitLab).Inc()
			c.logger.Warn("cicd-metrics: list pipelines failed", "project", project, "error", err)
			failed = true
			continue
		}
		for _, p := range pipelines {
			if ctx.Err() != nil {
				return
			}
			if !isTerminalPipelineStatus(p.Status) {
				continue // still running/pending — its durations are partial
			}
			key := project + "#" + strconv.Itoa(p.ID)
			if _, seen := c.seenPipelines[key]; seen {
				// Refresh the observation time so an actively-relisted
				// pipeline is not pruned out from under the dedup while it is
				// still inside the poll window.
				c.seenPipelines[key] = start
				continue
			}
			if err := c.countPipeline(ctx, project, p); err != nil {
				cicdScrapeErrorsTotal.WithLabelValues(cicdProviderGitLab).Inc()
				c.logger.Warn("cicd-metrics: count pipeline failed", "project", project, "pipeline", p.ID, "error", err)
				failed = true
				continue
			}
			c.seenPipelines[key] = start
		}
	}

	c.pruneSeenPipelines(start)

	cicdScrapeDurationSeconds.WithLabelValues(cicdProviderGitLab).Set(c.clock().Sub(start).Seconds())
	if !failed {
		cicdLastSuccessTimestamp.WithLabelValues(cicdProviderGitLab).Set(float64(c.clock().Unix()))
	}
}

// pruneSeenPipelines drops dedup entries not observed within the retention
// grace, keeping seenPipelines bounded to roughly one lookback window of
// pipelines rather than every terminal pipeline seen over the process's life.
// A dropped pipeline is older than the window and so is never re-listed —
// re-adding it is impossible, so dropping it cannot cause a double-count.
func (c *CICDMetricsCollector) pruneSeenPipelines(now time.Time) {
	cutoff := now.Add(-c.seenPipelineRetention())
	for key, seenAt := range c.seenPipelines {
		if seenAt.Before(cutoff) {
			delete(c.seenPipelines, key)
		}
	}
}

// countPipeline fetches one pipeline's jobs and adds Σ duration/60 to the
// minutes counter, Σ queued_duration to the queued counter, one runs increment,
// and per-job duration observations to the histogram.
func (c *CICDMetricsCollector) countPipeline(ctx context.Context, project string, p GitLabPipeline) error {
	jobs, err := c.pipelines.ListPipelineJobsByPath(ctx, project, p.ID)
	if err != nil {
		return err
	}
	refClass := refClass(p.Ref)
	var totalDuration, totalQueued float64
	for _, j := range jobs {
		if j.Duration != nil {
			totalDuration += *j.Duration
			cicdJobDurationSeconds.WithLabelValues(
				cicdProviderGitLab, project, j.Name, runnerType(j.Runner),
			).Observe(*j.Duration)
		}
		if j.QueuedDuration != nil {
			totalQueued += *j.QueuedDuration
		}
	}
	cicdMinutesTotal.WithLabelValues(cicdProviderGitLab, project, refClass, p.Status).Add(totalDuration / 60.0)
	cicdRunsTotal.WithLabelValues(cicdProviderGitLab, project, refClass, p.Status).Inc()
	if totalQueued > 0 {
		cicdQueuedSecondsTotal.WithLabelValues(cicdProviderGitLab, project).Add(totalQueued)
	}
	return nil
}

// collectFirstParty fetches the GraphQL usage series + REST quota and publishes
// the first-party gauges. On any error it increments the first-party scrape
// error counter and leaves the last-good gauges in place.
func (c *CICDMetricsCollector) collectFirstParty(ctx context.Context) {
	start := c.clock()

	months, err := c.usage.GetCIMinutesUsage(ctx, c.firstPartyNamespace, c.lookbackMonths)
	if err != nil {
		cicdScrapeErrorsTotal.WithLabelValues(cicdProviderGitLabFirstParty).Inc()
		cicdScrapeDurationSeconds.WithLabelValues(cicdProviderGitLabFirstParty).Set(c.clock().Sub(start).Seconds())
		c.logger.Warn("cicd-metrics: first-party usage fetch failed (needs group-owner token ka-0000006982)", "error", err)
		return
	}
	quota, err := c.usage.GetGroupComputeQuota(ctx, c.firstPartyGroupPath)
	if err != nil {
		cicdScrapeErrorsTotal.WithLabelValues(cicdProviderGitLabFirstParty).Inc()
		cicdScrapeDurationSeconds.WithLabelValues(cicdProviderGitLabFirstParty).Set(c.clock().Sub(start).Seconds())
		c.logger.Warn("cicd-metrics: first-party quota fetch failed", "error", err)
		return
	}

	c.publishFirstParty(months, quota, start)

	cicdScrapeDurationSeconds.WithLabelValues(cicdProviderGitLabFirstParty).Set(c.clock().Sub(start).Seconds())
	cicdLastSuccessTimestamp.WithLabelValues(cicdProviderGitLabFirstParty).Set(float64(c.clock().Unix()))
}

// publishFirstParty maps the usage months + quota into the first-party gauges.
// now is the collector's clock reading; its year-month selects the current
// month. Mirrors render_snapshot in the port source.
func (c *CICDMetricsCollector) publishFirstParty(months []GitLabCIMinutesMonth, quota GitLabComputeQuota, now time.Time) {
	ns := c.firstPartyGroupPath

	for _, m := range months {
		cicdGitLabMinutesUsed.WithLabelValues(ns, m.MonthISO8601).Set(m.Minutes)
	}

	current := currentMonth(months, now)
	if current == nil {
		return
	}

	cicdGitLabMinutesUsedCurrent.WithLabelValues(ns).Set(current.Minutes)

	base := deref(quota.Base)
	additional := deref(quota.Additional)
	if quota.Base != nil && quota.Additional != nil && base > 0 {
		total := base + additional
		remaining := total - current.Minutes
		if remaining < 0 {
			remaining = 0
		}
		cicdGitLabQuotaTotalCurrent.WithLabelValues(ns).Set(total)
		cicdGitLabRemainingCurrent.WithLabelValues(ns).Set(remaining)
		exceeded := 0.0
		if current.Minutes >= total {
			exceeded = 1.0
		}
		cicdGitLabExceededCurrent.WithLabelValues(ns).Set(exceeded)
	}

	for _, p := range current.Projects {
		cicdGitLabProjectMinutesCurrent.WithLabelValues(ns, p.FullPath).Set(p.Minutes)
	}
	for subgroup, minutes := range subgroupTotals(ns, current.Projects) {
		cicdGitLabSubgroupMinutesCurrent.WithLabelValues(ns, subgroup).Set(minutes)
	}
}

// currentMonth returns the record for now's year-month, falling back to the
// newest record (months is sorted newest-first). Returns nil for no records.
func currentMonth(months []GitLabCIMinutesMonth, now time.Time) *GitLabCIMinutesMonth {
	if len(months) == 0 {
		return nil
	}
	prefix := now.UTC().Format("2006-01")
	for i := range months {
		if strings.HasPrefix(months[i].MonthISO8601, prefix) {
			return &months[i]
		}
	}
	return &months[0]
}

// subgroupTotals rolls per-project minutes up to each ancestor subgroup under
// groupPath, so nested rollups can be shown. Mirrors _subgroup_totals in the
// port source.
func subgroupTotals(groupPath string, projects []GitLabCIMinutesProject) map[string]float64 {
	prefix := strings.Trim(groupPath, "/") + "/"
	totals := map[string]float64{}
	for _, p := range projects {
		if !strings.HasPrefix(p.FullPath, prefix) {
			continue
		}
		rel := strings.Split(strings.TrimPrefix(p.FullPath, prefix), "/")
		// The last component is the project; every preceding component is a
		// subgroup. Emit each ancestor.
		for i := 1; i < len(rel); i++ {
			subgroup := strings.Trim(groupPath, "/") + "/" + strings.Join(rel[:i], "/")
			totals[subgroup] += p.Minutes
		}
	}
	return totals
}

// isTerminalPipelineStatus reports whether a pipeline has finished (its job
// durations are stable and countable).
func isTerminalPipelineStatus(status string) bool {
	switch status {
	case "success", "failed", "canceled", "skipped":
		return true
	default:
		return false
	}
}

// refClass coarsens a git ref into a bounded label so branch/tag names never
// blow up cardinality. main → "main"; a calver-looking tag → "tag"; anything
// else → "branch".
func refClass(ref string) string {
	ref = strings.TrimSpace(ref)
	switch {
	case ref == "":
		return "unknown"
	case ref == "main" || ref == "master":
		return "main"
	case looksLikeTag(ref):
		return "tag"
	default:
		return "branch"
	}
}

// looksLikeTag reports whether ref looks like a release tag (starts with a
// digit or 'v' followed by a digit) rather than a branch name.
func looksLikeTag(ref string) bool {
	if ref == "" {
		return false
	}
	if ref[0] >= '0' && ref[0] <= '9' {
		return true
	}
	if len(ref) >= 2 && ref[0] == 'v' && ref[1] >= '0' && ref[1] <= '9' {
		return true
	}
	return false
}

// runnerType returns the bounded runner_type label for a job's runner, or
// "unknown" when the runner is absent (e.g. a skipped job).
func runnerType(r *GitLabRunner) string {
	if r == nil || r.RunnerType == "" {
		return "unknown"
	}
	return r.RunnerType
}
