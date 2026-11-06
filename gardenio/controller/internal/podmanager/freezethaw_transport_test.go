package podmanager

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// freezethaw_transport_test.go — guards for the SnapshotTransport seam (Δ5-B1,
// gd-k3D6bYJkN9). The seam is a pure refactor with a node-local no-op default;
// these tests pin the default's identity semantics, its off-node fail-closed
// refusal, and the manager wiring (default installed, override + nil-restore,
// zero-value nil safety) so an off-node transport (B2) can drop in without a
// behaviour change slipping through.

func TestNodeLocalSnapshotTransport_PushIsNoOp(t *testing.T) {
	tr := NewNodeLocalSnapshotTransport()
	snap := Snapshot{AgentName: "agent-a", Node: "node-1", ImagePath: "/var/lib/gardenio/ckpt/agent-a"}
	if err := tr.Push(context.Background(), snap); err != nil {
		t.Fatalf("node-local Push must be a no-op, got err: %v", err)
	}
}

func TestNodeLocalSnapshotTransport_PullSameNodeIsIdentity(t *testing.T) {
	tr := NewNodeLocalSnapshotTransport()
	snap := Snapshot{AgentName: "agent-a", Node: "node-1"}
	// Explicit same-node target — the only case thaw produces today.
	if err := tr.Pull(context.Background(), snap, "node-1"); err != nil {
		t.Fatalf("Pull onto the origin node must be identity, got err: %v", err)
	}
	// Unset target (defensive) is also treated as identity.
	if err := tr.Pull(context.Background(), snap, ""); err != nil {
		t.Fatalf("Pull with an unset target must be identity, got err: %v", err)
	}
	// Unset origin node is also treated as identity (no relocation implied).
	if err := tr.Pull(context.Background(), Snapshot{AgentName: "agent-a"}, "node-9"); err != nil {
		t.Fatalf("Pull with an unset origin node must be identity, got err: %v", err)
	}
}

func TestNodeLocalSnapshotTransport_PullOffNodeFailsClosed(t *testing.T) {
	tr := NewNodeLocalSnapshotTransport()
	snap := Snapshot{AgentName: "agent-a", Node: "node-1"}
	err := tr.Pull(context.Background(), snap, "node-2")
	if err == nil {
		t.Fatal("node-local Pull to a DIFFERENT node must fail closed (cannot relocate off-node); got nil")
	}
	// The error must name both nodes so an operator can see the mismatch.
	if !strings.Contains(err.Error(), "node-1") || !strings.Contains(err.Error(), "node-2") {
		t.Fatalf("off-node Pull error must name origin+target nodes, got: %v", err)
	}
}

func TestNewFreezeThawManager_InstallsNodeLocalTransportByDefault(t *testing.T) {
	mgr := NewFreezeThawManager(fake.NewSimpleClientset(), nil, NewInMemorySnapshotStore(), nil, nil)
	if mgr.transport == nil {
		t.Fatal("constructor must install a non-nil default SnapshotTransport")
	}
	if _, ok := mgr.transport.(*NodeLocalSnapshotTransport); !ok {
		t.Fatalf("default transport must be *NodeLocalSnapshotTransport, got %T", mgr.transport)
	}
}

func TestSetSnapshotTransport_OverrideAndNilRestore(t *testing.T) {
	mgr := NewFreezeThawManager(fake.NewSimpleClientset(), nil, NewInMemorySnapshotStore(), nil, nil)
	stub := &recordingTransport{}
	mgr.SetSnapshotTransport(stub)
	if mgr.transportOrDefault() != stub {
		t.Fatal("SetSnapshotTransport must install the provided transport")
	}
	// nil restores the node-local no-op rather than leaving the manager transport-less.
	mgr.SetSnapshotTransport(nil)
	if _, ok := mgr.transportOrDefault().(*NodeLocalSnapshotTransport); !ok {
		t.Fatalf("SetSnapshotTransport(nil) must restore the node-local default, got %T", mgr.transportOrDefault())
	}
}

func TestTransportOrDefault_ZeroValueIsSafe(t *testing.T) {
	// A manager whose transport field was never set (zero value / struct built
	// without the constructor) must still resolve to the node-local no-op.
	var mgr FreezeThawManager
	got := mgr.transportOrDefault()
	if got == nil {
		t.Fatal("transportOrDefault must never return nil")
	}
	if _, ok := got.(*NodeLocalSnapshotTransport); !ok {
		t.Fatalf("zero-value manager must fall back to *NodeLocalSnapshotTransport, got %T", got)
	}
}

// recordingTransport is a test double that records the verbs it is asked to run
// and can be primed to fail either one.
type recordingTransport struct {
	pushed   []Snapshot
	pulled   []Snapshot
	pushErr  error
	pullErr  error
	pullNode string
}

func (r *recordingTransport) Push(_ context.Context, snap Snapshot) error {
	r.pushed = append(r.pushed, snap)
	return r.pushErr
}

func (r *recordingTransport) Pull(_ context.Context, snap Snapshot, targetNode string) error {
	r.pulled = append(r.pulled, snap)
	r.pullNode = targetNode
	return r.pullErr
}

var _ SnapshotTransport = (*recordingTransport)(nil)
var _ SnapshotTransport = (*NodeLocalSnapshotTransport)(nil)

func TestRecordingTransport_ErrorPlumbing(t *testing.T) {
	sentinel := errors.New("boom")
	r := &recordingTransport{pushErr: sentinel}
	if err := r.Push(context.Background(), Snapshot{AgentName: "x"}); !errors.Is(err, sentinel) {
		t.Fatalf("Push must surface primed error, got %v", err)
	}
	if len(r.pushed) != 1 {
		t.Fatalf("Push must record the call, got %d", len(r.pushed))
	}
}
