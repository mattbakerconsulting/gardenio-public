package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/references"
)

var _ = sql.ErrNoRows // keep "database/sql" used across the test file

func TestReplaceBeadReferences_DeletesAndInserts(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:          "gd-ldkpaoah1s",
		Title:       "see gd-FNHTKRBDIQ",
		Description: "blocks gd-HKBGWAOTHB and gd-7YFI0RSI0U",
		Notes:       "no refs here",
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-ldkpaoah1s", "title", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-HKBGWAOTHB", "gd-7YFI0RSI0U").
		WillReturnResult(sqlmock.NewResult(0, 2))

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
}

// A decision-shaped bead keeps its operator-facing body in fields.context and
// leaves description EMPTY — the exact shape `gb decision create` writes. The
// ids there must land in the "fields" bucket so `kd list --mentions <id>` (the
// §5.0 pre-mutation decision gate) can see them. Regression test for
// ka-0000001507 (gate failing open).
func TestReplaceBeadReferences_ExtractsFieldsContext(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:          "gd-decision001",
		Title:       "decision: which policy wins",
		Description: "", // gb decision create leaves this empty
		Notes:       "",
		Fields:      []byte(`{"context":"parked: ka-0000001597 and ka-0000001471","options":[{"id":"a","label":"bless ka-0000003955"}]}`),
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-decision001", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Title carries no gd-id, description/notes are empty. The only INSERT is
	// the untyped fields bucket, covering all three ids regardless of which
	// key (context, options) holds them.
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-decision001", "fields", "ka-0000001597", "ka-0000001471", "ka-0000003955").
		WillReturnResult(sqlmock.NewResult(0, 3))

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// The fields bucket must be untyped-only: a `blocks(gd-X)` literal inside a
// fields blob must NOT mint a typed bead_references row or a deps row — only a
// flat mention. Otherwise arbitrary JSON blob content (option labels, prompts)
// would poison the dependency graph.
func TestReplaceBeadReferences_FieldsBucketIsUntypedOnly(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:          "gd-decision002",
		Title:       "no refs",
		Description: "",
		Notes:       "",
		Fields:      []byte(`{"context":"blocks(gd-FNHTKRBDIQ)"}`),
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-decision002", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Exactly one INSERT — the flat fields row. No typed bead_references row
	// and no deps INSERT (which would appear as additional ExecContext calls).
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-decision002", "fields", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestReplaceBeadReferences_NoRefs_OnlyDeletes(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:          "gd-brznypltde",
		Title:       "no refs",
		Description: "",
		Notes:       "still nothing",
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-brznypltde", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
}

func TestQueryListBeadsMentioning_NoMatches(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM bead_references br\s+JOIN beads b ON b\.id = br\.source_bead_id\s+WHERE br\.target_bead_id = \$1 AND b\.deleted_at IS NULL`).
		WithArgs("gd-target00001").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	rows, total, err := queryListBeadsMentioning(context.Background(), db, "gd-target00001", 50, 0, nil)
	if err != nil {
		t.Fatalf("queryListBeadsMentioning: %v", err)
	}
	if total != 0 {
		t.Errorf("total = %d, want 0", total)
	}
	if len(rows) != 0 {
		t.Errorf("len(rows) = %d, want 0", len(rows))
	}
}

func TestQueryListBeadsMentioning_HappyPath(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM bead_references br`).
		WithArgs("gd-target00001").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	pageRows := sqlmock.NewRows([]string{"id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-ldkpaoah1s", "First mention", "task", "open", "description", nil).
		AddRow("gd-hsr6nhjuuy", "Second mention", "epic", "in_progress", "comment:7", "parent")
	mock.ExpectQuery(`SELECT b\.id, b\.title, b\.type, b\.status, br\.source_field, br\.relationship_type\s+FROM bead_references br\s+JOIN beads b ON b\.id = br\.source_bead_id\s+WHERE br\.target_bead_id = \$1 AND b\.deleted_at IS NULL\s+ORDER BY br\.extracted_at DESC, b\.id, br\.source_field\s+LIMIT \$2 OFFSET \$3`).
		WithArgs("gd-target00001", 50, 0).
		WillReturnRows(pageRows)

	rows, total, err := queryListBeadsMentioning(context.Background(), db, "gd-target00001", 50, 0, nil)
	if err != nil {
		t.Fatalf("queryListBeadsMentioning: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].ID != "gd-ldkpaoah1s" || rows[0].SourceField != "description" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[0].RelationshipType != nil {
		t.Errorf("row 0 RelationshipType = %v, want nil (NULL column)", *rows[0].RelationshipType)
	}
	if rows[1].ID != "gd-hsr6nhjuuy" || rows[1].SourceField != "comment:7" {
		t.Errorf("row 1 = %+v", rows[1])
	}
	if rows[1].RelationshipType == nil || *rows[1].RelationshipType != "parent" {
		t.Errorf("row 1 RelationshipType = %v, want 'parent'", rows[1].RelationshipType)
	}
}

func TestQueryListBeadsMentioning_DefaultPagination(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("gd-target00001").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	pageRows := sqlmock.NewRows([]string{"id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-ldkpaoah1s", "t", "task", "open", "title", nil)
	// limit<=0 should default to 50; offset<0 should default to 0.
	mock.ExpectQuery(`LIMIT \$2 OFFSET \$3`).
		WithArgs("gd-target00001", 50, 0).
		WillReturnRows(pageRows)

	if _, _, err := queryListBeadsMentioning(context.Background(), db, "gd-target00001", 0, -5, nil); err != nil {
		t.Fatalf("queryListBeadsMentioning: %v", err)
	}
}

func TestQueryListBeadsMentioning_CountError(t *testing.T) {
	db, mock := newMockDB(t)

	boom := errors.New("connection lost")
	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("gd-target00001").
		WillReturnError(boom)

	if _, _, err := queryListBeadsMentioning(context.Background(), db, "gd-target00001", 50, 0, nil); err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestQueryListBeadReferencesFrom_DanglingTargets(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM bead_references br WHERE source_bead_id = \$1`).
		WithArgs("gd-ldkpaoah1s").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	// One target resolves, one is dangling (LEFT JOIN returns empty strings via COALESCE).
	pageRows := sqlmock.NewRows([]string{"target_bead_id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-FNHTKRBDIQ", "Real Bead", "task", "open", "description", nil).
		AddRow("gd-ZACQZ1SGLD", "", "", "", "description", nil)
	mock.ExpectQuery(`SELECT br\.target_bead_id,\s+COALESCE\(b\.title, ''\),\s+COALESCE\(b\.type, ''\),\s+COALESCE\(b\.status, ''\),\s+br\.source_field,\s+br\.relationship_type\s+FROM bead_references br\s+LEFT JOIN beads b ON b\.id = br\.target_bead_id AND b\.deleted_at IS NULL\s+WHERE br\.source_bead_id = \$1\s+ORDER BY br\.source_field, br\.target_bead_id\s+LIMIT \$2 OFFSET \$3`).
		WithArgs("gd-ldkpaoah1s", 50, 0).
		WillReturnRows(pageRows)

	rows, total, err := queryListBeadReferencesFrom(context.Background(), db, "gd-ldkpaoah1s", 50, 0, nil)
	if err != nil {
		t.Fatalf("queryListBeadReferencesFrom: %v", err)
	}
	if total != 2 {
		t.Errorf("total = %d, want 2", total)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].ID != "gd-FNHTKRBDIQ" || rows[0].Title != "Real Bead" || rows[0].Type != "task" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].ID != "gd-ZACQZ1SGLD" || rows[1].Title != "" || rows[1].Type != "" || rows[1].Status != "" {
		t.Errorf("row 1 = %+v (expected dangling target with empty fields)", rows[1])
	}
}

func TestQueryListBeadReferencesFrom_NoMatches(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM bead_references br WHERE source_bead_id = \$1`).
		WithArgs("gd-ldkpaoah1s").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	rows, total, err := queryListBeadReferencesFrom(context.Background(), db, "gd-ldkpaoah1s", 50, 0, nil)
	if err != nil {
		t.Fatalf("queryListBeadReferencesFrom: %v", err)
	}
	if total != 0 || len(rows) != 0 {
		t.Errorf("total=%d rows=%d, want 0/0", total, len(rows))
	}
}

func TestQueryListBeadReferencesFrom_Pagination(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\)`).
		WithArgs("gd-ldkpaoah1s").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(100))

	pageRows := sqlmock.NewRows([]string{"target_bead_id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-FNHTKRBDIQ", "t", "task", "open", "description", nil)
	// Custom limit/offset should pass through verbatim.
	mock.ExpectQuery(`LIMIT \$2 OFFSET \$3`).
		WithArgs("gd-ldkpaoah1s", 10, 20).
		WillReturnRows(pageRows)

	_, total, err := queryListBeadReferencesFrom(context.Background(), db, "gd-ldkpaoah1s", 10, 20, nil)
	if err != nil {
		t.Fatalf("queryListBeadReferencesFrom: %v", err)
	}
	if total != 100 {
		t.Errorf("total = %d, want 100", total)
	}
}

func TestReplaceCommentReferences_PerCommentScope(t *testing.T) {
	db, mock := newMockDB(t)
	c := &model.Comment{
		ID:     42,
		BeadID: "gd-zrem5da0ro",
		Author: "alice",
		Text:   "fixed by gd-5VCLRIVK3G",
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-zrem5da0ro", "comment:42").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-zrem5da0ro", "comment:42", "gd-5VCLRIVK3G").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := replaceCommentReferences(context.Background(), db, c); err != nil {
		t.Fatalf("replaceCommentReferences: %v", err)
	}
}

func TestReplaceCommentReferences_NoRefs_OnlyDeletes(t *testing.T) {
	db, mock := newMockDB(t)
	c := &model.Comment{
		ID:     7,
		BeadID: "gd-3rbjc370zr",
		Author: "bob",
		Text:   "looks good",
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-3rbjc370zr", "comment:7").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := replaceCommentReferences(context.Background(), db, c); err != nil {
		t.Fatalf("replaceCommentReferences: %v", err)
	}
}

func TestQueryReextractReferencesPage_FinalPageReportsTotal(t *testing.T) {
	db, mock := newMockDB(t)

	// One-bead page: list returns one row, then per-bead tx runs.
	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
			AddRow("gd-fnhtkrbdiq", "title ka-0000001713", "", "", nil))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).
			AddRow("title ka-0000001713", "", "", nil, *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-fnhtkrbdiq", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-fnhtkrbdiq", "title", "ka-0000001713").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id, text, operated_by_id FROM comments WHERE bead_id = \$1 ORDER BY id ASC`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"id", "text", "operated_by_id"}))
	mock.ExpectCommit()

	// Final-page total cardinality query.
	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, true, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if !stats.Done {
		t.Errorf("expected Done=true, got %v", stats.Done)
	}
	if stats.BeadsScanned != 1 {
		t.Errorf("BeadsScanned = %d, want 1", stats.BeadsScanned)
	}
	if stats.ReferencesWritten != 1 {
		t.Errorf("ReferencesWritten = %d, want 1", stats.ReferencesWritten)
	}
	if stats.LastID != "gd-fnhtkrbdiq" {
		t.Errorf("LastID = %q, want gd-fnhtkrbdiq", stats.LastID)
	}
	if stats.TotalReferences != 1 {
		t.Errorf("TotalReferences = %d, want 1", stats.TotalReferences)
	}
}

