// Package statusreporter syncs K8s pod status back to beads via the daemon HTTP API.
// This gives beads visibility into pod health.
package statusreporter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/lands/landns"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/reconciler"
)

// PodStatus represents the K8s pod state to report back to beads.
type PodStatus struct {
	PodName   string
	Namespace string
	Phase     string // Pending, Running, Succeeded, Failed, Unknown
	Ready     bool
	Message   string
}

// BackendMetadata holds connection info written to agent bead notes
// so that ResolveBackend() can discover how to connect to the agent.
type BackendMetadata struct {
	PodName   string // K8s pod name
	Namespace string // K8s namespace
	Backend   string // "coop" or "k8s"
	// No coop_url: nothing dials the agent pod any more (it subscribes to its
	// own mail), so the pod address is not published on the bead.
}

// Reporter syncs pod status back to beads.
type Reporter interface {
	// ReportPodStatus sends a single pod's status to beads.
	// agentName is the agent bead ID (e.g., "crew-gardenio-crew-furiosa").
	// prewarmed indicates the pod is a prewarmed agent whose state should
	// not be overwritten to "working" while waiting for assignment.
	ReportPodStatus(ctx context.Context, agentName string, status PodStatus, prewarmed bool) error

	// ReportBackendMetadata writes backend connection info to the agent bead's notes.
	ReportBackendMetadata(ctx context.Context, agentName string, meta BackendMetadata) error

	// SyncAll reconciles all agent pod statuses with beads.
	SyncAll(ctx context.Context) error

	// Metrics returns the current metrics snapshot.
	Metrics() MetricsSnapshot
}

// MetricsSnapshot holds current metric values for logging/monitoring.
type MetricsSnapshot struct {
	StatusReportsTotal int64
	StatusReportErrors int64
	SyncAllRuns        int64
	SyncAllErrors      int64
	AgentsByState      map[string]int64 // state -> count
}

// PhaseToAgentState maps a K8s pod phase to a beads agent_state.
// In addition to standard K8s phases, it handles the synthetic "Stopped"
// phase emitted by the controller for AgentStop events.
func PhaseToAgentState(phase string) string {
	switch corev1.PodPhase(phase) {
	case corev1.PodPending:
		return "spawning"
	case corev1.PodRunning:
		return "working"
	case corev1.PodSucceeded:
		return "done"
	case corev1.PodFailed:
		return "failed"
	default:
		// Handle synthetic phases from controller events.
		if phase == "Stopped" {
			return "done"
		}
		return ""
	}
}

// isStartupState reports whether a bead's agent_state is a pre-working
// startup state — one that the spawning→working transition must still be
// allowed to overwrite. Used by SyncAll's coop-idle guard so a coop "idle"
// reported during the startup window (before Claude's first hook) does not
// trap the bead at "spawning" by suppressing the one-time working write.
// An empty state (bead fetch failed) is treated as a startup state so we
// fail toward reporting working rather than leaving the card stuck.
// ka-0000000427.
func isStartupState(state string) bool {
	switch state {
	case "", "spawning", "assigning", "prewarmed":
		return true
	default:
		return false
	}
}

// IsPodSchedulingBlocked returns true when the pod is Pending and its
// PodScheduled condition reports reason=Unschedulable. This is the signal
// that the cluster cannot place the pod (e.g., out of capacity, no matching
// node selector). Callers use it to surface "stuck: cluster capacity" to
// operators and to drive the pod reaper.
func IsPodSchedulingBlocked(pod *corev1.Pod) bool {
	if pod == nil || pod.Status.Phase != corev1.PodPending {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodScheduled && cond.Reason == corev1.PodReasonUnschedulable {
			return true
		}
	}
	return false
}

// agentBeadID returns the bead ID for a pod. It prefers the explicit
// gardenio.io/bead-id annotation (set by Helm or the controller). If absent,
// it constructs: {mode}-{project}-{role}-{agent}.
func agentBeadID(pod *corev1.Pod) string {
	if id := pod.Annotations[podmanager.AnnotationBeadID]; id != "" {
		return id
	}
	project := pod.Labels[podmanager.LabelProject]
	role := pod.Labels[podmanager.LabelRole]
	agent := pod.Labels[podmanager.LabelAgent]
	mode := pod.Labels[podmanager.LabelMode]
	if mode == "" {
		mode = "crew"
	}

	return fmt.Sprintf("%s-%s-%s-%s", mode, project, role, agent)
}

// detectCoopPort returns the coop API port if the pod has a container
// exposing port 8080 (CoopDefaultPort) or a container named "coop".
// Returns 0 if no coop capability detected.
func detectCoopPort(pod *corev1.Pod) int32 {
	for _, c := range pod.Spec.Containers {
		if c.Name == "coop" {
			for _, p := range c.Ports {
				if p.ContainerPort == 8080 {
					return p.ContainerPort
				}
			}
			return 8080 // coop sidecar default
		}
		for _, p := range c.Ports {
			if p.ContainerPort == 8080 {
				return p.ContainerPort
			}
		}
	}
	return 0
}

// BeadUpdater is the interface for updating beads via the daemon HTTP API.
type BeadUpdater interface {
	UpdateBeadNotes(ctx context.Context, beadID, notes string) error
	UpdateAgentState(ctx context.Context, beadID, state string) error
	// UpdateBeadFields merges typed fields onto a bead. Used by SyncAll to
	// surface pod scheduling state (pod_scheduling_blocked) on the bead so
	// the bridge can render "stuck: cluster capacity" and the reaper can
	// detect unschedulable pods.
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	// UpdateBeadFieldsTyped merges fields carrying their native JSON types
	// (numbers stay numbers). Used by reportLiveUsage for context_tokens
	// (schema type=integer) and live_cost_usd (type=float): the string-map
	// UpdateBeadFields only coerces a string to a number when the field is
	// ALREADY set with a numeric type, so the first-ever write of a
	// never-seeded numeric field is sent as a JSON string and rejected by
	// the daemon's schema validation — the field then never lands on any
	// agent bead (ka-0000008019).
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	// GetBeadStatus returns the bead's status string ("open", "in_progress",
	// "closed", etc.). Used to skip transient writes against already-closed
	// beads during the K8s pod grace period (ka-0000005253).
	GetBeadStatus(ctx context.Context, beadID string) (string, error)
	// GetAgentState returns the bead's agent_state field. Used to detect
	// prewarmed pods whose bead has already been assigned off the pool: the
	// pod's gardenio.io/prewarmed annotation is never cleared on assignment, so
	// SyncAll falls back to the bead's own agent_state to decide whether to
	// keep suppressing working/spawning writes (ka-0000004312).
	GetAgentState(ctx context.Context, beadID string) (string, error)
	// GetAgentSessionID returns the agent bead's session identifier
	// (last_session_id field), used as the drill-down label on the
	// gardenio_agent_lifecycle_events_total{event="oom_killed"} series so the
	// FSM composite alert can resolve the OOM'd session's S3 recording key
	// (FSM_CORRELATION_CONTRACT §5a / §6). Best-effort: returns "" when the
	// bead carries no session id.
	GetAgentSessionID(ctx context.Context, beadID string) (string, error)
}

