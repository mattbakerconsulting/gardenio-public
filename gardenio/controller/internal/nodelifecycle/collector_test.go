package nodelifecycle

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// agentPoolSelector is the default scope the chart ships — the label Karpenter
// stamps on every node it provisions from the agent-pool NodePool.
const agentPoolSelector = "karpenter.sh/nodepool=agent-pool"

// readyNode builds a healthy, un-tainted node.
func readyNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// withPool stamps karpenter.sh/nodepool=<pool> on a node so the scoped List can
// filter it (mirrors what Karpenter stamps on real nodes).
func withPool(n *corev1.Node, pool string) *corev1.Node {
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	n.Labels["karpenter.sh/nodepool"] = pool
	return n
}

// installLabelFilterReactor makes the fake clientset honor the List label
// selector. The fake ObjectTracker filters only by namespace and ignores the
// selector entirely, so without this every scoped List would return all nodes
// and the scoping under test would be invisible. The reactor reads the parsed
// selector off the action and returns only the nodes that match — exactly what
// a real apiserver does server-side.
func installLabelFilterReactor(client *fake.Clientset) {
	client.PrependReactor("list", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
		la := action.(ktesting.ListActionImpl)
		sel := la.GetListRestrictions().Labels
		raw, err := client.Tracker().List(la.GetResource(), corev1.SchemeGroupVersion.WithKind("Node"), la.GetNamespace())
		if err != nil {
			return true, nil, err
		}
		all := raw.(*corev1.NodeList)
		if sel == nil || sel.Empty() {
			return true, all, nil
		}
		out := &corev1.NodeList{}
		for i := range all.Items {
			if sel.Matches(labels.Set(all.Items[i].Labels)) {
				out.Items = append(out.Items, all.Items[i])
			}
		}
		return true, out, nil
	})
}

func newTestCollector(t *testing.T, nodes ...*corev1.Node) *Collector {
	t.Helper()
	return newTestCollectorScoped(t, "", nodes...)
}

// newTestCollectorScoped builds a collector with an explicit node selector and a
// label-honoring fake client (see installLabelFilterReactor).
func newTestCollectorScoped(t *testing.T, selector string, nodes ...*corev1.Node) *Collector {
	t.Helper()
	client := fake.NewSimpleClientset()
	installLabelFilterReactor(client)
	ctx := context.Background()
	for _, n := range nodes {
		if _, err := client.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating node %s: %v", n.Name, err)
		}
	}
	return New(client, "test", time.Minute, selector, nil, slog.Default())
}

// newTestCollectorIdle builds a collector with idle-node detection enabled over
// podNamespaces, a label-honoring fake client (see installLabelFilterReactor),
// and the given nodes + pods pre-created. The node selector defaults to the
// agent-pool selector the chart ships.
func newTestCollectorIdle(t *testing.T, podNamespaces []string, nodes []*corev1.Node, pods []*corev1.Pod) *Collector {
	t.Helper()
	client := fake.NewSimpleClientset()
	installLabelFilterReactor(client)
	ctx := context.Background()
	for _, n := range nodes {
		if _, err := client.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating node %s: %v", n.Name, err)
		}
	}
	for _, p := range pods {
		if _, err := client.CoreV1().Pods(p.Namespace).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating pod %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
	return New(client, "test", time.Minute, agentPoolSelector, podNamespaces, slog.Default())
}

// agentPod builds an agent pod (app.kubernetes.io/name=gardenio) scheduled onto
// nodeName in namespace, in the given phase.
func agentPod(namespace, name, nodeName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "gardenio"},
		},
		Spec:   corev1.PodSpec{NodeName: nodeName},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func idleGaugeValue(t *testing.T, c *Collector) float64 {
	t.Helper()
	return testutil.ToFloat64(c.idleNodeCount.WithLabelValues("test"))
}

