package main

import (
	"testing"
	"time"
)

// fakeClock (scripted timestamps) is defined in init_stdout_tail_test.go and
// reused here.

func newTestSwitchTracker(clk *fakeClock) *switchTracker {
	// terminalCap 6 mirrors the production switchTerminalCap.
	s := newSwitchTracker(3, 10*time.Minute, 5*time.Second, 2*time.Minute, 6)
	s.now = clk.now
	return s
}

func TestSwitchTracker_BelowThresholdNoBackoff(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_000_000, 0)}
	s := newTestSwitchTracker(clk)

	// First threshold (3) switches in-window restart immediately.
	for i := 0; i < 3; i++ {
		if d := s.Record(); d != 0 {
			t.Fatalf("switch %d: expected 0 backoff, got %s", i+1, d)
		}
		clk.advance(time.Second)
	}
}

func TestSwitchTracker_EscalatesAboveThreshold(t *testing.T) {
	clk := &fakeClock{t: time.Unix(2_000_000, 0)}
	s := newTestSwitchTracker(clk)

	// Burn the first 3 (no backoff).
	for i := 0; i < 3; i++ {
		s.Record()
		clk.advance(time.Second)
	}
	// 4th switch (excess 1) → base = 5s.
	if d := s.Record(); d != 5*time.Second {
		t.Fatalf("4th switch: expected 5s, got %s", d)
	}
	clk.advance(time.Second)
	// 5th switch (excess 2) → 10s.
	if d := s.Record(); d != 10*time.Second {
		t.Fatalf("5th switch: expected 10s, got %s", d)
	}
	clk.advance(time.Second)
	// 6th switch (excess 3) → 20s.
	if d := s.Record(); d != 20*time.Second {
		t.Fatalf("6th switch: expected 20s, got %s", d)
	}
}

func TestSwitchTracker_CapsAtMax(t *testing.T) {
	clk := &fakeClock{t: time.Unix(3_000_000, 0)}
	s := newTestSwitchTracker(clk)

	// Drive many rapid switches; backoff must never exceed max (2m).
	var last time.Duration
	for i := 0; i < 20; i++ {
		last = s.Record()
		clk.advance(time.Second)
	}
	if last != 2*time.Minute {
		t.Fatalf("expected backoff capped at 2m, got %s", last)
	}
}

func TestSwitchTracker_WindowPrunesOldSwitches(t *testing.T) {
	clk := &fakeClock{t: time.Unix(4_000_000, 0)}
	s := newTestSwitchTracker(clk)

	// 3 switches, then wait out the window — they should be pruned so the
	// next switch starts a fresh streak with no backoff.
	for i := 0; i < 3; i++ {
		s.Record()
		clk.advance(time.Second)
	}
	clk.advance(11 * time.Minute) // exceeds the 10m window
	if d := s.Record(); d != 0 {
		t.Fatalf("after window expiry: expected 0 backoff (fresh streak), got %s", d)
	}
}

// -- Part B: terminal cap (ka-0000001718) ------------------------------------

// The terminal cap trips only after switchTerminalCap NON-PRODUCTIVE switches,
// not before.
func TestSwitchTracker_TerminalCapTripsAtCap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(5_000_000, 0)}
	s := newTestSwitchTracker(clk) // terminalCap = 6

	for i := 1; i < 6; i++ {
		s.Record()
		clk.advance(time.Second)
		if s.TrippedTerminal() {
			t.Fatalf("tripped early after %d switches (cap 6)", i)
		}
	}
	// The 6th switch reaches the cap.
	s.Record()
	if !s.TrippedTerminal() {
		t.Fatalf("did not trip terminal cap at %d switches", s.SinceProductive())
	}
	if s.SinceProductive() != 6 {
		t.Fatalf("expected since-productive count 6, got %d", s.SinceProductive())
	}
}

// A slow flap — each switch spaced BEYOND the sliding window — still trips the
// terminal cap: the since-productive counter is monotonic and NOT
// window-pruned, so time alone cannot reset it (only a productive session can).
// This is the exact failure mode of the ~277s model-429 loop, whose per-cycle
// spacing can exceed the 10m sliding window and so never engages the backoff.
func TestSwitchTracker_TerminalCapCatchesSlowFlap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(6_000_000, 0)}
	s := newTestSwitchTracker(clk)

	for i := 0; i < 6; i++ {
		d := s.Record()
		// Spaced beyond the 10m window, so the sliding-window backoff never
		// engages (each Record sees a pruned, single-entry window → 0 backoff).
		if d != 0 {
			t.Fatalf("switch %d: slow flap should not engage window backoff, got %s", i+1, d)
		}
		clk.advance(11 * time.Minute)
	}
	// Despite zero window backoff throughout, the monotonic since-productive
	// counter reached the cap — a slow flap must still terminate.
	if !s.TrippedTerminal() {
		t.Fatalf("slow flap did not trip terminal cap; since-productive=%d", s.SinceProductive())
	}
}

// A productive session resets BOTH the sliding-window backoff and the terminal
// cap: the flap circuit breaker is self-healing, not a one-shot permanent park.
func TestSwitchTracker_ResetOnProductiveSessionReArmsCap(t *testing.T) {
	clk := &fakeClock{t: time.Unix(7_000_000, 0)}
	s := newTestSwitchTracker(clk)

	// Drive to the terminal cap.
	for i := 0; i < 6; i++ {
		s.Record()
		clk.advance(time.Second)
	}
	if !s.TrippedTerminal() {
		t.Fatalf("precondition: expected terminal cap tripped, since-productive=%d", s.SinceProductive())
	}

	// Productive session: reset re-arms the cap and clears backoff.
	s.Reset()
	if s.TrippedTerminal() {
		t.Fatal("terminal cap still tripped after Reset (productive session)")
	}
	if s.SinceProductive() != 0 {
		t.Fatalf("since-productive not cleared by Reset: %d", s.SinceProductive())
	}
	// Post-reset, the first switches restart immediately again (fresh streak).
	if d := s.Record(); d != 0 {
		t.Fatalf("after reset: expected 0 backoff (fresh streak), got %s", d)
	}
}

// terminalCap == 0 disables the terminal cap entirely (never trips).
func TestSwitchTracker_TerminalCapDisabled(t *testing.T) {
	clk := &fakeClock{t: time.Unix(8_000_000, 0)}
	s := newSwitchTracker(3, 10*time.Minute, 5*time.Second, 2*time.Minute, 0)
	s.now = clk.now

	for i := 0; i < 50; i++ {
		s.Record()
		clk.advance(time.Second)
	}
	if s.TrippedTerminal() {
		t.Fatal("terminal cap tripped even though disabled (cap=0)")
	}
}
