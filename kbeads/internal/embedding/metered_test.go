package embedding

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// fakeInner is a minimal Embedder used to drive the metered decorator.
type fakeInner struct {
	vec  []float32
	vecs [][]float32
	err  error
}

func (f *fakeInner) Embed(context.Context, string) ([]float32, error) { return f.vec, f.err }
func (f *fakeInner) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return f.vecs, f.err
}
func (f *fakeInner) ModelName() string { return "fake-model" }
func (f *fakeInner) Dimensions() int   { return 3 }

func scrapeMetered(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

func TestMeteredEmbedder_RecordsDurationAndError(t *testing.T) {
	reg := prometheus.NewRegistry()
	metrics.RegisterEmbedding(t.Context(), reg, nil, 0)

	me := newMeteredEmbedder(&fakeInner{vec: []float32{0.1, 0.2, 0.3}}, metrics.BackendOpenAI)
	if _, err := me.Embed(context.Background(), "hello"); err != nil {
		t.Fatalf("Embed err = %v, want nil", err)
	}

	failing := newMeteredEmbedder(&fakeInner{err: errors.New("connection refused")}, metrics.BackendOpenAI)
	if _, err := failing.EmbedBatch(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("EmbedBatch err = nil, want error")
	}

	body := scrapeMetered(t, reg)
	if !strings.Contains(body, `gardenio_beads_embed_duration_seconds_count{backend="openai",op="embed"} 1`) {
		t.Errorf("expected 1 embed observation, body:\n%s", body)
	}
	if !strings.Contains(body, `gardenio_beads_embed_duration_seconds_count{backend="openai",op="embed_batch"} 1`) {
		t.Errorf("expected 1 embed_batch observation, body:\n%s", body)
	}
	if !strings.Contains(body, `gardenio_beads_embed_errors_total{backend="openai",reason="transport"} 1`) {
		t.Errorf("expected 1 transport error, body:\n%s", body)
	}
}

func TestMeteredEmbedder_PassesThroughModelAndDims(t *testing.T) {
	me := newMeteredEmbedder(&fakeInner{}, metrics.BackendOpenAI)
	if me.ModelName() != "fake-model" {
		t.Errorf("ModelName = %q, want fake-model", me.ModelName())
	}
	if me.Dimensions() != 3 {
		t.Errorf("Dimensions = %d, want 3", me.Dimensions())
	}
}
