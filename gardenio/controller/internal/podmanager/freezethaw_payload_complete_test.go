package podmanager

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"
)

// freezeStdoutWithPayload returns a freeze-OK node stdout with the payload-
// completeness sentinels (FT_WORK_CP_RC / FT_TMP_CP_RC / FT_GOFER_FAILED) appended
// to the canonical freezeOKStdout fixture. Empty strings OMIT that sentinel line so
// a test can also exercise the older-node-script (missing sentinel) path. Exercises
// the real parse + WARN path for ka-0000000276. It emits NO FT_*_CP_REASON, so it
// exercises the OLDER-NODE-SCRIPT (reason-absent) rc-fallback path (M8 back-compat)
// — a non-zero rc here is still treated as incomplete, unchanged from pre-fix.
func freezeStdoutWithPayload(workRC, tmpRC, goferFailed string) string {
	return freezeStdoutWithPayloadReason(workRC, tmpRC, goferFailed, "", "")
}

// freezeStdoutWithPayloadReason is freezeStdoutWithPayload plus the de-noise
// classification sentinels FT_WORK_CP_REASON / FT_TMP_CP_REASON (ka-0000006933).
// Empty reason strings OMIT that line (the reason-absent rc-fallback path). A
// present reason (clean|symlink|copyfail, or a malformed value) drives the new
// per-leg denoise verdict.
func freezeStdoutWithPayloadReason(workRC, tmpRC, goferFailed, workReason, tmpReason string) string {
	var b strings.Builder
	b.WriteString(freezeOKStdout)
	if workRC != "" {
		b.WriteString("FT_WORK_CP_RC=" + workRC + "\n")
	}
	if tmpRC != "" {
		b.WriteString("FT_TMP_CP_RC=" + tmpRC + "\n")
	}
	if goferFailed != "" {
		b.WriteString("FT_GOFER_FAILED=" + goferFailed + "\n")
	}
	if workReason != "" {
		b.WriteString("FT_WORK_CP_REASON=" + workReason + "\n")
	}
	if tmpReason != "" {
		b.WriteString("FT_TMP_CP_REASON=" + tmpReason + "\n")
	}
	return b.String()
}

// TestFreeze_PayloadCompleteRoundTripsClean asserts the happy path (ka-0000000276):
// FT_WORK_CP_RC=0, FT_TMP_CP_RC=0, FT_GOFER_FAILED=0 round-trip into the "freeze:
// snapshot persisted" post-state log as payloadComplete=true / goferFailed=0 and do
// NOT emit the incomplete-payload WARN. The freeze succeeds and persists a snapshot.
func TestFreeze_PayloadCompleteRoundTripsClean(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayload("0", "0", "0")}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap == nil {
		t.Fatal("expected a persisted snapshot on a clean freeze")
	}
	out := buf.String()
	if !strings.Contains(out, "freeze: snapshot persisted") {
		t.Fatalf("post-state persisted log missing, got:\n%s", out)
	}
	if !strings.Contains(out, "payloadComplete=true") {
		t.Errorf("clean payload must log payloadComplete=true, got:\n%s", out)
	}
	if strings.Contains(out, "workspace payload copy INCOMPLETE") {
		t.Errorf("clean payload must NOT emit the incomplete-payload WARN, got:\n%s", out)
	}
}

// TestFreeze_PayloadIncompleteWorkCP asserts the /work copy-failure path: a non-zero
// FT_WORK_CP_RC (a mid-copy ENOSPC/EIO or a dereferenced dangling symlink) round-trips
// as payloadComplete=false AND emits the incomplete-payload WARN. Critically the freeze
// is NOT gated — it still succeeds and persists a (thawable-with-cold-start) snapshot,
// because the affected mount already fails closed at thaw and a non-zero cp rc is
// dominated by benign dangling symlinks (gating would cold-start thawable agents).
func TestFreeze_PayloadIncompleteWorkCP(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayload("1", "0", "0")}}
	var buf bytes.Buffer
	store := NewInMemorySnapshotStore()
	m := bufLoggerManager(t, &buf, client, exec, store)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	// Log-only: an incomplete payload must NOT fail the freeze.
	if err != nil {
		t.Fatalf("incomplete payload must not gate the freeze, got err: %v", err)
	}
	if snap == nil {
		t.Fatal("incomplete payload must still persist a (cold-start-at-thaw) snapshot")
	}
	if got, err := store.Get(context.Background(), "agentsim"); err != nil || got.SandboxCID == "" {
		t.Fatalf("snapshot not persisted on incomplete /work payload: got=%+v err=%v", got, err)
	}
	out := buf.String()
	if !strings.Contains(out, "payloadComplete=false") {
		t.Errorf("incomplete /work payload must log payloadComplete=false, got:\n%s", out)
	}
	if !strings.Contains(out, "workspace payload copy INCOMPLETE") {
		t.Errorf("incomplete /work payload must emit the WARN, got:\n%s", out)
	}
	if !strings.Contains(out, "workCPRC=1") {
		t.Errorf("WARN must carry workCPRC=1, got:\n%s", out)
	}
}

