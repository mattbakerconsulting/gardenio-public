package poolmanager

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/podmanager"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// probePod builds a Ready gVisor probe pod on node.
func probePod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{DefaultProbeSelectorKey: DefaultProbeSelectorValue},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// testPoolImage is the agent image newTestPool configures; readyPlaceholder
// stamps it so a default-built placeholder reads as fresh (current-image) to the
// upgrade-staleness reaper (ka-0000000890).
const testPoolImage = "agent:latest"

// readyPlaceholder builds a Ready warm placeholder pod on node, stamped with the
// pool's current image (so it counts toward MinSize and is never cycled as
// stale). Use placeholderWithImage to build a stale-image (old-controller)
// placeholder.
func readyPlaceholder(name, ns, node string) *corev1.Pod {
	return placeholderWithImage(name, ns, node, testPoolImage)
}

// placeholderWithImage builds a Ready warm placeholder stamped with a specific
// source image. A value != testPoolImage (or "" for an unstamped pod minted
// before the stamp existed) reads as stale to the reaper.
func placeholderWithImage(name, ns, node, image string) *corev1.Pod {
	ann := map[string]string{}
	if image != "" {
		ann[podmanager.AnnotationWarmImage] = image
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: ann,
			Labels: map[string]string{
				podmanager.LabelApp:             podmanager.LabelAppValue,
				podmanager.LabelWarmPlaceholder: podmanager.WarmPlaceholderReady,
			},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

func newTestPool(t *testing.T, client *fake.Clientset, minSize int) *PlaceholderPool {
	t.Helper()
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace: "gvisor",
		Image:     "agent:latest",
		MinSize:   minSize,
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	return p
}

func countPlaceholders(t *testing.T, client *fake.Clientset, ns string) []corev1.Pod {
	t.Helper()
	pods, err := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: podmanager.LabelWarmPlaceholder,
	})
	if err != nil {
		t.Fatalf("listing placeholders: %v", err)
	}
	return pods.Items
}

func TestNewPlaceholderPool_DisabledReturnsNil(t *testing.T) {
	client := fake.NewClientset()
	cases := []struct {
		name string
		cfg  PlaceholderPoolConfig
	}{
		{"zero min size", PlaceholderPoolConfig{Namespace: "gvisor", Image: "img", MinSize: 0}},
		{"empty namespace", PlaceholderPoolConfig{Namespace: "", Image: "img", MinSize: 2}},
		{"empty image", PlaceholderPoolConfig{Namespace: "gvisor", Image: "", MinSize: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if p := NewPlaceholderPool(client, tc.cfg, testLogger()); p != nil {
				t.Errorf("expected nil pool for disabled config, got %v", p)
			}
		})
	}
}

func TestReconcile_CreatesPlaceholdersPerNode(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		probePod("probe-b", "gvisor", "node-b"),
	)
	p := newTestPool(t, client, 2)

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 4 { // 2 nodes × MinSize 2
		t.Fatalf("expected 4 placeholders (2 nodes × 2), got %d", len(pods))
	}
	perNode := map[string]int{}
	for _, pod := range pods {
		perNode[pod.Spec.NodeName]++
		if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != podmanager.RuntimeClassRunsc {
			t.Errorf("placeholder %s not pinned to runsc runtime class", pod.Name)
		}
		if pod.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderReady {
			t.Errorf("placeholder %s not labeled ready", pod.Name)
		}
		if _, hasAgent := pod.Labels[podmanager.LabelAgent]; hasAgent {
			t.Errorf("placeholder %s must NOT carry the agent label (would be reaped by reconciler)", pod.Name)
		}
	}
	if perNode["node-a"] != 2 || perNode["node-b"] != 2 {
		t.Errorf("expected 2 per node, got %v", perNode)
	}
}

