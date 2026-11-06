package main

import (
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func TestSortCalverDesc(t *testing.T) {
	// Deliberately out of order, with a non-calver tag and a cross-day
	// build that must not sort lexically.
	in := []string{"2026.65.8", "v2026.70.1", "not-a-tag", "2026.65.10", "2026.177.7", "2025.300.9"}
	got := sortCalverDesc(in)
	want := []string{"2026.177.7", "v2026.70.1", "2026.65.10", "2026.65.8", "2025.300.9"}
	if len(got) != len(want) {
		t.Fatalf("got %d tags, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].name != want[i] {
			t.Errorf("position %d: got %q, want %q (full: %+v)", i, got[i].name, want[i], got)
		}
	}
}
