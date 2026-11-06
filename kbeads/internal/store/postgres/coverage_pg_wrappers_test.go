package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
	"github.com/DATA-DOG/go-sqlmock"
)

// These tests exercise the thin PUBLIC wrapper methods on *PostgresStore that
// delegate straight through to an already-tested query* helper. Each sets up
// the exact mock expectations the underlying helper needs (copied from the
// existing helper-level Test*), then calls the public method so the wrapper
// line is covered. The SQL shape is locked down by the helper-level tests;
// here we only need a happy path (plus a cheap error case where useful) to
// cover the wrapper.
//
// All identifiers are prefixed TestCovPGW to stay globally unique across the
// package.

func TestCovPGW_Ping(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectPing()

	if err := pg.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestCovPGW_Close(t *testing.T) {
	// Close shuts the *sql.DB; build a throwaway db so the shared cleanup
	// (which also Close()s) does not double-close the one under test.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	mock.ExpectClose()
	pg := &PostgresStore{db: db}
	if err := pg.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestCovPGW_GetBead(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsLabels).AddRow(
		"gd-w1", nil, nil, nil, "issue", "task", "Wrapper bead", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "",
		nil,
	)
	mock.ExpectQuery(`SELECT .+ FROM beads .* WHERE beads\.id = \$1`).WithArgs("gd-w1").WillReturnRows(rows)
	emptyRelationalExpectations(mock, "gd-w1")

	bead, err := pg.GetBead(context.Background(), "gd-w1")
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if bead.ID != "gd-w1" {
		t.Errorf("id = %q, want gd-w1", bead.ID)
	}
}

func TestCovPGW_GetBead_Error(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT .+ FROM beads`).WithArgs("gd-missing").WillReturnError(sql.ErrNoRows)

	if _, err := pg.GetBead(context.Background(), "gd-missing"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GetBead: err = %v, want sql.ErrNoRows", err)
	}
}

func TestCovPGW_FindAgentsByNames(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumns).AddRow(
		"gd-agent1", nil, nil, nil, "issue", "agent", "Agent bead", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "",
	)
	mock.ExpectQuery(`SELECT DISTINCT ON .+ FROM beads .+WHERE beads\.type = 'agent'`).
		WillReturnRows(rows)

	out, err := pg.FindAgentsByNames(context.Background(), []string{"alice"}, []model.Status{model.StatusOpen})
	if err != nil {
		t.Fatalf("FindAgentsByNames: %v", err)
	}
	if len(out) != 1 || out[0].ID != "gd-agent1" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestCovPGW_ListBeads(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	r := addBeadWithTotalRow(sqlmock.NewRows(beadWithTotalColumns), 1, "gd-lb1", "issue", "task", "T", "open", 0, now)
	mock.ExpectQuery(`SELECT 0 AS total_count, .+ FROM beads`).WillReturnRows(r)
	mock.ExpectQuery("SELECT bead_id, label FROM labels").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "label"}))

	beads, total, err := pg.ListBeads(context.Background(), model.BeadFilter{})
	if err != nil {
		t.Fatalf("ListBeads: %v", err)
	}
	if len(beads) != 1 || total != 1 {
		t.Fatalf("got len=%d total=%d, want 1/1", len(beads), total)
	}
}

func TestCovPGW_UpdateBead(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()
	bead := &model.Bead{
		ID: "gd-w1", Kind: model.KindIssue, Type: model.TypeTask,
		Title: "Updated", Status: model.StatusOpen, CreatedAt: now, UpdatedAt: now,
	}

	// UpdateBead wraps queryUpdateBead + replaceBeadReferences in a tx.
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs(
			"gd-w1", sqlmock.AnyArg(), "issue", "task", "Updated", "", "",
			"open", 0, "", "",
			sqlmock.AnyArg(), "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), // short_title
			sqlmock.AnyArg(), // heraldry
		).
		WillReturnRows(sqlmock.NewRows([]string{"updated_at"}).AddRow(now))
	// replaceBeadReferences deletes existing refs then (no body refs) commits.
	// The DELETE binds the bead id plus a text[] of the three source fields.
	mock.ExpectExec("DELETE FROM bead_references").
		WithArgs("gd-w1", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	if err := pg.UpdateBead(context.Background(), bead); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
}

func TestCovPGW_IncrementBeadFields(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBase).AddRow(
		"gd-inc1", nil, nil, nil, "issue", "task", "Inc", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, []byte(`{"count":2}`), "default", nil, nil,
		nil,
	)
	mock.ExpectQuery("UPDATE beads SET").
		WithArgs("gd-inc1", float64(2)).
		WillReturnRows(rows)

	bead, err := pg.IncrementBeadFields(context.Background(), "gd-inc1", map[string]float64{"count": 2})
	if err != nil {
		t.Fatalf("IncrementBeadFields: %v", err)
	}
	if bead.ID != "gd-inc1" {
		t.Errorf("id = %q, want gd-inc1", bead.ID)
	}
}

func TestCovPGW_CloseBead(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumnsBaseLabels).AddRow(
		"gd-cl1", nil, nil, nil, "issue", "task", "Close me", nil, nil,
		"closed", 0, nil, nil, now, nil, now, now, "alice", nil, nil, nil, "default", nil, nil,
		nil,
		nil,
	)
	mock.ExpectQuery("UPDATE beads SET").WithArgs("gd-cl1", "alice").WillReturnRows(rows)
	emptyRelationalExpectations(mock, "gd-cl1")

	bead, err := pg.CloseBead(context.Background(), "gd-cl1", "alice")
	if err != nil {
		t.Fatalf("CloseBead: %v", err)
	}
	if bead.Status != "closed" || bead.ClosedBy != "alice" {
		t.Fatalf("got status=%q closed_by=%q", bead.Status, bead.ClosedBy)
	}
}

func TestCovPGW_DeleteBead(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectExec("UPDATE beads SET deleted_at = NOW().+ WHERE id = \\$1 AND deleted_at IS NULL").
		WithArgs("gd-del1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := pg.DeleteBead(context.Background(), "gd-del1"); err != nil {
		t.Fatalf("DeleteBead: %v", err)
	}
}

func TestCovPGW_GetStats(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT`).WillReturnRows(
		sqlmock.NewRows([]string{"open", "in_progress", "blocked", "closed", "deferred"}).
			AddRow(5, 3, 2, 10, 1),
	)

	stats, err := pg.GetStats(context.Background())
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalOpen != 5 || stats.TotalClosed != 10 {
		t.Fatalf("got open=%d closed=%d", stats.TotalOpen, stats.TotalClosed)
	}
}

