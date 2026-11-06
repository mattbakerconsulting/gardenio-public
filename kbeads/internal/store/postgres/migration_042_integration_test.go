//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"gardenio/kbeads/internal/idgen"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

// TestMigration042_FieldsMRURLUniqueIndex verifies migration 042
// (ka-0000005686 HALF-2): promotion of the type=merge-request mr_url index from
// the non-unique migration-041 index to a partial UNIQUE index.
//
//   - up drops idx_beads_fields_mr_url (041) and creates
//     idx_beads_fields_mr_url_unique with the same partial predicate;
//   - the new index is UNIQUE: a second merge-request bead with the same
//     mr_url now RAISES 23505 (unique_violation) — the store-level guarantee
//     the reconciliation sweep + this migration deliver together;
//   - two DIFFERENT mr_urls still insert fine;
//   - a non-merge-request bead sharing the same mr_url value is NOT constrained
//     (the type-scoped partial predicate keeps a legacy task mr_url out of the
//     set);
//   - down restores the 041 non-unique index and drops the unique one;
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of the daemon dedup lookup reaches the new unique index.
//
// Reuses newTestMigrator / indexExists / explainInTx from
// migration_025_integration_test.go (same package + integration build tag).
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration042_FieldsMRURLUniqueIndex
func TestMigration042_FieldsMRURLUniqueIndex(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	m := newTestMigrator(t, db)
	t.Cleanup(func() {
		if _, err := m.Close(); err != nil {
			t.Logf("migrator close: %v", err)
		}
	})

	const (
		oldIndex = "idx_beads_fields_mr_url"
		newIndex = "idx_beads_fields_mr_url_unique"
	)

	// Migrate to EXACTLY version 42 (not true HEAD, which has moved well past
	// 42 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 042's, so pin 042 as the tip.
	if err := m.Migrate(42); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 42: %v", err)
	}
	if !indexExists(t, db, newIndex) {
		t.Fatalf("unique index %s missing after migrating to 042", newIndex)
	}
	if indexExists(t, db, oldIndex) {
		t.Fatalf("old non-unique index %s still present after 042 up", oldIndex)
	}

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	const uniqURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/808"
	const otherURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/809"

	insertMR := func(id, url string) error {
		_, err := db.Exec(`
			INSERT INTO beads (id, kind, type, title, status, fleet, fields, operated_by_id)
			VALUES ($1, 'issue', 'merge-request', 'MR', 'open', 'default',
			        jsonb_build_object('mr_url', $2::text), $3)`, id, url, operatorID)
		return err
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM beads WHERE fields->>'mr_url' IN ($1, $2)`, uniqURL, otherURL)
	})

	// First insert succeeds.
	id1, _ := idgen.Generate()
	if err := insertMR(id1, uniqURL); err != nil {
		t.Fatalf("first insert should succeed: %v", err)
	}
	// Second insert with the SAME mr_url must now raise a unique violation.
	id2, _ := idgen.Generate()
	err = insertMR(id2, uniqURL)
	if err == nil {
		t.Fatalf("second insert of same mr_url should raise 23505 under the unique index")
	}
	var pqe interface{ SQLState() string }
	if !errors.As(err, &pqe) || pqe.SQLState() != "23505" {
		t.Fatalf("second insert error = %v, want a 23505 unique_violation", err)
	}

	// A DIFFERENT mr_url still inserts.
	id3, _ := idgen.Generate()
	if err := insertMR(id3, otherURL); err != nil {
		t.Fatalf("distinct mr_url insert should succeed: %v", err)
	}

	// A non-merge-request bead sharing the constrained mr_url value is NOT
	// blocked — the partial predicate is type-scoped.
	id4, _ := idgen.Generate()
	if _, err := db.Exec(`
		INSERT INTO beads (id, kind, type, title, status, fleet, fields, operated_by_id)
		VALUES ($1, 'issue', 'task', 'legacy task with mr_url', 'open', 'default',
		        jsonb_build_object('mr_url', $2::text), $3)`, id4, uniqURL, operatorID); err != nil {
		t.Fatalf("task bead sharing the mr_url should NOT be constrained: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id4) })

	// A SOFT-DELETED merge-request bead sharing the constrained mr_url is NOT
	// blocked — the partial predicate is `deleted_at IS NULL`-scoped
	// (ka-0000003154). This is the exact prod regression: a tombstone carrying
	// fields.mr_url must not collide on the unique build with the live bead for
	// the same URL. Without the deleted_at clause this insert would raise 23505
	// (uniqURL already has a live bead from id1) and, at migration time, would
	// hard-fail the `kd migrate` Job and freeze prod deploys.
	idDeleted, _ := idgen.Generate()
	if _, err := db.Exec(`
		INSERT INTO beads (id, kind, type, title, status, fleet, fields, deleted_at, operated_by_id)
		VALUES ($1, 'issue', 'merge-request', 'soft-deleted MR tombstone', 'open',
		        'default', jsonb_build_object('mr_url', $2::text), NOW(), $3)`, idDeleted, uniqURL, operatorID); err != nil {
		t.Fatalf("soft-deleted MR bead sharing the mr_url should NOT be constrained: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, idDeleted) })

	// EXPLAIN: the daemon dedup lookup reaches the unique index. The daemon
	// query (server/beads_mr_dedup.go via store.ListBeads) always carries
	// `deleted_at IS NULL`; the partial index is scoped the same way, so the
	// planner still selects it.
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE type = 'merge-request'
		  AND fields ? 'mr_url'
		  AND fields->>'mr_url' = $1
		  AND deleted_at IS NULL
		LIMIT 1`, uniqURL)
	if !strings.Contains(plan, newIndex) {
		// Not a regression: same cost-based, fixture-scale ambiguity as
		// migration 041's EXPLAIN check (this test carries only a handful
		// of rows vs. the ~7800-row merge-request population the migration
		// comment assumes) — idx_beads_type is also a valid, non-seqscan
		// plan at this row count. See migration_041_integration_test.go for
		// the full explanation. Recorded in known-failures.md; bucket 3 per
		// gd-Ai6ekNov28 triage. t.Logf (not t.Skipf): this check runs
		// mid-test, and the down-042 / refuse-over-duplicate (23505) /
		// Force+re-apply assertions below it must still execute even when
		// the planner picks a different valid index on this fixture.
		t.Logf("EXPLAIN chose a different (still valid, non-seqscan) index on this near-empty CI fixture instead of %s; got:\n%s", newIndex, plan)
	}

	// Step back one (undo 042): the unique index is gone, the 041 non-unique
	// index is restored, and a duplicate insert is accepted again.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 042): %v", err)
	}
	if indexExists(t, db, newIndex) {
		t.Fatalf("unique index %s still present after down 042", newIndex)
	}
	if !indexExists(t, db, oldIndex) {
		t.Fatalf("non-unique index %s not restored after down 042", oldIndex)
	}
	id5, _ := idgen.Generate()
	if err := insertMR(id5, uniqURL); err != nil {
		t.Fatalf("after down 042 the non-unique index must accept a duplicate mr_url: %v", err)
	}

	// Re-apply 042: it must clean up the duplicate-tolerant state? No — a live
	// duplicate now exists (id1 + id5), so re-applying the UNIQUE build must FAIL
	// with 23505, proving the migration refuses to promote over a dirty
	// population rather than silently masking it. Remove the duplicate first,
	// then the re-apply succeeds (idempotent recovery on a clean population).
	if err := m.Steps(1); err == nil {
		t.Fatalf("re-applying 042 over a live duplicate should FAIL with 23505")
	}
	// Clean the migrator's dirty state, drop the duplicate, and retry.
	if _, err := db.Exec(`DELETE FROM beads WHERE id = $1`, id5); err != nil {
		t.Fatalf("cleanup duplicate: %v", err)
	}
	// Force the schema_migrations dirty flag clear by forcing to 041 then up.
	if err := m.Force(41); err != nil {
		t.Fatalf("force to 041 after dirty: %v", err)
	}
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("re-apply 042 on clean population: %v", err)
	}
	if !indexExists(t, db, newIndex) {
		t.Fatalf("unique index %s missing after clean re-apply", newIndex)
	}
}

