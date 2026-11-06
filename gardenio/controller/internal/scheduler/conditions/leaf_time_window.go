package conditions

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The time_window leaf gates a trigger fire on the wall-clock time in a named
// timezone. It is the primitive behind the gardenio release "quiet hours"
// policy (ka-0000004693): no releases during PST business hours (07:00-18:00,
// weekdays) unless explicitly asked.
//
// JSON shape (a daemon-side leaf — addressed by Type):
//
//	{"type":"time_window","timezone":"America/Los_Angeles",
//	 "window":"07:00-18:00","days":["Mon","Tue","Wed","Thu","Fri"],
//	 "op":"outside"}
//
// Semantics — the leaf answers "is firing ALLOWED right now?":
//
//   - op "outside" (the default): TRUE when the current local time is OUTSIDE
//     the window on an in-scope day. This is the quiet-hours gate — AND-combine
//     it with the trigger's other conditions and the fire is suppressed for the
//     whole business-hours window, then resumes automatically at 18:00 local.
//   - op "inside": the complement — TRUE only INSIDE the window. Useful for a
//     trigger that should fire ONLY during a window.
//
// WHY controller-side, not shell-side: agent and CI pods have NO tzdata
// (no /usr/share/zoneinfo), so `TZ=America/Los_Angeles` in a shell silently
// resolves to UTC — a naive shell hour-gate is off by 7-8 hours and inverts
// the window. The controller imports `_ "time/tzdata"` (cmd/controller/main.go
// and the trigger_source_*.go files), so time.LoadLocation resolves the real
// IANA zone here. Any shell-side mirror of this policy (scripts/release.sh)
// must hand-roll the zone via a self-contained POSIX TZ string instead.
//
// Fail-closed: an unparseable window/timezone/op returns an error so the caller
// (EvaluateConditions / cronPreflightHolds) suppresses the fire rather than
// firing through a mis-configured gate. A gate that fails open would silently
// release during the very window it exists to protect.

// timeWindowNowFunc, when non-nil, overrides the clock the time_window leaf
// reads (tests only). Package-level hook rather than an Env field so the
// existing Env construction sites need no change — production always uses the
// real wall clock.
var timeWindowNowFunc func() time.Time

func timeWindowNow() time.Time {
	if timeWindowNowFunc != nil {
		return timeWindowNowFunc()
	}
	return time.Now()
}

// SetTimeWindowNowForTest overrides the wall clock the time_window leaf reads and
// returns a restore func — the exported seam so callers in OTHER packages (the
// wake-intent reconciler's cmd/gb tests) can pin the gate probe's point-in-time
// clock and assert the drift-candidate count is invariant to WHEN the reconciler
// runs relative to a quiet-hours window (ka-0000005666). Passing nil restores the
// real clock. Test-only: production never calls this, so the leaf always reads
// time.Now().
func SetTimeWindowNowForTest(f func() time.Time) (restore func()) {
	prev := timeWindowNowFunc
	timeWindowNowFunc = f
	return func() { timeWindowNowFunc = prev }
}

// evalTimeWindow evaluates a time_window leaf. It consults only the wall clock
// and the leaf's own fields — never the bead store or the event payload — so it
// is safe to dispatch before the daemon-leaf nil-client guard.
func evalTimeWindow(l Leaf) (bool, error) {
	return evalTimeWindowAt(l, timeWindowNow())
}

// EvalTimeWindowLeafAt evaluates a time_window leaf at an ARBITRARY instant —
// the exported seam the wake-intent reconciler's historical time_window overlay
// (cmd/gb/trigger_reconcile_timewindow.go) uses to ask "would this leaf have
// SUPPRESSED a fire at past instant `at`?". It answers the same question the
// engine's fire-time gate does (evalTimeWindowAt is what evalLeaf dispatches),
// but against a caller-supplied wall clock rather than time.Now(), so a missed
// cron slot can be attributed deterministically to the trigger's own quiet-hours
// window instead of the point-in-time would_fire_now snapshot that reads
// differently depending on WHEN the reconciler happens to run. Returns
// (fireAllowed, err): a leaf that ALLOWS firing at `at` returns true; one whose
// window suppresses the fire returns false. A malformed leaf returns the parse
// error so the caller can fail-soft (attribute nothing) rather than guess.
func EvalTimeWindowLeafAt(l Leaf, at time.Time) (bool, error) {
	return evalTimeWindowAt(l, at)
}

// evalTimeWindowAt is evalTimeWindow with an injectable clock so the policy is
// unit-testable against a fixed instant without the package-level clock hook.
func evalTimeWindowAt(l Leaf, clock time.Time) (bool, error) {
	loc := time.UTC
	if tz := strings.TrimSpace(l.Timezone); tz != "" {
		parsed, err := time.LoadLocation(tz)
		if err != nil {
			return false, fmt.Errorf("time_window: invalid timezone %q: %w", tz, err)
		}
		loc = parsed
	}

	startMin, endMin, err := parseWindow(l.Window)
	if err != nil {
		return false, err
	}

	days, err := parseWindowDays(l.Days)
	if err != nil {
		return false, err
	}

	now := clock.In(loc)
	inside := withinWindow(now, startMin, endMin, days)

	op := strings.TrimSpace(l.Op)
	switch op {
	case "", "outside":
		return !inside, nil
	case "inside":
		return inside, nil
	default:
		return false, fmt.Errorf("time_window: unknown op %q (want inside|outside)", op)
	}
}

