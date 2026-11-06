package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestQueryLastEventAtByOperatorAliases pins the SQL shape and result mapping
// for the per-bridge last_event_at lookup that backs /v1/health/snapshot's
// bridges tile (ka-0000003660). The query must (1) JOIN events through
// operator_aliases on the kind/subject pair, (2) only return aliases that
// have at least one event, and (3) group by (kind, subject) so a MAX(created_at)
// is computed per alias.
func TestQueryLastEventAtByOperatorAliases(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	now := time.Now().UTC().Truncate(time.Second)
	rows := sqlmock.NewRows([]string{"alias", "last_at"}).
		AddRow("system:slack-bridge", now.Add(-1*time.Minute)).
		AddRow("fleet:gardenio", now.Add(-30*time.Second))
	mock.ExpectQuery(`FROM operator_aliases oa\s+JOIN events e ON e.operated_by_id = oa.operator_id`).
		WillReturnRows(rows)

	got, err := queryLastEventAtByOperatorAliases(context.Background(), db, []string{
		"system:slack-bridge", "system:jira-bridge", "system:gitlab-bridge", "fleet:gardenio",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len: got %d, want 2 (only aliases with events appear)", len(got))
	}
	if !got["system:slack-bridge"].Equal(now.Add(-1 * time.Minute)) {
		t.Errorf("slack-bridge: got %v, want %v", got["system:slack-bridge"], now.Add(-1*time.Minute))
	}
	if !got["fleet:gardenio"].Equal(now.Add(-30 * time.Second)) {
		t.Errorf("controller: got %v, want %v", got["fleet:gardenio"], now.Add(-30*time.Second))
	}
	if _, ok := got["system:jira-bridge"]; ok {
		t.Errorf("jira-bridge present in result: must be omitted when no events")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestQueryLastEventAtByOperatorAliases_Empty pins the cheap-path: when
// callers pass no aliases, the function returns an empty map without
// touching the database. The bridges tile never asks for an empty list,
// but defending the zero-call contract keeps a future caller from issuing
// an `IN ()` query.
func TestQueryLastEventAtByOperatorAliases_Empty(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	got, err := queryLastEventAtByOperatorAliases(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len: got %d, want 0", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