// When a PriorityClassName is configured, every minted placeholder carries it
// so a cold real agent can preempt an idle placeholder under node pressure
// (ka-0000000722). Unset leaves placeholders at default priority.
func TestReconcile_StampsPriorityClass(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace:         "gvisor",
		Image:             "agent:latest",
		MinSize:           2,
		PriorityClassName: "gardenio-prewarmed",
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) == 0 {
		t.Fatal("expected placeholders to be created")
	}
	for _, pod := range pods {
		if pod.Spec.PriorityClassName != "gardenio-prewarmed" {
			t.Errorf("placeholder %s PriorityClassName = %q, want %q",
				pod.Name, pod.Spec.PriorityClassName, "gardenio-prewarmed")
		}
	}

	// No PriorityClassName configured → placeholders stay at default priority.
	plainClient := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	plain := newTestPool(t, plainClient, 1)
	if err := plain.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (plain): %v", err)
	}
	for _, pod := range countPlaceholders(t, plainClient, "gvisor") {
		if pod.Spec.PriorityClassName != "" {
			t.Errorf("placeholder %s PriorityClassName = %q, want empty when unset",
				pod.Name, pod.Spec.PriorityClassName)
		}
	}
}

// When ImagePullSecrets is configured, every minted legacy placeholder carries
// them so the runsc image pull authenticates to the private registry instead of
// falling back to an anonymous token and 403-ImagePullBackOffing on a new
// release tag (ka-0000000402). Unset leaves placeholders pulling anonymously.
func TestReconcile_StampsImagePullSecrets(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace:        "gvisor",
		Image:            "agent:latest",
		MinSize:          2,
		ImagePullSecrets: []string{"registry-pull-secret"},
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) == 0 {
		t.Fatal("expected placeholders to be created")
	}
	for _, pod := range pods {
		if len(pod.Spec.ImagePullSecrets) != 1 ||
			pod.Spec.ImagePullSecrets[0].Name != "registry-pull-secret" {
			t.Errorf("placeholder %s ImagePullSecrets = %+v, want [{registry-pull-secret}]",
				pod.Name, pod.Spec.ImagePullSecrets)
		}
	}

	// No ImagePullSecrets configured → placeholders carry none (legacy behavior).
	plainClient := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	plain := newTestPool(t, plainClient, 1)
	if err := plain.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile (plain): %v", err)
	}
	for _, pod := range countPlaceholders(t, plainClient, "gvisor") {
		if len(pod.Spec.ImagePullSecrets) != 0 {
			t.Errorf("placeholder %s ImagePullSecrets = %+v, want none when unset",
				pod.Name, pod.Spec.ImagePullSecrets)
		}
	}
}

// A placeholder that already carries pull secrets — as the snapshot builder
// produces by DeepCopying the live Pod A template — keeps its own secrets; the
// pool's configured secrets do NOT clobber them (ka-0000000402). This mirrors
// applyCommonConfig's len(spec.ImagePullSecrets)==0 guard for real agent pods.
func TestCreatePod_DoesNotClobberExistingImagePullSecrets(t *testing.T) {
	client := fake.NewClientset()
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace:        "gvisor",
		Image:            "agent:latest",
		MinSize:          1,
		ImagePullSecrets: []string{"pool-secret"},
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-snap-1", Namespace: "gvisor"},
		Spec: corev1.PodSpec{
			NodeName:         "node-a",
			ImagePullSecrets: []corev1.LocalObjectReference{{Name: "template-secret"}},
		},
	}
	if err := p.createPod(context.Background(), pod, "node-a"); err != nil {
		t.Fatalf("createPod: %v", err)
	}
	got, err := client.CoreV1().Pods("gvisor").Get(context.Background(), "warm-snap-1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get created pod: %v", err)
	}
	if len(got.Spec.ImagePullSecrets) != 1 || got.Spec.ImagePullSecrets[0].Name != "template-secret" {
		t.Errorf("ImagePullSecrets = %+v, want the template's [{template-secret}] untouched",
			got.Spec.ImagePullSecrets)
	}
}

func TestReconcile_NoGVisorNodesNoOp(t *testing.T) {
	client := fake.NewClientset() // no probe pods
	p := newTestPool(t, client, 2)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pods := countPlaceholders(t, client, "gvisor"); len(pods) != 0 {
		t.Fatalf("expected no placeholders without gVisor nodes, got %d", len(pods))
	}
}

