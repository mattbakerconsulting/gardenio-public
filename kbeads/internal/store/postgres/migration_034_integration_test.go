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

// TestMigration034_FieldsTriggerIDIndex verifies migration 034:
//
//   - up creates the partial expression index on beads.fields->>'trigger_id';
//   - down drops it cleanly;
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of the gardener run-count lookup (fields->>'trigger_id' = $1)
//     chooses the new partial index, not a Seq Scan.
//
// Bug: ka-0000002374 (504 on /beads/gardeners — the gardeners_enrich.go
// deriveGardenerRunCount trigger_id count_only queries degraded to a Seq Scan
// per gardener, blowing the gateway timeout once the fleet grew). Mirrors
// migration 031 (schedule_id index) which solved the same regression for the
// Gardener detail page.
//
// Reuses the newTestMigrator / indexExists / explainInTx helpers defined in
// migration_025_integration_test.go (same package + integration build tag).
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the
// default `go test ./...` workflow stays sqlmock-only per kbeads/CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration034_FieldsTriggerIDIndex
func TestMigration034_FieldsTriggerIDIndex(t *testing.T) {
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
		// Leave the DB at HEAD so a subsequent test run on the same
		// instance does not start from a partially-migrated state.
		if _, err := m.Close(); err != nil {
			t.Logf("migrator close: %v", err)
		}
	})

	const indexName = "idx_beads_fields_trigger_id"

	// Migrate to EXACTLY version 34 (not true HEAD, which has moved well past
	// 34 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 034's, so pin 034 as the tip.
	if err := m.Migrate(34); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 34: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up to head", indexName)
	}

	// Step back one (undo 034) and verify it is gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 034): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 034", indexName)
	}

	// Re-apply 034 and verify it comes back (idempotent recovery).
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 034): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}

	// EXPLAIN-fixture check: the gardener run-count "all iterations of gardener X"
	// lookup (fields->>'trigger_id' = $1) must reach a plan node naming the
	// partial index. Without it the planner falls back to a Seq Scan — the
	// exact regression this migration prevents. enable_seqscan = off (scoped
	// to the tx) stops the empty-fixture row-count heuristic from masking a
	// missing/unusable index.
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE fields ? 'trigger_id'
		  AND fields->>'trigger_id' = $1
		LIMIT 1`,
		"gd-GardenerX")
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
