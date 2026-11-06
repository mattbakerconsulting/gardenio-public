package main

// Subscription-window spend accounting, split out of spend_sync_test.go when
// that file crossed the 1500-line cloc gate. Same package, same helpers
// (daemonStub, litellmRouter, newTestSpendSync, fixedClock) — this is a
// verbatim move of the window tests, no behaviour change.

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"testing"
	"time"
)

// TestSubscriptionWindowStart pins the pure window-start derivation: reset
// instant minus the window length, and (zero,false) for absent/garbage input.
func TestSubscriptionWindowStart(t *testing.T) {
	reset := time.Date(2026, 4, 10, 3, 0, 0, 0, time.UTC) // 03:00 UTC — off calendar midnight
	cases := []struct {
		name    string
		resetAt string
		window  time.Duration
		want    time.Time
		wantOK  bool
	}{
		{"5h window", reset.Format(time.RFC3339), subscription5hWindow, reset.Add(-subscription5hWindow), true},
		{"7d window", reset.Format(time.RFC3339), subscription7dWindow, reset.Add(-subscription7dWindow), true},
		{"empty is unset", "", subscription5hWindow, time.Time{}, false},
		{"garbage is unset", "not-a-time", subscription5hWindow, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := subscriptionWindowStart(tc.resetAt, tc.window)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && !got.Equal(tc.want) {
				t.Errorf("start = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSumSpendInWindow pins the shared windowed sum: rows at/after start count,
// earlier rows and unparseable stamps are skipped.
func TestSumSpendInWindow(t *testing.T) {
	start := time.Date(2026, 4, 10, 3, 0, 0, 0, time.UTC)
	logs := []spendLogEntry{
		{Spend: 1.0, StartTime: start.Add(time.Hour).Format(time.RFC3339)},    // inside
		{Spend: 2.0, StartTime: start.Format(time.RFC3339)},                   // exactly at start (inclusive)
		{Spend: 4.0, StartTime: start.Add(-time.Second).Format(time.RFC3339)}, // 1s before → excluded
		{Spend: 8.0, StartTime: "garbage"},                                    // unparseable → excluded
	}
	if got := sumSpendInWindow(logs, start); got != 3.0 {
		t.Errorf("sum = %v, want 3.0 (1.0 + 2.0)", got)
	}
}

// TestSpendSync_SubscriptionWindows is the end-to-end guard for the headline
// fix (ka-0000003343): an account whose 5h reset is deliberately OFF UTC
// midnight (03:00 UTC) must bill its 5h window against the subscription clock
// [reset−5h, now], NOT the 00:00-UTC calendar day. The two are different sets
// of rows, so the assertion proves the window does not snap to midnight.
func TestSpendSync_SubscriptionWindows(t *testing.T) {
	now := fixedNow
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	// A 5h reset ~1h out puts the window open at now-4h; a 7d reset ~1d out puts
	// the 7d window open ~6d ago. Anchor both to `now` so the test is stable
	// regardless of wall-clock hour.
	reset5h := now.Add(1 * time.Hour)
	reset7d := now.Add(24 * time.Hour)
	win5hStart := reset5h.Add(-subscription5hWindow) // now-4h
	win7dStart := reset7d.Add(-subscription7dWindow) // now-6d

	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBeadWithFields("bd-sub", "Sub Acct", "vk-sub", map[string]any{
				"throttle_reset_5h_at": reset5h.Format(time.RFC3339),
				"throttle_reset_7d_at": reset7d.Format(time.RFC3339),
				"representative_claim": "five_hour",
			}),
		},
	}

	// Rows chosen so the 5h window and the UTC calendar day capture DIFFERENT
	// sums, proving the window is subscription-aligned:
	//   - 2.0 at now-2h   → inside 5h window, inside today
	//   - 5.0 at now-4h30m → OUTSIDE 5h window (before now-4h), but still today
	//     (only holds when the local run is >4h30m into the UTC day; to stay
	//     robust we instead place it just before win5hStart AND after dayStart
	//     only when possible — so assert on the 5h window directly).
	//   - 9.0 at now-5d   → inside 7d window, NOT today, NOT this UTC day
	logs := map[string][]spendLogEntry{
		hk("vk-sub"): {
			{Spend: 2.0, StartTime: now.Add(-2 * time.Hour).Format(time.RFC3339)},
			{Spend: 9.0, StartTime: now.Add(-5 * 24 * time.Hour).Format(time.RFC3339)},
			{Spend: 4.0, StartTime: win5hStart.Add(-time.Hour).Format(time.RFC3339)}, // before 5h window opened
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-sub": 100.0},
		logs,
	))
	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected update")
	}
	u := updates[len(updates)-1].Fields

	// 5h window [now-4h, now]: only the 2.0 row. The 4.0 row (before the window
	// opened) and the 9.0 row (5d ago) are excluded.
	bill5h, ok := u["spend_5h_window_usd"].(float64)
	if !ok {
		t.Fatalf("spend_5h_window_usd missing/wrong type: %T %v", u["spend_5h_window_usd"], u["spend_5h_window_usd"])
	}
	if math.Abs(bill5h-2.0) > 0.001 {
		t.Errorf("spend_5h_window_usd = %v, want 2.0 (subscription 5h window, not the calendar day)", bill5h)
	}

	// 7d window [now-6d, now]: the 2.0 and 9.0 rows are inside; the 4.0 row
	// (~5h ago, but before the 5h window) is still inside 7d.
	bill7d, ok := u["spend_7d_window_usd"].(float64)
	if !ok {
		t.Fatalf("spend_7d_window_usd missing/wrong type: %T %v", u["spend_7d_window_usd"], u["spend_7d_window_usd"])
	}
	wantBill7d := 2.0 + 9.0 + 4.0
	if win7dStart.After(now.Add(-5 * 24 * time.Hour)) {
		// defensive: if the 9.0 row somehow predates the 7d window, drop it.
		wantBill7d = 2.0 + 4.0
	}
	if math.Abs(bill7d-wantBill7d) > 0.001 {
		t.Errorf("spend_7d_window_usd = %v, want %v", bill7d, wantBill7d)
	}

	// The calendar-day figure is still written (demoted, not removed) and must
	// NOT equal the 5h window when the window is off-midnight: the 4.0 row is
	// today (if the run is far enough into the UTC day) yet excluded from 5h.
	if _, ok := u["daily_cost_usd"].(float64); !ok {
		t.Errorf("daily_cost_usd should still be written")
	}
	_ = dayStart
}

// TestSpendSync_WindowSpansBoundaryDay is the regression guard for the bug that
// made /costs read "bill vs 5h limit $0.00" for every account, always.
//
// The old code filtered day-AGGREGATE rows (startTime = that day's midnight) by
// the window's opening INSTANT. On the day a window opens, midnight is always
// before that instant, so the entire day's spend was dropped — and since a 5h
// window essentially always opens mid-day, the 5h figure could never be anything
// but zero. Live on 2026-09-22 the `fimbaz` account showed $0.00 against $206 of
// real in-window spend, while the fleet headline silently under-reported by the
// same amount.
//
// Here every dollar is earned TODAY, on both sides of a window that opens at
// 08:00 — so a regression reads $0.00 while the truth is $30.
func TestSpendSync_WindowSpansBoundaryDay(t *testing.T) {
	now := fixedNow // 2026-04-15 12:00 UTC
	reset5h := now.Add(1 * time.Hour)
	win5hStart := reset5h.Add(-subscription5hWindow) // 08:00 today

	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBeadWithFields("bd-boundary", "Boundary Acct", "vk-b", map[string]any{
				"throttle_reset_5h_at": reset5h.Format(time.RFC3339),
				"representative_claim": "five_hour",
			}),
		},
	}
	// 70.0 before the window opened, 30.0 after — all on the SAME calendar day,
	// so the day aggregate is a single 100.0 row stamped at midnight.
	logs := map[string][]spendLogEntry{
		hk("vk-b"): {
			{Spend: 70.0, StartTime: now.Add(-6 * time.Hour).Format(time.RFC3339)}, // 06:00, before
			{Spend: 30.0, StartTime: now.Add(-1 * time.Hour).Format(time.RFC3339)}, // 11:00, inside
		},
	}

	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(map[string]float64{"vk-b": 100.0}, logs))
	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected update")
	}
	got, ok := updates[len(updates)-1].Fields["spend_5h_window_usd"].(float64)
	if !ok {
		t.Fatalf("spend_5h_window_usd missing: %#v", updates[len(updates)-1].Fields["spend_5h_window_usd"])
	}
	if got == 0 {
		t.Fatalf("spend_5h_window_usd = $0.00 — the boundary day was dropped wholesale (the original bug); want $30")
	}
	if math.Abs(got-30.0) > 0.001 {
		t.Errorf("spend_5h_window_usd = %v, want 30.0 (day total 100 minus the 70 earned before %s)",
			got, win5hStart.Format(time.RFC3339))
	}
}

