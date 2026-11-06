package main

import (
	"testing"
	"time"
)

func TestApplyGardenerSpend(t *testing.T) {
	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)

	t.Run("never synced leaves HasSpend false", func(t *testing.T) {
		var row gardenerRowView
		applyGardenerSpend(&row, map[string]string{}, now)
		if row.HasSpend {
			t.Fatalf("HasSpend should be false when field absent")
		}
		if row.SpendLabel != "" {
			t.Fatalf("SpendLabel should be empty, got %q", row.SpendLabel)
		}
	})

	t.Run("synced zero renders $0.00", func(t *testing.T) {
		var row gardenerRowView
		applyGardenerSpend(&row, map[string]string{
			gardenerSpendField:         "0",
			gardenerSpendSyncedAtField: now.Format(time.RFC3339),
		}, now)
		if !row.HasSpend {
			t.Fatalf("HasSpend should be true for a synced zero")
		}
		if row.SpendLabel != "$0.00" {
			t.Fatalf("SpendLabel = %q, want $0.00", row.SpendLabel)
		}
		if row.SpendStale {
			t.Fatalf("fresh sync should not be stale")
		}
	})

	t.Run("formats and detects stale", func(t *testing.T) {
		var row gardenerRowView
		stale := now.Add(-2 * time.Hour).Format(time.RFC3339)
		applyGardenerSpend(&row, map[string]string{
			gardenerSpendField:         "12.5",
			gardenerSpendSyncedAtField: stale,
		}, now)
		if row.SpendLabel != "$12.50" {
			t.Fatalf("SpendLabel = %q, want $12.50", row.SpendLabel)
		}
		if !row.SpendStale {
			t.Fatalf("a 2h-old roll-up should be flagged stale (threshold %v)", gardenerSpendStaleAfter)
		}
		if row.spendUSD != 12.5 {
			t.Fatalf("spendUSD = %v, want 12.5", row.spendUSD)
		}
	})
}

func TestSortGardenerRowsBySpend(t *testing.T) {
	rows := []gardenerRowView{
		{ID: "cheap", HasSpend: true, spendUSD: 1.0},
		{ID: "unsynced"},
		{ID: "pricey", HasSpend: true, spendUSD: 99.0},
		{ID: "mid", HasSpend: true, spendUSD: 50.0},
	}
	sortGardenerRowsBySpend(rows)

	want := []string{"pricey", "mid", "cheap", "unsynced"}
	for i, id := range want {
		if rows[i].ID != id {
			t.Fatalf("rows[%d].ID = %q, want %q (order: %v)", i, rows[i].ID, id, idsOf(rows))
		}
	}
}

func idsOf(rows []gardenerRowView) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
