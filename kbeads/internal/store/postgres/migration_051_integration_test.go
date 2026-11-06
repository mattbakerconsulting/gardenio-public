//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/golang-migrate/migrate/v4"
)

// TestMigration051_ResolveHandleCounterGardenerID verifies migration 051
// (gd-3AHtqh5XXB, rename plan gd-5O9QUlA8zr): agent_handle_counters collapses
// the captain_id / gardener_id pair to a single gardener_id primary key.
//
//   - legacy `synthetic-captain:*` rows are re-keyed to `synthetic-gardener:*`;
//   - when both a legacy and a re-keyed row exist the larger next_seq wins, so
//     a handle sequence never rewinds;
//   - non-synthetic ids keep their value;
//   - captain_id is gone and re-running the up body is a no-op;
//   - down then up round-trips.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration051
func TestMigration051_ResolveHandleCounterGardenerID(t *testing.T) {
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

	// Pin to 050 (the pre-051 shape), seed rows, then step to 051.
	if err := m.Migrate(50); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to 050: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM agent_handle_counters`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	seed := []struct {
		captain, gardener string
		seq               int
	}{
		// Phase-0 dual-written legacy row, plus the row the post-rename code
		// created under the new prefix: larger seq must survive the merge.
		{"synthetic-captain:unknown", "synthetic-captain:unknown", 628},
		{"synthetic-gardener:unknown", "synthetic-gardener:unknown", 3},
		// Legacy row with no gardener_id yet (written before 050).
		{"synthetic-captain:slack", "", 41},
		// A real schedule id keeps its value.
		{"gd-Sched1", "gd-Sched1", 7},
	}
	for _, r := range seed {
		var g any
		if r.gardener != "" {
			g = r.gardener
		}
		if _, err := db.Exec(
			`INSERT INTO agent_handle_counters (captain_id, gardener_id, next_seq) VALUES ($1, $2, $3)`,
			r.captain, g, r.seq); err != nil {
			t.Fatalf("seed %q: %v", r.captain, err)
		}
	}

	if err := m.Migrate(51); err != nil {
		t.Fatalf("migrate to 051: %v", err)
	}
	if columnExists(t, db, "agent_handle_counters", "captain_id") {
		t.Fatalf("captain_id still present after 051")
	}

	want := map[string]int{
		"synthetic-gardener:unknown": 628,
		"synthetic-gardener:slack":   41,
		"gd-Sched1":                  7,
	}
	rows, err := db.Query(`SELECT gardener_id, next_seq FROM agent_handle_counters`)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	defer rows.Close()
	got := map[string]int{}
	for rows.Next() {
		var id string
		var seq int
		if err := rows.Scan(&id, &seq); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got[id] = seq
	}
	if len(got) != len(want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
	for id, seq := range want {
		if got[id] != seq {
			t.Errorf("gardener_id %q next_seq = %d, want %d", id, got[id], seq)
		}
	}

	// The up body must tolerate running twice (kbeads migration policy).
	upSQL, err := migrationsFS.ReadFile("migrations/051_agent_handle_counters_resolve_gardener_id.up.sql")
	if err != nil {
		t.Fatalf("read 051 up.sql: %v", err)
	}
	if _, err := db.Exec(string(upSQL)); err != nil {
		t.Fatalf("re-apply 051 up.sql body: %v", err)
	}

	// gardener_id is now the key: the allocator's upsert target must exist.
	var seq int64
	if err := db.QueryRow(`INSERT INTO agent_handle_counters (gardener_id, next_seq) VALUES ($1, 1)
		ON CONFLICT (gardener_id) DO UPDATE SET next_seq = agent_handle_counters.next_seq + 1
		RETURNING next_seq`, "synthetic-gardener:unknown").Scan(&seq); err != nil {
		t.Fatalf("allocator upsert: %v", err)
	}
	if seq != 629 {
		t.Errorf("allocated seq = %d, want 629", seq)
	}

	// Round-trip.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 051: %v", err)
	}
	if !columnExists(t, db, "agent_handle_counters", "captain_id") {
		t.Errorf("captain_id missing after down 051")
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 051 again: %v", err)
	}
	if columnExists(t, db, "agent_handle_counters", "captain_id") {
		t.Errorf("captain_id present after re-up 051")
	}
}
