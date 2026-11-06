package postgres

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// The transaction-scoped store (txStore) delegates each method to the same
// query helper as *PostgresStore, just over a *sql.Tx. The PostgresStore
// receivers are exercised elsewhere; this file drives the txStore variants
// through a real sqlmock transaction so the delegation wrappers are covered.

func TestTxStore_Gates(t *testing.T) {
	db, mock := newMockDB(t)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO session_gates`).
		WithArgs("agent-1", "commit-push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE session_gates\s+SET status = 'satisfied'`).
		WithArgs("agent-1", "commit-push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT status FROM session_gates`).
		WithArgs("agent-1", "commit-push").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("satisfied"))
	mock.ExpectQuery(`SELECT agent_bead_id, gate_id, status, satisfied_at\s+FROM session_gates`).
		WithArgs("agent-1").
		WillReturnRows(sqlmock.NewRows([]string{"agent_bead_id", "gate_id", "status", "satisfied_at"}).
			AddRow("agent-1", "commit-push", "satisfied", now))
	mock.ExpectExec(`UPDATE session_gates\s+SET status = 'pending'`).
		WithArgs("agent-1", "commit-push").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}
	ctx := context.Background()

	if err := ts.UpsertGate(ctx, "agent-1", "commit-push"); err != nil {
		t.Fatalf("UpsertGate: %v", err)
	}
	if err := ts.MarkGateSatisfied(ctx, "agent-1", "commit-push"); err != nil {
		t.Fatalf("MarkGateSatisfied: %v", err)
	}
	ok, err := ts.IsGateSatisfied(ctx, "agent-1", "commit-push")
	if err != nil || !ok {
		t.Fatalf("IsGateSatisfied = (%v, %v), want (true, nil)", ok, err)
	}
	gates, err := ts.ListGates(ctx, "agent-1")
	if err != nil || len(gates) != 1 {
		t.Fatalf("ListGates = (%+v, %v), want one gate", gates, err)
	}
	if gates[0].SatisfiedAt == nil {
		t.Error("ListGates: SatisfiedAt = nil, want non-nil")
	}
	if err := ts.ClearGate(ctx, "agent-1", "commit-push"); err != nil {
		t.Fatalf("ClearGate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestTxStore_IsGateSatisfied_MissingIsFalse(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT status FROM session_gates`).
		WithArgs("agent-1", "g").
		WillReturnRows(sqlmock.NewRows([]string{"status"})) // no rows
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}
	ok, err := ts.IsGateSatisfied(context.Background(), "agent-1", "g")
	if err != nil {
		t.Fatalf("IsGateSatisfied: err = %v, want nil for missing row", err)
	}
	if ok {
		t.Error("IsGateSatisfied = true, want false for missing row")
	}
	_ = tx.Rollback()
}

func TestTxStore_MutationHistory(t *testing.T) {
	db, mock := newMockDB(t)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-1", "actor", "status", "open", "closed", "update", nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM mutation_history\s+WHERE bead_id = \$1`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "bead_id", "actor", "field", "old_value", "new_value", "mutation_type", "created_at",
		}).AddRow(int64(1), "gd-1", "actor", "status", "open", "closed", "update", now))
	mock.ExpectQuery(`FROM mutation_history\s+WHERE id = \$1`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "bead_id", "actor", "field", "old_value", "new_value", "mutation_type", "created_at",
		}).AddRow(int64(1), "gd-1", "actor", "status", "open", "closed", "update", now))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}
	ctx := context.Background()

	if err := ts.RecordMutation(ctx, &model.MutationRow{
		BeadID: "gd-1", Actor: "actor", Field: "status",
		OldValue: "open", NewValue: "closed", MutationType: "update",
	}); err != nil {
		t.Fatalf("RecordMutation: %v", err)
	}
	muts, err := ts.ListMutations(ctx, "gd-1")
	if err != nil || len(muts) != 1 {
		t.Fatalf("ListMutations = (%+v, %v), want one row", muts, err)
	}
	got, err := ts.GetMutation(ctx, 1)
	if err != nil || got.ID != 1 {
		t.Fatalf("GetMutation = (%+v, %v), want id 1", got, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestTxStore_Followers(t *testing.T) {
	db, mock := newMockDB(t)

	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fwd := int64(7)
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO bead_followers`).
		WithArgs("gd-1", int64(42), &fwd, "FYI").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`FROM bead_followers f\s+JOIN operators o`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"bead_id", "operator_id", "kind", "subject", "display",
			"forwarded_by", "fkind", "fsubject", "fdisplay", "note", "created_at",
		}).AddRow("gd-1", int64(42), "oidc", "a@x.ai", "A", fwd, "oidc", "b@x.ai", "B", "FYI", now))
	mock.ExpectQuery(`SELECT bead_id FROM bead_followers WHERE operator_id = \$1.*LIMIT \$2`).
		WithArgs(int64(42), 10).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id"}).AddRow("gd-1"))
	mock.ExpectExec(`DELETE FROM bead_followers`).
		WithArgs("gd-1", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}
	ctx := context.Background()

	if err := ts.AddFollower(ctx, "gd-1", 42, &fwd, "FYI"); err != nil {
		t.Fatalf("AddFollower: %v", err)
	}
	followers, err := ts.ListFollowers(ctx, "gd-1")
	if err != nil || len(followers) != 1 {
		t.Fatalf("ListFollowers = (%+v, %v), want one follower", followers, err)
	}
	if followers[0].ForwardedBy == nil || *followers[0].ForwardedBy != 7 {
		t.Errorf("ForwardedBy = %v, want 7", followers[0].ForwardedBy)
	}
	ids, err := ts.ListFollowedBeadIDs(ctx, 42, 10)
	if err != nil || len(ids) != 1 || ids[0] != "gd-1" {
		t.Fatalf("ListFollowedBeadIDs = (%v, %v), want [gd-1]", ids, err)
	}
	removed, err := ts.RemoveFollower(ctx, "gd-1", 42)
	if err != nil || !removed {
		t.Fatalf("RemoveFollower = (%v, %v), want (true, nil)", removed, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// ListFollowedBeadIDs with no limit omits the LIMIT clause — drive the
// txStore variant down that branch.
func TestTxStore_ListFollowedBeadIDs_NoLimit(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT bead_id FROM bead_followers WHERE operator_id = \$1 ORDER BY created_at DESC, bead_id ASC$`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id"}).AddRow("gd-1").AddRow("gd-2"))
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}
	ids, err := ts.ListFollowedBeadIDs(context.Background(), 42, 0)
	if err != nil {
		t.Fatalf("ListFollowedBeadIDs: %v", err)
	}
	if len(ids) != 2 {
		t.Errorf("ids = %v, want 2", ids)
	}
	_ = tx.Rollback()
}
