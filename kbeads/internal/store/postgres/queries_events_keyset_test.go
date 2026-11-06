package postgres

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Regression guard for the events-feed keyset-pagination bug: the global
// /v1/events feed pages with a cursor transported as a bare events.id, but the
// two range queries ORDER BY the composite (created_at, id). A keyset scan is
// only correct when the cursor comparison uses the SAME key as the ORDER BY.
// events.created_at DEFAULTs to NOW() (transaction-start time) while events.id
// is a strictly-monotonic BIGSERIAL, so under concurrent inserts the two
// orderings can disagree. The original cursor predicate compared only the id
// (`events.id > $3` / `events.id < $3`) against a (created_at, id)-ordered
// scan, silently dropping and duplicating events across page boundaries. The
// fix reconstructs the full boundary from the cursor id and compares the
// composite tuple.
//
// This is a SOURCE-LEVEL guard (runs in the default sqlmock-only CI): sqlmock
// never executes the SQL, so the pinned-regexp unit tests above would happily
// pass with a bare-id cursor. This test reads queries.go and fails if either
// range query reverts to a scalar id cursor while sorting on the composite key.
// The end-to-end proof that no event is dropped/duplicated lives in the
// real-Postgres integration test (see the //go:build integration companion).
func TestEventsKeysetCursorSourceGuard(t *testing.T) {
	raw, err := os.ReadFile("queries.go")
	if err != nil {
		t.Fatalf("read queries.go: %v", err)
	}
	src := string(raw)

	// A bare scalar-id cursor predicate against a composite-ordered scan is the
	// bug shape. The composite tuple form `(events.created_at, events.id) > (...)`
	// is the fix.
	scalarCursor := regexp.MustCompile(`\$3 = 0 OR events\.id [<>] \$3`)
	cases := []struct {
		fn          string
		wantCompare string // composite tuple comparison direction
		wantOrder   string
	}{
		{"func queryGetEventsByTimeRange(", "(events.created_at, events.id) >", "ORDER BY events.created_at ASC, events.id ASC"},
		{"func queryGetEventsByTimeRangeDesc(", "(events.created_at, events.id) <", "ORDER BY events.created_at DESC, events.id DESC"},
	}
	for _, c := range cases {
		body := funcBody(t, src, c.fn)
		if scalarCursor.MatchString(body) {
			t.Errorf("%s still uses a scalar `events.id` cursor against a (created_at, id) sort — "+
				"that silently drops/duplicates events across page boundaries when id and "+
				"created_at order disagree; compare the composite tuple instead", c.fn)
		}
		if !strings.Contains(body, c.wantCompare) {
			t.Errorf("%s must compare the composite keyset tuple `%s (...)` so the cursor key "+
				"matches the ORDER BY key", c.fn, c.wantCompare)
		}
		if !strings.Contains(body, c.wantOrder) {
			t.Errorf("%s must keep `%s` (composite sort satisfied by idx_events_created_at_id, "+
				"migration 035)", c.fn, c.wantOrder)
		}
	}
}

// funcBody returns the source of the function whose signature starts with
// marker, from the marker up to the next top-level "\nfunc " (or EOF).
func funcBody(t *testing.T, src, marker string) string {
	t.Helper()
	start := strings.Index(src, marker)
	if start < 0 {
		t.Fatalf("marker %q not found in queries.go", marker)
	}
	rest := src[start+len(marker):]
	if end := strings.Index(rest, "\nfunc "); end >= 0 {
		return src[start : start+len(marker)+end]
	}
	return src[start:]
}
