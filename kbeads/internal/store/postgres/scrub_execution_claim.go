package postgres

import "context"

// ClaimScrubExecution and ReleaseScrubExecutionClaim implement the
// scrub-request execution idempotency guard (gd-P6uCMzQqU9), mirroring
// ClaimEscalationReplay / ReleaseEscalationReplayClaim
// (escalation_replay_claim.go) exactly. The only prior guard against a
// double execution was the in-Go `parsed.ExecutedAt != ""` check in
// executeScrubRequest, which covers a single sequential call but not two
// concurrent approve requests (double-click, client retry, or two requests
// landing on two different daemon replicas) racing the same fork-mode
// scrub-request — both could observe executed_at=="" and both proceed to
// call store.ScrubTaint, creating two successor beads (and, for
// purge/strip, double-cascading).
//
// The claim lives in the bead's existing `fields` JSONB as
// `scrub_execution_claimed` (no new table/column) and is claimed with a
// single guarded UPDATE: the WHERE clause requires the field not already be
// "true", so the claim and the check are one atomic statement with no
// read-then-write race, regardless of which replica or how many concurrent
// callers race it.

// queryClaimScrubExecution performs the guarded UPDATE. Returns
// claimed=true only when this call's UPDATE matched a row, i.e. this caller
// is the one that flipped scrub_execution_claimed from unset/false to true.
func queryClaimScrubExecution(ctx context.Context, db executor, beadID string) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE beads
		   SET fields = jsonb_set(COALESCE(fields, '{}'::jsonb), '{scrub_execution_claimed}', 'true'::jsonb, true),
		       updated_at = NOW()
		 WHERE id = $1
		   AND deleted_at IS NULL
		   AND COALESCE(fields ->> 'scrub_execution_claimed', 'false') <> 'true'`,
		beadID,
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n == 1, nil
}

// queryReleaseScrubExecutionClaim clears the claim unconditionally
// (idempotent: a no-op when already clear or the bead is missing/deleted).
func queryReleaseScrubExecutionClaim(ctx context.Context, db executor, beadID string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE beads
		   SET fields = jsonb_set(COALESCE(fields, '{}'::jsonb), '{scrub_execution_claimed}', 'false'::jsonb, true),
		       updated_at = NOW()
		 WHERE id = $1
		   AND deleted_at IS NULL`,
		beadID,
	)
	return err
}

func (s *PostgresStore) ClaimScrubExecution(ctx context.Context, beadID string) (bool, error) {
	return queryClaimScrubExecution(ctx, s.db, beadID)
}

func (s *PostgresStore) ReleaseScrubExecutionClaim(ctx context.Context, beadID string) error {
	return queryReleaseScrubExecutionClaim(ctx, s.db, beadID)
}

func (s *txStore) ClaimScrubExecution(ctx context.Context, beadID string) (bool, error) {
	return queryClaimScrubExecution(ctx, s.tx, beadID)
}

func (s *txStore) ReleaseScrubExecutionClaim(ctx context.Context, beadID string) error {
	return queryReleaseScrubExecutionClaim(ctx, s.tx, beadID)
}
