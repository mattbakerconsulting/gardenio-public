package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"gardenio/kbeads/internal/model"
)

// These tests pin the guarded status-only UPDATE SQL shape and the
// changed/not-changed contract for CASBeadStatus (gd-P6uCMzQqU9, third merge
// desk send-back on PR #169), mirroring scrub_execution_claim_test.go.

func TestCASBeadStatus_Changes(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads\s+SET status = \$2,\s+updated_at = NOW\(\)\s+WHERE id = \$1\s+AND deleted_at IS NULL\s+AND status = \$3`).
		WithArgs("scrub-1", "in_progress", "open").
		WillReturnResult(sqlmock.NewResult(0, 1))

	changed, err := store.CASBeadStatus(context.Background(), "scrub-1", model.StatusOpen, model.StatusInProgress)
	if err != nil {
		t.Fatalf("CASBeadStatus: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true when the guarded UPDATE matched a row")
	}
}

func TestCASBeadStatus_StaleFromIsNotAnError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	// 0 rows affected: the WHERE guard excluded the row because its status
	// is no longer `from` (a concurrent write already moved it on, e.g. to
	// closed) — this is the case a losing concurrent approve's status
	// transition must treat as a no-op, not an error.
	mock.ExpectExec(`UPDATE beads`).
		WithArgs("scrub-2", "in_progress", "open").
		WillReturnResult(sqlmock.NewResult(0, 0))

	changed, err := store.CASBeadStatus(context.Background(), "scrub-2", model.StatusOpen, model.StatusInProgress)
	if err != nil {
		t.Fatalf("CASBeadStatus: err = %v, want nil for a lost race", err)
	}
	if changed {
		t.Error("changed = true, want false when 0 rows matched")
	}
}

func TestCASBeadStatus_ExecError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("scrub-3", "in_progress", "open").
		WillReturnError(errors.New("boom"))

	if _, err := store.CASBeadStatus(context.Background(), "scrub-3", model.StatusOpen, model.StatusInProgress); err == nil {
		t.Fatal("CASBeadStatus: err = nil, want non-nil for exec error")
	}
}

func TestTxStore_CASBeadStatus(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE beads\s+SET status = \$2`).
		WithArgs("scrub-1", "in_progress", "open").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	changed, err := ts.CASBeadStatus(context.Background(), "scrub-1", model.StatusOpen, model.StatusInProgress)
	if err != nil {
		t.Fatalf("txStore.CASBeadStatus: %v", err)
	}
	if !changed {
		t.Error("changed = false, want true")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
