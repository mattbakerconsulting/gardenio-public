package server

import (
	"errors"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
)

func TestGRPCSemanticSearch_HappyPath(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})

	ms.beads["gd-1"] = &model.Bead{ID: "gd-1", Title: "login bug", Status: model.StatusOpen}
	ms.beads["gd-2"] = &model.Bead{ID: "gd-2", Title: "auth issue", Status: model.StatusOpen}
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-1"], Score: 0.91},
		{Bead: ms.beads["gd-2"], Score: 0.62},
	}

	resp, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{
		Query: "login authentication bug",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.GetModel() != "test-model" {
		t.Errorf("Model: got %q, want %q", resp.GetModel(), "test-model")
	}
	if len(resp.GetResults()) != 2 {
		t.Fatalf("results len: got %d, want 2", len(resp.GetResults()))
	}
	if resp.GetResults()[0].GetBead().GetId() != "gd-1" || resp.GetResults()[1].GetBead().GetId() != "gd-2" {
		t.Errorf("unexpected result order: %+v", resp.GetResults())
	}
	if resp.GetResults()[0].GetScore() < 0.9 || resp.GetResults()[0].GetScore() > 0.92 {
		t.Errorf("score: got %v, want ~0.91", resp.GetResults()[0].GetScore())
	}
	if ms.lastSemanticLimit != 10 {
		t.Errorf("store limit: got %d, want 10", ms.lastSemanticLimit)
	}
	if len(ms.lastSemanticVec) != 3 {
		t.Errorf("store vec len: got %d, want 3", len(ms.lastSemanticVec))
	}
}

func TestGRPCSemanticSearch_MissingQuery(t *testing.T) {
	srv, _, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})

	_, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: ""})
	requireCode(t, err, codes.InvalidArgument)
}

func TestGRPCSemanticSearch_NoEmbedder(t *testing.T) {
	srv, _, ctx := testCtx(t) // embedder left nil
	_, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: "x"})
	requireCode(t, err, codes.Unavailable)
}

func TestGRPCSemanticSearch_NoopEmbedder(t *testing.T) {
	srv, _, ctx := testCtx(t)
	srv.SetEmbedder(embedding.NewNoopEmbedder())

	_, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: "x"})
	requireCode(t, err, codes.Unavailable)
}

func TestGRPCSemanticSearch_EmbedError(t *testing.T) {
	srv, _, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{err: errors.New("remote down")})

	_, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: "x"})
	requireCode(t, err, codes.Internal)
}

func TestGRPCSemanticSearch_SearchError(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticErr = errors.New("db blew up")

	_, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: "x"})
	requireCode(t, err, codes.Internal)
}

func TestGRPCSemanticSearch_MinScoreFilter(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})

	ms.beads["gd-A"] = &model.Bead{ID: "gd-A", Status: model.StatusOpen}
	ms.beads["gd-B"] = &model.Bead{ID: "gd-B", Status: model.StatusOpen}
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-A"], Score: 0.92},
		{Bead: ms.beads["gd-B"], Score: 0.40},
	}

	resp, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{
		Query:    "x",
		MinScore: 0.75,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.GetResults()) != 1 || resp.GetResults()[0].GetBead().GetId() != "gd-A" {
		t.Errorf("min_score filter: got %+v", resp.GetResults())
	}
}

func TestGRPCSemanticSearch_FilterPassThrough(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	_, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{
		Query:    "auth",
		Status:   []string{"open", "in_progress"},
		Type:     []string{"bug", "task"},
		Kind:     []string{"issue"},
		Labels:   []string{"project:gardenio"},
		Assignee: "alice",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	f := ms.lastSemanticFilter
	if len(f.Status) != 2 || f.Status[0] != model.StatusOpen || f.Status[1] != model.StatusInProgress {
		t.Errorf("status filter: got %+v", f.Status)
	}
	if len(f.Type) != 2 || f.Type[0] != model.TypeBug || f.Type[1] != model.TypeTask {
		t.Errorf("type filter: got %+v", f.Type)
	}
	if len(f.Kind) != 1 || f.Kind[0] != model.KindIssue {
		t.Errorf("kind filter: got %+v", f.Kind)
	}
	if len(f.Labels) != 1 || f.Labels[0] != "project:gardenio" {
		t.Errorf("labels filter: got %+v", f.Labels)
	}
	if f.Assignee != "alice" {
		t.Errorf("assignee filter: got %q", f.Assignee)
	}
}

func TestGRPCSemanticSearch_DefaultLimit(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	if _, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: "x"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms.lastSemanticLimit != 20 {
		t.Errorf("default limit: got %d, want 20", ms.lastSemanticLimit)
	}
}

func TestGRPCSemanticSearch_LimitClamp(t *testing.T) {
	srv, ms, ctx := testCtx(t)
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	if _, err := srv.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: "x", Limit: 9999}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms.lastSemanticLimit != 100 {
		t.Errorf("clamped limit: got %d, want 100", ms.lastSemanticLimit)
	}
}
