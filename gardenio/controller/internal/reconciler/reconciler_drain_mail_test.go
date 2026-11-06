package reconciler

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/beadsapi"
)

// The drain nudge is interrupt mail, and mail stays open until it is closed.
// A new session's mail subscriber drains every open mail for its agent name,
// so a drain mail left open after its pod is gone tells the recreated session
// to commit, push and gb stop. These tests drive the reconciler through each
// way a drain ends and check that the pod recreated for the same agent finds
// no open drain mail (fakeMessenger.openMailFor is what its subscriber would
// drain).

func doomedNodeDrainReconciler() (*Reconciler, *mockManager, *fakeMessenger) {
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
	m := &fakeMessenger{bead: agentBeadInState("working")}
	r.upgradeTracker.SetMessenger(m)
	return r, mgr, m
}

// TestReconcile_NodeDrain_PodGoneClosesDrainMail: the node-drain branch never
// reaches DrainReady. When the node teardown takes the pod, the next pass
// prunes the entry (CleanStaleDrains) and closes the mail before the pod is
// recreated for the same agent.
func TestReconcile_NodeDrain_PodGoneClosesDrainMail(t *testing.T) {
	r, mgr, m := doomedNodeDrainReconciler()
	ctx := context.Background()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(m.openMailFor("alpha")) != 1 {
		t.Fatalf("pass 1: expected one open node-drain mail, got %v", m.open)
	}

	mgr.pods = nil // the node teardown deleted the pod
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(mgr.created) != 1 {
		t.Fatalf("pass 2: expected the agent's pod to be recreated, got %d", len(mgr.created))
	}
	if open := m.openMailFor("alpha"); len(open) != 0 {
		t.Errorf("recreated agent would drain the reclaimed pod's drain mail %v", open)
	}
}

// TestReconcile_NodeDrain_FailedPodRecreatedWithoutDrainMail: a pod killed by
// the node reclaim is usually left Failed, still listed under its name, so
// CleanStaleDrains keeps its entry and the terminal-pod branch deletes and
// recreates it in the same pass. The drain must end before that create.
func TestReconcile_NodeDrain_FailedPodRecreatedWithoutDrainMail(t *testing.T) {
	r, mgr, m := doomedNodeDrainReconciler()
	ctx := context.Background()

	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if len(m.openMailFor("alpha")) != 1 {
		t.Fatalf("pass 1: expected one open node-drain mail, got %v", m.open)
	}

	mgr.pods[0].Status.Phase = corev1.PodFailed
	if err := r.Reconcile(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if len(mgr.deleted) != 1 || len(mgr.created) != 1 {
		t.Fatalf("pass 2: failed pod must be deleted and recreated: deleted=%v created=%d",
			mgr.deleted, len(mgr.created))
	}
	if r.upgradeTracker.IsDraining("crew-proj-dev-alpha") {
		t.Error("the recreated pod must not inherit the drain entry")
	}
	if open := m.openMailFor("alpha"); len(open) != 0 {
		t.Errorf("recreated agent would drain the failed pod's drain mail %v", open)
	}
}