func TestCovPGW_FleetTypeStatusCounts(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"type", "status", "count"}).
		AddRow("task", "open", 5)
	mock.ExpectQuery(`SELECT type, status, COUNT\(\*\) FROM beads WHERE deleted_at IS NULL GROUP BY type, status`).
		WillReturnRows(rows)

	result, err := pg.FleetTypeStatusCounts(context.Background())
	if err != nil {
		t.Fatalf("FleetTypeStatusCounts: %v", err)
	}
	if got := result[store.FleetTypeStatusKey{Type: "task", Status: "open"}]; got != 5 {
		t.Errorf("task/open: want 5, got %d", got)
	}
}

func TestCovPGW_FleetActiveAgentsByProject(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"project", "count"}).AddRow("gardenio", 2)
	mock.ExpectQuery(`b.status IN \('open', 'in_progress'\)`).WillReturnRows(rows)

	result, err := pg.FleetActiveAgentsByProject(context.Background())
	if err != nil {
		t.Fatalf("FleetActiveAgentsByProject: %v", err)
	}
	if result["gardenio"] != 2 {
		t.Fatalf("unexpected active agents: %+v", result)
	}
}

func TestCovPGW_FleetAgentLifetimeRollup(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"project", "total_agents", "total_cost_usd", "session_count"}).
		AddRow("gardenio", 4, 19.75, 4.0)
	mock.ExpectQuery(`agent_values`).WillReturnRows(rows)

	result, err := pg.FleetAgentLifetimeRollup(context.Background())
	if err != nil {
		t.Fatalf("FleetAgentLifetimeRollup: %v", err)
	}
	if len(result) != 1 || result[0].Project != "gardenio" {
		t.Fatalf("unexpected rollup: %+v", result)
	}
}

