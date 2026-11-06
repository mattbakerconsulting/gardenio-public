package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gardenio/kbeads/internal/store"
)

// queryBackfillOperatorSlugs implements `kd admin backfill-operator-slugs`
// (ka-0000001149). It recomputes readable slugs for operators that are still
// on the opaque `op-<id>` form but now carry a real (non-sentinel) email —
// the human survivors of the operator-cluster merge cleanup (ka-0000006421),
// whose oidc / resync-harvested email lets us derive a memorable
// `matthew.baker`-style slug.
//
// A candidate is an operators row whose CURRENT slug is exactly `op-<id>`
// (the opaque fallback migration 028 assigned when no readable slug could be
// derived) AND whose email is a real, non-sentinel address. The candidate's
// readable slug comes from readableSlugFromEmail — kind-agnostic, so a
// slack / gitlab identity whose email was harvested by the resync jobs is
// slugged too. Rows whose email is still the synthetic sentinel domain
// (system:*, fleet:*, un-resynced slack) fail the derivation and are left on
// op-<id>: exactly the machine / sentinel rows the task says to leave alone.
//
// The rename preserves the old op-<id> slug as a RETIRED operator_slug_aliases
// row so existing /u/op-NNN external links keep resolving to the same
// operator. It also seeds the new readable slug as the canonical
// (retired_at IS NULL) alias.
//
// Collision safety: when the derived readable slug is already claimed by
// another operator (operators_slug_uniq / operator_slug_aliases PK), the
// candidate is left on op-<id> and counted under SkippedCollision rather than
// suffixed — the merge cleanup should have collapsed genuine duplicates, so a
// residual collision is a signal worth surfacing, not silently mangling.
//
// Per kbeads/CLAUDE.md §Migration policy — no eager in-migration backfill.
// The scan is keyset-paginated in batches of opts.BatchSize; each candidate's
// rename commits in its own transaction, so the backfill is resumable and
// re-running on already-renamed data is a no-op (the op-<id> predicate no
// longer matches).
func queryBackfillOperatorSlugs(ctx context.Context, db *sql.DB, opts store.BackfillOperatorSlugsOptions) (*store.BackfillOperatorSlugsStats, error) {
	start := time.Now()
	stats := &store.BackfillOperatorSlugsStats{DryRun: opts.DryRun}
	if err := opts.Validate(); err != nil {
		return nil, err
	}

	var err error
	stats.BeforeOpaque, err = countOpaqueOperatorSlugs(ctx, db)
	if err != nil {
		return nil, fmt.Errorf("count before: %w", err)
	}

	if err := backfillOperatorSlugsScan(ctx, db, opts, stats); err != nil {
		return stats, err
	}

	stats.AfterOpaque, err = countOpaqueOperatorSlugs(ctx, db)
	if err != nil {
		return stats, fmt.Errorf("count after: %w", err)
	}
	stats.DurationMs = time.Since(start).Milliseconds()
	return stats, nil
}

