//go:build integration

package postgres

import (
	"errors"
	"os"
	"strings"
	"testing"

	"database/sql"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

// TestMigration044_BeadsCommentsTrgm verifies migration 044 (ka-0000003620,
// Phase 2 of design ka-0000004060):
//
//   - up creates the pg_trgm extension and the three GIN trigram indexes
//     (idx_beads_title_trgm, idx_beads_description_trgm, idx_comments_text_trgm);
//   - down drops the three indexes cleanly (the extension is intentionally
//     left in place, mirroring migration 005's "don't drop the shared
//     extension" note);
//   - up + down + up round-trips without error (idempotent recovery);
//   - EXPLAIN of the rewritten queryListBeads text-search predicate reaches a
//     BitmapOr over the trgm/tsv indexes with NO `Seq Scan on beads` — the
//     whole point of the phase. This is a regression guard for ka-0000003621
//     (kd search 500) and ka-0000006261 (BeadsDaemonDown false page): if the
//     query is ever reverted to the correlated-EXISTS shape, or the indexes go
//     missing, the plan falls back to a seq scan and this test fails.
//
// Reuses newTestMigrator / indexExists / explainInTx
// (migration_025_integration_test.go — same package + integration build tag).
// explainInTx runs EXPLAIN with `SET LOCAL enable_seqscan = off`, which is
// what makes this assertion DETERMINISTIC regardless of test-corpus size: with
// the uncorrelated `id = ANY(ARRAY(...))` rewrite the planner has an index path
// and produces a BitmapOr/Bitmap-Heap-Scan on beads; with the OLD correlated
// `EXISTS (... WHERE comments.bead_id = beads.id ...)` shape NO index can serve
// the disjunction, so even under enable_seqscan=off (a 1e10 cost penalty on seq
// scans) the planner is FORCED back to a `Seq Scan on beads`. So the two shapes
// are distinguishable by this assertion at any corpus size — a small CI fixture
// cannot make the query rewrite look present when it is not.
//
// Gated by `//go:build integration` and BEADS_TEST_DATABASE_URL so the default
// `go test ./...` workflow stays sqlmock-only per kbeads/CLAUDE.md.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration044_BeadsCommentsTrgm
func TestMigration044_BeadsCommentsTrgm(t *testing.T) {
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

	indexes := []string{
		"idx_beads_title_trgm",
		"idx_beads_description_trgm",
		"idx_comments_text_trgm",
	}

	// Migrate to EXACTLY version 44 (not true HEAD, which has moved well past
	// 44 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 044's, so pin 044 as the tip.
	if err := m.Migrate(44); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 44: %v", err)
	}
	for _, idx := range indexes {
		if !indexExists(t, db, idx) {
			t.Fatalf("index %s missing after up to head", idx)
		}
	}
	// The extension must be installed.
	var hasExt bool
	if err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pg_trgm')`).Scan(&hasExt); err != nil {
		t.Fatalf("pg_extension lookup: %v", err)
	}
	if !hasExt {
		t.Fatalf("pg_trgm extension missing after up to head")
	}

	// Step back one (undo 044) and verify the indexes are gone.
	if err := m.Steps(-1); err != nil {
		t.Fatalf("steps -1 (down 044): %v", err)
	}
	for _, idx := range indexes {
		if indexExists(t, db, idx) {
			t.Fatalf("index %s still present after down 044", idx)
		}
	}

	// Re-apply 044 and verify they come back (idempotent recovery).
	if err := m.Steps(1); err != nil {
		t.Fatalf("steps +1 (up 044): %v", err)
	}
	for _, idx := range indexes {
		if !indexExists(t, db, idx) {
			t.Fatalf("index %s missing after re-apply", idx)
		}
	}

	// Seed a small corpus carrying the marker terms so the trgm indexes have
	// tuples to point at. Size is irrelevant to the assertion (explainInTx forces
	// enable_seqscan=off — see the doc comment); the rows exist only so the plan
	// nodes are concrete. All rows are cleaned up on completion.
	seedTrgmCorpus(t, db)

	// EXPLAIN the rewritten queryListBeads text-search predicate shape
	// (uncorrelated `id = ANY(ARRAY(...))` comment branch OR'd with the FTS
	// predicate) and assert BitmapOr / index scans, NO Seq Scan on beads.
	plan := explainInTx(t, db, `
		SELECT beads.id FROM beads
		WHERE deleted_at IS NULL
		AND (
		  (
		    (title ILIKE '%' || $1 || '%' OR description ILIKE '%' || $1 || '%' OR beads.id = ANY(ARRAY(SELECT bead_id FROM comments WHERE comments.text ILIKE '%' || $1 || '%')))
		    AND (title ILIKE '%' || $2 || '%' OR description ILIKE '%' || $2 || '%' OR beads.id = ANY(ARRAY(SELECT bead_id FROM comments WHERE comments.text ILIKE '%' || $2 || '%')))
		  )
		  OR (beads.search_tsv @@ websearch_to_tsquery('english', $3))
		)
		ORDER BY beads.created_at DESC
		LIMIT 50`,
		"zzgardener", "zzliveness", "zzgardener zzliveness")

	if strings.Contains(plan, "Seq Scan on beads") {
		t.Errorf("EXPLAIN plan still contains a Seq Scan on beads — the trgm indexes + query rewrite did not collapse the disjunction to a BitmapOr; got:\n%s", plan)
	}
	if !strings.Contains(plan, "BitmapOr") {
		t.Errorf("EXPLAIN plan does not contain a BitmapOr (expected trgm/tsv index scans OR'd together); got:\n%s", plan)
	}
}

