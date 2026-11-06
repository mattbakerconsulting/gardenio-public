package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"gardenio/kbeads/internal/model"
)

// Comment-text reference extraction is gated behind a config flag
// (extractCommentRefs / BEADS_REFERENCES_EXTRACT_COMMENTS). Default is OFF
// per decision posture on ka-0000000868: deployments opt in, run the
// reextract-references backfill, then flip on. These tests pin both
// halves of that contract — flag-off skips the bead_references write,
// flag-on still writes — so future refactors can't silently re-enable
// comment extraction.

func TestPostgresStore_AddComment_FlagOff_SkipsCommentReferences(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db, extractCommentRefs: false}

	now := time.Now().UTC()
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO comments").
		WithArgs("gd-zrem5da0ro", "alice", "ref gd-FNHTKRBDIQ", sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(99), now))
	// Crucially: no DELETE/INSERT against bead_references when the flag is off.
	mock.ExpectCommit()

	c := &model.Comment{BeadID: "gd-zrem5da0ro", Author: "alice", Text: "ref gd-FNHTKRBDIQ"}
	if err := s.AddComment(context.Background(), c); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if c.ID != 99 {
		t.Errorf("comment ID = %d, want 99", c.ID)
	}
}

func TestPostgresStore_AddComment_FlagOn_WritesCommentReferences(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db, extractCommentRefs: true}

	now := time.Now().UTC()
	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO comments").
		WithArgs("gd-zrem5da0ro", "alice", "ref gd-FNHTKRBDIQ", sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), now))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-zrem5da0ro", "comment:7").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-zrem5da0ro", "comment:7", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	c := &model.Comment{BeadID: "gd-zrem5da0ro", Author: "alice", Text: "ref gd-FNHTKRBDIQ"}
	if err := s.AddComment(context.Background(), c); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
}

func TestSetExtractCommentReferences_TogglesFlag(t *testing.T) {
	s := &PostgresStore{}
	if s.extractCommentRefs {
		t.Fatalf("default extractCommentRefs should be false")
	}
	s.SetExtractCommentReferences(true)
	if !s.extractCommentRefs {
		t.Fatalf("expected extractCommentRefs=true after SetExtractCommentReferences(true)")
	}
	s.SetExtractCommentReferences(false)
	if s.extractCommentRefs {
		t.Fatalf("expected extractCommentRefs=false after SetExtractCommentReferences(false)")
	}
}

func TestQueryReextractReferencesPage_FlagOff_SkipsCommentIteration(t *testing.T) {
	db, mock := newMockDB(t)

	// One bead in the page, with no comment iteration when the flag is off.
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
	// No SELECT id, text FROM comments — the flag-off path must not touch
	// the comments table at all.
	mock.ExpectCommit()

	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, false, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.CommentsScanned != 0 {
		t.Errorf("CommentsScanned = %d, want 0 (flag off)", stats.CommentsScanned)
	}
	if stats.BeadsScanned != 1 {
		t.Errorf("BeadsScanned = %d, want 1", stats.BeadsScanned)
	}
}

func TestQueryReextractReferencesPage_FlagOn_IteratesComments(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
			AddRow("gd-fnhtkrbdiq", "", "", "", nil))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).AddRow("", "", "", nil, *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-fnhtkrbdiq", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// One comment with a gd-id reference — flag-on backfill path must
	// iterate, scan, and replaceCommentReferences for it.
	mock.ExpectQuery(`SELECT id, text, operated_by_id FROM comments WHERE bead_id = \$1 ORDER BY id ASC`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"id", "text", "operated_by_id"}).
			AddRow(int64(13), "fixed by gd-5VCLRIVK3G", *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-fnhtkrbdiq", "comment:13").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references`).
		WithArgs("gd-fnhtkrbdiq", "comment:13", "gd-5VCLRIVK3G").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, true, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.CommentsScanned != 1 {
		t.Errorf("CommentsScanned = %d, want 1 (flag on, one comment)", stats.CommentsScanned)
	}
	if stats.ReferencesWritten != 1 {
		t.Errorf("ReferencesWritten = %d, want 1", stats.ReferencesWritten)
	}
}

// TestPostgresStore_MultipleComments_DistinctSourceFields exercises the
// per-comment scope contract: each comment's references are stored under
// source_field="comment:<id>" so writes from one comment don't disturb
// rows emitted by another comment on the same bead.
func TestPostgresStore_MultipleComments_DistinctSourceFields(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db, extractCommentRefs: true}

	now := time.Now().UTC()
	for _, tc := range []struct {
		id   int64
		text string
	}{
		{1, "see gd-FNHTKRBDIQ"},
		{2, "also see gd-HKBGWAOTHB"},
	} {
		mock.ExpectBegin()
		mock.ExpectQuery("INSERT INTO comments").
			WithArgs("gd-zrem5da0ro", "alice", tc.text, sqlmock.AnyArg(), "").
			WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(tc.id, now))
		// The DELETE is scoped per comment id — adding comment 2 must not
		// touch the rows emitted for comment 1.
		mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
			WithArgs("gd-zrem5da0ro", commentSourceField(tc.id)).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`INSERT INTO bead_references`).
			WithArgs("gd-zrem5da0ro", commentSourceField(tc.id), sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectCommit()

		c := &model.Comment{BeadID: "gd-zrem5da0ro", Author: "alice", Text: tc.text}
		if err := s.AddComment(context.Background(), c); err != nil {
			t.Fatalf("AddComment(%d): %v", tc.id, err)
		}
	}
}

// commentSourceField mirrors replaceCommentReferences' field naming so the
// expectation tracks the production code without re-importing the constant.
func commentSourceField(id int64) string {
	return refFieldCommentPrefix + itoa(id)
}

// itoa is a tiny base-10 helper to avoid pulling strconv into the test for
// a single conversion.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
