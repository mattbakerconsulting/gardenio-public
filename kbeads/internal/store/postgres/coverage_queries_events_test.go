package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"gardenio/kbeads/internal/model"
)

// These tests exercise four previously-zero-coverage query helpers in
// queries.go: queryFindAgentsByNames, queryEventHistogram,
// queryGetEventsByTimeRange, and queryGetEventsByTimeRangeDesc. They follow
// the existing sqlmock-based style and reuse the shared newMockDB helper plus
// beadRowColumns (the JOINed scanBeadFull projection).

// --- queryFindAgentsByNames ---

func TestCovQEvFindAgentsByNames_Found(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows(beadRowColumns).
		AddRow(
			"gd-agent1", nil, nil, nil, "issue", "agent", "Agent One", nil, nil,
			"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
			nil,
			"", "", "",
		).
		AddRow(
			"gd-agent2", nil, nil, nil, "issue", "agent", "Agent Two", nil, nil,
			"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
			nil,
			"", "", "",
		)
	mock.ExpectQuery(`SELECT DISTINCT ON \(beads\.fields->>'agent'\) .+ FROM beads .+ beads\.fields->>'agent' = ANY\(\$1\)`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(rows)

	out, err := queryFindAgentsByNames(context.Background(), db, []string{"one", "two"}, nil)
	if err != nil {
		t.Fatalf("queryFindAgentsByNames: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 beads, got %d", len(out))
	}
	if out[0].ID != "gd-agent1" || out[1].ID != "gd-agent2" {
		t.Fatalf("got ids %q, %q", out[0].ID, out[1].ID)
	}
	if string(out[0].Type) != "agent" {
		t.Fatalf("got type %q, want agent", out[0].Type)
	}
}

func TestCovQEvFindAgentsByNames_WithStatuses(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	// statuses appended as $2 placeholders after the names array.
	mock.ExpectQuery(`beads\.status IN \(\$2\)`).
		WithArgs(sqlmock.AnyArg(), "open").
		WillReturnRows(sqlmock.NewRows(beadRowColumns).AddRow(
			"gd-agentS", nil, nil, nil, "issue", "agent", "Agent S", nil, nil,
			"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
			nil,
			"", "", "",
		))

	out, err := queryFindAgentsByNames(context.Background(), db, []string{"s"}, []model.Status{model.StatusOpen})
	if err != nil {
		t.Fatalf("queryFindAgentsByNames: %v", err)
	}
	if len(out) != 1 || out[0].ID != "gd-agentS" {
		t.Fatalf("unexpected result: %+v", out)
	}
}

func TestCovQEvFindAgentsByNames_EmptyNamesShortCircuits(t *testing.T) {
	db, _ := newMockDB(t)

	out, err := queryFindAgentsByNames(context.Background(), db, nil, nil)
	if err != nil {
		t.Fatalf("expected nil error for empty names, got %v", err)
	}
	if out != nil {
		t.Fatalf("expected nil result for empty names, got %+v", out)
	}
}

func TestCovQEvFindAgentsByNames_QueryError(t *testing.T) {
	db, mock := newMockDB(t)

	mock.ExpectQuery(`SELECT DISTINCT ON`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnError(errors.New("boom"))

	if _, err := queryFindAgentsByNames(context.Background(), db, []string{"x"}, nil); err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}

// --- queryEventHistogram ---

func TestCovQEvEventHistogram_Found(t *testing.T) {
	db, mock := newMockDB(t)
	since := time.Now().UTC().Add(-time.Hour)
	until := time.Now().UTC()
	b1 := since.Truncate(time.Hour)
	b2 := b1.Add(time.Hour)

	mock.ExpectQuery(`SELECT date_trunc\(\$1, created_at\) AS bucket, count\(\*\)\s+FROM events`).
		WithArgs("hour", since, until).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "count"}).
			AddRow(b1, int64(3)).
			AddRow(b2, int64(7)))

	out, err := queryEventHistogram(context.Background(), db, since, until, "hour", model.EventReadScope{})
	if err != nil {
		t.Fatalf("queryEventHistogram: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 buckets, got %d", len(out))
	}
	if out[0].Count != 3 || out[1].Count != 7 {
		t.Fatalf("got counts %d, %d", out[0].Count, out[1].Count)
	}
	if !out[0].T.Equal(b1) || !out[1].T.Equal(b2) {
		t.Fatalf("got bucket times %v, %v", out[0].T, out[1].T)
	}
}

func TestCovQEvEventHistogram_InvalidBucket(t *testing.T) {
	db, _ := newMockDB(t)

	_, err := queryEventHistogram(context.Background(), db, time.Now(), time.Now(), "century", model.EventReadScope{})
	if err == nil {
		t.Fatal("expected error for invalid bucket, got nil")
	}
}

