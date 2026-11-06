package scheduler

import (
	"context"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// withGrandfatherEpoch temporarily overrides the package-level grandfather
// epoch for a test and restores it after, via the shared test seam. The epoch
// is a var precisely so the migration floor is testable without depending on
// where "now" falls relative to the production rollout date.
func withGrandfatherEpoch(t *testing.T, epoch time.Time) {
	t.Helper()
	SetWallClockGrandfatherEpochForTest(t, epoch)
}

// TestGardenerWallClockBaseline exercises the grandfather-epoch floor applied to
// the wall_clock baseline (ka-0000006967), the per-gardener stagger offset added
// to the FLOORED cohort (ka-0000000134), AND the per-bead revival override that
// makes in-place gardener revival possible (ka-0000006465). The baseline is
// latest(WallClockSince, CreatedAt), floored to epoch+stagger when that latest
// term predates the epoch, and zero when BOTH CreatedAt and WallClockSince are
// zero (wall_clock not enforceable).
func TestGardenerWallClockBaseline(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	withGrandfatherEpoch(t, epoch)
	preEpoch := epoch.Add(-30 * 24 * time.Hour)  // 30d before rollout
	postEpoch := epoch.Add(10 * 24 * time.Hour)  // 10d after rollout
	reviveLate := epoch.Add(40 * 24 * time.Hour) // 40d after rollout (a later revive)
	reviveEarly := epoch.Add(5 * 24 * time.Hour) // 5d after rollout (before postEpoch)
	preEpochRevive := epoch.Add(-3 * time.Hour)  // a revive stamped before the epoch (defensive)
	const window = 720 * time.Hour

	// The floored (pre-epoch, un-revived) case lands at epoch + a deterministic
	// per-bead offset, so derive the expected instant from the same helper the
	// code uses.
	preEpochWant := epoch.Add(gardenerWallClockStaggerOffset("gd-cap", window))

	cases := []struct {
		name           string
		createdAt      time.Time
		wallClockSince time.Time
		want           time.Time
	}{
		// --- Legacy behavior preserved (no revival override) ---
		{"zero_both_stays_zero", time.Time{}, time.Time{}, time.Time{}},
		{"pre_epoch_floored_to_epoch_plus_stagger", preEpoch, time.Time{}, preEpochWant},
		{"exactly_epoch_uses_created_at", epoch, time.Time{}, epoch},
		{"post_epoch_uses_created_at", postEpoch, time.Time{}, postEpoch},

		// --- Revival override (ka-0000006465): baseline advances to the revive
		//     instant when WallClockSince is the latest term. A post-epoch revive
		//     is used verbatim (unfloored, unstaggered). ---
		{"revive_after_created_uses_revive", postEpoch, reviveLate, reviveLate},
		{"revive_after_epoch_grandfathered_bead_uses_revive", preEpoch, reviveLate, reviveLate},
		{"revive_before_created_keeps_created", postEpoch, reviveEarly, postEpoch},
		{"revive_makes_zero_created_enforceable", time.Time{}, reviveLate, reviveLate},
		// A revive stamped before the epoch never SHORTENS the window: the epoch
		// floor (+stagger) still wins (defensive — production always stamps
		// time.Now(), well past the epoch, but the max()+floor must hold regardless).
		{"pre_epoch_revive_floored_to_epoch_plus_stagger", time.Time{}, preEpochRevive, preEpochWant},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gardenerWallClockBaseline("gd-cap", tc.createdAt, tc.wallClockSince, window)
			if !got.Equal(tc.want) {
				t.Fatalf("gardenerWallClockBaseline(%q, createdAt=%v, wallClockSince=%v, %v) = %v, want %v",
					"gd-cap", tc.createdAt, tc.wallClockSince, window, got, tc.want)
			}
			// A floored baseline must never precede the epoch (stagger is
			// non-negative — the window only extends, never shortens).
			latest := tc.createdAt
			if tc.wallClockSince.After(latest) {
				latest = tc.wallClockSince
			}
			if !latest.IsZero() && latest.Before(epoch) && got.Before(epoch) {
				t.Fatalf("floored baseline %v precedes epoch %v — stagger must be non-negative", got, epoch)
			}
		})
	}
}

