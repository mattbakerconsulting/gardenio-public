package main

// Tests that raise coverage for cmd/controller toward the 60% target.
// Pattern follows !1324 (cmd/trigger-engine), !1325 (cmd/wl-bridge),
// !1327 (internal/coopapi): focus on subcommand dispatch, helper
// branches, and error-exit paths. Pure main() wiring is intentionally
// not covered.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// ── main.go helpers ──────────────────────────────────────────────────────

func TestSetupLogger_Levels(t *testing.T) {
	for _, lvl := range []string{"debug", "info", "warn", "error", "other"} {
		l := setupLogger(lvl)
		if l == nil {
			t.Errorf("setupLogger(%q) returned nil", lvl)
		}
	}
}

// ── credsync_notifier.go ─────────────────────────────────────────────────

func TestNewCredSyncNotifier_Disabled(t *testing.T) {
	if n := newCredSyncNotifier("", "", "", discardLogger()); n != nil {
		t.Error("expected nil notifier with empty URL")
	}
}

func TestNewCredSyncNotifier_NATSDownRetries(t *testing.T) {
	// A NATS server that is not up yet at boot is retried in the background
	// (natsauth.StartupRetry, gd-XTyUibxpls): the notifier is built on a conn
	// that is not connected yet instead of being disabled for the process.
	n := newCredSyncNotifier("nats://127.0.0.1:1", "bad-token", "", discardLogger())
	if n == nil {
		t.Fatal("expected a retrying notifier while NATS is down, got nil")
	}
	defer n.Close()
	if n.conn.IsConnected() {
		t.Error("conn to a closed port reports connected")
	}
}

func TestCredSyncNotifier_NotifyMethodsOnNilConn(t *testing.T) {
	// Notifier with nil conn — publish returns early without panic.
	n := &credSyncNotifier{logger: discardLogger()}
	n.NotifyBulkSynced(3, 1, "startup")
	n.NotifyBulkFailed("listing failed", "periodic")
	n.NotifySingleSynced("acc", "claude")
	n.NotifySingleFailed("acc", "oops")
}

func TestCredSyncNotifier_NilReceiverSafe(t *testing.T) {
	// All methods must be safe to call on a nil notifier (simulates
	// "notifier disabled" path throughout the controller).
	var n *credSyncNotifier
	n.NotifyBulkSynced(0, 0, "t")
	n.NotifyBulkFailed("r", "t")
	n.NotifySingleSynced("a", "p")
	n.NotifySingleFailed("a", "r")
	n.Close()
	n.publish(credSyncEvent{Event: "x"})
}

func TestCredSyncEvent_JSONRoundTrip(t *testing.T) {
	evt := credSyncEvent{
		Event: "single_synced", Account: "acc-1", Provider: "claude",
		Synced: 2, Failed: 0, Trigger: "account_updated",
		Timestamp: "2026-03-20T03:00:00Z",
	}
	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back credSyncEvent
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back != evt {
		t.Errorf("roundtrip mismatch: %+v vs %+v", back, evt)
	}
}

// ── rotation_notifier.go ─────────────────────────────────────────────────

func TestRotationNotifier_AllNotifyMethodsOnNilConn(t *testing.T) {
	n := &rotationNotifier{logger: discardLogger()}
	n.NotifyRotating("p", "acc-a", "acc-b", "gd-1")
	n.NotifyRotated("p", "acc-a", "acc-b")
	n.NotifyNoFallback("p", "acc-a", "2026-03-20T03:00:00Z")
	n.NotifyThrottled("acc-a", "2026-03-20T03:00:00Z")
	n.NotifyRecovered("acc-a")
	n.NotifyWarming("acc-a", 0.85, 0.8)
	n.NotifyCooled("acc-a", 0.7, 0.8)
	n.Close()
}

func TestRotationNotifier_CloseNilSafe(t *testing.T) {
	var n *rotationNotifier
	n.Close()
}

func TestNewRotationNotifier_NATSDownRetries(t *testing.T) {
	n := newRotationNotifier("nats://127.0.0.1:1", "", "", discardLogger(), nil)
	if n == nil {
		t.Fatal("expected a retrying notifier while NATS is down, got nil")
	}
	defer n.Close()
	if n.conn.IsConnected() {
		t.Error("conn to a closed port reports connected")
	}
}

// ── account_exhaustion.go ────────────────────────────────────────────────

