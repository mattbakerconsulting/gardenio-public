package postgres

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// triggeredByPageQueryRE matches the candidate-page SELECT used by
// fetchTriggeredByBackfillPage. Anchored on the slack_thread_bead_id
// signal-field extraction and the LIMIT clause.
var triggeredByPageQueryRE = regexp.MustCompile(`SELECT b\.id,\s+NULLIF\(b\.fields->>'slack_thread_bead_id'[\s\S]+LIMIT \$3`)

func TestQueryBackfillTriggeredByDeps_NoCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(triggeredByPageQueryRE.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "slack_thread_bead_id", "trigger_id",
			"slack_target_exists", "trigger_target_exists",
			"has_existing_parent_dep", "has_existing_trigger_dep",
		}))

	stats, err := queryBackfillTriggeredByDeps(context.Background(), db, store.BackfillTriggeredByDepsOptions{
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.CandidateBeads != 0 || stats.EdgesAdded != 0 {
		t.Errorf("counts = %d/%d, want 0/0", stats.CandidateBeads, stats.EdgesAdded)
	}
	if stats.Batches != 0 {
		t.Errorf("Batches = %d, want 0", stats.Batches)
	}
}

func TestQueryBackfillTriggeredByDeps_DryRunCountsBySignal(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"id", "slack_thread_bead_id", "trigger_id",
		"slack_target_exists", "trigger_target_exists",
		"has_existing_parent_dep", "has_existing_trigger_dep",
	}).
		// Eligible: slack-thread signal only.
		AddRow("gd-fnhtkrbdiq",
			sql.NullString{String: "gd-thread1", Valid: true}, sql.NullString{},
			true, false, false, false).
		// Eligible: trigger signal only.
		AddRow("gd-hkbgwaothb",
			sql.NullString{}, sql.NullString{String: "gd-trig1", Valid: true},
			false, true, false, false).
		// Eligible: both signals point at same target (slack signal wins, single edge).
		AddRow("gd-7yfi0rsi0u",
			sql.NullString{String: "gd-same", Valid: true}, sql.NullString{String: "gd-same", Valid: true},
			true, true, false, false).
		// Skip: existing triggered-by dep.
		AddRow("gd-5vclrivk3g",
			sql.NullString{String: "gd-thread2", Valid: true}, sql.NullString{},
			true, false, false, true).
		// Skip: existing parent-child dep.
		AddRow("gd-fa6dcnnhzl",
			sql.NullString{String: "gd-thread3", Valid: true}, sql.NullString{},
			true, false, true, false).
		// Skip: self-reference (slack thread points at the candidate itself).
		AddRow("gd-efhcawukaf",
			sql.NullString{String: "gd-efhcawukaf", Valid: true}, sql.NullString{},
			true, false, false, false).
		// Skip: signal target missing/soft-deleted.
		AddRow("gd-f3dsu2bn2s",
			sql.NullString{String: "gd-gone", Valid: true}, sql.NullString{},
			false, false, false, false).
		// Skip: ambiguous — two signals disagree on target.
		AddRow("gd-5ckeclr4th",
			sql.NullString{String: "gd-alpha", Valid: true}, sql.NullString{String: "gd-beta", Valid: true},
			true, true, false, false)

	mock.ExpectQuery(triggeredByPageQueryRE.String()).
		WillReturnRows(rows)

	stats, err := queryBackfillTriggeredByDeps(context.Background(), db, store.BackfillTriggeredByDepsOptions{
		DryRun:    true,
		BatchSize: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.CandidateBeads != 8 {
		t.Errorf("CandidateBeads = %d, want 8", stats.CandidateBeads)
	}
	if stats.EdgesAdded != 3 {
		t.Errorf("EdgesAdded = %d, want 3", stats.EdgesAdded)
	}
	if got := stats.EdgesBySignal["slack_thread_bead_id"]; got != 2 {
		t.Errorf("EdgesBySignal[slack_thread_bead_id] = %d, want 2", got)
	}
	if got := stats.EdgesBySignal["trigger_id"]; got != 1 {
		t.Errorf("EdgesBySignal[trigger_id] = %d, want 1", got)
	}
	if stats.SkippedExistingDep != 2 {
		t.Errorf("SkippedExistingDep = %d, want 2", stats.SkippedExistingDep)
	}
	if stats.SkippedSelfReference != 1 {
		t.Errorf("SkippedSelfReference = %d, want 1", stats.SkippedSelfReference)
	}
	if stats.SkippedTargetMissing != 1 {
		t.Errorf("SkippedTargetMissing = %d, want 1", stats.SkippedTargetMissing)
	}
	if stats.SkippedAmbiguous != 1 {
		t.Errorf("SkippedAmbiguous = %d, want 1", stats.SkippedAmbiguous)
	}
	if stats.Batches != 1 {
		t.Errorf("Batches = %d, want 1", stats.Batches)
	}
}

func TestQueryBackfillTriggeredByDeps_LiveInsertsAndCommits(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"id", "slack_thread_bead_id", "trigger_id",
		"slack_target_exists", "trigger_target_exists",
		"has_existing_parent_dep", "has_existing_trigger_dep",
	}).
		AddRow("gd-fnhtkrbdiq",
			sql.NullString{String: "gd-thread1", Valid: true}, sql.NullString{},
			true, false, false, false).
		AddRow("gd-hkbgwaothb",
			sql.NullString{}, sql.NullString{String: "gd-trig1", Valid: true},
			false, true, false, false)

	mock.ExpectQuery(triggeredByPageQueryRE.String()).
		WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs(
			"gd-fnhtkrbdiq", "gd-thread1", "triggered-by",
			sqlmock.AnyArg(), "kd admin backfill-triggered-by-deps", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs(
			"gd-hkbgwaothb", "gd-trig1", "triggered-by",
			sqlmock.AnyArg(), "kd admin backfill-triggered-by-deps", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stats, err := queryBackfillTriggeredByDeps(context.Background(), db, store.BackfillTriggeredByDepsOptions{
		BatchSize: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.EdgesAdded != 2 {
		t.Errorf("EdgesAdded = %d, want 2", stats.EdgesAdded)
	}
	if stats.EdgesBySignal["slack_thread_bead_id"] != 1 || stats.EdgesBySignal["trigger_id"] != 1 {
		t.Errorf("EdgesBySignal = %v, want {slack:1, trigger:1}", stats.EdgesBySignal)
	}
}

func TestQueryBackfillTriggeredByDeps_RaceLost_CountsExistingDep(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"id", "slack_thread_bead_id", "trigger_id",
		"slack_target_exists", "trigger_target_exists",
		"has_existing_parent_dep", "has_existing_trigger_dep",
	}).
		AddRow("gd-fnhtkrbdiq",
			sql.NullString{String: "gd-thread1", Valid: true}, sql.NullString{},
			true, false, false, false)

	mock.ExpectQuery(triggeredByPageQueryRE.String()).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO deps`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // ON CONFLICT path
	mock.ExpectCommit()

	stats, err := queryBackfillTriggeredByDeps(context.Background(), db, store.BackfillTriggeredByDepsOptions{
		BatchSize: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.EdgesAdded != 0 {
		t.Errorf("EdgesAdded = %d, want 0", stats.EdgesAdded)
	}
	if stats.SkippedExistingDep != 1 {
		t.Errorf("SkippedExistingDep = %d, want 1", stats.SkippedExistingDep)
	}
}

func TestQueryBackfillTriggeredByDeps_MaxBatchesCaps(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"id", "slack_thread_bead_id", "trigger_id",
		"slack_target_exists", "trigger_target_exists",
		"has_existing_parent_dep", "has_existing_trigger_dep",
	}).
		AddRow("gd-fnhtkrbdiq",
			sql.NullString{String: "gd-thread1", Valid: true}, sql.NullString{},
			true, false, false, false).
		AddRow("gd-hkbgwaothb",
			sql.NullString{String: "gd-thread2", Valid: true}, sql.NullString{},
			true, false, false, false)
	mock.ExpectQuery(triggeredByPageQueryRE.String()).WillReturnRows(rows)

	stats, err := queryBackfillTriggeredByDeps(context.Background(), db, store.BackfillTriggeredByDepsOptions{
		DryRun:     true,
		BatchSize:  2,
		MaxBatches: 1,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !stats.BatchesCapped {
		t.Errorf("BatchesCapped = false, want true")
	}
	if stats.Batches != 1 {
		t.Errorf("Batches = %d, want 1", stats.Batches)
	}
}

func TestQueryBackfillTriggeredByDeps_ValidateRejectsNegativeSince(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryBackfillTriggeredByDeps(context.Background(), db, store.BackfillTriggeredByDepsOptions{
		Since: -time.Hour,
	})
	if err == nil {
		t.Fatal("expected error for negative --since")
	}
}

// classifyTriggeredByCandidate is a pure function — test the heuristics
// without a database. Each case names the skip reason explicitly so a
// future shift in precedence is obvious from the failing assertion.

func TestClassifyTriggeredByCandidate_OK_SlackOnly(t *testing.T) {
	ins, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id:                "gd-child",
		slackThreadBeadID: sql.NullString{String: "gd-thread", Valid: true},
		slackTargetExists: true,
	})
	if cls != classifyOK {
		t.Fatalf("classification = %v, want classifyOK", cls)
	}
	if ins.targetID != "gd-thread" || ins.signal != "slack_thread_bead_id" {
		t.Errorf("insert = %+v, want target=gd-thread signal=slack_thread_bead_id", ins)
	}
}

func TestClassifyTriggeredByCandidate_OK_TriggerOnly(t *testing.T) {
	ins, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id:                  "gd-child",
		triggerID:           sql.NullString{String: "gd-trig", Valid: true},
		triggerTargetExists: true,
	})
	if cls != classifyOK {
		t.Fatalf("classification = %v, want classifyOK", cls)
	}
	if ins.targetID != "gd-trig" || ins.signal != "trigger_id" {
		t.Errorf("insert = %+v, want target=gd-trig signal=trigger_id", ins)
	}
}

func TestClassifyTriggeredByCandidate_OK_BothAgree_PrefersSlack(t *testing.T) {
	// Both signals point at the same target — single edge, the
	// stable-ordering preference for slack_thread_bead_id wins.
	ins, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id:                  "gd-child",
		slackThreadBeadID:   sql.NullString{String: "gd-same", Valid: true},
		triggerID:           sql.NullString{String: "gd-same", Valid: true},
		slackTargetExists:   true,
		triggerTargetExists: true,
	})
	if cls != classifyOK {
		t.Fatalf("classification = %v, want classifyOK", cls)
	}
	if ins.targetID != "gd-same" || ins.signal != "slack_thread_bead_id" {
		t.Errorf("insert = %+v, want target=gd-same signal=slack_thread_bead_id", ins)
	}
}

func TestClassifyTriggeredByCandidate_Ambiguous(t *testing.T) {
	_, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id:                  "gd-child",
		slackThreadBeadID:   sql.NullString{String: "gd-alpha", Valid: true},
		triggerID:           sql.NullString{String: "gd-beta", Valid: true},
		slackTargetExists:   true,
		triggerTargetExists: true,
	})
	if cls != classifyAmbiguous {
		t.Errorf("classification = %v, want classifyAmbiguous", cls)
	}
}

func TestClassifyTriggeredByCandidate_SelfReference(t *testing.T) {
	_, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id:                "gd-self",
		slackThreadBeadID: sql.NullString{String: "gd-self", Valid: true},
		slackTargetExists: true,
	})
	if cls != classifySelfReference {
		t.Errorf("classification = %v, want classifySelfReference", cls)
	}
}

func TestClassifyTriggeredByCandidate_TargetMissing(t *testing.T) {
	_, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id:                "gd-child",
		slackThreadBeadID: sql.NullString{String: "gd-gone", Valid: true},
		slackTargetExists: false,
	})
	if cls != classifyTargetMissing {
		t.Errorf("classification = %v, want classifyTargetMissing", cls)
	}
}

func TestClassifyTriggeredByCandidate_NoSignal(t *testing.T) {
	// Page WHERE clause means this should not actually occur, but the
	// classifier defends against it.
	_, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id: "gd-child",
	})
	if cls != classifyNoSignal {
		t.Errorf("classification = %v, want classifyNoSignal", cls)
	}
}

func TestClassifyTriggeredByCandidate_SelfBeatsMissing(t *testing.T) {
	// Self-reference signal + missing-target signal — the self skip wins
	// because it surfaces a more actionable mis-tag than a vanished bead.
	_, cls := classifyTriggeredByCandidate(triggeredByCandidate{
		id:                  "gd-self",
		slackThreadBeadID:   sql.NullString{String: "gd-self", Valid: true},
		triggerID:           sql.NullString{String: "gd-gone", Valid: true},
		slackTargetExists:   true,
		triggerTargetExists: false,
	})
	if cls != classifySelfReference {
		t.Errorf("classification = %v, want classifySelfReference", cls)
	}
}
