package scheduler

import (
	"testing"
	"time"
)

// mustTime parses an RFC3339 timestamp or fails the test.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return parsed
}

func TestReconcileFires_AllMatched(t *testing.T) {
	// Hourly cron; three observed fires landing a couple minutes late each.
	start := mustTime(t, "2026-07-01T00:00:00Z")
	end := mustTime(t, "2026-07-01T03:30:00Z")
	observed := []time.Time{
		mustTime(t, "2026-07-01T01:02:00Z"),
		mustTime(t, "2026-07-01T02:01:00Z"),
		mustTime(t, "2026-07-01T03:03:00Z"),
	}
	// "0 * * * *" fires at 01:00, 02:00, 03:00 within (start, end].
	res, err := ReconcileFires("0 * * * *", "", observed, start, end, 0)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res.ExpectedCount != 3 {
		t.Fatalf("expected 3 cron fires, got %d (%+v)", res.ExpectedCount, res.Expected)
	}
	if res.MatchedCount != 3 || res.MissedCount != 0 || res.ExtraCount != 0 {
		t.Fatalf("want 3 matched/0 missed/0 extra, got %d/%d/%d",
			res.MatchedCount, res.MissedCount, res.ExtraCount)
	}
	// Lateness should be recorded and positive.
	for _, ef := range res.Expected {
		if !ef.Matched {
			t.Fatalf("fire %s should be matched", ef.At)
		}
		if ef.LatenessS <= 0 {
			t.Fatalf("fire %s expected positive lateness, got %d", ef.At, ef.LatenessS)
		}
	}
}

func TestReconcileFires_MissedFire(t *testing.T) {
	// Hourly cron, but the 02:00 fire never happened -> one missed.
	start := mustTime(t, "2026-07-01T00:00:00Z")
	end := mustTime(t, "2026-07-01T03:30:00Z")
	observed := []time.Time{
		mustTime(t, "2026-07-01T01:00:00Z"),
		mustTime(t, "2026-07-01T03:00:00Z"),
	}
	res, err := ReconcileFires("0 * * * *", "", observed, start, end, 0)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res.MatchedCount != 2 || res.MissedCount != 1 {
		t.Fatalf("want 2 matched/1 missed, got %d/%d", res.MatchedCount, res.MissedCount)
	}
	// The missed one must be the 02:00 slot.
	var missed []time.Time
	for _, ef := range res.Expected {
		if !ef.Matched {
			missed = append(missed, ef.At)
		}
	}
	if len(missed) != 1 || missed[0].UTC() != mustTime(t, "2026-07-01T02:00:00Z") {
		t.Fatalf("expected 02:00 missed, got %v", missed)
	}
}

func TestReconcileFires_OverFiring(t *testing.T) {
	// Cron says once at 01:00, but the gardener fired three extra times ->
	// over-firing (Matt's second point). Extra should capture the surplus.
	start := mustTime(t, "2026-07-01T00:00:00Z")
	end := mustTime(t, "2026-07-01T02:00:00Z")
	observed := []time.Time{
		mustTime(t, "2026-07-01T01:00:00Z"), // matches
		mustTime(t, "2026-07-01T01:20:00Z"), // extra (> tolerance from 01:00)
		mustTime(t, "2026-07-01T01:40:00Z"), // extra
		mustTime(t, "2026-07-01T00:15:00Z"), // extra (before the only fire)
	}
	res, err := ReconcileFires("0 1 * * *", "", observed, start, end, 5*time.Minute)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res.ExpectedCount != 1 || res.MatchedCount != 1 {
		t.Fatalf("want 1 expected/1 matched, got %d/%d", res.ExpectedCount, res.MatchedCount)
	}
	if res.ExtraCount != 3 {
		t.Fatalf("want 3 extra (over-firing), got %d: %v", res.ExtraCount, res.Extra)
	}
}

