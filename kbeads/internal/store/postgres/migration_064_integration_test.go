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

// TestMigration064_FieldsNextRespawnAtIndex verifies migration 064: up creates
// the partial expression index on fields->>'next_respawn_at', down drops it,
// up again restores it, and the wake scanner's presence filter plans onto it
// (gd-lCYkpAlNas). Same harness as TestMigration031_FieldsScheduleIDIndex.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration064_FieldsNextRespawnAtIndex
func TestMigration064_FieldsNextRespawnAtIndex(t *testing.T) {
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

	const indexName = "idx_beads_fields_next_respawn_at"
	// Pin 064 as the tip so Steps(-1)/Steps(1) run 064's own down and up.
	if err := m.Migrate(64); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 64: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up", indexName)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 064): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 064", indexName)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 064): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}

	// The predicate addFieldFilters emits for field=next_respawn_at=*, with
	// the key inlined as the custom plan sees it. The status/type terms the
	// scanner also sends are left out: on an empty fixture table their own
	// indexes cost the same, and this test pins only that the presence
	// predicate can reach the partial index (it has no indexable qual).
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE fields ? 'next_respawn_at' AND fields->>'next_respawn_at' != ''
		LIMIT 200`)
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
