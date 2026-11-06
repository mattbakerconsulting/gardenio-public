package postgres

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryAllocateAgentHandleSeq_ReturnsSeq verifies the atomic upsert issues
// the ON CONFLICT DO UPDATE ... RETURNING statement and surfaces the allocated
// sequence (ka-0000002870).
func TestQueryAllocateAgentHandleSeq_ReturnsSeq(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(regexp.QuoteMeta(
		"INSERT INTO agent_handle_counters (gardener_id, next_seq)")).
		WithArgs("synthetic-gardener:unknown").
		WillReturnRows(sqlmock.NewRows([]string{"next_seq"}).AddRow(int64(5)))

	seq, err := queryAllocateAgentHandleSeq(context.Background(), db, "synthetic-gardener:unknown")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seq != 5 {
		t.Fatalf("seq = %d, want 5", seq)
	}
}

// TestQueryAllocateAgentHandleSeq_WrapsError confirms a DB failure is wrapped
// with the gardener_id for diagnosability rather than swallowed.
func TestQueryAllocateAgentHandleSeq_WrapsError(t *testing.T) {
	db, mock := newMockDB(t)

	sentinel := errors.New("boom")
	mock.ExpectQuery(regexp.QuoteMeta(
		"INSERT INTO agent_handle_counters (gardener_id, next_seq)")).
		WithArgs("gd-Gardener1").
		WillReturnError(sentinel)

	_, err := queryAllocateAgentHandleSeq(context.Background(), db, "gd-Gardener1")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("error does not wrap sentinel: %v", err)
	}
}
