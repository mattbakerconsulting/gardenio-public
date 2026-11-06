package postgres

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// These cover runner_tokens.go (local-agents Task 7, gd-zPtKJKzZhE) with
// sqlmock: insert, lookup (found, absent, revoked) and per-runner revoke.

func TestInsertRunnerToken(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`INSERT INTO runner_tokens`).
		WithArgs("h1", "gd-r1", now).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := st.InsertRunnerToken(context.Background(), &model.RunnerToken{TokenHash: "h1", RunnerID: "gd-r1", CreatedAt: now}); err != nil {
		t.Fatalf("InsertRunnerToken: %v", err)
	}
}

func TestGetRunnerTokenByHash(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cols := []string{"token_hash", "runner_id", "created_at", "revoked_at"}
	mock.ExpectQuery(`FROM runner_tokens WHERE token_hash = \$1`).
		WithArgs("live").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("live", "gd-r1", now, nil))
	mock.ExpectQuery(`FROM runner_tokens WHERE token_hash = \$1`).
		WithArgs("dead").
		WillReturnRows(sqlmock.NewRows(cols).AddRow("dead", "gd-r1", now, now))
	mock.ExpectQuery(`FROM runner_tokens WHERE token_hash = \$1`).
		WithArgs("absent").
		WillReturnRows(sqlmock.NewRows(cols))

	live, err := st.GetRunnerTokenByHash(context.Background(), "live")
	if err != nil || live == nil || live.RunnerID != "gd-r1" || live.RevokedAt != nil {
		t.Fatalf("live token: %+v, %v", live, err)
	}
	dead, err := st.GetRunnerTokenByHash(context.Background(), "dead")
	if err != nil || dead == nil || dead.RevokedAt == nil || !dead.RevokedAt.Equal(now) {
		t.Fatalf("revoked token must carry revoked_at: %+v, %v", dead, err)
	}
	absent, err := st.GetRunnerTokenByHash(context.Background(), "absent")
	if err != nil || absent != nil {
		t.Fatalf("absent token must be (nil, nil), got %+v, %v", absent, err)
	}
}

func TestRevokeRunnerTokens(t *testing.T) {
	db, mock := newMockDB(t)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE runner_tokens SET revoked_at = \$2 WHERE runner_id = \$1 AND revoked_at IS NULL`).
		WithArgs("gd-r1", now).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	n, err := (&txStore{tx: tx}).RevokeRunnerTokens(context.Background(), "gd-r1", now)
	if err != nil || n != 2 {
		t.Fatalf("RevokeRunnerTokens = %d, %v; want 2, nil", n, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
