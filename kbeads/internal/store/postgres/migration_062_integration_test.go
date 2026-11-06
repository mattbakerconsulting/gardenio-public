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

// TestMigration062_FieldsSubjectIDIndex verifies migration 062: up creates
// the partial expression index on fields->>'subject_id', down drops it, up
// again restores it, and the Commons card-by-subject lookup plans onto it
// (gd-XOi5cqZP3I). Same harness as TestMigration031_FieldsScheduleIDIndex.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration062_FieldsSubjectIDIndex
func TestMigration062_FieldsSubjectIDIndex(t *testing.T) {
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

	const indexName = "idx_beads_fields_subject_id"
	// Pin 062 as the tip so Steps(-1)/Steps(1) run 062's own down and up.
	if err := m.Migrate(62); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 62: %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after up", indexName)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 062): %v", err)
	}
	if indexExists(t, db, indexName) {
		t.Fatalf("index %s still present after down 062", indexName)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 062): %v", err)
	}
	if !indexExists(t, db, indexName) {
		t.Fatalf("index %s missing after re-apply", indexName)
	}

	// The predicate addFieldFilters emits for field=kind=card and
	// field=subject_id=<id>, with the keys inlined as the custom plan sees them.
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE type = 'message'
		  AND fields ? 'kind' AND fields->>'kind' = 'card'
		  AND fields ? 'subject_id' AND fields->>'subject_id' = $1
		LIMIT 20`,
		"gd-SubjectX")
	if !strings.Contains(plan, indexName) {
		t.Errorf("EXPLAIN plan does not reference %s; got:\n%s", indexName, plan)
	}
}
