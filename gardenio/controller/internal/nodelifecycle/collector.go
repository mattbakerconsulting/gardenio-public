// Package nodelifecycle emits a counter of node-lifecycle transitions
// (gardenio_node_lifecycle_events_total{reason=...}) so the fleet has a live,
// alertable signal for the class of node churn that force-kills agents mid-work
// — spot interruption/rebalance, AWS EC2 health/retirement events, Karpenter
// disruption (consolidation / drift / AMI replacement / expireAfter TTL), and
// outright node deletion.
//
// The collector also carries two fleet-COST signals over the same node list:
// the fleet-size gauges (agent_node_count / agent_node_vcpus, P1b ka-0000003023)
// and, when idle-node detection is enabled, agent_idle_node_count — the count of
// agent-pool nodes hosting zero active agent pods (P1b-follow-up ka-0000001398).
// Idle-node detection is the UNBLOCKED half of the node-utilization ask:
// "provisioned but nothing scheduled" is derivable from the node list this
// collector already runs plus a cheap namespace-scoped agent-pod LIST (the
// controller's existing pods:list RBAC), with NO per-node CPU/mem series and NO
// new scrape. Real sustained-low CPU/mem UTILIZATION (a busy-looking node whose
// pods barely use their requests) stays a blocked follow-up — that needs
// node-exporter/cadvisor/Container-Insights series that are not in vmsingle
// today (the in-chart vmagent scrapes only annotated gardenio-system pods).
//
// Why a controller-emitted metric (Matt Baker, ka-0000006448 / ka-0000001509):
// the operator asked for monitoring + Slack alerts on OOMKill AND node-lifecycle
// events, and explicitly OK'd creating a metric if none exists. None did. The
// existing node signal — gardenio_agent_involuntary_deaths_total{family=
// "context_cancelled"} (AgentNodeReclaimDeaths) — only fires when a node
// teardown actually KILLS an agent; it says nothing about node churn that
// happens to miss the agents. The karpenter_nodes_{created,terminated}_total
// series would carry node lifecycle, but they reach vmsingle ONLY when the
// optional cross-namespace karpenter scrape is enabled (vmagent.karpenterScrape,
// off in the base chart), so an alert on them is silent wherever that overlay is
// absent. This collector needs no scrape config: it publishes on the controller
// :9092 /metrics endpoint that ALREADY carries every gardenio_agent_* series on
// prod (prometheus.io/scrape=true), so the node-lifecycle series is guaranteed
// present wherever the OOM series is.
//
// Mechanism: a periodic sweep of the cluster Node API (list), gated behind a
// values flag that also provisions the cluster-scoped nodes:list/watch RBAC the
// controller SA does not otherwise hold. Unlike a poller gauge, this is a
// monotonic COUNTER incremented on edge transitions
// (a node newly entering a lifecycle state), so increase()/rate() over a window
// answer "how many node teardowns in the last hour" — the exact shape the
// AgentNodeReclaimDeaths alert already uses. Counters are NEVER reset (the
// internal/metrics events.go contract): resetting breaks increase().
//
// Scoping (ka-0000003804): the sweep is scoped to a caller-supplied label
// selector (nodeSelector) rather than every cluster node. The alert is about
// churn that can COLD-RESUME an agent, so it must measure only the nodes agents
// actually run on — the Karpenter agent-pool (karpenter.sh/nodepool=agent-pool).
// Un-scoped, the consolidated prod cluster also carries the ephemeral-pool
// (per-MR/per-commit E2E staging namespaces, created + reaped every pipeline)
// and any other pool, whose teardown is EXPECTED BACKGROUND by design and has
// nothing to do with agents — summing it made the metric (and the threshold set
// against it) meaningless and tripped AgentNodeLifecycleEvents on its first
// firing. An empty selector preserves the list-every-node behavior as an
// explicit opt-out; the chart ships the agent-pool selector as the default.
package nodelifecycle

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// karpenterDisruptedTaintPrefix is the taint-key prefix Karpenter stamps on a
// node it is tearing down (karpenter.sh/disrupted, karpenter.sh/disruption).
// Kept in sync with reconciler.karpenterDisruptedTaintPrefix (WS1, upgrade.go) —
// the reconciler drains agents off a node carrying this taint, and this
// collector counts the same going-away signal so a node teardown is observable
// even when no agent was on the node to drain.
const karpenterDisruptedTaintPrefix = "karpenter.sh/disrupt"

