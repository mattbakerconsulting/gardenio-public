package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// Covers previously-untested read/write helpers in queries.go reached through
// the *PostgresStore receivers: RestoreBead, TransferBead,
// GetReverseDependencies, and FindAgentByName.

func TestRestoreBead(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBaseLabels).AddRow(
		"gd-1", nil, nil, nil, "issue", "task", "Restored", nil, nil,
		"open", 0, nil, nil, now, "alice", now,
		nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		nil,
	)
	mock.ExpectQuery(`UPDATE beads SET deleted_at = NULL`).
		WithArgs("gd-1").
		WillReturnRows(rows)
	emptyRelationalExpectations(mock, "gd-1")

	b, err := st.RestoreBead(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("RestoreBead: %v", err)
	}
	if b.ID != "gd-1" || b.DeletedAt != nil {
		t.Errorf("bead = %+v, want gd-1 with nil DeletedAt", b)
	}
}

func TestRestoreBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`UPDATE beads SET deleted_at = NULL`).
		WithArgs("gd-missing").
		WillReturnError(sql.ErrNoRows)

	if _, err := st.RestoreBead(context.Background(), "gd-missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestTransferBead(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	// TransferBead wraps the whole operation in a transaction.
	mock.ExpectBegin()
	// 1) SELECT old fleet
	mock.ExpectQuery(`SELECT fleet FROM beads WHERE id = \$1`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{"fleet"}).AddRow("old-fleet"))
	// 2) UPDATE ... RETURNING bead
	rows := sqlmock.NewRows(beadRowColumnsBaseLabels).AddRow(
		"gd-1", nil, nil, nil, "issue", "task", "T", nil, nil,
		"open", 0, nil, nil, now, "alice", now,
		nil, nil, nil, nil, nil, "new-fleet", nil, nil,
		nil,
		nil,
	)
	mock.ExpectQuery(`UPDATE beads SET fleet = \$2`).
		WithArgs("gd-1", "new-fleet").
		WillReturnRows(rows)
	// 3) the four related-table fleet updates
	for _, table := range []string{"deps", "labels", "comments", "events"} {
		mock.ExpectExec(`UPDATE `+table+` SET fleet = \$2 WHERE bead_id = \$1`).
			WithArgs("gd-1", "new-fleet").
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	// 4) relational hydration
	emptyRelationalExpectations(mock, "gd-1")
	mock.ExpectCommit()

	b, fromFleet, err := st.TransferBead(context.Background(), "gd-1", "new-fleet")
	if err != nil {
		t.Fatalf("TransferBead: %v", err)
	}
	if fromFleet != "old-fleet" {
		t.Errorf("fromFleet = %q, want old-fleet", fromFleet)
	}
	if b.Fleet != "new-fleet" {
		t.Errorf("bead fleet = %q, want new-fleet", b.Fleet)
	}
}

func TestTransferBead_MissingBead(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT fleet FROM beads WHERE id = \$1`).
		WithArgs("gd-missing").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	if _, _, err := st.TransferBead(context.Background(), "gd-missing", "f"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestGetReverseDependencies(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	mock.ExpectQuery(`FROM deps\s+WHERE depends_on_id = \$1`).
		WithArgs("gd-1").
		WillReturnRows(sqlmock.NewRows([]string{
			"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata",
		}).AddRow("gd-child", "gd-1", "blocks", now, "alice", nil))

	deps, err := st.GetReverseDependencies(context.Background(), "gd-1")
	if err != nil {
		t.Fatalf("GetReverseDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].BeadID != "gd-child" {
		t.Errorf("deps = %+v, want one dep from gd-child", deps)
	}
}

func TestGetReverseDependencies_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM deps\s+WHERE depends_on_id = \$1`).
		WithArgs("gd-1").
		WillReturnError(errors.New("boom"))

	if _, err := st.GetReverseDependencies(context.Background(), "gd-1"); err == nil {
		t.Fatal("GetReverseDependencies: err = nil, want non-nil")
	}
}

func TestFindAgentByName(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	// scanBeadFull projects the JOINed operator suffix -> beadRowColumns.
	rows := sqlmock.NewRows(beadRowColumns).AddRow(
		"gd-agent", nil, nil, nil, "config", "agent", "agent-x", nil, nil,
		"open", 0, nil, nil, now, nil, now,
		nil, nil, nil, nil, []byte(`{"agent":"agent-x"}`), "default", nil, nil,
		nil,
		"", "", "",
	)
	mock.ExpectQuery(`beads.type = 'agent'`).
		WithArgs("agent-x").
		WillReturnRows(rows)

	b, err := st.FindAgentByName(context.Background(), "agent-x", nil)
	if err != nil {
		t.Fatalf("FindAgentByName: %v", err)
	}
	if b.ID != "gd-agent" {
		t.Errorf("bead ID = %q, want gd-agent", b.ID)
	}
}

func TestFindAgentByName_WithStatusFilter(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumns).AddRow(
		"gd-agent", nil, nil, nil, "config", "agent", "agent-x", nil, nil,
		"in_progress", 0, nil, nil, now, nil, now,
		nil, nil, nil, nil, []byte(`{"agent":"agent-x"}`), "default", nil, nil,
		nil,
		"", "", "",
	)
	// The status filter appends an IN (...) clause + a second arg.
	mock.ExpectQuery(`beads.status IN`).
		WithArgs("agent-x", "in_progress").
		WillReturnRows(rows)

	b, err := st.FindAgentByName(context.Background(), "agent-x", []model.Status{model.StatusInProgress})
	if err != nil {
		t.Fatalf("FindAgentByName: %v", err)
	}
	if b.Status != model.StatusInProgress {
		t.Errorf("status = %q, want in_progress", b.Status)
	}
}

func TestFindAgentByName_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}

	mock.ExpectQuery(`beads.type = 'agent'`).
		WithArgs("ghost").
		WillReturnError(sql.ErrNoRows)

	if _, err := st.FindAgentByName(context.Background(), "ghost", nil); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}
