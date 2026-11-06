package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests pin the guarded-UPDATE SQL shape and the claimed/not-claimed
// contract for the escalation-replay idempotency guard (Phase 2 2-replica
// spike, gd-XxrObXvc8r).

func TestClaimEscalationReplay_Claims(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads\s+SET fields = jsonb_set\(COALESCE\(fields, '\{\}'::jsonb\), '\{replay_in_progress\}', 'true'::jsonb, true\)`).
		WithArgs("esc-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	claimed, err := store.ClaimEscalationReplay(context.Background(), "esc-1")
	if err != nil {
		t.Fatalf("ClaimEscalationReplay: %v", err)
	}
	if !claimed {
		t.Error("claimed = false, want true when the guarded UPDATE matched a row")
	}
}

func TestClaimEscalationReplay_AlreadyClaimedIsNotAnError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	// 0 rows affected: the WHERE guard excluded the row because
	// replay_in_progress is already "true" (or the bead is missing/deleted).
	mock.ExpectExec(`UPDATE beads`).
		WithArgs("esc-2").
		WillReturnResult(sqlmock.NewResult(0, 0))

	claimed, err := store.ClaimEscalationReplay(context.Background(), "esc-2")
	if err != nil {
		t.Fatalf("ClaimEscalationReplay: err = %v, want nil for a lost claim race", err)
	}
	if claimed {
		t.Error("claimed = true, want false when 0 rows matched")
	}
}

func TestClaimEscalationReplay_ExecError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("esc-3").
		WillReturnError(errors.New("boom"))

	if _, err := store.ClaimEscalationReplay(context.Background(), "esc-3"); err == nil {
		t.Fatal("ClaimEscalationReplay: err = nil, want non-nil for exec error")
	}
}

func TestReleaseEscalationReplayClaim(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads\s+SET fields = jsonb_set\(COALESCE\(fields, '\{\}'::jsonb\), '\{replay_in_progress\}', 'false'::jsonb, true\)`).
		WithArgs("esc-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.ReleaseEscalationReplayClaim(context.Background(), "esc-1"); err != nil {
		t.Fatalf("ReleaseEscalationReplayClaim: %v", err)
	}
}

func TestReleaseEscalationReplayClaim_MissingBeadIsNoOp(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("esc-missing").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := store.ReleaseEscalationReplayClaim(context.Background(), "esc-missing"); err != nil {
		t.Fatalf("ReleaseEscalationReplayClaim on missing bead: err = %v, want nil", err)
	}
}

func TestTxStore_ClaimEscalationReplay(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE beads\s+SET fields = jsonb_set`).
		WithArgs("esc-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	claimed, err := ts.ClaimEscalationReplay(context.Background(), "esc-1")
	if err != nil {
		t.Fatalf("txStore.ClaimEscalationReplay: %v", err)
	}
	if !claimed {
		t.Error("claimed = false, want true")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