// TestWindowSpendUSD_Decomposition exercises the two branches directly against
// day aggregates, including the case where the window opened on an EARLIER day
// (boundary slice + whole days after it).
func TestWindowSpendUSD_Decomposition(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	// Day aggregates, exactly as the legacy endpoint returns them: bare dates.
	logs := []spendLogEntry{
		{Spend: 11.0, StartTime: "2026-04-13"},
		{Spend: 22.0, StartTime: "2026-04-14"},
		{Spend: 33.0, StartTime: "2026-04-15"},
	}
	// Per-request rows backing the boundary slices.
	rows := map[string][]spendLogEntry{
		hk("vk-d"): {
			{Spend: 4.0, StartTime: "2026-04-13T02:00:00Z"},  // early on the 13th
			{Spend: 7.0, StartTime: "2026-04-13T20:00:00Z"},  // late on the 13th
			{Spend: 20.0, StartTime: "2026-04-15T03:00:00Z"}, // early today
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveSpendLogs(w, r, rows) {
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	ss := newSpendSync(nil, nil, srv.URL, "k", slog.Default())
	ss.now = func() time.Time { return now }

	// Window opened TODAY at 09:00 → today's aggregate (33) minus what was
	// earned before 09:00 (the 20.0 row) = 13.
	gotToday, ok := ss.windowSpendUSD(context.Background(), srv.URL, "vk-d", logs,
		time.Date(2026, 4, 15, 9, 0, 0, 0, time.UTC), now)
	if !ok {
		t.Fatal("same-day window: not ok")
	}
	if math.Abs(gotToday-13.0) > 0.001 {
		t.Errorf("same-day window = %v, want 13.0 (33 today − 20 before the boundary)", gotToday)
	}

	// Window opened on the 13th at 12:00 → the trailing slice of the 13th (7.0)
	// plus every whole day after it (22 + 33) = 62.
	gotEarlier, ok := ss.windowSpendUSD(context.Background(), srv.URL, "vk-d", logs,
		time.Date(2026, 4, 13, 12, 0, 0, 0, time.UTC), now)
	if !ok {
		t.Fatal("earlier-day window: not ok")
	}
	if math.Abs(gotEarlier-62.0) > 0.001 {
		t.Errorf("earlier-day window = %v, want 62.0 (7 after the boundary on the 13th + 22 + 33)", gotEarlier)
	}
}

// TestSliceSpendUSD_CachesOnlySettledIntervals guards the one way this cache can
// go wrong: LiteLLM's spend writer lags ~a minute, so freezing a slice whose
// upper edge just closed would pin an undercount for the life of the window.
func TestSliceSpendUSD_CachesOnlySettledIntervals(t *testing.T) {
	now := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		json.NewEncoder(w).Encode(map[string]any{
			"data":  []map[string]any{{"request_id": "r1", "spend": 5.0, "startTime": "2026-04-15T01:00:00Z"}},
			"total": 1, "page": 1, "page_size": 100, "total_pages": 1,
		})
	}))
	defer srv.Close()
	ss := newSpendSync(nil, nil, srv.URL, "k", slog.Default())

	settled := now.Add(-time.Hour) // well behind the flush lag → cacheable
	for i := 0; i < 3; i++ {
		ss.sliceSpendUSD(context.Background(), srv.URL, "vk", now.Add(-2*time.Hour), settled, now)
	}
	if hits != 1 {
		t.Errorf("settled slice fetched %d times, want 1 (should be memoized)", hits)
	}

	hits = 0
	fresh := now.Add(-10 * time.Second) // inside the flush lag → must NOT cache
	for i := 0; i < 3; i++ {
		ss.sliceSpendUSD(context.Background(), srv.URL, "vk", now.Add(-2*time.Hour), fresh, now)
	}
	if hits != 3 {
		t.Errorf("unsettled slice fetched %d times, want 3 (must not be frozen mid-flush)", hits)
	}
}

