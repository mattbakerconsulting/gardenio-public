package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/landmap"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/store"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsRefreshTimeout bounds a single background refresh of a DB-backed
// collector. It is generous relative to a healthy query but still finite, so a
// saturated Postgres cannot hang the refresher indefinitely — a refresh that
// exceeds it fails, the last snapshot is retained, and the staleness gauge
// keeps climbing (visible), rather than blocking anything. Crucially this
// deadline is NOT on the scrape path: Collect() only ever reads the cached
// snapshot, so /metrics stays pure in-memory regardless of DB latency
// (ka-0000006261).
const metricsRefreshTimeout = 25 * time.Second

// fleetRefreshInterval and fleetLifetimeRefreshInterval are the fleet
// collector's two cadences. The lifetime rollup (cost and sessions summed over
// every agent bead ever created) scans thousands of closed agents, so it runs
// every 5 minutes on its own ticker; everything else is cheap and runs every
// 30 s (gd-ipZ10t3oTo).
const (
	fleetRefreshInterval         = 30 * time.Second
	fleetLifetimeRefreshInterval = 5 * time.Minute
)

// fleetCollector is a Prometheus collector that exposes aggregate fleet
// metrics computed from the bead store. The store queries run on a background
// ticker (see start) — NOT on the scrape path. Collect() serves only the last
// cached snapshot, so scrape latency is independent of DB latency. A staleness
// gauge exposes the age of the current snapshot so a stalled refresher (e.g. a
// saturated Postgres that keeps timing out) is still visible to alerting.
// The lifetime cost/sessions rollup refreshes on its own slower ticker into
// its own snapshot with its own staleness gauge, so a slow lifetime query
// never holds back the other gauges.
//
// This decoupling exists because the /metrics endpoint backs the
// up{component="beads"} liveness signal (BeadsDaemonDown). Querying the DB on
// the scrape path coupled that liveness signal to the slowest query in the
// system: when lexical search seq-scanned the 695k-row beads table, /metrics
// took 10-20s against a 10s scrape_timeout and BeadsDaemonDown paged on a
// perfectly healthy daemon (ka-0000006261, downstream of ka-0000006300).
type fleetCollector struct {
	store       store.Store
	ttl         time.Duration
	lifetimeTTL time.Duration
	started     time.Time

	mu            sync.RWMutex
	lastFetch     time.Time // time of last SUCCESSFUL refresh (zero until first success)
	snapshot      *metricsSnapshot
	lifetimeFetch time.Time // time of last SUCCESSFUL lifetime refresh
	lifetime      *agentLifetimeSnapshot

	totalCostDesc      *prometheus.Desc
	activeAgentsDesc   *prometheus.Desc
	sessionsDesc       *prometheus.Desc
	beadsTotalDesc     *prometheus.Desc
	queueDepthDesc     *prometheus.Desc
	queueOldestAgeDesc *prometheus.Desc
	stalenessDesc      *prometheus.Desc

	lifetimeStalenessDesc *prometheus.Desc
}

type metricsSnapshot struct {
	activeAgentsByProject   map[string]int
	beadsByTypeStatus       map[store.FleetTypeStatusKey]int
	queueDepthByProject     map[store.FleetQueueDepthKey]int
	queueOldestAgeByProject map[store.FleetQueueDepthKey]float64
}

// agentLifetimeSnapshot holds the lifetime rollup. Every project with at
// least one agent bead has an entry in both maps.
type agentLifetimeSnapshot struct {
	costByProject     map[string]float64
	sessionsByProject map[string]float64
}

