package main

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestGardenerOwnedEpicID covers the owned-epic resolution: the modern
// gardener-epic:<id> label, the legacy assign_bead field (which wins), and the
// no-owned-epic case.
func TestGardenerOwnedEpicID(t *testing.T) {
	tests := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{"nil", nil, ""},
		{
			"label",
			&beadsapi.BeadDetail{Labels: []string{"role:gardener", "gardener-epic:gd-epic1"}},
			"gd-epic1",
		},
		{
			"assign_bead field",
			&beadsapi.BeadDetail{Fields: map[string]string{"assign_bead": "gd-legacy1"}},
			"gd-legacy1",
		},
		{
			"assign_bead wins over label",
			&beadsapi.BeadDetail{
				Fields: map[string]string{"assign_bead": "gd-legacy1"},
				Labels: []string{"gardener-epic:gd-epic1"},
			},
			"gd-legacy1",
		},
		{
			"empty label value ignored",
			&beadsapi.BeadDetail{Labels: []string{"gardener-epic:"}},
			"",
		},
		{
			"none",
			&beadsapi.BeadDetail{Labels: []string{"role:gardener"}},
			"",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gardenerOwnedEpicID(tc.bead); got != tc.want {
				t.Errorf("gardenerOwnedEpicID = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOwnedEpicRollupPctDone covers the percentage math, including the
// divide-by-zero guard for a childless epic.
func TestOwnedEpicRollupPctDone(t *testing.T) {
	tests := []struct {
		total, closed, want int
	}{
		{0, 0, 0},
		{40, 12, 30},
		{40, 40, 100},
		{3, 1, 33}, // truncates, not rounds
		{40, 0, 0},
	}
	for _, tc := range tests {
		r := ownedEpicRollup{Total: tc.total, Closed: tc.closed}
		if got := r.PctDone(); got != tc.want {
			t.Errorf("PctDone(%d/%d) = %d, want %d", tc.closed, tc.total, got, tc.want)
		}
	}
}

func testServerWithDaemon(d beadAPI) *Server {
	return &Server{daemon: d, logger: slog.Default()}
}

// TestEnrichGardenerOwnedEpics_Rollup asserts the batch rollup fills each row's
// owned-epic fields from the subtree counts, dedupes two gardeners pinned to the
// same epic, and gates the chip on a non-empty subtree.
func TestEnrichGardenerOwnedEpics_Rollup(t *testing.T) {
	fake := &fakeDaemon{
		epicSubtreeCount: map[string]int{
			"gd-epicA":        40,
			"gd-epicA|closed": 12,
			"gd-epicB":        5,
			"gd-epicB|closed": 5,
			// gd-epicEmpty has no children → total 0, no chip.
		},
	}
	rows := []gardenerRowView{
		{ID: "gd-cap1", ownedEpicID: "gd-epicA"},
		{ID: "gd-cap2", ownedEpicID: "gd-epicA"}, // shares epicA → deduped
		{ID: "gd-cap3", ownedEpicID: "gd-epicB"},
		{ID: "gd-cap4", ownedEpicID: "gd-epicEmpty"}, // childless → no chip
		{ID: "gd-cap5"}, // no owned epic → no chip
	}
	s := testServerWithDaemon(fake)
	s.enrichGardenerOwnedEpics(context.Background(), rows)

	// epicA gardeners.
	for _, i := range []int{0, 1} {
		r := rows[i]
		if !r.HasOwnedEpic {
			t.Fatalf("row %d (epicA) HasOwnedEpic = false", i)
		}
		if r.OwnedEpicTotal != 40 || r.OwnedEpicClosed != 12 || r.OwnedEpicOpen != 28 || r.OwnedEpicPct != 30 {
			t.Errorf("row %d epicA rollup = %d/%d open=%d pct=%d, want 12/40 open=28 pct=30",
				i, r.OwnedEpicClosed, r.OwnedEpicTotal, r.OwnedEpicOpen, r.OwnedEpicPct)
		}
		if r.OwnedEpicID != "gd-epicA" {
			t.Errorf("row %d OwnedEpicID = %q, want gd-epicA", i, r.OwnedEpicID)
		}
	}
	// epicB fully drained.
	if r := rows[2]; !r.HasOwnedEpic || r.OwnedEpicPct != 100 || r.OwnedEpicOpen != 0 {
		t.Errorf("epicB rollup = %+v, want 5/5 pct=100 open=0", r)
	}
	// Childless and no-epic rows carry no chip.
	if rows[3].HasOwnedEpic {
		t.Errorf("childless epic should not render a chip: %+v", rows[3])
	}
	if rows[4].HasOwnedEpic {
		t.Errorf("no-owned-epic row should not render a chip: %+v", rows[4])
	}
}

// TestEnrichGardenerOwnedEpics_QueryError leaves the row without a rollup when the
// subtree count query errors, rather than rendering a wrong/zero count.
func TestEnrichGardenerOwnedEpics_QueryError(t *testing.T) {
	fake := &fakeDaemon{listErr: context.DeadlineExceeded}
	rows := []gardenerRowView{{ID: "gd-cap1", ownedEpicID: "gd-epicA"}}
	s := testServerWithDaemon(fake)
	s.enrichGardenerOwnedEpics(context.Background(), rows)
	if rows[0].HasOwnedEpic {
		t.Errorf("query error should leave HasOwnedEpic false, got %+v", rows[0])
	}
}

// TestEnrichGardenerOwnedEpics_NoEpics is a no-op when no row owns an epic.
func TestEnrichGardenerOwnedEpics_NoEpics(t *testing.T) {
	fake := &fakeDaemon{}
	rows := []gardenerRowView{{ID: "gd-cap1"}, {ID: "gd-cap2"}}
	s := testServerWithDaemon(fake)
	s.enrichGardenerOwnedEpics(context.Background(), rows)
	for i, r := range rows {
		if r.HasOwnedEpic {
			t.Errorf("row %d should have no rollup, got %+v", i, r)
		}
	}
	if len(fake.listAllQ) != 0 {
		t.Errorf("no epics → no daemon queries, got %d", len(fake.listAllQ))
	}
}
