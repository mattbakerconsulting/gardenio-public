package spawnguard

import (
	"sync"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if c.RateMax <= 0 || c.TripThreshold <= 0 {
		t.Fatalf("defaults should enable both checks: %+v", c)
	}
	if c.TripThreshold <= c.RateMax {
		t.Fatalf("trip threshold should exceed rate cap so rate limit fires first: rate=%d trip=%d", c.RateMax, c.TripThreshold)
	}
}

func TestDisabledByZero(t *testing.T) {
	g := New(Config{})
	for i := range 1000 {
		ok, r := g.Allow()
		if !ok {
			t.Fatalf("zero config must always allow; got reason=%s at i=%d", r, i)
		}
		g.Record()
	}
}

func TestRateLimitBlocksWhenExceeded(t *testing.T) {
	now := time.Unix(0, 0)
	g := New(Config{RateMax: 3, RateWindow: time.Minute})
	g.SetNowFunc(func() time.Time { return now })

	for i := range 3 {
		ok, _ := g.Allow()
		if !ok {
			t.Fatalf("allow %d should pass", i)
		}
		g.Record()
	}
	ok, reason := g.Allow()
	if ok {
		t.Fatalf("4th allow should be rejected")
	}
	if reason != ReasonRateLimit {
		t.Fatalf("expected ReasonRateLimit, got %q", reason)
	}

	now = now.Add(time.Minute + time.Second)
	ok, _ = g.Allow()
	if !ok {
		t.Fatalf("after window rollover, should allow")
	}
}

func TestCircuitBreakerTripsAndCoolsDown(t *testing.T) {
	now := time.Unix(0, 0)
	g := New(Config{
		RateMax:       100,
		RateWindow:    time.Minute,
		TripThreshold: 5,
		TripWindow:    time.Minute,
		TripCooldown:  5 * time.Minute,
	})
	g.SetNowFunc(func() time.Time { return now })

	for i := range 5 {
		ok, _ := g.Allow()
		if !ok {
			t.Fatalf("pre-trip allow %d should pass", i)
		}
		g.Record()
	}
	if !g.IsOpen() {
		t.Fatalf("breaker should be open after 5 records")
	}
	ok, reason := g.Allow()
	if ok || reason != ReasonBreakerOpen {
		t.Fatalf("tripped breaker must reject with breaker_open; ok=%v reason=%q", ok, reason)
	}

	now = now.Add(4 * time.Minute)
	ok, reason = g.Allow()
	if ok || reason != ReasonBreakerOpen {
		t.Fatalf("before cooldown expiry, still open; ok=%v reason=%q", ok, reason)
	}

	now = now.Add(2 * time.Minute)
	ok, reason = g.Allow()
	if !ok {
		t.Fatalf("after cooldown, breaker should auto-reset and allow; reason=%q", reason)
	}
	if g.IsOpen() {
		t.Fatalf("breaker should be closed after cooldown")
	}
}

func TestBreakerTakesPrecedenceOverRate(t *testing.T) {
	now := time.Unix(0, 0)
	g := New(Config{
		RateMax:       2,
		RateWindow:    time.Minute,
		TripThreshold: 3,
		TripWindow:    time.Minute,
		TripCooldown:  time.Hour,
	})
	g.SetNowFunc(func() time.Time { return now })

	for range 3 {
		g.Record()
	}
	if !g.IsOpen() {
		t.Fatalf("should be tripped")
	}
	ok, reason := g.Allow()
	if ok {
		t.Fatalf("must reject when tripped")
	}
	if reason != ReasonBreakerOpen {
		t.Fatalf("expected breaker_open, got %q", reason)
	}
}

func TestConcurrentRecordRespectsCap(t *testing.T) {
	now := time.Unix(0, 0)
	g := New(Config{RateMax: 10, RateWindow: time.Minute})
	g.SetNowFunc(func() time.Time { return now })

	var (
		allowed int
		mu      sync.Mutex
		wg      sync.WaitGroup
	)
	for range 100 {
		wg.Go(func() {
			ok, _ := g.Allow()
			if ok {
				g.Record()
				mu.Lock()
				allowed++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if allowed > 10 {
		t.Fatalf("allowed %d exceeds cap of 10 under concurrent access", allowed)
	}
	if allowed == 0 {
		t.Fatalf("expected some allowances in concurrent test")
	}
}

func TestResetClearsEverything(t *testing.T) {
	g := New(Config{RateMax: 2, RateWindow: time.Minute, TripThreshold: 3, TripWindow: time.Minute, TripCooldown: time.Hour})
	for range 2 {
		ok, _ := g.Allow()
		if !ok {
			t.Fatalf("allow should pass under cap")
		}
		g.Record()
	}
	g.Record() // third record trips the breaker at TripThreshold=3
	if !g.IsOpen() {
		t.Fatalf("expected tripped")
	}
	if g.Count() != 2 {
		t.Fatalf("pre-reset count = %d, want 2", g.Count())
	}
	g.Reset()
	if g.IsOpen() {
		t.Fatalf("reset should close breaker")
	}
	if g.Count() != 0 {
		t.Fatalf("reset should zero the rate count")
	}
	ok, _ := g.Allow()
	if !ok {
		t.Fatalf("reset should allow new spawns")
	}
}

func TestCountReturnsCurrentWindow(t *testing.T) {
	now := time.Unix(0, 0)
	g := New(Config{RateMax: 100, RateWindow: time.Minute})
	g.SetNowFunc(func() time.Time { return now })

	for range 5 {
		ok, _ := g.Allow()
		if !ok {
			t.Fatalf("allow should pass under cap")
		}
	}
	if c := g.Count(); c != 5 {
		t.Fatalf("count = %d, want 5", c)
	}
	now = now.Add(2 * time.Minute)
	if c := g.Count(); c != 0 {
		t.Fatalf("after window, count = %d, want 0", c)
	}
}
