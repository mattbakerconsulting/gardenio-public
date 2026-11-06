package main

import (
	"testing"
	"time"
)

func TestGardenerSpendLevel_Defaults(t *testing.T) {
	// Env unset → the built-in warn=$100 / over=$250 thresholds.
	t.Setenv(gardenerSpendWarnEnv, "")
	t.Setenv(gardenerSpendOverEnv, "")

	cases := []struct {
		spend float64
		want  string
	}{
		{0, ""},
		{99.99, ""},
		{100, "warn"},
		{249.99, "warn"},
		{250, "over"},
		{581.6, "over"}, // prod top gardener (ground truth 2026-08-17)
	}
	for _, c := range cases {
		if got := gardenerSpendLevel(c.spend); got != c.want {
			t.Errorf("gardenerSpendLevel(%v) = %q, want %q", c.spend, got, c.want)
		}
	}
}

func TestGardenerSpendThresholds_EnvOverride(t *testing.T) {
	t.Setenv(gardenerSpendWarnEnv, "10")
	t.Setenv(gardenerSpendOverEnv, "20")
	warn, over := gardenerSpendThresholds()
	if warn != 10 || over != 20 {
		t.Fatalf("thresholds = (%v,%v), want (10,20)", warn, over)
	}
	if got := gardenerSpendLevel(15); got != "warn" {
		t.Errorf("with warn=10 over=20, level($15) = %q, want warn", got)
	}
	if got := gardenerSpendLevel(25); got != "over" {
		t.Errorf("with warn=10 over=20, level($25) = %q, want over", got)
	}
}

func TestGardenerSpendThresholds_InvertedOverIsClamped(t *testing.T) {
	// A misconfigured over<warn must not invert the buckets (which would make
	// the "over" runaway flag unreachable). over is floored at warn.
	t.Setenv(gardenerSpendWarnEnv, "100")
	t.Setenv(gardenerSpendOverEnv, "50")
	warn, over := gardenerSpendThresholds()
	if warn != 100 || over != 100 {
		t.Fatalf("thresholds = (%v,%v), want (100,100) after clamp", warn, over)
	}
}

func TestGardenerSpendThresholds_JunkFallsBackToDefault(t *testing.T) {
	t.Setenv(gardenerSpendWarnEnv, "not-a-number")
	t.Setenv(gardenerSpendOverEnv, "-5")
	warn, over := gardenerSpendThresholds()
	if warn != gardenerSpendWarnDefaultUSD || over != gardenerSpendOverDefaultUSD {
		t.Fatalf("junk env should fall back to defaults, got (%v,%v)", warn, over)
	}
}

// TestApplyGardenerSpend_LevelAndAttention exercises the C1 runaway bump: an
// "over" spend on an OPEN gardener sets SpendLevel and bumps NeedsAttention; a
// closed gardener (even over threshold) does not; a below-warn gardener gets no
// level and no bump.
func TestApplyGardenerSpend_LevelAndAttention(t *testing.T) {
	t.Setenv(gardenerSpendWarnEnv, "100")
	t.Setenv(gardenerSpendOverEnv, "250")
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	t.Run("over on open gardener bumps attention", func(t *testing.T) {
		row := gardenerRowView{IsClosed: false}
		applyGardenerSpend(&row, map[string]string{
			gardenerSpendField:         "300",
			gardenerSpendSyncedAtField: now.Format(time.RFC3339),
		}, now)
		if row.SpendLevel != "over" {
			t.Fatalf("SpendLevel = %q, want over", row.SpendLevel)
		}
		if !row.NeedsAttention {
			t.Fatalf("an over-spend open gardener should bump NeedsAttention")
		}
	})

	t.Run("over on closed gardener does not bump attention", func(t *testing.T) {
		row := gardenerRowView{IsClosed: true}
		applyGardenerSpend(&row, map[string]string{
			gardenerSpendField:         "300",
			gardenerSpendSyncedAtField: now.Format(time.RFC3339),
		}, now)
		if row.SpendLevel != "over" {
			t.Fatalf("SpendLevel = %q, want over (level still bucketed)", row.SpendLevel)
		}
		if row.NeedsAttention {
			t.Fatalf("a CLOSED gardener must never get the spend attention bump")
		}
	})

	t.Run("below warn gets no level and no bump", func(t *testing.T) {
		row := gardenerRowView{}
		applyGardenerSpend(&row, map[string]string{
			gardenerSpendField:         "5",
			gardenerSpendSyncedAtField: now.Format(time.RFC3339),
		}, now)
		if row.SpendLevel != "" {
			t.Fatalf("SpendLevel = %q, want empty below warn", row.SpendLevel)
		}
		if row.NeedsAttention {
			t.Fatalf("a cheap gardener should not need attention on spend")
		}
	})
}

// TestGardenerRowNeedsAttention_UnionsSpend confirms the shared predicate unions
// the three signals so the chip count and the ?filter=needs-attention view stay
// consistent with the row tint.
func TestGardenerRowNeedsAttention_UnionsSpend(t *testing.T) {
	cases := []struct {
		name string
		row  gardenerRowView
		want bool
	}{
		{"stalled verdict", gardenerRowView{VerdictKind: "stalled"}, true},
		{"pour stalled", gardenerRowView{PourStalled: true}, true},
		{"over spend open", gardenerRowView{SpendLevel: "over", IsClosed: false}, true},
		{"over spend closed", gardenerRowView{SpendLevel: "over", IsClosed: true}, false},
		{"warn spend only", gardenerRowView{SpendLevel: "warn"}, false},
		{"nothing", gardenerRowView{}, false},
	}
	for _, c := range cases {
		if got := gardenerRowNeedsAttention(c.row); got != c.want {
			t.Errorf("%s: gardenerRowNeedsAttention = %v, want %v", c.name, got, c.want)
		}
	}
}
