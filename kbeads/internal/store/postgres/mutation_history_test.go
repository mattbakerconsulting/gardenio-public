package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestRecordMutation(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec("INSERT INTO mutation_history").
		WithArgs("gd-a", "alice", "title", "old", "new", "update", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	mut := &model.MutationRow{
		BeadID: "gd-a", Actor: "alice", Field: "title",
		OldValue: "old", NewValue: "new", MutationType: model.MutationTypeUpdate,
	}
	if err := store.RecordMutation(context.Background(), mut); err != nil {
		t.Fatalf("RecordMutation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestListMutations(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "bead_id", "actor", "field", "old_value", "new_value", "mutation_type", "created_at"}).
		AddRow(int64(1), "gd-a", "alice", "*", "", "<created>", "create", now).
		AddRow(int64(2), "gd-a", "alice", "title", "x", "y", "update", now.Add(time.Second))

	mock.ExpectQuery("SELECT id, bead_id, actor, field, old_value, new_value, mutation_type, created_at FROM mutation_history").
		WithArgs("gd-a").
		WillReturnRows(rows)

	got, err := store.ListMutations(context.Background(), "gd-a")
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[0].MutationType != "create" || got[0].Field != "*" {
		t.Errorf("row 0 = %+v", got[0])
	}
	if got[1].MutationType != "update" || got[1].Field != "title" || got[1].OldValue != "x" || got[1].NewValue != "y" {
		t.Errorf("row 1 = %+v", got[1])
	}
}

func TestGetMutation(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "bead_id", "actor", "field", "old_value", "new_value", "mutation_type", "created_at"}).
		AddRow(int64(7), "gd-z", "alice", "title", "old", "new", "update", now)

	mock.ExpectQuery("SELECT id, bead_id, actor, field, old_value, new_value, mutation_type, created_at\\s+FROM mutation_history\\s+WHERE id = \\$1").
		WithArgs(int64(7)).
		WillReturnRows(rows)

	got, err := store.GetMutation(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetMutation: %v", err)
	}
	if got.ID != 7 || got.BeadID != "gd-z" || got.Field != "title" || got.OldValue != "old" || got.NewValue != "new" {
		t.Errorf("row = %+v", got)
	}
}

func TestGetMutation_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT id, bead_id, actor, field, old_value, new_value, mutation_type, created_at\\s+FROM mutation_history\\s+WHERE id = \\$1").
		WithArgs(int64(404)).
		WillReturnError(sql.ErrNoRows)

	_, err := store.GetMutation(context.Background(), 404)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err=%v, want sql.ErrNoRows", err)
	}
}