// TestGardenerWallClockStaggerOffset pins the stagger contract that keeps the
// grandfathered cohort from collapsing onto one close date (ka-0000000134):
// deterministic, in [0, window) at hour resolution, non-negative, and zero for
// a non-positive / sub-hour window. The determinism + spread properties are
// what actually break the cliff.
func TestGardenerWallClockStaggerOffset(t *testing.T) {
	const window = 720 * time.Hour

	// Deterministic: same id → same offset across repeated calls.
	for i := range 100 {
		if got := gardenerWallClockStaggerOffset("ka-0000001271", window); got != gardenerWallClockStaggerOffset("ka-0000001271", window) {
			t.Fatalf("offset not deterministic on iteration %d: %v", i, got)
		}
	}

	// In range [0, window) at whole-hour resolution.
	for _, id := range []string{"ka-0000001271", "ka-0000005781", "ka-0000001426", "ka-0000001526", "ka-0000004081"} {
		off := gardenerWallClockStaggerOffset(id, window)
		if off < 0 || off >= window {
			t.Fatalf("offset %v for %q out of [0, %v)", off, id, window)
		}
		if off%time.Hour != 0 {
			t.Fatalf("offset %v for %q not whole-hour", off, id)
		}
	}

	// Non-positive / sub-hour window ⇒ zero (nothing to stagger).
	for _, w := range []time.Duration{0, -1 * time.Hour, 59 * time.Minute} {
		if off := gardenerWallClockStaggerOffset("gd-cap", w); off != 0 {
			t.Fatalf("window %v: got offset %v, want 0", w, off)
		}
	}
}

// gardenerIDCorpus returns n deterministically generated bead ids of the real
// shape (`gd-` + 10 alphanumerics). The seed is FIXED, so every run sees the
// identical corpus: the statistical assertions below are deterministic and can
// never flake, while still describing thousands of ids rather than a handful.
func gardenerIDCorpus(seed int64, n int) []string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	r := rand.New(rand.NewSource(seed))
	ids := make([]string, n)
	var b strings.Builder
	for i := range ids {
		b.Reset()
		b.WriteString("gd-")
		for j := 0; j < 10; j++ {
			b.WriteByte(alphabet[r.Intn(len(alphabet))])
		}
		ids[i] = b.String()
	}
	return ids
}

