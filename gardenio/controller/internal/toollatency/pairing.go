package toollatency

import (
	"sync"
	"time"
)

// defaultPairingTTL bounds how long a PreToolUse arrival waits for its
// matching PostToolUse before being evicted. A tool call that never
// completes (crashed session, dropped event) must not leak memory forever;
// anything legitimately slower than this does not get a "derived" duration
// (it still gets recorded, just without a latency sample) rather than
// blocking eviction indefinitely.
const defaultPairingTTL = 30 * time.Minute

// defaultPairingMaxEntries caps the pairing map's size regardless of TTL, so
// a burst of PreToolUse events with no matching Post (or a TTL that's too
// generous for the actual event rate) can't grow memory unbounded. When full,
// the oldest entry is evicted to make room — a derived-duration miss under
// extreme load is an acceptable degradation, unbounded memory growth is not.
const defaultPairingMaxEntries = 10000

// Pairing is a bounded, TTL-evicted map from tool_use_id to the PreToolUse
// arrival time, used to compute a "derived" duration on the matching
// PostToolUse when Claude's own duration_ms is absent (P0 duration_source
// semantics). Safe for concurrent use.
type Pairing struct {
	mu          sync.Mutex
	ttl         time.Duration
	maxEntries  int
	entries     map[string]time.Time
	insertOrder []string // FIFO eviction order when maxEntries is hit
}

// NewPairing constructs a Pairing with the default TTL and size cap.
func NewPairing() *Pairing {
	return &Pairing{
		ttl:        defaultPairingTTL,
		maxEntries: defaultPairingMaxEntries,
		entries:    make(map[string]time.Time),
	}
}

// Observe records a PreToolUse arrival at time `at` for toolUseID. Calling it
// twice for the same id overwrites the earlier timestamp (the latest
// PreToolUse wins — a tool_use_id is not expected to repeat, but this keeps
// behavior well-defined if one does).
func (p *Pairing) Observe(toolUseID string, at time.Time) {
	if p == nil || toolUseID == "" {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictExpiredLocked(at)
	if _, exists := p.entries[toolUseID]; !exists {
		if len(p.entries) >= p.maxEntries {
			p.evictOldestLocked()
		}
		p.insertOrder = append(p.insertOrder, toolUseID)
	}
	p.entries[toolUseID] = at
}

// Take looks up and removes the PreToolUse arrival time for toolUseID,
// returning (duration since that arrival to `at`, true) if found and not
// expired, or (0, false) otherwise. A PostToolUse that arrives after the
// pairing already evicted (TTL or size cap) gets no derived duration — the
// caller falls back to recording the call without a latency sample rather
// than fabricating one.
func (p *Pairing) Take(toolUseID string, at time.Time) (time.Duration, bool) {
	if p == nil || toolUseID == "" {
		return 0, false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	start, ok := p.entries[toolUseID]
	if !ok {
		return 0, false
	}
	delete(p.entries, toolUseID)
	if at.Sub(start) > p.ttl {
		return 0, false
	}
	return at.Sub(start), true
}

// evictExpiredLocked drops entries older than ttl relative to `now`. Called
// opportunistically on every Observe rather than on a background ticker —
// the pairing map is only ever written from the consumer's single delivery
// goroutine, so no extra goroutine/lifecycle is needed.
func (p *Pairing) evictExpiredLocked(now time.Time) {
	if len(p.insertOrder) == 0 {
		return
	}
	cutoff := 0
	for _, id := range p.insertOrder {
		start, ok := p.entries[id]
		if !ok {
			cutoff++
			continue
		}
		if now.Sub(start) > p.ttl {
			delete(p.entries, id)
			cutoff++
			continue
		}
		break // insertOrder is chronological; first non-expired stops the scan
	}
	if cutoff > 0 {
		p.insertOrder = p.insertOrder[cutoff:]
	}
}

// evictOldestLocked drops the single oldest live entry to make room under
// defaultPairingMaxEntries.
func (p *Pairing) evictOldestLocked() {
	for len(p.insertOrder) > 0 {
		oldest := p.insertOrder[0]
		p.insertOrder = p.insertOrder[1:]
		if _, ok := p.entries[oldest]; ok {
			delete(p.entries, oldest)
			return
		}
	}
}
