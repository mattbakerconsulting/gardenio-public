package postgres

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/references"
)

// expectTypedDepInsert is the sqlmock helper that asserts the
// per-typed-mention dep row written by bulkInsertTypedDeps. Used wherever
// a closed-set verb (parent, child-of, blocks, blocked-by) would emit a
// dep row in addition to the bead_references row. Direction is the
// caller's responsibility — it must already match the depRowForTypedVerb
// convention (source-as-bead_id for parent/child-of/blocked-by, flipped
// to target-as-bead_id for forward `blocks`).
func expectTypedDepInsert(mock sqlmock.Sqlmock, beadID, dependsOnID string, depType model.DependencyType) {
	// Each dep INSERT is now wrapped in a SAVEPOINT so an FK violation
	// only rolls back that one INSERT, not the shared bead-write tx
	// (ka-0000000195). A successful INSERT is bracketed by SAVEPOINT …
	// RELEASE SAVEPOINT.
	mock.ExpectExec(`SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO deps \(bead_id, depends_on_id, type, created_at, created_by, operated_by_id, metadata, fleet\)`).
		WithArgs(beadID, dependsOnID, string(depType), sqlmock.AnyArg(), "kbeads/references/typed-mention", sqlmock.AnyArg(), []byte("{}")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`RELEASE SAVEPOINT typed_dep_sp`).WillReturnResult(sqlmock.NewResult(0, 0))
}

// Tests for typed-mention extraction wired through the write path.
// Cover the lifecycle cases enumerated in
// kbeads/docs/research-typed-mention-syntax.md § 6:
//
//   - typed mention round-trips into a relationship_type row alongside
//     the flat-mention row from the bare-id Extract pass;
//   - removing the typed verb on the next write deletes the typed row
//     (DELETE-then-bulk-INSERT lifecycle, no orphan);
//   - editing parent(gd-X) → child-of(gd-X) replaces the typed row;
//   - parent(gd-X) and blocks(gd-X) on the same field coexist as two
//     distinct typed rows (different relationship_type);
//   - unknown verbs (closes, fixes, …) degrade silently to the existing
//     flat mention — no relationship_type row;
//   - per-comment scope holds for typed mentions.

