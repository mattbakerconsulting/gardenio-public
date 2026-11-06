package main

import (
	"testing"
	"time"
)

// TestDeriveGardenerLiveness_A1 exercises the overdue / silent-gardener verdict
// (bug ka-0000006095 / A1) across the full matrix of firing-health shapes.
func TestDeriveGardenerLiveness_A1(t *testing.T) {
	now := mustTime(t, "2026-08-17T12:00:00Z")

	cases := []struct {
		name        string
		in          gardenerLivenessInput
		wantOverdue bool
		wantSilent  bool
	}{
		{
			// The ground-truth shape from the bug: enabled, 30m cron, last fired
			// >24h ago. Should have fired ~48 times since — clearly overdue.
			name: "cron_gardener_gone_dark",
			in: gardenerLivenessInput{
				Enabled:  true,
				Cron:     "27,57 * * * *",
				LastFire: mustTime(t, "2026-08-16T07:59:06Z"),
			},
			wantOverdue: true,
		},
		{
			// Firing on schedule: last fire well within cadence + tolerance.
			name: "cron_gardener_firing_on_schedule",
			in: gardenerLivenessInput{
				Enabled:  true,
				Cron:     "27,57 * * * *",
				LastFire: mustTime(t, "2026-08-17T11:57:00Z"),
			},
		},
		{
			// Just past the naive cron instant but inside the tolerance window —
			// must NOT flash overdue (that would be noise on every window edge).
			name: "cron_gardener_within_tolerance",
			in: gardenerLivenessInput{
				Enabled: true,
				Cron:    "*/30 * * * *", // every 30m
				// last fired 45m ago: 30m cadence + 30m tolerance = 60m budget,
				// so 45m is NOT overdue.
				LastFire: now.Add(-45 * time.Minute),
			},
		},
		{
			// Budget exhausted: RunsSpawned >= MaxRuns with a real positive max.
			name: "budget_exhausted",
			in: gardenerLivenessInput{
				Enabled:     true,
				RunsSpawned: 100,
				MaxRuns:     100,
			},
			wantOverdue: true,
		},
		{
			// Unbounded / standing gardener (MaxRuns 0) is never "over budget".
			name: "unbounded_gardener_never_over_budget",
			in: gardenerLivenessInput{
				Enabled:     true,
				RunsSpawned: 9999,
				MaxRuns:     0,
			},
		},
		{
			// Still spawning (recent fire) but progress heartbeat frozen >8h:
			// SILENT, not overdue.
			name: "silent_spawning_no_progress",
			in: gardenerLivenessInput{
				Enabled:      true,
				Cron:         "*/5 * * * *",
				LastFire:     now.Add(-3 * time.Minute), // still firing
				LastProgress: now.Add(-11 * time.Hour),  // but no headway in 11h
			},
			wantSilent: true,
		},
		{
			// Progress frozen AND not spawning (last fire old): OVERDUE via the
			// dark-progress path, not silent.
			name: "frozen_progress_not_spawning",
			in: gardenerLivenessInput{
				Enabled:      true,
				LastProgress: now.Add(-11 * time.Hour),
				LastFire:     now.Add(-11 * time.Hour),
			},
			wantOverdue: true,
		},
		{
			// Recent progress → neither flag, even while spawning.
			name: "fresh_progress_healthy",
			in: gardenerLivenessInput{
				Enabled:      true,
				Cron:         "*/5 * * * *",
				LastFire:     now.Add(-3 * time.Minute),
				LastProgress: now.Add(-10 * time.Minute),
			},
		},
		{
			// Disabled gardener: never flagged, even with an exhausted budget.
			name: "disabled_never_flagged",
			in: gardenerLivenessInput{
				Enabled:     false,
				RunsSpawned: 100,
				MaxRuns:     100,
				LastFire:    mustTime(t, "2026-08-16T07:59:06Z"),
				Cron:        "*/30 * * * *",
			},
		},
		{
			// Closed gardener: never flagged.
			name: "closed_never_flagged",
			in: gardenerLivenessInput{
				Enabled:     true,
				IsClosed:    true,
				RunsSpawned: 100,
				MaxRuns:     100,
			},
		},
		{
			// No cron + no recorded fire + no budget + no progress: nothing to
			// judge against, so stay silent rather than guess.
			name: "no_signals_no_flag",
			in: gardenerLivenessInput{
				Enabled: true,
			},
		},
		{
			// Cron gardener with NO recorded fire yet (brand new): cannot be late.
			name: "cron_no_last_fire",
			in: gardenerLivenessInput{
				Enabled: true,
				Cron:    "*/30 * * * *",
			},
		},
		{
			// Unparseable cron: skip cron-lateness (no expected cadence to read),
			// rather than fabricate an overdue from a cadence we could not parse.
			name: "unparseable_cron_no_flag",
			in: gardenerLivenessInput{
				Enabled:  true,
				Cron:     "not-a-cron",
				LastFire: mustTime(t, "2026-08-01T00:00:00Z"),
			},
		},
		{
			// Long cooldown widens the budget: a gardener that fired within
			// cadence + cooldown + tolerance is NOT overdue.
			name: "long_cooldown_widens_budget",
			in: gardenerLivenessInput{
				Enabled:         true,
				Cron:            "*/5 * * * *", // 5m cadence
				CooldownMinutes: 60,            // + 60m cooldown + 30m tol = 95m
				LastFire:        now.Add(-90 * time.Minute),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveGardenerLiveness(tc.in, now)
			if got.Overdue != tc.wantOverdue {
				t.Errorf("Overdue = %v (%q), want %v", got.Overdue, got.OverdueDetail, tc.wantOverdue)
			}
			if got.Silent != tc.wantSilent {
				t.Errorf("Silent = %v (%q), want %v", got.Silent, got.SilentDetail, tc.wantSilent)
			}
			// NeedsAttention is exactly the disjunction, and a flagged verdict
			// always carries human detail (never a bare, unexplained warning).
			if got.NeedsAttention() != (tc.wantOverdue || tc.wantSilent) {
				t.Errorf("NeedsAttention() = %v, want %v", got.NeedsAttention(), tc.wantOverdue || tc.wantSilent)
			}
			if got.Overdue && got.OverdueDetail == "" {
				t.Errorf("Overdue set but OverdueDetail empty")
			}
			if got.Silent && got.SilentDetail == "" {
				t.Errorf("Silent set but SilentDetail empty")
			}
		})
	}
}

