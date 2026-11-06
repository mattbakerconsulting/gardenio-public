package main

import (
	"path/filepath"
	"testing"
)

// TestStopsNotTakenSlowCallStopNeedsNoWrapup pins the guard on the
// slowest_calls gb stop (gd-3XE1oIjRyx): the bead-only vfyw session is a
// finding only while its agent bead stores no wrap-up inside the session. A
// wrap-up stamped after the session ended is a later session's stop and does
// not clear it.
func TestStopsNotTakenSlowCallStopNeedsNoWrapup(t *testing.T) {
	dir := filepath.Join(sweepFixtureDir, "own-bead-403", "positive-bead-only-vfyw")
	for _, tc := range []struct {
		name, wrapup string
		want         int
	}{
		{"no wrap-up", "", 1},
		{"wrap-up stored inside the session", `{"tldr":"done","timestamp":"2026-10-27T16:10:00.5Z"}`, 0},
		{"wrap-up from a later session", `{"tldr":"done","timestamp":"2026-10-28T01:00:00Z"}`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, in := loadSweepFixture(t, dir)
			if tc.wrapup != "" {
				in.Agents[0].Fields["wrapup"] = tc.wrapup
			}
			if got := detectStopsNotTaken(in); len(got) != tc.want {
				t.Fatalf("got %d finding(s), want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}
