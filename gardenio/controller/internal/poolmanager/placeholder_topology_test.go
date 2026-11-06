package poolmanager

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/podmanager"
)

// fakeSnapshotProvider is a test SnapshotProvider returning a fixed bucket set.
type fakeSnapshotProvider struct {
	buckets  []podmanager.WarmBucket
	topology map[string]string // agentName → topology
	listErr  error
}

func (f *fakeSnapshotProvider) WarmBuckets(context.Context) ([]podmanager.WarmBucket, error) {
	return f.buckets, f.listErr
}

func (f *fakeSnapshotProvider) SnapshotTopology(_ context.Context, agent string) (string, bool) {
	t, ok := f.topology[agent]
	return t, ok
}

// snapWithShape builds a snapshot with a distinguishable mount shape so its
// topology key is stable and non-empty.
func snapWithShape(agent, node, dst string) podmanager.Snapshot {
	return podmanager.Snapshot{
		AgentName: agent,
		Namespace: "gvisor",
		Node:      node,
		PodTemplate: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:         "agent",
				VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: dst}},
			}},
		},
	}
}

func topoOf(snap podmanager.Snapshot) string { return podmanager.SnapshotTopologyKey(snap) }

func TestReconcile_TopologyAwareFillsPerBucket(t *testing.T) {
	snapA := snapWithShape("a", "node-a", "/home/agent/bot")
	provider := &fakeSnapshotProvider{
		buckets: []podmanager.WarmBucket{
			{Node: "node-a", Topology: topoOf(snapA), Snapshot: snapA},
		},
	}
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	p := newTestPool(t, client, 2)
	p.SetSnapshotProvider(provider)

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 2 {
		t.Fatalf("expected 2 topology placeholders, got %d", len(pods))
	}
	for _, pod := range pods {
		if pod.Labels[podmanager.LabelWarmTopology] != topoOf(snapA) {
			t.Errorf("placeholder %s topology = %q, want %q", pod.Name,
				pod.Labels[podmanager.LabelWarmTopology], topoOf(snapA))
		}
		// Built from the snapshot template → carries the agent container with the
		// snapshot's mount destination, not a bare /work emptyDir.
		var hasWorkspaceMount bool
		for _, c := range pod.Spec.Containers {
			for _, m := range c.VolumeMounts {
				if m.MountPath == "/home/agent/bot" {
					hasWorkspaceMount = true
				}
			}
		}
		if !hasWorkspaceMount {
			t.Errorf("placeholder %s did not inherit the snapshot workspace mount", pod.Name)
		}
	}
}

// newFloorPool builds a pool with both a per-topology MinSize and a per-node
// NodeFloor (ka-0000000239 / WS1).
func newFloorPool(t *testing.T, client *fake.Clientset, minSize, nodeFloor int) *PlaceholderPool {
	t.Helper()
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace: "gvisor",
		Image:     testPoolImage,
		MinSize:   minSize,
		NodeFloor: nodeFloor,
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	return p
}

// TestReconcile_NodeFloorWarmsEveryNode: with NodeFloor>0, every gVisor node
// (even one with NO snapshot) gets a cheap legacy warm placeholder, unioned with
// the per-topology snapshot buckets. This is the WS1 floor that keeps a warm
// gVisor node always present so Karpenter's WhenEmpty consolidation never scales
// the substrate to zero (ka-0000000239). node-a holds a snapshot (gets MinSize
// topology pods + NodeFloor legacy pods); node-b has no snapshot (gets only the
// NodeFloor legacy pods).
func TestReconcile_NodeFloorWarmsEveryNode(t *testing.T) {
	snapA := snapWithShape("a", "node-a", "/home/agent/bot")
	provider := &fakeSnapshotProvider{
		buckets: []podmanager.WarmBucket{
			{Node: "node-a", Topology: topoOf(snapA), Snapshot: snapA},
		},
	}
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		probePod("probe-b", "gvisor", "node-b"),
	)
	p := newFloorPool(t, client, 2, 1) // MinSize=2 per topology, NodeFloor=1 per node
	p.SetSnapshotProvider(provider)

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")

	perNode := map[string]int{}
	legacyPerNode := map[string]int{}
	topoPerNode := map[string]int{}
	for i := range pods {
		pod := &pods[i]
		perNode[pod.Spec.NodeName]++
		if pod.Labels[podmanager.LabelWarmTopology] == "" {
			legacyPerNode[pod.Spec.NodeName]++
		} else {
			topoPerNode[pod.Spec.NodeName]++
		}
	}

	// node-a: 2 topology (snapshot bucket) + 1 legacy floor = 3.
	if perNode["node-a"] != 3 {
		t.Errorf("node-a: expected 3 (2 topology + 1 floor), got %d", perNode["node-a"])
	}
	if topoPerNode["node-a"] != 2 || legacyPerNode["node-a"] != 1 {
		t.Errorf("node-a: expected 2 topology + 1 legacy, got %d topology + %d legacy",
			topoPerNode["node-a"], legacyPerNode["node-a"])
	}
	// node-b: no snapshot, so only the 1 legacy floor pod — but it IS warmed
	// (the whole point: a snapshot-less node is not left to consolidate).
	if perNode["node-b"] != 1 || legacyPerNode["node-b"] != 1 {
		t.Errorf("node-b: expected 1 legacy floor pod, got %d (legacy %d)",
			perNode["node-b"], legacyPerNode["node-b"])
	}
}