func TestReconcileFires_ToleranceBoundary(t *testing.T) {
	// An observed fire exactly at tolerance matches; one just past does not.
	start := mustTime(t, "2026-07-01T00:00:00Z")
	end := mustTime(t, "2026-07-01T02:00:00Z")
	tol := 10 * time.Minute
	observed := []time.Time{
		mustTime(t, "2026-07-01T01:10:00Z"), // exactly +10m from 01:00 -> match
	}
	res, err := ReconcileFires("0 1 * * *", "", observed, start, end, tol)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res.MatchedCount != 1 || res.ExtraCount != 0 {
		t.Fatalf("boundary fire should match: got %d matched/%d extra", res.MatchedCount, res.ExtraCount)
	}

	observedLate := []time.Time{
		mustTime(t, "2026-07-01T01:11:00Z"), // +11m -> past tolerance -> extra + missed
	}
	res2, err := ReconcileFires("0 1 * * *", "", observedLate, start, end, tol)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res2.MatchedCount != 0 || res2.MissedCount != 1 || res2.ExtraCount != 1 {
		t.Fatalf("past-tolerance fire: want 0 matched/1 missed/1 extra, got %d/%d/%d",
			res2.MatchedCount, res2.MissedCount, res2.ExtraCount)
	}
}

func TestReconcileFires_GreedyOneToOne(t *testing.T) {
	// Two expected fires close together, two observed. Greedy nearest must not
	// let a single observed fire satisfy both expected slots.
	start := mustTime(t, "2026-06-30T23:59:00Z")
	end := mustTime(t, "2026-07-01T01:00:00Z")
	// "0,30 0 * * *" -> 00:00 and 00:30 (both after the 23:59 window start).
	observed := []time.Time{
		mustTime(t, "2026-07-01T00:02:00Z"),
		mustTime(t, "2026-07-01T00:31:00Z"),
	}
	res, err := ReconcileFires("0,30 0 * * *", "", observed, start, end, 10*time.Minute)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res.ExpectedCount != 2 || res.MatchedCount != 2 || res.ExtraCount != 0 {
		t.Fatalf("want 2 expected/2 matched/0 extra, got %d/%d/%d",
			res.ExpectedCount, res.MatchedCount, res.ExtraCount)
	}
}

func TestReconcileFires_Timezone(t *testing.T) {
	// "0 9 * * *" in America/Los_Angeles = 16:00 or 17:00 UTC depending on DST.
	// July -> PDT (UTC-7) -> 16:00Z.
	start := mustTime(t, "2026-07-01T00:00:00Z")
	end := mustTime(t, "2026-07-01T23:59:00Z")
	observed := []time.Time{
		mustTime(t, "2026-07-01T16:00:00Z"),
	}
	res, err := ReconcileFires("0 9 * * *", "America/Los_Angeles", observed, start, end, 0)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res.ExpectedCount != 1 {
		t.Fatalf("want 1 expected fire, got %d: %+v", res.ExpectedCount, res.Expected)
	}
	if res.Expected[0].At.UTC() != mustTime(t, "2026-07-01T16:00:00Z") {
		t.Fatalf("PDT 9am should be 16:00Z, got %s", res.Expected[0].At)
	}
	if res.MatchedCount != 1 {
		t.Fatalf("want the fire matched, got %d matched", res.MatchedCount)
	}
}

