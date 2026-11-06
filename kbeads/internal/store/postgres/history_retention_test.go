package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func TestDeleteHistoryBefore(t *testing.T) {
	cutoff := time.Now().UTC().Add(-14 * 24 * time.Hour)
	types := []string{"trigger", "agent"}

	t.Run("eventsScopedToUpdatedTopicAndTypes", func(t *testing.T) {
		db, mock := newMockDB(t)
		store := &PostgresStore{db: db}
		mock.ExpectExec(`DELETE FROM events\s+WHERE id IN \(\s+SELECT e.id FROM events e\s+JOIN beads b ON b.id = e.bead_id\s+WHERE e.topic = 'beads.bead.updated'\s+AND e.created_at < \$1\s+AND b.type = ANY\(\$2\)\s+LIMIT \$3`).
			WithArgs(cutoff, pq.Array(types), 100).
			WillReturnResult(sqlmock.NewResult(0, 100))
		n, err := store.DeleteBeadUpdatedEventsBefore(context.Background(), types, cutoff, 100)
		if err != nil || n != 100 {
			t.Fatalf("DeleteBeadUpdatedEventsBefore = %d, %v; want 100, nil", n, err)
		}
	})

	t.Run("mutationsScopedToFieldsAndTypes", func(t *testing.T) {
		db, mock := newMockDB(t)
		store := &PostgresStore{db: db}
		mock.ExpectExec(`DELETE FROM mutation_history\s+WHERE id IN \(\s+SELECT mh.id FROM mutation_history mh\s+JOIN beads b ON b.id = mh.bead_id\s+WHERE mh.field = 'fields'\s+AND mh.created_at < \$1\s+AND b.type = ANY\(\$2\)\s+LIMIT \$3`).
			WithArgs(cutoff, pq.Array(types), historyRetentionDefaultBatch).
			WillReturnResult(sqlmock.NewResult(0, 3))
		n, err := store.DeleteFieldsMutationsBefore(context.Background(), types, cutoff, 0)
		if err != nil || n != 3 {
			t.Fatalf("DeleteFieldsMutationsBefore = %d, %v; want 3, nil", n, err)
		}
	})

	t.Run("noTypesIsNoOp", func(t *testing.T) {
		db, _ := newMockDB(t)
		store := &PostgresStore{db: db}
		if n, err := store.DeleteFieldsMutationsBefore(context.Background(), nil, cutoff, 10); n != 0 || err != nil {
			t.Errorf("no types = %d, %v; want 0, nil and no query", n, err)
		}
	})
}
