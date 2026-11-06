package reconciler

import (
	"testing"
	"time"
)

func TestCircuitBreakerDisabled(t *testing.T) {
	cb := NewCircuitBreaker(0, time.Minute, time.Minute)
	if cb.IsOpen() {
		t.Fatal("disabled breaker should never be open")
	}
}

func TestCircuitBreakerTrips(t *testing.T) {
	now := time.Now()
	cb := NewCircuitBreaker(3, 2*time.Minute, 5*time.Minute)
	cb.nowFunc = func() time.Time { return now }

	// Record 3 creations — should trip on the 3rd.
	for i := 0; i < 3; i++ {
		if cb.IsOpen() {
			t.Fatalf("should allow before threshold, iteration %d", i)
		}
		cb.Record()
	}

	if !cb.IsOpen() {
		t.Fatal("breaker should be open after threshold reached")
	}
}

func TestCircuitBreakerCooldownAutoReset(t *testing.T) {
	now := time.Now()
	cb := NewCircuitBreaker(2, time.Minute, 5*time.Minute)
	cb.nowFunc = func() time.Time { return now }

	cb.Record()
	cb.Record() // trips

	if !cb.IsOpen() {
		t.Fatal("should be tripped")
	}

	// Advance past cooldown.
	now = now.Add(6 * time.Minute)

	if cb.IsOpen() {
		t.Fatal("should auto-reset after cooldown")
	}
}

func TestCircuitBreakerWindowExpiry(t *testing.T) {
	now := time.Now()
	cb := NewCircuitBreaker(3, time.Minute, 5*time.Minute)
	cb.nowFunc = func() time.Time { return now }

	// Record 2 creations.
	cb.Record()
	cb.Record()

	// Advance past the window so those expire.
	now = now.Add(2 * time.Minute)

	// Record 1 more — should NOT trip (previous 2 expired).
	cb.Record()
	if cb.IsOpen() {
		t.Fatal("should not trip — old timestamps expired")
	}
}
