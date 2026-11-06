package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// expectForgetLoad sets up the initial "load current state" SELECT.
func expectForgetLoad(mock sqlmock.Sqlmock, id int64, forgottenAt interface{}, hasSlug bool) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`SELECT forgotten_at, slug IS NOT NULL\s+FROM operators\s+WHERE id = \$1`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"forgotten_at", "has_slug"}).AddRow(forgottenAt, hasSlug))
}

// expectAuditCount sets up the five-table audit-row count.
func expectAuditCount(mock sqlmock.Sqlmock, id, total int64) *sqlmock.ExpectedQuery {
	return mock.ExpectQuery(`SELECT \(SELECT count\(\*\) FROM beads WHERE operated_by_id = \$1\)`).
		WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"total"}).AddRow(total))
}

func TestForgetOperator_HappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}

	forgotAt := time.Date(2026, 6, 16, 16, 24, 46, 0, time.UTC)
	expectForgetLoad(mock, 42, nil, true)
	expectAuditCount(mock, 42, 7)
	mock.ExpectQuery(`UPDATE operators\s+SET forgotten_at = NOW\(\),\s+slug = NULL,\s+forgotten_by = NULLIF\(\$2, ''\),\s+forgotten_reason = NULLIF\(\$3, ''\),\s+updated_at = NOW\(\)\s+WHERE id = \$1\s+AND forgotten_at IS NULL\s+RETURNING forgotten_at`).
		WithArgs(int64(42), "alice@example.com", "GDPR-9").
		WillReturnRows(sqlmock.NewRows([]string{"forgotten_at"}).AddRow(forgotAt))
	mock.ExpectExec(`UPDATE operator_slug_aliases\s+SET retired_at = NOW\(\)\s+WHERE operator_id = \$1\s+AND retired_at IS NULL`).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 2))

	res, err := s.ForgetOperator(context.Background(), 42, store.ForgetOperatorOptions{
		Actor:  "alice@example.com",
		Reason: "GDPR-9",
	})
	if err != nil {
		t.Fatalf("ForgetOperator: %v", err)
	}
	if res.AlreadyForgotten {
		t.Error("AlreadyForgotten = true, want false")
	}
	if !res.SlugCleared {
		t.Error("SlugCleared = false, want true")
	}
	if res.SlugAliasesRetired != 2 {
		t.Errorf("SlugAliasesRetired = %d, want 2", res.SlugAliasesRetired)
	}
	if res.AuditRows != 7 {
		t.Errorf("AuditRows = %d, want 7", res.AuditRows)
	}
	if !res.ForgottenAt.Equal(forgotAt) {
		t.Errorf("ForgottenAt = %v, want %v", res.ForgottenAt, forgotAt)
	}
}

func TestForgetOperator_Idempotent(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}

	orig := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	// Already forgotten: only the load SELECT runs, no count/update.
	expectForgetLoad(mock, 42, orig, false)

	res, err := s.ForgetOperator(context.Background(), 42, store.ForgetOperatorOptions{Actor: "bob@example.com"})
	if err != nil {
		t.Fatalf("ForgetOperator: %v", err)
	}
	if !res.AlreadyForgotten {
		t.Error("AlreadyForgotten = false, want true")
	}
	if !res.ForgottenAt.Equal(orig) {
		t.Errorf("ForgottenAt = %v, want original %v", res.ForgottenAt, orig)
	}
	if res.SlugCleared {
		t.Error("SlugCleared = true, want false on no-op")
	}
}

// TestForgetOperator_GuardTrips guards ka-0000005150: an operator with more
// audit rows than the threshold is refused before anything is written.
func TestForgetOperator_GuardTrips(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}

	expectForgetLoad(mock, 42, nil, true)
	expectAuditCount(mock, 42, 5000)
	// No UPDATE expected — the guard must refuse before mutating.

	_, err := s.ForgetOperator(context.Background(), 42, store.ForgetOperatorOptions{
		Actor:               "carol@example.com",
		AuditGuardThreshold: 1000,
	})
	var guard *store.OperatorForgetGuardError
	if !errors.As(err, &guard) {
		t.Fatalf("expected *OperatorForgetGuardError, got %v", err)
	}
	if guard.AuditRows != 5000 || guard.Threshold != 1000 {
		t.Errorf("guard mismatch: %+v", guard)
	}
}

// TestForgetOperator_GuardBypassedByConfirm: Confirm forgets an operator
// whatever its audit history (ka-0000005150).
func TestForgetOperator_GuardBypassedByConfirm(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}

	forgotAt := time.Date(2026, 6, 16, 0, 0, 0, 0, time.UTC)
	expectForgetLoad(mock, 42, nil, false)
	expectAuditCount(mock, 42, 5000)
	mock.ExpectQuery(`UPDATE operators\s+SET forgotten_at = NOW\(\)`).
		WithArgs(int64(42), "carol@example.com", "").
		WillReturnRows(sqlmock.NewRows([]string{"forgotten_at"}).AddRow(forgotAt))
	mock.ExpectExec(`UPDATE operator_slug_aliases`).
		WithArgs(int64(42)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	res, err := s.ForgetOperator(context.Background(), 42, store.ForgetOperatorOptions{
		Actor:               "carol@example.com",
		Confirm:             true,
		AuditGuardThreshold: 1000,
	})
	if err != nil {
		t.Fatalf("ForgetOperator with confirm: %v", err)
	}
	if res.AuditRows != 5000 {
		t.Errorf("AuditRows = %d, want 5000", res.AuditRows)
	}
	if res.SlugCleared {
		t.Error("SlugCleared = true, want false (slug was already null)")
	}
}

func TestForgetOperator_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	s := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT forgotten_at, slug IS NOT NULL`).
		WithArgs(int64(99)).
		WillReturnError(sql.ErrNoRows)

	_, err := s.ForgetOperator(context.Background(), 99, store.ForgetOperatorOptions{})
	var nf *store.OperatorNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected *OperatorNotFoundError, got %v", err)
	}
	if nf.OperatorID != 99 {
		t.Errorf("OperatorID = %d, want 99", nf.OperatorID)
	}
}

func TestForgetOperator_InvalidID(t *testing.T) {
	db, _ := newMockDB(t)
	s := &PostgresStore{db: db}
	// id <= 0 short-circuits to not-found without touching the DB (no mock
	// expectations registered).
	_, err := s.ForgetOperator(context.Background(), 0, store.ForgetOperatorOptions{})
	var nf *store.OperatorNotFoundError
	if !errors.As(err, &nf) {
		t.Fatalf("expected *OperatorNotFoundError, got %v", err)
	}
}
