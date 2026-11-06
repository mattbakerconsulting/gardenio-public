package postgres

import "context"

// ClaimEscalationReplay and ReleaseEscalationReplayClaim implement the
// escalation-replay idempotency guard from the Phase 2 2-replica spike
// (gd-XxrObXvc8r). The only prior guard against a double replay was the
// in-Go `bead.Status == StatusOpen` check in handleApproveEscalation, which
// covers a single sequential call but not two concurrent approve requests
// (double-click, client retry, or two requests landing on two different
// daemon replicas) — both could observe StatusOpen and both proceed to
// replayEscalation, executing the approved operation (e.g. an agent spawn)
// twice.
//
// The claim lives in the bead's existing `fields` JSONB as
// `replay_in_progress` (no new table/column — the doc's stated constraint)
// and is claimed with a single guarded UPDATE: the WHERE clause requires the
// field not already be "true", so the claim and the check are one atomic
// statement with no read-then-write race, regardless of which replica or how
// many concurrent callers race it.

// queryClaimEscalationReplay performs the guarded UPDATE. Returns
// claimed=true only when this call's UPDATE matched a row, i.e. this caller
// is the one that flipped replay_in_progress from unset/false to true.
func queryClaimEscalationReplay(ctx context.Context, db executor, beadID string) (bool, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE beads
		   SET fields = jsonb_set(COALESCE(fields, '{}'::jsonb), '{replay_in_progress}', 'true'::jsonb, true),
		       updated_at = NOW()
		 WHERE id = $1
		   AND deleted_at IS NULL
		   AND COALESCE(fields ->> 'replay_in_progress', 'false') <> 'true'`,
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

// queryReleaseEscalationReplayClaim clears the claim unconditionally
// (idempotent: a no-op when already clear or the bead is missing/deleted).
func queryReleaseEscalationReplayClaim(ctx context.Context, db executor, beadID string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE beads
		   SET fields = jsonb_set(COALESCE(fields, '{}'::jsonb), '{replay_in_progress}', 'false'::jsonb, true),
		       updated_at = NOW()
		 WHERE id = $1
		   AND deleted_at IS NULL`,
		beadID,
	)
	return err
}

func (s *PostgresStore) ClaimEscalationReplay(ctx context.Context, beadID string) (bool, error) {
	return queryClaimEscalationReplay(ctx, s.db, beadID)
}

func (s *PostgresStore) ReleaseEscalationReplayClaim(ctx context.Context, beadID string) error {
	return queryReleaseEscalationReplayClaim(ctx, s.db, beadID)
}

func (s *txStore) ClaimEscalationReplay(ctx context.Context, beadID string) (bool, error) {
	return queryClaimEscalationReplay(ctx, s.tx, beadID)
}

func (s *txStore) ReleaseEscalationReplayClaim(ctx context.Context, beadID string) error {
	return queryReleaseEscalationReplayClaim(ctx, s.tx, beadID)
}
