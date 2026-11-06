package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

// These tests cover the previously zero-coverage functions in ui_telemetry.go:
// the two pure helpers (uiTelemetryEffectiveLimit, appendWhere), the two query
// helpers (queryRecordUITelemetry, queryListUITelemetry), and the public
// PostgresStore + txStore wrappers around them. They follow the sqlmock-based
// style of postgres_test.go / dibs_test.go.

func TestCovUITelEffectiveLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  int
		want int
	}{
		{"zero", 0, uiTelemetryHardLimit},
		{"negative", -5, uiTelemetryHardLimit},
		{"overHardLimit", uiTelemetryHardLimit + 1, uiTelemetryHardLimit},
		{"atHardLimit", uiTelemetryHardLimit, uiTelemetryHardLimit},
		{"inRange", 50, 50},
		{"one", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := uiTelemetryEffectiveLimit(tc.req); got != tc.want {
				t.Errorf("uiTelemetryEffectiveLimit(%d) = %d, want %d", tc.req, got, tc.want)
			}
		})
	}
}

func TestCovUITelAppendWhere(t *testing.T) {
	// Empty prev -> " WHERE x".
	if got := appendWhere("", "event = $2"); got != " WHERE event = $2" {
		t.Errorf("appendWhere(\"\", ...) = %q, want %q", got, " WHERE event = $2")
	}
	// Non-empty prev -> " AND " join, preserving prev.
	prev := " WHERE event = $2"
	if got := appendWhere(prev, "created_at >= $3"); got != " WHERE event = $2 AND created_at >= $3" {
		t.Errorf("appendWhere(prev, ...) = %q, want %q", got, " WHERE event = $2 AND created_at >= $3")
	}
}

// uiTelemetryColumns is the SELECT projection for queryListUITelemetry.
var covUITelColumns = []string{
	"id", "event", "target", "from_path", "path", "operator_id", "metadata", "created_at",
}

func TestCovUITelRecord_SetsIDAndCreatedAt(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()

	row := &model.UITelemetryRow{
		Event:      "click",
		Target:     "tile",
		From:       "/a",
		Path:       "/b",
		OperatorID: "oidc:matt@x.ai",
		Metadata:   []byte(`{"k":"v"}`),
	}
	mock.ExpectQuery(`INSERT INTO ui_telemetry`).
		WithArgs("click", "tile", "/a", "/b", "oidc:matt@x.ai", []byte(`{"k":"v"}`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(7), now))

	if err := store.RecordUITelemetry(context.Background(), row); err != nil {
		t.Fatalf("RecordUITelemetry: %v", err)
	}
	if row.ID != 7 {
		t.Errorf("row.ID = %d, want 7", row.ID)
	}
	if !row.CreatedAt.Equal(now) {
		t.Errorf("row.CreatedAt = %v, want %v", row.CreatedAt, now)
	}
}

func TestCovUITelRecord_EmptyMetadataDefaultsToObject(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()

	// Empty Metadata -> "{}" default, empty OperatorID -> NULL operator_id.
	row := &model.UITelemetryRow{Event: "view", Target: "panel"}
	mock.ExpectQuery(`INSERT INTO ui_telemetry`).
		WithArgs("view", "panel", "", "", nil, []byte("{}")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(11), now))

	if err := store.RecordUITelemetry(context.Background(), row); err != nil {
		t.Fatalf("RecordUITelemetry: %v", err)
	}
	if row.ID != 11 {
		t.Errorf("row.ID = %d, want 11", row.ID)
	}
}

func TestCovUITelRecord_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`INSERT INTO ui_telemetry`).
		WithArgs("click", "", "", "", nil, []byte("{}")).
		WillReturnError(errors.New("boom"))

	row := &model.UITelemetryRow{Event: "click"}
	if err := store.RecordUITelemetry(context.Background(), row); err == nil {
		t.Fatal("RecordUITelemetry: err = nil, want non-nil for query error")
	}
}