// TestReconcileFires_DayOfWeekTimezoneDST pins the day-of-week + DST-sensitive
// timezone case that bug ka-0000004392 accused of being off-by-one. The
// OSS-Sanitization Audit Gardener (trigger ka-0000007535) fires "13 9 * * 1" in
// America/Los_Angeles — Monday 09:13 local. The drift report claimed the
// engine computed an EXPECTED fire on a SATURDAY, contradicting day-of-week=1.
//
// It does not. Both the summer (PDT, UTC-7) and winter (PST, UTC-8) Monday
// fires land on the correct UTC instant AND on a UTC weekday of Monday. The
// cron matcher keys day-of-week off Go's time.Weekday() (0=Sunday), which is
// the same 0=Sunday convention as the cron field, so Monday=1 with no off-by-
// one; and Next() walks in the trigger's local zone before normalizing to UTC,
// so the DST offset is applied in the right order. This test is the proof: the
// mechanism is correct, and the "Saturday" in the report was report prose, not
// engine output.
func TestReconcileFires_DayOfWeekTimezoneDST(t *testing.T) {
	cron := "13 9 * * 1" // Monday 09:13 local
	tz := "America/Los_Angeles"

	cases := []struct {
		name    string
		start   string
		end     string
		wantUTC string // the single expected fire, as a UTC RFC3339 instant
	}{
		{
			// Summer: 2026-07-27 is a Monday; PDT (UTC-7) => 16:13Z, still Monday.
			// This is the exact instant the drift report mislabeled "Saturday".
			name:    "summer_PDT",
			start:   "2026-07-26T00:00:00Z",
			end:     "2026-07-28T00:00:00Z",
			wantUTC: "2026-07-27T16:13:00Z",
		},
		{
			// Winter: 2026-01-05 is a Monday; PST (UTC-8) => 17:13Z, still Monday.
			// Different UTC offset proves the tz math is applied per-instant, not
			// with a fixed offset.
			name:    "winter_PST",
			start:   "2026-01-04T00:00:00Z",
			end:     "2026-01-06T00:00:00Z",
			wantUTC: "2026-01-05T17:13:00Z",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := mustTime(t, tc.start)
			end := mustTime(t, tc.end)
			want := mustTime(t, tc.wantUTC)

			res, err := ReconcileFires(cron, tz, nil, start, end, 0)
			if err != nil {
				t.Fatalf("ReconcileFires: %v", err)
			}
			if res.ExpectedCount != 1 {
				t.Fatalf("want exactly 1 expected Monday fire, got %d: %+v",
					res.ExpectedCount, res.Expected)
			}
			got := res.Expected[0].At.UTC()
			if !got.Equal(want) {
				t.Fatalf("expected fire UTC instant: want %s, got %s",
					want.Format(time.RFC3339), got.Format(time.RFC3339))
			}
			// The whole point of the bug: the UTC weekday must be Monday, not
			// Saturday. Assert it explicitly so a regression that shifts the
			// day-of-week reads as a weekday mismatch, not just a time delta.
			if got.Weekday() != time.Monday {
				t.Fatalf("expected fire must fall on Monday (UTC), got %s (%s)",
					got.Weekday(), got.Format(time.RFC3339))
			}
			// And it must be Monday in the trigger's own zone too (09:13 local).
			loc, err := time.LoadLocation(tz)
			if err != nil {
				t.Fatalf("LoadLocation(%q): %v", tz, err)
			}
			local := got.In(loc)
			if local.Weekday() != time.Monday || local.Hour() != 9 || local.Minute() != 13 {
				t.Fatalf("expected fire must be Monday 09:13 local, got %s",
					local.Format(time.RFC3339))
			}
		})
	}
}

func TestReconcileFires_InvalidCron(t *testing.T) {
	start := mustTime(t, "2026-07-01T00:00:00Z")
	end := mustTime(t, "2026-07-01T01:00:00Z")
	_, err := ReconcileFires("not a cron", "", nil, start, end, 0)
	if err == nil {
		t.Fatal("expected error for malformed cron")
	}
}

func TestReconcileFires_BadWindow(t *testing.T) {
	start := mustTime(t, "2026-07-01T01:00:00Z")
	end := mustTime(t, "2026-07-01T00:00:00Z") // end before start
	_, err := ReconcileFires("0 * * * *", "", nil, start, end, 0)
	if err == nil {
		t.Fatal("expected error for inverted window")
	}
}

func TestReconcileFires_NoObservedAllMissed(t *testing.T) {
	// A gardener that never fired: every expected slot is missed. This is the
	// "looks alive (evaluated) but never wakes" case the thread is about. The end
	// is well past the last slot (06:00 + 30m > 10m tolerance) so no slot is still
	// inside its grace period — every one is unambiguously overdue.
	start := mustTime(t, "2026-07-01T00:00:00Z")
	end := mustTime(t, "2026-07-01T06:30:00Z")
	res, err := ReconcileFires("0 * * * *", "", nil, start, end, 0)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if res.ExpectedCount != 6 || res.MissedCount != 6 || res.MatchedCount != 0 || res.PendingCount != 0 {
		t.Fatalf("want 6 expected/6 missed/0 matched/0 pending, got %d/%d/%d/%d",
			res.ExpectedCount, res.MissedCount, res.MatchedCount, res.PendingCount)
	}
}

