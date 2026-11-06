package scheduler

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CronExpr represents a parsed cron expression (5 fields: min hour dom month dow).
type CronExpr struct {
	Minutes     []bool // [0..59]
	Hours       []bool // [0..23]
	DaysOfMonth []bool // [1..31]
	Months      []bool // [1..12]
	DaysOfWeek  []bool // [0..6] (0=Sunday)

	// DomRestricted / DowRestricted record whether the day-of-month and
	// day-of-week fields were literally something other than "*". Standard
	// (Vixie/POSIX) cron uses OR semantics for the day gate ONLY when BOTH
	// fields are restricted; a bare all-true slice cannot distinguish "*"
	// from a full explicit range, so matchDay needs these flags. See matchDay.
	DomRestricted bool
	DowRestricted bool
}

// ParseCron parses a standard 5-field cron expression.
// Supports: *, ranges (1-5), steps (*/5), lists (1,3,5), and named months/days.
func ParseCron(expr string) (*CronExpr, error) {
	// Handle common aliases.
	switch expr {
	case "@yearly", "@annually":
		expr = "0 0 1 1 *"
	case "@monthly":
		expr = "0 0 1 * *"
	case "@weekly":
		expr = "0 0 * * 0"
	case "@daily", "@midnight":
		expr = "0 0 * * *"
	case "@hourly":
		expr = "0 * * * *"
	}

	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("expected 5 fields, got %d", len(fields))
	}

	minutes, err := parseField(fields[0], 0, 59, nil)
	if err != nil {
		return nil, fmt.Errorf("minute field: %w", err)
	}
	hours, err := parseField(fields[1], 0, 23, nil)
	if err != nil {
		return nil, fmt.Errorf("hour field: %w", err)
	}
	dom, err := parseField(fields[2], 1, 31, nil)
	if err != nil {
		return nil, fmt.Errorf("day-of-month field: %w", err)
	}
	months, err := parseField(fields[3], 1, 12, monthNames)
	if err != nil {
		return nil, fmt.Errorf("month field: %w", err)
	}
	dow, err := parseField(fields[4], 0, 6, dowNames)
	if err != nil {
		return nil, fmt.Errorf("day-of-week field: %w", err)
	}

	return &CronExpr{
		Minutes:     minutes,
		Hours:       hours,
		DaysOfMonth: dom,
		Months:      months,
		DaysOfWeek:  dow,
		// A field is a "star" (unrestricted) when its first character is "*",
		// which covers both "*" and "*/N" — matching Vixie cron, where the
		// DOM_STAR/DOW_STAR flag drives the day-gate AND-vs-OR choice.
		DomRestricted: !strings.HasPrefix(fields[2], "*"),
		DowRestricted: !strings.HasPrefix(fields[4], "*"),
	}, nil
}

// Equal reports whether two parsed cron expressions describe the SAME schedule
// (same set of matching minutes/hours/days/months/weekdays), independent of how
// each was written. "13 9 * * 1" and "13 09 * * 1" are Equal; "13 9 * * 1" and
// "17 6 * * 4" are not. Callers use this to tell a genuine schedule change from a
// cosmetic reformat — e.g. the wake-intent reconciler must not treat a fire that
// happened under a superseded cron as a fire under the current one, but it also
// must not drop a fire merely because the stamp was reformatted (that would
// manufacture a phantom missed fire, the opposite defect).
func (c *CronExpr) Equal(o *CronExpr) bool {
	if c == nil || o == nil {
		return c == o
	}
	return slices.Equal(c.Minutes, o.Minutes) &&
		slices.Equal(c.Hours, o.Hours) &&
		slices.Equal(c.DaysOfMonth, o.DaysOfMonth) &&
		slices.Equal(c.Months, o.Months) &&
		slices.Equal(c.DaysOfWeek, o.DaysOfWeek) &&
		// The DOM/DOW restriction flags change the day gate from AND to OR
		// (see matchDay), so two exprs with identical slices but different
		// restriction flags describe DIFFERENT firing sets — e.g. "0 9 1-31 * 5"
		// (fires daily via OR) vs "0 9 * * 5" (Fridays only) share every slice.
		c.DomRestricted == o.DomRestricted &&
		c.DowRestricted == o.DowRestricted
}

