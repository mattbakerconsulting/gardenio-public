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

// The PVC GC must also sweep the PSA-privileged gVisor namespace where runsc
// freeze/thaw agents (and their workspace PVCs) live (ka-0000008076). Without
// SetExtraNamespaces a gVisor-namespace PVC is invisible to GC and leaks.

const gvisorNS = "gardenio-gvisor"

func gvisorPVC(name string, age time.Duration) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         gvisorNS,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
			Labels:            map[string]string{"app.kubernetes.io/name": "gardenio"},
		},
	}
}

func TestCollect_SweepsExtraNamespace(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	// Orphan in the primary namespace.
	mainOrphan := gardenioPVC("crew-gardenio-crew-main-workspace", 2*time.Hour)
	if _, err := client.CoreV1().PersistentVolumeClaims(testNS).Create(ctx, &mainOrphan, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating main PVC: %v", err)
	}
	// Orphan in the gVisor namespace (no pod mounts it).
	gvOrphan := gvisorPVC("crew-gardenio-gvisor-poc-canary-workspace", 2*time.Hour)
	if _, err := client.CoreV1().PersistentVolumeClaims(gvisorNS).Create(ctx, &gvOrphan, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating gVisor PVC: %v", err)
	}

	c := New(client, testNS, time.Hour, slog.Default())
	c.SetExtraNamespaces(gvisorNS)
	stats := c.Collect(ctx)

	if stats.TotalPVCs != 2 {
		t.Errorf("TotalPVCs = %d, want 2 (main + gVisor)", stats.TotalPVCs)
	}
	if stats.OrphanPVCs != 2 {
		t.Errorf("OrphanPVCs = %d, want 2", stats.OrphanPVCs)
	}
	if stats.DeletedPVCs != 2 {
		t.Errorf("DeletedPVCs = %d, want 2", stats.DeletedPVCs)
	}

	if _, err := client.CoreV1().PersistentVolumeClaims(gvisorNS).Get(ctx, gvOrphan.Name, metav1.GetOptions{}); err == nil {
		t.Error("gVisor-namespace orphan PVC should have been deleted")
	}
}

// Without SetExtraNamespaces the gVisor PVC is invisible — guards that the sweep
// is opt-in and the single-namespace deployment is unchanged.
func TestCollect_ExtraNamespaceOptIn(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	gvOrphan := gvisorPVC("crew-gardenio-gvisor-poc-canary-workspace", 2*time.Hour)
	if _, err := client.CoreV1().PersistentVolumeClaims(gvisorNS).Create(ctx, &gvOrphan, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating gVisor PVC: %v", err)
	}

	c := New(client, testNS, time.Hour, slog.Default())
	// No SetExtraNamespaces.
	stats := c.Collect(ctx)

	if stats.TotalPVCs != 0 {
		t.Errorf("TotalPVCs = %d, want 0 (primary ns only)", stats.TotalPVCs)
	}
	if _, err := client.CoreV1().PersistentVolumeClaims(gvisorNS).Get(ctx, gvOrphan.Name, metav1.GetOptions{}); err != nil {
		t.Error("gVisor-namespace PVC must NOT be touched when sweep is not enabled")
	}
}

// A PVC mounted by a pod in its own namespace is not an orphan; the per-namespace
// pod listing must not treat a gVisor PVC as orphaned just because the mounting
// pod lives in a different namespace.
func TestCollect_ExtraNamespacePVCInUse(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()

	gvPVC := gvisorPVC("crew-gardenio-gvisor-poc-canary-workspace", 2*time.Hour)
	if _, err := client.CoreV1().PersistentVolumeClaims(gvisorNS).Create(ctx, &gvPVC, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating gVisor PVC: %v", err)
	}
	gvPod := podWithPVC("crew-gardenio-gvisor-poc-canary", gvPVC.Name)
	gvPod.Namespace = gvisorNS
	if _, err := client.CoreV1().Pods(gvisorNS).Create(ctx, &gvPod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating gVisor pod: %v", err)
	}

	c := New(client, testNS, time.Hour, slog.Default())
	c.SetExtraNamespaces(gvisorNS)
	stats := c.Collect(ctx)

	if stats.OrphanPVCs != 0 {
		t.Errorf("OrphanPVCs = %d, want 0 (PVC mounted in same ns)", stats.OrphanPVCs)
	}
	if stats.DeletedPVCs != 0 {
		t.Errorf("DeletedPVCs = %d, want 0", stats.DeletedPVCs)
	}
}

// SetExtraNamespaces ignores empty entries and the primary namespace itself, so
// a PVC is never double-listed (which would double-count stats).
func TestSetExtraNamespaces_IgnoresEmptyAndPrimary(t *testing.T) {
	c := New(fake.NewSimpleClientset(), testNS, time.Hour, slog.Default())
	c.SetExtraNamespaces("", testNS)
	if len(c.extraNamespaces) != 0 {
		t.Fatalf("empty and primary-namespace entries must be ignored, got %v", c.extraNamespaces)
	}
}
