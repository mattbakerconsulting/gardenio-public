package postgres

import (
	"context"
	"fmt"

	"github.com/lib/pq"

	"gardenio/kbeads/internal/store"
)

// PurgeBeads hard-deletes the beads req selects, in one transaction (Lands
// Track B spec §3.3, step 8.11: a closed land's data is deleted after the
// retention period). Unlike DeleteBead, which only stamps deleted_at, nothing
// of a purged bead stays: labels, deps, comments, embeddings, followers and
// taints go with the row (ON DELETE CASCADE), and the tables that key on a
// bead id without a foreign key (events, mutation_history, slow_tool_calls,
// bead_references targets) are cleared here. taint_audit keeps its rows: they
// hold ids and content hashes, never content, and are the security record.
func (s *PostgresStore) PurgeBeads(ctx context.Context, req store.PurgeRequest) ([]store.PurgedBead, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit
	beads, err := queryPurgeBeads(ctx, tx, req)
	if err != nil || req.DryRun {
		return beads, err
	}
	return beads, tx.Commit()
}

func (s *txStore) PurgeBeads(ctx context.Context, req store.PurgeRequest) ([]store.PurgedBead, error) {
	return queryPurgeBeads(ctx, s.tx, req)
}

// queryPurgeCandidates returns the beads req selects, soft-deleted ones
// included, sorted by id.
func queryPurgeCandidates(ctx context.Context, db executor, req store.PurgeRequest) ([]store.PurgedBead, error) {
	if (req.Label == "") == (len(req.IDs) == 0) {
		return nil, fmt.Errorf("purge: exactly one of label and ids is required")
	}
	keepTypes := req.KeepTypes
	if keepTypes == nil {
		keepTypes = []string{}
	}
	keepIDs := req.KeepIDs
	if keepIDs == nil {
		keepIDs = []string{}
	}
	var (
		q    string
		args []any
	)
	if req.Label != "" {
		q = `SELECT b.id, b.type, b.created_at FROM beads b JOIN labels l ON l.bead_id = b.id
			WHERE l.label = $1 AND NOT (b.type = ANY($2)) AND NOT (b.id = ANY($3))
			AND NOT ($4 AND EXISTS (SELECT 1 FROM labels o WHERE o.bead_id = b.id
				AND o.label LIKE 'project:%' AND o.label <> $1))
			ORDER BY b.id`
		args = []any{req.Label, pq.Array(keepTypes), pq.Array(keepIDs), req.KeepShared}
	} else {
		q = `SELECT b.id, b.type, b.created_at FROM beads b
			WHERE b.id = ANY($1) AND NOT (b.type = ANY($2)) AND NOT (b.id = ANY($3))
			ORDER BY b.id`
		args = []any{pq.Array(req.IDs), pq.Array(keepTypes), pq.Array(keepIDs)}
	}
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("purge: selecting beads: %w", err)
	}
	defer rows.Close()
	out := []store.PurgedBead{}
	for rows.Next() {
		var b store.PurgedBead
		if err := rows.Scan(&b.ID, &b.Type, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// queryPurgeBeads selects and, unless req.DryRun, deletes. The caller owns
// the transaction.
func queryPurgeBeads(ctx context.Context, db executor, req store.PurgeRequest) ([]store.PurgedBead, error) {
	beads, err := queryPurgeCandidates(ctx, db, req)
	if err != nil || req.DryRun || len(beads) == 0 {
		return beads, err
	}
	ids := make([]string, len(beads))
	for i, b := range beads {
		ids[i] = b.ID
	}
	arr := pq.Array(ids)
	for _, q := range []string{
		`DELETE FROM events WHERE bead_id = ANY($1)`,
		`DELETE FROM mutation_history WHERE bead_id = ANY($1)`,
		`DELETE FROM slow_tool_calls WHERE agent_bead_id = ANY($1)`,
		`DELETE FROM bead_references WHERE target_bead_id = ANY($1)`,
		`DELETE FROM beads WHERE id = ANY($1)`,
	} {
		if _, err := db.ExecContext(ctx, q, arr); err != nil {
			return nil, fmt.Errorf("purge: %s: %w", q, err)
		}
	}
	return beads, nil
}
