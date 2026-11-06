package reconciler

// Count returns the number of creations within the current window.
func (rl *CreationRateLimiter) Count() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.evictExpired()
	return len(rl.timestamps)
}
