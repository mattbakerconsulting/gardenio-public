package microvm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	mvtypes "github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	smtypes "github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
)

// fakeBeadStore records UpdateBeadFields / CloseBead calls for lifecycle tests.
type fakeBeadStore struct {
	updates   map[string][]map[string]string // beadID → sequence of field patches
	closed    map[string]map[string]string   // beadID → close fields
	updateErr error
	closeErr  error

	// runtimeIDs seeds RuntimeID's return per bead (empty/absent → not launched
	// yet). runtimeIDErr overrides it with an error so the bind-once guard's
	// fail-open path can be exercised. runtimeIDCalls counts the reads.
	runtimeIDs     map[string]string
	runtimeIDErr   error
	runtimeIDCalls int
}

func newFakeBeadStore() *fakeBeadStore {
	return &fakeBeadStore{updates: map[string][]map[string]string{}, closed: map[string]map[string]string{}}
}

func (f *fakeBeadStore) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.updates[beadID] = append(f.updates[beadID], fields)
	return nil
}

func (f *fakeBeadStore) RuntimeID(_ context.Context, beadID string) (string, error) {
	f.runtimeIDCalls++
	if f.runtimeIDErr != nil {
		return "", f.runtimeIDErr
	}
	return f.runtimeIDs[beadID], nil
}

func (f *fakeBeadStore) CloseBead(_ context.Context, beadID string, fields map[string]string) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	f.closed[beadID] = fields
	return nil
}

// lastUpdate returns the most recent field patch written for a bead.
func (f *fakeBeadStore) lastUpdate(beadID string) map[string]string {
	seq := f.updates[beadID]
	if len(seq) == 0 {
		return nil
	}
	return seq[len(seq)-1]
}

func mustLifecycle(t *testing.T, mv microvmAPI, sm secretsAPI, store BeadRuntimeStore) *Lifecycle {
	t.Helper()
	b := mustBackend(t, mv, sm, testBackendConfig())
	b.SetLauncherAccount("345678901234")
	return NewLifecycle(b, store, discardLogger())
}

// fakeRevoker records RevokeAgentCredentials calls for exit-path tests
// (ka-0000004762).
type fakeRevoker struct {
	calls []string // "<beadID>|<reason>"
}

func (f *fakeRevoker) RevokeAgentCredentials(_ context.Context, beadID, reason string) {
	f.calls = append(f.calls, beadID+"|"+reason)
}

// beads returns the bead IDs revoked, in order.
func (f *fakeRevoker) beads() []string {
	var out []string
	for _, c := range f.calls {
		out = append(out, c[:strings.Index(c, "|")])
	}
	return out
}

// --- IsMicrovmAgent / Available --------------------------------------------

func TestLifecycle_NilBackendIsInert(t *testing.T) {
	l := NewLifecycle(nil, newFakeBeadStore(), discardLogger())
	if l.Available() {
		t.Fatal("nil-backend lifecycle should be unavailable")
	}
	if l.IsMicrovmAgent(AgentBeadView{ExecutionTarget: ExecutionTargetLambdaMicroVM}) {
		t.Fatal("inert lifecycle must report no bead as a MicroVM agent")
	}
	// Reconcile on an inert lifecycle must be a no-op that reports Live (never reap).
	out, err := l.Reconcile(context.Background(), AgentBeadView{ID: "gd-x", ExecutionTarget: ExecutionTargetLambdaMicroVM})
	if err != nil || !out.Live || out.Closed {
		t.Fatalf("inert Reconcile = %+v, err=%v; want Live no-op", out, err)
	}
}

func TestLifecycle_IsMicrovmAgent(t *testing.T) {
	l := mustLifecycle(t, &fakeMicrovm{}, newFakeSecrets(), newFakeBeadStore())
	cases := []struct {
		target string
		want   bool
	}{
		{ExecutionTargetLambdaMicroVM, true},
		{"kubernetes", false},
		{"", false},
		{"  lambda_microvm  ", true}, // trimmed
	}
	for _, tc := range cases {
		if got := l.IsMicrovmAgent(AgentBeadView{ExecutionTarget: tc.target}); got != tc.want {
			t.Errorf("IsMicrovmAgent(%q) = %v, want %v", tc.target, got, tc.want)
		}
	}
}

// --- PersistLaunch ----------------------------------------------------------

func TestLifecycle_PersistLaunch(t *testing.T) {
	store := newFakeBeadStore()
	l := mustLifecycle(t, &fakeMicrovm{}, newFakeSecrets(), store)
	start := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	err := l.PersistLaunch(context.Background(), "gd-bead1", RuntimeHandle{
		BeadID:    "gd-bead1",
		MicrovmID: "mvm-1",
		StartedAt: start,
	})
	if err != nil {
		t.Fatalf("PersistLaunch: %v", err)
	}
	got := store.lastUpdate("gd-bead1")
	want := map[string]string{
		"execution_target":   ExecutionTargetLambdaMicroVM,
		"runtime_id":         "mvm-1",
		"runtime_state":      "PENDING",
		"runtime_region":     "us-east-2",
		"runtime_account":    "345678901234",
		"runtime_started_at": "2026-07-17T12:00:00Z",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("persisted %q = %q, want %q", k, got[k], v)
		}
	}
}

// --- Launch (backend + persist) --------------------------------------------