// countOpaqueOperatorSlugs counts operators whose current slug is the opaque
// `op-<id>` form (slug = 'op-' || id).
func countOpaqueOperatorSlugs(ctx context.Context, db *sql.DB) (int64, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM operators
		 WHERE slug = 'op-' || id`).Scan(&n)
	return n, err
}

// operatorSlugCandidate is one op-<id> operators row in the backfill scan.
type operatorSlugCandidate struct {
	id      int64
	oldSlug string
	email   string
}

// backfillOperatorSlugsScan pages through op-<id> operators in batches keyed
// by id. For each candidate it derives a readable slug from the email; when
// derivable (and — unless DryRun — free of a live collision) it renames the
// operator in its own transaction, preserving the old op-<id> slug as a
// retired alias.
func backfillOperatorSlugsScan(ctx context.Context, db *sql.DB, opts store.BackfillOperatorSlugsOptions, stats *store.BackfillOperatorSlugsStats) error {
	lastID := int64(0)
	for {
		batch, err := fetchOperatorSlugBatch(ctx, db, lastID, opts.BatchSize)
		if err != nil {
			return fmt.Errorf("fetch batch: %w", err)
		}
		if len(batch) == 0 {
			return nil
		}
		lastID = batch[len(batch)-1].id

		for _, c := range batch {
			newSlug, ok := readableSlugFromEmail(c.email)
			if !ok {
				// Sentinel-email machine / un-resynced row — no readable
				// slug derivable; leave it on op-<id>.
				stats.Machine++
				continue
			}
			if opts.DryRun {
				// Report the would-be rename, flagging a live collision the
				// same way the real path would skip it.
				taken, err := slugTaken(ctx, db, newSlug, c.id)
				if err != nil {
					return fmt.Errorf("dry-run collision check %q: %w", newSlug, err)
				}
				if taken {
					stats.SkippedCollision++
				} else {
					stats.Renamed++
				}
				continue
			}
			renamed, err := renameOperatorSlug(ctx, db, c, newSlug)
			if err != nil {
				return fmt.Errorf("rename operator %d -> %q: %w", c.id, newSlug, err)
			}
			if renamed {
				stats.Renamed++
			} else {
				stats.SkippedCollision++
			}
		}

		if len(batch) < opts.BatchSize {
			return nil
		}
	}
}

func fetchOperatorSlugBatch(ctx context.Context, db *sql.DB, afterID int64, limit int) ([]operatorSlugCandidate, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT id, slug, email
		  FROM operators
		 WHERE slug = 'op-' || id
		   AND id > $1
		 ORDER BY id
		 LIMIT $2`,
		afterID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []operatorSlugCandidate
	for rows.Next() {
		var c operatorSlugCandidate
		if err := rows.Scan(&c.id, &c.oldSlug, &c.email); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// slugTaken reports whether slug is already claimed by an operator other than
// selfID — either as a live operators.slug (operators_slug_uniq) or as an
// existing operator_slug_aliases row. Used by the dry-run path to mirror the
// real rename's collision skip without writing.
func slugTaken(ctx context.Context, db *sql.DB, slug string, selfID int64) (bool, error) {
	var n int64
	err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM operator_slug_aliases
		 WHERE slug = $1 AND operator_id <> $2`,
		slug, selfID,
	).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// renameOperatorSlug repoints one op-<id> operator to its readable slug in a
// single transaction. It returns (false, nil) — a benign skip — when the
// readable slug is already claimed by another operator (unique-violation on
// operators_slug_uniq or the alias PK), leaving the candidate on op-<id>.
//
// The transaction:
//  1. UPDATE operators SET slug = newSlug WHERE id = $id AND slug = oldSlug
//     (the oldSlug guard makes a re-run / concurrent rename a no-op);
//  2. INSERT the new readable slug as the canonical alias (retired_at NULL);
//  3. mark the old op-<id> alias RETIRED (retired_at = NOW()) so /u/op-NNN
//     still resolves to this operator as a retired alias.
func renameOperatorSlug(ctx context.Context, db *sql.DB, c operatorSlugCandidate, newSlug string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // best-effort rollback on failure path; no-op after a successful Commit.

	res, err := tx.ExecContext(ctx, `
		UPDATE operators
		   SET slug = $2, updated_at = NOW()
		 WHERE id = $1
		   AND slug = $3`,
		c.id, newSlug, c.oldSlug,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return false, nil
		}
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		// Already renamed by a prior run / concurrent writer — nothing to do.
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO operator_slug_aliases (slug, operator_id, retired_at)
		VALUES ($1, $2, NULL)
		ON CONFLICT (slug) DO NOTHING`,
		newSlug, c.id,
	); err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE operator_slug_aliases
		   SET retired_at = COALESCE(retired_at, NOW())
		 WHERE slug = $1 AND operator_id = $2`,
		c.oldSlug, c.id,
	); err != nil {
		return false, err
	}

	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
