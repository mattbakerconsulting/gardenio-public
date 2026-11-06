package reconciler

import (
	"sync"
	"time"
)

// CreationRateLimiter tracks pod creation timestamps in a sliding window
// and rejects new creations when the limit is exceeded. This prevents
// runaway pod stampedes where the reconciler creates too many pods too fast.
type CreationRateLimiter struct {
	maxCreations int
	window       time.Duration
	mu           sync.Mutex
	timestamps   []time.Time
	nowFunc      func() time.Time // injectable for testing
}

// NewCreationRateLimiter creates a rate limiter. If maxCreations <= 0,
// Allow() always returns true (no limiting).
func NewCreationRateLimiter(maxCreations int, window time.Duration) *CreationRateLimiter {
	return &CreationRateLimiter{
		maxCreations: maxCreations,
		window:       window,
		nowFunc:      time.Now,
	}
}

// Allow returns true if a new pod creation is permitted under the rate limit.
// It does NOT record the creation — call Record() after a successful create.
func (rl *CreationRateLimiter) Allow() bool {
	if rl.maxCreations <= 0 {
		return true
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.evictExpired()
	return len(rl.timestamps) < rl.maxCreations
}

// Record marks that a pod was just created. Call after successful creation.
func (rl *CreationRateLimiter) Record() {
	if rl.maxCreations <= 0 {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.timestamps = append(rl.timestamps, rl.nowFunc())
}

// evictExpired removes timestamps older than the window. Must be called with mu held.
func (rl *CreationRateLimiter) evictExpired() {
	cutoff := rl.nowFunc().Add(-rl.window)
	i := 0
	for i < len(rl.timestamps) && rl.timestamps[i].Before(cutoff) {
		i++
	}
	if i > 0 {
		rl.timestamps = rl.timestamps[i:]
	}
}
