package scheduler

import (
	"slices"
	"testing"
	"time"
	_ "time/tzdata"
)

func TestParseCron(t *testing.T) {
	tests := []struct {
		expr    string
		wantErr bool
	}{
		{"0 0 * * *", false},       // daily at midnight
		{"*/5 * * * *", false},     // every 5 minutes
		{"0 9 * * 1-5", false},     // weekdays at 9am
		{"0 0 1 * *", false},       // monthly
		{"30 2 * * *", false},      // 2:30 AM
		{"0 0 1 1 *", false},       // yearly
		{"@daily", false},          // alias
		{"@hourly", false},         // alias
		{"@weekly", false},         // alias
		{"@monthly", false},        // alias
		{"@yearly", false},         // alias
		{"0 9 * * mon-fri", false}, // named days
		{"0 0 1 jan *", false},     // named month
		{"bad", true},              // invalid
		{"0 0 0 * *", true},        // day 0 out of bounds
		{"60 0 * * *", true},       // minute 60 out of bounds
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			_, err := ParseCron(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParseCron(%q) error = %v, wantErr %v", tt.expr, err, tt.wantErr)
			}
		})
	}
}

func TestCronPrev(t *testing.T) {
	tests := []struct {
		name string
		expr string
		now  time.Time
		want time.Time
	}{
		{
			name: "daily midnight, checked at 3am",
			expr: "0 0 * * *",
			now:  time.Date(2026, 3, 7, 3, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "daily midnight, checked at midnight exactly",
			expr: "0 0 * * *",
			now:  time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "every 5 min, checked at 10:13",
			expr: "*/5 * * * *",
			now:  time.Date(2026, 3, 7, 10, 13, 0, 0, time.UTC),
			want: time.Date(2026, 3, 7, 10, 10, 0, 0, time.UTC),
		},
		{
			name: "weekdays 9am, checked Saturday",
			expr: "0 9 * * 1-5",
			now:  time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC), // Saturday
			want: time.Date(2026, 3, 6, 9, 0, 0, 0, time.UTC),  // Friday
		},
		{
			name: "monthly 1st at midnight, checked mid-month",
			expr: "0 0 1 * *",
			now:  time.Date(2026, 3, 15, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseCron(tt.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q) error: %v", tt.expr, err)
			}
			got := expr.Prev(tt.now)
			if !got.Equal(tt.want) {
				t.Errorf("Prev(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestCronNext(t *testing.T) {
	tests := []struct {
		name string
		expr string
		now  time.Time
		want time.Time
	}{
		{
			name: "daily midnight, checked at 3am",
			expr: "0 0 * * *",
			now:  time.Date(2026, 3, 7, 3, 0, 0, 0, time.UTC),
			want: time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "every 5 min, checked at 10:13",
			expr: "*/5 * * * *",
			now:  time.Date(2026, 3, 7, 10, 13, 0, 0, time.UTC),
			want: time.Date(2026, 3, 7, 10, 15, 0, 0, time.UTC),
		},
		{
			name: "weekdays 9am, checked Saturday",
			expr: "0 9 * * 1-5",
			now:  time.Date(2026, 3, 7, 12, 0, 0, 0, time.UTC), // Saturday
			want: time.Date(2026, 3, 9, 9, 0, 0, 0, time.UTC),  // Monday
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseCron(tt.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q) error: %v", tt.expr, err)
			}
			got := expr.Next(tt.now)
			if !got.Equal(tt.want) {
				t.Errorf("Next(%v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestCronPrev_WithTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal("failed to load timezone:", err)
	}

	expr, err := ParseCron("0 11-21 * * *")
	if err != nil {
		t.Fatal("ParseCron:", err)
	}

	// 14:49 UTC = 07:49 PDT — outside hours 11-21 PDT.
	now := time.Date(2026, 3, 19, 14, 49, 0, 0, time.UTC)
	nowPDT := now.In(loc)

	lastFire := expr.Prev(nowPDT)

	// Expected: 2026-03-18 21:00 PDT = 2026-03-19 04:00 UTC (previous day's window).
	expectedUTC := time.Date(2026, 3, 19, 4, 0, 0, 0, time.UTC)
	if !lastFire.UTC().Equal(expectedUTC) {
		t.Errorf("Prev(07:49 PDT) = %v (UTC: %v), want %v UTC",
			lastFire, lastFire.UTC(), expectedUTC)
	}

	// Without timezone (UTC): Prev should find 14:00 UTC (same day).
	lastFireUTC := expr.Prev(now)
	expectedUTCFire := time.Date(2026, 3, 19, 14, 0, 0, 0, time.UTC)
	if !lastFireUTC.Equal(expectedUTCFire) {
		t.Errorf("Prev(14:49 UTC) = %v, want %v", lastFireUTC, expectedUTCFire)
	}
}

func TestCronNext_WithTimezone(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal("failed to load timezone:", err)
	}

	expr, err := ParseCron("0 11-21 * * *")
	if err != nil {
		t.Fatal("ParseCron:", err)
	}

	// 14:49 UTC = 07:49 PDT — next fire should be 11:00 PDT = 18:00 UTC.
	now := time.Date(2026, 3, 19, 14, 49, 0, 0, time.UTC)
	nowPDT := now.In(loc)

	nextFire := expr.Next(nowPDT)
	expectedUTC := time.Date(2026, 3, 19, 18, 0, 0, 0, time.UTC)
	if !nextFire.UTC().Equal(expectedUTC) {
		t.Errorf("Next(07:49 PDT) = %v (UTC: %v), want %v UTC",
			nextFire, nextFire.UTC(), expectedUTC)
	}
}

func TestInWindow(t *testing.T) {
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal("failed to load timezone:", err)
	}

	expr, err := ParseCron("0 11-21 * * *")
	if err != nil {
		t.Fatal("ParseCron:", err)
	}

	tests := []struct {
		name string
		t    time.Time
		want bool
	}{
		{
			name: "07:49 PDT — outside window",
			t:    time.Date(2026, 3, 19, 14, 49, 0, 0, time.UTC).In(loc),
			want: false,
		},
		{
			name: "11:00 PDT — inside window",
			t:    time.Date(2026, 3, 19, 18, 0, 0, 0, time.UTC).In(loc),
			want: true,
		},
		{
			name: "15:30 PDT — inside window",
			t:    time.Date(2026, 3, 19, 22, 30, 0, 0, time.UTC).In(loc),
			want: true,
		},
		{
			name: "21:59 PDT — inside window",
			t:    time.Date(2026, 3, 20, 4, 59, 0, 0, time.UTC).In(loc),
			want: true,
		},
		{
			name: "22:00 PDT — outside window",
			t:    time.Date(2026, 3, 20, 5, 0, 0, 0, time.UTC).In(loc),
			want: false,
		},
		{
			name: "14:49 UTC — inside window when no timezone applied",
			t:    time.Date(2026, 3, 19, 14, 49, 0, 0, time.UTC),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := expr.InWindow(tt.t)
			if got != tt.want {
				t.Errorf("InWindow(%v) = %v, want %v", tt.t, got, tt.want)
			}
		})
	}
}

func TestInWindow_AllHours(t *testing.T) {
	// For "every 5 min" (all hours), InWindow should always return true.
	expr, err := ParseCron("*/5 * * * *")
	if err != nil {
		t.Fatal("ParseCron:", err)
	}

	at := time.Date(2026, 3, 19, 3, 15, 0, 0, time.UTC)
	if !expr.InWindow(at) {
		t.Error("InWindow should be true for * * * * * at any hour")
	}
}

func TestCronAliases(t *testing.T) {
	tests := []struct {
		alias     string
		equivExpr string
	}{
		{"@daily", "0 0 * * *"},
		{"@midnight", "0 0 * * *"},
		{"@hourly", "0 * * * *"},
		{"@weekly", "0 0 * * 0"},
		{"@monthly", "0 0 1 * *"},
		{"@yearly", "0 0 1 1 *"},
		{"@annually", "0 0 1 1 *"},
	}

	now := time.Date(2026, 6, 15, 12, 30, 0, 0, time.UTC)
	for _, tt := range tests {
		t.Run(tt.alias, func(t *testing.T) {
			a, err := ParseCron(tt.alias)
			if err != nil {
				t.Fatalf("ParseCron(%q) error: %v", tt.alias, err)
			}
			b, err := ParseCron(tt.equivExpr)
			if err != nil {
				t.Fatalf("ParseCron(%q) error: %v", tt.equivExpr, err)
			}
			aPrev := a.Prev(now)
			bPrev := b.Prev(now)
			if !aPrev.Equal(bPrev) {
				t.Errorf("Prev mismatch: %q=%v, %q=%v", tt.alias, aPrev, tt.equivExpr, bPrev)
			}
		})
	}
}

// --- timezone tests ---

func TestCronPrev_Timezone(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation failed: %v", err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("LoadLocation failed: %v", err)
	}

	tests := []struct {
		name    string
		expr    string
		now     time.Time
		wantUTC time.Time // expected result converted to UTC
	}{
		{
			name: "9am PDT in LA timezone, now is 5pm UTC (10am PDT)",
			expr: "0 9 * * *",
			// 2026-03-19 17:00 UTC = 2026-03-19 10:00 PDT
			now:     time.Date(2026, 3, 19, 17, 0, 0, 0, time.UTC).In(la),
			wantUTC: time.Date(2026, 3, 19, 16, 0, 0, 0, time.UTC), // 9am PDT = 16:00 UTC
		},
		{
			name: "9am in LA timezone, now is 8am PDT — should get yesterday 9am",
			expr: "0 9 * * *",
			// 2026-03-19 15:00 UTC = 2026-03-19 08:00 PDT
			now:     time.Date(2026, 3, 19, 15, 0, 0, 0, time.UTC).In(la),
			wantUTC: time.Date(2026, 3, 18, 16, 0, 0, 0, time.UTC), // yesterday 9am PDT
		},
		{
			name: "9am JST in Tokyo timezone, now is 1am UTC (10am JST)",
			expr: "0 9 * * *",
			// 2026-03-19 01:00 UTC = 2026-03-19 10:00 JST
			now:     time.Date(2026, 3, 19, 1, 0, 0, 0, time.UTC).In(tokyo),
			wantUTC: time.Date(2026, 3, 19, 0, 0, 0, 0, time.UTC), // 9am JST = 00:00 UTC
		},
		{
			name: "every-5-min in LA, result should be in LA timezone",
			expr: "*/5 * * * *",
			// 2026-03-19 17:13 UTC = 2026-03-19 10:13 PDT
			now:     time.Date(2026, 3, 19, 17, 13, 0, 0, time.UTC).In(la),
			wantUTC: time.Date(2026, 3, 19, 17, 10, 0, 0, time.UTC), // 10:10 PDT = 17:10 UTC
		},
		{
			name: "weekday hours 11-21 LA time, now is 7:49am PDT (14:49 UTC)",
			expr: "7,37 11-21 * * 1-5",
			// 2026-03-19 is a Thursday. 14:49 UTC = 7:49 PDT.
			// 7:49 PDT is before the 11am window, so Prev should be yesterday's 21:37 PDT.
			now:     time.Date(2026, 3, 19, 14, 49, 0, 0, time.UTC).In(la),
			wantUTC: time.Date(2026, 3, 19, 4, 37, 0, 0, time.UTC), // Wed 21:37 PDT = Thu 04:37 UTC
		},
		{
			name: "weekday hours 11-21 LA time, now is 2pm PDT (21:00 UTC)",
			expr: "7,37 11-21 * * 1-5",
			// 2026-03-19 is a Thursday. 21:00 UTC = 14:00 PDT.
			// Most recent fire in this window: 13:37 PDT = 20:37 UTC.
			now:     time.Date(2026, 3, 19, 21, 0, 0, 0, time.UTC).In(la),
			wantUTC: time.Date(2026, 3, 19, 20, 37, 0, 0, time.UTC), // 13:37 PDT
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseCron(tt.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q) error: %v", tt.expr, err)
			}
			got := expr.Prev(tt.now).UTC()
			if !got.Equal(tt.wantUTC) {
				t.Errorf("Prev() = %v, want %v (diff: %v)",
					got.Format(time.RFC3339), tt.wantUTC.Format(time.RFC3339),
					got.Sub(tt.wantUTC))
			}
		})
	}
}

