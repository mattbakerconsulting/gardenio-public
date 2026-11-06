package podmanager

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// cancelOnTriggerRehydrator simulates a controller leader-change / pod roll that
// fires WHILE the rehydration gate is running: when Trigger is invoked it cancels
// the inbound (leader-election) ctx, then records whether the ctx IT received is
// still alive. With the harden-thaw detachment (Thaw runs the post-commit section
// on context.WithoutCancel(ctx)), the received ctx must survive the parent cancel
// — proving a roll mid-rehydrate no longer aborts the gate at ~6.8s with
// "context canceled" (bug ka-0000004088 confound).
type cancelOnTriggerRehydrator struct {
	parentCancel context.CancelFunc
	sawCancel    bool
	called       bool
}

func (r *cancelOnTriggerRehydrator) Trigger(ctx context.Context, _ RehydrationRequest) error {
	r.called = true
	// Fire the "controller roll": cancel the inbound ctx the thaw was invoked
	// with. A correctly-detached thaw passes us a ctx decoupled from that
	// cancellation.
	r.parentCancel()
	// Give the cancellation a beat to propagate had we NOT detached.
	deadline := time.After(50 * time.Millisecond)
	for {
		select {
		case <-ctx.Done():
			r.sawCancel = true
			return ctx.Err()
		case <-deadline:
			// ctx stayed alive through the parent cancel — detachment works.
			return nil
		}
	}
}

func seedThawSnapshot(t *testing.T, store SnapshotStore, agent, ns, node string) {
	t.Helper()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: agent, BeadID: "gd-AGENT", Namespace: ns, PodName: agent,
		Node: node, ImagePath: "/var/tmp/gb-ckpt/" + agent, BundlePath: "/var/tmp/gb-bundle/" + agent,
		SandboxCID: "sandboxcid222", AgentCID: "agentcid111", PodUID: "a-uid-123", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA(agent, ns, node).Spec),
	})
}

// TestThaw_RehydrationDetachedFromInboundCtxCancel proves the post-commit
// critical section survives a controller roll: cancelling the inbound ctx the
// moment rehydration starts must NOT cancel the rehydration gate's ctx.
func TestThaw_RehydrationDetachedFromInboundCtxCancel(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	seedThawSnapshot(t, store, "agentsim", ns, node)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rehydr := &cancelOnTriggerRehydrator{parentCancel: cancel}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	res, err := m.Thaw(ctx, ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw should succeed despite inbound-ctx cancel mid-rehydrate: %v", err)
	}
	if !rehydr.called {
		t.Fatal("rehydration trigger was never called")
	}
	if rehydr.sawCancel {
		t.Error("rehydration ctx was cancelled by the inbound-ctx roll — detachment failed; the gate would abort early (ka-0000004088 confound)")
	}
	if !res.Rehydrated {
		t.Error("thaw should report Rehydrated=true (gate released, not aborted)")
	}
}

// TestThawHardDeadline_DefaultAndOverride checks the computed default cap and the
// explicit override.
func TestThawHardDeadline_DefaultAndOverride(t *testing.T) {
	m := &FreezeThawManager{}
	want := m.runscTimeout() + m.rehydrateDeadline() + thawTeardownMargin
	if got := m.thawHardDeadline(); got != want {
		t.Errorf("default thawHardDeadline = %v, want %v", got, want)
	}
	m.ThawHardDeadline = 7 * time.Minute
	if got := m.thawHardDeadline(); got != 7*time.Minute {
		t.Errorf("override thawHardDeadline = %v, want 7m", got)
	}
}