// TestGardenerWallClock_SpreadsCohortAcrossDays replaces an earlier test that
// pinned the five real pre-epoch gardener ids and asserted those particular
// five happened to close on distinct year-days. That asserted a property of
// the SAMPLE, not of the code: five ids into ~30 close-days collide ~29% of
// the time by the birthday effect, so the old green was luck and it turned red
// the moment the id corpus changed. It could not have caught a regression.
//
// The invariant the stagger exists for (the ka-0000000134 cliff) is SPREAD:
// grandfathered gardeners sharing one window and one epoch baseline must scatter
// across the window instead of all closing on the same day.
func TestGardenerWallClock_SpreadsCohortAcrossDays(t *testing.T) {
	epoch := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	withGrandfatherEpoch(t, epoch)
	// 720h window => offsets are whole hours in [0,720) => 30 distinct close-days.
	const (
		window    = 720 * time.Hour
		closeDays = int(window / (24 * time.Hour)) // 30
	)
	// A creation time well before the epoch so every member is floored and
	// therefore staggered (the only path that applies an offset).
	created := epoch.Add(-30 * 24 * time.Hour)

	closeDay := func(id string) int {
		baseline := gardenerWallClockBaseline(id, created, time.Time{}, window)
		if baseline.Before(epoch) {
			t.Fatalf("%s baseline %v precedes epoch %v", id, baseline, epoch)
		}
		return baseline.Add(window).YearDay()
	}

	t.Run("uniform across the whole close-day space", func(t *testing.T) {
		// N ids into K = 30 close-days: expected mu = N/K, and per-day
		// occupancy is Binomial(N, 1/K) so sigma = sqrt(N*(1/K)*(1-1/K)).
		//   N = 3000, K = 30  =>  mu = 100, sigma ~= 9.8
		// The bound is mu +/- 50, i.e. +/-5.1 sigma -- breached by a correct
		// hash with probability ~1e-5, and the fixed seed makes it
		// deterministic anyway. A regression is nowhere near: the pre-stagger
		// cliff put all 3000 on ONE day (30x mu) and emptied the other 29.
		const (
			n      = 3000
			mean   = n / closeDays // 100
			maxOcc = mean + 50     // 150
			minOcc = mean - 50     // 50
		)
		occ := map[int]int{}
		for _, id := range gardenerIDCorpus(11, n) {
			occ[closeDay(id)]++
		}
		if len(occ) != closeDays {
			t.Errorf("got %d distinct close-days, want all %d occupied", len(occ), closeDays)
		}
		for day, count := range occ {
			if count > maxOcc || count < minOcc {
				t.Errorf("year-day %d holds %d of %d gardeners, want within [%d,%d] (mu=%d)",
					day, count, n, minOcc, maxOcc, mean)
			}
		}
	})

	t.Run("spread does not depend on character order", func(t *testing.T) {
		// The ka- archive namespace is zero-padded sequential digits, so ids
		// that are permutations of one another are common. An order-blind
		// hash (a plain byte sum) maps every permutation of a multiset onto
		// ONE offset, recreating the exact cliff this stagger was written to
		// break. FNV-1a is order-sensitive and scatters them.
		//
		// Note FNV-1a's low bit is the XOR of every input byte's low bit and
		// so is permutation-invariant, which parity-locks h%720 to even hour
		// offsets for such a family. That halves the reachable OFFSETS but not
		// the reachable DAYS: each day still contains 12 even hours, so all 30
		// days stay in play and E[distinct] = 30*(1-(29/30)^200) ~= 30.0.
		const (
			perms    = 200
			wantDays = 25 // of 30; an order-blind hash scores 1
		)
		r := rand.New(rand.NewSource(13))
		digits := []byte("0123456789")
		seen := map[string]bool{}
		days := map[int]bool{}
		for len(seen) < perms {
			r.Shuffle(len(digits), func(i, j int) { digits[i], digits[j] = digits[j], digits[i] })
			id := "gd-" + string(digits)
			if seen[id] {
				continue
			}
			seen[id] = true
			days[closeDay(id)] = true
		}
		if len(days) < wantDays {
			t.Errorf("%d permutations of one digit multiset landed on only %d distinct close-days, want >= %d "+
				"(order-sensitive hash ~= 30; an order-blind hash scores 1)", perms, len(days), wantDays)
		}
	})

	t.Run("realistic cohort lands on many distinct days", func(t *testing.T) {
		// The operational question the cliff posed: given a real grandfathered
		// cohort, how many distinct close-days does it occupy? For c ids into
		// K days a uniform hash gives E[distinct] = K*(1-((K-1)/K)^c):
		//   c = 5, K = 30  =>  30*(1-(29/30)^5) ~= 4.68 of 5
		// Averaged over 2000 independent cohorts the sample mean is tight, so
		// 4.3 sits below expectation and far above the cliff itself, which
		// collapsed every member onto one day (score 1.0).
		const (
			cohorts    = 2000
			cohortSize = 5
			wantMean   = 4.3
		)
		ids := gardenerIDCorpus(12, cohorts*cohortSize)
		total := 0
		for i := 0; i < cohorts; i++ {
			seen := map[int]bool{}
			for _, id := range ids[i*cohortSize : (i+1)*cohortSize] {
				seen[closeDay(id)] = true
			}
			total += len(seen)
		}
		got := float64(total) / float64(cohorts)
		if got < wantMean {
			t.Errorf("mean distinct close-days per %d-gardener cohort = %.2f, want >= %.1f "+
				"(uniform hash ~= 4.68; the pre-stagger cliff scores 1.0)", cohortSize, got, wantMean)
		}
	})
}

