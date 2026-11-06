package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestDeleteSlowToolCallsBefore(t *testing.T) {
	cutoff := time.Now().UTC().Add(-60 * 24 * time.Hour)

	t.Run("deletesOldestBatch", func(t *testing.T) {
		db, mock := newMockDB(t)
		store := &PostgresStore{db: db}
		mock.ExpectExec(`DELETE FROM slow_tool_calls\s+WHERE id IN \(\s+SELECT id FROM slow_tool_calls\s+WHERE ts < \$1\s+ORDER BY ts\s+LIMIT \$2`).
			WithArgs(cutoff, 250).
			WillReturnResult(sqlmock.NewResult(0, 250))
		n, err := store.DeleteSlowToolCallsBefore(context.Background(), cutoff, 250)
		if err != nil {
			t.Fatalf("DeleteSlowToolCallsBefore: %v", err)
		}
		if n != 250 {
			t.Errorf("deleted = %d, want 250", n)
		}
	})

	t.Run("nonPositiveLimitUsesDefault", func(t *testing.T) {
		db, mock := newMockDB(t)
		store := &PostgresStore{db: db}
		mock.ExpectExec(`DELETE FROM slow_tool_calls`).
			WithArgs(cutoff, slowToolCallsDeleteDefaultBatch).
			WillReturnResult(sqlmock.NewResult(0, 3))
		if _, err := store.DeleteSlowToolCallsBefore(context.Background(), cutoff, 0); err != nil {
			t.Fatalf("DeleteSlowToolCallsBefore: %v", err)
		}
	})

	t.Run("propagatesError", func(t *testing.T) {
		db, mock := newMockDB(t)
		store := &PostgresStore{db: db}
		boom := errors.New("boom")
		mock.ExpectExec(`DELETE FROM slow_tool_calls`).WillReturnError(boom)
		if _, err := store.DeleteSlowToolCallsBefore(context.Background(), cutoff, 10); !errors.Is(err, boom) {
			t.Errorf("err = %v, want %v", err, boom)
		}
	})
}