// coopAgentResponse is the JSON response from coop's GET /api/v1/agent endpoint.
// Only the fields needed for error state detection and stagnation tracking
// are included.
type coopAgentResponse struct {
	State         string `json:"state"`
	ErrorCategory string `json:"error_category,omitempty"`
	ErrorDetail   string `json:"error_detail,omitempty"`
	ParkedReason  string `json:"parked_reason,omitempty"`
	SinceSeq      uint64 `json:"since_seq,omitempty"`
	ScreenSeq     uint64 `json:"screen_seq,omitempty"`
	// ContextTokens is the live context-window occupancy (input + cache) at
	// coop's most recent request, and CostUSD is the in-pod running cost
	// estimate. SyncAll mirrors both onto the agent bead so the beads-viewer
	// and Slack agent cards can show "context so far" / "cost so far" while
	// the agent is still running (ka-0000007210).
	ContextTokens uint64  `json:"context_tokens,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`
	// Turns is coop's cumulative LLM round-trip count (request_count) so far.
	// SyncAll mirrors it onto the agent bead as the live `live_turns` field —
	// the live counterpart of the authoritative total_request_count the
	// session-close reconciler owns (epic ka-0000006449).
	Turns uint64 `json:"request_count,omitempty"`
}

// StuckEvent is the payload published by the reporter when it detects an
// agent pod that is Running+Ready but whose coop sequences have not advanced
// for the configured stuck threshold while reporting agent_state=working.
// The controller's main loop translates these into subscriber.AgentStuck
// events so the existing handler can delete and recreate the pod
// (ka-0000001882).
type StuckEvent struct {
	BeadID    string
	Project   string
	Role      string
	Mode      string
	AgentName string
	Reason    string
}

// EventPublisher is the optional sink for events the reporter discovers
// during SyncAll. The controller wires this to a channel that fans into the
// main event loop so the existing AgentStuck handler can act.
type EventPublisher interface {
	PublishStuck(ctx context.Context, ev StuckEvent)
}

// stagnationEntry records the most recent liveness signals observed for an
// agent pod and the wall-clock time at which any of them last advanced.
// SyncAll uses this to decide whether to publish a StuckEvent.
//
// contextTokens/costUSD/turns are tracked alongside the terminal-screen
// sequences (sinceSeq/screenSeq) because they are an orthogonal proof-of-life:
// an agent mid-model-request grows its context window, running cost and
// round-trip count even while the terminal screen is quiet (bug ka-0000002709).
// Counting any of them as progress stops the heuristic from hard-killing a live
// agent that simply isn't printing to its terminal — e.g. a long Opus turn, a
// gardener waiting on a child agent, or a repro agent driving a headless
// browser.
//
// turns (coop's request_count) is the load-bearing one on the OAuth/Bedrock
// path: costUSD is structurally always zero there because coop only
// accumulates cost when LiteLLM stamps response_cost on the turn
// (coop/crates/cli/src/usage.rs), so it can never advance and contributes no
// safety. Without turns the effective signal set collapses to "did the
// terminal print", which is what let the watchdog kill a live e2e agent
// (ka-0000000084).
type stagnationEntry struct {
	sinceSeq      uint64
	screenSeq     uint64
	contextTokens uint64
	costUSD       float64
	turns         uint64
	lastAdvanceAt time.Time
}

// EventRecorder is the subset of the controller's event-driven Prometheus
// metrics that the status reporter emits to during SyncAll: pod phase
// transitions (E2.5), OOM-kill lifecycle events (E2.4), and the current-state
// pending-pods gauge (GAP-1). Satisfied by *metrics.EventMetrics. Kept as a
// local interface so statusreporter does not import the metrics package (avoids
// a dependency cycle and keeps the reporter testable with a fake recorder). A
// nil recorder disables emission. The map key type is intentionally a built-in
// ([2]string{project, role}) so this boundary carries no metrics-package types.
type EventRecorder interface {
	// RecordPodPhaseTransition increments gardenio_agent_pod_phase_transitions_total.
	RecordPodPhaseTransition(from, to, project, role string) bool
	// RecordLifecycleEvent increments gardenio_agent_lifecycle_events_total.
	// sessionID is non-empty only for oom_killed.
	RecordLifecycleEvent(event, project, role, sessionID string) bool
	// SetPodsPending replaces the gardenio_agent_pods_pending gauge with a fresh
	// snapshot: one series per {project,role} set to its Pending-pod count,
	// stale series cleared. SyncAll calls this once per sweep. The key is
	// [2]string{project, role}.
	SetPodsPending(counts map[[2]string]int)
	// SetUpstreamErrorActive replaces the
	// gardenio_agent_upstream_error_active gauge with a fresh snapshot: one
	// series per {category,project,role} set to its count of agents currently
	// sitting on that upstream provider error, stale series cleared. SyncAll
	// calls this once per sweep. The key is [3]string{category, project, role}.
	SetUpstreamErrorActive(counts map[[3]string]int)
	// RecordUpstreamErrorTransition increments
	// gardenio_agent_upstream_error_transitions_total once for an agent
	// ENTERING an upstream error category (edge-triggered — the reporter owns
	// the per-bead dedup).
	RecordUpstreamErrorTransition(category, project, role string) bool
	// ObserveLandSpawnToWorking observes gardenio_lands_spawn_to_working_seconds
	// once for a tenant-land agent reaching working for the first time (the
	// reporter owns the once-per-agent dedup, see observeLandSpawnToWorking).
	ObserveLandSpawnToWorking(land string, seconds float64) bool
}

// FieldLandFirstWorkingAt is the agent bead field (RFC 3339) the reporter
// stamps the first time a tenant-land agent reaches agent_state=working. It
// makes gardenio_lands_spawn_to_working_seconds once per agent: a stuck
// restart or a respawn takes the bead back through spawning, and a controller
// restart forgets everything in memory, but neither observes again once the
// bead carries it.
const FieldLandFirstWorkingAt = "land_first_working_at"

// beadReader is the optional daemon read the Lands spawn-to-working SLI needs
// (the bead's created_at and fields). *beadsapi.Client satisfies it; a
// BeadUpdater without it observes nothing.
type beadReader interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

var _ beadReader = (*beadsapi.Client)(nil)