func TestQueryReextractReferencesPage_PartialPageNotDone(t *testing.T) {
	db, mock := newMockDB(t)

	// Return batchSize=2 rows so Done=false (page is full).
	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
		WithArgs("", 2).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
			AddRow("gd-fnhtkrbdiq", "", "", "", nil).
			AddRow("gd-hkbgwaothb", "", "", "", nil))

	for _, id := range []string{"gd-fnhtkrbdiq", "gd-hkbgwaothb"} {
		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
			WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).AddRow("", "", "", nil, *testOperatorID))
		mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
			WithArgs(id, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectQuery(`SELECT id, text, operated_by_id FROM comments WHERE bead_id = \$1 ORDER BY id ASC`).
			WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"id", "text", "operated_by_id"}))
		mock.ExpectCommit()
	}

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 2, true, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.Done {
		t.Errorf("expected Done=false on full page, got true")
	}
	if stats.LastID != "gd-hkbgwaothb" {
		t.Errorf("LastID = %q, want gd-hkbgwaothb", stats.LastID)
	}
	if stats.TotalReferences != 0 {
		t.Errorf("TotalReferences = %d, want 0 (only set on Done=true)", stats.TotalReferences)
	}
}

func TestQueryReextractReferencesPage_EmptyPageDone(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
		WithArgs("gd-xiidb212hw", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}))
	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))

	stats, err := queryReextractReferencesPage(context.Background(), db, "gd-xiidb212hw", 500, true, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if !stats.Done {
		t.Errorf("expected Done=true on empty page")
	}
	if stats.BeadsScanned != 0 {
		t.Errorf("BeadsScanned = %d, want 0", stats.BeadsScanned)
	}
	if stats.LastID != "" {
		t.Errorf("LastID = %q, want empty", stats.LastID)
	}
	if stats.TotalReferences != 42 {
		t.Errorf("TotalReferences = %d, want 42", stats.TotalReferences)
	}
}

