// Package reconciler diffs desired agent bead state (from daemon) against
// actual K8s pod state and creates/deletes pods to converge.
package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/lands/wall"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/tracing"
)

// tracer instruments the reconcile loop. It resolves to the global no-op
// provider until tracing.Init installs the OTLP exporter, so this is free when
// GARDENIO_OTEL_TRACES_ENABLED is unset.
var tracer = tracing.Tracer("gardenio/controller/reconciler")

// BeadCloser closes a bead by ID with optional fields. Used by the reconciler
// to auto-close agent beads that have been deferred too long.
type BeadCloser interface {
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// beadGetter is an optional surface co-implemented with BeadCloser used to
// also close orphaned mirror task beads when their assigned agent expires in
// the coop queue. *beadsapi.Client satisfies both; tests that stub only
// BeadCloser are unaffected (the mirror-close path is a no-op).
type beadGetter interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// StateUpdater updates agent state on a bead. Used by the reconciler to mark
// deferred agents as "queued" so operators can see why a pod hasn't been created.
type StateUpdater interface {
	UpdateAgentState(ctx context.Context, beadID, state string) error
}

// HarnessStamper writes the controller-resolved harness onto an agent bead
// (ADR 0004 section 4) so downstream consumers read it instead of re-resolving.
// *beadsapi.Client satisfies it.
type HarnessStamper interface {
	StampResolvedHarness(ctx context.Context, beadID, harness string) error
}

// SpawnTaskClaimer makes an agent the assignee of the task its bead names, on
// the controller's machine principal, when nobody live holds it
// (gd-Yo28zKgkGx). *beadsapi.Client satisfies it.
type SpawnTaskClaimer interface {
	ClaimSpawnTask(ctx context.Context, agentBeadID string) (string, error)
}

// FailedAgentMarker marks an agent bead as permanently failed. Used by the
// reconciler to hard-fail agent beads on permanent K8s validation errors
// (e.g. DNS-1123 pod name violations) so they do not silently retry forever
// and so the bridge surfaces a crash notification to the operator.
type FailedAgentMarker interface {
	MarkAgentFailed(ctx context.Context, beadID, reason string) error
}

// SnapshotChecker reports whether a persisted freeze/thaw snapshot exists for
// an agent. The reconciler's leaked-bead reaper consults it to exempt agents
// that are frozen-awaiting-thaw: such an agent legitimately has a terminal
// agent_state ("done") AND no pod (Pod A was checkpointed and torn down) but
// has a snapshot on disk, so closing its bead would race and cold-start over
// the in-flight thaw (bug ka-0000000114, parent epic ka-0000007417).
//
// Satisfied by *podmanager.FreezeThawManager.HasSnapshot. Only wired when the
// freeze/thaw feature flag (cfg.FreezeThawEnabled) is on; when nil the reaper
// behaves exactly as before.
type SnapshotChecker func(ctx context.Context, agentName string) bool

// ThawChecker reports whether a thaw for an agent is currently in flight — from
// the top of FreezeThawManager.Thaw through its bounded rehydration hand-off.
// The reconciler's terminal-pod reaper consults it to exempt the restored
// placeholder Pod B while it is still rehydrating: Pod B reports phase=Failed
// within seconds of the restore graft, and reaping it inside the rehydration
// window tears down the kubelet-owned netns out from under the in-pod gb+coop
// watchers, so both rehydration acks fail ENETUNREACH and the thaw fail-closes
// (bug ka-0000002058, decision ka-0000005300: exempt_reaper). The thaw lifecycle
// already owns Pod B teardown on both the success and fail-closed paths.
//
// Satisfied by *podmanager.FreezeThawManager.IsThawing. Only wired when the
// freeze/thaw feature flag (cfg.FreezeThawEnabled) is on; when nil the reaper
// behaves exactly as before.
type ThawChecker func(agentName string) bool

// Tunables is the narrow accessor the reconciler uses to read runtime-tunable
// values from a config-bead backed source. Satisfied by *beadsapi.ConfigWatcher
// and by test stubs. When the field is nil, the compiled default is returned.
//
// Introduced for the pilot migration of COOP_QUEUED_WARN_AGE
// (ka-0000003317 / epic ka-0000006607): the accessor is consulted on every
// reconcile pass, which is what lets a config-bead update take effect
// without a pod restart.
type Tunables interface {
	GetDuration(key, envVar string, compiled time.Duration) time.Duration
}

// defaultQueuedWarnAge is the compiled default threshold for warning about
// stale deferred agents. Mirrors the old COOP_QUEUED_WARN_AGE default.
const defaultQueuedWarnAge = time.Hour

// ReconcileRecorder records the outcome of each reconcile pass as a Prometheus
// counter (gardenio_reconcile_total{controller,result}). Satisfied by
// *metrics.Collector via its RecordReconcile method. Defined here (rather than
// importing the metrics package) to keep the reconciler decoupled from the
// metrics registry and trivially fakeable in tests. When unset, reconcile
// outcomes are simply not counted.
type ReconcileRecorder interface {
	RecordReconcile(controller string, ok bool)
}

// LifecycleRecorder records agent lifecycle transitions the reconciler
// witnesses (gardenio_agent_lifecycle_events_total — here the "drained" event,
// ka-0000000550). Satisfied by *metrics.EventMetrics; defined locally for the
// same decoupling reason as ReconcileRecorder. When unset, drain episodes are
// simply not counted.
//
// RecordAccountLayer counts which applyAccountConfig layer produced a pod's
// account (gardenio_agent_account_layer_total, Phase 0 instrumentation
// ka-0000002142). The reconciler is the create-missing backstop for beads the
// event door missed, so it records the layer at its own create site to match
// the event-door count. layer is the enum stamped on spec.AccountLayer.
type LifecycleRecorder interface {
	RecordLifecycleEvent(event, project, role, sessionID string) bool
	RecordAccountLayer(layer, project, role string) bool
}

// reconcilerControllerName is the value of the `controller` label on
// gardenio_reconcile_total emitted by this reconciler. It names the agent-pod
// reconcile loop so SLO S2 (controller reconcile success rate) can be
// attributed per controller as more reconcilers are instrumented (ka-0000001213).
const reconcilerControllerName = "agent"

// SpecBuilder constructs an AgentPodSpec from config, bead identity, and metadata.
// The metadata map may contain per-bead overrides (e.g., image).
type SpecBuilder func(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec

// MicrovmReconciler is the C5 lifecycle seam the reconciler consults for
// lambda_microvm agents (epic ka-0000005010). A MicroVM agent legitimately has
// NO pod, so it must be exempted from the podless-bead reaper and the
// create-missing path, and reconciled against its runtime status instead. The
// reconciler passes a backend-neutral bead view; the coordinator returns
// whether the bead was a MicroVM agent and, if so, the reconcile outcome.
//
// Satisfied by *microvm.Lifecycle. Only wired when the MicroVM backend is
// available; when nil the reconciler behaves exactly as before (every agent is
// a Kubernetes agent).
type MicrovmReconciler interface {
	// IsMicrovmAgent reports whether the bead is a lambda_microvm agent this
	// coordinator owns (based on the resolved execution_target). False for every
	// bead when the backend is unavailable.
	IsMicrovmAgent(view microvm.AgentBeadView) bool
	// Reconcile converges a MicroVM agent bead against its runtime: it closes a
	// bead whose runtime is gone/terminal and leaves a live one open. The error
	// is non-nil only on a transient status read; the reconciler then leaves the
	// bead untouched this pass (never reaps on an inconclusive read).
	Reconcile(ctx context.Context, view microvm.AgentBeadView) (microvm.ReconcileOutcome, error)
}

// GitCredentialApplier is the pre-create seam for per-agent git credentials
// (GitHub App installation tokens, epic gd-BHLDnjX1Hw). The event door applies
// it before CreateAgentPod; the reconciler is a second, independent pod-creation
// door (spot-loss recreates, beads created during controller
// downtime, and retries of spawns the event door refused), so it must run the
// SAME hook or those pods would be created with the project's shared credential.
//
// Apply may rewrite the spec's git credential wiring. refuse=true means the
// pod must NOT be created (fail closed: never fall back to the shared
// credential); reason is non-secret. Release undoes Apply when CreateAgentPod
// then fails. A nil applier (the default) leaves the spec untouched.
type GitCredentialApplier struct {
	Apply   func(ctx context.Context, spec *podmanager.AgentPodSpec, beadID string) (refuse bool, reason string)
	Release func(ctx context.Context, spec podmanager.AgentPodSpec, beadID string)
}

// SpawnWall is the pre-create seam for the Lands spawn wall
// (internal/lands/wall, Track B spec §3.5, step 8.3), the twin of the event
// door's check in cmd/controller. Check returns the wall's verdict for a spawn
// of role into a project (the land's steward passes with tenant spawns off,
// owner ruling gd-KOAoUELMWo); Record writes a refusal onto the agent bead (idempotent: a bead
// already refused with the same code is left alone). The reconciler asks only
// for a new spawn (wall.NewSpawn): recreating a running agent's pod after a
// spot loss is not one. A nil Check (the default) admits
// everything.
//
// Halted is asked instead when the pod of an agent that already runs is
// recreated: the land's kill switches alone (wall.Halted: frozen, gone, or
// every land paused), memoised per project for the pass. A running agent
// continues past the purse wall, but not past a freeze (gd-Nc7yMpW7U6). A
// refusal is recorded like Check's, so the bead waits at budget_exceeded and
// is decided as a new spawn once the land is clear. A nil Halted admits.
//
// Rate is the per-land spawn rate (step 10.4), asked per bead after Check
// passed a tenant land (Refusal.Tenant): unlike Check it is never memoised,
// since each agent takes its own slot. A nil Rate admits.
type SpawnWall struct {
	Check  func(ctx context.Context, project, role string) wall.Refusal
	Halted func(ctx context.Context, project string) wall.Refusal
	Rate   func(ctx context.Context, bead beadsapi.AgentBead) wall.Refusal
	Record func(ctx context.Context, bead beadsapi.AgentBead, r wall.Refusal)
}

// wallKey memoises a SpawnWall.Check verdict for one pass. A Halted verdict
// is memoised under the project with an empty role and halted set.
type wallKey struct {
	project, role string
	halted        bool
}

// AgentTokenApplier is the pre-create seam for per-agent beads tokens
// (PER_AGENT_TOKENS). Like GitCredentialApplier, the reconciler is a second
// pod-creation door next to the event door, and a pod it builds from
// applyCommonConfig carries the SHARED beads token secret. Without this hook a
// spot-loss recreate, an image-update recreate, or a spawn made while the
// controller was down would silently put the primary token back on an agent
// pod that the flag exists to keep it off.
//
// Apply mints (or refreshes) the agent's token and points
// spec.DaemonTokenSecret at it. minted reports that this call created the
// token Secret, so Release (called when the pod is not created after all)
// revokes only what this attempt made. A non-nil err means the pod must NOT be
// created (fail closed; never fall back to the shared token). A nil applier
// (the default) leaves the spec untouched.
type AgentTokenApplier struct {
	Apply   func(ctx context.Context, spec *podmanager.AgentPodSpec, beadID, agentName string) (minted bool, err error)
	Release func(ctx context.Context, spec podmanager.AgentPodSpec, beadID string)
}

// desiredEntry pairs a pod name with its agent bead for sorted iteration.
type desiredEntry struct {
	name string
	bead beadsapi.AgentBead
}

// Reconciler diffs desired state (agent beads) against actual state (K8s pods)
// and creates/deletes pods to converge.
type Reconciler struct {
	lister             beadsapi.BeadLister
	pods               podmanager.Manager
	cfg                *config.Config
	logger             *slog.Logger
	specBuilder        SpecBuilder
	gitCreds           GitCredentialApplier
	spawnWall          SpawnWall
	agentTokens        AgentTokenApplier
	mu                 sync.Mutex // prevent concurrent reconciles
	upgradeTracker     *UpgradeTracker
	destructed         atomic.Bool
	rateLimiter        *CreationRateLimiter
	circuitBreaker     *CircuitBreaker
	deferredMetrics    *DeferredMetrics
	idleMetrics        *IdleMetrics         // Phase 0 idle:active ratio + idle memory waste (ka-0000006778)
	beadCloser         BeadCloser           // optional; auto-closes expired queued beads
	deferredSince      map[string]time.Time // bead ID → first deferred time
	stateUpdater       StateUpdater         // optional; marks deferred agents as "queued"
	failedMarker       FailedAgentMarker    // optional; hard-fails beads on permanent K8s errors
	harnessStamper     HarnessStamper       // optional; records the resolved harness on the bead at spawn
	taskClaimer        SpawnTaskClaimer     // optional; claims the agent's task at spawn when the spawner could not (gd-Yo28zKgkGx)
	emptyDesiredCycles int                  // consecutive reconcile cycles with zero desired beads
	massOrphanCycles   int                  // consecutive cycles where most of the fleet looked orphaned (partial daemon read guard)
	tunables           Tunables             // optional; if nil, compiled defaults are used
	hasSnapshot        SnapshotChecker      // optional; exempts frozen-awaiting-thaw agents from the leaked-bead reaper
	isThawing          ThawChecker          // optional; exempts the in-flight thaw placeholder Pod B from the terminal-pod reaper (ka-0000002058)
	microvm            MicrovmReconciler    // optional; reconciles lambda_microvm agents by runtime status instead of pod presence (C5, ka-0000001837)
	reconcileRecorder  ReconcileRecorder    // optional; counts reconcile pass outcomes as gardenio_reconcile_total (ka-0000001213)
	lifecycleRecorder  LifecycleRecorder    // optional; counts drain episodes as gardenio_agent_lifecycle_events_total{event="drained"} (ka-0000000550)
	// extraNamespaces are additional namespaces swept for actual agent pods
	// beyond cfg.Namespace. Used for the PSA-privileged gVisor namespace where
	// runsc freeze/thaw agents run (ka-0000008076): without this a runsc agent
	// in a separate namespace is invisible to desired-vs-actual reconcile, so
	// it is never reaped as an orphan nor recreated when it fails.
	extraNamespaces []string
	// namespaceSource, when set, returns more namespaces to sweep, re-read
	// every pass: the tenant land namespaces (land-<land>), which come and go
	// with the lands. Their pods then count toward CoopMaxPods and the land's
	// per-project cap, and are reconciled like any other (gd-NDJIOutLsv).
	namespaceSource func() []string
}

// New creates a Reconciler.
func New(
	lister beadsapi.BeadLister,
	pods podmanager.Manager,
	cfg *config.Config,
	logger *slog.Logger,
	specBuilder SpecBuilder,
) *Reconciler {
	return &Reconciler{
		lister:          lister,
		pods:            pods,
		cfg:             cfg,
		logger:          logger,
		specBuilder:     specBuilder,
		upgradeTracker:  NewUpgradeTracker(logger),
		rateLimiter:     NewCreationRateLimiter(cfg.CoopRateLimitMax, cfg.CoopRateLimitWindow),
		circuitBreaker:  NewCircuitBreaker(cfg.CoopCircuitBreakerThreshold, cfg.CoopCircuitBreakerWindow, cfg.CoopCircuitBreakerCooldown),
		deferredMetrics: NewDeferredMetrics(),
		idleMetrics:     NewIdleMetrics(),
	}
}

// SetGitCredentialApplier wires the pre-create git credential hook (see
// GitCredentialApplier). Only set when per-agent GitHub App tokens are enabled.
func (r *Reconciler) SetGitCredentialApplier(a GitCredentialApplier) {
	r.gitCreds = a
}

// SetSpawnWall wires the Lands spawn wall (see SpawnWall).
func (r *Reconciler) SetSpawnWall(w SpawnWall) {
	r.spawnWall = w
}

// SetAgentTokenApplier wires the pre-create per-agent token hook (see
// AgentTokenApplier). Only set when PER_AGENT_TOKENS is enabled.
func (r *Reconciler) SetAgentTokenApplier(a AgentTokenApplier) {
	r.agentTokens = a
}

// SetExtraNamespaces configures additional namespaces swept for actual agent
// pods beyond the primary cfg.Namespace. Empty or duplicate-of-primary entries
// are ignored. Used to include the PSA-privileged gVisor namespace where runsc
// freeze/thaw agents run (ka-0000008076), so they participate in
// desired-vs-actual orphan cleanup and failed-pod recreation.
func (r *Reconciler) SetExtraNamespaces(namespaces ...string) {
	for _, ns := range namespaces {
		if ns == "" || ns == r.cfg.Namespace {
			continue
		}
		r.extraNamespaces = append(r.extraNamespaces, ns)
	}
}

// SetNamespaceSource configures a function returning more namespaces to
// sweep for actual agent pods, re-read every pass (see namespaceSource). A
// namespace that is gone, or not yet bound to the controller, is skipped.
func (r *Reconciler) SetNamespaceSource(fn func() []string) {
	r.namespaceSource = fn
}

// SetBeadCloser configures the reconciler to auto-close agent beads that have
// been deferred longer than CoopQueueExpiryTimeout. Without this, deferred beads
// remain open indefinitely.
func (r *Reconciler) SetBeadCloser(c BeadCloser) {
	r.beadCloser = c
	r.deferredSince = make(map[string]time.Time)
}

// SetStateUpdater configures the reconciler to update bead agent_state when
// pods are deferred (e.g., due to max pod limits). Without this, deferred
// agents remain in "unknown" state instead of being marked "queued".
func (r *Reconciler) SetStateUpdater(u StateUpdater) {
	r.stateUpdater = u
}

// SetFailedAgentMarker configures the reconciler to hard-fail agent beads
// whose pod creation fails with a permanent K8s validation error (e.g. a pod
// name that violates DNS-1123). Without this, such beads would silently retry
// every reconcile pass and never surface a crash notification.
func (r *Reconciler) SetFailedAgentMarker(m FailedAgentMarker) {
	r.failedMarker = m
}

// SetHarnessStamper configures the reconciler to write the resolved harness
// onto the agent bead just before creating its pod, mirroring the event-door
// spawn path. Without it the create-missing backstop would spawn pods whose
// bead never carries resolved_harness.
func (r *Reconciler) SetHarnessStamper(s HarnessStamper) {
	r.harnessStamper = s
}

// SetSpawnTaskClaimer configures the reconciler to claim an agent's task for it
// just before creating its pod, mirroring the event-door spawn path, so a pod
// the create-missing backstop builds is not left on a task it cannot write.
func (r *Reconciler) SetSpawnTaskClaimer(c SpawnTaskClaimer) {
	r.taskClaimer = c
}

// SetSnapshotChecker installs a freeze/thaw snapshot-presence check so the
// leaked-bead reaper exempts frozen-awaiting-thaw agents (terminal agent_state
// + no pod, but a persisted snapshot). Without it the reaper behaves as before
// and may race an in-flight thaw (bug ka-0000000114). Only wired by the
// controller when cfg.FreezeThawEnabled is on.
func (r *Reconciler) SetSnapshotChecker(c SnapshotChecker) {
	r.hasSnapshot = c
}

// SetThawChecker installs an in-flight-thaw check so the terminal-pod reaper
// exempts the restored placeholder Pod B while its thaw is still rehydrating.
// Without it the reaper deletes Pod B (which goes phase=Failed seconds after the
// restore graft) inside the rehydration window, tearing down the netns under the
// in-pod watchers so the rehydration acks fail ENETUNREACH (bug ka-0000002058).
// Only wired by the controller when cfg.FreezeThawEnabled is on; nil preserves
// the prior behaviour.
func (r *Reconciler) SetThawChecker(c ThawChecker) {
	r.isThawing = c
}

// SetReconcileRecorder installs a recorder that counts each reconcile pass
// outcome as gardenio_reconcile_total{controller,result}. Wired by the
// controller to *metrics.Collector so the counter lands on the same registry
// vmagent scrapes. When unset, reconcile outcomes are not counted (ka-0000001213).
func (r *Reconciler) SetReconcileRecorder(rec ReconcileRecorder) {
	r.reconcileRecorder = rec
}

// SetLifecycleRecorder installs a recorder that counts agent drain episodes as
// gardenio_agent_lifecycle_events_total{event="drained"}. Wired by the
// controller to *metrics.EventMetrics. When unset, drains are not counted.
func (r *Reconciler) SetLifecycleRecorder(rec LifecycleRecorder) {
	r.lifecycleRecorder = rec
}

// SetMicrovmReconciler installs the C5 lambda_microvm lifecycle coordinator so
// MicroVM agents (which legitimately have no pod) are reconciled by runtime
// status instead of being reaped as podless leaks or cold-created as pods
// (epic ka-0000005010, ka-0000001837). Only wired by the controller when the
// MicroVM backend is available; nil preserves the prior all-Kubernetes behaviour.
func (r *Reconciler) SetMicrovmReconciler(m MicrovmReconciler) {
	r.microvm = m
}

// microvmView projects an agent bead into the backend-neutral view the C5
// lifecycle coordinator consumes. The runtime_* fields ride in Metadata (bead
// fields merged with notes in ListAgentBeads).
func microvmView(bead beadsapi.AgentBead) microvm.AgentBeadView {
	return microvm.AgentBeadView{
		ID:              bead.ID,
		AgentState:      bead.AgentState,
		ExecutionTarget: bead.Metadata["execution_target"],
		RuntimeID:       bead.Metadata["runtime_id"],
		RuntimeState:    bead.Metadata["runtime_state"],
		// AgentSecretARN is not persisted; Terminate falls back to the
		// deterministic per-run secret name (<prefix><bead>/agent-config).
	}
}

// reconcileMicrovmAgent runs the C5 lifecycle reconcile for a MicroVM agent and
// reports whether the reconciler should treat the bead as fully handled this
// pass (so it is skipped by the leaked-bead reaper and the create-missing
// path). It returns handled=true for every MicroVM agent — live ones are left
// open, terminal ones are closed by the coordinator, and a transient status
// error leaves the bead untouched (still handled: never reaped as a podless
// leak on an inconclusive read). handled=false only when the bead is not a
// MicroVM agent, so the ordinary Kubernetes path applies.
func (r *Reconciler) reconcileMicrovmAgent(ctx context.Context, bead beadsapi.AgentBead) (handled bool) {
	if r.microvm == nil {
		return false
	}
	view := microvmView(bead)
	if !r.microvm.IsMicrovmAgent(view) {
		return false
	}
	if _, err := r.microvm.Reconcile(ctx, view); err != nil {
		// Inconclusive runtime read: leave the bead exactly as-is this pass. It
		// is still "handled" — a MicroVM agent must never fall through to the
		// podless-leak reaper or the cold-start path.
		r.logger.Warn("microvm lifecycle reconcile failed; leaving bead untouched this pass",
			"bead", bead.ID, "agent", bead.AgentName, "error", err)
	}
	return true
}

// SetTunables installs a runtime-tunable provider (e.g. a ConfigWatcher).
// When set, values covered by the tunables source (currently only
// COOP_QUEUED_WARN_AGE) are read through it on every reconcile pass, so
// updating the backing config bead changes behavior without a restart.
func (r *Reconciler) SetTunables(t Tunables) {
	r.tunables = t
}

// queuedWarnAge returns the current effective threshold for warning about
// stale deferred agents. When no tunables source is attached, the compiled
// default is returned.
func (r *Reconciler) queuedWarnAge() time.Duration {
	if r.tunables == nil {
		return defaultQueuedWarnAge
	}
	return r.tunables.GetDuration("COOP_QUEUED_WARN_AGE", "COOP_QUEUED_WARN_AGE", defaultQueuedWarnAge)
}

// DeferredMetrics returns the deferred-agent Prometheus metrics for
// registration with a prometheus.Registerer.
func (r *Reconciler) DeferredMetrics() *DeferredMetrics {
	return r.deferredMetrics
}

// IdleMetrics returns the Phase 0 idle:active / idle-memory-waste Prometheus
// metrics (ka-0000006778) for registration with a prometheus.Registerer.
func (r *Reconciler) IdleMetrics() *IdleMetrics {
	return r.idleMetrics
}

// UpgradeTracker returns the drain/upgrade tracker so OTHER controller-initiated
// delete paths can route through the same graceful commit+push drain the
// node-drain path uses (ka-0000001482).
//
// Deliberately returns the reconciler's OWN tracker rather than letting callers
// construct their own: the drain map is keyed by pod name, so two independent
// trackers would each open a drain entry for the same pod and double-nudge it.
// One shared tracker means IsDraining dedups across every kill trigger.
func (r *Reconciler) UpgradeTracker() *UpgradeTracker {
	return r.upgradeTracker
}

// Autodestruct sets the autodestruct flag, force-deletes all agent pods, and
// returns the number of pods deleted. While the flag is set, Reconcile() is a
// no-op so deleted pods are not recreated.
func (r *Reconciler) Autodestruct(ctx context.Context, actor string) (int, error) {
	r.destructed.Store(true)
	r.logger.Warn("autodestruct activated", "actor", actor)
	// Sweep the primary namespace plus any extra namespaces (e.g. the gVisor
	// namespace, ka-0000008076) so runsc agents are torn down too. Errors are
	// aggregated; a failure in one namespace does not skip the others.
	var (
		total int
		errs  []error
	)
	for _, ns := range append([]string{r.cfg.Namespace}, r.extraNamespaces...) {
		deleted, err := r.pods.DeleteAllAgentPods(ctx, ns, true)
		total += deleted
		if err != nil {
			errs = append(errs, fmt.Errorf("namespace %s: %w", ns, err))
		}
	}
	return total, errors.Join(errs...)
}

// ClearAutodestruct clears the autodestruct flag, allowing Reconcile() to
// resume normal operation.
func (r *Reconciler) ClearAutodestruct(actor string) {
	r.destructed.Store(false)
	r.logger.Warn("autodestruct cleared", "actor", actor)
}

// IsDestructed returns true if autodestruct is active.
func (r *Reconciler) IsDestructed() bool {
	return r.destructed.Load()
}

// Reconcile performs a single reconciliation pass:
// 1. List desired beads from daemon
// 2. List actual pods from K8s
// 3. Create missing pods, delete orphan pods, recreate failed pods
//
// Each step lives in reconcile_steps.go; this function only orders them.
func (r *Reconciler) Reconcile(ctx context.Context) (err error) {
	ctx, span := tracer.Start(ctx, "reconciler.Reconcile")
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}()

	r.mu.Lock()
	defer r.mu.Unlock()

	// Count this reconcile pass on completion: result=ok when err is nil
	// (including the autodestruct / circuit-breaker skip no-ops, which are
	// successful completions), result=error when a non-nil error is returned.
	// Feeds gardenio_reconcile_total for SLO S2 (ka-0000001213).
	if r.reconcileRecorder != nil {
		defer func() {
			r.reconcileRecorder.RecordReconcile(reconcilerControllerName, err == nil)
		}()
	}

	if r.destructed.Load() {
		r.logger.Info("autodestruct active, skipping reconciliation")
		return nil
	}

	if r.circuitBreaker != nil && r.circuitBreaker.IsOpen() {
		r.logger.Warn("circuit breaker open, skipping pod creation this pass")
		return nil
	}

	// Get desired state from daemon.
	beads, err := r.lister.ListAgentBeads(ctx)
	if err != nil {
		// Fail-safe: if we can't reach the daemon, do NOT delete any pods.
		return fmt.Errorf("listing agent beads: %w", err)
	}
	desired := desiredPods(beads)

	actualMap, err := r.listActualPods(ctx)
	if err != nil {
		return err
	}

	span.SetAttributes(
		attribute.Int("gardenio.reconcile.desired_pods", len(desired)),
		attribute.Int("gardenio.reconcile.actual_pods", len(actualMap)),
	)

	if err := r.deleteOrphanPods(ctx, desired, actualMap); err != nil {
		return err
	}

	p := newReconcilePass(r.cfg, desired, actualMap)

	// Phase 0 measurement (ka-0000006778): emit the idle:active agent ratio and
	// the memory reserved by idle agents from the desired/actual maps just built.
	// Placed after orphan cleanup so actualMap holds only pods the reconciler is
	// keeping; a pure Reset+Set over in-hand data, no extra queries.
	r.recordIdleMetrics(desired, actualMap)

	p.microvmHandled = r.reconcileMicrovmAgents(ctx, desired)
	p.closedThisPass = r.closeLeakedBeads(ctx, desired, actualMap, p.microvmHandled)

	// Reset deferred metrics at the start of each pass so the gauge
	// reflects only this pass's deferred agents.
	r.deferredMetrics.DeferredTotal.Reset()

	r.cleanStaleDrains(actualMap)

	// Create missing pods and recreate failed pods, oldest bead
	// first. Respect CoopBurstLimit (max pods created per pass) and
	// CoopMaxPods (total active pod cap).
	for _, entry := range sortedByCreation(desired) {
		stop, err := r.reconcileDesired(ctx, p, entry)
		if err != nil {
			return err
		}
		if stop {
			break
		}
	}

	// Warn about deferred agents that have been waiting too long.
	r.warnStaleDeferrals(p.deferred)

	r.logPassSummary(p)

	// Track deferred beads and expire those that have waited too long.
	r.expireDeferredBeads(ctx, p.deferredThisPass)

	return nil
}

// recordDeferred increments the deferred gauge and observes wait time for a
// deferred agent bead.
func (r *Reconciler) recordDeferred(bead beadsapi.AgentBead, reason string) {
	project := bead.Project
	if project == "" {
		project = "unknown"
	}
	r.deferredMetrics.DeferredTotal.WithLabelValues(project, reason).Inc()
	if !bead.CreatedAt.IsZero() {
		wait := time.Since(bead.CreatedAt).Seconds()
		r.deferredMetrics.DeferredWaitSeconds.WithLabelValues(project).Observe(wait)
	}
}

// warnStaleDeferrals logs warnings for deferred agents whose bead creation
// time exceeds the current queued-warn-age threshold. The threshold is read
// from the attached Tunables source on every call so updates to the backing
// config bead take effect without a restart.
func (r *Reconciler) warnStaleDeferrals(deferred []desiredEntry) {
	threshold := r.queuedWarnAge()
	if threshold <= 0 || len(deferred) == 0 {
		return
	}
	now := time.Now()
	staleCount := 0
	for _, entry := range deferred {
		if entry.bead.CreatedAt.IsZero() {
			continue
		}
		age := now.Sub(entry.bead.CreatedAt)
		if age >= threshold {
			staleCount++
			r.logger.Warn("deferred agent exceeds queue age threshold",
				"pod", entry.name,
				"bead_id", entry.bead.ID,
				"project", entry.bead.Project,
				"age", age.Truncate(time.Second),
				"threshold", threshold,
			)
		}
	}
	if staleCount > 0 {
		r.logger.Warn("stale deferred agents detected",
			"stale_count", staleCount,
			"total_deferred", len(deferred),
			"threshold", threshold,
		)
	}
}

// expireDeferredBeads updates the deferred tracking map and auto-closes beads
// that have been waiting longer than CoopQueueExpiryTimeout. Beads not deferred
// this pass are removed from the tracker (they got a pod or were removed).
func (r *Reconciler) expireDeferredBeads(ctx context.Context, deferredThisPass map[string]beadsapi.AgentBead) {
	if r.beadCloser == nil || r.deferredSince == nil {
		return
	}

	now := time.Now()

	// Record first-seen time for newly deferred beads.
	for beadID := range deferredThisPass {
		if _, tracked := r.deferredSince[beadID]; !tracked {
			r.deferredSince[beadID] = now
		}
	}

	// Remove entries for beads that are no longer deferred.
	for beadID := range r.deferredSince {
		if _, stillDeferred := deferredThisPass[beadID]; !stillDeferred {
			delete(r.deferredSince, beadID)
		}
	}

	// Check for expired beads and auto-close them.
	if r.cfg.CoopQueueExpiryTimeout <= 0 {
		return
	}
	for beadID, since := range r.deferredSince {
		age := now.Sub(since)
		if age < r.cfg.CoopQueueExpiryTimeout {
			continue
		}
		bead := deferredThisPass[beadID]
		r.logger.Warn("auto-closing agent bead that exceeded queue expiry timeout",
			"bead", beadID, "agent", bead.AgentName, "project", bead.Project,
			"queued_for", age.Round(time.Second), "timeout", r.cfg.CoopQueueExpiryTimeout)
		if err := r.beadCloser.CloseBead(ctx, beadID, map[string]string{
			"agent_state":  "expired",
			"close_reason": fmt.Sprintf("queued for %s (limit: %s)", age.Round(time.Second), r.cfg.CoopQueueExpiryTimeout),
		}); err != nil {
			r.logger.Warn("failed to auto-close expired queued agent",
				"bead", beadID, "error", err)
		} else {
			r.closeOrphanedMirrorTask(ctx, bead)
		}
		delete(r.deferredSince, beadID)
	}
}

// closeOrphanedMirrorTask closes the per-run task bead (a ScheduleMirror
// "mirror") that an expired agent was assigned to. Without this the mirror
// leaks open forever (ka-0000003183): the scheduler/bridge per-run close path
// only fires when a gardener's LastAgentID is observed closed, but the
// reconciler can race ahead and never republish a fresh gardener tick that
// would notice. We close the mirror here, at the same moment we hard-close
// the agent, with reason="spawn expired before claim".
//
// Safety: only closes the mirror when (a) the agent carries a task_id field,
// (b) the task is still open, and (c) the task's owner pointer
// (trigger_id or schedule_id) matches the agent's matching field. The
// trigger/schedule ID match prevents accidentally closing an unrelated task
// if a hand-spawned agent was pre-assigned an arbitrary task.
func (r *Reconciler) closeOrphanedMirrorTask(ctx context.Context, agent beadsapi.AgentBead) {
	if r.beadCloser == nil || agent.Metadata == nil {
		return
	}
	taskID := agent.Metadata["task_id"]
	if taskID == "" {
		return
	}
	getter, ok := r.beadCloser.(beadGetter)
	if !ok {
		return
	}
	task, err := getter.GetBead(ctx, taskID)
	if err != nil || task == nil {
		if err != nil {
			r.logger.Debug("mirror-close: fetch failed, skipping",
				"agent", agent.ID, "task", taskID, "error", err)
		}
		return
	}
	if task.Status == "closed" {
		return
	}
	// Ownership check: the task must belong to the same trigger or schedule
	// that spawned the agent. Either pointer is acceptable — the bridge
	// trigger path stamps trigger_id, the legacy scheduler path stamps
	// schedule_id (see scheduler.ClosePerRunTaskBead).
	matched := false
	for _, key := range []string{"trigger_id", "schedule_id"} {
		taskOwner := task.Fields[key]
		agentOwner := agent.Metadata[key]
		if taskOwner != "" && agentOwner != "" && taskOwner == agentOwner {
			matched = true
			break
		}
	}
	if !matched {
		r.logger.Debug("mirror-close: owner mismatch, skipping",
			"agent", agent.ID, "task", taskID)
		return
	}
	if err := r.beadCloser.CloseBead(ctx, taskID, map[string]string{
		"close_reason": "spawn expired before claim",
	}); err != nil {
		r.logger.Warn("mirror-close: failed to close orphaned mirror task",
			"agent", agent.ID, "task", taskID, "error", err)
		return
	}
	r.logger.Info("mirror-close: closed orphaned mirror task",
		"agent", agent.ID, "task", taskID)
}

// markQueued best-effort sets agent_state=queued on a deferred bead. Errors are
// logged but do not fail the reconcile pass — state visibility is informational.
func (r *Reconciler) markQueued(ctx context.Context, bead beadsapi.AgentBead, reason string) {
	if r.stateUpdater == nil {
		return
	}
	if err := r.stateUpdater.UpdateAgentState(ctx, bead.ID, "queued"); err != nil {
		r.logger.Warn("failed to mark deferred agent as queued",
			"bead", bead.ID, "reason", reason, "error", err)
	}
}

// stampResolvedHarness best-effort writes the spec's resolved harness onto the
// agent bead. A failed write is logged and never blocks the spawn: the pod env
// (GARDENIO_HARNESS) still carries a non-default harness, and the next spawn
// re-stamps.
func (r *Reconciler) stampResolvedHarness(ctx context.Context, bead beadsapi.AgentBead, harness string) {
	if r.harnessStamper == nil || harness == "" {
		return
	}
	if err := r.harnessStamper.StampResolvedHarness(ctx, bead.ID, harness); err != nil {
		r.logger.Warn("failed to stamp resolved harness on agent bead",
			"bead", bead.ID, "harness", harness, "error", err)
	}
}

// claimSpawnTask best-effort claims the agent's task for it (see
// beadsapi.ClaimSpawnTask). A failure is logged and never blocks the spawn: the
// crew can still work, it just cannot write its own task.
func (r *Reconciler) claimSpawnTask(ctx context.Context, bead beadsapi.AgentBead, spec podmanager.AgentPodSpec) {
	if r.taskClaimer == nil || spec.TaskID == "" {
		return
	}
	outcome, err := r.taskClaimer.ClaimSpawnTask(ctx, bead.ID)
	if err != nil {
		r.logger.Warn("failed to claim agent's task at spawn",
			"bead", bead.ID, "agent", spec.AgentName, "task", spec.TaskID, "error", err)
		return
	}
	r.logger.Info("agent task claim at spawn",
		"bead", bead.ID, "agent", spec.AgentName, "task", spec.TaskID, "outcome", outcome)
}

// markAgentFailed hard-fails an agent bead when CreateAgentPod returned a
// permanent K8s error. The bridge watches bead field updates and will surface
// a crash notification from agent_state=failed. Errors from the API call are
// logged but do not fail the reconcile pass.
func (r *Reconciler) markAgentFailed(ctx context.Context, bead beadsapi.AgentBead, cause error) {
	if r.failedMarker == nil {
		return
	}
	if err := r.failedMarker.MarkAgentFailed(ctx, bead.ID, cause.Error()); err != nil {
		r.logger.Warn("failed to mark agent bead as failed",
			"bead", bead.ID, "cause", cause, "error", err)
	}
}

// errLoginlessNoProject is the failure reason stamped on an agent bead the
// reconciler refuses to spawn because it carries no project (and so would yield
// a loginless pod with no Claude OAuth credential). Mirrors the event-door
// guard in cmd/controller (ka-0000007608).
var errLoginlessNoProject = errors.New("agent bead has no project; refusing loginless pod spawn (no Claude OAuth credential)")

// errNonK8sBackendUnimplemented marks an agent whose resolved execution target
// is not the Kubernetes pod backend (e.g. lambda_microvm) while that backend is
// not yet wired (epic ka-0000005010, Changes C3-C6). Fail closed rather than
// realize it as a pod against the wrong backend.
var errNonK8sBackendUnimplemented = errors.New("agent execution_target is not kubernetes and the requested backend is not yet implemented")

// isPermanentCreateError reports whether a pod creation error is permanent
// (i.e. will never succeed on retry) and the bead should be hard-failed rather
// than left to spin forever. K8s validation errors — most notably DNS-1123
// violations in pod names — surface as IsInvalid.
func isPermanentCreateError(err error) bool {
	if err == nil {
		return false
	}
	return apierrors.IsInvalid(err)
}

// nodeDrainInProgress reports whether the node hosting pod is being
// force-reclaimed, so the agent should be gracefully drained before the
// kubelet SIGTERMs it (WS1, ka-0000007969). It is gated behind
// cfg.NodeDrainNudgeEnabled and is a no-op when node visibility is unavailable
// (r.pods lacks a node, GetNode errors) — a lookup miss must never block the
// reconcile loop or spuriously drain a healthy agent. Results are memoised in
// doomed for the duration of one reconcile pass so co-located pods share a
// single GetNode call.
func (r *Reconciler) nodeDrainInProgress(ctx context.Context, pod *corev1.Pod, doomed map[string]bool) bool {
	if !r.cfg.NodeDrainNudgeEnabled {
		return false
	}
	nodeName := pod.Spec.NodeName
	if nodeName == "" {
		return false // not yet scheduled; nothing to drain off
	}
	if v, ok := doomed[nodeName]; ok {
		return v
	}
	node, err := r.pods.GetNode(ctx, nodeName)
	if err != nil {
		// Missing/unreadable node: fail safe (do not drain). A genuinely deleted
		// node returns NotFound; treating that as "not doomed" is fine because the
		// pod is already gone or about to be reaped by the terminal-pod path.
		r.logger.Debug("node lookup failed during drain check, skipping",
			"pod", pod.Name, "node", nodeName, "error", err)
		doomed[nodeName] = false
		return false
	}
	d := nodeIsDoomed(node)
	doomed[nodeName] = d
	return d
}
