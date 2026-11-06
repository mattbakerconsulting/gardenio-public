package postgres

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestQueryBackfillOwner_TargetRequired(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryBackfillOwner(context.Background(), db, store.BackfillOwnerOptions{})
	if err == nil {
		t.Fatal("expected error for empty Target")
	}
}

func TestQueryBackfillOwner_EmptyCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	// before count
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WithArgs("slack:U0OPERATOR1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	// initial scan (no since path)
	mock.ExpectQuery("SELECT id, COALESCE\\(created_by").
		WithArgs("slack:U0OPERATOR1", "", 5000).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_by", "fields"}))
	// after count
	mock.ExpectQuery("SELECT count\\(\\*\\) FROM beads").
		WithArgs("slack:U0OPERATOR1").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))

	stats, err := queryBackfillOwner(context.Background(), db, store.BackfillOwnerOptions{
		Target: "slack:U0OPERATOR1",
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.Target != "slack:U0OPERATOR1" {
		t.Errorf("Target = %q", stats.Target)
	}
	if stats.CandidateBeads != 0 || stats.Batches != 0 {
		t.Errorf("expected zero candidates, got beads=%d batches=%d", stats.CandidateBeads, stats.Batches)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestIsOwnerSentinelSpawner(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"scheduler:cron", true},
		{"trigger:bead", true},
		{"escalation-bridge", true},
		{"system:unowned", true},
		{"fleet:default", true},
		{"alice-fleetlead-2026-01-01t12-00", false},
		{"thread-abc", false},
		{"", false},
	} {
		if got := isOwnerSentinelSpawner(c.in); got != c.want {
			t.Errorf("isOwnerSentinelSpawner(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestIsHumanShapedOperator(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"slack:U0OPERATOR1", true},
		{"oidc:alice@example.com", true},
		{"system:unowned", false},
		{"fleet:default", false},
		{"alice", false},
		{"", false},
	} {
		if got := isHumanShapedOperator(c.in); got != c.want {
			t.Errorf("isHumanShapedOperator(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
