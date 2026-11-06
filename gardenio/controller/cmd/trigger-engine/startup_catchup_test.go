// Tests for the startup catch-up gate (gd-niQPoeR7ja): the trigger-engine must
// RETRY a failed initial catch-up load with capped exponential backoff while
// staying up and NOT ready — never exit. The bug these pin: the engine logged
// "catch-up load failed" for every source, carried on with empty seen sets,
// logged "ready", and then (the health listener having never bound, so the
// kubelet SIGTERMed it mid-load) exited 0 into CrashLoopBackOff on every beads
// blip.
package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
)

// flakyDaemon is a fake beads daemon that fails the first failures list calls
// and succeeds forever after — the N-then-succeed shape of a DNS outage that
// clears, or a daemon rollout that finishes.
type flakyDaemon struct {
	emptyDaemon
	failures int32
	calls    atomic.Int32
}

func (d *flakyDaemon) ListBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if d.calls.Add(1) <= d.failures {
		return nil, errors.New("dial tcp: lookup beads on 10.43.0.10:53: no such host")
	}
	return &beadsapi.ListBeadsResult{}, nil
}

// testBackoff keeps the retry math real (capped exponential) but sub-millisecond
// so the tests stay fast.
func testBackoff() bridge.BackoffConfig {
	return bridge.BackoffConfig{Initial: time.Millisecond, Max: 4 * time.Millisecond, Factor: 2}
}

// newEngineOn builds a trigger engine with one real source over the given
// daemon, so the catch-up path under test is the production one.
func newEngineOn(daemon bridge.TriggerBeadClient) *bridge.TriggerEngine {
	e := bridge.NewTriggerEngine(bridge.TriggerEngineConfig{
		Daemon: daemon,
		Logger: discardLogger(),
	})
	e.RegisterSource(bridge.NewScheduleTriggerSource(discardLogger()))
	return e
}

// TestAwaitInitialCatchUp_RetriesThenReady is the headline case: a daemon that
// fails three times then succeeds must leave the process UP, NOT ready while
// failing, and ready once the load lands.
func TestAwaitInitialCatchUp_RetriesThenReady(t *testing.T) {
	daemon := &flakyDaemon{failures: 3}
	ready := &atomic.Bool{}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ok := awaitInitialCatchUp(ctx, newEngineOn(daemon), testBackoff(), ready, discardLogger())
	if !ok {
		t.Fatalf("awaitInitialCatchUp = false, want true: a daemon that recovers must NOT stop the engine")
	}
	if !ready.Load() {
		t.Errorf("ready = false after a successful catch-up, want true")
	}
	// 3 failures + 1 success, and the schedule source's two heal sweeps also
	// list, so assert the floor rather than an exact count.
	if got := daemon.calls.Load(); got < 4 {
		t.Errorf("daemon list calls = %d, want >= 4 (the load must be RETRIED, not abandoned)", got)
	}
}

// TestAwaitInitialCatchUp_NotReadyWhileDaemonDown is the "stays up and NOT
// ready" half of the contract: while the daemon is unreachable the gate keeps
// retrying — it does not return, does not flip ready, and (crucially) does not
// tell the caller to exit.
func TestAwaitInitialCatchUp_NotReadyWhileDaemonDown(t *testing.T) {
	// Never succeeds: failures is effectively unbounded for this test's life.
	daemon := &flakyDaemon{failures: 1 << 30}
	ready := &atomic.Bool{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan bool, 1)
	go func() {
		done <- awaitInitialCatchUp(ctx, newEngineOn(daemon), testBackoff(), ready, discardLogger())
	}()

	// Give the gate room to burn several attempts. It must still be retrying.
	deadline := time.Now().Add(2 * time.Second)
	for daemon.calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := daemon.calls.Load(); got < 3 {
		t.Fatalf("daemon list calls = %d after retrying, want >= 3", got)
	}
	select {
	case ok := <-done:
		t.Fatalf("awaitInitialCatchUp returned %v while the daemon was still down; it must keep retrying, never give up", ok)
	default:
	}
	if ready.Load() {
		t.Errorf("ready = true while the catch-up was still failing, want false")
	}

	// Only a shutdown ends the retry — and then it reports "do not start the
	// engine", NOT a crash.
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Errorf("awaitInitialCatchUp = true after ctx cancel, want false")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("awaitInitialCatchUp did not return after ctx cancel")
	}
	if ready.Load() {
		t.Errorf("ready = true after a cancelled catch-up, want false")
	}
}

