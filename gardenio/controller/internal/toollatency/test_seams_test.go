package toollatency

// Len reports the current number of pending (unmatched) PreToolUse entries.
// Exposed for tests and metrics, not required for correctness.
func (p *Pairing) Len() int {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}