func TestAccountExhaustionListener_HandleMessage(t *testing.T) {
	// Set up a mock daemon that never returns any affected projects — Handle
	// exits harmlessly after ListBeadsFiltered returns empty.
	store := newMockStore()
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	daemon := newClient(t, srv)
	failover := newAuthFailover(daemon, nil, discardLogger(), 0, 0)

	l := &accountExhaustionListener{
		failover: failover,
		logger:   discardLogger(),
		debounce: time.Millisecond,
		lastSeen: make(map[string]time.Time),
	}

	cases := []struct {
		name string
		data []byte
	}{
		{"malformed json", []byte("not json")},
		{"missing project", []byte(`{"event":"x"}`)},
		{"ok event", []byte(`{"event":"account_exhausted","project":"p1","account":"acc","agent":"worker"}`)},
		{"missing account", []byte(`{"event":"account_exhausted","project":"p2","agent":"worker"}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l.handleMessage(context.Background(), &nats.Msg{Subject: "test", Data: c.data})
		})
	}
	// Debounce: second "ok event" with same key should be skipped.
	l.handleMessage(context.Background(), &nats.Msg{
		Subject: "test",
		Data:    []byte(`{"event":"account_exhausted","project":"p1","account":"acc","agent":"worker"}`),
	})
}

// ── auth_failover.go: walkChainForHealthy + earliestResetAt ──────────────

func TestWalkChainForHealthy(t *testing.T) {
	now := time.Now()
	cooldown := now.Add(5 * time.Minute).Format(time.RFC3339)

	a := &beadsapi.BeadDetail{Title: "a", Fields: map[string]string{"fallback": "b"}}
	b := &beadsapi.BeadDetail{Title: "b", Fields: map[string]string{
		"fallback": "c", "status": "exhausted", "cooldown_until": cooldown,
	}}
	c := &beadsapi.BeadDetail{Title: "c", Fields: map[string]string{}}
	accounts := []*beadsapi.BeadDetail{a, b, c}

	if got := walkChainForHealthy(accounts, a, now); got != "c" {
		t.Errorf("walkChainForHealthy = %q, want c (b is in cooldown)", got)
	}

	// No-fallback start → "".
	solo := &beadsapi.BeadDetail{Title: "solo", Fields: map[string]string{}}
	if got := walkChainForHealthy([]*beadsapi.BeadDetail{solo}, solo, now); got != "" {
		t.Errorf("walkChainForHealthy empty-chain = %q, want empty", got)
	}

	// Cycle detection: a→b→a. No healthy tail reachable.
	a2 := &beadsapi.BeadDetail{Title: "a", Fields: map[string]string{
		"fallback": "b", "status": "exhausted", "cooldown_until": cooldown,
	}}
	b2 := &beadsapi.BeadDetail{Title: "b", Fields: map[string]string{
		"fallback": "a", "status": "exhausted", "cooldown_until": cooldown,
	}}
	if got := walkChainForHealthy([]*beadsapi.BeadDetail{a2, b2}, a2, now); got != "" {
		t.Errorf("walkChainForHealthy cycle = %q, want empty", got)
	}

	// Missing fallback bead in accounts list → "".
	x := &beadsapi.BeadDetail{Title: "x", Fields: map[string]string{"fallback": "missing"}}
	if got := walkChainForHealthy([]*beadsapi.BeadDetail{x}, x, now); got != "" {
		t.Errorf("walkChainForHealthy missing target = %q, want empty", got)
	}
}

func TestEarliestResetAt(t *testing.T) {
	a := &beadsapi.BeadDetail{Title: "a", Fields: map[string]string{
		"fallback": "b", "throttle_reset_at": "2026-03-20T05:00:00Z",
	}}
	b := &beadsapi.BeadDetail{Title: "b", Fields: map[string]string{
		"fallback": "c", "throttle_reset_at": "2026-03-20T04:00:00Z",
	}}
	c := &beadsapi.BeadDetail{Title: "c", Fields: map[string]string{
		"throttle_reset_at": "2026-03-20T06:00:00Z",
	}}
	got := earliestResetAt([]*beadsapi.BeadDetail{a, b, c}, a)
	if got != "2026-03-20T04:00:00Z" {
		t.Errorf("earliestResetAt = %q, want 2026-03-20T04:00:00Z", got)
	}

	// No reset anywhere → "".
	empty := []*beadsapi.BeadDetail{
		{Title: "a", Fields: map[string]string{}},
	}
	if got := earliestResetAt(empty, empty[0]); got != "" {
		t.Errorf("earliestResetAt empty = %q, want empty", got)
	}

	// Cycle detection.
	a2 := &beadsapi.BeadDetail{Title: "a", Fields: map[string]string{
		"fallback": "b", "throttle_reset_at": "2026-03-20T05:00:00Z",
	}}
	b2 := &beadsapi.BeadDetail{Title: "b", Fields: map[string]string{
		"fallback": "a", "throttle_reset_at": "2026-03-20T07:00:00Z",
	}}
	got2 := earliestResetAt([]*beadsapi.BeadDetail{a2, b2}, a2)
	if got2 != "2026-03-20T05:00:00Z" {
		t.Errorf("earliestResetAt cycle = %q, want 2026-03-20T05:00:00Z", got2)
	}
}

// ── failover_runtime.go: ProjectsByCurrent ───────────────────────────────

func TestFailoverRuntime_ProjectsByCurrent(t *testing.T) {
	rt := newFailoverRuntime()
	rt.Set("p1", failoverState{Primary: "acc-a", Current: "acc-b"})
	rt.Set("p2", failoverState{Primary: "acc-a", Current: "acc-c"})
	rt.Set("p3", failoverState{Primary: "acc-a", Current: "acc-b"})

	got := rt.ProjectsByCurrent("acc-b")
	want := map[string]bool{"p1": true, "p3": true}
	if len(got) != 2 {
		t.Fatalf("ProjectsByCurrent = %v, want 2 projects", got)
	}
	for _, p := range got {
		if !want[p] {
			t.Errorf("unexpected project: %s", p)
		}
	}

	if got := rt.ProjectsByCurrent("none"); len(got) != 0 {
		t.Errorf("ProjectsByCurrent unknown = %v, want empty", got)
	}
}

// ── probe_event_listener.go ──────────────────────────────────────────────

func TestNewProbeEventListener(t *testing.T) {
	l := newProbeEventListener(nil, nil, discardLogger())
	if l == nil || l.debounce != 2*time.Minute {
		t.Fatalf("probe event listener not initialized: %+v", l)
	}
}

func TestProbeEventListener_TryDebounce(t *testing.T) {
	l := &probeEventListener{
		debounce: 50 * time.Millisecond,
		lastSeen: make(map[string]time.Time),
		logger:   discardLogger(),
	}
	if !l.tryDebounce("acc-a") {
		t.Fatal("first call should pass")
	}
	if l.tryDebounce("acc-a") {
		t.Fatal("second call should be debounced")
	}
	if !l.tryDebounce("acc-b") {
		t.Fatal("different key should pass")
	}
	time.Sleep(75 * time.Millisecond)
	if !l.tryDebounce("acc-a") {
		t.Fatal("after window should pass")
	}
}

func TestProbeEventListener_Start_Disabled(t *testing.T) {
	l := newProbeEventListener(nil, nil, discardLogger())
	if err := l.Start(context.Background(), "", "", ""); err != nil {
		t.Fatalf("Start with empty NATS URL should be no-op: %v", err)
	}
}

func TestProbeEventListener_Start_NATSDownWaits(t *testing.T) {
	// NATS down at boot: Start keeps retrying until ctx ends rather than
	// returning a connect error that leaves the listener dead.
	l := newProbeEventListener(nil, nil, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := l.Start(ctx, "nats://127.0.0.1:1", "", ""); err != nil {
		t.Errorf("Start with NATS down = %v, want nil after ctx ends", err)
	}
}

func TestChainContains(t *testing.T) {
	a := &beadsapi.BeadDetail{Title: "a", Fields: map[string]string{"fallback": "b"}}
	b := &beadsapi.BeadDetail{Title: "b", Fields: map[string]string{"fallback": "c"}}
	c := &beadsapi.BeadDetail{Title: "c", Fields: map[string]string{}}
	accts := []*beadsapi.BeadDetail{a, b, c}

	if !chainContains(accts, a, "c") {
		t.Error("c should be in a's chain")
	}
	if chainContains(accts, a, "missing") {
		t.Error("unrelated account should not be in chain")
	}
	if chainContains(accts, nil, "c") {
		t.Error("nil start should return false")
	}

	// Cycle detection.
	a2 := &beadsapi.BeadDetail{Title: "a", Fields: map[string]string{"fallback": "b"}}
	b2 := &beadsapi.BeadDetail{Title: "b", Fields: map[string]string{"fallback": "a"}}
	if chainContains([]*beadsapi.BeadDetail{a2, b2}, a2, "z") {
		t.Error("chain with cycle should still return false for missing target")
	}

	// Missing fallback target in accounts list breaks the walk early.
	x := &beadsapi.BeadDetail{Title: "x", Fields: map[string]string{"fallback": "y"}}
	if chainContains([]*beadsapi.BeadDetail{x}, x, "z") {
		t.Error("missing fallback target should return false")
	}
}

func TestProbeEventListener_HandleThrottled(t *testing.T) {
	// Set up a stateful mock daemon with one project on acc-a, primary.
	store := newMockStore(
		account("b1", "acc-a", "p1", map[string]any{}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	daemon := newClient(t, srv)
	failover := newAuthFailover(daemon, nil, discardLogger(), 0, 0)
	l := newProbeEventListener(daemon, failover, discardLogger())
	l.debounce = time.Nanosecond // effectively disable debounce in this test

	// Malformed payload.
	l.handleThrottled(context.Background(), &nats.Msg{Data: []byte("not json")})

	// Missing from_account.
	l.handleThrottled(context.Background(), &nats.Msg{Data: []byte(`{"event":"x"}`)})

	// Valid event — single project primary is acc-a.
	l.handleThrottled(context.Background(), &nats.Msg{
		Data: []byte(`{"event":"account_throttled","from_account":"acc-a","timestamp":"2026-03-20T03:00:00Z"}`),
	})

	// Second identical event — debounced (debounce window is 1ns but test
	// is fast enough that this exercises the "lastSeen present" branch).
	l.debounce = time.Minute
	l.handleThrottled(context.Background(), &nats.Msg{
		Data: []byte(`{"event":"account_throttled","from_account":"acc-a","timestamp":"2026-03-20T03:00:00Z"}`),
	})

	// Event for account with no projects — findProjectsForAccount returns empty.
	l.debounce = time.Nanosecond
	l.handleThrottled(context.Background(), &nats.Msg{
		Data: []byte(`{"event":"account_throttled","from_account":"unknown-acct"}`),
	})
}

func TestProbeEventListener_HandleRecovered(t *testing.T) {
	store := newMockStore(
		account("b1", "acc-a", "p1", map[string]any{}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	daemon := newClient(t, srv)
	failover := newAuthFailover(daemon, nil, discardLogger(), 0, 0)
	l := newProbeEventListener(daemon, failover, discardLogger())

	l.handleRecovered(context.Background(), &nats.Msg{Data: []byte("not json")})
	l.handleRecovered(context.Background(), &nats.Msg{Data: []byte(`{"event":"x"}`)})
	l.handleRecovered(context.Background(), &nats.Msg{
		Data: []byte(`{"event":"account_recovered","from_account":"acc-a"}`),
	})
}

func TestProbeEventListener_RetryStuckFailovers(t *testing.T) {
	now := time.Now()
	cooldownFuture := now.Add(5 * time.Minute).Format(time.RFC3339)

	// Primary is in active cooldown, and recovered account is in its chain.
	store := newMockStore(
		account("b1", "acc-a", "p1", map[string]any{
			"fallback":       "acc-b",
			"status":         "exhausted",
			"cooldown_until": cooldownFuture,
		}),
		account("b2", "acc-b", "p1", map[string]any{}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	daemon := newClient(t, srv)
	failover := newAuthFailover(daemon, nil, discardLogger(), 0, 0)
	l := newProbeEventListener(daemon, failover, discardLogger())

	// Project's primary acc-a is stuck, recovered acc-b is in its chain →
	// retry path fires.
	l.retryStuckFailovers(context.Background(), "acc-b")

	// Recovered account NOT in chain → early-return path.
	l.retryStuckFailovers(context.Background(), "unrelated")

	// Daemon error branch: swap the server for one that 500s.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer badSrv.Close()
	badDaemon := newClient(t, badSrv)
	l2 := newProbeEventListener(badDaemon, newAuthFailover(badDaemon, nil, discardLogger(), 0, 0), discardLogger())
	l2.retryStuckFailovers(context.Background(), "acc-b")
}
