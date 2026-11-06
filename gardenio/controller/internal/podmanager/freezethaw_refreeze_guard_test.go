package podmanager

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Re-freeze guard tests (ka-0000001878).
//
// DECISION: option (b). A path-b checkpoint is node-local and single-shot —
// multi-cycle re-freeze of a thawed Pod B needs the snapshot-transport follow-up
// (criu-image-streamer), explicitly out of scope. The production-readiness
// requirement is that a SECOND freeze of an already-thawed agent is handled
// SAFELY and the agent degrades to a clean cold start, never clobbering the live
// thawed Pod B's bundle.
//
// The mechanism: a verified thaw reaps the 3.0G image (Step 7) but the snapshot
// RECORD survives (its bundle backs the live Pod B). Thaw now marks that record
// Consumed=true. Freeze refuses + discards a consumed snapshot
// (ErrFreezeAlreadyThawed), and HasSnapshot/SnapshotNode/SnapshotTopology/
// WarmBuckets treat a consumed snapshot as absent so the next spawn cold-starts
// directly instead of attempting a doomed thaw against the reaped image.

// consumedSnapshot is the record left after a verified single freeze→thaw cycle:
// a good snapshot whose image has been reaped and which Thaw has flagged
// consumed. Its bundle path is the live overlay backing the thawed Pod B.
func consumedSnapshot(agent, ns, node string) Snapshot {
	s := goodSnapshot(agent, ns, node)
	s.Namespace = ns
	s.ImagePath = "/var/lib/gardenio/ckpt/" + agent
	s.BundlePath = "/var/lib/gardenio/bundle/" + agent
	s.Consumed = true
	return s
}

// TestFreeze_RefusesConsumedSnapshot is the core re-freeze guard: a Freeze for an
// agent whose snapshot is already consumed must REFUSE (no node exec), return
// ErrFreezeAlreadyThawed, and DISCARD the stale record so the next spawn
// cold-starts cleanly instead of thawing against the reaped image.
func TestFreeze_RefusesConsumedSnapshot(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	// The LIVE pod here is the thawed Pod B — perfectly healthy, NOT terminating,
	// so the existing duplicate-Freeze guard (podGoneOrTerminating) would never
	// catch it. Only the consumed flag does.
	client := fake.NewClientset(livePodA("agentsim-thaw-b", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	if err := store.Put(context.Background(), consumedSnapshot("agentsim", ns, node)); err != nil {
		t.Fatalf("seed consumed snapshot: %v", err)
	}
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim-thaw-b", Namespace: ns,
	})
	if !errors.Is(err, ErrFreezeAlreadyThawed) {
		t.Fatalf("want ErrFreezeAlreadyThawed, got err=%v", err)
	}
	// The refused freeze returns the existing (consumed) snapshot for diagnostics.
	if snap == nil || !snap.Consumed {
		t.Fatalf("want the consumed snapshot returned, got %+v", snap)
	}
	// NO node exec — the refusal must precede any checkpoint.
	if len(exec.scripts) != 0 {
		t.Fatalf("re-freeze must not exec on the node; got %d script(s)", len(exec.scripts))
	}
	// The stale, un-thawable record must be discarded so the next spawn does not
	// attempt a doomed thaw against the reaped image.
	if _, getErr := store.Get(context.Background(), "agentsim"); getErr == nil {
		t.Fatal("consumed snapshot must be discarded after a refused re-freeze")
	}
}

// TestFreeze_RefusesConsumedSnapshotWithInterrupt repeats the guard with the
// interrupt machinery armed (InterruptEnabled=true): the consumed-snapshot
// refusal must run BEFORE registerInflight, so a refused re-freeze never even
// registers an in-flight slot (and the abort defer is inert).
func TestFreeze_RefusesConsumedSnapshotWithInterrupt(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim-thaw-b", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), consumedSnapshot("agentsim", ns, node))
	m := newFreezeThawManager(t, client, exec, store, nil)
	m.InterruptEnabled = true

	_, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim-thaw-b", Namespace: ns,
	})
	if !errors.Is(err, ErrFreezeAlreadyThawed) {
		t.Fatalf("want ErrFreezeAlreadyThawed, got err=%v", err)
	}
	if m.IsFreezing("agentsim") {
		t.Fatal("a refused re-freeze must not leave an in-flight slot registered")
	}
	if len(exec.scripts) != 0 {
		t.Fatalf("re-freeze must not exec on the node; got %d", len(exec.scripts))
	}
}

