package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestUpsertGate(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec("INSERT INTO session_gates").
		WithArgs("agent-1", "commit-push").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := store.UpsertGate(context.Background(), "agent-1", "commit-push")
	if err != nil {
		t.Fatalf("UpsertGate: %v", err)
	}
}

func TestUpsertGateError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec("INSERT INTO session_gates").
		WithArgs("agent-1", "commit-push").
		WillReturnError(sql.ErrConnDone)

	err := store.UpsertGate(context.Background(), "agent-1", "commit-push")
	if err == nil {
		t.Fatal("expected error from UpsertGate")
	}
}

func TestMarkGateSatisfied(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec("UPDATE session_gates").
		WithArgs("agent-1", "decision").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := store.MarkGateSatisfied(context.Background(), "agent-1", "decision")
	if err != nil {
		t.Fatalf("MarkGateSatisfied: %v", err)
	}
}

func TestClearGate(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec("UPDATE session_gates").
		WithArgs("agent-1", "decision").
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := store.ClearGate(context.Background(), "agent-1", "decision")
	if err != nil {
		t.Fatalf("ClearGate: %v", err)
	}
}

func TestIsGateSatisfiedTrue(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT status FROM session_gates").
		WithArgs("agent-1", "commit-push").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("satisfied"))

	ok, err := store.IsGateSatisfied(context.Background(), "agent-1", "commit-push")
	if err != nil {
		t.Fatalf("IsGateSatisfied: %v", err)
	}
	if !ok {
		t.Error("expected gate to be satisfied")
	}
}

func TestIsGateSatisfiedFalse(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT status FROM session_gates").
		WithArgs("agent-1", "commit-push").
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("pending"))

	ok, err := store.IsGateSatisfied(context.Background(), "agent-1", "commit-push")
	if err != nil {
		t.Fatalf("IsGateSatisfied: %v", err)
	}
	if ok {
		t.Error("expected gate to be pending (not satisfied)")
	}
}

func TestIsGateSatisfiedNotFound(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT status FROM session_gates").
		WithArgs("agent-1", "nonexistent").
		WillReturnError(sql.ErrNoRows)

	ok, err := store.IsGateSatisfied(context.Background(), "agent-1", "nonexistent")
	if err != nil {
		t.Fatalf("IsGateSatisfied should not error on not-found: %v", err)
	}
	if ok {
		t.Error("expected false for missing gate")
	}
}

func TestIsGateSatisfiedDBError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT status FROM session_gates").
		WithArgs("agent-1", "commit-push").
		WillReturnError(sql.ErrConnDone)

	_, err := store.IsGateSatisfied(context.Background(), "agent-1", "commit-push")
	if err == nil {
		t.Fatal("expected error from IsGateSatisfied")
	}
}

func TestListGatesEmpty(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT .+ FROM session_gates").
		WithArgs("agent-1").
		WillReturnRows(sqlmock.NewRows([]string{"agent_bead_id", "gate_id", "status", "satisfied_at"}))

	gates, err := store.ListGates(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("ListGates: %v", err)
	}
	if len(gates) != 0 {
		t.Errorf("expected 0 gates, got %d", len(gates))
	}
}

func TestListGatesWithRows(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"agent_bead_id", "gate_id", "status", "satisfied_at"}).
		AddRow("agent-1", "commit-push", "satisfied", now).
		AddRow("agent-1", "decision", "pending", nil)

	mock.ExpectQuery("SELECT .+ FROM session_gates").
		WithArgs("agent-1").
		WillReturnRows(rows)

	gates, err := store.ListGates(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("ListGates: %v", err)
	}
	if len(gates) != 2 {
		t.Fatalf("expected 2 gates, got %d", len(gates))
	}

	// First gate: satisfied with timestamp.
	if gates[0].GateID != "commit-push" {
		t.Errorf("gate[0].GateID = %q, want commit-push", gates[0].GateID)
	}
	if gates[0].Status != "satisfied" {
		t.Errorf("gate[0].Status = %q, want satisfied", gates[0].Status)
	}
	if gates[0].SatisfiedAt == nil {
		t.Error("gate[0].SatisfiedAt should not be nil")
	}

	// Second gate: pending with no timestamp.
	if gates[1].GateID != "decision" {
		t.Errorf("gate[1].GateID = %q, want decision", gates[1].GateID)
	}
	if gates[1].Status != "pending" {
		t.Errorf("gate[1].Status = %q, want pending", gates[1].Status)
	}
	if gates[1].SatisfiedAt != nil {
		t.Error("gate[1].SatisfiedAt should be nil")
	}
}

func TestListGatesQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT .+ FROM session_gates").
		WithArgs("agent-1").
		WillReturnError(sql.ErrConnDone)

	_, err := store.ListGates(context.Background(), "agent-1")
	if err == nil {
		t.Fatal("expected error from ListGates")
	}
}
