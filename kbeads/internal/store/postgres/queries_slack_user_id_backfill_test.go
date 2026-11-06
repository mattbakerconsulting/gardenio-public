package postgres

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestThreadTSFromCreatedBy(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"thread-1786497645-713979", "1786497645.713979"},
		{"thread-1700000000-000100", "1700000000.000100"},
		{"static", ""},                   // not a thread- prefix
		{"thread-", ""},                  // empty ts
		{"thread-noseparator", ""},       // no dash to split on
		{"thread-1786497645-", ""},       // trailing dash → no fractional part
		{"gardenio-some-agent-slug", ""}, // not a thread- prefix
	}
	for _, c := range cases {
		if got := threadTSFromCreatedBy(c.in); got != c.want {
			t.Errorf("threadTSFromCreatedBy(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBackfillSlackUserID_BadBatchSize(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryBackfillSlackUserID(context.Background(), db, store.BackfillSlackUserIDOptions{
		BatchSize: -1,
	})
	if err == nil {
		t.Fatal("expected error for negative batch size")
	}
}

func TestBackfillSlackUserID_EmptyCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	// before count
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	// scan first batch → empty
	mock.ExpectQuery("SELECT id, created_by, COALESCE\\(fields").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_by", "fields"}))
	// after count
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillSlackUserID(context.Background(), db, store.BackfillSlackUserIDOptions{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.BeforeMissing != 0 || stats.AfterMissing != 0 || stats.Updated != 0 {
		t.Errorf("counts = before %d after %d updated %d, want 0/0/0",
			stats.BeforeMissing, stats.AfterMissing, stats.Updated)
	}
}

// TestBackfillSlackUserID_DryRunResolveViaThreadID exercises the full scan:
// one candidate resolves via fields.thread_id, one via created_by ts, and one
// is unresolved. DryRun makes it read-only (no UPDATE / mutation_history).
func TestBackfillSlackUserID_DryRunResolveBothStrategies(t *testing.T) {
	db, mock := newMockDB(t)

	// before count
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	// batch scan
	mock.ExpectQuery("SELECT id, created_by, COALESCE\\(fields").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_by", "fields"}).
			AddRow("gd-aaa", "thread-1786497645-713979", []byte(`{"thread_id":"gd-root1"}`)).
			AddRow("gd-bbb", "thread-1786411836-176999", []byte(`{}`)).
			AddRow("gd-ccc", "thread-9999999999-000001", []byte(`{}`)))

	// gd-aaa: strategy 1 lookup on gd-root1 → resolves
	mock.ExpectQuery("SELECT fields->>'slack_user_id' FROM beads WHERE id = \\$1").
		WithArgs("gd-root1").
		WillReturnRows(sqlmock.NewRows([]string{"slack_user_id"}).AddRow("U0PERSON009"))

	// gd-bbb: no thread_id → strategy 2 by slack_thread_ts → resolves
	mock.ExpectQuery("SELECT fields->>'slack_user_id'\\s+FROM beads\\s+WHERE created_by = 'slack-bridge'").
		WithArgs("1786411836.176999").
		WillReturnRows(sqlmock.NewRows([]string{"slack_user_id"}).AddRow("U0OPERATOR1"))

	// gd-ccc: strategy 2 → no matching root
	mock.ExpectQuery("SELECT fields->>'slack_user_id'\\s+FROM beads\\s+WHERE created_by = 'slack-bridge'").
		WithArgs("9999999999.000001").
		WillReturnRows(sqlmock.NewRows([]string{"slack_user_id"}))

	// after count (dry-run: unchanged)
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))

	stats, err := queryBackfillSlackUserID(context.Background(), db, store.BackfillSlackUserIDOptions{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Updated != 2 {
		t.Errorf("Updated = %d, want 2", stats.Updated)
	}
	if stats.ResolvedViaThreadID != 1 {
		t.Errorf("ResolvedViaThreadID = %d, want 1", stats.ResolvedViaThreadID)
	}
	if stats.ResolvedViaCreatedBy != 1 {
		t.Errorf("ResolvedViaCreatedBy = %d, want 1", stats.ResolvedViaCreatedBy)
	}
	if stats.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1", stats.Unresolved)
	}
}

// TestBackfillSlackUserID_ApplyWritesMutationHistory verifies the write path:
// one resolved candidate produces an UPDATE grouped by slack_user_id plus one
// mutation_history row, wrapped in a transaction.
func TestBackfillSlackUserID_ApplyWritesMutationHistory(t *testing.T) {
	db, mock := newMockDB(t)

	// before count
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	// batch scan
	mock.ExpectQuery("SELECT id, created_by, COALESCE\\(fields").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_by", "fields"}).
			AddRow("gd-aaa", "thread-1786497645-713979", []byte(`{"thread_id":"gd-root1"}`)))
	// strategy 1 lookup
	mock.ExpectQuery("SELECT fields->>'slack_user_id' FROM beads WHERE id = \\$1").
		WithArgs("gd-root1").
		WillReturnRows(sqlmock.NewRows([]string{"slack_user_id"}).AddRow("U0PERSON009"))

	// apply transaction: UPDATE + one mutation_history INSERT + commit
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE beads\\s+SET fields = jsonb_set").
		WithArgs("U0PERSON009", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO mutation_history").
		WithArgs("gd-aaa", "kd admin backfill-slack-user-id", "slack_user_id", "", "U0PERSON009", "backfill-slack-user-id").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	// after count
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillSlackUserID(context.Background(), db, store.BackfillSlackUserIDOptions{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Updated != 1 {
		t.Errorf("Updated = %d, want 1", stats.Updated)
	}
	if stats.AfterMissing != 0 {
		t.Errorf("AfterMissing = %d, want 0", stats.AfterMissing)
	}
}
