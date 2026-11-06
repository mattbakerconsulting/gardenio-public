// Tests for the catch-up retry contract (gd-niQPoeR7ja). CatchUpAll must REPORT
// a failed load rather than shrug it off with empty seen sets, and
// CatchUpWithRetry must keep retrying — with capped exponential backoff — until
// the daemon answers or the caller is shut down. Never exit, never report a
// clean catch-up it did not get.
package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func retryTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fastRetry keeps the backoff real but sub-millisecond.
func fastRetry() BackoffConfig {
	return BackoffConfig{Initial: time.Millisecond, Max: 4 * time.Millisecond, Factor: 2}
}

// flakySource is a TriggerSource whose LoadTriggers fails the first failures
// calls — standing in for a daemon that is unreachable and then comes back.
type flakySource struct {
	failures  int32
	loadCalls atomic.Int32
	keyCalls  atomic.Int32
}

func (s *flakySource) Name() string { return "flaky" }

func (s *flakySource) LoadTriggers(_ context.Context, _ TriggerBeadClient) ([]TriggerDef, error) {
	if s.loadCalls.Add(1) <= s.failures {
		return nil, errors.New("beads daemon unreachable")
	}
	return []TriggerDef{{ID: "trig-1", Title: "flaky trigger", LastChecked: time.Now()}}, nil
}

func (s *flakySource) CatchUpKeys(_ context.Context, _ TriggerBeadClient, _ string) ([]string, error) {
	s.keyCalls.Add(1)
	return []string{"key-a", "key-b"}, nil
}

func (s *flakySource) Evaluate(_ context.Context, _ TriggerDef) ([]TriggerMatch, error) {
	return nil, nil
}

// keyFailSource loads fine but fails every per-trigger key lookup.
type keyFailSource struct{ loadCalls atomic.Int32 }

func (s *keyFailSource) Name() string { return "keyfail" }

func (s *keyFailSource) LoadTriggers(_ context.Context, _ TriggerBeadClient) ([]TriggerDef, error) {
	s.loadCalls.Add(1)
	return []TriggerDef{{ID: "trig-1", LastChecked: time.Now()}}, nil
}

func (s *keyFailSource) CatchUpKeys(_ context.Context, _ TriggerBeadClient, _ string) ([]string, error) {
	return nil, errors.New("catch-up keys query failed")
}

func (s *keyFailSource) Evaluate(_ context.Context, _ TriggerDef) ([]TriggerMatch, error) {
	return nil, nil
}

func newRetryEngine() *TriggerEngine {
	return NewTriggerEngine(TriggerEngineConfig{
		Daemon: &mockTriggerDaemon{},
		Logger: retryTestLogger(),
	})
}

// TestCatchUpAll_ReportsLoadFailure: the old code logged and returned, so the
// caller could not tell a seeded engine from an empty one. It must report.
func TestCatchUpAll_ReportsLoadFailure(t *testing.T) {
	e := newRetryEngine()
	src := &flakySource{failures: 1}
	e.RegisterSource(src)

	if err := e.CatchUpAll(context.Background()); err == nil {
		t.Fatalf("CatchUpAll = nil on a failed load, want an error")
	}
	if err := e.CatchUpAll(context.Background()); err != nil {
		t.Errorf("CatchUpAll = %v on a healthy load, want nil", err)
	}
}

// TestCatchUpAll_PerTriggerKeyFailureIsNotFatal: a single malformed trigger
// must NOT be able to hold the engine permanently un-ready. Only an
// unreachable daemon (a failed LOAD) gates readiness.
func TestCatchUpAll_PerTriggerKeyFailureIsNotFatal(t *testing.T) {
	e := newRetryEngine()
	e.RegisterSource(&keyFailSource{})

	if err := e.CatchUpAll(context.Background()); err != nil {
		t.Errorf("CatchUpAll = %v for a per-trigger key failure, want nil (load succeeded)", err)
	}
}

// TestCatchUpWithRetry_FailsNTimesThenSucceeds is the bead's named case: a fake
// daemon that fails N times then succeeds must end in a seeded, successful
// catch-up — with the load actually retried N+1 times.
func TestCatchUpWithRetry_FailsNTimesThenSucceeds(t *testing.T) {
	e := newRetryEngine()
	src := &flakySource{failures: 4}
	e.RegisterSource(src)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := e.CatchUpWithRetry(ctx, fastRetry()); err != nil {
		t.Fatalf("CatchUpWithRetry = %v, want nil once the daemon recovers", err)
	}
	if got := src.loadCalls.Load(); got != 5 {
		t.Errorf("LoadTriggers calls = %d, want 5 (4 failures + 1 success)", got)
	}
	if src.keyCalls.Load() == 0 {
		t.Errorf("seen sets were never seeded: CatchUpKeys was not called after the load landed")
	}
}