func TestReconcile_RefillsToMinSize(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("warm-existing-1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 3)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// 1 existing + 2 created = 3
	if pods := countPlaceholders(t, client, "gvisor"); len(pods) != 3 {
		t.Fatalf("expected 3 placeholders after refill, got %d", len(pods))
	}
}

func TestReconcile_AtTargetNoCreate(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
		readyPlaceholder("w2", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 2)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pods := countPlaceholders(t, client, "gvisor"); len(pods) != 2 {
		t.Fatalf("expected pool to stay at 2, got %d", len(pods))
	}
}

func TestClaimWarmPlaceholder_FlipsLabelAndReturnsName(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 1)

	name, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", "")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if name != "w1" {
		t.Fatalf("expected to claim w1, got %q", name)
	}
	got, err := client.CoreV1().Pods("gvisor").Get(context.Background(), "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get claimed pod: %v", err)
	}
	if got.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderClaimed {
		t.Errorf("expected claimed label, got %q", got.Labels[podmanager.LabelWarmPlaceholder])
	}
}

func TestClaimWarmPlaceholder_MissOnEmptyNode(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 1)
	// No placeholder on node-b.
	if _, err := p.ClaimWarmPlaceholder(context.Background(), "node-b", ""); err != ErrNoWarmPlaceholder {
		t.Fatalf("expected ErrNoWarmPlaceholder for empty node, got %v", err)
	}
}

func TestClaimWarmPlaceholder_NotReadySkipped(t *testing.T) {
	pending := readyPlaceholder("w-pending", "gvisor", "node-a")
	pending.Status.Phase = corev1.PodPending
	pending.Status.Conditions = nil
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		pending,
	)
	p := newTestPool(t, client, 1)
	if _, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", ""); err != ErrNoWarmPlaceholder {
		t.Fatalf("expected miss when only a pending placeholder exists, got %v", err)
	}
}

func TestClaim_ThenReconcileRefills(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 1)

	if _, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// After claim, the Ready pool on node-a is empty (w1 is "claimed"), so a
	// reconcile must create one fresh Ready placeholder back to MinSize.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	ready := 0
	for _, pod := range countPlaceholders(t, client, "gvisor") {
		if pod.Labels[podmanager.LabelWarmPlaceholder] == podmanager.WarmPlaceholderReady {
			ready++
		}
	}
	if ready != 1 {
		t.Fatalf("expected 1 fresh ready placeholder after claim+reconcile, got %d", ready)
	}
}

func TestReconcile_ReapsStaleClaimed(t *testing.T) {
	claimed := readyPlaceholder("w-claimed", "gvisor", "node-a")
	claimed.Labels[podmanager.LabelWarmPlaceholder] = podmanager.WarmPlaceholderClaimed
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		claimed,
	)
	p := newTestPool(t, client, 1)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// The stale claimed pod is reaped; a fresh ready placeholder is created.
	for _, pod := range countPlaceholders(t, client, "gvisor") {
		if pod.Name == "w-claimed" {
			t.Errorf("stale claimed placeholder was not reaped")
		}
	}
}

func TestClaimWarmPlaceholder_NilPoolReturnsMiss(t *testing.T) {
	var p *PlaceholderPool
	if _, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", ""); err != ErrNoWarmPlaceholder {
		t.Fatalf("nil pool should miss, got %v", err)
	}
}

// TestReconcile_ReapsTerminalPlaceholder: a terminal (Failed/Succeeded)
// placeholder — e.g. one the kubelet rejected with OutOfcpu — must be REAPED,
// not merely uncounted. The legacy behavior (skip-but-keep) leaked these pods on
// the API server forever and left the bucket permanently below target, so the
// fill re-minted a doomed batch every pass (ka-0000000239, the d2a948e7 storm of
// 399 dead pods). After this pass the dead pod is gone and exactly one fresh
// (legacy, blind /work) placeholder remains.
func TestReconcile_ReapsTerminalPlaceholder(t *testing.T) {
	dead := readyPlaceholder("w-dead", "gvisor", "node-a")
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
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("expected dead pod reaped + 1 fresh = 1, got %d", len(pods))
	}
	for _, pod := range pods {
		if pod.Name == "w-dead" {
			t.Fatalf("terminal placeholder w-dead was not reaped")
		}
	}
}

// rejectOnCreate installs a fake-client reactor that simulates the kubelet
// synchronously rejecting a node-pinned placeholder it cannot admit (OutOfcpu):
// the pod is stored in a terminal Failed phase rather than Pending. This is the
// d2a948e7 storm signature — every minted pod dies on admission (ka-0000000239).
func rejectOnCreate(client *fake.Clientset) {
	client.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Reason = "OutOfcpu"
		// Let the default tracker store it; we only mutate the object in place.
		return false, nil, nil
	})
}

