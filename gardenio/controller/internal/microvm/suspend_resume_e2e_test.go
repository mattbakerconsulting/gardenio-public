package microvm

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

// TestE2E_MicrovmSuspendResume_SubstrateContinuity is the substrate half of the
// suspend/resume PROOF (task gd-5NehzMXXO7, fix-now bar of decision
// gd-kymiq5dVge). Where the cmd/controller E2E proves the two production seams
// hand off correctly, this drives the REAL Lifecycle + REAL Backend (only the
// AWS MicroVM/Secrets APIs are faked) through the whole life story of ONE VM —
// launch → work → suspend-on-stop → resume — and asserts that the SAME VM
// survives the whole cycle, with the two landed blocker fixes as validated
// preconditions baked into the flow:
//
//	ka-0000002917 (idle-suspend-kill): a freshly launched egress-only MicroVM
//	  was being AWS-idle-suspended ~13s into its first turn because the IdlePolicy
//	  idle clock (measured by INBOUND traffic) never advances in an egress-only
//	  topology and sat at/above threshold. The fix raises the effective
//	  MaxIdleDurationSeconds ABOVE the VM lifetime cap so AWS idle-suspend can
//	  never fire mid-turn — leaving controller-driven SuspendMicrovm (on gb stop)
//	  as the only suspend path. This test seeds the pre-fix DANGEROUS IdlePolicy
//	  (maxIdle at the 60s AWS floor) and asserts the launch RunMicrovm carries the
//	  RAISED value, so the working VM is never idle-reaped.
//
//	ka-0000001013 (S3 session persistence): the MEMORY half of resume — restoring
//	  the transcript from S3 — depends on the per-run session credentials
//	  surviving standby. This test asserts Suspend does NOT revoke/delete per-run
//	  secrets, so the resumed VM keeps its S3 session authority and restored
//	  session (the exact survival the resume path needs).
func TestE2E_MicrovmSuspendResume_SubstrateContinuity(t *testing.T) {
	const (
		beadID = "gd-mvm-e2e-sub"
		vmID   = "mvm-e2e-vm0" // the ONE VM that must survive launch→suspend→resume
	)

	// A freeze-eligible MicroVM thread agent. The IdlePolicy is deliberately
	// seeded at the PRE-FIX dangerous shape (maxIdle at the 60s AWS floor) so we
	// can prove the fix raised it out of the mid-turn kill zone.
	cfg := testBackendConfig()
	cfg.IdlePolicy = NewIdlePolicy(true /*enabled*/, false /*autoResume*/, 60 /*maxIdle*/, 900 /*suspended*/)

	mv := &fakeMicrovm{runID: vmID}
	sm := newFakeSecrets()
	store := newFakeBeadStore()
	b := mustBackend(t, mv, sm, cfg)
	b.SetLauncherAccount("345678901234")
	l := NewLifecycle(b, store, discardLogger())

	// --- Phase 0: the idle-suspend-kill fix (ka-0000002917) is in force --------
	// newBackendWithClients ran normalizeIdlePolicy, which must have raised
	// MaxIdleDurationSeconds above the VM lifetime cap. MaximumDurationSeconds is
	// unset → defaults to 3600, so the floor is 3600+300=3900.
	wantMinIdle := defaultMaximumDurationSeconds + idleLifetimeSlackSeconds
	if got := aws.ToInt32(cfg.IdlePolicy.MaxIdleDurationSeconds); got < wantMinIdle {
		t.Fatalf("idle-suspend-kill fix NOT in force: MaxIdleDurationSeconds=%d, want >= %d (raised above VM lifetime so AWS never idle-reaps mid-turn)", got, wantMinIdle)
	}

	// --- Phase 1: launch (mention → work) — VM0 comes up ----------------------
	handle, err := l.Launch(context.Background(), AgentSpec{
		BeadID:      beadID,
		Project:     "gardenio",
		Role:        "gardenio-microvms",
		Environment: map[string]string{"AWS_ACCESS_KEY_ID": "S3-session-cred"}, // the vended S3 session credential (ka-0000001013)
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if handle.MicrovmID != vmID {
		t.Fatalf("launched MicrovmID = %q, want %q", handle.MicrovmID, vmID)
	}
	if mv.runCalls != 1 {
		t.Fatalf("RunMicrovm calls = %d, want 1", mv.runCalls)
	}
	// The launch request must carry the RAISED idle window — proof the working VM
	// is safe from AWS mid-turn idle-suspend (ka-0000002917).
	if mv.runInput == nil || mv.runInput.IdlePolicy == nil {
		t.Fatal("RunMicrovm carried no IdlePolicy; the idle-policy fix must ship a raised policy on the launch")
	}
	if got := aws.ToInt32(mv.runInput.IdlePolicy.MaxIdleDurationSeconds); got < wantMinIdle {
		t.Fatalf("RunMicrovm IdlePolicy.MaxIdleDurationSeconds = %d, want >= %d", got, wantMinIdle)
	}
	// runtime_id VM0 was persisted onto the bead — the continuity anchor a later
	// re-mention reads to resume the SAME VM.
	if got := lastFieldValue(store, beadID, "runtime_id"); got != vmID {
		t.Fatalf("persisted runtime_id = %q, want %q", got, vmID)
	}

	// --- Phase 2: gb stop → Suspend (standby, resume target) ------------------
	// This is the controller-driven suspend the idle-fix leaves as the sole path.
	suspendView := AgentBeadView{
		ID: beadID, ExecutionTarget: ExecutionTargetLambdaMicroVM,
		RuntimeID: vmID, AgentState: "stopping", RuntimeState: "RUNNING",
	}
	if err := l.Suspend(context.Background(), suspendView, "agent_stop"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(mv.suspCalls) != 1 || mv.suspCalls[0] != vmID {
		t.Fatalf("SuspendMicrovm calls = %v, want [%s]", mv.suspCalls, vmID)
	}
	if len(mv.termCalls) != 0 {
		t.Fatalf("suspend must NOT terminate the VM (it is the resume target), got %v", mv.termCalls)
	}
	if _, closed := store.closed[beadID]; closed {
		t.Fatal("suspend must leave the bead OPEN (a close would fire AgentDone→Terminate and destroy the VM)")
	}
	if got := lastFieldValue(store, beadID, "runtime_state"); got != "SUSPENDED" {
		t.Fatalf("post-suspend runtime_state = %q, want SUSPENDED", got)
	}
	if got := lastFieldValue(store, beadID, "runtime_suspended_at"); got == "" {
		t.Fatal("suspend must stamp runtime_suspended_at (standby-age anchor)")
	} else if _, perr := time.Parse(time.RFC3339, got); perr != nil {
		t.Fatalf("runtime_suspended_at = %q not RFC3339: %v", got, perr)
	}
	// S3 session-credential survival (ka-0000001013): suspend must NOT delete the
	// per-run secret — the resumed VM needs its session creds to restore the
	// transcript from S3. (Terminate deletes it; Suspend deliberately does not.)
	if len(sm.deleted) != 0 {
		t.Fatalf("suspend must NOT delete per-run secrets — S3 session creds must survive standby for the transcript restore, got deleted=%v", sm.deleted)
	}

	// --- Phase 3: re-mention → Resume the SAME VM (continuity) -----------------
	// A re-mention's handler reads runtime_id off the still-open bead and resumes
	// THAT VM. Read it back from the SAME store to prove the handoff is genuine.
	resumedRuntimeID := lastFieldValue(store, beadID, "runtime_id")
	if resumedRuntimeID != vmID {
		t.Fatalf("runtime_id on the suspended bead = %q, want %q (session continuity anchor)", resumedRuntimeID, vmID)
	}
	resumeView := AgentBeadView{
		ID: beadID, ExecutionTarget: ExecutionTargetLambdaMicroVM,
		RuntimeID: resumedRuntimeID, AgentState: "stopping", RuntimeState: "SUSPENDED",
	}
	if err := l.Resume(context.Background(), resumeView); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	// The resume must target the SAME VM the suspend left — never a fresh
	// RunMicrovm. runCalls stays 1; a resume of VM0 was issued.
	if mv.runCalls != 1 {
		t.Fatalf("resume must not launch a new VM: RunMicrovm calls = %d, want still 1", mv.runCalls)
	}
	if len(mv.resCalls) != 1 || mv.resCalls[0] != vmID {
		t.Fatalf("ResumeMicrovm calls = %v, want [%s] (SAME VM)", mv.resCalls, vmID)
	}
	if len(mv.termCalls) != 0 {
		t.Fatalf("resume must not terminate any VM, got %v", mv.termCalls)
	}
	if _, closed := store.closed[beadID]; closed {
		t.Fatal("resume must leave the bead OPEN (a close would terminate the resumed VM)")
	}
	// The bead flips out of standby: working, runtime_suspended_at cleared so the
	// max-standby-age sweep can't reap a live VM off a stale anchor.
	if got := lastFieldValue(store, beadID, "agent_state"); got != "working" {
		t.Fatalf("post-resume agent_state = %q, want working", got)
	}
	if got := lastFieldValue(store, beadID, "runtime_suspended_at"); got != "" {
		t.Fatalf("post-resume runtime_suspended_at = %q, want cleared", got)
	}

	// --- Outcome: ONE VM, ONE launch, ONE suspend, ONE resume, ZERO terminate --
	// The whole idle→suspend→S3-survival→resume cycle ran on a single VM whose
	// identity (vmID) never changed — the definition of native suspend/resume.
	if mv.runCalls != 1 || len(mv.suspCalls) != 1 || len(mv.resCalls) != 1 || len(mv.termCalls) != 0 {
		t.Fatalf("cycle call tally = run:%d suspend:%d resume:%d terminate:%d, want 1/1/1/0",
			mv.runCalls, len(mv.suspCalls), len(mv.resCalls), len(mv.termCalls))
	}
}

// lastFieldValue returns the most recent value written for a field on a bead
// across the fakeBeadStore's recorded UpdateBeadFields patches (later patches
// win), or "" if never written. Lets the E2E read the bead's converged state
// the way a re-mention's handler would read the persisted fields.
func lastFieldValue(store *fakeBeadStore, beadID, field string) string {
	var val string
	for _, patch := range store.updates[beadID] {
		if v, ok := patch[field]; ok {
			val = v
		}
	}
	return val
}
