package poolmanager

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/podmanager"
)

// runningOnCreate installs a reactor that stores every freshly-created
// placeholder in Phase=Running (no Ready condition yet). The immediate
// post-Create podRejected() Get therefore reads "not-yet-terminal", so the mint
// loop records the pod in recentlyMinted — the exact entry the per-pass prune
// (ka-0000005097) is responsible for retiring once the pod survives its first
// pass. This is the healthy counterpart to placeholder_test.go's rejectOnCreate.
func runningOnCreate(client *fake.Clientset) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})
}

// markReady flips a stored placeholder to Running+Ready so the next reconcile
// counts it as readyFresh (a live, serviceable warm pod).
func markReady(t *testing.T, client *fake.Clientset, ns, name string) {
	t.Helper()
	pod, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if _, err := client.CoreV1().Pods(ns).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark %s ready: %v", name, err)
	}
}

// failPod flips a stored placeholder to a terminal Failed phase, simulating a
// death (OOM eviction, runtime crash, node drain).
func failPod(t *testing.T, client *fake.Clientset, ns, name, reason string) {
	t.Helper()
	pod, err := client.CoreV1().Pods(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = reason
	pod.Status.Conditions = nil
	if _, err := client.CoreV1().Pods(ns).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("fail %s: %v", name, err)
	}
}

// TestReconcile_RecentlyMintedPrunedAfterSurvivingPass is the ka-0000005097 leak
// fix: the recentlyMinted docstring promised a per-pass prune to bounded size
// that never existed — the only deleter was the reclassification path, so a
// placeholder that reached Ready kept its recentlyMinted entry for the life of
// the pool. This test drives the healthy path: pass 1 mints a pod that comes up
// Running (podRejected miss → recorded); the pod then reaches Ready; pass 2 must
// PRUNE it from recentlyMinted (it has survived its single mint-time
// delayed-terminal window) so the map does not grow unbounded.
func TestReconcile_RecentlyMintedPrunedAfterSurvivingPass(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	runningOnCreate(client)
	p := newTestPool(t, client, 1)

	// Pass 1: mints 1 (Running). recentlyMinted records it.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if len(p.recentlyMinted) != 1 {
		t.Fatalf("pass 1 must record the minted placeholder, got %+v", p.recentlyMinted)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("pass 1 must mint exactly 1, got %d", len(pods))
	}

	// The pod reaches Ready between passes — a healthy, serviceable warm pod.
	markReady(t, client, "gvisor", pods[0].Name)

	// Pass 2: the pod is at target (deficit 0, no new mint) and has survived a
	// full pass without landing terminal, so it is pruned. recentlyMinted must
	// now be empty — the map is bounded to a single pass's mints.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if len(p.recentlyMinted) != 0 {
		t.Fatalf("pass 2 must prune the survived mint from recentlyMinted, got %+v", p.recentlyMinted)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs != nil {
		t.Fatalf("a healthy Ready bucket must not arm backoff, got %+v", bs)
	}
}