// TestReconcile_AdmissionFailureBackoff: a bucket whose every minted placeholder
// is immediately rejected (OutOfcpu) must enter an exponential mint cooldown so
// the pool stops re-storming a doomed batch every pass. Without the backoff this
// is the d2a948e7 leak: 3 dead pods minted per 30s reconcile, forever.
func TestReconcile_AdmissionFailureBackoff(t *testing.T) {
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"))
	rejectOnCreate(client)
	p := newTestPool(t, client, 3)

	clock := time.Unix(1_700_000_000, 0)
	p.nowFunc = func() time.Time { return clock }

	// Pass 1: bucket is empty, mints 3 — all rejected. Backoff arms (1m).
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}
	bs := p.mintBackoff[bucketKey("node-a", "")]
	if bs == nil || bs.failures != 1 {
		t.Fatalf("expected backoff failures=1 after first all-rejected pass, got %+v", bs)
	}

	// Pass 2 immediately after: still inside the 1m cooldown — must NOT mint.
	// (The reaper removes the prior pass's terminal pods, so a non-backoff pool
	// would mint a fresh doomed batch here; the backoff is what prevents it.)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}
	if got := len(countPlaceholders(t, client, "gvisor")); got != 0 {
		t.Fatalf("expected 0 live placeholders during backoff (all reaped, none re-minted), got %d", got)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs.failures != 1 {
		t.Fatalf("backoff must not escalate while skipping mint, failures=%d", bs.failures)
	}

	// Advance past the cooldown: pass 3 mints again, all rejected again → escalate.
	clock = clock.Add(2 * time.Minute)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs.failures != 2 {
		t.Fatalf("expected backoff failures=2 after second all-rejected mint pass, got %d", bs.failures)
	}
}

// TestReconcile_BackoffClearsWhenBucketHealthy: once a bucket reaches its Ready
// target, any prior admission-failure backoff is cleared so a transient capacity
// crunch never leaves a now-healthy bucket throttled (ka-0000000239).
func TestReconcile_BackoffClearsWhenBucketHealthy(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 1)
	// Pre-seed a stale backoff as if earlier passes had failed.
	p.mintBackoff[bucketKey("node-a", "")] = &backoffState{failures: 3, until: p.now().Add(time.Hour)}

	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if bs := p.mintBackoff[bucketKey("node-a", "")]; bs != nil {
		t.Fatalf("expected backoff cleared for at-target bucket, got %+v", bs)
	}
}

func TestShortNode(t *testing.T) {
	cases := map[string]string{
		"ip-10-0-1-2.us-west-2.compute.internal": "ip-10-0-1-2",
		"node-a":                                 "node-a",
		"averyveryveryverylongnodenamewithnodot": "averyveryveryverylon",
	}
	for in, want := range cases {
		if got := shortNode(in); got != want {
			t.Errorf("shortNode(%q) = %q, want %q", in, got, want)
		}
	}
}

// --- C2 warm-B claim+promote (ka-0000005266) --------------------------------

func TestClaimWarmPlaceholderForThaw_PromotesAtomically(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 1)

	promo := podmanager.WarmPlaceholderPromotion{
		AgentName:    "gardenio-agent-x",
		BeadID:       "gd-bead",
		CanonicalPod: "crew-gardenio-crew-gardenio-agent-x",
	}
	name, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-a", "", promo)
	if err != nil {
		t.Fatalf("claim+promote: %v", err)
	}
	if name != "w1" {
		t.Fatalf("expected to claim w1, got %q", name)
	}

	got, err := client.CoreV1().Pods("gvisor").Get(context.Background(), "w1", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get promoted pod: %v", err)
	}
	// The warm-placeholder label MUST be gone (so the pool reaper never deletes
	// it mid-graft) — neither "ready" nor "claimed".
	if _, ok := got.Labels[podmanager.LabelWarmPlaceholder]; ok {
		t.Errorf("promoted pod still carries warm-placeholder label: %q", got.Labels[podmanager.LabelWarmPlaceholder])
	}
	// It MUST now read as the agent's live thaw placeholder.
	if got.Labels[podmanager.LabelAgent] != promo.AgentName {
		t.Errorf("agent label = %q, want %q", got.Labels[podmanager.LabelAgent], promo.AgentName)
	}
	if got.Labels[podmanager.LabelThawPlaceholder] != "true" {
		t.Errorf("thaw-placeholder label = %q, want true", got.Labels[podmanager.LabelThawPlaceholder])
	}
	if got.Annotations[podmanager.AnnotationBeadID] != promo.BeadID {
		t.Errorf("bead-id annotation = %q, want %q", got.Annotations[podmanager.AnnotationBeadID], promo.BeadID)
	}
	if got.Annotations[podmanager.AnnotationCanonicalPod] != promo.CanonicalPod {
		t.Errorf("canonical-pod annotation = %q, want %q", got.Annotations[podmanager.AnnotationCanonicalPod], promo.CanonicalPod)
	}
}