// TestInflightThawCount_TracksThawWindow verifies the in-flight counter is held
// for the whole Thaw call (the shutdown drain depends on it) and released after.
func TestInflightThawCount_TracksThawWindow(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	store := NewInMemorySnapshotStore()
	seedThawSnapshot(t, store, "agentsim", ns, node)

	// Block inside rehydration until we observe the in-flight count, then release.
	gate := make(chan struct{})
	observed := make(chan int, 1)
	rehydr := rehydratorFunc(func(ctx context.Context, _ RehydrationRequest) error {
		<-gate
		return nil
	})
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	if n := m.InflightThawCount(); n != 0 {
		t.Fatalf("pre-thaw inflight = %d, want 0", n)
	}

	done := make(chan error, 1)
	go func() {
		_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
		done <- err
	}()

	// Poll until the thaw is registered in flight (it should be for the whole call).
	deadline := time.After(2 * time.Second)
	for {
		if n := m.InflightThawCount(); n > 0 {
			observed <- n
			break
		}
		select {
		case <-deadline:
			t.Fatal("thaw never registered in flight")
		case <-time.After(time.Millisecond):
		}
	}
	if n := <-observed; n != 1 {
		t.Errorf("inflight during thaw = %d, want 1", n)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if n := m.InflightThawCount(); n != 0 {
		t.Errorf("post-thaw inflight = %d, want 0 (slot must release)", n)
	}
}

// TestDrainThaws_WaitsThenReturnsZero blocks a thaw in flight, confirms DrainThaws
// does not return until the thaw releases, and that it reports 0 once drained.
func TestDrainThaws_WaitsThenReturnsZero(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	store := NewInMemorySnapshotStore()
	seedThawSnapshot(t, store, "agentsim", ns, node)

	gate := make(chan struct{})
	rehydr := rehydratorFunc(func(ctx context.Context, _ RehydrationRequest) error {
		<-gate
		return nil
	})
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	go func() { _, _ = m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}) }()
	// Wait for the thaw to be in flight.
	for m.InflightThawCount() == 0 {
		time.Sleep(time.Millisecond)
	}

	// Drain with a generous budget; it must block until we release the gate.
	drained := make(chan int, 1)
	go func() {
		drained <- m.DrainThaws(context.Background(), time.Millisecond)
	}()
	select {
	case <-drained:
		t.Fatal("DrainThaws returned while a thaw was still in flight")
	case <-time.After(20 * time.Millisecond):
	}
	close(gate)
	select {
	case n := <-drained:
		if n != 0 {
			t.Errorf("DrainThaws returned %d, want 0 after release", n)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("DrainThaws did not return after the thaw released")
	}
}

// TestDrainThaws_BudgetExpiresWithThawInFlight verifies the drain returns the
// still-in-flight count when its budget expires (so shutdown does not hang).
func TestDrainThaws_BudgetExpiresWithThawInFlight(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	store := NewInMemorySnapshotStore()
	seedThawSnapshot(t, store, "agentsim", ns, node)

	gate := make(chan struct{})
	defer close(gate)
	rehydr := rehydratorFunc(func(ctx context.Context, _ RehydrationRequest) error {
		<-gate
		return nil
	})
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	go func() { _, _ = m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}) }()
	for m.InflightThawCount() == 0 {
		time.Sleep(time.Millisecond)
	}

	dctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if n := m.DrainThaws(dctx, time.Millisecond); n != 1 {
		t.Errorf("DrainThaws on expired budget = %d, want 1 (still in flight)", n)
	}
}

// TestDrainThaws_NoThaws returns immediately with zero.
func TestDrainThaws_NoThaws(t *testing.T) {
	m := newFreezeThawManager(t, fake.NewClientset(), &fakeNodeExecutor{}, NewInMemorySnapshotStore(), nil)
	if n := m.DrainThaws(context.Background(), time.Millisecond); n != 0 {
		t.Errorf("DrainThaws with no thaws = %d, want 0", n)
	}
}

// TestNilManager_DrainSafe — a nil manager (freeze/thaw disabled) drains instantly.
func TestNilManager_DrainSafe(t *testing.T) {
	var m *FreezeThawManager
	if n := m.InflightThawCount(); n != 0 {
		t.Errorf("nil InflightThawCount = %d, want 0", n)
	}
	if n := m.DrainThaws(context.Background(), time.Millisecond); n != 0 {
		t.Errorf("nil DrainThaws = %d, want 0", n)
	}
}

// rehydratorFunc adapts a func to the RehydrationTrigger interface for tests.
type rehydratorFunc func(ctx context.Context, req RehydrationRequest) error

func (f rehydratorFunc) Trigger(ctx context.Context, req RehydrationRequest) error {
	return f(ctx, req)
}