// Lifecycle reason enum — the only values allowed as the `reason` label. Bounded
// so the series cardinality stays at {env} x {these}. The going-away classes
// (disrupting, deleting, terminated) are what the AgentNodeLifecycleEvents alert
// sums; not_ready is a node-health transition kept for the dashboard breakdown
// (it can flap transiently, so it is deliberately excluded from the default
// alert expr).
const (
	// ReasonDisrupting: a Karpenter disruption taint appeared on the node
	// (consolidation, drift/AMI replacement, expireAfter TTL, or an
	// interruption-queue reclaim). The node will be drained and removed.
	ReasonDisrupting = "disrupting"
	// ReasonDeleting: the Node object's DeletionTimestamp was set — the node is
	// being deleted from the cluster (Karpenter finalizing a teardown, or a
	// direct kubectl/AWS-driven delete).
	ReasonDeleting = "deleting"
	// ReasonTerminated: a node observed in a prior sweep is gone from the
	// listing — the instance was reclaimed/terminated (spot reclaim, AWS health
	// event, scale-down). The terminal node-lifecycle signal.
	ReasonTerminated = "terminated"
	// ReasonNotReady: the node's Ready condition left True (kubelet unhealthy,
	// network partition, imminent EC2 health failure). A health transition, NOT
	// a guaranteed teardown — excluded from the default alert (can flap).
	ReasonNotReady = "not_ready"
)

// nodeState is the set of lifecycle flags observed for one node on a sweep. The
// collector diffs the previous sweep's flags against the current to detect the
// false->true edge that constitutes a countable event.
type nodeState struct {
	disrupting bool
	deleting   bool
	notReady   bool
}

