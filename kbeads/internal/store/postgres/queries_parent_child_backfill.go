package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"gardenio/kbeads/internal/store"
)

// queryBackfillParentChildDeps implements `kd admin
// backfill-parent-child-deps` (ka-0000006775). See the bead description
// and gardenio/docs/research/attribution-chain-spawn-paths.md
// (Follow-up beads § P3) for the algorithm.
//
// Each candidate row is an agent-authored bead created within
// opts.Since (default 6 months) or — when IncludeClosed is true — any
// closed bead regardless of age. The single SQL below carries out
// the join to the author's agent bead and reads fields.task_id in one
// pass, and the LEFT JOIN to deps filters out rows that already have an
// outbound parent-child edge. Per-page work runs in one transaction so
// a mid-run failure leaves a coherent, resumable state.
//
// Per kbeads/CLAUDE.md §Migration policy — no eager full-table
// updates. The page-size keyset cursor (id > lastID ORDER BY id LIMIT
// BatchSize) keeps each tx narrow, and the insert uses
// `ON CONFLICT … DO NOTHING` (queryAddDependency) so re-runs on
// already-linked data are no-ops. INSERTs record the standard deps row
// with created_by='kd admin backfill-parent-child-deps' so the audit
// trail is queryable.
func queryBackfillParentChildDeps(
	ctx context.Context,
	db *sql.DB,
	opts store.BackfillParentChildDepsOptions,
) (*store.BackfillParentChildDepsStats, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	stats := &store.BackfillParentChildDepsStats{
		DryRun:        opts.DryRun,
		EdgesByAuthor: map[string]int64{},
	}

	cutoff := time.Now().Add(-opts.Since)
	lastID := ""
	for {
		page, err := fetchParentChildBackfillPage(ctx, db, cutoff, opts.IncludeClosed, lastID, opts.BatchSize)
		if err != nil {
			return stats, fmt.Errorf("fetch page (after %q): %w", lastID, err)
		}
		if len(page) == 0 {
			break
		}
		lastID = page[len(page)-1].id
		stats.LastID = lastID
		stats.CandidateBeads += int64(len(page))

		if err := applyParentChildPage(ctx, db, page, opts.DryRun, stats); err != nil {
			return stats, fmt.Errorf("apply page (after %q): %w", lastID, err)
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

// parentChildCandidate is one bead returned by the page scan. taskID is
// the resolved fields.task_id from the author's agent bead (may be
// empty when the agent bead has no task_id), createdBy is the author
// slug (used for grouping in stats.EdgesByAuthor and for the audit
// trail), description is inspected to skip rows that already declare a
// `parent(gd-…)` mention.
type parentChildCandidate struct {
	id             string
	createdBy      string
	description    string
	taskID         sql.NullString
	taskBeadExists bool
	hasExistingDep bool
}

// fetchParentChildBackfillPage returns one keyset-paginated page of
// agent-authored beads in scope. The single SELECT carries out four
// jobs:
//
//  1. Restrict by scope: created_by present, not soft-deleted, within
//     window (created_at >= cutoff) or — when includeClosed — closed.
//  2. LEFT JOIN to the author's agent bead via fields.agent.
//  3. LEFT JOIN to deps to detect existing parent-child outbound edges.
//  4. Carry task_id and task-existence so per-row filtering and the
//     would-be-edge count can run in Go.
func fetchParentChildBackfillPage(
	ctx context.Context,
	db *sql.DB,
	cutoff time.Time,
	includeClosed bool,
	afterID string,
	limit int,
) ([]parentChildCandidate, error) {
	scopeCond := "b.created_at >= $1"
	if includeClosed {
		scopeCond = "(b.created_at >= $1 OR b.status = 'closed')"
	}
	// Exclude agent-lifecycle + comms bookkeeping beads (type=agent / session /
	// message / mail) from reconstruction, matching the create-time skip in the
	// daemon's maybeAddAutoParent (bug ka-0000002199, ka-0000004462). These are
	// not work and must never be (re-)parented into a goal-bearing epic's child
	// set — doing so is the exact epic-pollution the create-time fix removes, so
	// a manual backfill run must not silently re-introduce it. Their real
	// linkage is the `assigned` / `session_of` edge or the `thread_id` field,
	// which this reconstruction never touches. This list must stay in lockstep
	// with autoParentSkipTypes (internal/server/auto_parent.go).
	const excludeBookkeepingTypes = " AND b.type NOT IN ('agent', 'session', 'message', 'mail')"
	q := `
		SELECT b.id,
		       b.created_by,
		       COALESCE(b.description, ''),
		       a.task_id,
		       (a.task_id IS NOT NULL AND tb.id IS NOT NULL) AS task_bead_exists,
		       (existing.bead_id IS NOT NULL) AS has_existing_dep
		  FROM beads b
		  LEFT JOIN LATERAL (
		      SELECT (ag.fields->>'task_id') AS task_id
		        FROM beads ag
		       WHERE ag.type = 'agent'
		         AND ag.deleted_at IS NULL
		         AND (ag.fields->>'agent') = b.created_by
		       ORDER BY ag.created_at DESC
		       LIMIT 1
		  ) a ON true
		  LEFT JOIN beads tb
		    ON tb.id = a.task_id
		   AND tb.deleted_at IS NULL
		  LEFT JOIN LATERAL (
		      SELECT 1 AS bead_id
		        FROM deps d
		       WHERE d.bead_id = b.id
		         AND d.type = 'parent-child'
		       LIMIT 1
		  ) existing ON true
		 WHERE b.created_by IS NOT NULL
		   AND b.created_by <> ''
		   AND b.created_by <> 'static'
		   AND b.deleted_at IS NULL` + excludeBookkeepingTypes + `
		   AND ` + scopeCond + `
		   AND b.id > $2
		 ORDER BY b.id
		 LIMIT $3`
	rows, err := db.QueryContext(ctx, q, cutoff, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []parentChildCandidate
	for rows.Next() {
		var c parentChildCandidate
		if err := rows.Scan(
			&c.id,
			&c.createdBy,
			&c.description,
			&c.taskID,
			&c.taskBeadExists,
			&c.hasExistingDep,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// applyParentChildPage classifies one page of candidates, tallying the
// skip reasons and — outside DryRun — inserting the parent-child deps
// inside a single transaction. The INSERT uses the standard deps
// `ON CONFLICT … DO NOTHING` path so a re-run or a concurrent insert
// leaves the row untouched and the stats accurate (we only bump
// EdgesAdded when the INSERT actually wrote a row).
func applyParentChildPage(
	ctx context.Context,
	db *sql.DB,
	page []parentChildCandidate,
	dryRun bool,
	stats *store.BackfillParentChildDepsStats,
) error {
	type insertRow struct {
		beadID string
		taskID string
		author string
	}
	var inserts []insertRow

	for _, c := range page {
		if c.hasExistingDep {
			stats.SkippedExistingDep++
			continue
		}
		if strings.HasPrefix(c.description, "parent(gd-") || strings.HasPrefix(c.description, "parent(ka-") {
			stats.SkippedParentMention++
			continue
		}
		if !c.taskID.Valid {
			// LEFT JOIN to agent bead missed entirely.
			stats.SkippedNoAgent++
			continue
		}
		if c.taskID.String == "" {
			stats.SkippedNoTaskID++
			continue
		}
		if c.taskID.String == c.id {
			stats.SkippedSelfParent++
			continue
		}
		if !c.taskBeadExists {
			stats.SkippedTaskMissing++
			continue
		}
		inserts = append(inserts, insertRow{
			beadID: c.id,
			taskID: c.taskID.String,
			author: c.createdBy,
		})
	}

	if dryRun {
		for _, ins := range inserts {
			stats.EdgesAdded++
			stats.EdgesByAuthor[ins.author]++
		}
		return nil
	}
	if len(inserts) == 0 {
		return nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on failure path; no-op if Commit() succeeded.

	now := time.Now().UTC()
	for _, ins := range inserts {
		// INSERT mirrors queryAddDependency so the dep row is shaped
		// identically to one produced by the normal API path. fleet is
		// inherited from the source bead; we set created_by to the
		// admin identifier so the audit trail is searchable.
		res, err := tx.ExecContext(ctx, `
			INSERT INTO deps (bead_id, depends_on_id, type, created_at, created_by, metadata, fleet)
			VALUES (
			    $1, $2, $3, $4, $5, $6,
			    COALESCE((SELECT fleet FROM beads WHERE id = $1), 'default')
			)
			ON CONFLICT (bead_id, depends_on_id, type) DO NOTHING`,
			ins.beadID,
			ins.taskID,
			"parent-child",
			now,
			"kd admin backfill-parent-child-deps",
			[]byte("{}"),
		)
		if err != nil {
			// Foreign-key races (task bead soft-deleted between scan and
			// insert, or bead row removed mid-flight) should not abort
			// the whole batch — log a skip and continue.
			if isFKViolation(err) {
				stats.SkippedTaskMissing++
				continue
			}
			return fmt.Errorf("insert dep %s -> %s: %w", ins.beadID, ins.taskID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// Lost the race with another writer — count as existing-dep.
			stats.SkippedExistingDep++
			continue
		}
		stats.EdgesAdded++
		stats.EdgesByAuthor[ins.author]++
	}
	return tx.Commit()
}

// isFKViolation returns true for pq foreign-key violation errors. The
// scan-then-write loop occasionally races a soft-delete of the parent
// task; the row count check above suppresses ON CONFLICT cases, this
// covers FK-misses.
func isFKViolation(err error) bool {
	if err == nil {
		return false
	}
	// pq.Error carries a SQLSTATE; 23503 is foreign_key_violation. We
	// match on the string form to avoid pulling in the pq error type at
	// the package boundary.
	var pqe interface{ SQLState() string }
	if errors.As(err, &pqe) {
		return pqe.SQLState() == "23503"
	}
	msg := err.Error()
	return strings.Contains(msg, "foreign key constraint") ||
		strings.Contains(msg, "violates foreign key")
}