// Prev returns the most recent time before or equal to t that matches the cron expression.
// Returns zero time if no match is found within 366 days.
func (c *CronExpr) Prev(t time.Time) time.Time {
	// Truncate to the current minute.
	t = t.Truncate(time.Minute)

	// Search backwards up to 366 days.
	limit := t.Add(-366 * 24 * time.Hour)

	for t.After(limit) {
		if !c.matchMonth(t) {
			// Go to end of previous month.
			t = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location()).Add(-time.Minute)
			continue
		}
		if !c.matchDay(t) {
			// Go to end of previous day.
			t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).Add(-time.Minute)
			continue
		}
		if !c.matchHour(t) {
			// Go to end of previous hour.
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), 0, 0, 0, t.Location()).Add(-time.Minute)
			continue
		}
		if !c.matchMinute(t) {
			t = t.Add(-time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// Next returns the next time after t that matches the cron expression.
// Returns zero time if no match is found within 366 days.
func (c *CronExpr) Next(t time.Time) time.Time {
	// Start from the next minute.
	t = t.Truncate(time.Minute).Add(time.Minute)

	limit := t.Add(366 * 24 * time.Hour)

	for t.Before(limit) {
		if !c.matchMonth(t) {
			// Skip to first day of next month.
			if t.Month() == 12 {
				t = time.Date(t.Year()+1, 1, 1, 0, 0, 0, 0, t.Location())
			} else {
				t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, t.Location())
			}
			continue
		}
		if !c.matchDay(t) {
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, t.Location())
			continue
		}
		if !c.matchHour(t) {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, t.Location())
			continue
		}
		if !c.matchMinute(t) {
			t = t.Add(time.Minute)
			continue
		}
		return t
	}
	return time.Time{}
}

// InWindow reports whether t falls within the schedule's active time window
// (matching month, day-of-month, day-of-week, and hour). Minute is not checked
// because the reconciler fires on its own interval, not at exact cron minutes.
// Use this to prevent catch-up fires outside the intended schedule window.
func (c *CronExpr) InWindow(t time.Time) bool {
	return c.matchMonth(t) && c.matchDay(t) && c.matchHour(t)
}

func (c *CronExpr) matchMonth(t time.Time) bool {
	return c.Months[int(t.Month())]
}

// matchDay reports whether t satisfies the day gate. Standard (Vixie/POSIX)
// cron treats the day-of-month and day-of-week fields specially: when BOTH are
// restricted (neither is "*"), a day matches if EITHER field matches (OR); when
// at most one is restricted, the ordinary intersection (AND) applies — and
// because the unrestricted field's slice is all-true, that AND collapses to the
// restricted field alone. Using AND unconditionally would silently drop the
// "13th of the month OR any Friday" (e.g. "0 9 13 * 5") style schedules, which
// is exactly the both-restricted case cron authors reach for.
func (c *CronExpr) matchDay(t time.Time) bool {
	dom := c.DaysOfMonth[t.Day()]
	dow := c.DaysOfWeek[int(t.Weekday())]
	if c.DomRestricted && c.DowRestricted {
		return dom || dow
	}
	return dom && dow
}

func (c *CronExpr) matchHour(t time.Time) bool {
	return c.Hours[t.Hour()]
}

func (c *CronExpr) matchMinute(t time.Time) bool {
	return c.Minutes[t.Minute()]
}

var monthNames = map[string]int{
	"jan": 1, "feb": 2, "mar": 3, "apr": 4,
	"may": 5, "jun": 6, "jul": 7, "aug": 8,
	"sep": 9, "oct": 10, "nov": 11, "dec": 12,
}

var dowNames = map[string]int{
	"sun": 0, "mon": 1, "tue": 2, "wed": 3,
	"thu": 4, "fri": 5, "sat": 6,
}

// parseField parses a single cron field into a boolean slice.
// The slice is indexed from 0 to max; for fields starting at 1 (months, DOM),
// index 0 is unused.
func parseField(field string, min, max int, names map[string]int) ([]bool, error) {
	bits := make([]bool, max+1)

	for _, part := range strings.Split(field, ",") {
		if err := parsePart(part, min, max, names, bits); err != nil {
			return nil, err
		}
	}
	return bits, nil
}

func parsePart(part string, min, max int, names map[string]int, bits []bool) error {
	// Handle step: "*/2", "1-10/3"
	step := 1
	if idx := strings.Index(part, "/"); idx >= 0 {
		var err error
		step, err = strconv.Atoi(part[idx+1:])
		if err != nil || step <= 0 {
			return fmt.Errorf("invalid step in %q", part)
		}
		part = part[:idx]
	}

	// Handle wildcard.
	if part == "*" {
		for i := min; i <= max; i += step {
			bits[i] = true
		}
		return nil
	}

	// Handle range: "1-5"
	if idx := strings.Index(part, "-"); idx >= 0 {
		lo, err := resolveValue(part[:idx], names)
		if err != nil {
			return err
		}
		hi, err := resolveValue(part[idx+1:], names)
		if err != nil {
			return err
		}
		if lo < min || hi > max || lo > hi {
			return fmt.Errorf("range %d-%d out of bounds [%d,%d]", lo, hi, min, max)
		}
		for i := lo; i <= hi; i += step {
			bits[i] = true
		}
		return nil
	}

	// Single value.
	val, err := resolveValue(part, names)
	if err != nil {
		return err
	}
	if val < min || val > max {
		return fmt.Errorf("value %d out of bounds [%d,%d]", val, min, max)
	}
	bits[val] = true
	return nil
}

func resolveValue(s string, names map[string]int) (int, error) {
	s = strings.TrimSpace(s)
	if names != nil {
		if v, ok := names[strings.ToLower(s)]; ok {
			return v, nil
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid value %q", s)
	}
	return v, nil
}
