package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// ptrStrPG returns a *string pointing at s — used by the BeadFilter cases
// for the three-state MentionsType slot (nil / non-nil "" / non-nil verb).
func ptrStrPG(s string) *string { return &s }

// ptrBoolPG returns a *bool pointing at b — used by the BeadFilter cases
// for the three-state HasReport slot (nil / *true / *false).
func ptrBoolPG(b bool) *bool { return &b }

// newMockDB creates a sqlmock database with automatic cleanup and expectation checking.
func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("failed to create sqlmock: %v", err)
	}
	t.Cleanup(func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unfulfilled expectations: %v", err)
		}
		db.Close()
	})
	return db, mock
}

// beadWithTotalColumns is the column list for queryListBeads results
// (total_count + standard bead columns + JOINed operator alias/display/email +
// has_report projection + daemon-computed blocked_on_decision_bead_id from
// ka-0000001065).
var beadWithTotalColumns = []string{
	"total_count",
	"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
	"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
	"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
	"operated_by_id",
	"operated_by", "operated_by_display", "operated_by_email", "has_report",
	"blocked_on_decision_bead_id",
	// parked_on_decision_bead_id: text-mention parked projection (ka-0000003797).
	// Constant '' when neither the projection nor the filter is requested.
	"parked_on_decision_bead_id",
	// relevance_score: trailing float8 search-relevance projection
	// (ka-0000003832). Constant 0 on non-search list paths.
	"relevance_score",
}

// beadRowColumns is the column list for the JOINed scanBeadFull projection
// — the standard bead columns (including the operated_by_id FK) followed by
// the canonical operators.kind || ':' || operators.subject alias +
// operators.display + operators.email resolved via LEFT JOIN on
// operated_by_id.
var beadRowColumns = []string{
	"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
	"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
	"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
	"operated_by_id",
	"operated_by", "operated_by_display", "operated_by_email",
}

// beadRowColumnsLabels is beadRowColumns plus the trailing labels text[]
// column beadLabelsSelectSuffix adds to the single-bead reads (queryGetBead,
// queryGetBeadIncludingDeleted).
var beadRowColumnsLabels = append(slices.Clone(beadRowColumns), "labels")

// beadRowColumnsBaseLabels is beadRowColumnsBase plus the trailing labels
// column, for the UPDATE...RETURNING paths that fold labels in (close,
// restore, transfer).
var beadRowColumnsBaseLabels = append(slices.Clone(beadRowColumnsBase), "labels")

// beadRowColumnsBase is the column list for the unjoined scanBead projection
// — used by write-path RETURNING clauses (UPDATE / INSERT) that do not need
// the resolved operator chip on the response.
var beadRowColumnsBase = []string{
	"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
	"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
	"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
	"operated_by_id",
}

// addBeadWithTotalRow adds a minimal bead row with a leading total_count to a
// sqlmock.Rows. Trailing operated_by_display/email default to empty strings,
// has_report defaults to false, blocked_on_decision_bead_id and
// parked_on_decision_bead_id default to "" (unblocked / un-parked), and
// relevance_score defaults to 0 (no search).
func addBeadWithTotalRow(rows *sqlmock.Rows, total int, id, kind, typ, title, status string, priority int, now time.Time) *sqlmock.Rows {
	return rows.AddRow(
		total,
		id, nil, nil, nil, kind, typ, title, nil, nil,
		status, priority, nil, nil, now, nil, now,
		nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "", false,
		"",
		"",
		0.0,
	)
}

// emptyRelationalExpectations sets up sqlmock expectations for the two relational
// queries (deps, comments) that follow a single-bead query, returning empty
// results. Labels ride on the bead row itself (beadLabelsSelectSuffix), so
// there is no labels query to expect. The comments query is JOINed with
// operators so the column list includes the resolved display/email.
func emptyRelationalExpectations(mock sqlmock.Sqlmock, id string) {
	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id = \\$1").WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}))
	mock.ExpectQuery("SELECT .+ FROM comments .* WHERE comments\\.bead_id = \\$1").WithArgs(id).
		WillReturnRows(sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at", "operated_by", "operated_by_display", "operated_by_email", "impersonated_by"}))
}

func TestParseSortClause(t *testing.T) {
	// Sort columns are qualified with `beads.` because queryListBeads JOINs
	// operators (which has its own created_at / updated_at) — without the
	// qualification the ORDER BY clause is ambiguous on JOINed reads.
	for _, tc := range []struct {
		input string
		want  string
	}{
		// beads.id is always the final unique tiebreak so LIMIT/OFFSET pagination
		// is deterministic across pages even when the low-cardinality sort keys
		// tie (ka-0000002466).
		{"", "beads.created_at DESC, beads.id"},
		{"priority", "beads.priority ASC, beads.id"},
		{"-priority", "beads.priority DESC, beads.id"},
		{"evil_column", "beads.created_at DESC, beads.id"},
		{"-evil_column", "beads.created_at DESC, beads.id"},
		// Multi-field sort: comma-separated, evaluated left to right.
		{"priority,-created_at", "beads.priority ASC, beads.created_at DESC, beads.id"},
		{"priority, -created_at", "beads.priority ASC, beads.created_at DESC, beads.id"},
		{"-priority,-updated_at", "beads.priority DESC, beads.updated_at DESC, beads.id"},
		// Invalid columns in multi-field sort are skipped.
		{"priority,evil,-created_at", "beads.priority ASC, beads.created_at DESC, beads.id"},
		// All-invalid multi-field sort falls back to default.
		{"evil,-bogus", "beads.created_at DESC, beads.id"},
		// Empty segments tolerated.
		{"priority,,-created_at", "beads.priority ASC, beads.created_at DESC, beads.id"},
	} {
		if got := parseSortClause(tc.input, ""); got != tc.want {
			t.Errorf("parseSortClause(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	// All allowed columns.
	for _, col := range []string{"priority", "created_at", "updated_at", "title", "status", "type"} {
		want := "beads." + col + " ASC, beads.id"
		if got := parseSortClause(col, ""); got != want {
			t.Errorf("parseSortClause(%q) = %q, want %q", col, got, want)
		}
		wantDesc := "beads." + col + " DESC, beads.id"
		if got := parseSortClause("-"+col, ""); got != wantDesc {
			t.Errorf("parseSortClause(-%q) = %q, want %q", col, got, wantDesc)
		}
	}
}

// TestParseSortClauseRelevance pins the ka-0000004745 relevance-boost wiring:
// when a non-empty search-relevance expression is supplied, it must be the
// PRIMARY ORDER BY key (DESC), explicit user sort columns slot in between, and
// `beads.created_at DESC` is always the final tiebreak (unless the caller
// already sorts on created_at). When the relevance expression is empty the
// behavior is identical to the no-search path — non-search listings unchanged.
func TestParseSortClauseRelevance(t *testing.T) {
	const rel = "CASE WHEN title ILIKE $1 || '%' THEN 3 ELSE 0 END"
	for _, tc := range []struct {
		name string
		sort string
		rel  string
		want string
	}{
		{
			// No explicit sort + relevance → relevance first, created_at tiebreak.
			name: "RelevanceOnly",
			sort: "",
			rel:  rel,
			want: rel + " DESC, beads.created_at DESC, beads.id",
		},
		{
			// Explicit sort slots between relevance and the created_at tiebreak.
			name: "RelevancePlusPriority",
			sort: "-priority",
			rel:  rel,
			want: rel + " DESC, beads.priority DESC, beads.created_at DESC, beads.id",
		},
		{
			// Caller already sorts on created_at → no redundant duplicate created_at
			// key, but the unique beads.id tiebreak is still appended.
			name: "RelevancePlusCreatedAt",
			sort: "created_at",
			rel:  rel,
			want: rel + " DESC, beads.created_at ASC, beads.id",
		},
		{
			// Empty relevance is the legacy path: identical to no-search ORDER BY.
			name: "EmptyRelevanceUnchanged",
			sort: "-priority",
			rel:  "",
			want: "beads.priority DESC, beads.id",
		},
		{
			// Empty relevance + empty sort → the default created_at DESC.
			name: "EmptyRelevanceEmptySort",
			sort: "",
			rel:  "",
			want: "beads.created_at DESC, beads.id",
		},
	} {
		if got := parseSortClause(tc.sort, tc.rel); got != tc.want {
			t.Errorf("%s: parseSortClause(%q, rel) = %q, want %q", tc.name, tc.sort, got, tc.want)
		}
	}
}

func TestScanHelpers(t *testing.T) {
	// nullTimePtr
	if nullTimePtr(nil).Valid {
		t.Error("nullTimePtr(nil) should be invalid")
	}
	now := time.Now()
	if nt := nullTimePtr(&now); !nt.Valid || !nt.Time.Equal(now) {
		t.Errorf("nullTimePtr(now) = %v", nt)
	}

	// nullString
	if nullString("").Valid {
		t.Error("nullString(\"\") should be invalid")
	}
	if ns := nullString("hello"); !ns.Valid || ns.String != "hello" {
		t.Errorf("nullString(\"hello\") = %v", ns)
	}

	// jsonbBytes
	if jsonbBytes(nil) != nil {
		t.Error("jsonbBytes(nil) should be nil")
	}
	if jsonbBytes(json.RawMessage{}) != nil {
		t.Error("jsonbBytes({}) should be nil")
	}
	input := json.RawMessage(`{"key":"value"}`)
	if string(jsonbBytes(input)) != `{"key":"value"}` {
		t.Errorf("jsonbBytes = %s", jsonbBytes(input))
	}
}

func TestQueryCreateBead(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-test1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Test bead", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
	}
	mock.ExpectExec("INSERT INTO beads").
		WithArgs(
			"gd-test1", sqlmock.AnyArg(), "issue", "task", "Test bead", "", "",
			"open", 0, "", "", now, "", now,
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"default", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryCreateBead(context.Background(), db, bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryGetBead(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsLabels).AddRow(
		"gd-test1", nil, nil, nil, "issue", "task", "Test bead", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "",
		"{urgent}",
	)
	mock.ExpectQuery("SELECT .+ FROM labels l WHERE l\\.bead_id = beads\\.id.+ FROM beads .* WHERE beads\\.id = \\$1").WithArgs("gd-test1").WillReturnRows(rows)
	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id = \\$1").WithArgs("gd-test1").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}))
	mock.ExpectQuery("SELECT .+ FROM comments .* WHERE comments\\.bead_id = \\$1").WithArgs("gd-test1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at", "operated_by", "operated_by_display", "operated_by_email", "impersonated_by"}))

	bead, err := queryGetBead(context.Background(), db, "gd-test1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bead.ID != "gd-test1" || bead.Title != "Test bead" {
		t.Fatalf("got id=%q title=%q", bead.ID, bead.Title)
	}
	if len(bead.Labels) != 1 || bead.Labels[0] != "urgent" {
		t.Fatalf("expected labels=[urgent], got %v", bead.Labels)
	}
}

// TestQueryGetBead_OperatedByID is a regression test for ka-0000001989:
// the operated_by_id FK must be projected and scanned on the JOINed read
// path. Before the fix the column was never selected, so every API read
// returned operated_by_id=null even though the DB column was populated —
// and the controller's session-spend reconciler rendered operator_id="0"
// on every gardenio_operator_cost_usd_total series.
func TestQueryGetBead_OperatedByID(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsLabels).AddRow(
		"gd-op1", nil, nil, nil, "issue", "agent", "Agent bead", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		int64(7),
		"slack:U0OPERATOR1", "Matt", "matt@example.com",
		nil,
	)
	// Pin the FK projection into the emitted SQL — dropping
	// beads.operated_by_id from beadColumnsBeads must fail this test, not
	// just shift scan destinations.
	mock.ExpectQuery(`SELECT .+beads\.operated_by_id.+ FROM beads .* WHERE beads\.id = \$1`).WithArgs("gd-op1").WillReturnRows(rows)
	emptyRelationalExpectations(mock, "gd-op1")

	bead, err := queryGetBead(context.Background(), db, "gd-op1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bead.OperatedByID == nil || *bead.OperatedByID != 7 {
		t.Fatalf("operated_by_id: got %v, want 7", bead.OperatedByID)
	}
	if bead.OperatedBy != "slack:U0OPERATOR1" {
		t.Fatalf("operated_by: got %q", bead.OperatedBy)
	}
}

func TestQueryGetBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+ FROM beads .* WHERE beads\\.id = \\$1").WithArgs("nonexistent").WillReturnError(sql.ErrNoRows)

	_, err := queryGetBead(context.Background(), db, "nonexistent")
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

// TestQueryGetBead_FiltersSoftDeleted is a regression test for ka-0000007077:
// queryGetBead must include `deleted_at IS NULL` so soft-deleted rows surface
// as not-found to `kd show`, `mol show`, and other GetBead callers. Without
// this filter, `kd delete <id>` returned success but `kd show <id>` still
// returned the bead, breaking the e2e config-beads-crud module.
func TestQueryGetBead_FiltersSoftDeleted(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+ FROM beads .* WHERE beads\\.id = \\$1 AND beads\\.deleted_at IS NULL").
		WithArgs("gd-deleted").
		WillReturnError(sql.ErrNoRows)

	_, err := queryGetBead(context.Background(), db, "gd-deleted")
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows for soft-deleted bead, got %v", err)
	}
}

func TestQueryDeleteBead(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("UPDATE beads SET deleted_at = NOW().+ WHERE id = \\$1 AND deleted_at IS NULL").
		WithArgs("gd-del1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryDeleteBead(context.Background(), db, "gd-del1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryDeleteBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("UPDATE beads SET deleted_at = NOW().+ WHERE id = \\$1 AND deleted_at IS NULL").
		WithArgs("nonexistent").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM beads WHERE id = \\$1\\)").
		WithArgs("nonexistent").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(false))

	if err := queryDeleteBead(context.Background(), db, "nonexistent"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestQueryDeleteBead_AlreadyDeleted(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("UPDATE beads SET deleted_at = NOW().+ WHERE id = \\$1 AND deleted_at IS NULL").
		WithArgs("gd-del2").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT EXISTS\\(SELECT 1 FROM beads WHERE id = \\$1\\)").
		WithArgs("gd-del2").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))

	if err := queryDeleteBead(context.Background(), db, "gd-del2"); err != nil {
		t.Fatalf("expected nil (idempotent on already-deleted), got %v", err)
	}
}

func TestQueryUpdateBead(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-test1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Updated bead", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
	}
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs(
			"gd-test1", sqlmock.AnyArg(), "issue", "task", "Updated bead", "", "",
			"open", 0, "", "",
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(now))

	if err := queryUpdateBead(context.Background(), db, bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryUpdateBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	bead := &model.Bead{ID: "nonexistent", Kind: model.KindIssue, Type: model.TypeTask, Title: "Test", Status: model.StatusOpen}
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs(
			"nonexistent", sqlmock.AnyArg(), "issue", "task", "Test", "", "",
			"open", 0, "", "",
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnError(sql.ErrNoRows)

	if err := queryUpdateBead(context.Background(), db, bead); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestQueryAddDependency(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	dep := &model.Dependency{
		BeadID: "gd-a", DependsOnID: "gd-b", Type: model.DepBlocks, CreatedAt: now, CreatedBy: "alice",
	}
	mock.ExpectExec("INSERT INTO deps").
		WithArgs("gd-a", "gd-b", "blocks", now, "alice", "", sqlmock.AnyArg(), typedMentionDepCreator).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryAddDependency(context.Background(), db, dep); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryGetDependencies(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}).
		AddRow("gd-a", "gd-b", "blocks", now, nil, nil).
		AddRow("gd-a", "gd-c", "related", now, "alice", nil)
	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id = \\$1").WithArgs("gd-a").WillReturnRows(rows)

	deps, err := queryGetDependencies(context.Background(), db, "gd-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(deps) != 2 {
		t.Fatalf("expected 2 dependencies, got %d", len(deps))
	}
	if deps[0].DependsOnID != "gd-b" || deps[1].CreatedBy != "alice" {
		t.Fatalf("got deps[0].DependsOnID=%q deps[1].CreatedBy=%q", deps[0].DependsOnID, deps[1].CreatedBy)
	}
}

func TestQueryRemoveDependency(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM deps").
		WithArgs("gd-a", "gd-b", "blocks").
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := queryRemoveDependency(context.Background(), db, "gd-a", "gd-b", model.DepBlocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("rows affected = %d; want 1", n)
	}
}

// TestQueryRemoveDependency_NoMatch pins the 0-row case: deleting an edge that
// does not exist reports 0 rows affected and, critically, is NOT an error —
// idempotent callers rely on that (ka-0000001966).
func TestQueryRemoveDependency_NoMatch(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM deps").
		WithArgs("gd-a", "gd-b", "parent-child").
		WillReturnResult(sqlmock.NewResult(0, 0))

	n, err := queryRemoveDependency(context.Background(), db, "gd-a", "gd-b", model.DepParentChild)
	if err != nil {
		t.Fatalf("a 0-match must not be an error, got: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected = %d; want 0", n)
	}
}

// TestQueryRemoveDependency_RowsAffectedUnsupported covers a driver whose
// RowsAffected errors (database/sql permits that). Assume-success is the safe
// read: reporting 0 would turn a real removal into a bogus "no such dependency".
func TestQueryRemoveDependency_RowsAffectedUnsupported(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM deps").
		WithArgs("gd-a", "gd-b", "blocks").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("RowsAffected not supported")))

	n, err := queryRemoveDependency(context.Background(), db, "gd-a", "gd-b", model.DepBlocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("rows affected = %d; want 1 (assume-success when unsupported)", n)
	}
}

func TestQueryAddLabel(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("INSERT INTO labels").WithArgs("gd-a", "urgent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryAddLabel(context.Background(), db, "gd-a", "urgent"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryRemoveLabel(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM labels").WithArgs("gd-a", "urgent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := queryRemoveLabel(context.Background(), db, "gd-a", "urgent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("rows affected = %d; want 1", n)
	}
}

