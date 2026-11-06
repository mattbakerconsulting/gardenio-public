package postgres

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// queryListTaintAudit backs the viewer's Audit tab (docs/adr/taints.md §9.1),
// gd-nOnc1Q8Zcv: filterable by action/actor, paged by limit/offset.

func TestQueryListTaintAudit_Unfiltered(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM taint_audit WHERE taint_id = \$1`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	mock.ExpectQuery(`SELECT .+ FROM taint_audit WHERE taint_id = \$1 ORDER BY created_at DESC`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "taint_id", "action", "bead_id", "request_id", "actor", "approval_mode", "content_sha", "created_at"}).
			AddRow(int64(2), int64(1), model.TaintAuditGrant, nil, nil, "gd-caller", nil, nil, now).
			AddRow(int64(1), int64(1), model.TaintAuditApply, "gd-bead1", nil, "gd-system", nil, "abc123", now.Add(-time.Hour)))

	got, total, err := queryListTaintAudit(context.Background(), db, 1, model.TaintAuditFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected total=2, got %d", total)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(got))
	}
	if got[1].BeadID != "gd-bead1" || got[1].ContentSHA != "abc123" {
		t.Fatalf("unexpected second row: %+v", got[1])
	}
}

func TestQueryListTaintAudit_FiltersByActionAndActor(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM taint_audit WHERE taint_id = \$1 AND action = \$2 AND actor = \$3`).
		WithArgs(int64(1), model.TaintAuditScrubApprove, "gd-approver").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	mock.ExpectQuery(`SELECT .+ FROM taint_audit WHERE taint_id = \$1 AND action = \$2 AND actor = \$3 ORDER BY created_at DESC`).
		WithArgs(int64(1), model.TaintAuditScrubApprove, "gd-approver").
		WillReturnRows(sqlmock.NewRows([]string{"id", "taint_id", "action", "bead_id", "request_id", "actor", "approval_mode", "content_sha", "created_at"}).
			AddRow(int64(3), int64(1), model.TaintAuditScrubApprove, nil, "gd-req1", "gd-approver", "human", nil, now))

	got, total, err := queryListTaintAudit(context.Background(), db, 1, model.TaintAuditFilter{
		Action: string(model.TaintAuditScrubApprove),
		Actor:  "gd-approver",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 || len(got) != 1 {
		t.Fatalf("expected 1 filtered row, got total=%d len=%d", total, len(got))
	}
	if got[0].RequestID != "gd-req1" || got[0].ApprovalMode != "human" {
		t.Fatalf("unexpected row: %+v", got[0])
	}
}

func TestQueryListTaintAudit_LimitAndOffset(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM taint_audit WHERE taint_id = \$1`).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(5))
	mock.ExpectQuery(`SELECT .+ FROM taint_audit WHERE taint_id = \$1 ORDER BY created_at DESC LIMIT \$2 OFFSET \$3`).
		WithArgs(int64(1), 10, 20).
		WillReturnRows(sqlmock.NewRows([]string{"id", "taint_id", "action", "bead_id", "request_id", "actor", "approval_mode", "content_sha", "created_at"}).
			AddRow(int64(4), int64(1), model.TaintAuditRevoke, nil, nil, "gd-caller", nil, nil, now))

	got, total, err := queryListTaintAudit(context.Background(), db, 1, model.TaintAuditFilter{Limit: 10, Offset: 20})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 5 {
		t.Fatalf("expected total=5 (unpaged count), got %d", total)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 page row, got %d", len(got))
	}
}