func TestClaimWarmPlaceholderForThaw_ReaperSkipsPromoted(t *testing.T) {
	// After promotion the pod no longer carries the warm-placeholder label, so a
	// pool Reconcile must NOT see it as ready/pending/claimed and must NOT reap
	// it — the reconciler (not the pool) now owns its lifecycle.
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 1)

	if _, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-a", "",
		podmanager.WarmPlaceholderPromotion{AgentName: "a", CanonicalPod: "c"}); err != nil {
		t.Fatalf("claim+promote: %v", err)
	}
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	// w1 (now a live agent pod) must still exist; the reaper only deletes
	// warm-placeholder=claimed leftovers, and w1 no longer carries that label.
	if _, err := client.CoreV1().Pods("gvisor").Get(context.Background(), "w1", metav1.GetOptions{}); err != nil {
		t.Fatalf("promoted pod was deleted by reconcile (should be owned by reconciler now): %v", err)
	}
	// The pool listing (LabelWarmPlaceholder) no longer counts it, so the
	// reconcile refills node-a back to MinSize with a fresh placeholder.
	pods := countPlaceholders(t, client, "gvisor")
	if len(pods) != 1 {
		t.Fatalf("expected exactly 1 fresh warm placeholder after refill, got %d", len(pods))
	}
	if pods[0].Name == "w1" {
		t.Fatalf("the refill counted the promoted pod w1 — it must be invisible to the pool")
	}
}

func TestClaimWarmPlaceholderForThaw_MissOnEmptyNode(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	p := newTestPool(t, client, 1)
	_, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-b", "",
		podmanager.WarmPlaceholderPromotion{AgentName: "a", CanonicalPod: "c"})
	if err != ErrNoWarmPlaceholder {
		t.Fatalf("expected ErrNoWarmPlaceholder for empty node, got %v", err)
	}
}

func TestClaimWarmPlaceholderForThaw_NilPoolReturnsMiss(t *testing.T) {
	var p *PlaceholderPool
	_, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-a", "",
		podmanager.WarmPlaceholderPromotion{AgentName: "a"})
	if err != ErrNoWarmPlaceholder {
		t.Fatalf("nil pool must return ErrNoWarmPlaceholder, got %v", err)
	}
}

func TestDeletePromoted(t *testing.T) {
	// A promoted pod no longer carries the warm-placeholder label; DeletePromoted
	// removes it by name (the caller owns teardown after a failed graft).
	promoted := readyPlaceholder("warm-b-node-a-1", "gvisor", "node-a")
	delete(promoted.Labels, podmanager.LabelWarmPlaceholder)
	promoted.Labels[podmanager.LabelAgent] = "alpha"
	client := fake.NewClientset(probePod("probe-a", "gvisor", "node-a"), promoted)
	p := newTestPool(t, client, 1)

	if err := p.DeletePromoted(context.Background(), "warm-b-node-a-1"); err != nil {
		t.Fatalf("DeletePromoted: %v", err)
	}
	if _, err := client.CoreV1().Pods("gvisor").Get(context.Background(), "warm-b-node-a-1", metav1.GetOptions{}); err == nil {
		t.Fatal("promoted pod should be deleted")
	}
	// Idempotent: a second delete (already gone) is not an error.
	if err := p.DeletePromoted(context.Background(), "warm-b-node-a-1"); err != nil {
		t.Errorf("DeletePromoted on absent pod must be a no-op, got %v", err)
	}
	// nil pool and empty name are no-ops.
	var nilPool *PlaceholderPool
	if err := nilPool.DeletePromoted(context.Background(), "x"); err != nil {
		t.Errorf("nil pool DeletePromoted must be nil, got %v", err)
	}
	if err := p.DeletePromoted(context.Background(), ""); err != nil {
		t.Errorf("empty name DeletePromoted must be nil, got %v", err)
	}
}

