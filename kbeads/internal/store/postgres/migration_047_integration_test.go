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

// TestMigration047_TaintRegistry verifies migration 047 (docs/adr/taints.md
// §3, gd-zor6fhJyP8): the taints, bead_taints, taint_grants, and taint_audit
// tables land exactly as the ADR specifies.
//
//   - up creates all four tables;
//   - re-running up (idempotent per kbeads migration policy) is a no-op,
//     not an error;
//   - bead_taints.bead_id cascades on beads delete;
//   - bead_taints' primary key is (bead_id, taint_id) — a duplicate insert
//     is rejected;
//   - taint_grants' primary key is (taint_id, principal_kind, principal) and
//     granted_by references operators(id);
//   - down drops all four tables and up recreates them (round-trip).
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the
// default `go test ./...` workflow stays sqlmock-only per kbeads/CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration047_TaintRegistry
func TestMigration047_TaintRegistry(t *testing.T) {
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

	// Migrate to EXACTLY version 47 (an empty database exercises the full
	// chain up to it, matching the acceptance criteria's "runs cleanly against
	// an empty database"). Not true HEAD: 048+ sit on top of 047, and under
	// Steps(-1)/Steps(1) below a later migration's down/up would run instead
	// of 047's (gd-Ai6ekNov28, same fix as the 025..044 tests).
	if err := m.Migrate(47); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 47: %v", err)
	}

	wantTables := []string{"taints", "bead_taints", "taint_grants", "taint_audit"}
	for _, tbl := range wantTables {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %s missing after up to head", tbl)
		}
	}
	if !indexExists(t, db, "idx_bead_taints_taint") {
		t.Errorf("index idx_bead_taints_taint missing after up to head")
	}

	// Re-running the migration to 047 must be a clean no-op — this is the
	// "a second time without error" half of the idempotency criterion.
	if err := m.Migrate(47); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("second migrate to 47 (already there): %v", err)
	}

	// Re-apply the 047 up.sql body directly against an already-migrated DB
	// to prove the DDL itself (IF NOT EXISTS everywhere) tolerates being
	// run twice, independent of golang-migrate's version bookkeeping.
	upSQL, err := migrationsFS.ReadFile("migrations/047_taint_registry.up.sql")
	if err != nil {
		t.Fatalf("read 047 up.sql: %v", err)
	}
	if _, err := db.Exec(string(upSQL)); err != nil {
		t.Fatalf("re-apply 047 up.sql body: %v", err)
	}
	for _, tbl := range wantTables {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %s missing after re-applying 047 up.sql body", tbl)
		}
	}

	var operatorID int64
	if err := db.QueryRow(`SELECT id FROM operators LIMIT 1`).Scan(&operatorID); err != nil {
		t.Fatalf("select seeded operator: %v", err)
	}

	// bead_id cascades on beads delete.
	const beadID = "test-taint-047-bead"
	if _, err := db.Exec(
		`INSERT INTO beads (id, title, kind, type, status, operated_by_id) VALUES ($1, 'x', 'issue', 'task', 'open', $2)`,
		beadID, operatorID,
	); err != nil {
		t.Fatalf("insert bead: %v", err)
	}
	var taintID int64
	if err := db.QueryRow(`INSERT INTO taints (name, owner_team) VALUES ('test-taint-047', 'team-x') RETURNING id`).Scan(&taintID); err != nil {
		t.Fatalf("insert taint: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO bead_taints (bead_id, taint_id, source, applied_by) VALUES ($1, $2, 'explicit', 'test')`, beadID, taintID); err != nil {
		t.Fatalf("insert bead_taint: %v", err)
	}

	// Composite PK (bead_id, taint_id) rejects a duplicate.
	if _, err := db.Exec(`INSERT INTO bead_taints (bead_id, taint_id, source, applied_by) VALUES ($1, $2, 'explicit', 'test')`, beadID, taintID); err == nil {
		t.Fatalf("duplicate (bead_id, taint_id) insert into bead_taints unexpectedly succeeded")
	}

	if _, err := db.Exec(`DELETE FROM beads WHERE id = $1`, beadID); err != nil {
		t.Fatalf("delete bead: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM bead_taints WHERE bead_id = $1`, beadID).Scan(&remaining); err != nil {
		t.Fatalf("count bead_taints after cascade delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("bead_taints rows survived beads delete: got %d, want 0", remaining)
	}

	// taint_grants.granted_by references operators(id); a bogus id is
	// rejected, and a real operator id succeeds with the composite PK.
	if _, err := db.Exec(
		`INSERT INTO taint_grants (taint_id, principal_kind, principal, granted_by) VALUES ($1, 'team', 'team-x', $2)`,
		taintID, int64(1<<62),
	); err == nil {
		t.Fatalf("taint_grants insert with bogus granted_by unexpectedly succeeded")
	}

	if _, err := db.Exec(
		`INSERT INTO taint_grants (taint_id, principal_kind, principal, granted_by) VALUES ($1, 'team', 'team-x', $2)`,
		taintID, operatorID,
	); err != nil {
		t.Fatalf("insert taint_grants with real operator id: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO taint_grants (taint_id, principal_kind, principal, granted_by) VALUES ($1, 'team', 'team-x', $2)`,
		taintID, operatorID,
	); err == nil {
		t.Fatalf("duplicate (taint_id, principal_kind, principal) insert into taint_grants unexpectedly succeeded")
	}

	// Round-trip: down then up recreates all four tables.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 047): %v", err)
	}
	for _, tbl := range wantTables {
		if tableExists(t, db, tbl) {
			t.Errorf("table %s still present after down 047", tbl)
		}
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 047): %v", err)
	}
	for _, tbl := range wantTables {
		if !tableExists(t, db, tbl) {
			t.Errorf("table %s missing after re-applying up 047", tbl)
		}
	}
}

// tableExists returns true iff a table of the given name exists in the
// public schema of the connected database.
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	const q = `SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = $1`
	var one int
	err := db.QueryRow(q, name).Scan(&one)
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows):
		return false
	default:
		t.Fatalf("information_schema.tables lookup for %s: %v", name, err)
		return false
	}
}
