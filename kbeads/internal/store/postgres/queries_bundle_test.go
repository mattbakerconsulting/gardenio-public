package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestHasBundleChild_True(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("gd-parent-1", sqlmock.AnyArg(), "bundle").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	got, err := store.HasBundleChild(context.Background(), "gd-parent-1")
	if err != nil {
		t.Fatalf("HasBundleChild: %v", err)
	}
	if !got {
		t.Errorf("expected has_bundle=true, got false")
	}
}

func TestHasBundleChild_False(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("gd-parent-2", sqlmock.AnyArg(), "bundle").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	got, err := store.HasBundleChild(context.Background(), "gd-parent-2")
	if err != nil {
		t.Fatalf("HasBundleChild: %v", err)
	}
	if got {
		t.Errorf("expected has_bundle=false, got true")
	}
}

func TestHasBundleChild_Error(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT EXISTS").
		WithArgs("gd-parent-3", sqlmock.AnyArg(), "bundle").
		WillReturnError(sql.ErrConnDone)

	_, err := store.HasBundleChild(context.Background(), "gd-parent-3")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}
