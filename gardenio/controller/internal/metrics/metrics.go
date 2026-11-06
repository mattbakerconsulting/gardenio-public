// Package metrics provides Prometheus metrics for the gardenio controller.
// It periodically queries agent, account, and project beads from the daemon
// and publishes cost, token usage, fleet size, and FSM resource-mapping gauges.
package metrics

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

const namespace = "gardenio"

// BeadQuerier abstracts the bead queries needed by the metrics collector.
type BeadQuerier interface {
	ListAgentBeads(ctx context.Context) ([]beadsapi.AgentBead, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	// ListAllBeadsFiltered PAGES the fetch and returns the whole matching set.
	// The advice-catalog budget gauges use it so an open-advice corpus larger
	// than a single page is measured in FULL — a single capped fetch (the old
	// Limit:500) would silently under-report the budget it exists to track (the
	// metrics half of the F10 silent-truncation bug, ka-0000005760). The advice
	// corpus is small and bounded, so paging it is a single round-trip in practice
	// and never risks the controller OOM class (ka-0000003555).
	ListAllBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
}

// Collector periodically queries beads and updates Prometheus gauges.
type Collector struct {
	querier  BeadQuerier
	logger   *slog.Logger
	interval time.Duration

	// defaultEnv is the deployment environment this controller runs in
	// (e.g. "prod", "e2e", "dev"), read from GARDENIO_ENV. It is the
	// fallback `env` label value for account gauges when an account bead
	// carries no explicit `env` field. This is the dimension that
	// distinguishes the same account name across prod / gardenio-next /
	// e2e so cross-environment dashboards no longer alias
	// (E2.2, epic ka-0000002391). Empty resolves to "unknown".
	defaultEnv string

	// Fleet gauges
	agentsActive *prometheus.GaugeVec

	// prewarmedPoolDepth is the live warm-pool member count by
	// {project,role,execution_target,state}, snapshotted each scrape from the
	// same agent-bead list agents_active reads. state=prewarmed is a ready
	// assignable member; queued/idle are parked members (capacity-deferred /
	// started-but-unassigned). execution_target=lambda_microvm + state=prewarmed
	// is the STANDBY-MicroVM count that bills for its whole idle life — a
	// dashboard multiplies it by a $/VM-hour constant for est. idle cost (the
	// warm-pool observability frontier, epic ka-0000004145 / ka-0000001143).
	// Reset-then-Set each scrape (like agents_active) so a drained pool reads 0.
	prewarmedPoolDepth *prometheus.GaugeVec

	// Per-agent cost/usage gauges (populated from agent bead fields)
	agentCostUSD      *prometheus.GaugeVec
	agentInputTokens  *prometheus.GaugeVec
	agentOutputTokens *prometheus.GaugeVec
	agentCacheRead    *prometheus.GaugeVec
	agentCacheWrite   *prometheus.GaugeVec
	agentRequests     *prometheus.GaugeVec
	agentSessions     *prometheus.GaugeVec
	agentToolCalls    *prometheus.GaugeVec
	agentErrors       *prometheus.GaugeVec
	agentCompactions  *prometheus.GaugeVec

	// agentAge is now - created_at for every open agent bead. created_at
	// (not updated_at) is deliberate: a wedged-but-churning agent keeps
	// updated_at fresh via SyncAll's periodic bead touches, which would hide
	// its true runtime (see TriggerAgentMaxRuntime's doc comment,
	// gardenio/controller/internal/config/config.go). This is the source
	// metric for the AgentRuntimeCeiling and AgentRestartChurn alerts
	// (gd-5MTDWjwNjm).
	agentAge *prometheus.GaugeVec

	// Per-account aggregate gauges
	accountCostUSD      *prometheus.GaugeVec
	accountSessions     *prometheus.GaugeVec
	accountInputTokens  *prometheus.GaugeVec
	accountOutputTokens *prometheus.GaugeVec
	accountCacheRead    *prometheus.GaugeVec
	accountCacheWrite   *prometheus.GaugeVec
	accountRequests     *prometheus.GaugeVec

	// Per-account ratelimit probe gauges
	accountUtilization5h *prometheus.GaugeVec
	accountUtilization7d *prometheus.GaugeVec
	accountThrottled     *prometheus.GaugeVec
	accountWarming       *prometheus.GaugeVec

	// Tenant land credit gauges (lands_credits.go): the credit rate in force
	// and each land's granted credits, refreshed every landCreditsInterval.
	landCreditRate     *prometheus.GaugeVec
	landCreditsGranted *prometheus.GaugeVec
	landCreditsAt      time.Time

	// Open-PR health gauges (open_prs.go): the PR sweeper's last count.
	openPRs   openPRGauges
	openPRsAt time.Time

	// FSM cross-estate join artifact: project → {ECS, RDS} resource mapping.
	// One presence series (value 1) per (project, env, cluster, service,
	// db_instance) tuple. See gardenio/docs/FSM_CORRELATION_CONTRACT.md §2.
	resourceMapping *prometheus.GaugeVec

	// Advice-catalog size gauges: matched advice bytes/bead-count per
	// (project, role) archetype by targeting scope, simulated through the
	// same pipeline as `gb advice audit` (advice_catalog.go). Phase 1 of
	// the context-crisis plan (ka-0000003733 / ka-0000006708) — makes
	// advice-corpus growth visible in Grafana over time instead of only in
	// one-off audits.
	adviceCatalogBytes *prometheus.GaugeVec
	adviceCatalogBeads *prometheus.GaugeVec

	// Abbreviated-render demand per (project, role) archetype: the bytes the
	// non-inline matched items emit through the real gb prime abbreviated
	// rendering (advice.AbbrevRenderBytes), i.e. the quantity that accumulates
	// against the abbreviated byte budget. This is the render half of the R4
	// tripwire (ka-0000002748): alert when a real archetype's demand crosses
	// ~80% of the 60000-byte budget, before silent truncation can recur.
	// Distinct from adviceCatalogBytes (full-description matched-catalog churn).
	advicePrimeRenderBytes *prometheus.GaugeVec

	// Per-project bare-project broadcast advice-bead count, by delivery
	// status (delivered opt-in vs denied). Phase 3 of the context-crisis
	// plan (ka-0000003733 / ka-0000004186) — makes unexpected broadcasts
	// visible so the Part-A cut cannot regress by accident.
	adviceBroadcastBeads *prometheus.GaugeVec

	// Advice-corpus size + churn gauges — the direct feed for the R4 tripwire
	// (advice-budget hardening bundle, ka-0000002665 / ka-0000006409). Unlike
	// the per-archetype catalog gauges above (which measure what a single
	// agent's loadout costs), these measure the WHOLE open pool and its
	// creation/close churn so a rising corpus is visible before it breaches the
	// R1 500-pool cap or the byte budget:
	//   adviceOpenTotal      — open, real (non-fixture, non-expired) advice pool.
	//   adviceSupersededOpen — subset still open but already superseded; the
	//                          compaction backlog the sweeper drains (should
	//                          trend to ~0 once R3's sweep runs).
	//   adviceCreated30d/adviceClosed30d — trailing-30d gross creation and
	//                          closes; net growth = created − closed.
	adviceOpenTotal      prometheus.Gauge
	adviceSupersededOpen prometheus.Gauge
	adviceCreated30d     prometheus.Gauge
	adviceClosed30d      prometheus.Gauge

	// reconcileTotal counts controller reconcile-loop completions, labelled by
	// the controller/reconciler name and the outcome (result=ok|error). It is
	// incremented from internal/reconciler on each reconcile completion and is
	// the direct measurement source for prod SLO S2 (controller reconcile
	// success rate >= 99%) and the ControllerReconcileErrorRate alert
	// (ka-0000001213). Lives on the Collector so it shares the controller's
	// single Prometheus registry that vmagent scrapes via /metrics — no new
	// scrape config is required.
	reconcileTotal *prometheus.CounterVec

	// Scrape metadata
	lastScrapeTime prometheus.Gauge
	scrapeErrors   prometheus.Counter

	mu      sync.Mutex
	started bool

	// Fleet-archetype cache (advice_catalog.go). The archetype grid — which
	// (project, role) pairs the fleet actually runs — is derived from a recent
	// window of agent beads (~thousands of beads), and it changes only when a
	// new role is introduced, not per-scrape. Caching it for fleetArchetypeTTL
	// keeps the heavy agent-bead query off the 30s scrape hot path. Guarded by
	// fleetMu. Zero fleetArchetypesAt means "never fetched".
	//
	// The refresh runs ASYNCHRONOUSLY: a cache-miss/stale scrape kicks off a
	// background walk (fleetRefreshing is the single-flight guard) and returns
	// the last good grid immediately, so the paged agent-bead walk never runs on
	// the scrape goroutine. Paging already bounds that walk to one page, but
	// keeping it off the scrape path is belt-and-suspenders for the guaranteed
	// cold-miss on the first scrape after every restart — the miss the 15m TTL
	// cannot cover, and the exact moment the unbounded fetch used to OOM the
	// process (ka-0000006669).
	fleetMu           sync.Mutex
	fleetArchetypes   []advice.Archetype
	fleetArchetypesAt time.Time
	fleetRefreshing   bool
}

// fleetArchetypeTTL bounds how often the collector refetches agent beads to
// re-derive the fleet archetype grid. 15 min trades a slightly stale grid (a
// brand-new role appears on the dashboard within one TTL) for keeping the
// ~thousands-of-beads window query off the 30s scrape loop.
const fleetArchetypeTTL = 15 * time.Minute

// New creates a new metrics collector. Call Register() to register with
// a prometheus.Registerer, then Start() to begin background collection.
func New(querier BeadQuerier, logger *slog.Logger, interval time.Duration) *Collector {
	agentLabels := []string{"agent", "project", "role"}
	// Account gauges carry an `env` label so the same account name does not
	// alias across prod / gardenio-next / e2e (E2.2). The value is
	// sourced per-account from the bead's `env` field, falling back to this
	// deployment's GARDENIO_ENV.
	accountLabels := []string{"account", "env"}

	c := &Collector{
		querier:    querier,
		logger:     logger.With("component", "metrics"),
		interval:   interval,
		defaultEnv: os.Getenv("GARDENIO_ENV"),

		agentsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agents_active",
			Help:      "Number of active agents by project, role, and state.",
		}, []string{"project", "role", "state"}),

		prewarmedPoolDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "prewarmed_pool_depth",
			Help:      "Current warm-pool member count by {project,role,execution_target,state,env}, snapshotted each scrape. state=prewarmed is a ready assignable member; queued/idle are parked members (capacity-deferred / started-but-unassigned). execution_target=lambda_microvm + state=prewarmed is the standby-MicroVM count that bills for its whole idle life — multiply by a $/VM-hour dashboard constant for est. idle cost (epic ka-0000004145).",
		}, []string{"project", "role", "execution_target", "state", "env"}),

		agentCostUSD: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_cost_usd",
			Help:      "Total cost in USD for an agent across all sessions.",
		}, agentLabels),

		agentInputTokens: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_input_tokens_current",
			Help:      "Total input tokens consumed by an agent.",
		}, agentLabels),

		agentOutputTokens: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_output_tokens_current",
			Help:      "Total output tokens produced by an agent.",
		}, agentLabels),

		agentCacheRead: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_cache_read_tokens_current",
			Help:      "Total cache-read tokens for an agent.",
		}, agentLabels),

		agentCacheWrite: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_cache_write_tokens_current",
			Help:      "Total cache-write tokens for an agent.",
		}, agentLabels),

		agentRequests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_requests_current",
			Help:      "Total API requests made by an agent.",
		}, agentLabels),

		agentSessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_sessions_current",
			Help:      "Total sessions for an agent.",
		}, agentLabels),

		agentToolCalls: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_tool_calls_current",
			Help:      "Total tool calls made by an agent.",
		}, agentLabels),

		agentErrors: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_errors_current",
			Help:      "Total errors encountered by an agent.",
		}, agentLabels),

		agentCompactions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_compactions_current",
			Help:      "Total context compactions for an agent.",
		}, agentLabels),

		agentAge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "agent_age_seconds",
			Help:      "Seconds since the agent bead was created (now - created_at), for every open agent. Uses created_at rather than updated_at so a wedged-but-churning agent cannot mask its true age.",
		}, agentLabels),

		accountCostUSD: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_cost_usd",
			Help:      "Total cost in USD for a credential account.",
		}, accountLabels),

		accountSessions: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_sessions_current",
			Help:      "Total sessions for a credential account.",
		}, accountLabels),

		accountInputTokens: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_input_tokens_current",
			Help:      "Total input tokens consumed across a credential account.",
		}, accountLabels),

		accountOutputTokens: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_output_tokens_current",
			Help:      "Total output tokens produced across a credential account.",
		}, accountLabels),

		accountCacheRead: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_cache_read_tokens_current",
			Help:      "Total cache-read tokens across a credential account.",
		}, accountLabels),

		accountCacheWrite: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_cache_write_tokens_current",
			Help:      "Total cache-write tokens across a credential account.",
		}, accountLabels),

		accountRequests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_requests_current",
			Help:      "Total API requests across a credential account.",
		}, accountLabels),

		accountUtilization5h: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_utilization_5h",
			Help:      "Current 5-hour window utilization ratio (0.0-1.0) from Anthropic rate-limit probe.",
		}, accountLabels),

		accountUtilization7d: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_utilization_7d",
			Help:      "Current 7-day window utilization ratio (0.0-1.0) from Anthropic rate-limit probe.",
		}, accountLabels),

		accountThrottled: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_throttled",
			Help:      "Whether the account is currently rate-limited (1=throttled, 0=allowed).",
		}, accountLabels),

		accountWarming: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "account_warming",
			Help:      "Whether the account is in soft-failover warming state (1=warming, 0=normal).",
		}, accountLabels),

		resourceMapping: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "resource_mapping",
			Help:      "Presence gauge (always 1) mapping a gardenio project+env to the ACME ECS/RDS resources it exercises. The cross-estate FSM join artifact (FSM_CORRELATION_CONTRACT.md §2).",
		}, []string{"project", "env", "cluster", "service", "db_instance"}),

		reconcileTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "reconcile_total",
			Help:      "Total controller reconcile-loop completions by controller name and result (ok|error). Direct measurement source for prod SLO S2 (reconcile success rate).",
		}, []string{"controller", "result"}),

		lastScrapeTime: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "metrics_last_scrape_timestamp_seconds",
			Help:      "Unix timestamp of last successful metrics scrape.",
		}),

		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "metrics_scrape_errors_total",
			Help:      "Total number of errors when scraping bead data.",
		}),
	}
	c.adviceCatalogBytes, c.adviceCatalogBeads, c.advicePrimeRenderBytes = newAdviceCatalogGauges()
	c.adviceBroadcastBeads = newAdviceBroadcastGauge()
	c.adviceOpenTotal, c.adviceSupersededOpen, c.adviceCreated30d, c.adviceClosed30d = newAdviceCorpusGauges()
	c.landCreditRate, c.landCreditsGranted = newLandCreditGauges()
	c.openPRs = newOpenPRGauges()
	return c
}

