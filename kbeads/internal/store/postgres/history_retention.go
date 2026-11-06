package postgres

import (
	"context"
	"time"

	"github.com/lib/pq"
)

// historyRetentionDefaultBatch bounds one history-retention DELETE when the
// caller passes limit <= 0. A trigger's history rows held its whole fields blob
// (avg ~19 KB, up to 190 KB in events), so batches stay small.
const historyRetentionDefaultBatch = 500

func (s *PostgresStore) DeleteBeadUpdatedEventsBefore(ctx context.Context, types []string, cutoff time.Time, limit int) (int64, error) {
	return queryDeleteBeadUpdatedEventsBefore(ctx, s.db, types, cutoff, limit)
}

func (s *txStore) DeleteBeadUpdatedEventsBefore(ctx context.Context, types []string, cutoff time.Time, limit int) (int64, error) {
	return queryDeleteBeadUpdatedEventsBefore(ctx, s.tx, types, cutoff, limit)
}

func (s *PostgresStore) DeleteFieldsMutationsBefore(ctx context.Context, types []string, cutoff time.Time, limit int) (int64, error) {
	return queryDeleteFieldsMutationsBefore(ctx, s.db, types, cutoff, limit)
}

func (s *txStore) DeleteFieldsMutationsBefore(ctx context.Context, types []string, cutoff time.Time, limit int) (int64, error) {
	return queryDeleteFieldsMutationsBefore(ctx, s.tx, types, cutoff, limit)
}

// queryDeleteBeadUpdatedEventsBefore deletes up to limit beads.bead.updated
// events older than cutoff on beads of the given types. No other topic is
// touched: create, close, comment, label and dependency events stay.
func queryDeleteBeadUpdatedEventsBefore(ctx context.Context, db executor, types []string, cutoff time.Time, limit int) (int64, error) {
	if len(types) == 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = historyRetentionDefaultBatch
	}
	res, err := db.ExecContext(ctx, `
		DELETE FROM events
		WHERE id IN (
			SELECT e.id FROM events e
			JOIN beads b ON b.id = e.bead_id
			WHERE e.topic = 'beads.bead.updated'
			  AND e.created_at < $1
			  AND b.type = ANY($2)
			LIMIT $3
		)`, cutoff, pq.Array(types), limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// queryDeleteFieldsMutationsBefore deletes up to limit field='fields'
// mutation_history rows older than cutoff on beads of the given types. Rows for
// every other field (status, assignee, owner, the create and close rows, the
// per-key "fields.<key>" rows) stay.
func queryDeleteFieldsMutationsBefore(ctx context.Context, db executor, types []string, cutoff time.Time, limit int) (int64, error) {
	if len(types) == 0 {
		return 0, nil
	}
	if limit <= 0 {
		limit = historyRetentionDefaultBatch
	}
	res, err := db.ExecContext(ctx, `
		DELETE FROM mutation_history
		WHERE id IN (
			SELECT mh.id FROM mutation_history mh
			JOIN beads b ON b.id = mh.bead_id
			WHERE mh.field = 'fields'
			  AND mh.created_at < $1
			  AND b.type = ANY($2)
			LIMIT $3
		)`, cutoff, pq.Array(types), limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
