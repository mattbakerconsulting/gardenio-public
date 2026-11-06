package podmanager

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// blockingNodeExecutor blocks inside Exec until either its ctx is cancelled (the
// abort path) or release is signalled (the let-it-complete path). It records
// whether the ctx was cancelled, so a test can prove AbortFreeze actually
// interrupted the node-checkpoint exec. It models ProbeNodeExecutor, whose
// StreamWithContext returns promptly when the freeze ctx is cancelled.
type blockingNodeExecutor struct {
	started  chan struct{} // closed once Exec is entered (freeze is in flight)
	release  chan struct{} // close to let a non-aborted Exec return success
	okStdout string
	// ignoreCancel models the exec that finished its node op microseconds before
	// the abort's ctx-cancel propagated: it returns success on release even if
	// ctx is already cancelled, so the freeze races to a successful Put and the
	// finalize defer (not the exec) is what observes the abort. Used by the
	// raced-completion test to make that race deterministic.
	ignoreCancel bool

	mu          sync.Mutex
	ctxCanceled bool
	startedOnce sync.Once
	execCount   int
	// reapNodes/reapScripts record the (node, script) of every NON-first Exec —
	// i.e. the post-abort checkpoint-image reaper (discardAbortedFreeze). The
	// mid-checkpoint abort test asserts the reaper fired against
	// freezeImagePath(agent) on the resolved node (ka-0000004367).
	reapNodes   []string
	reapScripts []string
}

func newBlockingNodeExecutor(okStdout string) *blockingNodeExecutor {
	return &blockingNodeExecutor{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		okStdout: okStdout,
	}
}

func (b *blockingNodeExecutor) Exec(ctx context.Context, node string, script string) (NodeExecResult, error) {
	b.mu.Lock()
	b.execCount++
	first := b.execCount == 1
	if !first {
		b.reapNodes = append(b.reapNodes, node)
		b.reapScripts = append(b.reapScripts, script)
	}
	b.mu.Unlock()
	// Only the first call is the freeze checkpoint exec (which the test blocks on
	// + aborts). Subsequent calls (e.g. the post-abort image reaper in
	// discardAbortedFreeze) return immediately so they don't deadlock.
	if !first {
		return NodeExecResult{Stdout: "FT_IMAGE_REAPED=yes"}, nil
	}
	b.startedOnce.Do(func() { close(b.started) })
	if b.ignoreCancel {
		// Model an exec that completed before cancellation propagated: wait only
		// for release, then return success regardless of ctx state.
		<-b.release
		return NodeExecResult{Stdout: b.okStdout}, nil
	}
	select {
	case <-ctx.Done():
		b.mu.Lock()
		b.ctxCanceled = true
		b.mu.Unlock()
		return NodeExecResult{}, ctx.Err()
	case <-b.release:
		return NodeExecResult{Stdout: b.okStdout}, nil
	}
}

func (b *blockingNodeExecutor) wasCanceled() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.ctxCanceled
}

// reaps returns the (node, script) pairs of every post-checkpoint reaper exec.
func (b *blockingNodeExecutor) reaps() (nodes, scripts []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.reapNodes...), append([]string(nil), b.reapScripts...)
}

// reapTrackingStore wraps InMemorySnapshotStore to record Delete calls so the
// race test can assert a just-persisted snapshot was discarded.
type reapTrackingStore struct {
	*InMemorySnapshotStore
	mu      sync.Mutex
	deleted []string
}

func newReapTrackingStore() *reapTrackingStore {
	return &reapTrackingStore{InMemorySnapshotStore: NewInMemorySnapshotStore()}
}

func (s *reapTrackingStore) Delete(ctx context.Context, agentName string) error {
	s.mu.Lock()
	s.deleted = append(s.deleted, agentName)
	s.mu.Unlock()
	return s.InMemorySnapshotStore.Delete(ctx, agentName)
}

func (s *reapTrackingStore) deletedAgents() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.deleted...)
}