// TestFreeze_PayloadIncompleteGoferFailed asserts the .gofer sweep failure path: a
// non-zero FT_GOFER_FAILED (a genuine copytree ENOSPC/EIO on one or more gofer mounts)
// round-trips as payloadComplete=false / goferFailed=<n> and emits the WARN, without
// gating the freeze.
func TestFreeze_PayloadIncompleteGoferFailed(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayload("0", "0", "2")}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("gofer copy failure must not gate the freeze, got err: %v", err)
	}
	if snap == nil {
		t.Fatal("gofer copy failure must still persist a snapshot")
	}
	out := buf.String()
	if !strings.Contains(out, "payloadComplete=false") {
		t.Errorf("gofer failure must log payloadComplete=false, got:\n%s", out)
	}
	if !strings.Contains(out, "goferFailed=2") {
		t.Errorf("gofer failure must log goferFailed=2, got:\n%s", out)
	}
	if !strings.Contains(out, "workspace payload copy INCOMPLETE") {
		t.Errorf("gofer failure must emit the WARN, got:\n%s", out)
	}
}

// TestFreeze_PayloadGoferFailedMalformedWarns asserts the corrupt-telemetry case
// (ka-0000000735): a PRESENT-but-unparseable FT_GOFER_FAILED (e.g. a python traceback
// line bleeding into the sentinel, here FT_GOFER_FAILED=oops) must NOT silently collapse
// to goferFailed==0 / payloadComplete=true and suppress the wasted-freeze signal. Instead
// it round-trips as payloadComplete=false and emits the dedicated "sentinel present but
// unparseable" WARN. This is the blind spot the discarded intField known bool created:
// distinguishing a MISSING sentinel (older script => clean, TestFreeze_PayloadSentinels-
// MissingIsCleanNotWarn) from a MALFORMED-but-present one (parse failure => surface it).
// Critically it is still LOG-ONLY — the freeze is NOT gated and persists a snapshot.
func TestFreeze_PayloadGoferFailedMalformedWarns(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	// FT_WORK_CP_RC=0, FT_TMP_CP_RC=0 (clean), but FT_GOFER_FAILED=oops is non-integer.
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayload("0", "0", "oops")}}
	var buf bytes.Buffer
	store := NewInMemorySnapshotStore()
	m := bufLoggerManager(t, &buf, client, exec, store)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	// Log-only: a corrupt sentinel must NOT fail the freeze.
	if err != nil {
		t.Fatalf("malformed FT_GOFER_FAILED must not gate the freeze, got err: %v", err)
	}
	if snap == nil {
		t.Fatal("malformed FT_GOFER_FAILED must still persist a snapshot")
	}
	if got, err := store.Get(context.Background(), "agentsim"); err != nil || got.SandboxCID == "" {
		t.Fatalf("snapshot not persisted on malformed FT_GOFER_FAILED: got=%+v err=%v", got, err)
	}
	out := buf.String()
	if !strings.Contains(out, "payloadComplete=false") {
		t.Errorf("malformed FT_GOFER_FAILED must log payloadComplete=false (not silently 0), got:\n%s", out)
	}
	if !strings.Contains(out, "FT_GOFER_FAILED sentinel present but unparseable") {
		t.Errorf("malformed FT_GOFER_FAILED must emit the dedicated parse-error WARN, got:\n%s", out)
	}
	if !strings.Contains(out, "rawGoferFailed=oops") {
		t.Errorf("parse-error WARN must carry the raw unparseable value, got:\n%s", out)
	}
	// The corrupt sentinel is telemetry-only: it must NOT be misreported as an observed
	// warm-copy failure (the generic copy-INCOMPLETE WARN is for a real non-clean rc/count).
	if strings.Contains(out, "workspace payload copy INCOMPLETE") {
		t.Errorf("a malformed sentinel must not masquerade as an observed warm-copy failure, got:\n%s", out)
	}
}