// Collector periodically lists cluster Nodes, detects lifecycle-state edge
// transitions per node, and increments gardenio_node_lifecycle_events_total.
type Collector struct {
	client   kubernetes.Interface
	env      string
	interval time.Duration
	logger   *slog.Logger

	// nodeSelector is the label selector applied to every node LIST so the
	// metric counts only the nodepool agents run on (default in the chart:
	// karpenter.sh/nodepool=agent-pool). Empty = list every cluster node
	// (explicit opt-out). See the package doc for why scoping matters.
	nodeSelector string

	// podNamespaces are the namespaces the collector LISTs agent pods in each
	// sweep to derive the idle-node signal (P1b-follow-up, ka-0000001398): an
	// agent-pool node hosting zero active agent pods is provisioned-but-idle —
	// paying On-Demand for nothing until Karpenter consolidates it. Empty =
	// idle-node detection DISABLED (the idleNodeCount gauge is neither registered
	// nor emitted, so no alert evaluates against a phantom series); the chart
	// passes the release namespace (+ the gVisor namespace when configured),
	// where every agent pod carries app.kubernetes.io/name=gardenio. Uses the
	// controller's EXISTING namespace-scoped pods:list RBAC — no new grant, no
	// new scrape (contrast real CPU/mem usage util, which stays blocked on
	// per-node series absent from vmsingle; see the package doc).
	podNamespaces []string

	// events is the monotonic counter, keyed on {reason, env}. Never Reset()
	// (the internal/metrics counter contract): increase()/rate() interpret a
	// drop as a reset and double-count.
	events *prometheus.CounterVec

	// nodeCount / nodeVCPUs are current-state GAUGES of the live agent-pool
	// fleet size (Deliverable 1 of P1b fleet-size alerting, ka-0000003023): the
	// number of agent-pool nodes and their summed vCPU capacity as of the last
	// sweep. These are the LEADING cost indicators Matt named ("alert when
	// running agent-node count / total vCPU provisioned exceeds a threshold") —
	// they rise the moment Karpenter provisions a node, hours before the
	// On-Demand BoxUsage lands on the bill. Free to compute: the collector
	// already LISTs exactly these nodes every sweep for the lifecycle counter,
	// so the gauges are a tally over the same list, on the same already-scraped
	// :9092 endpoint (no karpenter_* cross-namespace scrape, no CloudWatch
	// Container Insights agent). Label-less-but-env: a single series each,
	// overwritten (Set) each sweep — the poller-gauge pattern,
	// contrast the events counter that must never be reset.
	nodeCount *prometheus.GaugeVec
	nodeVCPUs *prometheus.GaugeVec

	// idleNodeCount is the current-state GAUGE of agent-pool nodes hosting ZERO
	// active agent pods (P1b-follow-up idle-node detection, ka-0000001398). It is
	// the unblocked half of the utilization ask: "provisioned but nothing
	// scheduled" is derivable from the node list the collector already runs plus
	// a cheap agent-pod list (the controller's existing pods:list RBAC), with NO
	// per-node CPU/mem series and NO new scrape. A node in this count is On-Demand
	// capacity Karpenter has not yet consolidated — the direct idle-cost signal.
	// Backs the AgentFleetIdleNodes Slack alert into #aws-cost-management.
	// Current-state gauge overwritten each sweep; nil (never registered/emitted)
	// when podNamespaces is empty, so the alert is armed only where the signal is
	// real. Real sustained-low CPU/mem UTILIZATION (as opposed to zero-scheduled
	// idle) remains a blocked follow-up — it needs node-exporter/cadvisor series
	// absent from vmsingle today.
	idleNodeCount *prometheus.GaugeVec

	// prev holds the last-observed lifecycle flags per node name. The first time
	// a node is seen its flags are recorded WITHOUT emitting (baseline), so a
	// controller restart mid-disruption does not replay a burst of phantom
	// events for every already-tainted node — mirrors the witnessed-transition-
	// only rule in statusreporter.recordPodLifecycleMetrics. A node absent from
	// a later sweep but present here is counted as terminated, then dropped.
	prev map[string]nodeState
}