// TestGardenerStop_WallClock exercises StopKindWallClock end-to-end through
// EvaluateGardenerStopEntry, with the grandfather epoch pinned to a fixed PAST
// instant so every case is deterministic regardless of the real clock. It
// covers the regressions the CreatedAt wiring fixes/guards:
//   - zero CreatedAt ⇒ never fires (the pre-wiring no-op — preserved so a
//     trigger with no surfaced creation time cannot be closed by wall_clock).
//   - a post-epoch gardener within its window ⇒ not-met.
//   - a post-epoch gardener past its window ⇒ met.
//   - a gardener OLDER than its wall_clock in absolute terms but whose bead
//     predates the epoch ⇒ met/not-met depending ONLY on time since the epoch,
//     never on the (ancient) creation time — the "don't retro-close live
//     gardeners" guarantee.
func TestGardenerStop_WallClock(t *testing.T) {
	// Epoch 1000h in the past: a 168h window measured from the epoch has
	// already elapsed, but a fresh (epoch-1h) window has not.
	epoch := time.Now().Add(-1000 * time.Hour).UTC()
	withGrandfatherEpoch(t, epoch)

	entry := StopEntry{Kind: StopKindWallClock, Duration: "168h"} // 1 week

	cases := []struct {
		name      string
		createdAt time.Time
		want      bool
	}{
		{
			name:      "zero_created_at_never_fires",
			createdAt: time.Time{},
			want:      false,
		},
		{
			name:      "post_epoch_within_window_not_met",
			createdAt: time.Now().Add(-24 * time.Hour), // 1 day old, under 168h
			want:      false,
		},
		{
			name:      "post_epoch_past_window_fires",
			createdAt: time.Now().Add(-200 * time.Hour), // >168h old, after epoch
			want:      true,
		},
		{
			name: "ancient_pre_epoch_bead_measured_from_epoch_fires",
			// 100 days old — far older than 168h — and predating the epoch. The
			// floor resets its window to the epoch; the epoch is 1000h past, so
			// 168h HAS elapsed since it ⇒ fires. Crucially it fires on
			// time-since-EPOCH, not time-since-creation.
			createdAt: epoch.Add(-100 * 24 * time.Hour),
			want:      true,
		},
		{
			name: "pre_epoch_bead_within_epoch_window_grandfathered",
			// Same ancient bead, but with the epoch only 1h in the past (the
			// realistic just-after-rollout state): the floored window has NOT
			// elapsed, so the gardener is grandfathered and must NOT fire even
			// though it is 100 days old.
			createdAt: time.Now().Add(-100 * 24 * time.Hour),
			want:      false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "pre_epoch_bead_within_epoch_window_grandfathered" {
				// This case needs a fresh (recent) epoch to demonstrate the
				// grace window; override just for it.
				withGrandfatherEpoch(t, time.Now().Add(-1*time.Hour).UTC())
			}
			got := EvaluateGardenerStopEntry(context.Background(), nil, nil, GardenerStopContext{
				BeadID:    "gd-cap",
				CreatedAt: tc.createdAt,
			}, entry)
			if got != tc.want {
				t.Fatalf("createdAt=%v: got %v, want %v", tc.createdAt, got, tc.want)
			}
		})
	}
}

