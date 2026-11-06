//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	migratepg "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/lib/pq"
)

// TestMigration025_FieldsHotIndexes verifies migration 025:
//
//   - up creates the five expression indexes on beads.fields hot keys
//     (agent, agent_state, project, role, slack_user_id);
//   - down drops them cleanly;
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of a representative equality query (fields->>'agent' = $1)
//     chooses the new partial index, not a Seq Scan.
//
// Bead: ka-0000005120. Parent epic: ka-0000003045.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the
// default `go test ./...` workflow stays sqlmock-only per kbeads/CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration025_FieldsHotIndexes
func TestMigration025_FieldsHotIndexes(t *testing.T) {
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

	// Migrate to EXACTLY version 25 (not true HEAD, which has moved well past
	// 25 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 025's, so pin 025 as the tip.
	if err := m.Migrate(25); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 25: %v", err)
	}

	wantIndexes := []string{
		"idx_beads_fields_agent",
		"idx_beads_fields_agent_state",
		"idx_beads_fields_project",
		"idx_beads_fields_role",
		"idx_beads_fields_slack_user_id",
	}

	assertIndexes := func(t *testing.T, want bool) {
		t.Helper()
		for _, name := range wantIndexes {
			got := indexExists(t, db, name)
			if got != want {
				t.Errorf("index %s: present=%v, want=%v", name, got, want)
			}
		}
	}

	// After migrating to 025, all five indexes must be present.
	assertIndexes(t, true)

	// Step back one (undo 025) and verify they are gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 025): %v", err)
	}
	assertIndexes(t, false)

	// Re-apply 025 and verify the indexes come back.
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 025): %v", err)
	}
	assertIndexes(t, true)

	// EXPLAIN-fixture check: a representative equality query on
	// fields->>'agent' must reach a plan node that names our partial
	// index. Without the index the planner falls back to a Seq Scan,
	// which is exactly the regression this migration prevents.
	//
	// SET LOCAL enable_seqscan = off inside a tx prevents the row-count
	// heuristic from masking a missing index on a small fixture — with
	// seqscan disabled, if the planner still has to fall back the
	// index genuinely does not exist or is not usable for this filter.
	plan := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE fields ? 'agent'
		  AND fields->>'agent' = $1
		LIMIT 1`,
		"some-agent-name")
	if !strings.Contains(plan, "idx_beads_fields_agent") {
		t.Errorf("EXPLAIN plan does not reference idx_beads_fields_agent; got:\n%s", plan)
	}
}

// newTestMigrator builds a *migrate.Migrate against the embedded
// migrations FS and the live test DB. Mirrors runMigrations but exposes
// the *Migrate handle so the test can drive Up/Down/Steps directly.
func newTestMigrator(t *testing.T, db *sql.DB) *migrate.Migrate {
	t.Helper()
	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("iofs source: %v", err)
	}
	drv, err := migratepg.WithInstance(db, &migratepg.Config{})
	if err != nil {
		t.Fatalf("postgres driver: %v", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", drv)
	if err != nil {
		t.Fatalf("new migrator: %v", err)
	}
	return m
}

// indexExists returns true iff an index of the given name exists in the
// public schema of the connected database.
func indexExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	const q = `SELECT 1 FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`
	var one int
	err := db.QueryRow(q, name).Scan(&one)
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows):
		return false
	default:
		t.Fatalf("pg_indexes lookup for %s: %v", name, err)
		return false
	}
}

// explainInTx runs EXPLAIN inside a tx with enable_seqscan disabled so
// the plan reflects the index landscape, not the empty-fixture row-count
// heuristic. Returns the joined plan text. Pinning the SET to one tx
// keeps the session-level GUC unchanged for other tests.
func explainInTx(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec("SET LOCAL enable_seqscan = off"); err != nil {
		t.Fatalf("set local enable_seqscan: %v", err)
	}

	rows, err := tx.Query("EXPLAIN "+query, args...)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN: %v", err)
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return strings.Join(lines, "\n")
}
