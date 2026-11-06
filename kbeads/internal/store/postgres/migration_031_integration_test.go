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

// TestMigration031_FieldsScheduleIDIndex verifies migration 031:
//
//   - up creates the partial expression index on beads.fields->>'schedule_id';
//   - down drops it cleanly;
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of the Gardener Page lookup query (fields->>'schedule_id' = $1)
//     chooses the new partial index, not a Seq Scan.
//
// Bead: ka-0000000412. Parent epic: ka-0000001579.
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
//	  ./internal/store/postgres/ -run TestMigration031_FieldsScheduleIDIndex
func TestMigration031_FieldsScheduleIDIndex(t *testing.T) {
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

	const indexName = "idx_beads_fields_schedule_id"

	// Migrate to EXACTLY version 31 (not true HEAD, which has moved well past
	// 31 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 031's, so pin 031 as the tip.
	if err := m.Migrate(31); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 31: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up to head", indexName)
	}

	// Step back one (undo 031) and verify it is gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 031): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 031", indexName)
	}

	// Re-apply 031 and verify it comes back (idempotent recovery).
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 031): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}

	// EXPLAIN-fixture check: the Gardener Page "all iterations of gardener X"
	// lookup (fields->>'schedule_id' = $1) must reach a plan node naming the
	// partial index. Without it the planner falls back to a Seq Scan — the
	// exact regression this migration prevents. enable_seqscan = off (scoped
	// to the tx) stops the empty-fixture row-count heuristic from masking a
	// missing/unusable index.
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE fields ? 'schedule_id'
		  AND fields->>'schedule_id' = $1
		LIMIT 1`,
		"gd-GardenerX")
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
