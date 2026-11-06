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

// TestMigration063_FieldsTaskIDIndex verifies migration 063: up creates
// the partial expression index on fields->>'task_id', down drops it, up
// again restores it, and the agent-by-task lookup plans onto it
// (gd-qCPRRNtm4M). Same harness as TestMigration031_FieldsScheduleIDIndex.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration063_FieldsTaskIDIndex
func TestMigration063_FieldsTaskIDIndex(t *testing.T) {
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

	const indexName = "idx_beads_fields_task_id"
	// Pin 063 as the tip so Steps(-1)/Steps(1) run 063's own down and up.
	if err := m.Migrate(63); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 63: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up", indexName)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 063): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 063", indexName)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 063): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}

	// The predicate addFieldFilters emits for field=task_id=<id>, with the key
	// inlined as the custom plan sees it.
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE type = 'agent'
		  AND fields ? 'task_id' AND fields->>'task_id' = $1
		LIMIT 50`,
		"gd-TaskX")
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
