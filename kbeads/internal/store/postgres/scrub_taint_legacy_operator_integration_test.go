//go:build integration

package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// TestScrubTaint_Purge_LegacyNullOperatorHistoryRow_RealPostgres pins the fix
// for gd-DWM1o3VZAC: migration 026 added `CHECK (operated_by_id IS NOT NULL)
// NOT VALID` to events and mutation_history, and its own comment claimed
// "only NEW inserts must satisfy the constraint" — but Postgres re-validates a
// NOT VALID CHECK against the NEW tuple on every UPDATE too, not only on
// INSERT. Before the fix, cascadeContent's bare `UPDATE events ... WHERE
// bead_id = $1` / `UPDATE mutation_history ... WHERE bead_id = $1` on a bead
// carrying a legacy pre-attribution row (operated_by_id NULL, deliberately
// left that way by ADR human-attribution-identity-v2 §4.5) raised a check
// violation, aborted the whole scrub transaction, and left the tainted
// content in place. sqlmock cannot catch this: it never enforces real
// Postgres constraints. This test manufactures that exact legacy-row shape
// against a real Postgres and asserts the purge succeeds and COALESCEs the
// NULL operated_by_id to the resolved scrub operator.
//
// Gated by //go:build integration and BEADS_TEST_DATABASE_URL so the default
// sqlmock-only `go test ./...` workflow is unaffected (see CLAUDE.md). Run:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestScrubTaint_Purge_LegacyNullOperatorHistoryRow_RealPostgres
func TestScrubTaint_Purge_LegacyNullOperatorHistoryRow_RealPostgres(t *testing.T) {
	dsn := requireTestDSN(t)

	store, err := New(dsn)
	if err != nil {
		t.Fatalf("connect to test postgres: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	suffix := genTestID(t)
	fleet := "test-scrub-legacy-op-" + suffix
	operatorID := anyOperatorID(t, dsn)
	now := time.Now().UTC()

	beadID := "gd-" + suffix
	bead := &model.Bead{
		ID: beadID, Kind: model.KindIssue, Type: model.TypeTask,
		Title: "legacy-op scrub target", Description: "secret prod content",
		Status:       model.StatusOpen,
		CreatedAt:    now,
		UpdatedAt:    now,
		Fleet:        fleet,
		OperatedByID: &operatorID,
	}
	if err := store.CreateBead(ctx, bead); err != nil {
		t.Fatalf("create bead: %v", err)
	}

	// Migration 026's NOT VALID CHECK is still enforced against the NEW tuple
	// on every INSERT (NOT VALID only exempts the migration's own one-time
	// backfill scan of pre-existing rows, not later writes) -- so inserting a
	// legacy NULL-operator row the normal way would itself raise 23514 before
	// ScrubTaint ever runs. Drop the constraint, insert the legacy rows this
	// test needs, then restore the exact same NOT VALID CHECK so the rest of
	// this connection's writes (including ScrubTaint's own cascade) are
	// exercised under the real constraint, matching prod.
	dropLegacyOperatorConstraints(ctx, t, store.db)
	t.Cleanup(func() { restoreLegacyOperatorConstraints(context.Background(), t, store.db) })

	// A legacy events row with NULL operated_by_id -- the exact shape ADR
	// human-attribution-identity-v2 §4.5 deliberately grandfathered and
	// migration 026's NOT VALID CHECK was supposed to leave alone.
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO events (topic, bead_id, actor, payload, fleet, operated_by_id)
		VALUES ('beads.test.legacy', $1, 'legacy-actor', '{"secret":"leak"}'::jsonb, $2, NULL)`,
		beadID, fleet,
	); err != nil {
		t.Fatalf("insert legacy events row: %v", err)
	}

	// A legacy mutation_history row, also NULL operated_by_id, on a field the
	// cascade's WHERE clause matches (title) so it is actually touched.
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO mutation_history (bead_id, actor, field, old_value, new_value, mutation_type, created_at, operated_by_id)
		VALUES ($1, 'legacy-actor', 'title', 'old secret', 'secret prod content', 'update', NOW(), NULL)`,
		beadID,
	); err != nil {
		t.Fatalf("insert legacy mutation_history row: %v", err)
	}

	restoreLegacyOperatorConstraints(ctx, t, store.db)

	taintName := "test-scrub-legacy-op-" + suffix
	var taintID int64
	if err := store.db.QueryRowContext(ctx,
		`INSERT INTO taints (name, owner_team) VALUES ($1, 'team-test') RETURNING id`,
		taintName,
	).Scan(&taintID); err != nil {
		t.Fatalf("insert taint: %v", err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO bead_taints (bead_id, taint_id, source, applied_by) VALUES ($1, $2, 'explicit', 'test')`,
		beadID, taintID,
	); err != nil {
		t.Fatalf("insert bead_taints: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		_, _ = store.db.ExecContext(cleanupCtx, `DELETE FROM taint_audit WHERE taint_id = $1`, taintID)
		_, _ = store.db.ExecContext(cleanupCtx, `DELETE FROM bead_taints WHERE taint_id = $1`, taintID)
		_, _ = store.db.ExecContext(cleanupCtx, `DELETE FROM taints WHERE id = $1`, taintID)
		_, _ = store.db.ExecContext(cleanupCtx, `DELETE FROM events WHERE bead_id = $1`, beadID)
		_, _ = store.db.ExecContext(cleanupCtx, `DELETE FROM mutation_history WHERE bead_id = $1`, beadID)
		_, _ = store.db.ExecContext(cleanupCtx, `UPDATE beads SET deleted_at = COALESCE(deleted_at, NOW()) WHERE id = $1`, beadID)
	})

	req := &model.ScrubTaintRequest{
		RequestID:       "test-req-" + suffix,
		TaintID:         taintID,
		Kind:            "purge",
		BeadIDs:         []string{beadID},
		Actor:           "oidc:tester@example.com",
		ActorOperatorID: operatorID,
	}
	result, err := store.ScrubTaint(ctx, req)
	if err != nil {
		t.Fatalf("ScrubTaint purge on a bead with legacy NULL-operator history rows: %v "+
			"(gd-DWM1o3VZAC regression: migration 026's NOT VALID CHECK on "+
			"events/mutation_history.operated_by_id still fires on UPDATE, not just INSERT)", err)
	}
	if len(result.Beads) != 1 || result.Beads[0].BeadID != beadID {
		t.Fatalf("unexpected result: %+v", result)
	}

	// The legacy NULL row must have been COALESCEd to the resolved operator,
	// not left NULL and not overwritten if it already had one.
	var eventsOperatorID sql.NullInt64
	if err := store.db.QueryRowContext(ctx,
		`SELECT operated_by_id FROM events WHERE bead_id = $1 AND topic = 'beads.test.legacy'`, beadID,
	).Scan(&eventsOperatorID); err != nil {
		t.Fatalf("select events.operated_by_id: %v", err)
	}
	if !eventsOperatorID.Valid || eventsOperatorID.Int64 != operatorID {
		t.Errorf("events.operated_by_id = %+v, want %d (COALESCE must stamp the resolved scrub operator)", eventsOperatorID, operatorID)
	}
	// The cascade must also have blanked the payload, per the existing purge contract.
	var payload string
	if err := store.db.QueryRowContext(ctx,
		`SELECT payload::text FROM events WHERE bead_id = $1 AND topic = 'beads.test.legacy'`, beadID,
	).Scan(&payload); err != nil {
		t.Fatalf("select events.payload: %v", err)
	}
	if payload != "{}" {
		t.Errorf("events.payload = %q, want blanked {}", payload)
	}

	var mutOperatorID sql.NullInt64
	var oldValue, newValue string
	if err := store.db.QueryRowContext(ctx,
		`SELECT operated_by_id, old_value, new_value FROM mutation_history WHERE bead_id = $1 AND field = 'title'`, beadID,
	).Scan(&mutOperatorID, &oldValue, &newValue); err != nil {
		t.Fatalf("select mutation_history row: %v", err)
	}
	if !mutOperatorID.Valid || mutOperatorID.Int64 != operatorID {
		t.Errorf("mutation_history.operated_by_id = %+v, want %d (COALESCE must stamp the resolved scrub operator)", mutOperatorID, operatorID)
	}
	if oldValue != "" || newValue != "" {
		t.Errorf("mutation_history old_value/new_value = %q/%q, want both blanked by the cascade", oldValue, newValue)
	}

	// The taint marker itself must be gone -- the cascade's final step.
	var taintCount int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM bead_taints WHERE bead_id = $1 AND taint_id = $2`, beadID, taintID,
	).Scan(&taintCount); err != nil {
		t.Fatalf("select bead_taints: %v", err)
	}
	if taintCount != 0 {
		t.Errorf("bead_taints marker still present after purge scrub")
	}
}

// dropLegacyOperatorConstraints drops migration 026's two NOT VALID CHECK
// constraints so a legacy NULL-operator row can be inserted the way real
// pre-attribution history rows already sit in prod. Postgres enforces a NOT
// VALID CHECK against the NEW tuple on every INSERT (not only on rows
// touched by the migration's own backfill scan), so inserting with the
// constraint live would 23514 before the test ever reaches ScrubTaint.
func dropLegacyOperatorConstraints(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `ALTER TABLE events DROP CONSTRAINT IF EXISTS events_operated_by_id_not_null`); err != nil {
		t.Fatalf("drop events_operated_by_id_not_null: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE mutation_history DROP CONSTRAINT IF EXISTS mutation_history_operated_by_id_not_null`); err != nil {
		t.Fatalf("drop mutation_history_operated_by_id_not_null: %v", err)
	}
}

// restoreLegacyOperatorConstraints re-adds the exact NOT VALID CHECK
// constraints migration 026 installs, mirroring its up migration. Idempotent
// (DROP IF EXISTS then ADD), so it is safe to call both eagerly -- right
// after the legacy-row setup, so ScrubTaint itself runs under the real
// constraint -- and again from t.Cleanup as a safety net if the test fails
// before reaching that point.
func restoreLegacyOperatorConstraints(ctx context.Context, t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `ALTER TABLE events DROP CONSTRAINT IF EXISTS events_operated_by_id_not_null`); err != nil {
		t.Errorf("restore: drop events_operated_by_id_not_null: %v", err)
		return
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE events ADD CONSTRAINT events_operated_by_id_not_null CHECK (operated_by_id IS NOT NULL) NOT VALID`); err != nil {
		t.Errorf("restore: add events_operated_by_id_not_null: %v", err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE mutation_history DROP CONSTRAINT IF EXISTS mutation_history_operated_by_id_not_null`); err != nil {
		t.Errorf("restore: drop mutation_history_operated_by_id_not_null: %v", err)
		return
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE mutation_history ADD CONSTRAINT mutation_history_operated_by_id_not_null CHECK (operated_by_id IS NOT NULL) NOT VALID`); err != nil {
		t.Errorf("restore: add mutation_history_operated_by_id_not_null: %v", err)
	}
}