// TestReconcile_LateFailureDoesNotArmBackoff is the core ka-0000005097
// misclassification fix: a long-lived healthy placeholder that fails LATER (a
// genuine eviction / runtime OOM / node drain — NOT the mint-time async-OOM
// admission race) must NOT be reclassified as a delayed mint rejection and must
// NOT arm the OOM-race backoff. Before the per-pass prune, the survivor kept its
// recentlyMinted entry forever, so its late death was counted as a mint-time
// rejection and falsely armed the backoff — throttling a bucket whose only sin
// was a single late pod death. With the prune, the survivor is gone from
// recentlyMinted by the time it dies, so the reap treats it as a foreign/late
// terminal (reaped, no arm).
func TestReconcile_LateFailureDoesNotArmBackoff(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	runningOnCreate(client)
	p := newTestPool(t, client, 1)

	// Pass 1: mint (Running) → recorded in recentlyMinted.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("pass 1 must mint exactly 1, got %d", len(pods))
	}
	survivor := pods[0].Name

	// The placeholder reaches Ready and serves as a healthy warm pod.
	markReady(t, client, "gvisor", survivor)

	// Pass 2: survivor is pruned from recentlyMinted (past its mint-time window).
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if _, ok := p.recentlyMinted[survivor]; ok {
		t.Fatalf("survivor must be pruned from recentlyMinted after a full pass")
	}

	// Now the long-lived pod dies LATE — well past its mint-time race window.
	failPod(t, client, "gvisor", survivor, "Evicted")

	// Pass 3: the late terminal is reaped. Because it is no longer in
	// recentlyMinted, it is NOT reclassified as a delayed mint rejection, so the
	// backoff MUST NOT arm. (Pre-fix: recentlyMinted still held it → delayed
	// rejection → noteMintOutcome(1,1) → armed. This assertion is the regression
	// guard.)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs != nil {
		t.Errorf("a late failure of a survived placeholder must not arm the OOM-race backoff, got %+v", bs)
	}
	// The dead pod is reaped and a fresh replacement minted.
	for _, pod := range countPlaceholders(t, client, "gvisor") {
		if pod.Name == survivor && podIsTerminal(&pod) {
			t.Errorf("late-failed placeholder %s was not reaped", survivor)
		}
	}
}

// TestReconcile_RecentlyMintedBoundedUnderClaimChurn is the faithful
// reproduction of the ka-0000005097 unbounded-growth leak. The real production
// growth path is NOT two consecutive minting passes on one bucket (a
// Running-but-not-Ready mint counts as pendingFresh, so the deficit-fill does
// not re-mint on top of it) — it is claim+refill CHURN: every wake claims a
// Ready placeholder and the next Reconcile reaps it and mints a FRESH
// replacement, which reaches Ready and, in the buggy code, is recorded in
// recentlyMinted forever (the reclassification path only ever removed TERMINAL
// pods, never a healthy mint). Over a long-lived pool that is thousands of
// successfully-served placeholders accumulating in the map.
//
// This test runs many claim→reconcile→ready cycles and asserts the map stays
// bounded to a single pass's mints. Against the reverted (unpruned) code the map
// would grow by one entry per cycle (== cycle count); the fix keeps it at ~1.
func TestReconcile_RecentlyMintedBoundedUnderClaimChurn(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	runningOnCreate(client)
	const minSize = 1
	p := newTestPool(t, client, minSize)

	// Prime the pool with one Ready placeholder.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("prime reconcile: %v", err)
	}
	for _, pod := range countPlaceholders(t, client, "gvisor") {
		markReady(t, client, "gvisor", pod.Name)
	}

	const cycles = 12
	for i := range cycles {
		// Claim the Ready placeholder (wake path) — flips it to "claimed".
		if _, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", ""); err != nil {
			t.Fatalf("cycle %d claim: %v", i, err)
		}
		// Reconcile: reaps the claimed pod and mints a FRESH replacement
		// (recorded in recentlyMinted), and prunes the prior cohort.
		if err := p.Reconcile(context.Background()); err != nil {
			t.Fatalf("cycle %d reconcile: %v", i, err)
		}
		// The fresh mint reaches Ready so the next cycle can claim it.
		for _, pod := range countPlaceholders(t, client, "gvisor") {
			if pod.Labels[podmanager.LabelWarmPlaceholder] == podmanager.WarmPlaceholderReady && !podIsReady(&pod) {
				markReady(t, client, "gvisor", pod.Name)
			}
		}
		// The load-bearing bound: with the per-pass prune, recentlyMinted holds
		// at most this cycle's fresh mint(s) — never the accumulation of every
		// prior served placeholder. Against the buggy code this reaches i+1 and
		// climbs without limit.
		if len(p.recentlyMinted) > minSize {
			t.Fatalf("cycle %d: recentlyMinted grew past one pass of mints (%d > %d) — the leak regressed: %+v",
				i, len(p.recentlyMinted), minSize, p.recentlyMinted)
		}
	}
}