// seedTrgmCorpus inserts a small corpus of beads + comments where the marker
// terms zzgardener / zzliveness occur at low selectivity, so the planner's real
// cost model favors the trgm/tsv indexes over a seq scan. Cleaned up via
// t.Cleanup. The 'zz'-prefixed markers avoid colliding with any real seeded
// data in a shared test database.
func seedTrgmCorpus(t *testing.T, db *sql.DB) {
	t.Helper()
	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	var operatorID int64
	if err := db.QueryRow(`SELECT id FROM operators ORDER BY id LIMIT 1`).Scan(&operatorID); err != nil {
		t.Fatalf("resolve operator id: %v", err)
	}
	// Bulk filler with no marker terms, plus a handful of marker-bearing rows.
	if _, err := db.Exec(`
		INSERT INTO beads (id, kind, type, title, description, status, priority, operated_by_id)
		SELECT 'gd-trgm44-'||g, 'issue', 'task',
		       'filler item '||g, 'generic body '||g, 'open', 2, $1
		FROM generate_series(1, 5000) g`, operatorID); err != nil {
		t.Fatalf("seed filler beads: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE beads SET title = title || ' zzgardener' WHERE id = 'gd-trgm44-10'`); err != nil {
		t.Fatalf("seed title marker: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE beads SET description = description || ' zzliveness' WHERE id = 'gd-trgm44-20'`); err != nil {
		t.Fatalf("seed description marker: %v", err)
	}
	if _, err := db.Exec(`
		INSERT INTO comments (bead_id, text, operated_by_id) VALUES ('gd-trgm44-30', 'note zzgardener zzliveness in a comment', $1)`,
		operatorID); err != nil {
		t.Fatalf("seed comment marker: %v", err)
	}
	if _, err := db.Exec(`ANALYZE beads`); err != nil {
		t.Fatalf("analyze beads: %v", err)
	}
	if _, err := db.Exec(`ANALYZE comments`); err != nil {
		t.Fatalf("analyze comments: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM comments WHERE bead_id LIKE 'gd-trgm44-%'`)
		_, _ = db.Exec(`DELETE FROM beads WHERE id LIKE 'gd-trgm44-%'`)
	})
}