func TestLifecycle_Launch_PersistsMetadata(t *testing.T) {
	mv := &fakeMicrovm{runID: "mvm-42", runState: mvtypes.MicrovmStatePending}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	handle, err := l.Launch(context.Background(), AgentSpec{BeadID: "gd-bead2", Environment: map[string]string{"A": "1"}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if handle.MicrovmID != "mvm-42" {
		t.Fatalf("handle.MicrovmID = %q, want mvm-42", handle.MicrovmID)
	}
	if got := store.lastUpdate("gd-bead2")["runtime_id"]; got != "mvm-42" {
		t.Errorf("runtime_id persisted = %q, want mvm-42", got)
	}
}

func TestLifecycle_Launch_ErrorPersistsNothing(t *testing.T) {
	mv := &fakeMicrovm{runErr: errors.New("boom")}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	if _, err := l.Launch(context.Background(), AgentSpec{BeadID: "gd-bead3"}); err == nil {
		t.Fatal("Launch should return the backend error")
	}
	if len(store.updates["gd-bead3"]) != 0 {
		t.Errorf("no metadata should be persisted on launch failure, got %v", store.updates["gd-bead3"])
	}
}

// --- Bind-once identity guard (warm-pool step ka-0000006503) -----------------
//
// The warm pool reuses the SAME agent bead across prewarm→assign, so the per-run
// Ziti lease + Secrets Manager secrets minted at the first Launch must stay bound
// with NO re-mint. These tests pin that a re-emitted spawn for an already-launched
// bead issues exactly ZERO new leases and ZERO new secrets, while a genuine first
// launch (no persisted runtime_id) proceeds normally, and a runtime_id read error
// fails open.

// (mustPerRunLifecycle — a lifecycle over a per-run-identity backend so a test
// can count IssueLease/CreateSecret/RunMicrovm through the full Launch path — is
// defined in broker_test.go and reused here.)

// TestLifecycle_Launch_AlreadyLaunchedSkipsReMint is the core guard proof: a bead
// that already carries a persisted runtime_id is NOT re-launched — zero leases,
// zero secrets, zero RunMicrovm — and Launch returns the existing binding.
func TestLifecycle_Launch_AlreadyLaunchedSkipsReMint(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{runID: "mvm-existing"}
	iss := &fakeIssuer{}
	store := newFakeBeadStore()
	// The bead already launched a runtime (first Launch stamped it).
	store.runtimeIDs = map[string]string{"gd-warm": "mvm-existing"}
	l := mustPerRunLifecycle(t, mv, sm, iss, store)

	handle, err := l.Launch(context.Background(), AgentSpec{BeadID: "gd-warm"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// ZERO new identity leases minted.
	if len(iss.issued) != 0 {
		t.Errorf("issued leases = %v, want none (identity already bound to the bead)", iss.issued)
	}
	// ZERO new secrets written (identity + agent-config).
	if len(sm.created) != 0 || len(sm.putCalls) != 0 {
		t.Errorf("secret writes = created:%v put:%v, want none on re-launch", sm.created, sm.putCalls)
	}
	// ZERO RunMicrovm — the backend launch path was never entered.
	if mv.runCalls != 0 {
		t.Errorf("runCalls = %d, want 0 (backend.Launch must not run for an already-launched bead)", mv.runCalls)
	}
	// The returned handle carries the existing runtime binding.
	if handle.BeadID != "gd-warm" || handle.MicrovmID != "mvm-existing" {
		t.Errorf("handle = {BeadID:%q MicrovmID:%q}, want {gd-warm mvm-existing}", handle.BeadID, handle.MicrovmID)
	}
	// No runtime_state churn from a re-persist either.
	if len(store.updates["gd-warm"]) != 0 {
		t.Errorf("bead updates = %v, want none (no re-persist on skipped launch)", store.updates["gd-warm"])
	}
}

// TestLifecycle_Launch_FirstLaunchMintsOnce confirms the guard does not block a
// genuine first launch: a bead with no persisted runtime_id mints exactly one
// lease + its secrets and runs the VM.
func TestLifecycle_Launch_FirstLaunchMintsOnce(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{runID: "mvm-first"}
	iss := &fakeIssuer{}
	store := newFakeBeadStore() // no seeded runtime_id → not launched yet
	l := mustPerRunLifecycle(t, mv, sm, iss, store)

	handle, err := l.Launch(context.Background(), AgentSpec{BeadID: "gd-cold"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if len(iss.issued) != 1 || iss.issued[0] != "gd-cold" {
		t.Errorf("issued leases = %v, want exactly [gd-cold]", iss.issued)
	}
	if mv.runCalls != 1 {
		t.Errorf("runCalls = %d, want 1 (first launch must run the VM)", mv.runCalls)
	}
	// Both per-run secrets written.
	if _, ok := sm.created["ziti-microvm-agents/gd-cold/ziti-identity"]; !ok {
		t.Errorf("identity secret not created; created=%v", sm.created)
	}
	if _, ok := sm.created["ziti-microvm-agents/gd-cold/agent-config"]; !ok {
		t.Errorf("agent-config secret not created; created=%v", sm.created)
	}
	// runtime_id was persisted from the fresh launch.
	if got := store.lastUpdate("gd-cold")["runtime_id"]; got != "mvm-first" {
		t.Errorf("runtime_id persisted = %q, want mvm-first", got)
	}
	if handle.MicrovmID != "mvm-first" {
		t.Errorf("handle.MicrovmID = %q, want mvm-first", handle.MicrovmID)
	}
}

// TestLifecycle_Launch_RuntimeIDReadErrorFailsOpen: a transient runtime_id read
// error must not wedge a real launch — the guard falls through to backend.Launch
// (the bead ID is still the AWS ClientToken, and leak GC backstops any stray
// lease), so the VM still comes up.
func TestLifecycle_Launch_RuntimeIDReadErrorFailsOpen(t *testing.T) {
	sm := newFakeSecrets()
	mv := &fakeMicrovm{runID: "mvm-open"}
	iss := &fakeIssuer{}
	store := newFakeBeadStore()
	store.runtimeIDErr = errors.New("daemon unreachable")
	l := mustPerRunLifecycle(t, mv, sm, iss, store)

	handle, err := l.Launch(context.Background(), AgentSpec{BeadID: "gd-blip"})
	if err != nil {
		t.Fatalf("Launch should fail open on a read error, got %v", err)
	}
	if store.runtimeIDCalls != 1 {
		t.Errorf("runtimeID reads = %d, want 1", store.runtimeIDCalls)
	}
	if mv.runCalls != 1 || len(iss.issued) != 1 {
		t.Errorf("fail-open launch: runCalls=%d issued=%v, want 1 run + 1 lease", mv.runCalls, iss.issued)
	}
	if handle.MicrovmID != "mvm-open" {
		t.Errorf("handle.MicrovmID = %q, want mvm-open", handle.MicrovmID)
	}
}

// --- Reconcile --------------------------------------------------------------

func TestLifecycle_Reconcile_LiveLeavesOpen(t *testing.T) {
	mv := &fakeMicrovm{getState: mvtypes.MicrovmStateRunning}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	out, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-live", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-live", RuntimeState: "PENDING",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Live || out.Closed {
		t.Fatalf("running runtime should be Live and not Closed, got %+v", out)
	}
	if _, closed := store.closed["gd-live"]; closed {
		t.Error("a live MicroVM agent bead must not be closed")
	}
	// State changed PENDING→RUNNING so it should be persisted.
	if got := store.lastUpdate("gd-live")["runtime_state"]; got != "RUNNING" {
		t.Errorf("runtime_state persisted = %q, want RUNNING", got)
	}
}

// TestLifecycle_Reconcile_SuspendedLeavesStandbyOpen: the reconciler-side half of
// step 4's "standby != leak" (ka-0000006202). A SUSPENDED runtime is Live
// (statusFromState), so Reconcile must persist runtime_state=SUSPENDED and LEAVE
// THE BEAD OPEN — closing it would fire AgentDone → Terminate and destroy the
// very VM that step 3's resume-on-message targets. The bead sits at
// agent_state=stopping (frozen-awaiting-resume) throughout; Reconcile must not
// touch it.
func TestLifecycle_Reconcile_SuspendedLeavesStandbyOpen(t *testing.T) {
	for name, st := range map[string]mvtypes.MicrovmState{
		"suspended":  mvtypes.MicrovmStateSuspended,
		"suspending": mvtypes.MicrovmStateSuspending,
	} {
		t.Run(name, func(t *testing.T) {
			mv := &fakeMicrovm{getState: st}
			store := newFakeBeadStore()
			l := mustLifecycle(t, mv, newFakeSecrets(), store)
			out, err := l.Reconcile(context.Background(), AgentBeadView{
				ID: "gd-standby", ExecutionTarget: ExecutionTargetLambdaMicroVM,
				RuntimeID: "mvm-standby", RuntimeState: "RUNNING", AgentState: "stopping",
			})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if !out.Live || out.Closed {
				t.Fatalf("a %s runtime must be Live and not Closed, got %+v", st, out)
			}
			if _, closed := store.closed["gd-standby"]; closed {
				t.Error("a suspended standby bead must NOT be closed (it is the resume target)")
			}
			// The observed standby state is persisted for the operator/viewer.
			if got := store.lastUpdate("gd-standby")["runtime_state"]; got != string(st) {
				t.Errorf("runtime_state persisted = %q, want %s", got, st)
			}
			// Reconcile must not tear the VM down.
			if len(mv.termCalls) != 0 {
				t.Errorf("Reconcile must not terminate a suspended standby VM, got %v", mv.termCalls)
			}
		})
	}
}

func TestLifecycle_Reconcile_TerminalCloses(t *testing.T) {
	mv := &fakeMicrovm{getState: mvtypes.MicrovmStateTerminated}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	out, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-term", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-term", AgentState: "working",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Closed || out.Live {
		t.Fatalf("terminated runtime should close the bead, got %+v", out)
	}
	cf, closed := store.closed["gd-term"]
	if !closed {
		t.Fatal("terminated runtime should close the bead")
	}
	if cf["agent_state"] != "done" {
		t.Errorf("close agent_state = %q, want done (benign)", cf["agent_state"])
	}
}

func TestLifecycle_Reconcile_GoneCloses(t *testing.T) {
	// GetMicrovm ResourceNotFound → runtime gone → close.
	mv := &fakeMicrovm{getErr: &mvtypes.ResourceNotFoundException{}}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	out, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-gone", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-gone", AgentState: "working",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Closed {
		t.Fatalf("vanished runtime should close the bead, got %+v", out)
	}
}

func TestLifecycle_Reconcile_FailedPreservesFailedState(t *testing.T) {
	mv := &fakeMicrovm{getState: mvtypes.MicrovmStateTerminated}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	_, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-failed", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-f", AgentState: "failed",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := store.closed["gd-failed"]["agent_state"]; got != "failed" {
		t.Errorf("close agent_state = %q, want failed (preserved)", got)
	}
}

func TestLifecycle_Reconcile_NoRuntimeIDLeavesOpen(t *testing.T) {
	// A bead whose launch is still in flight (no runtime_id yet) must not be reaped.
	store := newFakeBeadStore()
	l := mustLifecycle(t, &fakeMicrovm{}, newFakeSecrets(), store)
	out, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-inflight", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "",
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !out.Live || out.Closed {
		t.Fatalf("no-runtime-id bead should be left alone, got %+v", out)
	}
}

func TestLifecycle_Reconcile_TransientErrorReturnsError(t *testing.T) {
	// A non-notfound GetMicrovm error is transient: the bead must NOT be closed;
	// the error propagates so the caller leaves it untouched this pass.
	mv := &fakeMicrovm{getErr: errors.New("throttled")}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	_, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-trans", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-t", AgentState: "working",
	})
	if err == nil {
		t.Fatal("transient status error should propagate")
	}
	if _, closed := store.closed["gd-trans"]; closed {
		t.Error("a bead must never be closed on an inconclusive status read")
	}
}

// --- Terminate --------------------------------------------------------------

func TestLifecycle_Terminate(t *testing.T) {
	mv := &fakeMicrovm{}
	sm := newFakeSecrets()
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, sm, store)
	err := l.Terminate(context.Background(), AgentBeadView{
		ID: "gd-stop", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-stop",
	}, "AgentStop")
	if err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if len(mv.termCalls) != 1 || mv.termCalls[0] != "mvm-stop" {
		t.Errorf("expected TerminateMicrovm(mvm-stop), got %v", mv.termCalls)
	}
	// The deterministic per-run secret must be revoked even without an ARN on the view.
	if len(sm.deleted) == 0 {
		t.Error("Terminate should delete the per-run agent-config secret")
	}
}

// --- Resume (step 3, plan ka-0000004518) -----------------------------------

// TestLifecycle_Resume: the happy path resumes the persisted runtime_id and
// flips the bead out of frozen-awaiting-thaw (runtime_state=PENDING,
// agent_state=working). It must NOT close the bead (a close would terminate the
// resumed VM) and must NOT touch per-run credentials.
func TestLifecycle_Resume(t *testing.T) {
	mv := &fakeMicrovm{}
	store := newFakeBeadStore()
	rev := &fakeRevoker{}
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	l.SetCredentialRevoker(rev)

	err := l.Resume(context.Background(), AgentBeadView{
		ID: "gd-resume", ExecutionTarget: ExecutionTargetLambdaMicroVM,
		RuntimeID: "mvm-resume", AgentState: "stopping", RuntimeState: "SUSPENDED",
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if len(mv.resCalls) != 1 || mv.resCalls[0] != "mvm-resume" {
		t.Errorf("expected ResumeMicrovm(mvm-resume), got %v", mv.resCalls)
	}
	// The bead must flip back to a working shape.
	last := store.lastUpdate("gd-resume")
	if last["agent_state"] != "working" {
		t.Errorf("agent_state after resume = %q, want working", last["agent_state"])
	}
	if last["runtime_state"] != "PENDING" {
		t.Errorf("runtime_state after resume = %q, want PENDING", last["runtime_state"])
	}
	// Resume must clear the standby-age anchor — the VM is no longer suspended,
	// so a future max-standby-age sweep must not reap it off a stale timestamp.
	if got, ok := last["runtime_suspended_at"]; !ok || got != "" {
		t.Errorf("runtime_suspended_at after resume = %q (present=%v), want cleared to \"\"", got, ok)
	}
	// A resume must never close the bead (that would terminate the VM).
	if _, closed := store.closed["gd-resume"]; closed {
		t.Error("Resume must not close the bead")
	}
	// A resume must never revoke per-run credentials (they survive standby).
	if got := rev.beads(); len(got) != 0 {
		t.Errorf("Resume must not revoke credentials, got %v", got)
	}
}

// TestLifecycle_Resume_Idempotent: a gone (ResourceNotFound) or wrong-state
// (Conflict) VM is a benign no-op success — a duplicate message / reconciler
// race must not surface as an error.
func TestLifecycle_Resume_Idempotent(t *testing.T) {
	for name, resErr := range map[string]error{
		"not-found":   &mvtypes.ResourceNotFoundException{Message: aws.String("gone")},
		"wrong-state": &mvtypes.ConflictException{Message: aws.String("already running")},
	} {
		t.Run(name, func(t *testing.T) {
			store := newFakeBeadStore()
			l := mustLifecycle(t, &fakeMicrovm{resErr: resErr}, newFakeSecrets(), store)
			if err := l.Resume(context.Background(), AgentBeadView{
				ID: "gd-idem", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-idem",
			}); err != nil {
				t.Fatalf("Resume(%s) should be a benign no-op, got %v", name, err)
			}
			// Even on the benign no-op the bead is flipped to working — the VM is
			// (already) up, so the agent should not stay stuck at stopping.
			if got := store.lastUpdate("gd-idem")["agent_state"]; got != "working" {
				t.Errorf("agent_state = %q, want working", got)
			}
		})
	}
}

// TestLifecycle_Resume_RealErrorPropagates: a non-benign AWS error propagates so
// the caller (HTTP handler) can 500 and the bridge can fall back / retry.
func TestLifecycle_Resume_RealErrorPropagates(t *testing.T) {
	l := mustLifecycle(t, &fakeMicrovm{resErr: errors.New("throttled")}, newFakeSecrets(), newFakeBeadStore())
	if err := l.Resume(context.Background(), AgentBeadView{
		ID: "gd-err", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-err",
	}); err == nil {
		t.Fatal("Resume should propagate a non-benign AWS error")
	}
}

// TestLifecycle_Resume_NilBackendIsNoop: an unavailable coordinator (backend off)
// is a silent no-op success, so the wiring is safe to call unconditionally.
func TestLifecycle_Resume_NilBackendIsNoop(t *testing.T) {
	l := NewLifecycle(nil, newFakeBeadStore(), discardLogger())
	if err := l.Resume(context.Background(), AgentBeadView{ID: "gd-x", RuntimeID: "mvm-x"}); err != nil {
		t.Fatalf("Resume on unavailable lifecycle should be a no-op, got %v", err)
	}
}

// --- Suspend (native suspend-on-stop, ka-0000007189) ------------------------

// TestLifecycle_Suspend: a graceful stop of a freeze-eligible MicroVM thread
// agent SUSPENDS the VM (SuspendMicrovm, not TerminateMicrovm), stamps
// runtime_state=SUSPENDED on the still-open bead, and does NOT close it — the
// bead is the resume target for step 3.
func TestLifecycle_Suspend(t *testing.T) {
	mv := &fakeMicrovm{}
	sm := newFakeSecrets()
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, sm, store)
	err := l.Suspend(context.Background(), AgentBeadView{
		ID: "gd-susp", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-susp",
	}, "agent_stop")
	if err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(mv.suspCalls) != 1 || mv.suspCalls[0] != "mvm-susp" {
		t.Errorf("expected SuspendMicrovm(mvm-susp), got %v", mv.suspCalls)
	}
	// Suspend must NOT terminate the VM — it is the resume target.
	if len(mv.termCalls) != 0 {
		t.Errorf("Suspend must not terminate the VM, got termCalls=%v", mv.termCalls)
	}
	// The bead must be stamped SUSPENDED and NOT closed.
	last := store.lastUpdate("gd-susp")
	if got := last["runtime_state"]; got != "SUSPENDED" {
		t.Errorf("runtime_state stamped = %q, want SUSPENDED", got)
	}
	// runtime_suspended_at is the standby-age anchor the max-standby-age cap
	// measures from (step 4); it must be stamped as a parseable RFC3339 time.
	if got := last["runtime_suspended_at"]; got == "" {
		t.Error("Suspend must stamp runtime_suspended_at (standby-age anchor)")
	} else if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("runtime_suspended_at = %q not RFC3339: %v", got, err)
	}
	if _, closed := store.closed["gd-susp"]; closed {
		t.Error("a suspended MicroVM agent bead must stay open (not closed)")
	}
	// Per-run secrets must SURVIVE standby — Suspend deletes nothing.
	if len(sm.deleted) != 0 {
		t.Errorf("Suspend must not delete per-run secrets (they survive standby), got %v", sm.deleted)
	}
}

// TestLifecycle_Suspend_DoesNotRevokeCredentials: per-run credentials + Ziti
// leases MUST outlive the standby window so the resumed VM stays authenticated
// (12h session creds backstop). Suspend, unlike Terminate, must NOT revoke.
func TestLifecycle_Suspend_DoesNotRevokeCredentials(t *testing.T) {
	rev := &fakeRevoker{}
	l := mustLifecycle(t, &fakeMicrovm{}, newFakeSecrets(), newFakeBeadStore())
	l.SetCredentialRevoker(rev)
	if err := l.Suspend(context.Background(), AgentBeadView{
		ID: "gd-susp2", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-susp2",
	}, "agent_stop"); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if len(rev.calls) != 0 {
		t.Fatalf("Suspend must NOT revoke credentials (they survive standby), got %v", rev.calls)
	}
}

// TestLifecycle_Suspend_Idempotent: a SuspendMicrovm on a gone/wrong-state VM is
// a benign no-op success (backend.Suspend tolerates NotFound/Conflict), so a
// duplicate AgentStop or a race with the reconciler never surfaces as a failure.
func TestLifecycle_Suspend_Idempotent(t *testing.T) {
	mv := &fakeMicrovm{suspErr: &mvtypes.ConflictException{}}
	l := mustLifecycle(t, mv, newFakeSecrets(), newFakeBeadStore())
	if err := l.Suspend(context.Background(), AgentBeadView{
		ID: "gd-susp3", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-susp3",
	}, "agent_stop"); err != nil {
		t.Fatalf("Suspend on wrong-state VM should be a no-op success, got %v", err)
	}
}

// TestLifecycle_Suspend_PropagatesError: a genuine SuspendMicrovm error (not the
// NotFound/Conflict family) propagates so the teardown caller can fall back to
// Terminate rather than leaking a half-suspended runtime with an open bead.
func TestLifecycle_Suspend_PropagatesError(t *testing.T) {
	mv := &fakeMicrovm{suspErr: errors.New("throttled")}
	l := mustLifecycle(t, mv, newFakeSecrets(), newFakeBeadStore())
	if err := l.Suspend(context.Background(), AgentBeadView{
		ID: "gd-susp4", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-susp4",
	}, "agent_stop"); err == nil {
		t.Fatal("a genuine SuspendMicrovm error should propagate")
	}
}

// TestLifecycle_Suspend_InertWhenUnavailable: a nil-backend (default-off)
// coordinator's Suspend is a no-op success, mirroring Terminate.
func TestLifecycle_Suspend_InertWhenUnavailable(t *testing.T) {
	l := NewLifecycle(nil, newFakeBeadStore(), discardLogger())
	if err := l.Suspend(context.Background(), AgentBeadView{ID: "gd-x", RuntimeID: "mvm-x"}, "agent_stop"); err != nil {
		t.Fatalf("inert Suspend should be a no-op success, got %v", err)
	}
}

// --- Credential revocation on exit paths (ka-0000004762) --------------------

// TestLifecycle_Terminate_RevokesCredentials: the NORMAL-TERMINATION exit path
// must revoke per-run credentials, and must do so even when the AWS teardown
// itself fails (the leak GC reaps the runtime later; the credentials must not
// outlive either outcome).
func TestLifecycle_Terminate_RevokesCredentials(t *testing.T) {
	rev := &fakeRevoker{}
	l := mustLifecycle(t, &fakeMicrovm{}, newFakeSecrets(), newFakeBeadStore())
	l.SetCredentialRevoker(rev)
	if err := l.Terminate(context.Background(), AgentBeadView{
		ID: "gd-stop", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-stop",
	}, "AgentStop"); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if got := rev.beads(); len(got) != 1 || got[0] != "gd-stop" {
		t.Fatalf("revoked beads = %v, want [gd-stop]", got)
	}

	// Teardown failure still revokes.
	rev2 := &fakeRevoker{}
	l2 := mustLifecycle(t, &fakeMicrovm{termErr: errors.New("throttled")}, newFakeSecrets(), newFakeBeadStore())
	l2.SetCredentialRevoker(rev2)
	if err := l2.Terminate(context.Background(), AgentBeadView{
		ID: "gd-stuck", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-stuck",
	}, "AgentKill"); err == nil {
		t.Fatal("Terminate should propagate the teardown error")
	}
	if got := rev2.beads(); len(got) != 1 || got[0] != "gd-stuck" {
		t.Fatalf("revoked beads on failed teardown = %v, want [gd-stuck]", got)
	}
}

// TestLifecycle_Reconcile_TerminalRevokesCredentials: the TIMEOUT exit path (a
// runtime that hit MaximumDurationInSeconds or otherwise self-terminated) is
// only ever observed by Reconcile — it must revoke before closing the bead. A
// live runtime must NOT trigger revocation.
func TestLifecycle_Reconcile_TerminalRevokesCredentials(t *testing.T) {
	rev := &fakeRevoker{}
	l := mustLifecycle(t, &fakeMicrovm{getState: mvtypes.MicrovmStateTerminated}, newFakeSecrets(), newFakeBeadStore())
	l.SetCredentialRevoker(rev)
	out, err := l.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-timeout", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-to", AgentState: "working",
	})
	if err != nil || !out.Closed {
		t.Fatalf("Reconcile = %+v, err=%v; want Closed", out, err)
	}
	if got := rev.beads(); len(got) != 1 || got[0] != "gd-timeout" {
		t.Fatalf("revoked beads = %v, want [gd-timeout]", got)
	}

	rev2 := &fakeRevoker{}
	l2 := mustLifecycle(t, &fakeMicrovm{getState: mvtypes.MicrovmStateRunning}, newFakeSecrets(), newFakeBeadStore())
	l2.SetCredentialRevoker(rev2)
	if _, err := l2.Reconcile(context.Background(), AgentBeadView{
		ID: "gd-alive", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-a",
	}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(rev2.calls) != 0 {
		t.Fatalf("live runtime must not revoke credentials, got %v", rev2.calls)
	}
}

// TestLifecycle_GC_RevokesLeakedSecretCredentials: the LEAK-GC exit path must
// revoke credentials for the owning bead of every leaked per-run secret it
// deletes — and only those (live/young/untagged secrets don't revoke).
func TestLifecycle_GC_RevokesLeakedSecretCredentials(t *testing.T) {
	old := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	now := old.Add(time.Hour)
	sm := newFakeSecrets()
	sm.listSecrets = []smtypes.SecretListEntry{
		{Name: aws.String("ziti-microvm-agents/gd-live/agent-config"), ARN: aws.String("arn:live"), CreatedDate: aws.Time(old),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-live")}}},
		{Name: aws.String("ziti-microvm-agents/gd-leak/agent-config"), ARN: aws.String("arn:leak"), CreatedDate: aws.Time(old),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-leak")}}},
	}
	rev := &fakeRevoker{}
	l := mustLifecycle(t, &fakeMicrovm{}, sm, newFakeBeadStore())
	l.SetCredentialRevoker(rev)
	if _, err := l.GC(context.Background(), map[string]bool{}, map[string]bool{"gd-live": true}, nil, now); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if got := rev.beads(); len(got) != 1 || got[0] != "gd-leak" {
		t.Fatalf("revoked beads = %v, want [gd-leak]", got)
	}
}

// TestLifecycle_RevokeCredentials_NilSafe: the spawn door calls
// RevokeCredentials directly on the FAILED-LAUNCH exit path; with no revoker
// installed it must be a no-op, never a panic.
func TestLifecycle_RevokeCredentials_NilSafe(t *testing.T) {
	l := mustLifecycle(t, &fakeMicrovm{}, newFakeSecrets(), newFakeBeadStore())
	l.RevokeCredentials(context.Background(), "gd-none", "failed launch") // no revoker installed
	rev := &fakeRevoker{}
	l.SetCredentialRevoker(rev)
	l.RevokeCredentials(context.Background(), "gd-some", "failed launch")
	if got := rev.beads(); len(got) != 1 || got[0] != "gd-some" {
		t.Fatalf("revoked beads = %v, want [gd-some]", got)
	}
}

// --- GC ---------------------------------------------------------------------

func TestLifecycle_GC_ReapsLeakedRuntimeAndSecret(t *testing.T) {
	old := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	now := old.Add(time.Hour) // well past gcMinAge
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-live"), State: mvtypes.MicrovmStateRunning, StartedAt: aws.Time(old)},
		{MicrovmId: aws.String("mvm-leak"), State: mvtypes.MicrovmStateRunning, StartedAt: aws.Time(old)},
	}}
	sm := newFakeSecrets()
	sm.listSecrets = []smtypes.SecretListEntry{
		{Name: aws.String("ziti-microvm-agents/gd-live/agent-config"), ARN: aws.String("arn:live"), CreatedDate: aws.Time(old),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-live")}}},
		{Name: aws.String("ziti-microvm-agents/gd-leak/agent-config"), ARN: aws.String("arn:leak"), CreatedDate: aws.Time(old),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-leak")}}},
	}
	l := mustLifecycle(t, mv, sm, newFakeBeadStore())

	liveRuntimeIDs := map[string]bool{"mvm-live": true}
	liveBeadIDs := map[string]bool{"gd-live": true}
	stats, err := l.GC(context.Background(), liveRuntimeIDs, liveBeadIDs, nil, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.RuntimesReaped != 1 {
		t.Errorf("RuntimesReaped = %d, want 1", stats.RuntimesReaped)
	}
	if len(mv.termCalls) != 1 || mv.termCalls[0] != "mvm-leak" {
		t.Errorf("GC should terminate only mvm-leak, got %v", mv.termCalls)
	}
	if stats.SecretsDeleted != 1 {
		t.Errorf("SecretsDeleted = %d, want 1", stats.SecretsDeleted)
	}
	if len(sm.deleted) != 1 || sm.deleted[0] != "arn:leak" {
		t.Errorf("GC should delete only arn:leak, got %v", sm.deleted)
	}
}

func TestLifecycle_GC_SkipsYoungResources(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	young := now.Add(-1 * time.Minute) // younger than gcMinAge
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-young"), State: mvtypes.MicrovmStateRunning, StartedAt: aws.Time(young)},
	}}
	sm := newFakeSecrets()
	sm.listSecrets = []smtypes.SecretListEntry{
		{Name: aws.String("ziti-microvm-agents/gd-young/agent-config"), ARN: aws.String("arn:young"), CreatedDate: aws.Time(young),
			Tags: []smtypes.Tag{{Key: aws.String(tagResourceBead), Value: aws.String("gd-young")}}},
	}
	l := mustLifecycle(t, mv, sm, newFakeBeadStore())
	stats, err := l.GC(context.Background(), map[string]bool{}, map[string]bool{}, nil, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.RuntimesReaped != 0 || len(mv.termCalls) != 0 {
		t.Errorf("young runtime should not be reaped: reaped=%d calls=%v", stats.RuntimesReaped, mv.termCalls)
	}
	if stats.SecretsDeleted != 0 || len(sm.deleted) != 0 {
		t.Errorf("young secret should not be deleted: deleted=%d calls=%v", stats.SecretsDeleted, sm.deleted)
	}
}

func TestLifecycle_GC_SkipsTerminatingRuntime(t *testing.T) {
	old := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	now := old.Add(time.Hour)
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-dying"), State: mvtypes.MicrovmStateTerminating, StartedAt: aws.Time(old)},
	}}
	l := mustLifecycle(t, mv, newFakeSecrets(), newFakeBeadStore())
	stats, err := l.GC(context.Background(), map[string]bool{}, map[string]bool{}, nil, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.RuntimesReaped != 0 || len(mv.termCalls) != 0 {
		t.Errorf("terminating runtime should not be re-terminated: %v", mv.termCalls)
	}
}

func TestLifecycle_GC_SkipsUntaggedSecret(t *testing.T) {
	old := time.Date(2026, 7, 17, 0, 0, 0, 0, time.UTC)
	now := old.Add(time.Hour)
	sm := newFakeSecrets()
	sm.listSecrets = []smtypes.SecretListEntry{
		{Name: aws.String("ziti-microvm-agents/hand-made"), ARN: aws.String("arn:untagged"), CreatedDate: aws.Time(old)},
	}
	l := mustLifecycle(t, &fakeMicrovm{}, sm, newFakeBeadStore())
	stats, err := l.GC(context.Background(), map[string]bool{}, map[string]bool{}, nil, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.SecretsDeleted != 0 || len(sm.deleted) != 0 {
		t.Errorf("untagged secret must not be deleted: %v", sm.deleted)
	}
}

func TestLifecycle_GC_ListErrorReapsNothing(t *testing.T) {
	mv := &fakeMicrovm{listErr: errors.New("throttled")}
	l := mustLifecycle(t, mv, newFakeSecrets(), newFakeBeadStore())
	_, err := l.GC(context.Background(), map[string]bool{}, map[string]bool{}, nil, time.Now())
	if err == nil {
		t.Fatal("GC should return the enumeration error")
	}
	if len(mv.termCalls) != 0 {
		t.Errorf("GC must reap nothing on a list error, got %v", mv.termCalls)
	}
}

// --- Max-standby-age cap (step 4, ka-0000006202) ----------------------------

// TestLifecycle_GC_Standby_WithinCapLeftAlone: a SUSPENDED thread-agent VM
// claimed by an open bead is intentional standby — GC must NOT terminate it or
// close its bead while it is still within the max-standby-age cap. This is the
// core "standby != leak" guarantee: the VM appears in liveRuntimeIDs (so the
// leak loop skips it) AND its standby age is under the cap.
func TestLifecycle_GC_Standby_WithinCapLeftAlone(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	suspendedAt := now.Add(-1 * time.Hour) // 1h of standby
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		// SUSPENDED VM is Live (statusFromState), so the leak loop's !Live skip
		// does not apply — only the liveRuntimeIDs claim keeps it safe.
		{MicrovmId: aws.String("mvm-standby"), State: mvtypes.MicrovmStateSuspended, StartedAt: aws.Time(now.Add(-3 * time.Hour))},
	}}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	l.SetMaxStandbyAge(24 * time.Hour)

	standby := []StandbyCandidate{{
		BeadID: "gd-standby", RuntimeID: "mvm-standby", AgentState: "stopping",
		RuntimeSuspendedAt: suspendedAt.Format(time.RFC3339),
	}}
	stats, err := l.GC(context.Background(),
		map[string]bool{"mvm-standby": true}, map[string]bool{"gd-standby": true}, standby, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.StandbyReaped != 0 || len(mv.termCalls) != 0 {
		t.Errorf("within-cap standby must be left alone: reaped=%d term=%v", stats.StandbyReaped, mv.termCalls)
	}
	if _, closed := store.closed["gd-standby"]; closed {
		t.Error("within-cap standby bead must stay open")
	}
}

// TestLifecycle_GC_Standby_OverCapReapedAndClosed: a SUSPENDED thread-agent VM
// whose standby exceeds the cap is terminated and its bead closed as done — the
// belt-and-suspenders that stops a never-resumed agent from billing forever.
func TestLifecycle_GC_Standby_OverCapReapedAndClosed(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	suspendedAt := now.Add(-30 * time.Hour) // past a 24h cap
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-old"), State: mvtypes.MicrovmStateSuspended, StartedAt: aws.Time(now.Add(-40 * time.Hour))},
	}}
	rev := &fakeRevoker{}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	l.SetMaxStandbyAge(24 * time.Hour)
	l.SetCredentialRevoker(rev)

	standby := []StandbyCandidate{{
		BeadID: "gd-old", RuntimeID: "mvm-old", AgentState: "stopping",
		RuntimeSuspendedAt: suspendedAt.Format(time.RFC3339),
	}}
	stats, err := l.GC(context.Background(),
		map[string]bool{"mvm-old": true}, map[string]bool{"gd-old": true}, standby, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.StandbyReaped != 1 {
		t.Errorf("StandbyReaped = %d, want 1", stats.StandbyReaped)
	}
	// Full teardown: the VM must be terminated (this run is over — no resume came).
	if len(mv.termCalls) != 1 || mv.termCalls[0] != "mvm-old" {
		t.Errorf("over-cap standby should terminate mvm-old, got %v", mv.termCalls)
	}
	// The resume-target bead is closed as done (benign standby timeout).
	cf, closed := store.closed["gd-old"]
	if !closed {
		t.Fatal("over-cap standby bead must be closed")
	}
	if cf["agent_state"] != "done" {
		t.Errorf("close agent_state = %q, want done", cf["agent_state"])
	}
	if cf["runtime_state"] != "TERMINATED" {
		t.Errorf("close runtime_state = %q, want TERMINATED", cf["runtime_state"])
	}
	// Per-run credentials die with the terminated VM.
	if got := rev.beads(); len(got) != 1 || got[0] != "gd-old" {
		t.Errorf("over-cap standby should revoke credentials for gd-old, got %v", got)
	}
}

// TestLifecycle_GC_Standby_CapDisabledLeavesAlone: with the cap disabled (<=0),
// even a long-suspended standby VM is left to the AWS IdlePolicy backstop.
func TestLifecycle_GC_Standby_CapDisabledLeavesAlone(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-x"), State: mvtypes.MicrovmStateSuspended, StartedAt: aws.Time(now.Add(-100 * time.Hour))},
	}}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	// No SetMaxStandbyAge → cap disabled (0).

	standby := []StandbyCandidate{{
		BeadID: "gd-x", RuntimeID: "mvm-x", AgentState: "stopping",
		RuntimeSuspendedAt: now.Add(-99 * time.Hour).Format(time.RFC3339),
	}}
	stats, err := l.GC(context.Background(),
		map[string]bool{"mvm-x": true}, map[string]bool{"gd-x": true}, standby, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if stats.StandbyReaped != 0 || len(mv.termCalls) != 0 || len(store.closed) != 0 {
		t.Errorf("disabled cap must never reap standby: reaped=%d term=%v closed=%v",
			stats.StandbyReaped, mv.termCalls, store.closed)
	}
}

// TestLifecycle_GC_Standby_NoAnchorLeftAlone: a standby candidate with no (or an
// unparseable) runtime_suspended_at cannot be aged, so GC leaves it to the AWS
// backstop rather than reaping it on a guess — even when the cap is enabled.
func TestLifecycle_GC_Standby_NoAnchorLeftAlone(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-noanchor"), State: mvtypes.MicrovmStateSuspended, StartedAt: aws.Time(now.Add(-50 * time.Hour))},
	}}
	l := mustLifecycle(t, mv, newFakeSecrets(), newFakeBeadStore())
	l.SetMaxStandbyAge(1 * time.Hour)

	for _, anchor := range []string{"", "not-a-timestamp"} {
		mv.termCalls = nil
		standby := []StandbyCandidate{{
			BeadID: "gd-noanchor", RuntimeID: "mvm-noanchor", AgentState: "stopping",
			RuntimeSuspendedAt: anchor,
		}}
		stats, err := l.GC(context.Background(),
			map[string]bool{"mvm-noanchor": true}, map[string]bool{"gd-noanchor": true}, standby, now)
		if err != nil {
			t.Fatalf("GC(anchor=%q): %v", anchor, err)
		}
		if stats.StandbyReaped != 0 || len(mv.termCalls) != 0 {
			t.Errorf("anchor=%q: unparseable standby anchor must be left alone, got reaped=%d term=%v",
				anchor, stats.StandbyReaped, mv.termCalls)
		}
	}
}

