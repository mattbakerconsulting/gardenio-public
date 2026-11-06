package main

// Per-gardener spend roll-up surfacing (operator request ka-0000003220).
//
// The controller's gardenerSpendSync loop maintains a gardener_spend_usd field on
// each gardener trigger/schedule bead: the summed authoritative total_cost_usd
// over the gardener's iteration runs PLUS the crew (and crew-of-crew) those runs
// transitively spawned. This file projects that maintained field onto the
// gardener index row so /gardeners can show and SORT by spend — surfacing
// "wasteful gardeners" at a glance, which is exactly what the operator asked for.
//
// The viewer NEVER recomputes the roll-up at render time (that BFS belongs in
// the controller, off the request path); it only reads the field the controller
// already wrote and decides how to display it.

import (
	"sort"
	"time"
)

// gardenerSpendField / gardenerSpendSyncedAtField mirror the controller-side field
// names (controller/gardener_spend_sync.go). Duplicated here rather than shared
// because the two binaries live in different packages and the field contract is
// a small, stable string pair.
const (
	gardenerSpendField         = "gardener_spend_usd"
	gardenerSpendSyncedAtField = "gardener_spend_synced_at"
)

// gardenerSpendStaleAfter is how old a roll-up may be before the index marks the
// figure approximate. The controller recomputes every ~10m; a figure older than
// this means the loop is wedged or the gardener bead stopped being matched, so a
// "~" hint warns the operator the number may lag reality. Generous relative to
// the 10m cadence so a single skipped tick does not flap the chip.
const gardenerSpendStaleAfter = 45 * time.Minute

// applyGardenerSpend reads the controller-maintained gardener_spend_usd off the
// gardener bead's fields and projects it onto the row. A gardener that has never
// been rolled up carries no field; HasSpend is left false so the column renders
// blank rather than a misleading "$0.00". A genuine zero-cost gardener that HAS
// been synced (field present, value 0) renders "$0.00" so the operator can tell
// "cost nothing" from "not measured yet".
func applyGardenerSpend(row *gardenerRowView, fields map[string]string, now time.Time) {
	raw, ok := fields[gardenerSpendField]
	if !ok || raw == "" {
		return
	}
	spend := parseCostField(raw)
	row.HasSpend = true
	row.spendUSD = spend
	row.SpendLabel = fmtUSD(spend)
	// Runaway-cost bucket (bug ka-0000001009 / C1): color the chip and, for an
	// OPEN gardener in the "over" bucket, bump NeedsAttention so a quietly-burning
	// loop bubbles up without the operator having to Spend-sort. Scoped to open
	// gardeners like the verdict-based attention bump (a closed gardener cannot be
	// operated, so flagging its spend is noise).
	row.SpendLevel = gardenerSpendLevel(spend)
	if row.SpendLevel == "over" && !row.IsClosed {
		row.NeedsAttention = true
	}
	if syncedAt := fields[gardenerSpendSyncedAtField]; syncedAt != "" {
		if t, perr := time.Parse(time.RFC3339, syncedAt); perr == nil {
			row.SpendStale = now.Sub(t) > gardenerSpendStaleAfter
		}
	}
}

// sortGardenerRowsBySpend re-orders rows by spend descending so the costliest
// gardeners lead — the "surface wasteful gardeners" view (ka-0000003220). It is a
// STABLE sort applied AFTER the default run-centric sortGardenerRows, so rows
// with equal spend (and the unsynced, spend-0 tail) retain that ordering as the
// tie-break. Synced rows always outrank never-synced ones at the same numeric
// spend: a measured $0.00 is more informative than an unmeasured blank, so the
// blanks sink to the bottom.
func sortGardenerRowsBySpend(rows []gardenerRowView) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.HasSpend != b.HasSpend {
			return a.HasSpend // measured rows above never-synced blanks
		}
		if a.spendUSD != b.spendUSD {
			return a.spendUSD > b.spendUSD // costliest first
		}
		return false // equal spend: keep prior (run-centric) order
	})
}
