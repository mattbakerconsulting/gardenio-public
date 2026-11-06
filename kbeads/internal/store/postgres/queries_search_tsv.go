package postgres

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// searchTSVExpr is the SQL expression that computes a bead's search_tsv. It
// delegates to the IMMUTABLE database function beads_search_tsv_expr(title,
// description, fields) — the SINGLE source of truth introduced in migration
// 040_beads_search_tsv_fields.up.sql, which the maintenance trigger
// beads_search_tsv_update() also calls. Because both the trigger (new/updated
// rows) and this backfill (legacy / reindexed rows) invoke the same function,
// they produce identical tsvectors by construction — no more hand-kept sync
// between duplicated SQL (the 039 footgun this collapses). The vector weights
// title 'A' > description 'B' > fields->>'context' 'C' > option label/short
// 'D', matching the websearch_to_tsquery('english', …) regconfig used at query
// time (ka-0000003832 title/desc; ka-0000005839 context/options).
const searchTSVExpr = `beads_search_tsv_expr(title, description, fields)`

// queryBackfillSearchTSVPage fills or refreshes the search_tsv FTS column on
// one keyset-paginated page of beads (migration 039 / ka-0000003832; fields
// extension migration 040 / ka-0000005839). It mirrors
// queryReextractReferencesPage's keyset cursor (id > afterID ORDER BY id ASC
// LIMIT batchSize) so concurrent inserts during the backfill can't shift rows
// past the cursor.
//
// Two modes, selected by reindex:
//
//   - reindex == false (default legacy backfill): candidate rows are those
//     whose search_tsv IS NULL (legacy rows that pre-date migration 039). The
//     per-page UPDATE fills them and re-guards `search_tsv IS NULL` so a
//     concurrent trigger that already filled a row makes the UPDATE a no-op.
//     Remaining (final page) reports the count of rows still NULL.
//
//   - reindex == true (migration 040 fields extension): candidate rows are
//     those carrying a `context` or `options` field — exactly the set whose
//     040 vector (which now indexes those fields) differs from the
//     title+description-only vector 039 stored. These rows are already
//     NON-NULL, so the NULL-fill mode would never touch them; reindex mode
//     recomputes their vector. The UPDATE guards `search_tsv IS DISTINCT FROM
//     <expr>` so a re-run (or a row the trigger already refreshed) is a true
//     no-op. Remaining is not meaningful here and is left 0; Done (page <
//     batchSize) is the completion signal.
//
// In BOTH modes the page's rows are updated in a SINGLE autocommit UPDATE keyed
// by `id = ANY($1::text[])` (advice ka-0000000229 — never IN-list N params,
// which overflows lib/pq at 65535). One UPDATE per page = one transaction per
// page, honoring the no-eager-backfill policy (kbeads/CLAUDE.md): no single
// statement ever locks the whole table. The keyset cursor advances by the last
// CANDIDATE id (from the SELECT), so it advances even when the guarded UPDATE
// changes nothing.
func queryBackfillSearchTSVPage(ctx context.Context, db executor, afterID string, batchSize int, reindex bool) (*store.BackfillSearchTSVStats, error) {
	if batchSize <= 0 {
		batchSize = 500
	}

	// Candidate predicate differs by mode: NULL rows for the legacy fill,
	// context/options-bearing rows for the fields reindex. Keyset (id ASC)
	// rather than OFFSET so inserts during the sweep stay stable.
	candidatePredicate := `search_tsv IS NULL`
	if reindex {
		candidatePredicate = `(fields ? 'context' OR fields ? 'options')`
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id
		FROM beads
		WHERE `+candidatePredicate+` AND id > $1
		ORDER BY id ASC
		LIMIT $2`,
		afterID, batchSize,
	)
	if err != nil {
		return nil, fmt.Errorf("list beads for search_tsv backfill: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan bead id for search_tsv backfill: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate beads for search_tsv backfill: %w", err)
	}
	rows.Close()

	// The per-page UPDATE's own guard: NULL-fill only touches still-NULL rows;
	// reindex only rewrites rows whose stored vector actually differs from the
	// freshly-computed one (so re-runs and trigger-refreshed rows are no-ops).
	updateGuard := `search_tsv IS NULL`
	if reindex {
		updateGuard = `search_tsv IS DISTINCT FROM ` + searchTSVExpr
	}

	stats := &store.BackfillSearchTSVStats{}
	if len(ids) > 0 {
		res, err := db.ExecContext(ctx, `
			UPDATE beads
			SET search_tsv = `+searchTSVExpr+`
			WHERE id = ANY($1::text[]) AND `+updateGuard,
			pq.Array(ids),
		)
		if err != nil {
			return stats, fmt.Errorf("update search_tsv batch: %w", err)
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
	// Remaining only has a meaning in NULL-fill mode (rows still un-backfilled);
	// in reindex mode the candidate set is drained by the keyset cursor, so
	// Remaining stays 0 and Done is the completion signal.
	if stats.Done && !reindex {
		var remaining int64
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM beads WHERE search_tsv IS NULL`).Scan(&remaining); err != nil {
			return stats, fmt.Errorf("count remaining NULL search_tsv: %w", err)
		}
		stats.Remaining = remaining
	}
	return stats, nil
}