// HTTPReporter reports backend metadata to beads via the daemon HTTP API.
type HTTPReporter struct {
	daemon    BeadUpdater
	client    kubernetes.Interface
	namespace string
	// extraNamespaces are additional namespaces SyncAll sweeps for agent pods
	// beyond the primary `namespace`. Used for the PSA-privileged gVisor
	// namespace where runsc freeze/thaw agents run (bug ka-0000004301): without
	// it, a runsc agent's bead never receives phase/coop_url updates. Empty in
	// the single-namespace deployment.
	extraNamespaces []string
	// namespaceSource, when set, returns more namespaces to sweep, re-read
	// every SyncAll: the tenant land namespaces (land-<land>, Lands step
	// 10.2), which come and go with the lands. Without it a tenant agent's
	// bead never leaves spawning (gd-Vuu1J7y4J0).
	namespaceSource func() []string
	logger          *slog.Logger
	coopClient      *http.Client // short-timeout client for coop agent status queries

	reportsTotal atomic.Int64
	reportErrors atomic.Int64
	syncRuns     atomic.Int64
	syncErrors   atomic.Int64

	// Stuck-agent heuristic state (ka-0000001882).
	stuckThreshold time.Duration
	// stuckLongLivedMultiplier scales stuckThreshold for long-lived
	// orchestrator roles (gardener / plan-gardener). These roles legitimately
	// produce no terminal-screen output for long stretches while orchestrating
	// child agents / hosting a Workflow fan-out, so the base threshold
	// mis-flags them as wedged and the AgentStuck delete+recreate hard-kills a
	// live agent mid-turn — the dominant involuntary-death vector
	// (bug ka-0000002709). A multiplier of 1 (or <1) disables the role bias.
	stuckLongLivedMultiplier float64
	publisher                EventPublisher
	now                      func() time.Time
	stagnationMu             sync.Mutex
	stagnation               map[string]stagnationEntry // beadID -> last observed seqs

	// Event-driven metrics emission (E2.4/E2.5). events is nil until
	// SetEventRecorder wires it. lastPhase tracks the previously observed pod
	// phase per beadID so SyncAll can detect phase changes and emit
	// gardenio_agent_pod_phase_transitions_total; oomReported dedups OOM
	// lifecycle events so a pod that lingers Failed for several sync cycles
	// only increments gardenio_agent_lifecycle_events_total{event="oom_killed"}
	// once. Both maps are guarded by phaseMu.
	events      EventRecorder
	phaseMu     sync.Mutex
	lastPhase   map[string]string   // beadID -> last observed pod phase
	oomReported map[string]struct{} // beadID -> OOM already counted
	oomStamped  map[string]string   // beadID -> FinishedAt of the kill whose sessions are stamped

	// lastUpstreamError tracks the previously observed coop error_category per
	// beadID ("" = healthy) so SyncAll can fire
	// gardenio_agent_upstream_error_transitions_total on the ENTRY EDGE only.
	// Without this dedup the counter would increment on every poll for as long
	// as an agent sat on one error, making it a duration proxy rather than the
	// independent cross-check of the active gauge it is meant to be
	// (ka-0000003454, advice ka-0000005924). Guarded by upstreamErrorMu.
	upstreamErrorMu   sync.Mutex
	lastUpstreamError map[string]string // beadID -> last observed error category
}

// NewHTTPReporter creates a reporter that updates beads via daemon HTTP API.
func NewHTTPReporter(daemon BeadUpdater, client kubernetes.Interface, namespace string, logger *slog.Logger) *HTTPReporter {
	return &HTTPReporter{
		daemon:    daemon,
		client:    client,
		namespace: namespace,
		logger:    logger,
		coopClient: &http.Client{
			Timeout: 3 * time.Second,
		},
		now:               time.Now,
		stagnation:        make(map[string]stagnationEntry),
		lastPhase:         make(map[string]string),
		oomReported:       make(map[string]struct{}),
		oomStamped:        make(map[string]string),
		lastUpstreamError: make(map[string]string),
	}
}

// SetExtraNamespaces configures additional namespaces SyncAll sweeps for agent
// pods beyond the primary namespace. Empty or duplicate-of-primary entries are
// ignored. Used to include the PSA-privileged gVisor namespace where runsc
// freeze/thaw agents run (bug ka-0000004301).
func (r *HTTPReporter) SetExtraNamespaces(namespaces ...string) {
	for _, ns := range namespaces {
		if ns == "" || ns == r.namespace {
			continue
		}
		r.extraNamespaces = append(r.extraNamespaces, ns)
	}
}

// SetNamespaceSource configures a function SyncAll calls on every sweep for
// more namespaces to list agent pods in, beyond the primary and extra
// namespaces. The controller passes the tenant land namespaces, which change as
// lands open and close. A namespace that is gone, or not yet bound to the
// controller, is skipped quietly.
func (r *HTTPReporter) SetNamespaceSource(fn func() []string) {
	r.namespaceSource = fn
}

// SetEventRecorder wires the event-driven Prometheus metrics (E2.4/E2.5) that
// SyncAll emits to. Pass nil to disable emission (the reporter no-ops cleanly).
func (r *HTTPReporter) SetEventRecorder(rec EventRecorder) {
	r.events = rec
}

// SetStuckDetection wires the stuck-agent heuristic. If publisher is nil or
// threshold is zero, stagnation tracking is disabled and no StuckEvents are
// published. The threshold should be larger than the SyncAll interval so a
// pod has at least a few cycles to advance its sequences.
//
// longLivedMultiplier scales the threshold for long-lived orchestrator roles
// (gardener / plan-gardener) so they get a proportionally longer grace window
// before being flagged as wedged (bug ka-0000002709). A value <= 1 disables
// the role bias; the effective per-role threshold is computed in
// stuckThresholdForRole.
func (r *HTTPReporter) SetStuckDetection(publisher EventPublisher, threshold time.Duration, longLivedMultiplier float64) {
	r.publisher = publisher
	r.stuckThreshold = threshold
	r.stuckLongLivedMultiplier = longLivedMultiplier
}

// ReportPodStatus updates the agent's state in beads based on pod phase.
// Maps K8s pod phases to beads agent states via the daemon HTTP API.
// Set prewarmed=true to skip overwriting agent_state for prewarmed pods
// (their state stays "prewarmed" until explicitly assigned).
func (r *HTTPReporter) ReportPodStatus(ctx context.Context, agentName string, status PodStatus, prewarmed bool) error {
	_, _, err := r.reportPodStatus(ctx, agentName, status, prewarmed)
	return err
}

// reportPodStatus is ReportPodStatus, also returning the bead's agent_state it
// read before writing ("" when it did not read one, or the read failed) and the
// agent_state it wrote ("" when it wrote none). SyncAll uses the pair to see a
// first transition to working.
func (r *HTTPReporter) reportPodStatus(ctx context.Context, agentName string, status PodStatus, prewarmed bool) (prev, wrote string, err error) {
	r.reportsTotal.Add(1)

	state := PhaseToAgentState(status.Phase)
	if state == "" {
		r.logger.Debug("skipping status report for unknown phase",
			"agent", agentName, "phase", status.Phase)
		return "", "", nil
	}

	// Prewarmed agents stay prewarmed until explicitly assigned.
	// Only allow terminal states (failed, done) through; skip spawning and working.
	if prewarmed && state != "failed" && state != "done" {
		r.logger.Debug("skipping status update for prewarmed agent",
			"agent", agentName, "phase", status.Phase, "suppressed_state", state)
		return "", "", nil
	}

	// Skip writes against already-closed beads. After `gb stop` closes the
	// bead, the K8s pod sticks around for the grace period — without this
	// guard, the reporter would PATCH agent_state=working back over the
	// agent's clean done state (ka-0000005253).
	if r.beadIsClosed(ctx, agentName) {
		r.logger.Debug("skipping status update for closed bead",
			"agent", agentName, "phase", status.Phase, "suppressed_state", state)
		return "", "", nil
	}

	// Skip writes against a SUSPEND-latched bead (agent_state=stopping). A
	// freeze-eligible `gb stop` leaves the bead OPEN at agent_state=stopping as
	// the frozen-awaiting-thaw signal (suspendAgent, cmd/gb/stop.go) while the
	// controller checkpoints and tears the pod down. During that window — and,
	// on prod, for as long as a not-yet-completed freeze leaves the pod Running
	// — this reporter maps PodRunning→"working" and would PATCH it back over the
	// stopping latch, un-suspending the agent exactly as the in-pod monitorIdle
	// poller did (ka-0000006721). The closed-bead skip above does not cover it
	// because a suspended bead is deliberately OPEN. Terminal writes (done/failed)
	// still pass so a freeze that fails and the pod dies is reported truthfully.
	// A fetch failure is treated as NOT latched (fail-open) so a transient
	// daemon blip never drops a healthy write.
	if state != "done" && state != "failed" {
		if got, err := r.daemon.GetAgentState(ctx, agentName); err == nil {
			prev = got
		}
		if prev == "stopping" {
			r.logger.Debug("skipping status update for suspend-latched bead (agent_state=stopping)",
				"agent", agentName, "phase", status.Phase, "suppressed_state", state)
			return prev, "", nil
		}
	}

	r.logger.Info("reporting pod status via HTTP",
		"agent", agentName, "pod", status.PodName,
		"phase", status.Phase, "state", state, "ready", status.Ready)

	if err := r.daemon.UpdateAgentState(ctx, agentName, state); err != nil {
		r.reportErrors.Add(1)
		r.logger.Warn("failed to report pod status",
			"agent", agentName, "state", state, "error", err)
		return prev, "", fmt.Errorf("reporting status for %s: %w", agentName, err)
	}

	return prev, state, nil
}

