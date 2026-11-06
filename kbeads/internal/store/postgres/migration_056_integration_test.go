//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

// TestMigration056_BeadsShortTitle verifies migration 056 (plan
// 2026-10-14-short-titles, gd-V5nGseRS5f): beads.short_title is a nullable
// TEXT column, a bead written with one reads it back, down drops it and
// up + down + up round-trips.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration056
func TestMigration056_BeadsShortTitle(t *testing.T) {
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

	if err := m.Migrate(56); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 056: %v", err)
	}
	if !columnExists(t, db, "beads", "short_title") {
		t.Fatal("beads.short_title missing after up")
	}

	operatorID := anyOperatorID(t, dsn)
	id := "gd-mig056-" + idMust(t)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id) })
	if _, err := db.Exec(`
		INSERT INTO beads (id, kind, type, title, short_title, status, priority, created_at, updated_at, fleet, operated_by_id)
		VALUES ($1, 'issue', 'task', 'forward duration_ms through the gb hook relay', 'forward duration_ms through relay',
		        'open', 0, NOW(), NOW(), 'mig056', $2)`,
		id, operatorID); err != nil {
		t.Fatalf("insert: %v", err)
	}
	// Read the column itself, not beadColumns: at version 56 the columns later
	// migrations add (057's heraldry) do not exist yet.
	var shortTitle string
	if err := db.QueryRow(`SELECT short_title FROM beads WHERE id = $1`, id).Scan(&shortTitle); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if shortTitle != "forward duration_ms through relay" {
		t.Fatalf("short_title = %q, want %q", shortTitle, "forward duration_ms through relay")
	}

	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 056: %v", err)
	}
	if columnExists(t, db, "beads", "short_title") {
		t.Fatal("beads.short_title still present after down")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("re-up 056: %v", err)
	}
	if !columnExists(t, db, "beads", "short_title") {
		t.Fatal("beads.short_title missing after re-up")
	}
}
