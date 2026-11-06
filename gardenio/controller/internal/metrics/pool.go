package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// PoolMetrics holds the warm-placeholder pool churn counters (ka-0000002975).
// The headline series is warm_placeholders_cycled_total: the count of
// not-in-use (Ready/unclaimed) warm placeholders the pool recycled because they
// were minted from an OLD agent image and a controller upgrade has since rolled
// a new one (epic ka-0000002868, the deployment-side counterpart to the
// squawk-from-prewarmed bug ka-0000006843). A spike on this series is the visible
// signature of an image roll draining the stale pool; a non-zero steady-state
// would mean the pool is churning when it should be quiescent — a regression
// signal.
type PoolMetrics struct {
	// env is baked in at construction (GARDENIO_ENV), the cross-estate join key
	// every gardenio series carries.
	env string

	cycledStale          *prometheus.CounterVec
	prewarmedCycledStale *prometheus.CounterVec

	// warmClaims counts warm-B placeholder claim attempts on the thaw wake
	// path by outcome (hit/miss/error). Before this counter only stale-image
	// recycling was visible — whether the pool actually SERVES thaws (its
	// entire reason to exist) was unmeasured (plan ka-0000003747 step C8).
	warmClaims *prometheus.CounterVec

	// prewarmedAssigns counts prewarmed-pool assignment attempts
	// (AssignPrewarmed, the Slack-thread fast path) by outcome
	// (hit/empty/error). The pool-empty rate is the direct "how often does a
	// mention pay a cold mint" signal.
	prewarmedAssigns *prometheus.CounterVec

	// prewarmedAssignDuration is the wall-clock a single AssignPrewarmed call
	// spends turning a warm pool member into an assigned agent — the WARM path's
	// answer to gardenio_microvm_launch_duration_seconds (the cold path). It is
	// split by execution_target so a lambda_microvm project's warm-assign latency
	// (the ~1-2s bead-flip the MicroVM warm pool exists to deliver, epic
	// ka-0000004145) is a first-class series next to the ~27s cold launch, and a
	// warm-vs-cold graph is a direct histogram_quantile over the two metrics.
	// Recorded on EVERY outcome (hit/empty/error) so a slow-then-empty assign
	// (a large pool scan that finds nothing) is still measured. The _count
	// sub-series by outcome doubles as a backend-split pool HIT-RATE
	// (hit/(hit+empty)), so the existing prewarmedAssigns counter — already
	// wired into the resume-latency dashboard — is left undisturbed.
	//
	// NOTE the pool's live DEPTH / standby-VM count is a separate,
	// scrape-derived gauge (gardenio_prewarmed_pool_depth) that lives on the
	// Collector next to agents_active — it is Set from the periodic agent-bead
	// scrape, not from a pool event, so it does not belong on this event-metric
	// struct.
	prewarmedAssignDuration *prometheus.HistogramVec
}

// Pool claim/assign outcome enum — the only values recorders accept, so a
// caller cannot inflate label cardinality.
const (
	// PoolOutcomeHit: a warm pod was claimed/assigned.
	PoolOutcomeHit = "hit"
	// PoolOutcomeMiss: the warm-B pool had no Ready placeholder for the
	// (node, topology) bucket — the thaw fell back to create-then-restore.
	PoolOutcomeMiss = "miss"
	// PoolOutcomeEmpty: the prewarmed pool had no assignable candidate for
	// the (project, role) — the caller falls back to a cold mint.
	PoolOutcomeEmpty = "empty"
	// PoolOutcomeError: the claim/assign failed for a reason other than an
	// empty pool (list/update error) — operationally distinct from a miss.
	PoolOutcomeError = "error"
)

func poolClaimOutcomeValid(o string) bool {
	switch o {
	case PoolOutcomeHit, PoolOutcomeMiss, PoolOutcomeError:
		return true
	default:
		return false
	}
}

func prewarmedAssignOutcomeValid(o string) bool {
	switch o {
	case PoolOutcomeHit, PoolOutcomeEmpty, PoolOutcomeError:
		return true
	default:
		return false
	}
}

