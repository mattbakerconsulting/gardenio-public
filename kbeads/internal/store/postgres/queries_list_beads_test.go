package postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"gardenio/kbeads/internal/model"
)

func TestAddIn(t *testing.T) {
	w := &beadListWhere{}
	addIn(w, "status IN", []model.Status{})
	if len(w.clauses) != 0 || w.argIdx != 0 {
		t.Fatalf("empty vals added %v (argIdx %d)", w.clauses, w.argIdx)
	}
	addIn(w, "status IN", []model.Status{"open", "closed"})
	addIn(w, "type NOT IN", []model.BeadType{"agent"})
	wantClauses := []string{"status IN ($1, $2)", "type NOT IN ($3)"}
	if !reflect.DeepEqual(w.clauses, wantClauses) {
		t.Errorf("clauses = %q, want %q", w.clauses, wantClauses)
	}
	wantArgs := []any{"open", "closed", "agent"}
	if !reflect.DeepEqual(w.args, wantArgs) {
		t.Errorf("args = %#v, want %#v (plain strings, not the named types)", w.args, wantArgs)
	}
}

func TestBeadListWhereSQL(t *testing.T) {
	w := &beadListWhere{}
	if got := w.sql(); got != "" {
		t.Errorf("no clauses: sql() = %q, want empty", got)
	}
	w.clauses = []string{"a", "b"}
	if got := w.sql(); got != " WHERE a AND b" {
		t.Errorf("sql() = %q", got)
	}
}

// The default list (no filter at all) still hides soft-deleted beads.
func TestBuildBeadListWhere_EmptyFilter(t *testing.T) {
	w := buildBeadListWhere(model.BeadFilter{})
	if got := w.sql(); got != " WHERE deleted_at IS NULL" {
		t.Errorf("sql() = %q", got)
	}
	if len(w.args) != 0 || w.searchRelevanceExpr != "" {
		t.Errorf("args %v, relevance %q: want none", w.args, w.searchRelevanceExpr)
	}
}

// Placeholders are numbered across steps in call order, so a clause from a
// later step always binds after every earlier step's args.
func TestBuildBeadListWhere_PlaceholdersFollowStepOrder(t *testing.T) {
	prio := 1
	w := buildBeadListWhere(model.BeadFilter{
		Status:   []model.Status{"open"},
		Priority: &prio,
		Labels:   []string{"project:x"},
		ParentID: "gd-1",
		Fields:   map[string]string{"role": "crew"},
		Fleet:    "f1",
	})
	sql := w.sql()
	for i, frag := range []string{"status IN ($1)", "priority = $2", "labels.label = $3", "deps.depends_on_id = $4", "fields ? $5 AND fields->>$5 = $6", "fleet = $7"} {
		if !strings.Contains(sql, frag) {
			t.Errorf("fragment %d %q missing from %q", i, frag, sql)
		}
	}
	want := []any{"open", 1, "project:x", "gd-1", "role", "crew", "f1"}
	if !reflect.DeepEqual(w.args, want) {
		t.Errorf("args = %#v, want %#v", w.args, want)
	}
}

// FieldsIn binds the key once and each value in turn, after the equality
// field filters, keys in sorted order so the SQL text is stable.
func TestBuildBeadListWhere_FieldsIn(t *testing.T) {
	w := buildBeadListWhere(model.BeadFilter{
		Fields:   map[string]string{"kind": "message"},
		FieldsIn: map[string][]string{"thread_id": {"t1"}, "channel_id": {"c1", "c2", "c3"}, "empty": nil},
	})
	sql := w.sql()
	for i, frag := range []string{"fields ? $1 AND fields->>$1 = $2", "fields ? $3 AND fields->>$3 IN ($4, $5, $6)", "fields ? $7 AND fields->>$7 IN ($8)"} {
		if !strings.Contains(sql, frag) {
			t.Errorf("fragment %d %q missing from %q", i, frag, sql)
		}
	}
	want := []any{"kind", "message", "channel_id", "c1", "c2", "c3", "thread_id", "t1"}
	if !reflect.DeepEqual(w.args, want) {
		t.Errorf("args = %#v, want %#v", w.args, want)
	}
}

func TestAddSearchFilter(t *testing.T) {
	t.Run("blank search adds nothing", func(t *testing.T) {
		w := &beadListWhere{}
		w.addSearchFilter(model.BeadFilter{Search: "   "})
		if len(w.clauses) != 0 || len(w.args) != 0 {
			t.Errorf("clauses %v args %v", w.clauses, w.args)
		}
	})
	t.Run("terms then full query, relevance set", func(t *testing.T) {
		w := &beadListWhere{}
		w.addSearchFilter(model.BeadFilter{Search: "break-glass pod"})
		want := []any{"break%glass", "pod", "break-glass pod"}
		if !reflect.DeepEqual(w.args, want) {
			t.Errorf("args = %#v, want %#v", w.args, want)
		}
		if len(w.clauses) != 1 || !strings.Contains(w.clauses[0], "websearch_to_tsquery('english', $3)") {
			t.Errorf("clauses = %q", w.clauses)
		}
		if !strings.Contains(w.searchRelevanceExpr, "title ILIKE $3 || '%'") {
			t.Errorf("relevance = %q", w.searchRelevanceExpr)
		}
	})
	t.Run("count only binds no relevance", func(t *testing.T) {
		w := &beadListWhere{}
		w.addSearchFilter(model.BeadFilter{Search: "pod", CountOnly: true})
		if w.searchRelevanceExpr != "" {
			t.Errorf("relevance = %q, want empty", w.searchRelevanceExpr)
		}
		if len(w.args) != 2 {
			t.Errorf("args = %#v, want term + full query", w.args)
		}
	})
}