func TestCovQEvEventHistogram_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	since := time.Now().UTC().Add(-time.Hour)
	until := time.Now().UTC()

	mock.ExpectQuery(`FROM events`).
		WithArgs("day", since, until).
		WillReturnError(errors.New("boom"))

	if _, err := queryEventHistogram(context.Background(), db, since, until, "day", model.EventReadScope{}); err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}

// --- queryGetEventsByTimeRange (oldest-first) ---

func TestCovQEvGetEventsByTimeRange_Found(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(int64(10), "beads.bead.created", "gd-a", "alice", []byte(`{}`), now, "oidc:matt@x.ai").
		AddRow(int64(11), "beads.bead.updated", "gd-a", nil, []byte(`{}`), now.Add(time.Minute), "")
	mock.ExpectQuery(`FROM events\s+LEFT JOIN operators .+ events\.created_at >= \$1 AND events\.created_at <= \$2\s+AND \(\$3 = 0 OR \(events\.created_at, events\.id\) >\s+\(\(SELECT c\.created_at FROM events c WHERE c\.id = \$3\), \$3\)\)\s+ORDER BY events\.created_at ASC, events\.id ASC\s+LIMIT \$4`).
		WithArgs(since, now, int64(0), 50).
		WillReturnRows(rows)

	out, err := queryGetEventsByTimeRange(context.Background(), db, since, now, 0, 50, model.EventReadScope{})
	if err != nil {
		t.Fatalf("queryGetEventsByTimeRange: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 events, got %d", len(out))
	}
	if out[0].ID != 10 || out[1].ID != 11 {
		t.Fatalf("got ids %d, %d", out[0].ID, out[1].ID)
	}
	if out[0].OperatedBy != "oidc:matt@x.ai" || out[1].OperatedBy != "" {
		t.Fatalf("got operated_by %q, %q", out[0].OperatedBy, out[1].OperatedBy)
	}
}

func TestCovQEvGetEventsByTimeRange_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	mock.ExpectQuery(`ORDER BY events\.created_at ASC, events\.id ASC`).
		WithArgs(since, now, int64(5), 25).
		WillReturnError(errors.New("boom"))

	if _, err := queryGetEventsByTimeRange(context.Background(), db, since, now, 5, 25, model.EventReadScope{}); err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}

// --- queryGetEventsByTimeRangeDesc (newest-first) ---

func TestCovQEvGetEventsByTimeRangeDesc_Found(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(int64(20), "beads.bead.updated", "gd-b", "bob", []byte(`{}`), now, "").
		AddRow(int64(19), "beads.bead.created", "gd-b", "carol", []byte(`{}`), now.Add(-time.Minute), "")
	mock.ExpectQuery(`AND \(\$3 = 0 OR \(events\.created_at, events\.id\) <\s+\(\(SELECT c\.created_at FROM events c WHERE c\.id = \$3\), \$3\)\)\s+ORDER BY events\.created_at DESC, events\.id DESC\s+LIMIT \$4`).
		WithArgs(since, now, int64(0), 50).
		WillReturnRows(rows)

	out, err := queryGetEventsByTimeRangeDesc(context.Background(), db, since, now, 0, 50, model.EventReadScope{})
	if err != nil {
		t.Fatalf("queryGetEventsByTimeRangeDesc: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 events, got %d", len(out))
	}
	if out[0].ID != 20 || out[1].ID != 19 {
		t.Fatalf("got ids %d, %d (expected newest-first)", out[0].ID, out[1].ID)
	}
}

func TestCovQEvGetEventsByTimeRangeDesc_WithCursor(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	mock.ExpectQuery(`\(events\.created_at, events\.id\) <`).
		WithArgs(since, now, int64(19), 10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
			AddRow(int64(18), "beads.bead.created", "gd-b", "carol", []byte(`{}`), now.Add(-2*time.Minute), ""))

	out, err := queryGetEventsByTimeRangeDesc(context.Background(), db, since, now, 19, 10, model.EventReadScope{})
	if err != nil {
		t.Fatalf("queryGetEventsByTimeRangeDesc: %v", err)
	}
	if len(out) != 1 || out[0].ID != 18 {
		t.Fatalf("unexpected result: %+v", out)
	}
}

// --- EventReadScope: the SQL-level cross-project gate (ka-0000006863) ---

