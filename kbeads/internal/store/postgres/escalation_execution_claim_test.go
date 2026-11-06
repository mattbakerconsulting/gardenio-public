package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// These tests pin the guarded-UPDATE SQL shape and the claimed/lost/missing
// contract of the escalation execution claim (P7 Task 12, spec D10).

func TestClaimEscalationExecution_Claims(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads\s+SET fields = jsonb_set\(COALESCE\(fields, '\{\}'::jsonb\), '\{execution_dispatch\}', to_jsonb\(\$2::text\), true\).*COALESCE\(fields ->> 'execution_dispatch', ''\) = ''`).
		WithArgs("esc-1", "local").
		WillReturnResult(sqlmock.NewResult(0, 1))

	claimed, current, err := store.ClaimEscalationExecution(context.Background(), "esc-1", "local")
	if err != nil || !claimed || current != "local" {
		t.Fatalf("ClaimEscalationExecution = (%v, %q, %v), want (true, \"local\", nil)", claimed, current, err)
	}
}

func TestClaimEscalationExecution_LostReturnsWinner(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("esc-2", "server").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT fields ->> 'execution_dispatch' FROM beads`).
		WithArgs("esc-2").
		WillReturnRows(sqlmock.NewRows([]string{"execution_dispatch"}).AddRow("local"))

	claimed, current, err := store.ClaimEscalationExecution(context.Background(), "esc-2", "server")
	if err != nil || claimed || current != "local" {
		t.Fatalf("ClaimEscalationExecution = (%v, %q, %v), want (false, \"local\", nil)", claimed, current, err)
	}
}

func TestClaimEscalationExecution_MissingBead(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("esc-3", "local").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`SELECT fields ->> 'execution_dispatch' FROM beads`).
		WithArgs("esc-3").
		WillReturnError(sql.ErrNoRows)

	claimed, current, err := store.ClaimEscalationExecution(context.Background(), "esc-3", "local")
	if err != nil || claimed || current != "" {
		t.Fatalf("ClaimEscalationExecution = (%v, %q, %v), want (false, \"\", nil)", claimed, current, err)
	}
}

func TestClaimEscalationExecution_ExecError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`UPDATE beads`).
		WithArgs("esc-4", "local").
		WillReturnError(errors.New("boom"))

	if _, _, err := store.ClaimEscalationExecution(context.Background(), "esc-4", "local"); err == nil {
		t.Fatal("ClaimEscalationExecution: err = nil, want non-nil for exec error")
	}
}