// TestFreeze_PayloadSentinelsMissingIsCleanNotWarn asserts the older-node-script case:
// a freeze stdout with NO FT_WORK_CP_RC / FT_TMP_CP_RC / FT_GOFER_FAILED sentinels must
// be treated as complete (payloadComplete=true) and must NOT emit the WARN — a missing
// sentinel is an older script, not a failure, so "unknown" never masquerades as a
// broken payload (which would spuriously alarm on every legacy snapshot). This mirrors
// the FT_PAUSE_RC missing-sentinel semantics but errs toward NOT alarming, because
// unlike pauseRC there is no observed rc to distrust — absence just means the copy
// steps ran with the pre-fix no-signal shape.
func TestFreeze_PayloadSentinelsMissingIsCleanNotWarn(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	// Canonical fixture carries none of the payload-completeness sentinels.
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	if _, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	}); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "payloadComplete=true") {
		t.Errorf("missing payload sentinels must log payloadComplete=true (older script, not a failure), got:\n%s", out)
	}
	if strings.Contains(out, "workspace payload copy INCOMPLETE") {
		t.Errorf("missing payload sentinels must NOT emit the WARN, got:\n%s", out)
	}
}

// TestFreeze_PayloadWorkCPSymlinkReasonIsComplete asserts the DE-NOISE core
// (ka-0000006933 finding #1): a non-zero FT_WORK_CP_RC whose FT_WORK_CP_REASON=symlink
// (the node classifier positively proved the copy is byte-complete and the rc was
// tripped ONLY by a benign dangling symlink cp -aL skipped) must round-trip as
// payloadComplete=TRUE and must NOT emit the incomplete-payload WARN. This is the
// whole point of the change: the benign symlink noise that pre-fix flagged
// payloadComplete=false on a large fraction of byte-complete freezes no longer does.
func TestFreeze_PayloadWorkCPSymlinkReasonIsComplete(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	// rc=1 but reason=symlink: byte-complete, benign dangling-link-only rc.
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayloadReason("1", "0", "0", "symlink", "clean")}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	snap, timing, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap == nil {
		t.Fatal("expected a persisted snapshot")
	}
	out := buf.String()
	if !strings.Contains(out, "payloadComplete=true") {
		t.Errorf("symlink-only rc must DE-NOISE to payloadComplete=true, got:\n%s", out)
	}
	if strings.Contains(out, "workspace payload copy INCOMPLETE") {
		t.Errorf("symlink-only rc must NOT emit the incomplete-payload WARN, got:\n%s", out)
	}
	// The verdict must mark the leg as a benign symlink skip (drives the
	// payload_symlink_skip metric), NOT a genuine copy failure.
	if !timing.Payload.WorkSymlinkSkip {
		t.Errorf("symlink-only /work rc must set Payload.WorkSymlinkSkip, got %+v", timing.Payload)
	}
	if timing.Payload.WorkCopyFailed {
		t.Errorf("symlink-only /work rc must NOT set Payload.WorkCopyFailed, got %+v", timing.Payload)
	}
}

// TestFreeze_PayloadWorkCPCopyFailReasonIsIncomplete asserts the genuine-failure
// path with the new reason (ka-0000006933): a non-zero FT_WORK_CP_RC whose
// FT_WORK_CP_REASON=copyfail (a real ENOSPC/EIO/short-write, or the classifier
// could not prove completeness) round-trips as payloadComplete=false + the WARN,
// and the verdict marks a genuine WorkCopyFailed (drives the ALERT metric). Still
// LOG-ONLY — the freeze persists.
func TestFreeze_PayloadWorkCPCopyFailReasonIsIncomplete(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayloadReason("1", "0", "0", "copyfail", "clean")}}
	var buf bytes.Buffer
	store := NewInMemorySnapshotStore()
	m := bufLoggerManager(t, &buf, client, exec, store)

	snap, timing, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("genuine copy failure must not gate the freeze, got err: %v", err)
	}
	if snap == nil {
		t.Fatal("genuine copy failure must still persist a snapshot")
	}
	out := buf.String()
	if !strings.Contains(out, "payloadComplete=false") {
		t.Errorf("copyfail reason must log payloadComplete=false, got:\n%s", out)
	}
	if !strings.Contains(out, "workspace payload copy INCOMPLETE") {
		t.Errorf("copyfail reason must emit the WARN, got:\n%s", out)
	}
	if !strings.Contains(out, "workCPReason=copyfail") {
		t.Errorf("WARN must carry the workCPReason=copyfail attribution, got:\n%s", out)
	}
	if !timing.Payload.WorkCopyFailed || timing.Payload.WorkSymlinkSkip {
		t.Errorf("copyfail /work must set WorkCopyFailed and NOT WorkSymlinkSkip, got %+v", timing.Payload)
	}
}

