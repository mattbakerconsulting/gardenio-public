package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/model"
)

// fakeEmbedder is a test double that returns a fixed vector (or a fixed error).
type fakeEmbedder struct {
	vec  []float32
	name string
	dims int
	err  error
}

func (f *fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.vec, nil
}

func (f *fakeEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = f.vec
	}
	return out, nil
}
func (f *fakeEmbedder) ModelName() string { return f.name }
func (f *fakeEmbedder) Dimensions() int   { return f.dims }

func TestHandleSemanticSearch_HappyPath(t *testing.T) {
	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})

	ms.beads["gd-1"] = &model.Bead{ID: "gd-1", Title: "login bug", Status: model.StatusOpen}
	ms.beads["gd-2"] = &model.Bead{ID: "gd-2", Title: "auth issue", Status: model.StatusOpen}
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-1"], Score: 0.91},
		{Bead: ms.beads["gd-2"], Score: 0.62},
	}

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic",
		map[string]any{"query": "login authentication bug", "limit": 10})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var resp SemanticSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Model != "test-model" {
		t.Errorf("Model: got %q, want %q", resp.Model, "test-model")
	}
	if len(resp.Results) != 2 {
		t.Fatalf("results len: got %d, want 2", len(resp.Results))
	}
	if resp.Results[0].Bead.ID != "gd-1" || resp.Results[1].Bead.ID != "gd-2" {
		t.Errorf("unexpected result order: %+v", resp.Results)
	}
	if ms.lastSemanticLimit != 10 {
		t.Errorf("store limit: got %d, want 10", ms.lastSemanticLimit)
	}
	if len(ms.lastSemanticVec) != 3 {
		t.Errorf("store vec len: got %d, want 3", len(ms.lastSemanticVec))
	}
}

func TestHandleSemanticSearch_MissingQuery_400(t *testing.T) {
	s, _, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", map[string]any{"query": ""})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSemanticSearch_InvalidJSON_400(t *testing.T) {
	_, _, h := newTestServer()
	// Send invalid JSON via a raw body.
	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSemanticSearch_NoEmbedderConfigured_503(t *testing.T) {
	_, _, h := newTestServer() // embedder left nil
	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", map[string]any{"query": "x"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSemanticSearch_NoopEmbedder_503(t *testing.T) {
	s, _, h := newTestServer()
	s.SetEmbedder(embedding.NewNoopEmbedder())

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", map[string]any{"query": "x"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", rec.Code, rec.Body.String())
	}
}

// TestHandleSemanticSearch_503_AttachesServerError pins the ka-0000002434 fix:
// the two "semantic search disabled" 503 paths (nil embedder + noop embedder
// ErrDisabled) must attach a structured server error so the loggingMiddleware
// "http 5xx root cause" WARN names the disabled state instead of emitting the
// misleading "unknown (handler did not call AttachServerError)" line. This is
// what makes the e2e fixture's expected-503 fallback read as a known degraded
// backend rather than an unexplained 5xx.
func TestHandleSemanticSearch_503_AttachesServerError(t *testing.T) {
	for _, tc := range []struct {
		name     string
		embedder embedding.Embedder // nil → no embedder configured
	}{
		{name: "nil embedder", embedder: nil},
		{name: "noop embedder", embedder: embedding.NewNoopEmbedder()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			var mu sync.Mutex
			h := slog.NewTextHandler(&lockedWriter{buf: &buf, mu: &mu}, &slog.HandlerOptions{Level: slog.LevelDebug})
			prev := slog.Default()
			slog.SetDefault(slog.New(h))
			t.Cleanup(func() { slog.SetDefault(prev) })

			s, _, handler := newTestServer()
			if tc.embedder != nil {
				s.SetEmbedder(tc.embedder)
			}

			rec := doJSON(t, handler, http.MethodPost, "/v1/search/semantic",
				map[string]any{"query": "x"})
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status: got %d, want 503; body=%s", rec.Code, rec.Body.String())
			}

			mu.Lock()
			out := buf.String()
			mu.Unlock()

			// The 5xx root-cause WARN fires (503 ≥ 500) but must carry a real
			// cause — not the "handler did not call AttachServerError" sentinel.
			if got := strings.Count(out, `msg="http 5xx root cause"`); got != 1 {
				t.Errorf("expected exactly 1 '5xx root cause' log, got %d; out:\n%s", got, out)
			}
			if strings.Contains(out, "handler did not call AttachServerError") {
				t.Errorf("503 path must attach a structured cause; got the sentinel:\n%s", out)
			}
			if !strings.Contains(out, "handler=handleSemanticSearch") {
				t.Errorf("expected handler=handleSemanticSearch in the WARN; out:\n%s", out)
			}
			if !strings.Contains(out, "disabled") {
				t.Errorf("expected the disabled cause in the WARN; out:\n%s", out)
			}
		})
	}
}

func TestHandleSemanticSearch_EmbedError_500(t *testing.T) {
	s, _, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{err: errors.New("remote down")})

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", map[string]any{"query": "x"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSemanticSearch_SearchError_500(t *testing.T) {
	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticErr = errors.New("db blew up")

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", map[string]any{"query": "x"})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

func TestHandleSemanticSearch_MinScoreFilter(t *testing.T) {
	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})

	ms.beads["gd-A"] = &model.Bead{ID: "gd-A", Status: model.StatusOpen}
	ms.beads["gd-B"] = &model.Bead{ID: "gd-B", Status: model.StatusOpen}
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-A"], Score: 0.92},
		{Bead: ms.beads["gd-B"], Score: 0.40},
	}

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic",
		map[string]any{"query": "x", "min_score": 0.75})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp SemanticSearchResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Results) != 1 || resp.Results[0].Bead.ID != "gd-A" {
		t.Errorf("min_score filter: got %+v", resp.Results)
	}
}

func TestHandleSemanticSearch_FilterPassThrough(t *testing.T) {
	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	body := map[string]any{
		"query":    "auth",
		"status":   []string{"open", "in_progress"},
		"type":     []string{"bug", "task"},
		"kind":     []string{"issue"},
		"labels":   []string{"project:gardenio"},
		"assignee": "alice",
	}
	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
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

func TestHandleSemanticSearch_DefaultLimit(t *testing.T) {
	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic", map[string]any{"query": "x"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if ms.lastSemanticLimit != 20 {
		t.Errorf("default limit: got %d, want 20", ms.lastSemanticLimit)
	}
}

func TestHandleSemanticSearch_LimitClamp(t *testing.T) {
	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic",
		map[string]any{"query": "x", "limit": 9999})
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, body=%s", rec.Code, rec.Body.String())
	}
	if ms.lastSemanticLimit != 100 {
		t.Errorf("clamped limit: got %d, want 100", ms.lastSemanticLimit)
	}
}
