package main

// Tests for the rate-limit probe liveness hardening (ka-0000001889):
//
//   - A panicking probe iteration does NOT terminate RunLoop — the loop keeps
//     ticking and the panic is recovered + counted (the acceptance-criteria
//     regression test for the 2026-07-05 silent-freeze incident).
//   - A per-account panic is isolated: sibling accounts in the same pass are
//     still probed.
//   - The watchdog detects a stale last-tick and raises its alert once the
//     staleness crosses the stall threshold, and stays quiet before that.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
)

// newBareProbe builds a rateLimitProbe wired to a stub daemon returning the
// given account beads and a fake k8s client holding the given secrets. Unlike
// buildProbeWithMockAnthropicURL it does not rewrite the Anthropic URL — tests
// here either inject panics before any HTTP call or drive the watchdog with a
// synthetic clock, so no upstream is contacted.
func newBareProbe(t *testing.T, accountBeads []map[string]any, secrets map[string]string) (*rateLimitProbe, func()) {
	t.Helper()

	daemonSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/v1/beads" {
			var beads []map[string]any
			if r.URL.Query().Get("type") == "account" {
				beads = accountBeads
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
			return
		}
		if r.Method == "PATCH" {
			_, _ = w.Write([]byte("{}"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: daemonSrv.URL})
	if err != nil {
		t.Fatalf("failed to create beads client: %v", err)
	}

	fakeK8s := fake.NewSimpleClientset()
	for name, token := range secrets {
		if _, err := fakeK8s.CoreV1().Secrets("gardenio").Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gardenio"},
			Data:       map[string][]byte{"token": []byte(token)},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create secret %s: %v", name, err)
		}
	}

	probe := newRateLimitProbe(daemon, nil, fakeK8s, "gardenio", nil, nil, testProbeLogger())
	return probe, daemonSrv.Close
}

func probeAccountBead(id, title, secret string) map[string]any {
	return map[string]any{
		"id":     id,
		"title":  title,
		"type":   "account",
		"status": "open",
		"fields": map[string]string{
			"credential": "secret:" + secret,
			"provider":   "claude",
		},
	}
}

// TestRunLoop_SurvivesPanickingProbe is the core acceptance-criteria test: a
// probe iteration that panics on EVERY account must not terminate RunLoop. The
// loop must keep ticking (advancing its heartbeat) and count the recovered
// panics. Before the fix the first panic unwound the RunLoop goroutine and the
// probe went silent for ~24h (ka-0000001889).
func TestRunLoop_SurvivesPanickingProbe(t *testing.T) {
	probe, cleanup := newBareProbe(t,
		[]map[string]any{probeAccountBead("gd-a1", "acct-a", "tok-a")},
		map[string]string{"tok-a": "sk-ant-a"})
	defer cleanup()

	// Every per-account probe panics.
	probe.probeHook = func(account string) {
		panic("boom in probe for " + account)
	}

	// Drive the loop fast so several ticks land within the test window.
	probe.tickInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() { probe.RunLoop(ctx); close(done) }()

	// Wait until the loop has ticked several times despite the panics.
	deadline := time.After(2 * time.Second)
	for probe.tickCount.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("RunLoop did not keep ticking through panics: ticks=%d panics=%d",
				probe.tickCount.Load(), probe.panicsRecovered.Load())
		case <-time.After(2 * time.Millisecond):
		}
	}

	// The panics must have been recovered + counted, not have killed the loop.
	if got := probe.panicsRecovered.Load(); got < 3 {
		t.Errorf("expected >=3 recovered panics, got %d", got)
	}

	// The loop is still alive: cancel and confirm it returns cleanly.
	cancel()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("RunLoop did not return after context cancel")
	}
}