// TestFreeze_AbortMidCheckpoint proves the C2 happy path: an AbortFreeze while
// the node-checkpoint exec is in flight cancels that exec, Freeze returns
// ErrFreezeAborted, and NO snapshot is persisted — the agent is left live.
func TestFreeze_AbortMidCheckpoint(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := newBlockingNodeExecutor(freezeOKStdout)
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)
	m.InterruptEnabled = true

	type result struct {
		snap *Snapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, _, err := m.Freeze(context.Background(), FreezeRequest{
			AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
		})
		done <- result{snap, err}
	}()

	// Wait until the freeze is inside the node exec, then assert it is tracked
	// and abort it.
	<-exec.started
	if !m.IsFreezing("agentsim") {
		t.Fatalf("IsFreezing should be true while the checkpoint exec is in flight")
	}
	if !m.AbortFreeze("agentsim") {
		t.Fatalf("AbortFreeze should report it signalled an in-flight freeze")
	}

	select {
	case r := <-done:
		if !errors.Is(r.err, ErrFreezeAborted) {
			t.Fatalf("Freeze err = %v, want ErrFreezeAborted", r.err)
		}
		if r.snap != nil {
			t.Errorf("snap = %+v, want nil on abort", r.snap)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Freeze did not return after abort (exec ctx not cancelled?)")
	}

	if !exec.wasCanceled() {
		t.Errorf("node-checkpoint exec ctx was not cancelled by the abort")
	}
	if _, err := store.Get(context.Background(), "agentsim"); err == nil {
		t.Errorf("a snapshot was persisted despite the abort; want none")
	}
	if m.IsFreezing("agentsim") {
		t.Errorf("IsFreezing should be false after the freeze returned")
	}

	// A mid-checkpoint abort must REAP the partial image at freezeImagePath on the
	// resolved node — the runsc checkpoint writes its image directly there, so a
	// cancel mid-write leaves a partial dir that nothing else collects until the
	// next freeze of this agent (ka-0000004367, the "NO orphaned image" invariant).
	nodes, scripts := exec.reaps()
	if len(scripts) != 1 {
		t.Fatalf("expected exactly 1 post-abort reaper exec, got %d", len(scripts))
	}
	if nodes[0] != node {
		t.Errorf("reaper ran on node %q, want %q", nodes[0], node)
	}
	if want := freezeImagePath("agentsim"); !strings.Contains(scripts[0], want) {
		t.Errorf("reaper script does not target the partial image path %q:\n%s", want, scripts[0])
	}
}

