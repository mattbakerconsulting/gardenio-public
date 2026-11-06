package scheduler

import (
	"testing"
	"time"
)

// TestGardenerWallClockBaseline_Exported verifies the exported baseline view
// (ka-0000007024, ka-0000001868) matches the ENGINE's baseline exactly: a
// pre-epoch gardener is floored to the epoch PLUS its per-gardener stagger offset,
// a post-epoch gardener measures from its real created_at, and a zero created_at
// stays zero (wall_clock not enforceable). The CLI's already-expired warning
// relies on reproducing the engine — including the stagger — so it never
// false-alarms a live gardener.
func TestGardenerWallClockBaseline_Exported(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	withGrandfatherEpoch(t, epoch)
	preEpoch := epoch.Add(-30 * 24 * time.Hour)
	postEpoch := epoch.Add(10 * 24 * time.Hour)
	const window = 720 * time.Hour
	const id = "gd-cap"
	floored := epoch.Add(gardenerWallClockStaggerOffset(id, window))

	cases := []struct {
		name      string
		createdAt time.Time
		want      time.Time
	}{
		{"zero_created_at_stays_zero", time.Time{}, time.Time{}},
		{"pre_epoch_floored_to_epoch_plus_stagger", preEpoch, floored},
		{"exactly_epoch_uses_epoch", epoch, epoch},
		{"post_epoch_uses_created_at", postEpoch, postEpoch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GardenerWallClockBaseline(id, tc.createdAt, window)
			if !got.Equal(tc.want) {
				t.Fatalf("GardenerWallClockBaseline(%q, %v, %v) = %v, want %v", id, tc.createdAt, window, got, tc.want)
			}
			// The exported view must be byte-for-byte the engine's baseline —
			// that identity is the whole guarantee behind the CLI verdict.
			if eng := gardenerWallClockBaseline(id, tc.createdAt, time.Time{}, window); !got.Equal(eng) {
				t.Fatalf("exported baseline %v != engine baseline %v — CLI would drift from the engine", got, eng)
			}
		})
	}
}

// TestGardenerWallClockExpiry is the crux of the bug (ka-0000007024) with the
// stagger correction (ka-0000001868): a gardener created BEFORE the grandfather
// epoch measures its wall_clock from the epoch PLUS its per-gardener stagger, so
// the exported expiry equals the instant the engine actually closes it — not the
// shared unstaggered floor that false-alarmed eight live gardeners.
func TestGardenerWallClockExpiry(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	withGrandfatherEpoch(t, epoch)
	const id = "gd-cap"

	// A pre-epoch gardener with a 168h window: baseline floors to
	// epoch+stagger(id,168h), so the ceiling lands there +168h — the SAME
	// instant the engine's StopKindWallClock fires.
	preEpochCreated := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	expiry, ok := GardenerWallClockExpiry(id, preEpochCreated, 168*time.Hour)
	if !ok {
		t.Fatal("expected a computable expiry for a pre-epoch gardener with a 168h window")
	}
	wantExpiry := epoch.Add(gardenerWallClockStaggerOffset(id, 168*time.Hour)).Add(168 * time.Hour)
	if !expiry.Equal(wantExpiry) {
		t.Fatalf("pre-epoch 168h expiry = %v, want %v (epoch+stagger+window, not created_at+window)", expiry, wantExpiry)
	}

	// A post-epoch gardener measures from its real created_at, untouched by the
	// floor (and therefore by the stagger): created 2026-07-20 + 168h.
	postEpochCreated := time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC)
	expiry, ok = GardenerWallClockExpiry(id, postEpochCreated, 168*time.Hour)
	if !ok {
		t.Fatal("expected a computable expiry for a post-epoch gardener")
	}
	if want := postEpochCreated.Add(168 * time.Hour); !expiry.Equal(want) {
		t.Fatalf("post-epoch 168h expiry = %v, want %v (measured from created_at)", expiry, want)
	}

	// Not computable: zero created_at (wall_clock not enforceable).
	if _, ok := GardenerWallClockExpiry(id, time.Time{}, 168*time.Hour); ok {
		t.Error("zero created_at must yield ok=false (wall_clock not enforceable)")
	}
	// Not computable: non-positive window.
	if _, ok := GardenerWallClockExpiry(id, postEpochCreated, 0); ok {
		t.Error("zero window must yield ok=false")
	}
	if _, ok := GardenerWallClockExpiry(id, postEpochCreated, -1*time.Hour); ok {
		t.Error("negative window must yield ok=false")
	}
}

