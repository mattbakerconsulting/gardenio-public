package reconciler

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/microvm"
	"gardenio/controller/internal/podmanager"
)

// microvmSpecBuilder is like simpleSpecBuilder but surfaces execution_target
// from the bead metadata onto the spec, so the reconciler's non-k8s guard sees
// it (mirroring the real BuildSpecFromBeadInfo → resolveExecutionTarget path).
func microvmSpecBuilder(image string) SpecBuilder {
	return func(cfg *config.Config, project, mode, role, agentName string, metadata map[string]string) podmanager.AgentPodSpec {
		return podmanager.AgentPodSpec{
			Project:         project,
			Mode:            mode,
			Role:            role,
			AgentName:       agentName,
			Image:           image,
			Namespace:       cfg.Namespace,
			ExecutionTarget: metadata["execution_target"],
		}
	}
}

// fakeMicrovmReconciler implements MicrovmReconciler for the C5 exemption tests.
// It recognises any bead whose ExecutionTarget is lambda_microvm and records the
// beads it was asked to reconcile.
type fakeMicrovmReconciler struct {
	reconciled []string
	closeIDs   map[string]bool // bead IDs Reconcile should report Closed
	err        error
}

func (f *fakeMicrovmReconciler) IsMicrovmAgent(view microvm.AgentBeadView) bool {
	return view.ExecutionTarget == microvm.ExecutionTargetLambdaMicroVM
}

func (f *fakeMicrovmReconciler) Reconcile(_ context.Context, view microvm.AgentBeadView) (microvm.ReconcileOutcome, error) {
	f.reconciled = append(f.reconciled, view.ID)
	if f.err != nil {
		return microvm.ReconcileOutcome{}, f.err
	}
	if f.closeIDs[view.ID] {
		return microvm.ReconcileOutcome{Closed: true}, nil
	}
	return microvm.ReconcileOutcome{Live: true}, nil
}

// microvmBead builds a lambda_microvm agent bead (execution_target ridin in
// Metadata, as ListAgentBeads populates it).
func microvmBead(id, agentState, runtimeID string) beadsapi.AgentBead {
	return beadsapi.AgentBead{
		ID: id, Project: "proj", Mode: "crew", Role: "dev", AgentName: id,
		AgentState: agentState,
		Metadata: map[string]string{
			"execution_target": microvm.ExecutionTargetLambdaMicroVM,
			"runtime_id":       runtimeID,
		},
	}
}

// TestReconcile_MicrovmAgent_NotCreatedAsPod verifies a lambda_microvm agent is
// never cold-started as a K8s pod by the reconciler's create-missing path.
func TestReconcile_MicrovmAgent_NotCreatedAsPod(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			microvmBead("bd-mvm", "working", "mvm-1"),
			{ID: "bd-pod", Project: "proj", Mode: "crew", Role: "dev", AgentName: "pod"},
		},
	}
	mgr := &mockManager{pods: nil}
	fake := &fakeMicrovmReconciler{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetMicrovmReconciler(fake)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the ordinary K8s bead should have a pod created.
	if len(mgr.created) != 1 || mgr.created[0].AgentName != "pod" {
		t.Fatalf("expected only the K8s agent pod created, got %v", mgr.created)
	}
	// The MicroVM agent must have been reconciled by the coordinator.
	if len(fake.reconciled) == 0 {
		t.Fatal("expected the MicroVM agent to be reconciled by the coordinator")
	}
	found := false
	for _, id := range fake.reconciled {
		if id == "bd-mvm" {
			found = true
		}
	}
	if !found {
		t.Errorf("bd-mvm should have been reconciled, got %v", fake.reconciled)
	}
}

// TestReconcile_MicrovmAgent_NotClosedAsPodlessLeak verifies a lambda_microvm
// agent in a terminal-looking state with no pod is NOT closed by the
// podless-leak reaper (that reaper is pod-based); it is routed to the C5
// coordinator instead.
func TestReconcile_MicrovmAgent_NotClosedAsPodlessLeak(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			microvmBead("bd-mvm", "done", "mvm-1"), // terminal state, no pod
		},
	}
	mgr := &mockManager{pods: nil}
	closer := &mockBeadCloser{}
	fake := &fakeMicrovmReconciler{} // reports Live → not closed

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)
	r.SetMicrovmReconciler(fake)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The podless-leak closer must NOT have closed the MicroVM bead.
	for _, c := range closer.closed {
		if c.beadID == "bd-mvm" {
			t.Errorf("MicroVM agent bd-mvm must not be closed by the podless-leak reaper, got %v", closer.closed)
		}
	}
	// It must have been routed to the coordinator's Reconcile instead.
	if len(fake.reconciled) != 1 || fake.reconciled[0] != "bd-mvm" {
		t.Errorf("bd-mvm should be reconciled by the coordinator, got %v", fake.reconciled)
	}
}

// TestReconcile_MicrovmAgent_CoordinatorClosesTerminal verifies that when the
// coordinator closes a terminal-runtime bead, the reconciler does not also
// cold-start a pod for it.
func TestReconcile_MicrovmAgent_CoordinatorClosesTerminal(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			microvmBead("bd-mvm", "working", "mvm-gone"),
		},
	}
	mgr := &mockManager{pods: nil}
	closer := &mockBeadCloser{}
	fake := &fakeMicrovmReconciler{closeIDs: map[string]bool{"bd-mvm": true}}

	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)
	r.SetMicrovmReconciler(fake)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// No pod created for the MicroVM agent.
	if len(mgr.created) != 0 {
		t.Errorf("no pod should be created for a MicroVM agent, got %v", mgr.created)
	}
}

// TestReconcile_NilMicrovmReconciler_UnchangedBehaviour verifies that without a
// MicroVM coordinator wired, the reconciler behaves exactly as before: a bead
// carrying execution_target=lambda_microvm falls through to the non-k8s guard
// and is hard-failed (never pod-created).
func TestReconcile_NilMicrovmReconciler_FailsClosed(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			microvmBead("bd-mvm", "", "mvm-1"),
		},
	}
	mgr := &mockManager{pods: nil}
	marker := &mockFailedMarker{}

	r := New(lister, mgr, testConfig("ns"), testLogger(), microvmSpecBuilder("img:v1"))
	r.SetFailedAgentMarker(marker)
	// NOTE: no SetMicrovmReconciler — backend unavailable.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.created) != 0 {
		t.Errorf("a MicroVM bead must never be pod-created when the backend is unavailable, got %v", mgr.created)
	}
	if len(marker.calls) != 1 || marker.calls[0].beadID != "bd-mvm" {
		t.Fatalf("bd-mvm should be hard-failed (fail closed), got %v", marker.calls)
	}
}