// TestCovQEv_GetEventsByTimeRange_ScopeSQL asserts a scoped caller emits the
// labels EXISTS predicates (project gate) with the fixed args followed by the
// scope args, and that the query still returns rows.
func TestCovQEv_GetEventsByTimeRange_ScopeSQL(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
		AddRow(int64(10), "beads.bead.created", "gd-a", "alice", []byte(`{}`), now, "")
	// RequireAll appends one text arg; RequireAny appends a text[] arg. The
	// EXISTS sub-queries gate events.bead_id through the labels table.
	mock.ExpectQuery(`EXISTS \(SELECT 1 FROM labels WHERE labels\.bead_id = events\.bead_id AND labels\.label = \$5\).+EXISTS \(SELECT 1 FROM labels WHERE labels\.bead_id = events\.bead_id AND labels\.label = ANY\(\$6::text\[\]\)\)`).
		WithArgs(since, now, int64(0), 50, "project:projA", pq.Array([]string{"project:projB"})).
		WillReturnRows(rows)

	scope := model.EventReadScope{
		RequireAllLabels: []string{"project:projA"},
		RequireAnyLabels: []string{"project:projB"},
	}
	out, err := queryGetEventsByTimeRange(context.Background(), db, since, now, 0, 50, scope)
	if err != nil {
		t.Fatalf("queryGetEventsByTimeRange scoped: %v", err)
	}
	if len(out) != 1 || out[0].ID != 10 {
		t.Fatalf("scoped query: got %+v, want one event id=10", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scope SQL/args mismatch: %v", err)
	}
}

// TestCovQEv_EventHistogram_ScopeSQL asserts the histogram aggregation applies
// the same labels EXISTS gate before the GROUP BY (post-filtering counts is
// impossible).
func TestCovQEv_EventHistogram_ScopeSQL(t *testing.T) {
	db, mock := newMockDB(t)
	since := time.Now().UTC().Add(-time.Hour)
	until := time.Now().UTC()
	b1 := since.Truncate(time.Hour)

	mock.ExpectQuery(`FROM events\s+WHERE created_at >= \$2 AND created_at <= \$3 AND EXISTS \(SELECT 1 FROM labels WHERE labels\.bead_id = events\.bead_id AND labels\.label = ANY\(\$4::text\[\]\)\)\s+GROUP BY bucket`).
		WithArgs("hour", since, until, pq.Array([]string{"project:projA"})).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "count"}).AddRow(b1, int64(2)))

	scope := model.EventReadScope{RequireAnyLabels: []string{"project:projA"}}
	out, err := queryEventHistogram(context.Background(), db, since, until, "hour", scope)
	if err != nil {
		t.Fatalf("queryEventHistogram scoped: %v", err)
	}
	if len(out) != 1 || out[0].Count != 2 {
		t.Fatalf("scoped histogram: got %+v, want one bucket count=2", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("scope SQL/args mismatch: %v", err)
	}
}

func TestCovQEvGetEventsByTimeRangeDesc_QueryError(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	mock.ExpectQuery(`ORDER BY events\.created_at DESC, events\.id DESC`).
		WithArgs(since, now, int64(0), 50).
		WillReturnError(sql.ErrConnDone)

	if _, err := queryGetEventsByTimeRangeDesc(context.Background(), db, since, now, 0, 50, model.EventReadScope{}); err == nil {
		t.Fatal("expected error from query failure, got nil")
	}
}

// ExcludeBeadTypes composes a NOT EXISTS predicate on the bead's type after
// the fixed args (gd-Mfr8MRXbYG), for both orders.
func TestCovQEvGetEventsByTimeRange_ExcludeBeadTypes(t *testing.T) {
	for name, run := range map[string]func(db *sql.DB, since, now time.Time) ([]*model.Event, error){
		"asc": func(db *sql.DB, since, now time.Time) ([]*model.Event, error) {
			return queryGetEventsByTimeRange(context.Background(), db, since, now, 0, 50, model.EventReadScope{ExcludeBeadTypes: []string{"agent", "trigger"}})
		},
		"desc": func(db *sql.DB, since, now time.Time) ([]*model.Event, error) {
			return queryGetEventsByTimeRangeDesc(context.Background(), db, since, now, 0, 50, model.EventReadScope{ExcludeBeadTypes: []string{"agent", "trigger"}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			db, mock := newMockDB(t)
			now := time.Now().UTC()
			since := now.Add(-time.Hour)
			rows := sqlmock.NewRows([]string{"id", "topic", "bead_id", "actor", "payload", "created_at", "operated_by"}).
				AddRow(int64(10), "beads.bead.updated", "gd-a", "alice", []byte(`{}`), now, "")
			mock.ExpectQuery(`AND NOT EXISTS \(SELECT 1 FROM beads xb WHERE xb\.id = events\.bead_id AND xb\.type = ANY\(\$5::text\[\]\)\)\s+ORDER BY`).
				WithArgs(since, now, int64(0), 50, sqlmock.AnyArg()).
				WillReturnRows(rows)
			out, err := run(db, since, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(out) != 1 {
				t.Fatalf("expected 1 event, got %d", len(out))
			}
		})
	}
}