// beadIsClosed returns true when the bead is already closed. A failure to
// fetch is treated as not-closed so transient daemon errors do not silently
// drop reporter writes; the server-side filter (kbeads handleUpdateBead) is
// the safety net for that case.
func (r *HTTPReporter) beadIsClosed(ctx context.Context, beadID string) bool {
	status, err := r.daemon.GetBeadStatus(ctx, beadID)
	if err != nil {
		return false
	}
	return status == "closed"
}

// ReportBackendMetadata writes backend connection info to the agent bead's
// notes field via the daemon HTTP API.
func (r *HTTPReporter) ReportBackendMetadata(ctx context.Context, agentName string, meta BackendMetadata) error {
	r.reportsTotal.Add(1)

	if r.beadIsClosed(ctx, agentName) {
		r.logger.Debug("skipping backend metadata report for closed bead",
			"agent", agentName)
		return nil
	}

	var lines []string
	if meta.Backend != "" {
		lines = append(lines, fmt.Sprintf("backend: %s", meta.Backend))
	}
	if meta.PodName != "" {
		lines = append(lines, fmt.Sprintf("pod_name: %s", meta.PodName))
	}
	if meta.Namespace != "" {
		lines = append(lines, fmt.Sprintf("pod_namespace: %s", meta.Namespace))
	}

	if len(lines) == 0 {
		return nil
	}

	notes := strings.Join(lines, "\n")
	r.logger.Info("reporting backend metadata via HTTP",
		"agent", agentName, "backend", meta.Backend, "pod", meta.PodName)

	if err := r.daemon.UpdateBeadNotes(ctx, agentName, notes); err != nil {
		r.reportErrors.Add(1)
		r.logger.Warn("failed to report backend metadata",
			"agent", agentName, "error", err)
		return fmt.Errorf("reporting backend metadata for %s: %w", agentName, err)
	}
	return nil
}

