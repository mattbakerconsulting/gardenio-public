package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// --- Image change tests ---

func TestReconcile_ImageChange_LeavesRunningPodInPlace(t *testing.T) {
	// A running crew pod whose image tag changed must NOT be drained or
	// recreated — the new image is only picked up by pods created afterward.
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v2"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 0 {
		t.Errorf("expected 0 deletions on an image change, got %d", len(mgr.deleted))
	}
	if len(mgr.created) != 0 {
		t.Errorf("expected 0 creations on an image change, got %d", len(mgr.created))
	}
}

// --- Error handling tests ---

func TestReconcile_DaemonUnreachable_ReturnsError(t *testing.T) {
	lister := &mockLister{err: errors.New("connection refused")}
	mgr := &mockManager{pods: nil}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("expected error when daemon is unreachable")
	}
	if mgr.deleted != nil {
		t.Errorf("should not delete any pods when daemon is unreachable")
	}
	if mgr.created != nil {
		t.Errorf("should not create any pods when daemon is unreachable")
	}
}

func TestReconcile_PodListError_ReturnsError(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{listErr: errors.New("k8s API down")}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("expected error when pod listing fails")
	}
}

func TestReconcile_PodCreateError_ContinuesGracefully(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods:      nil,
		createErr: errors.New("quota exceeded"),
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("expected graceful continue on pod create error, got: %v", err)
	}
}

func TestReconcile_PodDeleteError_ReturnsError(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", corev1.PodRunning),
			makePod("crew-proj-dev-orphan", "ns", "crew", "proj", "dev", "orphan", corev1.PodRunning),
		},
		deleteErr: errors.New("forbidden"),
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("expected error when pod deletion fails")
	}
}

// --- Edge case tests ---

func TestReconcile_EmptyState_NoBeadsNoPods(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{}}
	mgr := &mockManager{pods: []corev1.Pod{}}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.created) != 0 || len(mgr.deleted) != 0 {
		t.Errorf("expected no-op for empty state, got %d created, %d deleted", len(mgr.created), len(mgr.deleted))
	}
}

func TestReconcile_SingleBeadSinglePod(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "solo"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			makePod("crew-proj-dev-solo", "ns", "crew", "proj", "dev", "solo", corev1.PodRunning),
		},
	}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.created) != 0 || len(mgr.deleted) != 0 {
		t.Errorf("expected no-op for converged single-pod state")
	}
}

func TestReconcile_IgnoresPodsWithoutAgentLabel(t *testing.T) {
	// Pods without the gardenio.io/agent label should be ignored (e.g., controller pod).
	lister := &mockLister{beads: nil}
	infraPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardenio-controller-xyz",
			Namespace: "ns",
			Labels: map[string]string{
				podmanager.LabelApp: podmanager.LabelAppValue,
				// No LabelAgent — this is an infrastructure pod.
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	mgr := &mockManager{pods: []corev1.Pod{infraPod}}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Infrastructure pod should NOT be deleted despite not being in desired set.
	if len(mgr.deleted) != 0 {
		t.Errorf("expected 0 deletions (infra pod should be ignored), got %d", len(mgr.deleted))
	}
}

func TestReconcile_BeadIDPassedToSpec(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-custom-id", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{pods: nil}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 1 {
		t.Fatalf("expected 1 pod created, got %d", len(mgr.created))
	}
	if mgr.created[0].BeadID != "bd-custom-id" {
		t.Errorf("expected BeadID bd-custom-id, got %s", mgr.created[0].BeadID)
	}
}

func TestReconcile_ConcurrencySafe(t *testing.T) {
	// Verify that concurrent Reconcile calls don't panic (mutex protection).
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{pods: nil}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))

	done := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			done <- r.Reconcile(context.Background())
		}()
	}

	for i := 0; i < 3; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("concurrent reconcile returned error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for concurrent reconciles")
		}
	}
}
