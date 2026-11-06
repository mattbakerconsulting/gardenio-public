package postgres

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// beads.project (migration 057) follows the project: label: adding or
// removing one rewrites the column from the labels table; other labels never
// touch the beads row.

const syncProjectPat = `UPDATE beads SET project = \(SELECT CASE WHEN count\(\*\) = 1 THEN substr\(min\(labels\.label\), 9\) END\s+FROM labels WHERE labels\.bead_id = beads\.id AND labels\.label LIKE 'project:_%'\)\s+WHERE id = \$1 AND project IS DISTINCT FROM`

func TestQueryAddLabel_ProjectLabelSyncsProject(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("INSERT INTO labels").WithArgs("gd-a", "project:gardenio").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(syncProjectPat).WithArgs("gd-a").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryAddLabel(context.Background(), db, "gd-a", "project:gardenio"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryRemoveLabel_ProjectLabelSyncsProject(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM labels").WithArgs("gd-a", "project:gardenio").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(syncProjectPat).WithArgs("gd-a").
		WillReturnResult(sqlmock.NewResult(0, 1))

	n, err := queryRemoveLabel(context.Background(), db, "gd-a", "project:gardenio")
	if err != nil || n != 1 {
		t.Fatalf("queryRemoveLabel = %d, %v; want 1, nil", n, err)
	}
}

// A remove that matched nothing changed no label, so the column is left alone.
func TestQueryRemoveLabel_ProjectNoMatchSkipsSync(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM labels").WithArgs("gd-a", "project:gardenio").
		WillReturnResult(sqlmock.NewResult(0, 0))

	if n, err := queryRemoveLabel(context.Background(), db, "gd-a", "project:gardenio"); err != nil || n != 0 {
		t.Fatalf("queryRemoveLabel = %d, %v; want 0, nil", n, err)
	}
}

// PostgresStore.AddLabel runs the label insert and the project sync in one
// transaction for a project: label, and as one bare statement otherwise
// (gd-pTtoEKamiI).
func TestPostgresStoreAddLabel_ProjectLabelInTransaction(t *testing.T) {
	db, mock := newMockDB(t)
	st := &PostgresStore{db: db}
	mock.ExpectBegin()
	mock.ExpectExec("INSERT INTO labels").WithArgs("gd-a", "project:gardenio").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(syncProjectPat).WithArgs("gd-a").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := st.AddLabel(context.Background(), "gd-a", "project:gardenio"); err != nil {
		t.Fatalf("AddLabel: %v", err)
	}

	mock.ExpectExec("INSERT INTO labels").WithArgs("gd-a", "urgent").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := st.AddLabel(context.Background(), "gd-a", "urgent"); err != nil {
		t.Fatalf("AddLabel non-project: %v", err)
	}
}

func TestIsProjectLabel(t *testing.T) {
	for label, want := range map[string]bool{
		"project:gardenio": true,
		"project:":         true, // the SQL's LIKE 'project:_%' skips the empty value
		"projects:x":       false,
		"area:project":     false,
		"":                 false,
	} {
		if got := isProjectLabel(label); got != want {
			t.Errorf("isProjectLabel(%q) = %v; want %v", label, got, want)
		}
	}
}