// TestFreeze_AbortRacesCompletion proves the TOCTOU rule: an AbortFreeze that
// lands AFTER the snapshot is persisted still results in ErrFreezeAborted and
// the just-persisted snapshot is deleted (lazy reap) — never a live agent with a
// dangling thaw target. We force the race by setting the aborted flag while the
// exec is still blocked, then releasing it so the freeze runs all the way to a
// successful Put before its finalize defer reads the aborted verdict.
func TestFreeze_AbortRacesCompletion(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := newBlockingNodeExecutor(freezeOKStdout)
	exec.ignoreCancel = true // exec completes despite the abort's ctx-cancel (raced completion)
	store := newReapTrackingStore()
	m := newFreezeThawManager(t, client, exec, store, nil)
	m.InterruptEnabled = true

	type result struct {
		snap *Snapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, _, err := m.Freeze(context.Background(), FreezeRequest{
			AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
		})
		done <- result{snap, err}
	}()

	<-exec.started
	// Mark aborted WITHOUT cancelling-to-failure: flip the flag, then release the
	// exec so the freeze completes the checkpoint + Put. cctx is also cancelled by
	// AbortFreeze, but the exec returns via release before observing it (the node
	// op already finished microseconds before the abort) — modelling the freeze
	// that raced to completion. The finalize defer then reads aborted=true and
	// discards the snapshot.
	m.AbortFreeze("agentsim")
	close(exec.release)

	select {
	case r := <-done:
		if !errors.Is(r.err, ErrFreezeAborted) {
			t.Fatalf("Freeze err = %v, want ErrFreezeAborted on raced completion", r.err)
		}
		if r.snap != nil {
			t.Errorf("snap = %+v, want nil after raced-completion discard", r.snap)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Freeze did not return")
	}

	// The just-persisted snapshot must have been deleted (lazy reap) so no thaw
	// targets the agent we kept live.
	if _, err := store.Get(context.Background(), "agentsim"); err == nil {
		t.Errorf("snapshot still present after raced-completion abort; want deleted")
	}
	if got := store.deletedAgents(); len(got) != 1 || got[0] != "agentsim" {
		t.Errorf("Delete calls = %v, want [agentsim]", got)
	}
}

// TestFreeze_PostPutDelaySeam proves the TOCTOU e2e test seam (ka-0000000996):
// with PostPutDelay set, an AbortFreeze that fires AFTER the exec completed (the
// freeze raced to a successful Put) but DURING the widened post-Put window still
// results in ErrFreezeAborted and a discarded snapshot. This is the same
// invariant TestFreeze_AbortRacesCompletion proves, but driven through the
// real-substrate-shaped timing: the freeze persists, then sits in the post-Put
// window, then the abort lands — exactly what the e2e exercises against the live
// controller. We prove the abort's cctx-cancel ends the window early (the freeze
// returns well within the delay) AND the snapshot is reaped.
func TestFreeze_PostPutDelaySeam(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := newBlockingNodeExecutor(freezeOKStdout)
	store := newReapTrackingStore()
	m := newFreezeThawManager(t, client, exec, store, nil)
	m.InterruptEnabled = true
	// A long window: if the abort's cctx-cancel did NOT end the wait early the
	// freeze would block for the full 30s and the 5s deadline below would trip,
	// failing the test — so this also proves the window honours cancellation.
	m.PostPutDelay = 30 * time.Second

	type result struct {
		snap *Snapshot
		err  error
	}
	done := make(chan result, 1)
	go func() {
		snap, _, err := m.Freeze(context.Background(), FreezeRequest{
			AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
		})
		done <- result{snap, err}
	}()

	// Let the freeze complete the checkpoint + Put, then enter the post-Put
	// window. Release the exec immediately so the freeze runs to the Put; the
	// abort lands while the freeze is parked in the widened window.
	<-exec.started
	close(exec.release)
	// Poll the store so we abort strictly AFTER the snapshot was persisted —
	// modelling the e2e, which fires the interrupt only once it observes the
	// snapshot ConfigMap appear.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := store.Get(context.Background(), "agentsim"); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := store.Get(context.Background(), "agentsim"); err != nil {
		t.Fatalf("snapshot should be persisted before the abort (post-Put window not reached): %v", err)
	}
	if !m.AbortFreeze("agentsim") {
		t.Fatalf("AbortFreeze should report it signalled the in-flight (window-parked) freeze")
	}

	select {
	case r := <-done:
		if !errors.Is(r.err, ErrFreezeAborted) {
			t.Fatalf("Freeze err = %v, want ErrFreezeAborted (abort landed in the post-Put window)", r.err)
		}
		if r.snap != nil {
			t.Errorf("snap = %+v, want nil after post-Put-window abort", r.snap)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Freeze did not return within 5s — the post-Put window did not honour the abort's ctx-cancel")
	}

	if _, err := store.Get(context.Background(), "agentsim"); err == nil {
		t.Errorf("snapshot still present after post-Put-window abort; want deleted")
	}
	if got := store.deletedAgents(); len(got) != 1 || got[0] != "agentsim" {
		t.Errorf("Delete calls = %v, want [agentsim]", got)
	}
}

// TestFreeze_PostPutDelayInertWhenInterruptDisabled proves the seam is gated:
// with InterruptEnabled off, PostPutDelay is never consulted (the registry/defer
// are inert), so a freeze completes immediately and persists its snapshot — the
// seam can never slow or alter a production freeze that has the interrupt path
// off.
func TestFreeze_PostPutDelayInertWhenInterruptDisabled(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)
	// InterruptEnabled defaults to false; a huge delay must be ignored.
	m.PostPutDelay = time.Hour

	done := make(chan error, 1)
	go func() {
		_, _, err := m.Freeze(context.Background(), FreezeRequest{
			AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Freeze should succeed immediately with interrupt disabled: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Freeze blocked despite InterruptEnabled=false — the post-Put seam was consulted when it must be inert")
	}
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr != nil {
		t.Errorf("snapshot not persisted with interrupt disabled: %v", gerr)
	}
}

// TestFreeze_InterruptDisabled proves the rollback switch: with InterruptEnabled
// off, the in-flight registry is never populated, AbortFreeze is a no-op, and a
// freeze runs to completion exactly as in Phase 4.
func TestFreeze_InterruptDisabled(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)
	// InterruptEnabled defaults to false.

	if m.IsFreezing("agentsim") {
		t.Errorf("IsFreezing should be false when interrupt disabled")
	}
	if m.AbortFreeze("agentsim") {
		t.Errorf("AbortFreeze should be a no-op (false) when interrupt disabled")
	}

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze should succeed with interrupt disabled: %v", err)
	}
	if snap == nil {
		t.Fatalf("snap = nil, want a persisted snapshot")
	}
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr != nil {
		t.Errorf("snapshot not persisted with interrupt disabled: %v", gerr)
	}
}

// TestAbortFreeze_NoInflight proves AbortFreeze reports false when there is no
// freeze in flight (nothing to interrupt) even with the flag on.
func TestAbortFreeze_NoInflight(t *testing.T) {
	m := newFreezeThawManager(t, fake.NewClientset(), &fakeNodeExecutor{}, NewInMemorySnapshotStore(), nil)
	m.InterruptEnabled = true
	if m.AbortFreeze("nobody") {
		t.Errorf("AbortFreeze on an agent with no in-flight freeze should be false")
	}
	if m.IsFreezing("nobody") {
		t.Errorf("IsFreezing on an agent with no in-flight freeze should be false")
	}
}