func newFleetCollector(s store.Store, ttl, lifetimeTTL time.Duration) *fleetCollector {
	return &fleetCollector{
		store:       s,
		ttl:         ttl,
		lifetimeTTL: lifetimeTTL,
		started:     time.Now(),
		totalCostDesc: prometheus.NewDesc(
			"gardenio_beads_total_cost_usd",
			"Total accumulated cost in USD for agents in a project",
			[]string{"project"}, nil,
		),
		activeAgentsDesc: prometheus.NewDesc(
			"gardenio_beads_active_agents",
			"Number of active agents (open or in_progress) in a project",
			[]string{"project"}, nil,
		),
		sessionsDesc: prometheus.NewDesc(
			"gardenio_beads_sessions_total",
			"Total number of agent sessions in a project",
			[]string{"project"}, nil,
		),
		beadsTotalDesc: prometheus.NewDesc(
			"gardenio_beads_total",
			"Total number of beads by type and status",
			[]string{"type", "status"}, nil,
		),
		queueDepthDesc: prometheus.NewDesc(
			"gardenio_beads_open_queue_depth",
			"Point-in-time count of OPEN operator-facing queue beads (decision, escalation, mail) per project — the undrained backlog a human still has to act on. Complements gardenio_beads_total{type,status} by adding the project dimension.",
			[]string{"project", "type"}, nil,
		),
		queueOldestAgeDesc: prometheus.NewDesc(
			"gardenio_beads_open_queue_oldest_age_seconds",
			"Age in seconds of the OLDEST open operator-facing queue bead (decision, escalation, mail) per project — head-of-line staleness of the undrained backlog. Complements gardenio_beads_open_queue_depth (the count): a bucket can be shallow yet stale when one item sits un-actioned. Absent (not 0) for an empty queue.",
			[]string{"project", "type"}, nil,
		),
		stalenessDesc: prometheus.NewDesc(
			"gardenio_beads_fleet_metrics_staleness_seconds",
			"Age of the fleet-metrics snapshot in seconds (time since the last successful background refresh). Climbs without bound if the refresher is stalled — alert on it.",
			nil, nil,
		),
		lifetimeStalenessDesc: prometheus.NewDesc(
			"gardenio_beads_fleet_lifetime_metrics_staleness_seconds",
			"Age in seconds of the agent lifetime rollup behind gardenio_beads_total_cost_usd and gardenio_beads_sessions_total (time since its last successful refresh; it refreshes every 5 minutes). Climbs without bound if that refresher is stalled.",
			nil, nil,
		),
	}
}

func (c *fleetCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.totalCostDesc
	ch <- c.activeAgentsDesc
	ch <- c.sessionsDesc
	ch <- c.beadsTotalDesc
	ch <- c.queueDepthDesc
	ch <- c.queueOldestAgeDesc
	ch <- c.stalenessDesc
	ch <- c.lifetimeStalenessDesc
}

// Collect serves ONLY the last cached snapshot — it never touches the store,
// so scrape latency is bounded by in-memory map iteration regardless of DB
// health. The snapshot is refreshed on a background ticker (see start).
func (c *fleetCollector) Collect(ch chan<- prometheus.Metric) {
	snap, lastFetch := c.currentSnapshot()
	life, lifetimeFetch := c.currentLifetime()

	// Always emit the staleness gauges, even before the first successful
	// refresh, so a refresher that has never completed is visible (its
	// staleness climbs from process start rather than reading absent/zero).
	ch <- prometheus.MustNewConstMetric(c.stalenessDesc, prometheus.GaugeValue, c.stalenessSeconds(lastFetch))
	ch <- prometheus.MustNewConstMetric(c.lifetimeStalenessDesc, prometheus.GaugeValue, c.stalenessSeconds(lifetimeFetch))

	if life != nil {
		for project, cost := range life.costByProject {
			ch <- prometheus.MustNewConstMetric(c.totalCostDesc, prometheus.GaugeValue, cost, project)
		}
		for project, count := range life.sessionsByProject {
			ch <- prometheus.MustNewConstMetric(c.sessionsDesc, prometheus.CounterValue, count, project)
		}
	}

	if snap == nil {
		return
	}

	for project, count := range snap.activeAgentsByProject {
		ch <- prometheus.MustNewConstMetric(c.activeAgentsDesc, prometheus.GaugeValue, float64(count), project)
	}
	// A project whose agents are all closed reads 0 active, not absent.
	if life != nil {
		for project := range life.costByProject {
			if _, ok := snap.activeAgentsByProject[project]; !ok {
				ch <- prometheus.MustNewConstMetric(c.activeAgentsDesc, prometheus.GaugeValue, 0, project)
			}
		}
	}
	for key, count := range snap.beadsByTypeStatus {
		ch <- prometheus.MustNewConstMetric(c.beadsTotalDesc, prometheus.GaugeValue, float64(count), key.Type, key.Status)
	}
	for key, count := range snap.queueDepthByProject {
		ch <- prometheus.MustNewConstMetric(c.queueDepthDesc, prometheus.GaugeValue, float64(count), key.Project, key.Type)
	}
	for key, ageSeconds := range snap.queueOldestAgeByProject {
		ch <- prometheus.MustNewConstMetric(c.queueOldestAgeDesc, prometheus.GaugeValue, ageSeconds, key.Project, key.Type)
	}
}

