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

// TestFieldsPresenceReachesPartialIndex is the real-Postgres proof for
// bug ka-0000001686: the `-f key=*` presence sentinel must emit the
// leading `fields ? key` guard so the planner can reach the migration-025
// partial expression indexes (agent/agent_state/project/role/
// slack_user_id), 031 (schedule_id), 034 (trigger_id).
//
// It asserts the fix at the plan level, not just at the SQL-string level
// (postgres_test.go's FilterByFieldPresence case covers the string):
//
//   - The NEW, guarded predicate — `fields ? 'role' AND fields->>'role'
//     != ''` — reaches idx_beads_fields_role.
//   - The OLD, unguarded predicate — `fields->>'role' IS NOT NULL AND
//     fields->>'role' != ''` — CANNOT, even with seqscan disabled: the
//     planner has no way to derive the index's `WHERE fields ? 'role'`
//     predicate from a bare `->>` test, so it never names the partial
//     index. This is the Seq Scan the bug described (same failure mode as
//     the 504 in ka-0000002374).
//
// Gated by `//go:build integration` + BEADS_TEST_DATABASE_URL so the
// default sqlmock-only `go test ./...` workflow (kbeads/CLAUDE.md) is
// unaffected. Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestFieldsPresenceReachesPartialIndex
func TestFieldsPresenceReachesPartialIndex(t *testing.T) {
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
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("up to head: %v", err)
	}

	// The presence branch's `->>` non-empty term must NOT reach the index
	// on its own — only the `fields ? key` guard the fix adds does. So the
	// GUARDED plan names the partial index and the UNGUARDED one does not,
	// even with enable_seqscan off (which forces the planner to prefer any
	// usable index — its inability to use one here is exactly the bug).
	guarded := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE fields ? 'role'
		  AND fields->>'role' != ''
		LIMIT 1`)
	if !strings.Contains(guarded, "idx_beads_fields_role") {
		t.Errorf("guarded presence predicate does not reach idx_beads_fields_role; plan:\n%s", guarded)
	}

	unguarded := explainInTx(t, db, `
		SELECT id FROM beads
		WHERE fields->>'role' IS NOT NULL
		  AND fields->>'role' != ''
		LIMIT 1`)
	if strings.Contains(unguarded, "idx_beads_fields_role") {
		t.Errorf("unguarded (pre-fix) presence predicate unexpectedly reached the partial index; the bug's premise no longer holds. plan:\n%s", unguarded)
	}
}