func TestCovPGW_AcceptanceMetrics(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectQuery(`matt_owner_strings`).
		WithArgs(mattSlackSubject, mattOidcSubject, MattSlackOperator, MattOidcOperator).
		WillReturnRows(sqlmock.NewRows([]string{"matt", "total"}).AddRow(2, 10))
	mock.ExpectQuery(`AND NOT \(`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`o\.kind = 'fleet'`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(`type = 'user'`).
		WillReturnRows(sqlmock.NewRows([]string{"total", "matched"}).AddRow(4, 3))
	mock.ExpectQuery(`FROM human_ops`).
		WithArgs(sentinelEmailDomain).
		WillReturnRows(sqlmock.NewRows([]string{"humans", "operator_rows"}).AddRow(5, 5))

	m, err := pg.AcceptanceMetrics(context.Background())
	if err != nil {
		t.Fatalf("AcceptanceMetrics: %v", err)
	}
	if m.OwnerShareMatt.Matt != 2 || m.OwnerShareMatt.Total != 10 {
		t.Fatalf("got %d/%d", m.OwnerShareMatt.Matt, m.OwnerShareMatt.Total)
	}
}

func TestCovPGW_GetDependenciesForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}).
		AddRow("gd-d1", "gd-d2", "blocks", now, nil, nil)
	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id = ANY").WillReturnRows(rows)

	result, err := pg.GetDependenciesForBeads(context.Background(), []string{"gd-d1"})
	if err != nil {
		t.Fatalf("GetDependenciesForBeads: %v", err)
	}
	if len(result["gd-d1"]) != 1 {
		t.Fatalf("want 1 dep, got %d", len(result["gd-d1"]))
	}
}

func TestCovPGW_GetReverseDependenciesForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata", "child_type"}).
		AddRow("gd-child", "gd-parent", "blocks", now, nil, nil, "task")
	mock.ExpectQuery("SELECT .+ FROM deps d LEFT JOIN beads child .+ WHERE d.depends_on_id").WillReturnRows(rows)

	result, err := pg.GetReverseDependenciesForBeads(context.Background(), []string{"gd-parent"})
	if err != nil {
		t.Fatalf("GetReverseDependenciesForBeads: %v", err)
	}
	if len(result["gd-parent"]) != 1 {
		t.Fatalf("want 1 reverse dep, got %d", len(result["gd-parent"]))
	}
}

func TestCovPGW_GetDependencyCounts(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectQuery("SELECT bead_id, COUNT\\(\\*\\) FROM deps WHERE bead_id = ANY").
		WillReturnRows(sqlmock.NewRows([]string{"bead_id", "count"}).AddRow("gd-c1", 3))
	mock.ExpectQuery("SELECT depends_on_id, COUNT\\(\\*\\) FROM deps WHERE depends_on_id = ANY").
		WillReturnRows(sqlmock.NewRows([]string{"depends_on_id", "count"}).AddRow("gd-c1", 1))

	result, err := pg.GetDependencyCounts(context.Background(), []string{"gd-c1"})
	if err != nil {
		t.Fatalf("GetDependencyCounts: %v", err)
	}
	if result["gd-c1"].DependencyCount != 3 || result["gd-c1"].DependentCount != 1 {
		t.Fatalf("got fwd=%d rev=%d", result["gd-c1"].DependencyCount, result["gd-c1"].DependentCount)
	}
}

