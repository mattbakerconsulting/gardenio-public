package microvm

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeProber is a LivenessProber whose verdict the test controls. It records the
// agent names probed and can flip from "not live" to "live" after N reads
// (modeling a resumed VM whose heartbeat lands mid-window), or return an error
// (modeling a transient roster read failure).
type fakeProber struct {
	mu         sync.Mutex
	liveAfter  int          // become live after this many reads (0 = live from the first read)
	reads      atomic.Int32 // total ProvenLiveSince calls
	err        error        // when set, every read errors (never proves live)
	neverLive  bool         // when true, always report not-live (models a dead lease)
	sinceSeen  time.Time
	agentNames []string
}

func (p *fakeProber) ProvenLiveSince(_ context.Context, agentName string, since time.Time) (bool, error) {
	n := p.reads.Add(1)
	p.mu.Lock()
	p.agentNames = append(p.agentNames, agentName)
	p.sinceSeen = since
	p.mu.Unlock()
	if p.err != nil {
		return false, p.err
	}
	if p.neverLive {
		return false, nil
	}
	return int(n) > p.liveAfter, nil
}

func (p *fakeProber) readCount() int { return int(p.reads.Load()) }

// newRehydrateLifecycle builds a lifecycle with the rehydration gate on, a fast
// deadline/poll for the test, and the supplied prober.
func newRehydrateLifecycle(t *testing.T, mv microvmAPI, store BeadRuntimeStore, prober LivenessProber, deadline time.Duration) *Lifecycle {
	t.Helper()
	l := mustLifecycle(t, mv, newFakeSecrets(), store)
	l.SetResumeRehydration(true, deadline)
	l.SetLivenessProber(prober)
	return l
}

// --- Resume stamps `resuming` (gated) vs `working` (rollback) --------------

