package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests pin the guarded-UPDATE SQL shape and the claimed/not-claimed
// contract for the scrub-request execution idempotency guard
// (gd-P6uCMzQqU9), mirroring escalation_replay_claim_test.go.

func TestClaimScrubExecution_Claims(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads\s+SET fields = jsonb_set\(COALESCE\(fields, '\{\}'::jsonb\), '\{scrub_execution_claimed\}', 'true'::jsonb, true\)`).
		WithArgs("scrub-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	claimed, err := store.ClaimScrubExecution(context.Background(), "scrub-1")
	if err != nil {
		t.Fatalf("ClaimScrubExecution: %v", err)
	}
	if !claimed {
		t.Error("claimed = false, want true when the guarded UPDATE matched a row")
	}
}

func TestClaimScrubExecution_AlreadyClaimedIsNotAnError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	// 0 rows affected: the WHERE guard excluded the row because
	// scrub_execution_claimed is already "true" (or the bead is
	// missing/deleted) — this is the case a concurrent approve loses.
	mock.ExpectExec(`UPDATE beads`).
		WithArgs("scrub-2").
		WillReturnResult(sqlmock.NewResult(0, 0))

	claimed, err := store.ClaimScrubExecution(context.Background(), "scrub-2")
	if err != nil {
		t.Fatalf("ClaimScrubExecution: err = %v, want nil for a lost claim race", err)
	}
	if claimed {
		t.Error("claimed = true, want false when 0 rows matched")
	}
}

func TestClaimScrubExecution_ExecError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("scrub-3").
		WillReturnError(errors.New("boom"))

	if _, err := store.ClaimScrubExecution(context.Background(), "scrub-3"); err == nil {
		t.Fatal("ClaimScrubExecution: err = nil, want non-nil for exec error")
	}
}

func TestReleaseScrubExecutionClaim(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads\s+SET fields = jsonb_set\(COALESCE\(fields, '\{\}'::jsonb\), '\{scrub_execution_claimed\}', 'false'::jsonb, true\)`).
		WithArgs("scrub-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.ReleaseScrubExecutionClaim(context.Background(), "scrub-1"); err != nil {
		t.Fatalf("ReleaseScrubExecutionClaim: %v", err)
	}
}

func TestReleaseScrubExecutionClaim_MissingBeadIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("scrub-missing").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.ReleaseScrubExecutionClaim(context.Background(), "scrub-missing"); err != nil {
		t.Fatalf("ReleaseScrubExecutionClaim on missing bead: err = %v, want nil", err)
	}
}

func TestTxStore_ClaimScrubExecution(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE beads\s+SET fields = jsonb_set`).
		WithArgs("scrub-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	claimed, err := ts.ClaimScrubExecution(context.Background(), "scrub-1")
	if err != nil {
		t.Fatalf("txStore.ClaimScrubExecution: %v", err)
	}
	if !claimed {
		t.Error("claimed = false, want true")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