// TestReconcileFires_PendingBoundaryFire pins the fix for ka-0000001791: an
// unmatched cron slot whose scheduled time is younger than the tolerance
// relative to the window end is NOT YET OVERDUE — a matching observed fire could
// still legitimately arrive up to At+tolerance, which is past the window end — so
// it must be counted PENDING, never MISSED. This is the false-DARK the
// gardener-bugfix release_owner_live probe hit: the owner's 03:13 fire was 75s old
// against a 600s tolerance when the reconcile window ended at 03:14:15, yet it
// was scored as a residual miss, flipping the lane to DARK.
func TestReconcileFires_PendingBoundaryFire(t *testing.T) {
	start := mustTime(t, "2026-09-01T00:13:00Z")
	end := mustTime(t, "2026-09-01T03:14:15Z")
	tol := 10 * time.Minute
	// Cron "13,43 * * * *" fires at :13 and :43 each hour. Every slot up to 02:43
	// has an observed fire; the trailing 03:13 slot (75s before the window end) has
	// none yet — it is inside its grace period, not a miss.
	observed := []time.Time{
		mustTime(t, "2026-09-01T00:43:00Z"),
		mustTime(t, "2026-09-01T01:13:00Z"),
		mustTime(t, "2026-09-01T01:43:00Z"),
		mustTime(t, "2026-09-01T02:13:00Z"),
		mustTime(t, "2026-09-01T02:43:00Z"),
	}
	res, err := ReconcileFires("13,43 * * * *", "", observed, start, end, tol)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	// Expected slots in (00:13, 03:14:15]: 00:43,01:13,01:43,02:13,02:43,03:13 = 6.
	if res.ExpectedCount != 6 {
		t.Fatalf("want 6 expected slots, got %d: %+v", res.ExpectedCount, res.Expected)
	}
	if res.MatchedCount != 5 || res.MissedCount != 0 || res.PendingCount != 1 {
		t.Fatalf("want 5 matched/0 missed/1 pending, got %d/%d/%d",
			res.MatchedCount, res.MissedCount, res.PendingCount)
	}
	// The single pending slot must be the 03:13 boundary fire, flagged Pending and
	// unmatched (never counted as a miss).
	var pending []time.Time
	for _, ef := range res.Expected {
		if ef.Pending {
			if ef.Matched {
				t.Fatalf("pending slot %s must not also be matched", ef.At)
			}
			pending = append(pending, ef.At)
		}
	}
	if len(pending) != 1 || pending[0].UTC() != mustTime(t, "2026-09-01T03:13:00Z") {
		t.Fatalf("want 03:13 pending, got %v", pending)
	}
	// Invariant: expected == matched + missed + pending.
	if res.ExpectedCount != res.MatchedCount+res.MissedCount+res.PendingCount {
		t.Fatalf("invariant broken: %d != %d+%d+%d",
			res.ExpectedCount, res.MatchedCount, res.MissedCount, res.PendingCount)
	}
}

// TestReconcileFires_PendingBecomesMissedPastGrace is the twin of the pending
// test: once the window end is far enough past the same trailing slot that its
// full grace period has elapsed (no observed fire ever arrived), it flips from
// PENDING to a genuine MISS — the fix must not permanently mask a truly dark
// trailing slot, only defer the verdict until its tolerance has run out.
func TestReconcileFires_PendingBecomesMissedPastGrace(t *testing.T) {
	start := mustTime(t, "2026-09-01T02:00:00Z")
	// 03:13 slot is now 12 minutes before the window end (> 10m tolerance), so its
	// grace period has elapsed with no match: a real miss.
	end := mustTime(t, "2026-09-01T03:25:00Z")
	tol := 10 * time.Minute
	observed := []time.Time{
		mustTime(t, "2026-09-01T02:13:00Z"),
		mustTime(t, "2026-09-01T02:43:00Z"),
	}
	res, err := ReconcileFires("13,43 * * * *", "", observed, start, end, tol)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	// Expected in (02:00, 03:25]: 02:13,02:43,03:13 = 3. 03:13 is unmatched and
	// past grace → MISSED, not pending.
	if res.MatchedCount != 2 || res.MissedCount != 1 || res.PendingCount != 0 {
		t.Fatalf("want 2 matched/1 missed/0 pending, got %d/%d/%d",
			res.MatchedCount, res.MissedCount, res.PendingCount)
	}
}

func TestReconcileFires_TruncationCap(t *testing.T) {
	// Every-minute cron over a very wide window trips the safety cap.
	start := mustTime(t, "2026-01-01T00:00:00Z")
	end := mustTime(t, "2026-12-31T00:00:00Z")
	res, err := ReconcileFires("* * * * *", "", nil, start, end, 0)
	if err != nil {
		t.Fatalf("ReconcileFires: %v", err)
	}
	if !res.Truncated {
		t.Fatal("expected Truncated=true for every-minute cron over a year")
	}
	if res.ExpectedCount > maxExpectedFires {
		t.Fatalf("expected count capped at %d, got %d", maxExpectedFires, res.ExpectedCount)
	}
}
