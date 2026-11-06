package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// Store methods GrantTaint/RevokeTaint/GetTaintGrant/ApplyTaint/GetBeadTaints
// against taint_grants/bead_taints (docs/adr/taints.md §3/§5.1/§7/§10.4),
// gd-sIMJB8eY3q.

func TestQueryGrantTaint_Creates(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)

	mock.ExpectQuery(`INSERT INTO taint_grants`).
		WithArgs(int64(1), "operator", "42", int64(7), &until).
		WillReturnRows(sqlmock.NewRows([]string{"granted_at"}).AddRow(now))

	g := &model.TaintGrant{
		TaintID:       1,
		PrincipalKind: "operator",
		Principal:     "42",
		GrantedBy:     7,
		ExpiresAt:     &until,
	}
	if err := queryGrantTaint(context.Background(), db, g); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !g.GrantedAt.Equal(now) {
		t.Fatalf("expected granted_at %v, got %v", now, g.GrantedAt)
	}
}

func TestQueryGrantTaint_RejectsUnknownPrincipalKind(t *testing.T) {
	db, _ := newMockDB(t)
	g := &model.TaintGrant{TaintID: 1, PrincipalKind: "human", Principal: "x", GrantedBy: 7}
	err := queryGrantTaint(context.Background(), db, g)
	assertValidationErrorOnField(t, err, "principal_kind")
}

func TestQueryGrantTaint_RejectsMissingPrincipal(t *testing.T) {
	db, _ := newMockDB(t)
	g := &model.TaintGrant{TaintID: 1, PrincipalKind: "team", Principal: "", GrantedBy: 7}
	err := queryGrantTaint(context.Background(), db, g)
	assertValidationErrorOnField(t, err, "principal")
}

func TestQueryGrantTaint_RejectsMissingGrantedBy(t *testing.T) {
	db, _ := newMockDB(t)
	g := &model.TaintGrant{TaintID: 1, PrincipalKind: "team", Principal: "gd-acme"}
	err := queryGrantTaint(context.Background(), db, g)
	assertValidationErrorOnField(t, err, "granted_by")
}

func TestQueryRevokeTaint_RemovesExisting(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec(`DELETE FROM taint_grants WHERE taint_id = \$1 AND principal_kind = \$2 AND principal = \$3`).
		WithArgs(int64(1), "team", "gd-acme").
		WillReturnResult(sqlmock.NewResult(0, 1))

	removed, err := queryRevokeTaint(context.Background(), db, 1, "team", "gd-acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !removed {
		t.Fatal("expected removed=true")
	}
}

func TestQueryRevokeTaint_IdempotentWhenNoMatch(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec(`DELETE FROM taint_grants WHERE taint_id = \$1 AND principal_kind = \$2 AND principal = \$3`).
		WithArgs(int64(1), "team", "gd-acme").
		WillReturnResult(sqlmock.NewResult(0, 0))

	removed, err := queryRevokeTaint(context.Background(), db, 1, "team", "gd-acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if removed {
		t.Fatal("expected removed=false")
	}
}

func TestQueryGetTaintGrant_Found(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT .+ FROM taint_grants WHERE taint_id = \$1 AND principal_kind = \$2 AND principal = \$3`).
		WithArgs(int64(1), "team", "gd-acme").
		WillReturnRows(sqlmock.NewRows([]string{"taint_id", "principal_kind", "principal", "granted_by", "granted_at", "expires_at"}).
			AddRow(int64(1), "team", "gd-acme", int64(7), now, nil))

	got, err := queryGetTaintGrant(context.Background(), db, 1, "team", "gd-acme")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Principal != "gd-acme" || got.ExpiresAt != nil {
		t.Fatalf("unexpected grant: %+v", got)
	}
}

func TestQueryGetTaintGrant_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT .+ FROM taint_grants WHERE taint_id = \$1 AND principal_kind = \$2 AND principal = \$3`).
		WithArgs(int64(1), "team", "gd-ghost").
		WillReturnError(sql.ErrNoRows)

	if _, err := queryGetTaintGrant(context.Background(), db, 1, "team", "gd-ghost"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestQueryApplyTaint_Writes(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec(`INSERT INTO bead_taints`).
		WithArgs("gd-bead1", int64(1), "explicit", (*string)(nil), "gd-caller").
		WillReturnResult(sqlmock.NewResult(0, 1))

	bt := &model.BeadTaint{
		BeadID:    "gd-bead1",
		TaintID:   1,
		Source:    model.TaintSourceExplicit,
		AppliedBy: "gd-caller",
	}
	inserted, err := queryApplyTaint(context.Background(), db, bt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !inserted {
		t.Fatal("expected inserted=true for a new apply")
	}
}

func TestQueryApplyTaint_NoopOnConflictReportsNotInserted(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec(`INSERT INTO bead_taints`).
		WithArgs("gd-bead1", int64(1), "explicit", (*string)(nil), "gd-caller").
		WillReturnResult(sqlmock.NewResult(0, 0))

	bt := &model.BeadTaint{
		BeadID:    "gd-bead1",
		TaintID:   1,
		Source:    model.TaintSourceExplicit,
		AppliedBy: "gd-caller",
	}
	inserted, err := queryApplyTaint(context.Background(), db, bt)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if inserted {
		t.Fatal("expected inserted=false on ON CONFLICT DO NOTHING no-op")
	}
}

func TestQueryApplyTaint_RejectsMissingFields(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryApplyTaint(context.Background(), db, &model.BeadTaint{})
	assertValidationErrorOnField(t, err, "bead_id")
}

func TestQueryRecordTaintAudit_Writes(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`INSERT INTO taint_audit`).
		WithArgs(int64(1), model.TaintAuditGrant, (*string)(nil), (*string)(nil), "gd-caller", (*string)(nil), (*string)(nil)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), now))

	a := &model.TaintAudit{
		TaintID: 1,
		Action:  model.TaintAuditGrant,
		Actor:   "gd-caller",
	}
	if err := queryRecordTaintAudit(context.Background(), db, a); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.ID != 7 || !a.CreatedAt.Equal(now) {
		t.Fatalf("expected id/created_at to be populated, got %+v", a)
	}
}