// TestResume_GatedStampsResuming: with the rehydration gate ON, Resume must
// stamp agent_state=resuming (NOT working) — the agent is not live until
// proof-of-life. This is the whole point of the correctness boundary.
func TestResume_GatedStampsResuming(t *testing.T) {
	store := newFakeBeadStore()
	l := newRehydrateLifecycle(t, &fakeMicrovm{}, store, &fakeProber{}, time.Second)
	if err := l.Resume(context.Background(), AgentBeadView{
		ID: "gd-g", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-g", AgentState: "stopping",
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	last := store.lastUpdate("gd-g")
	if last["agent_state"] != AgentStateResuming {
		t.Errorf("gated Resume agent_state = %q, want %q", last["agent_state"], AgentStateResuming)
	}
	if last["runtime_state"] != "PENDING" {
		t.Errorf("runtime_state = %q, want PENDING", last["runtime_state"])
	}
}

// TestResume_GateOffStampsWorking: with the gate OFF (rollback), Resume keeps
// the step-3 behavior — stamp working immediately, no proof-of-life gate.
func TestResume_GateOffStampsWorking(t *testing.T) {
	store := newFakeBeadStore()
	l := mustLifecycle(t, &fakeMicrovm{}, newFakeSecrets(), store) // gate defaults off
	if l.ResumeRehydrateEnabled() {
		t.Fatal("gate should default off when SetResumeRehydration is never called")
	}
	if err := l.Resume(context.Background(), AgentBeadView{
		ID: "gd-off", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-off",
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := store.lastUpdate("gd-off")["agent_state"]; got != AgentStateWorking {
		t.Errorf("gate-off Resume agent_state = %q, want %q", got, AgentStateWorking)
	}
}

// --- AwaitRehydration release path (proof-of-life within deadline) ----------

// TestAwaitRehydration_ProofReleases: when proof-of-life arrives within the
// deadline, the agent is marked working — the release path.
func TestAwaitRehydration_ProofReleases(t *testing.T) {
	store := newFakeBeadStore()
	// live from the second read (models a heartbeat landing shortly after resume).
	prober := &fakeProber{liveAfter: 1}
	l := newRehydrateLifecycle(t, &fakeMicrovm{}, store, prober, 5*time.Second)

	view := AgentBeadView{ID: "gd-rel", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-rel"}
	out := l.AwaitRehydration(context.Background(), view, "thread-1-2", time.Now())
	if !out.Released {
		t.Fatalf("expected released, got %+v", out)
	}
	last := store.lastUpdate("gd-rel")
	if last["agent_state"] != AgentStateWorking {
		t.Errorf("released agent_state = %q, want %q", last["agent_state"], AgentStateWorking)
	}
	if last["runtime_state"] != "RUNNING" {
		t.Errorf("released runtime_state = %q, want RUNNING", last["runtime_state"])
	}
	if prober.readCount() < 1 {
		t.Error("prober should have been consulted")
	}
	// The proof-of-life gate must NEVER be the one to close/terminate on release.
	if _, closed := store.closed["gd-rel"]; closed {
		t.Error("release path must not close the bead")
	}
}

// TestAwaitRehydration_ProbesTheAgentName: the prober is called with the agent
// NAME (the presence-tracker actor key), not the bead ID.
func TestAwaitRehydration_ProbesTheAgentName(t *testing.T) {
	prober := &fakeProber{} // live from the first read
	l := newRehydrateLifecycle(t, &fakeMicrovm{}, newFakeBeadStore(), prober, 5*time.Second)
	_ = l.AwaitRehydration(context.Background(), AgentBeadView{ID: "gd-name", RuntimeID: "mvm-n"}, "thread-42-99", time.Now())
	prober.mu.Lock()
	defer prober.mu.Unlock()
	if len(prober.agentNames) == 0 || prober.agentNames[0] != "thread-42-99" {
		t.Errorf("prober probed %v, want first = thread-42-99 (the agent name, not bead ID)", prober.agentNames)
	}
}

// --- AwaitRehydration fail-closed path (no proof within deadline) -----------

// TestAwaitRehydration_TimeoutFailsClosed: when proof-of-life never arrives, the
// rendezvous FAILS CLOSED — it terminates the un-rehydratable VM and stamps
// resume_failed, so a half-rehydrated VM never serves the user (M2). Models the
// dead-Ziti-lease / failed-boot case.
func TestAwaitRehydration_TimeoutFailsClosed(t *testing.T) {
	mv := &fakeMicrovm{}
	store := newFakeBeadStore()
	prober := &fakeProber{neverLive: true} // never proves life (e.g. revoked lease)
	// Tiny deadline so the test fails closed quickly.
	l := newRehydrateLifecycle(t, mv, store, prober, 50*time.Millisecond)

	view := AgentBeadView{ID: "gd-fc", ExecutionTarget: ExecutionTargetLambdaMicroVM, RuntimeID: "mvm-fc"}
	out := l.AwaitRehydration(context.Background(), view, "thread-fc", time.Now())
	if out.Released {
		t.Fatalf("expected fail-closed (not released), got %+v", out)
	}
	// Fail-closed MUST terminate the VM.
	if len(mv.termCalls) != 1 || mv.termCalls[0] != "mvm-fc" {
		t.Errorf("fail-closed should TerminateMicrovm(mvm-fc), got %v", mv.termCalls)
	}
	// ...and stamp resume_failed.
	last := store.lastUpdate("gd-fc")
	if last["agent_state"] != AgentStateResumeFailed {
		t.Errorf("fail-closed agent_state = %q, want %q", last["agent_state"], AgentStateResumeFailed)
	}
	if last["runtime_state"] != "TERMINATED" {
		t.Errorf("fail-closed runtime_state = %q, want TERMINATED", last["runtime_state"])
	}
}

// TestAwaitRehydration_TransientReadErrorsRetry: a transient prober read error
// must NOT short-circuit the gate into a spurious fail-close mid-window — it is
// treated as "not yet proven" and retried under the deadline. Here the error
// clears and proof lands, so the rendezvous still releases.
func TestAwaitRehydration_TransientReadErrorsRetry(t *testing.T) {
	store := newFakeBeadStore()
	prober := &flakyProber{errFor: 2} // first 2 reads error, then prove live
	l := newRehydrateLifecycle(t, &fakeMicrovm{}, store, prober, 5*time.Second)
	out := l.AwaitRehydration(context.Background(), AgentBeadView{ID: "gd-flaky", RuntimeID: "mvm-fl"}, "thread-fl", time.Now())
	if !out.Released {
		t.Fatalf("transient errors should retry then release, got %+v", out)
	}
	if store.lastUpdate("gd-flaky")["agent_state"] != AgentStateWorking {
		t.Error("should have released to working after the transient errors cleared")
	}
}

// flakyProber errors for the first errFor reads, then reports live.
type flakyProber struct {
	reads  atomic.Int32
	errFor int
}

func (p *flakyProber) ProvenLiveSince(_ context.Context, _ string, _ time.Time) (bool, error) {
	n := int(p.reads.Add(1))
	if n <= p.errFor {
		return false, context.DeadlineExceeded // any error
	}
	return true, nil
}

// --- Gate disabled / unavailable no-ops -------------------------------------

// TestAwaitRehydration_GateOffIsNoop: with the gate off, AwaitRehydration is a
// released no-op (Resume already stamped working) — it must not touch the VM.
func TestAwaitRehydration_GateOffIsNoop(t *testing.T) {
	mv := &fakeMicrovm{}
	store := newFakeBeadStore()
	l := mustLifecycle(t, mv, newFakeSecrets(), store) // gate off
	out := l.AwaitRehydration(context.Background(), AgentBeadView{ID: "gd-x", RuntimeID: "mvm-x"}, "a", time.Now())
	if !out.Released {
		t.Errorf("gate-off AwaitRehydration should be a released no-op, got %+v", out)
	}
	if len(mv.termCalls) != 0 {
		t.Errorf("gate-off AwaitRehydration must not terminate, got %v", mv.termCalls)
	}
}

// TestAwaitRehydration_NilBackendIsNoop: an unavailable coordinator is a
// released no-op, so the wiring is safe to call unconditionally.
func TestAwaitRehydration_NilBackendIsNoop(t *testing.T) {
	l := NewLifecycle(nil, newFakeBeadStore(), discardLogger())
	out := l.AwaitRehydration(context.Background(), AgentBeadView{ID: "gd-x"}, "a", time.Now())
	if !out.Released {
		t.Errorf("nil-backend AwaitRehydration should be a released no-op, got %+v", out)
	}
}

// --- In-flight registry (dedup + status) ------------------------------------

// TestRehydration_InFlightRegistry: IsRehydrating reports true while a rendezvous
// is in flight and false once it settles; a duplicate AwaitRehydration attaches
// to the live one rather than starting a second.
func TestRehydration_InFlightRegistry(t *testing.T) {
	store := newFakeBeadStore()
	// A prober that blocks until released, so we can observe the in-flight window.
	gate := make(chan struct{})
	prober := &blockingProber{release: gate}
	l := newRehydrateLifecycle(t, &fakeMicrovm{}, store, prober, 5*time.Second)

	if l.IsRehydrating("thread-if") {
		t.Fatal("should not be rehydrating before AwaitRehydration starts")
	}

	done := make(chan RehydrationOutcome, 1)
	go func() {
		done <- l.AwaitRehydration(context.Background(), AgentBeadView{ID: "gd-if", RuntimeID: "mvm-if"}, "thread-if", time.Now())
	}()

	// Wait for the rendezvous to register in-flight.
	waitFor(t, func() bool { return l.IsRehydrating("thread-if") })

	// A duplicate resume for the same agent must attach, not start a second
	// rendezvous. Kick it off; it should also be waiting on the same gate.
	dup := make(chan RehydrationOutcome, 1)
	go func() {
		dup <- l.AwaitRehydration(context.Background(), AgentBeadView{ID: "gd-if", RuntimeID: "mvm-if"}, "thread-if", time.Now())
	}()

	close(gate) // release both
	first := <-done
	second := <-dup
	if !first.Released || !second.Released {
		t.Errorf("both the primary and the attached duplicate should release: %+v / %+v", first, second)
	}
	// After settle, no longer in flight.
	waitFor(t, func() bool { return !l.IsRehydrating("thread-if") })
}

// blockingProber reports not-live until `release` is closed, then live.
type blockingProber struct {
	release chan struct{}
	reads   atomic.Int32
}

func (p *blockingProber) ProvenLiveSince(ctx context.Context, _ string, _ time.Time) (bool, error) {
	p.reads.Add(1)
	select {
	case <-p.release:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	default:
		return false, nil
	}
}

// waitFor polls cond up to ~2s, failing the test if it never holds.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition never held within 2s")
}

// TestIsRehydrating_NilSafe: IsRehydrating is safe on nil/unavailable lifecycles.
func TestIsRehydrating_NilSafe(t *testing.T) {
	var l *Lifecycle
	if l.IsRehydrating("a") {
		t.Error("nil lifecycle should report not-rehydrating")
	}
	l2 := NewLifecycle(nil, newFakeBeadStore(), discardLogger())
	if l2.IsRehydrating("a") {
		t.Error("unavailable lifecycle should report not-rehydrating")
	}
}
