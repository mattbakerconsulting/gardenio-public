package poolmanager

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/podmanager"
)

// fakeCycleRecorder captures RecordWarmPlaceholderCycled calls for assertions.
type fakeCycleRecorder struct {
	total   int
	byNode  map[string]int
	byTopo  map[string]int
	ncalled int
}

func newFakeCycleRecorder() *fakeCycleRecorder {
	return &fakeCycleRecorder{byNode: map[string]int{}, byTopo: map[string]int{}}
}

func (r *fakeCycleRecorder) RecordWarmPlaceholderCycled(node, topology string, count int) {
	r.total += count
	r.byNode[node] += count
	r.byTopo[topology] += count
	r.ncalled++
}

// newStalePool builds a pool with an explicit stale drain rate and an attached
// cycle recorder, for the upgrade-staleness tests (ka-0000000890).
func newStalePool(t *testing.T, client *fake.Clientset, minSize, drainRate int, rec CycleRecorder) *PlaceholderPool {
	t.Helper()
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace:      "gvisor",
		Image:          testPoolImage,
		MinSize:        minSize,
		StaleDrainRate: drainRate,
		CycleRecorder:  rec,
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	return p
}

func podNames(pods []corev1.Pod) map[string]corev1.Pod {
	out := make(map[string]corev1.Pod, len(pods))
	for _, p := range pods {
		out[p.Name] = p
	}
	return out
}

// TestCreatePlaceholder_StampsCurrentImage verifies the pool stamps its
// configured image on every minted placeholder so the reaper can tell
// current-controller pods from old ones (ka-0000006974).
func TestCreatePlaceholder_StampsCurrentImage(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	p := newStalePool(t, client, 2, 0, nil)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 2 {
		t.Fatalf("expected 2 placeholders, got %d", len(pods))
	}
	for _, pod := range pods {
		if got := pod.Annotations[podmanager.AnnotationWarmImage]; got != testPoolImage {
			t.Errorf("placeholder %s: warm-image = %q, want %q", pod.Name, got, testPoolImage)
		}
	}
}

// TestReconcile_SameImageKept: a Ready placeholder stamped with the current
// image is never cycled and counts toward MinSize (no churn at steady state).
func TestReconcile_SameImageKept(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("fresh-1", "gvisor", "node-a"),
		readyPlaceholder("fresh-2", "gvisor", "node-a"),
	)
	rec := newFakeCycleRecorder()
	p := newStalePool(t, client, 2, 2, rec)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 2 {
		t.Fatalf("expected steady pool of 2, got %d", len(pods))
	}
	if rec.total != 0 {
		t.Errorf("expected 0 cycled at steady state, got %d", rec.total)
	}
	byName := podNames(pods)
	if _, ok := byName["fresh-1"]; !ok {
		t.Error("fresh-1 should be kept")
	}
	if _, ok := byName["fresh-2"]; !ok {
		t.Error("fresh-2 should be kept")
	}
}

// TestReconcile_StaleImageReaped: once a full MinSize of fresh Ready exists, a
// stale-image Ready placeholder is cycled (and the cycle is recorded).
func TestReconcile_StaleImageReaped(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		// MinSize 1 of fresh already present, plus a stale leftover.
		readyPlaceholder("fresh-1", "gvisor", "node-a"),
		placeholderWithImage("stale-1", "gvisor", "node-a", "agent:OLD"),
	)
	rec := newFakeCycleRecorder()
	p := newStalePool(t, client, 1, 2, rec)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	byName := podNames(countPlaceholders(t, client, "gvisor"))
	if _, ok := byName["stale-1"]; ok {
		t.Error("stale-image placeholder should have been cycled")
	}
	if _, ok := byName["fresh-1"]; !ok {
		t.Error("fresh placeholder must be kept")
	}
	if rec.total != 1 {
		t.Errorf("expected 1 cycled recorded, got %d", rec.total)
	}
	if rec.byNode["node-a"] != 1 {
		t.Errorf("expected 1 cycled on node-a, got %d", rec.byNode["node-a"])
	}
}