// --- C8 warm-B claim hit/miss metric (ka-0000000550) -------------------------

// fakeClaimRecorder captures RecordWarmPlaceholderClaim calls.
type fakeClaimRecorder struct {
	calls []string // "outcome/node/topology"
}

func (f *fakeClaimRecorder) RecordWarmPlaceholderClaim(outcome, node, topology string) bool {
	f.calls = append(f.calls, outcome+"/"+node+"/"+topology)
	return true
}

func newClaimPool(t *testing.T, client *fake.Clientset, rec ClaimRecorder) *PlaceholderPool {
	t.Helper()
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace:     "gvisor",
		Image:         "agent:latest",
		MinSize:       1,
		ClaimRecorder: rec,
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	return p
}

func TestClaimWarmPlaceholderForThaw_RecordsHit(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	rec := &fakeClaimRecorder{}
	p := newClaimPool(t, client, rec)

	if _, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-a", "",
		podmanager.WarmPlaceholderPromotion{AgentName: "a", CanonicalPod: "c"}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "hit/node-a/" {
		t.Errorf("calls = %v, want [hit/node-a/]", rec.calls)
	}
}

func TestClaimWarmPlaceholderForThaw_RecordsMiss(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		readyPlaceholder("w1", "gvisor", "node-a"),
	)
	rec := &fakeClaimRecorder{}
	p := newClaimPool(t, client, rec)

	// node-b has no placeholder → miss.
	if _, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-b", "",
		podmanager.WarmPlaceholderPromotion{AgentName: "a", CanonicalPod: "c"}); err != ErrNoWarmPlaceholder {
		t.Fatalf("expected ErrNoWarmPlaceholder, got %v", err)
	}
	if len(rec.calls) != 1 || rec.calls[0] != "miss/node-b/" {
		t.Errorf("calls = %v, want [miss/node-b/]", rec.calls)
	}
}

func TestClaimWarmPlaceholderForThaw_NilPoolRecordsNothing(t *testing.T) {
	// A nil pool (feature off) must not panic — and cannot record (no recorder).
	var p *PlaceholderPool
	if _, err := p.ClaimWarmPlaceholderForThaw(context.Background(), "node-a", "",
		podmanager.WarmPlaceholderPromotion{AgentName: "a"}); err != ErrNoWarmPlaceholder {
		t.Fatalf("nil pool must return ErrNoWarmPlaceholder, got %v", err)
	}
}

// --- Global concurrent-ephemeral cap (ka-0000004377 / ka-0000004129) ---

// newTestPoolWithCap builds an enabled pool with an explicit MaxConcurrent so
// the global-cap tests can exercise a ceiling below the per-bucket MinSize sum.
func newTestPoolWithCap(t *testing.T, client *fake.Clientset, minSize, maxConcurrent int) *PlaceholderPool {
	t.Helper()
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace:     "gvisor",
		Image:         "agent:latest",
		MinSize:       minSize,
		MaxConcurrent: maxConcurrent,
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	return p
}

// An unset (zero) MaxConcurrent falls back to DefaultMaxConcurrent so the pool
// is never left globally unbounded (ka-0000004377 / ka-0000004129).
func TestNewPlaceholderPool_MaxConcurrentDefaults(t *testing.T) {
	client := fake.NewClientset()
	p := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace: "gvisor", Image: "img", MinSize: 1,
	}, testLogger())
	if p == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	if p.maxConcurrent != DefaultMaxConcurrent {
		t.Fatalf("maxConcurrent = %d, want default %d", p.maxConcurrent, DefaultMaxConcurrent)
	}
	// A negative override is also coerced to the default.
	p2 := NewPlaceholderPool(client, PlaceholderPoolConfig{
		Namespace: "gvisor", Image: "img", MinSize: 1, MaxConcurrent: -5,
	}, testLogger())
	if p2.maxConcurrent != DefaultMaxConcurrent {
		t.Fatalf("negative MaxConcurrent → %d, want default %d", p2.maxConcurrent, DefaultMaxConcurrent)
	}
}