// TestProbeAll_PerAccountPanicIsolation proves one account panicking does not
// abort the pass for its siblings: the second account is still probed. We track
// which accounts probeHook saw and confirm both were reached even though the
// first panics.
func TestProbeAll_PerAccountPanicIsolation(t *testing.T) {
	probe, cleanup := newBareProbe(t,
		[]map[string]any{
			probeAccountBead("gd-a1", "acct-a", "tok-a"),
			probeAccountBead("gd-b2", "acct-b", "tok-b"),
		},
		map[string]string{"tok-a": "sk-ant-a", "tok-b": "sk-ant-b"})
	defer cleanup()

	var mu sync.Mutex
	seen := map[string]bool{}
	probe.probeHook = func(account string) {
		mu.Lock()
		seen[account] = true
		mu.Unlock()
		if account == "acct-a" {
			panic("boom in acct-a")
		}
		// acct-b: panic too, so no real HTTP call is made. Both are isolated.
		panic("boom in acct-b")
	}

	probe.probeAll(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if !seen["acct-a"] || !seen["acct-b"] {
		t.Errorf("expected both accounts probed despite panics; seen=%v", seen)
	}
	if got := probe.panicsRecovered.Load(); got != 2 {
		t.Errorf("expected 2 recovered panics (one per account), got %d", got)
	}
}

// TestTick_HeartbeatAdvancesThroughPerAccountPanic confirms the heartbeat is
// recorded AFTER the (recovered) probe pass: even when every account panics, the
// tick still lands lastProbeTick + tickCount so the watchdog sees a live loop.
func TestTick_HeartbeatAdvancesThroughPerAccountPanic(t *testing.T) {
	probe, cleanup := newBareProbe(t,
		[]map[string]any{probeAccountBead("gd-a1", "acct-a", "tok-a")},
		map[string]string{"tok-a": "sk-ant-a"})
	defer cleanup()

	probe.probeHook = func(string) { panic("boom") }

	before := probe.tickCount.Load()
	probe.tick(context.Background())

	if probe.tickCount.Load() != before+1 {
		t.Errorf("tick heartbeat did not advance: before=%d after=%d", before, probe.tickCount.Load())
	}
	if probe.lastProbeTick.Load() == 0 {
		t.Error("expected lastProbeTick to be set after tick")
	}
	if probe.panicsRecovered.Load() == 0 {
		t.Error("expected the per-account panic to be recorded")
	}
}

// TestTick_OuterRecoverCatchesProbeAllPanic covers the OUTER recover() in tick()
// — the one guarding a panic that escapes probeAll itself (e.g. the bead-listing
// scaffolding faults), not just a per-account probe. A probe with a nil daemon
// panics on the very first line of probeAll (nil pointer deref in
// ListAccountBeads); tick() must swallow it, count it, and still advance the
// heartbeat so RunLoop keeps going.
func TestTick_OuterRecoverCatchesProbeAllPanic(t *testing.T) {
	// nil daemon → probeAll panics before reaching any per-account recover().
	probe := newRateLimitProbe(nil, nil, fake.NewSimpleClientset(), "gardenio", nil, nil, testProbeLogger())

	before := probe.tickCount.Load()
	// Must not panic out of tick().
	probe.tick(context.Background())

	if probe.tickCount.Load() != before+1 {
		t.Errorf("tick heartbeat did not advance through probeAll panic: before=%d after=%d",
			before, probe.tickCount.Load())
	}
	if probe.panicsRecovered.Load() == 0 {
		t.Error("expected the probeAll-scaffolding panic to be recovered + counted")
	}
	if probe.lastProbeTick.Load() == 0 {
		t.Error("expected lastProbeTick to advance even when probeAll panics")
	}
}

// TestWatchdog_DetectsStall drives checkLiveness with a synthetic clock: no
// stall while the last tick is fresh, an alert once it crosses the threshold.
func TestWatchdog_DetectsStall(t *testing.T) {
	probe, cleanup := newBareProbe(t, nil, nil)
	defer cleanup()
	probe.stallThreshold = 10 * time.Minute

	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

	// Before any tick, checkLiveness is a no-op (nothing to be stale against).
	probe.now = func() time.Time { return base }
	probe.checkLiveness()
	if probe.stallsDetected.Load() != 0 {
		t.Fatalf("expected no stall before first tick, got %d", probe.stallsDetected.Load())
	}

	// Record a tick at base.
	probe.tick(context.Background())

	// 5 minutes later: still fresh (< 10m threshold) — no alert.
	probe.now = func() time.Time { return base.Add(5 * time.Minute) }
	probe.checkLiveness()
	if probe.stallsDetected.Load() != 0 {
		t.Errorf("expected no stall at 5min (<10min threshold), got %d", probe.stallsDetected.Load())
	}

	// 11 minutes after the tick: stale — exactly one alert.
	probe.now = func() time.Time { return base.Add(11 * time.Minute) }
	probe.checkLiveness()
	if probe.stallsDetected.Load() != 1 {
		t.Errorf("expected exactly 1 stall at 11min (>10min threshold), got %d", probe.stallsDetected.Load())
	}
}

// TestWatchdog_ClearsAfterRecovery proves the watchdog stops alerting once the
// loop resumes ticking: a fresh tick moves lastProbeTick forward, so a
// subsequent liveness check within the threshold sees no stall.
func TestWatchdog_ClearsAfterRecovery(t *testing.T) {
	probe, cleanup := newBareProbe(t, nil, nil)
	defer cleanup()
	probe.stallThreshold = 10 * time.Minute

	base := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)

	// Tick at base, then go stale and alert.
	probe.now = func() time.Time { return base }
	probe.tick(context.Background())
	probe.now = func() time.Time { return base.Add(11 * time.Minute) }
	probe.checkLiveness()
	if probe.stallsDetected.Load() != 1 {
		t.Fatalf("expected 1 stall while wedged, got %d", probe.stallsDetected.Load())
	}

	// Loop recovers: a fresh tick at t+11m advances the heartbeat.
	probe.tick(context.Background())

	// One minute after recovery: fresh again — no new stall.
	probe.now = func() time.Time { return base.Add(12 * time.Minute) }
	probe.checkLiveness()
	if probe.stallsDetected.Load() != 1 {
		t.Errorf("expected stall count to stay 1 after recovery, got %d", probe.stallsDetected.Load())
	}
}