func TestCronNext_Timezone(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("LoadLocation failed: %v", err)
	}

	tests := []struct {
		name    string
		expr    string
		now     time.Time
		wantUTC time.Time
	}{
		{
			name: "next 9am PDT, currently 8am PDT",
			expr: "0 9 * * *",
			// 15:00 UTC = 8:00 PDT
			now:     time.Date(2026, 3, 19, 15, 0, 0, 0, time.UTC).In(la),
			wantUTC: time.Date(2026, 3, 19, 16, 0, 0, 0, time.UTC), // 9am PDT = 16:00 UTC
		},
		{
			name: "next 9am PDT, currently 10am PDT",
			expr: "0 9 * * *",
			// 17:00 UTC = 10:00 PDT
			now:     time.Date(2026, 3, 19, 17, 0, 0, 0, time.UTC).In(la),
			wantUTC: time.Date(2026, 3, 20, 16, 0, 0, 0, time.UTC), // tomorrow 9am PDT
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			expr, err := ParseCron(tt.expr)
			if err != nil {
				t.Fatalf("ParseCron(%q) error: %v", tt.expr, err)
			}
			got := expr.Next(tt.now).UTC()
			if !got.Equal(tt.wantUTC) {
				t.Errorf("Next() = %v, want %v", got.Format(time.RFC3339), tt.wantUTC.Format(time.RFC3339))
			}
		})
	}
}

