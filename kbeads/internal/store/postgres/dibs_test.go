package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests exercise the dibs operator-flip query layer (dibs.go) with
// sqlmock. They pin the SQL shape and the "missing/soft-deleted bead is a
// no-op" contract that the dibs hot path depends on.

func TestGetBeadOperator_Found(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT o\.kind \|\| ':' \|\| o\.subject`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{"op"}).AddRow("oidc:matt@x.ai"))

	op, err := store.GetBeadOperator(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("GetBeadOperator: %v", err)
	}
	if op != "oidc:matt@x.ai" {
		t.Errorf("op = %q, want oidc:matt@x.ai", op)
	}
}

func TestGetBeadOperator_MissingBeadIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT o\.kind \|\| ':' \|\| o\.subject`).
		WithArgs("gd-missing").
		WillReturnError(sql.ErrNoRows)

	op, err := store.GetBeadOperator(context.Background(), "gd-missing")
	if err != nil {
		t.Fatalf("GetBeadOperator on missing bead: err = %v, want nil", err)
	}
	if op != "" {
		t.Errorf("op = %q, want empty for missing bead", op)
	}
}

func TestGetBeadOperator_NullOperator(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	// Bead exists but operated_by_id IS NULL -> NULL scanned -> "".
	mock.ExpectQuery(`SELECT o\.kind \|\| ':' \|\| o\.subject`).
		WithArgs("gd-2").
		WillReturnRows(sqlmock.NewRows([]string{"op"}).AddRow(nil))

	op, err := store.GetBeadOperator(context.Background(), "gd-2")
	if err != nil {
		t.Fatalf("GetBeadOperator: %v", err)
	}
	if op != "" {
		t.Errorf("op = %q, want empty for NULL operator", op)
	}
}

func TestGetBeadOperator_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT o\.kind`).
		WithArgs("gd-3").
		WillReturnError(errors.New("boom"))

	if _, err := store.GetBeadOperator(context.Background(), "gd-3"); err == nil {
		t.Fatal("GetBeadOperator: err = nil, want non-nil for query error")
	}
}

func TestUpdateBeadOperator(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads\s+SET operated_by_id = \$2,\s+updated_at = CASE WHEN status = 'closed' THEN updated_at ELSE NOW\(\) END`).
		WithArgs("gd-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.UpdateBeadOperator(context.Background(), "gd-1", 42); err != nil {
		t.Fatalf("UpdateBeadOperator: %v", err)
	}
}

func TestUpdateBeadOperator_MissingBeadIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	// 0 rows affected (missing/soft-deleted) is not an error.
	mock.ExpectExec(`UPDATE beads`).
		WithArgs("gd-missing", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.UpdateBeadOperator(context.Background(), "gd-missing", 42); err != nil {
		t.Fatalf("UpdateBeadOperator on missing bead: err = %v, want nil", err)
	}
}

func TestUpdateBeadOperator_ExecError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("gd-1", int64(42)).
		WillReturnError(errors.New("boom"))

	if err := store.UpdateBeadOperator(context.Background(), "gd-1", 42); err == nil {
		t.Fatal("UpdateBeadOperator: err = nil, want non-nil for exec error")
	}
}

// --- txStore variants: drive through a real *sql.Tx from sqlmock so the
//     transaction-scoped dibs methods are covered too. ---

func TestTxStore_DibsRoundTrip(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT o\.kind \|\| ':' \|\| o\.subject`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{"op"}).AddRow("oidc:a@x.ai"))
	mock.ExpectExec(`UPDATE beads`).
		WithArgs("gd-1", int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	op, err := ts.GetBeadOperator(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("txStore.GetBeadOperator: %v", err)
	}
	if op != "oidc:a@x.ai" {
		t.Errorf("op = %q, want oidc:a@x.ai", op)
	}
	if err := ts.UpdateBeadOperator(context.Background(), "gd-1", 9); err != nil {
		t.Fatalf("txStore.UpdateBeadOperator: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestTxStore_GetBeadOperator_MissingIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT o\.kind`).
		WithArgs("gd-x").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	op, err := ts.GetBeadOperator(context.Background(), "gd-x")
	if err != nil {
		t.Fatalf("txStore.GetBeadOperator missing: err = %v, want nil", err)
	}
	if op != "" {
		t.Errorf("op = %q, want empty", op)
	}
	_ = tx.Rollback()
}
