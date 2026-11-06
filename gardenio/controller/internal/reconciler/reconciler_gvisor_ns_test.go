package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/beadsapi"
)

// The reconciler must sweep actual pods in the gVisor namespace too, so runsc
// freeze/thaw agents participate in desired-vs-actual orphan cleanup and
// failed-pod recreation (ka-0000008076). Without SetExtraNamespaces a runsc agent's pod in
// a separate namespace is invisible: a live desired pod would be re-created
// (duplicate), and an orphan there would never be reaped.

// A runsc agent whose bead is desired and whose pod already exists in the gVisor
// namespace must be recognized as present — not re-created as if missing.
func TestReconcile_ExtraNamespacePodCountedAsPresent(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-gv", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "gvisor"},
		},
	}
	mgr := &mockManager{
		podsByNS: map[string][]corev1.Pod{
			"gardenio": nil,
			"gardenio-gvisor": {
				makePod("crew-gardenio-crew-gvisor", "gardenio-gvisor", "crew", "gardenio", "crew", "gvisor", corev1.PodRunning),
			},
		},
	}

	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetExtraNamespaces("gardenio-gvisor")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 0 {
		t.Errorf("expected 0 pods created (gVisor pod already present), got %d", len(mgr.created))
	}
	if len(mgr.deleted) != 0 {
		t.Errorf("expected 0 pods deleted, got %v", mgr.deleted)
	}
}

// An orphan pod (no matching bead) in the gVisor namespace must be deleted from
// that namespace, using the pod's own namespace.
func TestReconcile_DeletesOrphanInExtraNamespace(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		// One desired bead so desired is non-empty (avoids the 3-cycle guard).
		{ID: "bd-keep", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "keep"},
	}}
	mgr := &mockManager{
		podsByNS: map[string][]corev1.Pod{
			"gardenio": {
				makePod("crew-gardenio-crew-keep", "gardenio", "crew", "gardenio", "crew", "keep", corev1.PodRunning),
			},
			"gardenio-gvisor": {
				makePod("crew-gardenio-crew-orphan", "gardenio-gvisor", "crew", "gardenio", "crew", "orphan", corev1.PodRunning),
			},
		},
	}

	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetExtraNamespaces("gardenio-gvisor")
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 1 || mgr.deleted[0] != "crew-gardenio-crew-orphan" {
		t.Fatalf("expected orphan deleted, got %v", mgr.deleted)
	}
	if got := mgr.deletedNS["crew-gardenio-crew-orphan"]; got != "gardenio-gvisor" {
		t.Errorf("orphan deleted from namespace %q, want gardenio-gvisor", got)
	}
}

// Without SetExtraNamespaces the gVisor pod is invisible — guards opt-in and
// preserves single-namespace behaviour.
func TestReconcile_ExtraNamespaceOptIn(t *testing.T) {
	lister := &mockLister{beads: []beadsapi.AgentBead{
		{ID: "bd-gv", Project: "gardenio", Mode: "crew", Role: "crew", AgentName: "gvisor"},
	}}
	mgr := &mockManager{
		podsByNS: map[string][]corev1.Pod{
			"gardenio": nil,
			"gardenio-gvisor": {
				makePod("crew-gardenio-crew-gvisor", "gardenio-gvisor", "crew", "gardenio", "crew", "gvisor", corev1.PodRunning),
			},
		},
	}

	r := New(lister, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	// No SetExtraNamespaces — the gVisor namespace is never listed.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The desired bead's pod is invisible (only gardenio ns swept, which is
	// empty), so the reconciler creates it.
	if len(mgr.created) != 1 {
		t.Errorf("expected 1 pod created (gVisor ns not swept), got %d", len(mgr.created))
	}
}

// SetExtraNamespaces ignores empty entries and the primary namespace.
func TestReconcile_SetExtraNamespacesIgnoresEmptyAndPrimary(t *testing.T) {
	r := New(&mockLister{}, &mockManager{}, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetExtraNamespaces("", "gardenio")
	if len(r.extraNamespaces) != 0 {
		t.Fatalf("empty and primary-namespace entries must be ignored, got %v", r.extraNamespaces)
	}
}

// Autodestruct sweeps the gVisor namespace too.
func TestAutodestruct_SweepsExtraNamespace(t *testing.T) {
	mgr := &mockManager{deleteAllResult: 2}
	r := New(&mockLister{}, mgr, testConfig("gardenio"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetExtraNamespaces("gardenio-gvisor")

	total, err := r.Autodestruct(context.Background(), "tester")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 4 {
		t.Errorf("total deleted = %d, want 4 (2 per namespace)", total)
	}
	if len(mgr.deleteAllNS) != 2 ||
		mgr.deleteAllNS[0] != "gardenio" || mgr.deleteAllNS[1] != "gardenio-gvisor" {
		t.Errorf("DeleteAllAgentPods namespaces = %v, want [gardenio gardenio-gvisor]", mgr.deleteAllNS)
	}
}
