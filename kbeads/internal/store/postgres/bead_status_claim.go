package postgres

import (
	"context"

	"gardenio/kbeads/internal/model"
)

// queryCASBeadStatus performs the guarded status-only UPDATE backing
// store.CASBeadStatus (see that interface doc for the fields-clobber bug it
// fixes). Returns changed=true only when this call's UPDATE matched a row,
// i.e. the bead existed, was not soft-deleted, and its status was exactly
// `from`.
func queryCASBeadStatus(ctx context.Context, db executor, beadID string, from, to model.Status) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE beads
		   SET status = $2,
		       updated_at = NOW()
		 WHERE id = $1
		   AND deleted_at IS NULL
		   AND status = $3`,
		beadID, string(to), string(from),
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

func (s *PostgresStore) CASBeadStatus(ctx context.Context, id string, from, to model.Status) (bool, error) {
	return queryCASBeadStatus(ctx, s.db, id, from, to)
}

func (s *txStore) CASBeadStatus(ctx context.Context, id string, from, to model.Status) (bool, error) {
	return queryCASBeadStatus(ctx, s.tx, id, from, to)
}