// stalenessSeconds reports how old the current snapshot is. Before the first
// successful refresh (lastFetch zero) it reports the age since the collector
// started, so "never refreshed" reads as a large, growing value rather than 0.
func (c *fleetCollector) stalenessSeconds(lastFetch time.Time) float64 {
	ref := lastFetch
	if ref.IsZero() {
		ref = c.started
	}
	if ref.IsZero() {
		return 0
	}
	return time.Since(ref).Seconds()
}

// currentSnapshot returns the cached snapshot and the time of the last
// successful refresh under a read lock. Never blocks on the store.
func (c *fleetCollector) currentSnapshot() (*metricsSnapshot, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot, c.lastFetch
}

// currentLifetime returns the cached lifetime rollup and the time of its last
// successful refresh under a read lock. Never blocks on the store.
func (c *fleetCollector) currentLifetime() (*agentLifetimeSnapshot, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.lifetime, c.lifetimeFetch
}

// start populates both caches once SYNCHRONOUSLY, in parallel (so the first
// scrape after the daemon boots — and the unit tests — see data immediately,
// off the scrape path), then launches one background goroutine per cadence
// that refreshes on every tick until ctx is cancelled. This is the ONLY place
// the store is queried for fleet metrics.
func (c *fleetCollector) start(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.refresh(ctx) }()
	go func() { defer wg.Done(); c.refreshLifetime(ctx) }()
	wg.Wait()
	go refreshEvery(ctx, c.ttl, c.refresh)
	go refreshEvery(ctx, c.lifetimeTTL, c.refreshLifetime)
}

func refreshEvery(ctx context.Context, every time.Duration, refresh func(context.Context)) {
	if every <= 0 {
		// A non-positive interval means "no periodic refresh"; time.NewTicker
		// would panic on it. start() already did the one-shot refresh.
		return
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			refresh(ctx)
		}
	}
}

// refresh runs the DB queries and swaps in a new snapshot on success. On error
// (including a saturated-DB timeout) the previous snapshot is retained and the
// staleness gauge keeps climbing — the fault is visible without ever blocking
// a scrape.
func (c *fleetCollector) refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, metricsRefreshTimeout)
	defer cancel()

	snap, err := c.fetchSnapshot(fetchCtx)
	if err != nil {
		slog.Warn("fleet metrics refresh failed; retaining stale snapshot", "error", err)
		return
	}

	c.mu.Lock()
	c.snapshot = snap
	c.lastFetch = time.Now()
	c.mu.Unlock()
}

// refreshLifetime is refresh for the lifetime rollup: on error the previous
// lifetime snapshot is kept and its staleness gauge keeps climbing.
func (c *fleetCollector) refreshLifetime(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, metricsRefreshTimeout)
	defer cancel()

	rollup, err := c.store.FleetAgentLifetimeRollup(fetchCtx)
	if err != nil {
		slog.Warn("fleet lifetime metrics refresh failed; retaining stale snapshot", "error", err)
		return
	}
	life := &agentLifetimeSnapshot{
		costByProject:     make(map[string]float64, len(rollup)),
		sessionsByProject: make(map[string]float64, len(rollup)),
	}
	for _, r := range rollup {
		// Ensure every project with at least one agent bead has an entry, so
		// Prometheus always emits HELP/TYPE lines for these metrics — even
		// when no agent has recorded cost or session usage yet (e.g. fresh
		// e2e environment).
		life.costByProject[r.Project] = r.TotalCostUSD
		life.sessionsByProject[r.Project] = r.SessionCount
	}

	c.mu.Lock()
	c.lifetime = life
	c.lifetimeFetch = time.Now()
	c.mu.Unlock()
}

