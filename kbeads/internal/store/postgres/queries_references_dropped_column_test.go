package postgres

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestReextractQueries_DoNotSelectDroppedOperatedByColumn is a schema-drift
// guard for ka-0000001094 / migration 024, which PERMANENTLY dropped the
// legacy `operated_by` TEXT column from beads and comments ("POINT OF NO
// RETURN"). reextractOneBead re-reads bead/comment text inside its tx; a
// previous edit reintroduced `COALESCE(operated_by, '')` into both SELECTs
// AFTER the column was already dropped, so the very first bead of
// `kd admin reextract-references` 500'd with `column "operated_by" does not
// exist` on every post-024 database. The mocked sqlmock tests could not catch
// it — their ExpectQuery regexps were edited to match the broken SQL — so this
// test asserts against the ACTUAL source instead of a mock expectation.
//
// operated_by_id (the live BIGINT operator FK, migration 015) is the only
// operator column reextractOneBead needs, so it is explicitly allowed.
func TestReextractQueries_DoNotSelectDroppedOperatedByColumn(t *testing.T) {
	src, err := os.ReadFile("queries_references.go")
	if err != nil {
		t.Fatalf("read queries_references.go: %v", err)
	}
	// Match a bare `operated_by` token NOT immediately followed by `_id`, so
	// the live operated_by_id FK is permitted but the dropped TEXT column is
	// flagged. Word-boundary on the left keeps `created_by`/`closed_by` clear.
	dropped := regexp.MustCompile(`\boperated_by\b(?:_id)?`)
	for i, line := range strings.Split(string(src), "\n") {
		// Only inspect executable SQL, not the surrounding doc comments (they
		// legitimately discuss the dropped column and this migration).
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if !strings.Contains(line, "SELECT") && !strings.Contains(line, "FROM beads") && !strings.Contains(line, "FROM comments") {
			continue
		}
		for _, m := range dropped.FindAllString(line, -1) {
			if m == "operated_by_id" {
				continue // the live FK — allowed
			}
			t.Errorf("line %d selects the migration-024-dropped operated_by TEXT column: %q", i+1, strings.TrimSpace(line))
		}
	}
}
