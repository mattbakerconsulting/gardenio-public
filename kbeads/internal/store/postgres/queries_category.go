package postgres

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// queryBackfillCategoryPage re-stamps the beads.category materialized column on
// one keyset-paginated page of beads whose stored category disagrees with the
// current derive_bead_category(id, type) rule table (migration 043 /
// ka-0000007274). It mirrors queryBackfillSearchTSVPage's keyset cursor (id >
// afterID ORDER BY id ASC LIMIT batchSize) so concurrent inserts during the
// sweep can't shift rows past the cursor.
//
// The candidate set is every row whose stored category is stale —
// `category IS DISTINCT FROM derive_bead_category(id, type)`. In practice this
// is dominated by the deliverable/artifact rows written between migration 020
// and 043 that carry a stale `intermediate` (the migration-043 rule change is
// the only reason a previously-stamped row can now disagree), plus any legacy
// row whose category was NULL. New and updated rows are kept current by the
// migration-020 BEFORE INSERT/UPDATE trigger, so those never appear here.
//
// The page's rows are updated in a SINGLE autocommit UPDATE keyed by
// `id = ANY($1::text[])` (advice ka-0000000229 — never IN-list N params, which
// overflows lib/pq at 65535). One UPDATE per page = one transaction per page,
// honoring the no-eager-backfill policy (kbeads/CLAUDE.md): no single statement
// ever locks the whole table. The keyset cursor advances by the last CANDIDATE
// id (from the SELECT), so it advances even when the guarded UPDATE changes
// nothing (e.g. a row the trigger refreshed between the SELECT and the UPDATE).
//
// The UPDATE re-guards the same staleness predicate so a concurrent trigger
// write that already corrected a row makes that row's UPDATE a no-op. The SET
// re-invokes derive_bead_category rather than trusting the SELECT-time value,
// so the write reflects the row's state at UPDATE time.
func queryBackfillCategoryPage(ctx context.Context, db executor, afterID string, batchSize int) (*store.BackfillCategoryStats, error) {
	if batchSize <= 0 {
		batchSize = 500
	}

	// Keyset (id ASC) rather than OFFSET so inserts during the sweep stay
	// stable. The candidate predicate is the staleness check itself.
	rows, err := db.QueryContext(ctx, `
		SELECT id
		FROM beads
		WHERE category IS DISTINCT FROM derive_bead_category(id, type)
		  AND id > $1
		ORDER BY id ASC
		LIMIT $2`,
		afterID, batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("list beads for category backfill: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan bead id for category backfill: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate beads for category backfill: %w", err)
	}
	rows.Close()

	stats := &store.BackfillCategoryStats{}
	if len(ids) > 0 {
		res, err := db.ExecContext(ctx, `
			UPDATE beads
			SET category = derive_bead_category(id, type)
			WHERE id = ANY($1::text[])
			  AND category IS DISTINCT FROM derive_bead_category(id, type)`,
			pq.Array(ids),
		)
		if err != nil {
			return stats, fmt.Errorf("update category batch: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil {
			stats.BeadsScanned = int(n)
		} else {
			// Driver doesn't report RowsAffected — fall back to page size.
			stats.BeadsScanned = len(ids)
		}
		// Cursor advances by the last CANDIDATE id, not the last updated id,
		// so a guarded no-op UPDATE still moves the sweep forward.
		stats.LastID = ids[len(ids)-1]
	}

	stats.Done = len(ids) < batchSize
	return stats, nil
}