func TestCovPGW_GetChildrenStatusHistogram(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"depends_on_id", "status", "count"}).
		AddRow("gd-p1", "open", 2)
	mock.ExpectQuery(`SELECT d\.depends_on_id, b\.status, COUNT.*FROM deps d.*JOIN beads b`).
		WillReturnRows(rows)

	result, err := pg.GetChildrenStatusHistogram(context.Background(), []string{"gd-p1"})
	if err != nil {
		t.Fatalf("GetChildrenStatusHistogram: %v", err)
	}
	if result["gd-p1"]["open"] != 2 {
		t.Fatalf("want open=2, got %v", result["gd-p1"])
	}
}

func TestCovPGW_GetLabelsForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"bead_id", "label"}).
		AddRow("gd-l1", "urgent")
	mock.ExpectQuery("SELECT bead_id, label FROM labels WHERE bead_id = ANY").WillReturnRows(rows)

	result, err := pg.GetLabelsForBeads(context.Background(), []string{"gd-l1"})
	if err != nil {
		t.Fatalf("GetLabelsForBeads: %v", err)
	}
	if len(result["gd-l1"]) != 1 || result["gd-l1"][0] != "urgent" {
		t.Fatalf("want [urgent], got %v", result["gd-l1"])
	}
}

func TestCovPGW_GetTaintsForBead(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"taint_id"}).AddRow(int64(3))
	mock.ExpectQuery("SELECT bt.taint_id FROM bead_taints bt").WillReturnRows(rows)

	result, err := pg.GetTaintsForBead(context.Background(), "gd-agent1")
	if err != nil {
		t.Fatalf("GetTaintsForBead: %v", err)
	}
	if len(result) != 1 || result[0] != 3 {
		t.Fatalf("want [3], got %v", result)
	}
}

func TestCovPGW_GetBlockedByForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id"}).
		AddRow("gd-blocked1", "gd-blocker1")
	mock.ExpectQuery("SELECT d.bead_id, d.depends_on_id FROM deps").WillReturnRows(rows)

	result, err := pg.GetBlockedByForBeads(context.Background(), []string{"gd-blocked1"})
	if err != nil {
		t.Fatalf("GetBlockedByForBeads: %v", err)
	}
	if len(result["gd-blocked1"]) != 1 {
		t.Fatalf("want 1 blocker, got %d", len(result["gd-blocked1"]))
	}
}

func TestCovPGW_GetBeadsByIDs(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumns).AddRow(
		"gd-id1", nil, nil, nil, "issue", "task", "Bead 1", nil, nil, "open", 0, nil, nil, now, nil, now,
		nil, nil, nil, nil, nil, "default", nil, nil, nil, "", "", "",
	)
	mock.ExpectQuery(`SELECT .+ FROM beads .+WHERE beads\.id = ANY`).WillReturnRows(rows)
	// Labels are populated in a follow-up batch query so batch fetch matches
	// the single-GET / list shapes (ka-0000000676).
	labelRows := sqlmock.NewRows([]string{"bead_id", "label"}).
		AddRow("gd-id1", "squawk").
		AddRow("gd-id1", "from:agent-x")
	mock.ExpectQuery("SELECT bead_id, label FROM labels WHERE bead_id = ANY").WillReturnRows(labelRows)

	result, err := pg.GetBeadsByIDs(context.Background(), []string{"gd-id1"})
	if err != nil {
		t.Fatalf("GetBeadsByIDs: %v", err)
	}
	if len(result) != 1 || result[0].ID != "gd-id1" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if len(result[0].Labels) != 2 || result[0].Labels[0] != "squawk" || result[0].Labels[1] != "from:agent-x" {
		t.Fatalf("GetBeadsByIDs labels = %v, want [squawk from:agent-x]", result[0].Labels)
	}
}