// SyncAll reconciles all agent pod statuses with beads.
func (r *HTTPReporter) SyncAll(ctx context.Context) error {
	r.syncRuns.Add(1)

	pods, err := r.client.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/name=gardenio",
	})
	if err != nil {
		r.syncErrors.Add(1)
		return fmt.Errorf("listing agent pods: %w", err)
	}

	// Sweep any extra namespaces (e.g. the PSA-privileged gVisor namespace where
	// runsc freeze/thaw agents run, bug ka-0000004301) and merge their pods into
	// the same per-pod loop. Each pod already carries its own namespace, so
	// downstream reporting is namespace-correct. A failure to list one extra
	// namespace is logged but does not abort the primary sweep.
	for _, ns := range r.extraNamespaces {
		extra, err := r.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=gardenio",
		})
		if err != nil {
			r.syncErrors.Add(1)
			r.logger.Warn("SyncAll: failed to list agent pods in extra namespace",
				"namespace", ns, "error", err)
			continue
		}
		pods.Items = append(pods.Items, extra.Items...)
	}

	// Sweep the namespaces the source names this time: the tenant land
	// namespaces, where the land fence places tenant pods (gd-Vuu1J7y4J0). A
	// land namespace the provisioner has not made yet, or has just deleted, is
	// not found or not bound to the controller; that is the land's lifecycle,
	// not a sweep failure, so it is skipped at debug.
	if r.namespaceSource != nil {
		swept := map[string]bool{r.namespace: true}
		for _, ns := range r.extraNamespaces {
			swept[ns] = true
		}
		for _, ns := range r.namespaceSource() {
			if ns == "" || swept[ns] {
				continue
			}
			swept[ns] = true
			more, err := r.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=gardenio",
			})
			if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
				r.logger.Debug("SyncAll: skipping land namespace not ready for listing",
					"namespace", ns, "error", err)
				continue
			}
			if err != nil {
				r.syncErrors.Add(1)
				r.logger.Warn("SyncAll: failed to list agent pods in land namespace",
					"namespace", ns, "error", err)
				continue
			}
			pods.Items = append(pods.Items, more.Items...)
		}
	}

	// pendingCounts is the current-state tally of agent pods in K8s phase
	// Pending, keyed by [project, role], accumulated across this sweep and
	// flushed once to the gardenio_agent_pods_pending gauge (GAP-1,
	// ka-0000005418) after the loop. It is populated for EVERY labelled agent
	// pod regardless of bead status — the gauge observes pod phase, not bead
	// lifecycle — so it is tallied before the closed-bead skip below, alongside
	// recordPodLifecycleMetrics. A non-nil-but-empty map is intentional: it
	// clears the gauge to no-data when nothing is Pending (the healthy state).
	pendingCounts := make(map[[2]string]int)

	// upstreamErrorCounts is the same shape of current-state tally for agents
	// sitting on an upstream provider error, keyed by [category, project, role]
	// and flushed once to the gardenio_agent_upstream_error_active gauge (leg A of
	// the Claude-outage detector, ka-0000003454). Unlike pendingCounts this is
	// populated from coop's error_category inside the running-pod branch below,
	// since it needs the per-pod coop query. A non-nil-but-empty map is again
	// intentional: it clears the gauge when no agent is erroring, which is what
	// lets the outage alert RESOLVE.
	upstreamErrorCounts := make(map[[3]string]int)

	seenBeads := make(map[string]struct{}, len(pods.Items))
	for _, pod := range pods.Items {
		agentLabel := pod.Labels[podmanager.LabelAgent]
		projectLabel := pod.Labels[podmanager.LabelProject]
		roleLabel := pod.Labels[podmanager.LabelRole]
		if agentLabel == "" || projectLabel == "" || roleLabel == "" {
			continue
		}

		beadID := agentBeadID(&pod)
		seenBeads[beadID] = struct{}{}

		// Tally Pending pods for the current-state gauge. Counted here, before
		// the closed-bead skip, so a pod that is Pending is reflected even in
		// the transient window where its bead reads closed — the gauge is a pod-
		// phase observation, independent of bead state, and both prewarmed and
		// assigned pods that cannot schedule are exactly the spawn-stall signal
		// the GAP-1 alert exists to catch.
		if pod.Status.Phase == corev1.PodPending {
			pendingCounts[[2]string{projectLabel, roleLabel}]++
		}

		// Emit event-driven metrics (E2.4 OOM lifecycle + E2.5 pod phase
		// transitions) BEFORE the closed-bead skip below: a pod that OOMs and
		// then has its bead closed must still be counted, and a Running→Failed
		// transition is exactly the close-window case we care about. This is a
		// pure local-state + Prometheus operation (no daemon writes), so it is
		// safe to run for closed beads.
		r.recordPodLifecycleMetrics(ctx, &pod, beadID, projectLabel, roleLabel)
		// Also before the skip: the agent bead may close before the session
		// the kill ended is stamped (gd-DgSORP1h8P).
		r.stampOOMKilledSessions(ctx, &pod, beadID)

		// Skip closed beads early — once `gb stop` has closed the agent bead,
		// the K8s pod sticks around for the grace period. Continuing to PATCH
		// transient fields (agent_state, coop_url) flips a clean done state
		// back to working in the activity feed (ka-0000005253).
		if r.beadIsClosed(ctx, beadID) {
			r.logger.Debug("SyncAll: skipping closed bead",
				"bead", beadID, "pod", pod.Name)
			continue
		}

		status := PodStatus{
			PodName:   pod.Name,
			Namespace: pod.Namespace,
			Phase:     string(pod.Status.Phase),
			Ready:     reconciler.IsPodReady(&pod),
			Message:   pod.Status.Message,
		}

		// The gardenio.io/prewarmed annotation is stamped once at pod creation
		// and never cleared when poolmanager.AssignPrewarmed() takes the pod
		// off the pool. So the annotation alone over-reports: an assigned pod
		// running a real agent still carries it, and suppressing state writes
		// would leave agent_state stuck at "assigning" for the pod's whole life
		// (ka-0000004312). Cross-check the bead's own agent_state: once it has
		// moved off "prewarmed" (to assigning/working/...), the pod is no
		// longer genuinely idle in the pool and we must report normally.
		isPrewarmed := pod.Annotations[podmanager.AnnotationPrewarmed] == "true"
		if isPrewarmed {
			if state, err := r.daemon.GetAgentState(ctx, beadID); err == nil && state != "" && state != "prewarmed" {
				r.logger.Debug("SyncAll: prewarmed annotation is stale, bead already assigned",
					"bead", beadID, "pod", pod.Name, "agent_state", state)
				isPrewarmed = false
			}
		}
		podReady := reconciler.IsPodReady(&pod)

		// For running coop-enabled pods, check coop's agent API for error states
		// (rate_limited, budget_exceeded) that the pod phase alone cannot detect.
		// This propagates coop error states to bead agent_state, enabling the
		// SSE watcher → auth failover path.
		coopPort := detectCoopPort(&pod)
		if pod.Status.Phase == corev1.PodRunning && coopPort > 0 && pod.Status.PodIP != "" && !isPrewarmed {
			coopInfo := r.queryCoopAgentState(ctx, pod.Status.PodIP, coopPort, pod.Name)
			// Tally the upstream-error gauge and fire the edge-triggered counter
			// for EVERY reported category, before the overrideState branch below
			// returns early via `goto metadata` — otherwise rate_limited and
			// budget_exceeded (the two categories that DO set a pill) would skip
			// counting entirely. ka-0000003454.
			if coopInfo.errorCategory != "" {
				upstreamErrorCounts[[3]string{coopInfo.errorCategory, projectLabel, roleLabel}]++
			}
			r.trackUpstreamError(beadID, coopInfo.errorCategory, projectLabel, roleLabel)
			// Mirror live context-window size + running cost onto the bead for
			// agent cards — but NOT when idle, because the resulting updated_at
			// bump defeats the stale reaper's wall-clock check. ka-0000006338.
			if !coopInfo.idle {
				r.reportLiveUsage(ctx, beadID, pod.Name, coopInfo)
			}
			if coopInfo.overrideState != "" {
				r.reportsTotal.Add(1)
				r.logger.Info("SyncAll: propagating coop error state to bead",
					"bead", beadID, "pod", pod.Name, "coop_state", coopInfo.overrideState)
				if err := r.daemon.UpdateAgentState(ctx, beadID, coopInfo.overrideState); err != nil {
					r.reportErrors.Add(1)
					r.logger.Warn("SyncAll: failed to propagate coop error state",
						"bead", beadID, "pod", pod.Name, "state", coopInfo.overrideState, "error", err)
				}
				// Error state preempts the stuck check — drop any prior tracking
				// so a later recovery starts from a fresh window.
				r.resetStagnation(beadID)
				// Skip normal pod-phase reporting — coop state takes precedence.
				goto metadata
			}
			// When coop reports state=idle, the pod is running but Claude has no
			// active work (e.g. finished a task but never called `gb stop`). Skip
			// the UpdateAgentState call so updated_at does not get bumped —
			// otherwise the stale reaper's wall-clock check never fires and the
			// pod runs forever. ka-0000003615.
			//
			// BUT coop also reports idle during the *startup window* — after the
			// pod goes Running but before Claude's first hook fires. If we skip
			// there, the one-time spawning→working transition never happens and
			// the bead is trapped at "spawning" for the whole idle window —
			// leaving every consumer (Slack card, nudge gating, stale reaper,
			// beads-viewer) showing "starting" while the agent is plainly
			// Running. The render-layer self-heal (ka-0000005799) can't cover
			// this because there are no tool events yet either. So honor the
			// idle-skip only once the bead has *already* reached a live state;
			// while still in a startup state (spawning/assigning/prewarmed) let
			// the working write through so consumers promote immediately.
			// ka-0000000427.
			if coopInfo.idle {
				// Idle (whether genuine-parked or startup-window) is never a
				// stuck-agent signal — no seq advance is expected. Clear any
				// prior stagnation entry in both sub-cases.
				r.resetStagnation(beadID)
				beadState, _ := r.daemon.GetAgentState(ctx, beadID)
				if isStartupState(beadState) {
					// Startup window: emit the one-time spawning→working write so
					// consumers promote, then skip the rest of the per-pod work
					// (stuck check, normal report) — equivalent to the genuine
					// idle path but with the transition emitted first.
					r.logger.Debug("SyncAll: coop idle during startup window, emitting working transition",
						"bead", beadID, "pod", pod.Name, "bead_state", beadState)
					prev, wrote, err := r.reportPodStatus(ctx, beadID, status, isPrewarmed)
					if err != nil {
						r.logger.Warn("SyncAll: failed to report startup working status",
							"bead", beadID, "pod", pod.Name, "error", err)
					}
					r.observeLandSpawnToWorking(ctx, &pod, beadID, prev, wrote)
				} else {
					r.logger.Debug("SyncAll: skipping state bump for idle coop agent",
						"bead", beadID, "pod", pod.Name, "bead_state", beadState)
				}
				goto metadata
			}
			// Stuck-agent heuristic (ka-0000001882): when the pod is
			// Running+Ready and coop reports a non-idle, non-error state but
			// none of its liveness signals (since_seq, screen_seq,
			// context_tokens, cost) have advanced for the role's threshold,
			// publish a StuckEvent so the existing AgentStuck handler can
			// restart the pod. The role gates the threshold so long-lived
			// orchestrators get a longer grace window (bug ka-0000002709).
			if podReady && r.checkStagnation(beadID, roleLabel, coopInfo, pod.Name) {
				r.publishStuck(ctx, &pod, beadID)
			}
		}

		// Surface pod scheduling state on the bead while the pod is Pending so
		// the bridge can render "stuck: cluster capacity" instead of a generic
		// spawning spinner, and the stale reaper can detect pods stuck
		// Pending+Unschedulable. Bridge and reaper both gate on agent_state
		// being "spawning"; once the pod advances to Running, the stale value
		// is ignored, so we only need to write while still Pending.
		if !isPrewarmed && pod.Status.Phase == corev1.PodPending {
			val := "false"
			if IsPodSchedulingBlocked(&pod) {
				val = "true"
			}
			if err := r.daemon.UpdateBeadFields(ctx, beadID, map[string]string{
				"pod_scheduling_blocked": val,
			}); err != nil {
				r.logger.Debug("SyncAll: failed to update pod_scheduling_blocked",
					"bead", beadID, "pod", pod.Name, "error", err)
			}
		}

		if prev, wrote, err := r.reportPodStatus(ctx, beadID, status, isPrewarmed); err != nil {
			r.logger.Warn("SyncAll: failed to report pod status",
				"bead", beadID, "pod", pod.Name, "error", err)
		} else {
			r.observeLandSpawnToWorking(ctx, &pod, beadID, prev, wrote)
		}

	metadata:
		// Write backend metadata for coop-enabled pods so ResolveBackend() works
		// after controller restarts. Detect coop by checking for port 8080 on any container.
		// Uses pod IP because individual pods don't have DNS entries (no headless Service).
		if coopPort > 0 && pod.Status.PodIP != "" {
			if err := r.ReportBackendMetadata(ctx, beadID, BackendMetadata{
				PodName:   pod.Name,
				Namespace: pod.Namespace,
				Backend:   "coop",
			}); err != nil {
				r.logger.Warn("SyncAll: failed to report backend metadata",
					"bead", beadID, "pod", pod.Name, "error", err)
			}
		}
	}

	// Flush the current-state pending-pods gauge (GAP-1). One call replaces the
	// whole vector, so (project,role) tuples that were Pending on a prior sweep
	// but are no longer clear correctly. No-op when no recorder is wired.
	if r.events != nil {
		r.events.SetPodsPending(pendingCounts)
		// Same one-call-replaces-the-vector flush for the upstream-error gauge:
		// a category whose agents have all recovered must disappear so the
		// outage alert resolves instead of latching (leg A, ka-0000003454).
		r.events.SetUpstreamErrorActive(upstreamErrorCounts)
	}

	r.gcStagnation(seenBeads)
	r.gcPhaseTracking(seenBeads)
	r.gcUpstreamError(seenBeads)

	r.logger.Info("sync completed", "pods", len(pods.Items))
	return nil
}

