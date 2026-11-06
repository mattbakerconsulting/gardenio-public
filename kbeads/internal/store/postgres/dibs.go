package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// queryGetBeadOperator returns the canonical "kind:subject" string for the
// bead's operated_by_id (joined to operators). Returns ("", sql.ErrNoRows)
// when the bead does not exist or is soft-deleted; the dibs caller treats
// either as "no flip". Returns ("", nil) when the bead exists but has no
// operator yet (operated_by_id IS NULL — possible only on history-only
// tables; beads.operated_by_id is NOT NULL after migration 017).
func queryGetBeadOperator(ctx context.Context, db executor, beadID string) (string, error) {
	var op sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT o.kind || ':' || o.subject
		  FROM beads b
		  LEFT JOIN operators o ON o.id = b.operated_by_id
		 WHERE b.id = $1 AND b.deleted_at IS NULL`,
		beadID,
	).Scan(&op)
	if err != nil {
		return "", err
	}
	if !op.Valid {
		return "", nil
	}
	return op.String, nil
}

// queryUpdateBeadOperator flips beads.operated_by_id for the dibs rule.
// Returns the number of rows updated (0 when the bead is missing /
// soft-deleted, 1 on success). Touches updated_at so the row's audit
// timestamp keeps moving, except on a closed bead: there the flip follows the
// close itself (a human closing a machine-filed decision), and a closed bead
// whose updated_at is past closed_at reads as written after it closed. The
// Lands Gate refuses such a decision's answer, so a dibs flip, which changes
// attribution and not content, leaves a closed bead's updated_at alone.
func queryUpdateBeadOperator(ctx context.Context, db executor, beadID string, operatorID int64) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE beads
		   SET operated_by_id = $2,
		       updated_at = CASE WHEN status = 'closed' THEN updated_at ELSE NOW() END
		 WHERE id = $1 AND deleted_at IS NULL`,
		beadID, operatorID,
	)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return n, nil
}

// GetBeadOperator returns the canonical "kind:subject" string for the
// bead's current operator, or "" with no error if the bead is missing /
// soft-deleted (the dibs hot path treats either as a no-op).
func (s *PostgresStore) GetBeadOperator(ctx context.Context, beadID string) (string, error) {
	op, err := queryGetBeadOperator(ctx, s.db, beadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return op, err
}

// UpdateBeadOperator sets beads.operated_by_id for the dibs rule. Returns
// nil and no error when the bead is missing / soft-deleted (the dibs hot
// path treats either as a no-op).
func (s *PostgresStore) UpdateBeadOperator(ctx context.Context, beadID string, operatorID int64) error {
	_, err := queryUpdateBeadOperator(ctx, s.db, beadID, operatorID)
	return err
}

func (s *txStore) GetBeadOperator(ctx context.Context, beadID string) (string, error) {
	op, err := queryGetBeadOperator(ctx, s.tx, beadID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return op, err
}

func (s *txStore) UpdateBeadOperator(ctx context.Context, beadID string, operatorID int64) error {
	_, err := queryUpdateBeadOperator(ctx, s.tx, beadID, operatorID)
	return err
}