// TestQueryRemoveLabel_NoMatch pins the 0-row case: removing a label the bead
// never carried reports 0 rows affected and, critically, is NOT an error —
// idempotent callers rely on that (ka-0000000424).
func TestQueryRemoveLabel_NoMatch(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM labels").
		WithArgs("gd-a", "not-a-real-label").
		WillReturnResult(sqlmock.NewResult(0, 0))

	n, err := queryRemoveLabel(context.Background(), db, "gd-a", "not-a-real-label")
	if err != nil {
		t.Fatalf("a 0-match must not be an error, got: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected = %d; want 0", n)
	}
}

// TestQueryRemoveLabel_RowsAffectedUnsupported covers a driver whose
// RowsAffected errors (database/sql permits that). Assume-success is the safe
// read: reporting 0 would turn a real removal into a bogus "no such label".
func TestQueryRemoveLabel_RowsAffectedUnsupported(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM labels").
		WithArgs("gd-a", "urgent").
		WillReturnResult(sqlmock.NewErrorResult(errors.New("RowsAffected not supported")))

	n, err := queryRemoveLabel(context.Background(), db, "gd-a", "urgent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 1 {
		t.Errorf("rows affected = %d; want 1 (assume-success when unsupported)", n)
	}
}

func TestQueryGetLabels(t *testing.T) {
	db, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{"label"}).AddRow("urgent").AddRow("frontend")
	mock.ExpectQuery("SELECT label FROM labels WHERE bead_id = \\$1").WithArgs("gd-a").WillReturnRows(rows)

	labels, err := queryGetLabels(context.Background(), db, "gd-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(labels) != 2 || labels[0] != "urgent" || labels[1] != "frontend" {
		t.Fatalf("expected [urgent, frontend], got %v", labels)
	}
}

func TestQueryGetTaintsForBead(t *testing.T) {
	db, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{"taint_id"}).AddRow(int64(1)).AddRow(int64(2))
	mock.ExpectQuery("SELECT bt.taint_id FROM bead_taints bt").WithArgs("gd-agent1").WillReturnRows(rows)

	taints, err := queryGetTaintsForBead(context.Background(), db, "gd-agent1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(taints) != 2 || taints[0] != 1 || taints[1] != 2 {
		t.Fatalf("expected [1, 2], got %v", taints)
	}
}

func TestQueryGetTaintsForBead_NoTaints(t *testing.T) {
	db, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{"taint_id"})
	mock.ExpectQuery("SELECT bt.taint_id FROM bead_taints bt").WithArgs("gd-clean").WillReturnRows(rows)

	taints, err := queryGetTaintsForBead(context.Background(), db, "gd-clean")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(taints) != 0 {
		t.Fatalf("expected no taints, got %v", taints)
	}
}

func TestQueryGetAllTaintsForBead(t *testing.T) {
	db, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{"taint_id"}).AddRow(int64(1)).AddRow(int64(2))
	mock.ExpectQuery("SELECT taint_id FROM bead_taints WHERE bead_id = \\$1").WithArgs("gd-agent1").WillReturnRows(rows)

	taints, err := queryGetAllTaintsForBead(context.Background(), db, "gd-agent1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(taints) != 2 || taints[0] != 1 || taints[1] != 2 {
		t.Fatalf("expected [1, 2], got %v", taints)
	}
}

// The batch form reads every taint, gate or not, for all ids in one query
// with a single array parameter.
func TestQueryGetAllTaintsForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{"bead_id", "taint_id"}).
		AddRow("gd-a", int64(1)).AddRow("gd-a", int64(2)).AddRow("gd-b", int64(2))
	mock.ExpectQuery("SELECT bead_id, taint_id FROM bead_taints\\s+WHERE bead_id = ANY\\(\\$1::text\\[\\]\\)").
		WithArgs(sqlmock.AnyArg()).WillReturnRows(rows)

	got, err := queryGetAllTaintsForBeads(context.Background(), db, []string{"gd-a", "gd-b", "gd-c"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || len(got["gd-a"]) != 2 || len(got["gd-b"]) != 1 || got["gd-b"][0] != 2 {
		t.Fatalf("got %v; want gd-a:[1 2] gd-b:[2]", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryGetAllTaintsForBeadsEmpty(t *testing.T) {
	got, err := queryGetAllTaintsForBeads(context.Background(), nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v; want empty map, nil", got, err)
	}
}

func TestQueryRecordEvent(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	event := &model.Event{
		Topic: "beads.bead.created", BeadID: "gd-a", Actor: "alice",
		Payload: json.RawMessage(`{"bead":{"id":"gd-a"}}`),
	}
	mock.ExpectQuery("INSERT INTO events").
		WithArgs("beads.bead.created", "gd-a", "alice", []byte(`{"bead":{"id":"gd-a"}}`), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(1, now))

	if err := queryRecordEvent(context.Background(), db, event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if event.ID != 1 {
		t.Fatalf("expected id=1, got %d", event.ID)
	}
}

func TestQueryGetEvents(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(1, "beads.bead.created", "gd-a", "alice", []byte(`{}`), now, "matt@example.com").
		AddRow(2, "beads.bead.updated", "gd-a", nil, []byte(`{}`), now, nil)
	mock.ExpectQuery(`SELECT .+ FROM events .+WHERE events\.bead_id = \$1`).WithArgs("gd-a").WillReturnRows(rows)

	evts, err := queryGetEvents(context.Background(), db, "gd-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evts))
	}
	if evts[0].Actor != "alice" || evts[1].Actor != "" {
		t.Fatalf("got actors=%q %q", evts[0].Actor, evts[1].Actor)
	}
	if evts[0].OperatedBy != "matt@example.com" {
		t.Fatalf("got operated_by=%q, want %q", evts[0].OperatedBy, "matt@example.com")
	}
	if evts[1].OperatedBy != "" {
		t.Fatalf("got operated_by=%q, want empty", evts[1].OperatedBy)
	}
}

func TestQueryGetRecentEvents(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(2, "beads.bead.updated", "gd-a", "bob", []byte(`{}`), now.Add(time.Minute), "").
		AddRow(3, "beads.label.added", "gd-a", "carol", []byte(`{}`), now.Add(2*time.Minute), "")
	mock.ExpectQuery(`SELECT .+ FROM \(\s*SELECT .+ FROM events .+WHERE events\.bead_id = \$1\s+ORDER BY events\.id DESC\s+LIMIT \$2\s*\) AS recent\s+ORDER BY created_at ASC`).
		WithArgs("gd-a", 2).WillReturnRows(rows)

	evts, err := queryGetRecentEvents(context.Background(), db, "gd-a", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 2 {
		t.Fatalf("expected 2 events, got %d", len(evts))
	}
	if evts[0].ID != 2 || evts[1].ID != 3 {
		t.Fatalf("expected ids 2,3, got %d,%d", evts[0].ID, evts[1].ID)
	}
}

// TestQueryGetEventsByTopic pins the ?topic= store read (gd-cP9tODid8j): the
// same newest-N-then-ASC shape as queryGetRecentEvents, with the topic bound
// as $2.
func TestQueryGetEventsByTopic(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(4, "beads.label.removed", "gd-a", "bob", []byte(`{"label":"x"}`), now, "")
	mock.ExpectQuery(`SELECT .+ FROM \(\s*SELECT .+ FROM events .+WHERE events\.bead_id = \$1 AND events\.topic = \$2\s+ORDER BY events\.id DESC\s+LIMIT \$3\s*\) AS recent\s+ORDER BY created_at ASC`).
		WithArgs("gd-a", "beads.label.removed", 1).WillReturnRows(rows)

	evts, err := queryGetEventsByTopic(context.Background(), db, "gd-a", "beads.label.removed", 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(evts) != 1 || evts[0].ID != 4 || evts[0].Topic != "beads.label.removed" {
		t.Fatalf("got %+v, want the one label.removed event", evts)
	}
}

// TestQueryGetEventsByTopic_NoLimit pins limit <= 0 binding a NULL LIMIT
// (LIMIT ALL in Postgres) rather than LIMIT 0.
func TestQueryGetEventsByTopic_NoLimit(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`FROM events .+WHERE events\.bead_id = \$1 AND events\.topic = \$2`).
		WithArgs("gd-a", "beads.label.added", nil).
		WillReturnRows(sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}))

	if _, err := queryGetEventsByTopic(context.Background(), db, "gd-a", "beads.label.added", 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQuerySetConfig(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	config := &model.Config{Key: "view:inbox", Value: json.RawMessage(`{"filter":{"status":["open"]}}`)}
	mock.ExpectQuery("INSERT INTO configs").
		WithArgs("view:inbox", []byte(`{"filter":{"status":["open"]}}`)).
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "updated_at"}).AddRow(now, now))

	if err := querySetConfig(context.Background(), db, config); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.CreatedAt.IsZero() {
		t.Fatal("expected created_at to be set")
	}
}

func TestQueryGetConfig(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT .+ FROM configs WHERE key = \\$1").WithArgs("view:inbox").
		WillReturnRows(sqlmock.NewRows([]string{"key", "value", "created_at", "updated_at"}).
			AddRow("view:inbox", []byte(`{}`), now, now))

	config, err := queryGetConfig(context.Background(), db, "view:inbox")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if config.Key != "view:inbox" {
		t.Fatalf("got key=%q", config.Key)
	}
}

func TestQueryGetConfig_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+ FROM configs WHERE key = \\$1").WithArgs("nonexistent").
		WillReturnError(sql.ErrNoRows)

	if _, err := queryGetConfig(context.Background(), db, "nonexistent"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestQueryListConfigs(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT .+ FROM configs WHERE key LIKE").WithArgs("view").
		WillReturnRows(sqlmock.NewRows([]string{"key", "value", "created_at", "updated_at"}).
			AddRow("view:inbox", []byte(`{}`), now, now).
			AddRow("view:my-tasks", []byte(`{}`), now, now))

	configs, err := queryListConfigs(context.Background(), db, "view")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
}

func TestQueryListAllConfigs(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery("SELECT .+ FROM configs ORDER BY key").
		WillReturnRows(sqlmock.NewRows([]string{"key", "value", "created_at", "updated_at"}).
			AddRow("context:prime", []byte(`{}`), now, now).
			AddRow("view:inbox", []byte(`{}`), now, now))

	configs, err := queryListAllConfigs(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(configs) != 2 {
		t.Fatalf("expected 2 configs, got %d", len(configs))
	}
	if configs[0].Key != "context:prime" || configs[1].Key != "view:inbox" {
		t.Fatalf("unexpected keys: %q, %q", configs[0].Key, configs[1].Key)
	}
}

func TestQueryDeleteConfig(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM configs WHERE key = \\$1").WithArgs("view:inbox").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryDeleteConfig(context.Background(), db, "view:inbox"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryDeleteConfig_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM configs WHERE key = \\$1").WithArgs("nonexistent").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if err := queryDeleteConfig(context.Background(), db, "nonexistent"); err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestQueryAddComment(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	comment := &model.Comment{BeadID: "gd-a", Author: "alice", Text: "Hello world"}
	mock.ExpectQuery("INSERT INTO comments").
		WithArgs("gd-a", "alice", "Hello world", sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), now))

	if err := queryAddComment(context.Background(), db, comment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if comment.ID != 1 || comment.CreatedAt.IsZero() {
		t.Fatalf("got id=%d created_at=%v", comment.ID, comment.CreatedAt)
	}
}

func TestQueryGetComments(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at", "operated_by", "operated_by_display", "operated_by_email", "impersonated_by"}).
		AddRow(int64(1), "gd-a", "alice", "First", now, "matt@example.com", "", "", "").
		AddRow(int64(2), "gd-a", nil, "Second", now, nil, "", "", "")
	mock.ExpectQuery(`SELECT .+ FROM comments .+WHERE comments\.bead_id = \$1`).WithArgs("gd-a").WillReturnRows(rows)

	comments, err := queryGetComments(context.Background(), db, "gd-a")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments, got %d", len(comments))
	}
	if comments[0].Author != "alice" || comments[1].Author != "" {
		t.Fatalf("got authors=%q %q", comments[0].Author, comments[1].Author)
	}
	if comments[0].OperatedBy != "matt@example.com" {
		t.Fatalf("got operated_by=%q, want %q", comments[0].OperatedBy, "matt@example.com")
	}
	if comments[1].OperatedBy != "" {
		t.Fatalf("got operated_by=%q, want empty", comments[1].OperatedBy)
	}
}

func TestQueryDeleteComment(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`DELETE FROM comments\s+WHERE id = \$1 AND bead_id = \$2`).
		WithArgs(int64(7), "gd-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at"}).
			AddRow(int64(7), "gd-a", "alice", "old comment", now))

	got, err := queryDeleteComment(context.Background(), db, "gd-a", 7)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil || got.ID != 7 || got.Author != "alice" {
		t.Fatalf("unexpected returned comment: %+v", got)
	}
}

func TestQueryDeleteComment_NoRowsReturnsNilNil(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`DELETE FROM comments\s+WHERE id = \$1 AND bead_id = \$2`).
		WithArgs(int64(99), "gd-a").
		WillReturnError(sql.ErrNoRows)

	got, err := queryDeleteComment(context.Background(), db, "gd-a", 99)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for no-rows, got %+v", got)
	}
}

func TestQueryTrimComments_NoFilter(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`WITH keep AS \(`).
		WithArgs("gd-a", 2, "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at"}).
			AddRow(int64(1), "gd-a", "scheduler", "old1", now.Add(-3*time.Hour)).
			AddRow(int64(2), "gd-a", "alice", "old2", now.Add(-2*time.Hour)))

	out, err := queryTrimComments(context.Background(), db, "gd-a", 2, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 deleted rows, got %d", len(out))
	}
	if out[0].ID != 1 || out[1].ID != 2 {
		t.Fatalf("expected ascending order by created_at, got %d, %d", out[0].ID, out[1].ID)
	}
}

func TestQueryTrimComments_AuthorFilter(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	mock.ExpectQuery(`WITH keep AS \(`).
		WithArgs("gd-a", 1, "scheduler").
		WillReturnRows(sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at"}).
			AddRow(int64(5), "gd-a", "scheduler", "old-sched", now.Add(-1*time.Hour)))

	out, err := queryTrimComments(context.Background(), db, "gd-a", 1, "scheduler")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(out) != 1 || out[0].Author != "scheduler" {
		t.Fatalf("unexpected output: %+v", out)
	}
}

func TestQueryTrimComments_NegativeKeepLast(t *testing.T) {
	db, _ := newMockDB(t)
	if _, err := queryTrimComments(context.Background(), db, "gd-a", -1, ""); err == nil {
		t.Fatalf("expected error for negative keepLast")
	}
}

