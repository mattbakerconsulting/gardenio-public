package pvgc

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func newTestCollector(t *testing.T, ttl time.Duration, pvs []corev1.PersistentVolume) *Collector {
	t.Helper()
	client := fake.NewSimpleClientset()
	ctx := context.Background()

	for i := range pvs {
		if _, err := client.CoreV1().PersistentVolumes().Create(ctx, &pvs[i], metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating PV %s: %v", pvs[i].Name, err)
		}
	}

	return &Collector{
		client:     client,
		namespaces: map[string]bool{"gardenio": true, "gardenio-e2e": true},
		ttl:        ttl,
		logger:     slog.Default(),
	}
}

func releasedPV(name, namespace, pvcName string, age time.Duration) corev1.PersistentVolume {
	return corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.Time{Time: time.Now().Add(-age)},
			Finalizers: []string{
				"external-provisioner.volume.kubernetes.io/finalizer",
				"kubernetes.io/pv-protection",
				"external-attacher/ebs-csi-aws-com",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("10Gi"),
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			ClaimRef: &corev1.ObjectReference{
				Namespace: namespace,
				Name:      pvcName,
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{
					VolumeID: "vol-abc123",
					FSType:   "ext4",
				},
			},
		},
		Status: corev1.PersistentVolumeStatus{
			Phase: corev1.VolumeReleased,
		},
	}
}

func boundPV(name, namespace, pvcName string) corev1.PersistentVolume {
	pv := releasedPV(name, namespace, pvcName, time.Hour)
	pv.Status.Phase = corev1.VolumeBound
	pv.Finalizers = nil
	return pv
}

func TestCollect_NoReleasedPVs(t *testing.T) {
	pvs := []corev1.PersistentVolume{
		boundPV("pvc-111", "gardenio", "crew-abc-workspace"),
	}
	c := newTestCollector(t, 24*time.Hour, pvs)
	stats := c.Collect(context.Background())

	if stats.TotalPVs != 1 {
		t.Errorf("TotalPVs = %d, want 1", stats.TotalPVs)
	}
	if stats.ReleasedPVs != 0 {
		t.Errorf("ReleasedPVs = %d, want 0", stats.ReleasedPVs)
	}
	if stats.DeletedPVs != 0 {
		t.Errorf("DeletedPVs = %d, want 0", stats.DeletedPVs)
	}
}

func TestCollect_ReleasedAboveTTL(t *testing.T) {
	pvs := []corev1.PersistentVolume{
		releasedPV("pvc-old", "gardenio", "crew-old-workspace", 48*time.Hour),
	}
	c := newTestCollector(t, 24*time.Hour, pvs)
	stats := c.Collect(context.Background())

	if stats.ReleasedPVs != 1 {
		t.Errorf("ReleasedPVs = %d, want 1", stats.ReleasedPVs)
	}
	if stats.DeletedPVs != 1 {
		t.Errorf("DeletedPVs = %d, want 1", stats.DeletedPVs)
	}

	remaining, err := c.client.CoreV1().PersistentVolumes().List(
		context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items) != 0 {
		t.Errorf("expected 0 PVs remaining, got %d", len(remaining.Items))
	}
}

func TestCollect_ReleasedBelowTTL(t *testing.T) {
	pvs := []corev1.PersistentVolume{
		releasedPV("pvc-recent", "gardenio", "crew-new-workspace", 6*time.Hour),
	}
	c := newTestCollector(t, 24*time.Hour, pvs)
	stats := c.Collect(context.Background())

	if stats.ReleasedPVs != 1 {
		t.Errorf("ReleasedPVs = %d, want 1", stats.ReleasedPVs)
	}
	if stats.DeletedPVs != 0 {
		t.Errorf("DeletedPVs = %d, want 0 (below TTL)", stats.DeletedPVs)
	}
}

func TestCollect_UnmanagedNamespace(t *testing.T) {
	pvs := []corev1.PersistentVolume{
		releasedPV("pvc-other", "monitoring", "prometheus-data", 48*time.Hour),
	}
	c := newTestCollector(t, 24*time.Hour, pvs)
	stats := c.Collect(context.Background())

	if stats.ReleasedPVs != 1 {
		t.Errorf("ReleasedPVs = %d, want 1", stats.ReleasedPVs)
	}
	if stats.DeletedPVs != 0 {
		t.Errorf("DeletedPVs = %d, want 0 (unmanaged namespace)", stats.DeletedPVs)
	}
}

func TestCollect_MixedPVs(t *testing.T) {
	pvs := []corev1.PersistentVolume{
		boundPV("pvc-bound", "gardenio", "crew-active-workspace"),
		releasedPV("pvc-old-gardenio", "gardenio", "crew-old-workspace", 48*time.Hour),
		releasedPV("pvc-old-e2e", "gardenio-e2e", "crew-e2e-workspace", 48*time.Hour),
		releasedPV("pvc-recent", "gardenio", "crew-new-workspace", 6*time.Hour),
		releasedPV("pvc-other-ns", "monitoring", "data", 48*time.Hour),
	}
	c := newTestCollector(t, 24*time.Hour, pvs)
	stats := c.Collect(context.Background())

	if stats.TotalPVs != 5 {
		t.Errorf("TotalPVs = %d, want 5", stats.TotalPVs)
	}
	if stats.ReleasedPVs != 4 {
		t.Errorf("ReleasedPVs = %d, want 4", stats.ReleasedPVs)
	}
	// Only pvc-old-gardenio and pvc-old-e2e should be deleted (above TTL + managed NS)
	if stats.DeletedPVs != 2 {
		t.Errorf("DeletedPVs = %d, want 2", stats.DeletedPVs)
	}
}

