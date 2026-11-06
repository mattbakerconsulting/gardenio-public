package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestAddFollower_Insert pins the upsert SQL: a fresh (bead_id, operator_id)
// pair lands as a plain INSERT.
func TestAddFollower_Insert(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	forwarder := int64(7)
	mock.ExpectExec(`INSERT INTO bead_followers \(bead_id, operator_id, forwarded_by, note\)\s+VALUES \(\$1, \$2, \$3, \$4\)\s+ON CONFLICT \(bead_id, operator_id\)\s+DO UPDATE SET forwarded_by = EXCLUDED\.forwarded_by,\s+note\s+= EXCLUDED\.note`).
		WithArgs("gd-FNHTKRBDIQ", int64(42), forwarder, "FYI").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.AddFollower(context.Background(), "gd-FNHTKRBDIQ", 42, &forwarder, "FYI"); err != nil {
		t.Fatalf("AddFollower: %v", err)
	}
}

// TestAddFollower_NilForwarder exercises the system-insert path: forwardedBy
// is nil, mapping to a NULL forwarded_by column.
func TestAddFollower_NilForwarder(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`INSERT INTO bead_followers`).
		WithArgs("gd-FNHTKRBDIQ", int64(42), nil, "").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.AddFollower(context.Background(), "gd-FNHTKRBDIQ", 42, nil, ""); err != nil {
		t.Fatalf("AddFollower: %v", err)
	}
}

// TestRemoveFollower_Hit pins the DELETE shape and the rows-affected → bool
// translation: a matched row returns true.
func TestRemoveFollower_Hit(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`DELETE FROM bead_followers WHERE bead_id = \$1 AND operator_id = \$2`).
		WithArgs("gd-FNHTKRBDIQ", int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	ok, err := store.RemoveFollower(context.Background(), "gd-FNHTKRBDIQ", 42)
	if err != nil {
		t.Fatalf("RemoveFollower: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true")
	}
}

// TestRemoveFollower_Miss exercises the idempotent path: no matched row
// returns (false, nil) so callers can blindly issue an unfollow.
func TestRemoveFollower_Miss(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`DELETE FROM bead_followers WHERE bead_id = \$1 AND operator_id = \$2`).
		WithArgs("gd-FNHTKRBDIQ", int64(99)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	ok, err := store.RemoveFollower(context.Background(), "gd-FNHTKRBDIQ", 99)
	if err != nil {
		t.Fatalf("RemoveFollower: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false")
	}
}

// TestListFollowers pins the JOIN shape and the NULLABLE forwarded_by →
// *int64 translation: a row with a non-NULL forwarder lands as a non-nil
// pointer; a NULL forwarder lands as nil. Order is created_at ASC.
func TestListFollowers(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	t1 := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 5, 8, 13, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT f\.bead_id,\s+f\.operator_id,\s+o\.kind,\s+o\.subject,\s+o\.display,\s+f\.forwarded_by,\s+COALESCE\(fo\.kind, ''\),\s+COALESCE\(fo\.subject, ''\),\s+COALESCE\(fo\.display, ''\),\s+f\.note,\s+f\.created_at\s+FROM bead_followers f\s+JOIN operators o\s+ON o\.id\s+= f\.operator_id\s+LEFT JOIN operators fo ON fo\.id = f\.forwarded_by\s+WHERE f\.bead_id = \$1\s+ORDER BY f\.created_at ASC, f\.operator_id ASC`).
		WithArgs("gd-FNHTKRBDIQ").
		WillReturnRows(sqlmock.NewRows([]string{
			"bead_id", "operator_id", "kind", "subject", "display",
			"forwarded_by", "fwd_kind", "fwd_subject", "fwd_display",
			"note", "created_at",
		}).
			AddRow("gd-FNHTKRBDIQ", int64(42), "slack", "U0OPERATOR1", "alice",
				int64(7), "slack", "U07OPERATOR", "matt",
				"FYI", t1).
			AddRow("gd-FNHTKRBDIQ", int64(43), "oidc", "bob@example.com", "bob",
				nil, "", "", "",
				"", t2))

	rows, err := store.ListFollowers(context.Background(), "gd-FNHTKRBDIQ")
	if err != nil {
		t.Fatalf("ListFollowers: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	if rows[0].OperatorID != 42 || rows[0].OperatorDisplay != "alice" {
		t.Errorf("rows[0] = %+v", rows[0])
	}
	if rows[0].ForwardedBy == nil || *rows[0].ForwardedBy != 7 {
		t.Errorf("rows[0].ForwardedBy = %v; want *7", rows[0].ForwardedBy)
	}
	if rows[0].ForwardedByDisplay != "matt" {
		t.Errorf("rows[0].ForwardedByDisplay = %q; want matt", rows[0].ForwardedByDisplay)
	}
	if rows[1].ForwardedBy != nil {
		t.Errorf("rows[1].ForwardedBy = %v; want nil", rows[1].ForwardedBy)
	}
	if rows[1].Note != "" {
		t.Errorf("rows[1].Note = %q; want empty", rows[1].Note)
	}
}

// TestListFollowedBeadIDs_WithLimit pins the inbox-UNION right-side query:
// a flat list of bead IDs scoped to the operator, newest-first, with the
// LIMIT clause attached when limit > 0.
func TestListFollowedBeadIDs_WithLimit(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT bead_id FROM bead_followers WHERE operator_id = \$1 ORDER BY created_at DESC, bead_id ASC LIMIT \$2`).
		WithArgs(int64(42), 100).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id"}).
			AddRow("gd-FNHTKRBDIQ").
			AddRow("gd-HKBGWAOTHB"))

	ids, err := store.ListFollowedBeadIDs(context.Background(), 42, 100)
	if err != nil {
		t.Fatalf("ListFollowedBeadIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != "gd-FNHTKRBDIQ" || ids[1] != "gd-HKBGWAOTHB" {
		t.Fatalf("ids = %v", ids)
	}
}

// TestListFollowedBeadIDs_NoLimit exercises the unlimited path: limit ≤ 0
// drops the LIMIT clause.
func TestListFollowedBeadIDs_NoLimit(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT bead_id FROM bead_followers WHERE operator_id = \$1 ORDER BY created_at DESC, bead_id ASC$`).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id"}))

	ids, err := store.ListFollowedBeadIDs(context.Background(), 42, 0)
	if err != nil {
		t.Fatalf("ListFollowedBeadIDs: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v; want empty", ids)
	}
}