func TestQueryListBeads(t *testing.T) {
	now := time.Now().UTC()
	pri := func(v int) *int { return &v }

	for _, tc := range []struct {
		name      string
		filter    model.BeadFilter
		queryPat  string
		args      []driver.Value
		wantCount int
		wantTotal int
	}{
		{
			name:      "NoFilter",
			filter:    model.BeadFilter{},
			queryPat:  "SELECT 0 AS total_count, .+ FROM beads .+WHERE deleted_at IS NULL ORDER BY beads\\.created_at DESC",
			wantCount: 2,
			wantTotal: 2,
		},
		{
			name:      "FilterByStatus",
			filter:    model.BeadFilter{Status: []model.Status{model.StatusOpen, model.StatusDeferred}},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1, \\$2\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"open", "deferred"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByType",
			filter:    model.BeadFilter{Type: []model.BeadType{model.TypeBug}},
			queryPat:  "SELECT .+ FROM beads .+WHERE type IN \\(\\$1\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"bug"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByExcludeClosedTypes (the land window, gd-FC7tIACC5f):
			// the closed beads of these types go, their open ones stay.
			name:      "FilterByExcludeClosedTypes",
			filter:    model.BeadFilter{Type: []model.BeadType{model.TypeTask, model.TypeEscalation}, ExcludeClosedTypes: []model.BeadType{model.TypeEscalation, model.TypeDecision}},
			queryPat:  "SELECT .+ FROM beads .+WHERE type IN \\(\\$1, \\$2\\) AND NOT \\(status = 'closed' AND type IN \\(\\$3, \\$4\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"task", "escalation", "escalation", "decision"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:     "FilterByKind",
			filter:   model.BeadFilter{Kind: []model.Kind{model.KindData}},
			queryPat: "SELECT .+ FROM beads .+WHERE beads\\.kind IN \\(\\$1\\) AND deleted_at IS NULL ORDER BY",
			args:     []driver.Value{"data"},
		},
		{
			name:      "FilterByPriority",
			filter:    model.BeadFilter{Priority: pri(3)},
			queryPat:  "SELECT .+ FROM beads .+WHERE priority = \\$1 AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{3},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByAssignee",
			filter:    model.BeadFilter{Assignee: "alice"},
			queryPat:  "SELECT .+ FROM beads .+WHERE assignee = ANY\\(\\$1::text\\[\\]\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// Unassigned emits its own NULL-safe clause rather than joining the
			// assignee = ANY() set: mergeNonEmpty strips the empty string, which
			// is exactly why `?assignee=` could never express "nobody on it"
			// (ka-0000007209). Both spellings of absent must be covered — the
			// column is `assignee TEXT DEFAULT ''` so unclaimed beads carry '',
			// but NULL is legal and an `= ''` alone would silently drop them.
			name:      "FilterByUnassigned",
			filter:    model.BeadFilter{Unassigned: true},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(assignee IS NULL OR assignee = ''\\) AND deleted_at IS NULL ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByCreatedBy",
			filter:    model.BeadFilter{CreatedBy: "alice"},
			queryPat:  "SELECT .+ FROM beads .+WHERE created_by = ANY\\(\\$1::text\\[\\]\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByLabels",
			filter:    model.BeadFilter{Labels: []string{"urgent"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM labels .+\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"urgent"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByExcludeLabels",
			filter:    model.BeadFilter{ExcludeLabels: []string{"gardener-delivery-skip"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE NOT EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label = ANY\\(\\$1::text\\[\\]\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// ExclusiveProject (ka-0000003411) narrows to beads that carry NO
			// project:<other> label — a NOT EXISTS over labels beginning
			// `project:` and differing from the named project's own label. The
			// bound arg is the full `project:<name>` label. Pushing this into the
			// WHERE clause lets LIMIT account for project narrowing so gb
			// ready/news no longer over-fetch.
			name:      "FilterByExclusiveProject",
			filter:    model.BeadFilter{ExclusiveProject: "gardenio"},
			queryPat:  "SELECT .+ FROM beads .+WHERE NOT EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label LIKE 'project:%' AND labels\\.label <> \\$1\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"project:gardenio"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// Titles is an exact-title match (role beads are keyed by title):
			// one ANY() over the set, no LIKE / full-text.
			name:      "FilterByTitles",
			filter:    model.BeadFilter{Titles: []string{"reviewer", "crew"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE beads\\.title = ANY\\(\\$1::text\\[\\]\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// LabelsOrNoProject (gd-sDjQZ4kgW9): a per-agent token listing
			// advice sees its project's beads OR beads with no project label
			// (Global / role-only advice). `project:_%` needs a non-empty name.
			name:      "FilterByLabelsOrNoProject",
			filter:    model.BeadFilter{LabelsOrNoProject: []string{"project:gardenio"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label = ANY\\(\\$1::text\\[\\]\\)\\) OR NOT EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label LIKE 'project:_%'\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// LabelsAny (OIDC project-scope path) ORs all values into a SINGLE
			// EXISTS … label = ANY() clause — contrast FilterByLabels, which
			// emits one EXISTS per label (AND). ka-0000003307.
			name:      "FilterByLabelsAny",
			filter:    model.BeadFilter{LabelsAny: []string{"release-stuck", "preflight-broken"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label = ANY\\(\\$1::text\\[\\]\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// LabelsAnyUser (the user-facing --label-any) emits its OWN
			// EXISTS … label = ANY() clause, structurally identical to LabelsAny
			// but on a separate filter slice so it ANDs with the OIDC-scoping
			// LabelsAny rather than merging into it. ka-0000003307.
			name:      "FilterByLabelsAnyUser",
			filter:    model.BeadFilter{LabelsAnyUser: []string{"release-stuck", "preflight-broken", "release-pipeline-broken"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label = ANY\\(\\$1::text\\[\\]\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// When BOTH the OIDC LabelsAny and the user LabelsAnyUser are set,
			// they produce TWO separate ANY() clauses ANDed together ($1 then
			// $2) — the user any-of narrows WITHIN project scope, never widens
			// it. This is the core ka-0000003307 invariant.
			name: "FilterByLabelsAnyAndUserAreSeparateClauses",
			filter: model.BeadFilter{
				LabelsAny:     []string{"project:gardenio"},
				LabelsAnyUser: []string{"release-stuck", "preflight-broken"},
			},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label = ANY\\(\\$1::text\\[\\]\\)\\) AND EXISTS \\(SELECT 1 FROM labels WHERE labels\\.bead_id = beads\\.id AND labels\\.label = ANY\\(\\$2::text\\[\\]\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg(), sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// ka-0000004745 (Option 1) + ka-0000003832 (Option 2) + ka-0000004282
			// (Option 3): a single-term search binds the term ($1) and the
			// full-query arg ($2). The per-term ILIKE group is OR'd with the FTS
			// predicate (beads.search_tsv @@ websearch_to_tsquery('english', $2)) —
			// the recall FLOOR plus the GIN-backed FTS branch. ORDER BY is the
			// composite score: the Option-1 title-tier CASE (cast to float8) PLUS
			// ts_rank_cd(search_tsv, …, 32) PLUS the Option-3 openness/priority/type
			// WITHIN-TIER boost CASE, threaded as the PRIMARY key ahead of the
			// created_at tiebreak. Args are unchanged — the FTS predicate and
			// ts_rank reuse the same $2 bind, and the boost CASE binds no args (it
			// reads only the status/type/priority columns).
			name:      "FilterBySearch",
			filter:    model.BeadFilter{Search: "login"},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(\\(\\(title ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$1 \\|\\| '%'\\)\\)\\)\\) OR beads\\.search_tsv @@ websearch_to_tsquery\\('english', \\$2\\)\\) AND deleted_at IS NULL ORDER BY \\(CASE WHEN title ILIKE \\$2 \\|\\| '%' THEN 3 WHEN title ILIKE '%' \\|\\| \\$2 \\|\\| '%' THEN 2 WHEN title ILIKE '%' \\|\\| \\$1 \\|\\| '%' THEN 1 ELSE 0 END\\)::float8 \\+ COALESCE\\(ts_rank_cd\\(beads\\.search_tsv, websearch_to_tsquery\\('english', \\$2\\), 32\\), 0\\) \\+ \\(CASE WHEN type IN \\('message','agent'\\) THEN -0.5 WHEN status IN \\('open','in_progress'\\) AND type IN \\('escalation','decision'\\) THEN 0.5 WHEN status IN \\('open','in_progress'\\) AND priority <= 0 THEN 0.3 WHEN status IN \\('open','in_progress'\\) THEN 0.1 ELSE 0 END\\) DESC, beads\\.created_at DESC",
			args:      []driver.Value{"login", "login"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// ka-0000004745 + ka-0000003832 + ka-0000004282 (Option 3): a multi-word
			// search tokenizes on whitespace and ANDs one (title/description ILIKE
			// %term%) clause PER term inside the ILIKE group, which is then OR'd with
			// the FTS predicate. The per-term placeholders ($1,$2) bind each token;
			// $3 is the full joined query reused by BOTH the FTS predicate and the
			// ts_rank score. The "every term in the title" tier reuses $1 AND $2.
			// The Option-3 openness/priority/type boost CASE trails the ts_rank
			// component and binds no args. Regression guard for symptom A of
			// ka-0000000507 plus the Option-2 FTS OR-clause plus the Option-3 boost.
			name:      "FilterBySearchMultiTerm",
			filter:    model.BeadFilter{Search: "litellm ProxyException"},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(\\(\\(title ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$1 \\|\\| '%'\\)\\)\\) AND \\(title ILIKE '%' \\|\\| \\$2 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$2 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$2 \\|\\| '%'\\)\\)\\)\\) OR beads\\.search_tsv @@ websearch_to_tsquery\\('english', \\$3\\)\\) AND deleted_at IS NULL ORDER BY \\(CASE WHEN title ILIKE \\$3 \\|\\| '%' THEN 3 WHEN title ILIKE '%' \\|\\| \\$3 \\|\\| '%' THEN 2 WHEN title ILIKE '%' \\|\\| \\$1 \\|\\| '%' AND title ILIKE '%' \\|\\| \\$2 \\|\\| '%' THEN 1 ELSE 0 END\\)::float8 \\+ COALESCE\\(ts_rank_cd\\(beads\\.search_tsv, websearch_to_tsquery\\('english', \\$3\\), 32\\), 0\\) \\+ \\(CASE WHEN type IN \\('message','agent'\\) THEN -0.5 WHEN status IN \\('open','in_progress'\\) AND type IN \\('escalation','decision'\\) THEN 0.5 WHEN status IN \\('open','in_progress'\\) AND priority <= 0 THEN 0.3 WHEN status IN \\('open','in_progress'\\) THEN 0.1 ELSE 0 END\\) DESC, beads\\.created_at DESC",
			args:      []driver.Value{"litellm", "ProxyException", "litellm ProxyException"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// Multi-term search composes with pagination: the search predicate
			// (ILIKE group OR FTS) precedes deleted_at, and the full-query arg ($3)
			// lands BEFORE the LIMIT/OFFSET placeholders ($4,$5) — locking in that
			// the score bind never collides with pagination binds. The tier CASE
			// and the Option-3 boost CASE are both wildcarded (.+) here since this
			// test guards bind ORDERING, not the exact score expression; the
			// full-fidelity ORDER BY is pinned by FilterBySearch/FilterBySearchMultiTerm.
			name:      "FilterBySearchMultiTermPagination",
			filter:    model.BeadFilter{Search: "litellm ProxyException", Limit: 10, Offset: 5},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(\\(\\(title ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$1 \\|\\| '%'\\)\\)\\) AND \\(title ILIKE '%' \\|\\| \\$2 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$2 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$2 \\|\\| '%'\\)\\)\\)\\) OR beads\\.search_tsv @@ websearch_to_tsquery\\('english', \\$3\\)\\) AND deleted_at IS NULL ORDER BY \\(CASE .+ END\\)::float8 \\+ COALESCE\\(ts_rank_cd\\(.+\\), 0\\) \\+ \\(CASE .+ END\\) DESC, beads\\.created_at DESC, beads\\.id LIMIT \\$4 OFFSET \\$5",
			args:      []driver.Value{"litellm", "ProxyException", "litellm ProxyException", 10, 5},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// ka-0000002772: a hyphenated query term is bound with its
			// separators expanded to the '%' ILIKE wildcard ("break-glass" ->
			// "break%glass"), so the SAME per-term ILIKE / FTS SQL SHAPE still
			// applies — the transform is VALUE-only, the term stays a bound
			// param ($1) — but the BOUND VALUE now matches "break glass",
			// "breakglass" and "break-glass" alike. The FTS full-query bind ($2)
			// stays RAW (websearch_to_tsquery gets the original "break-glass").
			// The title-tier CASE reuses the SAME expanded $1 for the "term in
			// title" tier and the raw $2 for the exact/substring tiers.
			name:     "FilterBySearchHyphenExpanded",
			filter:   model.BeadFilter{Search: "break-glass"},
			queryPat: "SELECT .+ FROM beads .+WHERE \\(\\(\\(title ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$1 \\|\\| '%'\\)\\)\\)\\) OR beads\\.search_tsv @@ websearch_to_tsquery\\('english', \\$2\\)\\) AND deleted_at IS NULL ORDER BY \\(CASE WHEN title ILIKE \\$2 \\|\\| '%' THEN 3 WHEN title ILIKE '%' \\|\\| \\$2 \\|\\| '%' THEN 2 WHEN title ILIKE '%' \\|\\| \\$1 \\|\\| '%' THEN 1 ELSE 0 END\\)::float8 \\+ COALESCE\\(ts_rank_cd\\(beads\\.search_tsv, websearch_to_tsquery\\('english', \\$2\\), 32\\), 0\\)",
			// $1 is the expanded per-term value; $2 is the raw full query.
			args:      []driver.Value{"break%glass", "break-glass"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// Underscore and slash separators expand identically, and a
			// multi-term hyphenated query expands EACH token independently while
			// the full-query FTS bind ($3) stays raw. Guards that '_' is treated
			// as a SEPARATOR (-> '%'), NOT escaped to a literal single-char
			// wildcard.
			name:      "FilterBySearchUnderscoreSlashExpanded",
			filter:    model.BeadFilter{Search: "cluster_admin api/v1"},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(\\(\\(title ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$1 \\|\\| '%'\\)\\)\\) AND \\(title ILIKE '%' \\|\\| \\$2 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$2 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$2 \\|\\| '%'\\)\\)\\)\\) OR beads\\.search_tsv @@ websearch_to_tsquery\\('english', \\$3\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"cluster%admin", "api%v1", "cluster_admin api/v1"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// A literal ILIKE metacharacter ('%') the user typed is ESCAPED in
			// the bound value ("50%" -> "50\%") so it matches a literal percent
			// sign instead of acting as a wildcard, while the raw full-query
			// bind ($2) is untouched. Locks in the escape-before-substitute
			// contract for the one metachar that is NOT also a separator.
			name:      "FilterBySearchLiteralPercentEscaped",
			filter:    model.BeadFilter{Search: "50%"},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(\\(\\(title ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR description ILIKE '%' \\|\\| \\$1 \\|\\| '%' OR beads\\.id = ANY\\(ARRAY\\(SELECT bead_id FROM comments WHERE comments\\.text ILIKE '%' \\|\\| \\$1 \\|\\| '%'\\)\\)\\)\\) OR beads\\.search_tsv @@ websearch_to_tsquery\\('english', \\$2\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"50\\%", "50%"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// Short page (1 < Limit): total is derived as offset+rows, no count query.
			name:      "WithLimitAndOffset",
			filter:    model.BeadFilter{Limit: 10, Offset: 5},
			queryPat:  "SELECT .+ FROM beads .+WHERE deleted_at IS NULL ORDER BY .+ LIMIT \\$1 OFFSET \\$2",
			args:      []driver.Value{10, 5},
			wantCount: 1,
			wantTotal: 6,
		},
		{
			name:     "WithSort",
			filter:   model.BeadFilter{Sort: "-priority"},
			queryPat: "SELECT .+ FROM beads .+WHERE deleted_at IS NULL ORDER BY beads\\.priority DESC",
		},
		{
			name:      "FilterByField",
			filter:    model.BeadFilter{Fields: map[string]string{"sprint": "3"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE fields \\? \\$1 AND fields->>\\$1 = \\$2 AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"sprint", "3"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// The presence sentinel MUST emit the leading `fields ? key` guard
			// so the planner can reach the migration-025/031/034 partial
			// expression indexes; a bare `fields->>key IS NOT NULL` cannot imply
			// the index predicate and Seq Scans the whole table (ka-0000001686).
			name:      "FilterByFieldPresence",
			filter:    model.BeadFilter{Fields: map[string]string{"run_id": "*"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(fields \\? \\$1 AND fields->>\\$1 != ''\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"run_id"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByFieldAbsence",
			filter:    model.BeadFilter{Fields: map[string]string{"run_id": ""}},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(fields->>\\$1 IS NULL OR fields->>\\$1 = ''\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"run_id"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "CombinedFilters",
			filter:    model.BeadFilter{Status: []model.Status{model.StatusOpen}, Assignee: "bob", Limit: 5},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1\\) AND assignee = ANY\\(\\$2::text\\[\\]\\) AND deleted_at IS NULL ORDER BY .+ LIMIT \\$3",
			args:      []driver.Value{"open", sqlmock.AnyArg(), 5},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterNoOpenDeps must restrict the blocker EXISTS clause to genuinely
			// blocking dep types (blocks, blocked-by, depends-on). Structural deps
			// (parent-of, child-of, parent-child) must NOT keep a bead out of the
			// ready queue — child tasks under an open epic are still ready to claim.
			// Regression guard for ka-0000000173.
			name:      "FilterNoOpenDeps",
			filter:    model.BeadFilter{NoOpenDeps: true},
			queryPat:  "SELECT .+ FROM beads .+WHERE NOT EXISTS .+deps.+d\\.type IN \\('blocks', 'blocked-by', 'depends-on'\\).+dep\\.status IN.+ AND deleted_at IS NULL ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterNoParent (ka-0000005267): the orphan predicate must negate
			// the CHILD-first parent-child edge (deps.bead_id=child,
			// deps.depends_on_id=parent). A bead with a parent-child parent is
			// excluded; a bead with only a blocks/related dep still matches
			// (unlike --no-blockers this is agnostic to dep STATUS — a child of a
			// closed epic is still parented). Lets the triage sweep ask for
			// parent-less open work in one query instead of N+1 `kd dep list`.
			//
			// ka-0000008107: the type predicate is an IN-list over BOTH the
			// canonical 'parent-child' AND the legacy 'parent' alias, so children
			// of pre-canonicalization epics (typed 'parent') are NOT mis-reported
			// as orphans. The explicit alias order is pinned by
			// TestListBeadsParentChildAliasClauses below.
			name:      "FilterNoParent",
			filter:    model.BeadFilter{NoParent: true},
			queryPat:  "SELECT .+ FROM beads .+WHERE NOT EXISTS \\(SELECT 1 FROM deps d WHERE d\\.bead_id = beads\\.id AND d\\.type IN \\('parent-child', 'parent'\\)\\) AND deleted_at IS NULL ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterShortTitleMissing: the short-title groomer's queue (plan
			// 2026-10-14-short-titles D5). NULL and '' both count as unset.
			name:      "FilterShortTitleMissing",
			filter:    model.BeadFilter{ShortTitleMissing: true},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(beads\\.short_title IS NULL OR beads\\.short_title = ''\\) AND deleted_at IS NULL ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterHeraldry: the heraldry= list filter matches the run, or a
			// value still in the shield form whose places read as it.
			name:      "FilterHeraldry",
			filter:    model.BeadFilter{Heraldry: "🔥🧭📦"},
			queryPat:  "SELECT .+ FROM beads .+WHERE \\(beads\\.heraldry = \\$1 OR \\(beads\\.heraldry LIKE '% %' AND replace\\(replace\\(substr\\(beads\\.heraldry, strpos\\(beads\\.heraldry, ' '\\) \\+ 1\\), ' ', ''\\), '\\.', ''\\) = \\$1\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"🔥🧭📦"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterHeraldryRegister: the daemon's register read, keyed on
			// beads.project (migration 057), not the project label.
			name:      "FilterHeraldryRegister",
			filter:    model.BeadFilter{HeraldryRegister: "gardenio"},
			queryPat:  "SELECT .+ FROM beads .+WHERE beads\\.heraldry IS NOT NULL AND beads\\.project = \\$1 AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"gardenio"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterNoParentWithStatus locks in composition: the orphan predicate
			// AND-merges with an explicit status filter rather than replacing it.
			// This is the exact triage-intake shape (open + parent-less).
			name: "FilterNoParentWithStatus",
			filter: model.BeadFilter{
				Status:   []model.Status{model.StatusOpen},
				NoParent: true,
			},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1\\) AND NOT EXISTS \\(SELECT 1 FROM deps d WHERE d\\.bead_id = beads\\.id AND d\\.type IN \\('parent-child', 'parent'\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"open"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByParentID (kd list --parent <epic>): the direct-child
			// predicate matches deps CHILD-first (bead_id=child →
			// depends_on_id=parent). ka-0000008107 widened its type match to the
			// parent-child alias IN-list so descendants of a legacy 'parent' epic
			// are enumerated too. $1 binds the parent id.
			name:      "FilterByParentID",
			filter:    model.BeadFilter{ParentID: "gd-epic01"},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM deps WHERE deps\\.bead_id = beads\\.id AND deps\\.depends_on_id = \\$1 AND deps\\.type IN \\('parent-child', 'parent'\\)\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"gd-epic01"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByParentSubtreeID (kd list --parent transitive / gardener
			// subtree preflight): the recursive CTE walks CHILD→PARENT and
			// ka-0000008107 matches the alias IN-list at BOTH the seed and the
			// recursive join so a mixed 'parent'/'parent-child' subtree is walked
			// whole. $1 binds the root id (reused via %[1]s in both CTE arms).
			name:      "FilterByParentSubtreeID",
			filter:    model.BeadFilter{ParentSubtreeID: "gd-epic01"},
			queryPat:  "SELECT .+ FROM beads .+WHERE beads\\.id IN \\(\\s*WITH RECURSIVE subtree\\(id\\) AS \\(\\s*SELECT bead_id FROM deps WHERE depends_on_id = \\$1 AND type IN \\('parent-child', 'parent'\\)\\s*UNION\\s*SELECT d\\.bead_id FROM deps d JOIN subtree s ON d\\.depends_on_id = s\\.id AND d\\.type IN \\('parent-child', 'parent'\\)\\s*\\)\\s*SELECT id FROM subtree WHERE id <> \\$1\\s*\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"gd-epic01"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByParentSubtreeIDs (the land window, gd-rXMNcFrzDp): the
			// same walk seeded from every root in one ANY() array, and no
			// root exclusion, so a plan nested in another stays in its closure.
			name:      "FilterByParentSubtreeIDs",
			filter:    model.BeadFilter{ParentSubtreeIDs: []string{"gd-plan01", "gd-plan02"}},
			queryPat:  "SELECT .+ FROM beads .+WHERE beads\\.id IN \\(\\s*WITH RECURSIVE subtree\\(id\\) AS \\(\\s*SELECT bead_id FROM deps WHERE depends_on_id = ANY\\(\\$1::text\\[\\]\\) AND type IN \\('parent-child', 'parent'\\)\\s*UNION\\s*SELECT d\\.bead_id FROM deps d JOIN subtree s ON d\\.depends_on_id = s\\.id AND d\\.type IN \\('parent-child', 'parent'\\)\\s*\\)\\s*SELECT id FROM subtree\\s*\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{sqlmock.AnyArg()},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByFleet",
			filter:    model.BeadFilter{Fleet: "production"},
			queryPat:  "SELECT .+ FROM beads .+WHERE fleet = \\$1 AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"production"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "FilterByFleetAndStatus",
			filter:    model.BeadFilter{Fleet: "staging", Status: []model.Status{model.StatusOpen}},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1\\) AND fleet = \\$2 AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"open", "staging"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// Cross-fleet types (project, account) bypass fleet scoping even when
			// the caller is fleet-scoped — fleet-bound service tokens (e.g.
			// bridges) still need to resolve system-level config beads. See
			// model.AllTypesCrossFleet. Regression guard for the 2026-05-24
			// outage where MR !2902's bridge fleet-tokens hid project beads.
			name:      "FilterByFleetButTypeProjectIsCrossFleet",
			filter:    model.BeadFilter{Fleet: "slack-bridge", Type: []model.BeadType{model.TypeProject}},
			queryPat:  "SELECT .+ FROM beads .+WHERE type IN \\(\\$1\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"project"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// Mixed type queries that include a non-cross-fleet type keep the
			// fleet clause so we don't accidentally leak per-fleet beads of
			// task/etc to a fleet-scoped caller asking for "tasks + projects".
			name:      "FilterByFleetMixedTypesKeepsScoping",
			filter:    model.BeadFilter{Fleet: "slack-bridge", Type: []model.BeadType{model.TypeProject, model.TypeTask}},
			queryPat:  "SELECT .+ FROM beads .+WHERE type IN \\(\\$1, \\$2\\) AND fleet = \\$3 AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"project", "task", "slack-bridge"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "IncludeDeleted",
			filter:    model.BeadFilter{IncludeDeleted: true},
			queryPat:  "SELECT .+ FROM beads .+ORDER BY beads\\.created_at DESC",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			name:      "OnlyDeleted",
			filter:    model.BeadFilter{OnlyDeleted: true},
			queryPat:  "SELECT .+ FROM beads .+WHERE deleted_at IS NOT NULL ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// ExcludeScheduled hides beads carrying a schedule_id JSONB
			// field (auto-spawned by the cron scheduler each tick). The
			// predicate matches both unset and empty values so beads where
			// the field was cleared still surface. Regression guard for
			// ka-0000006652. ka-0000007340 added two further predicates that
			// drop per-run cron/schedule *firing* beads which carry no
			// schedule_id. ka-0000007651 generalized the multi predicate from
			// multi_entry_kind='cron' to multi_fire_key presence so the
			// event-driven firing variant (multi_entry_kind='bead_event') is
			// dropped too.
			name:      "ExcludeScheduled",
			filter:    model.BeadFilter{ExcludeScheduled: true},
			queryPat:  "SELECT .+ FROM beads .+WHERE deleted_at IS NULL AND \\(fields->>'schedule_id' IS NULL OR fields->>'schedule_id' = ''\\) AND \\(fields->>'multi_fire_key' IS NULL OR fields->>'multi_fire_key' = ''\\) AND \\(fields->>'schedule_fire_key' IS NULL OR fields->>'schedule_fire_key' = ''\\) ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// ExcludeScheduledWithStatus locks in composition: the
			// schedule_id + firing-bead predicates AND-merge with explicit
			// status/type filters rather than replacing them.
			name: "ExcludeScheduledWithStatus",
			filter: model.BeadFilter{
				Status:           []model.Status{model.StatusOpen},
				ExcludeScheduled: true,
			},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1\\) AND deleted_at IS NULL AND \\(fields->>'schedule_id' IS NULL OR fields->>'schedule_id' = ''\\) AND \\(fields->>'multi_fire_key' IS NULL OR fields->>'multi_fire_key' = ''\\) AND \\(fields->>'schedule_fire_key' IS NULL OR fields->>'schedule_fire_key' = ''\\) ORDER BY",
			args:      []driver.Value{"open"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByParentSubtree restricts to descendants under a parent
			// bead via a recursive CTE on parent-child deps. Used by
			// scheduler bead_query conditions with a `parent` field.
			// See gardenio/plans/epic-mention-looping-gardener.md §T2.
			name:      "FilterByParentSubtree",
			filter:    model.BeadFilter{ParentSubtreeID: "gd-Epic1"},
			queryPat:  "SELECT .+ FROM beads .+WHERE beads\\.id IN \\(\\s*WITH RECURSIVE subtree\\(id\\) AS \\(\\s*SELECT bead_id FROM deps WHERE depends_on_id = \\$1 AND type IN \\('parent-child', 'parent'\\)\\s+UNION\\s+SELECT d\\.bead_id FROM deps d JOIN subtree s ON d\\.depends_on_id = s\\.id AND d\\.type IN \\('parent-child', 'parent'\\)\\s*\\)\\s*SELECT id FROM subtree WHERE id <> \\$1\\s*\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"gd-Epic1"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByParentSubtreeWithType locks in composition with type/status
			// — the gardener "open tasks under epic" bead_query path. The CTE
			// AND-merges with type/status IN(...) clauses.
			name: "FilterByParentSubtreeWithType",
			filter: model.BeadFilter{
				ParentSubtreeID: "gd-Epic1",
				Type:            []model.BeadType{model.TypeTask},
				Status:          []model.Status{model.StatusOpen},
			},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1\\) AND type IN \\(\\$2\\) AND beads\\.id IN \\(\\s*WITH RECURSIVE subtree.+depends_on_id = \\$3 AND type IN \\('parent-child', 'parent'\\).+\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"open", "task", "gd-Epic1"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByParentSubtreeDormancyGate is the exact query shape the
			// meta-gardener scorer must use to count an epic's open children —
			// ParentSubtreeID + status IN (open, in_progress, blocked). It is
			// the REVERSE child query (children point AT the epic via the CTE's
			// `depends_on_id = $N`), so an epic that carries an UPWARD
			// parent-child edge to a grandparent no longer 0-outs the count
			// (ka-0000000208). The end-to-end row-behavior proof lives in
			// queries_parent_subtree_integration_test.go; this case pins the
			// SQL so the dormancy-gate query can never silently regress.
			name: "FilterByParentSubtreeDormancyGate",
			filter: model.BeadFilter{
				ParentSubtreeID: "gd-Epic1",
				Status: []model.Status{
					model.StatusOpen, model.StatusInProgress, model.StatusBlocked,
				},
			},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1, \\$2, \\$3\\) AND beads\\.id IN \\(\\s*WITH RECURSIVE subtree.+SELECT bead_id FROM deps WHERE depends_on_id = \\$4 AND type IN \\('parent-child', 'parent'\\).+SELECT id FROM subtree WHERE id <> \\$4\\s*\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"open", "in_progress", "blocked", "gd-Epic1"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByMentions joins through bead_references with an EXISTS
			// subquery so it composes via AND with every other filter and so
			// pagination still applies to the outer beads result set.
			name:      "FilterByMentions",
			filter:    model.BeadFilter{Mentions: "ka-0000004973"},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM bead_references WHERE bead_references\\.source_bead_id = beads\\.id AND bead_references\\.target_bead_id = \\$1\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"ka-0000004973"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByMentionsAndStatus exercises composition with other
			// structured filters — the EXISTS clause should AND-merge with
			// status IN(...) and not replace it.
			name: "FilterByMentionsAndStatus",
			filter: model.BeadFilter{
				Mentions: "ka-0000004973",
				Status:   []model.Status{model.StatusOpen},
			},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1\\) AND EXISTS \\(SELECT 1 FROM bead_references WHERE bead_references\\.source_bead_id = beads\\.id AND bead_references\\.target_bead_id = \\$2\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"open", "ka-0000004973"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByMentionsWithPagination locks in that --mentions composes
			// with limit/offset rather than truncating early — the EXISTS
			// clause appears before the pagination placeholders.
			name:      "FilterByMentionsWithPagination",
			filter:    model.BeadFilter{Mentions: "ka-0000004973", Limit: 10, Offset: 5},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM bead_references .+ AND bead_references\\.target_bead_id = \\$1\\) AND deleted_at IS NULL ORDER BY .+ LIMIT \\$2 OFFSET \\$3",
			args:      []driver.Value{"ka-0000004973", 10, 5},
			wantCount: 1,
			wantTotal: 6,
		},
		{
			// FilterByMentionsTypeVerb narrows --mentions by relationship_type.
			// The EXISTS clause must include `AND bead_references.relationship_type = $N`
			// with the verb as a bound parameter.
			name:      "FilterByMentionsTypeVerb",
			filter:    model.BeadFilter{Mentions: "ka-0000004973", MentionsType: ptrStrPG("blocks")},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM bead_references WHERE bead_references\\.source_bead_id = beads\\.id AND bead_references\\.target_bead_id = \\$1 AND bead_references\\.relationship_type = \\$2\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"ka-0000004973", "blocks"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// FilterByMentionsTypeUntyped — non-nil "" means "untyped only";
			// rendered as IS NULL with no extra bound argument.
			name:      "FilterByMentionsTypeUntyped",
			filter:    model.BeadFilter{Mentions: "ka-0000004973", MentionsType: ptrStrPG("")},
			queryPat:  "SELECT .+ FROM beads .+WHERE EXISTS \\(SELECT 1 FROM bead_references WHERE bead_references\\.source_bead_id = beads\\.id AND bead_references\\.target_bead_id = \\$1 AND bead_references\\.relationship_type IS NULL\\) AND deleted_at IS NULL ORDER BY",
			args:      []driver.Value{"ka-0000004973"},
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// HasReportTrue (R1, ka-0000007923): *true emits the EXISTS
			// predicate over deps + beads child join — no bound parameters
			// (predicate uses literal type list and parent-child dep type).
			// AND-merges after the default deleted_at IS NULL clause.
			name:      "HasReportTrue",
			filter:    model.BeadFilter{HasReport: ptrBoolPG(true)},
			queryPat:  "SELECT .+ FROM beads .+WHERE deleted_at IS NULL AND EXISTS \\(\\s*SELECT 1 FROM deps d JOIN beads c ON c\\.id = d\\.bead_id\\s+WHERE d\\.depends_on_id = beads\\.id\\s+AND d\\.type = 'parent-child'\\s+AND c\\.type IN \\('report', 'rich-report', 'bundle'\\)\\s+AND c\\.deleted_at IS NULL\\s*\\) ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// HasReportFalse — *false negates with NOT EXISTS so beads
			// without a report child surface and beads with one are
			// excluded.
			name:      "HasReportFalse",
			filter:    model.BeadFilter{HasReport: ptrBoolPG(false)},
			queryPat:  "SELECT .+ FROM beads .+WHERE deleted_at IS NULL AND NOT EXISTS \\(\\s*SELECT 1 FROM deps d JOIN beads c ON c\\.id = d\\.bead_id\\s+WHERE d\\.depends_on_id = beads\\.id\\s+AND d\\.type = 'parent-child'\\s+AND c\\.type IN \\('report', 'rich-report', 'bundle'\\)\\s+AND c\\.deleted_at IS NULL\\s*\\) ORDER BY",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// HasReportNilNoFilter — nil pointer means "no has_report
			// filter"; the resulting WHERE clause must NOT contain any
			// reference to has_report, the report type list, or the deps
			// EXISTS predicate.
			name:      "HasReportNilNoFilter",
			filter:    model.BeadFilter{},
			queryPat:  "SELECT .+ FROM beads .+WHERE deleted_at IS NULL ORDER BY beads\\.created_at DESC",
			wantCount: 1,
			wantTotal: 1,
		},
		{
			// HasReportComposesWithStatus — locks in that HasReport AND-merges
			// with status filters, surfacing both predicates in the WHERE.
			name: "HasReportComposesWithStatus",
			filter: model.BeadFilter{
				Status:    []model.Status{model.StatusOpen},
				HasReport: ptrBoolPG(true),
			},
			queryPat:  "SELECT .+ FROM beads .+WHERE status IN \\(\\$1\\) AND deleted_at IS NULL AND EXISTS \\(\\s*SELECT 1 FROM deps d JOIN beads c ON c\\.id = d\\.bead_id\\s+WHERE d\\.depends_on_id = beads\\.id\\s+AND d\\.type = 'parent-child'\\s+AND c\\.type IN \\('report', 'rich-report', 'bundle'\\)\\s+AND c\\.deleted_at IS NULL\\s*\\) ORDER BY",
			args:      []driver.Value{"open"},
			wantCount: 1,
			wantTotal: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newMockDB(t)
			eq := mock.ExpectQuery(tc.queryPat)
			if len(tc.args) > 0 {
				eq.WithArgs(tc.args...)
			}
			r := sqlmock.NewRows(beadWithTotalColumns)
			for i := range tc.wantCount {
				addBeadWithTotalRow(r, tc.wantTotal, fmt.Sprintf("gd-%d", i+1), "issue", "task", "T", "open", 0, now)
			}
			eq.WillReturnRows(r)

			// queryListBeads fetches labels in a second query for non-empty
			// results via queryGetLabelsForBeads (bead_id = ANY($1::text[])).
			if tc.wantCount > 0 {
				mock.ExpectQuery("SELECT bead_id, label FROM labels").
					WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
			}

			beads, total, err := queryListBeads(context.Background(), db, tc.filter)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(beads) != tc.wantCount {
				t.Fatalf("expected %d beads, got %d", tc.wantCount, len(beads))
			}
			if total != tc.wantTotal {
				t.Fatalf("expected total=%d, got %d", tc.wantTotal, total)
			}
		})
	}
}

// TestExpandSearchTerm pins the separator-expansion / metacharacter-escape
// contract for the value bound into each per-term ILIKE pattern (ka-0000002772).
// Separators '-', '_' and '/' become the '%' multi-char wildcard so a
// hyphenated query term matches spaced / joined / underscored spellings alike;
// the true ILIKE metacharacters '%' and '\' are escaped so a literal one the
// user typed matches literally rather than acting as a wildcard.
func TestExpandSearchTerm(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "login", "login"},
		{"hyphen", "break-glass", "break%glass"},
		{"underscore", "cluster_admin", "cluster%admin"},
		{"slash", "api/v1", "api%v1"},
		{"mixed-separators", "a-b_c/d", "a%b%c%d"},
		{"literal-percent-escaped", "50%", "50\\%"},
		{"literal-backslash-escaped", "a\\b", "a\\\\b"},
		{"leading-and-trailing-separators", "-x-", "%x%"},
		{"empty", "", ""},
		// '_' is treated as a SEPARATOR, so a literal "a_b" still matches an
		// "a_b" bead (the '%' spans the underscore) while ALSO matching "a-b" /
		// "a b" / "ab" — recall is a strict superset.
		{"underscore-not-escaped", "a_b", "a%b"},
		// '%' next to a separator: the separator becomes '%' and the literal
		// percent is escaped, never merged into one metacharacter.
		{"percent-adjacent-separator", "x-%", "x%\\%"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandSearchTerm(tc.in); got != tc.want {
				t.Fatalf("expandSearchTerm(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestQueryListBeadsNoOpenDepsExcludesStructuralDeps is a regression guard for
// ka-0000000173: `gb ready` / `kd list --no-blockers` previously treated every
// non-closed dependency as a blocker, hiding child tasks of open epics that were
// linked via parent-of / child-of / parent-child structural edges. The fix
// limits the blocker EXISTS clause to dep types {blocks, blocked-by, depends-on},
// so structural types (parent-of / child-of / parent-child / related / etc.)
// no longer keep a bead out of the ready queue.
//
// sqlmock's regex matcher only allows the call if the executed SQL matches —
// asserting the dep-type allow-list is present is therefore enough to lock in
// the fix. A bead with a parent-of edge to an open epic still surfaces because
// parent-of is excluded from the allow-list by construction.
func TestQueryListBeadsNoOpenDepsExcludesStructuralDeps(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`d\.type IN \('blocks', 'blocked-by', 'depends-on'\)`).
		WillReturnRows(addBeadWithTotalRow(
			sqlmock.NewRows(beadWithTotalColumns),
			1, "gd-child", "issue", "task", "child of open epic", "open", 0, now,
		))
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{NoOpenDeps: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(beads) != 1 || total != 1 {
		t.Fatalf("expected 1 bead with total=1, got %d/%d", len(beads), total)
	}
	if beads[0].ID != "gd-child" {
		t.Fatalf("expected gd-child, got %q", beads[0].ID)
	}
}

// TestQueryListBeadsNoOpenDepsCountsBlockedStatus pins the fix for
// ka-0000006285: a dep in 'blocked' status is not terminal-for-progression, so
// it must keep its dependent OUT of the ready queue. The ready-queue blocker
// EXISTS clause previously listed only ('open', 'in_progress', 'deferred'),
// disagreeing with the blocked-by badge (queryGetBlockedByForBeads) and the
// formula step dispatcher, both of which treat 'blocked' as still-blocking.
// The dep-status IN list now includes 'blocked'; sqlmock's regex matcher only
// admits the call if the executed SQL contains the whole list, so matching on
// it locks the alignment in.
func TestQueryListBeadsNoOpenDepsCountsBlockedStatus(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery(`dep\.status IN \('open', 'in_progress', 'blocked', 'deferred'\)`).
		WillReturnRows(addBeadWithTotalRow(
			sqlmock.NewRows(beadWithTotalColumns),
			1, "gd-ready", "issue", "task", "no blocked upstream", "open", 0, now,
		))
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{NoOpenDeps: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(beads) != 1 || total != 1 {
		t.Fatalf("expected 1 bead with total=1, got %d/%d", len(beads), total)
	}
}

// TestBlockingDepTypesAllowList pins the allow-list of dep types that gate
// the no-blockers / gb-ready filter. This is the explicit, named-types form
// of the regression guard for ka-0000000173 and ka-0000004488 — if someone
// adds a new structural relationship to blockingDepTypes, this test fails
// loudly.
//
// Rationale: a parent-child link from a child bug to its open epic is a
// hierarchy edge, not a wait edge. Treating it as blocking causes gardener
// auto-filed E2E regression bugs (parented under their tracking epic) to
// silently disappear from `gb ready`, breaking the auto-bug-filing → triage
// → delivery pipeline.
func TestBlockingDepTypesAllowList(t *testing.T) {
	wantBlocking := map[string]bool{
		"blocks":     true,
		"blocked-by": true,
		"depends-on": true,
	}
	wantStructural := []string{
		"parent-child",
		"parent-of",
		"child-of",
		"related",
		"duplicates",
		"supersedes",
		"assigned",
		"session_of",
		"promoted",
	}

	got := make(map[string]bool, len(blockingDepTypes))
	for _, t := range blockingDepTypes {
		got[t] = true
	}

	if len(got) != len(wantBlocking) {
		t.Fatalf("blockingDepTypes size: got %d %v, want %d %v",
			len(got), blockingDepTypes, len(wantBlocking), wantBlocking)
	}
	for typ := range wantBlocking {
		if !got[typ] {
			t.Errorf("blockingDepTypes missing %q (must gate readiness)", typ)
		}
	}
	for _, typ := range wantStructural {
		if got[typ] {
			t.Errorf("blockingDepTypes contains structural type %q (must NOT gate readiness)", typ)
		}
	}
}

// TestQueryListBeadsNoOpenDepsSQLContainsAllBlockingTypes is a regression
// guard that the SQL builder for FilterNoOpenDeps actually emits every type
// from blockingDepTypes — so a future maintainer who adds a new blocking
// type to the allow-list cannot forget to wire it into the SQL string.
func TestQueryListBeadsNoOpenDepsSQLContainsAllBlockingTypes(t *testing.T) {
	db, mock := newMockDB(t)

	// Build the regex from blockingDepTypes so the test stays in sync if the
	// allow-list grows. Order matches the SQL builder's strings.Join.
	var pat strings.Builder
	pat.WriteString(`d\.type IN \(`)
	for i, typ := range blockingDepTypes {
		if i > 0 {
			pat.WriteString(`, `)
		}
		pat.WriteString(`'` + typ + `'`)
	}
	pat.WriteString(`\)`)

	// Return zero rows so the labels follow-up query is not triggered.
	mock.ExpectQuery(pat.String()).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{NoOpenDeps: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestParentChildTypeInList pins the SQL IN-list literal shared by every
// parent-child read filter (ka-0000008107). It MUST contain the canonical
// "parent-child" first followed by the legacy "parent" alias — the pinned
// queryPat cases in TestQueryListBeads depend on this exact order.
func TestParentChildTypeInList(t *testing.T) {
	if got, want := parentChildTypeInList(), "'parent-child', 'parent'"; got != want {
		t.Errorf("parentChildTypeInList() = %q, want %q", got, want)
	}
	// The helper must render exactly the model source-of-truth set — if a new
	// alias is added to model.ParentChildDepTypes it has to appear here too.
	for _, typ := range model.ParentChildDepTypes {
		if !strings.Contains(parentChildTypeInList(), "'"+typ+"'") {
			t.Errorf("parentChildTypeInList() missing alias %q — %q", typ, parentChildTypeInList())
		}
	}
}

// TestQueryListBeadsParentChildAliasClauses is the core ka-0000008107
// regression guard: every parent-child read filter (NoParent, ParentID,
// ParentSubtreeID) must emit an IN-list that matches BOTH the canonical
// "parent-child" AND the legacy "parent" alias. Before the fix these clauses
// matched only 'parent-child', so children of legacy epics (typed 'parent')
// were mis-reported as orphans by --no-parent and dropped from --parent /
// subtree walks. The regexes are built from model.ParentChildDepTypes so a
// future maintainer who adds an alias cannot forget to wire it into the SQL.
func TestQueryListBeadsParentChildAliasClauses(t *testing.T) {
	// aliasInList reproduces the IN-list the builder emits, as a regex.
	var aliasInList strings.Builder
	aliasInList.WriteString(`IN \(`)
	for i, typ := range model.ParentChildDepTypes {
		if i > 0 {
			aliasInList.WriteString(`, `)
		}
		aliasInList.WriteString(`'` + typ + `'`)
	}
	aliasInList.WriteString(`\)`)
	inList := aliasInList.String()

	for _, tc := range []struct {
		name   string
		filter model.BeadFilter
		// pat is a fragment that must appear in the emitted SQL, with the
		// alias IN-list spliced in.
		pat string
	}{
		{
			name:   "NoParent negates the alias set",
			filter: model.BeadFilter{NoParent: true},
			pat:    `NOT EXISTS \(SELECT 1 FROM deps d WHERE d\.bead_id = beads\.id AND d\.type ` + inList + `\)`,
		},
		{
			name:   "ParentID matches the alias set",
			filter: model.BeadFilter{ParentID: "gd-epic01"},
			pat:    `deps\.type ` + inList,
		},
		{
			name:   "ParentSubtreeID seed matches the alias set",
			filter: model.BeadFilter{ParentSubtreeID: "gd-epic01"},
			pat:    `WHERE depends_on_id = \$1 AND type ` + inList,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock := newMockDB(t)
			mock.ExpectQuery(tc.pat).
				WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))
			if _, _, err := queryListBeads(context.Background(), db, tc.filter); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// addBeadWithTotalRowBlocked is like addBeadWithTotalRow but lets the caller
// stamp a non-empty blocked_on_decision_bead_id on the row — used by the
// ka-0000001065 / R2 tests to assert the scan round-trip. parked_on_decision_
// bead_id defaults to "" (un-parked); use addBeadWithTotalRowParked for the
// parked round-trip (ka-0000003797).
func addBeadWithTotalRowBlocked(rows *sqlmock.Rows, total int, id, kind, typ, title, status string, priority int, now time.Time, blockedDecisionID string) *sqlmock.Rows {
	return rows.AddRow(
		total,
		id, nil, nil, nil, kind, typ, title, nil, nil,
		status, priority, nil, nil, now, nil, now,
		nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "", false,
		blockedDecisionID,
		"",
		0.0,
	)
}

// addBeadWithTotalRowParked is like addBeadWithTotalRow but stamps a non-empty
// parked_on_decision_bead_id (text-mention parked projection, ka-0000003797) so
// the scan round-trip for ParkedOnDecision / ParkedOnDecisionBeadID can be
// asserted. blocked_on_decision_bead_id defaults to "" (un-blocked).
func addBeadWithTotalRowParked(rows *sqlmock.Rows, total int, id, kind, typ, title, status string, priority int, now time.Time, parkedDecisionID string) *sqlmock.Rows {
	return rows.AddRow(
		total,
		id, nil, nil, nil, kind, typ, title, nil, nil,
		status, priority, nil, nil, now, nil, now,
		nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "", false,
		"",
		parkedDecisionID,
		0.0,
	)
}

// TestQueryListBeadsBlockedOnDecisionPredicateShape pins the four predicate
// invariants from ka-0000001065 / R2
// (gardenio/docs/research/sequencer-report-decision-visibility.md) into the
// emitted SQL. sqlmock's regex matcher is a Go regexp executed against the
// rendered SQL string — asserting these fragments are present is enough to
// lock in the four edge cases:
//
//  1. Closed decision clears: the decision-side EXISTS constrains
//     `d.status = 'open'`, so a closed decision (status = 'closed') is
//     filtered out and the projection collapses to ”.
//  2. Agent crashed before clearing: the agent path matches on
//     `ag.fields->>'decision_bead_id' = d.id` AND requires d.status = 'open'.
//     A "stuck" decision_bead_id pointing at a now-closed decision therefore
//     does NOT flag — the closed-decision branch was already filtered.
//  3. Orphaned agent: the agent path's `dpa.depends_on_id IN <scope>`
//     subquery requires the agent to be in this bead's parent-child subtree,
//     so an agent unparented (or parented under another bead) does not flag.
//  4. Unrelated decision does not flag: the decision path's
//     `dp.depends_on_id IN <scope>` subquery requires the decision to have a
//     parent-child edge into this bead's subtree.
//
// All four edge cases are enforced by the SQL the builder emits — the row
// data returned by Postgres is just the COALESCEd MIN(d.id) (or ” when no
// decision matches).
func TestQueryListBeadsBlockedOnDecisionPredicateShape(t *testing.T) {
	db, mock := newMockDB(t)

	// One pattern matching all four invariants in any order. sqlmock uses
	// regexp.Compile, so each (?s)…(?=…) lookahead lets us assert presence
	// without dictating order. Go's regexp does not support lookahead, so
	// instead we match a relaxed pattern and verify substrings via a second
	// pass below.
	mock.ExpectQuery(`SELECT 0 AS total_count.+blocked_on_decision_bead_id`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Re-render the SELECT-projection fragment from the same constants the
	// query builder uses and assert each invariant fragment is present.
	want := []string{
		// (1) closed decision clears
		`d.status = 'open'`,
		// (2) agent crashed: stuck decision_bead_id -> only flags when d is open
		`ag.fields->>'decision_bead_id'`,
		// (3) orphaned agent: agent parent-child must land in subtree scope
		`dpa.depends_on_id IN`,
		// (4) unrelated decision: decision parent-child must land in subtree scope
		`dp.depends_on_id IN`,
		// stable lex-first tie-break when multiple decisions match
		`MIN(d.id)`,
		// COALESCE so the projection is a NOT NULL string
		`COALESCE(`,
	}
	sql := blockedOnDecisionSelectExpr
	for _, frag := range want {
		if !strings.Contains(sql, frag) {
			t.Errorf("SELECT expression missing %q\n--- expr:\n%s", frag, sql)
		}
	}
}

// TestQueryListBeadsBlockedOnDecisionScanTrue asserts the per-row scan
// surfaces BlockedOnDecision=true and BlockedOnDecisionBeadID when Postgres
// returns a non-empty gd-id from the blocked_on_decision_bead_id projection.
func TestQueryListBeadsBlockedOnDecisionScanTrue(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery("blocked_on_decision_bead_id").
		WillReturnRows(addBeadWithTotalRowBlocked(
			sqlmock.NewRows(beadWithTotalColumns),
			1, "gd-task1", "issue", "task", "blocked task", "open", 0, now,
			"gd-decisionA",
		))
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 || len(beads) != 1 {
		t.Fatalf("expected 1 bead, got total=%d len=%d", total, len(beads))
	}
	if !beads[0].BlockedOnDecision {
		t.Errorf("expected BlockedOnDecision=true, got false")
	}
	if beads[0].BlockedOnDecisionBeadID != "gd-decisionA" {
		t.Errorf("BlockedOnDecisionBeadID: got %q want gd-decisionA", beads[0].BlockedOnDecisionBeadID)
	}
}

// TestQueryListBeadsBlockedOnDecisionScanFalse asserts the per-row scan
// surfaces BlockedOnDecision=false when Postgres returns the empty-string
// fallback from the COALESCE wrapper. This covers all four edge cases at the
// scan level — closed decision, agent crashed pointing at closed decision,
// orphaned agent, and unrelated decision all collapse to ” in the SQL.
func TestQueryListBeadsBlockedOnDecisionScanFalse(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery("blocked_on_decision_bead_id").
		WillReturnRows(addBeadWithTotalRowBlocked(
			sqlmock.NewRows(beadWithTotalColumns),
			1, "gd-task2", "issue", "task", "unblocked task", "open", 0, now,
			"",
		))
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 || len(beads) != 1 {
		t.Fatalf("expected 1 bead, got total=%d len=%d", total, len(beads))
	}
	if beads[0].BlockedOnDecision {
		t.Errorf("expected BlockedOnDecision=false, got true")
	}
	if beads[0].BlockedOnDecisionBeadID != "" {
		t.Errorf("BlockedOnDecisionBeadID: got %q want empty", beads[0].BlockedOnDecisionBeadID)
	}
}

// TestQueryListBeadsBlockedOnDecisionFilterTrue asserts the WHERE clause
// applies when filter.BlockedOnDecision = &true: the predicate becomes
// `(<expr>) <> ”` and only blocked rows are returned.
func TestQueryListBeadsBlockedOnDecisionFilterTrue(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`<> ''`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	tr := true
	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{BlockedOnDecision: &tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsBlockedOnDecisionFilterFalse asserts the WHERE clause
// applies when filter.BlockedOnDecision = &false: the predicate becomes
// `(<expr>) = ”` and only unblocked rows are returned.
func TestQueryListBeadsBlockedOnDecisionFilterFalse(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`= ''`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	fa := false
	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{BlockedOnDecision: &fa})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsProjectionsDefaultOff pins the perf-critical default:
// with neither WithHasReport / WithBlockedOnDecision nor the matching filter
// set, the SELECT must use cheap constant placeholders for has_report and
// blocked_on_decision_bead_id rather than running their subqueries. This is
// the contract that keeps slack-bridge / scheduler / gb-ready callers off
// the per-row MIN(d.id) decision-walk that previously melted /v1/beads.
func TestQueryListBeadsProjectionsDefaultOff(t *testing.T) {
	db, mock := newMockDB(t)

	// Constant placeholders, in the order scanBeadWithTotalFull expects. The
	// trailing 0::float8 relevance_score projection is the no-search constant
	// (ka-0000003832).
	mock.ExpectQuery(`FALSE AS has_report, '' AS blocked_on_decision_bead_id, '' AS parked_on_decision_bead_id, 0::float8 AS relevance_score FROM beads`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	if _, _, err := queryListBeads(context.Background(), db, model.BeadFilter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsHasReportProjectionOptIn pins that WithHasReport=true
// (filter unset) opts the EXISTS predicate back into the projection without
// adding it as a WHERE clause — beads-viewer's badge fetch needs the column
// for every row but does not want to filter.
func TestQueryListBeadsHasReportProjectionOptIn(t *testing.T) {
	db, mock := newMockDB(t)

	// Predicate fragment present in projection, NOT in a WHERE clause. The
	// trailing relevance_score constant projection (ka-0000003832) follows
	// blocked_on_decision_bead_id before FROM.
	mock.ExpectQuery(`(?s)EXISTS \(\s*SELECT 1 FROM deps d JOIN beads c.+blocked_on_decision_bead_id, '' AS parked_on_decision_bead_id, 0::float8 AS relevance_score FROM beads .+WHERE deleted_at IS NULL ORDER BY`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{WithHasReport: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsBlockedOnDecisionProjectionOptIn pins that
// WithBlockedOnDecision=true (filter unset) opts the MIN(d.id) subquery back
// into the projection without adding it as a WHERE clause.
func TestQueryListBeadsBlockedOnDecisionProjectionOptIn(t *testing.T) {
	db, mock := newMockDB(t)

	// Subquery fragment present in projection, NOT in a WHERE clause. The
	// trailing relevance_score constant projection (ka-0000003832) follows
	// blocked_on_decision_bead_id before FROM.
	mock.ExpectQuery(`(?s)MIN\(d\.id\) FROM beads d.+AS blocked_on_decision_bead_id, '' AS parked_on_decision_bead_id, 0::float8 AS relevance_score FROM beads .+WHERE deleted_at IS NULL ORDER BY`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{WithBlockedOnDecision: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsParkedOnDecisionPredicateShape pins the invariants of the
// text-mention parked projection (ka-0000003797) into the emitted SQL: it
// scopes to OPEN decisions, probes bead_references d->this-bead, tie-breaks on
// MIN(d.id), and COALESCEs to ” so the trailing column is a NOT NULL string.
func TestQueryListBeadsParkedOnDecisionPredicateShape(t *testing.T) {
	want := []string{
		// scoped to open, non-deleted decisions
		`d.type = 'decision'`,
		`d.status = 'open'`,
		`d.deleted_at IS NULL`,
		// text-mention join: a bead_references edge from the decision to this bead
		`FROM bead_references br`,
		`br.source_bead_id = d.id`,
		`br.target_bead_id = beads.id`,
		// stable lex-first tie-break
		`MIN(d.id)`,
		// COALESCE so the projection is a NOT NULL string
		`COALESCE(`,
	}
	expr := parkedOnDecisionSelectExpr
	for _, frag := range want {
		if !strings.Contains(expr, frag) {
			t.Errorf("parked SELECT expression missing %q\n--- expr:\n%s", frag, expr)
		}
	}
}

// TestQueryListBeadsParkedOnDecisionScanTrue asserts the per-row scan surfaces
// ParkedOnDecision=true + ParkedOnDecisionBeadID when Postgres returns a
// non-empty gd-id from the parked_on_decision_bead_id projection.
func TestQueryListBeadsParkedOnDecisionScanTrue(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery("parked_on_decision_bead_id").
		WillReturnRows(addBeadWithTotalRowParked(
			sqlmock.NewRows(beadWithTotalColumns),
			1, "gd-task1", "issue", "task", "parked task", "open", 0, now,
			"gd-decisionP",
		))
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 || len(beads) != 1 {
		t.Fatalf("expected 1 bead, got total=%d len=%d", total, len(beads))
	}
	if !beads[0].ParkedOnDecision {
		t.Errorf("expected ParkedOnDecision=true, got false")
	}
	if beads[0].ParkedOnDecisionBeadID != "gd-decisionP" {
		t.Errorf("ParkedOnDecisionBeadID: got %q want gd-decisionP", beads[0].ParkedOnDecisionBeadID)
	}
	// The blocked projection must be untouched by a parked-only row — the two
	// signals are independent (edge-based vs text-mention).
	if beads[0].BlockedOnDecision {
		t.Errorf("expected BlockedOnDecision=false on a parked-only row, got true")
	}
}

// TestQueryListBeadsParkedOnDecisionScanFalse asserts the scan surfaces
// ParkedOnDecision=false when Postgres returns the COALESCE ” fallback — the
// "closed decision clears the badge" / "no false positives" contract.
func TestQueryListBeadsParkedOnDecisionScanFalse(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectQuery("parked_on_decision_bead_id").
		WillReturnRows(addBeadWithTotalRowParked(
			sqlmock.NewRows(beadWithTotalColumns),
			1, "gd-task2", "issue", "task", "un-parked task", "open", 0, now,
			"",
		))
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 1 || len(beads) != 1 {
		t.Fatalf("expected 1 bead, got total=%d len=%d", total, len(beads))
	}
	if beads[0].ParkedOnDecision {
		t.Errorf("expected ParkedOnDecision=false, got true")
	}
	if beads[0].ParkedOnDecisionBeadID != "" {
		t.Errorf("ParkedOnDecisionBeadID: got %q want empty", beads[0].ParkedOnDecisionBeadID)
	}
}

// TestQueryListBeadsParkedOnDecisionFilterTrue asserts filter.ParkedOnDecision
// = &true renders the `(<expr>) <> ”` WHERE predicate.
func TestQueryListBeadsParkedOnDecisionFilterTrue(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`(?s)bead_references.+<> ''`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	tr := true
	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{ParkedOnDecision: &tr})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsParkedOnDecisionFilterFalse asserts filter.ParkedOnDecision
// = &false renders the `(<expr>) = ”` WHERE predicate.
func TestQueryListBeadsParkedOnDecisionFilterFalse(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`(?s)bead_references.+= ''`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	fa := false
	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{ParkedOnDecision: &fa})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsParkedOnDecisionProjectionDefaultOff pins that with neither
// WithParkedOnDecision nor the filter set, the SELECT uses the cheap constant
// placeholder rather than the bead_references EXISTS subquery — the same
// perf-default discipline as blocked_on_decision.
func TestQueryListBeadsParkedOnDecisionProjectionDefaultOff(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`'' AS parked_on_decision_bead_id, 0::float8 AS relevance_score FROM beads`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	if _, _, err := queryListBeads(context.Background(), db, model.BeadFilter{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsParkedOnDecisionProjectionOptIn pins that
// WithParkedOnDecision=true (filter unset) opts the bead_references EXISTS
// subquery into the projection without adding a WHERE clause — `gb ready` /
// `gb news` need the column on every row but do not filter.
func TestQueryListBeadsParkedOnDecisionProjectionOptIn(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`(?s)FROM bead_references br.+AS parked_on_decision_bead_id, 0::float8 AS relevance_score FROM beads .+WHERE deleted_at IS NULL ORDER BY`).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))

	_, _, err := queryListBeads(context.Background(), db, model.BeadFilter{WithParkedOnDecision: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryListBeadsCountOnly pins that filter.CountOnly switches to a
// SELECT COUNT(*) query — no COUNT(*) OVER(), no ORDER BY, no LIMIT, no
// projections. Returning ([], total, nil) is the contract callers (the
// queued-count tile etc.) rely on to skip the per-row work.
func TestQueryListBeadsCountOnly(t *testing.T) {
	db, mock := newMockDB(t)

	// SELECT COUNT(*) FROM beads WHERE … — must NOT contain COUNT(*) OVER,
	// ORDER BY, LIMIT, or any of the projection column expressions. The
	// negative-lookahead-style assertion is just a regex over the rendered
	// query (no lookahead in Go's RE2): match a count-only SELECT and assert
	// the dangerous fragments are absent in a follow-up substring check.
	mock.ExpectQuery(`^SELECT COUNT\(\*\) FROM beads WHERE `).
		WithArgs("open").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{
		Status:    []model.Status{model.StatusOpen},
		CountOnly: true,
		// Limit / Sort / WithHasReport must be ignored on the count path.
		Limit:                 500,
		Sort:                  "-created_at",
		WithHasReport:         true,
		WithBlockedOnDecision: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if beads != nil {
		t.Errorf("CountOnly should return nil beads, got len=%d", len(beads))
	}
	if total != 42 {
		t.Errorf("expected total=42, got %d", total)
	}
}

// TestQueryListBeadsCountOnlyUnassigned pins that the Unassigned predicate rides
// the CountOnly fast path too, so `kd count --unassigned` counts exactly the rows
// `kd list --unassigned` would page. Both flow through the same where-builder, so
// this is the one assertion that keeps them from drifting (ka-0000007209).
//
// It matters because the count is what a work-seeking agent scripts against
// (N=$(kd count … --unassigned)): if the predicate were dropped only on the count
// path, the count would silently report the whole open set as available work.
func TestQueryListBeadsCountOnlyUnassigned(t *testing.T) {
	db, mock := newMockDB(t)

	// The NULL-safe clause must appear verbatim in the bare COUNT(*) query.
	mock.ExpectQuery(`^SELECT COUNT\(\*\) FROM beads WHERE status IN \(\$1\) AND \(assignee IS NULL OR assignee = ''\) AND deleted_at IS NULL$`).
		WithArgs("open").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(15))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{
		Status:     []model.Status{model.StatusOpen},
		Unassigned: true,
		CountOnly:  true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if beads != nil {
		t.Errorf("CountOnly should return nil beads, got len=%d", len(beads))
	}
	if total != 15 {
		t.Errorf("expected total=15, got %d", total)
	}
}

// TestQueryListBeadsCountOnlyWithSearch is a regression guard for the search
// fast path. The CountOnly path emits a bare `SELECT COUNT(*)` with NO ORDER BY
// and NO relevance_score projection, so the score CASE must NOT be built (it
// would reference no column). But the count MUST still match the ranked result
// set, so the FULL search predicate is present: the tokenized per-term ILIKE
// group OR'd with the Option-2 FTS predicate (ka-0000003832). The full-query
// arg ($3) is bound because the FTS predicate references it — a 2-term search
// binds $1, $2 (terms) and $3 (full query), and the count query references all
// three, so there is no bind-count mismatch.
func TestQueryListBeadsCountOnlyWithSearch(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`^SELECT COUNT\(\*\) FROM beads WHERE \(\(\(title ILIKE '%' \|\| \$1 \|\| '%' OR description ILIKE '%' \|\| \$1 \|\| '%' OR beads\.id = ANY\(ARRAY\(SELECT bead_id FROM comments WHERE comments\.text ILIKE '%' \|\| \$1 \|\| '%'\)\)\) AND \(title ILIKE '%' \|\| \$2 \|\| '%' OR description ILIKE '%' \|\| \$2 \|\| '%' OR beads\.id = ANY\(ARRAY\(SELECT bead_id FROM comments WHERE comments\.text ILIKE '%' \|\| \$2 \|\| '%'\)\)\)\) OR beads\.search_tsv @@ websearch_to_tsquery\('english', \$3\)\) AND deleted_at IS NULL$`).
		WithArgs("litellm", "ProxyException", "litellm ProxyException").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))

	beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{
		Search:    "litellm ProxyException",
		CountOnly: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if beads != nil {
		t.Errorf("CountOnly should return nil beads, got len=%d", len(beads))
	}
	if total != 7 {
		t.Errorf("expected total=7, got %d", total)
	}
}

func TestQueryCloseBead(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBaseLabels)
	rows.AddRow(
		"gd-cls1", nil, nil, nil, "issue", "task", "Close me", nil, nil,
		"closed", 0, nil, nil, now, nil, now,
		now, "alice", nil, nil, nil, "default", nil, nil,
		nil,
		nil,
	)
	mock.ExpectQuery("UPDATE beads SET").WithArgs("gd-cls1", "alice").WillReturnRows(rows)
	emptyRelationalExpectations(mock, "gd-cls1")

	bead, err := queryCloseBead(context.Background(), db, "gd-cls1", "alice")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bead.ID != "gd-cls1" || bead.Status != "closed" || bead.ClosedBy != "alice" {
		t.Fatalf("got id=%q status=%q closed_by=%q", bead.ID, bead.Status, bead.ClosedBy)
	}
	if bead.ClosedAt == nil {
		t.Fatal("expected closed_at to be set")
	}
}

func TestQueryCloseBead_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("UPDATE beads SET").WithArgs("nonexistent", "").WillReturnError(sql.ErrNoRows)

	_, err := queryCloseBead(context.Background(), db, "nonexistent", "")
	if err != sql.ErrNoRows {
		t.Fatalf("expected sql.ErrNoRows, got %v", err)
	}
}

func TestQueryCloseBead_WithRelationalData(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBaseLabels)
	rows.AddRow(
		"gd-cls2", nil, nil, nil, "issue", "task", "Close me", nil, nil,
		"closed", 0, nil, nil, now, nil, now,
		now, "bob", nil, nil, nil, "default", nil, nil,
		nil,
		"{backend,urgent}", // labels ride on the RETURNING row
	)
	mock.ExpectQuery(`UPDATE beads\s+SET status = 'closed'.+RETURNING .+FROM labels l WHERE l\.bead_id = beads\.id`).WithArgs("gd-cls2", "bob").WillReturnRows(rows)

	// Dependencies
	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id = \\$1").WithArgs("gd-cls2").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}).
			AddRow("gd-cls2", "gd-other", "blocks", now, nil, nil))
	// Comments
	mock.ExpectQuery(`SELECT .+ FROM comments .+WHERE comments\.bead_id = \$1`).WithArgs("gd-cls2").
		WillReturnRows(sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at", "operated_by", "operated_by_display", "operated_by_email", "impersonated_by"}).
			AddRow(int64(1), "gd-cls2", "alice", "Done!", now, nil, "", "", ""))

	bead, err := queryCloseBead(context.Background(), db, "gd-cls2", "bob")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bead.Labels) != 2 {
		t.Fatalf("expected 2 labels, got %d", len(bead.Labels))
	}
	if len(bead.Dependencies) != 1 {
		t.Fatalf("expected 1 dependency, got %d", len(bead.Dependencies))
	}
	if len(bead.Comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(bead.Comments))
	}
}

func TestScanBead_WithOptionalFields(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	closedAt := now.Add(-time.Hour)
	dueAt := now.Add(24 * time.Hour)

	rows := sqlmock.NewRows([]string{
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
		"operated_by_id",
	}).AddRow(
		"gd-full", "test-slug", "fix the bridge", nil, "issue", "task", "Full bead", "A description", "Some notes",
		"closed", 2, "bob", "alice", now, "carol", now,
		closedAt, "dave", dueAt, nil, []byte(`{"foo":"bar"}`), "production", nil, nil,
		int64(42),
	)
	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	bead, err := scanBead(db.QueryRow("SELECT"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bead.Slug != "test-slug" || bead.Description != "A description" {
		t.Fatalf("got slug=%q description=%q", bead.Slug, bead.Description)
	}
	if bead.ShortTitle != "fix the bridge" {
		t.Fatalf("got short_title=%q, want %q", bead.ShortTitle, "fix the bridge")
	}
	if bead.Notes != "Some notes" {
		t.Fatalf("got notes=%q", bead.Notes)
	}
	if bead.Priority != 2 || bead.Assignee != "bob" || bead.Owner != "alice" || bead.CreatedBy != "carol" {
		t.Fatalf("got priority=%d assignee=%q owner=%q created_by=%q", bead.Priority, bead.Assignee, bead.Owner, bead.CreatedBy)
	}
	if bead.ClosedAt == nil || bead.DueAt == nil || bead.DeferUntil != nil {
		t.Fatalf("got closed_at=%v due_at=%v defer_until=%v", bead.ClosedAt, bead.DueAt, bead.DeferUntil)
	}
	if string(bead.Fields) != `{"foo":"bar"}` {
		t.Fatalf("got fields=%s", bead.Fields)
	}
	if bead.OperatedByID == nil || *bead.OperatedByID != 42 {
		t.Fatalf("got operated_by_id=%v, want 42", bead.OperatedByID)
	}
}

func TestQueryGetDependenciesForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	depRows := sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}).
		AddRow("gd-b1", "gd-b2", "blocks", now, nil, nil).
		AddRow("gd-b1", "gd-b3", "related", now, nil, nil)
	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id = ANY").
		WillReturnRows(depRows)

	result, err := queryGetDependenciesForBeads(context.Background(), db, []string{"gd-b1", "gd-b2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["gd-b1"]) != 2 {
		t.Fatalf("expected 2 deps for gd-b1, got %d", len(result["gd-b1"]))
	}
}

func TestQueryGetDependencyCounts(t *testing.T) {
	db, mock := newMockDB(t)

	fwdRows := sqlmock.NewRows([]string{"bead_id", "count"}).
		AddRow("gd-c1", 3)
	mock.ExpectQuery("SELECT bead_id, COUNT\\(\\*\\) FROM deps WHERE bead_id = ANY").
		WillReturnRows(fwdRows)

	revRows := sqlmock.NewRows([]string{"depends_on_id", "count"}).
		AddRow("gd-c1", 1)
	mock.ExpectQuery("SELECT depends_on_id, COUNT\\(\\*\\) FROM deps WHERE depends_on_id = ANY").
		WillReturnRows(revRows)

	result, err := queryGetDependencyCounts(context.Background(), db, []string{"gd-c1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["gd-c1"].DependencyCount != 3 {
		t.Fatalf("expected dep count 3, got %d", result["gd-c1"].DependencyCount)
	}
	if result["gd-c1"].DependentCount != 1 {
		t.Fatalf("expected dep-by count 1, got %d", result["gd-c1"].DependentCount)
	}
}

func TestQueryGetLabelsForBeads(t *testing.T) {
	db, mock := newMockDB(t)

	labelRows := sqlmock.NewRows([]string{"bead_id", "label"}).
		AddRow("gd-l1", "urgent").
		AddRow("gd-l1", "frontend").
		AddRow("gd-l2", "backend")
	mock.ExpectQuery("SELECT bead_id, label FROM labels WHERE bead_id = ANY").
		WillReturnRows(labelRows)

	result, err := queryGetLabelsForBeads(context.Background(), db, []string{"gd-l1", "gd-l2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["gd-l1"]) != 2 {
		t.Fatalf("expected 2 labels for gd-l1, got %d", len(result["gd-l1"]))
	}
	if len(result["gd-l2"]) != 1 {
		t.Fatalf("expected 1 label for gd-l2, got %d", len(result["gd-l2"]))
	}
}

func TestQueryGetBlockedByForBeads(t *testing.T) {
	db, mock := newMockDB(t)

	blockedRows := sqlmock.NewRows([]string{"bead_id", "depends_on_id"}).
		AddRow("gd-blocked1", "gd-blocker1").
		AddRow("gd-blocked1", "gd-blocker2")
	mock.ExpectQuery("SELECT d.bead_id, d.depends_on_id FROM deps").
		WillReturnRows(blockedRows)

	result, err := queryGetBlockedByForBeads(context.Background(), db, []string{"gd-blocked1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["gd-blocked1"]) != 2 {
		t.Fatalf("expected 2 blockers, got %d", len(result["gd-blocked1"]))
	}
}

// TestQueryGetOpenBlockersForBeads pins the enriched batch blocker join that
// backs `kd list --with-blockers` (ka-0000000945): the caller gets the
// blocker's status/type/title on the edge, so no follow-up lookup is needed.
func TestQueryGetOpenBlockersForBeads(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "status", "blocker_type", "title", "in_scope"}).
		AddRow("gd-bug1", "gd-dec1", "blocks", "open", "decision", "Operator must pick A or B", true).
		AddRow("gd-bug1", "gd-task9", "depends-on", "in_progress", "task", "Upstream refactor", true)
	mock.ExpectQuery("SELECT d.bead_id, d.depends_on_id, d.type, blocker.status, blocker.type, blocker.title").
		WillReturnRows(rows)

	result, err := queryGetOpenBlockersForBeads(context.Background(), db, []string{"gd-bug1"}, blockerReadScope{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := result["gd-bug1"]
	if len(got) != 2 {
		t.Fatalf("expected 2 open blockers, got %d", len(got))
	}
	// The whole point of the denormalization: status/type/title ride along on
	// the edge. A caller reading these must never need a second round-trip.
	if got[0].ID != "gd-dec1" || got[0].Status != model.StatusOpen ||
		got[0].Type != "decision" || got[0].Title != "Operator must pick A or B" {
		t.Errorf("blocker[0] not fully denormalized: %+v", got[0])
	}
	if got[0].EdgeType != model.DepBlocks {
		t.Errorf("blocker[0].EdgeType = %q, want blocks", got[0].EdgeType)
	}
	if got[0].OutOfScope {
		t.Errorf("in-scope blocker[0] must not be marked out_of_scope: %+v", got[0])
	}
	if got[1].ID != "gd-task9" || got[1].Status != model.StatusInProgress {
		t.Errorf("blocker[1] wrong: %+v", got[1])
	}
}

// TestQueryGetOpenBlockersForBeads_CrossProjectStub is the regression guard for
// ka-0000007462: a blocker OUTSIDE the caller's project scope must still be
// REPORTED (so a gated bead never reads as unblocked), but with its free-text
// title REDACTED and OutOfScope set. Before the fix, the label scope FILTERED
// the join, so a bead whose only open blocking edge was cross-project came back
// with an empty open_blockers[] — directly contradicting --no-blockers, which
// (applying no scope to the blocker) had hidden that same row.
func TestQueryGetOpenBlockersForBeads_CrossProjectStub(t *testing.T) {
	db, mock := newMockDB(t)

	// The blocker join now SELECTs a computed in_scope boolean instead of
	// filtering on the label predicate. The out-of-scope row (in_scope=false)
	// still comes back — the store, not the SQL, redacts it.
	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "status", "blocker_type", "title", "in_scope"}).
		AddRow("gd-bug1", "gd-inproj", "blocks", "open", "task", "Same-project blocker", true).
		AddRow("gd-bug1", "gd-xproj", "blocks", "open", "escalation", "Merge monorepo!1998 (leaky title)", false)
	mock.ExpectQuery("SELECT d.bead_id, d.depends_on_id, d.type, blocker.status, blocker.type, blocker.title").
		WillReturnRows(rows)

	result, err := queryGetOpenBlockersForBeads(context.Background(), db, []string{"gd-bug1"},
		blockerReadScope{RequireAllLabels: []string{"project:gardenio"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := result["gd-bug1"]
	// The cross-project blocker must NOT vanish — that was the bug.
	if len(got) != 2 {
		t.Fatalf("expected 2 open blockers (in- AND out-of-scope), got %d: %+v", len(got), got)
	}
	if got[0].ID != "gd-inproj" || got[0].OutOfScope || got[0].Title != "Same-project blocker" {
		t.Errorf("in-scope blocker mangled: %+v", got[0])
	}
	// The out-of-scope blocker keeps its non-free-text fields (id/status/type/
	// edge_type) but its title is redacted and OutOfScope is set.
	oos := got[1]
	if oos.ID != "gd-xproj" || oos.Status != model.StatusOpen || oos.Type != "escalation" || oos.EdgeType != model.DepBlocks {
		t.Errorf("out-of-scope blocker lost its safe identifying fields: %+v", oos)
	}
	if !oos.OutOfScope {
		t.Errorf("cross-project blocker must be marked OutOfScope, got %+v", oos)
	}
	if oos.Title != "" {
		t.Errorf("cross-project blocker title must be redacted, got %q", oos.Title)
	}
}

// TestQueryListBeads_WithOpenBlockers exercises the REAL production path
// (queryListBeads -> queryGetOpenBlockersForBeads), not a mock's hand-written
// reimplementation of the blocking rules. It pins the two things the
// server-level test cannot see: the enrichment fires only when opted in, and
// an unblocked bead gets a present-but-EMPTY list rather than nil — the
// distinction that lets a client tell "checked, clear" from "never answered".
// TestQueryListBeads_TotalWithoutWindowCount pins the gd-g25jihMHFs contract:
// the data query no longer carries COUNT(*) OVER(); a full page (or an empty
// page past offset 0) takes one narrow SELECT COUNT(*) with the same WHERE and
// args, while a short page derives the total as offset+rows with no count.
func TestQueryListBeads_TotalWithoutWindowCount(t *testing.T) {
	now := time.Now().UTC()

	t.Run("FullPageCounts", func(t *testing.T) {
		db, mock := newMockDB(t)
		r := sqlmock.NewRows(beadWithTotalColumns)
		addBeadWithTotalRow(r, 0, "gd-1", "issue", "task", "T", "open", 0, now)
		addBeadWithTotalRow(r, 0, "gd-2", "issue", "task", "T", "open", 0, now)
		mock.ExpectQuery(`SELECT 0 AS total_count.+WHERE status IN \(\$1\) AND deleted_at IS NULL ORDER BY .+ LIMIT \$2`).
			WithArgs("open", 2).WillReturnRows(r)
		mock.ExpectQuery(`^SELECT COUNT\(\*\) FROM beads WHERE status IN \(\$1\) AND deleted_at IS NULL$`).
			WithArgs("open").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(6484))
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		_, total, err := queryListBeads(context.Background(), db, model.BeadFilter{
			Status: []model.Status{model.StatusOpen}, Limit: 2})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 6484 {
			t.Fatalf("total = %d, want 6484", total)
		}
	})

	t.Run("EmptyPagePastOffsetCounts", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))
		mock.ExpectQuery(`^SELECT COUNT\(\*\) FROM beads WHERE deleted_at IS NULL$`).
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
		beads, total, err := queryListBeads(context.Background(), db, model.BeadFilter{Limit: 10, Offset: 50})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(beads) != 0 || total != 7 {
			t.Fatalf("got %d beads total=%d, want 0 beads total=7", len(beads), total)
		}
	})

	t.Run("NoTotalFullPageSkipsCount", func(t *testing.T) {
		db, mock := newMockDB(t)
		r := sqlmock.NewRows(beadWithTotalColumns)
		addBeadWithTotalRow(r, 0, "gd-1", "issue", "task", "T", "open", 0, now)
		addBeadWithTotalRow(r, 0, "gd-2", "issue", "task", "T", "open", 0, now)
		mock.ExpectQuery(`SELECT 0 AS total_count.+LIMIT \$2`).WithArgs("open", 2).WillReturnRows(r)
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		_, total, err := queryListBeads(context.Background(), db, model.BeadFilter{
			Status: []model.Status{model.StatusOpen}, Limit: 2, NoTotal: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != -1 {
			t.Fatalf("total = %d, want -1 (not counted)", total)
		}
	})

	t.Run("NoTotalShortPageExact", func(t *testing.T) {
		db, mock := newMockDB(t)
		r := sqlmock.NewRows(beadWithTotalColumns)
		addBeadWithTotalRow(r, 0, "gd-1", "issue", "task", "T", "open", 0, now)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(r)
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		_, total, err := queryListBeads(context.Background(), db, model.BeadFilter{Limit: 10, Offset: 20, NoTotal: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 21 {
			t.Fatalf("total = %d, want 21 (offset + rows)", total)
		}
	})

	t.Run("EmptyFirstPageNoCount", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))
		_, total, err := queryListBeads(context.Background(), db, model.BeadFilter{Limit: 10})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if total != 0 {
			t.Fatalf("total = %d, want 0", total)
		}
	})
}

func TestQueryListBeads_WithOpenBlockers(t *testing.T) {
	now := time.Now().UTC()
	listRow := func() *sqlmock.Rows {
		return sqlmock.NewRows(beadWithTotalColumns).
			AddRow(2, "gd-blocked", nil, nil, nil, "issue", "task", "Blocked", nil, nil, "open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil, nil, "", "", "", false, "", "", 0.0).
			AddRow(2, "gd-free", nil, nil, nil, "issue", "task", "Free", nil, nil, "open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil, nil, "", "", "", false, "", "", 0.0)
	}

	t.Run("opt-in populates blocked and empty-but-present for unblocked", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(listRow())
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		mock.ExpectQuery("SELECT d.bead_id").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "status", "blocker_type", "title", "in_scope"}).
				AddRow("gd-blocked", "gd-dec", "blocks", "open", "decision", "operator must choose", true))

		beads, _, err := queryListBeads(context.Background(), db, model.BeadFilter{WithOpenBlockers: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		byID := map[string]*model.Bead{}
		for _, b := range beads {
			byID[b.ID] = b
		}
		blocked := byID["gd-blocked"]
		if blocked.OpenBlockers == nil || len(*blocked.OpenBlockers) != 1 {
			t.Fatalf("gd-blocked should carry 1 blocker, got %v", blocked.OpenBlockers)
		}
		if got := (*blocked.OpenBlockers)[0]; got.ID != "gd-dec" || got.Type != "decision" || got.Title != "operator must choose" {
			t.Errorf("blocker not denormalized: %+v", got)
		}
		// The three-state contract: unblocked must be present-and-empty.
		free := byID["gd-free"]
		if free.OpenBlockers == nil {
			t.Fatal("an unblocked bead must get a present-but-empty list, not nil — nil means 'not answered'")
		}
		if len(*free.OpenBlockers) != 0 {
			t.Errorf("gd-free should have no blockers, got %+v", *free.OpenBlockers)
		}
	})

	t.Run("cross-project-only blocker yields non-empty open_blockers (ka-0000007462)", func(t *testing.T) {
		// The live repro: a scoped list (Labels=project:gardenio) over a bead
		// whose ONLY open blocking edge points at another project. --no-blockers
		// applies no scope to the blocker and HIDES the row; the enrichment must
		// agree — it must report the blocker (redacted), never an empty list that
		// reads as dispatchable.
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(
			sqlmock.NewRows(beadWithTotalColumns).
				AddRow(1, "gd-gated", nil, nil, nil, "issue", "bug", "Gated bug", nil, nil, "open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil, nil, "", "", "", false, "", "", 0.0))
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}).
				AddRow("gd-gated", "project:gardenio"))
		// The blocker join returns the cross-project blocker with in_scope=false.
		mock.ExpectQuery("SELECT d.bead_id").
			WithArgs(sqlmock.AnyArg(), "project:gardenio").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "status", "blocker_type", "title", "in_scope"}).
				AddRow("gd-gated", "gd-xproj", "blocks", "open", "escalation", "Merge monorepo!1998 (secret title)", false))

		beads, _, err := queryListBeads(context.Background(), db, model.BeadFilter{
			WithOpenBlockers: true,
			Labels:           []string{"project:gardenio"},
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(beads) != 1 {
			t.Fatalf("expected 1 bead, got %d", len(beads))
		}
		ob := beads[0].OpenBlockers
		if ob == nil || len(*ob) != 1 {
			t.Fatalf("gated bead must report its cross-project blocker, not an empty list — got %v", ob)
		}
		b := (*ob)[0]
		if b.ID != "gd-xproj" || !b.OutOfScope {
			t.Errorf("cross-project blocker must be present and marked out_of_scope: %+v", b)
		}
		if b.Title != "" {
			t.Errorf("cross-project blocker title must be redacted, got %q", b.Title)
		}
	})

	t.Run("default path runs no blocker query and leaves the field nil", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(listRow())
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		// No third ExpectQuery: sqlmock's ExpectationsWereMet (t.Cleanup in
		// newMockDB) fails if an unexpected query is issued, so this asserts
		// the default caller pays NOTHING for the enrichment.

		beads, _, err := queryListBeads(context.Background(), db, model.BeadFilter{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, b := range beads {
			if b.OpenBlockers != nil {
				t.Errorf("%s: OpenBlockers must stay nil when not opted in, got %v", b.ID, *b.OpenBlockers)
			}
		}
	})
}

// TestQueryListBeads_WithParent guards the ka-0000006083 enrichment: the daemon
// surfaces each row's immediate parent-child parent id (and title, in scope)
// ONLY when opted in, and pays NOTHING for it otherwise. Mirrors the
// WithOpenBlockers test — the parent hydration is a single batched query keyed
// on the returned page, never an N+1.
func TestQueryListBeads_WithParent(t *testing.T) {
	now := time.Now().UTC()
	listRow := func() *sqlmock.Rows {
		return sqlmock.NewRows(beadWithTotalColumns).
			AddRow(2, "gd-leaf", nil, nil, nil, "issue", "task", "Leaf", nil, nil, "open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil, nil, "", "", "", false, "", "", 0.0).
			AddRow(2, "gd-orphan", nil, nil, nil, "issue", "task", "Orphan", nil, nil, "open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil, nil, "", "", "", false, "", "", 0.0)
	}

	t.Run("opt-in populates parent id+title for parented, leaves orphan empty", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(listRow())
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		// Only gd-leaf has a parent-child parent row; gd-orphan is absent from
		// the batched result and must read back an empty parent id.
		mock.ExpectQuery("SELECT DISTINCT ON \\(d.bead_id\\)").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "title"}).
				AddRow("gd-leaf", "gd-epic", "Delivery Epic"))

		beads, _, err := queryListBeads(context.Background(), db, model.BeadFilter{WithParent: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		byID := map[string]*model.Bead{}
		for _, b := range beads {
			byID[b.ID] = b
		}
		if got := byID["gd-leaf"]; got.ParentID != "gd-epic" || got.ParentTitle != "Delivery Epic" {
			t.Errorf("gd-leaf parent not denormalized: id=%q title=%q", got.ParentID, got.ParentTitle)
		}
		if got := byID["gd-orphan"]; got.ParentID != "" || got.ParentTitle != "" {
			t.Errorf("gd-orphan must have empty parent, got id=%q title=%q", got.ParentID, got.ParentTitle)
		}
	})

	t.Run("in-scope parent id survives even when title is NULL (out of project scope)", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(listRow())
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		mock.ExpectQuery("SELECT DISTINCT ON \\(d.bead_id\\)").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "title"}).
				AddRow("gd-leaf", "gd-epic", nil)) // NULL title -> out-of-scope parent

		beads, _, err := queryListBeads(context.Background(), db, model.BeadFilter{WithParent: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, b := range beads {
			if b.ID == "gd-leaf" {
				if b.ParentID != "gd-epic" {
					t.Errorf("parent id must survive a NULL (out-of-scope) title, got %q", b.ParentID)
				}
				if b.ParentTitle != "" {
					t.Errorf("out-of-scope parent title must be empty, got %q", b.ParentTitle)
				}
			}
		}
	})

	t.Run("default path runs no parent query and leaves the field empty", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count`).WillReturnRows(listRow())
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))
		// No third ExpectQuery: newMockDB's ExpectationsWereMet cleanup fails on
		// any unexpected query, so this asserts the default caller pays NOTHING.

		beads, _, err := queryListBeads(context.Background(), db, model.BeadFilter{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		for _, b := range beads {
			if b.ParentID != "" || b.ParentTitle != "" {
				t.Errorf("%s: parent must stay empty when not opted in, got id=%q title=%q", b.ID, b.ParentID, b.ParentTitle)
			}
		}
	})
}

// TestQueryGetParentsForBeads_AppliesReadScope guards the authz boundary for the
// parent enrichment. The join reaches the PARENT bead outside the caller's
// result set and returns its free-text TITLE, so it must re-apply the caller's
// project-label predicates to the parent row — via the LEFT JOIN's ON clause so
// an out-of-scope parent yields a NULL title rather than dropping the child row.
func TestQueryGetParentsForBeads_AppliesReadScope(t *testing.T) {
	cols := []string{"bead_id", "depends_on_id", "title"}

	t.Run("unscoped caller emits no label predicate", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery("SELECT DISTINCT ON \\(d.bead_id\\)").
			WithArgs(sqlmock.AnyArg()). // just the ids array
			WillReturnRows(sqlmock.NewRows(cols))
		if _, err := queryGetParentsForBeads(context.Background(), db, []string{"gd-x"}, blockerReadScope{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("token project scope constrains the PARENT row", func(t *testing.T) {
		db, mock := newMockDB(t)
		// The label predicate must target labels.bead_id = parent.id — NOT the
		// child bead — or it scopes the wrong side of the join.
		mock.ExpectQuery(`labels\.bead_id = parent\.id`).
			WithArgs(sqlmock.AnyArg(), "project:alpha").
			WillReturnRows(sqlmock.NewRows(cols))
		_, err := queryGetParentsForBeads(context.Background(), db, []string{"gd-x"},
			blockerReadScope{RequireAllLabels: []string{"project:alpha"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("oidc any-of scope constrains the PARENT row", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`labels\.bead_id = parent\.id`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows(cols))
		_, err := queryGetParentsForBeads(context.Background(), db, []string{"gd-x"},
			blockerReadScope{RequireAnyLabels: []string{"project:alpha", "project:beta"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

// TestQueryGetParentsForBeads_MatchesParentChildAliases guards that the parent
// enrichment matches the SAME parent-child alias set (canonical 'parent-child'
// AND legacy 'parent') the rest of the read path uses (parentChildTypeInList),
// so children of legacy epics resolve a parent rather than reading as orphans.
func TestQueryGetParentsForBeads_MatchesParentChildAliases(t *testing.T) {
	db, mock := newMockDB(t)
	// The rendered IN-list must carry both aliases.
	mock.ExpectQuery(`d\.type IN \('parent-child', 'parent'\)`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "title"}))
	if _, err := queryGetParentsForBeads(context.Background(), db, []string{"gd-x"}, blockerReadScope{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryGetParentsForBeads_MeaningfulTieBreak guards that a child with several
// parent-child edges resolves to a MEANINGFUL primary parent, not the
// lexicographically-first id (ka-0000001227). The DISTINCT ON tie-break must
// prefer an OPEN parent over a closed one, then an EPIC over a non-epic, then the
// most recent edge — so `gb ready` names the live container a bead hangs under
// rather than a dead one that merely sorts earlier.
func TestQueryGetParentsForBeads_MeaningfulTieBreak(t *testing.T) {
	db, mock := newMockDB(t)
	// The rendered ORDER BY must carry the open-over-closed, epic-over-non-epic,
	// and most-recent-edge keys — NOT a bare `ORDER BY d.bead_id, d.depends_on_id`.
	mock.ExpectQuery(`ORDER BY d\.bead_id,\s*COALESCE\(parent\.status = 'closed', true\),\s*COALESCE\(parent\.type <> 'epic', true\),\s*d\.created_at DESC,\s*d\.depends_on_id`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "depends_on_id", "title"}))
	if _, err := queryGetParentsForBeads(context.Background(), db, []string{"gd-x"}, blockerReadScope{}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestQueryGetOpenBlockersForBeads_AppliesReadScope guards the authz boundary.
// The blocker join reaches beads OUTSIDE the caller's result set and returns
// their free-text TITLE, so it must re-apply the caller's project-label
// predicates to the BLOCKER row. Without this a project-bound fleet token
// could read a cross-project blocker's title through the enrichment, even
// though the list rows themselves were correctly scoped.
func TestQueryGetOpenBlockersForBeads_AppliesReadScope(t *testing.T) {
	cols := []string{"bead_id", "depends_on_id", "type", "status", "blocker_type", "title", "in_scope"}

	t.Run("unscoped caller emits no label predicate", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery("SELECT d.bead_id").
			WithArgs(sqlmock.AnyArg()). // just the ids array
			WillReturnRows(sqlmock.NewRows(cols))
		if _, err := queryGetOpenBlockersForBeads(context.Background(), db, []string{"gd-x"}, blockerReadScope{}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("token project scope constrains the blocker row", func(t *testing.T) {
		db, mock := newMockDB(t)
		// The label predicate must target labels.bead_id = blocker.id — NOT
		// the blocked bead — or it scopes the wrong side of the join. It now
		// lives in the SELECT list as an in_scope flag rather than a WHERE
		// filter (ka-0000007462: the blocker must still be RETURNED, only its
		// title redacted), but it must still bind the caller's project label
		// against the blocker side.
		mock.ExpectQuery(`labels\.bead_id = blocker\.id`).
			WithArgs(sqlmock.AnyArg(), "project:alpha").
			WillReturnRows(sqlmock.NewRows(cols))
		_, err := queryGetOpenBlockersForBeads(context.Background(), db, []string{"gd-x"},
			blockerReadScope{RequireAllLabels: []string{"project:alpha"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("oidc any-of scope constrains the blocker row", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`labels\.bead_id = blocker\.id`).
			WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
			WillReturnRows(sqlmock.NewRows(cols))
		_, err := queryGetOpenBlockersForBeads(context.Background(), db, []string{"gd-x"},
			blockerReadScope{RequireAnyLabels: []string{"project:alpha", "project:beta"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("out-of-scope blocker is NOT filtered out of the query", func(t *testing.T) {
		// Regression guard for the root cause of ka-0000007462: the label scope
		// must NOT appear in a WHERE/AND clause that would drop the blocker row.
		// It belongs only in the SELECT-list in_scope expression. A scoped query
		// whose SQL still filtered on the label would silently omit cross-project
		// blockers again.
		db, mock := newMockDB(t)
		mock.ExpectQuery("SELECT d.bead_id").
			WithArgs(sqlmock.AnyArg(), "project:alpha").
			WillReturnRows(sqlmock.NewRows(cols).
				AddRow("gd-x", "gd-other", "blocks", "open", "task", "cross-project title", false))
		got, err := queryGetOpenBlockersForBeads(context.Background(), db, []string{"gd-x"},
			blockerReadScope{RequireAllLabels: []string{"project:alpha"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got["gd-x"]) != 1 {
			t.Fatalf("out-of-scope blocker must survive the scoped query, got %d", len(got["gd-x"]))
		}
		if b := got["gd-x"][0]; !b.OutOfScope || b.Title != "" {
			t.Errorf("out-of-scope blocker must be redacted (title=\"\", OutOfScope=true), got %+v", b)
		}
	})
}

// TestQueryGetOpenBlockersForBeads_MatchesReadyQueueSemantics guards the
// contract that makes --with-blockers trustworthy: it must explain exactly the
// rows --no-blockers hides. Both must key on the SAME blocking edge types and
// the SAME open-status set.
//
// The status half is the regression the bug actually observed: the audited
// triage agent hand-rolled IN("open","in_progress","deferred") and silently
// MISSED a blocker sitting in status 'blocked' (ka-0000000945). If someone
// narrows either set here without narrowing NoOpenDeps, that false all-clear
// comes straight back.
func TestQueryGetOpenBlockersForBeads_MatchesReadyQueueSemantics(t *testing.T) {
	// Every canonical blocking edge type must be in the allow-list the query
	// renders. It reads blockingDepTypes directly — the same variable
	// FilterNoOpenDeps renders — so the two cannot drift apart silently.
	for _, want := range []string{"blocks", "blocked-by", "depends-on"} {
		if !slices.Contains(blockingDepTypes, want) {
			t.Errorf("blocking dep type %q missing from --with-blockers query (drifted from FilterNoOpenDeps)", want)
		}
	}
	// And 'blocked' must count as an open status — the exact miss in the audit.
	for _, want := range []string{"open", "in_progress", "blocked", "deferred"} {
		if !strings.Contains(openBlockerStatusSQL, "'"+want+"'") {
			t.Errorf("status %q missing from --with-blockers open-status set (drifted from FilterNoOpenDeps)", want)
		}
	}

	// The assertions above only prove the SHARED constants are right; they
	// would stay green if NoOpenDeps stopped USING them and re-inlined its own
	// literal. So also assert the rendered ready-queue SQL contains both
	// shared renderings verbatim — that is what actually pins the lockstep.
	db, mock := newMockDB(t)
	mock.ExpectQuery(regexp.QuoteMeta("AND d.type IN (" + blockingDepTypeInList() + ") " +
		"AND dep.status IN (" + openBlockerStatusSQL + ")")).
		WillReturnRows(sqlmock.NewRows(beadWithTotalColumns))
	if _, _, err := queryListBeads(context.Background(), db, model.BeadFilter{NoOpenDeps: true}); err != nil {
		t.Fatalf("NoOpenDeps SQL no longer renders the shared blocking-type/status lists "+
			"that --with-blockers uses — the two have drifted: %v", err)
	}
}

func TestQueryGetBeadsByIDs(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	beadRows := sqlmock.NewRows(beadRowColumns).
		AddRow("gd-id1", nil, nil, nil, "issue", "task", "Bead 1", nil, nil, "open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil, nil, "", "", "")
	mock.ExpectQuery(`SELECT .+ FROM beads .+WHERE beads\.id = ANY`).
		WillReturnRows(beadRows)
	// Labels populated via follow-up batch query (ka-0000000676).
	mock.ExpectQuery("SELECT bead_id, label FROM labels WHERE bead_id = ANY").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	result, err := queryGetBeadsByIDs(context.Background(), db, []string{"gd-id1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 1 || result[0].ID != "gd-id1" {
		t.Fatalf("expected 1 bead with id=gd-id1, got %v", result)
	}
}

func TestQueryGetDependenciesForBeads_Empty(t *testing.T) {
	result, err := queryGetDependenciesForBeads(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Fatalf("expected empty map, got %d entries", len(result))
	}
}

func TestQueryGetStats(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`SELECT`).WillReturnRows(
		sqlmock.NewRows([]string{"open", "in_progress", "blocked", "closed", "deferred"}).
			AddRow(5, 3, 2, 10, 1),
	)

	stats, err := queryGetStats(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalOpen != 5 {
		t.Fatalf("expected total_open=5, got %d", stats.TotalOpen)
	}
	if stats.TotalInProgress != 3 {
		t.Fatalf("expected total_in_progress=3, got %d", stats.TotalInProgress)
	}
	if stats.TotalBlocked != 2 {
		t.Fatalf("expected total_blocked=2, got %d", stats.TotalBlocked)
	}
	if stats.TotalClosed != 10 {
		t.Fatalf("expected total_closed=10, got %d", stats.TotalClosed)
	}
	if stats.TotalDeferred != 1 {
		t.Fatalf("expected total_deferred=1, got %d", stats.TotalDeferred)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestQueryCreateBead_WithFleet(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-fleet1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Fleet bead", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
		Fleet: "qa-fleet",
	}
	mock.ExpectExec("INSERT INTO beads").
		WithArgs(
			"gd-fleet1", sqlmock.AnyArg(), "issue", "task", "Fleet bead", "", "",
			"open", 0, "", "", now, "", now,
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"qa-fleet", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryCreateBead(context.Background(), db, bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryCreateBead_EmptyFleetDefaultsToDefault(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-nofleet", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "No fleet", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
		// Fleet intentionally empty.
	}
	mock.ExpectExec("INSERT INTO beads").
		WithArgs(
			"gd-nofleet", sqlmock.AnyArg(), "issue", "task", "No fleet", "", "",
			"open", 0, "", "", now, "", now,
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			"default", sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryCreateBead(context.Background(), db, bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryAddLabel_InheritsFleetFromBead(t *testing.T) {
	db, mock := newMockDB(t)
	// The INSERT now includes fleet via COALESCE subquery.
	mock.ExpectExec("INSERT INTO labels \\(bead_id, label, fleet\\)").
		WithArgs("gd-a", "urgent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryAddLabel(context.Background(), db, "gd-a", "urgent"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryAddComment_InheritsFleetFromBead(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	comment := &model.Comment{BeadID: "gd-a", Author: "alice", Text: "Hello"}
	// The INSERT now includes fleet via COALESCE subquery.
	mock.ExpectQuery("INSERT INTO comments \\(bead_id, author, text, fleet, operated_by_id, impersonated_by\\)").
		WithArgs("gd-a", "alice", "Hello", sqlmock.AnyArg(), "").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), now))

	if err := queryAddComment(context.Background(), db, comment); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryRecordEvent_InheritsFleetFromBead(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	event := &model.Event{
		Topic: "beads.bead.created", BeadID: "gd-a", Actor: "alice",
		Payload: json.RawMessage(`{}`),
	}
	// The INSERT now includes fleet via COALESCE subquery.
	mock.ExpectQuery("INSERT INTO events \\(topic, bead_id, actor, payload, fleet, operated_by_id\\)").
		WithArgs("beads.bead.created", "gd-a", "alice", []byte(`{}`), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(1, now))

	if err := queryRecordEvent(context.Background(), db, event); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryAddDependency_InheritsFleetFromBead(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	dep := &model.Dependency{
		BeadID: "gd-a", DependsOnID: "gd-b", Type: model.DepBlocks, CreatedAt: now, CreatedBy: "alice",
	}
	// The INSERT now includes fleet via COALESCE subquery, and an 8th arg
	// (the typed-mention marker) for the claim-on-conflict WHERE guard.
	mock.ExpectExec("INSERT INTO deps \\(bead_id, depends_on_id, type, created_at, created_by, metadata, fleet, operated_by_id\\)").
		WithArgs("gd-a", "gd-b", "blocks", now, "alice", "", sqlmock.AnyArg(), typedMentionDepCreator).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryAddDependency(context.Background(), db, dep); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryGetBead_ReadsFleet(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsLabels).AddRow(
		"gd-f1", nil, nil, nil, "issue", "task", "Fleet bead", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "production", nil, nil,
		nil,
		"", "", "",
		nil,
	)
	mock.ExpectQuery(`SELECT .+ FROM beads .+WHERE beads\.id = \$1`).WithArgs("gd-f1").WillReturnRows(rows)
	emptyRelationalExpectations(mock, "gd-f1")

	bead, err := queryGetBead(context.Background(), db, "gd-f1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bead.Fleet != "production" {
		t.Fatalf("expected fleet=production, got %q", bead.Fleet)
	}
}

func TestBackfillFleet(t *testing.T) {
	tables := []string{"beads", "labels", "deps", "comments", "events", "configs"}

	t.Run("Noop_DefaultFleet", func(t *testing.T) {
		s := &PostgresStore{}
		n, err := s.BackfillFleet(context.Background(), "default")
		if err != nil || n != 0 {
			t.Fatalf("expected no-op, got n=%d err=%v", n, err)
		}
	})

	t.Run("Noop_EmptyFleet", func(t *testing.T) {
		s := &PostgresStore{}
		n, err := s.BackfillFleet(context.Background(), "")
		if err != nil || n != 0 {
			t.Fatalf("expected no-op, got n=%d err=%v", n, err)
		}
	})

	t.Run("UpdatesAllTables", func(t *testing.T) {
		db, mock := newMockDB(t)
		s := &PostgresStore{db: db}

		for _, table := range tables {
			mock.ExpectExec("UPDATE " + table + " SET fleet").
				WithArgs("gardenio").
				WillReturnResult(sqlmock.NewResult(0, 10))
		}

		n, err := s.BackfillFleet(context.Background(), "gardenio")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if n != 60 {
			t.Fatalf("expected 60 rows affected, got %d", n)
		}
	})

	t.Run("ErrorOnTable", func(t *testing.T) {
		db, mock := newMockDB(t)
		s := &PostgresStore{db: db}

		mock.ExpectExec("UPDATE beads SET fleet").
			WithArgs("gardenio").
			WillReturnResult(sqlmock.NewResult(0, 5))
		mock.ExpectExec("UPDATE labels SET fleet").
			WithArgs("gardenio").
			WillReturnError(fmt.Errorf("connection lost"))

		n, err := s.BackfillFleet(context.Background(), "gardenio")
		if err == nil {
			t.Fatal("expected error")
		}
		if n != 5 {
			t.Fatalf("expected 5 rows before error, got %d", n)
		}
	})
}

// mockPinger implements pinger for testing pingWithRetry.
type mockPinger struct {
	failCount int // number of Ping calls that should fail
	calls     int
}

func (m *mockPinger) Ping() error {
	m.calls++
	if m.calls <= m.failCount {
		return fmt.Errorf("read tcp 10.0.0.1:1234->10.0.0.2:5432: read: connection reset by peer")
	}
	return nil
}

func TestPingWithRetry_SucceedsFirstAttempt(t *testing.T) {
	p := &mockPinger{failCount: 0}
	err := pingWithRetry(p, 5, time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("expected 1 call, got %d", p.calls)
	}
}

func TestPingWithRetry_SucceedsAfterRetries(t *testing.T) {
	p := &mockPinger{failCount: 3}
	err := pingWithRetry(p, 5, time.Millisecond, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("expected success after retries, got: %v", err)
	}
	if p.calls != 4 {
		t.Fatalf("expected 4 calls (3 failures + 1 success), got %d", p.calls)
	}
}

func TestPingWithRetry_ExhaustsRetries(t *testing.T) {
	p := &mockPinger{failCount: 10}
	err := pingWithRetry(p, 2, time.Millisecond, 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	// 1 initial attempt + 2 retries = 3 calls
	if p.calls != 3 {
		t.Fatalf("expected 3 calls, got %d", p.calls)
	}
}

func TestPingWithRetry_BackoffCapsAtMax(t *testing.T) {
	p := &mockPinger{failCount: 5}
	start := time.Now()
	// initialDelay=1ms, maxDelay=4ms, 5 retries
	// delays: 1ms, 2ms, 4ms, 4ms, 4ms = 15ms total
	err := pingWithRetry(p, 5, time.Millisecond, 4*time.Millisecond)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 15*time.Millisecond {
		t.Fatalf("expected at least 15ms of backoff, got %v", elapsed)
	}
}

// TestQueryListBeads_OmitBodies guards the gd-zXdZq4Traf projection: with
// OmitDescription / OmitNotes the list SELECT reads a NULL in place of each
// body column (so Postgres never ships it), the column layout the scanner
// expects is unchanged, and the default path still selects both bodies.
func TestQueryListBeads_OmitBodies(t *testing.T) {
	now := time.Now().UTC()
	listRow := func() *sqlmock.Rows {
		return sqlmock.NewRows(beadWithTotalColumns).
			AddRow(1, "gd-trig", nil, nil, nil, "config", "trigger", "Trig", nil, nil, "open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, []byte(`{"cron":"* * * * *"}`), "default", nil, nil, nil, "", "", "", false, "", "", 0.0)
	}

	t.Run("omit both selects NULL placeholders", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT 0 AS total_count, beads\.id, beads\.slug, beads\.short_title, beads\.heraldry, beads\.kind, beads\.type, beads\.title, NULL::text AS description, NULL::text AS notes,`).
			WillReturnRows(listRow())
		mock.ExpectQuery("SELECT bead_id, label FROM labels").
			WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

		beads, _, err := queryListBeads(context.Background(), db, model.BeadFilter{OmitDescription: true, OmitNotes: true})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(beads) != 1 || beads[0].Description != "" || beads[0].Notes != "" {
			t.Fatalf("want one row with empty bodies, got %+v", beads)
		}
		if beads[0].Fields == nil {
			t.Error("fields must still be projected when only the bodies are omitted")
		}
	})

	t.Run("omit description only keeps notes", func(t *testing.T) {
		cols := beadListColumns(model.BeadFilter{OmitDescription: true})
		if strings.Contains(cols, "beads.description") || !strings.Contains(cols, "NULL::text AS description") {
			t.Errorf("description not replaced: %s", cols)
		}
		if !strings.Contains(cols, "beads.notes,") {
			t.Errorf("notes must still be selected: %s", cols)
		}
	})

	t.Run("default selects both bodies", func(t *testing.T) {
		if got := beadListColumns(model.BeadFilter{}); got != beadColumnsBeads {
			t.Errorf("default projection changed:\n got %s\nwant %s", got, beadColumnsBeads)
		}
	})
}

// TestBeadListColumns_OmitFields pins the omit_fields SQL (gd-zXdZq4Traf):
// each dotted path becomes one `#-` path literal on beads.fields, and an
// invalid path never reaches the SQL text.
func TestBeadListColumns_OmitFields(t *testing.T) {
	cols := beadListColumns(model.BeadFilter{OmitFields: []string{"prompt", "gardener.run_log", "x';DROP TABLE beads;--"}})
	want := `(beads.fields #- '{prompt}' #- '{gardener,run_log}') AS fields,`
	if !strings.Contains(cols, want) {
		t.Errorf("want %s in:\n%s", want, cols)
	}
	if strings.Contains(cols, "DROP") {
		t.Errorf("invalid path leaked into SQL: %s", cols)
	}
	if got := beadListColumns(model.BeadFilter{OmitFields: []string{"a;b"}}); got != beadColumnsBeads {
		t.Errorf("all-invalid paths must leave the projection unchanged, got %s", got)
	}
}

// TestBeadListColumns_FieldsOnly pins the fields_only SQL (gd-6PXLuR3juD):
// the kept keys become one IN list over jsonb_each, a NULL/non-object fields
// value is guarded, FieldsOnly wins over OmitFields, and an invalid or dotted
// key never reaches the SQL text.
func TestBeadListColumns_FieldsOnly(t *testing.T) {
	cols := beadListColumns(model.BeadFilter{
		FieldsOnly: []string{"total_cost_usd", "agent", "x';DROP TABLE beads;--", "a.b"},
		OmitFields: []string{"prompt"},
	})
	want := `(SELECT COALESCE(jsonb_object_agg(kv.k, kv.v), '{}'::jsonb) FROM jsonb_each(CASE WHEN jsonb_typeof(beads.fields) = 'object' THEN beads.fields ELSE '{}'::jsonb END) AS kv(k, v) WHERE kv.k IN ('total_cost_usd', 'agent')) AS fields,`
	if !strings.Contains(cols, want) {
		t.Errorf("want %s in:\n%s", want, cols)
	}
	if strings.Contains(cols, "DROP") || strings.Contains(cols, "'a.b'") {
		t.Errorf("invalid key leaked into SQL: %s", cols)
	}
	if strings.Contains(cols, "#-") {
		t.Errorf("fields_only must take precedence over omit_fields: %s", cols)
	}
	if got := beadListColumns(model.BeadFilter{FieldsOnly: []string{"a;b"}}); got != beadColumnsBeads {
		t.Errorf("all-invalid keys must leave the projection unchanged, got %s", got)
	}
}