// New constructs a node-lifecycle collector. env is the coarse cross-estate join
// key (GARDENIO_ENV), attached to every series as the internal/metrics catalog
// mandates. nodeSelector scopes the node LIST to the agent nodepool (empty =
// every cluster node; see the package doc). podNamespaces are the namespaces
// swept for agent pods to derive the idle-node gauge (empty = idle-node
// detection disabled; the idleNodeCount gauge is not created, registered, or
// emitted). The metrics are created here but exported only once Register is
// called on the controller's Prometheus registry.
func New(client kubernetes.Interface, env string, interval time.Duration, nodeSelector string, podNamespaces []string, logger *slog.Logger) *Collector {
	c := &Collector{
		client:        client,
		env:           env,
		interval:      interval,
		nodeSelector:  nodeSelector,
		podNamespaces: podNamespaces,
		logger:        logger,
		prev:          make(map[string]nodeState),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "gardenio",
			Name:      "node_lifecycle_events_total",
			Help:      "Count of observed agent-nodepool node lifecycle transitions by reason (disrupting/deleting/terminated/not_ready), incremented on the false->true edge per node. Scoped to the agent nodepool (karpenter.sh/nodepool=agent-pool) so it excludes ephemeral-pool/reaper churn that cannot cold-resume an agent. Backs the AgentNodeLifecycleEvents Slack alert (ka-0000001509). Emitted on the controller :9092 /metrics endpoint, which is scraped on prod without any extra scrape config — unlike karpenter_* which needs the optional cross-namespace scrape.",
		}, []string{"reason", "env"}),
		nodeCount: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gardenio",
			Name:      "agent_node_count",
			Help:      "Current number of agent-pool nodes (karpenter.sh/nodepool=agent-pool) live as of the last collector sweep. The leading fleet-size cost indicator (P1b, ka-0000003023): rises the moment Karpenter provisions a node, hours before the On-Demand BoxUsage bill lands. Backs the AgentFleetSizeHigh Slack alert into #aws-cost-management. Current-state gauge overwritten each sweep (0 on a drained fleet); scoped to the same agent nodepool as node_lifecycle_events_total and served on the same already-scraped :9092 endpoint — no karpenter_* cross-namespace scrape or CloudWatch Container Insights agent required.",
		}, []string{"env"}),
		nodeVCPUs: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gardenio",
			Name:      "agent_node_vcpus",
			Help:      "Current total vCPU capacity provisioned across all agent-pool nodes (sum of node.Status.Capacity.cpu), as of the last collector sweep. The $/hr-proportional half of the P1b fleet-size signal (ka-0000003023): total provisioned vCPU tracks On-Demand spend more directly than a raw node count when instance types are mixed (r7i.2xlarge vs m5.large). Current-state gauge overwritten each sweep; same agent-pool scope + :9092 endpoint as agent_node_count.",
		}, []string{"env"}),
	}
	// Idle-node detection is opt-in: only construct the gauge when at least one
	// pod namespace is configured, so where it is off the series is genuinely
	// absent (an alert on it yields no result) rather than a constant 0 that
	// could read as "no idle nodes" on a cluster the collector never measured.
	if len(podNamespaces) > 0 {
		c.idleNodeCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "gardenio",
			Name:      "agent_idle_node_count",
			Help:      "Current number of agent-pool nodes (karpenter.sh/nodepool=agent-pool) hosting ZERO active agent pods as of the last collector sweep — provisioned-but-idle On-Demand capacity Karpenter has not yet consolidated (P1b-follow-up idle-node detection, ka-0000001398). Derived from the node list the collector already runs plus a namespace-scoped agent-pod list (app.kubernetes.io/name=gardenio), using the controller's existing pods:list RBAC — no per-node CPU/mem series and no new scrape (the unblocked half of the utilization ask; real sustained-low-usage util stays blocked on node-exporter/cadvisor series absent from vmsingle). Backs the AgentFleetIdleNodes Slack alert into #aws-cost-management. Current-state gauge overwritten each sweep; only present when idle-node detection is enabled (pod namespaces configured).",
		}, []string{"env"})
	}
	return c
}

// Register registers the counter and the fleet-size gauges with reg. Served
// alongside the other controller metrics on the :9092 /metrics endpoint. The
// idle-node gauge is registered only when idle-node detection is enabled (pod
// namespaces configured); otherwise the series stays absent so an alert on it
// yields no result rather than a phantom constant 0.
func (c *Collector) Register(reg prometheus.Registerer) error {
	if err := reg.Register(c.events); err != nil {
		return err
	}
	if err := reg.Register(c.nodeCount); err != nil {
		return err
	}
	if err := reg.Register(c.nodeVCPUs); err != nil {
		return err
	}
	if c.idleNodeCount != nil {
		if err := reg.Register(c.idleNodeCount); err != nil {
			return err
		}
	}
	return nil
}

// nodeIsNotReady reports whether the node's Ready condition is anything other
// than True (False/Unknown/absent). A node with no Ready condition yet
// (just-registered) is treated as not-ready.
func nodeIsNotReady(node *corev1.Node) bool {
	for _, cond := range node.Status.Conditions {
		if cond.Type == corev1.NodeReady {
			return cond.Status != corev1.ConditionTrue
		}
	}
	return true
}

