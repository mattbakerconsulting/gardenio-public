package main

import (
	"fmt"
	"time"
)

// parseTimeCutoff resolves a time-window flag value into an absolute cutoff.
// It accepts an RFC3339 timestamp or a duration back from now ("4h", "30m",
// "2d" — 'd' days supported via parseDuration). Shared by `kd comment list
// --since` and the `kd list` --created-after/--updated-after/--closed-after
// family so every windowed flag speaks the same two spellings. flagName is
// only used in the error message.
func parseTimeCutoff(flagName, val string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, val); err == nil {
		return t, nil
	}
	if d, err := parseDuration(val); err == nil {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%s: %q is neither an RFC3339 timestamp (2026-07-18T09:00:00Z) nor a duration (4h, 30m, 2d)", flagName, val)
}
