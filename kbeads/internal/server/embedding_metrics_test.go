package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
)

// scrapeServerMetrics collects the daemon's /metrics output through the same
// registry MetricsHandler builds in production.
func scrapeServerMetrics(t *testing.T, srv *BeadsServer) string {
	t.Helper()
	handler := srv.MetricsHandler(t.Context())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics: got %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

// TestSemanticSearchEmitsPhaseMetrics verifies the HTTP search path observes
// the embed_query / pgvector_scan / total phases and the result-count
// histogram. Regression guard for ka-0000003559 gap #2.
func TestSemanticSearchEmitsPhaseMetrics(t *testing.T) {
	// Reset the package-global histograms so counts are deterministic even
	// when other tests in this package exercise the search path.
	metrics.SemanticSearchDuration.Reset()

	s, ms, h := newTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})
	ms.beads["gd-1"] = &model.Bead{ID: "gd-1", Title: "login bug", Status: model.StatusOpen}
	ms.semanticResults = []*model.SemanticResult{{Bead: ms.beads["gd-1"], Score: 0.91}}

	rec := doJSON(t, h, http.MethodPost, "/v1/search/semantic",
		map[string]any{"query": "login authentication bug", "limit": 10})
	if rec.Code != http.StatusOK {
		t.Fatalf("search status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	body := scrapeServerMetrics(t, s)
	for _, phase := range []string{metrics.PhaseEmbedQuery, metrics.PhasePgvector, metrics.PhaseTotal} {
		want := `gardenio_beads_semantic_search_duration_seconds_count{phase="` + phase + `"} 1`
		if !strings.Contains(body, want) {
			t.Errorf("missing phase observation %q, body:\n%s", want, body)
		}
	}
	// One search returned exactly one result: the [1,2) result-count bucket
	// must have recorded a sample.
	if !strings.Contains(body, `gardenio_beads_semantic_search_results_total_count 1`) {
		t.Errorf("missing result-count observation, body:\n%s", body)
	}
}

// TestStoreEmbeddingStatsProvider_Emits verifies the store-backed provider
// wired via SetEmbeddingStatsProvider surfaces live coverage/stale gauges on
// /metrics instead of the noop fallback. Regression guard for gap #3.
func TestStoreEmbeddingStatsProvider_Emits(t *testing.T) {
	ms := newMockStore()
	ms.embeddingStats = &model.EmbeddingStats{
		TotalBeads:    200,
		EmbeddedBeads: 180,
		StaleCount:    7,
	}
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetEmbeddingStatsProvider(NewStoreEmbeddingStatsProvider(ms, "gardenio"))

	body := scrapeServerMetrics(t, srv)
	if !strings.Contains(body, `gardenio_beads_embedding_coverage_ratio{fleet="gardenio"} 0.9`) {
		t.Errorf("missing coverage ratio, body:\n%s", body)
	}
	if !strings.Contains(body, `gardenio_beads_embedding_stale_count{fleet="gardenio"} 7`) {
		t.Errorf("missing stale count, body:\n%s", body)
	}
}

// TestStoreEmbeddingStatsProvider_Passthrough checks the adapter maps the
// store aggregate onto a single fleet-labeled snapshot and defaults an empty
// fleet to "default".
func TestStoreEmbeddingStatsProvider_Passthrough(t *testing.T) {
	ms := newMockStore()
	ms.embeddingStats = &model.EmbeddingStats{TotalBeads: 10, EmbeddedBeads: 5, StaleCount: 2}
	p := NewStoreEmbeddingStatsProvider(ms, "")

	got, err := p.EmbeddingStats(t.Context())
	if err != nil {
		t.Fatalf("EmbeddingStats err = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 snapshot", len(got))
	}
	want := metrics.EmbeddingStats{Fleet: "default", TotalBeads: 10, EmbeddedBeads: 5, StaleCount: 2}
	if got[0] != want {
		t.Errorf("snapshot = %+v, want %+v", got[0], want)
	}
}

// TestStoreEmbeddingStatsProvider_Error propagates store errors so the
// collector reuses its previous snapshot instead of publishing zeros.
func TestStoreEmbeddingStatsProvider_Error(t *testing.T) {
	ms := newMockStore()
	ms.embeddingStatsErr = errors.New("db down")
	p := NewStoreEmbeddingStatsProvider(ms, "gardenio")

	if _, err := p.EmbeddingStats(t.Context()); err == nil {
		t.Fatal("EmbeddingStats err = nil, want error")
	}
}
