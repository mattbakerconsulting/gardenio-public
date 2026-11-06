package postgres

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var depColumns = []string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata"}

// revDepColumns mirrors queryGetReverseDependenciesForBeads' SELECT, which
// carries the dependent (child) bead's type as a trailing column so callers can
// filter children by type before hydration (ka-0000003113).
var revDepColumns = []string{"bead_id", "depends_on_id", "type", "created_at", "created_by", "metadata", "child_type"}

func TestQueryGetReverseDependenciesForBeadsEmpty(t *testing.T) {
	result, err := queryGetReverseDependenciesForBeads(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestQueryGetReverseDependenciesForBeads(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(revDepColumns).
		AddRow("child-1", "parent-1", "blocks", now, nil, nil, "task").
		AddRow("child-2", "parent-1", "blocks", now, nil, nil, nil)

	mock.ExpectQuery("SELECT .+ FROM deps d LEFT JOIN beads child .+ WHERE d.depends_on_id").
		WillReturnRows(rows)

	result, err := queryGetReverseDependenciesForBeads(context.Background(), db, []string{"parent-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	deps := result["parent-1"]
	if len(deps) != 2 {
		t.Fatalf("expected 2 reverse deps for parent-1, got %d", len(deps))
	}
	if deps[0].BeadID != "child-1" {
		t.Errorf("dep[0].BeadID = %q, want child-1", deps[0].BeadID)
	}
	// The joined child type flows onto BeadType; a NULL child type (dangling /
	// soft-deleted child) scans to "".
	if deps[0].BeadType != "task" {
		t.Errorf("dep[0].BeadType = %q, want task", deps[0].BeadType)
	}
	if deps[1].BeadType != "" {
		t.Errorf("dep[1].BeadType = %q, want empty (NULL child type)", deps[1].BeadType)
	}
}

func TestQueryGetReverseDependenciesForBeadsQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+ FROM deps d LEFT JOIN beads child .+ WHERE d.depends_on_id").
		WillReturnError(sql.ErrConnDone)

	_, err := queryGetReverseDependenciesForBeads(context.Background(), db, []string{"parent-1"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestQueryGetDependenciesForBeadsEmptyInput(t *testing.T) {
	result, err := queryGetDependenciesForBeads(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestQueryGetDependenciesForBeadsHappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(depColumns).
		AddRow("child-1", "parent-1", "blocks", now, "user1", nil)

	mock.ExpectQuery("SELECT .+ FROM deps WHERE bead_id").
		WillReturnRows(rows)

	result, err := queryGetDependenciesForBeads(context.Background(), db, []string{"child-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result["child-1"]) != 1 {
		t.Errorf("expected 1 dep for child-1, got %d", len(result["child-1"]))
	}
}

func TestQueryGetDependencyCountsEmptyInput(t *testing.T) {
	result, err := queryGetDependencyCounts(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestQueryGetDependencyCountsForwardAndReverse(t *testing.T) {
	db, mock := newMockDB(t)

	// Forward counts.
	fwdRows := sqlmock.NewRows([]string{"bead_id", "count"}).
		AddRow("bead-1", 3)
	mock.ExpectQuery("SELECT bead_id, COUNT.*FROM deps.*WHERE bead_id").
		WillReturnRows(fwdRows)

	// Reverse counts — bead-1 already exists in result, bead-2 is new.
	revRows := sqlmock.NewRows([]string{"depends_on_id", "count"}).
		AddRow("bead-1", 2).
		AddRow("bead-2", 5)
	mock.ExpectQuery("SELECT depends_on_id, COUNT.*FROM deps.*WHERE depends_on_id").
		WillReturnRows(revRows)

	result, err := queryGetDependencyCounts(context.Background(), db, []string{"bead-1", "bead-2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if dc := result["bead-1"]; dc == nil {
		t.Fatal("expected counts for bead-1")
	} else {
		if dc.DependencyCount != 3 {
			t.Errorf("bead-1 DependencyCount = %d, want 3", dc.DependencyCount)
		}
		if dc.DependentCount != 2 {
			t.Errorf("bead-1 DependentCount = %d, want 2", dc.DependentCount)
		}
	}

	// bead-2 only has reverse deps.
	if dc := result["bead-2"]; dc == nil {
		t.Fatal("expected counts for bead-2")
	} else {
		if dc.DependencyCount != 0 {
			t.Errorf("bead-2 DependencyCount = %d, want 0", dc.DependencyCount)
		}
		if dc.DependentCount != 5 {
			t.Errorf("bead-2 DependentCount = %d, want 5", dc.DependentCount)
		}
	}
}

func TestQueryGetChildrenStatusHistogramEmptyInput(t *testing.T) {
	result, err := queryGetChildrenStatusHistogram(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestQueryGetChildrenStatusHistogramHappyPath(t *testing.T) {
	db, mock := newMockDB(t)

	// Two parents. parent-1 has open=2, in_progress=1, closed=3.
	// parent-2 has open=1.
	rows := sqlmock.NewRows([]string{"depends_on_id", "status", "count"}).
		AddRow("parent-1", "open", 2).
		AddRow("parent-1", "in_progress", 1).
		AddRow("parent-1", "closed", 3).
		AddRow("parent-2", "open", 1)

	mock.ExpectQuery(`SELECT d\.depends_on_id, b\.status, COUNT.*FROM deps d.*JOIN beads b.*WHERE d\.depends_on_id = ANY.*AND d\.type = 'parent-child'.*AND b\.deleted_at IS NULL`).
		WillReturnRows(rows)

	result, err := queryGetChildrenStatusHistogram(context.Background(), db, []string{"parent-1", "parent-2", "parent-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	p1, ok := result["parent-1"]
	if !ok {
		t.Fatal("expected parent-1 in result")
	}
	if p1["open"] != 2 || p1["in_progress"] != 1 || p1["closed"] != 3 {
		t.Errorf("parent-1 histogram = %v, want {open:2, in_progress:1, closed:3}", p1)
	}

	p2, ok := result["parent-2"]
	if !ok {
		t.Fatal("expected parent-2 in result")
	}
	if p2["open"] != 1 {
		t.Errorf("parent-2 histogram = %v, want {open:1}", p2)
	}

	// parent-3 had no children — must be omitted from the result map per
	// queryGetChildrenStatusHistogram's documented contract.
	if _, ok := result["parent-3"]; ok {
		t.Errorf("parent-3 should be omitted (no children), got %v", result["parent-3"])
	}
}

func TestQueryGetChildrenStatusHistogramQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery(`SELECT d\.depends_on_id, b\.status, COUNT.*FROM deps d`).
		WillReturnError(sql.ErrConnDone)

	_, err := queryGetChildrenStatusHistogram(context.Background(), db, []string{"parent-1"})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestQueryGetLabelsForBeadsEmpty(t *testing.T) {
	result, err := queryGetLabelsForBeads(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestQueryGetBlockedByForBeadsEmpty(t *testing.T) {
	result, err := queryGetBlockedByForBeads(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty map, got %d entries", len(result))
	}
}

func TestQueryGetBeadsByIDsEmptyInput(t *testing.T) {
	beads, err := queryGetBeadsByIDs(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(beads) != 0 {
		t.Errorf("expected empty slice, got %d beads", len(beads))
	}
}

func TestQueryGetBeadsByIDsHappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumns).AddRow(
		"gd-abc", nil, nil, nil, "issue", "task", "test bead", nil, nil,
		"open", 2, nil, nil, now, nil, now,
		nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "",
	)
	mock.ExpectQuery(`SELECT .+ FROM beads .+WHERE beads\.id = ANY`).
		WillReturnRows(rows)
	// Labels are populated in a follow-up batch query (ka-0000000676) so
	// batch fetch matches queryGetBead / queryListBeads.
	labelRows := sqlmock.NewRows([]string{"bead_id", "label"}).
		AddRow("gd-abc", "squawk")
	mock.ExpectQuery("SELECT bead_id, label FROM labels WHERE bead_id = ANY").
		WillReturnRows(labelRows)

	beads, err := queryGetBeadsByIDs(context.Background(), db, []string{"gd-abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(beads) != 1 {
		t.Fatalf("expected 1 bead, got %d", len(beads))
	}
	if beads[0].ID != "gd-abc" {
		t.Errorf("bead ID = %q, want gd-abc", beads[0].ID)
	}
	if len(beads[0].Labels) != 1 || beads[0].Labels[0] != "squawk" {
		t.Errorf("labels = %v, want [squawk]", beads[0].Labels)
	}
}