// TestReadyzHandler_GatedOnCatchUp pins the probe contract k8s reads: 503 with a
// machine-readable reason until the catch-up lands, 200 after. Readiness is the
// signal for "up but not serving", which is what keeps a beads blip from
// becoming a restart.
func TestReadyzHandler_GatedOnCatchUp(t *testing.T) {
	ready := &atomic.Bool{}
	h := readyzHandler(ready)

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("pre-catch-up /readyz = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"startup_catch_up_pending"`) {
		t.Errorf("pre-catch-up /readyz body = %s, want startup_catch_up_pending", rec.Body.String())
	}

	ready.Store(true)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("post-catch-up /readyz = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}

// TestLiveHealthzHandler_BootstrappingSkipsStaleness pins the other half of the
// crash-loop fix: while the catch-up is still retrying there is no Evaluate()
// cycle to be stale, so /healthz must report ok. 503ing here would have the
// kubelet restart a pod that is correctly waiting on the daemon — the exact
// loop this bead removes.
func TestLiveHealthzHandler_BootstrappingSkipsStaleness(t *testing.T) {
	ready := &atomic.Bool{}
	h := liveHealthzHandler(time.Minute, ready)

	// Deadman clock deliberately ancient: the staleness check must not run yet.
	lastEvaluateNano.Store(time.Now().Add(-24 * time.Hour).UnixNano())

	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("bootstrapping /healthz = %d, want 200 (liveness must not kill a pod waiting on beads); body=%s",
			rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"state":"bootstrapping"`) {
		t.Errorf("bootstrapping /healthz body = %s, want state bootstrapping", rec.Body.String())
	}

	// Once ready, the deadman is armed again and the stale clock 503s.
	ready.Store(true)
	rec = httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("ready+stale /healthz = %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestGateUntilReady_BlocksWebhooksUntilCatchUp: the webhook listener now binds
// before the catch-up so probes always have a socket, so it must refuse
// webhooks until the seen sets are seeded — otherwise an early delivery fires
// triggers the engine has already handled.
func TestGateUntilReady_BlocksWebhooksUntilCatchUp(t *testing.T) {
	ready := &atomic.Bool{}
	var served atomic.Int32
	h := gateUntilReady(ready, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/tok", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("pre-catch-up webhook = %d, want 503", rec.Code)
	}
	if served.Load() != 0 {
		t.Errorf("webhook reached the handler before the catch-up landed")
	}

	ready.Store(true)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/webhooks/tok", nil))
	if rec.Code != http.StatusOK || served.Load() != 1 {
		t.Errorf("post-catch-up webhook = %d served=%d, want 200/1", rec.Code, served.Load())
	}
}

// TestAwaitInitialCatchUp_RearmsDeadmanOnReady pins the crash-loop hole that
// the rest of this fix would otherwise leave open.
//
// main seeds the deadman clock at process start so a fresh pod gets one
// health-staleness window before /healthz can 503 it (ka-0000005325). But the
// catch-up now RETRIES, and a real outage — the 2026-09-20 DNS event ran for
// far longer than the 300s default window — burns that grace entirely while
// the gate is still waiting. If ready flipped without re-stamping the clock,
// the first probe after recovery would see a clock hours old, 503, and have the
// kubelet restart a pod that had just successfully recovered: the same
// CrashLoopBackOff this bead removes, re-entered through liveness instead of
// exit 0.
//
// So: going ready must restart the grace window, giving the engine loop that
// starts immediately afterwards a full window to complete its first Evaluate.
func TestAwaitInitialCatchUp_RearmsDeadmanOnReady(t *testing.T) {
	daemon := &flakyDaemon{failures: 2}
	ready := &atomic.Bool{}

	// The pod has been up far longer than the staleness window, retrying the
	// catch-up the whole time — exactly the post-outage state.
	staleness := time.Minute
	lastEvaluateNano.Store(time.Now().Add(-24 * time.Hour).UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if !awaitInitialCatchUp(ctx, newEngineOn(daemon), testBackoff(), ready, discardLogger()) {
		t.Fatalf("awaitInitialCatchUp = false, want true")
	}

	// Now ready, so /healthz arms the staleness check against a clock that must
	// have been re-stamped at the flip.
	rec := httptest.NewRecorder()
	liveHealthzHandler(staleness, ready)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d immediately after going ready, want 200: "+
			"the deadman grace window must restart when the catch-up lands, or a "+
			"recovered pod is liveness-killed straight back into CrashLoopBackOff; body=%s",
			rec.Code, rec.Body.String())
	}
	if age := time.Since(time.Unix(0, lastEvaluateNano.Load())); age > staleness {
		t.Errorf("deadman clock age = %s after going ready, want < %s", age, staleness)
	}
}