// TestGardenerWallClockExpiry_EightGardenerFalseExpiredRegression is the direct
// regression for ka-0000001868: the eight real live gardeners that shared the
// 2026-08-02 unstaggered floor must each report a DISTINCT, still-future expiry
// via the exported CLI primitive — none flagged expired at the shared floor
// instant. Before the fix the exported view dropped the stagger, so all eight
// reported EXPIRED at epoch+720h (2026-08-02T00:00Z) while every one was still
// firing days-to-weeks later.
func TestGardenerWallClockExpiry_EightGardenerFalseExpiredRegression(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	withGrandfatherEpoch(t, epoch)
	const window = 720 * time.Hour
	// "now" at the shared unstaggered floor: epoch+720h = 2026-08-02T00:00Z —
	// the instant the buggy CLI reported all eight EXPIRED.
	sharedFloor := epoch.Add(window)

	// The eight gardeners from the bug report, created well before the epoch.
	cohort := []string{
		"ka-0000001271", "ka-0000001526", "ka-0000006579", "ka-0000001510",
		"gd-sh3TT9XFsr", "ka-0000005781", "ka-0000004081", "ka-0000001426",
	}
	created := epoch.Add(-30 * 24 * time.Hour)

	seenExpiry := map[int64]string{}
	for _, id := range cohort {
		expiry, ok := GardenerWallClockExpiry(id, created, window)
		if !ok {
			t.Fatalf("%s: expected a computable expiry", id)
		}
		// The load-bearing assertion: NOT expired at the shared floor instant.
		if !expiry.After(sharedFloor) {
			t.Fatalf("%s: expiry %v is at/before the shared floor %v — false EXPIRED (the bug)", id, expiry, sharedFloor)
		}
		// Distinct expiries: the stagger de-synchronizes the cohort.
		key := expiry.Unix()
		if other, dup := seenExpiry[key]; dup {
			t.Fatalf("%s and %s share expiry %v — stagger not applied in the exported view", id, other, expiry)
		}
		seenExpiry[key] = id
	}
	if len(seenExpiry) != len(cohort) {
		t.Fatalf("expected %d distinct expiries, got %d", len(cohort), len(seenExpiry))
	}
}

