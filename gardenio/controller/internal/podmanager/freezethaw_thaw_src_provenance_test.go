package podmanager

import (
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// TestThawScript_SourceProvenanceMarkerProbe proves the rendered thaw script
// carries the [6d] POST-restore thaw-state source-provenance content-marker probe
// (ka-0000001208): it writes a fresh unique marker into the live per-agent host
// source $THAWDIR and reads it back from INSIDE the restored sentry at $THAWDEST
// (/run/gardenio-thaw) via runsc exec, fail-closing FT_ROOT_RC=128 on a definitive
// ABSENT (mismatch). We assert on fragments rather than the mountinfo scrape the
// bead literally names because gVisor serves every host-backed bind mount through
// the gofer, so neither guest nor /proc/<RPID> mountinfo carries the host bind
// SOURCE path to compare against $THAWDIR (root-cause ka-0000000673) — the
// discriminator canary #7 actually used was mount CONTENTS.
func TestThawScript_SourceProvenanceMarkerProbe(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		"FT_THAW_SRC_PROVENANCE=",      // the [6d] outcome sentinel
		".ft-srcprov-",                 // the fresh unique host-written marker name
		`HMNT touch "$THAWDIR/$PMARK"`, // marker created in the HOST source $THAWDIR
		`"$THAWDEST/$PMARK"`,           // read back through the guest mount point
		"PRESENT",                      // positive-proof token (marker visible)
		"ABSENT",                       // definitive-negative token (marker missing)
		`[ "$FT_THAW_SRC_PROVENANCE" = mismatch ]`, // fail-closed on a definitive mismatch
		"FT_ROOT_RC=128", // the fail-closed rc the script drives
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing [6d] source-provenance fragment %q", want)
		}
	}
}

// TestThaw_ThawSrcProvenanceMismatchFailsClosed is the Go-side red bar for
// ka-0000001208: a restore whose [6d] probe reports FT_THAW_SRC_PROVENANCE=mismatch
// — the fresh host-written marker was NOT visible inside the restored sentry, so the
// restore re-walked /run/gardenio-thaw to a stale/placeholder/checkpointed 9p-gofer
// source (canary #7's "rc=0 against a different-content dir") — must fail loud and NOT
// fire rehydration. The node script already fails-closed FT_ROOT_RC=128 on a mismatch;
// here we drive rc=0 to prove the Go acceptance is defense-in-depth even if the
// script's own fail-close were bypassed.
func TestThaw_ThawSrcProvenanceMismatchFailsClosed(t *testing.T) {
	mutated := thawStdoutReplace("FT_THAW_SRC_PROVENANCE=ok", "FT_THAW_SRC_PROVENANCE=mismatch")

	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: mutated}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "does NOT resolve to the live per-agent host source") {
		t.Fatalf("FT_THAW_SRC_PROVENANCE=mismatch must FAIL the thaw — the restored /run/gardenio-thaw resolved to a stale/wrong source; got %v", err)
	}
	if len(rehydr.reqs) != 0 {
		t.Errorf("rehydration must not fire when the thaw-state source provenance mismatches; got %d requests", len(rehydr.reqs))
	}
}

// TestThaw_ThawSrcProvenanceUnknownStillThaws proves the [6d] probe never
// false-fail-closes a GOOD resume: FT_THAW_SRC_PROVENANCE=unknown (the in-sentry
// exec flaked / $THAWDIR was empty on a legacy snapshot) is INCONCLUSIVE and must NOT
// cold-start an otherwise-verified restore — the [3a] pre-restore dangling-source
// guard + the rehydrate ack remain the functional gate. Mirrors the sentry-egress
// inconclusive/skipped carve-out.
func TestThaw_ThawSrcProvenanceUnknownStillThaws(t *testing.T) {
	mutated := thawStdoutReplace("FT_THAW_SRC_PROVENANCE=ok", "FT_THAW_SRC_PROVENANCE=unknown")

	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: mutated}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("an inconclusive (unknown) thaw-state provenance probe must NOT fail the thaw, got %v", err)
	}
}

// TestThaw_LegacySnapshotWithoutProvenanceSentinel proves backward-compatibility: a
// snapshot restored by an older node script that never emits FT_THAW_SRC_PROVENANCE
// is treated as not-asserted (the [3a] guard + ack gate still gate it), NOT a failure,
// so older in-flight snapshots still thaw.
func TestThaw_LegacySnapshotWithoutProvenanceSentinel(t *testing.T) {
	stdout := strings.Replace(thawOKStdout, "FT_THAW_SRC_PROVENANCE=ok\n", "", 1)

	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("legacy snapshot without the FT_THAW_SRC_PROVENANCE sentinel should still thaw, got %v", err)
	}
}
