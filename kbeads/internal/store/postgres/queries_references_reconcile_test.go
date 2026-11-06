package postgres

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"gardenio/kbeads/internal/model"
)

// Tests for reconcileTypedMentionDeps — Proposal B (ka-0000006581):
// reconcile-on-reextract auto-delete of stale parser-created parent-child
// dep rows. Full design + hazard/guardrail matrix:
// kbeads/docs/design-reconcile-typed-mention-deps.md.
//
// The function issues a single scoped DELETE:
//
//	DELETE FROM deps
//	WHERE bead_id = $1
//	  AND type = $2                       -- parent-child ONLY
//	  AND created_by = $3                 -- parser marker ONLY
//	  AND depends_on_id <> ALL($4::text[]) -- keep set from live text
//
// so the tests assert (a) the correct keep set is computed from the live
// fields (title/description/notes, and comments when the flag is on) and
// (b) the DELETE is scoped to type=parent-child + created_by=marker, which
// is what protects manual rows and blocks rows from ever being touched.

// reconcileDeleteRe matches the scoped parent-child DELETE reconcile emits.
var reconcileDeleteRe = regexp.MustCompile(
	`DELETE FROM deps\s+WHERE bead_id = \$1\s+AND type = \$2\s+AND created_by = \$3\s+AND depends_on_id <> ALL\(\$4::text\[\]\)`)

// expectReconcileDelete asserts the scoped DELETE with the exact keep set.
// keep must already be sorted (reconcile sorts for deterministic binds).
func expectReconcileDelete(mock sqlmock.Sqlmock, beadID string, keep []string, rowsDeleted int64) {
	mock.ExpectExec(reconcileDeleteRe.String()).
		WithArgs(beadID, string(model.DepParentChild), typedMentionDepCreator, pq.Array(keep)).
		WillReturnResult(sqlmock.NewResult(0, rowsDeleted))
}

// When the last parent mention is removed from every field, the keep set is
// empty, so `depends_on_id <> ALL('{}')` is vacuously true and every
// parser-created parent-child row for the bead is deleted. This is the core
// behavior: deleting `parent: gd-X` text removes the edge automatically.
func TestReconcileTypedMentionDeps_MentionRemoved_DeletesAll(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	// No parent mention anywhere → empty keep → delete all parser rows.
	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "no parents here", Description: "plain text", Notes: ""}
	expectReconcileDelete(mock, bead.ID, []string{}, 1)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A live `parent: gd-X` in the title keeps gd-X (it is in the keep set, so it
// is excluded from the delete) while any other parser parent-child row is
// removed.
func TestReconcileTypedMentionDeps_MentionInTitle_KeepsTarget(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "work parent(gd-3qj45ohhrr)", Description: "", Notes: ""}
	expectReconcileDelete(mock, bead.ID, []string{"gd-3qj45ohhrr"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A mention present in the description (not the title) is still discovered —
// reconcile scans title, description AND notes.
func TestReconcileTypedMentionDeps_MentionInDescription_KeepsTarget(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "plain", Description: "parent: gd-3qj45ohhrr", Notes: ""}
	expectReconcileDelete(mock, bead.ID, []string{"gd-3qj45ohhrr"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A mention present only in notes is discovered too.
func TestReconcileTypedMentionDeps_MentionInNotes_KeepsTarget(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "plain", Description: "", Notes: "child-of(gd-3qj45ohhrr)"}
	expectReconcileDelete(mock, bead.ID, []string{"gd-3qj45ohhrr"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// Two distinct parents across fields → both kept, in sorted order (the keep
// set is sorted for deterministic bind order).
func TestReconcileTypedMentionDeps_MultipleParents_KeepsAllSorted(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	// One parent in title, another in description → both kept, sorted.
	bead := &model.Bead{
		ID:          "gd-iacjlrmpg4",
		Title:       "parent(gd-fz47twzqoy)",
		Description: "parent: gd-ykqbdxnb0t",
		Notes:       "",
	}
	expectReconcileDelete(mock, bead.ID, []string{"gd-fz47twzqoy", "gd-ykqbdxnb0t"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// The same target mentioned in more than one field is de-duplicated into a
// single keep entry (liveParents is a set).
func TestReconcileTypedMentionDeps_DuplicateTargetAcrossFields_Dedup(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	bead := &model.Bead{
		ID:          "gd-iacjlrmpg4",
		Title:       "parent(gd-3qj45ohhrr)",
		Description: "also parent: gd-3qj45ohhrr",
		Notes:       "",
	}
	expectReconcileDelete(mock, bead.ID, []string{"gd-3qj45ohhrr"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// With extractComments OFF, reconcile does NOT read the comments table — the
// keep set is derived from fields only. (No SELECT expectation is queued, so
// an unexpected comment query would fail the mock.)
func TestReconcileTypedMentionDeps_CommentsFlagOff_NoCommentQuery(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "parent(gd-3qj45ohhrr)", Description: "", Notes: ""}
	expectReconcileDelete(mock, bead.ID, []string{"gd-3qj45ohhrr"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// With extractComments ON, reconcile UNIONS parent-edge targets named by live
// comments into the keep set. Comments are immutable, so a comment-sourced
// parent edge must never be reconciled away by a body edit that dropped it.
// Here the body has no parent mention but a comment does — the comment target
// survives.
func TestReconcileTypedMentionDeps_CommentsFlagOn_KeepsCommentSourced(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "no parent in body", Description: "", Notes: ""}

	// Comment carries the only live parent mention.
	rows := sqlmock.NewRows([]string{"text"}).
		AddRow("parent: gd-sjur6jr1iq").
		AddRow("unrelated note")
	mock.ExpectQuery(`SELECT text FROM comments WHERE bead_id = \$1`).
		WithArgs(bead.ID).
		WillReturnRows(rows)

	// Comment-sourced target is kept; nothing else.
	expectReconcileDelete(mock, bead.ID, []string{"gd-sjur6jr1iq"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, true); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// With extractComments ON, the keep set is the UNION of field targets and
// comment targets (both preserved).
func TestReconcileTypedMentionDeps_CommentsFlagOn_UnionsFieldAndComment(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "parent(gd-ykqbdxnb0t)", Description: "", Notes: ""}

	rows := sqlmock.NewRows([]string{"text"}).
		AddRow("parent: gd-fz47twzqoy")
	mock.ExpectQuery(`SELECT text FROM comments WHERE bead_id = \$1`).
		WithArgs(bead.ID).
		WillReturnRows(rows)

	// Union of the field mention and the comment mention, sorted.
	expectReconcileDelete(mock, bead.ID, []string{"gd-fz47twzqoy", "gd-ykqbdxnb0t"}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, true); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

// A `blocks(gd-X)` mention is NOT a parent edge, so it never enters the keep
// set and never affects the parent-child-scoped DELETE. (The keep set stays
// empty; the DELETE only ever touches type=parent-child rows anyway.)
func TestReconcileTypedMentionDeps_BlocksMention_NotAParentEdge(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	// Only a blocks mention → no parent targets → empty keep set. The DELETE
	// is scoped to type=parent-child, so blocks dep rows are never eligible.
	bead := &model.Bead{ID: "gd-iacjlrmpg4", Title: "blocks(gd-cvrijccj5a)", Description: "", Notes: ""}
	expectReconcileDelete(mock, bead.ID, []string{}, 0)

	if err := reconcileTypedMentionDeps(context.Background(), db, bead, false); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
