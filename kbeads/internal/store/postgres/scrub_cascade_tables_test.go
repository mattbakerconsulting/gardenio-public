package postgres

import (
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestScrubCascadeCoversContentTables is the information_schema-diff guard
// ADR docs/adr/taints.md §11 invariant 7 requires: "A new table that stores
// bead content must be added to ScrubTaint's cascade in the same MR,
// enforced by a test that diffs information_schema against the cascade
// list." gd-NaWcBVaQxO.
//
// It reads the live table list from the store's own migration-tracked
// schema (migrationsFS, embedded from migrations/*.up.sql — no running
// Postgres needed, so this runs in plain `go test ./...`, unlike the
// //go:build integration tests elsewhere in this package that need
// BEADS_TEST_DATABASE_URL and are not wired into CI at all today) rather
// than querying information_schema.tables directly. The two are equivalent:
// every table in the schema is created by exactly one CREATE TABLE in a
// migration, and (per kbeads' migration policy) tables are never dropped.
//
// Every table the schema defines must land in exactly one of two lists:
//
//   - cascadeContentTables (scrub_cascade_tables.go): stores bead-derived
//     prose content per ADR §1.3, and therefore MUST be part of
//     cascadeContent's purge/detaint-strip cascade.
//   - scrubCascadeNonContentTables (below): does not store bead-derived
//     prose content, so ScrubTaint has nothing to scrub from it. Each entry
//     documents WHY, in terms of which columns it has and whether they are
//     content or just ids/metadata/audit trail (per ADR §1.3).
//
// A table in neither list fails the test — an unclassified table forces a
// reviewer to triage it in the same MR that introduced it, which is what
// invariant 7 actually asks for: an explicit decision, not silent coverage
// creep. A table classified as content but missing from cascadeContentTables
// also fails, with a message naming the table.
func TestScrubCascadeCoversContentTables(t *testing.T) {
	schemaTables := tablesFromMigrations(t)

	cascade := make(map[string]bool, len(cascadeContentTables))
	for _, tbl := range cascadeContentTables {
		cascade[tbl] = true
	}

	nonContent := make(map[string]string, len(scrubCascadeNonContentTables))
	for _, e := range scrubCascadeNonContentTables {
		nonContent[e.table] = e.reason
	}

	var uncovered []string
	for tbl := range schemaTables {
		if cascade[tbl] {
			continue
		}
		if _, ok := nonContent[tbl]; ok {
			continue
		}
		uncovered = append(uncovered, tbl)
	}
	sort.Strings(uncovered)
	if len(uncovered) > 0 {
		t.Errorf("table(s) %s exist in the schema but are classified neither as a "+
			"cascadeContentTables entry (scrub_cascade_tables.go) nor as a "+
			"scrubCascadeNonContentTables entry (scrub_cascade_tables_test.go): "+
			"per ADR docs/adr/taints.md §1.3, classify whether this table stores "+
			"bead-derived prose content (add it to cascadeContentTables — purge and "+
			"detaint/strip must cascade over it) or not (add it to "+
			"scrubCascadeNonContentTables with a one-line reason)",
			strings.Join(uncovered, ", "))
	}

	// Catch a typo/stale entry in cascadeContentTables itself: every table it
	// names must actually exist in the schema.
	var ghosts []string
	for tbl := range cascade {
		if !schemaTables[tbl] {
			ghosts = append(ghosts, tbl)
		}
	}
	sort.Strings(ghosts)
	if len(ghosts) > 0 {
		t.Errorf("cascadeContentTables names table(s) %s which do not exist in any "+
			"migration — stale or misspelled entry", strings.Join(ghosts, ", "))
	}

	var ghostsNonContent []string
	for tbl := range nonContent {
		if !schemaTables[tbl] {
			ghostsNonContent = append(ghostsNonContent, tbl)
		}
	}
	sort.Strings(ghostsNonContent)
	if len(ghostsNonContent) > 0 {
		t.Errorf("scrubCascadeNonContentTables names table(s) %s which do not exist "+
			"in any migration — stale or misspelled entry", strings.Join(ghostsNonContent, ", "))
	}
}

// scrubCascadeNonContentTableEntry documents one table this repo's schema
// defines that ScrubTaint's cascadeContent helper does NOT need to touch,
// and why — per ADR §1.3, which columns (if any) a table has that are not
// "bead content": ids, foreign keys, timestamps, counters, or an audit
// trail written by the scrub operation itself rather than read by it.
type scrubCascadeNonContentTableEntry struct {
	table  string
	reason string
}

// scrubCascadeNonContentTables is the explicit "not bead content" side of
// the classification TestScrubCascadeCoversContentTables enforces is
// exhaustive. See that test's doc comment for how this list is used.
var scrubCascadeNonContentTables = []scrubCascadeNonContentTableEntry{
	{"bead_references", "extracted bead ids only — ADR §1.3 explicitly excludes this despite sitting beside the content tables it lists ('extracted ids only... no content')"},
	{"bead_followers", "join table: bead_id + follower identity, no prose columns"},
	{"labels", "bead_id + a short label string; not listed among the ADR §1.3 content copies"},
	{"configs", "server-side key/value configuration, not bead-derived content"},
	{"agent_handle_counters", "a counter (next sequence value) per prefix, no bead content"},
	{"bead_embedding_migrations", "bookkeeping row tracking embedding-backfill progress, not embedding content itself (see bead_embeddings, which IS cascaded)"},
	{"operators", "operator identity rows (id, external subject, display name) — not bead content"},
	{"operator_aliases", "operator identity aliasing — not bead content"},
	{"operator_slug_aliases", "operator identity aliasing — not bead content"},
	{"refresh_tokens", "auth token bookkeeping — not bead content"},
	{"revoked_tokens", "auth token bookkeeping — not bead content"},
	{"runner_tokens", "auth token bookkeeping (hash + runner bead id + timestamps) — not bead content"},
	{"ui_telemetry", "client telemetry events, not bead-derived content"},
	{"push_devices", "push device tokens keyed by owner email, no bead reference or prose"},
	{"slow_tool_calls", "agent tool-call latency ledger (redacted command/argv), not bead-derived content; agent_bead_id is an FK, not a content copy"},
	{"taints", "taint registry definitions (name, owner, policy) — not bead content"},
	{"taint_grants", "who may read a taint's beads — not bead content"},
	{"bead_taints", "the taint marker itself: ScrubTaint DELETEs the row for the scrubbed taint (§6.5, or directly for detaint/lift per §15.4) as part of lifting the marker, but the row has no bead content to blank — it names bead_id/taint_id/source/applied_by/applied_at, none of them prose"},
	{"taint_audit", "the audit trail ScrubTaint WRITES to as part of a scrub — it records that a scrub happened (action, content_sha, actor, timestamp), it is not a copy of bead content ScrubTaint must destroy"},
}

// createTableRE matches a top-level `CREATE TABLE [IF NOT EXISTS] <name>` at
// the start of a (comment-stripped) line, capturing the bare table name.
// kbeads migrations never schema-qualify or quote table names (verified
// across migrations/*.up.sql), so a bare identifier is sufficient.
var createTableRE = regexp.MustCompile(`(?i)^CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([a-zA-Z_][a-zA-Z0-9_]*)`)

// dropTableRE mirrors createTableRE for DROP TABLE, so a future migration
// that removes a table (none exist today — kbeads migration policy is
// additive) is reflected in tablesFromMigrations rather than leaving a
// ghost entry.
var dropTableRE = regexp.MustCompile(`(?i)^DROP\s+TABLE\s+(?:IF\s+EXISTS\s+)?([a-zA-Z_][a-zA-Z0-9_]*)`)

// tablesFromMigrations statically derives the set of tables that exist at
// the head of the migration chain by scanning every migrations/*.up.sql file
// (embedded in migrationsFS, postgres.go) in migration order for CREATE
// TABLE / DROP TABLE statements. It does not run any SQL and needs no
// database connection.
func tablesFromMigrations(t *testing.T) map[string]bool {
	t.Helper()

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	var upFiles []string
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() && strings.HasSuffix(name, ".up.sql") {
			upFiles = append(upFiles, name)
		}
	}
	sort.Strings(upFiles) // zero-padded numeric prefixes sort in migration order

	tables := make(map[string]bool)
	for _, name := range upFiles {
		raw, err := fs.ReadFile(migrationsFS, "migrations/"+name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			// Strip a `-- ...` line comment (kbeads migrations never put
			// "--" inside a CREATE/DROP TABLE statement's leading tokens).
			if i := strings.Index(line, "--"); i >= 0 {
				line = line[:i]
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if m := createTableRE.FindStringSubmatch(line); m != nil {
				tables[m[1]] = true
				continue
			}
			if m := dropTableRE.FindStringSubmatch(line); m != nil {
				delete(tables, m[1])
			}
		}
	}

	if len(tables) == 0 {
		t.Fatal("tablesFromMigrations found zero tables — parser is broken")
	}
	return tables
}

// TestTablesFromMigrationsSanity pins a few known-stable tables so a future
// change to the migration file format (e.g. multi-line CREATE TABLE
// statements) that silently breaks the regex parser above is caught here
// rather than only by the classification test going quietly vacuous.
func TestTablesFromMigrationsSanity(t *testing.T) {
	tables := tablesFromMigrations(t)
	for _, want := range []string{"beads", "comments", "events", "taints", "bead_taints"} {
		if !tables[want] {
			t.Errorf("tablesFromMigrations: expected table %q not found; regex parser may be broken (found %d tables: %v)",
				want, len(tables), sortedKeys(tables))
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Compile-time-ish guard against a duplicate entry silently shadowing
// another (either within scrubCascadeNonContentTables, or across the two
// lists) — a duplicate would let TestScrubCascadeCoversContentTables pass
// while actually leaving a table's true classification ambiguous.
func TestScrubCascadeClassificationHasNoDuplicates(t *testing.T) {
	seen := make(map[string]string) // table -> where first seen
	for _, tbl := range cascadeContentTables {
		if src, ok := seen[tbl]; ok {
			t.Errorf("table %q listed twice: cascadeContentTables and %s", tbl, src)
		}
		seen[tbl] = "cascadeContentTables"
	}
	for _, e := range scrubCascadeNonContentTables {
		if src, ok := seen[e.table]; ok {
			t.Errorf("table %q listed twice: scrubCascadeNonContentTables and %s", e.table, src)
		}
		seen[e.table] = "scrubCascadeNonContentTables"
	}
}