// TestCollect_UnmanagedPVsNeverMutated is the F7 (ka-0000004901) security
// regression test. It verifies that PVs whose claimRef.namespace is NOT in
// the configured managed-namespace set are never patched (finalizer-clear)
// nor deleted by the collector, even though the controller's ClusterRole
// grants those verbs cluster-wide. A failure here means a compromised or
// buggy controller could mutate arbitrary PVs in the cluster.
func TestCollect_UnmanagedPVsNeverMutated(t *testing.T) {
	pvs := []corev1.PersistentVolume{
		// Released, well past TTL, in unmanaged namespaces. None of these
		// must be touched.
		releasedPV("pvc-monitoring", "monitoring", "prometheus-data", 48*time.Hour),
		releasedPV("pvc-kube-system", "kube-system", "etcd-snapshot", 48*time.Hour),
		releasedPV("pvc-other-tenant", "tenant-x", "app-data", 48*time.Hour),
		// One legitimately-managed PV, to confirm the collector still runs
		// against the managed set (the test would otherwise pass trivially
		// if the collector exited early for unrelated reasons).
		releasedPV("pvc-gardenio-ok", "gardenio", "crew-old-workspace", 48*time.Hour),
	}
	c := newTestCollector(t, 24*time.Hour, pvs)
	client := c.client.(*fake.Clientset)
	client.ClearActions()

	stats := c.Collect(context.Background())

	if stats.DeletedPVs != 1 {
		t.Errorf("DeletedPVs = %d, want 1 (only pvc-gardenio-ok)", stats.DeletedPVs)
	}

	// Walk every API call the collector made and assert no patch/delete
	// targeted an unmanaged PV. List is fine; everything else is suspect.
	unmanaged := map[string]bool{
		"pvc-monitoring":  true,
		"pvc-kube-system": true,
		"pvc-other-tenant": true,
	}
	for _, action := range client.Actions() {
		switch a := action.(type) {
		case ktesting.PatchAction:
			if unmanaged[a.GetName()] {
				t.Errorf("collector patched unmanaged PV %q (verb=%s)", a.GetName(), a.GetVerb())
			}
		case ktesting.DeleteAction:
			if unmanaged[a.GetName()] {
				t.Errorf("collector deleted unmanaged PV %q (verb=%s)", a.GetName(), a.GetVerb())
			}
		}
	}
}

func TestCollect_NoPVs(t *testing.T) {
	c := newTestCollector(t, 24*time.Hour, nil)
	stats := c.Collect(context.Background())

	if stats.TotalPVs != 0 {
		t.Errorf("TotalPVs = %d, want 0", stats.TotalPVs)
	}
}

func TestCollect_NoClaimRef(t *testing.T) {
	pv := releasedPV("pvc-no-ref", "gardenio", "something", 48*time.Hour)
	pv.Spec.ClaimRef = nil
	pvs := []corev1.PersistentVolume{pv}
	c := newTestCollector(t, 24*time.Hour, pvs)
	stats := c.Collect(context.Background())

	if stats.DeletedPVs != 0 {
		t.Errorf("DeletedPVs = %d, want 0 (no claimRef)", stats.DeletedPVs)
	}
}

func TestReleasedAge_WithTransitionTime(t *testing.T) {
	now := time.Now()
	transitionTime := now.Add(-3 * time.Hour)
	pv := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.Time{Time: now.Add(-48 * time.Hour)},
		},
		Status: corev1.PersistentVolumeStatus{
			LastPhaseTransitionTime: &metav1.Time{Time: transitionTime},
		},
	}
	age := releasedAge(pv, now)
	// Should use lastPhaseTransitionTime (3h) not creation (48h).
	if age < 2*time.Hour || age > 4*time.Hour {
		t.Errorf("age = %v, want ~3h (from lastPhaseTransitionTime)", age)
	}
}

func TestReleasedAge_FallbackToCreation(t *testing.T) {
	now := time.Now()
	pv := corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			CreationTimestamp: metav1.Time{Time: now.Add(-48 * time.Hour)},
		},
		Status: corev1.PersistentVolumeStatus{},
	}
	age := releasedAge(pv, now)
	if age < 47*time.Hour || age > 49*time.Hour {
		t.Errorf("age = %v, want ~48h (from creationTimestamp)", age)
	}
}

func TestVolumeHandle_AWSEbs(t *testing.T) {
	pv := corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				AWSElasticBlockStore: &corev1.AWSElasticBlockStoreVolumeSource{
					VolumeID: "vol-abc123",
				},
			},
		},
	}
	if got := volumeHandle(pv); got != "vol-abc123" {
		t.Errorf("volumeHandle = %q, want %q", got, "vol-abc123")
	}
}

func TestVolumeHandle_CSI(t *testing.T) {
	pv := corev1.PersistentVolume{
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					VolumeHandle: "vol-def456",
				},
			},
		},
	}
	if got := volumeHandle(pv); got != "vol-def456" {
		t.Errorf("volumeHandle = %q, want %q", got, "vol-def456")
	}
}
