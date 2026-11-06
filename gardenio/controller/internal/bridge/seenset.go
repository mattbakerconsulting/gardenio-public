package bridge

import (
	"sort"
	"sync"
	"time"
)

// Dedup horizon and sweep cadence for the SSE-event seenSet maps. The
// slack-bridge is a process-lifetime handler (RegisterHandlers), so a plain
// map[string]bool of every distinct bead ID ever observed grows monotonically
// = a slow unbounded heap leak until pod restart (ka-0000008059). These bound
// it the same way AgentStreamManager.gcStaleSessions bounds its session maps.
const (
	// seenTTL is how long a dedup entry is retained. SSE reconnect (the reason
	// dedup exists) re-delivers within seconds-to-minutes, so a 24h window is
	// far beyond any reconnect gap while still evicting the long tail of beads
	// never seen again. Matches streamSessionMaxAge.
	seenTTL = 24 * time.Hour
	// seenMaxEntries is a hard safety cap that bounds worst-case memory between
	// gc sweeps, in case a pathological burst of distinct keys arrives inside a
	// single sweep interval.
	seenMaxEntries = 100_000
	// seenGCInterval is the background sweep cadence, matching the
	// AgentStreamManager gc ticker.
	seenGCInterval = 5 * time.Minute
)

// seenSet is a TTL- and size-bounded dedup set for SSE event keys. It replaces
// the unbounded map[string]bool dedup used by the long-lived bridge watchers so
// the "already notified" bookkeeping cannot leak for the process lifetime.
//
// It carries its own mutex: the dedup state is independent of the other maps a
// watcher guards (e.g. Agents.taskAssignees, BeadActivity.pending), so no
// caller needs to hold both locks atomically.
type seenSet struct {
	ttl        time.Duration
	maxEntries int

	mu sync.Mutex
	at map[string]time.Time // dedup key → first-observed time
}

// newSeenSet creates a seenSet with the given retention TTL and hard entry cap.
// A non-positive maxEntries disables the between-sweep hard cap (TTL eviction
// still applies).
func newSeenSet(ttl time.Duration, maxEntries int) *seenSet {
	return &seenSet{
		ttl:        ttl,
		maxEntries: maxEntries,
		at:         make(map[string]time.Time),
	}
}

// markSeen records key and reports whether it was already present within the
// TTL window. A present-but-expired entry is treated as not seen and its
// timestamp is refreshed, which both preserves the dedup-window semantics and
// self-heals a stale entry the gc sweep has not yet reached.
func (s *seenSet) markSeen(key string) bool {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()

	if t, ok := s.at[key]; ok && now.Sub(t) <= s.ttl {
		return true
	}

	// Safety cap: keep the map bounded even if the periodic sweep has not run.
	if s.maxEntries > 0 && len(s.at) >= s.maxEntries {
		s.gcLocked(now)
		if len(s.at) >= s.maxEntries {
			s.evictOldestLocked(len(s.at) - s.maxEntries + 1)
		}
	}

	s.at[key] = now
	return false
}

// release drops a key that markSeen just claimed, so a caller that CLAIMS the
// key before performing a fallible side effect can hand the claim back when
// that side effect fails and let a later redelivery retry it.
//
// Without this, "claim then send" turns a transient send failure into a
// permanent drop, while "send then claim" opens a double-send window. Claim,
// send, release-on-error keeps exactly-once in the happy path and still retries
// (ka-0000002131, where the side effect is an accountability Slack DM that must
// never be silently lost).
func (s *seenSet) release(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.at, key)
}

// gc removes all entries older than the TTL. Intended to be called on a
// periodic ticker.
func (s *seenSet) gc() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcLocked(time.Now())
}

func (s *seenSet) gcLocked(now time.Time) {
	for k, t := range s.at {
		if now.Sub(t) > s.ttl {
			delete(s.at, k)
		}
	}
}

// evictOldestLocked drops the n oldest entries. Used only as a last-resort cap
// when a burst of fresh keys fills the map faster than the sweep evicts them;
// re-notifying an evicted key later is acceptable degradation versus unbounded
// growth. Callers must hold s.mu.
func (s *seenSet) evictOldestLocked(n int) {
	if n <= 0 {
		return
	}
	type kt struct {
		key string
		t   time.Time
	}
	entries := make([]kt, 0, len(s.at))
	for k, t := range s.at {
		entries = append(entries, kt{k, t})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].t.Before(entries[j].t) })
	if n > len(entries) {
		n = len(entries)
	}
	for i := 0; i < n; i++ {
		delete(s.at, entries[i].key)
	}
}

// len reports the current entry count (test/observability helper).
func (s *seenSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.at)
}
