package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// queryBackfillEmptyOwner implements `kd admin backfill-empty-owner`
// (ka-0000007029). See gardenio/docs/research/ownership-defaults.md
// §Recommended backfill sequence.
//
// Fills the `owner` text column on beads where it is still empty (`”` or
// NULL) after the ka-0000000269 server-side default landed. Three
// sequential phases, each a chunked `UPDATE … LIMIT BatchSize` loop that
// runs until zero rows change:
//
//	Phase 1 — owner ← the resolved operator identity (operators.kind ||
//	          ':' || operators.subject via beads.operated_by_id), excluding
//	          the legacy-pre-attribution sentinel.
//	Phase 2 — owner ← created_by (legacy-sentinel rows with a usable
//	          created_by; excludes pure system/bridge created_by values).
//	Phase 3 — owner ← UnownedSentinel for the residual.
//
// SCHEMA NOTE (migration 024): the legacy `beads.operated_by` TEXT column
// was dropped in the operator-identity-v2 cutover; operator identity now
// lives in the `operators` table, reached via the NOT-NULL
// `beads.operated_by_id` FK (migration 022). Phase 1 resolves the operator
// through the same LEFT/INNER JOIN the read path uses (queries.go
// beadOperatorSelectSuffix), so the backfilled owner matches exactly the
// `operated_by` alias beads-viewer renders. Rows with no real operator were
// sentinel-bound to system:legacy-pre-attribution by migration 022 Pass 3,
// so they are excluded here and fall through to Phase 2's created_by.
// `created_by` remains a live TEXT column (migration 001).
//
// Per kbeads/CLAUDE.md §Migration policy — no eager full-table updates.
// Each batch is its own transaction; every UPDATE writes one
// mutation_history row per affected bead with field='owner',
// actor='kd admin backfill-empty-owner', mutation_type='backfill-empty-owner-phaseN'.
// Idempotent via the `COALESCE(owner,”) = ”` filter — a re-run on a
// drained set is a no-op.
func queryBackfillEmptyOwner(ctx context.Context, db *sql.DB, opts store.BackfillEmptyOwnerOptions) (*store.BackfillEmptyOwnerStats, error) {
	start := time.Now()
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	stats := &store.BackfillEmptyOwnerStats{DryRun: opts.DryRun}

	before, err := countEmptyOwner(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("count before: %w", err)
	}
	stats.BeforeEmpty = before

	if opts.DryRun {
		if err := dryRunEmptyOwner(ctx, db, opts, stats); err != nil {
			return stats, err
		}
		stats.DurationMs = time.Since(start).Milliseconds()
		return stats, nil
	}

	// Phase 1: owner ← resolved operator identity (operators join on
	// operated_by_id), excluding the legacy-pre-attribution sentinel.
	p1 := emptyOwnerPhase{
		mutationType: "backfill-empty-owner-phase1",
		joinExtra:    `JOIN operators op ON op.id = b.operated_by_id`,
		ownerExpr:    `op.kind || ':' || op.subject`,
		whereExtra:   `AND op.kind || ':' || op.subject <> $1`,
		args:         []any{opts.LegacyOperatedBySentinel},
	}
	n1, batches1, capped1, err := runEmptyOwnerPhase(ctx, db, opts, p1)
	if err != nil {
		return stats, fmt.Errorf("phase 1: %w", err)
	}
	stats.Phase1Filled = n1
	stats.Phase1Batches = batches1

	// Phase 2: owner ← created_by (legacy-sentinel rows with a usable
	// created_by).
	p2 := emptyOwnerPhase{
		mutationType: "backfill-empty-owner-phase2",
		ownerExpr:    `b.created_by`,
		whereExtra:   `AND COALESCE(b.created_by,'') <> '' AND b.created_by <> ALL($1::text[])`,
		args:         []any{pq.Array(opts.Phase2Exclusions)},
	}
	n2, batches2, capped2, err := runEmptyOwnerPhase(ctx, db, opts, p2)
	if err != nil {
		return stats, fmt.Errorf("phase 2: %w", err)
	}
	stats.Phase2Filled = n2
	stats.Phase2Batches = batches2

	// Phase 3: owner ← UnownedSentinel for the residual.
	p3 := emptyOwnerPhase{
		mutationType: "backfill-empty-owner-phase3",
		ownerExpr:    `$1`,
		args:         []any{opts.UnownedSentinel},
	}
	n3, batches3, capped3, err := runEmptyOwnerPhase(ctx, db, opts, p3)
	if err != nil {
		return stats, fmt.Errorf("phase 3: %w", err)
	}
	stats.Phase3Filled = n3
	stats.Phase3Batches = batches3

	stats.BatchesCapped = capped1 || capped2 || capped3

	after, err := countEmptyOwner(ctx, db)
	if err != nil {
		return stats, fmt.Errorf("count after: %w", err)
	}
	stats.AfterEmpty = after
	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// countEmptyOwner returns the number of non-deleted beads whose owner is
// still empty (`”` or NULL).
func countEmptyOwner(ctx context.Context, db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM beads
		 WHERE COALESCE(owner,'') = ''
		   AND deleted_at IS NULL`,
	).Scan(&n)
	return n, err
}

// emptyOwnerPhase is one phase's SQL fragments.
//
//   - joinExtra: an extra FROM-clause join on the batch-select's `beads b`
//     (may be empty). Phase 1 joins the operators table.
//   - ownerExpr: the SQL expression the owner column is set to, evaluated in
//     the batch select (so it may reference joinExtra's columns).
//   - whereExtra: an extra predicate ANDed onto the empty-owner candidate
//     select (may be empty).
//   - args: the positional parameters ($1, $2, …) referenced by whereExtra
//     and/or ownerExpr; the batch LIMIT is appended as the final positional
//     parameter.
type emptyOwnerPhase struct {
	mutationType string
	joinExtra    string
	ownerExpr    string
	whereExtra   string
	args         []any
}

// runEmptyOwnerPhase drains one phase: a chunked UPDATE loop that runs
// until zero rows change (or MaxBatches iterations). Returns the total
// rows filled, the number of batch iterations, and whether the phase hit
// the MaxBatches cap before draining.
func runEmptyOwnerPhase(
	ctx context.Context,
	db *sql.DB,
	opts store.BackfillEmptyOwnerOptions,
	phase emptyOwnerPhase,
) (filled int64, batches int, capped bool, err error) {
	// operatedByID stamps every mutation_history row this phase writes.
	operatedByID := opts.OperatedByID
	// The batch LIMIT is the final positional parameter.
	limitPos := len(phase.args) + 1
	// The batch select resolves the new owner value (ownerExpr) alongside the
	// locked bead id, so the UPDATE simply copies batch.new_owner. This lets
	// Phase 1 carry the operators-join expression through to the UPDATE.
	// FOR UPDATE OF b locks only the beads rows, never joined operators rows.
	query := fmt.Sprintf(`
		WITH batch AS (
		    SELECT b.id, (%s) AS new_owner FROM beads b
		    %s
		     WHERE COALESCE(b.owner,'') = ''
		       AND b.deleted_at IS NULL
		       %s
		     ORDER BY b.id
		     LIMIT $%d
		     FOR UPDATE OF b SKIP LOCKED
		)
		UPDATE beads b
		   SET owner = batch.new_owner
		  FROM batch
		 WHERE b.id = batch.id
		RETURNING b.id, b.owner`,
		phase.ownerExpr, phase.joinExtra, phase.whereExtra, limitPos,
	)

	for {
		if opts.MaxBatches > 0 && batches >= opts.MaxBatches {
			return filled, batches, true, nil
		}
		n, err := runEmptyOwnerBatch(ctx, db, query, phase, opts.BatchSize, operatedByID)
		if err != nil {
			return filled, batches, capped, err
		}
		if n == 0 {
			return filled, batches, false, nil
		}
		batches++
		filled += n
	}
}

// runEmptyOwnerBatch executes one batch UPDATE + mutation_history inserts
// inside a single transaction and returns the number of rows updated.
// operatedByID is stamped on every mutation_history row (migration 026's
// NOT-NULL CHECK); the caller resolves it fail-closed.
func runEmptyOwnerBatch(
	ctx context.Context,
	db *sql.DB,
	query string,
	phase emptyOwnerPhase,
	batchSize int,
	operatedByID int64,
) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on failure path; no-op after Commit().

	args := append(append([]any{}, phase.args...), batchSize)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("update: %w", err)
	}
	type filled struct{ id, owner string }
	var updates []filled
	for rows.Next() {
		var f filled
		if err := rows.Scan(&f.id, &f.owner); err != nil {
			rows.Close()
			return 0, err
		}
		updates = append(updates, f)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, u := range updates {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type, operated_by_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			u.id,
			"kd admin backfill-empty-owner",
			"owner",
			"", u.owner, phase.mutationType,
			operatedByID,
		); err != nil {
			return 0, fmt.Errorf("mutation_history %s: %w", u.id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int64(len(updates)), nil
}

// dryRunEmptyOwner partitions the current owner=” set into the three
// phases without writing. Because the phases are sequential, a row is
// attributed to the FIRST phase whose predicate it satisfies:
//
//	Phase 1: operator identity resolves to a non-legacy-sentinel value.
//	Phase 2: NOT phase-1 AND created_by usable.
//	Phase 3: everything else (the residual).
//
// The three counts sum to BeforeEmpty (AfterEmpty stays 0 in a dry-run).
func dryRunEmptyOwner(ctx context.Context, db *sql.DB, opts store.BackfillEmptyOwnerOptions, stats *store.BackfillEmptyOwnerStats) error {
	// Phase 1 candidates: operator resolves to a non-legacy identity.
	var p1 int64
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM beads b
		  JOIN operators op ON op.id = b.operated_by_id
		 WHERE COALESCE(b.owner,'') = ''
		   AND b.deleted_at IS NULL
		   AND op.kind || ':' || op.subject <> $1`,
		opts.LegacyOperatedBySentinel,
	).Scan(&p1); err != nil {
		return fmt.Errorf("dry-run phase 1 count: %w", err)
	}
	stats.Phase1Filled = p1

	// Phase 2 candidates: not phase-1-eligible (operator is the legacy
	// sentinel), but created_by usable.
	var p2 int64
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM beads b
		  JOIN operators op ON op.id = b.operated_by_id
		 WHERE COALESCE(b.owner,'') = ''
		   AND b.deleted_at IS NULL
		   AND op.kind || ':' || op.subject = $1
		   AND COALESCE(b.created_by,'') <> ''
		   AND b.created_by <> ALL($2::text[])`,
		opts.LegacyOperatedBySentinel, pq.Array(opts.Phase2Exclusions),
	).Scan(&p2); err != nil {
		return fmt.Errorf("dry-run phase 2 count: %w", err)
	}
	stats.Phase2Filled = p2

	// Phase 3 is the residual.
	stats.Phase3Filled = stats.BeforeEmpty - p1 - p2
	if stats.Phase3Filled < 0 {
		stats.Phase3Filled = 0
	}
	return nil
}
