package postgres

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryBackfillSearchTSVPage_DefaultMode pins the SQL SHAPE of the legacy
// NULL-fill mode (migration 039): candidate SELECT filters `search_tsv IS
// NULL`, the UPDATE re-guards `search_tsv IS NULL`, and on the final page a
// remaining-NULL count is issued. (ka-0000003832)
func TestQueryBackfillSearchTSVPage_DefaultMode(t *testing.T) {
	db, mock := newMockDB(t)

	// Candidate SELECT: NULL predicate + keyset cursor.
	mock.ExpectQuery(`SELECT id\s+FROM beads\s+WHERE search_tsv IS NULL AND id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("gd-a").AddRow("gd-b"))

	// UPDATE: sets vector via the shared expr, guarded on still-NULL.
	mock.ExpectExec(`UPDATE beads\s+SET search_tsv = beads_search_tsv_expr\(title, description, fields\)\s+WHERE id = ANY\(\$1::text\[\]\) AND search_tsv IS NULL`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	// Final page (2 < 500) → remaining-NULL count.
	mock.ExpectQuery(`SELECT count\(\*\) FROM beads WHERE search_tsv IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillSearchTSVPage(context.Background(), db, "", 500, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeadsScanned != 2 {
		t.Errorf("BeadsScanned = %d, want 2", stats.BeadsScanned)
	}
	if stats.LastID != "gd-b" {
		t.Errorf("LastID = %q, want gd-b", stats.LastID)
	}
	if !stats.Done {
		t.Errorf("Done = false, want true (page < batchSize)")
	}
	if stats.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0", stats.Remaining)
	}
}

// TestQueryBackfillSearchTSVPage_ReindexMode pins the SQL SHAPE of the fields
// reindex mode (migration 040): candidate SELECT filters context/options-
// bearing rows, the UPDATE guards on `search_tsv IS DISTINCT FROM <expr>`, and
// NO remaining-NULL count is issued (Remaining is not meaningful).
// (ka-0000005839)
func TestQueryBackfillSearchTSVPage_ReindexMode(t *testing.T) {
	db, mock := newMockDB(t)

	// Candidate SELECT: context/options presence predicate + keyset cursor.
	mock.ExpectQuery(`SELECT id\s+FROM beads\s+WHERE \(fields \? 'context' OR fields \? 'options'\) AND id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("gd-x"))

	// UPDATE: guarded on the vector actually differing (idempotent re-run).
	mock.ExpectExec(`UPDATE beads\s+SET search_tsv = beads_search_tsv_expr\(title, description, fields\)\s+WHERE id = ANY\(\$1::text\[\]\) AND search_tsv IS DISTINCT FROM beads_search_tsv_expr\(title, description, fields\)`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Final page (1 < 500) and reindex mode → NO remaining-NULL count query.
	// (If the code issued one, ExpectationsWereMet would fail on the missing
	// expectation, so the absence is asserted by not registering it.)

	stats, err := queryBackfillSearchTSVPage(context.Background(), db, "", 500, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeadsScanned != 1 {
		t.Errorf("BeadsScanned = %d, want 1", stats.BeadsScanned)
	}
	if stats.LastID != "gd-x" {
		t.Errorf("LastID = %q, want gd-x", stats.LastID)
	}
	if !stats.Done {
		t.Errorf("Done = false, want true (page < batchSize)")
	}
	if stats.Remaining != 0 {
		t.Errorf("Remaining = %d, want 0 (not meaningful in reindex mode)", stats.Remaining)
	}
}

// TestQueryBackfillSearchTSVPage_EmptyPageNoUpdate verifies that an empty
// candidate page issues no UPDATE and reports Done with a zero cursor — the
// belt-and-suspenders path the CLI driver guards against looping on.
func TestQueryBackfillSearchTSVPage_EmptyPageNoUpdate(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id\s+FROM beads\s+WHERE search_tsv IS NULL AND id > \$1`).
		WithArgs("gd-cursor", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	// Empty page → Done, so the remaining-NULL count still runs in NULL-fill mode.
	mock.ExpectQuery(`SELECT count\(\*\) FROM beads WHERE search_tsv IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillSearchTSVPage(context.Background(), db, "gd-cursor", 500, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeadsScanned != 0 {
		t.Errorf("BeadsScanned = %d, want 0", stats.BeadsScanned)
	}
	if stats.LastID != "" {
		t.Errorf("LastID = %q, want empty (no rows in page)", stats.LastID)
	}
	if !stats.Done {
		t.Errorf("Done = false, want true (empty page)")
	}
}