// RecordReconcile increments gardenio_reconcile_total for one reconcile-loop
// completion. controller is the reconciler/controller name (the `controller`
// label); ok selects the `result` label value ("ok" on success, "error" on a
// returned error). Safe for concurrent use. This is the wiring point the
// reconciler calls on every pass so SLO S2 can be measured directly rather
// than proxied (ka-0000001213).
func (c *Collector) RecordReconcile(controller string, ok bool) {
	result := "ok"
	if !ok {
		result = "error"
	}
	c.reconcileTotal.WithLabelValues(controller, result).Inc()
}

// Register registers all metrics with the given registerer.
func (c *Collector) Register(reg prometheus.Registerer) error {
	collectors := []prometheus.Collector{
		c.agentsActive,
		c.prewarmedPoolDepth,
		c.agentCostUSD,
		c.agentInputTokens,
		c.agentOutputTokens,
		c.agentCacheRead,
		c.agentCacheWrite,
		c.agentRequests,
		c.agentSessions,
		c.agentToolCalls,
		c.agentErrors,
		c.agentCompactions,
		c.agentAge,
		c.accountCostUSD,
		c.accountSessions,
		c.accountInputTokens,
		c.accountOutputTokens,
		c.accountCacheRead,
		c.accountCacheWrite,
		c.accountRequests,
		c.accountUtilization5h,
		c.accountUtilization7d,
		c.accountThrottled,
		c.accountWarming,
		c.landCreditRate,
		c.landCreditsGranted,
		c.resourceMapping,
		c.adviceCatalogBytes,
		c.adviceCatalogBeads,
		c.advicePrimeRenderBytes,
		c.adviceBroadcastBeads,
		c.adviceOpenTotal,
		c.adviceSupersededOpen,
		c.adviceCreated30d,
		c.adviceClosed30d,
		c.reconcileTotal,
		c.lastScrapeTime,
		c.scrapeErrors,
	}
	collectors = append(collectors, c.openPRs.collectors()...)
	for _, col := range collectors {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}

// Start begins the background collection loop. It blocks until ctx is cancelled.
func (c *Collector) Start(ctx context.Context) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()

	// Do an initial collection immediately.
	c.collect(ctx)

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

func (c *Collector) collect(ctx context.Context) {
	c.collectAgents(ctx)
	c.collectAccounts(ctx)
	c.collectLandCredits(ctx)
	c.collectOpenPRs(ctx)
	c.collectResourceMapping(ctx)
	c.collectAdviceCatalog(ctx)
	c.lastScrapeTime.SetToCurrentTime()
}

func (c *Collector) collectAgents(ctx context.Context) {
	agents, err := c.querier.ListAgentBeads(ctx)
	if err != nil {
		c.logger.Warn("failed to list agent beads for metrics", "error", err)
		c.scrapeErrors.Inc()
		return
	}

	// Reset vector gauges so departed agents disappear.
	c.agentsActive.Reset()
	c.prewarmedPoolDepth.Reset()
	c.agentCostUSD.Reset()
	c.agentInputTokens.Reset()
	c.agentOutputTokens.Reset()
	c.agentCacheRead.Reset()
	c.agentCacheWrite.Reset()
	c.agentRequests.Reset()
	c.agentSessions.Reset()
	c.agentToolCalls.Reset()
	c.agentErrors.Reset()
	c.agentCompactions.Reset()
	c.agentAge.Reset()

	// poolDepth accumulates warm-pool member counts keyed by
	// {project,role,execution_target,state}, Set below after the loop. Counting
	// into a map first (rather than Inc per bead) keeps a single Set per bucket,
	// matching the Reset-then-Set discipline the other scrape gauges use.
	type poolKey struct{ project, role, target, state string }
	poolDepth := map[poolKey]int{}

	for _, a := range agents {
		state := a.AgentState
		if state == "" {
			state = "unknown"
		}
		c.agentsActive.WithLabelValues(a.Project, a.Role, state).Inc()

		// Warm-pool depth: an UNASSIGNED pool member is minted with a
		// "prewarmed-" name prefix and carries no thread/task until it is
		// assigned off the pool (the same convention poolmanager's
		// isParkedPoolMember / listPrewarmedAgents use). Count only members
		// still parked in the pool — state prewarmed (ready/assignable) or
		// queued/idle (parked) — so a member that has been handed work drops out
		// of depth the instant it transitions. execution_target distinguishes the
		// lambda_microvm standby-VM count (which bills for its idle life) from the
		// K8s warm pool.
		if isUnassignedPoolMember(a) {
			target := a.Metadata["execution_target"]
			if target == "" {
				// Match effectiveExecutionTarget's default: an unstamped member
				// is a Kubernetes pool pod (the MicroVM path always stamps it).
				target = "kubernetes"
			}
			poolDepth[poolKey{a.Project, a.Role, target, state}]++
		}

		labels := prometheus.Labels{
			"agent":   a.AgentName,
			"project": a.Project,
			"role":    a.Role,
		}

		c.agentCostUSD.With(labels).Set(parseFloat(a.Metadata["total_cost_usd"]))
		c.agentInputTokens.With(labels).Set(parseFloat(a.Metadata["total_input_tokens"]))
		c.agentOutputTokens.With(labels).Set(parseFloat(a.Metadata["total_output_tokens"]))
		c.agentCacheRead.With(labels).Set(parseFloat(a.Metadata["total_cache_read_tokens"]))
		c.agentCacheWrite.With(labels).Set(parseFloat(a.Metadata["total_cache_write_tokens"]))
		c.agentRequests.With(labels).Set(parseFloat(a.Metadata["total_request_count"]))
		c.agentSessions.With(labels).Set(parseFloat(a.Metadata["session_count"]))
		c.agentToolCalls.With(labels).Set(parseFloat(a.Metadata["total_tool_calls"]))
		c.agentErrors.With(labels).Set(parseFloat(a.Metadata["total_errors"]))
		c.agentCompactions.With(labels).Set(parseFloat(a.Metadata["total_compactions"]))
		if !a.CreatedAt.IsZero() {
			c.agentAge.With(labels).Set(time.Since(a.CreatedAt).Seconds())
		}
	}

	// Emit one gauge series per warm-pool bucket. Reset above already cleared
	// last pass's series, so a (project,role,target,state) that dropped to zero
	// simply is not re-Set and reads absent/0.
	for k, n := range poolDepth {
		c.prewarmedPoolDepth.WithLabelValues(k.project, k.role, k.target, k.state, c.defaultEnv).Set(float64(n))
	}
}

// isUnassignedPoolMember reports whether an agent bead is a warm-pool member
// still parked in the pool (not yet handed work). It mirrors poolmanager's own
// pool-membership view (isParkedPoolMember / listPrewarmedAgents): a member is
// minted with a "prewarmed-" name prefix and carries no slack thread and no
// task until AssignPrewarmed flips it out of the pool. Only the parked states
// count toward depth — prewarmed (ready/assignable) or queued/idle (capacity-
// deferred / started-but-unassigned); a member that has left for real work no
// longer has an unassigned state and drops out. Keeping this predicate aligned
// with poolmanager means depth is exactly the population that struct reconciles.
func isUnassignedPoolMember(a beadsapi.AgentBead) bool {
	if !strings.HasPrefix(a.AgentName, "prewarmed-") {
		return false
	}
	if a.Metadata["slack_thread_ts"] != "" || a.Metadata["task_id"] != "" {
		return false
	}
	switch a.AgentState {
	case "prewarmed", "queued", "idle":
		return true
	default:
		return false
	}
}

// accountListLimit bounds the account-bead query so it never silently rides
// the daemon's defaultListBeadsLimit (1000, kbeads http_beads.go). It is set to
// the daemon's maxListBeadsLimit so the collector fetches every active account
// in one page — the active-account population is tiny (tens), and the loud
// Total-vs-len check below is what actually guards against silent truncation if
// that assumption ever breaks. See ka-0000005987.
const accountListLimit = 10000

func (c *Collector) collectAccounts(ctx context.Context) {
	// Filter to non-closed accounts and cap the page explicitly. Without the
	// status filter, closed account beads (which never age out) accumulate
	// monotonically toward the daemon's silent 1000-row default; without an
	// explicit Limit the daemon applies that default with an HTTP 200 and no
	// error, so overflow accounts would just vanish from the cost gauge. The
	// Total-vs-len check below turns any remaining truncation into a loud log
	// + scrapeErrors bump instead of a silent gap (ka-0000005987).
	result, err := c.querier.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: beadsapi.ActiveStatuses(),
		Limit:    accountListLimit,
	})
	if err != nil {
		c.logger.Warn("failed to list account beads for metrics", "error", err)
		c.scrapeErrors.Inc()
		return
	}
	if result.Total > len(result.Beads) {
		c.logger.Warn("account bead list truncated for metrics; some accounts will be missing from cost/usage gauges",
			"returned", len(result.Beads), "total", result.Total, "limit", accountListLimit)
		c.scrapeErrors.Inc()
	}

	c.accountCostUSD.Reset()
	c.accountSessions.Reset()
	c.accountInputTokens.Reset()
	c.accountOutputTokens.Reset()
	c.accountCacheRead.Reset()
	c.accountCacheWrite.Reset()
	c.accountRequests.Reset()
	c.accountUtilization5h.Reset()
	c.accountUtilization7d.Reset()
	c.accountThrottled.Reset()
	c.accountWarming.Reset()

	// spendSync writes total_cost_usd from LiteLLM GET /key/info?key=<litellm_key>,
	// which is a PER-KEY total. When two distinct account beads intentionally share
	// one virtual key (e.g. litellm-bedrock pinned to fsm-triage and glm-bedrock
	// pinned to glm-lab both carry sk-…pX0heA1A), each bead is stamped with the SAME
	// key-total, so a naive per-bead cost series double-counts that bill in
	// sum(max by(account,env)(gardenio_account_cost_usd)) — the P1 bill-conservation
	// side A — inflating it by one key's worth (ka-0000001340). Book each shared key's
	// cost exactly ONCE by attributing it to a single representative account per
	// (litellm_key, env); the other beads on that key emit a 0 cost series so their
	// {account} label still exists for joins but contributes nothing to the sum.
	// Only the cost gauge is per-key; session/token/request/utilization counters are
	// genuinely per-account (the beads differ in sessions, requests, etc.) and stay
	// per-bead below.
	costKeyOwner := c.pickCostOwners(result.Beads)

	for _, b := range result.Beads {
		name := b.Title
		if name == "" {
			continue
		}
		// Never emit a gauge series for an E2E test account (ka-0000005167). The
		// harness seeds throwaway accounts against the same long-lived deployment,
		// and a series OUTLIVES the bead for the vmsingle retention window — so this
		// producer-side skip is the long-term fix that keeps the by-account rollup
		// and the Accounts dropdown clean even after the harness closes the bead.
		// The viewer's PromQL exclusion covers only the retention tail of series
		// emitted before this landed.
		if beadsapi.IsE2EAccountBead(b) {
			continue
		}
		env := c.accountEnv(b)
		cost := parseFloat(b.Fields["total_cost_usd"])
		if key := b.Fields["litellm_key"]; key != "" && costKeyOwner[costOwnerKey{key: key, env: env}] != name {
			// A sibling account bead owns this shared key's cost; emit a 0 series
			// here so the shared per-key bill is counted once, not N times.
			cost = 0
		}
		c.accountCostUSD.WithLabelValues(name, env).Set(cost)
		c.accountSessions.WithLabelValues(name, env).Set(parseFloat(b.Fields["session_count"]))
		c.accountInputTokens.WithLabelValues(name, env).Set(parseFloat(b.Fields["total_input_tokens"]))
		c.accountOutputTokens.WithLabelValues(name, env).Set(parseFloat(b.Fields["total_output_tokens"]))
		c.accountCacheRead.WithLabelValues(name, env).Set(parseFloat(b.Fields["total_cache_read_tokens"]))
		c.accountCacheWrite.WithLabelValues(name, env).Set(parseFloat(b.Fields["total_cache_write_tokens"]))
		c.accountRequests.WithLabelValues(name, env).Set(parseFloat(b.Fields["total_request_count"]))
		c.accountUtilization5h.WithLabelValues(name, env).Set(parseFloat(b.Fields["utilization_5h"]))
		c.accountUtilization7d.WithLabelValues(name, env).Set(parseFloat(b.Fields["utilization_7d"]))
		c.accountThrottled.WithLabelValues(name, env).Set(boolToFloat(b.Fields["throttle_status"] == "rejected"))
		c.accountWarming.WithLabelValues(name, env).Set(boolToFloat(b.Fields["soft_failover_state"] == "warming"))
	}
}

