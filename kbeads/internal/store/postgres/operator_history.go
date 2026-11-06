package postgres

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// GetOperatorHistory returns rows from the bead_operator_history VIEW
// for one bead, newest first. Migration 014 defines the VIEW as a
// projection over mutation_history filtered to field='operated_by'; see
// kbeads/internal/store/postgres/migrations/014_operated_by_dibs.up.sql
// and gardenio/docs/research/operated-by-transitivity-r4-audit.md §4 / §7.
func (s *PostgresStore) GetOperatorHistory(ctx context.Context, beadID string) ([]*model.OperatorHistoryRow, error) {
	return queryOperatorHistory(ctx, s.db, beadID)
}

func (s *txStore) GetOperatorHistory(ctx context.Context, beadID string) ([]*model.OperatorHistoryRow, error) {
	return queryOperatorHistory(ctx, s.tx, beadID)
}

func queryOperatorHistory(ctx context.Context, db execer, beadID string) ([]*model.OperatorHistoryRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, bead_id, operator_id, became_owner_at, prev_operator, actor, source_kind
		FROM bead_operator_history
		WHERE bead_id = $1
		ORDER BY became_owner_at DESC, id DESC`, beadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.OperatorHistoryRow
	for rows.Next() {
		r := &model.OperatorHistoryRow{}
		if err := rows.Scan(&r.ID, &r.BeadID, &r.OperatorID, &r.BecameOwnerAt, &r.PrevOperator, &r.Actor, &r.SourceKind); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
