package ratelimit

import (
	"testing"
	"time"
)

func TestAllow_BurstThenExhaust(t *testing.T) {
	l := New(3, 1)
	for i := range 3 {
		if !l.Allow("k") {
			t.Fatalf("burst token %d denied", i)
		}
	}
	if l.Allow("k") {
		t.Fatal("expected denial after burst exhaustion")
	}
}

func TestAllow_Refills(t *testing.T) {
	l := New(2, 10) // 10 tokens/sec, burst 2
	now := time.Unix(0, 0)
	l.SetClock(func() time.Time { return now })

	if !l.Allow("k") {
		t.Fatal("first allow")
	}
	if !l.Allow("k") {
		t.Fatal("second allow")
	}
	if l.Allow("k") {
		t.Fatal("expected denial after burst")
	}

	now = now.Add(150 * time.Millisecond) // +1.5 tokens
	if !l.Allow("k") {
		t.Fatal("expected allow after refill")
	}
	if l.Allow("k") {
		t.Fatal("expected denial — only 0.5 tokens left")
	}
}

func TestAllow_PerKeyIsolated(t *testing.T) {
	l := New(1, 0)
	if !l.Allow("a") {
		t.Fatal("a denied")
	}
	if !l.Allow("b") {
		t.Fatal("b denied")
	}
	if l.Allow("a") {
		t.Fatal("a second call should be denied")
	}
}

func TestSweepBefore(t *testing.T) {
	l := New(1, 0)
	now := time.Unix(1000, 0)
	l.SetClock(func() time.Time { return now })

	l.Allow("old")
	now = now.Add(time.Hour)
	l.Allow("fresh")

	cutoff := now.Add(-30 * time.Minute)
	removed := l.SweepBefore(cutoff)
	if removed != 1 {
		t.Fatalf("removed=%d want 1", removed)
	}
	if !l.LastSeen("old").IsZero() {
		t.Fatal("old should be gone")
	}
	if l.LastSeen("fresh").IsZero() {
		t.Fatal("fresh should remain")
	}
}

func TestLastSeen_UnknownKey(t *testing.T) {
	l := New(1, 0)
	if !l.LastSeen("nope").IsZero() {
		t.Fatal("expected zero time for unknown key")
	}
}

func TestReset(t *testing.T) {
	l := New(1, 0)
	l.Allow("a")
	l.Reset()
	if !l.LastSeen("a").IsZero() {
		t.Fatal("Reset should clear all keys")
	}
	if !l.Allow("a") {
		t.Fatal("after Reset bucket should be fresh")
	}
}

func TestNew_ClampsBadInputs(t *testing.T) {
	l := New(0, -5)
	if l.burst != 1 {
		t.Fatalf("burst=%v want 1", l.burst)
	}
	if l.rate != 0 {
		t.Fatalf("rate=%v want 0", l.rate)
	}
}

func TestSetClock_Nil(t *testing.T) {
	l := New(1, 1)
	l.SetClock(nil) // should fall back to time.Now
	if !l.Allow("k") {
		t.Fatal("Allow after SetClock(nil) should still work")
	}
}
