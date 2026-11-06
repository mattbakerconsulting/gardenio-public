package reconciler

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
)

// Phase 0 measurement (ka-0000006778): instrument the idle:active agent ratio
// and the memory reserved by idle agents. This is the baseline that quantifies
// how much reserved capacity idle agents waste today — the number the
// agent-substrate freeze/thaw work (epic ka-0000003796, Phases 1-4) has to beat
// to be worth shipping. The freeze/thaw substrate reclaims an idle agent's
// pod (default 2Gi request, podmanager.DefaultMemoryRequest) by checkpointing
// it out of the cluster; before we can claim a saving we must first measure the
// reservation being held while an agent does no work.
//
// The reconcile pass is the one place that already holds BOTH halves of this
// join in memory every cycle: the desired agent beads (which carry agent_state)
// AND the live pods (which carry the real memory requests kubelet reserves).
// Deriving the measurement here reuses that data with zero extra daemon or K8s
// queries — the same discipline the DeferredMetrics gauge follows, and the
// paging/OOM guard advice (ka-0000003555) demands. Emission is a pure Reset +
// Set over the in-hand maps, so it cannot page, fan out, or OOM.

// IdleMetrics exposes Prometheus gauges measuring the idle:active split of the
// agent fleet and the pod memory reserved by each half. Both are labelled by
// {project, activity} where activity is "active" or "idle" (see
// classifyActivity), so a dashboard reads the idle:active ratio and the idle
// memory waste directly, per project and fleet-wide.
type IdleMetrics struct {
	// AgentsByActivity is the count of desired agents split by activity.
	// gardenio_agents_by_activity{project,activity="idle"} /
	// {activity="active"} is the idle:active ratio. Reset+Set each pass so a
	// drained project reads absent/0.
	AgentsByActivity *prometheus.GaugeVec

	// ReservedMemoryBytes is the sum of pod memory REQUESTS (the bytes kubelet
	// reserves, not live RSS) over agents that have a live pod, split by
	// activity. The activity="idle" series is the headline "idle memory waste"
	// figure: memory held by agents that are doing no work and are exactly the
	// population freeze/thaw would checkpoint out. Only agents with a matching
	// pod contribute — a bead with no pod reserves nothing.
	ReservedMemoryBytes *prometheus.GaugeVec
}

// NewIdleMetrics creates the idle/active measurement gauges.
func NewIdleMetrics() *IdleMetrics {
	return &IdleMetrics{
		AgentsByActivity: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "agents_by_activity",
			Help:      "Desired agents split by activity (active|idle). The idle/active series ratio is the idle:active agent ratio (Phase 0 measurement, ka-0000006778).",
		}, []string{"project", "activity"}),

		ReservedMemoryBytes: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace,
			Name:      "agent_reserved_memory_bytes",
			Help:      "Sum of pod memory requests (bytes kubelet reserves) over agents that have a live pod, split by activity (active|idle). The activity=idle series is the idle memory waste — reserved capacity held by agents doing no work (Phase 0 measurement, ka-0000006778).",
		}, []string{"project", "activity"}),
	}
}

// Register registers the idle metrics with the given registerer.
func (m *IdleMetrics) Register(reg prometheus.Registerer) error {
	if err := reg.Register(m.AgentsByActivity); err != nil {
		return err
	}
	return reg.Register(m.ReservedMemoryBytes)
}

// activeAgentStates is the set of agent_state values that mean the agent is
// doing (or is about to do) real work — its reserved capacity is earning its
// keep and is NOT a freeze/thaw candidate. Everything else a live agent bead
// can carry is treated as idle by classifyActivity's default branch.
//
//   - working:   actively running a turn.
//   - spawning:  booting; the pod is coming up to do work imminently.
//   - assigning: a pool member being handed a thread (AssignPrewarmed) — about
//     to work; counting it idle would flap on every assignment.
//   - resuming:  a microvm agent being rehydrated to continue work
//     (microvm.AgentStateResuming).
//
// "yielding" is deliberately NOT here: a yielding agent is blocked on a human
// decision (gb yield) and holds its full pod reservation while doing no work —
// exactly the waste Phase 0 must surface and Phase 1 (coop snapshot-suspend for
// idle/yielded agents, ka-0000002921) targets. See docs/idle-active-measurement.md.
var activeAgentStates = map[string]struct{}{
	"working":   {},
	"spawning":  {},
	"assigning": {},
	"resuming":  {},
}

