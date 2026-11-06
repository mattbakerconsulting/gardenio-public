package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/lib/pq"
)

// queryBackfillOperatedByLegacy implements `kd admin
// backfill-operated-by-legacy` (ka-0000001880). See
// gardenio/docs/research/legacy-operated-by-matt-cleanup.md §3.2.
//
// Pass 0 resolves the target operator id from target="<kind>:<subject>".
// Pass 1 scans candidate beads (operated_by_id = target, status filter,
// not soft-deleted) in keyset-paginated batches; each batch is its own
// transaction. Per batch we categorize rows in Go (A/B/D — C is deferred
// to pass 2) and issue one UPDATE per (category, new_operator_id) group
// followed by mutation_history INSERTs. Pass 2 iterates a parent-walk
// UPDATE until no rows change or max_iterations is hit; each iteration
// is its own transaction.
//
// Per kbeads/CLAUDE.md §Migration policy — no eager full-table updates.
// Every UPDATE writes one mutation_history row per affected bead with
// field='operated_by_id', actor='kd admin backfill-operated-by-legacy',
// mutation_type='backfill-legacy-<category>'.
func queryBackfillOperatedByLegacy(ctx context.Context, db *sql.DB, opts store.BackfillOperatedByLegacyOptions) (*store.BackfillOperatedByLegacyStats, error) {
	start := time.Now()
	stats := &store.BackfillOperatedByLegacyStats{
		DryRun:      opts.DryRun,
		Target:      opts.Target,
		RebindCount: map[string]int64{},
	}
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	kind, subject, ok := strings.Cut(opts.Target, ":")
	if !ok || kind == "" || subject == "" {
		return nil, fmt.Errorf("invalid --target %q (want <kind>:<subject>)", opts.Target)
	}

	// Pass 0: resolve target operator id. If absent the cleanup is a no-op.
	var targetOpID int64
	err := db.QueryRowContext(ctx,
		`SELECT id FROM operators WHERE kind = $1 AND subject = $2`,
		kind, subject,
	).Scan(&targetOpID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("target operator %s:%s not found", kind, subject)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve target operator: %w", err)
	}
	stats.TargetOperatorID = targetOpID

	// Initial candidate count for the manifest's before/after delta.
	stats.BeforeOpenCandidates, err = countOperatedByTarget(ctx, db, targetOpID, opts.Statuses)
	if err != nil {
		return nil, fmt.Errorf("count before: %w", err)
	}

	// Pass 1: batched scan + per-category dispatch.
	if err := pass1CategorizeAndDispatch(ctx, db, opts, targetOpID, stats); err != nil {
		return stats, fmt.Errorf("pass 1: %w", err)
	}

	// Pass 2: iterative parent-walk for category C.
	if err := pass2ParentWalk(ctx, db, opts, targetOpID, stats); err != nil {
		return stats, fmt.Errorf("pass 2: %w", err)
	}

	// Pass 3: emit final count.
	stats.AfterOpenCandidates, err = countOperatedByTarget(ctx, db, targetOpID, opts.Statuses)
	if err != nil {
		return stats, fmt.Errorf("count after: %w", err)
	}
	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// countOperatedByTarget returns the number of non-deleted candidate beads
// currently bound to targetOpID in the given statuses.
func countOperatedByTarget(ctx context.Context, db *sql.DB, targetOpID int64, statuses []string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM beads
		 WHERE operated_by_id = $1
		   AND status = ANY($2::text[])
		   AND deleted_at IS NULL`,
		targetOpID, pq.Array(statuses),
	).Scan(&n)
	return n, err
}

// pass1CategorizeAndDispatch pages through candidate beads in batches of
// opts.BatchSize. For each batch, in a single transaction, it categorizes
// each row (A/B/D) in Go and applies UPDATEs grouped by destination
// operator id, then writes mutation_history per affected bead. Category C
// rows are counted but otherwise left for pass 2.
func pass1CategorizeAndDispatch(
	ctx context.Context,
	db *sql.DB,
	opts store.BackfillOperatedByLegacyOptions,
	targetOpID int64,
	stats *store.BackfillOperatedByLegacyStats,
) error {
	lastID := ""
	for {
		batch, err := fetchCandidateBatch(ctx, db, targetOpID, opts.Statuses, lastID, opts.BatchSize)
		if err != nil {
			return fmt.Errorf("fetch batch: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}
		lastID = batch[len(batch)-1].id

		groupsA := map[int64][]string{}
		groupsB := map[int64][]string{}
		catC := 0
		catD := 0

		for _, b := range batch {
			cat, newOpID, err := classifyBead(ctx, db, b, opts.TargetSubject(), targetOpID)
			if err != nil {
				return fmt.Errorf("classify %s: %w", b.id, err)
			}
			switch cat {
			case "A":
				groupsA[newOpID] = append(groupsA[newOpID], b.id)
			case "B":
				groupsB[newOpID] = append(groupsB[newOpID], b.id)
			case "C":
				catC++
			default:
				catD++
			}
		}
		stats.CategoryC += int64(catC)
		stats.CategoryD += int64(catD)

		if opts.DryRun {
			// Tally would-be changes; no writes.
			for op, ids := range groupsA {
				stats.CategoryA += int64(len(ids))
				stats.RebindCount[formatRebindKey("A", op)] += int64(len(ids))
			}
			for op, ids := range groupsB {
				stats.CategoryB += int64(len(ids))
				stats.RebindCount[formatRebindKey("B", op)] += int64(len(ids))
			}
		} else {
			if err := applyBatchRebinds(ctx, db, targetOpID, groupsA, groupsB, stats); err != nil {
				return err
			}
		}

		if len(batch) < opts.BatchSize {
			return nil
		}
	}
}

// candidateRow is one bead row in pass 1's scan output.
type candidateRow struct {
	id        string
	createdBy string
	fields    json.RawMessage
}

func fetchCandidateBatch(
	ctx context.Context,
	db *sql.DB,
	targetOpID int64,
	statuses []string,
	afterID string,
	limit int,
) ([]candidateRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, created_by, COALESCE(fields, '{}'::jsonb)
		  FROM beads
		 WHERE operated_by_id = $1
		   AND status = ANY($2::text[])
		   AND deleted_at IS NULL
		   AND id > $3
		 ORDER BY id
		 LIMIT $4`,
		targetOpID, pq.Array(statuses), afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []candidateRow
	for rows.Next() {
		var r candidateRow
		if err := rows.Scan(&r.id, &r.createdBy, &r.fields); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// classifyBead returns the category and (for A/B) the destination
// operator id to rebind to. Category C beads are left alone (the
// parent-walk pass handles them). Category D ("genuinely Matt") and
// any A/B row that fails to resolve a non-target destination also
// returns "D".
func classifyBead(
	ctx context.Context,
	db *sql.DB,
	b candidateRow,
	targetSubject string,
	targetOpID int64,
) (cat string, newOpID int64, err error) {
	fields := decodeFieldsLegacy(b.fields)

	// Category A: Slack-thread agent triggered by a non-target Slack user.
	if strings.HasPrefix(b.createdBy, "thread-") {
		uid, _ := fields["slack_user_id"].(string)
		if uid == "" {
			uid, _ = fields["trigger_created_by_subject"].(string)
		}
		if uid != "" && uid != targetSubject {
			id, err := lookupOperatorAliasID(ctx, db, "slack", uid)
			if err == nil && id != 0 && id != targetOpID {
				return "A", id, nil
			}
		}
		// Thread agent attributed to target or unresolvable → genuinely Matt.
		return "D", 0, nil
	}

	// Category B: scheduler-driven; bind to the schedule's operated_by_id.
	if b.createdBy == "static" {
		if scheduleID, _ := fields["schedule_id"].(string); scheduleID != "" {
			id, err := lookupBeadOperatorID(ctx, db, scheduleID)
			if err == nil && id != 0 && id != targetOpID {
				return "B", id, nil
			}
		}
		return "D", 0, nil
	}

	// Category D direct-CLI invocations stay put.
	if b.createdBy == "" {
		return "D", 0, nil
	}

	// Category C: agent-spawned (sched-*, gardenio-*, monorepo-*, kbeads-*,
	// or any other identifier that looks like an agent slug). Parent-walk
	// pass 2 handles these in one shot.
	return "C", 0, nil
}

// applyBatchRebinds executes one transaction containing every category-A
// and category-B UPDATE for this batch plus a mutation_history row per
// affected bead.
func applyBatchRebinds(
	ctx context.Context,
	db *sql.DB,
	targetOpID int64,
	groupsA, groupsB map[int64][]string,
	stats *store.BackfillOperatedByLegacyStats,
) error {
	if len(groupsA) == 0 && len(groupsB) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on failure path; no-op if Commit() succeeded.

	if err := rebindGroup(ctx, tx, "A", targetOpID, groupsA, stats); err != nil {
		return err
	}
	if err := rebindGroup(ctx, tx, "B", targetOpID, groupsB, stats); err != nil {
		return err
	}
	return tx.Commit()
}

func rebindGroup(
	ctx context.Context,
	tx *sql.Tx,
	category string,
	targetOpID int64,
	groups map[int64][]string,
	stats *store.BackfillOperatedByLegacyStats,
) error {
	for newOpID, ids := range groups {
		res, err := tx.ExecContext(ctx, `
			UPDATE beads
			   SET operated_by_id = $1
			 WHERE id = ANY($2::text[])
			   AND operated_by_id = $3`,
			newOpID, pq.Array(ids), targetOpID,
		)
		if err != nil {
			return fmt.Errorf("update cat %s -> op %d: %w", category, newOpID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue
		}
		mutationType := "backfill-legacy-" + category
		oldVal := fmt.Sprintf("%d", targetOpID)
		newVal := fmt.Sprintf("%d", newOpID)
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				id,
				"kd admin backfill-operated-by-legacy",
				"operated_by_id",
				oldVal, newVal, mutationType,
			); err != nil {
				return fmt.Errorf("mutation_history %s: %w", id, err)
			}
		}
		switch category {
		case "A":
			stats.CategoryA += n
		case "B":
			stats.CategoryB += n
		}
		stats.RebindCount[formatRebindKey(category, newOpID)] += n
	}
	return nil
}

// pass2ParentWalk repeatedly rebinds Cat-C beads to their parent's
// operated_by_id until no rows change or max_iterations is hit. Each
// iteration is its own transaction so a failure mid-walk leaves a
// coherent half-walked state that a re-run can finish.
func pass2ParentWalk(
	ctx context.Context,
	db *sql.DB,
	opts store.BackfillOperatedByLegacyOptions,
	targetOpID int64,
	stats *store.BackfillOperatedByLegacyStats,
) error {
	maxIter := opts.MaxIterations
	if maxIter <= 0 {
		maxIter = 5
	}
	statuses := pq.Array(opts.Statuses)

	for i := 0; i < maxIter; i++ {
		if opts.DryRun {
			// Dry-run: count would-be rebinds without writing.
			var n int64
			err := db.QueryRowContext(ctx, `
				SELECT count(*)
				  FROM beads b
				  JOIN deps d ON d.bead_id = b.id AND d.type = 'parent-child'
				  JOIN beads p ON p.id = d.depends_on_id
				 WHERE b.operated_by_id = $1
				   AND p.operated_by_id IS NOT NULL
				   AND p.operated_by_id <> $1
				   AND b.status = ANY($2::text[])
				   AND b.deleted_at IS NULL`,
				targetOpID, statuses,
			).Scan(&n)
			if err != nil {
				return fmt.Errorf("dry-run iter %d: %w", i+1, err)
			}
			stats.Iterations = i + 1
			if n == 0 {
				return nil
			}
			stats.CategoryC += n
			// Dry-run reports only the first iteration's would-be count —
			// subsequent passes would cascade off rebinds we did not apply.
			return nil
		}

		n, err := pass2OneIteration(ctx, db, targetOpID, statuses, stats)
		if err != nil {
			return fmt.Errorf("iter %d: %w", i+1, err)
		}
		stats.Iterations = i + 1
		if n == 0 {
			return nil
		}
	}
	stats.IterationsCapped = true
	return nil
}

func pass2OneIteration(
	ctx context.Context,
	db *sql.DB,
	targetOpID int64,
	statuses any,
	stats *store.BackfillOperatedByLegacyStats,
) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on failure path; no-op if Commit() succeeded.

	rows, err := tx.QueryContext(ctx, `
		WITH walked AS (
		    SELECT b.id, p.operated_by_id AS new_op
		      FROM beads b
		      JOIN deps  d ON d.bead_id = b.id AND d.type = 'parent-child'
		      JOIN beads p ON p.id = d.depends_on_id
		     WHERE b.operated_by_id = $1
		       AND p.operated_by_id IS NOT NULL
		       AND p.operated_by_id <> $1
		       AND b.status = ANY($2::text[])
		       AND b.deleted_at IS NULL
		)
		UPDATE beads b
		   SET operated_by_id = w.new_op
		  FROM walked w
		 WHERE b.id = w.id
		   AND b.operated_by_id = $1
		RETURNING b.id, w.new_op`,
		targetOpID, statuses,
	)
	if err != nil {
		return 0, fmt.Errorf("parent-walk update: %w", err)
	}
	type update struct {
		id    string
		newOp int64
	}
	var updates []update
	for rows.Next() {
		var u update
		if err := rows.Scan(&u.id, &u.newOp); err != nil {
			rows.Close()
			return 0, err
		}
		updates = append(updates, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	oldVal := fmt.Sprintf("%d", targetOpID)
	for _, u := range updates {
		newVal := fmt.Sprintf("%d", u.newOp)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			u.id,
			"kd admin backfill-operated-by-legacy",
			"operated_by_id",
			oldVal, newVal, "backfill-legacy-C",
		); err != nil {
			return 0, fmt.Errorf("mutation_history %s: %w", u.id, err)
		}
		stats.RebindCount[formatRebindKey("C", u.newOp)]++
	}
	stats.CategoryC += int64(len(updates))

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return int64(len(updates)), nil
}

// lookupOperatorAliasID resolves (kind, subject) to operators.id via
// operator_aliases. Returns 0 (and nil) when no alias matches.
func lookupOperatorAliasID(ctx context.Context, db *sql.DB, kind, subject string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		`SELECT operator_id FROM operator_aliases WHERE kind = $1 AND subject = $2`,
		kind, subject,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// lookupBeadOperatorID returns the operated_by_id for the supplied bead.
// Returns 0 when the bead has no FK binding (or does not exist).
func lookupBeadOperatorID(ctx context.Context, db *sql.DB, beadID string) (int64, error) {
	var id sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT operated_by_id FROM beads WHERE id = $1`, beadID,
	).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

func decodeFieldsLegacy(raw json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(raw) == 0 {
		return m
	}
	_ = json.Unmarshal(raw, &m)
	return m
}

func formatRebindKey(category string, opID int64) string {
	return fmt.Sprintf("%s:%d", category, opID)
}