func TestCronExpr_Equal(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"identical", "13 9 * * 1", "13 9 * * 1", true},
		{"cosmetic zero-pad minute is equal", "13 9 * * 1", "13 09 * * 1", true},
		{"list vs range same set", "1,2,3 9 * * 1", "1-3 9 * * 1", true},
		{"named dow equals numeric", "13 9 * * mon", "13 9 * * 1", true},
		{"wildcard step vs full list", "*/1 9 * * 1", "* 9 * * 1", true},
		// The exact bug ka-0000004209 coordinates: the OLD cron the 07-30 fire
		// was stamped with vs the CURRENT cron. These are genuinely different
		// schedules and MUST compare unequal so a stale-schedule fire is dropped.
		{"different schedule (bug: 17 6 thu vs 13 9 mon)", "17 6 * * 4", "13 9 * * 1", false},
		{"different minute", "13 9 * * 1", "14 9 * * 1", false},
		{"different weekday", "13 9 * * 1", "13 9 * * 2", false},
		{"different hour", "13 9 * * 1", "13 10 * * 1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := ParseCron(tt.a)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tt.a, err)
			}
			b, err := ParseCron(tt.b)
			if err != nil {
				t.Fatalf("ParseCron(%q): %v", tt.b, err)
			}
			if got := a.Equal(b); got != tt.want {
				t.Errorf("(%q).Equal(%q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			// Symmetry: Equal must not depend on argument order.
			if got := b.Equal(a); got != tt.want {
				t.Errorf("(%q).Equal(%q) = %v, want %v (asymmetric!)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}

func TestCronExpr_Equal_Nil(t *testing.T) {
	expr, err := ParseCron("13 9 * * 1")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if expr.Equal(nil) {
		t.Error("non-nil.Equal(nil) should be false")
	}
	var a, b *CronExpr
	if !a.Equal(b) {
		t.Error("nil.Equal(nil) should be true")
	}
	if a.Equal(expr) {
		t.Error("nil.Equal(non-nil) should be false")
	}
}

// TestCronExpr_Equal_MismatchedLengths covers the length-mismatch path in Equal.
// Parsed crons always have fixed-length field slices, so this is unreachable via
// ParseCron — exercise it with hand-built structs so a truncated/corrupt
// CronExpr compares unequal rather than panicking.
func TestCronExpr_Equal_MismatchedLengths(t *testing.T) {
	full, err := ParseCron("13 9 * * 1")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	truncated := &CronExpr{
		Minutes:     full.Minutes[:len(full.Minutes)-1], // one short
		Hours:       full.Hours,
		DaysOfMonth: full.DaysOfMonth,
		Months:      full.Months,
		DaysOfWeek:  full.DaysOfWeek,
	}
	if full.Equal(truncated) {
		t.Error("Equal should be false when a field slice length differs")
	}
}

// TestMatchDay_BothRestricted_OR verifies standard (Vixie/POSIX) cron day-gate
// semantics: when BOTH the day-of-month and day-of-week fields are restricted
// (neither is "*"), a day matches if EITHER field matches. Regression for
// gd-5GYCLwj3f2, where matchDay unconditionally ANDed the two fields and
// silently dropped a both-restricted schedule's "13th of the month" fires on
// every month whose 13th was not the chosen weekday.
func TestMatchDay_BothRestricted_OR(t *testing.T) {
	// "0 9 13 * 5" — 09:00 on the 13th of any month, OR 09:00 on any Friday.
	expr, err := ParseCron("0 9 13 * 5")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}

	tests := []struct {
		name string
		t    time.Time
		want bool // InWindow (month+day+hour); minute not checked
	}{
		{
			// 13th of the month, a Wednesday — DOM matches, DOW does not.
			// AND semantics would (wrongly) reject; OR must accept.
			name: "13th on a non-Friday matches via DOM",
			t:    time.Date(2027, 1, 13, 9, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			// A Friday that is not the 13th — DOW matches, DOM does not.
			name: "Friday that is not the 13th matches via DOW",
			t:    time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			// Friday the 13th — both match.
			name: "Friday the 13th matches (both)",
			t:    time.Date(2027, 8, 13, 9, 0, 0, 0, time.UTC),
			want: true,
		},
		{
			// Neither the 13th nor a Friday — no match.
			name: "non-13th non-Friday does not match",
			t:    time.Date(2027, 1, 14, 9, 0, 0, 0, time.UTC),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := expr.InWindow(tt.t); got != tt.want {
				t.Errorf("InWindow(%v) = %v, want %v", tt.t, got, tt.want)
			}
		})
	}
}

// TestMatchDay_SingleRestricted_AND verifies the AND path is unchanged when at
// most one of DOM/DOW is restricted (the common case): the unrestricted field's
// all-true slice makes AND collapse to the restricted field alone.
func TestMatchDay_SingleRestricted_AND(t *testing.T) {
	// Only DOM restricted: "0 9 13 * *" fires strictly on the 13th, any weekday.
	domOnly, err := ParseCron("0 9 13 * *")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if !domOnly.InWindow(time.Date(2027, 1, 13, 9, 0, 0, 0, time.UTC)) {
		t.Error("0 9 13 * * should match the 13th (Wednesday)")
	}
	if domOnly.InWindow(time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC)) {
		t.Error("0 9 13 * * must NOT match a non-13th Friday (single-restricted stays AND)")
	}

	// Only DOW restricted: "0 9 * * 5" fires strictly on Fridays, any date.
	dowOnly, err := ParseCron("0 9 * * 5")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if !dowOnly.InWindow(time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC)) {
		t.Error("0 9 * * 5 should match a Friday")
	}
	if dowOnly.InWindow(time.Date(2027, 1, 13, 9, 0, 0, 0, time.UTC)) {
		t.Error("0 9 * * 5 must NOT match the 13th when it is a Wednesday")
	}

	// Step form "*/N" counts as a star (unrestricted), so DOW step + DOM leaves
	// the day gate on AND, not OR.
	if !domOnly.DomRestricted || domOnly.DowRestricted {
		t.Errorf("0 9 13 * * flags: DomRestricted=%v DowRestricted=%v, want true/false",
			domOnly.DomRestricted, domOnly.DowRestricted)
	}
}

// TestCronExpr_Equal_RestrictionFlags guards that two exprs with identical
// slices but different day-gate restriction (AND vs OR) are NOT Equal — they
// describe different firing sets, and the wake-intent reconciler relies on
// Equal to detect a genuine schedule change.
func TestCronExpr_Equal_RestrictionFlags(t *testing.T) {
	// "0 9 1-31 * 5" (both restricted -> OR: fires daily) vs "0 9 * * 5"
	// (DOW only -> Fridays). DaysOfMonth 1..31 all-true == "*" slice, and
	// DaysOfWeek both mark only Friday, so every slice is identical.
	orExpr, err := ParseCron("0 9 1-31 * 5")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	fridayOnly, err := ParseCron("0 9 * * 5")
	if err != nil {
		t.Fatalf("ParseCron: %v", err)
	}
	if !slices.Equal(orExpr.DaysOfMonth, fridayOnly.DaysOfMonth) ||
		!slices.Equal(orExpr.DaysOfWeek, fridayOnly.DaysOfWeek) {
		t.Fatal("precondition: expected identical DOM/DOW slices for this test")
	}
	if orExpr.Equal(fridayOnly) {
		t.Error("Equal must be false: identical slices but AND vs OR day gate = different schedules")
	}
}