func TestCovPGW_GetTextReferencesForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"source_bead_id", "target_bead_id", "source_field", "relationship_type"}).
		AddRow("gd-FNHTKRBDIQ", "gd-HKBGWAOTHB", "description", nil)
	mock.ExpectQuery(`SELECT source_bead_id, target_bead_id, source_field, relationship_type\s+FROM bead_references`).
		WillReturnRows(rows)

	result, err := pg.GetTextReferencesForBeads(context.Background(), []string{"gd-FNHTKRBDIQ", "gd-HKBGWAOTHB"})
	if err != nil {
		t.Fatalf("GetTextReferencesForBeads: %v", err)
	}
	if len(result) != 1 || result[0].SourceBeadID != "gd-FNHTKRBDIQ" {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestCovPGW_AddDependency(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()
	dep := &model.Dependency{
		BeadID: "gd-a", DependsOnID: "gd-b", Type: model.DepBlocks, CreatedAt: now, CreatedBy: "alice",
	}
	mock.ExpectExec("INSERT INTO deps").
		WithArgs("gd-a", "gd-b", "blocks", now, "alice", "", sqlmock.AnyArg(), typedMentionDepCreator).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := pg.AddDependency(context.Background(), dep); err != nil {
		t.Fatalf("AddDependency: %v", err)
	}
}

func TestCovPGW_RemoveDependency(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectExec("DELETE FROM deps").
		WithArgs("gd-a", "gd-b", "blocks").
		WillReturnResult(sqlmock.NewResult(0, 1))

	removed, err := pg.RemoveDependency(context.Background(), "gd-a", "gd-b", model.DepBlocks)
	if err != nil {
		t.Fatalf("RemoveDependency: %v", err)
	}
	if !removed {
		t.Error("removed = false; want true when the delete affected 1 row")
	}
}

func TestCovPGW_GetDependencies(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}).
		AddRow("gd-a", "gd-b", "blocks", now, nil, nil)
	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id = \\$1").WithArgs("gd-a").WillReturnRows(rows)

	deps, err := pg.GetDependencies(context.Background(), "gd-a")
	if err != nil {
		t.Fatalf("GetDependencies: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != "gd-b" {
		t.Fatalf("unexpected deps: %+v", deps)
	}
}

