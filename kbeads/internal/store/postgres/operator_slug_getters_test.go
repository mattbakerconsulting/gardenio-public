package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These cover the slug<->operator-id resolution getters used by the
// /u/{slug} and /me viewer routes.

func TestGetOperatorBySlug_Found(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT operator_id\s+FROM operator_slug_aliases\s+WHERE slug = \$1`).
		WithArgs("matt-baker").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(42)))

	id, err := store.GetOperatorBySlug(context.Background(), "matt-baker")
	if err != nil {
		t.Fatalf("GetOperatorBySlug: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}
}

func TestGetOperatorBySlug_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM operator_slug_aliases`).
		WithArgs("ghost").
		WillReturnError(sql.ErrNoRows)

	_, err := store.GetOperatorBySlug(context.Background(), "ghost")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestGetOperatorSlug_Found(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT slug FROM operators WHERE id = \$1`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("matt-baker"))

	slug, err := store.GetOperatorSlug(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetOperatorSlug: %v", err)
	}
	if slug != "matt-baker" {
		t.Errorf("slug = %q, want matt-baker", slug)
	}
}

func TestGetOperatorSlug_NullSlugTreatedAsNotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	// A NULL slug column maps to sql.ErrNoRows per the getter contract.
	mock.ExpectQuery(`SELECT slug FROM operators WHERE id = \$1`).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow(nil))

	_, err := store.GetOperatorSlug(context.Background(), 7)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows for NULL slug", err)
	}
}

func TestGetOperatorSlug_EmptySlugTreatedAsNotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT slug FROM operators WHERE id = \$1`).
		WithArgs(int64(8)).
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow(""))

	_, err := store.GetOperatorSlug(context.Background(), 8)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows for empty slug", err)
	}
}

func TestGetOperatorSlug_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT slug FROM operators`).
		WithArgs(int64(9)).
		WillReturnError(errors.New("boom"))

	if _, err := store.GetOperatorSlug(context.Background(), 9); err == nil {
		t.Fatal("GetOperatorSlug: err = nil, want non-nil for query error")
	}
}

func TestTxStore_OperatorSlugGetters(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`FROM operator_slug_aliases`).
		WithArgs("matt-baker").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(42)))
	mock.ExpectQuery(`SELECT slug FROM operators WHERE id = \$1`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"slug"}).AddRow("matt-baker"))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	id, err := ts.GetOperatorBySlug(context.Background(), "matt-baker")
	if err != nil || id != 42 {
		t.Fatalf("txStore.GetOperatorBySlug = (%d, %v), want (42, nil)", id, err)
	}
	slug, err := ts.GetOperatorSlug(context.Background(), 42)
	if err != nil || slug != "matt-baker" {
		t.Fatalf("txStore.GetOperatorSlug = (%q, %v), want (matt-baker, nil)", slug, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