// withinWindow reports whether local time now falls inside the recurring daily
// window [startMin, endMin) restricted to the given set of weekdays.
//
// The window is a minute-of-day range. A range that does NOT cross midnight
// (startMin < endMin) is the common case (business hours 07:00-18:00). A range
// that crosses midnight (startMin > endMin, e.g. 22:00-06:00) wraps: it is
// active from startMin to end-of-day AND from start-of-day to endMin. The
// weekday scope is keyed off the day the window OPENS: for a non-wrapping
// window that is simply "today"; for a wrapping window the pre-midnight segment
// is scoped to today and the post-midnight segment to the PREVIOUS day's
// membership (so a Fri 22:00-06:00 window still covers the Sat 02:00 tail even
// when Sat is out of scope).
func withinWindow(now time.Time, startMin, endMin int, days map[time.Weekday]bool) bool {
	minOfDay := now.Hour()*60 + now.Minute()

	dayIn := func(d time.Weekday) bool {
		if days == nil {
			return true // no day restriction ⇒ every day
		}
		return days[d]
	}

	if startMin == endMin {
		// Degenerate empty window — never inside.
		return false
	}
	if startMin < endMin {
		// Non-wrapping: active [start,end) on an in-scope day.
		return minOfDay >= startMin && minOfDay < endMin && dayIn(now.Weekday())
	}
	// Wrapping window (crosses midnight).
	if minOfDay >= startMin {
		// Pre-midnight segment, scoped to today.
		return dayIn(now.Weekday())
	}
	if minOfDay < endMin {
		// Post-midnight segment, scoped to the day the window opened (yesterday).
		prev := now.AddDate(0, 0, -1).Weekday()
		return dayIn(prev)
	}
	return false
}

// parseWindow parses "HH:MM-HH:MM" (24-hour local time) into start/end
// minute-of-day. An empty window is an error: a time_window leaf with no window
// is a mis-configuration, and fail-closed demands we surface it rather than
// treat "no window" as "always allow".
func parseWindow(raw string) (startMin, endMin int, err error) {
	w := strings.TrimSpace(raw)
	if w == "" {
		return 0, 0, fmt.Errorf("time_window: empty window (want HH:MM-HH:MM)")
	}
	lo, hi, ok := strings.Cut(w, "-")
	if !ok {
		return 0, 0, fmt.Errorf("time_window: window %q missing '-' separator (want HH:MM-HH:MM)", raw)
	}
	startMin, err = parseClock(lo)
	if err != nil {
		return 0, 0, fmt.Errorf("time_window: window start: %w", err)
	}
	endMin, err = parseClock(hi)
	if err != nil {
		return 0, 0, fmt.Errorf("time_window: window end: %w", err)
	}
	return startMin, endMin, nil
}

// parseClock parses "HH:MM" into a minute-of-day in [0,1440). "24:00" is
// accepted as an alias for end-of-day (1440) so a window may run to midnight.
func parseClock(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	hh, mm, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("clock %q missing ':' (want HH:MM)", raw)
	}
	h, err := strconv.Atoi(strings.TrimSpace(hh))
	if err != nil {
		return 0, fmt.Errorf("clock %q: bad hour: %w", raw, err)
	}
	m, err := strconv.Atoi(strings.TrimSpace(mm))
	if err != nil {
		return 0, fmt.Errorf("clock %q: bad minute: %w", raw, err)
	}
	if m < 0 || m > 59 {
		return 0, fmt.Errorf("clock %q: minute out of range 0-59", raw)
	}
	if h == 24 && m == 0 {
		return 1440, nil // end-of-day alias
	}
	if h < 0 || h > 23 {
		return 0, fmt.Errorf("clock %q: hour out of range 0-23 (or 24:00)", raw)
	}
	return h*60 + m, nil
}

// weekdayAliases maps the accepted weekday spellings to time.Weekday. Short
// three-letter forms are the canonical config spelling; full names and numeric
// 0-6 (0=Sunday, cron convention) are also accepted.
var weekdayAliases = map[string]time.Weekday{
	"sun": time.Sunday, "sunday": time.Sunday, "0": time.Sunday,
	"mon": time.Monday, "monday": time.Monday, "1": time.Monday,
	"tue": time.Tuesday, "tues": time.Tuesday, "tuesday": time.Tuesday, "2": time.Tuesday,
	"wed": time.Wednesday, "weds": time.Wednesday, "wednesday": time.Wednesday, "3": time.Wednesday,
	"thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday, "thursday": time.Thursday, "4": time.Thursday,
	"fri": time.Friday, "friday": time.Friday, "5": time.Friday,
	"sat": time.Saturday, "saturday": time.Saturday, "6": time.Saturday,
}

// parseWindowDays lowers the leaf's Days list to a weekday set. An empty list
// returns nil ("every day"). "weekdays"/"weekends" are accepted as shorthands.
func parseWindowDays(raw []string) (map[time.Weekday]bool, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	set := make(map[time.Weekday]bool, 7)
	for _, d := range raw {
		key := strings.ToLower(strings.TrimSpace(d))
		switch key {
		case "":
			continue
		case "weekday", "weekdays":
			set[time.Monday] = true
			set[time.Tuesday] = true
			set[time.Wednesday] = true
			set[time.Thursday] = true
			set[time.Friday] = true
			continue
		case "weekend", "weekends":
			set[time.Saturday] = true
			set[time.Sunday] = true
			continue
		}
		wd, ok := weekdayAliases[key]
		if !ok {
			return nil, fmt.Errorf("time_window: unknown day %q", d)
		}
		set[wd] = true
	}
	if len(set) == 0 {
		return nil, nil
	}
	return set, nil
}
