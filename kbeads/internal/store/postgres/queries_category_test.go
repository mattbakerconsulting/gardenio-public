package postgres

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryBackfillCategoryPage_RestampsStaleRows pins the SQL SHAPE of the
// category materialization backfill (migration 043 / ka-0000007274): the
// candidate SELECT filters rows whose stored category disagrees with
// derive_bead_category, the UPDATE re-guards the same staleness predicate and
// re-derives via the SQL function, and the keyset cursor advances by the last
// candidate id.
func TestQueryBackfillCategoryPage_RestampsStaleRows(t *testing.T) {
	db, mock := newMockDB(t)

	// Candidate SELECT: staleness predicate + keyset cursor.
	mock.ExpectQuery(`SELECT id\s+FROM beads\s+WHERE category IS DISTINCT FROM derive_bead_category\(id, type\)\s+AND id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("gd-a").AddRow("gd-b"))

	// UPDATE: re-derives via the function, guarded on the same staleness check.
	mock.ExpectExec(`UPDATE beads\s+SET category = derive_bead_category\(id, type\)\s+WHERE id = ANY\(\$1::text\[\]\)\s+AND category IS DISTINCT FROM derive_bead_category\(id, type\)`).
		WillReturnResult(sqlmock.NewResult(0, 2))

	stats, err := queryBackfillCategoryPage(context.Background(), db, "", 500)
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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestQueryBackfillCategoryPage_EmptyPageIsDone pins that an empty candidate
// page issues no UPDATE and reports Done with an empty cursor.
func TestQueryBackfillCategoryPage_EmptyPageIsDone(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id\s+FROM beads\s+WHERE category IS DISTINCT FROM derive_bead_category\(id, type\)\s+AND id > \$1`).
		WithArgs("gd-z", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	stats, err := queryBackfillCategoryPage(context.Background(), db, "gd-z", 500)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeadsScanned != 0 {
		t.Errorf("BeadsScanned = %d, want 0", stats.BeadsScanned)
	}
	if stats.LastID != "" {
		t.Errorf("LastID = %q, want empty", stats.LastID)
	}
	if !stats.Done {
		t.Errorf("Done = false, want true (empty page)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
