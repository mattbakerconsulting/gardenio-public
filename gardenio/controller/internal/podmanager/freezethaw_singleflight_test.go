package podmanager

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

// gatedThawExecutor blocks the FIRST restore exec (the barrier-restore script,
// identified by the nsenter prefix) on a release channel so a concurrent Thaw
// call for the same agent overlaps the in-flight one. It counts how many
// restore scripts were executed so the single-flight invariant ("exactly one
// runsc restore per agent per wake", design ka-0000005125) is assertable.
type gatedThawExecutor struct {
	mu            sync.Mutex
	restoreCount  int
	started       chan struct{} // closed when the first restore exec begins
	release       chan struct{} // the test closes this to let the first restore finish
	startedClosed bool
}

func (g *gatedThawExecutor) Exec(_ context.Context, _ string, script string) (NodeExecResult, error) {
	isRestore := strings.Contains(script, "nsenter -t 1 -m -p -n")
	if isRestore {
		g.mu.Lock()
		g.restoreCount++
		first := g.restoreCount == 1
		if !g.startedClosed {
			g.startedClosed = true
			close(g.started)
		}
		g.mu.Unlock()
		if first {
			<-g.release // hold the first restore in flight until the test releases it
		}
	}
	return NodeExecResult{Stdout: thawOKStdout}, nil
}

func (g *gatedThawExecutor) restores() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.restoreCount
}

// TestThaw_SingleflightJoinsConcurrentThaw is the DEDUP acceptance for the
// re-nudge mid-THAW design (ka-0000005125 / impl ka-0000005830, e2e scenario 2
// ka-0000007641): two concurrent Thaw calls for the SAME agent must produce
// exactly ONE `runsc restore` — the second caller joins the first and returns
// its shared result — so a duplicate spawn event / reconciler pass during an
// in-flight thaw cannot mint a second Pod B (ka-0000005680 admit-fail).
func TestThaw_SingleflightJoinsConcurrentThaw(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &gatedThawExecutor{started: make(chan struct{}), release: make(chan struct{})}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", BeadID: "gd-AGENT", Namespace: ns, PodName: "agentsim",
		Node: node, ImagePath: "/var/tmp/gb-ckpt/agentsim", BundlePath: "/var/tmp/gb-bundle/agentsim",
		SandboxCID: "sandboxcid222", AgentCID: "agentcid111", PodUID: "a-uid-123", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	type thawOut struct {
		res *ThawResult
		err error
	}
	out1 := make(chan thawOut, 1)
	out2 := make(chan thawOut, 1)

	// Caller 1 owns the thaw; it will block inside the gated restore exec.
	go func() {
		res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
		out1 <- thawOut{res, err}
	}()

	// Wait until the first restore is provably in flight, THEN fire a second
	// concurrent Thaw for the same agent — it must JOIN, not start a 2nd restore.
	select {
	case <-exec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("first thaw never reached the restore exec")
	}
	go func() {
		res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
		out2 <- thawOut{res, err}
	}()

	// Give the joiner a moment to attach, then release the first restore.
	time.Sleep(50 * time.Millisecond)
	close(exec.release)

	var r1, r2 thawOut
	select {
	case r1 = <-out1:
	case <-time.After(5 * time.Second):
		t.Fatal("first thaw did not return")
	}
	select {
	case r2 = <-out2:
	case <-time.After(5 * time.Second):
		t.Fatal("joined thaw did not return")
	}

	if r1.err != nil {
		t.Fatalf("owner thaw failed: %v", r1.err)
	}
	if r2.err != nil {
		t.Fatalf("joined thaw failed: %v", r2.err)
	}
	// The invariant: exactly one runsc restore drove this wake.
	if got := exec.restores(); got != 1 {
		t.Errorf("restore execs = %d, want 1 (single-flight must collapse concurrent thaws)", got)
	}
	// The joiner shares the owner's result (same Pod B), not a second one.
	if r1.res == nil || r2.res == nil {
		t.Fatalf("nil results: owner=%v joiner=%v", r1.res, r2.res)
	}
	if r1.res.PlaceholderPod != r2.res.PlaceholderPod {
		t.Errorf("joined thaw got a different Pod B: owner=%q joiner=%q", r1.res.PlaceholderPod, r2.res.PlaceholderPod)
	}
}

// TestThaw_SingleflightClearsAfterCompletion verifies a SEQUENTIAL second thaw
// (a later wake, after the first fully resolved) starts a FRESH restore rather
// than re-reading the prior result — the single-flight entry must be released
// on return, not cached.
func TestThaw_SingleflightClearsAfterCompletion(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", BeadID: "gd-AGENT", Namespace: ns, PodName: "agentsim",
		Node: node, ImagePath: "/var/tmp/gb-ckpt/agentsim", BundlePath: "/var/tmp/gb-bundle/agentsim",
		SandboxCID: "sandboxcid222", AgentCID: "agentcid111", PodUID: "a-uid-123", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("first thaw: %v", err)
	}
	// Entry must be cleared so a fresh thaw runs (not a cached join).
	m.thawSingleMu.Lock()
	_, stillPresent := m.thawSingleflight["agentsim"]
	m.thawSingleMu.Unlock()
	if stillPresent {
		t.Error("single-flight entry not cleared after the thaw returned")
	}
	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("second (sequential) thaw: %v", err)
	}
	// Two sequential wakes ⇒ two restore execs (one per wake).
	restores := 0
	for _, s := range exec.scripts {
		if strings.Contains(s, "nsenter -t 1 -m -p -n") {
			restores++
		}
	}
	if restores != 2 {
		t.Errorf("restore execs = %d, want 2 (each sequential wake runs its own restore)", restores)
	}
}
