package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// pageQueryRE matches the candidate-page SELECT used by
// fetchParentChildBackfillPage. The full SQL is too long to match
// verbatim with a regexp; instead we anchor on the distinctive LATERAL
// agent lookup and the LIMIT clause. Both the include-closed and
// open-only variants share this anchor.
var pageQueryRE = regexp.MustCompile(`SELECT b\.id,\s+b\.created_by[\s\S]+LIMIT \$3`)

func TestQueryBackfillParentChildDeps_NoCandidates(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(pageQueryRE.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "created_by", "description", "task_id", "task_bead_exists", "has_existing_dep",
		}))

	stats, err := queryBackfillParentChildDeps(context.Background(), db, store.BackfillParentChildDepsOptions{
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

func TestQueryBackfillParentChildDeps_DryRunCountsByAuthor(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"id", "created_by", "description", "task_id", "task_bead_exists", "has_existing_dep",
	}).
		// Eligible: agent A → task X.
		AddRow("gd-fnhtkrbdiq", "agent-A", "child 1", sql.NullString{String: "gd-taskX", Valid: true}, true, false).
		// Eligible: agent A → task X (second child).
		AddRow("gd-hkbgwaothb", "agent-A", "child 2", sql.NullString{String: "gd-taskX", Valid: true}, true, false).
		// Eligible: agent B → task Y.
		AddRow("gd-7yfi0rsi0u", "agent-B", "child 3", sql.NullString{String: "gd-taskY", Valid: true}, true, false).
		// Skip: has existing parent-child dep.
		AddRow("gd-5vclrivk3g", "agent-A", "", sql.NullString{String: "gd-taskX", Valid: true}, true, true).
		// Skip: description begins with parent(gd-…).
		AddRow("gd-fa6dcnnhzl", "agent-A", "parent(gd-explicit) explicit parent", sql.NullString{String: "gd-taskX", Valid: true}, true, false).
		// Skip: no agent bead matched.
		AddRow("gd-efhcawukaf", "agent-orphan", "no agent", sql.NullString{}, false, false).
		// Skip: agent has no task_id.
		AddRow("gd-f3dsu2bn2s", "agent-A", "no task", sql.NullString{String: "", Valid: true}, false, false).
		// Skip: self-parent (task_id == bead.id).
		AddRow("gd-5ckeclr4th", "agent-A", "self", sql.NullString{String: "gd-5ckeclr4th", Valid: true}, true, false).
		// Skip: task missing/soft-deleted.
		AddRow("gd-5adv7sgjem", "agent-A", "missing task", sql.NullString{String: "gd-gone", Valid: true}, false, false)

	mock.ExpectQuery(pageQueryRE.String()).
		WillReturnRows(rows)

	stats, err := queryBackfillParentChildDeps(context.Background(), db, store.BackfillParentChildDepsOptions{
		DryRun:    true,
		BatchSize: 100, // larger than the page returned, terminates the loop
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.CandidateBeads != 9 {
		t.Errorf("CandidateBeads = %d, want 9", stats.CandidateBeads)
	}
	if stats.EdgesAdded != 3 {
		t.Errorf("EdgesAdded = %d, want 3", stats.EdgesAdded)
	}
	if got := stats.EdgesByAuthor["agent-A"]; got != 2 {
		t.Errorf("EdgesByAuthor[agent-A] = %d, want 2", got)
	}
	if got := stats.EdgesByAuthor["agent-B"]; got != 1 {
		t.Errorf("EdgesByAuthor[agent-B] = %d, want 1", got)
	}
	if stats.SkippedExistingDep != 1 {
		t.Errorf("SkippedExistingDep = %d, want 1", stats.SkippedExistingDep)
	}
	if stats.SkippedParentMention != 1 {
		t.Errorf("SkippedParentMention = %d, want 1", stats.SkippedParentMention)
	}
	if stats.SkippedNoAgent != 1 {
		t.Errorf("SkippedNoAgent = %d, want 1", stats.SkippedNoAgent)
	}
	if stats.SkippedNoTaskID != 1 {
		t.Errorf("SkippedNoTaskID = %d, want 1", stats.SkippedNoTaskID)
	}
	if stats.SkippedSelfParent != 1 {
		t.Errorf("SkippedSelfParent = %d, want 1", stats.SkippedSelfParent)
	}
	if stats.SkippedTaskMissing != 1 {
		t.Errorf("SkippedTaskMissing = %d, want 1", stats.SkippedTaskMissing)
	}
	if stats.Batches != 1 {
		t.Errorf("Batches = %d, want 1", stats.Batches)
	}
}

func TestQueryBackfillParentChildDeps_LiveInsertsAndCommits(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"id", "created_by", "description", "task_id", "task_bead_exists", "has_existing_dep",
	}).
		AddRow("gd-fnhtkrbdiq", "agent-A", "child 1", sql.NullString{String: "gd-taskX", Valid: true}, true, false).
		AddRow("gd-hkbgwaothb", "agent-B", "child 2", sql.NullString{String: "gd-taskY", Valid: true}, true, false)

	mock.ExpectQuery(pageQueryRE.String()).
		WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs(
			"gd-fnhtkrbdiq", "gd-taskX", "parent-child",
			sqlmock.AnyArg(), "kd admin backfill-parent-child-deps", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO deps`).
		WithArgs(
			"gd-hkbgwaothb", "gd-taskY", "parent-child",
			sqlmock.AnyArg(), "kd admin backfill-parent-child-deps", sqlmock.AnyArg(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	stats, err := queryBackfillParentChildDeps(context.Background(), db, store.BackfillParentChildDepsOptions{
		BatchSize: 100,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.EdgesAdded != 2 {
		t.Errorf("EdgesAdded = %d, want 2", stats.EdgesAdded)
	}
	if stats.EdgesByAuthor["agent-A"] != 1 || stats.EdgesByAuthor["agent-B"] != 1 {
		t.Errorf("EdgesByAuthor = %v, want {agent-A:1, agent-B:1}", stats.EdgesByAuthor)
	}
}

func TestQueryBackfillParentChildDeps_RaceLost_CountsExistingDep(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"id", "created_by", "description", "task_id", "task_bead_exists", "has_existing_dep",
	}).
		AddRow("gd-fnhtkrbdiq", "agent-A", "child 1", sql.NullString{String: "gd-taskX", Valid: true}, true, false)

	mock.ExpectQuery(pageQueryRE.String()).WillReturnRows(rows)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO deps`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // ON CONFLICT path
	mock.ExpectCommit()

	stats, err := queryBackfillParentChildDeps(context.Background(), db, store.BackfillParentChildDepsOptions{
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

func TestQueryBackfillParentChildDeps_MaxBatchesCaps(t *testing.T) {
	db, mock := newMockDB(t)

	// Page returns 2 rows AND BatchSize=2 — looks like a full page, so
	// the loop would continue, except MaxBatches=1 caps it. Verify the
	// cap is set even with a full page.
	rows := sqlmock.NewRows([]string{
		"id", "created_by", "description", "task_id", "task_bead_exists", "has_existing_dep",
	}).
		AddRow("gd-fnhtkrbdiq", "agent-A", "child", sql.NullString{String: "gd-taskX", Valid: true}, true, false).
		AddRow("gd-hkbgwaothb", "agent-A", "child", sql.NullString{String: "gd-taskY", Valid: true}, true, false)
	mock.ExpectQuery(pageQueryRE.String()).WillReturnRows(rows)

	stats, err := queryBackfillParentChildDeps(context.Background(), db, store.BackfillParentChildDepsOptions{
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

func TestQueryBackfillParentChildDeps_ValidateRejectsNegativeSince(t *testing.T) {
	db, _ := newMockDB(t)
	_, err := queryBackfillParentChildDeps(context.Background(), db, store.BackfillParentChildDepsOptions{
		Since: -time.Hour,
	})
	if err == nil {
		t.Fatal("expected error for negative --since")
	}
}

// TestQueryBackfillParentChildDeps_ExcludesBookkeepingTypes pins the SQL-side
// exclusion of type=agent / session / message / mail candidate beads (bug
// ka-0000002199, ka-0000004462): the manual parent-child reconstruction must not
// re-parent agent-lifecycle or comms bookkeeping beads into a goal-bearing epic
// and re-introduce the exact epic-pollution the create-time skip in
// maybeAddAutoParent removes. Since sqlmock returns the rows we hand it (it
// cannot enforce a WHERE clause), we assert the emitted candidate-page SQL
// carries the full exclusion predicate — it must stay in lockstep with
// autoParentSkipTypes (internal/server/auto_parent.go).
func TestQueryBackfillParentChildDeps_ExcludesBookkeepingTypes(t *testing.T) {
	db, mock := newMockDB(t)

	// Match a candidate-page query that also carries the type exclusion. If the
	// exclusion clause were dropped (or narrowed back to agent/session only)
	// this expectation would not match and the query would fail (unexpected
	// query), reddening the test.
	excludeRE := regexp.MustCompile(`b\.type NOT IN \('agent', 'session', 'message', 'mail'\)`)
	mock.ExpectQuery(excludeRE.String()).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "created_by", "description", "task_id", "task_bead_exists", "has_existing_dep",
		}))

	if _, err := queryBackfillParentChildDeps(context.Background(), db, store.BackfillParentChildDepsOptions{
		DryRun: true,
	}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expected candidate-page SQL to carry the type-exclusion predicate: %v", err)
	}
}

func TestIsFKViolation(t *testing.T) {
	if isFKViolation(nil) {
		t.Errorf("nil err must not classify as FK violation")
	}
	if !isFKViolation(errors.New("insert violates foreign key constraint deps_depends_on_id_fkey")) {
		t.Errorf("expected FK classification on string match")
	}
	if isFKViolation(errors.New("unrelated db error")) {
		t.Errorf("unrelated error must not classify")
	}
}