// costOwnerKey identifies a shared-cost scope: a LiteLLM virtual key within one
// env. spendSync stamps the same per-key /key/info total onto every account bead
// carrying that key, so the key+env pair is the unit whose cost must be booked
// exactly once (ka-0000001340).
type costOwnerKey struct {
	key string
	env string
}

// pickCostOwners chooses, for each (litellm_key, env), the single account bead
// whose total_cost_usd series carries the shared per-key bill. Every other bead on
// the same key emits a 0 cost series so the shared bill is counted once in
// sum(max by(account,env)(gardenio_account_cost_usd)) rather than once per bead.
//
// The owner is the lexicographically-first account TITLE for the key+env, so the
// choice is deterministic and stable across scrapes (no flapping owner as the bead
// list order changes). Accounts with an empty litellm_key are not deduped — each
// keeps its own cost — so single-key accounts are unaffected. Returns a map from
// costOwnerKey to the owning account title.
//
// E2E test accounts are excluded from the election (ka-0000005167). They are
// skipped in the emit loop, so electing one as a key's cost owner would silently
// ZERO the real sibling account on that key: the e2e owner emits nothing and every
// real bead on the key sees `costKeyOwner[...] != name` and emits cost = 0, losing
// that key's whole bill. Excluding them here keeps the owner a bead that actually
// emits. (No e2e account shares a key with a real one today — all carry unique
// sk-test-key-* values — so this is hardening against a latent trap, not a live
// regression.)
func (c *Collector) pickCostOwners(beads []*beadsapi.BeadDetail) map[costOwnerKey]string {
	owners := make(map[costOwnerKey]string)
	for _, b := range beads {
		name := b.Title
		if name == "" {
			continue
		}
		if beadsapi.IsE2EAccountBead(b) {
			continue
		}
		key := b.Fields["litellm_key"]
		if key == "" {
			continue
		}
		ck := costOwnerKey{key: key, env: c.accountEnv(b)}
		if cur, ok := owners[ck]; !ok || name < cur {
			owners[ck] = name
		}
	}
	return owners
}

