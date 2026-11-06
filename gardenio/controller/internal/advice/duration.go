package advice

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// ParseDuration parses a duration string accepting both Go-style values
// understood by time.ParseDuration ("4h", "30m", "4h30m") and an extended
// natural-language form that adds day/week units and human-readable spellings
// ("7d", "1 week", "4 hours", "1d12h", "30 minutes").
//
// Supported units (case-insensitive): s/sec/second(s), m/min/minute(s),
// h/hr/hour(s), d/day(s), w/week(s). Days are 24h, weeks are 7d (no DST or
// calendar awareness).
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	return parseExtendedDuration(s)
}

func parseExtendedDuration(s string) (time.Duration, error) {
	runes := []rune(strings.ToLower(s))
	var total time.Duration
	matched := false
	i := 0
	for i < len(runes) {
		for i < len(runes) && unicode.IsSpace(runes[i]) {
			i++
		}
		if i >= len(runes) {
			break
		}

		numStart := i
		sawDot := false
		for i < len(runes) && (unicode.IsDigit(runes[i]) || (runes[i] == '.' && !sawDot)) {
			if runes[i] == '.' {
				sawDot = true
			}
			i++
		}
		if i == numStart {
			return 0, fmt.Errorf("invalid duration %q: expected number at position %d", s, numStart)
		}
		n, err := strconv.ParseFloat(string(runes[numStart:i]), 64)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: %w", s, err)
		}

		for i < len(runes) && unicode.IsSpace(runes[i]) {
			i++
		}

		unitStart := i
		for i < len(runes) && unicode.IsLetter(runes[i]) {
			i++
		}
		if i == unitStart {
			return 0, fmt.Errorf("invalid duration %q: missing unit after %g", s, n)
		}
		mult, ok := unitMultiplier(string(runes[unitStart:i]))
		if !ok {
			return 0, fmt.Errorf("invalid duration %q: unknown unit %q", s, string(runes[unitStart:i]))
		}
		total += time.Duration(n * float64(mult))
		matched = true
	}
	if !matched {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return total, nil
}

func unitMultiplier(u string) (time.Duration, bool) {
	switch u {
	case "s", "sec", "secs", "second", "seconds":
		return time.Second, true
	case "m", "min", "mins", "minute", "minutes":
		return time.Minute, true
	case "h", "hr", "hrs", "hour", "hours":
		return time.Hour, true
	case "d", "day", "days":
		return 24 * time.Hour, true
	case "w", "week", "weeks":
		return 7 * 24 * time.Hour, true
	}
	return 0, false
}