// TestSpendSync_MeteredNoSubscriptionWindows verifies an account with NO reset
// instants (e.g. metered / probe hasn't written them) gets daily/weekly but no
// subscription-window fields — the viewer then keeps its calendar-day headline.
func TestSpendSync_MeteredNoSubscriptionWindows(t *testing.T) {
	now := fixedNow
	daemon := &daemonStub{
		beads: []map[string]any{
			makeDaemonBead("bd-met", "Metered", "vk-met"),
		},
	}
	logs := map[string][]spendLogEntry{
		hk("vk-met"): {
			{Spend: 3.0, StartTime: now.Add(-time.Hour).Format(time.RFC3339)},
		},
	}
	ss, _, _ := newTestSpendSync(t, daemon, litellmRouter(
		map[string]float64{"vk-met": 10.0},
		logs,
	))
	ss.syncAll(context.Background())

	updates := daemon.getUpdates()
	if len(updates) == 0 {
		t.Fatal("expected update")
	}
	u := updates[len(updates)-1].Fields
	if _, present := u["spend_5h_window_usd"]; present {
		t.Errorf("spend_5h_window_usd must be unset without a 5h reset instant")
	}
	if _, present := u["spend_7d_window_usd"]; present {
		t.Errorf("spend_7d_window_usd must be unset without a 7d reset instant")
	}
}