// nodeHasDisruptTaint reports whether the node carries a Karpenter disruption
// taint with a scheduling-affecting effect. Mirrors reconciler.nodeIsDoomed's
// taint check.
func nodeHasDisruptTaint(node *corev1.Node) bool {
	for _, t := range node.Spec.Taints {
		if strings.HasPrefix(t.Key, karpenterDisruptedTaintPrefix) &&
			(t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute) {
			return true
		}
	}
	return false
}

// stateOf computes the current lifecycle flags for a node.
func stateOf(node *corev1.Node) nodeState {
	return nodeState{
		disrupting: nodeHasDisruptTaint(node),
		deleting:   node.DeletionTimestamp != nil,
		notReady:   nodeIsNotReady(node),
	}
}

// nodeVCPUCapacity returns the node's advertised CPU capacity in whole vCPUs (cores),
// read from node.Status.Capacity.cpu. A node with no CPU capacity reported yet
// (just-registered, capacity not populated) contributes 0. Capacity (not
// Allocatable) is used deliberately: the fleet-size cost signal is about what
// EC2 is BILLING for — the whole instance's vCPUs — not the reservable
// remainder net of system daemons.
func nodeVCPUCapacity(node *corev1.Node) float64 {
	q, ok := node.Status.Capacity[corev1.ResourceCPU]
	if !ok {
		return 0
	}
	// CPU is expressed in cores (may be milli-cores, e.g. "8" or "8000m");
	// AsApproximateFloat64 normalizes both to a whole-core float.
	return q.AsApproximateFloat64()
}

// inc increments the counter for one reason.
func (c *Collector) inc(reason string) {
	c.events.WithLabelValues(reason, c.env).Inc()
}

// Collect runs a single sweep: it lists the scoped nodes (c.nodeSelector — the
// agent nodepool, empty = every node), diffs each against its last observed
// lifecycle state to count false->true edges, counts nodes that vanished since
// the previous sweep as terminated, and replaces the tracked state. On a list
// error the tracked state and counters are left UNTOUCHED — a transient
// apiserver failure must not fabricate terminated events for every node (they
// would all look "vanished") nor reset the baseline. err is non-nil only on a
// list failure.
//
// The selector is applied server-side so a node LEAVING the scope (relabelled
// off the agent pool — rare) reads identically to a vanished node: gone from
// the listing, counted terminated once, then dropped. That is the correct
// signal — a node no longer in the agent pool can no longer host an agent.
func (c *Collector) Collect(ctx context.Context) error {
	nodeList, err := c.client.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: c.nodeSelector})
	if err != nil {
		c.logger.Error("failed to list nodes for node-lifecycle collector", "error", err)
		return err
	}

	seen := make(map[string]nodeState, len(nodeList.Items))
	var totalVCPUs float64
	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		cur := stateOf(node)
		seen[node.Name] = cur

		// Tally the fleet-size gauges (P1b, ka-0000003023) over the same list.
		// CPU capacity is the schedulable vCPU the instance advertises; summed
		// across the pool it is the $/hr-proportional leading cost indicator.
		totalVCPUs += nodeVCPUCapacity(node)

		prev, hadPrev := c.prev[node.Name]
		if !hadPrev {
			// First observation: establish a baseline without emitting so a
			// controller restart does not replay one event per already-tainted
			// node. The next sweep counts any genuine new transition.
			continue
		}
		// Emit on each false->true edge.
		if cur.disrupting && !prev.disrupting {
			c.inc(ReasonDisrupting)
			c.logger.Info("node entering disruption", "node", node.Name, "reason", ReasonDisrupting)
		}
		if cur.deleting && !prev.deleting {
			c.inc(ReasonDeleting)
			c.logger.Info("node marked for deletion", "node", node.Name, "reason", ReasonDeleting)
		}
		if cur.notReady && !prev.notReady {
			c.inc(ReasonNotReady)
			c.logger.Info("node left Ready state", "node", node.Name, "reason", ReasonNotReady)
		}
	}

	// Nodes present last sweep but gone now were terminated/reclaimed.
	for name := range c.prev {
		if _, still := seen[name]; !still {
			c.inc(ReasonTerminated)
			c.logger.Info("node terminated (gone from listing)", "node", name, "reason", ReasonTerminated)
		}
	}

	// Overwrite the current-state fleet-size gauges each sweep (poller-gauge
	// pattern): the live agent-pool node count and its summed vCPU capacity. A
	// drained fleet correctly reports 0, not a stale high-water mark. Set only
	// on a successful list — an early return above leaves the last-known values
	// intact rather than flashing 0 on a transient apiserver blip (which would
	// spuriously resolve AgentFleetSizeHigh).
	c.nodeCount.WithLabelValues(c.env).Set(float64(len(nodeList.Items)))
	c.nodeVCPUs.WithLabelValues(c.env).Set(totalVCPUs)

	// Idle-node detection (P1b-follow-up, ka-0000001398): count agent-pool nodes
	// hosting zero active agent pods. Runs only when enabled (gauge constructed)
	// and reuses the agent-pool node names from THIS sweep's list (seen), so the
	// idle set is diffed against the exact fleet just measured. A pod-list failure
	// leaves the last-known idle gauge intact rather than flashing a wrong value.
	if c.idleNodeCount != nil {
		c.updateIdleNodeGauge(ctx, seen)
	}

	c.prev = seen
	return nil
}