func (c *fleetCollector) fetchSnapshot(ctx context.Context) (*metricsSnapshot, error) {
	// Every query pushes aggregation into the database. Per-scrape Go heap is
	// bounded by the number of distinct (type,status) pairs and the number
	// of distinct project labels — typically a few KB regardless of corpus
	// size. See kbeads/docs/oom-fleet-collector-investigation.md.
	activeAgents, err := c.store.FleetActiveAgentsByProject(ctx)
	if err != nil {
		return nil, err
	}

	typeStatusCounts, err := c.store.FleetTypeStatusCounts(ctx)
	if err != nil {
		return nil, err
	}

	queueDepth, err := c.store.FleetQueueDepthByProject(ctx)
	if err != nil {
		return nil, err
	}

	queueOldestAge, err := c.store.FleetQueueOldestAgeByProject(ctx)
	if err != nil {
		return nil, err
	}

	return &metricsSnapshot{
		activeAgentsByProject:   activeAgents,
		beadsByTypeStatus:       typeStatusCounts,
		queueDepthByProject:     queueDepth,
		queueOldestAgeByProject: queueOldestAge,
	}, nil
}

// projectFromLabels extracts the project name from labels like "project:gardenio".
func projectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" {
			return v
		}
	}
	return "unknown"
}

// MetricsHandler returns an http.Handler that serves the Prometheus /metrics
// endpoint. It is intended to be mounted on a separate internal-only port
// so that metrics are not exposed through the public API ingress.
//
// The two DB-backed collectors (fleet aggregates + embedding coverage/stale)
// are refreshed on background tickers whose lifetime is bound to ctx — their
// Collect() only reads an in-memory cache, so /metrics latency is independent
// of DB latency. This keeps the up{component="beads"} liveness signal (backing
// BeadsDaemonDown) decoupled from the slowest query in the system: previously
// a saturated Postgres made /metrics take 10-20s against a 10s scrape_timeout
// and paged BeadsDaemonDown on a healthy daemon (ka-0000006261). Cancel ctx
// (daemon shutdown) to stop the refreshers.
func (s *BeadsServer) MetricsHandler(ctx context.Context) http.Handler {
	reg := prometheus.NewRegistry()
	fc := newFleetCollector(s.store, fleetRefreshInterval, fleetLifetimeRefreshInterval)
	fc.start(ctx)
	reg.MustRegister(fc)
	plc := newProjectLabelLintCollector(s.store, projectLabelLintInterval)
	plc.start(ctx)
	reg.MustRegister(plc)
	reg.MustRegister(tokenRevocationsTotal, revokedTokensCurrent)
	reg.MustRegister(edgeAuthVerifyTotal)
	reg.MustRegister(authzDenialsTotal)
	reg.MustRegister(auditWritesTotal, auditWriteFailuresTotal)
	reg.MustRegister(auditRetentionClosedTotal, auditRetentionDeletedTotal)
	reg.MustRegister(historyRetentionDeletedTotal)
	reg.MustRegister(landmap.FeedDropped, landmap.Requests, landmap.DenylistHits)
	reg.MustRegister(windowLiveBuilds)
	reg.MustRegister(authzLegacyFallthroughTotal)
	reg.MustRegister(landsUntrustedInputUnusableTotal)
	reg.MustRegister(authzOwnershipAdmitsTotal, authzOwnershipConditionErrorsTotal)
	reg.MustRegister(beadsWritesRejectedTotal)
	reg.MustRegister(serviceFieldCrossFleetWrite)
	reg.MustRegister(operatorActivityTotal)
	reg.MustRegister(dbAuthErrorsTotal)
	reg.MustRegister(queryCanceledErrorsTotal)
	registerDBPoolMetrics(reg, s.store)
	registerOIDCVerifierMetrics(reg)
	reg.MustRegister(
		operatedByHeaderArrivalTotal,
		operatedByWritesTotal,
		operatedByResolverErrorsTotal,
		operatedByFallbackTotal,
		operatedByAmbiguousAgentTotal,
		writesWithAgentOperatorTotal,
		operatedByLegacySentinelTotal,
		operatedByWritesBySourceTotal,
		authoringHumanGateTotal,
		shortTitleGateTotal,
		heraldryGateTotal,
		operationTypeGateTotal,
		projectLabelInvariantTotal,
		operatorFleetFallbackTotal,
		requestsByAuthTypeTotal,
		responseBytesTotal,
		requestDuration,
	)
	if s.revocation != nil {
		revokedTokensCurrent.Set(float64(s.revocation.Size()))
	}
	metrics.RegisterEmbedding(ctx, reg, s.embedStats, 30*time.Second)
	metrics.RegisterReferences(reg)
	metrics.RegisterStore(reg)
	metrics.RegisterDecisions(reg)
	metrics.RegisterBeadEvents(reg)
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
}
