package postgres

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestQueryFleetTypeStatusCounts(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"type", "status", "count"}).
		AddRow("task", "open", 5).
		AddRow("task", "closed", 3).
		AddRow("bug", "open", 2)
	mock.ExpectQuery(`SELECT type, status, COUNT\(\*\) FROM beads WHERE deleted_at IS NULL GROUP BY type, status`).
		WillReturnRows(rows)

	result, err := queryFleetTypeStatusCounts(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result[store.FleetTypeStatusKey{Type: "task", Status: "open"}]; got != 5 {
		t.Errorf("task/open: want 5, got %d", got)
	}
	if got := result[store.FleetTypeStatusKey{Type: "task", Status: "closed"}]; got != 3 {
		t.Errorf("task/closed: want 3, got %d", got)
	}
	if got := result[store.FleetTypeStatusKey{Type: "bug", Status: "open"}]; got != 2 {
		t.Errorf("bug/open: want 2, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryFleetQueueDepthByProject(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"project", "type", "count"}).
		AddRow("gardenio", "decision", 3).
		AddRow("gardenio", "escalation", 7).
		AddRow("gardenio", "mail", 12).
		AddRow("unknown", "escalation", 1)
	// GROUP BY 1: beads has a project column, which GROUP BY project would
	// bind instead of the output alias, and Postgres rejects the query.
	mock.ExpectQuery(`FROM beads b\s+WHERE b.deleted_at IS NULL\s+AND b.status = 'open'\s+AND b.type = ANY\(\$1::text\[\]\)\s+GROUP BY 1, b.type$`).
		WillReturnRows(rows)

	result, err := queryFleetQueueDepthByProject(context.Background(), db, store.FleetQueueTypes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result[store.FleetQueueDepthKey{Project: "gardenio", Type: "decision"}]; got != 3 {
		t.Errorf("gardenio/decision: want 3, got %d", got)
	}
	if got := result[store.FleetQueueDepthKey{Project: "gardenio", Type: "escalation"}]; got != 7 {
		t.Errorf("gardenio/escalation: want 7, got %d", got)
	}
	if got := result[store.FleetQueueDepthKey{Project: "gardenio", Type: "mail"}]; got != 12 {
		t.Errorf("gardenio/mail: want 12, got %d", got)
	}
	if got := result[store.FleetQueueDepthKey{Project: "unknown", Type: "escalation"}]; got != 1 {
		t.Errorf("unknown/escalation: want 1, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryFleetQueueOldestAgeByProject(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"project", "type", "oldest_age_seconds"}).
		AddRow("gardenio", "decision", 7200.0).
		AddRow("gardenio", "escalation", 345.5).
		AddRow("unknown", "mail", 90000.0)
	mock.ExpectQuery(`EXTRACT\(EPOCH FROM \(now\(\) - MIN\(b.created_at\)\)\).*FROM beads b\s+WHERE b.deleted_at IS NULL\s+AND b.status = 'open'\s+AND b.type = ANY\(\$1::text\[\]\)\s+GROUP BY 1, b.type$`).
		WillReturnRows(rows)

	result, err := queryFleetQueueOldestAgeByProject(context.Background(), db, store.FleetQueueTypes)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := result[store.FleetQueueDepthKey{Project: "gardenio", Type: "decision"}]; got != 7200.0 {
		t.Errorf("gardenio/decision: want 7200, got %v", got)
	}
	if got := result[store.FleetQueueDepthKey{Project: "gardenio", Type: "escalation"}]; got != 345.5 {
		t.Errorf("gardenio/escalation: want 345.5, got %v", got)
	}
	if got := result[store.FleetQueueDepthKey{Project: "unknown", Type: "mail"}]; got != 90000.0 {
		t.Errorf("unknown/mail: want 90000, got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryFleetActiveAgentsByProject(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"project", "count"}).
		AddRow("gardenio", 12).
		AddRow("unknown", 1)
	mock.ExpectQuery(`WHERE b.type = 'agent'\s+AND b.status IN \('open', 'in_progress'\)\s+AND b.deleted_at IS NULL\s+GROUP BY 1$`).
		WillReturnRows(rows)

	result, err := queryFleetActiveAgentsByProject(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["gardenio"] != 12 || result["unknown"] != 1 || len(result) != 2 {
		t.Errorf("unexpected active agents: %+v", result)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryFleetAgentLifetimeRollup(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"project", "total_agents", "total_cost_usd", "session_count"}).
		AddRow("gardenio", 4, 19.75, 4.0).
		AddRow("other", 1, 5.0, 2.0).
		AddRow("unknown", 0, 0.0, 0.0)
	mock.ExpectQuery(`WITH agent_values AS MATERIALIZED`).
		WillReturnRows(rows)

	result, err := queryFleetAgentLifetimeRollup(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 3 {
		t.Fatalf("want 3 rows, got %d", len(result))
	}

	byProject := map[string]store.FleetAgentProjectRollup{}
	for _, r := range result {
		byProject[r.Project] = r
	}
	if r := byProject["gardenio"]; r.TotalAgents != 4 || r.TotalCostUSD != 19.75 || r.SessionCount != 4.0 {
		t.Errorf("gardenio row mismatch: %+v", r)
	}
	if r := byProject["other"]; r.TotalAgents != 1 || r.TotalCostUSD != 5.0 || r.SessionCount != 2.0 {
		t.Errorf("other row mismatch: %+v", r)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
