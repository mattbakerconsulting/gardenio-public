package poolmanager

import (
	"testing"
	"time"
)

func TestTargetPoolSize(t *testing.T) {
	tests := []struct {
		name      string
		spawnRate int
		min       int
		max       int
		want      int
	}{
		{"zero spawns → min", 0, 2, 10, 2},
		{"max spawns → max", 10, 2, 10, 10},
		{"over max spawns → capped at max", 20, 2, 10, 10},
		{"half max spawns → midpoint", 5, 2, 10, 6},
		{"one spawn → just above min", 1, 2, 10, 3},
		{"min equals max → always that value", 3, 5, 5, 5},
		{"zero min zero max → zero", 0, 0, 0, 0},
		{"one spawn with small pool", 1, 1, 3, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := targetPoolSize(tt.spawnRate, tt.min, tt.max)
			if got != tt.want {
				t.Errorf("targetPoolSize(%d, %d, %d) = %d, want %d",
					tt.spawnRate, tt.min, tt.max, got, tt.want)
			}
		})
	}
}

func TestSpawnTracker_RecordAndRate(t *testing.T) {
	now := time.Date(2026, 3, 12, 12, 0, 0, 0, time.UTC)
	tracker := newSpawnTracker()
	tracker.nowFunc = func() time.Time { return now }

	// No spawns → rate 0.
	if rate := tracker.Rate("proj"); rate != 0 {
		t.Fatalf("expected rate 0, got %d", rate)
	}

	// Record 3 spawns.
	tracker.Record("proj")
	tracker.Record("proj")
	tracker.Record("proj")

	if rate := tracker.Rate("proj"); rate != 3 {
		t.Fatalf("expected rate 3, got %d", rate)
	}

	// Different project has independent rate.
	if rate := tracker.Rate("other"); rate != 0 {
		t.Fatalf("expected rate 0 for other project, got %d", rate)
	}
}

func TestSpawnTracker_RatePrunesOldEntries(t *testing.T) {
	now := time.Date(2026, 3, 12, 12, 0, 0, 0, time.UTC)
	tracker := newSpawnTracker()
	tracker.nowFunc = func() time.Time { return now }

	// Record spawns.
	tracker.Record("proj")
	tracker.Record("proj")

	// Advance time past the window.
	now = now.Add(spawnWindow + time.Minute)
	tracker.nowFunc = func() time.Time { return now }

	// Old spawns should be pruned.
	if rate := tracker.Rate("proj"); rate != 0 {
		t.Fatalf("expected rate 0 after window expired, got %d", rate)
	}
}

func TestSpawnTracker_MixedAges(t *testing.T) {
	now := time.Date(2026, 3, 12, 12, 0, 0, 0, time.UTC)
	tracker := newSpawnTracker()
	tracker.nowFunc = func() time.Time { return now }

	// Record 2 spawns in the past.
	tracker.Record("proj")
	tracker.Record("proj")

	// Advance time so the first 2 are old but still within window.
	now = now.Add(10 * time.Minute)
	tracker.nowFunc = func() time.Time { return now }

	// Record 1 more recent spawn.
	tracker.Record("proj")

	// All 3 should still be in window (10min < 15min window).
	if rate := tracker.Rate("proj"); rate != 3 {
		t.Fatalf("expected rate 3, got %d", rate)
	}

	// Advance past window for the first 2 but not the last.
	now = now.Add(6 * time.Minute) // 16min total for first 2, 6min for last
	tracker.nowFunc = func() time.Time { return now }

	if rate := tracker.Rate("proj"); rate != 1 {
		t.Fatalf("expected rate 1 (only recent spawn), got %d", rate)
	}
}
