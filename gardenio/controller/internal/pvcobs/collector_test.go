package pvcobs

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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// fixedNow is the reference clock used by every test.
var fixedNow = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

// pvc builds a PVC in the given phase, created `age` before fixedNow, requesting
// `size` (e.g. "10Gi"), with the given storage class ("" = default/nil).
func pvc(namespace, name string, phase corev1.PersistentVolumeClaimPhase, age time.Duration, size, sc string) *corev1.PersistentVolumeClaim {
	p := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			CreationTimestamp: metav1.NewTime(fixedNow.Add(-age)),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: phase},
	}
	if sc != "" {
		p.Spec.StorageClassName = &sc
	}
	return p
}

// podRef builds a pod in `namespace` that mounts the named claim.
func podRef(namespace, podName, claimName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: namespace},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
				},
			}},
		},
	}
}

func newTestCollector(t *testing.T, objs ...runtime.Object) (*Collector, *fake.Clientset) {
	t.Helper()
	client := fake.NewSimpleClientset(objs...)
	c := New(client, DefaultThreshold, time.Minute, slog.Default())
	c.now = func() time.Time { return fixedNow }
	return c, client
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	return testutil.ToFloat64(g)
}

func TestCollect_ClassifiesOrphanedPendingPVCs(t *testing.T) {
	c, _ := newTestCollector(t,
		// Orphaned: Pending, 200d old, no pod. The tempo case.
		pvc("observability", "tempo-0", corev1.ClaimPending, 200*24*time.Hour, "10Gi", "gp2"),
		// Orphaned: Pending, 148d old, no pod. The relocated-redis case.
		pvc("dolt", "redis-master-0", corev1.ClaimPending, 148*24*time.Hour, "1Gi", "gardenio-gp3"),
		// NOT orphaned: Pending but young (within bind window).
		pvc("app", "fresh-0", corev1.ClaimPending, time.Hour, "5Gi", "gp2"),
		// NOT orphaned: Bound, regardless of age.
		pvc("app", "bound-0", corev1.ClaimBound, 300*24*time.Hour, "20Gi", "gp2"),
	)

	stats, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if stats.TotalPVCs != 4 {
		t.Errorf("TotalPVCs = %d, want 4", stats.TotalPVCs)
	}
	if stats.PendingPVCs != 3 {
		t.Errorf("PendingPVCs = %d, want 3", stats.PendingPVCs)
	}
	if stats.OrphanedPVCs != 2 {
		t.Errorf("OrphanedPVCs = %d, want 2", stats.OrphanedPVCs)
	}
	// 10Gi + 1Gi = 11 * 1024^3.
	wantBytes := int64(11) * 1024 * 1024 * 1024
	if stats.RequestedBytes != wantBytes {
		t.Errorf("RequestedBytes = %d, want %d", stats.RequestedBytes, wantBytes)
	}
	if want := 200 * 24 * time.Hour; stats.MaxAge != want {
		t.Errorf("MaxAge = %v, want %v", stats.MaxAge, want)
	}

	if got := gaugeValue(t, c.count); got != 2 {
		t.Errorf("count gauge = %v, want 2", got)
	}
	if got := gaugeValue(t, c.maxAgeGauge); got != (200 * 24 * time.Hour).Seconds() {
		t.Errorf("maxAge gauge = %v, want %v", got, (200 * 24 * time.Hour).Seconds())
	}
	if got := gaugeValue(t, c.requestBytes); got != float64(wantBytes) {
		t.Errorf("requestBytes gauge = %v, want %v", got, float64(wantBytes))
	}
	// Per-PVC info series present for both orphans, absent for the healthy ones.
	if got := testutil.ToFloat64(c.info.WithLabelValues("observability", "tempo-0", "gp2")); got != (200 * 24 * time.Hour).Seconds() {
		t.Errorf("info{tempo-0} = %v, want %v", got, (200 * 24 * time.Hour).Seconds())
	}
	if n := testutil.CollectAndCount(c.info); n != 2 {
		t.Errorf("info series count = %d, want 2", n)
	}
}

func TestCollect_LivePodExcludesFromOrphaned(t *testing.T) {
	// A long-Pending PVC that a live pod still references is NOT orphaned — it
	// is mid-scheduling / unschedulable, a different problem.
	c, _ := newTestCollector(t,
		pvc("app", "wanted-0", corev1.ClaimPending, 30*24*time.Hour, "10Gi", "gp2"),
		podRef("app", "consumer", "wanted-0"),
	)
	stats, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if stats.OrphanedPVCs != 0 {
		t.Errorf("OrphanedPVCs = %d, want 0 (live consumer)", stats.OrphanedPVCs)
	}
	if got := gaugeValue(t, c.count); got != 0 {
		t.Errorf("count gauge = %v, want 0", got)
	}
}

