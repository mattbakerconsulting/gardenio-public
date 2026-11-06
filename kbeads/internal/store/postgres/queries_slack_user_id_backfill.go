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

// queryBackfillSlackUserID implements `kd admin backfill-slack-user-id`
// (ka-0000001582). It backfills the missing `fields.slack_user_id` on
// thread-derived legacy beads so the Pass-1 A heuristic in
// backfill-operated-by-legacy can fire (see investigation ka-0000000959).
//
// A "thread-derived candidate" is a non-deleted bead whose created_by begins
// with "thread-" (an agent spawned from a Slack thread, or a child it wrote)
// AND whose fields lack a non-empty slack_user_id. The originating thread's
// user id is not stored on these beads, but it IS stored on the root "Slack
// thread:" bead the slack-bridge creates at thread-open time (fields
// slack_user_id + slack_thread_ts). Each candidate references that root two
// ways, tried in order:
//
//	Strategy 1 (fields.thread_id): the candidate carries a `thread_id` field
//	  holding the root bead-id — a direct FK. Look up that bead's
//	  fields.slack_user_id.
//	Strategy 2 (created_by ts): created_by = "thread-<sanitized-ts>" where
//	  sanitized-ts is the Slack thread_ts with '.' replaced by '-'
//	  (bridge sanitizeTS). Reconstruct the ts and find any slack-bridge root
//	  bead whose fields.slack_thread_ts matches; all roots for a ts agree on
//	  the opener, so any match supplies the id.
//
// No Slack API call is made — this is a pure intra-DB join, which also makes
// the backfill idempotent and safe to re-run.
//
// Per kbeads/CLAUDE.md §Migration policy — no eager full-table update. The
// scan is keyset-paginated in batches of opts.BatchSize; each batch's writes
// commit in their own transaction. Every write records one mutation_history
// row (field='slack_user_id', mutation_type='backfill-slack-user-id').
func queryBackfillSlackUserID(ctx context.Context, db *sql.DB, opts store.BackfillSlackUserIDOptions) (*store.BackfillSlackUserIDStats, error) {
	start := time.Now()
	stats := &store.BackfillSlackUserIDStats{DryRun: opts.DryRun}
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	// Before: candidate beads still missing slack_user_id.
	var err error
	stats.BeforeMissing, err = countSlackUserIDCandidates(ctx, db, opts.Statuses)
	if err != nil {
		return nil, fmt.Errorf("count before: %w", err)
	}

	if err := backfillSlackUserIDScan(ctx, db, opts, stats); err != nil {
		return stats, err
	}

	stats.AfterMissing, err = countSlackUserIDCandidates(ctx, db, opts.Statuses)
	if err != nil {
		return stats, fmt.Errorf("count after: %w", err)
	}
	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// countSlackUserIDCandidates counts non-deleted thread-derived beads in the
// given statuses that lack a non-empty fields.slack_user_id.
func countSlackUserIDCandidates(ctx context.Context, db *sql.DB, statuses []string) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM beads
		 WHERE created_by LIKE 'thread-%'
		   AND status = ANY($1::text[])
		   AND deleted_at IS NULL
		   AND COALESCE(fields->>'slack_user_id', '') = ''`,
		pq.Array(statuses),
	).Scan(&n)
	return n, err
}

// slackUserIDCandidate is one bead in the backfill scan.
type slackUserIDCandidate struct {
	id        string
	createdBy string
	fields    json.RawMessage
}

// backfillSlackUserIDScan pages through candidate beads in batches. For each
// batch it resolves the originating slack_user_id per row in Go (via the two
// strategies), then — unless DryRun — applies one UPDATE per (slack_user_id)
// group plus a mutation_history row per affected bead, all in one transaction.
func backfillSlackUserIDScan(ctx context.Context, db *sql.DB, opts store.BackfillSlackUserIDOptions, stats *store.BackfillSlackUserIDStats) error {
	lastID := ""
	for {
		batch, err := fetchSlackUserIDBatch(ctx, db, opts.Statuses, lastID, opts.BatchSize)
		if err != nil {
			return fmt.Errorf("fetch batch: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}
		lastID = batch[len(batch)-1].id

		// group[slackUserID] = candidate bead ids to stamp with it.
		groups := map[string][]string{}
		for _, c := range batch {
			uid, viaThreadID, err := resolveSlackUserID(ctx, db, c)
			if err != nil {
				return fmt.Errorf("resolve %s: %w", c.id, err)
			}
			if uid == "" {
				stats.Unresolved++
				continue
			}
			groups[uid] = append(groups[uid], c.id)
			if viaThreadID {
				stats.ResolvedViaThreadID++
			} else {
				stats.ResolvedViaCreatedBy++
			}
		}

		if opts.DryRun {
			for _, ids := range groups {
				stats.Updated += int64(len(ids))
			}
		} else if err := applySlackUserIDGroups(ctx, db, groups, stats); err != nil {
			return err
		}

		if len(batch) < opts.BatchSize {
			return nil
		}
	}
}

func fetchSlackUserIDBatch(ctx context.Context, db *sql.DB, statuses []string, afterID string, limit int) ([]slackUserIDCandidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, created_by, COALESCE(fields, '{}'::jsonb)
		  FROM beads
		 WHERE created_by LIKE 'thread-%'
		   AND status = ANY($1::text[])
		   AND deleted_at IS NULL
		   AND COALESCE(fields->>'slack_user_id', '') = ''
		   AND id > $2
		 ORDER BY id
		 LIMIT $3`,
		pq.Array(statuses), afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []slackUserIDCandidate
	for rows.Next() {
		var c slackUserIDCandidate
		if err := rows.Scan(&c.id, &c.createdBy, &c.fields); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// resolveSlackUserID returns the originating slack_user_id for a candidate and
// whether it was resolved via the fields.thread_id FK (true) or via the
// created_by timestamp (false). Returns "" when neither strategy resolves.
func resolveSlackUserID(ctx context.Context, db *sql.DB, c slackUserIDCandidate) (uid string, viaThreadID bool, err error) {
	fields := decodeFieldsLegacy(c.fields)

	// Strategy 1: fields.thread_id is a direct FK to the root bead.
	if threadID, _ := fields["thread_id"].(string); threadID != "" {
		uid, err := lookupBeadSlackUserID(ctx, db, threadID)
		if err != nil {
			return "", false, err
		}
		if uid != "" {
			return uid, true, nil
		}
	}

	// Strategy 2: parse the Slack thread_ts out of created_by and match a
	// slack-bridge root bead by fields.slack_thread_ts.
	if ts := threadTSFromCreatedBy(c.createdBy); ts != "" {
		uid, err := lookupRootSlackUserIDByThreadTS(ctx, db, ts)
		if err != nil {
			return "", false, err
		}
		if uid != "" {
			return uid, false, nil
		}
	}

	return "", false, nil
}

// threadTSFromCreatedBy reconstructs the Slack thread_ts from a
// created_by of the form "thread-<sanitized-ts>". The bridge's sanitizeTS
// replaces every '.' with '-'; a Slack ts has exactly one '.', so the final
// '-' is the decimal point. Returns "" for a created_by that is not a
// thread-<ts> or has no recoverable ts.
func threadTSFromCreatedBy(createdBy string) string {
	rest, ok := strings.CutPrefix(createdBy, "thread-")
	if !ok || rest == "" {
		return ""
	}
	i := strings.LastIndex(rest, "-")
	if i <= 0 || i == len(rest)-1 {
		return ""
	}
	return rest[:i] + "." + rest[i+1:]
}

// lookupBeadSlackUserID returns fields.slack_user_id for the supplied bead,
// or "" when the bead is absent or has no non-empty slack_user_id.
func lookupBeadSlackUserID(ctx context.Context, db *sql.DB, beadID string) (string, error) {
	var uid sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT fields->>'slack_user_id' FROM beads WHERE id = $1 AND deleted_at IS NULL`,
		beadID,
	).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !uid.Valid {
		return "", nil
	}
	return uid.String, nil
}

// lookupRootSlackUserIDByThreadTS finds a slack-bridge root bead whose
// fields.slack_thread_ts matches ts and returns its slack_user_id. All root
// beads for one thread agree on the opener, so the first non-empty match is
// authoritative. Returns "" when no root carries a non-empty slack_user_id.
func lookupRootSlackUserIDByThreadTS(ctx context.Context, db *sql.DB, ts string) (string, error) {
	var uid sql.NullString
	err := db.QueryRowContext(ctx, `
		SELECT fields->>'slack_user_id'
		  FROM beads
		 WHERE created_by = 'slack-bridge'
		   AND fields->>'slack_thread_ts' = $1
		   AND COALESCE(fields->>'slack_user_id', '') <> ''
		   AND deleted_at IS NULL
		 ORDER BY id
		 LIMIT 1`,
		ts,
	).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !uid.Valid {
		return "", nil
	}
	return uid.String, nil
}

// applySlackUserIDGroups writes every resolved (slack_user_id → bead ids)
// group for one batch in a single transaction, plus a mutation_history row
// per affected bead. The UPDATE re-checks the field is still empty so a
// concurrent write or re-run is a no-op for that row.
func applySlackUserIDGroups(ctx context.Context, db *sql.DB, groups map[string][]string, stats *store.BackfillSlackUserIDStats) error {
	if len(groups) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on failure path; no-op if Commit() succeeded.

	for uid, ids := range groups {
		res, err := tx.ExecContext(ctx, `
			UPDATE beads
			   SET fields = jsonb_set(
			           COALESCE(fields, '{}'::jsonb),
			           ARRAY['slack_user_id']::text[],
			           to_jsonb($1::text),
			           true
			       ),
			       updated_at = NOW()
			 WHERE id = ANY($2::text[])
			   AND COALESCE(fields->>'slack_user_id', '') = ''`,
			uid, pq.Array(ids),
		)
		if err != nil {
			return fmt.Errorf("update slack_user_id -> %s: %w", uid, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			continue
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type)
				VALUES ($1, $2, $3, $4, $5, $6)`,
				id,
				"kd admin backfill-slack-user-id",
				"slack_user_id",
				"", uid, "backfill-slack-user-id",
			); err != nil {
				return fmt.Errorf("mutation_history %s: %w", id, err)
			}
		}
		stats.Updated += n
	}
	return tx.Commit()
}
