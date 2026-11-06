package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests pin TryAdvisoryLock's SQL shape (pg_try_advisory_xact_lock)
// and its commit/rollback-on-acquired/skip-on-contention behavior (Phase 2
// 2-replica spike, gd-XxrObXvc8r).

func TestTryAdvisoryLock_AcquiredRunsFnAndCommits(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT pg_try_advisory_xact_lock\(\$1\)`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_xact_lock"}).AddRow(true))
	mock.ExpectCommit()

	ran := false
	acquired, err := store.TryAdvisoryLock(context.Background(), 42, func(ctx context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("TryAdvisoryLock: %v", err)
	}
	if !acquired {
		t.Error("acquired = false, want true")
	}
	if !ran {
		t.Error("fn was not invoked despite the lock being acquired")
	}
}

func TestTryAdvisoryLock_NotAcquiredSkipsFn(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT pg_try_advisory_xact_lock\(\$1\)`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_xact_lock"}).AddRow(false))
	mock.ExpectRollback()

	ran := false
	acquired, err := store.TryAdvisoryLock(context.Background(), 42, func(ctx context.Context) error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("TryAdvisoryLock: %v", err)
	}
	if acquired {
		t.Error("acquired = true, want false when another session holds the lock")
	}
	if ran {
		t.Error("fn was invoked despite the lock not being acquired")
	}
}

func TestTryAdvisoryLock_FnErrorRollsBack(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT pg_try_advisory_xact_lock\(\$1\)`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"pg_try_advisory_xact_lock"}).AddRow(true))
	mock.ExpectRollback()

	acquired, err := store.TryAdvisoryLock(context.Background(), 7, func(ctx context.Context) error {
		return errors.New("sweep failed")
	})
	if !acquired {
		t.Error("acquired = false, want true — the lock WAS acquired even though fn errored")
	}
	if err == nil {
		t.Fatal("err = nil, want the fn error propagated")
	}
}

func TestTryAdvisoryLock_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT pg_try_advisory_xact_lock\(\$1\)`).
		WithArgs(int64(1)).
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if _, err := store.TryAdvisoryLock(context.Background(), 1, func(ctx context.Context) error {
		t.Fatal("fn should not run when the lock query itself errors")
		return nil
	}); err == nil {
		t.Fatal("err = nil, want non-nil for a lock query error")
	}
}
