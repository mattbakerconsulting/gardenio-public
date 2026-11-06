package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gardenio/kbeads/internal/store"
)

// queryBackfillTriggeredByDeps implements `kd admin
// backfill-triggered-by-deps` — the M4 auto-link backfill scanner from
// epic ka-0000000933 / design doc ka-0000000827. Walks recently-created
// beads (default 24h window) that have no parent-child or triggered-by
// dep and writes a triggered-by edge when the bead's fields carry a
// single unambiguous origin signal:
//
//   - fields.slack_thread_bead_id — slack-thread spawn (bot_mentions.go).
//   - fields.trigger_id           — trigger-engine spawn (triggers.go).
//
// Both fields hold canonical bead IDs at write time, so there is no
// lookup ambiguity. When both signals are set and disagree, the scan
// records an ambiguous skip rather than guessing.
//
// Append-only: never modifies existing deps. Idempotent — the INSERT
// uses the standard ON CONFLICT … DO NOTHING dep path, so re-running on
// already-linked data is a no-op. Each batch is its own transaction so
// a mid-run failure leaves a coherent, resumable state.
func queryBackfillTriggeredByDeps(
	ctx context.Context,
	db *sql.DB,
	opts store.BackfillTriggeredByDepsOptions,
) (*store.BackfillTriggeredByDepsStats, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	stats := &store.BackfillTriggeredByDepsStats{
		DryRun:        opts.DryRun,
		EdgesBySignal: map[string]int64{},
	}

	cutoff := time.Now().Add(-opts.Since)
	lastID := ""
	for {
		page, err := fetchTriggeredByBackfillPage(ctx, db, cutoff, lastID, opts.BatchSize)
		if err != nil {
			return stats, fmt.Errorf("fetch page (after %q): %w", lastID, err)
		}
		if len(page) == 0 {
			break
		}
		lastID = page[len(page)-1].id
		stats.LastID = lastID
		stats.CandidateBeads += int64(len(page))

		if err := applyTriggeredByPage(ctx, db, page, opts.DryRun, stats); err != nil {
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

// triggeredByCandidate is one bead returned by the page scan. The two
// signal fields and the per-target existence flags are evaluated in Go
// (rather than collapsed in SQL) so the skip tallies surface what the
// scanner saw vs. what it wrote.
type triggeredByCandidate struct {
	id                    string
	slackThreadBeadID     sql.NullString
	triggerID             sql.NullString
	slackTargetExists     bool
	triggerTargetExists   bool
	hasExistingParentDep  bool
	hasExistingTriggerDep bool
}

// fetchTriggeredByBackfillPage returns one keyset-paginated page of
// candidate beads in scope. Restricts to beads created within the
// rolling window that carry at least one of the signal fields — beads
// with no signal are excluded by the WHERE clause (no skip tally for
// them since they would dominate the page count otherwise).
//
// Both signal-target existence checks (LEFT JOIN to beads tb) are
// carried as booleans so per-row classification can run in Go and the
// SkippedTargetMissing tally reports correctly.
//
// The existing-dep checks LEFT JOIN against deps separately for
// parent-child and triggered-by so the scanner can distinguish "already
// triggered-by linked" (no-op) from "intentionally parent-child
// parented" (skip, do not double-link).
func fetchTriggeredByBackfillPage(
	ctx context.Context,
	db *sql.DB,
	cutoff time.Time,
	afterID string,
	limit int,
) ([]triggeredByCandidate, error) {
	q := `
		SELECT b.id,
		       NULLIF(b.fields->>'slack_thread_bead_id', '') AS slack_thread_bead_id,
		       NULLIF(b.fields->>'trigger_id', '')           AS trigger_id,
		       (slack_tb.id IS NOT NULL)                     AS slack_target_exists,
		       (trig_tb.id IS NOT NULL)                      AS trigger_target_exists,
		       (existing_pc.bead_id IS NOT NULL)             AS has_existing_parent_dep,
		       (existing_tb.bead_id IS NOT NULL)             AS has_existing_trigger_dep
		  FROM beads b
		  LEFT JOIN beads slack_tb
		    ON slack_tb.id = NULLIF(b.fields->>'slack_thread_bead_id', '')
		   AND slack_tb.deleted_at IS NULL
		  LEFT JOIN beads trig_tb
		    ON trig_tb.id = NULLIF(b.fields->>'trigger_id', '')
		   AND trig_tb.deleted_at IS NULL
		  LEFT JOIN LATERAL (
		      SELECT 1 AS bead_id
		        FROM deps d
		       WHERE d.bead_id = b.id
		         AND d.type = 'parent-child'
		       LIMIT 1
		  ) existing_pc ON true
		  LEFT JOIN LATERAL (
		      SELECT 1 AS bead_id
		        FROM deps d
		       WHERE d.bead_id = b.id
		         AND d.type = 'triggered-by'
		       LIMIT 1
		  ) existing_tb ON true
		 WHERE b.deleted_at IS NULL
		   AND b.created_at >= $1
		   AND b.id > $2
		   AND (
		       NULLIF(b.fields->>'slack_thread_bead_id', '') IS NOT NULL
		    OR NULLIF(b.fields->>'trigger_id', '')           IS NOT NULL
		   )
		 ORDER BY b.id
		 LIMIT $3`
	rows, err := db.QueryContext(ctx, q, cutoff, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []triggeredByCandidate
	for rows.Next() {
		var c triggeredByCandidate
		if err := rows.Scan(
			&c.id,
			&c.slackThreadBeadID,
			&c.triggerID,
			&c.slackTargetExists,
			&c.triggerTargetExists,
			&c.hasExistingParentDep,
			&c.hasExistingTriggerDep,
		); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// triggeredByInsert is one resolved (bead, target, signal) tuple to
// insert. signal is the field name that carried the candidate ID; it
// drives the EdgesBySignal tally and is recorded in the deps row
// metadata for audit.
type triggeredByInsert struct {
	beadID   string
	targetID string
	signal   string
}

// applyTriggeredByPage classifies a page of candidates, accumulates skip
// tallies, and (outside DryRun) inserts the resolved triggered-by deps
// inside one transaction. Each candidate is classified in this order:
//
//  1. Existing parent-child or triggered-by dep → SkippedExistingDep.
//     This is the conservative regression guard the task acceptance
//     criteria call out — never modify already-parented beads.
//  2. Resolve candidates: each signal field is honored only if (a) the
//     field is set, (b) it is not a self-reference, and (c) the target
//     bead exists and is not soft-deleted. Each signal contributes at
//     most one candidate ID; the per-skip tallies record why a signal
//     was dropped (SkippedSelfReference, SkippedTargetMissing).
//  3. Collapse to a single unambiguous target: if two surviving signals
//     point at different beads, SkippedAmbiguous; if both point at the
//     same bead, prefer the more specific signal name in EdgesBySignal
//     but it remains one write.
//  4. If no signals survived classification, SkippedNoSignal.
//
// Per-skip tallies are mutually exclusive at the bead level — one bead
// contributes to exactly one skip bucket or to the EdgesAdded path.
func applyTriggeredByPage(
	ctx context.Context,
	db *sql.DB,
	page []triggeredByCandidate,
	dryRun bool,
	stats *store.BackfillTriggeredByDepsStats,
) error {
	var inserts []triggeredByInsert

	for _, c := range page {
		if c.hasExistingTriggerDep || c.hasExistingParentDep {
			stats.SkippedExistingDep++
			continue
		}

		ins, classification := classifyTriggeredByCandidate(c)
		switch classification {
		case classifyOK:
			inserts = append(inserts, ins)
		case classifySelfReference:
			stats.SkippedSelfReference++
		case classifyTargetMissing:
			stats.SkippedTargetMissing++
		case classifyAmbiguous:
			stats.SkippedAmbiguous++
		case classifyNoSignal:
			stats.SkippedNoSignal++
		}
	}

	if dryRun {
		for _, ins := range inserts {
			stats.EdgesAdded++
			stats.EdgesBySignal[ins.signal]++
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
		// identically to one written through the normal API path.
		// created_by names the admin driver; metadata records which
		// recovery signal won so the audit trail is searchable.
		metadata := fmt.Appendf(nil, `{"signal":%q}`, ins.signal)
		res, err := tx.ExecContext(ctx, `
			INSERT INTO deps (bead_id, depends_on_id, type, created_at, created_by, metadata, fleet)
			VALUES (
			    $1, $2, $3, $4, $5, $6,
			    COALESCE((SELECT fleet FROM beads WHERE id = $1), 'default')
			)
			ON CONFLICT (bead_id, depends_on_id, type) DO NOTHING`,
			ins.beadID,
			ins.targetID,
			"triggered-by",
			now,
			"kd admin backfill-triggered-by-deps",
			metadata,
		)
		if err != nil {
			// Foreign-key races (target soft-deleted between scan and
			// insert) should not abort the whole batch — log a skip and
			// continue. Reuses the parent-child backfill's isFKViolation
			// classifier.
			if isFKViolation(err) {
				stats.SkippedTargetMissing++
				continue
			}
			return fmt.Errorf("insert triggered-by %s -> %s: %w", ins.beadID, ins.targetID, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// Lost the race with another writer — count as existing-dep.
			stats.SkippedExistingDep++
			continue
		}
		stats.EdgesAdded++
		stats.EdgesBySignal[ins.signal]++
	}
	return tx.Commit()
}

// classifyTriggeredByCandidate decides the fate of one candidate row
// based on its signal fields. Pure function over the candidate so the
// match heuristics can be unit-tested without a database. Returns the
// resolved insert (only meaningful for classifyOK) and the
// classification outcome.
//
// The order here defines precedence — two surviving signals must agree
// for an OK result; disagreement is always ambiguous. When multiple
// signals point at the same target, the signal name carried into the
// EdgesBySignal tally is the first one in {slack_thread_bead_id,
// trigger_id} order — stable so audit metadata is reproducible.
func classifyTriggeredByCandidate(c triggeredByCandidate) (triggeredByInsert, triggeredByClassification) {
	type candidate struct {
		signal string
		target string
		exists bool
	}
	var live []candidate

	check := func(signal string, raw sql.NullString, exists bool) {
		if !raw.Valid || raw.String == "" {
			return
		}
		c := candidate{signal: signal, target: raw.String, exists: exists}
		if raw.String == "" {
			return
		}
		// self-reference check fires before existence — a candidate that
		// would target the bead itself is dropped even if the target
		// exists (it always does — it's the candidate bead).
		// Use the parent candidate ID via closure below.
		live = append(live, c)
	}

	check("slack_thread_bead_id", c.slackThreadBeadID, c.slackTargetExists)
	check("trigger_id", c.triggerID, c.triggerTargetExists)

	// Annotate self / missing per candidate.
	var resolved []candidate
	sawSelf, sawMissing := false, false
	for _, cand := range live {
		if cand.target == c.id {
			sawSelf = true
			continue
		}
		if !cand.exists {
			sawMissing = true
			continue
		}
		resolved = append(resolved, cand)
	}

	if len(resolved) == 0 {
		// No surviving candidate — explain via priority: self > missing > no-signal.
		// (A self-reference is a clearer mis-tag than just-missing — surface it.)
		switch {
		case sawSelf:
			return triggeredByInsert{}, classifySelfReference
		case sawMissing:
			return triggeredByInsert{}, classifyTargetMissing
		default:
			return triggeredByInsert{}, classifyNoSignal
		}
	}

	// Collapse to a single target.
	target := resolved[0].target
	for _, r := range resolved[1:] {
		if r.target != target {
			return triggeredByInsert{}, classifyAmbiguous
		}
	}
	return triggeredByInsert{beadID: c.id, targetID: target, signal: resolved[0].signal}, classifyOK
}

// triggeredByClassification enumerates the outcomes of
// classifyTriggeredByCandidate. Kept as an int enum (rather than a
// string) so callers update the right stats counter via a switch with
// the compiler's exhaustiveness check on the table at the call site.
type triggeredByClassification int

const (
	classifyOK triggeredByClassification = iota
	classifyNoSignal
	classifySelfReference
	classifyTargetMissing
	classifyAmbiguous
)
