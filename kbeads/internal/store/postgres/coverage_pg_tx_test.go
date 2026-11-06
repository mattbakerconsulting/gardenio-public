package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// These tests cover the transaction-opening *PostgresStore methods
// (CreateBead, UpdateBead, AddComment, TransferBead) and their parallel
// *txStore counterparts. The plain-text fixtures carry no gd-id mentions, so
// replaceBeadReferences / replaceCommentReferences only run their DELETE
// (no INSERTs) — keeping the expectation set minimal and deterministic.

// --- PostgresStore.CreateBead (tx-opening) ---

func TestCovPGTxCreateBead(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-create1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Create me", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
	}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO beads").
		WithArgs(
			"gd-create1", sqlmock.AnyArg(), "issue", "task", "Create me", "", "",
			"open", 0, "", "", now, "", now,
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"default", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// replaceBeadReferences DELETEs the three text source_fields; no INSERT
	// because the plain-text bead carries no gd-id mentions.
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-create1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if err := store.CreateBead(context.Background(), bead); err != nil {
		t.Fatalf("CreateBead: %v", err)
	}
}

func TestCovPGTxCreateBead_InsertError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	bead := &model.Bead{ID: "gd-bad", Kind: model.KindIssue, Type: model.TypeTask, Title: "x", Status: model.StatusOpen}

	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO beads").WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := store.CreateBead(context.Background(), bead); err == nil {
		t.Fatal("CreateBead: expected error from failed INSERT")
	}
}

// --- PostgresStore.UpdateBead (tx-opening) ---

func TestCovPGTxUpdateBead(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-update1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Updated", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
	}

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs(
			"gd-update1", sqlmock.AnyArg(), "issue", "task", "Updated", "", "",
			"open", 0, "", "",
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(now))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = ANY\(\$2::text\[\]\)`).
		WithArgs("gd-update1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if err := store.UpdateBead(context.Background(), bead); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
}

func TestCovPGTxUpdateBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	bead := &model.Bead{ID: "gd-missing", Kind: model.KindIssue, Type: model.TypeTask, Title: "x", Status: model.StatusOpen}

	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE beads SET").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	if err := store.UpdateBead(context.Background(), bead); err != sql.ErrNoRows {
		t.Fatalf("UpdateBead: expected sql.ErrNoRows, got %v", err)
	}
}

// --- PostgresStore.AddComment (tx-opening; extractCommentRefs default false) ---

func TestCovPGTxAddComment(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()
	comment := &model.Comment{BeadID: "gd-c1", Author: "alice", Text: "Hello"}

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO comments").
		WithArgs("gd-c1", "alice", "Hello", sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), now))
	// extractCommentRefs defaults to false -> no replaceCommentReferences DELETE.
	mock.ExpectCommit()

	if err := store.AddComment(context.Background(), comment); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if comment.ID != 7 {
		t.Fatalf("expected comment id=7, got %d", comment.ID)
	}
}

func TestCovPGTxAddComment_WithRefExtraction(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	store.SetExtractCommentReferences(true)
	now := time.Now().UTC()
	// Plain text (no gd-id) -> replaceCommentReferences runs only its DELETE.
	comment := &model.Comment{BeadID: "gd-c2", Author: "bob", Text: "no refs here"}

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO comments").
		WithArgs("gd-c2", "bob", "no refs here", sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(8), now))
	mock.ExpectExec(`DELETE FROM bead_references WHERE source_bead_id = \$1 AND source_field = \$2`).
		WithArgs("gd-c2", "comment:8").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if err := store.AddComment(context.Background(), comment); err != nil {
		t.Fatalf("AddComment: %v", err)
	}
}

func TestCovPGTxAddComment_InsertError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	comment := &model.Comment{BeadID: "gd-c3", Author: "carol", Text: "x"}

	mock.ExpectBegin()
	mock.ExpectQuery("INSERT INTO comments").WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	if err := store.AddComment(context.Background(), comment); err == nil {
		t.Fatal("AddComment: expected error from failed INSERT")
	}
}

// --- PostgresStore.TransferBead (tx-opening) ---

func TestCovPGTxTransferBead(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()

	mock.ExpectBegin()
	// Capture old fleet.
	mock.ExpectQuery(`SELECT fleet FROM beads WHERE id = \$1`).
		WithArgs("gd-xfer1").
		WillReturnRows(sqlmock.NewRows([]string{"fleet"}).AddRow("staging"))
	// UPDATE … RETURNING uses the unjoined scanBead projection.
	xferRows := sqlmock.NewRows(beadRowColumnsBaseLabels).AddRow(
		"gd-xfer1", nil, nil, nil, "issue", "task", "Transfer me", nil, nil,
		"open", 0, nil, nil, now, nil, now,
		nil, nil, nil, nil, nil, "production", nil, nil,
		nil,
		nil,
	)
	mock.ExpectQuery(`UPDATE beads SET fleet = \$2, updated_at = NOW\(\)`).
		WithArgs("gd-xfer1", "production").
		WillReturnRows(xferRows)
	// Related-row fleet updates.
	mock.ExpectExec(`UPDATE deps SET fleet = \$2 WHERE bead_id = \$1`).
		WithArgs("gd-xfer1", "production").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE labels SET fleet = \$2 WHERE bead_id = \$1`).
		WithArgs("gd-xfer1", "production").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE comments SET fleet = \$2 WHERE bead_id = \$1`).
		WithArgs("gd-xfer1", "production").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE events SET fleet = \$2 WHERE bead_id = \$1`).
		WithArgs("gd-xfer1", "production").WillReturnResult(sqlmock.NewResult(0, 0))
	// Relational fetch (deps, comments; labels rode on the RETURNING row).
	emptyRelationalExpectations(mock, "gd-xfer1")
	mock.ExpectCommit()

	bead, fromFleet, err := store.TransferBead(context.Background(), "gd-xfer1", "production")
	if err != nil {
		t.Fatalf("TransferBead: %v", err)
	}
	if fromFleet != "staging" {
		t.Fatalf("expected fromFleet=staging, got %q", fromFleet)
	}
	if bead.ID != "gd-xfer1" || bead.Fleet != "production" {
		t.Fatalf("got id=%q fleet=%q", bead.ID, bead.Fleet)
	}
}

func TestCovPGTxTransferBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT fleet FROM beads WHERE id = \$1`).
		WithArgs("gd-missing").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	_, _, err := store.TransferBead(context.Background(), "gd-missing", "production")
	if err != sql.ErrNoRows {
		t.Fatalf("TransferBead: expected sql.ErrNoRows, got %v", err)
	}
}

// --- txStore methods via RunInTransaction ---

// TestCovPGTxRunInTransaction_BeadLifecycle exercises a chain of txStore
// methods inside a single RunInTransaction: CreateBead, GetBead, ListBeads,
// UpdateBead, AddLabel, GetLabels, AddComment, RecordEvent. All expectations
// are consumed inside the one Begin/Commit envelope.
func TestCovPGTxRunInTransaction_BeadLifecycle(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	mock.ExpectBegin()

	// txStore.CreateBead -> INSERT + replaceBeadReferences DELETE.
	mock.ExpectExec("INSERT INTO beads").
		WithArgs(
			"gd-tx1", sqlmock.AnyArg(), "issue", "task", "Tx bead", "", "",
			"open", 0, "", "", now, "", now,
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"default", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`DELETE FROM bead_references`).
		WithArgs("gd-tx1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// txStore.GetBead -> JOINed read (labels folded in) + 2 relational queries.
	getRows := sqlmock.NewRows(beadRowColumnsLabels).AddRow(
		"gd-tx1", nil, nil, nil, "issue", "task", "Tx bead", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "",
		nil,
	)
	mock.ExpectQuery(`SELECT .+ FROM beads .* WHERE beads\.id = \$1`).WithArgs("gd-tx1").WillReturnRows(getRows)
	emptyRelationalExpectations(mock, "gd-tx1")

	// txStore.ListBeads -> COUNT OVER projection + labels follow-up.
	mock.ExpectQuery(`SELECT 0 AS total_count, .+ FROM beads`).
		WillReturnRows(addBeadWithTotalRow(sqlmock.NewRows(beadWithTotalColumns), 1, "gd-tx1", "issue", "task", "Tx bead", "open", 0, now))
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	// txStore.UpdateBead -> UPDATE RETURNING + replaceBeadReferences DELETE.
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs(
			"gd-tx1", sqlmock.AnyArg(), "issue", "task", "Tx bead", "", "",
			"open", 0, "", "",
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(now))
	mock.ExpectExec(`DELETE FROM bead_references`).
		WithArgs("gd-tx1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	// txStore.AddLabel.
	mock.ExpectExec("INSERT INTO labels").WithArgs("gd-tx1", "urgent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// txStore.GetLabels.
	mock.ExpectQuery("SELECT label FROM labels WHERE bead_id = \\$1").WithArgs("gd-tx1").
		WillReturnRows(sqlmock.NewRows([]string{"label"}).AddRow("urgent"))

	// txStore.AddComment (extractCommentRefs false -> no ref DELETE).
	mock.ExpectQuery("INSERT INTO comments").
		WithArgs("gd-tx1", "alice", "a comment", sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), now))

	// txStore.RecordEvent.
	mock.ExpectQuery("INSERT INTO events").
		WithArgs("beads.bead.created", "gd-tx1", "alice", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), now))

	mock.ExpectCommit()

	bead := &model.Bead{
		ID: "gd-tx1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Tx bead", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
	}
	err := st.RunInTransaction(context.Background(), func(tx store.Store) error {
		if err := tx.CreateBead(context.Background(), bead); err != nil {
			return err
		}
		got, err := tx.GetBead(context.Background(), "gd-tx1")
		if err != nil {
			return err
		}
		if got.ID != "gd-tx1" {
			t.Errorf("GetBead id = %q, want gd-tx1", got.ID)
		}
		beads, total, err := tx.ListBeads(context.Background(), model.BeadFilter{})
		if err != nil {
			return err
		}
		if total != 1 || len(beads) != 1 {
			t.Errorf("ListBeads total=%d len=%d, want 1/1", total, len(beads))
		}
		if err := tx.UpdateBead(context.Background(), bead); err != nil {
			return err
		}
		if err := tx.AddLabel(context.Background(), "gd-tx1", "urgent"); err != nil {
			return err
		}
		labels, err := tx.GetLabels(context.Background(), "gd-tx1")
		if err != nil {
			return err
		}
		if len(labels) != 1 || labels[0] != "urgent" {
			t.Errorf("GetLabels = %v, want [urgent]", labels)
		}
		if err := tx.AddComment(context.Background(), &model.Comment{BeadID: "gd-tx1", Author: "alice", Text: "a comment"}); err != nil {
			return err
		}
		return tx.RecordEvent(context.Background(), &model.Event{
			Topic: "beads.bead.created", BeadID: "gd-tx1", Actor: "alice",
		})
	})
	if err != nil {
		t.Fatalf("RunInTransaction: %v", err)
	}
}

// TestCovPGTxRunInTransaction_RollbackOnError asserts a returned error from fn
// triggers a rollback (and the failing inner query is exercised).
func TestCovPGTxRunInTransaction_RollbackOnError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT label FROM labels WHERE bead_id = \$1`).
		WithArgs("gd-err").
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	err := st.RunInTransaction(context.Background(), func(tx store.Store) error {
		_, gErr := tx.GetLabels(context.Background(), "gd-err")
		return gErr
	})
	if err == nil {
		t.Fatal("RunInTransaction: expected error to propagate and trigger rollback")
	}
}

// TestCovPGTxStore_NoopAndUnsupported covers txStore's trivial no-op methods
// (Ping, Close, RunInTransaction reuse) and one of the intentionally
// unsupported backfill methods, all via a real *sql.Tx from sqlmock.
func TestCovPGTxStore_NoopAndUnsupported(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	if err := ts.Ping(context.Background()); err != nil {
		t.Errorf("txStore.Ping: %v", err)
	}
	if err := ts.Close(); err != nil {
		t.Errorf("txStore.Close: %v", err)
	}
	// RunInTransaction reuses the existing tx (no nesting): fn runs with ts.
	called := false
	if err := ts.RunInTransaction(context.Background(), func(inner store.Store) error {
		called = true
		if inner != store.Store(ts) {
			t.Error("RunInTransaction should pass the same txStore")
		}
		return nil
	}); err != nil {
		t.Errorf("txStore.RunInTransaction: %v", err)
	}
	if !called {
		t.Error("RunInTransaction fn was not called")
	}
	// One representative intentionally-unsupported backfill method.
	if _, err := ts.ReextractReferencesPage(context.Background(), "", 10, ""); err == nil {
		t.Error("txStore.ReextractReferencesPage: expected unsupported error")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