// NewPoolMetrics constructs the warm-placeholder pool churn metrics. env is
// attached to every series as the cross-estate join key. Register() must be
// called before the metric is served.
func NewPoolMetrics(env string) *PoolMetrics {
	return &PoolMetrics{
		env: env,
		cycledStale: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "warm_placeholders_cycled_total",
			Help:      "Count of not-in-use (Ready/unclaimed) warm-B placeholders cycled because their stamped source image differs from the controller's currently-configured agent image — i.e. idle warm pods minted before a controller upgrade, recycled so a claimed+promoted thaw never grafts onto an old-image sandbox (epic ka-0000002868). Labels {node,topology,env}. A spike marks an image roll draining the stale pool; non-zero steady-state means unexpected pool churn.",
		}, []string{"node", "topology", "env"}),
		prewarmedCycledStale: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "prewarmed_agents_cycled_total",
			Help:      "Count of unassigned (prewarmed/idle, no thread, no task) prewarmed agent pods recycled because their stamped source image differs from the controller's currently-configured agent image for that (project,role) pool — i.e. warm pods minted before a release, recycled so a thread/adhoc task never lands on an old-image pod (ka-0000001689, the regular-pool counterpart to warm_placeholders_cycled_total). Labels {project,role,env}. A spike marks a release draining the stale pool; non-zero steady-state means unexpected pool churn.",
		}, []string{"project", "role", "env"}),
		warmClaims: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "warm_placeholder_claims_total",
			Help:      "Warm-B placeholder claim attempts on the thaw wake path by outcome. hit = a Ready placeholder was atomically promoted and the thaw grafted onto it (the sub-second R4 path); miss = no Ready placeholder in the (node,topology) bucket, thaw fell back to create-then-restore (R3); error = the claim itself failed (list/update error), also a fallback. Labels {outcome,node,topology,env}. hit/(hit+miss) is the pool's serve rate — the pool's entire purpose, previously unmeasured (ka-0000000550).",
		}, []string{"outcome", "node", "topology", "env"}),
		prewarmedAssigns: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "prewarmed_assigns_total",
			Help:      "Prewarmed-pool assignment attempts (AssignPrewarmed, the Slack-thread warm fast path) by outcome. hit = a warm pod was atomically assigned; empty = no assignable candidate for the (project,role), caller pays a cold mint; error = list/update failure. Labels {outcome,project,role,env}. The empty rate is the direct how-often-a-mention-cold-starts signal (ka-0000000550).",
		}, []string{"outcome", "project", "role", "env"}),
		prewarmedAssignDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "prewarmed_assign_duration_seconds",
			Help:      "Warm-assign latency: seconds a single AssignPrewarmed call spends turning a warm pool member into an assigned agent (bead scan + atomic agent_state flip). The WARM-path counterpart to gardenio_microvm_launch_duration_seconds (the ~27s cold launch). Labels {outcome,project,role,execution_target,env}: execution_target=lambda_microvm isolates the MicroVM warm pool's ~1-2s assign so a warm-vs-cold graph is one histogram_quantile over the two metrics (epic ka-0000004145). Recorded on every outcome; the _count by outcome is a backend-split pool hit-rate.",
			// 5ms..2s: dense in the sub-second bead-flip regime the warm assign
			// lives in, with a coarse tail to catch a slow daemon list/update.
			Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2},
		}, []string{"outcome", "project", "role", "execution_target", "env"}),
	}
}

// Register registers the pool churn metrics with reg. Served alongside the other
// controller metrics on the :9092 /metrics endpoint. A nil receiver is a no-op.
func (m *PoolMetrics) Register(reg prometheus.Registerer) error {
	if m == nil {
		return nil
	}
	for _, col := range []prometheus.Collector{
		m.cycledStale,
		m.prewarmedCycledStale,
		m.warmClaims,
		m.prewarmedAssigns,
		m.prewarmedAssignDuration,
	} {
		if err := reg.Register(col); err != nil {
			return err
		}
	}
	return nil
}

