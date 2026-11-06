package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"gardenio/kbeads/internal/store"
)

// queryBackfillOperators implements the P3 operators-FK backfill described
// in docs/adr/human-attribution-identity-v2.md §4.3 / §4.5 / §4.6.
//
// Pass 1: walk DISTINCT operated_by across the five audit tables (beads,
//
//	comments, deps, events, mutation_history), prefix-decode on the first
//	`:` into (kind, subject), upsert operators + operator_aliases with
//	source='migration'. Values without a `:` are quarantined (returned in
//	stats.QuarantinedOperatedBy).
//
// Pass 2: per audit table T, batched UPDATE setting
//
//	`operated_by_id = a.operator_id` from operator_aliases a where the TEXT
//	prefix/suffix matches kind/subject. WHERE operated_by_id IS NULL AND
//	operated_by <> ''. Batch by primary-key range to keep transactions small
//	(deps has a composite PK and a small row count, so it gets a single-shot
//	UPDATE).
//
// Empty-string policy (§4.5):
//   - beads / comments / deps  → bind to the `system:legacy-pre-attribution`
//     sentinel operator (precursor migration 016 inserts that row).
//   - events / mutation_history → leave operated_by_id NULL ("we do not
//     know who operated this").
//
// Verifier query at the end reports per-table counts of
// (operated_by <> ” AND operated_by_id IS NULL). Must be zero on success
// modulo any rows whose TEXT was quarantined in Pass 1.
//
// Idempotent — every UPDATE carries WHERE operated_by_id IS NULL so
// re-running on already-bound data is a no-op.
func queryBackfillOperators(ctx context.Context, db *sql.DB, opts store.BackfillOperatorsOptions) (*store.BackfillOperatorsStats, error) {
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = 50000
	}
	stats := &store.BackfillOperatorsStats{DryRun: opts.DryRun}
	start := time.Now()

	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Close()

	// Pass 1: collect distinct operated_by values across the five tables,
	// quarantine non-splittable values, and (when not dry-run) upsert
	// operators + operator_aliases.
	if err := collectAndUpsertOperators(ctx, conn, opts.DryRun, stats); err != nil {
		return stats, fmt.Errorf("pass 1: %w", err)
	}

	// Resolve the legacy-pre-attribution sentinel id (required for empty-string
	// binding on live-state tables; precursor migration 016 inserts it).
	sentinelID, err := lookupLegacyPreAttributionSentinel(ctx, conn)
	if err != nil {
		return stats, fmt.Errorf("legacy-pre-attribution sentinel: %w", err)
	}

	// Pass 2 (TEXT-populated rows): batched UPDATE per table.
	type tableSpec struct {
		name        string
		pkCol       string // "" → single-shot (deps composite PK)
		updateField *int64
	}
	tables := []tableSpec{
		{name: "beads", pkCol: "id", updateField: &stats.BeadsUpdated},
		{name: "comments", pkCol: "id", updateField: &stats.CommentsUpdated},
		{name: "events", pkCol: "id", updateField: &stats.EventsUpdated},
		{name: "mutation_history", pkCol: "id", updateField: &stats.MutationsUpdated},
		{name: "deps", pkCol: "", updateField: &stats.DepsUpdated},
	}

	for _, t := range tables {
		var n int64
		var err error
		if opts.DryRun {
			n, err = countOperatorsBackfillCandidates(ctx, conn, t.name)
		} else if t.pkCol == "" {
			n, err = singleShotOperatorsBackfill(ctx, conn, t.name)
		} else {
			n, err = batchedOperatorsBackfill(ctx, conn, t.name, t.pkCol, batchSize)
		}
		if err != nil {
			return stats, fmt.Errorf("%s: %w", t.name, err)
		}
		*t.updateField = n
	}

	// Pass 2 (empty-string rows on live-state tables): bind to sentinel.
	emptyTables := []struct {
		name        string
		updateField *int64
	}{
		{name: "beads", updateField: &stats.BeadsEmptySentinel},
		{name: "comments", updateField: &stats.CommentsEmptySentinel},
		{name: "deps", updateField: &stats.DepsEmptySentinel},
	}
	for _, t := range emptyTables {
		var n int64
		var err error
		if opts.DryRun {
			n, err = countEmptySentinelCandidates(ctx, conn, t.name)
		} else {
			n, err = bindEmptyToSentinel(ctx, conn, t.name, sentinelID)
		}
		if err != nil {
			return stats, fmt.Errorf("%s empty-sentinel: %w", t.name, err)
		}
		*t.updateField = n
	}

	// Verifier: count rows where TEXT is populated but FK is NULL.
	if err := runVerifier(ctx, conn, stats); err != nil {
		return stats, fmt.Errorf("verifier: %w", err)
	}

	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// collectAndUpsertOperators walks DISTINCT operated_by across the five