// TestGardenerWallClockBaselineSince covers the exported wall_clock_since-aware
// baseline (bug ka-0000005774) with the stagger correction (ka-0000001868): the
// enable guard must reproduce the ENGINE's baseline INCLUDING the per-gardener
// stagger on the floored cohort AND a fresh window a --rebaseline just stamped,
// so a gardener revived to now no longer reads as expired off its ancient
// created_at.
func TestGardenerWallClockBaselineSince(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	withGrandfatherEpoch(t, epoch)
	const window = 720 * time.Hour
	const id = "gd-cap"
	preEpoch := epoch.Add(-30 * 24 * time.Hour)
	postEpoch := epoch.Add(10 * 24 * time.Hour)
	rebaselined := epoch.Add(20 * 24 * time.Hour) // a post-epoch revive instant
	floored := epoch.Add(gardenerWallClockStaggerOffset(id, window))

	cases := []struct {
		name           string
		createdAt      time.Time
		wallClockSince time.Time
		want           time.Time
	}{
		{"both_zero_stays_zero", time.Time{}, time.Time{}, time.Time{}},
		{"pre_epoch_no_revive_floored_plus_stagger", preEpoch, time.Time{}, floored},
		{"pre_epoch_rebaselined_measures_from_revive", preEpoch, rebaselined, rebaselined},
		{"post_epoch_no_revive_uses_created_at", postEpoch, time.Time{}, postEpoch},
		{"revive_wins_when_later", postEpoch, rebaselined, rebaselined},
		{"created_at_wins_when_later", rebaselined, postEpoch, rebaselined},
		{"zero_created_at_but_revived_is_enforceable", time.Time{}, rebaselined, rebaselined},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := GardenerWallClockBaselineSince(id, tc.createdAt, tc.wallClockSince, window)
			if !got.Equal(tc.want) {
				t.Fatalf("GardenerWallClockBaselineSince(%q, %v, %v, %v) = %v, want %v", id, tc.createdAt, tc.wallClockSince, window, got, tc.want)
			}
			// Identity with the engine baseline (the whole guarantee).
			if eng := gardenerWallClockBaseline(id, tc.createdAt, tc.wallClockSince, window); !got.Equal(eng) {
				t.Fatalf("exported baseline-since %v != engine baseline %v", got, eng)
			}
		})
	}

	// The whole point of the override: a pre-epoch gardener whose window is long
	// expired is made live again by a wall_clock_since = now, so the expiry moves
	// from the past to now+window.
	t.Run("rebaseline_moves_expiry_into_future", func(t *testing.T) {
		now := epoch.Add(25 * 24 * time.Hour)
		staleExpiry, ok := GardenerWallClockExpirySince(id, preEpoch, time.Time{}, 168*time.Hour)
		if !ok || !staleExpiry.Before(now) {
			t.Fatalf("pre-epoch 168h must be expired before now: expiry=%v now=%v ok=%v", staleExpiry, now, ok)
		}
		freshExpiry, ok := GardenerWallClockExpirySince(id, preEpoch, now, 168*time.Hour)
		if !ok {
			t.Fatal("rebaselined expiry must be computable")
		}
		if want := now.Add(168 * time.Hour); !freshExpiry.Equal(want) {
			t.Fatalf("rebaselined expiry = %v, want now+168h %v", freshExpiry, want)
		}
		if !freshExpiry.After(now) {
			t.Error("rebaselined expiry must be in the FUTURE — the whole point of --rebaseline")
		}
	})
}

// TestGardenerWallClockExpiry_MatchesEngineClose is the load-bearing invariant:
// the exported expiry the CLI warns from must line up EXACTLY with the instant
// the ENGINE's StopKindWallClock actually fires — including the per-gardener
// stagger the exported view now carries (ka-0000001868). baseline+window must
// equal the exported expiry, and the baseline must equal the engine floor
// (epoch+stagger).
func TestGardenerWallClockExpiry_MatchesEngineClose(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	withGrandfatherEpoch(t, epoch)
	const window = 168 * time.Hour
	const id = "gd-cap"
	preEpochCreated := epoch.Add(-30 * 24 * time.Hour)

	expiry, ok := GardenerWallClockExpiry(id, preEpochCreated, window)
	if !ok {
		t.Fatal("expected computable expiry")
	}
	// time.Since(baseline) >= d is the engine's fire condition (gardener_stop.go),
	// and the exported baseline IS the engine baseline, so baseline+window must
	// equal the exported expiry to the nanosecond.
	baseline := GardenerWallClockBaseline(id, preEpochCreated, window)
	if got := baseline.Add(window); !got.Equal(expiry) {
		t.Fatalf("exported expiry %v != baseline+window %v — CLI warning would drift from the engine close", expiry, got)
	}
	// And that baseline equals what the engine floors to (epoch+stagger).
	if want := epoch.Add(gardenerWallClockStaggerOffset(id, window)); !baseline.Equal(want) {
		t.Fatalf("exported baseline %v != epoch+stagger %v — not reproducing the engine floor", baseline, want)
	}
}