// observeLandSpawnToWorking observes gardenio_lands_spawn_to_working_seconds
// for a tenant-land agent whose bead SyncAll just moved from a startup state
// (prev) to working (wrote): now minus the agent bead's created_at, labelled
// with the land. A tenant pod is one in its project's land namespace, where
// the land fence always places it; any other pod is not observed.
//
// It is once per agent. The bead is stamped FieldLandFirstWorkingAt before the
// observation and a bead that already carries it is skipped, so a stuck
// restart or respawn (back through spawning) does not count again, and a
// controller restart does not either: a bead already at working is not a
// startup state. A failed read or stamp skips the observation and leaves the
// bead unstamped; the agent is then not observed, which loses one sample but
// never counts one twice. Poll lag is up to one SyncAll interval.
func (r *HTTPReporter) observeLandSpawnToWorking(ctx context.Context, pod *corev1.Pod, beadID, prev, wrote string) {
	if r.events == nil || wrote != "working" || !isStartupState(prev) {
		return
	}
	land := pod.Labels[podmanager.LabelProject]
	if ns, err := landns.Name(land); err != nil || pod.Namespace != ns {
		return
	}
	reader, ok := r.daemon.(beadReader)
	if !ok {
		return
	}
	bead, err := reader.GetBead(ctx, beadID)
	if err != nil || bead == nil || bead.CreatedAt.IsZero() {
		r.logger.Debug("SyncAll: cannot read agent bead for land spawn-to-working",
			"bead", beadID, "land", land, "error", err)
		return
	}
	if bead.Fields[FieldLandFirstWorkingAt] != "" {
		return
	}
	now := r.now()
	if err := r.daemon.UpdateBeadFields(ctx, beadID, map[string]string{
		FieldLandFirstWorkingAt: now.UTC().Format(time.RFC3339),
	}); err != nil {
		r.logger.Warn("SyncAll: failed to stamp land first-working time; not observing",
			"bead", beadID, "land", land, "error", err)
		return
	}
	r.events.ObserveLandSpawnToWorking(land, now.Sub(bead.CreatedAt).Seconds())
}

// coopAgentInfo summarizes the subset of coop's /api/v1/agent response that
// SyncAll acts on. overrideState, when non-empty, is the bead agent_state to
// propagate (rate_limited or budget_exceeded). idle indicates coop reports
// the agent is parked with no active work — SyncAll uses this to skip the
// normal working-state bump so updated_at ages out and the stale reaper fires.
// sinceSeq/screenSeq feed the stuck-agent heuristic (ka-0000001882).
type coopAgentInfo struct {
	overrideState string
	idle          bool
	sinceSeq      uint64
	screenSeq     uint64
	// contextTokens / costUSD are the live usage telemetry SyncAll mirrors onto
	// the agent bead (context_tokens / live_cost_usd) for the agent cards.
	contextTokens uint64
	costUSD       float64
	// turns is coop's live request_count, mirrored onto the agent bead as
	// live_turns — the live counterpart of total_request_count (ka-0000006449).
	turns uint64
	// errorCategory is coop's raw error_category for this poll ("" when the
	// agent is healthy), normalized onto the bounded metrics enum. Distinct
	// from overrideState: only rate_limited / budget_exceeded map to a bead
	// agent_state pill, but EVERY category — server_error above all — must be
	// counted, which is the gap that made the 2026-07-29 outage invisible
	// fleet-wide (leg A, ka-0000003454).
	errorCategory string
}

// queryCoopAgentState queries a coop pod's /api/v1/agent endpoint and returns
// a summary of the agent state. An empty coopAgentInfo means the agent is
// unreachable or in a normal active state — SyncAll should fall through to
// the usual pod-phase reporting.
func (r *HTTPReporter) queryCoopAgentState(ctx context.Context, podIP string, port int32, podName string) coopAgentInfo {
	url := fmt.Sprintf("http://%s:%d/api/v1/agent", podIP, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return coopAgentInfo{}
	}

	resp, err := r.coopClient.Do(req)
	if err != nil {
		// Connection failures are expected for pods still starting up.
		r.logger.Debug("SyncAll: coop agent query failed",
			"pod", podName, "error", err)
		return coopAgentInfo{}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return coopAgentInfo{}
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return coopAgentInfo{}
	}

	var agentResp coopAgentResponse
	if err := json.Unmarshal(body, &agentResp); err != nil {
		return coopAgentInfo{}
	}

	info := coopAgentInfo{
		idle:          agentResp.State == "idle",
		sinceSeq:      agentResp.SinceSeq,
		screenSeq:     agentResp.ScreenSeq,
		contextTokens: agentResp.ContextTokens,
		costUSD:       agentResp.CostUSD,
		turns:         agentResp.Turns,
	}
	// Capture EVERY reported category for the metrics, independently of the
	// agent_state override below. Only rate_limited / budget_exceeded map to a
	// bead state pill (they are the two that drive the SSE→auth-failover path);
	// server_error, no_internet and the rest are handled in-pod by gb init's
	// dismiss+backoff+re-nudge branches and deliberately get no pill. But every
	// one of them still needs COUNTING — before this, `server_error` fell
	// through this switch and was dropped on the floor, which is precisely why
	// the 2026-07-29 Claude outage was invisible fleet-wide despite the
	// controller recovering agents from it the whole time (ka-0000003454).
	info.errorCategory = gardeniometrics.NormalizeUpstreamErrorCategory(agentResp.ErrorCategory)
	switch agentResp.ErrorCategory {
	case "rate_limited":
		info.overrideState = "rate_limited"
	case "budget_exceeded":
		info.overrideState = "budget_exceeded"
	}
	return info
}

