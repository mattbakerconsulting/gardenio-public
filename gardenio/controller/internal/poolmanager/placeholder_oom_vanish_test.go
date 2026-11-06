package poolmanager

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/podmanager"
)

// TestReconcile_VanishedOOMMintArmsBackoff is the ka-0000005929 fix: on the
// single full fat gVisor node, the kubelet rejects an over-fenced warm-B
// placeholder OutOfmemory and the terminal pod is garbage-collected within
// seconds — faster than the 30s reconcile. The rejection is therefore invisible
// to BOTH the synchronous podRejected() Get (the reject lands async, after
// Create) AND the next pass's terminal list (the pod is already gone), so the
// pre-fix bucket saw zero rejections, cleared its backoff, and re-minted a fresh
// doomed batch every pass — the ~225-events/window churn the bug tracks.
//
// This test drives that exact GC-beats-reconcile race: pass 1's mint comes up
// Running (podRejected miss → recorded in recentlyMinted); between passes the
// pod is DELETED outright (the reject + immediate GC), so it never appears in
// any terminal list; pass 2 must reclassify the vanished prior mint (confirmed
// gone via Get → NotFound) as a delayed rejection and arm the backoff.
func TestReconcile_VanishedOOMMintArmsBackoff(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	// Fresh mints come up Running (not terminal) so the synchronous podRejected()
	// Get in the mint loop misses — the exact blind spot; the pod is recorded in
	// recentlyMinted and its fate is decided on the next pass.
	runningOnCreate(client)

	p := newTestPool(t, client, 1)
	clock := time.Unix(1_700_000_000, 0)
	p.nowFunc = func() time.Time { return clock }

	// Pass 1: mints 1 (Running). recentlyMinted records it; backoff not armed
	// (no rejection has been observed yet).
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	if len(p.recentlyMinted) != 1 {
		t.Fatalf("pass 1 must record the just-minted placeholder, got %+v", p.recentlyMinted)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs != nil {
		t.Fatalf("pass 1 must NOT arm backoff (no rejection observed yet), got %+v", bs)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("pass 1 must mint exactly 1 placeholder, got %d", len(pods))
	}
	mintedName := pods[0].Name

	// Simulate the OutOfmemory reject + IMMEDIATE GC: the Failed pod is cleaned
	// up before the next reconcile can ever observe it in the terminal list. This
	// is what defeats the async-terminal reclassification (ka-0000000088) — the
	// pod is simply gone.
	if err := client.CoreV1().Pods("gvisor").Delete(context.Background(), mintedName, metav1.DeleteOptions{}); err != nil {
		t.Fatalf("simulating reject+GC (delete): %v", err)
	}

	// Pass 2: the vanished prior mint is confirmed gone (Get → NotFound) and
	// reclassified as a delayed rejection, arming the backoff — even though no
	// Failed pod was ever observed. Pre-fix this pass would have cleared the
	// backoff and re-minted, storming forever.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs == nil {
		t.Errorf("a vanished (OOM-rejected + GC'd) mint must arm the backoff, got nil")
	}
}

// TestReconcile_PromotedMintNotCountedAsRejection is the false-positive guard on
// the ka-0000005929 vanished-mint reclassification: a mint that reached Ready
// and was claimed+promoted to a live thawed agent (ClaimWarmPlaceholderForThaw)
// ALSO drops out of the warm-placeholder listing — but its pod still EXISTS
// under the same name. A blind "no longer a warm placeholder ⇒ rejected"
// heuristic would falsely arm the backoff on a bucket that is actively serving
// thaws, starving the very pool the feature exists to keep warm. The confirming
// Get (podVanished) is what prevents this: only a definite NotFound counts.
func TestReconcile_PromotedMintNotCountedAsRejection(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	runningOnCreate(client)

	p := newTestPool(t, client, 1)
	clock := time.Unix(1_700_000_000, 0)
	p.nowFunc = func() time.Time { return clock }

	// Pass 1: mint 1 (Running) → recorded in recentlyMinted.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("pass 1 must mint exactly 1, got %d", len(pods))
	}
	mintedName := pods[0].Name

	// The mint reaches Ready and a thaw claims+promotes it: it drops the
	// warm-placeholder label and becomes a live agent pod under the SAME name. It
	// therefore leaves the warm-placeholder listing (like a GC'd reject) but the
	// pod is very much alive.
	pod, err := client.CoreV1().Pods("gvisor").Get(context.Background(), mintedName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get minted pod: %v", err)
	}
	delete(pod.Labels, podmanager.LabelWarmPlaceholder)
	pod.Labels[podmanager.LabelAgent] = "gardenio-thawed-agent"
	pod.Labels[podmanager.LabelThawPlaceholder] = "true"
	if _, err := client.CoreV1().Pods("gvisor").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("promote minted pod: %v", err)
	}

	// Pass 2: the promoted mint is gone from the warm-placeholder listing, so the
	// vanished-mint path inspects it — but the confirming Get finds it alive, so
	// it is NOT counted as a rejection. The bucket re-mints healthily and the
	// backoff must stay clear.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs != nil {
		t.Errorf("a claimed+promoted mint (still alive) must not arm the backoff, got %+v", bs)
	}
}
