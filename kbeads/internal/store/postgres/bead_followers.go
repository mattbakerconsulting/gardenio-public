package postgres

import (
	"context"
	"database/sql"

	"gardenio/kbeads/internal/model"
)

// AddFollower idempotently inserts (beadID, operatorID) into bead_followers,
// recording forwardedBy + note. Re-forwarding the same pair refreshes the
// row so the latest forwarder wins (and re-runs are safe). Migration 023.
//
// forwardedBy is *int64 because the column is NULLABLE — system inserts
// (e.g. typed-mention sync) may legitimately have no operator-of-record.
func (s *PostgresStore) AddFollower(ctx context.Context, beadID string, operatorID int64, forwardedBy *int64, note string) error {
	return addFollower(ctx, s.db, beadID, operatorID, forwardedBy, note)
}

func (s *txStore) AddFollower(ctx context.Context, beadID string, operatorID int64, forwardedBy *int64, note string) error {
	return addFollower(ctx, s.tx, beadID, operatorID, forwardedBy, note)
}

func addFollower(ctx context.Context, db execer, beadID string, operatorID int64, forwardedBy *int64, note string) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO bead_followers (bead_id, operator_id, forwarded_by, note)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (bead_id, operator_id)
		DO UPDATE SET forwarded_by = EXCLUDED.forwarded_by,
		              note         = EXCLUDED.note`,
		beadID, operatorID, forwardedBy, note)
	return err
}

// RemoveFollower deletes (beadID, operatorID) and returns true when a row
// was matched. A miss is not an error — the API is idempotent so callers
// can blindly issue an unfollow.
func (s *PostgresStore) RemoveFollower(ctx context.Context, beadID string, operatorID int64) (bool, error) {
	return removeFollower(ctx, s.db, beadID, operatorID)
}

func (s *txStore) RemoveFollower(ctx context.Context, beadID string, operatorID int64) (bool, error) {
	return removeFollower(ctx, s.tx, beadID, operatorID)
}

func removeFollower(ctx context.Context, db execer, beadID string, operatorID int64) (bool, error) {
	res, err := db.ExecContext(ctx, `
		DELETE FROM bead_followers WHERE bead_id = $1 AND operator_id = $2`,
		beadID, operatorID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// ListFollowers returns the followers of one bead joined with the parent
// operators row so callers get canonical (kind, subject, display) for both
// the follower and the forwarder without a second round trip. forwarded_by
// is LEFT JOINed because the column is NULLABLE.
//
// Order: created_at ASC so the UI renders followers in the order they were
// added (oldest at the top, like a chat thread).
func (s *PostgresStore) ListFollowers(ctx context.Context, beadID string) ([]*model.BeadFollower, error) {
	return listFollowers(ctx, s.db, beadID)
}

func (s *txStore) ListFollowers(ctx context.Context, beadID string) ([]*model.BeadFollower, error) {
	return listFollowers(ctx, s.tx, beadID)
}

func listFollowers(ctx context.Context, db execer, beadID string) ([]*model.BeadFollower, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT f.bead_id,
		       f.operator_id,
		       o.kind,
		       o.subject,
		       o.display,
		       f.forwarded_by,
		       COALESCE(fo.kind, ''),
		       COALESCE(fo.subject, ''),
		       COALESCE(fo.display, ''),
		       f.note,
		       f.created_at
		  FROM bead_followers f
		  JOIN operators o  ON o.id  = f.operator_id
		  LEFT JOIN operators fo ON fo.id = f.forwarded_by
		 WHERE f.bead_id = $1
		 ORDER BY f.created_at ASC, f.operator_id ASC`, beadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*model.BeadFollower
	for rows.Next() {
		var (
			r           model.BeadFollower
			forwardedBy sql.NullInt64
		)
		if err := rows.Scan(
			&r.BeadID,
			&r.OperatorID,
			&r.OperatorKind,
			&r.OperatorSubject,
			&r.OperatorDisplay,
			&forwardedBy,
			&r.ForwardedByKind,
			&r.ForwardedBySubject,
			&r.ForwardedByDisplay,
			&r.Note,
			&r.CreatedAt,
		); err != nil {
			return nil, err
		}
		if forwardedBy.Valid {
			id := forwardedBy.Int64
			r.ForwardedBy = &id
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// ListFollowedBeadIDs returns the bead IDs the given operator follows. The
// inbox-query UNION needs the right side as a flat ID list so callers can
// hydrate beads through the existing list path. Order: created_at DESC so a
// just-forwarded bead surfaces at the top of the inbox.
//
// limit ≤ 0 means no limit (caller-decided cap; the inbox handler passes
// inboxListLimit). Always pass a sane cap on hot paths — followers can grow
// unbounded over a long-lived operator's tenure.
func (s *PostgresStore) ListFollowedBeadIDs(ctx context.Context, operatorID int64, limit int) ([]string, error) {
	return listFollowedBeadIDs(ctx, s.db, operatorID, limit)
}

func (s *txStore) ListFollowedBeadIDs(ctx context.Context, operatorID int64, limit int) ([]string, error) {
	return listFollowedBeadIDs(ctx, s.tx, operatorID, limit)
}

func listFollowedBeadIDs(ctx context.Context, db execer, operatorID int64, limit int) ([]string, error) {
	q := `SELECT bead_id FROM bead_followers WHERE operator_id = $1 ORDER BY created_at DESC, bead_id ASC`
	args := []any{operatorID}
	if limit > 0 {
		q += ` LIMIT $2`
		args = append(args, limit)
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
