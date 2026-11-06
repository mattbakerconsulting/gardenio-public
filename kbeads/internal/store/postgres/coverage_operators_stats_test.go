package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestCovOpsStatsAggCols mirrors statAggCols (defined in operators_stats_test.go)
// — the ordered superset GROUP BY operated_by_id column set. We reuse the
// shared statAggCols var directly rather than redefine it; this comment marks
// the dependency.

// testCovOpsStatsTxStore begins a transaction on the mock db and returns a
// txStore bound to it. The caller must arrange for the tx to be committed or
// rolled back so ExpectationsWereMet passes; these wrapper tests commit.
func testCovOpsStatsTxStore(t *testing.T, db *sql.DB, mock sqlmock.Sqlmock) (*txStore, *sql.Tx) {
	t.Helper()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	return &txStore{tx: tx}, tx
}

// TestCovOpsStatsOperatorDirectoryStatsTx exercises the txStore wrapper for
// OperatorDirectoryStats (operators_stats.go:46) and the package-level
// operatorDirectoryStats helper via the tx executor. It pins the two-query
// shape and asserts the aggregated result struct fields round-trip through the
// scan.
func TestCovOpsStatsOperatorDirectoryStatsTx(t *testing.T) {
	db, mock := newMockDB(t)
	ts, tx := testCovOpsStatsTxStore(t, db, mock)

	t0 := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`GROUP BY operated_by_id`).
		WillReturnRows(sqlmock.NewRows(statAggCols).
			AddRow(int64(7), 4, 2, 3, 1, 1, 0, 2, 1, 1, t0).
			AddRow(int64(8), 0, 0, 0, 0, 0, 0, 0, 0, 0, t0.Add(-2*time.Hour)))

	mock.ExpectQuery(`ROW_NUMBER\(\) OVER`).
		WithArgs(5, 5000, 168).
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id", "id", "title", "created_at"}).
			AddRow(int64(7), "gd-x1", "Newest", t0).
			AddRow(int64(8), "gd-y1", "Bee", t0.Add(-time.Hour)))

	got, err := ts.OperatorDirectoryStats(context.Background(), 5, 5000, 168)
	if err != nil {
		t.Fatalf("OperatorDirectoryStats(tx): %v", err)
	}
	if len(got.Stats) != 2 {
		t.Fatalf("Stats len = %d, want 2", len(got.Stats))
	}
	s0 := got.Stats[0]
	if s0.OperatorID != 7 || s0.OpenBeads != 4 || s0.InProgress != 2 ||
		s0.Created7d != 3 || s0.Closed7d != 1 || s0.MRsOpen != 1 ||
		s0.MRsMerged7d != 0 || s0.DecisionsPending != 2 || s0.AgentsActive != 1 ||
		s0.GardenersActive != 1 || !s0.LastActive.Equal(t0) {
		t.Errorf("Stats[0] = %+v, want operator 7 with the seeded counts", s0)
	}
	if got.Stats[1].OperatorID != 8 || got.Stats[1].OpenBeads != 0 {
		t.Errorf("Stats[1] = %+v, want all-zero operator 8", got.Stats[1])
	}
	if len(got.Recent) != 2 {
		t.Fatalf("Recent len = %d, want 2", len(got.Recent))
	}
	if got.Recent[0].BeadID != "gd-x1" || got.Recent[0].OperatorID != 7 {
		t.Errorf("Recent[0] = %+v, want gd-x1 / 7", got.Recent[0])
	}
	if got.Recent[1].BeadID != "gd-y1" || got.Recent[1].OperatorID != 8 {
		t.Errorf("Recent[1] = %+v, want gd-y1 / 8", got.Recent[1])
	}

	mock.ExpectCommit()
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// TestCovOpsStatsOperatorDirectoryStatsTxError surfaces a failure on the first
// (aggregate) query through the txStore wrapper without running the second.
func TestCovOpsStatsOperatorDirectoryStatsTxError(t *testing.T) {
	db, mock := newMockDB(t)
	ts, tx := testCovOpsStatsTxStore(t, db, mock)

	boom := errors.New("agg boom")
	mock.ExpectQuery(`GROUP BY operated_by_id`).WillReturnError(boom)

	if _, err := ts.OperatorDirectoryStats(context.Background(), 5, 5000, 168); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want agg boom", err)
	}

	mock.ExpectRollback()
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}
