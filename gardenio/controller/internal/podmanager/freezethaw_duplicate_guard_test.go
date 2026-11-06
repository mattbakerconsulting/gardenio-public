package podmanager

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// goodSnapshot is a representative full snapshot, as if a first (live-pod)
// freeze had persisted it. Its CIDs distinguish it from any value a degenerate
// second checkpoint would write.
func goodSnapshot(agent, ns, node string) Snapshot {
	return Snapshot{
		AgentName:      agent,
		Namespace:      ns,
		PodName:        agent,
		Node:           node,
		SandboxCID:     "good-sandbox",
		AgentCID:       "good-agent",
		ContainerCount: 2,
	}
}

// terminatingPodA is a Pod A that is already Terminating (DeletionTimestamp
// set) — the state Pod A is in ~2s after the first --leave-running freeze,
// when the duplicate AgentStop arrives.
func terminatingPodA(name, ns, node string) *corev1.Pod {
	p := livePodA(name, ns, node)
	now := metav1.Now()
	p.DeletionTimestamp = &now
	return p
}

// TestFreeze_RefusesDuplicateWhenPodTerminating is the regression guard for bug
// ka-0000000194: a duplicate Freeze arrives while a good snapshot already
// exists and Pod A is Terminating. Freeze must REFUSE to re-checkpoint (no node
// exec) and return the existing good snapshot + ErrFreezeAlreadySnapshotted,
// rather than overwriting it with a degenerate one.
func TestFreeze_RefusesDuplicateWhenPodTerminating(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(terminatingPodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	// Pre-seed the GOOD snapshot the first (live) freeze persisted.
	if err := store.Put(context.Background(), goodSnapshot("agentsim", ns, node)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if !errors.Is(err, ErrFreezeAlreadySnapshotted) {
		t.Fatalf("want ErrFreezeAlreadySnapshotted, got err=%v", err)
	}
	if snap == nil || snap.SandboxCID != "good-sandbox" {
		t.Fatalf("want existing good snapshot returned, got %+v", snap)
	}
	if len(exec.scripts) != 0 {
		t.Fatalf("duplicate freeze must NOT run a node checkpoint exec; ran %d", len(exec.scripts))
	}
	// The persisted snapshot must be UNCHANGED (not overwritten/degenerate).
	got, err := store.Get(context.Background(), "agentsim")
	if err != nil {
		t.Fatalf("snapshot vanished: %v", err)
	}
	if got.SandboxCID != "good-sandbox" || got.ContainerCount != 2 {
		t.Fatalf("good snapshot was clobbered: %+v", got)
	}
}

// TestFreeze_RefusesDuplicateWhenPodGone covers the variant where Pod A has
// already been fully deleted (Get errors) by the time the duplicate fires.
func TestFreeze_RefusesDuplicateWhenPodGone(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset() // no pod
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	if err := store.Put(context.Background(), goodSnapshot("agentsim", ns, node)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if !errors.Is(err, ErrFreezeAlreadySnapshotted) {
		t.Fatalf("want ErrFreezeAlreadySnapshotted, got err=%v", err)
	}
	if snap == nil || snap.SandboxCID != "good-sandbox" {
		t.Fatalf("want existing good snapshot returned, got %+v", snap)
	}
	if len(exec.scripts) != 0 {
		t.Fatalf("duplicate freeze must NOT run a node checkpoint exec; ran %d", len(exec.scripts))
	}
}

// TestFreeze_ProceedsWhenLivePodHasSnapshot verifies the guard does NOT block a
// legitimate re-freeze: if a snapshot exists but Pod A is still LIVE (no
// DeletionTimestamp), Freeze proceeds and re-checkpoints normally. The guard
// keys on pod-gone/Terminating, not merely on snapshot-exists, so an idle agent
// that was previously frozen, thawed, and is being frozen again is unaffected.
func TestFreeze_ProceedsWhenLivePodHasSnapshot(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	if err := store.Put(context.Background(), goodSnapshot("agentsim", ns, node)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze on live pod should proceed, got err=%v", err)
	}
	// Re-checkpoint ran and persisted the fresh snapshot (CIDs from freezeOKStdout).
	if snap == nil || snap.SandboxCID != "sandboxcid222" {
		t.Fatalf("want fresh snapshot from re-checkpoint, got %+v", snap)
	}
	if len(exec.scripts) != 1 {
		t.Fatalf("live-pod freeze should run exactly one node exec; ran %d", len(exec.scripts))
	}
}

// TestFreeze_FirstFreezeOnTerminatingPodWithNoSnapshotStillErrors verifies the
// guard does not swallow the get-pod error when there is NO prior snapshot to
// protect: a Terminating/absent pod with no snapshot falls through to the
// normal get-pod error path (today's behaviour — cold start next spawn).
func TestFreeze_FirstFreezeOnGonePodWithNoSnapshotStillErrors(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset() // no pod, no snapshot
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err == nil || errors.Is(err, ErrFreezeAlreadySnapshotted) {
		t.Fatalf("want normal get-pod error (no snapshot to protect), got %v", err)
	}
}