// The pool must never mint more than MaxConcurrent live placeholders across all
// (node,topology) buckets combined, even when the per-bucket MinSize fill would
// otherwise want more (ka-0000004377). Without the global cap, 3 nodes × MinSize
// 2 would mint 6 ephemeral placeholders — the unbounded footprint ka-0000004129
// guards against.
func TestReconcile_GlobalConcurrentCapClamps(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		probePod("probe-b", "gvisor", "node-b"),
		probePod("probe-c", "gvisor", "node-c"),
	)
	p := newTestPoolWithCap(t, client, 2, 4)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pods := countPlaceholders(t, client, "gvisor"); len(pods) != 4 {
		t.Fatalf("expected fill clamped to MaxConcurrent=4 (not 3×2=6), got %d", len(pods))
	}
}

// With MaxConcurrent unset the default 10 still bounds the pool: 6 nodes × MinSize
// 2 (=12 desired) is clamped to 10.
func TestReconcile_DefaultCapBoundsLargeFill(t *testing.T) {
	objs := []runtime.Object{}
	for _, n := range []string{"n1", "n2", "n3", "n4", "n5", "n6"} {
		objs = append(objs, probePod("probe-"+n, "gvisor", n))
	}
	client := fake.NewClientset(objs...)
	p := newTestPool(t, client, 2) // MaxConcurrent unset → DefaultMaxConcurrent (10)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pods := countPlaceholders(t, client, "gvisor"); len(pods) != DefaultMaxConcurrent {
		t.Fatalf("expected default cap %d (not 6×2=12), got %d", DefaultMaxConcurrent, len(pods))
	}
}

// A resume consumes a pool member (claim), and the next reconcile replenishes to
// maintain depth — but total live placeholders must stay at or below the global
// cap the whole time (ka-0000004377). The pool starts at the cap of 3 (2 on
// node-a, 1 on node-b) with MinSize 2 (desired 4 > cap). After a claim consumes a
// member on node-a, the refill mints exactly one replacement — restoring the pool
// to the cap of 3 total and never to the uncapped 4. (The fake clientset does not
// run kubelet, so the replacement is minted pending; the assertion is on the total
// footprint, which is what the cap bounds.)
func TestReconcile_ReplenishRespectsCapAfterClaim(t *testing.T) {
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		probePod("probe-b", "gvisor", "node-b"),
		readyPlaceholder("w-a1", "gvisor", "node-a"),
		readyPlaceholder("w-a2", "gvisor", "node-a"),
		readyPlaceholder("w-b1", "gvisor", "node-b"),
	)
	p := newTestPoolWithCap(t, client, 2, 3)

	// Consume a member (resume grafts onto w-a1 on node-a).
	if _, err := p.ClaimWarmPlaceholder(context.Background(), "node-a", ""); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Replenish: reap the claimed leftover and refill toward MinSize, bounded by
	// the cap. Net result: back to 3 total, never 4.
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("replenish reconcile: %v", err)
	}
	if total := len(countPlaceholders(t, client, "gvisor")); total != 3 {
		t.Fatalf("expected pool replenished back to the cap of 3 total, got %d", total)
	}
	// A fresh replacement was minted for the consumed member (the claimed leftover
	// was reaped, so w-a1 is gone and a new placeholder took a slot).
	names := map[string]bool{}
	for _, pod := range countPlaceholders(t, client, "gvisor") {
		names[pod.Name] = true
	}
	if names["w-a1"] {
		t.Errorf("claimed leftover w-a1 should have been reaped")
	}
}

// When the pool is already at the global cap with a stale-image bucket, the fill
// does not mint fresh replacements past the cap: existing live placeholders
// (fresh + stale) count against the budget so replenishment holds until the drain
// pass frees room (ka-0000004377).
func TestReconcile_CapCountsStaleAndPendingTowardBudget(t *testing.T) {
	stale := placeholderWithImage("w-stale", "gvisor", "node-a", "old-image:v1")
	client := fake.NewClientset(
		probePod("probe-a", "gvisor", "node-a"),
		stale,
	)
	// MinSize 2 wants a fresh fill, but the cap of 1 is already met by the single
	// stale placeholder, so no fresh placeholder is minted this pass.
	p := newTestPoolWithCap(t, client, 2, 1)
	if err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if total := len(countPlaceholders(t, client, "gvisor")); total != 1 {
		t.Fatalf("expected cap 1 to block fresh mint while stale holds the budget, got %d total", total)
	}
}