// TestReconcile_UnstampedTreatedStale: a placeholder with no warm-image
// annotation (minted before the stamp existed) is treated as stale and cycled
// once a fresh MinSize exists.
func TestReconcile_UnstampedTreatedStale(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("fresh-1", "gvisor", "node-a"),
		placeholderWithImage("unstamped-1", "gvisor", "node-a", ""), // no annotation
	)
	rec := newFakeCycleRecorder()
	p := newStalePool(t, client, 1, 2, rec)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	byName := podNames(countPlaceholders(t, client, "gvisor"))
	if _, ok := byName["unstamped-1"]; ok {
		t.Error("unstamped placeholder should be treated as stale and cycled")
	}
	if rec.total != 1 {
		t.Errorf("expected 1 cycled, got %d", rec.total)
	}
}

// TestReconcile_NoColdStartGap: when the whole pool is stale (the moment after an
// upgrade), the reconcile must NOT drop the pool below MinSize Ready. It first
// mints a fresh placeholder and keeps the stale Ready ones until the fresh ones
// are Ready; a stale Ready pod is only cycled while total Ready stays >= MinSize.
func TestReconcile_NoColdStartGap(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		// Exactly MinSize stale Ready pods — deleting any of them before a fresh
		// one is Ready would dip below MinSize.
		placeholderWithImage("stale-1", "gvisor", "node-a", "agent:OLD"),
		placeholderWithImage("stale-2", "gvisor", "node-a", "agent:OLD"),
	)
	rec := newFakeCycleRecorder()
	p := newStalePool(t, client, 2, 2, rec)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	byName := podNames(countPlaceholders(t, client, "gvisor"))
	// Both stale Ready pods are retained (surplus is 0 until fresh ones Ready).
	if _, ok := byName["stale-1"]; !ok {
		t.Error("stale-1 must be retained — deleting it would drop below MinSize Ready")
	}
	if _, ok := byName["stale-2"]; !ok {
		t.Error("stale-2 must be retained — deleting it would drop below MinSize Ready")
	}
	if rec.total != 0 {
		t.Errorf("expected 0 cycled while no fresh surplus exists, got %d", rec.total)
	}
	// And the deficit-fill minted MinSize fresh placeholders (the new image).
	fresh := 0
	for _, pod := range byName {
		if pod.Annotations[podmanager.AnnotationWarmImage] == testPoolImage {
			fresh++
		}
	}
	if fresh != 2 {
		t.Errorf("expected 2 fresh placeholders minted, got %d", fresh)
	}
}

// TestReconcile_StaleDrainRateLimited: more stale surplus than the drain rate
// only cycles drainRate pods per pass.
func TestReconcile_StaleDrainRateLimited(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		// 2 fresh Ready (== MinSize) so all stale Ready are surplus.
		readyPlaceholder("fresh-1", "gvisor", "node-a"),
		readyPlaceholder("fresh-2", "gvisor", "node-a"),
		placeholderWithImage("stale-1", "gvisor", "node-a", "agent:OLD"),
		placeholderWithImage("stale-2", "gvisor", "node-a", "agent:OLD"),
		placeholderWithImage("stale-3", "gvisor", "node-a", "agent:OLD"),
	)
	rec := newFakeCycleRecorder()
	// drainRate 2: only 2 of the 3 surplus stale pods cycle this pass.
	p := newStalePool(t, client, 2, 2, rec)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if rec.total != 2 {
		t.Errorf("expected exactly 2 cycled (rate limit), got %d", rec.total)
	}
	staleLeft := 0
	for _, pod := range countPlaceholders(t, client, "gvisor") {
		if pod.Annotations[podmanager.AnnotationWarmImage] == "agent:OLD" {
			staleLeft++
		}
	}
	if staleLeft != 1 {
		t.Errorf("expected 1 stale placeholder left after rate-limited drain, got %d", staleLeft)
	}
}

