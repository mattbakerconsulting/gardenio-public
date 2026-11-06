package postgres

import (
	"context"
	"regexp"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// reversedScanRE matches the reversed-edge scan SELECT used by
// fetchReversedParentChildPage. Anchored on the distinctive epic/child
// joins and the LIMIT clause.
var reversedScanRE = regexp.MustCompile(`SELECT d\.bead_id, d\.depends_on_id, child\.type[\s\S]+LIMIT \$3`)

var reversedScanCols = []string{"bead_id", "depends_on_id", "child_type", "created_by"}

func TestRepairReversedParentChildDeps_NoReversedRows(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(reversedScanRE.String()).
		WithArgs("", "", 500).
		WillReturnRows(sqlmock.NewRows(reversedScanCols))

	stats, err := queryRepairReversedParentChildDeps(context.Background(), db, store.RepairReversedParentChildDepsOptions{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.ReversedRows != 0 || stats.EdgesFlipped != 0 {
		t.Errorf("counts = %d/%d, want 0/0", stats.ReversedRows, stats.EdgesFlipped)
	}
	if stats.Batches != 0 {
		t.Errorf("Batches = %d, want 0", stats.Batches)
	}
}

func TestRepairReversedParentChildDeps_DryRunReportsWithoutWriting(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows(reversedScanCols).
		AddRow("gd-epic1", "gd-childA", "task", "thread-1").
		AddRow("gd-epic1", "gd-childB", "feature", "thread-1")

	mock.ExpectQuery(reversedScanRE.String()).
		WithArgs("", "", 500).
		WillReturnRows(rows)
	// DryRun: no INSERT/DELETE expected, and the scan runs once.

	stats, err := queryRepairReversedParentChildDeps(context.Background(), db, store.RepairReversedParentChildDepsOptions{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.ReversedRows != 2 {
		t.Errorf("ReversedRows = %d, want 2", stats.ReversedRows)
	}
	if stats.EdgesFlipped != 0 {
		t.Errorf("EdgesFlipped = %d, want 0 (dry-run)", stats.EdgesFlipped)
	}
	if len(stats.Edges) != 2 || stats.Edges[0].EpicID != "gd-epic1" || stats.Edges[0].ChildID != "gd-childA" {
		t.Errorf("Edges = %+v, want the two reversed rows reported", stats.Edges)
	}
}

func TestRepairReversedParentChildDeps_FlipsReversedRow(t *testing.T) {
	db, mock := newMockDB(t)

	// Page 1: one reversed row found.
	mock.ExpectQuery(reversedScanRE.String()).
		WithArgs("", "", 500).
		WillReturnRows(sqlmock.NewRows(reversedScanCols).
			AddRow("gd-epic1", "gd-childA", "task", "thread-1"))

	// Repair tx: insert canonical (child -> epic) then delete reversed.
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs("gd-childA", "gd-epic1", "gd-epic1", "gd-childA").
		WillReturnResult(sqlmock.NewResult(0, 1)) // 1 row inserted → flipped
	mock.ExpectExec(`DELETE FROM deps`).
		WithArgs("gd-epic1", "gd-childA").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Page 1 had < BatchSize rows, so the loop stops after one batch.

	stats, err := queryRepairReversedParentChildDeps(context.Background(), db, store.RepairReversedParentChildDepsOptions{
		DryRun: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.ReversedRows != 1 {
		t.Errorf("ReversedRows = %d, want 1", stats.ReversedRows)
	}
	if stats.EdgesFlipped != 1 {
		t.Errorf("EdgesFlipped = %d, want 1", stats.EdgesFlipped)
	}
	if stats.SkippedCanonicalExists != 0 {
		t.Errorf("SkippedCanonicalExists = %d, want 0", stats.SkippedCanonicalExists)
	}
}

func TestRepairReversedParentChildDeps_SkipsWhenCanonicalExists(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(reversedScanRE.String()).
		WithArgs("", "", 500).
		WillReturnRows(sqlmock.NewRows(reversedScanCols).
			AddRow("gd-epic1", "gd-childA", "task", "thread-1"))

	mock.ExpectBegin()
	// Canonical twin already exists → ON CONFLICT DO NOTHING → 0 rows.
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs("gd-childA", "gd-epic1", "gd-epic1", "gd-childA").
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Reversed row is still removed.
	mock.ExpectExec(`DELETE FROM deps`).
		WithArgs("gd-epic1", "gd-childA").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stats, err := queryRepairReversedParentChildDeps(context.Background(), db, store.RepairReversedParentChildDepsOptions{
		DryRun: false,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.EdgesFlipped != 0 {
		t.Errorf("EdgesFlipped = %d, want 0 (canonical twin existed)", stats.EdgesFlipped)
	}
	if stats.SkippedCanonicalExists != 1 {
		t.Errorf("SkippedCanonicalExists = %d, want 1", stats.SkippedCanonicalExists)
	}
}

func TestRepairReversedParentChildDepsOptions_Validate(t *testing.T) {
	o := store.RepairReversedParentChildDepsOptions{}
	if err := o.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if o.BatchSize != 500 {
		t.Errorf("default BatchSize = %d, want 500", o.BatchSize)
	}

	bad := store.RepairReversedParentChildDepsOptions{BatchSize: -1}
	if err := bad.Validate(); err == nil {
		t.Errorf("Validate() with negative BatchSize: error = nil, want non-nil")
	}
}
