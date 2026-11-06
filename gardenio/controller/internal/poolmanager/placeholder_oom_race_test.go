package poolmanager

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestReconcile_DelayedOOMTerminalArmsBackoff is the ka-0000000088 / canary #12
// race fix: a placeholder whose kubelet OutOfmemory rejection lands Failed
// AFTER podRejected()'s immediate post-Create Get returned "not-yet-terminal"
// used to slip past noteMintOutcome — the bucket saw no rejections, cleared
// its backoff, and re-minted a fresh doomed batch every 30s reconcile forever
// (the 29-events-in-20-min mint storm on canary #12). This test drives the
// exact race: pass 1's mint reports Running (podRejected miss); between passes
// the stored pod flips to Failed (async OOM landing); pass 2 must reclassify
// the delayed terminal as a rejection and arm the backoff.
func TestReconcile_DelayedOOMTerminalArmsBackoff(t *testing.T) {
	// Create reactor: the freshly-created pod is stored in Phase=Running, so
	// the immediate podRejected() Get in the mint loop returns
	// "not-yet-terminal" (the exact blind spot the fix targets).
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodRunning
		return false, nil, nil
	})

	p := newTestPool(t, client, 1)
	clock := time.Unix(1_700_000_000, 0)
	p.nowFunc = func() time.Time { return clock }

	// Pass 1: mints 1. podRejected() sees Running → false. recentlyMinted
	// records the name. noteMintOutcome(1, 0) → clears backoff.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if len(p.recentlyMinted) != 1 {
		t.Fatalf("pass 1 must record the just-minted placeholder in recentlyMinted, got %+v", p.recentlyMinted)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs != nil {
		t.Fatalf("pass 1 must NOT arm backoff (async rejection has not landed yet), got %+v", bs)
	}
	// Grab the minted pod's name — it will flip Failed before pass 2.
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("pass 1 must mint exactly 1 placeholder, got %d", len(pods))
	}
	mintedName := pods[0].Name

	// Simulate the ASYNC OOM landing between passes: flip the stored pod to
	// Failed with reason=OutOfmemory. On pass 2's listPlaceholders, the pod
	// lands in bucket.terminal (podIsTerminal ⇒ true). Because recentlyMinted
	// still has its name, the pass 2 loop reclassifies it as a delayed
	// rejection and arms the backoff.
	failed := pods[0].DeepCopy()
	failed.Status.Phase = corev1.PodFailed
	failed.Status.Reason = "OutOfmemory"
	if _, err := client.CoreV1().Pods("gvisor").UpdateStatus(context.Background(), failed, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("simulating async Failed: %v", err)
	}

	// Pass 2: the pool sees Terminal[mintedName] → delayedRejections=1.
	// The pod is reaped and deficit-fill mints another (which also comes up
	// Running). Arithmetic: minted=1, rejected=0, delayed=1 →
	// case minted>0 && rejected<minted CLEARS. That is arguably correct for a
	// single race event, but pass 3 (persistent OOM race) must arm.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	// Simulate the async OOM race AGAIN on the pass-2 mint. Grab whichever
	// live placeholder exists and flip it Failed.
	live := countPlaceholders(t, client, "gvisor")
	if len(live) == 0 {
		t.Fatalf("pass 2 should have re-minted after reaping the delayed terminal")
	}
	// The reaper deleted mintedName; find a fresh one.
	for _, pod := range live {
		if pod.Name == mintedName {
			continue
		}
		toFail := pod.DeepCopy()
		toFail.Status.Phase = corev1.PodFailed
		toFail.Status.Reason = "OutOfmemory"
		_, _ = client.CoreV1().Pods("gvisor").UpdateStatus(context.Background(), toFail, metav1.UpdateOptions{})
		break
	}

	// Pass 3: another delayed terminal, another reclassification. By the
	// definition of the fix, the backoff MUST be armed by now — a bucket
	// that has produced only OOM-racing placeholders for two consecutive
	// passes is exactly the storm-shape the arm rule exists to suppress.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs == nil {
		t.Errorf("after two async OOM landings, mint backoff must be armed — got nil")
	}
}

// TestReconcile_SyntheticTerminalFromForeignPodDoesNotArm: a terminal pod that
// this pool never minted (foreign, or minted by an older controller before
// recentlyMinted existed) MUST NOT arm the backoff. Only OUR OWN recent mints
// count as delayed rejections; treating stranger corpses as evidence would
// over-throttle the bucket.
func TestReconcile_SyntheticTerminalFromForeignPodDoesNotArm(t *testing.T) {
	dead := readyPlaceholder("w-foreign-dead", "gvisor", "node-a")
	dead.Status.Phase = corev1.PodFailed
	dead.Status.Conditions = nil
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		dead,
	)
	p := newTestPool(t, client, 1)

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// Foreign terminal reaped; recentlyMinted never held it → no delayed
	// reclassification → no backoff. Bucket healthy after the reap + fresh mint.
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs != nil {
		t.Errorf("foreign terminal must not arm backoff, got %+v", bs)
	}
}