// reportLiveUsage mirrors coop's live context-window size and running cost
// estimate onto the agent bead so the beads-viewer and Slack agent cards can
// show "context so far" / "cost so far" while the agent is still running.
//
// Written to dedicated fields (context_tokens, live_cost_usd) rather than the
// authoritative total_cost_usd, which the session-close reconciler owns
// (reconcile_session_spend.go) — the in-pod cost here is a non-authoritative
// estimate and must not overwrite the reconciled figure. No-op when coop
// reported nothing yet (both zero), so a freshly-started agent doesn't get a
// premature "0 tokens · $0.00" written before its first request lands.
func (r *HTTPReporter) reportLiveUsage(ctx context.Context, beadID, podName string, info coopAgentInfo) {
	if info.contextTokens == 0 && info.costUSD == 0 {
		return
	}
	// Write native JSON types, NOT strings: context_tokens is schema type
	// integer and live_cost_usd is float. The string-map UpdateBeadFields only
	// coerces to a number when the field already holds one, so a first-ever
	// write of these never-seeded fields would be sent as a JSON string and
	// rejected ("must be an integer") — which is why they never landed on any
	// agent bead (ka-0000008019). UpdateBeadFieldsTyped preserves the numeric
	// type so validation passes on the bootstrap write.
	fields := map[string]any{
		"context_tokens": info.contextTokens,
		"live_cost_usd":  info.costUSD,
		// live_turns is the LIVE counterpart of total_request_count: it advances
		// with round-trips while the agent runs (ka-0000006449). Written as a
		// native int (not a string) for the same reason as context_tokens — the
		// string-map path would send this never-seeded integer field as a JSON
		// string and the daemon would reject it.
		"live_turns": info.turns,
	}
	if err := r.daemon.UpdateBeadFieldsTyped(ctx, beadID, fields); err != nil {
		r.logger.Debug("SyncAll: failed to write live usage to bead",
			"bead", beadID, "pod", podName, "error", err)
	}
}

// longLivedStuckRoles are the agent roles whose pods run long enough (hours,
// often orchestrating child agents or hosting a Workflow fan-out) that a quiet
// terminal is expected rather than a wedge, so the stuck heuristic grants them
// a longer grace window (bug ka-0000002709). Kept in sync with the controller's
// account_spread.longLivedRoles; duplicated here to avoid a statusreporter →
// main import.
var longLivedStuckRoles = map[string]bool{
	"gardener":      true,
	"plan-gardener": true,
}

// roleIsLongLived reports whether the primary role in a (possibly
// comma-separated) role string is a long-lived orchestrator role. Mirrors the
// controller's roleIsLongLived: the first role is authoritative.
func roleIsLongLived(role string) bool {
	primary, _, _ := strings.Cut(role, ",")
	return longLivedStuckRoles[strings.TrimSpace(primary)]
}

// stuckThresholdForRole returns the effective stuck threshold for a pod's
// role. Long-lived orchestrator roles (gardener / plan-gardener) legitimately
// go quiet for long stretches while orchestrating child agents or hosting a
// Workflow fan-out, so they get stuckLongLivedMultiplier× the base threshold
// before being flagged as wedged (bug ka-0000002709). A multiplier <= 1
// leaves the base threshold unchanged for every role.
func (r *HTTPReporter) stuckThresholdForRole(role string) time.Duration {
	if r.stuckLongLivedMultiplier > 1 && roleIsLongLived(role) {
		return time.Duration(float64(r.stuckThreshold) * r.stuckLongLivedMultiplier)
	}
	return r.stuckThreshold
}

// checkStagnation updates the per-bead stagnation tracker with the latest coop
// liveness signals and returns true when NONE of them (terminal since_seq /
// screen_seq, context-window growth, or running-cost growth) has advanced for
// at least the role's effective threshold. The first observation of a bead
// initialises lastAdvanceAt to "now" so a freshly-seen pod has at least one
// full threshold window to make progress before being flagged.
//
// Tracking context_tokens/cost alongside the terminal sequences is what keeps
// the heuristic from hard-killing a live agent mid-turn: an agent waiting on a
// long model request (a big Opus turn, a gardener blocked on a child) advances
// neither terminal sequence but is plainly alive because its context window
// and cost keep growing (bug ka-0000002709).
//
// Returns false (and leaves the tracker untouched) when stuck detection is
// disabled (publisher nil or threshold zero).
func (r *HTTPReporter) checkStagnation(beadID, role string, info coopAgentInfo, podName string) bool {
	if r.publisher == nil || r.stuckThreshold <= 0 {
		return false
	}
	r.stagnationMu.Lock()
	defer r.stagnationMu.Unlock()

	now := r.now()
	entry := stagnationEntry{
		sinceSeq:      info.sinceSeq,
		screenSeq:     info.screenSeq,
		contextTokens: info.contextTokens,
		costUSD:       info.costUSD,
		turns:         info.turns,
		lastAdvanceAt: now,
	}
	prev, ok := r.stagnation[beadID]
	if !ok {
		r.stagnation[beadID] = entry
		return false
	}

	advanced := info.sinceSeq != prev.sinceSeq ||
		info.screenSeq != prev.screenSeq ||
		info.contextTokens != prev.contextTokens ||
		info.costUSD != prev.costUSD ||
		info.turns != prev.turns
	if advanced {
		r.stagnation[beadID] = entry
		return false
	}

	// No liveness signal advanced — keep the prior lastAdvanceAt and check the
	// gap against the role's effective threshold.
	threshold := r.stuckThresholdForRole(role)
	if now.Sub(prev.lastAdvanceAt) > threshold {
		r.logger.Warn("SyncAll: detected stuck agent (no liveness advance)",
			"bead", beadID, "pod", podName, "role", role,
			"since_seq", info.sinceSeq, "screen_seq", info.screenSeq,
			"context_tokens", info.contextTokens, "cost_usd", info.costUSD,
			"turns", info.turns,
			"stagnant_for", now.Sub(prev.lastAdvanceAt).Round(time.Second),
			"threshold", threshold)
		return true
	}
	return false
}

// resetStagnation drops any tracker entry for the bead. Called when SyncAll
// observes a state that already has its own recovery path (idle → stale
// reaper, error_category → coop error propagation) so the heuristic does
// not double-flag the same pod.
func (r *HTTPReporter) resetStagnation(beadID string) {
	if r.publisher == nil || r.stuckThreshold <= 0 {
		return
	}
	r.stagnationMu.Lock()
	defer r.stagnationMu.Unlock()
	delete(r.stagnation, beadID)
}