// TestReconcile_ClaimedNeverCycledByStaleDrain: a claimed (in-use / promoting)
// placeholder is never touched by the stale-drain path — only Ready/unclaimed
// pods are cycled. (A claimed leftover is still reaped by the existing
// stale-claimed sweep, which is a separate concern; here we assert the stale
// drain itself never deletes a pod still labeled claimed as "stale".)
func TestReconcile_ClaimedNeverCycledByStaleDrain(t *testing.T) {
	// A claimed pod stamped with an OLD image: it is in-use (being grafted), so
	// the stale-drain must not classify it for cycling. The existing
	// reapsStaleClaimed sweep handles genuinely-leaked claimed pods, but that is
	// a leftover-claim concern, not an image-staleness one. We assert the drain
	// recorder never counts it.
	claimed := placeholderWithImage("claimed-old", "gvisor", "node-a", "agent:OLD")
	claimed.Labels[podmanager.LabelWarmPlaceholder] = podmanager.WarmPlaceholderClaimed
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("fresh-1", "gvisor", "node-a"),
		claimed,
	)
	rec := newFakeCycleRecorder()
	p := newStalePool(t, client, 1, 2, rec)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The stale-drain recorder must not have counted the claimed pod. (The
	// claimed pod itself is reaped by the separate stale-claimed sweep — that is
	// existing behavior, asserted elsewhere.)
	if rec.total != 0 {
		t.Errorf("stale-drain must not cycle claimed/in-use pods, recorded %d", rec.total)
	}
}

// TestReconcile_StaleAcrossPassesConverges simulates the upgrade convergence:
// pass 1 mints fresh + drains what surplus exists; once the fresh pods are Ready,
// the remaining stale ones drain out, and the pool settles at MinSize all-fresh.
func TestReconcile_StaleAcrossPassesConverges(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		placeholderWithImage("stale-1", "gvisor", "node-a", "agent:OLD"),
		placeholderWithImage("stale-2", "gvisor", "node-a", "agent:OLD"),
	)
	rec := newFakeCycleRecorder()
	p := newStalePool(t, client, 2, 5, rec)

	// Pass 1: 2 stale Ready (==MinSize) → no surplus, retain both, mint 2 fresh
	// (which start as pending/not-Ready in the fake client).
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile pass 1: %v", err)
	}
	// Mark the freshly-minted (pending) pods Ready to simulate them coming up.
	markFreshReady(t, client)

	// Pass 2: now 2 fresh Ready + 2 stale Ready = 4 total Ready, surplus 2 → both
	// stale drain out.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile pass 2: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	for _, pod := range pods {
		if pod.Annotations[podmanager.AnnotationWarmImage] == "agent:OLD" {
			t.Errorf("stale pod %s should have converged out, still present", pod.Name)
		}
	}
	if len(pods) != 2 {
		t.Errorf("expected pool to settle at MinSize 2 all-fresh, got %d", len(pods))
	}
	if rec.total != 2 {
		t.Errorf("expected 2 total cycled across convergence, got %d", rec.total)
	}
}

// markFreshReady flips any current-image placeholder that is not yet Ready to
// Running+Ready, simulating the kubelet bringing the freshly-minted pods up.
func markFreshReady(t *testing.T, client *fake.Clientset) {
	t.Helper()
	pods := countPlaceholders(t, client, "gvisor")
	for i := range pods {
		pod := pods[i]
		if pod.Annotations[podmanager.AnnotationWarmImage] != testPoolImage {
			continue
		}
		if podIsReady(&pod) {
			continue
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue},
		}
		if _, err := client.CoreV1().Pods("gvisor").UpdateStatus(context.Background(), &pod, metav1.UpdateOptions{}); err != nil {
			// Fake client may route status via Update; fall back.
			if _, err2 := client.CoreV1().Pods("gvisor").Update(context.Background(), &pod, metav1.UpdateOptions{}); err2 != nil {
				t.Fatalf("marking fresh pod ready: %v / %v", err, err2)
			}
		}
	}
}
