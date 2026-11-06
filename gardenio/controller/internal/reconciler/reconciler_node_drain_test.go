package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
)

// fakeLifecycleRecorder captures RecordLifecycleEvent calls (ka-0000000550).
type fakeLifecycleRecorder struct {
	events []recordedLifecycle
}

type recordedLifecycle struct {
	event, project, role, sessionID string
}

func (f *fakeLifecycleRecorder) RecordLifecycleEvent(event, project, role, sessionID string) bool {
	f.events = append(f.events, recordedLifecycle{event, project, role, sessionID})
	return true
}

// RecordAccountLayer satisfies the LifecycleRecorder interface (Phase 0
// instrumentation, ka-0000002142). The drain test does not exercise the
// create-missing path, so this is a no-op stub.
func (f *fakeLifecycleRecorder) RecordAccountLayer(layer, project, role string) bool {
	return true
}

// podOnNode returns a running agent pod scheduled on nodeName. The tests that
// use it wire no drain messenger, so the drain branch mails nobody — they
// assert the branch is TAKEN and that it does not delete/recreate the pod, not
// the drain mail (covered by the upgrade_test.go drain-tracker tests).
func podOnNode(name, namespace, mode, project, role, agent, nodeName string) corev1.Pod {
	p := makePod(name, namespace, mode, project, role, agent, corev1.PodRunning)
	p.Spec.NodeName = nodeName
	return p
}

// TestReconcile_NodeDrain_NudgesButDoesNotRecreate verifies WS1
// (ka-0000007969): when an agent's node is being force-reclaimed (Karpenter
// disruption taint), the reconciler routes the agent through the graceful drain
// path and does NOT delete or recreate the pod — the node teardown owns that.
func TestReconcile_NodeDrain_NudgesButDoesNotRecreate(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			podOnNode("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", "node-a"),
		},
		nodesByName: map[string]*corev1.Node{
			"node-a": {
				ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{
					{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
				}},
			},
		},
	}

	cfg := testConfig("ns")
	cfg.NodeDrainNudgeEnabled = true
	// Image matches, so there is no drift; the ONLY reason to act is node doom.
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 0 {
		t.Errorf("node-drain must not delete the pod (node teardown owns it), got %v", mgr.deleted)
	}
	if len(mgr.created) != 0 {
		t.Errorf("node-drain must not recreate the pod, got %d creations", len(mgr.created))
	}
}

// TestReconcile_NodeDrain_HealthyNodeNoOp verifies a running agent on a healthy
// node is left completely alone even with the feature enabled.
func TestReconcile_NodeDrain_HealthyNodeNoOp(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			podOnNode("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", "node-a"),
		},
		nodesByName: map[string]*corev1.Node{
			"node-a": {ObjectMeta: metav1.ObjectMeta{Name: "node-a"}},
		},
	}

	cfg := testConfig("ns")
	cfg.NodeDrainNudgeEnabled = true
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mgr.deleted) != 0 || len(mgr.created) != 0 {
		t.Errorf("healthy node: expected no-op, got %d deletions / %d creations",
			len(mgr.deleted), len(mgr.created))
	}
}

// TestReconcile_NodeDrain_DisabledSkipsNodeLookup verifies that with the flag
// off the reconciler never calls GetNode — the feature is a true no-op and adds
// no per-pod API cost when disabled (which is the default).
func TestReconcile_NodeDrain_DisabledSkipsNodeLookup(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	// A doomed node is present, but the flag is OFF, so it must be ignored.
	mgr := &mockManager{
		pods: []corev1.Pod{
			podOnNode("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", "node-a"),
		},
		// Force GetNode to blow up if it is ever called with the flag off.
		getNodeErr: context.Canceled,
	}

	cfg := testConfig("ns") // NodeDrainNudgeEnabled defaults to false
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mgr.deleted) != 0 || len(mgr.created) != 0 {
		t.Errorf("disabled node-drain: expected no-op, got %d deletions / %d creations",
			len(mgr.deleted), len(mgr.created))
	}
}

// TestReconcile_NodeDrain_EmitsDrainedLifecycleEvent verifies that opening a
// node-drain episode emits exactly one gardenio_agent_lifecycle_events_total
// {event="drained"} keyed by the agent's (project,role), and that a second
// reconcile pass while the agent is still draining does NOT re-emit it — the
// drain tracker keeps the entry across passes so IsDraining dedups the
// per-episode event (ka-0000000550).
func TestReconcile_NodeDrain_EmitsDrainedLifecycleEvent(t *testing.T) {
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			{ID: "bd-1", Project: "proj", Mode: "crew", Role: "dev", AgentName: "alpha"},
		},
	}
	mgr := &mockManager{
		pods: []corev1.Pod{
			podOnNode("crew-proj-dev-alpha", "ns", "crew", "proj", "dev", "alpha", "node-a"),
		},
		nodesByName: map[string]*corev1.Node{
			"node-a": {
				ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
				Spec: corev1.NodeSpec{Taints: []corev1.Taint{
					{Key: "karpenter.sh/disrupted", Effect: corev1.TaintEffectNoSchedule},
				}},
			},
		},
	}

	cfg := testConfig("ns")
	cfg.NodeDrainNudgeEnabled = true
	r := New(lister, mgr, cfg, testLogger(), simpleSpecBuilder("ghcr.io/org/agent:v1"))
	// The drain mail goes to a fake messenger; the agent is still working.
	m := &fakeMessenger{bead: agentBeadInState("working")}
	r.upgradeTracker.SetMessenger(m)
	rec := &fakeLifecycleRecorder{}
	r.SetLifecycleRecorder(rec)

	// Pass 1: node is doomed → drain opens → one "drained" event.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 1: unexpected error: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("pass 1: expected exactly 1 lifecycle event, got %+v", rec.events)
	}
	ev := rec.events[0]
	if ev.event != "drained" || ev.project != "proj" || ev.role != "dev" || ev.sessionID != "" {
		t.Fatalf("pass 1: unexpected drained event: %+v", ev)
	}

	// Pass 2: same pod still draining → the drain entry persists until the pod
	// is gone or the drain ends, so IsDraining is true and no second event is
	// emitted.
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("pass 2: unexpected error: %v", err)
	}
	if len(rec.events) != 1 {
		t.Fatalf("pass 2: drained must not re-emit while still draining, got %+v", rec.events)
	}
	// One drain mail across both passes, to the bead's agent, with the
	// node-reclaim wording.
	if len(m.sent) != 1 || m.to[0] != "alpha" || m.sent[0] != nodeDrainNudgeMsg {
		t.Fatalf("expected one node-drain mail to alpha, got to=%v sent=%v", m.to, m.sent)
	}
}
