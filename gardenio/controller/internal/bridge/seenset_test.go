package bridge

import (
	"testing"
	"time"
)

func TestSeenSetDedup(t *testing.T) {
	s := newSeenSet(time.Hour, 0)

	if s.markSeen("a") {
		t.Fatal("first markSeen(a) = true, want false (not yet seen)")
	}
	if !s.markSeen("a") {
		t.Fatal("second markSeen(a) = false, want true (already seen)")
	}
	if s.markSeen("b") {
		t.Fatal("first markSeen(b) = true, want false")
	}
	if got := s.len(); got != 2 {
		t.Fatalf("len = %d, want 2", got)
	}
}

func TestSeenSetTTLEviction(t *testing.T) {
	s := newSeenSet(time.Hour, 0)
	s.markSeen("stale")
	s.markSeen("fresh")

	// Backdate "stale" past the TTL horizon, keep "fresh" recent.
	s.mu.Lock()
	s.at["stale"] = time.Now().Add(-2 * time.Hour)
	s.mu.Unlock()

	s.gc()

	if got := s.len(); got != 1 {
		t.Fatalf("after gc, len = %d, want 1 (stale evicted)", got)
	}
	// A gc'd key is treated as unseen again — dedup window has passed.
	if s.markSeen("stale") {
		t.Fatal("markSeen(stale) after eviction = true, want false")
	}
	// Fresh key survived and still dedups.
	if !s.markSeen("fresh") {
		t.Fatal("markSeen(fresh) = false, want true (still within TTL)")
	}
}

// TestSeenSetExpiredEntryTreatedAsUnseen guards the self-heal path: a key whose
// entry has aged past the TTL but has not yet been gc'd must be reported as
// not-seen and re-stamped, not falsely deduped.
func TestSeenSetExpiredEntryTreatedAsUnseen(t *testing.T) {
	s := newSeenSet(time.Hour, 0)
	s.markSeen("k")

	s.mu.Lock()
	s.at["k"] = time.Now().Add(-2 * time.Hour)
	s.mu.Unlock()

	if s.markSeen("k") {
		t.Fatal("markSeen(k) with expired entry = true, want false")
	}
	// Now re-stamped fresh, so it dedups again.
	if !s.markSeen("k") {
		t.Fatal("markSeen(k) immediately after re-stamp = false, want true")
	}
}

// TestSeenSetHardCap verifies the between-sweep safety cap bounds the map even
// when TTL eviction has nothing to evict (all entries fresh). This is the
// property that turns the unbounded leak (ka-0000008059) into bounded memory.
func TestSeenSetHardCap(t *testing.T) {
	const cap = 50
	s := newSeenSet(time.Hour, cap)

	for i := 0; i < cap*4; i++ {
		s.markSeen(string(rune('A')) + "-" + time.Duration(i).String())
	}

	if got := s.len(); got > cap {
		t.Fatalf("len = %d, want <= cap %d (hard cap not enforced)", got, cap)
	}
}