func TestCovUITelRecord_TxStore(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO ui_telemetry`).
		WithArgs("click", "", "", "", nil, []byte("{}")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(3), now))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	row := &model.UITelemetryRow{Event: "click"}
	if err := ts.RecordUITelemetry(context.Background(), row); err != nil {
		t.Fatalf("txStore.RecordUITelemetry: %v", err)
	}
	if row.ID != 3 {
		t.Errorf("row.ID = %d, want 3", row.ID)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

func TestCovUITelList_NoFilter(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()

	// No filter -> only the LIMIT arg ($1 = hard limit). No WHERE clause.
	mock.ExpectQuery(`SELECT .+ FROM ui_telemetry ORDER BY id DESC LIMIT \$1`).
		WithArgs(uiTelemetryHardLimit).
		WillReturnRows(sqlmock.NewRows(covUITelColumns).
			AddRow(int64(2), "click", "tile", "/from", "/path", "oidc:a@x.ai", []byte(`{"x":1}`), now).
			AddRow(int64(1), "view", "panel", "", "", nil, nil, now))

	out, err := store.ListUITelemetry(context.Background(), model.UITelemetryFilter{})
	if err != nil {
		t.Fatalf("ListUITelemetry: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 rows, got %d", len(out))
	}
	if out[0].ID != 2 || out[0].OperatorID != "oidc:a@x.ai" || string(out[0].Metadata) != `{"x":1}` {
		t.Errorf("row0 = %+v", out[0])
	}
	// Nullable operator_id and NULL metadata collapse to zero values.
	if out[1].OperatorID != "" {
		t.Errorf("row1 OperatorID = %q, want empty for NULL", out[1].OperatorID)
	}
	if out[1].Metadata != nil {
		t.Errorf("row1 Metadata = %v, want nil for NULL", out[1].Metadata)
	}
}

func TestCovUITelList_EventFilter(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()

	// Event filter appends event = $2; args are [limit, event].
	mock.ExpectQuery(`SELECT .+ FROM ui_telemetry WHERE event = \$2 ORDER BY id DESC LIMIT \$1`).
		WithArgs(uiTelemetryHardLimit, "click").
		WillReturnRows(sqlmock.NewRows(covUITelColumns).
			AddRow(int64(5), "click", "tile", "", "", nil, []byte("{}"), now))

	out, err := store.ListUITelemetry(context.Background(), model.UITelemetryFilter{Event: "click"})
	if err != nil {
		t.Fatalf("ListUITelemetry: %v", err)
	}
	if len(out) != 1 || out[0].Event != "click" {
		t.Fatalf("unexpected rows: %+v", out)
	}
}

func TestCovUITelList_SinceFilter(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	// Since filter appends created_at >= $2; args are [limit, since].
	mock.ExpectQuery(`SELECT .+ FROM ui_telemetry WHERE created_at >= \$2 ORDER BY id DESC LIMIT \$1`).
		WithArgs(uiTelemetryHardLimit, since).
		WillReturnRows(sqlmock.NewRows(covUITelColumns).
			AddRow(int64(8), "view", "", "", "", nil, nil, now))

	out, err := store.ListUITelemetry(context.Background(), model.UITelemetryFilter{Since: since})
	if err != nil {
		t.Fatalf("ListUITelemetry: %v", err)
	}
	if len(out) != 1 || out[0].ID != 8 {
		t.Fatalf("unexpected rows: %+v", out)
	}
}

func TestCovUITelList_BothFilters(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}
	now := time.Now().UTC()
	since := now.Add(-2 * time.Hour)

	// Both filters -> event = $2 AND created_at >= $3; custom Limit honored.
	mock.ExpectQuery(`SELECT .+ FROM ui_telemetry WHERE event = \$2 AND created_at >= \$3 ORDER BY id DESC LIMIT \$1`).
		WithArgs(25, "click", since).
		WillReturnRows(sqlmock.NewRows(covUITelColumns).
			AddRow(int64(9), "click", "tile", "/f", "/p", "oidc:b@x.ai", []byte(`{"y":2}`), now))

	out, err := store.ListUITelemetry(context.Background(), model.UITelemetryFilter{
		Event: "click",
		Since: since,
		Limit: 25,
	})
	if err != nil {
		t.Fatalf("ListUITelemetry: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	if out[0].OperatorID != "oidc:b@x.ai" || string(out[0].Metadata) != `{"y":2}` {
		t.Errorf("row = %+v", out[0])
	}
}

func TestCovUITelList_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`SELECT .+ FROM ui_telemetry`).
		WithArgs(uiTelemetryHardLimit).
		WillReturnError(errors.New("boom"))

	if _, err := store.ListUITelemetry(context.Background(), model.UITelemetryFilter{}); err == nil {
		t.Fatal("ListUITelemetry: err = nil, want non-nil for query error")
	}
}

func TestCovUITelList_TxStore(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .+ FROM ui_telemetry ORDER BY id DESC LIMIT \$1`).
		WithArgs(uiTelemetryHardLimit).
		WillReturnRows(sqlmock.NewRows(covUITelColumns).
			AddRow(int64(1), "click", "", "", "", nil, nil, now))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	ts := &txStore{tx: tx}

	out, err := ts.ListUITelemetry(context.Background(), model.UITelemetryFilter{})
	if err != nil {
		t.Fatalf("txStore.ListUITelemetry: %v", err)
	}
	if len(out) != 1 || out[0].ID != 1 {
		t.Fatalf("unexpected rows: %+v", out)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}