// TestReconcile_NodeFloorOnlyNoSnapshots: NodeFloor>0 with MinSize=0 and no
// snapshot provider keeps every node warm at the floor — the minimal
// "always-warm-node" config (ka-0000000239).
func TestReconcile_NodeFloorOnlyNoSnapshots(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		probePod("probe-b", "gvisor", "node-b"),
	)
	p := newFloorPool(t, client, 0, 2) // pool active via NodeFloor alone
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 4 { // 2 nodes × NodeFloor 2
		t.Fatalf("expected 4 floor placeholders (2 nodes × 2), got %d", len(pods))
	}
	for i := range pods {
		if pods[i].Labels[podmanager.LabelWarmTopology] != "" {
			t.Errorf("floor placeholder %s should be legacy (empty topology), got %q",
				pods[i].Name, pods[i].Labels[podmanager.LabelWarmTopology])
		}
	}
}

func TestReconcile_SkipsBucketsOnNodesWithoutProbe(t *testing.T) {
	snapB := snapWithShape("b", "node-b", "/home/agent/bot")
	provider := &fakeSnapshotProvider{
		buckets: []podmanager.WarmBucket{
			{Node: "node-b", Topology: topoOf(snapB), Snapshot: snapB},
		},
	}
	// Only node-a has a Ready probe; the snapshot bucket is on node-b. With no
	// usable snapshot bucket, the pool falls back to a legacy fill on node-a.
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	p := newTestPool(t, client, 1)
	p.SetSnapshotProvider(provider)

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("expected 1 legacy fallback placeholder on node-a, got %d", len(pods))
	}
	// Fallback bucket is legacy (no topology label).
	if pods[0].Labels[podmanager.LabelWarmTopology] != "" {
		t.Errorf("expected legacy (empty topology) fallback, got %q", pods[0].Labels[podmanager.LabelWarmTopology])
	}
}

func TestClaim_TopologyMismatchMisses(t *testing.T) {
	// A ready placeholder labeled topology "T1"; a claim for "T2" must miss.
	ph := readyPlaceholder("w1", "gvisor", "node-a")
	ph.Labels[podmanager.LabelWarmTopology] = "T1"
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"), ph)
	p := newTestPool(t, client, 1)

	if _, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", "T2"); err != ErrNoWarmPlaceholder {
		t.Fatalf("topology mismatch must miss, got %v", err)
	}
	// Matching topology hits.
	name, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", "T1")
	if err != nil || name != "w1" {
		t.Fatalf("matching topology must claim w1, got name=%q err=%v", name, err)
	}
}

func TestClaimForThaw_TopologyMatchPromotes(t *testing.T) {
	ph := readyPlaceholder("w1", "gvisor", "node-a")
	ph.Labels[podmanager.LabelWarmTopology] = "T1"
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"), ph)
	p := newTestPool(t, client, 1)

	promo := podmanager.WarmPlaceholderPromotion{AgentName: "agent-x", CanonicalPod: "c"}
	// Mismatch → miss, no promotion.
	if _, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-a", "T2", promo); err != ErrNoWarmPlaceholder {
		t.Fatalf("topology mismatch must miss, got %v", err)
	}
	got, _ := client.CoreV1().Pods("gvisor").Get(context.Background(), "w1", metav1.GetOptions{})
	if got.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderReady {
		t.Fatalf("mismatched claim must leave the pod ready, got %q", got.Labels[podmanager.LabelWarmPlaceholder])
	}
	// Match → promote.
	name, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-a", "T1", promo)
	if err != nil || name != "w1" {
		t.Fatalf("matching topology must promote w1, got name=%q err=%v", name, err)
	}
	got, _ = client.CoreV1().Pods("gvisor").Get(context.Background(), "w1", metav1.GetOptions{})
	if got.Labels[podmanager.LabelAgent] != "agent-x" {
		t.Errorf("promoted pod must carry agent label, got %q", got.Labels[podmanager.LabelAgent])
	}
}

func TestReconcile_DistinctTopologyBucketsCoexist(t *testing.T) {
	// Two distinct shapes on the same node → two buckets, each filled to MinSize.
	snapA := snapWithShape("a", "node-a", "/home/agent/bot")
	snapB := snapWithShape("b", "node-a", "/srv/work")
	provider := &fakeSnapshotProvider{
		buckets: []podmanager.WarmBucket{
			{Node: "node-a", Topology: topoOf(snapA), Snapshot: snapA},
			{Node: "node-a", Topology: topoOf(snapB), Snapshot: snapB},
		},
	}
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	p := newTestPool(t, client, 1)
	p.SetSnapshotProvider(provider)

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	byTopo := map[string]int{}
	for _, pod := range countPlaceholders(t, client, "gvisor") {
		byTopo[pod.Labels[podmanager.LabelWarmTopology]]++
	}
	if byTopo[topoOf(snapA)] != 1 || byTopo[topoOf(snapB)] != 1 {
		t.Fatalf("expected 1 placeholder per topology bucket, got %v", byTopo)
	}
}
