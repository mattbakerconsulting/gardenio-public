package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestGetOperatorHistory pins the bead_operator_history VIEW projection +
// ordering and the row scan into model.OperatorHistoryRow.
func TestGetOperatorHistory(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"id", "bead_id", "operator_id", "became_owner_at", "prev_operator", "actor", "source_kind",
	}).
		AddRow(int64(2), "gd-1", "oidc:b@x.ai", now, "oidc:a@x.ai", "agent-x", "dibs-flip").
		AddRow(int64(1), "gd-1", "oidc:a@x.ai", now.Add(-time.Hour), "", "creator", "create")

	mock.ExpectQuery(`FROM bead_operator_history\s+WHERE bead_id = \$1\s+ORDER BY became_owner_at DESC, id DESC`).
		WithArgs("gd-1").
		WillReturnRows(rows)

	out, err := store.GetOperatorHistory(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("GetOperatorHistory: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("rows = %d, want 2", len(out))
	}
	if out[0].SourceKind != "dibs-flip" || out[0].OperatorID != "oidc:b@x.ai" {
		t.Errorf("row0 = %+v, want dibs-flip/oidc:b@x.ai", out[0])
	}
	if out[1].SourceKind != "create" || out[1].PrevOperator != "" {
		t.Errorf("row1 = %+v, want create/empty-prev", out[1])
	}
}

func TestGetOperatorHistory_Empty(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM bead_operator_history`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "bead_id", "operator_id", "became_owner_at", "prev_operator", "actor", "source_kind",
		}))

	out, err := store.GetOperatorHistory(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("GetOperatorHistory: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("rows = %d, want 0", len(out))
	}
}

func TestGetOperatorHistory_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM bead_operator_history`).
		WithArgs("gd-1").
		WillReturnError(errors.New("boom"))

	if _, err := store.GetOperatorHistory(context.Background(), "gd-1"); err == nil {
		t.Fatal("GetOperatorHistory: err = nil, want non-nil for query error")
	}
}

func TestTxStore_GetOperatorHistory(t *testing.T) {
	db, mock := newMockDB(t)

	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(`FROM bead_operator_history`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "bead_id", "operator_id", "became_owner_at", "prev_operator", "actor", "source_kind",
		}).AddRow(int64(1), "gd-1", "oidc:a@x.ai", now, "", "creator", "create"))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}
	out, err := ts.GetOperatorHistory(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("txStore.GetOperatorHistory: %v", err)
	}
	if len(out) != 1 {
		t.Errorf("rows = %d, want 1", len(out))
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