// TestLifecycle_GC_Standby_OverCapPreservesFailedState: a standby VM whose prior
// agent_state was already failed closes as failed, not done — microvmCloseState
// discipline (a crashed run that happened to be suspended stays a crash).
func TestLifecycle_GC_Standby_OverCapPreservesFailedState(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-failed"), State: mvtypes.MicrovmStateSuspended, StartedAt: aws.Time(now.Add(-40 * time.Hour))},
	}}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	l.SetMaxStandbyAge(24 * time.Hour)

	standby := []StandbyCandidate{{
		BeadID: "gd-failed", RuntimeID: "mvm-failed", AgentState: "failed",
		RuntimeSuspendedAt: now.Add(-30 * time.Hour).Format(time.RFC3339),
	}}
	if _, err := l.GC(context.Background(),
		map[string]bool{"mvm-failed": true}, map[string]bool{"gd-failed": true}, standby, now); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if cf := store.closed["gd-failed"]; cf["agent_state"] != "failed" {
		t.Errorf("close agent_state = %q, want failed (preserved)", cf["agent_state"])
	}
}

// TestLifecycle_GC_Standby_LeakAndStandbyCoexist: the leak loop and the standby
// cap must not interfere — a running-orphan (no live bead) is still reaped as a
// leak, while a within-cap suspended standby VM is left untouched, in one sweep.
func TestLifecycle_GC_Standby_LeakAndStandbyCoexist(t *testing.T) {
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * time.Hour) // past gcMinAge
	mv := &fakeMicrovm{listItems: []mvtypes.MicrovmItem{
		{MicrovmId: aws.String("mvm-orphan"), State: mvtypes.MicrovmStateRunning, StartedAt: aws.Time(old)},
		{MicrovmId: aws.String("mvm-standby"), State: mvtypes.MicrovmStateSuspended, StartedAt: aws.Time(old)},
	}}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	l.SetMaxStandbyAge(24 * time.Hour)

	standby := []StandbyCandidate{{
		BeadID: "gd-standby", RuntimeID: "mvm-standby", AgentState: "stopping",
		RuntimeSuspendedAt: now.Add(-1 * time.Hour).Format(time.RFC3339), // within cap
	}}
	stats, err := l.GC(context.Background(),
		map[string]bool{"mvm-standby": true}, map[string]bool{"gd-standby": true}, standby, now)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	// The unclaimed running VM is a leak → reaped.
	if stats.RuntimesReaped != 1 || len(mv.termCalls) != 1 || mv.termCalls[0] != "mvm-orphan" {
		t.Errorf("running orphan should be reaped as a leak, got reaped=%d term=%v", stats.RuntimesReaped, mv.termCalls)
	}
	// The within-cap standby VM is left alone.
	if stats.StandbyReaped != 0 {
		t.Errorf("within-cap standby must survive, StandbyReaped=%d", stats.StandbyReaped)
	}
	if _, closed := store.closed["gd-standby"]; closed {
		t.Error("within-cap standby bead must stay open")
	}
}