// counterValue reads gardenio_node_lifecycle_events_total for one reason label.
func counterValue(t *testing.T, c *Collector, reason string) float64 {
	t.Helper()
	return testutil.ToFloat64(c.events.WithLabelValues(reason, "test"))
}

func setDisruptTaint(n *corev1.Node) {
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key:    "karpenter.sh/disrupted",
		Effect: corev1.TaintEffectNoSchedule,
	})
}

func setNotReady(n *corev1.Node) {
	n.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}
}

// TestFirstSweepIsBaseline: the first observation of a node — even one already
// disrupting/deleting — emits NO event, so a controller restart mid-teardown
// does not replay phantom events.
func TestFirstSweepIsBaseline(t *testing.T) {
	tainted := readyNode("node-a")
	setDisruptTaint(tainted)
	deleting := readyNode("node-b")
	now := metav1.Now()
	deleting.DeletionTimestamp = &now

	c := newTestCollector(t, tainted, deleting, readyNode("node-c"))
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, r := range []string{ReasonDisrupting, ReasonDeleting, ReasonTerminated, ReasonNotReady} {
		if got := counterValue(t, c, r); got != 0 {
			t.Errorf("reason %q = %v on baseline sweep, want 0", r, got)
		}
	}
	if len(c.prev) != 3 {
		t.Errorf("prev tracked %d nodes, want 3", len(c.prev))
	}
}

// TestDisruptEdgeCounts: a node that becomes disrupting between sweeps counts
// exactly one disrupting event, and does not re-count on subsequent sweeps.
func TestDisruptEdgeCounts(t *testing.T) {
	c := newTestCollector(t, readyNode("node-a"))
	ctx := context.Background()
	if err := c.Collect(ctx); err != nil { // baseline
		t.Fatalf("baseline: %v", err)
	}

	// Taint the node and re-sweep.
	n, _ := c.client.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	setDisruptTaint(n)
	if _, err := c.client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := counterValue(t, c, ReasonDisrupting); got != 1 {
		t.Fatalf("disrupting = %v after taint, want 1", got)
	}
	// A third sweep with the taint still present must NOT re-count (level, not
	// edge).
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("third: %v", err)
	}
	if got := counterValue(t, c, ReasonDisrupting); got != 1 {
		t.Errorf("disrupting = %v after re-sweep, want 1 (edge-triggered, no re-count)", got)
	}
}

// TestTerminatedWhenNodeVanishes: a node present in a prior sweep but gone from
// the next listing counts one terminated event and is dropped from tracking.
func TestTerminatedWhenNodeVanishes(t *testing.T) {
	c := newTestCollector(t, readyNode("node-a"), readyNode("node-b"))
	ctx := context.Background()
	if err := c.Collect(ctx); err != nil { // baseline (2 nodes)
		t.Fatalf("baseline: %v", err)
	}

	if err := c.client.CoreV1().Nodes().Delete(ctx, "node-a", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := counterValue(t, c, ReasonTerminated); got != 1 {
		t.Errorf("terminated = %v, want 1", got)
	}
	if _, still := c.prev["node-a"]; still {
		t.Error("node-a still tracked after termination, want dropped")
	}
	// A subsequent sweep must not re-count the already-gone node.
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("third: %v", err)
	}
	if got := counterValue(t, c, ReasonTerminated); got != 1 {
		t.Errorf("terminated = %v after re-sweep, want 1 (not re-counted)", got)
	}
}

// TestNotReadyEdge: a node leaving Ready counts one not_ready event.
func TestNotReadyEdge(t *testing.T) {
	c := newTestCollector(t, readyNode("node-a"))
	ctx := context.Background()
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	n, _ := c.client.CoreV1().Nodes().Get(ctx, "node-a", metav1.GetOptions{})
	setNotReady(n)
	if _, err := c.client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := counterValue(t, c, ReasonNotReady); got != 1 {
		t.Errorf("not_ready = %v, want 1", got)
	}
}

