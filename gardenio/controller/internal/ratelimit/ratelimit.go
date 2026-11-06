// Package ratelimit implements a per-key token-bucket rate limiter used to
// gate the htmx widget surface in beads-viewer.
//
// A bucket per key (e.g. per widget-instance) tracks a token count that
// refills at a constant rate up to a configured burst. Allow returns true
// and consumes a token when one is available; false otherwise.
//
// The implementation uses no goroutines and no channels; callers periodically
// run SweepBefore to drop idle keys so the map does not grow without bound.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a per-key token-bucket rate limiter.
//
// Zero value is not usable; construct with New.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64 // tokens per second
	burst   float64 // bucket capacity
	now     func() time.Time
}

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New returns a Limiter that refills at ratePerSec tokens per second up to
// the configured burst. burst must be >= 1; ratePerSec may be zero (no
// refill — once a bucket is exhausted, new keys must be created).
func New(burst int, ratePerSec float64) *Limiter {
	if burst < 1 {
		burst = 1
	}
	if ratePerSec < 0 {
		ratePerSec = 0
	}
	return &Limiter{
		buckets: map[string]*bucket{},
		rate:    ratePerSec,
		burst:   float64(burst),
		now:     time.Now,
	}
}

// Allow consumes one token from key's bucket and returns true. If the bucket
// has no tokens it returns false without modifying it.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.lastSeen).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.rate
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
		b.lastSeen = now
	}
	if b.tokens >= 1 {
		b.tokens -= 1
		return true
	}
	return false
}

// LastSeen returns the timestamp of the last Allow call for key, or the zero
// time when key is unknown. Used by tests + the SweepBefore garbage collector.
func (l *Limiter) LastSeen(key string) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if b, ok := l.buckets[key]; ok {
		return b.lastSeen
	}
	return time.Time{}
}

// Reset clears all per-key state. Tests use this between subtests to keep
// bucket state from leaking between cases.
func (l *Limiter) Reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buckets = map[string]*bucket{}
}

// SweepBefore drops bucket entries last seen before cutoff and returns the
// number of keys removed. Callers should run this periodically (e.g. once per
// minute) so the map does not grow without bound from one-off keys.
func (l *Limiter) SweepBefore(cutoff time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for k, b := range l.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(l.buckets, k)
			n++
		}
	}
	return n
}

// SetClock overrides the clock function. Tests set this to drive deterministic
// refill behavior; production code leaves it alone.
func (l *Limiter) SetClock(now func() time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now == nil {
		now = time.Now
	}
	l.now = now
}
