package postgres

import (
	"context"
	"database/sql"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestQueryGetDependencyEdgesByType(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{"bead_id", "depends_on_id"}).
		AddRow("gd-a", "gd-b").
		AddRow("gd-b", "gd-a")

	mock.ExpectQuery("SELECT .+ FROM deps").
		WithArgs("blocks").
		WillReturnRows(rows)

	edges, err := queryGetDependencyEdgesByType(context.Background(), db, model.DepBlocks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(edges))
	}
	if edges[0].BeadID != "gd-a" || edges[0].DependsOnID != "gd-b" {
		t.Errorf("edge[0] = %+v; want {gd-a gd-b}", edges[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestQueryGetDependencyEdgesByTypeQueryError(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .+ FROM deps").
		WithArgs("blocks").
		WillReturnError(sql.ErrConnDone)

	if _, err := queryGetDependencyEdgesByType(context.Background(), db, model.DepBlocks); err == nil {
		t.Fatal("expected error")
	}
}