// TestCatchUpWithRetry_RetriesForeverUntilShutdown: the retry budget for the
// startup load is unbounded by design. Only a cancelled ctx ends it, and the
// error then identifies a shutdown — not a reason to exit non-zero.
func TestCatchUpWithRetry_RetriesForeverUntilShutdown(t *testing.T) {
	e := newRetryEngine()
	src := &flakySource{failures: 1 << 30}
	e.RegisterSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- e.CatchUpWithRetry(ctx, fastRetry()) }()

	deadline := time.Now().Add(2 * time.Second)
	for src.loadCalls.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := src.loadCalls.Load(); got < 5 {
		t.Fatalf("LoadTriggers calls = %d, want >= 5: the retry must not give up", got)
	}
	select {
	case err := <-errCh:
		t.Fatalf("CatchUpWithRetry returned %v while the daemon was still down; it must keep retrying", err)
	default:
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("CatchUpWithRetry = %v after cancel, want a context.Canceled error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("CatchUpWithRetry did not return after ctx cancel")
	}
}

// TestRetryWithBackoff_CapsTheDelay pins the "capped" in capped exponential:
// the delay grows 1→2→4→4… and never exceeds Max.
func TestRetryWithBackoff_CapsTheDelay(t *testing.T) {
	var starts []time.Time
	cfg := BackoffConfig{Initial: 10 * time.Millisecond, Max: 20 * time.Millisecond, Factor: 2, MaxAttempts: 5}

	err := RetryWithBackoff(context.Background(), retryTestLogger(), "test", cfg, func(context.Context) error {
		starts = append(starts, time.Now())
		return errors.New("still down")
	})
	if err == nil {
		t.Fatalf("RetryWithBackoff = nil, want the last error once MaxAttempts is spent")
	}
	if len(starts) != 5 {
		t.Fatalf("attempts = %d, want 5 (MaxAttempts)", len(starts))
	}
	// Gaps should be ~10, ~20, ~20, ~20 — growing then capped, never beyond Max
	// plus scheduler slack.
	for i := 1; i < len(starts); i++ {
		gap := starts[i].Sub(starts[i-1])
		if gap > 500*time.Millisecond {
			t.Errorf("gap %d = %s, want <= Max (20ms) plus slack — the cap is not holding", i, gap)
		}
	}
}

// TestRetryWithBackoff_ZeroConfigIsBounded: the zero value must not silently
// mean "one attempt". It means "retry with the defaults", and MaxAttempts 0
// means retry until ctx is done — so an already-cancelled ctx returns
// immediately rather than blocking.
func TestRetryWithBackoff_ZeroConfigStopsOnDoneCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var calls int
	err := RetryWithBackoff(ctx, retryTestLogger(), "test", BackoffConfig{}, func(context.Context) error {
		calls++
		return errors.New("down")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RetryWithBackoff = %v on a cancelled ctx, want context.Canceled", err)
	}
	if calls != 0 {
		t.Errorf("fn called %d times on an already-cancelled ctx, want 0", calls)
	}
}

// TestHealListsRetryTransientFailure covers the heal half of the bead: the
// wedged-trigger and stalled-open sweeps used to skip an entire cycle on one
// transient list error. They must retry — bounded, since the next poll is
// another chance — and the load behind them must still succeed.
func TestHealListsRetryTransientFailure(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	// Fail only the very first list call (the wedged-trigger heal's), then
	// serve every later one.
	daemon.listBeadsHook = func(n int, _ beadsapi.ListBeadsQuery) error {
		if n == 1 {
			return errors.New("transient: connection reset by peer")
		}
		return nil
	}

	src := NewScheduleTriggerSource(retryTestLogger())
	src.healBackoff = BackoffConfig{Initial: time.Millisecond, Max: 2 * time.Millisecond, Factor: 2, MaxAttempts: 3}

	if _, err := src.LoadTriggers(context.Background(), daemon); err != nil {
		t.Fatalf("LoadTriggers = %v, want nil: a retried heal list must not fail the load", err)
	}

	daemon.mu.Lock()
	calls := daemon.listBeadsCalls
	daemon.mu.Unlock()
	// 1 failed + 1 retried wedged list, 1 stalled-open list that the main load
	// reuses (gd-6PXLuR3juD). Without the retry it would be 2.
	if calls < 3 {
		t.Errorf("list calls = %d, want >= 3 (the failed heal list must be RETRIED, not skipped)", calls)
	}
}

// TestHealListRetryIsBounded: the heal retry must never become open-ended — it
// runs inside the evaluation cycle, so retrying forever there would wedge every
// other source. A permanently-failing list gives up after MaxAttempts and the
// load still completes.
func TestHealListRetryIsBounded(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	var healCalls atomic.Int32
	daemon.listBeadsHook = func(_ int, q beadsapi.ListBeadsQuery) error {
		// Fail only the heal queries; let the main open-load through.
		if len(q.Statuses) == 1 && q.Statuses[0] == "in_progress" {
			healCalls.Add(1)
			return errors.New("heal list down")
		}
		return nil
	}

	src := NewScheduleTriggerSource(retryTestLogger())
	src.healBackoff = BackoffConfig{Initial: time.Millisecond, Max: 2 * time.Millisecond, Factor: 2, MaxAttempts: 3}

	done := make(chan error, 1)
	go func() {
		_, err := src.LoadTriggers(context.Background(), daemon)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("LoadTriggers = %v, want nil: a dead heal list is best-effort", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("LoadTriggers never returned: the heal retry is not bounded")
	}
	if got := healCalls.Load(); got != 3 {
		t.Errorf("wedged heal list attempts = %d, want exactly 3 (MaxAttempts)", got)
	}
}