// TestFreeze_PayloadWorkCPMalformedReasonIsIncomplete asserts M6: a non-zero rc
// with a PRESENT-but-unrecognized FT_WORK_CP_REASON (e.g. a SIGKILL-truncated
// "symlin", here "garbage") must NOT be read as the benign symlink verdict — it
// falls through to a genuine incomplete (WorkCopyFailed) so a lost freeze is never
// downgraded to complete by a corrupt reason.
func TestFreeze_PayloadWorkCPMalformedReasonIsIncomplete(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayloadReason("1", "0", "0", "garbage", "clean")}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	_, timing, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "payloadComplete=false") {
		t.Errorf("a corrupt reason on an rc!=0 leg must stay incomplete (never a false-clean), got:\n%s", out)
	}
	if !timing.Payload.WorkCopyFailed || timing.Payload.WorkSymlinkSkip {
		t.Errorf("corrupt reason must set WorkCopyFailed, NOT WorkSymlinkSkip, got %+v", timing.Payload)
	}
}

// TestFreeze_PayloadCleanReasonNoVerdict asserts a fully clean freeze (rc=0 +
// reason=clean on both legs, goferFailed=0) sets NO payload verdict flags — so the
// caller-side recorder increments NEITHER counter (S8 negative guard, at the
// verdict layer).
func TestFreeze_PayloadCleanReasonNoVerdict(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeStdoutWithPayloadReason("0", "0", "0", "clean", "clean")}}
	var buf bytes.Buffer
	m := bufLoggerManager(t, &buf, client, exec, NewInMemorySnapshotStore())

	_, timing, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	v := timing.Payload
	if v.WorkCopyFailed || v.TmpCopyFailed || v.WorkSymlinkSkip || v.TmpSymlinkSkip || v.GoferFailed || v.GoferMalformed {
		t.Errorf("a clean freeze must leave every payload verdict flag false, got %+v", v)
	}
}

// TestClassifyCopyLeg is a direct table test of the per-leg verdict resolver
// (ka-0000006933) covering the full (rc, rcKnown, reason) matrix — the M6/M8
// safety table: clean, benign symlink, genuine copyfail, older-node rc-fallback,
// and the corrupt/unknown-reason-never-a-false-clean cases.
func TestClassifyCopyLeg(t *testing.T) {
	cases := []struct {
		name            string
		rc              int
		rcKnown         bool
		reason          string
		wantFailed      bool
		wantSymlinkSkip bool
	}{
		{"rc0 clears regardless of reason", 0, true, "", false, false},
		{"rc0 with clean reason", 0, true, "clean", false, false},
		{"reason clean on rc1", 1, true, "clean", false, false},
		{"reason symlink denoises rc1", 1, true, "symlink", false, true},
		{"reason copyfail on rc1", 1, true, "copyfail", true, false},
		{"older node: rc1 no reason falls back to incomplete", 1, true, "", true, false},
		{"older node: rc0 no reason is clean", 0, true, "", false, false},
		{"no rc at all (older-still) is clean", 0, false, "", false, false},
		{"corrupt reason on rc1 stays incomplete (M6)", 1, true, "symlin", true, false},
		{"corrupt reason with unknown rc stays incomplete (M6)", 0, false, "garbage", true, false},
	}
	for _, c := range cases {
		failed, skip := classifyCopyLeg(c.rc, c.rcKnown, c.reason)
		if failed != c.wantFailed || skip != c.wantSymlinkSkip {
			t.Errorf("%s: classifyCopyLeg(%d,%v,%q)=(%v,%v) want (%v,%v)",
				c.name, c.rc, c.rcKnown, c.reason, failed, skip, c.wantFailed, c.wantSymlinkSkip)
		}
	}
}
