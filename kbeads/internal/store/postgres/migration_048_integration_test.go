//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	"github.com/lib/pq"
)

// TestMigration048_TaintEffects verifies migration 048 (docs/adr/taints.md
// §15.2, gd-9YQiNJVW6u): taints.effects lands as a NOT NULL TEXT[] defaulting
// to {gate}, and pre-existing rows get that default with no manual backfill.
//
//   - up adds the column, backfilling every existing row to {gate};
//   - re-running up (idempotent per kbeads migration policy) is a no-op;
//   - down drops the column and up recreates it (round-trip).
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the
// default `go test ./...` workflow stays sqlmock-only per kbeads/CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration048_TaintEffects
func TestMigration048_TaintEffects(t *testing.T) {
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

	// Migrate to EXACTLY version 48 (pinned, not HEAD, so Steps(-1)/Steps(1)
	// below keep exercising 048 once a later migration lands -- the rot that
	// broke 025..047, gd-Ai6ekNov28) against an empty database, matching the acceptance
	// criteria's "runs cleanly from empty".
	if err := m.Migrate(48); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("initial up to head: %v", err)
	}
	if !columnExists(t, db, "taints", "effects") {
		t.Fatalf("column taints.effects missing after up to head")
	}

	// A row inserted before this migration existed (i.e. without specifying
	// effects) must read back {gate} — the default makes every existing row
	// mean what it meant before this task.
	var taintID int64
	if err := db.QueryRow(`INSERT INTO taints (name, owner_team) VALUES ('test-taint-048', 'team-x') RETURNING id`).Scan(&taintID); err != nil {
		t.Fatalf("insert taint: %v", err)
	}
	var effects []string
	if err := db.QueryRow(`SELECT effects FROM taints WHERE id = $1`, taintID).Scan(pq.Array(&effects)); err != nil {
		t.Fatalf("select effects: %v", err)
	}
	if len(effects) != 1 || effects[0] != "gate" {
		t.Errorf("expected default effects {gate}, got %v", effects)
	}

	// Re-running the migration to 048 must be a clean no-op.
	if err := m.Migrate(48); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("second up (already at head): %v", err)
	}

	// Re-apply the 048 up.sql body directly against an already-migrated DB to
	// prove the DDL itself (ADD COLUMN IF NOT EXISTS) tolerates running twice,
	// independent of golang-migrate's version bookkeeping.
	upSQL, err := migrationsFS.ReadFile("migrations/048_taint_effects.up.sql")
	if err != nil {
		t.Fatalf("read 048 up.sql: %v", err)
	}
	if _, err := db.Exec(string(upSQL)); err != nil {
		t.Fatalf("re-apply 048 up.sql body: %v", err)
	}
	if !columnExists(t, db, "taints", "effects") {
		t.Fatalf("column taints.effects missing after re-applying 048 up.sql body")
	}

	// Round-trip: down then up recreates the column with its default intact.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 048): %v", err)
	}
	if columnExists(t, db, "taints", "effects") {
		t.Errorf("column taints.effects still present after down 048")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 048): %v", err)
	}
	if !columnExists(t, db, "taints", "effects") {
		t.Fatalf("column taints.effects missing after re-applying up 048")
	}
	effects = nil
	if err := db.QueryRow(`SELECT effects FROM taints WHERE id = $1`, taintID).Scan(pq.Array(&effects)); err != nil {
		t.Fatalf("select effects after round-trip: %v", err)
	}
	if len(effects) != 1 || effects[0] != "gate" {
		t.Errorf("expected default effects {gate} after round-trip, got %v", effects)
	}
}
