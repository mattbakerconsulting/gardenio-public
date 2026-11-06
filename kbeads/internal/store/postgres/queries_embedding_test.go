package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
)

func TestVectorLiteral(t *testing.T) {
	for _, tc := range []struct {
		in   []float32
		want string
	}{
		{nil, "[]"},
		{[]float32{}, "[]"},
		{[]float32{0.5}, "[0.5]"},
		{[]float32{0.1, 0.2, 0.3}, "[0.1,0.2,0.3]"},
		{[]float32{-1, 0, 1}, "[-1,0,1]"},
	} {
		if got := vectorLiteral(tc.in); got != tc.want {
			t.Errorf("vectorLiteral(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestQueryUpsertEmbedding(t *testing.T) {
	db, mock := newMockDB(t)
	embedding := []float32{0.1, 0.2, 0.3}
	mock.ExpectExec("INSERT INTO bead_embeddings").
		WithArgs("gd-abc", "[0.1,0.2,0.3]", "nomic-embed-text", "deadbeef").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryUpsertEmbedding(context.Background(), db, "gd-abc", embedding, "nomic-embed-text", "deadbeef"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryDeleteEmbedding(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec("DELETE FROM bead_embeddings WHERE bead_id = \\$1").
		WithArgs("gd-abc").
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := queryDeleteEmbedding(context.Background(), db, "gd-abc"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQuerySemanticSearch(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()
	queryVec := []float32{0.1, 0.2, 0.3}

	rows := sqlmock.NewRows([]string{
		"score",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
		"operated_by_id",
		"operated_by", "operated_by_display", "operated_by_email",
	}).AddRow(
		0.87,
		"gd-hit1", nil, nil, nil, "issue", "task", "Matching bead", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "",
	).AddRow(
		0.72,
		"gd-hit2", nil, nil, nil, "issue", "bug", "Another hit", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
		"", "", "",
	)

	// Bumping ivfflat.probes is part of the semantic-search contract — pgvector
	// defaults to probes=1 which on a 100-list index gives ~5% recall on the
	// e2e test corpus and produced a release-blocking regression (ka-0000005881).
	mock.ExpectExec(`SET ivfflat\.probes = 10`).WillReturnResult(sqlmock.NewResult(0, 0))
	// Regex pins:
	//  - the score expression
	//  - the b.deleted_at projection + soft-delete WHERE clause (ka-0000004444)
	//  - the canonical operated_by alias projection (op.kind || ':' || op.subject)
	//    which scanBeadWithScoreFull expects after ka-0000006816. Missing this
	//    column produces a runtime "sql: expected 26 destination arguments in
	//    Scan, not 25" → HTTP 500 "semantic search failed" (ka-0000007367).
	mock.ExpectQuery(`SELECT 1 - .e.embedding <=> .1::vector. AS score.*b\.deleted_at.*op\.kind \|\| ':' \|\| op\.subject.*operated_by_display.*b\.deleted_at IS NULL`).
		WithArgs("[0.1,0.2,0.3]", "open", 10).
		WillReturnRows(rows)

	status := []model.Status{model.StatusOpen}
	results, err := querySemanticSearch(context.Background(), db, queryVec, model.BeadFilter{Status: status}, 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Bead.ID != "gd-hit1" || results[0].Score != 0.87 {
		t.Errorf("results[0]: id=%q score=%v", results[0].Bead.ID, results[0].Score)
	}
	// Distance = 1 - score.
	if delta := results[0].Distance - 0.13; delta < -1e-9 || delta > 1e-9 {
		t.Errorf("results[0].Distance = %v, want ~0.13", results[0].Distance)
	}
}

func TestQuerySemanticSearch_DefaultLimit(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec(`SET ivfflat\.probes = 10`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT 1 - .e.embedding <=> .1::vector. AS score").
		WithArgs("[0.1]", 20). // limit defaults to 20 when <= 0
		WillReturnRows(sqlmock.NewRows([]string{
			"score",
			"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
			"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
			"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
			"operated_by_id",
			"operated_by", "operated_by_display", "operated_by_email",
		}))

	if _, err := querySemanticSearch(context.Background(), db, []float32{0.1}, model.BeadFilter{}, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryGetEmbeddingStats(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT").
		WillReturnRows(sqlmock.NewRows([]string{
			"total_beads", "embedded_beads", "model", "stale_count",
		}).AddRow(100, 42, "nomic-embed-text", 3))

	stats, err := queryGetEmbeddingStats(context.Background(), db)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if stats.TotalBeads != 100 || stats.EmbeddedBeads != 42 || stats.Model != "nomic-embed-text" || stats.StaleCount != 3 {
		t.Errorf("unexpected stats: %+v", stats)
	}
}

func TestParseVectorLiteral(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    []float32
		wantErr bool
	}{
		{name: "empty brackets", in: "[]", want: []float32{}},
		{name: "single", in: "[0.5]", want: []float32{0.5}},
		{name: "triple", in: "[0.1,0.2,0.3]", want: []float32{0.1, 0.2, 0.3}},
		{name: "negative", in: "[-1,0,1]", want: []float32{-1, 0, 1}},
		{name: "whitespace tolerated", in: " [ 1 , 2 , 3 ] ", want: []float32{1, 2, 3}},
		{name: "missing brackets", in: "1,2,3", wantErr: true},
		{name: "only open bracket", in: "[1,2", wantErr: true},
		{name: "non-numeric element", in: "[1,abc,3]", wantErr: true},
		{name: "empty string", in: "", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseVectorLiteral(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("parseVectorLiteral(%q) err = nil, want error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseVectorLiteral(%q) err = %v", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("len = %d, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("got[%d] = %v, want %v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestParseVectorLiteral_VectorLiteralRoundTrip(t *testing.T) {
	// vectorLiteral -> parseVectorLiteral should be lossless for finite float32 values.
	in := []float32{0, 1, -1, 0.5, -0.25, 3.14159, 1e-6}
	encoded := vectorLiteral(in)
	decoded, err := parseVectorLiteral(encoded)
	if err != nil {
		t.Fatalf("parseVectorLiteral(%q) err = %v", encoded, err)
	}
	if len(decoded) != len(in) {
		t.Fatalf("len = %d, want %d", len(decoded), len(in))
	}
	for i, v := range in {
		if decoded[i] != v {
			t.Errorf("round-trip %d: got %v, want %v", i, decoded[i], v)
		}
	}
}

func TestQueryGetEmbedding(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT embedding::text, model FROM bead_embeddings WHERE bead_id = \\$1").
		WithArgs("gd-abc").
		WillReturnRows(sqlmock.NewRows([]string{"embedding", "model"}).
			AddRow("[0.1,0.2,0.3]", "nomic-embed-text"))

	vec, modelName, err := queryGetEmbedding(context.Background(), db, "gd-abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if modelName != "nomic-embed-text" {
		t.Errorf("model = %q, want nomic-embed-text", modelName)
	}
	if len(vec) != 3 || vec[0] != 0.1 || vec[1] != 0.2 || vec[2] != 0.3 {
		t.Errorf("vec = %v, want [0.1 0.2 0.3]", vec)
	}
}

func TestQueryGetEmbedding_NotFound(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT embedding::text, model FROM bead_embeddings WHERE bead_id = \\$1").
		WithArgs("gd-missing").
		WillReturnError(sql.ErrNoRows)

	_, _, err := queryGetEmbedding(context.Background(), db, "gd-missing")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestQueryGetEmbedding_InvalidStoredVector(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectQuery("SELECT embedding::text, model FROM bead_embeddings WHERE bead_id = \\$1").
		WithArgs("gd-bad").
		WillReturnRows(sqlmock.NewRows([]string{"embedding", "model"}).
			AddRow("not-a-vector", "nomic-embed-text"))

	_, _, err := queryGetEmbedding(context.Background(), db, "gd-bad")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestQuerySemanticSearch_AllFilters exercises the query builder's filter
// branches: type, kind, labels, priority, assignee, fleet. The goal is to
// confirm the WHERE-clause wiring, not to reproduce the exact SQL byte-for-byte.
func TestQuerySemanticSearch_AllFilters(t *testing.T) {
	db, mock := newMockDB(t)
	rowCols := []string{
		"score",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet",
	}

	mock.ExpectExec(`SET ivfflat\.probes = 10`).WillReturnResult(sqlmock.NewResult(0, 0))
	// Use ExpectQuery with a regex covering the key filter clauses we expect.
	// Argument order follows the query builder's code path:
	// $1=vec, $2=status, $3=type, $4=kind, $5=priority, $6=assignee,
	// $7=labels-scalar, $8=fleet, $9=limit.
	mock.ExpectQuery("b.status IN .* b.type IN .* b.kind IN .* b.priority = .* b.assignee = .* EXISTS .SELECT 1 FROM labels .* b.fleet = .* ORDER BY").
		WithArgs(
			"[0.1]",
			"open",
			"bug",
			"issue",
			1,
			"alice",
			"project:gardenio",
			"fleet-1",
			25,
		).
		WillReturnRows(sqlmock.NewRows(rowCols))

	p := 1
	_, err := querySemanticSearch(context.Background(), db, []float32{0.1}, model.BeadFilter{
		Status:   []model.Status{model.StatusOpen},
		Type:     []model.BeadType{model.TypeBug},
		Kind:     []model.Kind{model.KindIssue},
		Priority: &p,
		Assignee: "alice",
		Labels:   []string{"project:gardenio"},
		Fleet:    "fleet-1",
	}, 25)
	if err != nil {
		t.Fatalf("querySemanticSearch err = %v", err)
	}
}

// TestQuerySemanticSearch_FleetAll verifies that the "__all__" sentinel
// omits the fleet WHERE clause entirely (cross-fleet search).
func TestQuerySemanticSearch_FleetAll(t *testing.T) {
	db, mock := newMockDB(t)
	rowCols := []string{
		"score",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet",
	}

	mock.ExpectExec(`SET ivfflat\.probes = 10`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT 1 - .e.embedding <=> .1::vector. AS score").
		WithArgs("[0.1]", 10). // only vec + limit — no fleet arg
		WillReturnRows(sqlmock.NewRows(rowCols))

	if _, err := querySemanticSearch(context.Background(), db,
		[]float32{0.1},
		model.BeadFilter{Fleet: model.FleetAll},
		10,
	); err != nil {
		t.Fatalf("querySemanticSearch err = %v", err)
	}
}

// TestQuerySemanticSearch_LabelsAny exercises the LabelsAny branch which
// uses the pq.Array argument form — distinct from the per-label Labels path.
func TestQuerySemanticSearch_LabelsAny(t *testing.T) {
	db, mock := newMockDB(t)
	rowCols := []string{
		"score",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet",
	}

	mock.ExpectExec(`SET ivfflat\.probes = 10`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("EXISTS .SELECT 1 FROM labels .* labels.label = ANY..*::text..").
		WithArgs("[0.1]", sqlmock.AnyArg(), 10).
		WillReturnRows(sqlmock.NewRows(rowCols))

	if _, err := querySemanticSearch(context.Background(), db,
		[]float32{0.1},
		model.BeadFilter{LabelsAny: []string{"a", "b"}},
		10,
	); err != nil {
		t.Fatalf("querySemanticSearch err = %v", err)
	}
}

// TestQuerySemanticSearch_Rows_Error ensures a rows.Err returned after
// iteration surfaces as an error.
func TestQuerySemanticSearch_Rows_Error(t *testing.T) {
	db, mock := newMockDB(t)
	rowCols := []string{
		"score",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet",
	}

	rows := sqlmock.NewRows(rowCols).RowError(0, errors.New("row scan boom")).AddRow(
		0.5,
		"gd-x", nil, nil, nil, "issue", "task", "t", nil, nil,
		"open", 0, nil, nil, time.Now(), nil, time.Now(), nil, nil, nil, nil, nil, "default",
	)
	mock.ExpectExec(`SET ivfflat\.probes = 10`).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery("SELECT 1 - .e.embedding <=> .1::vector. AS score").
		WithArgs("[0.1]", 10).
		WillReturnRows(rows)

	_, err := querySemanticSearch(context.Background(), db, []float32{0.1}, model.BeadFilter{}, 10)
	if err == nil {
		t.Fatal("expected error from rows iteration, got nil")
	}
}

func TestQueryListBeadsNeedingEmbedding(t *testing.T) {
	db, mock := newMockDB(t)
	now := time.Now().UTC()

	rows := sqlmock.NewRows([]string{
		"total_count",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
		"operated_by_id",
	}).AddRow(
		7,
		"gd-need", nil, nil, nil, "issue", "task", "Needs embedding", nil, nil,
		"open", 0, nil, nil, now, nil, now, nil, nil, nil, nil, nil, "default", nil, nil,
		nil,
	)

	// Expect the LEFT JOIN + (e.bead_id IS NULL OR b.updated_at > e.updated_at)
	// pre-filter, the soft-delete filter, the b.deleted_at projection, and
	// limit/offset positional args. The b.deleted_at portion guards against
	// the projection drifting out of sync with scanBeadWithTotal — that
	// mismatch produced HTTP 500 "failed to list beads" on `kd embed sync`
	// after migration 009 added the soft-delete column (ka-0000004444).
	mock.ExpectQuery(`b\.deleted_at.*FROM beads b LEFT JOIN bead_embeddings e ON e.bead_id = b.id WHERE \(e.bead_id IS NULL OR b.updated_at > e.updated_at\) AND b\.deleted_at IS NULL`).
		WithArgs(50, 0).
		WillReturnRows(rows)

	beads, total, err := queryListBeadsNeedingEmbedding(context.Background(), db, "", 50, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if total != 7 {
		t.Errorf("total: got %d, want 7 (workload count)", total)
	}
	if len(beads) != 1 || beads[0].ID != "gd-need" {
		t.Fatalf("beads: got %+v", beads)
	}
}

func TestQueryListBeadsNeedingEmbedding_FleetScope(t *testing.T) {
	db, mock := newMockDB(t)

	rows := sqlmock.NewRows([]string{
		"total_count",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet", "deleted_at", "category",
		"operated_by_id",
	})

	// Fleet scoping injects an extra arg before limit/offset. b.deleted_at
	// must still appear in projection + WHERE (ka-0000004444).
	mock.ExpectQuery(`b\.deleted_at.*b\.deleted_at IS NULL.*b.fleet = \$1`).
		WithArgs("alpha", 25, 10).
		WillReturnRows(rows)

	if _, _, err := queryListBeadsNeedingEmbedding(context.Background(), db, "alpha", 25, 10); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestQueryRefreshEmbeddingTimestamps(t *testing.T) {
	db, mock := newMockDB(t)
	mock.ExpectExec(`UPDATE bead_embeddings SET updated_at = NOW\(\) WHERE bead_id = ANY\(\$1\)`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := queryRefreshEmbeddingTimestamps(context.Background(), db, []string{"gd-a", "gd-b"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("rows affected: got %d, want 2", n)
	}
}

func TestQueryRefreshEmbeddingTimestamps_EmptyIsNoop(t *testing.T) {
	db, _ := newMockDB(t)
	// No ExpectExec — the function must short-circuit on empty input.
	n, err := queryRefreshEmbeddingTimestamps(context.Background(), db, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 0 {
		t.Errorf("rows affected: got %d, want 0", n)
	}
}

func TestQueryListBeadsNeedingEmbedding_FleetAllSentinelOmitsFilter(t *testing.T) {
	db, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{
		"total_count",
		"id", "slug", "short_title", "heraldry", "kind", "type", "title", "description", "notes",
		"status", "priority", "assignee", "owner", "created_at", "created_by", "updated_at",
		"closed_at", "closed_by", "due_at", "defer_until", "fields", "fleet",
	})

	// FleetAll sentinel must NOT add a fleet predicate — just limit + offset.
	mock.ExpectQuery(`FROM beads b LEFT JOIN bead_embeddings e`).
		WithArgs(50, 0).
		WillReturnRows(rows)

	if _, _, err := queryListBeadsNeedingEmbedding(context.Background(), db, model.FleetAll, 50, 0); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
