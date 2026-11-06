package postgres

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryBackfillOperatedByAgents_HappyPath drives one full pass:
// sentinel resolves, scan returns one agent bead carrying
// fields.slack_user_id, the alias lookup resolves to op 99, and the
// UPDATE + mutation_history pair commits.
func TestQueryBackfillOperatedByAgents_HappyPath(t *testing.T) {
	db, mock := newMockDB(t)

	// Sentinel lookup.
	mock.ExpectQuery(`SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))

	// First scan page — one candidate.
	mock.ExpectQuery(`SELECT id, COALESCE\(fields`).
		WithArgs(int64(1), "", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fields"}).
			AddRow("gd-AAA", []byte(`{"slack_user_id":"U_ALICE"}`)))

	// Slack alias resolves to op 99.
	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("slack", "U_ALICE").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(99)))

	// Apply batch transaction.
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE beads\s+SET operated_by_id = \$1\s+WHERE id = \$2\s+AND type = 'agent'\s+AND \(operated_by_id IS NULL OR operated_by_id = \$3\)`).
		WithArgs(int64(99), "gd-AAA", int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO mutation_history`).
		WithArgs("gd-AAA", "kd admin backfill-operated-by-agents", "operated_by_id", "", "99", "backfill-operated-by-agents").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stats, err := queryBackfillOperatedByAgents(context.Background(), db, store.BackfillOperatedByAgentsOptions{
		IncludeSentinel: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed = %d, want 1", stats.Processed)
	}
	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}
	if stats.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", stats.Skipped)
	}
	if got := stats.RebindBySignal["slack_user_id"]; got != 1 {
		t.Errorf("RebindBySignal[slack_user_id] = %d, want 1", got)
	}
	if stats.LastID != "gd-AAA" {
		t.Errorf("LastID = %q, want gd-AAA", stats.LastID)
	}
}

// TestQueryBackfillOperatedByAgents_SkipsUnresolvable verifies that an
// agent bead with no resolvable identity fields is counted as Skipped
// and not written.
func TestQueryBackfillOperatedByAgents_SkipsUnresolvable(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"})) // sentinel absent

	// includeSentinel=false path: WHERE operated_by_id IS NULL only.
	mock.ExpectQuery(`SELECT id, COALESCE\(fields`).
		WithArgs("", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fields"}).
			AddRow("gd-BBB", []byte(`{}`)))

	stats, err := queryBackfillOperatedByAgents(context.Background(), db, store.BackfillOperatedByAgentsOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Processed != 1 {
		t.Errorf("Processed = %d, want 1", stats.Processed)
	}
	if stats.Updated != 0 {
		t.Errorf("Updated = %d, want 0", stats.Updated)
	}
	if stats.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1", stats.Skipped)
	}
}

// TestQueryBackfillOperatedByAgents_DryRun verifies that DryRun runs
// the resolver but does not begin a transaction or write UPDATEs.
func TestQueryBackfillOperatedByAgents_DryRun(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT id FROM operators WHERE kind = 'system' AND subject = 'legacy-pre-attribution'`).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(1)))

	mock.ExpectQuery(`SELECT id, COALESCE\(fields`).
		WithArgs(int64(1), "", 500).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fields"}).
			AddRow("gd-CCC", []byte(`{"gitlab_username":"alice"}`)))

	mock.ExpectQuery(`SELECT operator_id FROM operator_aliases WHERE kind = \$1 AND subject = \$2`).
		WithArgs("gitlab", "alice").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(7)))

	stats, err := queryBackfillOperatedByAgents(context.Background(), db, store.BackfillOperatedByAgentsOptions{
		IncludeSentinel: true,
		DryRun:          true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stats.DryRun {
		t.Error("expected DryRun=true on stats")
	}
	if stats.Processed != 1 {
		t.Errorf("Processed = %d, want 1", stats.Processed)
	}
	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}
	if stats.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0", stats.Skipped)
	}
	if got := stats.RebindBySignal["gitlab_username"]; got != 1 {
		t.Errorf("RebindBySignal[gitlab_username] = %d, want 1", got)
	}
}

func TestBackfillOperatedByAgentsOptions_Validate(t *testing.T) {
	opts := store.BackfillOperatedByAgentsOptions{}
	if err := opts.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.BatchSize != 500 {
		t.Errorf("BatchSize = %d, want 500", opts.BatchSize)
	}

	bad := store.BackfillOperatedByAgentsOptions{BatchSize: -1}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for negative BatchSize")
	}
}
