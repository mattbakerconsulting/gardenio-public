//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigration054_CasefoldForgeLogins verifies migration 054 (gd-9AXizUgtzG):
// gitlab and github subjects in operator_aliases and operators are lowercased
// to match canonicalAliasSubject.
//
//   - spellings of one login on the same operator collapse to one row, keeping
//     the canonical spelling if present, else the oldest;
//   - spellings on different operators are left unchanged;
//   - other mixed-case gitlab/github rows are renamed; other kinds are not;
//   - operators rows are renamed unless another operator holds the canonical
//     subject;
//   - re-running the up body is a no-op and down then up round-trips.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration054
func TestMigration054_CasefoldForgeLogins(t *testing.T) {
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

	if err := m.Migrate(53); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 053: %v", err)
	}
	cleanup := func() {
		if _, err := db.Exec(`DELETE FROM operators WHERE email LIKE 'm054-%'`); err != nil {
			t.Fatalf("clear: %v", err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)

	op := func(kind, subject, email string) int64 {
		t.Helper()
		var id int64
		if err := db.QueryRow(`INSERT INTO operators (kind, subject, email) VALUES ($1, $2, $3) RETURNING id`,
			kind, subject, email).Scan(&id); err != nil {
			t.Fatalf("seed operator %s:%s: %v", kind, subject, err)
		}
		return id
	}
	alias := func(id int64, kind, subject, createdAt string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO operator_aliases (operator_id, kind, subject, source, created_at)
			VALUES ($1, $2, $3, 'test', $4::timestamptz)`, id, kind, subject, createdAt); err != nil {
			t.Fatalf("seed alias %s:%s: %v", kind, subject, err)
		}
	}

	a := op("oidc", "m054-a@example.com", "m054-a@example.com")
	b := op("oidc", "m054-b@example.com", "m054-b@example.com")
	c := op("oidc", "m054-c@example.com", "m054-c@example.com")
	d := op("oidc", "m054-d@example.com", "m054-d@example.com")
	e := op("oidc", "m054-e@example.com", "m054-e@example.com")
	g := op("gitlab", "M054-Gina", "m054-gina@noreply.kbeads.local")
	h := op("gitlab", "M054-Hank", "m054-hank1@noreply.kbeads.local")
	op("gitlab", "m054-hank", "m054-hank2@noreply.kbeads.local")

	// Same operator, canonical spelling present: it survives.
	alias(a, "gitlab", "M054-Alice", "2026-01-01")
	alias(a, "gitlab", "m054-alice", "2026-02-01")
	// Same operator, no canonical spelling: the oldest survives, folded.
	alias(b, "gitlab", "M054-Bob", "2026-02-01")
	alias(b, "gitlab", " M054-BOB", "2026-01-01")
	// Different operators: a conflict, left as is.
	alias(c, "gitlab", "M054-Carol", "2026-01-01")
	alias(d, "gitlab", "m054-CAROL", "2026-01-01")
	// Plain renames, and kinds 054 does not touch.
	alias(e, "github", "M054-Dave", "2026-01-01")
	alias(e, "gitlab", "M054-Erin", "2026-01-01")
	alias(e, "slack", "UM054X", "2026-01-01")
	alias(e, "jira", "M054-Jira", "2026-01-01")

	if err := m.Migrate(54); err != nil {
		t.Fatalf("migrate to 054: %v", err)
	}

	want := map[string]int64{
		"gitlab:m054-alice": a,
		"gitlab:m054-bob":   b,
		"gitlab:M054-Carol": c,
		"gitlab:m054-CAROL": d,
		"github:m054-dave":  e,
		"gitlab:m054-erin":  e,
		"slack:UM054X":      e,
		"jira:M054-Jira":    e,
	}
	check := func(stage string) {
		t.Helper()
		rows, err := db.Query(`SELECT kind || ':' || subject, operator_id FROM operator_aliases
			WHERE operator_id IN ($1, $2, $3, $4, $5)`, a, b, c, d, e)
		if err != nil {
			t.Fatalf("%s: select aliases: %v", stage, err)
		}
		defer rows.Close()
		got := map[string]int64{}
		for rows.Next() {
			var k string
			var id int64
			if err := rows.Scan(&k, &id); err != nil {
				t.Fatalf("%s: scan: %v", stage, err)
			}
			got[k] = id
		}
		if len(got) != len(want) {
			t.Errorf("%s: aliases = %v, want %v", stage, got, want)
		}
		for k, id := range want {
			if got[k] != id {
				t.Errorf("%s: alias %q -> %d, want %d", stage, k, got[k], id)
			}
		}

		var gina, hank string
		if err := db.QueryRow(`SELECT subject FROM operators WHERE id = $1`, g).Scan(&gina); err != nil {
			t.Fatalf("%s: select gina: %v", stage, err)
		}
		if gina != "m054-gina" {
			t.Errorf("%s: gitlab operator subject = %q, want m054-gina", stage, gina)
		}
		if err := db.QueryRow(`SELECT subject FROM operators WHERE id = $1`, h).Scan(&hank); err != nil {
			t.Fatalf("%s: select hank: %v", stage, err)
		}
		if hank != "M054-Hank" {
			t.Errorf("%s: conflicting gitlab operator subject = %q, want M054-Hank unchanged", stage, hank)
		}
	}
	check("after 054")

	// The up body must tolerate running twice (kbeads migration policy).
	upSQL, err := migrationsFS.ReadFile("migrations/054_operator_aliases_casefold_forge_logins.up.sql")
	if err != nil {
		t.Fatalf("read 054 up.sql: %v", err)
	}
	if _, err := db.Exec(string(upSQL)); err != nil {
		t.Fatalf("re-apply 054 up.sql body: %v", err)
	}
	check("after re-apply")

	// Round-trip.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 054: %v", err)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 054 again: %v", err)
	}
	check("after round-trip")
}
