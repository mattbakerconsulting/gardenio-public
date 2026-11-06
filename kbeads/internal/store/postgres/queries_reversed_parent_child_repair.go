package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gardenio/kbeads/internal/store"
)

// queryRepairReversedParentChildDeps implements `kd admin
// repair-reversed-parent-child-deps` (ka-0000006880).
//
// It scans the deps table for parent-child rows stored in the REVERSED
// direction — bead_id resolves to a type=epic bead while depends_on_id
// resolves to a NON-epic bead. The canonical direction is
// bead_id = CHILD, depends_on_id = PARENT, which is what the
// ParentSubtreeID recursive CTE (queries.go) seeds from
// (`SELECT bead_id FROM deps WHERE depends_on_id = <root>`). A reversed
// row is invisible to that CTE, so subtree counts come back 0 and every
// gardener `bead_query` loop_end fires immediately.
//
// Repair (DryRun=false) flips each reversed row to canonical by deleting
// the reversed row and inserting the canonical one (preserving
// created_at / created_by / metadata / operated_by_id, recomputing fleet
// from the child bead). The canonical insert uses ON CONFLICT … DO
// NOTHING: if the canonical twin already exists, the reversed row is
// still removed but counted as SkippedCanonicalExists rather than
// EdgesFlipped. The whole operation is idempotent — a re-run finds
// nothing because no epic remains on the bead_id side.
//
// Per kbeads/CLAUDE.md §Migration policy the work is paged: each batch is
// its own transaction, bounded by BatchSize. Because every repaired row
// is removed from the candidate set, the scan re-runs from the start
// each batch (no keyset cursor needed) and terminates when a page comes
// back empty or MaxBatches is hit.
func queryRepairReversedParentChildDeps(
	ctx context.Context,
	db *sql.DB,
	opts store.RepairReversedParentChildDepsOptions,
) (*store.RepairReversedParentChildDepsStats, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	stats := &store.RepairReversedParentChildDepsStats{
		DryRun: opts.DryRun,
	}

	// Keyset cursor over (bead_id, depends_on_id) = (epic, child). For the
	// repair path the just-fixed rows fall out of the candidate set, but a
	// cursor is still correct and avoids re-scanning skipped rows. For the
	// dry-run path the cursor is REQUIRED — nothing is removed, so a
	// from-the-start re-scan would loop forever re-reporting the same rows.
	var afterEpic, afterChild string
	for {
		page, err := fetchReversedParentChildPage(ctx, db, afterEpic, afterChild, opts.BatchSize)
		if err != nil {
			return stats, fmt.Errorf("fetch reversed page: %w", err)
		}
		if len(page) == 0 {
			break
		}
		stats.ReversedRows += int64(len(page))
		stats.Edges = append(stats.Edges, page...)
		last := page[len(page)-1]
		afterEpic, afterChild = last.EpicID, last.ChildID

		if !opts.DryRun {
			if err := applyReversedParentChildPage(ctx, db, page, stats); err != nil {
				return stats, fmt.Errorf("apply reversed page: %w", err)
			}
		}

		stats.Batches++
		if opts.MaxBatches > 0 && stats.Batches >= opts.MaxBatches {
			if len(page) == opts.BatchSize {
				stats.BatchesCapped = true
			}
			break
		}
		if len(page) < opts.BatchSize {
			break
		}
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// fetchReversedParentChildPage returns up to limit reversed parent-child
// rows: deps of type='parent-child' whose bead_id bead is type=epic and
// whose depends_on_id bead exists and is NOT type=epic. epic→epic edges
// (legitimate sub-epics) and rows with a missing/soft-deleted endpoint
// are excluded. Soft-deleted beads on either side are ignored. Results
// are keyset-paginated by (bead_id, depends_on_id) strictly greater than
// (afterEpic, afterChild); pass empty strings for the first page.
func fetchReversedParentChildPage(
	ctx context.Context,
	db *sql.DB,
	afterEpic, afterChild string,
	limit int,
) ([]store.ReversedParentChildEdge, error) {
	const q = `
		SELECT d.bead_id, d.depends_on_id, child.type, COALESCE(d.created_by, '')
		  FROM deps d
		  JOIN beads epic
		    ON epic.id = d.bead_id
		   AND epic.deleted_at IS NULL
		   AND epic.type = 'epic'
		  JOIN beads child
		    ON child.id = d.depends_on_id
		   AND child.deleted_at IS NULL
		   AND child.type <> 'epic'
		 WHERE d.type = 'parent-child'
		   AND (d.bead_id, d.depends_on_id) > ($1, $2)
		 ORDER BY d.bead_id, d.depends_on_id
		 LIMIT $3`
	rows, err := db.QueryContext(ctx, q, afterEpic, afterChild, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ReversedParentChildEdge
	for rows.Next() {
		var e store.ReversedParentChildEdge
		if err := rows.Scan(&e.EpicID, &e.ChildID, &e.ChildType, &e.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// applyReversedParentChildPage flips each reversed row to canonical
// inside a single transaction. For each (epic, child): the canonical row
// (bead_id=child, depends_on_id=epic, type=parent-child) is inserted
// carrying the reversed row's created_at / created_by / metadata /
// operated_by_id and a fleet recomputed from the child bead; then the
// reversed row is deleted. The INSERT uses ON CONFLICT DO NOTHING — when
// the canonical twin already exists the insert is a no-op (counted as
// SkippedCanonicalExists), but the reversed row is removed regardless so
// the broken edge is gone.
func applyReversedParentChildPage(
	ctx context.Context,
	db *sql.DB,
	page []store.ReversedParentChildEdge,
	stats *store.RepairReversedParentChildDepsStats,
) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	for _, e := range page {
		// Insert canonical (child -> epic), carrying the reversed row's
		// attribution. Recompute fleet from the child bead. ON CONFLICT
		// DO NOTHING so an existing canonical twin is left intact.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO deps (bead_id, depends_on_id, type, created_at, created_by, metadata, fleet, operated_by_id)
			SELECT $1, $2, 'parent-child', d.created_at, d.created_by, d.metadata,
			       COALESCE((SELECT fleet FROM beads WHERE id = $1), d.fleet, 'default'),
			       d.operated_by_id
			  FROM deps d
			 WHERE d.bead_id = $3 AND d.depends_on_id = $4 AND d.type = 'parent-child'
			ON CONFLICT (bead_id, depends_on_id, type) DO NOTHING`,
			e.ChildID, e.EpicID, e.EpicID, e.ChildID,
		)
		if err != nil {
			return fmt.Errorf("insert canonical (%s -> %s): %w", e.ChildID, e.EpicID, err)
		}
		inserted, _ := res.RowsAffected()
		if inserted == 0 {
			stats.SkippedCanonicalExists++
		} else {
			stats.EdgesFlipped++
		}

		// Delete the reversed row.
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM deps
			 WHERE bead_id = $1 AND depends_on_id = $2 AND type = 'parent-child'`,
			e.EpicID, e.ChildID,
		); err != nil {
			return fmt.Errorf("delete reversed (%s -> %s): %w", e.EpicID, e.ChildID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