// TestFreeze_AllowsUnconsumedSnapshot is the negative control: a normal,
// not-yet-thawed snapshot (the duplicate-guard's good snapshot, but with a LIVE
// pod) must NOT be treated as consumed — Freeze proceeds to re-checkpoint the
// still-live pod (today's behaviour for a re-freeze before any thaw). This
// guards against the consumed check accidentally swallowing a legitimate freeze.
func TestFreeze_AllowsUnconsumedSnapshot(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	// A good (NOT consumed) snapshot already exists, pod is live.
	_ = store.Put(context.Background(), goodSnapshot("agentsim", ns, node))
	m := newFreezeThawManager(t, client, exec, store, nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns,
	})
	if errors.Is(err, ErrFreezeAlreadyThawed) {
		t.Fatalf("an unconsumed snapshot must not trip the re-freeze guard; got %v", err)
	}
	if err != nil {
		t.Fatalf("freeze of a live pod with an unconsumed snapshot should succeed; got %v", err)
	}
	// The checkpoint ran on the node.
	if len(exec.scripts) == 0 {
		t.Fatal("freeze of a live, unconsumed agent must exec the checkpoint on the node")
	}
}

// TestThaw_MarksSnapshotConsumed proves the mechanism end-to-end on the verb:
// after a verified thaw (image reaped, rehydration acked), the persisted
// snapshot record is flagged Consumed=true while its bundle path is preserved
// (it backs the live Pod B).
func TestThaw_MarksSnapshotConsumed(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := thawThenReapExec("FT_IMAGE_REAPED=yes\n", nil)
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath:  "/var/lib/gardenio/ckpt/agentsim",
		BundlePath: "/var/lib/gardenio/bundle/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if !res.ImageReaped {
		t.Fatal("precondition: thaw should have reaped the image")
	}
	got, gerr := store.Get(context.Background(), "agentsim")
	if gerr != nil {
		t.Fatalf("snapshot record must survive a thaw (it backs the live Pod B): %v", gerr)
	}
	if !got.Consumed {
		t.Error("snapshot must be marked Consumed after a verified thaw")
	}
	// The bundle path is preserved — it is the live Pod B's overlay source.
	if got.BundlePath != "/var/lib/gardenio/bundle/agentsim" {
		t.Errorf("consumed snapshot must preserve BundlePath (backs live Pod B); got %q", got.BundlePath)
	}
}

// TestThaw_SkipRehydrationDoesNotMarkConsumed: the SkipRehydration caller drives
// its own retry-from-snapshot lifecycle, so its record must stay live+thawable
// (mirrors the image NOT being reaped on that path). Marking it consumed would
// strand its retry un-thawable.
func TestThaw_SkipRehydrationDoesNotMarkConsumed(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-789"))
	exec := thawThenReapExec("FT_IMAGE_REAPED=yes\n", nil)
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath:  "/var/lib/gardenio/ckpt/agentsim",
		BundlePath: "/var/lib/gardenio/bundle/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", SkipRehydration: true})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if res.ImageReaped {
		t.Fatal("precondition: SkipRehydration must not reap the image")
	}
	got, _ := store.Get(context.Background(), "agentsim")
	if got.Consumed {
		t.Error("SkipRehydration thaw must NOT mark the snapshot consumed (its retry needs it thawable)")
	}
}

// TestConsumedSnapshotTreatedAsAbsent: the existence predicates the wake path and
// warm-B pool consult must all treat a consumed snapshot as if no snapshot
// exists — so the next spawn cold-starts directly and the pool wastes no
// capacity on a single-shot cycle that already completed.
func TestConsumedSnapshotTreatedAsAbsent(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), consumedSnapshot("agentsim", ns, node))
	m := newFreezeThawManager(t, fake.NewClientset(), &fakeNodeExecutor{}, store, nil)
	ctx := context.Background()

	if m.HasSnapshot(ctx, "agentsim") {
		t.Error("HasSnapshot must report false for a consumed snapshot (image reaped → un-thawable)")
	}
	if _, ok := m.SnapshotNode(ctx, "agentsim"); ok {
		t.Error("SnapshotNode must report (\"\", false) for a consumed snapshot")
	}
	if _, ok := m.SnapshotTopology(ctx, "agentsim"); ok {
		t.Error("SnapshotTopology must report (\"\", false) for a consumed snapshot")
	}
	buckets, err := m.WarmBuckets(ctx)
	if err != nil {
		t.Fatalf("WarmBuckets: %v", err)
	}
	if len(buckets) != 0 {
		t.Errorf("WarmBuckets must skip consumed snapshots; got %d bucket(s)", len(buckets))
	}
}

