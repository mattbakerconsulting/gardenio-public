package reconciler

import (
	"testing"
	"time"
)

func TestCreationRateLimiter_AllowsUpToMax(t *testing.T) {
	rl := NewCreationRateLimiter(3, 5*time.Minute)
	for i := 0; i < 3; i++ {
		if !rl.Allow() {
			t.Fatalf("Allow() returned false on creation %d, expected true", i+1)
		}
		rl.Record()
	}
	if rl.Allow() {
		t.Error("Allow() returned true after max creations reached, expected false")
	}
}

func TestCreationRateLimiter_EvictsExpiredTimestamps(t *testing.T) {
	now := time.Now()
	rl := NewCreationRateLimiter(3, 5*time.Minute)
	rl.nowFunc = func() time.Time { return now }

	// Fill up the window.
	for i := 0; i < 3; i++ {
		rl.Record()
	}
	if rl.Allow() {
		t.Fatal("expected rate limit to be hit")
	}

	// Advance past the window.
	rl.nowFunc = func() time.Time { return now.Add(6 * time.Minute) }
	if !rl.Allow() {
		t.Error("expected Allow() = true after window expired")
	}
	if rl.Count() != 0 {
		t.Errorf("expected Count() = 0 after expiry, got %d", rl.Count())
	}
}

func TestCreationRateLimiter_ZeroMaxDisablesLimiting(t *testing.T) {
	rl := NewCreationRateLimiter(0, 5*time.Minute)
	for i := 0; i < 100; i++ {
		if !rl.Allow() {
			t.Fatalf("Allow() returned false with max=0 on iteration %d", i)
		}
		rl.Record()
	}
}

func TestCreationRateLimiter_CountReflectsWindow(t *testing.T) {
	now := time.Now()
	rl := NewCreationRateLimiter(10, 5*time.Minute)
	rl.nowFunc = func() time.Time { return now }

	rl.Record()
	rl.Record()
	if c := rl.Count(); c != 2 {
		t.Errorf("Count() = %d, want 2", c)
	}

	// Advance 3 minutes — still within window.
	rl.nowFunc = func() time.Time { return now.Add(3 * time.Minute) }
	rl.Record()
	if c := rl.Count(); c != 3 {
		t.Errorf("Count() = %d, want 3", c)
	}

	// Advance past the original two records' window.
	rl.nowFunc = func() time.Time { return now.Add(6 * time.Minute) }
	if c := rl.Count(); c != 1 {
		t.Errorf("Count() = %d, want 1 (first two should be evicted)", c)
	}
}
