package podmanager

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// freezethaw_reloc_reflink_warn_test.go is the WP7 guard (ka-0000001867). It proves the
// rootfs-relocation NON-REFLINK verdict (freezethaw_freeze.go, ka-0000007174) is promoted from a
// value buried in the 12-field "freeze: snapshot persisted" Info line to its OWN WARN when
// the exact canary #13 signature holds — reflinkHits==0 with a NON-empty relocation stack
// (relocReflinkFallbacks>0) — so a freeze that full-copied the multi-GB rootfs off the
// critical path is not a silent soft-PASS. It ALSO pins the three no-WARN cases so the
// promotion never becomes noisy (a clean reflink, the STACK-empty merged-cp fallback that
// legitimately reflinks nothing, and a self-medium / older-node freeze that emits no
// counters at all). The WARN is DIAGNOSTIC — every case below still persists a snapshot and
// returns no error, because a full-copy relocation produced a correct snapshot; gating would
// trade a slow freeze for a lost one (the FT_PAUSE_RC / payload-completeness stance).

// freezeStdoutWithRelocReflink returns a freeze-OK node stdout with the per-container
// relocation reflink counters appended (FT_RELOC_REFLINK_HITS_agent / _FALLBACKS_agent),
// exercising the real sumSuffixedSentinels parse + WARN path. It reuses the canonical
// freezeOKStdout fixture (which passes every post-state assertion), so only the reflink
// verdict varies across cases.
func freezeStdoutWithRelocReflink(hits, fallbacks string) string {
	return freezeOKStdout +
		"FT_RELOC_REFLINK_HITS_agent=" + hits + "\n" +
		"FT_RELOC_REFLINK_FALLBACKS_agent=" + fallbacks + "\n"
}

// runFreezeForReflinkVerdict drives a real Freeze against the given node stdout and returns
// the captured post-state log. It asserts the freeze SUCCEEDED and persisted a snapshot —
// the reflink verdict is diagnostic, so every case must publish a usable snapshot.
func runFreezeForReflinkVerdict(t *testing.T, stdout string) string {
	t.Helper()
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	var buf bytes.Buffer
	store := NewInMemorySnapshotStore()
	m := bufLoggerManager(t, &buf, client, exec, store)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: reflink verdict must never gate the freeze, got err: %v", err)
	}
	if snap == nil {
		t.Fatal("Freeze: a diagnostic reflink verdict must still persist a snapshot")
	}
	if got, gerr := store.Get(context.Background(), "agentsim"); gerr != nil || got.SandboxCID == "" {
		t.Fatalf("snapshot not persisted: got=%+v err=%v", got, gerr)
	}
	out := buf.String()
	if !strings.Contains(out, "freeze: snapshot persisted") {
		t.Fatalf("post-state persisted log missing, got:\n%s", out)
	}
	return out
}

const relocReflinkWarnMsg = "rootfs relocation did NOT reflink"

// TestFreeze_RelocReflink_NonEmptyStackZeroHits_WARNs is the core WP7 guard: the canary #13
// signature (hits=0, fallbacks>0 = a non-empty stack that reflinked nothing) MUST emit the
// dedicated WARN, on top of the persisted Info line. This is the exact "did NOT reflink"
// smoking gun the fix exists to make loud.
func TestFreeze_RelocReflink_NonEmptyStackZeroHits_WARNs(t *testing.T) {
	out := runFreezeForReflinkVerdict(t, freezeStdoutWithRelocReflink("0", "2"))
	if !strings.Contains(out, relocReflinkWarnMsg) {
		t.Errorf("hits=0 with a non-empty stack (fallbacks=2) MUST emit the non-reflink WARN (the canary #13 signature), got:\n%s", out)
	}
	// The WARN must carry the accounting so an operator can act on it without joining the
	// Info line.
	for _, want := range []string{"relocReflinkHits=0", "relocReflinkFallbacks=2"} {
		if !strings.Contains(out, want) {
			t.Errorf("non-reflink WARN must carry %q, got:\n%s", want, out)
		}
	}
	// Diagnostic-only: the persisted Info line still records the known verdict too.
	if !strings.Contains(out, "relocReflinkKnown=true") {
		t.Errorf("a non-empty stack means the counters were emitted → relocReflinkKnown=true, got:\n%s", out)
	}
}

// TestFreeze_RelocReflink_CleanReflink_NoWARN pins the happy path: every stacked segment
// reflinked (hits>0, fallbacks==0), so NO WARN fires. This is what canary #14 produced
// (relocReflinkHits=6, fallbacks=0).
func TestFreeze_RelocReflink_CleanReflink_NoWARN(t *testing.T) {
	out := runFreezeForReflinkVerdict(t, freezeStdoutWithRelocReflink("6", "0"))
	if strings.Contains(out, relocReflinkWarnMsg) {
		t.Errorf("a clean all-reflink relocation (hits=6, fallbacks=0) must NOT emit the non-reflink WARN, got:\n%s", out)
	}
	if !strings.Contains(out, "relocReflinkHits=6") {
		t.Errorf("clean reflink must log relocReflinkHits=6, got:\n%s", out)
	}
}

// TestFreeze_RelocReflink_StackEmptyFallback_NoWARN is the critical false-positive guard.
// The STACK-empty merged-cp fallback (ka-0000007174 KS-1) emits FT_RELOC_REFLINK_HITS=0 AND
// FALLBACKS=0 — there were zero segments to reflink, so hits=0 is CORRECT, not a failure.
// The WARN keys off fallbacks>0 precisely so this legitimate zero-stack case does NOT WARN
// (a hits=0 alone rule would spuriously fire here).
func TestFreeze_RelocReflink_StackEmptyFallback_NoWARN(t *testing.T) {
	out := runFreezeForReflinkVerdict(t, freezeStdoutWithRelocReflink("0", "0"))
	if strings.Contains(out, relocReflinkWarnMsg) {
		t.Errorf("the STACK-empty merged-cp fallback (hits=0, fallbacks=0) must NOT WARN — zero segments to reflink is correct, not the canary #13 signature, got:\n%s", out)
	}
	// It IS a known verdict (the counters were emitted), just an empty-stack one.
	if !strings.Contains(out, "relocReflinkKnown=true") {
		t.Errorf("emitted 0/0 counters → relocReflinkKnown=true, got:\n%s", out)
	}
}

// TestFreeze_RelocReflink_SelfMediumNoCounters_NoWARN pins the self-medium / older-node
// case: NO FT_RELOC_REFLINK_* sentinels are emitted, so relocReflinkKnown=false and the WARN
// must NOT fire (unknown must never masquerade as the non-reflink signature). The canonical
// freezeOKStdout carries no reflink counters, so it exercises exactly this path.
func TestFreeze_RelocReflink_SelfMediumNoCounters_NoWARN(t *testing.T) {
	out := runFreezeForReflinkVerdict(t, freezeOKStdout)
	if strings.Contains(out, relocReflinkWarnMsg) {
		t.Errorf("a self-medium / older-node freeze emits no reflink counters (unknown) — must NOT WARN, got:\n%s", out)
	}
	if !strings.Contains(out, "relocReflinkKnown=false") {
		t.Errorf("no reflink counters → relocReflinkKnown=false (unknown, not a known all-fallbacks run), got:\n%s", out)
	}
}