// TestListErrorLeavesStateUntouched: a list failure must not fabricate
// terminated events (every node would look vanished) nor reset the baseline.
func TestListErrorLeavesStateUntouched(t *testing.T) {
	c := newTestCollector(t, readyNode("node-a"), readyNode("node-b"))
	ctx := context.Background()
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	before := len(c.prev)

	fakeClient := c.client.(*fake.Clientset)
	fakeClient.PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom: apiserver unavailable")
	})

	if err := c.Collect(ctx); err == nil {
		t.Fatal("Collect returned nil on a list failure, want error")
	}
	if got := counterValue(t, c, ReasonTerminated); got != 0 {
		t.Errorf("terminated = %v after list error, want 0 (no phantom terminations)", got)
	}
	if len(c.prev) != before {
		t.Errorf("prev size = %d after list error, want %d (state untouched)", len(c.prev), before)
	}
}

// TestScopedSelectorExcludesOtherPools: the crux of ka-0000003804. With the
// agent-pool selector, only agent-pool nodes are swept — disruption + deletion +
// termination on an ephemeral-pool node (the E2E-staging/reaper churn that
// tripped the alert on first firing) is NOT counted, while the same edges on an
// agent-pool node ARE.
func TestScopedSelectorExcludesOtherPools(t *testing.T) {
	agent := withPool(readyNode("agent-1"), "agent-pool")
	ephemeral := withPool(readyNode("ephemeral-1"), "ephemeral-pool")
	c := newTestCollectorScoped(t, agentPoolSelector, agent, ephemeral)
	ctx := context.Background()

	// Baseline: only the agent node is in scope, so only it is tracked.
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if len(c.prev) != 1 {
		t.Fatalf("baseline tracked %d nodes, want 1 (agent-pool only)", len(c.prev))
	}
	if _, ok := c.prev["agent-1"]; !ok {
		t.Fatalf("agent-1 not tracked; prev=%v", c.prev)
	}

	// Disrupt + delete the ephemeral node — must produce ZERO events (out of
	// scope, never even seen).
	en, _ := c.client.CoreV1().Nodes().Get(ctx, "ephemeral-1", metav1.GetOptions{})
	setDisruptTaint(en)
	now := metav1.Now()
	en.DeletionTimestamp = &now
	if _, err := c.client.CoreV1().Nodes().Update(ctx, en, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update ephemeral: %v", err)
	}
	// Also fully remove it — a terminated edge for an out-of-scope node must not
	// count either.
	if err := c.client.CoreV1().Nodes().Delete(ctx, "ephemeral-1", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete ephemeral: %v", err)
	}

	// Disrupt the agent node — must produce exactly one disrupting event.
	an, _ := c.client.CoreV1().Nodes().Get(ctx, "agent-1", metav1.GetOptions{})
	setDisruptTaint(an)
	if _, err := c.client.CoreV1().Nodes().Update(ctx, an, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update agent: %v", err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}

	if got := counterValue(t, c, ReasonDisrupting); got != 1 {
		t.Errorf("disrupting = %v, want 1 (agent-pool node only; ephemeral excluded)", got)
	}
	for _, r := range []string{ReasonDeleting, ReasonTerminated, ReasonNotReady} {
		if got := counterValue(t, c, r); got != 0 {
			t.Errorf("reason %q = %v, want 0 (ephemeral-pool churn must not count)", r, got)
		}
	}
}

// TestScopedTerminatedCountsAgentPoolOnly: an agent-pool node vanishing counts a
// terminated event; an ephemeral-pool node vanishing does not (it was never in
// scope, so it was never a tracked baseline).
func TestScopedTerminatedCountsAgentPoolOnly(t *testing.T) {
	agent := withPool(readyNode("agent-1"), "agent-pool")
	ephemeral := withPool(readyNode("ephemeral-1"), "ephemeral-pool")
	c := newTestCollectorScoped(t, agentPoolSelector, agent, ephemeral)
	ctx := context.Background()
	if err := c.Collect(ctx); err != nil { // baseline: agent-1 only
		t.Fatalf("baseline: %v", err)
	}
	// Reclaim both nodes.
	for _, name := range []string{"agent-1", "ephemeral-1"} {
		if err := c.client.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := counterValue(t, c, ReasonTerminated); got != 1 {
		t.Errorf("terminated = %v, want 1 (only the agent-pool node was in scope)", got)
	}
}

// TestEmptySelectorListsAllNodes: an empty selector is the documented opt-out —
// it preserves the pre-fix list-every-node behavior, so churn on any pool counts.
func TestEmptySelectorListsAllNodes(t *testing.T) {
	agent := withPool(readyNode("agent-1"), "agent-pool")
	ephemeral := withPool(readyNode("ephemeral-1"), "ephemeral-pool")
	c := newTestCollectorScoped(t, "", agent, ephemeral)
	ctx := context.Background()
	if err := c.Collect(ctx); err != nil { // baseline: both tracked
		t.Fatalf("baseline: %v", err)
	}
	if len(c.prev) != 2 {
		t.Fatalf("baseline tracked %d nodes, want 2 (empty selector = all)", len(c.prev))
	}
	en, _ := c.client.CoreV1().Nodes().Get(ctx, "ephemeral-1", metav1.GetOptions{})
	setDisruptTaint(en)
	if _, err := c.client.CoreV1().Nodes().Update(ctx, en, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("second: %v", err)
	}
	if got := counterValue(t, c, ReasonDisrupting); got != 1 {
		t.Errorf("disrupting = %v, want 1 (empty selector counts ephemeral churn too)", got)
	}
}

func TestRegisterIdempotent(t *testing.T) {
	c := newTestCollector(t)
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := c.Register(reg); err == nil {
		t.Error("second Register returned nil, want duplicate-registration error")
	}
}

// withVCPUs stamps CPU capacity on a node so the fleet-size gauge tally has
// something to sum (mirrors what the kubelet reports on a real node).
func withVCPUs(n *corev1.Node, cores int64) *corev1.Node {
	if n.Status.Capacity == nil {
		n.Status.Capacity = corev1.ResourceList{}
	}
	n.Status.Capacity[corev1.ResourceCPU] = *resource.NewQuantity(cores, resource.DecimalSI)
	return n
}

func gaugeValue(t *testing.T, g *prometheus.GaugeVec) float64 {
	t.Helper()
	return testutil.ToFloat64(g.WithLabelValues("test"))
}

// TestFleetSizeGauges: agent_node_count and agent_node_vcpus report the live
// agent-pool fleet size (P1b, ka-0000003023) — the leading cost indicators.
// They are set on the first sweep (unlike the baseline-only event counter), and
// track provisioning up and drain down over subsequent sweeps.
func TestFleetSizeGauges(t *testing.T) {
	a := withVCPUs(withPool(readyNode("agent-1"), "agent-pool"), 8)
	b := withVCPUs(withPool(readyNode("agent-2"), "agent-pool"), 4)
	c := newTestCollectorScoped(t, agentPoolSelector, a, b)
	ctx := context.Background()

	// First sweep sets the gauges even though it is the event baseline.
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if got := gaugeValue(t, c.nodeCount); got != 2 {
		t.Errorf("agent_node_count = %v, want 2", got)
	}
	if got := gaugeValue(t, c.nodeVCPUs); got != 12 {
		t.Errorf("agent_node_vcpus = %v, want 12 (8+4)", got)
	}

	// Karpenter provisions a third node → both gauges rise.
	d := withVCPUs(withPool(readyNode("agent-3"), "agent-pool"), 16)
	if _, err := c.client.CoreV1().Nodes().Create(ctx, d, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create agent-3: %v", err)
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("grow: %v", err)
	}
	if got := gaugeValue(t, c.nodeCount); got != 3 {
		t.Errorf("agent_node_count = %v, want 3 after scale-up", got)
	}
	if got := gaugeValue(t, c.nodeVCPUs); got != 28 {
		t.Errorf("agent_node_vcpus = %v, want 28 (8+4+16)", got)
	}

	// Drain the whole fleet → gauges report 0, not a stale high-water mark.
	for _, name := range []string{"agent-1", "agent-2", "agent-3"} {
		if err := c.client.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if got := gaugeValue(t, c.nodeCount); got != 0 {
		t.Errorf("agent_node_count = %v, want 0 on a drained fleet", got)
	}
	if got := gaugeValue(t, c.nodeVCPUs); got != 0 {
		t.Errorf("agent_node_vcpus = %v, want 0 on a drained fleet", got)
	}
}

// TestFleetSizeGaugesUntouchedOnListError: a transient apiserver list failure
// must leave the last-known gauge values intact — flashing 0 would spuriously
// resolve AgentFleetSizeHigh mid-incident.
func TestFleetSizeGaugesUntouchedOnListError(t *testing.T) {
	a := withVCPUs(withPool(readyNode("agent-1"), "agent-pool"), 8)
	c := newTestCollectorScoped(t, agentPoolSelector, a)
	ctx := context.Background()
	if err := c.Collect(ctx); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if got := gaugeValue(t, c.nodeCount); got != 1 {
		t.Fatalf("agent_node_count = %v, want 1", got)
	}

	// Inject a list error on the next sweep.
	c.client.(*fake.Clientset).PrependReactor("list", "nodes", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	if err := c.Collect(ctx); err == nil {
		t.Fatal("Collect returned nil on a list error, want error")
	}
	if got := gaugeValue(t, c.nodeCount); got != 1 {
		t.Errorf("agent_node_count = %v after list error, want last-known 1 (not flashed to 0)", got)
	}
	if got := gaugeValue(t, c.nodeVCPUs); got != 8 {
		t.Errorf("agent_node_vcpus = %v after list error, want last-known 8", got)
	}
}

// TestIdleNodeGaugeDisabledWhenNoNamespaces: with no pod namespaces configured,
// idle-node detection is OFF — the gauge is never constructed, so Register does
// not export it and Collect never touches it. Guards the "absent, not phantom 0"
// contract (an alert must not evaluate against a series the collector never
// measured).
func TestIdleNodeGaugeDisabledWhenNoNamespaces(t *testing.T) {
	c := newTestCollectorScoped(t, agentPoolSelector, withPool(readyNode("agent-1"), "agent-pool"))
	if c.idleNodeCount != nil {
		t.Fatal("idleNodeCount gauge constructed with no pod namespaces; want nil (detection disabled)")
	}
	// Collect must not panic on the nil gauge.
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
}

// TestIdleNodeGauge: agent_idle_node_count reports agent-pool nodes hosting zero
// active agent pods. A node with a Running agent is busy; a node with only a
// terminal (Succeeded/Failed) pod, or no pod at all, is idle.
func TestIdleNodeGauge(t *testing.T) {
	nodes := []*corev1.Node{
		withPool(readyNode("busy-1"), "agent-pool"), // hosts a Running agent
		withPool(readyNode("idle-1"), "agent-pool"), // hosts nothing
		withPool(readyNode("idle-2"), "agent-pool"), // hosts only a Succeeded pod
	}
	pods := []*corev1.Pod{
		agentPod("gardenio-system", "agent-a", "busy-1", corev1.PodRunning),
		agentPod("gardenio-system", "agent-b", "idle-2", corev1.PodSucceeded),
	}
	c := newTestCollectorIdle(t, []string{"gardenio-system"}, nodes, pods)
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	// idle-1 (empty) + idle-2 (only a terminal pod) are idle; busy-1 is not.
	if got := idleGaugeValue(t, c); got != 2 {
		t.Errorf("agent_idle_node_count = %v, want 2 (idle-1, idle-2)", got)
	}
}

// TestIdleNodeGaugeAllBusy: when every agent-pool node hosts an active agent,
// the idle count is 0.
func TestIdleNodeGaugeAllBusy(t *testing.T) {
	nodes := []*corev1.Node{
		withPool(readyNode("busy-1"), "agent-pool"),
		withPool(readyNode("busy-2"), "agent-pool"),
	}
	pods := []*corev1.Pod{
		agentPod("gardenio-system", "agent-a", "busy-1", corev1.PodRunning),
		agentPod("gardenio-system", "agent-b", "busy-2", corev1.PodPending),
	}
	c := newTestCollectorIdle(t, []string{"gardenio-system"}, nodes, pods)
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := idleGaugeValue(t, c); got != 0 {
		t.Errorf("agent_idle_node_count = %v, want 0 (all nodes busy)", got)
	}
}

// TestIdleNodeGaugeMultiNamespace: a busy pod in ANY configured namespace marks
// its node busy — the gVisor namespace agents must count too.
func TestIdleNodeGaugeMultiNamespace(t *testing.T) {
	nodes := []*corev1.Node{
		withPool(readyNode("busy-main"), "agent-pool"),
		withPool(readyNode("busy-gvisor"), "agent-pool"),
		withPool(readyNode("idle-1"), "agent-pool"),
	}
	pods := []*corev1.Pod{
		agentPod("gardenio-system", "agent-a", "busy-main", corev1.PodRunning),
		agentPod("gardenio-gvisor", "agent-b", "busy-gvisor", corev1.PodRunning),
	}
	c := newTestCollectorIdle(t, []string{"gardenio-system", "gardenio-gvisor"}, nodes, pods)
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := idleGaugeValue(t, c); got != 1 {
		t.Errorf("agent_idle_node_count = %v, want 1 (idle-1)", got)
	}
}

// TestIdleNodeGaugeUnchangedOnPodListError: a transient pod-list failure leaves
// the last-known idle gauge intact rather than flashing a wrong value that could
// spuriously fire or resolve AgentFleetIdleNodes.
func TestIdleNodeGaugeUnchangedOnPodListError(t *testing.T) {
	nodes := []*corev1.Node{
		withPool(readyNode("busy-1"), "agent-pool"),
		withPool(readyNode("idle-1"), "agent-pool"),
	}
	pods := []*corev1.Pod{
		agentPod("gardenio-system", "agent-a", "busy-1", corev1.PodRunning),
	}
	c := newTestCollectorIdle(t, []string{"gardenio-system"}, nodes, pods)
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("baseline Collect: %v", err)
	}
	if got := idleGaugeValue(t, c); got != 1 {
		t.Fatalf("agent_idle_node_count = %v, want 1 baseline", got)
	}

	// Inject a pod-list error on the next sweep. The node list still succeeds.
	c.client.(*fake.Clientset).PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("apiserver unavailable")
	})
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect after pod-list error should still succeed (node sweep ok): %v", err)
	}
	if got := idleGaugeValue(t, c); got != 1 {
		t.Errorf("agent_idle_node_count = %v after pod-list error, want last-known 1 (not flashed)", got)
	}
}

// TestIdleNodeGaugeRegistered: when enabled, the idle gauge is exported by
// Register alongside the fleet-size gauges.
func TestIdleNodeGaugeRegistered(t *testing.T) {
	c := newTestCollectorIdle(t, []string{"gardenio-system"},
		[]*corev1.Node{withPool(readyNode("agent-1"), "agent-pool")}, nil)
	reg := prometheus.NewRegistry()
	if err := c.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Sweep once so the gauge has an observed series to gather (a GaugeVec with
	// no label combination set yet emits nothing).
	if err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	var found bool
	for _, mf := range mfs {
		if mf.GetName() == "gardenio_agent_idle_node_count" {
			found = true
		}
	}
	if !found {
		t.Error("gardenio_agent_idle_node_count not registered when idle-node detection enabled")
	}
}
