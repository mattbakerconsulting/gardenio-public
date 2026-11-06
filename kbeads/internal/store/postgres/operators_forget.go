package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gardenio/kbeads/internal/store"
)

// forgetAuditTables are the five tables that carry the operated_by_id audit FK.
// ForgetOperator counts an operator's total references across them to drive the
// large-audit-history guard; MergeOperators repoints the same set. Kept as one
// list so the two paths can never drift on which tables count as "audit
// history".
var forgetAuditTables = []string{"beads", "comments", "deps", "events", "mutation_history"}

// ForgetOperator implements store.Store — the soft-delete primitive behind
// `kd admin forget-operator` (ka-0000005150). See the interface doc for the
// contract; the guard + idempotency + slug-retire logic lives in forgetOperator
// so *PostgresStore and *txStore share one implementation.
func (s *PostgresStore) ForgetOperator(ctx context.Context, id int64, opts store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
	return forgetOperator(ctx, s.db, id, opts)
}

func (s *txStore) ForgetOperator(ctx context.Context, id int64, opts store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
	return forgetOperator(ctx, s.tx, id, opts)
}

func forgetOperator(ctx context.Context, db execer, id int64, opts store.ForgetOperatorOptions) (*store.ForgetOperatorResult, error) {
	if id <= 0 {
		return nil, &store.OperatorNotFoundError{OperatorID: id}
	}

	// Read the current state first: existence, whether it is already forgotten
	// (idempotent no-op), and whether it currently carries a slug (so the
	// result can report SlugCleared accurately).
	var (
		alreadyForgotten sql.NullTime
		hasSlug          bool
	)
	err := db.QueryRowContext(ctx, `
		SELECT forgotten_at, slug IS NOT NULL
		  FROM operators
		 WHERE id = $1`, id).Scan(&alreadyForgotten, &hasSlug)
	if err == sql.ErrNoRows {
		return nil, &store.OperatorNotFoundError{OperatorID: id}
	}
	if err != nil {
		return nil, fmt.Errorf("load operator %d: %w", id, err)
	}

	result := &store.ForgetOperatorResult{ID: id}

	// Already forgotten — idempotent no-op. Return the ORIGINAL forgotten_at
	// and do NOT re-stamp forgotten_by/reason or re-run the guard: the row is
	// already redacted, so a second call is harmless and must not overwrite the
	// first forget's attribution.
	if alreadyForgotten.Valid {
		result.AlreadyForgotten = true
		result.ForgottenAt = alreadyForgotten.Time
		return result, nil
	}

	// Count audit history across the FK tables for both the guard and the
	// returned stat. This runs even when Confirm is set so the result always
	// reports how much history the redacted operator owned.
	auditRows, err := countOperatorAuditRows(ctx, db, id)
	if err != nil {
		return nil, fmt.Errorf("count audit rows for operator %d: %w", id, err)
	}
	result.AuditRows = auditRows

	threshold := opts.AuditGuardThreshold
	if threshold <= 0 {
		threshold = store.DefaultForgetOperatorAuditGuard
	}
	if !opts.Confirm && auditRows > int64(threshold) {
		return nil, &store.OperatorForgetGuardError{
			OperatorID: id,
			AuditRows:  auditRows,
			Threshold:  threshold,
		}
	}

	// Stamp the soft-delete + audit attribution and null the slug in one
	// statement. The `forgotten_at IS NULL` guard makes this a no-op under a
	// concurrent forget (the row we already saw un-forgotten could race), in
	// which case RowsAffected is 0 and we fall back to reporting the winning
	// forget's timestamp.
	var forgottenAt sql.NullTime
	err = db.QueryRowContext(ctx, `
		UPDATE operators
		   SET forgotten_at = NOW(),
		       slug = NULL,
		       forgotten_by = NULLIF($2, ''),
		       forgotten_reason = NULLIF($3, ''),
		       updated_at = NOW()
		 WHERE id = $1
		   AND forgotten_at IS NULL
		RETURNING forgotten_at`, id, opts.Actor, opts.Reason).Scan(&forgottenAt)
	if err == sql.ErrNoRows {
		// Lost a concurrent forget race — report the row's current forgotten_at
		// as an idempotent no-op rather than erroring.
		result.AlreadyForgotten = true
		if ts, tsErr := operatorForgottenAt(ctx, db, id); tsErr == nil {
			result.ForgottenAt = ts
		}
		return result, nil
	}
	if err != nil {
		return nil, fmt.Errorf("forget operator %d: %w", id, err)
	}
	result.ForgottenAt = forgottenAt.Time
	result.SlugCleared = hasSlug

	// Retire the operator's live slug aliases so /u/{slug} keeps resolving to
	// the (now redacted) operator instead of 404ing, and so the alias no longer
	// reads as a live directory entry. Mirrors absorbOperatorSlug's retire step
	// on the merge path. Idempotent — already-retired aliases are untouched by
	// the retired_at IS NULL predicate.
	res, err := db.ExecContext(ctx, `
		UPDATE operator_slug_aliases
		   SET retired_at = NOW()
		 WHERE operator_id = $1
		   AND retired_at IS NULL`, id)
	if err != nil {
		return nil, fmt.Errorf("retire slug aliases for operator %d: %w", id, err)
	}
	if n, raErr := res.RowsAffected(); raErr == nil {
		result.SlugAliasesRetired = n
	}

	return result, nil
}

// countOperatorAuditRows sums operated_by_id references for operatorID across
// the five audit-FK tables in a single round trip. Each subquery is an index
// probe on the per-table operated_by_id index; the sum is the "how much history
// does this operator own" figure the forget guard checks.
func countOperatorAuditRows(ctx context.Context, db execer, operatorID int64) (int64, error) {
	var total int64
	// Build one `SELECT (a)+(b)+... ` so the whole count is a single statement.
	// The table list is a fixed in-code allowlist (forgetAuditTables), never
	// user input, so concatenating the names is injection-safe.
	q := "SELECT "
	for i, t := range forgetAuditTables {
		if i > 0 {
			q += " + "
		}
		q += "(SELECT count(*) FROM " + t + " WHERE operated_by_id = $1)"
	}
	if err := db.QueryRowContext(ctx, q, operatorID).Scan(&total); err != nil {
		return 0, err
	}
	return total, nil
}

// operatorForgottenAt reads the current forgotten_at for an operator, used on
// the concurrent-forget-race fallback to report the winning timestamp.
func operatorForgottenAt(ctx context.Context, db execer, id int64) (time.Time, error) {
	var ts sql.NullTime
	if err := db.QueryRowContext(ctx, `SELECT forgotten_at FROM operators WHERE id = $1`, id).Scan(&ts); err != nil {
		return time.Time{}, err
	}
	return ts.Time, nil
}