// audit tables, splits on the first `:`, and (when not dry-run) upserts
// operators + operator_aliases. Quarantines TEXT values that do not split.
func collectAndUpsertOperators(ctx context.Context, conn *sql.Conn, dryRun bool, stats *store.BackfillOperatorsStats) error {
	rows, err := conn.QueryContext(ctx, `
		SELECT DISTINCT operated_by FROM (
		    SELECT operated_by FROM beads             WHERE operated_by <> ''
		    UNION
		    SELECT operated_by FROM comments          WHERE operated_by <> ''
		    UNION
		    SELECT operated_by FROM deps              WHERE operated_by <> ''
		    UNION
		    SELECT operated_by FROM events            WHERE operated_by <> ''
		    UNION
		    SELECT operated_by FROM mutation_history  WHERE operated_by <> ''
		) u
		ORDER BY operated_by`)
	if err != nil {
		return fmt.Errorf("select distinct operated_by: %w", err)
	}
	defer rows.Close()

	var distinct []struct{ kind, subject, raw string }
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return fmt.Errorf("scan distinct row: %w", err)
		}
		idx := strings.IndexByte(v, ':')
		if idx <= 0 || idx == len(v)-1 {
			// No colon, leading colon, or trailing colon — not splittable
			// into a meaningful (kind, subject). Quarantine for manual review.
			stats.QuarantinedOperatedBy = append(stats.QuarantinedOperatedBy, v)
			continue
		}
		distinct = append(distinct, struct{ kind, subject, raw string }{
			kind: v[:idx], subject: v[idx+1:], raw: v,
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate distinct: %w", err)
	}
	stats.DistinctOperatedBy = len(distinct)

	if dryRun || len(distinct) == 0 {
		return nil
	}

	// Upsert operators (idempotent). Re-runs land on existing rows and the
	// ON CONFLICT clause keeps them stable; we tally only inserts that
	// actually went in by comparing rows-affected.
	for _, d := range distinct {
		var inserted bool
		if err := conn.QueryRowContext(ctx, `
			INSERT INTO operators (kind, subject) VALUES ($1, $2)
			ON CONFLICT (kind, subject) DO NOTHING
			RETURNING true`, d.kind, d.subject).Scan(&inserted); err != nil {
			if err == sql.ErrNoRows {
				// Row already existed — not an error.
			} else {
				return fmt.Errorf("upsert operator %q: %w", d.raw, err)
			}
		}
		if inserted {
			stats.OperatorsUpserted++
		}
	}

	// Upsert canonical aliases pointing at the just-upserted operators.
	// One bulk INSERT … SELECT covers every (kind, subject) we collected.
	// We rebuild via a VALUES list to keep the operation atomic.
	if len(distinct) > 0 {
		var b strings.Builder
		args := make([]any, 0, len(distinct)*2)
		b.WriteString(`
			INSERT INTO operator_aliases (operator_id, kind, subject, source)
			SELECT o.id, v.kind, v.subject, 'migration'
			  FROM operators o
			  JOIN (VALUES `)
		for i, d := range distinct {
			if i > 0 {
				b.WriteString(", ")
			}
			fmt.Fprintf(&b, "($%d::text, $%d::text)", 2*i+1, 2*i+2)
			args = append(args, d.kind, d.subject)
		}
		b.WriteString(`) AS v(kind, subject)
			    ON o.kind = v.kind AND o.subject = v.subject
			ON CONFLICT (kind, subject) DO NOTHING`)
		res, err := conn.ExecContext(ctx, b.String(), args...)
		if err != nil {
			return fmt.Errorf("upsert operator_aliases: %w", err)
		}
		n, _ := res.RowsAffected()
		stats.AliasesUpserted = int(n)
	}
	return nil
}

// lookupLegacyPreAttributionSentinel returns the operators(id) for the
// system:legacy-pre-attribution sentinel inserted by migration 016.
func lookupLegacyPreAttributionSentinel(ctx context.Context, conn *sql.Conn) (int64, error) {
	var id int64
	err := conn.QueryRowContext(ctx, `
		SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution'`,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, fmt.Errorf("system:legacy-pre-attribution sentinel missing — apply migration 016 first")
	}
	if err != nil {
		return 0, fmt.Errorf("lookup sentinel: %w", err)
	}
	return id, nil
}

// countOperatorsBackfillCandidates returns the number of TEXT-populated rows
// on this table that the real backfill would update — i.e. rows where
// operated_by_id IS NULL AND operated_by <> ” AND a matching alias exists.
func countOperatorsBackfillCandidates(ctx context.Context, conn *sql.Conn, table string) (int64, error) {
	q := fmt.Sprintf(`
		SELECT count(*)
		  FROM %[1]s t
		  JOIN operator_aliases a
		    ON a.kind = split_part(t.operated_by, ':', 1)
		   AND a.subject = substring(t.operated_by from position(':' in t.operated_by) + 1)
		 WHERE t.operated_by_id IS NULL
		   AND t.operated_by <> ''
		   AND position(':' in t.operated_by) > 1`, table)
	var n int64
	if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count candidates: %w", err)
	}
	return n, nil
}

