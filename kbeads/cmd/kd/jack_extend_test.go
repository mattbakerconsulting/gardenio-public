package main

import (
	"testing"
	"time"
)

// TestJackExtendedExpiry_ExtendsFromExistingDeadline is the regression guard for the
// bug where `kd jack extend` reset the expiry to now+ttl, silently discarding the time
// still left on a jack (and retroactively resurrecting an already-expired jack).
func TestJackExtendedExpiry_ExtendsFromExistingDeadline(t *testing.T) {
	t0 := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		existing time.Time // zero => no/empty stored expiry
		now      time.Time
		ttl      time.Duration
		want     time.Time
	}{
		{
			// The core bug: a 24h jack, 1h in, extended by 1h. Pre-fix this returned
			// now+1h = T0+2h — 23h EARLIER than the jack's own deadline. The extension
			// must push the deadline OUT to (T0+24h)+1h.
			name:     "live jack extends from its future deadline",
			existing: t0.Add(24 * time.Hour),
			now:      t0.Add(1 * time.Hour),
			ttl:      1 * time.Hour,
			want:     t0.Add(25 * time.Hour),
		},
		{
			// An already-lapsed deadline must not be resurrected retroactively: the
			// base clamps to now, so the jack gets ttl from the present, not from the
			// stale past deadline.
			name:     "expired jack extends from now, not the stale deadline",
			existing: t0.Add(-2 * time.Hour),
			now:      t0,
			ttl:      1 * time.Hour,
			want:     t0.Add(1 * time.Hour),
		},
		{
			// Missing/unparseable stored expiry falls back to now+ttl.
			name:     "no stored expiry falls back to now",
			existing: time.Time{},
			now:      t0,
			ttl:      3 * time.Hour,
			want:     t0.Add(3 * time.Hour),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			existingStr := ""
			if !tc.existing.IsZero() {
				existingStr = tc.existing.Format(time.RFC3339)
			}
			got := jackExtendedExpiry(existingStr, tc.now, tc.ttl)
			if !got.Equal(tc.want) {
				t.Fatalf("jackExtendedExpiry(%q, %v, %v) = %v; want %v",
					existingStr, tc.now, tc.ttl, got, tc.want)
			}
			// The new expiry must never move earlier than the present.
			if got.Before(tc.now) {
				t.Fatalf("new expiry %v is before now %v", got, tc.now)
			}
		})
	}
}

// TestJackExtendedExpiry_NeverDiscardsRemainingTime asserts the general invariant that
// extending a still-live jack never yields a deadline earlier than the one it had.
func TestJackExtendedExpiry_NeverDiscardsRemainingTime(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	existing := now.Add(48 * time.Hour)
	got := jackExtendedExpiry(existing.Format(time.RFC3339), now, 1*time.Hour)
	if got.Before(existing) {
		t.Fatalf("extend moved deadline earlier: got %v, existing %v", got, existing)
	}
}
