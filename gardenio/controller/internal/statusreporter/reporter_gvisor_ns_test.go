package statusreporter

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// SyncAll must sweep extra namespaces (the PSA-privileged gVisor namespace where
// runsc freeze/thaw agents run, bug ka-0000004301) in addition to the primary
// namespace, so a runsc agent's bead still receives phase updates.

func TestSyncAll_SweepsExtraNamespace(t *testing.T) {
	mainPod := makePod("crew-proj-dev-alpha", "gardenio", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	gvisorPod := makePod("crew-gardenio-gvisor-poc-canary", "gardenio-gvisor", corev1.PodRunning,
		agentLabels("gardenio", "gvisor-poc", "canary"), "10.0.0.2")

	client := fake.NewSimpleClientset(mainPod, gvisorPod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "gardenio", testLogger())
	r.SetExtraNamespaces("gardenio-gvisor")

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 2 {
		t.Fatalf("expected 2 state updates (main + gVisor ns), got %d (%+v)",
			len(daemon.stateCalls), daemon.stateCalls)
	}
}

// Without SetExtraNamespaces the gVisor pod is invisible — guards that the sweep
// is opt-in and the single-namespace deployment is unchanged.
func TestSyncAll_ExtraNamespaceOptIn(t *testing.T) {
	mainPod := makePod("crew-proj-dev-alpha", "gardenio", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")
	gvisorPod := makePod("crew-gardenio-gvisor-poc-canary", "gardenio-gvisor", corev1.PodRunning,
		agentLabels("gardenio", "gvisor-poc", "canary"), "10.0.0.2")

	client := fake.NewSimpleClientset(mainPod, gvisorPod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "gardenio", testLogger())
	// No SetExtraNamespaces.

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected 1 state update (main ns only), got %d (%+v)",
			len(daemon.stateCalls), daemon.stateCalls)
	}
}

// SetExtraNamespaces ignores empty entries and the primary namespace itself, so
// a pod is never double-listed (which would double-count updates).
func TestSetExtraNamespaces_IgnoresEmptyAndPrimary(t *testing.T) {
	mainPod := makePod("crew-proj-dev-alpha", "gardenio", corev1.PodRunning,
		agentLabels("proj", "dev", "alpha"), "10.0.0.1")

	client := fake.NewSimpleClientset(mainPod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "gardenio", testLogger())
	r.SetExtraNamespaces("", "gardenio")

	if len(r.extraNamespaces) != 0 {
		t.Fatalf("empty and primary-namespace entries must be ignored, got %v", r.extraNamespaces)
	}

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(daemon.stateCalls) != 1 {
		t.Fatalf("expected exactly 1 state update (no double-list), got %d", len(daemon.stateCalls))
	}
}