// RecordWarmPlaceholderCycled records that count not-in-use stale-image
// placeholders were cycled in the (node, topology) bucket. A non-positive count
// or a nil receiver is a no-op. An empty topology (the legacy blind bucket)
// collapses to "legacy" so the label is never empty. It satisfies
// poolmanager.CycleRecorder.
func (m *PoolMetrics) RecordWarmPlaceholderCycled(node, topology string, count int) {
	if m == nil || count <= 0 {
		return
	}
	if topology == "" {
		topology = "legacy"
	}
	m.cycledStale.WithLabelValues(node, topology, m.env).Add(float64(count))
}

// RecordPrewarmedAgentCycled records that count unassigned stale-image prewarmed
// agents were cycled in the (project, role) pool. A non-positive count or a nil
// receiver is a no-op. It satisfies poolmanager.PrewarmedCycleRecorder.
func (m *PoolMetrics) RecordPrewarmedAgentCycled(project, role string, count int) {
	if m == nil || count <= 0 {
		return
	}
	m.prewarmedCycledStale.WithLabelValues(project, role, m.env).Add(float64(count))
}

// RecordWarmPlaceholderClaim records one warm-B claim attempt on the thaw wake
// path. outcome must be PoolOutcomeHit / PoolOutcomeMiss / PoolOutcomeError
// (anything else is dropped, returning false, so cardinality stays bounded).
// An empty topology (the legacy blind bucket) collapses to "legacy", mirroring
// RecordWarmPlaceholderCycled. Nil receiver is a no-op. It satisfies
// poolmanager.ClaimRecorder.
func (m *PoolMetrics) RecordWarmPlaceholderClaim(outcome, node, topology string) bool {
	if m == nil {
		return false
	}
	if !poolClaimOutcomeValid(outcome) {
		return false
	}
	if topology == "" {
		topology = "legacy"
	}
	m.warmClaims.WithLabelValues(outcome, node, topology, m.env).Inc()
	return true
}

// RecordPrewarmedAssign records one prewarmed-pool assignment attempt. outcome
// must be PoolOutcomeHit / PoolOutcomeEmpty / PoolOutcomeError (anything else
// is dropped, returning false). Nil receiver is a no-op. It satisfies
// poolmanager.AssignRecorder.
func (m *PoolMetrics) RecordPrewarmedAssign(outcome, project, role string) bool {
	if m == nil {
		return false
	}
	if !prewarmedAssignOutcomeValid(outcome) {
		return false
	}
	m.prewarmedAssigns.WithLabelValues(outcome, project, role, m.env).Inc()
	return true
}

// RecordPrewarmedAssignDuration observes one warm-assign in seconds, labeled by
// the same {outcome,project,role} the counter uses plus execution_target so the
// MicroVM warm pool's assign latency is a distinct series from the K8s pool's.
// outcome must be a valid prewarmedAssign outcome (hit/empty/error) — anything
// else is DROPPED (returns false) so a bad label never poisons a percentile.
// A negative duration (clock skew) is dropped. Empty project/role collapse to
// "unknown"; an empty execution_target collapses to "kubernetes" (the default
// backend, matching effectiveExecutionTarget's own default) so the label is
// never blank. Nil receiver is a no-op. Recorded on the SAME outcomes as
// RecordPrewarmedAssign so the two stay in lockstep.
func (m *PoolMetrics) RecordPrewarmedAssignDuration(seconds float64, outcome, project, role, executionTarget string) bool {
	if m == nil {
		return false
	}
	if seconds < 0 {
		return false
	}
	if !prewarmedAssignOutcomeValid(outcome) {
		return false
	}
	if project == "" {
		project = "unknown"
	}
	if role == "" {
		role = "unknown"
	}
	if executionTarget == "" {
		executionTarget = "kubernetes"
	}
	m.prewarmedAssignDuration.WithLabelValues(outcome, project, role, executionTarget, m.env).Observe(seconds)
	return true
}