// singleShotOperatorsBackfill issues one UPDATE with no batch boundary.
// Used for deps (composite PK, small row count). Returns rows updated.
func singleShotOperatorsBackfill(ctx context.Context, conn *sql.Conn, table string) (int64, error) {
	q := fmt.Sprintf(`
		UPDATE %[1]s t
		   SET operated_by_id = a.operator_id
		  FROM operator_aliases a
		 WHERE t.operated_by_id IS NULL
		   AND t.operated_by <> ''
		   AND position(':' in t.operated_by) > 1
		   AND a.kind    = split_part(t.operated_by, ':', 1)
		   AND a.subject = substring(t.operated_by from position(':' in t.operated_by) + 1)`, table)
	res, err := conn.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("update: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// batchedOperatorsBackfill issues UPDATEs in chunks of batchSize, picking
// the next slice of unbound matching rows by primary key each iteration.
// Each statement implicitly autocommits so a failure mid-loop leaves a
// coherent partially-bound table that re-running can finish.
func batchedOperatorsBackfill(ctx context.Context, conn *sql.Conn, table, pkCol string, batchSize int) (int64, error) {
	q := fmt.Sprintf(`
		UPDATE %[1]s t
		   SET operated_by_id = a.operator_id
		  FROM operator_aliases a
		 WHERE t.%[2]s IN (
		           SELECT %[2]s
		             FROM %[1]s
		            WHERE operated_by_id IS NULL
		              AND operated_by <> ''
		              AND position(':' in operated_by) > 1
		            ORDER BY %[2]s
		            LIMIT $1
		       )
		   AND t.operated_by_id IS NULL
		   AND a.kind    = split_part(t.operated_by, ':', 1)
		   AND a.subject = substring(t.operated_by from position(':' in t.operated_by) + 1)`, table, pkCol)
	var total int64
	for {
		res, err := conn.ExecContext(ctx, q, batchSize)
		if err != nil {
			return total, fmt.Errorf("batched update: %w", err)
		}
		n, _ := res.RowsAffected()
		total += n
		if n == 0 || n < int64(batchSize) {
			return total, nil
		}
		// Brief pause between batches to give other writers a window
		// (mirrors ADR §4.6 "brief sleep between batches").
		time.Sleep(50 * time.Millisecond)
	}
}

// countEmptySentinelCandidates returns the number of empty-string TEXT
// rows on this table that the real backfill would bind to the sentinel.
func countEmptySentinelCandidates(ctx context.Context, conn *sql.Conn, table string) (int64, error) {
	q := fmt.Sprintf(`
		SELECT count(*) FROM %[1]s
		 WHERE operated_by_id IS NULL AND operated_by = ''`, table)
	var n int64
	if err := conn.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0, fmt.Errorf("count empty-sentinel: %w", err)
	}
	return n, nil
}

// bindEmptyToSentinel binds every empty-string-operated_by row on `table`
// to the system:legacy-pre-attribution sentinel operator (ADR §4.5).
// Single-shot UPDATE — the empty-row count on live-state tables is bounded
// by the pre-attribution corpus and stays manageable.
func bindEmptyToSentinel(ctx context.Context, conn *sql.Conn, table string, sentinelID int64) (int64, error) {
	q := fmt.Sprintf(`
		UPDATE %[1]s
		   SET operated_by_id = $1
		 WHERE operated_by_id IS NULL AND operated_by = ''`, table)
	res, err := conn.ExecContext(ctx, q, sentinelID)
	if err != nil {
		return 0, fmt.Errorf("bind empty: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// runVerifier reports per-table counts of rows where operated_by is
// populated but operated_by_id is still NULL. Must be zero on success
// modulo any quarantined operated_by values.
func runVerifier(ctx context.Context, conn *sql.Conn, stats *store.BackfillOperatorsStats) error {
	for _, t := range []struct {
		name string
		into *int64
	}{
		{"beads", &stats.VerifierBeads},
		{"comments", &stats.VerifierComments},
		{"deps", &stats.VerifierDeps},
		{"events", &stats.VerifierEvents},
		{"mutation_history", &stats.VerifierMutations},
	} {
		q := fmt.Sprintf(`
			SELECT count(*) FROM %s
			 WHERE operated_by <> '' AND operated_by_id IS NULL`, t.name)
		if err := conn.QueryRowContext(ctx, q).Scan(t.into); err != nil {
			return fmt.Errorf("verify %s: %w", t.name, err)
		}
	}
	return nil
}