// TestFreezeThaw_MultiCycle_DegradesToColdStart is the full single-cycle story
// asserted as a sequence: freeze → thaw (consumes) → re-freeze (refused +
// discarded) → next-spawn predicate reports cold-start. This is the unit-level
// mirror of the gvisor-poc e2e (test-gvisor-multicycle-refreeze.sh).
func TestFreezeThaw_MultiCycle_DegradesToColdStart(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	ctx := context.Background()

	// ── cycle 1: freeze a live Pod A ──
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	freezeExec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, freezeExec, store, nil)
	if _, _, err := m.Freeze(ctx, FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns}); err != nil {
		t.Fatalf("cycle-1 freeze: %v", err)
	}
	if !m.HasSnapshot(ctx, "agentsim") {
		t.Fatal("after freeze, a thawable snapshot must exist")
	}

	// ── thaw: restore + reap + mark consumed ──
	thawClient := fake.NewClientset()
	thawClient.PrependReactor("create", "pods", readyPodReactor("b-uid-1"))
	thawExec := thawThenReapExec("FT_IMAGE_REAPED=yes\n", nil)
	mThaw := newFreezeThawManager(t, thawClient, thawExec, store, &recordingRehydrator{})
	if _, err := mThaw.Thaw(ctx, ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("thaw: %v", err)
	}
	// After thaw the agent is no longer thawable (single-shot consumed).
	if mThaw.HasSnapshot(ctx, "agentsim") {
		t.Fatal("after a verified thaw, the consumed snapshot must report as absent")
	}

	// ── cycle 2: graceful stop of the thawed Pod B reaches Freeze → refused ──
	// (model the seam that DOES resolve the real -thaw-b pod name.)
	thawClient2 := fake.NewClientset(livePodA("agentsim-thaw-b", ns, node))
	refreezeExec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	mRe := newFreezeThawManager(t, thawClient2, refreezeExec, store, nil)
	_, _, err := mRe.Freeze(ctx, FreezeRequest{AgentName: "agentsim", PodName: "agentsim-thaw-b", Namespace: ns})
	if !errors.Is(err, ErrFreezeAlreadyThawed) {
		t.Fatalf("cycle-2 re-freeze must be refused with ErrFreezeAlreadyThawed; got %v", err)
	}
	if len(refreezeExec.scripts) != 0 {
		t.Fatal("refused re-freeze must not checkpoint the live thawed Pod B's bundle")
	}

	// ── next spawn: snapshot discarded → cold start ──
	if m.HasSnapshot(ctx, "agentsim") {
		t.Fatal("after the refused re-freeze discarded the stale record, the next spawn must cold-start (no snapshot)")
	}
	if _, getErr := store.Get(ctx, "agentsim"); getErr == nil {
		t.Fatal("the consumed record must be gone from the store")
	}
}

// TestFreeze_OverwritesConsumedAfterColdStart proves the generation reset: once a
// consumed snapshot has been discarded (refused re-freeze) and the agent
// cold-starts, a genuine freeze of the fresh cold-started pod under the canonical
// name persists a NEW, unconsumed snapshot — re-opening the agent's single-cycle
// freeze/thaw for that generation.
func TestFreeze_OverwritesConsumedAfterColdStart(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	ctx := context.Background()
	store := NewInMemorySnapshotStore()
	// Seed a consumed snapshot, then discard it the way the guard does.
	_ = store.Put(ctx, consumedSnapshot("agentsim", ns, node))
	_ = store.Delete(ctx, "agentsim")

	// Fresh cold-started pod under the canonical name; freeze it.
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	m := newFreezeThawManager(t, client, exec, store, nil)
	if _, _, err := m.Freeze(ctx, FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns}); err != nil {
		t.Fatalf("post-cold-start freeze: %v", err)
	}
	got, gerr := store.Get(ctx, "agentsim")
	if gerr != nil {
		t.Fatalf("a fresh freeze must persist a new snapshot: %v", gerr)
	}
	if got.Consumed {
		t.Error("a freshly minted snapshot must be unconsumed (new generation re-opened)")
	}
	if !m.HasSnapshot(ctx, "agentsim") {
		t.Error("the fresh snapshot must be thawable")
	}
}

// Compile-time assurance that the helper's pod phase is Running (a live, healthy
// thawed Pod B is NOT terminating — the whole point the consumed flag, not
// podGoneOrTerminating, is what catches a re-freeze).
var _ = corev1.PodRunning
