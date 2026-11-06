package bridge

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"
)

func newStampThrottleEngine(daemon *mockTriggerDaemon) *TriggerEngine {
	return NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
}

// A steady-state "no matches" evaluation must not re-PATCH the trigger bead
// every cycle: each PATCH emits a MutationUpdate with the full fields blob
// (gd-Mfr8MRXbYG).
func TestStampCheckedOutcome_SteadyStateWritesOnce(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	e := newStampThrottleEngine(daemon)
	ctx := context.Background()
	const outcome = "no source matches (out of window / preflight not met / already fired)"

	for i := 0; i < 20; i++ {
		if err := e.stampCheckedOutcome(ctx, "t1", outcome); err != nil {
			t.Fatalf("stamp %d: %v", i, err)
		}
	}
	if got := len(daemon.updatedIDs); got != 1 {
		t.Fatalf("20 identical-outcome stamps wrote %d times, want 1", got)
	}
}

func TestStampCheckedOutcome_WritesOnOutcomeChange(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	e := newStampThrottleEngine(daemon)
	ctx := context.Background()

	_ = e.stampCheckedOutcome(ctx, "t1", "deferred: max_concurrent lease full")
	_ = e.stampCheckedOutcome(ctx, "t1", "deferred: max_concurrent lease full")
	_ = e.stampCheckedOutcome(ctx, "t1", "no source matches (out of window / preflight not met / already fired)")
	if got := len(daemon.updatedIDs); got != 2 {
		t.Fatalf("writes = %d, want 2 (first stamp + outcome change)", got)
	}
}

func TestStampCheckedOutcome_FiresAndBootstrapAlwaysWrite(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	e := newStampThrottleEngine(daemon)
	ctx := context.Background()

	_ = e.stampCheckedOutcome(ctx, "t1", "fired: spawned 1")
	_ = e.stampCheckedOutcome(ctx, "t1", "fired: spawned 1")
	_ = e.stampLastChecked(ctx, "t1")
	_ = e.stampLastChecked(ctx, "t1")
	if got := len(daemon.updatedIDs); got != 4 {
		t.Fatalf("writes = %d, want 4 (fires and outcome-less stamps are never throttled)", got)
	}
}

func TestStampCheckedOutcome_RewritesAfterInterval(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	e := newStampThrottleEngine(daemon)
	ctx := context.Background()
	const outcome = "no source matches (out of window / preflight not met / already fired)"

	_ = e.stampCheckedOutcome(ctx, "t1", outcome)
	// Age the recorded write past the interval: the liveness signal must
	// still advance on a steady-state trigger.
	e.mu.Lock()
	st := e.checkStamps["t1"]
	st.wroteAt = st.wroteAt.Add(-checkStampMinInterval - time.Second)
	e.checkStamps["t1"] = st
	e.mu.Unlock()
	_ = e.stampCheckedOutcome(ctx, "t1", outcome)
	if got := len(daemon.updatedIDs); got != 2 {
		t.Fatalf("writes = %d, want 2 (refresh after %s)", got, checkStampMinInterval)
	}
}

// The interval must stay well inside the tightest liveness floor, or a
// throttled stamp could read as a dark trigger.
func TestCheckStampMinInterval_BelowLivenessFloors(t *testing.T) {
	for name, floor := range map[string]time.Duration{
		"gardener liveness": livenessMinStaleThreshold,
		"check liveness":    checkLivenessMinThreshold,
		"stalled-open heal": stalledOpenTriggerMinStale,
	} {
		if checkStampMinInterval*3 > floor {
			t.Errorf("checkStampMinInterval %s is not well below the %s floor %s", checkStampMinInterval, name, floor)
		}
	}
}

// A throttled stamp leaves the bead's last_checked old, but the cooldown
// window must still run from the evaluation, not from the stale bead value.
func TestLastEvaluated_ThrottledStampStillStartsCooldown(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	e := newStampThrottleEngine(daemon)
	ctx := context.Background()
	const outcome = "no source matches (out of window / preflight not met / already fired)"

	_ = e.stampCheckedOutcome(ctx, "t1", outcome)
	_ = e.stampCheckedOutcome(ctx, "t1", outcome) // throttled
	stale := time.Now().UTC().Add(-time.Hour)
	got := e.lastEvaluated(TriggerDef{ID: "t1", LastChecked: stale})
	if !got.After(stale.Add(time.Minute)) {
		t.Fatalf("lastEvaluated = %s, want a recent evaluation time", got)
	}
	if want := stale; !e.lastEvaluated(TriggerDef{ID: "other", LastChecked: want}).Equal(want) {
		t.Fatal("untouched trigger must fall back to its bead last_checked")
	}
}

// End to end through evalOne: once a stamp is throttled, the bead's
// last_checked is stale but the cooldown gate must still skip the cycle.
func TestEvalOne_ThrottledStampStillCooldownSkips(t *testing.T) {
	var logBuf bytes.Buffer
	e := NewTriggerEngine(TriggerEngineConfig{
		Daemon: &mockTriggerDaemon{},
		Logger: slog.New(slog.NewTextHandler(&logBuf, nil)),
	})
	const outcome = "no source matches (out of window / preflight not met / already fired)"
	ctx := context.Background()
	_ = e.stampCheckedOutcome(ctx, "t1", outcome)
	_ = e.stampCheckedOutcome(ctx, "t1", outcome) // throttled

	e.evalOne(ctx, &mockTriggerSource{name: "jira"}, TriggerDef{
		ID: "t1", Title: "steady", CooldownMinutes: 10,
		LastChecked: time.Now().UTC().Add(-time.Hour),
	})
	if !bytes.Contains(logBuf.Bytes(), []byte("skipped (cooldown)")) {
		t.Fatalf("expected a cooldown skip, log:\n%s", logBuf.String())
	}
}
