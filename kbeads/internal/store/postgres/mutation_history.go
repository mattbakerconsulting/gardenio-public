package postgres

import (
	"context"
	"database/sql"

	"gardenio/kbeads/internal/model"
)

// RecordMutation appends one row to the mutation_history table.
func (s *PostgresStore) RecordMutation(ctx context.Context, mut *model.MutationRow) error {
	return insertMutation(ctx, s.db, mut)
}

// ListMutations returns mutation_history rows for a bead, oldest first.
func (s *PostgresStore) ListMutations(ctx context.Context, beadID string) ([]*model.MutationRow, error) {
	return queryMutations(ctx, s.db, beadID)
}

func (s *txStore) RecordMutation(ctx context.Context, mut *model.MutationRow) error {
	return insertMutation(ctx, s.tx, mut)
}

func (s *txStore) ListMutations(ctx context.Context, beadID string) ([]*model.MutationRow, error) {
	return queryMutations(ctx, s.tx, beadID)
}

// GetMutation returns one mutation_history row by primary key.
func (s *PostgresStore) GetMutation(ctx context.Context, id int64) (*model.MutationRow, error) {
	return getMutation(ctx, s.db, id)
}

func (s *txStore) GetMutation(ctx context.Context, id int64) (*model.MutationRow, error) {
	return getMutation(ctx, s.tx, id)
}

func getMutation(ctx context.Context, db execer, id int64) (*model.MutationRow, error) {
	row := db.QueryRowContext(ctx, `
		SELECT id, bead_id, actor, field, old_value, new_value, mutation_type, created_at
		FROM mutation_history
		WHERE id = $1`, id)
	m := &model.MutationRow{}
	if err := row.Scan(&m.ID, &m.BeadID, &m.Actor, &m.Field, &m.OldValue, &m.NewValue, &m.MutationType, &m.CreatedAt); err != nil {
		return nil, err
	}
	return m, nil
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func insertMutation(ctx context.Context, db execer, mut *model.MutationRow) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type, operated_by_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		mut.BeadID, mut.Actor, mut.Field, mut.OldValue, mut.NewValue, mut.MutationType, nullInt64Ptr(mut.OperatedByID),
	)
	return err
}

func queryMutations(ctx context.Context, db execer, beadID string) ([]*model.MutationRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, bead_id, actor, field, old_value, new_value, mutation_type, created_at
		FROM mutation_history
		WHERE bead_id = $1
		ORDER BY created_at ASC, id ASC`, beadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*model.MutationRow
	for rows.Next() {
		m := &model.MutationRow{}
		if err := rows.Scan(&m.ID, &m.BeadID, &m.Actor, &m.Field, &m.OldValue, &m.NewValue, &m.MutationType, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