// gcStagnation drops tracker entries for beads no longer present in the
// pod listing. Without this, a deleted pod's entry would linger in memory
// indefinitely.
func (r *HTTPReporter) gcStagnation(seen map[string]struct{}) {
	if r.publisher == nil || r.stuckThreshold <= 0 {
		return
	}
	r.stagnationMu.Lock()
	defer r.stagnationMu.Unlock()
	for beadID := range r.stagnation {
		if _, ok := seen[beadID]; !ok {
			delete(r.stagnation, beadID)
		}
	}
}

// recordPodLifecycleMetrics emits the event-driven pod metrics for one pod
// observed during SyncAll: a pod phase transition (E2.5,
// gardenio_agent_pod_phase_transitions_total) whenever the phase differs from
// the last value seen for this beadID, and an OOM lifecycle event (E2.4,
// gardenio_agent_lifecycle_events_total{event="oom_killed"}) when a container
// in the pod was OOMKilled. Both are deduped via per-beadID state so a pod
// that lingers in a phase for several sync cycles is counted once. No-op when
// no EventRecorder is wired.
func (r *HTTPReporter) recordPodLifecycleMetrics(ctx context.Context, pod *corev1.Pod, beadID, project, role string) {
	if r.events == nil {
		return
	}
	phase := string(pod.Status.Phase)
	oom := podWasOOMKilled(pod)

	r.phaseMu.Lock()
	prev, hadPrev := r.lastPhase[beadID]
	r.lastPhase[beadID] = phase
	_, alreadyOOM := r.oomReported[beadID]
	emitOOM := oom && !alreadyOOM
	if emitOOM {
		r.oomReported[beadID] = struct{}{}
	}
	r.phaseMu.Unlock()

	// Phase transition: only when we have a prior phase and it changed. The
	// first observation of a pod establishes the baseline without emitting
	// (we did not witness the transition into that phase).
	if hadPrev && prev != phase {
		r.events.RecordPodPhaseTransition(prev, phase, project, role)
		// "started" lifecycle event (ka-0000000550): the pod reached Running —
		// the agent process is actually up, vs "spawned" (the create request
		// was issued in handleEvent). Same witnessed-transition-only rule as
		// the phase counter: a pod first observed already Running (controller
		// restart) sets a baseline without emitting, so a controller roll never
		// replays a started burst for the whole fleet. A stuck-restart recreate
		// walks the same beadID through Pending→Running again and counts a
		// second start — which it is. Phase never returns to Running within one
		// pod (container restarts keep phase=Running), so no double-count
		// within a pod's life.
		if phase == string(corev1.PodRunning) {
			r.events.RecordLifecycleEvent("started", project, role, "")
		}
	}

	if emitOOM {
		// Best-effort session_id drill-down label (FSM contract §6). A lookup
		// failure must not block the OOM count — emit with an empty session_id.
		sessionID, err := r.daemon.GetAgentSessionID(ctx, beadID)
		if err != nil {
			r.logger.Debug("SyncAll: failed to resolve session_id for OOM event",
				"bead", beadID, "pod", pod.Name, "error", err)
			sessionID = ""
		}
		r.events.RecordLifecycleEvent("oom_killed", project, role, sessionID)
		r.logger.Info("SyncAll: recorded agent OOM lifecycle event",
			"bead", beadID, "pod", pod.Name, "project", project, "role", role)
	}
}

// gcPhaseTracking drops phase/OOM tracker entries for beads no longer present
// in the pod listing, mirroring gcStagnation. Prevents unbounded growth of the
// per-beadID maps as pods come and go.
func (r *HTTPReporter) gcPhaseTracking(seen map[string]struct{}) {
	r.phaseMu.Lock()
	defer r.phaseMu.Unlock()
	for beadID := range r.lastPhase {
		if _, ok := seen[beadID]; !ok {
			delete(r.lastPhase, beadID)
			delete(r.oomReported, beadID)
			delete(r.oomStamped, beadID)
		}
	}
}

// trackUpstreamError updates the per-bead upstream-error tracker with this
// sweep's observed category and fires the edge-triggered transition counter when
// the agent has just ENTERED an error category it was not already in.
//
// Called once per running coop-enabled pod per sweep, with category=="" for a
// healthy agent (which clears the tracker so a later recurrence counts as a
// fresh entry). A category that persists across sweeps increments nothing —
// that sustained state is what the active gauge measures. A category that
// CHANGES (e.g. server_error → rate_limited) counts as an entry into the new
// one, since it is a distinct failure the oncall may need to see.
func (r *HTTPReporter) trackUpstreamError(beadID, category, project, role string) {
	if r.events == nil {
		return
	}
	r.upstreamErrorMu.Lock()
	previous := r.lastUpstreamError[beadID]
	if category == "" {
		delete(r.lastUpstreamError, beadID)
	} else {
		r.lastUpstreamError[beadID] = category
	}
	r.upstreamErrorMu.Unlock()

	if category == "" || category == previous {
		return
	}
	if r.events.RecordUpstreamErrorTransition(category, project, role) {
		r.logger.Info("agent entered upstream error category",
			"bead", beadID, "category", category, "project", project, "role", role,
			"previous_category", previous)
	}
}

// gcUpstreamError drops tracker entries for beads no longer present in the pod
// listing, mirroring gcStagnation / gcPhaseTracking. Without it a deleted pod's
// entry would linger in memory indefinitely, and a bead ID reused after a gap
// would suppress a legitimate transition count.
func (r *HTTPReporter) gcUpstreamError(seen map[string]struct{}) {
	r.upstreamErrorMu.Lock()
	defer r.upstreamErrorMu.Unlock()
	for beadID := range r.lastUpstreamError {
		if _, ok := seen[beadID]; !ok {
			delete(r.lastUpstreamError, beadID)
		}
	}
}

// podWasOOMKilled reports whether any container in the pod was terminated with
// reason OOMKilled — either in its current state or its last-termination state
// (the latter covers a container that OOMed and was restarted). This is the
// kubelet's own signal, surfaced on the pod's container statuses.
func podWasOOMKilled(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	statuses := make([]corev1.ContainerStatus, 0,
		len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	for _, cs := range statuses {
		if t := cs.State.Terminated; t != nil && t.Reason == "OOMKilled" {
			return true
		}
		if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
			return true
		}
	}
	return false
}

// publishStuck emits a StuckEvent for the given pod and clears its
// stagnation entry so the next SyncAll cycle (post-restart) starts a fresh
// threshold window.
func (r *HTTPReporter) publishStuck(ctx context.Context, pod *corev1.Pod, beadID string) {
	if r.publisher == nil {
		return
	}
	mode := pod.Labels[podmanager.LabelMode]
	if mode == "" {
		mode = "crew"
	}
	ev := StuckEvent{
		BeadID:    beadID,
		Project:   pod.Labels[podmanager.LabelProject],
		Role:      pod.Labels[podmanager.LabelRole],
		Mode:      mode,
		AgentName: pod.Labels[podmanager.LabelAgent],
		Reason:    fmt.Sprintf("no coop seq advance for %s", r.stuckThreshold),
	}
	r.publisher.PublishStuck(ctx, ev)
	r.resetStagnation(beadID)
}

// Metrics returns a snapshot of current metric values.
func (r *HTTPReporter) Metrics() MetricsSnapshot {
	return MetricsSnapshot{
		StatusReportsTotal: r.reportsTotal.Load(),
		StatusReportErrors: r.reportErrors.Load(),
		SyncAllRuns:        r.syncRuns.Load(),
		SyncAllErrors:      r.syncErrors.Load(),
	}
}