// Typed mentions of `parent(gd-X)` produce both an untyped row (from the
// bare gd-X picked up by references.Extract) and a typed row with
// relationship_type='parent' (the surface verb the author wrote). Per-field
// flow: DELETE all field buckets, then untyped INSERT for the field, then
// typed INSERT keyed by relationship_type.
func TestReplaceBeadReferences_TypedMention_EmitsTypedRow(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:           "gd-ldkpaoah1s",
		Description:  "see parent(gd-FNHTKRBDIQ)",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Bare-id Extract still matches gd-FNHTKRBDIQ inside the parens, so
	// the untyped row is also written. relationship_type is NULL here
	// (column omitted from the INSERT).
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Typed row carries the surface verb in the relationship_type column,
	// preserving forward/inverse direction (M0 of ka-0000000933).
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "parent", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link: typed mention `parent(gd-X)` also emits a deps row
	// (source as child of target).
	expectTypedDepInsert(mock, "gd-ldkpaoah1s", "gd-FNHTKRBDIQ", model.DepParentChild)

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Two typed verbs that map to different DependencyTypes on the same
// (source, target, field) coexist as two distinct typed rows. The unique
// index keys on relationship_type (with COALESCE for NULL untyped rows),
// so this is the case the schema migration (013) was shaped to support.
func TestReplaceBeadReferences_TwoTypedMentions_SameTarget_SameField(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:           "gd-ldkpaoah1s",
		Description:  "parent(gd-FNHTKRBDIQ) and blocks(gd-FNHTKRBDIQ)",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Bare-id Extract collapses the duplicate gd-FNHTKRBDIQ to one row.
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Typed inserts are emitted in lexicographic surface-verb order:
	// "blocks" before "parent".
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "blocks", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "parent", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link: dep emit follows the typed slice order (left-to-right
	// in the source text), not the lex-sorted typed-refs INSERT order.
	// "parent(gd-A) and blocks(gd-A)" → parent dep first, then blocks dep
	// (which flips so gd-A becomes the dependent of source).
	expectTypedDepInsert(mock, "gd-ldkpaoah1s", "gd-FNHTKRBDIQ", model.DepParentChild)
	expectTypedDepInsert(mock, "gd-FNHTKRBDIQ", "gd-ldkpaoah1s", model.DepBlocks)

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Removing the typed verb on the next write must drop the typed row.
// The DELETE-then-bulk-INSERT lifecycle handles this for free: the next
// replace call deletes everything in the field bucket, and the new pass
// has no typed mention to re-INSERT.
func TestReplaceBeadReferences_TypedMentionRemoved_DeletesTypedRow(t *testing.T) {
	db, mock := newMockDB(t)
	// New text has no verb form — only a bare gd-id.
	bead := &model.Bead{
		ID:           "gd-ldkpaoah1s",
		Description:  "see gd-FNHTKRBDIQ",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Crucially: no typed INSERT — no `relationship_type` column should
	// appear in the INSERT shape this pass.

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Editing parent(gd-X) → child-of(gd-X) is a replace: the new write deletes
// the source_field bucket and re-inserts under the new surface verb. Under
// surface-verb storage (M0 of ka-0000000933) "parent" and "child-of" are
// distinct relationship_type values — the prior row was 'parent', the new
// pass writes 'child-of'.
func TestReplaceBeadReferences_EditTypedVerb_ReplacesTypedRow(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:           "gd-ldkpaoah1s",
		Description:  "child-of(gd-FNHTKRBDIQ)",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "child-of", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link: child-of is the inverse of parent and shares the
	// parent-child DependencyType with source-as-bead_id orientation.
	expectTypedDepInsert(mock, "gd-ldkpaoah1s", "gd-FNHTKRBDIQ", model.DepParentChild)

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// An unknown verb (e.g. "closes") falls outside the closed core. It must
// degrade silently to a flat mention — the bare-id Extract pass already
// covers this — and emit no typed INSERT. The verb itself is not stored
// (that would need an additional column; T1 schema does not add one).
func TestReplaceBeadReferences_UnknownVerb_DegradesToFlatMention(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:           "gd-ldkpaoah1s",
		Description:  "closes(gd-FNHTKRBDIQ)",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No typed INSERT — unknown verb degrades to flat mention.

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Typed mentions across all three text fields each fan out into per-field
// typed INSERTs. This exercises the "title, description, notes" column
// loop in replaceBeadReferences, ensuring each field's typed extractions
// are routed under the correct source_field bucket.
func TestReplaceBeadReferences_TypedMentions_AllFields(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:           "gd-ldkpaoah1s",
		Title:        "parent(gd-FNHTKRBDIQ)",
		Description:  "blocks(gd-HKBGWAOTHB)",
		Notes:        "related(gd-7YFI0RSI0U)",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// title — untyped then typed (surface verb "parent")
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "title", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "title", "parent", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A: parent → parent-child dep, source-as-bead_id.
	expectTypedDepInsert(mock, "gd-ldkpaoah1s", "gd-FNHTKRBDIQ", model.DepParentChild)
	// description — untyped then typed (surface verb "blocks")
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-HKBGWAOTHB").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "blocks", "gd-HKBGWAOTHB").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A: forward `blocks(gd-B)` flips so gd-B becomes the dependent of source.
	expectTypedDepInsert(mock, "gd-HKBGWAOTHB", "gd-ldkpaoah1s", model.DepBlocks)
	// notes — untyped then typed (surface verb "related"). `related` is NOT
	// in the M5 closed promotion set, so no deps INSERT is expected here.
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "notes", "gd-7YFI0RSI0U").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "notes", "related", "gd-7YFI0RSI0U").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Typed mention inside a comment writes a typed row keyed under the
// per-comment source_field. Replacing that comment text on edit must
// clear and re-INSERT the typed row.
func TestReplaceCommentReferences_TypedMention(t *testing.T) {
	db, mock := newMockDB(t)
	c := &model.Comment{
		ID:           42,
		BeadID:       "gd-zrem5da0ro",
		Text:         "parent(gd-5VCLRIVK3G)",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-zrem5da0ro", "comment:42").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-zrem5da0ro", "comment:42", "gd-5VCLRIVK3G").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-zrem5da0ro", "comment:42", "parent", "gd-5VCLRIVK3G").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link applies to comment-sourced typed mentions too.
	expectTypedDepInsert(mock, "gd-zrem5da0ro", "gd-5VCLRIVK3G", model.DepParentChild)

	if err := replaceCommentReferences(context.Background(), db, c); err != nil {
		t.Fatalf("replaceCommentReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Edit a comment to remove the verb form: the next replaceCommentReferences
// pass deletes the typed row and only re-INSERTs the flat mention.
func TestReplaceCommentReferences_TypedMentionRemoved_OnEdit(t *testing.T) {
	db, mock := newMockDB(t)
	c := &model.Comment{
		ID:           42,
		BeadID:       "gd-zrem5da0ro",
		Text:         "see gd-5VCLRIVK3G",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-zrem5da0ro", "comment:42").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-zrem5da0ro", "comment:42", "gd-5VCLRIVK3G").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := replaceCommentReferences(context.Background(), db, c); err != nil {
		t.Fatalf("replaceCommentReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Forward verb (parent) and inverse verb (child-of) on the same target in
// the same field MUST produce two distinct typed rows under surface-verb
// storage (M0 of auto-link epic ka-0000000933 / fix ka-0000006126). The
// inverse direction is preserved because the consumer-side reversal needs
// to know which surface form was written; collapsing both onto a single
// canonical "parent-child" row erases that signal. Inserts are emitted in
// lexicographic surface-verb order: "child-of" before "parent".
func TestReplaceBeadReferences_ForwardAndInverseVerb_PreserveDirection(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:           "gd-ldkpaoah1s",
		Description:  "parent(gd-FNHTKRBDIQ) and child-of(gd-FNHTKRBDIQ)",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-ldkpaoah1s", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Two distinct typed INSERTs, one per surface verb, lexicographic order.
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "child-of", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-ldkpaoah1s", "description", "parent", "gd-FNHTKRBDIQ").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link: parent and child-of share the parent-child
	// DependencyType with source-as-bead_id orientation, so the two typed
	// rows dedupe to a single dep INSERT — emitted in slice order
	// (parent comes first in the source text).
	expectTypedDepInsert(mock, "gd-ldkpaoah1s", "gd-FNHTKRBDIQ", model.DepParentChild)

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Table-driven coverage that every inverse-verb pair produces two distinct
// typed rows. This is the M0 acceptance check: bulkInsertTypedRefs for a
// forward verb and its inverse on the same (source, target, field) must
// emit two INSERTs with distinct relationship_type values. Each pair is
// run in isolation so the per-verb INSERT order assertion stays simple.
func TestBulkInsertTypedRefs_InverseVerbPairs_ProduceDistinctRows(t *testing.T) {
	tests := []struct {
		name        string
		forward     string
		inverse     string
		firstInsert string // lex-min of forward/inverse
		secondInert string // lex-max of forward/inverse
	}{
		{
			name:        "parent / child-of",
			forward:     "parent",
			inverse:     "child-of",
			firstInsert: "child-of",
			secondInert: "parent",
		},
		{
			name:        "blocks / blocked-by",
			forward:     "blocks",
			inverse:     "blocked-by",
			firstInsert: "blocked-by",
			secondInert: "blocks",
		},
		{
			name:        "duplicates / duplicated-by",
			forward:     "duplicates",
			inverse:     "duplicated-by",
			firstInsert: "duplicated-by",
			secondInert: "duplicates",
		},
		{
			name:        "supersedes / superseded-by",
			forward:     "supersedes",
			inverse:     "superseded-by",
			firstInsert: "superseded-by",
			secondInert: "supersedes",
		},
		{
			name:        "triggered-by / triggers",
			forward:     "triggered-by",
			inverse:     "triggers",
			firstInsert: "triggered-by",
			secondInert: "triggers",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newMockDB(t)
			refs := []references.TypedReference{
				{TargetID: "gd-FNHTKRBDIQ", Verb: tc.forward},
				{TargetID: "gd-FNHTKRBDIQ", Verb: tc.inverse},
			}
			// Two distinct rows, lex-ordered per-verb groups.
			mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
				WithArgs("gd-ldkpaoah1s", "description", tc.firstInsert, "gd-FNHTKRBDIQ").
				WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
				WithArgs("gd-ldkpaoah1s", "description", tc.secondInert, "gd-FNHTKRBDIQ").
				WillReturnResult(sqlmock.NewResult(0, 1))

			if err := bulkInsertTypedRefs(context.Background(), db, "gd-ldkpaoah1s", "description", refs); err != nil {
				t.Fatalf("bulkInsertTypedRefs: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("expectations: %v", err)
			}
		})
	}
}

// Inverse-direction acceptance from ka-0000006126:
// bulkInsertTypedRefs(blocked-by=[X]) and bulkInsertTypedRefs(blocks=[X])
// produce two distinct rows queryable in both directions. Runs each call
// independently and asserts the surface verb is stored as written.
func TestBulkInsertTypedRefs_BlocksAndBlockedBy_AreSeparateRows(t *testing.T) {
	t.Run("forward only: blocks", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
			WithArgs("gd-ldkpaoah1s", "description", "blocks", "gd-FNHTKRBDIQ").
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := bulkInsertTypedRefs(context.Background(), db, "gd-ldkpaoah1s", "description",
			[]references.TypedReference{{TargetID: "gd-FNHTKRBDIQ", Verb: "blocks"}}); err != nil {
			t.Fatalf("bulkInsertTypedRefs: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
	t.Run("inverse only: blocked-by", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
			WithArgs("gd-ldkpaoah1s", "description", "blocked-by", "gd-FNHTKRBDIQ").
			WillReturnResult(sqlmock.NewResult(0, 1))
		if err := bulkInsertTypedRefs(context.Background(), db, "gd-ldkpaoah1s", "description",
			[]references.TypedReference{{TargetID: "gd-FNHTKRBDIQ", Verb: "blocked-by"}}); err != nil {
			t.Fatalf("bulkInsertTypedRefs: %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("expectations: %v", err)
		}
	})
}

// Empty text: bulkInsertTypedRefs is a no-op and emits no typed INSERT.
// (The DELETE still fires; the untyped Extract returns nil, so no untyped
// INSERT either.) Pins the no-typed-rows pathway against future
// regressions that might emit a zero-row INSERT.
func TestReplaceBeadReferences_NoTypedMentions_NoTypedInsert(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{
		ID:           "gd-brznypltde",
		Description:  "no refs at all",
		OperatedBy:   testOperator,
		OperatedByID: testOperatorID,
	}

	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-brznypltde", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := replaceBeadReferences(context.Background(), db, bead); err != nil {
		t.Fatalf("replaceBeadReferences: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Tests for the typed-mention backfill (T4 of epic ka-0000003840). The
// backfill layers typed rows on top of the flat rows that
// replaceBeadReferences/replaceCommentReferences already write — so a
// single bead's text yielding both `gd-X` and `parent(gd-X)` produces
// two rows: one with relationship_type=NULL and one with
// relationship_type='parent'. Both coexist on the extended unique index.

func TestCountCanonicalTyped(t *testing.T) {
	tests := []struct {
		name string
		in   []references.TypedReference
		want int
	}{
		{name: "empty", in: nil, want: 0},
		{
			name: "all unknown verbs skipped",
			in: []references.TypedReference{
				{TargetID: "gd-FNHTKRBDIQ", Verb: "", RawVerb: "closes"},
				{TargetID: "gd-HKBGWAOTHB", Verb: "", RawVerb: "fixes"},
			},
			want: 0,
		},
		{
			name: "two distinct typed rows",
			in: []references.TypedReference{
				{TargetID: "gd-FNHTKRBDIQ", Verb: "parent"},
				{TargetID: "gd-HKBGWAOTHB", Verb: "blocks"},
			},
			want: 2,
		},
		{
			name: "duplicate (target, verb) collapses",
			in: []references.TypedReference{
				{TargetID: "gd-FNHTKRBDIQ", Verb: "parent"},
				{TargetID: "gd-FNHTKRBDIQ", Verb: "parent"},
			},
			want: 1,
		},
		{
			name: "same target with different verbs are distinct",
			in: []references.TypedReference{
				{TargetID: "gd-FNHTKRBDIQ", Verb: "parent"},
				{TargetID: "gd-FNHTKRBDIQ", Verb: "blocks"},
				{TargetID: "gd-FNHTKRBDIQ", Verb: "", RawVerb: "tracks"},
			},
			want: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := countCanonicalTyped(tt.in); got != tt.want {
				t.Errorf("countCanonicalTyped = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestQueryReextractReferencesPage_TypedBackfill_PopulatesRelationshipType
// is the integration-shaped check: a bead whose description contains both
// `gd-X` (flat) and `parent(gd-X)` (typed) must produce a flat row with
// relationship_type=NULL AND a typed row with relationship_type='parent'
// after one backfill pass. The flat path is unchanged from the pre-T4
// behavior; the new typed path is the addition under test.
func TestQueryReextractReferencesPage_TypedBackfill_PopulatesRelationshipType(t *testing.T) {
	db, mock := newMockDB(t)

	// Page list: one bead.
	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
			AddRow("gd-fnhtkrbdiq", "", "parent(ka-0000001713) and gd-1VBHX67QHQ", "", nil))

	// Per-bead tx.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).
			AddRow("", "parent(ka-0000001713) and gd-1VBHX67QHQ", "", nil, *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-fnhtkrbdiq", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Flat insert: description contains both ka-0000001713 and gd-1VBHX67QHQ.
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-fnhtkrbdiq", "description", "ka-0000001713", "gd-1VBHX67QHQ").
		WillReturnResult(sqlmock.NewResult(0, 2))
	// Typed insert: description's parent(ka-0000001713) yields one typed row.
	// Title and notes are empty so no other typed inserts run. Args are
	// [source, source_field, relationship_type, target] per bulkInsertTypedRefs;
	// relationship_type is the surface verb ("parent") as of M0 (ka-0000006126),
	// not the canonical DependencyType.
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-fnhtkrbdiq", "description", "parent", "ka-0000001713").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link: typed parent mention promotes to a dep row.
	expectTypedDepInsert(mock, "gd-fnhtkrbdiq", "ka-0000001713", model.DepParentChild)
	mock.ExpectCommit()

	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, false, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.BeadsScanned != 1 {
		t.Errorf("BeadsScanned = %d, want 1", stats.BeadsScanned)
	}
	// 2 flat (gd-X, gd-Y) + 1 typed (parent gd-X) = 3.
	if stats.ReferencesWritten != 3 {
		t.Errorf("ReferencesWritten = %d, want 3", stats.ReferencesWritten)
	}
	if stats.TotalReferences != 3 {
		t.Errorf("TotalReferences = %d, want 3", stats.TotalReferences)
	}
}

// TestQueryReextractReferencesPage_TypedBackfill_Idempotent runs the same
// per-bead extraction twice in sequence and asserts the second pass issues
// the identical sequence of writes (DELETE + same INSERTs). Because each
// pass deletes the source_field rows before re-inserting, and ON CONFLICT
// DO NOTHING guards the typed insert defensively, two consecutive runs
// converge to the same post-state.
func TestQueryReextractReferencesPage_TypedBackfill_Idempotent(t *testing.T) {
	db, mock := newMockDB(t)

	for pass := range 2 {
		mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
			WithArgs("", 500).
			WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
				AddRow("gd-fnhtkrbdiq", "blocks(gd-HKBGWAOTHB)", "", "", nil))

		mock.ExpectBegin()
		mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
			WithArgs("gd-fnhtkrbdiq").
			WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).
				AddRow("blocks(gd-HKBGWAOTHB)", "", "", nil, *testOperatorID))
		mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
			WithArgs("gd-fnhtkrbdiq", sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
			WithArgs("gd-fnhtkrbdiq", "title", "gd-HKBGWAOTHB").
			WillReturnResult(sqlmock.NewResult(0, 1))
		// Surface verb "blocks" (M0): the forward verb stores as-is.
		mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
			WithArgs("gd-fnhtkrbdiq", "title", "blocks", "gd-HKBGWAOTHB").
			WillReturnResult(sqlmock.NewResult(0, 1))
		// M5/A auto-link: forward `blocks(gd-B)` flips so gd-B is the dependent.
		expectTypedDepInsert(mock, "gd-HKBGWAOTHB", "gd-fnhtkrbdiq", model.DepBlocks)
		mock.ExpectCommit()

		mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

		stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, false, false, "")
		if err != nil {
			t.Fatalf("pass %d: queryReextractReferencesPage: %v", pass, err)
		}
		if stats.ReferencesWritten != 2 {
			t.Errorf("pass %d: ReferencesWritten = %d, want 2", pass, stats.ReferencesWritten)
		}
	}
}

// TestQueryReextractReferencesPage_TypedBackfill_MixedRowsConverge models
// the pre-T4 production state: the table contains only flat rows
// (relationship_type=NULL) for some beads, and possibly already-typed
// rows for beads that landed under a deployment that included T3. After
// one backfill pass, every bead reaches the expected typed state — the
// per-bead DELETE wipes the field's rows and the re-extract re-emits both
// flat and typed for the current text, regardless of the prior state.
func TestQueryReextractReferencesPage_TypedBackfill_MixedRowsConverge(t *testing.T) {
	db, mock := newMockDB(t)

	// Two beads in the page:
	//   bead A: title "parent(gd-D2MHVYIMA9)" — typed parent in title.
	//   bead B: description "gd-JXGVF55OZ5" — flat-only mention.
	// The pre-state of bead_references is irrelevant because the per-bead
	// DELETE-then-INSERT cycle determines the post-state purely from the
	// current text.
	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
			AddRow("gd-fnhtkrbdiq", "parent(gd-D2MHVYIMA9)", "", "", nil).
			AddRow("gd-hkbgwaothb", "", "gd-JXGVF55OZ5", "", nil))

	// bead A.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).
			AddRow("parent(gd-D2MHVYIMA9)", "", "", nil, *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-fnhtkrbdiq", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-fnhtkrbdiq", "title", "gd-D2MHVYIMA9").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-fnhtkrbdiq", "title", "parent", "gd-D2MHVYIMA9").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link: parent → parent-child dep, source-as-bead_id.
	expectTypedDepInsert(mock, "gd-fnhtkrbdiq", "gd-D2MHVYIMA9", model.DepParentChild)
	mock.ExpectCommit()

	// bead B — flat-only, so no typed insert runs.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-hkbgwaothb").
		WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).
			AddRow("", "gd-JXGVF55OZ5", "", nil, *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-hkbgwaothb", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-hkbgwaothb", "description", "gd-JXGVF55OZ5").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, false, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.BeadsScanned != 2 {
		t.Errorf("BeadsScanned = %d, want 2", stats.BeadsScanned)
	}
	// bead A: 1 flat + 1 typed = 2; bead B: 1 flat = 1; total = 3.
	if stats.ReferencesWritten != 3 {
		t.Errorf("ReferencesWritten = %d, want 3", stats.ReferencesWritten)
	}
}

// TestQueryReextractReferencesPage_TypedBackfill_UnknownVerbWritesOnlyFlat
// pins the contract that an unknown verb (e.g. `closes(gd-X)`) does not
// produce a typed row. The flat extractor still picks up gd-X via the
// untyped Pattern, so the bead ends up with one row (NULL relationship_type)
// rather than two.
func TestQueryReextractReferencesPage_TypedBackfill_UnknownVerbWritesOnlyFlat(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id, title, description, notes, fields\s+FROM beads\s+WHERE id > \$1`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "title", "description", "notes", "fields"}).
			AddRow("gd-fnhtkrbdiq", "closes(ka-0000001713)", "", "", nil))

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT title, description, notes, fields, operated_by_id FROM beads WHERE id = \$1`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"title", "description", "notes", "fields", "operated_by_id"}).
			AddRow("closes(ka-0000001713)", "", "", nil, *testOperatorID))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-fnhtkrbdiq", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Flat insert lifts ka-0000001713 out of the parens.
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-fnhtkrbdiq", "title", "ka-0000001713").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No typed insert — `closes` is not a canonical verb.
	mock.ExpectCommit()

	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, false, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.ReferencesWritten != 1 {
		t.Errorf("ReferencesWritten = %d, want 1 (flat only, unknown verb)", stats.ReferencesWritten)
	}
}

// TestQueryReextractReferencesPage_TypedBackfill_CommentsFlagOn exercises
// the comment-iteration branch with a typed mention in the comment body.
// The typed comment row is written under source_field="comment:<id>" so
// per-comment scoping is preserved.
func TestQueryReextractReferencesPage_TypedBackfill_CommentsFlagOn(t *testing.T) {
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
	mock.ExpectQuery(`SELECT id, text, operated_by_id FROM comments WHERE bead_id = \$1 ORDER BY id ASC`).
		WithArgs("gd-fnhtkrbdiq").
		WillReturnRows(sqlmock.NewRows([]string{"id", "text", "operated_by_id"}).
			AddRow(int64(7), "blocks(gd-HKBGWAOTHB)", *testOperatorID))
	// Per-comment flat path.
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-fnhtkrbdiq", "comment:7").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field\) VALUES`).
		WithArgs("gd-fnhtkrbdiq", "comment:7", "gd-HKBGWAOTHB").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// Per-comment typed path.
	mock.ExpectExec(`INSERT INTO bead_references \(source_bead_id, target_bead_id, source_field, relationship_type\) VALUES`).
		WithArgs("gd-fnhtkrbdiq", "comment:7", "blocks", "gd-HKBGWAOTHB").
		WillReturnResult(sqlmock.NewResult(0, 1))
	// M5/A auto-link: comment-sourced `blocks(gd-B)` flips so gd-B is the
	// dependent of the comment's bead.
	expectTypedDepInsert(mock, "gd-HKBGWAOTHB", "gd-fnhtkrbdiq", model.DepBlocks)
	mock.ExpectCommit()

	mock.ExpectQuery(`SELECT count\(\*\) FROM bead_references`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	stats, err := queryReextractReferencesPage(context.Background(), db, "", 500, true, false, "")
	if err != nil {
		t.Fatalf("queryReextractReferencesPage: %v", err)
	}
	if stats.CommentsScanned != 1 {
		t.Errorf("CommentsScanned = %d, want 1", stats.CommentsScanned)
	}
	if stats.ReferencesWritten != 2 {
		t.Errorf("ReferencesWritten = %d, want 2 (1 flat + 1 typed)", stats.ReferencesWritten)
	}
}
