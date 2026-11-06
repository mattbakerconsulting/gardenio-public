package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestQueryBackfillOperatedByLegacy_TargetNotFound(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SELECT id FROM operators WHERE kind = \\$1 AND subject = \\$2").
		WithArgs("slack", "UNOPE").
		WillReturnRows(sqlmock.NewRows([]string{"id"}))

	_, err := queryBackfillOperatedByLegacy(context.Background(), db, store.BackfillOperatedByLegacyOptions{
		Target: "slack:UNOPE",
	})
	if err == nil {
		t.Fatal("expected error when target operator missing")
	}
}

func TestQueryBackfillOperatedByLegacy_BadTarget(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryBackfillOperatedByLegacy(context.Background(), db, store.BackfillOperatedByLegacyOptions{
		Target: "not-a-colon",
	})
	if err == nil {
		t.Fatal("expected error for malformed target")
	}
}

func TestQueryBackfillOperatedByLegacy_EmptyCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery("SELECT id FROM operators WHERE kind = \\$1 AND subject = \\$2").
		WithArgs("slack", "U0OPERATOR1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery("SELECT id, created_by, COALESCE\\(fields").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_by", "fields"}))
	// Pass-2 dry-run count.
	mock.ExpectQuery("SELECT count\\(\\*\\)\\s+FROM beads b").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillOperatedByLegacy(context.Background(), db, store.BackfillOperatedByLegacyOptions{
		Target: "slack:U0OPERATOR1",
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TargetOperatorID != 42 {
		t.Errorf("TargetOperatorID = %d, want 42", stats.TargetOperatorID)
	}
	if stats.BeforeOpenCandidates != 0 || stats.AfterOpenCandidates != 0 {
		t.Errorf("counts = %d/%d, want 0/0", stats.BeforeOpenCandidates, stats.AfterOpenCandidates)
	}
	if stats.CategoryA+stats.CategoryB+stats.CategoryC+stats.CategoryD != 0 {
		t.Errorf("expected zero category counts, got A=%d B=%d C=%d D=%d",
			stats.CategoryA, stats.CategoryB, stats.CategoryC, stats.CategoryD)
	}
}

func TestClassifyBead_CategoryD_DirectCLI(t *testing.T) {
	db, _ := newMockDB(t)
	cat, _, err := classifyBead(context.Background(), db, candidateRow{
		id:        "gd-XXX",
		createdBy: "",
		fields:    json.RawMessage(`{}`),
	}, "U0OPERATOR1", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cat != "D" {
		t.Errorf("cat = %q, want D", cat)
	}
}

func TestClassifyBead_CategoryD_ThreadAttributedToTarget(t *testing.T) {
	db, _ := newMockDB(t)
	cat, _, err := classifyBead(context.Background(), db, candidateRow{
		id:        "gd-XXX",
		createdBy: "thread-foo",
		fields:    json.RawMessage(`{"slack_user_id":"U0OPERATOR1"}`),
	}, "U0OPERATOR1", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cat != "D" {
		t.Errorf("cat = %q, want D", cat)
	}
}

func TestClassifyBead_CategoryA_ThreadAgent(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT operator_id FROM operator_aliases WHERE kind = \\$1 AND subject = \\$2").
		WithArgs("slack", "U_OTHER").
		WillReturnRows(sqlmock.NewRows([]string{"operator_id"}).AddRow(int64(99)))

	cat, newOp, err := classifyBead(context.Background(), db, candidateRow{
		id:        "gd-YYY",
		createdBy: "thread-bar",
		fields:    json.RawMessage(`{"slack_user_id":"U_OTHER"}`),
	}, "U0OPERATOR1", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cat != "A" {
		t.Errorf("cat = %q, want A", cat)
	}
	if newOp != 99 {
		t.Errorf("newOp = %d, want 99", newOp)
	}
}

func TestClassifyBead_CategoryB_StaticSchedule(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT operated_by_id FROM beads WHERE id = \\$1").
		WithArgs("sched-foo").
		WillReturnRows(sqlmock.NewRows([]string{"operated_by_id"}).AddRow(int64(7)))

	cat, newOp, err := classifyBead(context.Background(), db, candidateRow{
		id:        "gd-ZZZ",
		createdBy: "static",
		fields:    json.RawMessage(`{"schedule_id":"sched-foo"}`),
	}, "U0OPERATOR1", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cat != "B" {
		t.Errorf("cat = %q, want B", cat)
	}
	if newOp != 7 {
		t.Errorf("newOp = %d, want 7", newOp)
	}
}

func TestClassifyBead_CategoryC_AgentSpawned(t *testing.T) {
	db, _ := newMockDB(t)
	cat, _, err := classifyBead(context.Background(), db, candidateRow{
		id:        "gd-WWW",
		createdBy: "gardenio-some-agent",
		fields:    json.RawMessage(`{}`),
	}, "U0OPERATOR1", 42)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cat != "C" {
		t.Errorf("cat = %q, want C", cat)
	}
}

func TestOptionsValidate_TargetRequired(t *testing.T) {
	opts := store.BackfillOperatedByLegacyOptions{}
	if err := opts.Validate(); err == nil {
		t.Fatal("expected error when Target empty")
	}
}

func TestOptionsValidate_FillsDefaults(t *testing.T) {
	opts := store.BackfillOperatedByLegacyOptions{Target: "slack:UXX"}
	if err := opts.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(opts.Statuses) == 0 || opts.Statuses[0] != "open" {
		t.Errorf("Statuses = %v, want [open]", opts.Statuses)
	}
	if opts.BatchSize != 200 {
		t.Errorf("BatchSize = %d, want 200", opts.BatchSize)
	}
	if opts.MaxIterations != 5 {
		t.Errorf("MaxIterations = %d, want 5", opts.MaxIterations)
	}
}

func TestOptionsTargetSubject(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"slack:U0OPERATOR1", "U0OPERATOR1"},
		{"kind:multi:colon", "multi:colon"},
		{"no-colon", ""},
		{"", ""},
	} {
		opts := store.BackfillOperatedByLegacyOptions{Target: tc.in}
		if got := opts.TargetSubject(); got != tc.want {
			t.Errorf("TargetSubject(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