// gardenioPodSelector selects agent pods: the standard app label every agent pod
// carries (podmanager.LabelApp = app.kubernetes.io/name = gardenio). Kept as a
// string literal rather than importing podmanager to avoid a dependency cycle —
// the collector is a leaf that podmanager's package must not pull in.
const gardenioPodSelector = "app.kubernetes.io/name=gardenio"

// updateIdleNodeGauge lists active agent pods across the configured namespaces,
// marks each agent-pool node that hosts one as busy, and sets the idle-node
// gauge to the count of agent-pool nodes hosting none. agentNodes is the set of
// agent-pool node names observed in the current sweep (keys of `seen`).
//
// A pod is "active" if it is not in a terminal phase (Succeeded/Failed): a node
// hosting only completed pods is idle capacity. On any namespace list error the
// gauge is left UNTOUCHED — a transient apiserver failure must not flash a wrong
// idle count that could spuriously fire or resolve AgentFleetIdleNodes.
func (c *Collector) updateIdleNodeGauge(ctx context.Context, agentNodes map[string]nodeState) {
	busy := make(map[string]struct{}, len(agentNodes))
	for _, ns := range c.podNamespaces {
		if ns == "" {
			continue
		}
		podList, err := c.client.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: gardenioPodSelector})
		if err != nil {
			c.logger.Warn("idle-node detection: failed to list agent pods; leaving idle gauge unchanged", "namespace", ns, "error", err)
			return
		}
		for i := range podList.Items {
			pod := &podList.Items[i]
			if pod.Spec.NodeName == "" {
				continue // unscheduled — not yet occupying a node
			}
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				continue // terminal — no longer holding the node busy
			}
			busy[pod.Spec.NodeName] = struct{}{}
		}
	}

	var idle int
	for name := range agentNodes {
		if _, isBusy := busy[name]; !isBusy {
			idle++
		}
	}
	c.idleNodeCount.WithLabelValues(c.env).Set(float64(idle))
}

// RunLoop runs an immediate baseline sweep, then re-sweeps every interval until
// ctx is cancelled. A non-positive interval is a no-op (the collector is
// disabled) — the caller should simply not construct one, but this guards the
// direct call.
func (c *Collector) RunLoop(ctx context.Context) {
	if c.interval <= 0 {
		return
	}

	if err := c.Collect(ctx); err == nil {
		c.logger.Info("node-lifecycle collector started", "interval", c.interval, "env", c.env, "nodes", len(c.prev))
	}

	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			_ = c.Collect(ctx)
		case <-ctx.Done():
			return
		}
	}
}
