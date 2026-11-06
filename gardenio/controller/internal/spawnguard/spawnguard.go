// Package spawnguard provides a global rate cap and circuit breaker for
// agent-bead spawns. It is the first line of defense against runaway spawn
// storms (many triggers/schedules firing in parallel) — distinct from the
// reconciler's pod-creation rate limiter, which gates K8s Pod objects for
// beads that already exist.
//
// One SpawnGuard is shared by every spawn source in a process (scheduler,
// trigger engine). Callers check Allow() before creating an agent bead and
// call Record() on success. When the trip threshold is exceeded in the
// breaker window, Allow() returns false for the cooldown period regardless
// of rate — so a sustained storm degrades to zero spawns rather than
// oscillating at the rate cap.
package spawnguard

import (
	"sync"
	"time"
)

// Config parameterises a SpawnGuard. Zero-valued fields disable the
// corresponding check (useful in tests and for incremental rollout).
type Config struct {
	// RateMax is the maximum number of spawns permitted in RateWindow.
	// 0 disables rate limiting.
	RateMax    int
	RateWindow time.Duration

	// TripThreshold is the number of spawns in TripWindow that trips the
	// breaker. 0 disables the breaker.
	TripThreshold int
	TripWindow    time.Duration
	// TripCooldown is how long the breaker stays open after tripping.
	TripCooldown time.Duration
}

// DefaultConfig returns the recommended guard configuration: 30 spawns/min
// soft cap, 120 spawns/min hard ceiling with 5-minute cooldown after trip.
func DefaultConfig() Config {
	return Config{
		RateMax:       30,
		RateWindow:    time.Minute,
		TripThreshold: 120,
		TripWindow:    time.Minute,
		TripCooldown:  5 * time.Minute,
	}
}

// SpawnGuard combines a sliding-window rate limiter with a circuit breaker.
// Safe for concurrent use.
type SpawnGuard struct {
	cfg Config

	mu           sync.Mutex
	rateStamps   []time.Time
	breakerStamps []time.Time
	trippedAt    time.Time
	nowFunc      func() time.Time
}

// New creates a SpawnGuard with the given config.
func New(cfg Config) *SpawnGuard {
	return &SpawnGuard{cfg: cfg, nowFunc: time.Now}
}

// Reason describes why Allow returned false.
type Reason string

const (
	ReasonAllowed    Reason = ""
	ReasonRateLimit  Reason = "rate_limit"
	ReasonBreakerOpen Reason = "breaker_open"
)

// Allow reports whether a new spawn is permitted, atomically reserving a
// slot against the rate cap if so. Callers MUST call Record() after a
// successful spawn so it counts toward the circuit breaker. Failed spawns
// (Record not called) still consume the rate-cap slot until the window
// rolls over — this is conservative and prevents tight failure-retry
// storms from saturating spawns. The reason (when not allowed) is returned
// for logging and metrics.
func (g *SpawnGuard) Allow() (bool, Reason) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.nowFunc()

	// Breaker check (hard ceiling).
	if g.cfg.TripThreshold > 0 && !g.trippedAt.IsZero() {
		if now.Sub(g.trippedAt) < g.cfg.TripCooldown {
			return false, ReasonBreakerOpen
		}
		// Cooldown elapsed — auto-reset.
		g.trippedAt = time.Time{}
		g.breakerStamps = nil
	}

	// Rate cap check (soft cap). Reserve atomically so concurrent callers
	// cannot all observe count<max in their own Allow() before any of them
	// records a stamp.
	if g.cfg.RateMax > 0 {
		g.rateStamps = evictBefore(g.rateStamps, now.Add(-g.cfg.RateWindow))
		if len(g.rateStamps) >= g.cfg.RateMax {
			return false, ReasonRateLimit
		}
		g.rateStamps = append(g.rateStamps, now)
	}

	return true, ReasonAllowed
}

// Record marks a successful spawn against the circuit breaker. The
// rate-cap slot was already reserved by Allow(), so Record() only updates
// breaker state. Always call this after a successful spawn; failed spawns
// must NOT call Record() so transient errors don't trip the breaker.
func (g *SpawnGuard) Record() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.nowFunc()

	if g.cfg.TripThreshold > 0 {
		g.breakerStamps = append(g.breakerStamps, now)
		g.breakerStamps = evictBefore(g.breakerStamps, now.Add(-g.cfg.TripWindow))
		if len(g.breakerStamps) >= g.cfg.TripThreshold {
			g.trippedAt = now
		}
	}
}

// IsOpen reports whether the circuit breaker is currently tripped.
func (g *SpawnGuard) IsOpen() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg.TripThreshold <= 0 || g.trippedAt.IsZero() {
		return false
	}
	if g.nowFunc().Sub(g.trippedAt) >= g.cfg.TripCooldown {
		g.trippedAt = time.Time{}
		g.breakerStamps = nil
		return false
	}
	return true
}

// Reset clears the tripped state and all timestamps. For tests and manual
// recovery.
func (g *SpawnGuard) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.trippedAt = time.Time{}
	g.rateStamps = nil
	g.breakerStamps = nil
}

// Count returns the current number of spawns within the rate window.
// Useful for observability.
func (g *SpawnGuard) Count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.rateStamps = evictBefore(g.rateStamps, g.nowFunc().Add(-g.cfg.RateWindow))
	return len(g.rateStamps)
}

// SetNowFunc overrides the clock. Test-only.
func (g *SpawnGuard) SetNowFunc(f func() time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.nowFunc = f
}

// evictBefore drops timestamps older than cutoff, preserving order. The
// slice is assumed monotonically non-decreasing.
func evictBefore(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	if i == 0 {
		return ts
	}
	return ts[i:]
}