// accountEnv resolves the `env` label value for an account gauge series.
// Precedence: the account bead's explicit `env` field (set by the
// gardenio-next config mirror or an operator to disambiguate a shared
// account), then this controller's GARDENIO_ENV (the env every account it
// scrapes belongs to), then "unknown". The `env` label is what keeps the
// same account name from aliasing across prod / gardenio-next / e2e
// on cross-environment dashboards (E2.2).
func (c *Collector) accountEnv(b *beadsapi.BeadDetail) string {
	if v := b.Fields["env"]; v != "" {
		return v
	}
	if c.defaultEnv != "" {
		return c.defaultEnv
	}
	return "unknown"
}

// collectResourceMapping emits the gardenio-owned project → {ECS, RDS}
// resource mapping that is the primary cross-estate FSM join key
// (gardenio/docs/FSM_CORRELATION_CONTRACT.md §2). It reads each managed
// project's declared resource_mapping entries and emits one presence series
// (value 1) per (project, env, cluster, service, db_instance) tuple, so a
// Grafana variable can resolve an agent project to the ACME resource IDs it
// exercises. Rows with an empty env are skipped: env is a join key and an
// unlabeled row cannot be correlated against the ACME estate.
func (c *Collector) collectResourceMapping(ctx context.Context) {
	projects, err := c.querier.ListProjectBeads(ctx)
	if err != nil {
		c.logger.Warn("failed to list project beads for resource mapping", "error", err)
		c.scrapeErrors.Inc()
		return
	}

	// Reset so retired projects / mapping rows disappear on the next scrape.
	c.resourceMapping.Reset()

	for name, p := range projects {
		for _, m := range p.ResourceMapping {
			if m.Env == "" {
				c.logger.Warn("skipping resource_mapping row with empty env",
					"project", name, "cluster", m.Cluster, "service", m.Service, "db_instance", m.DBInstance)
				continue
			}
			c.resourceMapping.WithLabelValues(
				name, m.Env, m.Cluster, m.Service, m.DBInstance,
			).Set(1)
		}
	}
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