func TestQueryRecordTaintAudit_PopulatesBeadIDForApply(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`INSERT INTO taint_audit`).
		WithArgs(int64(1), model.TaintAuditApply, sqlmock.AnyArg(), (*string)(nil), "gd-caller", (*string)(nil), (*string)(nil)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(8), time.Now().UTC()))

	a := &model.TaintAudit{
		TaintID: 1,
		Action:  model.TaintAuditApply,
		BeadID:  "gd-bead1",
		Actor:   "gd-caller",
	}
	if err := queryRecordTaintAudit(context.Background(), db, a); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryListTaintGrants_ReturnsAllIncludingExpired(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	past := now.Add(-time.Hour)

	mock.ExpectQuery(`SELECT .+ FROM taint_grants WHERE taint_id = \$1 ORDER BY granted_at DESC`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"taint_id", "principal_kind", "principal", "granted_by", "granted_at", "expires_at"}).
			AddRow(int64(1), "team", "gd-acme", int64(7), now, nil).
			AddRow(int64(1), "operator", "42", int64(7), now.Add(-time.Hour), &past))

	got, err := queryListTaintGrants(context.Background(), db, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 grants (including expired), got %d", len(got))
	}
	if got[1].ExpiresAt == nil || !got[1].ExpiresAt.Equal(past) {
		t.Fatalf("expected second row to carry the expired expires_at, got %+v", got[1])
	}
}

func TestQueryListTaintGrants_EmptyWhenNone(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT .+ FROM taint_grants WHERE taint_id = \$1 ORDER BY granted_at DESC`).
		WithArgs(int64(9)).
		WillReturnRows(sqlmock.NewRows([]string{"taint_id", "principal_kind", "principal", "granted_by", "granted_at", "expires_at"}))

	got, err := queryListTaintGrants(context.Background(), db, 9)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no grants, got %d", len(got))
	}
}

func TestQueryGetBeadTaintsForTaint_KeysByBeadID(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT .+ FROM bead_taints bt`).
		WithArgs(int64(1), pq.Array([]string{"gd-bead1", "gd-bead2"})).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "taint_id", "name", "source", "source_bead_id", "applied_by", "applied_at"}).
			AddRow("gd-bead1", int64(1), "acme-production", "explicit", nil, "gd-caller", now).
			AddRow("gd-bead2", int64(1), "acme-production", "inherit:create", "gd-parent", "gd-system", now))

	got, err := queryGetBeadTaintsForTaint(context.Background(), db, 1, []string{"gd-bead1", "gd-bead2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got["gd-bead1"].Source != "explicit" {
		t.Fatalf("unexpected source for gd-bead1: %+v", got["gd-bead1"])
	}
	if got["gd-bead2"].SourceBeadID != "gd-parent" {
		t.Fatalf("unexpected source_bead_id for gd-bead2: %+v", got["gd-bead2"])
	}
}

func TestQueryGetBeadTaintsForTaint_EmptyBeadIDsShortCircuits(t *testing.T) {
	db, _ := newMockDB(t)
	got, err := queryGetBeadTaintsForTaint(context.Background(), db, 1, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil map, got %+v", got)
	}
}

func TestQueryGetBeadTaints_ListsJoinedWithName(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT .+ FROM bead_taints bt`).
		WithArgs("gd-bead1").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "taint_id", "name", "source", "source_bead_id", "applied_by", "applied_at"}).
			AddRow("gd-bead1", int64(1), "acme-production", "explicit", nil, "gd-caller", now).
			AddRow("gd-bead1", int64(2), "jira-prod", "inherit:create", "gd-parent", "gd-system", now))

	got, err := queryGetBeadTaints(context.Background(), db, "gd-bead1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 bead taints, got %d", len(got))
	}
	if got[0].TaintName != "acme-production" || got[0].SourceBeadID != "" {
		t.Fatalf("unexpected first row: %+v", got[0])
	}
	if got[1].SourceBeadID != "gd-parent" {
		t.Fatalf("unexpected second row source_bead_id: %+v", got[1])
	}
}