// TestGardenerLastFire_FreshestWins asserts gardenerLastFire takes the later of
// last_outcome_at and last_run, tolerating an absent or unparseable field.
func TestGardenerLastFire_FreshestWins(t *testing.T) {
	older := "2026-08-16T07:00:00Z"
	newer := "2026-08-16T08:00:00Z"

	cases := []struct {
		name   string
		fields map[string]string
		want   string // "" => zero
	}{
		{"both_outcome_newer", map[string]string{"last_outcome_at": newer, "last_run": older}, newer},
		{"both_run_newer", map[string]string{"last_outcome_at": older, "last_run": newer}, newer},
		{"only_run", map[string]string{"last_run": newer}, newer},
		{"only_outcome", map[string]string{"last_outcome_at": newer}, newer},
		{"neither", map[string]string{}, ""},
		{"unparseable_ignored", map[string]string{"last_outcome_at": "garbage", "last_run": newer}, newer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := gardenerLastFire(tc.fields)
			if tc.want == "" {
				if !got.IsZero() {
					t.Fatalf("got %v, want zero", got)
				}
				return
			}
			if !got.Equal(mustTime(t, tc.want)) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestGardenerCooldownMinutes covers the parse + non-negative clamp.
func TestGardenerCooldownMinutes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"5", 5},
		{"", 0},
		{"  15 ", 15},
		{"garbage", 0},
		{"-3", 0}, // negative clamps to 0
	}
	for _, tc := range cases {
		got := gardenerCooldownMinutes(map[string]string{"cooldown_minutes": tc.in})
		if got != tc.want {
			t.Errorf("cooldown %q = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestFreshestProgress picks the newest last_progress_at across the life-thread
// and returns zero when none carry a heartbeat.
func TestFreshestProgress(t *testing.T) {
	t1 := mustTime(t, "2026-08-16T07:00:00Z")
	t2 := mustTime(t, "2026-08-16T09:00:00Z")
	iters := []gardenerIterationView{
		{agentRowView: agentRowView{LastProgressAt: t1}},
		{agentRowView: agentRowView{LastProgressAt: t2}},
		{agentRowView: agentRowView{}}, // no heartbeat
	}
	if got := freshestProgress(iters); !got.Equal(t2) {
		t.Fatalf("freshestProgress = %v, want %v", got, t2)
	}
	if got := freshestProgress([]gardenerIterationView{{}, {}}); !got.IsZero() {
		t.Fatalf("freshestProgress with no heartbeats = %v, want zero", got)
	}
	if got := freshestProgress(nil); !got.IsZero() {
		t.Fatalf("freshestProgress(nil) = %v, want zero", got)
	}
}
