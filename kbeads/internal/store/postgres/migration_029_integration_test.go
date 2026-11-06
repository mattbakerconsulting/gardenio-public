//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

// TestMigration029_IdempotencyKey verifies migration 029:
//
//   - up creates the partial unique index idx_beads_idempotency_key over
//     (fleet, fields->>'idempotency_key');
//   - down drops it cleanly;
//   - up + down + up round-trips without error (idempotent recovery);
//   - the index ENFORCES uniqueness — a second live bead carrying the same
//     (fleet, idempotency_key) is rejected with a 23505;
//   - per-fleet scope: the same key in a different fleet is allowed;
//   - soft-deleted beads are excluded — deleting a keyed bead frees the key;
//   - EXPLAIN of the server's lookup query reaches the index, not a Seq Scan.
//
// Bead: ka-0000002856.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the
// default `go test ./...` workflow stays sqlmock-only per kbeads/CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration029_IdempotencyKey
func TestMigration029_IdempotencyKey(t *testing.T) {
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

	// Migrate to EXACTLY version 29 (not true HEAD, which has moved well past
	// 29 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 029's, so pin 029 as the tip.
	if err := m.Migrate(29); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 29: %v", err)
	}

	const indexName = "idx_beads_idempotency_key"

	// After migrating to 029, the index must be present.
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up to head", indexName)
	}

	// Step back one (undo 029) and verify it is gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 029): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Errorf("index %s present after down", indexName)
	}

	// Re-apply 029 and verify the index comes back (idempotent recovery).
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 029): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Errorf("index %s missing after re-up", indexName)
	}

	// --- Uniqueness enforcement -------------------------------------------

	// Clean any rows a previous run of this test left behind.
	if _, err := db.Exec(`DELETE FROM beads WHERE id LIKE 'gd-idem-test-%'`); err != nil {
		t.Fatalf("cleanup pre-existing test rows: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM beads WHERE id LIKE 'gd-idem-test-%'`)
	})

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	insert := func(id, fleet, key string, deleted bool) error {
		del := "NULL"
		if deleted {
			del = "NOW()"
		}
		_, err := db.Exec(`
			INSERT INTO beads (id, kind, type, title, status, fleet, fields, deleted_at, operated_by_id)
			VALUES ($1, 'issue', 'task', 'idem test', 'open', $2,
			        jsonb_build_object('idempotency_key', $3::text), `+del+`, $4)`,
			id, fleet, key, operatorID)
		return err
	}

	// First insert with a key in fleet 'default' succeeds.
	if err := insert("gd-idem-test-1", "default", "dup-key", false); err != nil {
		t.Fatalf("first keyed insert: %v", err)
	}

	// Second LIVE insert with the same (fleet, key) must be rejected (23505).
	err = insert("gd-idem-test-2", "default", "dup-key", false)
	if err == nil {
		t.Errorf("duplicate (fleet, idempotency_key) insert was accepted; expected unique violation")
	} else if !strings.Contains(err.Error(), "idx_beads_idempotency_key") &&
		!strings.Contains(err.Error(), "unique") {
		t.Errorf("duplicate insert failed with unexpected error: %v", err)
	}

	// Same key in a DIFFERENT fleet is allowed (per-fleet scope).
	if err := insert("gd-idem-test-3", "other-fleet", "dup-key", false); err != nil {
		t.Errorf("same key in different fleet rejected: %v", err)
	}

	// A SOFT-DELETED bead with the key does not occupy the slot: inserting a
	// fresh live bead with a key that only a deleted bead carries succeeds.
	if err := insert("gd-idem-test-4", "default", "freed-key", true); err != nil {
		t.Fatalf("soft-deleted keyed insert: %v", err)
	}
	if err := insert("gd-idem-test-5", "default", "freed-key", false); err != nil {
		t.Errorf("live insert blocked by a soft-deleted keyed bead: %v", err)
	}

	// --- Plan check -------------------------------------------------------

	// The server's lookup (fields->>'idempotency_key' = $1, scoped by fleet)
	// must reach the partial index, not a Seq Scan.
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE fields ? 'idempotency_key'
		  AND fields->>'idempotency_key' = $1
		  AND deleted_at IS NULL
		LIMIT 1`,
		"dup-key")
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
