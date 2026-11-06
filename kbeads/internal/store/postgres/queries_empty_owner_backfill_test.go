package postgres

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// testOperatedByID is a stand-in resolved operators(id) FK the non-dry-run
// tests pass; migration 026's NOT-NULL CHECK on
// mutation_history.operated_by_id requires a non-zero value.
const testOperatedByID int64 = 42

func TestQueryBackfillEmptyOwner_BatchSizeInvalid(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryBackfillEmptyOwner(context.Background(), db, store.BackfillEmptyOwnerOptions{BatchSize: -1})
	if err == nil {
		t.Fatal("expected error for negative BatchSize")
	}
}

func TestQueryBackfillEmptyOwner_NonDryRunRequiresOperator(t *testing.T) {
	db, _ := newMockDB(t)
	// A non-dry-run with no resolved OperatedByID must error BEFORE any
	// query runs — otherwise migration 026's NOT-NULL CHECK on
	// mutation_history.operated_by_id would 500 on the first phase-1 batch.
	_, err := queryBackfillEmptyOwner(context.Background(), db, store.BackfillEmptyOwnerOptions{})
	if err == nil {
		t.Fatal("expected error for non-dry-run without OperatedByID")
	}
}

func TestQueryBackfillEmptyOwner_DryRunPartitions(t *testing.T) {
	db, mock := newMockDB(t)

	// before count: total owner='' rows.
	mock.ExpectQuery(`SELECT count\(\*\) FROM beads`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(100)))
	// phase 1 dry count (operator resolves to a non-legacy identity).
	mock.ExpectQuery(`JOIN operators op`).
		WithArgs("system:legacy-pre-attribution").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(80)))
	// phase 2 dry count (legacy-sentinel operator, but created_by usable).
	mock.ExpectQuery(`b\.created_by <> ALL`).
		WithArgs("system:legacy-pre-attribution", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(15)))

	stats, err := queryBackfillEmptyOwner(context.Background(), db, store.BackfillEmptyOwnerOptions{DryRun: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeforeEmpty != 100 {
		t.Errorf("BeforeEmpty = %d, want 100", stats.BeforeEmpty)
	}
	if stats.Phase1Filled != 80 {
		t.Errorf("Phase1Filled = %d, want 80", stats.Phase1Filled)
	}
	if stats.Phase2Filled != 15 {
		t.Errorf("Phase2Filled = %d, want 15", stats.Phase2Filled)
	}
	// Residual = 100 - 80 - 15 = 5.
	if stats.Phase3Filled != 5 {
		t.Errorf("Phase3Filled = %d, want 5", stats.Phase3Filled)
	}
	// Dry-run never touches after count / batches.
	if stats.AfterEmpty != 0 || stats.Phase1Batches != 0 {
		t.Errorf("dry-run left AfterEmpty=%d Phase1Batches=%d, want 0/0", stats.AfterEmpty, stats.Phase1Batches)
	}
	if !stats.DryRun {
		t.Error("DryRun flag not propagated")
	}
}

func TestQueryBackfillEmptyOwner_LiveThreePhaseDrain(t *testing.T) {
	db, mock := newMockDB(t)

	// before count.
	mock.ExpectQuery(`SELECT count\(\*\) FROM beads`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))

	// Each phase: one non-empty batch (1 row + mutation_history insert),
	// then one empty terminating batch (0 rows).
	expectPhase := func(beadID, newOwner, mutationType string) {
		// non-empty batch
		mock.ExpectBegin()
		mock.ExpectQuery(`WITH batch AS`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "owner"}).AddRow(beadID, newOwner))
		mock.ExpectExec(`INSERT INTO mutation_history`).
			WithArgs(beadID, "kd admin backfill-empty-owner", "owner", "", newOwner, mutationType, testOperatedByID).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()
		// empty terminating batch
		mock.ExpectBegin()
		mock.ExpectQuery(`WITH batch AS`).
			WillReturnRows(sqlmock.NewRows([]string{"id", "owner"}))
		mock.ExpectCommit()
	}

	expectPhase("b1", "slack:U1", "backfill-empty-owner-phase1")
	expectPhase("b2", "gardenio-agent-x", "backfill-empty-owner-phase2")
	expectPhase("b3", "system:unowned", "backfill-empty-owner-phase3")

	// after count: drained.
	mock.ExpectQuery(`SELECT count\(\*\) FROM beads`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillEmptyOwner(context.Background(), db, store.BackfillEmptyOwnerOptions{OperatedByID: testOperatedByID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeforeEmpty != 3 {
		t.Errorf("BeforeEmpty = %d, want 3", stats.BeforeEmpty)
	}
	if stats.AfterEmpty != 0 {
		t.Errorf("AfterEmpty = %d, want 0", stats.AfterEmpty)
	}
	if stats.Phase1Filled != 1 || stats.Phase2Filled != 1 || stats.Phase3Filled != 1 {
		t.Errorf("filled = %d/%d/%d, want 1/1/1", stats.Phase1Filled, stats.Phase2Filled, stats.Phase3Filled)
	}
	if stats.Phase1Batches != 1 || stats.Phase2Batches != 1 || stats.Phase3Batches != 1 {
		t.Errorf("batches = %d/%d/%d, want 1/1/1", stats.Phase1Batches, stats.Phase2Batches, stats.Phase3Batches)
	}
	if stats.BatchesCapped {
		t.Error("BatchesCapped = true, want false")
	}
}

func TestQueryBackfillEmptyOwner_MaxBatchesCaps(t *testing.T) {
	db, mock := newMockDB(t)

	// before count.
	mock.ExpectQuery(`SELECT count\(\*\) FROM beads`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(10)))

	// Phase 1 with MaxBatches=1: one batch runs (returns rows), then the
	// cap halts the phase before draining.
	mock.ExpectBegin()
	mock.ExpectQuery(`WITH batch AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner"}).AddRow("b1", "slack:U1"))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Phase 2 with MaxBatches=1: same shape.
	mock.ExpectBegin()
	mock.ExpectQuery(`WITH batch AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner"}).AddRow("b2", "agent-x"))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Phase 3 with MaxBatches=1: same shape.
	mock.ExpectBegin()
	mock.ExpectQuery(`WITH batch AS`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "owner"}).AddRow("b3", "system:unowned"))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// after count: still 7 remaining because each phase was capped at 1 batch.
	mock.ExpectQuery(`SELECT count\(\*\) FROM beads`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(7)))

	stats, err := queryBackfillEmptyOwner(context.Background(), db, store.BackfillEmptyOwnerOptions{MaxBatches: 1, OperatedByID: testOperatedByID})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stats.BatchesCapped {
		t.Error("BatchesCapped = false, want true (MaxBatches hit)")
	}
	if stats.AfterEmpty != 7 {
		t.Errorf("AfterEmpty = %d, want 7", stats.AfterEmpty)
	}
}