// TestQueryReextractReferencesPage_TypeFilterNarrowsWalk asserts a non-empty
// beadType adds an `AND type = $2` predicate to the page walk with the type as
// the second bind and the LIMIT shifted to $3 — the mechanism that lets a
// targeted `--type decision` pass reindex only decisions and make the
// parked_on_decision badge sound in seconds (ka-0000005898). The regexp
// requires the type predicate, so it fails if the filter silently degrades to
// a full-table walk.
func TestQueryReextractReferencesPage_TypeFilterNarrowsWalk(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1 AND type = \$2\s+ORDER BY id ASC\s+LIMIT \$3`).
		WithArgs("", "decision", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
			AddRow("gd-cfuibfupjl", "parks ka-0000001713", "", "", nil))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-cfuibfupjl").
		WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).
			AddRow("parks ka-0000001713", "", "", nil, *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-cfuibfupjl", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-cfuibfupjl", "title", "ka-0000001713").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT id, text, operated_by_id FROM comments WHERE bead_id = \$1 ORDER BY id ASC`).
		WithArgs("gd-cfuibfupjl").
		WillReturnRows(sqlmock.NewRows([]string{"id", "text", "operated_by_id"}))
	mock.ExpectCommit()

	// Partial page (1 < 500) so Done=true and the total-cardinality query runs.
	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, true, false, "decision")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.BeadsScanned != 1 {
		t.Errorf("BeadsScanned = %d, want 1", stats.BeadsScanned)
	}
	if stats.LastID != "gd-cfuibfupjl" {
		t.Errorf("LastID = %q, want gd-cfuibfupjl", stats.LastID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestReplaceBeadReferences_DedupsAcrossField(t *testing.T) {
	db, mock := newMockDB(t)
	// Same target ID appears twice in description; references.Extract collapses
	// duplicates within a single field, so the bulk INSERT should emit one row.
	bead := &model.Bead{
		ID:          "gd-nxgyixlfym",
		Description: "see gd-FA6DCNNHZL again gd-FA6DCNNHZL",
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-nxgyixlfym", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-nxgyixlfym", "description", "gd-FA6DCNNHZL").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
}

func TestQueryGetTextReferencesForBeads_Empty(t *testing.T) {
	result, err := queryGetTextReferencesForBeads(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("expected nil error for empty input, got %v", err)
	}
	if result != nil {
		t.Fatalf("expected nil result for empty input, got %v", result)
	}
}

func TestQueryGetTextReferencesForBeads_FiltersBothEndpoints(t *testing.T) {
	db, mock := newMockDB(t)

	// JOIN-equivalent: both source AND target must be in the supplied
	// window. Dangling targets are filtered at the SQL layer (ka-0000002463
	// acceptance: dangling target id → edge omitted).
	//
	// relationship_type column added in migration 013 (epic ka-0000003840)
	// — typed mentions populate it; untyped rows leave it NULL.
	rows := sqlmock.NewRows([]string{"source_bead_id", "target_bead_id", "source_field", "relationship_type"}).
		AddRow("gd-FNHTKRBDIQ", "gd-HKBGWAOTHB", "description", nil).
		AddRow("gd-FNHTKRBDIQ", "gd-HKBGWAOTHB", "comment:7", "blocks").
		AddRow("gd-7YFI0RSI0U", "gd-FNHTKRBDIQ", "notes", nil)

	mock.ExpectQuery(`SELECT source_bead_id, target_bead_id, source_field, relationship_type\s+FROM bead_references\s+WHERE source_bead_id = ANY\(\$1::text\[\]\)\s+AND target_bead_id = ANY\(\$1::text\[\]\)`).
		WithArgs(pq.Array([]string{"gd-FNHTKRBDIQ", "gd-HKBGWAOTHB", "gd-7YFI0RSI0U"})).
		WillReturnRows(rows)

	result, err := queryGetTextReferencesForBeads(context.Background(), db,
		[]string{"gd-FNHTKRBDIQ", "gd-HKBGWAOTHB", "gd-7YFI0RSI0U"})
	if err != nil {
		t.Fatalf("queryGetTextReferencesForBeads: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("expected 3 reference rows, got %d", len(result))
	}
	if result[0].SourceBeadID != "gd-FNHTKRBDIQ" || result[0].TargetBeadID != "gd-HKBGWAOTHB" || result[0].SourceField != "description" {
		t.Fatalf("unexpected first row: %+v", result[0])
	}
	if result[0].RelationshipType != nil {
		t.Fatalf("row 0: want NULL relationship_type, got %q", *result[0].RelationshipType)
	}
	if result[1].RelationshipType == nil || *result[1].RelationshipType != "blocks" {
		t.Fatalf("row 1: want relationship_type=blocks, got %v", result[1].RelationshipType)
	}
	if result[2].RelationshipType != nil {
		t.Fatalf("row 2: want NULL relationship_type, got %q", *result[2].RelationshipType)
	}
}

// TestQueryListBeadsMentioning_TypeFilter_Verb confirms the typed-mention
// filter (T6, kbeads/docs/research-typed-mention-syntax.md § 10) appends the
// br.relationship_type = $N predicate to both the COUNT and SELECT queries
// and threads the verb through as the next placeholder.
func TestQueryListBeadsMentioning_TypeFilter_Verb(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM bead_references br\s+JOIN beads b ON b\.id = br\.source_bead_id\s+WHERE br\.target_bead_id = \$1 AND b\.deleted_at IS NULL AND br\.relationship_type = \$2`).
		WithArgs("gd-target00001", "parent").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	pageRows := sqlmock.NewRows([]string{"id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-ldkpaoah1s", "child of target", "task", "open", "description", "parent")
	mock.ExpectQuery(`SELECT b\.id, b\.title, b\.type, b\.status, br\.source_field, br\.relationship_type\s+FROM bead_references br\s+JOIN beads b ON b\.id = br\.source_bead_id\s+WHERE br\.target_bead_id = \$1 AND b\.deleted_at IS NULL AND br\.relationship_type = \$2\s+ORDER BY br\.extracted_at DESC, b\.id, br\.source_field\s+LIMIT \$3 OFFSET \$4`).
		WithArgs("gd-target00001", "parent", 50, 0).
		WillReturnRows(pageRows)

	verb := "parent"
	rows, total, err := queryListBeadsMentioning(context.Background(), db, "gd-target00001", 50, 0, &verb)
	if err != nil {
		t.Fatalf("queryListBeadsMentioning: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total=%d rows=%d, want 1/1", total, len(rows))
	}
	if rows[0].RelationshipType == nil || *rows[0].RelationshipType != "parent" {
		t.Errorf("rows[0].RelationshipType = %v, want 'parent'", rows[0].RelationshipType)
	}
}

// TestQueryListBeadsMentioning_TypeFilter_NullOnly confirms that a non-nil
// empty-string filter emits the "br.relationship_type IS NULL" predicate
// (untyped-mentions-only mode) and does NOT append a placeholder for the
// filter value.
func TestQueryListBeadsMentioning_TypeFilter_NullOnly(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\)\s+FROM bead_references br\s+JOIN beads b ON b\.id = br\.source_bead_id\s+WHERE br\.target_bead_id = \$1 AND b\.deleted_at IS NULL AND br\.relationship_type IS NULL`).
		WithArgs("gd-target00001").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	pageRows := sqlmock.NewRows([]string{"id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-ldkpaoah1s", "untyped mention", "task", "open", "description", nil)
	mock.ExpectQuery(`SELECT b\.id, b\.title, b\.type, b\.status, br\.source_field, br\.relationship_type\s+FROM bead_references br\s+JOIN beads b ON b\.id = br\.source_bead_id\s+WHERE br\.target_bead_id = \$1 AND b\.deleted_at IS NULL AND br\.relationship_type IS NULL\s+ORDER BY br\.extracted_at DESC, b\.id, br\.source_field\s+LIMIT \$2 OFFSET \$3`).
		WithArgs("gd-target00001", 50, 0).
		WillReturnRows(pageRows)

	empty := ""
	rows, total, err := queryListBeadsMentioning(context.Background(), db, "gd-target00001", 50, 0, &empty)
	if err != nil {
		t.Fatalf("queryListBeadsMentioning: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total=%d rows=%d, want 1/1", total, len(rows))
	}
	if rows[0].RelationshipType != nil {
		t.Errorf("rows[0].RelationshipType = %v, want nil for NULL-only filter", *rows[0].RelationshipType)
	}
}

// TestQueryListBeadReferencesFrom_TypeFilter_Verb verifies the same plumbing
// on the outgoing direction.
func TestQueryListBeadReferencesFrom_TypeFilter_Verb(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM bead_references br WHERE source_bead_id = \$1 AND br\.relationship_type = \$2`).
		WithArgs("gd-ldkpaoah1s", "blocks").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	pageRows := sqlmock.NewRows([]string{"target_bead_id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-FNHTKRBDIQ", "Blocked Bead", "task", "open", "description", "blocks")
	mock.ExpectQuery(`SELECT br\.target_bead_id,\s+COALESCE\(b\.title, ''\),\s+COALESCE\(b\.type, ''\),\s+COALESCE\(b\.status, ''\),\s+br\.source_field,\s+br\.relationship_type\s+FROM bead_references br\s+LEFT JOIN beads b ON b\.id = br\.target_bead_id AND b\.deleted_at IS NULL\s+WHERE br\.source_bead_id = \$1 AND br\.relationship_type = \$2\s+ORDER BY br\.source_field, br\.target_bead_id\s+LIMIT \$3 OFFSET \$4`).
		WithArgs("gd-ldkpaoah1s", "blocks", 50, 0).
		WillReturnRows(pageRows)

	verb := "blocks"
	rows, total, err := queryListBeadReferencesFrom(context.Background(), db, "gd-ldkpaoah1s", 50, 0, &verb)
	if err != nil {
		t.Fatalf("queryListBeadReferencesFrom: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total=%d rows=%d, want 1/1", total, len(rows))
	}
	if rows[0].RelationshipType == nil || *rows[0].RelationshipType != "blocks" {
		t.Errorf("rows[0].RelationshipType = %v, want 'blocks'", rows[0].RelationshipType)
	}
}

// TestQueryListBeadReferencesFrom_TypeFilter_NullOnly mirrors the NULL-only
// case for the outgoing direction.
func TestQueryListBeadReferencesFrom_TypeFilter_NullOnly(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM bead_references br WHERE source_bead_id = \$1 AND br\.relationship_type IS NULL`).
		WithArgs("gd-ldkpaoah1s").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	pageRows := sqlmock.NewRows([]string{"target_bead_id", "title", "type", "status", "source_field", "relationship_type"}).
		AddRow("gd-FNHTKRBDIQ", "Untyped target", "task", "open", "description", nil)
	mock.ExpectQuery(`WHERE br\.source_bead_id = \$1 AND br\.relationship_type IS NULL`).
		WithArgs("gd-ldkpaoah1s", 50, 0).
		WillReturnRows(pageRows)

	empty := ""
	rows, total, err := queryListBeadReferencesFrom(context.Background(), db, "gd-ldkpaoah1s", 50, 0, &empty)
	if err != nil {
		t.Fatalf("queryListBeadReferencesFrom: %v", err)
	}
	if total != 1 || len(rows) != 1 {
		t.Fatalf("total=%d rows=%d, want 1/1", total, len(rows))
	}
	if rows[0].RelationshipType != nil {
		t.Errorf("rows[0].RelationshipType = %v, want nil", *rows[0].RelationshipType)
	}
}

// ----------------------------------------------------------------------------
// M5/A auto-link: typed-mention deps promotion
// ----------------------------------------------------------------------------
//
// Tests below cover bulkInsertTypedDeps directly so the closed-verb-set
// promotion contract is pinned independently of replaceBeadReferences:
//
//   - direction correctness for each verb in the closed set
//     (parent/child-of source-as-bead_id; blocks flips to target-as-bead_id;
//     blocked-by source-as-bead_id);
//   - idempotency via ON CONFLICT (bead_id, depends_on_id, type) DO NOTHING;
//   - dedupe of forward+inverse pair (parent + child-of) to a single dep row;
//   - FK-violation skip when target bead is missing or cross-fleet;
//   - self-reference skip (target == source);
//   - unknown / non-promoted verbs do not emit dep rows (related, duplicates,
//     supersedes, triggered-by).
//
// See task ka-0000006487, design doc ka-0000000827 § M5/A, unit plan
// ka-0000004136.

// testOperatorID is the canned operators(id) FK every M5 unit test passes
// in as the source-bead operator. Real values come from the caller (a
// *model.Bead or *model.Comment) — the unit tests just need a non-nil
// pointer to satisfy the operator-inheritance contract.
var testOperatorID = func() *int64 { v := int64(42); return &v }()

const testOperator = "slack:UTEST"

// expectM5DepInsert mirrors expectTypedDepInsert in queries_references_typed_test.go.
// Local helper keeps test setup compact while keeping the SQL-shape regex
// pinned to the canonical insert exactly once.
func expectM5DepInsert(mock sqlmock.Sqlmock, beadID, dependsOnID string, depType model.DependencyType) {
	// SAVEPOINT-wrapped so a dangling target rolls back only this INSERT,
	// not the shared bead-write tx (ka-0000000195).
	mock.ExpectExec(`SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO deps \(bead_id, depends_on_id, type, created_at, created_by, operated_by_id, metadata, fleet\)`).
		WithArgs(beadID, dependsOnID, string(depType), sqlmock.AnyArg(), "kbeads/references/typed-mention", *testOperatorID, []byte("{}")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`RELEASE SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
}

// expectM5DepInsertOnConflict simulates the case where the dep row already
// exists — `ON CONFLICT DO NOTHING` returns (0, 0). The function must still
// succeed and proceed to the next ref.
func expectM5DepInsertOnConflict(mock sqlmock.Sqlmock, beadID, dependsOnID string, depType model.DependencyType) {
	// ON CONFLICT DO NOTHING returns (0, 0) but is NOT an error, so the
	// savepoint is still RELEASEd (not rolled back) — same bracket as a
	// successful insert (ka-0000000195).
	mock.ExpectExec(`SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO deps \(bead_id, depends_on_id, type, created_at, created_by, operated_by_id, metadata, fleet\)`).
		WithArgs(beadID, dependsOnID, string(depType), sqlmock.AnyArg(), "kbeads/references/typed-mention", *testOperatorID, []byte("{}")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`RELEASE SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
}

// TestBulkInsertTypedDeps_ClosedVerbSet_Direction is the table-driven
// direction-correctness check for the M5/A promotion verb set.
func TestBulkInsertTypedDeps_ClosedVerbSet_Direction(t *testing.T) {
	const source = "gd-ldkpaoah1s"
	const target = "gd-lzwcnercx2"
	tests := []struct {
		name        string
		verb        string
		beadID      string
		dependsOnID string
		depType     model.DependencyType
	}{
		{
			name:        "parent: source is child of target",
			verb:        "parent",
			beadID:      source,
			dependsOnID: target,
			depType:     model.DepParentChild,
		},
		{
			name:        "child-of: source is child of target (inverse, no flip)",
			verb:        "child-of",
			beadID:      source,
			dependsOnID: target,
			depType:     model.DepParentChild,
		},
		{
			name:        "blocks: target is the dependent of source (FLIP)",
			verb:        "blocks",
			beadID:      target,
			dependsOnID: source,
			depType:     model.DepBlocks,
		},
		{
			name:        "blocked-by: source is the dependent of target (no flip)",
			verb:        "blocked-by",
			beadID:      source,
			dependsOnID: target,
			depType:     model.DepBlocks,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db, mock := newMockDB(t)
			expectM5DepInsert(mock, tt.beadID, tt.dependsOnID, tt.depType)

			refs := []references.TypedReference{{TargetID: target, Verb: tt.verb}}
			if err := bulkInsertTypedDeps(context.Background(), db, source, testOperatorID, refs); err != nil {
				t.Fatalf("bulkInsertTypedDeps: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}

// TestBulkInsertTypedDeps_NonPromotedVerbs_NoInsert pins the negative
// contract: verbs outside the closed promotion set (related, duplicates,
// supersedes, triggered-by) and unknown verbs (Verb=="") do NOT emit a
// dep row. Their bead_references row continues to be written by the
// typed-refs path; the deps table remains untouched.
func TestBulkInsertTypedDeps_NonPromotedVerbs_NoInsert(t *testing.T) {
	verbs := []string{
		"related",
		"duplicates",
		"duplicated-by",
		"supersedes",
		"superseded-by",
		"triggered-by",
		"triggers",
		"", // unknown verb degrades to flat — no dep
	}
	for _, v := range verbs {
		t.Run("verb="+v, func(t *testing.T) {
			db, mock := newMockDB(t)
			refs := []references.TypedReference{{TargetID: "gd-lzwcnercx2", Verb: v}}
			if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs); err != nil {
				t.Fatalf("bulkInsertTypedDeps: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}

// TestBulkInsertTypedDeps_IdempotentOnConflict checks that a second pass
// over the same input is harmless: the SQL emits ON CONFLICT DO NOTHING,
// and a 0-rows-affected result is not treated as an error. Two
// consecutive calls produce two INSERTs; the second reflects the
// conflict-do-nothing path.
func TestBulkInsertTypedDeps_IdempotentOnConflict(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{{TargetID: "gd-lzwcnercx2", Verb: "parent"}}

	// First pass writes the row.
	expectM5DepInsert(mock, "gd-ldkpaoah1s", "gd-lzwcnercx2", model.DepParentChild)
	// Second pass hits the unique key — ON CONFLICT swallows it (0 rows).
	expectM5DepInsertOnConflict(mock, "gd-ldkpaoah1s", "gd-lzwcnercx2", model.DepParentChild)

	for pass := range 2 {
		if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs); err != nil {
			t.Fatalf("pass %d: bulkInsertTypedDeps: %v", pass, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestBulkInsertTypedDeps_OnConflictClauseIsPresent pins the SQL string —
// idempotency relies on the deps PK plus `ON CONFLICT (bead_id,
// depends_on_id, type) DO NOTHING`. A regression that drops the clause
// would surface here.
func TestBulkInsertTypedDeps_OnConflictClauseIsPresent(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{{TargetID: "gd-lzwcnercx2", Verb: "blocked-by"}}
	mock.ExpectExec(`SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO deps.*ON CONFLICT \(bead_id, depends_on_id, type\) DO NOTHING`).
		WithArgs("gd-ldkpaoah1s", "gd-lzwcnercx2", string(model.DepBlocks),
			sqlmock.AnyArg(), "kbeads/references/typed-mention", *testOperatorID, []byte("{}")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`RELEASE SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs); err != nil {
		t.Fatalf("bulkInsertTypedDeps: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestBulkInsertTypedDeps_NilOperator_UsesSentinelSubquery pins the
// operatedByID==nil path (ka-0000002664). When the source bead/comment has
// no operator FK, the INSERT must fill operated_by_id from the inline
// system:legacy-pre-attribution subquery (no $6 placeholder consumed) and
// bind metadata as $6 with EXACTLY six args. A regression that leaves
// metadata pinned at $7 in the nil branch supplies only six args for a
// seven-placeholder statement, which lib/pq rejects with a non-FK bind
// error that propagates out and 500s the outer bead/comment write — the
// exact failure this test guards against.
func TestBulkInsertTypedDeps_NilOperator_UsesSentinelSubquery(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{{TargetID: "gd-lzwcnercx2", Verb: "parent"}}
	// No operator arg: operated_by_id is the inline sentinel subquery and
	// metadata shifts to $6, so EXACTLY six args are bound. The query regex
	// pins the VALUES shape — sentinel subquery for operated_by_id followed
	// by `$6` for metadata — so a regression that leaves metadata at `$7`
	// (the seven-placeholder/six-arg bind mismatch) fails to match here even
	// though sqlmock does not itself enforce placeholder-vs-arg count.
	mock.ExpectExec(`SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`VALUES \(\s*\$1, \$2, \$3, \$4, \$5, \(SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution'\), \$6,`).
		WithArgs("gd-ldkpaoah1s", "gd-lzwcnercx2", string(model.DepParentChild),
			sqlmock.AnyArg(), "kbeads/references/typed-mention", []byte("{}")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`RELEASE SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))

	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", nil, refs); err != nil {
		t.Fatalf("bulkInsertTypedDeps with nil operator: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestBulkInsertTypedDeps_DedupesForwardAndInverse covers the case from
// the M0 fix (ka-0000006126): parent(X) and child-of(X) on the same
// (source, target) collapse to the SAME dep row in the deps table even
// though they are two distinct rows in bead_references. The dedupe is
// in-process (the (bead_id, depends_on_id, type) seen-map), so only one
// INSERT is emitted.
func TestBulkInsertTypedDeps_DedupesForwardAndInverse(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{
		{TargetID: "gd-lzwcnercx2", Verb: "parent"},
		{TargetID: "gd-lzwcnercx2", Verb: "child-of"},
	}
	// Exactly one dep INSERT — the second mention dedupes.
	expectM5DepInsert(mock, "gd-ldkpaoah1s", "gd-lzwcnercx2", model.DepParentChild)

	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs); err != nil {
		t.Fatalf("bulkInsertTypedDeps: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestBulkInsertTypedDeps_BlocksAndBlockedBy_DedupeToOneRow is the
// blocks-side counterpart: forward `blocks(X)` flips to (target,
// source); inverse `blocked-by(X)` keeps (source, target). They produce
// DIFFERENT dep rows — same DependencyType, opposite direction. So we
// expect TWO INSERTs in this case, NOT a dedupe (this pins the
// direction-aware seen-map keying — keying by (verb) would dedupe
// incorrectly).
func TestBulkInsertTypedDeps_BlocksAndBlockedBy_KeepBothDirections(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{
		{TargetID: "gd-lzwcnercx2", Verb: "blocks"},     // FLIP: target -> source
		{TargetID: "gd-lzwcnercx2", Verb: "blocked-by"}, // keep: source -> target
	}
	expectM5DepInsert(mock, "gd-lzwcnercx2", "gd-ldkpaoah1s", model.DepBlocks)
	expectM5DepInsert(mock, "gd-ldkpaoah1s", "gd-lzwcnercx2", model.DepBlocks)

	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs); err != nil {
		t.Fatalf("bulkInsertTypedDeps: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestBulkInsertTypedDeps_FKViolationSkipped covers the dangling-target
// case: the typed mention references a gd-id that doesn't exist (typo,
// soft-deleted, cross-fleet). The deps table has FKs to beads(id), so
// PG returns a foreign_key_violation. The function logs a skip and
// proceeds — the bead_references row is still fine, it just doesn't
// have a deps shadow.
func TestBulkInsertTypedDeps_FKViolationSkipped(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{
		{TargetID: "gd-zacqz1sgld", Verb: "parent"},
		{TargetID: "gd-lzwcnercx2", Verb: "blocks"},
	}
	// First INSERT fails with FK violation — the savepoint is rolled back
	// (recovering the shared tx from the aborted state) and the function
	// continues (ka-0000000195).
	mock.ExpectExec(`SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs("gd-ldkpaoah1s", "gd-zacqz1sgld", string(model.DepParentChild),
			sqlmock.AnyArg(), "kbeads/references/typed-mention", *testOperatorID, []byte("{}")).
		WillReturnError(&pq.Error{
			Code:    "23503",
			Message: "insert violates foreign key constraint deps_depends_on_id_fkey",
		})
	mock.ExpectExec(`ROLLBACK TO SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	// Second INSERT (different target) succeeds.
	expectM5DepInsert(mock, "gd-lzwcnercx2", "gd-ldkpaoah1s", model.DepBlocks)

	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs); err != nil {
		t.Fatalf("bulkInsertTypedDeps should swallow FK violation, got: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestBulkInsertTypedDeps_SelfReferenceSkipped pins the self-loop guard:
// a bead whose description contains a typed mention pointing at itself
// (e.g. an author's editorial mistake) MUST NOT emit a self-edge in the
// deps graph. The bead_references row is harmless (self-mentions are
// allowed there), but the deps cycle would break the dep graph
// invariants.
func TestBulkInsertTypedDeps_SelfReferenceSkipped(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{
		{TargetID: "gd-ldkpaoah1s", Verb: "parent"},
		{TargetID: "gd-ldkpaoah1s", Verb: "blocks"},
		{TargetID: "gd-ldkpaoah1s", Verb: "blocked-by"},
	}
	// No INSERTs expected — every ref resolves to (source, source) which
	// the self-loop guard drops before INSERT.

	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs); err != nil {
		t.Fatalf("bulkInsertTypedDeps: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestBulkInsertTypedDeps_EmptyInput is a no-op even with mock that
// expects nothing — a defensive check that the early-return on
// `len(typed) == 0` doesn't issue a stray query.
func TestBulkInsertTypedDeps_EmptyInput(t *testing.T) {
	db, mock := newMockDB(t)
	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, nil); err != nil {
		t.Fatalf("nil typed: %v", err)
	}
	if err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, []references.TypedReference{}); err != nil {
		t.Fatalf("empty typed: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestDepRowForTypedVerb_Table is a pure-Go unit test — no mock — that
// pins the (verb -> bead_id, depends_on_id, type) shape mapping. The
// table mirrors closedM5DepsVerbs plus a few negative cases.
func TestDepRowForTypedVerb_Table(t *testing.T) {
	const source = "gd-ldkpaoah1s"
	const target = "gd-lzwcnercx2"
	tests := []struct {
		verb        string
		wantOK      bool
		beadID      string
		dependsOnID string
		depType     model.DependencyType
	}{
		{verb: "parent", wantOK: true, beadID: source, dependsOnID: target, depType: model.DepParentChild},
		{verb: "child-of", wantOK: true, beadID: source, dependsOnID: target, depType: model.DepParentChild},
		{verb: "blocks", wantOK: true, beadID: target, dependsOnID: source, depType: model.DepBlocks},
		{verb: "blocked-by", wantOK: true, beadID: source, dependsOnID: target, depType: model.DepBlocks},
		// Outside the closed set:
		{verb: "related", wantOK: false},
		{verb: "duplicates", wantOK: false},
		{verb: "supersedes", wantOK: false},
		{verb: "triggered-by", wantOK: false},
		{verb: "", wantOK: false},
		{verb: "closes", wantOK: false}, // not canonical at all
	}
	for _, tt := range tests {
		t.Run(tt.verb, func(t *testing.T) {
			beadID, depID, dt, ok := depRowForTypedVerb(tt.verb, source, target)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if beadID != tt.beadID {
				t.Errorf("beadID = %q, want %q", beadID, tt.beadID)
			}
			if depID != tt.dependsOnID {
				t.Errorf("dependsOnID = %q, want %q", depID, tt.dependsOnID)
			}
			if dt != tt.depType {
				t.Errorf("depType = %q, want %q", dt, tt.depType)
			}
		})
	}
}

// TestBulkInsertTypedDeps_NonFKErrorAborts checks that a non-FK SQL
// error is NOT swallowed — it aborts the call and propagates up so the
// caller can roll back the enclosing tx. This guards the contract that
// only FK violations are tolerated; everything else is fatal.
func TestBulkInsertTypedDeps_NonFKErrorAborts(t *testing.T) {
	db, mock := newMockDB(t)
	refs := []references.TypedReference{{TargetID: "gd-lzwcnercx2", Verb: "parent"}}
	// SAVEPOINT is taken, INSERT errors with a non-FK error, and the
	// function returns without ROLLBACK TO — the tx is aborted and the
	// caller is expected to roll back the whole transaction (ka-0000000195).
	mock.ExpectExec(`SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs("gd-ldkpaoah1s", "gd-lzwcnercx2", string(model.DepParentChild),
			sqlmock.AnyArg(), "kbeads/references/typed-mention", *testOperatorID, []byte("{}")).
		WillReturnError(errors.New("connection reset by peer"))

	err := bulkInsertTypedDeps(context.Background(), db, "gd-ldkpaoah1s", testOperatorID, refs)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}