func TestSearchRelevanceSQL(t *testing.T) {
	got := searchRelevanceSQL("$3", []string{"$1", "$2"})
	for _, frag := range []string{
		"WHEN title ILIKE $3 || '%' THEN 3",
		"WHEN title ILIKE '%' || $1 || '%' AND title ILIKE '%' || $2 || '%' THEN 1",
		"ts_rank_cd(beads.search_tsv, websearch_to_tsquery('english', $3), 32)",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("%q missing from %q", frag, got)
		}
	}
}

func TestBeadListProjections(t *testing.T) {
	yes := true
	cases := []struct {
		name   string
		filter model.BeadFilter
		expr   string
		want   []string
	}{
		{"defaults", model.BeadFilter{}, "", []string{
			", FALSE AS has_report", ", '' AS blocked_on_decision_bead_id", ", '' AS parked_on_decision_bead_id", ", 0::float8 AS relevance_score"}},
		{"filters imply projections", model.BeadFilter{HasReport: &yes, BlockedOnDecision: &yes, ParkedOnDecision: &yes}, "X", []string{
			beadHasReportSelect, beadBlockedOnDecisionSelectSuffix, beadParkedOnDecisionSelectSuffix, ", (X) AS relevance_score"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, want := beadListProjections(tc.filter, tc.expr), strings.Join(tc.want, ""); got != want {
				t.Errorf("got %q\nwant %q", got, want)
			}
		})
	}
}

func TestBuildBeadListDataQuery(t *testing.T) {
	w := &beadListWhere{args: []any{"open"}, argIdx: 1}
	q, args := buildBeadListDataQuery(model.BeadFilter{Limit: 10, Offset: 20}, w, " WHERE status = $1")
	if !strings.HasPrefix(q, "SELECT 0 AS total_count, ") {
		t.Errorf("non-search query should not use the window count: %q", q[:40])
	}
	if !strings.HasSuffix(q, " LIMIT $2 OFFSET $3") {
		t.Errorf("query tail = %q", q[len(q)-30:])
	}
	if want := []any{"open", 10, 20}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
	if want := []any{"open"}; !reflect.DeepEqual(w.args, want) {
		t.Errorf("WHERE args changed to %#v; the count statement reuses them", w.args)
	}

	w = &beadListWhere{searchRelevanceExpr: "1"}
	q, _ = buildBeadListDataQuery(model.BeadFilter{}, w, "")
	if !strings.HasPrefix(q, "SELECT COUNT(*) OVER() AS total_count, ") {
		t.Errorf("search query should use the window count: %q", q[:50])
	}
}

func TestResolveListTotal(t *testing.T) {
	cases := []struct {
		name   string
		filter model.BeadFilter
		n      int
		want   int
	}{
		{"short page", model.BeadFilter{Limit: 10, Offset: 30}, 4, 34},
		{"no limit", model.BeadFilter{}, 7, 7},
		{"empty first page", model.BeadFilter{Limit: 10}, 0, 0},
		{"full page, no total", model.BeadFilter{Limit: 2, NoTotal: true}, 2, -1},
		{"empty page past start, no total", model.BeadFilter{Limit: 2, Offset: 4, NoTotal: true}, 0, -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A nil db proves these paths never query.
			got, err := resolveListTotal(context.Background(), nil, tc.filter, tc.n, "", nil)
			if err != nil || got != tc.want {
				t.Errorf("got %d, %v; want %d", got, err, tc.want)
			}
		})
	}

	t.Run("full page counts", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT COUNT\(\*\) FROM beads WHERE x = \$1`).WithArgs("v").
			WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))
		got, err := resolveListTotal(context.Background(), db, model.BeadFilter{Limit: 2}, 2, " WHERE x = $1", []any{"v"})
		if err != nil || got != 42 {
			t.Errorf("got %d, %v; want 42", got, err)
		}
	})
	t.Run("count error", func(t *testing.T) {
		db, mock := newMockDB(t)
		mock.ExpectQuery(`SELECT COUNT`).WillReturnError(errors.New("boom"))
		_, err := resolveListTotal(context.Background(), db, model.BeadFilter{Limit: 2}, 2, "", nil)
		if err == nil || !strings.HasPrefix(err.Error(), "count beads: ") {
			t.Errorf("err = %v, want a count beads error", err)
		}
	})
}

func TestEnrichListedBeads_EmptyPageQueriesNothing(t *testing.T) {
	filter := model.BeadFilter{WithOpenBlockers: true, WithParent: true}
	if err := enrichListedBeads(context.Background(), nil, filter, nil); err != nil {
		t.Fatal(err)
	}
}