func TestCollect_ClaimNameIsNamespaceScoped(t *testing.T) {
	// A pod in namespace A referencing "shared" must NOT rescue an orphaned
	// "shared" PVC in namespace B (claimName is only unique within a namespace).
	c, _ := newTestCollector(t,
		pvc("b", "shared", corev1.ClaimPending, 30*24*time.Hour, "10Gi", "gp2"),
		podRef("a", "consumer", "shared"),
	)
	stats, err := c.Collect(context.Background())
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if stats.OrphanedPVCs != 1 {
		t.Errorf("OrphanedPVCs = %d, want 1 (cross-namespace pod must not rescue)", stats.OrphanedPVCs)
	}
}

func TestCollect_NilStorageClassLabel(t *testing.T) {
	c, _ := newTestCollector(t,
		pvc("app", "noclass-0", corev1.ClaimPending, 30*24*time.Hour, "10Gi", ""),
	)
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := testutil.ToFloat64(c.info.WithLabelValues("app", "noclass-0", "<none>")); got == 0 {
		t.Error("expected info series under storageclass=<none> for nil-class PVC")
	}
}

func TestCollect_ResolvedOrphanDropsFromInfoSeries(t *testing.T) {
	c, client := newTestCollector(t,
		pvc("observability", "tempo-0", corev1.ClaimPending, 200*24*time.Hour, "10Gi", "gp2"),
	)
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("first Collect: %v", err)
	}
	if n := testutil.CollectAndCount(c.info); n != 1 {
		t.Fatalf("info series after first sweep = %d, want 1", n)
	}
	// Operator cleans it up; next sweep must drop the stale per-PVC series and
	// zero the summary gauges.
	if err := client.CoreV1().PersistentVolumeClaims("observability").Delete(context.Background(), "tempo-0", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("second Collect: %v", err)
	}
	if n := testutil.CollectAndCount(c.info); n != 0 {
		t.Errorf("info series after cleanup = %d, want 0 (stale series must drop)", n)
	}
	if got := gaugeValue(t, c.count); got != 0 {
		t.Errorf("count gauge after cleanup = %v, want 0", got)
	}
	if got := gaugeValue(t, c.maxAgeGauge); got != 0 {
		t.Errorf("maxAge gauge after cleanup = %v, want 0", got)
	}
}

func TestCollect_ListErrorLeavesGaugesUntouched(t *testing.T) {
	c, client := newTestCollector(t,
		pvc("observability", "tempo-0", corev1.ClaimPending, 200*24*time.Hour, "10Gi", "gp2"),
	)
	// Seed a known-good value.
	if _, err := c.Collect(context.Background()); err != nil {
		t.Fatalf("seed Collect: %v", err)
	}
	seeded := gaugeValue(t, c.count)
	if seeded != 1 {
		t.Fatalf("seeded count = %v, want 1", seeded)
	}

	client.PrependReactor("list", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom: apiserver unavailable")
	})
	if _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("Collect returned nil error on a list failure, want error")
	}
	if got := gaugeValue(t, c.count); got != seeded {
		t.Errorf("count gauge = %v after list error, want %v (must be left untouched, not zeroed)", got, seeded)
	}
}

func TestCollect_PodListErrorIsFatalToSweep(t *testing.T) {
	c, client := newTestCollector(t,
		pvc("observability", "tempo-0", corev1.ClaimPending, 200*24*time.Hour, "10Gi", "gp2"),
	)
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("boom: pods list forbidden")
	})
	if _, err := c.Collect(context.Background()); err == nil {
		t.Fatal("Collect returned nil error when pod list failed, want error")
	}
}

func TestNew_DefaultsThreshold(t *testing.T) {
	c := New(fake.NewSimpleClientset(), 0, time.Minute, slog.Default())
	if c.threshold != DefaultThreshold {
		t.Errorf("threshold = %v, want DefaultThreshold %v", c.threshold, DefaultThreshold)
	}
}

func TestRegister(t *testing.T) {
	c := New(fake.NewSimpleClientset(), DefaultThreshold, time.Minute, slog.Default())
	if err := c.Register(prometheus.NewRegistry()); err != nil {
		t.Fatalf("Register: %v", err)
	}
}

func TestRunLoop_DisabledOnNonPositiveInterval(t *testing.T) {
	c := New(fake.NewSimpleClientset(), DefaultThreshold, 0, slog.Default())
	// Must return immediately (no ticker) rather than block.
	done := make(chan struct{})
	go func() { c.RunLoop(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunLoop did not return on non-positive interval")
	}
}