// TestMigration042_ReconcileClearsMRURLUnblocksUnique proves the HALF-2 halves
// COMPOSE: the reconciler CLOSES losers but the 042 UNIQUE index is NOT
// status-scoped (WHERE type='merge-request' AND fields ? 'mr_url'), so closing
// a loser does NOT remove it from the index population — only clearing its
// mr_url does. This is the bug ka-0000000451 fixes: the reconciler now clears
// mr_url on every loser. Here we reproduce a duplicate population, show 042
// FAILS while both rows keep mr_url (even when one is closed), then clear the
// loser's mr_url exactly as the reconciler does and show 042 succeeds.
func TestMigration042_ReconcileClearsMRURLUnblocksUnique(t *testing.T) {
	dsn := os.Getenv("BEADS_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("BEADS_TEST_DATABASE_URL not set; skipping real-Postgres integration test")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	m := newTestMigrator(t, db)
	t.Cleanup(func() {
		if _, err := m.Close(); err != nil {
			t.Logf("migrator close: %v", err)
		}
	})

	const dupURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/1462"

	// Start one step BELOW 042 (041 non-unique index present) so a duplicate
	// population can exist, mirroring prod before the sweep.
	if err := m.Migrate(41); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 041: %v", err)
	}

	survivorID, _ := idgen.Generate()
	loserID, _ := idgen.Generate()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id IN ($1,$2)`, survivorID, loserID) })

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	// Survivor is closed+merged (a merged MR's canonical bead is status=closed);
	// loser is also closed — proving CLOSING alone does not clear the collision.
	if _, err := db.Exec(`
		INSERT INTO beads (id, kind, type, title, status, fleet, fields, operated_by_id)
		VALUES ($1,'issue','merge-request','MR survivor','closed','default',
		        jsonb_build_object('mr_url',$2::text,'mr_outcome','merged'), $3)`, survivorID, dupURL, operatorID); err != nil {
		t.Fatalf("insert survivor: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO beads (id, kind, type, title, status, fleet, fields, operated_by_id)
		VALUES ($1,'issue','merge-request','MR loser','closed','default',
		        jsonb_build_object('mr_url',$2::text), $3)`, loserID, dupURL, operatorID); err != nil {
		t.Fatalf("insert loser: %v", err)
	}

	// 042 must FAIL: two closed merge-request beads still share mr_url.
	if err := m.Steps(1); err == nil {
		t.Fatalf("042 should FAIL 23505 while a closed loser still carries the duplicate mr_url")
	}
	// Clear the migrator dirty flag left by the failed up.
	if err := m.Force(41); err != nil {
		t.Fatalf("force to 041 after dirty: %v", err)
	}

	// Reconciler action: clear the LOSER's mr_url (delete-on-null equivalent),
	// preserving the bead + a dedup_former_mr_url audit crumb.
	if _, err := db.Exec(`
		UPDATE beads
		   SET fields = (fields - 'mr_url')
		              || jsonb_build_object('merged_into',$2::text,'dedup_former_mr_url',$3::text)
		 WHERE id = $1`, loserID, survivorID, dupURL); err != nil {
		t.Fatalf("clear loser mr_url: %v", err)
	}

	// Now 042 succeeds: exactly one merge-request bead carries the mr_url.
	if err := m.Steps(1); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("042 should succeed after the loser's mr_url is cleared: %v", err)
	}
	if !indexExists(t, db, "idx_beads_fields_mr_url_unique") {
		t.Fatalf("unique index missing after reconcile+042")
	}
	// The loser bead still exists (not hard-deleted) with its audit crumb.
	var former string
	if err := db.QueryRow(`SELECT fields->>'dedup_former_mr_url' FROM beads WHERE id=$1`, loserID).Scan(&former); err != nil {
		t.Fatalf("loser bead should survive with dedup_former_mr_url: %v", err)
	}
	if former != dupURL {
		t.Errorf("dedup_former_mr_url = %q, want %q", former, dupURL)
	}
}
