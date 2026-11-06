package postgres

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"gardenio/kbeads/internal/store"
)

func TestPurgeBeads_ByLabelDeletesEverythingKeyedOnThem(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}

	mock.ExpectBegin()
	born := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT b.id, b.type, b.created_at FROM beads b JOIN labels l ON l.bead_id = b.id`)).
		WithArgs("project:acme", sqlmock.AnyArg(), sqlmock.AnyArg(), true).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "created_at"}).AddRow("a-1", "agent", born).AddRow("t-1", "task", born))
	for _, q := range []string{
		`DELETE FROM events WHERE bead_id = ANY($1)`,
		`DELETE FROM mutation_history WHERE bead_id = ANY($1)`,
		`DELETE FROM slow_tool_calls WHERE agent_bead_id = ANY($1)`,
		`DELETE FROM bead_references WHERE target_bead_id = ANY($1)`,
		`DELETE FROM beads WHERE id = ANY($1)`,
	} {
		mock.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 2))
	}
	mock.ExpectCommit()

	got, err := s.PurgeBeads(context.Background(), store.PurgeRequest{
		Label: "project:acme", KeepTypes: []string{"credit_grant"}, KeepIDs: []string{"p-acme"}, KeepShared: true,
	})
	if err != nil {
		t.Fatalf("PurgeBeads: %v", err)
	}
	if len(got) != 2 || got[0] != (store.PurgedBead{ID: "a-1", Type: "agent", CreatedAt: born}) {
		t.Fatalf("got %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeBeads_DryRunDeletesNothing(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT b.id, b.type, b.created_at FROM beads b`)).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "type", "created_at"}).AddRow("pa-1", "plot_application", time.Now()))
	mock.ExpectRollback()

	got, err := s.PurgeBeads(context.Background(), store.PurgeRequest{IDs: []string{"pa-1"}, DryRun: true})
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPurgeBeads_NeedsExactlyOneSelector(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}
	for _, req := range []store.PurgeRequest{{}, {Label: "project:a", IDs: []string{"x"}}} {
		mock.ExpectBegin()
		mock.ExpectRollback()
		if _, err := s.PurgeBeads(context.Background(), req); err == nil {
			t.Fatalf("%+v: want an error", req)
		}
	}
}
