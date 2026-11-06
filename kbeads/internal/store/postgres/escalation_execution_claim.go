package postgres

import (
	"context"
	"database/sql"
	"errors"
)

// ClaimEscalationExecution records the one executor of an approved
// escalation (P7 Task 12, spec D10) in fields.execution_dispatch. Like
// ClaimEscalationReplay, the guard (execution_dispatch unset or empty) and the
// write are one UPDATE, so two approves on two replicas cannot both win. The
// loser reads back the winning value so the caller can report it.
func queryClaimEscalationExecution(ctx context.Context, db executor, beadID, dispatch string) (bool, string, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE beads
		   SET fields = jsonb_set(COALESCE(fields, '{}'::jsonb), '{execution_dispatch}', to_jsonb($2::text), true),
		       updated_at = NOW()
		 WHERE id = $1
		   AND deleted_at IS NULL
		   AND COALESCE(fields ->> 'execution_dispatch', '') = ''`,
		beadID, dispatch,
	)
	if err != nil {
		return false, "", err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, "", err
	}
	if n == 1 {
		return true, dispatch, nil
	}
	var current sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT fields ->> 'execution_dispatch' FROM beads WHERE id = $1 AND deleted_at IS NULL`,
		beadID,
	).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return false, current.String, nil
}

func (s *PostgresStore) ClaimEscalationExecution(ctx context.Context, beadID, dispatch string) (bool, string, error) {
	return queryClaimEscalationExecution(ctx, s.db, beadID, dispatch)
}

func (s *txStore) ClaimEscalationExecution(ctx context.Context, beadID, dispatch string) (bool, string, error) {
	return queryClaimEscalationExecution(ctx, s.tx, beadID, dispatch)
}
