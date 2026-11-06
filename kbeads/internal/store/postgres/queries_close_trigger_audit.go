package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// queryCloseTriggerAuditBeads implements `kd admin
// close-trigger-audit-beads` (ka-0000006341). Closes legacy type=event
// beads labeled `auto:trigger-audit` whose status is still open. The
// forward-fix in ka-0000004100 (recordTriggerAudit now creates audit
// beads with status=closed) does not address the ~226k legacy rows from
// before the fix; this one-shot sweep does.
//
// Per kbeads/CLAUDE.md §Migration policy — no eager full-table updates.
// Each batch is a keyset-paginated page over candidate ids, applied in
// its own transaction. WHERE status != 'closed' on the UPDATE makes
// re-runs on already-closed rows a no-op.
//
// We do not record per-row mutation_history entries — these are
// append-only ledger rows with no subscribers, and emitting 226k
// audit-of-audit rows would defeat the purpose.
func queryCloseTriggerAuditBeads(
	ctx context.Context,
	db *sql.DB,
	opts store.CloseTriggerAuditBeadsOptions,
) (*store.CloseTriggerAuditBeadsStats, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	stats := &store.CloseTriggerAuditBeadsStats{DryRun: opts.DryRun}

	lastID := ""
	for {
		ids, err := fetchTriggerAuditPage(ctx, db, lastID, opts.BatchSize)
		if err != nil {
			return stats, fmt.Errorf("fetch page (after %q): %w", lastID, err)
		}
		if len(ids) == 0 {
			break
		}
		lastID = ids[len(ids)-1]
		stats.LastID = lastID
		stats.CandidateBeads += int64(len(ids))

		if !opts.DryRun {
			n, err := closeTriggerAuditBatch(ctx, db, ids)
			if err != nil {
				return stats, fmt.Errorf("close batch (after %q): %w", lastID, err)
			}
			stats.BeadsClosed += n
		} else {
			stats.BeadsClosed += int64(len(ids))
		}

		stats.Batches++
		if opts.MaxBatches > 0 && stats.Batches >= opts.MaxBatches {
			if len(ids) == opts.BatchSize {
				stats.BatchesCapped = true
			}
			break
		}
		if len(ids) < opts.BatchSize {
			break
		}
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// fetchTriggerAuditPage returns one keyset-paginated page of candidate
// bead ids: type=event, label=auto:trigger-audit, status != closed,
// not soft-deleted, id > afterID. The EXISTS clause uses the
// idx_labels_label index for cheap label filtering.
func fetchTriggerAuditPage(
	ctx context.Context,
	db *sql.DB,
	afterID string,
	limit int,
) ([]string, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT b.id
		  FROM beads b
		 WHERE b.type = 'event'
		   AND b.status <> 'closed'
		   AND b.deleted_at IS NULL
		   AND b.id > $1
		   AND EXISTS (
		       SELECT 1 FROM labels l
		        WHERE l.bead_id = b.id
		          AND l.label = 'auto:trigger-audit'
		   )
		 ORDER BY b.id
		 LIMIT $2`,
		afterID, limit,
	)
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

// closeTriggerAuditBatch flips status to closed on every id in the page
// in a single transaction. closed_at/updated_at carry the run timestamp
// and closed_by records the admin command for the audit trail. The
// WHERE status != 'closed' guard makes a re-run a no-op.
func closeTriggerAuditBatch(
	ctx context.Context,
	db *sql.DB,
	ids []string,
) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on failure path; no-op if Commit() succeeded.

	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
		UPDATE beads
		   SET status = 'closed',
		       closed_at = $1,
		       closed_by = $2,
		       updated_at = $1
		 WHERE id = ANY($3::text[])
		   AND status <> 'closed'`,
		now,
		"kd admin close-trigger-audit-beads",
		pq.Array(ids),
	)
	if err != nil {
		return 0, fmt.Errorf("update beads: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return n, nil
}
