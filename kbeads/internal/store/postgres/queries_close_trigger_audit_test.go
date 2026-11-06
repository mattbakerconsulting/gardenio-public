package postgres

import (
	"context"
	"regexp"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// triggerAuditPageRE matches the candidate-page SELECT used by
// fetchTriggerAuditPage. Anchored on the EXISTS-on-labels subquery and
// the LIMIT clause.
var triggerAuditPageRE = regexp.MustCompile(`SELECT b\.id\s+FROM beads b[\s\S]+auto:trigger-audit[\s\S]+LIMIT \$2`)

// triggerAuditUpdateRE matches the per-batch UPDATE in
// closeTriggerAuditBatch.
var triggerAuditUpdateRE = regexp.MustCompile(`UPDATE beads\s+SET status = 'closed'[\s\S]+id = ANY\(\$3::text\[\]\)`)

func TestQueryCloseTriggerAuditBeads_NoCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(triggerAuditPageRE.String()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	stats, err := queryCloseTriggerAuditBeads(context.Background(), db, store.CloseTriggerAuditBeadsOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.CandidateBeads != 0 || stats.BeadsClosed != 0 {
		t.Errorf("counts = %d/%d, want 0/0", stats.CandidateBeads, stats.BeadsClosed)
	}
	if stats.Batches != 0 {
		t.Errorf("Batches = %d, want 0", stats.Batches)
	}
}

func TestQueryCloseTriggerAuditBeads_DryRunCountsCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"id"}).
		AddRow("gd-fnhtkrbdiq").
		AddRow("gd-hkbgwaothb").
		AddRow("gd-7yfi0rsi0u")

	mock.ExpectQuery(triggerAuditPageRE.String()).
		WillReturnRows(rows)

	stats, err := queryCloseTriggerAuditBeads(context.Background(), db, store.CloseTriggerAuditBeadsOptions{
		DryRun:    true,
		BatchSize: 100, // larger than page returned, terminates loop
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.CandidateBeads != 3 {
		t.Errorf("CandidateBeads = %d, want 3", stats.CandidateBeads)
	}
	if stats.BeadsClosed != 3 {
		t.Errorf("BeadsClosed = %d, want 3 (dry-run reports would-be closes)", stats.BeadsClosed)
	}
	if stats.LastID != "gd-7yfi0rsi0u" {
		t.Errorf("LastID = %q, want gd-7yfi0rsi0u", stats.LastID)
	}
}

func TestQueryCloseTriggerAuditBeads_RealRunUpdatesPerBatch(t *testing.T) {
	db, mock := newMockDB(t)

	// First page returns two ids.
	mock.ExpectQuery(triggerAuditPageRE.String()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("gd-fnhtkrbdiq").
			AddRow("gd-hkbgwaothb"))
	mock.ExpectBegin()
	mock.ExpectExec(triggerAuditUpdateRE.String()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	stats, err := queryCloseTriggerAuditBeads(context.Background(), db, store.CloseTriggerAuditBeadsOptions{
		BatchSize: 100, // larger than page, terminates loop
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeadsClosed != 2 {
		t.Errorf("BeadsClosed = %d, want 2", stats.BeadsClosed)
	}
	if stats.Batches != 1 {
		t.Errorf("Batches = %d, want 1", stats.Batches)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestQueryCloseTriggerAuditBeads_MaxBatchesCap(t *testing.T) {
	db, mock := newMockDB(t)

	// Page exactly fills BatchSize, so loop would continue but MaxBatches=1 caps it.
	mock.ExpectQuery(triggerAuditPageRE.String()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).
			AddRow("gd-fnhtkrbdiq").
			AddRow("gd-hkbgwaothb"))
	mock.ExpectBegin()
	mock.ExpectExec(triggerAuditUpdateRE.String()).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	stats, err := queryCloseTriggerAuditBeads(context.Background(), db, store.CloseTriggerAuditBeadsOptions{
		BatchSize:  2,
		MaxBatches: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stats.BatchesCapped {
		t.Errorf("BatchesCapped = false, want true (page == BatchSize and we hit MaxBatches)")
	}
	if stats.Batches != 1 {
		t.Errorf("Batches = %d, want 1", stats.Batches)
	}
}

func TestQueryCloseTriggerAuditBeads_ValidatesBatchSize(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryCloseTriggerAuditBeads(context.Background(), db, store.CloseTriggerAuditBeadsOptions{
		BatchSize: -1,
	})
	if err == nil {
		t.Fatalf("expected error for negative BatchSize, got nil")
	}
}
