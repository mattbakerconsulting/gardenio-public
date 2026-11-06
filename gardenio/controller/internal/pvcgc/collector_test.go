package pvcgc

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testNS = "gardenio"

func newTestCollector(t *testing.T, ttl time.Duration, pvcs []corev1.PersistentVolumeClaim, pods []corev1.Pod) *Collector {
	t.Helper()
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	for i := range pvcs {
		if _, err := client.CoreV1().PersistentVolumeClaims(testNS).Create(ctx, &pvcs[i], metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating PVC %s: %v", pvcs[i].Name, err)
		}
	}
	for i := range pods {
		if _, err := client.CoreV1().Pods(testNS).Create(ctx, &pods[i], metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating pod %s: %v", pods[i].Name, err)
		}
	}

	return New(client, testNS, ttl, slog.Default())
}

func gardenioPVC(name string, age time.Duration) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         testNS,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
			Labels:            map[string]string{"app.kubernetes.io/name": "gardenio"},
		},
	}
}

func podWithPVC(name, pvcName string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNS,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "agent", Image: "test"}},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
}

func TestCollect_NoOrphans(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{
		gardenioPVC("crew-gardenio-crew-abc-workspace", 2*time.Hour),
	}
	pods := []corev1.Pod{
		podWithPVC("crew-gardenio-crew-abc", "crew-gardenio-crew-abc-workspace"),
	}
	c := newTestCollector(t, time.Hour, pvcs, pods)
	stats := c.Collect(context.Background())

	if stats.TotalPVCs != 1 {
		t.Errorf("TotalPVCs = %d, want 1", stats.TotalPVCs)
	}
	if stats.OrphanPVCs != 0 {
		t.Errorf("OrphanPVCs = %d, want 0", stats.OrphanPVCs)
	}
	if stats.DeletedPVCs != 0 {
		t.Errorf("DeletedPVCs = %d, want 0", stats.DeletedPVCs)
	}
}

func TestCollect_OrphanAboveTTL(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{
		gardenioPVC("crew-gardenio-crew-old-workspace", 2*time.Hour),
	}
	c := newTestCollector(t, time.Hour, pvcs, nil)
	stats := c.Collect(context.Background())

	if stats.OrphanPVCs != 1 {
		t.Errorf("OrphanPVCs = %d, want 1", stats.OrphanPVCs)
	}
	if stats.DeletedPVCs != 1 {
		t.Errorf("DeletedPVCs = %d, want 1", stats.DeletedPVCs)
	}

	// Verify the PVC was actually deleted.
	remaining, err := c.client.CoreV1().PersistentVolumeClaims(testNS).List(
		context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items) != 0 {
		t.Errorf("expected 0 PVCs remaining, got %d", len(remaining.Items))
	}
}

func TestCollect_OrphanBelowTTL(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{
		gardenioPVC("crew-gardenio-crew-new-workspace", 30*time.Minute),
	}
	c := newTestCollector(t, time.Hour, pvcs, nil)
	stats := c.Collect(context.Background())

	if stats.OrphanPVCs != 1 {
		t.Errorf("OrphanPVCs = %d, want 1", stats.OrphanPVCs)
	}
	if stats.DeletedPVCs != 0 {
		t.Errorf("DeletedPVCs = %d, want 0 (below TTL)", stats.DeletedPVCs)
	}
}

func TestCollect_MixedPVCs(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{
		gardenioPVC("crew-gardenio-crew-active-workspace", 3*time.Hour),
		gardenioPVC("crew-gardenio-crew-old-workspace", 5*time.Hour),
		gardenioPVC("crew-gardenio-crew-recent-workspace", 10*time.Minute),
	}
	pods := []corev1.Pod{
		podWithPVC("crew-gardenio-crew-active", "crew-gardenio-crew-active-workspace"),
	}
	c := newTestCollector(t, time.Hour, pvcs, pods)
	stats := c.Collect(context.Background())

	if stats.TotalPVCs != 3 {
		t.Errorf("TotalPVCs = %d, want 3", stats.TotalPVCs)
	}
	if stats.OrphanPVCs != 2 {
		t.Errorf("OrphanPVCs = %d, want 2", stats.OrphanPVCs)
	}
	if stats.DeletedPVCs != 1 {
		t.Errorf("DeletedPVCs = %d, want 1 (only old one above TTL)", stats.DeletedPVCs)
	}
}

func TestCollect_NoPVCs(t *testing.T) {
	c := newTestCollector(t, time.Hour, nil, nil)
	stats := c.Collect(context.Background())

	if stats.TotalPVCs != 0 {
		t.Errorf("TotalPVCs = %d, want 0", stats.TotalPVCs)
	}
}

func TestMountedPVCs(t *testing.T) {
	pods := []corev1.Pod{
		podWithPVC("pod-a", "pvc-a"),
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pod-b"},
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Name: "c", Image: "i"}},
				Volumes: []corev1.Volume{
					{Name: "empty", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "ws", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pvc-b"}}},
				},
			},
		},
	}

	m := mountedPVCs(pods)
	if !m["pvc-a"] {
		t.Error("pvc-a should be mounted")
	}
	if !m["pvc-b"] {
		t.Error("pvc-b should be mounted")
	}
	if m["pvc-c"] {
		t.Error("pvc-c should not be mounted")
	}
}

// TestCollect_AnnotationPreservesPVCAfterRespawn covers the bug-fix
// scenario: a PVC may be CreationTimestamp-old, but if the
// AnnotationPVCLastActive timestamp was refreshed by a recent ensurePVC
// (i.e. an agent re-spawned against the same PVC name), the GC must NOT
// delete it. Without this carry-through, session resume breaks for
// long-lived thread agents.
func TestCollect_AnnotationPreservesPVCAfterRespawn(t *testing.T) {
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "respawned-workspace",
			Namespace:         testNS,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-72 * time.Hour)}, // ancient
			Annotations: map[string]string{
				AnnotationPVCLastActive: time.Now().Add(-30 * time.Minute).Format(time.RFC3339),
			},
			Labels: map[string]string{"app.kubernetes.io/name": "gardenio"},
		},
	}
	c := newTestCollector(t, time.Hour, []corev1.PersistentVolumeClaim{pvc}, nil)
	stats := c.Collect(context.Background())

	if stats.OrphanPVCs != 1 {
		t.Errorf("OrphanPVCs = %d, want 1 (pod-less but recently active)", stats.OrphanPVCs)
	}
	if stats.DeletedPVCs != 0 {
		t.Errorf("DeletedPVCs = %d, want 0 (last-active < TTL)", stats.DeletedPVCs)
	}
}

// TestCollect_AnnotationDeletesIdlePVC: the inverse — annotation present,
// but its timestamp is older than TTL, so the PVC must be deleted.
func TestCollect_AnnotationDeletesIdlePVC(t *testing.T) {
	pvc := corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "idle-workspace",
			Namespace:         testNS,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-30 * time.Minute)}, // recently created
			Annotations: map[string]string{
				AnnotationPVCLastActive: time.Now().Add(-25 * time.Hour).Format(time.RFC3339),
			},
			Labels: map[string]string{"app.kubernetes.io/name": "gardenio"},
		},
	}
	c := newTestCollector(t, 24*time.Hour, []corev1.PersistentVolumeClaim{pvc}, nil)
	stats := c.Collect(context.Background())

	if stats.DeletedPVCs != 1 {
		t.Errorf("DeletedPVCs = %d, want 1 (last-active > TTL)", stats.DeletedPVCs)
	}
}
