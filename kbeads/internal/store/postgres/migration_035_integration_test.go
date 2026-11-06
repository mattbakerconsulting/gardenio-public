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

// TestMigration035_EventsCreatedAtIDIndex verifies migration 035:
//
//   - up creates the composite index on events(created_at, id);
//   - down drops it cleanly;
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of the global event-log scan (the /v1/events feed-seed query)
//     chooses the new index, not a Seq Scan + Sort.
//
// Task: ka-0000002634 (beads-viewer /feed first-paint slow — the durable
// event-log seed full-scanned + sorted the events table because nothing
// indexed created_at). Mirrors migration 032 (deps depends_on_id index) which
// solved the same Seq-Scan-on-a-hot-path regression for /v1/beads.
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
//	  ./internal/store/postgres/ -run TestMigration035_EventsCreatedAtIDIndex
func TestMigration035_EventsCreatedAtIDIndex(t *testing.T) {
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

	const indexName = "idx_events_created_at_id"

	// Migrate to EXACTLY version 35 (not true HEAD, which has moved well past
	// 35 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 035's, so pin 035 as the tip.
	if err := m.Migrate(35); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 35: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up to head", indexName)
	}

	// Step back one (undo 035) and verify it is gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 035): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 035", indexName)
	}

	// Re-apply 035 and verify it comes back (idempotent recovery).
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 035): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}

	// EXPLAIN-fixture check: the global event-log scan (created_at range +
	// id cursor, ordered by created_at, id) must reach a plan node naming the
	// new index. Without it the planner falls back to a Seq Scan + Sort — the
	// exact regression this migration prevents. enable_seqscan = off (scoped
	// to the tx) stops the empty-fixture row-count heuristic from masking a
	// missing/unusable index.
	plan := explainInTx(t, db, `
		SELECT events.id FROM events
		WHERE events.created_at >= $1 AND events.created_at <= $2
		  AND ($3 = 0 OR events.id > $3)
		ORDER BY events.created_at ASC, events.id ASC
		LIMIT $4`,
		"2026-01-01T00:00:00Z", "2026-12-31T23:59:59Z", int64(0), 100)
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
