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

// TestMigration038_FieldsSlackCardTSIndex verifies migration 038:
//
//   - up creates the partial expression index on fields->>'slack_card_ts';
//   - down drops it cleanly;
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of the card_ts → wish-head lookup chooses the new index, not a
//     Seq Scan.
//
// Task: ka-0000000693 (1b-13 — stamp slack_card_ts on the wish HEAD so the
// done-react-on-card close gesture resolves card_ts → head with a direct
// indexed field-query, robust across bridge restart / cache eviction). Mirrors
// migration 037 (thread_id index) which solved the same per-lookup Seq-Scan
// regression for thread membership / roll-up queries.
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
//	  ./internal/store/postgres/ -run TestMigration038_FieldsSlackCardTSIndex
func TestMigration038_FieldsSlackCardTSIndex(t *testing.T) {
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

	const indexName = "idx_beads_fields_slack_card_ts"

	// Migrate to EXACTLY version 38 (not true HEAD, which has moved well past
	// 38 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 038's, so pin 038 as the tip.
	if err := m.Migrate(38); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 38: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up to head", indexName)
	}

	// Step back one (undo 038) and verify it is gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 038): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 038", indexName)
	}

	// Re-apply 038 and verify it comes back (idempotent recovery).
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 038): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}

	// EXPLAIN-fixture check: the card_ts → head lookup (single-key equality on
	// a sparse JSON key) must reach a plan node naming the new partial index.
	// Without it the planner falls back to a Seq Scan — the exact per-reaction
	// regression this migration prevents. enable_seqscan = off (scoped to the
	// tx) stops the empty-fixture row-count heuristic from masking a
	// missing/unusable index.
	//
	// The query MUST repeat the index's own predicate (`fields ? 'slack_card_ts'`)
	// as an explicit conjunct. Postgres's partial-index predicate-implication
	// proof (predtest.c) only matches clauses within the same operator
	// family; it cannot infer that a `->>` equality implies a `?`
	// key-existence test even though the former is impossible to satisfy
	// without the latter. Without this clause the planner correctly
	// considers the index UNUSABLE for the query (not a cost choice — a
	// Seq Scan even survives an enable_seqscan=off penalty when it is the
	// only plan Postgres can prove correct) — this bit the real daemon
	// dedup lookup in migration 041 (ka-0000002762), which is why that
	// query already carries the matching explicit clause.
	plan := explainInTx(t, db, `
		SELECT beads.id FROM beads
		WHERE beads.fields ? 'slack_card_ts'
		  AND beads.fields->>'slack_card_ts' = $1
		LIMIT 2`,
		"200.5")
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
