//go:build integration

package postgres

import (
	"errors"
	"os"
	"strings"
	"testing"

	"database/sql"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

// TestMigration039_BeadsSearchTSV verifies migration 039 (ka-0000003832 /
// gd-search relevance Option 2):
//
//   - up adds the search_tsv column, the maintenance trigger, and the GIN index;
//   - the trigger populates search_tsv on INSERT and UPDATE (so new/updated rows
//     never need backfilling);
//   - down drops the index, trigger, function, and column cleanly;
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of the FTS predicate chooses the GIN index, not a Seq Scan.
//
// The "no eager backfill" half of the policy is proven by the backfill sub-test
// in queries_search_relevance_integration_test.go (legacy NULL rows filled via
// BackfillSearchTSVPage); this test focuses on the migration DDL + trigger.
//
// Reuses newTestMigrator / indexExists / explainInTx from
// migration_025_integration_test.go (same package + integration build tag).
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration039_BeadsSearchTSV
func TestMigration039_BeadsSearchTSV(t *testing.T) {
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
		// Restore the DB to HEAD so a sibling integration test running after
		// this one on the same instance does not start from version 39 (this
		// test intentionally pins the tip at 039 — see the Migrate(39) below).
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			t.Logf("restore to head: %v", err)
		}
		if _, err := m.Close(); err != nil {
			t.Logf("migrator close: %v", err)
		}
	})

	const indexName = "idx_beads_search_tsv"

	// Migrate to EXACTLY version 39 (not HEAD): later migrations (040+) sit on
	// top and their downs would run first, so this test pins 039 as the tip so
	// the Steps(-1)/Steps(1) round-trip below undoes/redoes 039 specifically.
	if err := m.Migrate(39); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 39: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up to head", indexName)
	}
	if !columnExists(t, db, "beads", "search_tsv") {
		t.Fatalf("column beads.search_tsv missing after up to head")
	}
	if !triggerExists(t, db, "beads", "beads_search_tsv_trigger") {
		t.Fatalf("trigger beads_search_tsv_trigger missing after up to head")
	}

	// Trigger check: a freshly-inserted row gets search_tsv populated, and an
	// UPDATE of the title refreshes it.
	t.Run("trigger_populates_on_insert_and_update", func(t *testing.T) {
		id := "gd-mig039-" + t.Name()
		t.Cleanup(func() {
			_, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id)
		})
		// beads.operated_by_id is NOT NULL (migration 022); resolve a real
		// operator FK from the DB rather than hardcoding an id that varies
		// per instance.
		operatorID := anyOperatorID(t, dsn)
		if _, err := db.Exec(`
			INSERT INTO beads (id, kind, type, title, description, status, priority, created_at, updated_at, fleet, operated_by_id)
			VALUES ($1, 'issue', 'bug', 'kafka rebalance storm', 'consumer group churn', 'open', 0, NOW(), NOW(), 'mig039', $2)`,
			id, operatorID); err != nil {
			t.Fatalf("insert: %v", err)
		}
		var matchesInsert bool
		if err := db.QueryRow(`
			SELECT search_tsv @@ websearch_to_tsquery('english', 'kafka') FROM beads WHERE id = $1`,
			id).Scan(&matchesInsert); err != nil {
			t.Fatalf("post-insert FTS check: %v", err)
		}
		if !matchesInsert {
			t.Errorf("trigger did not populate search_tsv on insert (no FTS match for 'kafka')")
		}

		// Update the title to a disjoint term; the tsvector must follow.
		if _, err := db.Exec(`UPDATE beads SET title = 'zookeeper quorum loss' WHERE id = $1`, id); err != nil {
			t.Fatalf("update: %v", err)
		}
		var matchesNew, matchesOld bool
		if err := db.QueryRow(`
			SELECT search_tsv @@ websearch_to_tsquery('english', 'zookeeper'),
			       search_tsv @@ websearch_to_tsquery('english', 'kafka')
			FROM beads WHERE id = $1`, id).Scan(&matchesNew, &matchesOld); err != nil {
			t.Fatalf("post-update FTS check: %v", err)
		}
		if !matchesNew {
			t.Errorf("trigger did not refresh search_tsv on update (no FTS match for 'zookeeper')")
		}
		if matchesOld {
			t.Errorf("stale tsvector after update (still matches old term 'kafka')")
		}
	})

	// EXPLAIN-fixture check: the FTS predicate must reach a plan node naming the
	// GIN index. enable_seqscan = off (scoped to the tx) stops the
	// empty-fixture row-count heuristic from masking a missing/unusable index.
	plan := explainInTx(t, db, `
		SELECT beads.id FROM beads
		WHERE beads.search_tsv @@ websearch_to_tsquery('english', $1)
		LIMIT 2`,
		"kafka")
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}

	// Step back one (undo 039) and verify the objects are gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 039): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 039", indexName)
	}
	if columnExists(t, db, "beads", "search_tsv") {
		t.Fatalf("column beads.search_tsv still present after down 039")
	}
	if triggerExists(t, db, "beads", "beads_search_tsv_trigger") {
		t.Fatalf("trigger still present after down 039")
	}

	// Re-apply 039 and verify it comes back (idempotent recovery).
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 039): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}
	if !columnExists(t, db, "beads", "search_tsv") {
		t.Fatalf("column beads.search_tsv missing after re-apply")
	}
}

// columnExists returns true iff the given column exists on the given table in
// the public schema.
func columnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	const q = `SELECT 1 FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2`
	var one int
	err := db.QueryRow(q, table, column).Scan(&one)
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows):
		return false
	default:
		t.Fatalf("information_schema.columns lookup for %s.%s: %v", table, column, err)
		return false
	}
}

// triggerExists returns true iff the named trigger exists on the given table.
func triggerExists(t *testing.T, db *sql.DB, table, trigger string) bool {
	t.Helper()
	const q = `SELECT 1 FROM information_schema.triggers
		WHERE trigger_schema = 'public' AND event_object_table = $1 AND trigger_name = $2
		LIMIT 1`
	var one int
	err := db.QueryRow(q, table, trigger).Scan(&one)
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows):
		return false
	default:
		t.Fatalf("information_schema.triggers lookup for %s.%s: %v", table, trigger, err)
		return false
	}
}
