package scheduler

import (
	"strconv"
	"time"
)

// parseDurationField parses a string as a duration, returning defVal if empty
// or invalid. Supports Go duration strings (e.g., "30m", "1h", "45s") and
// plain integers as minutes. Used by EvaluateTriggers to interpret per-entry
// throttle / debounce fields.
func parseDurationField(s string, defVal time.Duration) time.Duration {
	if s == "" {
		return defVal
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		if mins, err := strconv.Atoi(s); err == nil && mins > 0 {
			return time.Duration(mins) * time.Minute
		}
		return defVal
	}
	if d <= 0 {
		return defVal
	}
	return d
}