// classifyActivity maps an agent_state to "active" or "idle". Active states are
// the enumerated work-in-progress set; every other value — idle, prewarmed,
// queued, yielding, rate_limited, budget_exceeded, stopping, and any unknown
// future state — is treated as idle (reserving capacity without doing work).
// Defaulting unknown states to idle is the conservative choice for a WASTE
// measurement: a new resting state we forget to enumerate shows up as waste to
// investigate rather than silently vanishing from the idle figure.
func classifyActivity(agentState string) string {
	if _, ok := activeAgentStates[agentState]; ok {
		return "active"
	}
	return "idle"
}

// podMemoryRequestBytes returns the total memory request across a pod's app
// containers (init containers are short-lived and excluded). This is the
// capacity kubelet RESERVES for the pod for its whole life — the quantity an
// idle agent wastes and freeze/thaw reclaims — not live RSS. A container with
// no explicit memory request contributes 0.
func podMemoryRequestBytes(pod *corev1.Pod) int64 {
	var total int64
	for i := range pod.Spec.Containers {
		if q, ok := pod.Spec.Containers[i].Resources.Requests[corev1.ResourceMemory]; ok {
			total += q.Value()
		}
	}
	return total
}

// recordIdleMetrics emits the Phase 0 idle:active + idle-memory-waste gauges for
// one reconcile pass. desired is the agent-bead map (podName → bead) and
// actualMap is the live-pod map (podName → pod) the pass already built; the two
// share the same podName key, which is exactly the join needed to attribute a
// pod's reserved memory to its agent's activity. Terminal/finished pods
// (Failed/Succeeded) are excluded: they are being torn down this pass and their
// reservation is releasing, so counting them would double-book against the
// recreated pod. Reset-then-Set so departed agents and drained projects fall to
// absent/0 rather than sticking at a stale value.
func (r *Reconciler) recordIdleMetrics(desired map[string]beadsapi.AgentBead, actualMap map[string]corev1.Pod) {
	if r.idleMetrics == nil {
		return
	}
	r.idleMetrics.AgentsByActivity.Reset()
	r.idleMetrics.ReservedMemoryBytes.Reset()

	type acc struct {
		count    int
		memBytes int64
	}
	// Pre-seed both activity buckets per project so a project with only active
	// (or only idle) agents still emits an explicit 0 for the other bucket. That
	// keeps the idle:active ratio a well-defined division on the dashboard
	// instead of a divide-by-absent, and makes "0 idle agents" a visible fact
	// rather than a missing series.
	buckets := map[string]map[string]*acc{}
	bucketFor := func(project, activity string) *acc {
		if project == "" {
			project = "unknown"
		}
		byActivity, ok := buckets[project]
		if !ok {
			byActivity = map[string]*acc{"active": {}, "idle": {}}
			buckets[project] = byActivity
		}
		return byActivity[activity]
	}

	for podName, bead := range desired {
		activity := classifyActivity(bead.AgentState)
		b := bucketFor(bead.Project, activity)
		b.count++

		// Attribute reserved memory only when the agent actually has a live
		// (non-terminal) pod holding a reservation. A deferred/podless agent
		// reserves nothing yet, so it contributes to the count but not to the
		// wasted-bytes figure.
		if pod, ok := actualMap[podName]; ok &&
			pod.Status.Phase != corev1.PodFailed &&
			pod.Status.Phase != corev1.PodSucceeded {
			b.memBytes += podMemoryRequestBytes(&pod)
		}
	}

	for project, byActivity := range buckets {
		for activity, a := range byActivity {
			r.idleMetrics.AgentsByActivity.WithLabelValues(project, activity).Set(float64(a.count))
			r.idleMetrics.ReservedMemoryBytes.WithLabelValues(project, activity).Set(float64(a.memBytes))
		}
	}
}

// idleActiveSummary rolls up the fleet-wide idle:active split and the idle
// reserved-memory total for one reconcile pass, so the measurement is visible
// in the reconcile-complete controller log without a Grafana round-trip. It
// walks the same desired/actual maps recordIdleMetrics does; both are cheap
// map walks over the in-hand data.
func idleActiveSummary(desired map[string]beadsapi.AgentBead, actualMap map[string]corev1.Pod) (active, idle int, idleMemBytes int64) {
	for podName, bead := range desired {
		isActive := classifyActivity(bead.AgentState) == "active"
		if isActive {
			active++
		} else {
			idle++
		}
		if isActive {
			continue
		}
		if pod, ok := actualMap[podName]; ok &&
			pod.Status.Phase != corev1.PodFailed &&
			pod.Status.Phase != corev1.PodSucceeded {
			idleMemBytes += podMemoryRequestBytes(&pod)
		}
	}
	return active, idle, idleMemBytes
}