func TestCovPGW_AddLabel(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectExec("INSERT INTO labels").WithArgs("gd-a", "urgent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := pg.AddLabel(context.Background(), "gd-a", "urgent"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}
}

func TestCovPGW_RemoveLabel(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	mock.ExpectExec("DELETE FROM labels").WithArgs("gd-a", "urgent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	removed, err := pg.RemoveLabel(context.Background(), "gd-a", "urgent")
	if err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if !removed {
		t.Error("removed = false; want true when the delete affected 1 row")
	}
}

func TestCovPGW_GetLabels(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}

	rows := sqlmock.NewRows([]string{"label"}).AddRow("urgent")
	mock.ExpectQuery("SELECT label FROM labels WHERE bead_id = \\$1").WithArgs("gd-a").WillReturnRows(rows)

	labels, err := pg.GetLabels(context.Background(), "gd-a")
	if err != nil {
		t.Fatalf("GetLabels: %v", err)
	}
	if len(labels) != 1 || labels[0] != "urgent" {
		t.Fatalf("want [urgent], got %v", labels)
	}
}

func TestCovPGW_GetComments(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at", "operated_by", "operated_by_display", "operated_by_email", "impersonated_by"}).
		AddRow(int64(1), "gd-a", "alice", "Hi", now, nil, "", "", "")
	mock.ExpectQuery(`SELECT .+ FROM comments .+WHERE comments\.bead_id = \$1`).WithArgs("gd-a").WillReturnRows(rows)

	comments, err := pg.GetComments(context.Background(), "gd-a")
	if err != nil {
		t.Fatalf("GetComments: %v", err)
	}
	if len(comments) != 1 || comments[0].Author != "alice" {
		t.Fatalf("unexpected comments: %+v", comments)
	}
}

func TestCovPGW_DeleteComment(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	mock.ExpectQuery(`DELETE FROM comments\s+WHERE id = \$1 AND bead_id = \$2`).
		WithArgs(int64(7), "gd-a").
		WillReturnRows(sqlmock.NewRows([]string{"id", "bead_id", "author", "text", "created_at"}).
			AddRow(int64(7), "gd-a", "alice", "old", now))

	got, err := pg.DeleteComment(context.Background(), "gd-a", 7)
	if err != nil {
		t.Fatalf("DeleteComment: %v", err)
	}
	if got == nil || got.ID != 7 {
		t.Fatalf("unexpected comment: %+v", got)
	}
}

func TestCovPGW_RecordEvent(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()
	event := &model.Event{
		Topic: "beads.bead.created", BeadID: "gd-a", Actor: "alice",
		Payload: json.RawMessage(`{}`),
	}
	mock.ExpectQuery("INSERT INTO events").
		WithArgs("beads.bead.created", "gd-a", "alice", []byte(`{}`), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(1, now))

	if err := pg.RecordEvent(context.Background(), event); err != nil {
		t.Fatalf("RecordEvent: %v", err)
	}
	if event.ID != 1 {
		t.Errorf("event.ID = %d, want 1", event.ID)
	}
}

func TestCovPGW_GetEvents(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(1, "beads.bead.created", "gd-a", "alice", []byte(`{}`), now, "")
	mock.ExpectQuery(`SELECT .+ FROM events .+WHERE events\.bead_id = \$1`).WithArgs("gd-a").WillReturnRows(rows)

	evts, err := pg.GetEvents(context.Background(), "gd-a")
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(evts) != 1 || evts[0].Actor != "alice" {
		t.Fatalf("unexpected events: %+v", evts)
	}
}

func TestCovPGW_GetRecentEvents(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(2, "beads.bead.updated", "gd-a", "bob", []byte(`{}`), now, "")
	mock.ExpectQuery(`SELECT .+ FROM \(\s*SELECT .+ FROM events .+WHERE events\.bead_id = \$1\s+ORDER BY events\.id DESC\s+LIMIT \$2\s*\) AS recent\s+ORDER BY created_at ASC`).
		WithArgs("gd-a", 5).WillReturnRows(rows)

	evts, err := pg.GetRecentEvents(context.Background(), "gd-a", 5)
	if err != nil {
		t.Fatalf("GetRecentEvents: %v", err)
	}
	if len(evts) != 1 || evts[0].ID != 2 {
		t.Fatalf("unexpected events: %+v", evts)
	}
}

func TestCovPGW_EventHistogram(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	since := time.Now().UTC().Add(-time.Hour)
	until := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"bucket", "count"}).AddRow(since, 4)
	mock.ExpectQuery(`SELECT date_trunc\(\$1, created_at\) AS bucket, count\(\*\)\s+FROM events`).
		WithArgs("hour", since, until).
		WillReturnRows(rows)

	out, err := pg.EventHistogram(context.Background(), since, until, "hour", model.EventReadScope{})
	if err != nil {
		t.Fatalf("EventHistogram: %v", err)
	}
	if len(out) != 1 || out[0].Count != 4 {
		t.Fatalf("unexpected histogram: %+v", out)
	}
}

func TestCovPGW_EventHistogram_InvalidBucket(t *testing.T) {
	db, _ := newMockDB(t)
	pg := &PostgresStore{db: db}

	if _, err := pg.EventHistogram(context.Background(), time.Now(), time.Now(), "century", model.EventReadScope{}); err == nil {
		t.Fatal("expected error for invalid bucket")
	}
}

func TestCovPGW_GetEventsByTimeRange(t *testing.T) {
	db, mock := newMockDB(t)
	pg := &PostgresStore{db: db}
	since := time.Now().UTC().Add(-time.Hour)
	until := time.Now().UTC()

	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(1, "beads.bead.created", "gd-a", "alice", []byte(`{}`), since, "")
	mock.ExpectQuery(`SELECT .+ FROM events .+WHERE events\.created_at >= \$1 AND events\.created_at <= \$2`).
		WithArgs(since, until, int64(0), 10).
		WillReturnRows(rows)

	evts, err := pg.GetEventsByTimeRange(context.Background(), since, until, 0, 10, model.EventReadScope{})
	if err != nil {
		t.Fatalf("GetEventsByTimeRange: %v", err)
	}
	if len(evts) != 1 || evts[0].ID != 1 {
		t.Fatalf("unexpected events: %+v", evts)
	}
}
