//go:build integration

package postgres

import (
	"database/sql"
	"errors"
	"os"
	"testing"

	"gardenio/kbeads/internal/idgen"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/lib/pq"
)

// TestMigration040_BeadsSearchTSVFields verifies migration 040
// (ka-0000005839) — extending the search_tsv FTS vector to index a decision's
// context and option label/short text (which live in the bead FIELDS jsonb),
// not just its title + description:
//
//   - the shared IMMUTABLE function beads_search_tsv_expr(title,desc,fields)
//     exists and the maintenance trigger delegates to it;
//   - the trigger populates search_tsv from context/option text on INSERT and
//     refreshes it when `fields` is UPDATEd (the 040-widened trigger event);
//   - down restores the 039 title+description-only behavior (a fields-only term
//     no longer matches) and drops the shared expression function;
//   - up + down + up round-trips without error (idempotent recovery).
//
// The reindex backfill path (BackfillSearchTSVPage reindex=true) is exercised
// separately in TestSearchRelevanceOption2Fields_RealPostgres (query-layer),
// mirroring how the 039 backfill is tested in the relevance suite.
//
// Run with:
//
//	BEADS_TEST_DATABASE_URL=postgres://... go test -tags=integration \
//	  ./internal/store/postgres/ -run TestMigration040_BeadsSearchTSVFields
func TestMigration040_BeadsSearchTSVFields(t *testing.T) {
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

	// Migrate to EXACTLY version 40 (not true HEAD, which has moved well past
	// 40 since this test was authored): under Steps(-1)/Steps(1) below, later
	// migrations' downs/ups would run instead of 040's, so pin 040 as the tip.
	if err := m.Migrate(40); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate to version 40: %v", err)
	}
	if !functionExists(t, db, "beads_search_tsv_expr") {
		t.Fatalf("function beads_search_tsv_expr missing after up to head")
	}
	if !triggerExists(t, db, "beads", "beads_search_tsv_trigger") {
		t.Fatalf("trigger beads_search_tsv_trigger missing after up to head")
	}

	// beads.operated_by_id is NOT NULL (migration 022); resolve a real operator
	// FK from the DB rather than hardcoding an id that varies per instance.
	operatorID := anyOperatorID(t, dsn)

	// A decision whose only "kubectl" mention is inside an option label, and
	// whose only "escalate" mention is inside the context field, must match
	// both via FTS once 040's trigger has populated the vector.
	t.Run("trigger_indexes_context_and_options_on_insert", func(t *testing.T) {
		id := "gd-mig040-ins-" + idMust(t)
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id) })

		const fields = `{
			"context": "operator must escalate to the cluster owner",
			"options": [
				{"id":"a","short":"run it","label":"run the kubectl rollout","artifact_type":"plan"},
				{"id":"b","short":"defer","label":"file a followup","artifact_type":"bug"}
			]
		}`
		if _, err := db.Exec(`
			INSERT INTO beads (id, kind, type, title, description, status, priority, created_at, updated_at, fleet, fields, operated_by_id)
			VALUES ($1, 'issue', 'decision', 'pick a rollout strategy', 'no free-text here', 'open', 0, NOW(), NOW(), 'mig040', $2::jsonb, $3)`,
			id, fields, operatorID); err != nil {
			t.Fatalf("insert: %v", err)
		}

		for _, term := range []string{"kubectl", "escalate", "followup"} {
			var match bool
			if err := db.QueryRow(`
				SELECT search_tsv @@ websearch_to_tsquery('english', $2) FROM beads WHERE id = $1`,
				id, term).Scan(&match); err != nil {
				t.Fatalf("FTS check for %q: %v", term, err)
			}
			if !match {
				t.Errorf("search_tsv did not index fields text: no FTS match for %q", term)
			}
		}
	})

	// Editing the fields JSON (options/context) must refresh the vector — this
	// is the 040-widened trigger event (BEFORE UPDATE OF ..., fields).
	t.Run("trigger_refreshes_on_fields_update", func(t *testing.T) {
		id := "gd-mig040-upd-" + idMust(t)
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id) })

		if _, err := db.Exec(`
			INSERT INTO beads (id, kind, type, title, description, status, priority, created_at, updated_at, fleet, fields, operated_by_id)
			VALUES ($1, 'issue', 'decision', 'a decision', 'desc', 'open', 0, NOW(), NOW(), 'mig040',
			        '{"options":[{"id":"a","label":"terraform apply"}]}'::jsonb, $2)`,
			id, operatorID); err != nil {
			t.Fatalf("insert: %v", err)
		}
		// Replace the options so the old term disappears and a new one appears.
		if _, err := db.Exec(`
			UPDATE beads SET fields = '{"options":[{"id":"a","label":"helm rollback"}]}'::jsonb WHERE id = $1`,
			id); err != nil {
			t.Fatalf("update fields: %v", err)
		}
		var matchesNew, matchesOld bool
		if err := db.QueryRow(`
			SELECT search_tsv @@ websearch_to_tsquery('english', 'helm'),
			       search_tsv @@ websearch_to_tsquery('english', 'terraform')
			FROM beads WHERE id = $1`, id).Scan(&matchesNew, &matchesOld); err != nil {
			t.Fatalf("post-update FTS check: %v", err)
		}
		if !matchesNew {
			t.Errorf("trigger did not refresh search_tsv on fields update (no FTS match for 'helm')")
		}
		if matchesOld {
			t.Errorf("stale tsvector after fields update (still matches old option term 'terraform')")
		}
	})

	// Malformed / non-array options must not error the trigger (robustness).
	t.Run("trigger_tolerates_malformed_options", func(t *testing.T) {
		id := "gd-mig040-bad-" + idMust(t)
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id) })
		// options is a string (not an array); bare-string and non-object elements
		// elsewhere. The expression must skip them, not raise.
		const fields = `{"context":"guarded context text","options":"not-an-array"}`
		if _, err := db.Exec(`
			INSERT INTO beads (id, kind, type, title, description, status, priority, created_at, updated_at, fleet, fields, operated_by_id)
			VALUES ($1, 'issue', 'decision', 't', 'd', 'open', 0, NOW(), NOW(), 'mig040', $2::jsonb, $3)`,
			id, fields, operatorID); err != nil {
			t.Fatalf("insert with malformed options must not error: %v", err)
		}
		var match bool
		if err := db.QueryRow(`
			SELECT search_tsv @@ websearch_to_tsquery('english', 'guarded') FROM beads WHERE id = $1`,
			id).Scan(&match); err != nil {
			t.Fatalf("FTS check: %v", err)
		}
		if !match {
			t.Errorf("context text should still be indexed when options is malformed")
		}
	})

	// Step back one (undo 040) and verify 039 behavior is restored: a
	// fields-only term no longer matches, and the shared fn is gone.
	t.Run("down_restores_039_behavior", func(t *testing.T) {
		id := "gd-mig040-down-" + idMust(t)
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM beads WHERE id = $1`, id) })

		if err := m.Steps(-1); err != nil {
			t.Fatalf("steps -1 (down 040): %v", err)
		}
		if functionExists(t, db, "beads_search_tsv_expr") {
			t.Fatalf("beads_search_tsv_expr still present after down 040")
		}
		// A row inserted under 039's trigger: fields text is NOT indexed.
		if _, err := db.Exec(`
			INSERT INTO beads (id, kind, type, title, description, status, priority, created_at, updated_at, fleet, fields, operated_by_id)
			VALUES ($1, 'issue', 'decision', 'title only', 'desc only', 'open', 0, NOW(), NOW(), 'mig040',
			        '{"options":[{"id":"a","label":"kubectl only in option"}]}'::jsonb, $2)`,
			id, operatorID); err != nil {
			t.Fatalf("insert under 039 trigger: %v", err)
		}
		var match bool
		if err := db.QueryRow(`
			SELECT search_tsv @@ websearch_to_tsquery('english', 'kubectl') FROM beads WHERE id = $1`,
			id).Scan(&match); err != nil {
			t.Fatalf("FTS check under 039: %v", err)
		}
		if match {
			t.Errorf("039 trigger must NOT index option text, but 'kubectl' matched")
		}

		// Re-apply 040 (idempotent recovery) and confirm the fn returns.
		if err := m.Steps(1); err != nil {
			t.Fatalf("steps +1 (re-up 040): %v", err)
		}
		if !functionExists(t, db, "beads_search_tsv_expr") {
			t.Fatalf("beads_search_tsv_expr missing after re-apply of 040")
		}
	})
}

// idMust returns a fresh nanoid or fails the test.
func idMust(t *testing.T) string {
	t.Helper()
	id, err := idgen.Generate()
	if err != nil {
		t.Fatalf("idgen: %v", err)
	}
	return id
}

// functionExists returns true iff a function with the given name exists in the
// public schema (any signature).
func functionExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	const q = `SELECT 1 FROM pg_proc p
		JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname = 'public' AND p.proname = $1 LIMIT 1`
	var one int
	err := db.QueryRow(q, name).Scan(&one)
	switch {
	case err == nil:
		return true
	case errors.Is(err, sql.ErrNoRows):
		return false
	default:
		t.Fatalf("pg_proc lookup for %s: %v", name, err)
		return false
	}
}