// TestGardenerStop_WallClock_ReviveResetsWindow is the reproduction + fix proof
// for bug ka-0000006465: a gardener that has hard-stopped on wall_clock could
// never be revived in place because the baseline was max(created_at, epoch) with
// no resettable term. This models the exact-match instance the bug cites,
// gd-3Ub6rYSqUY (created 2026-06-21 → floored to the epoch → +168h elapsed):
//
//  1. BEFORE the revive (WallClockSince zero) the gardener is past its window →
//     wall_clock fires (the terminal close, correct at close time).
//  2. A bare reopen that does NOT stamp WallClockSince still fires on the very
//     next evaluation — this is the reopen→re-close loop the bug describes and
//     the reason the runbook's un-retire procedure could not work.
//  3. Stamping WallClockSince to now (what the reopen / second-life paths now
//     do) advances the baseline past the expired window → wall_clock no longer
//     fires. Revival in place is now deliverable.
func TestGardenerStop_WallClock_ReviveResetsWindow(t *testing.T) {
	// Epoch 1000h in the past so a 168h window measured from it has elapsed —
	// exactly the gd-3Ub6rYSqUY shape (bead older than the epoch, floored to it).
	epoch := time.Now().Add(-1000 * time.Hour).UTC()
	withGrandfatherEpoch(t, epoch)

	entry := StopEntry{Kind: StopKindWallClock, Duration: "168h"}
	// A pre-epoch creation time (like gd-3Ub6rYSqUY's 2026-06-21) — floored to
	// the epoch, so the effective window started 1000h ago and 168h has elapsed.
	createdAt := epoch.Add(-30 * 24 * time.Hour)

	eval := func(wallClockSince time.Time) bool {
		return EvaluateGardenerStopEntry(context.Background(), nil, nil, GardenerStopContext{
			BeadID:         "gd-cap",
			CreatedAt:      createdAt,
			WallClockSince: wallClockSince,
		}, entry)
	}

	// (1) At close time: wall_clock has elapsed → fires. (This is correct — the
	// close itself is not the bug; the bug is the inability to revive after it.)
	if !eval(time.Time{}) {
		t.Fatalf("pre-revive: expected wall_clock to fire on an expired window")
	}

	// (2) A bare reopen with NO baseline stamp re-fires immediately — the
	// reopen→re-close loop. This is the buggy status quo, asserted so a
	// regression that drops the WallClockSince stamp is caught here.
	if !eval(time.Time{}) {
		t.Fatalf("bare reopen (no WallClockSince) must still fire — this is the loop the fix ends")
	}

	// (3) The fix: stamping WallClockSince to now advances the baseline past the
	// expired window, so the revived gardener is inside a fresh window and
	// wall_clock does NOT fire.
	if eval(time.Now()) {
		t.Fatalf("post-revive (WallClockSince=now): wall_clock must NOT fire — revival in place is the fix")
	}

	// And the fresh window is still enforceable: fast-forward past it (stamp the
	// baseline 200h ago) and wall_clock fires again — the revived gardener is
	// bounded exactly as a fresh one, not made immortal.
	if !eval(time.Now().Add(-200 * time.Hour)) {
		t.Fatalf("a revived gardener past its fresh 168h window must fire again (revival is not immortality)")
	}
}

// TestGardenerStop_WallClock_UnparseableOrNonPositive confirms a malformed or
// non-positive duration never fires, regardless of age.
func TestGardenerStop_WallClock_UnparseableOrNonPositive(t *testing.T) {
	withGrandfatherEpoch(t, time.Now().Add(-10000*time.Hour).UTC())
	old := time.Now().Add(-365 * 24 * time.Hour)
	for _, dur := range []string{"", "forever", "0s", "-1h"} {
		entry := StopEntry{Kind: StopKindWallClock, Duration: dur}
		got := EvaluateGardenerStopEntry(context.Background(), nil, nil, GardenerStopContext{
			BeadID:    "gd-cap",
			CreatedAt: old,
		}, entry)
		if got {
			t.Fatalf("duration=%q: got true, want false (never fire on bad duration)", dur)
		}
	}
}
