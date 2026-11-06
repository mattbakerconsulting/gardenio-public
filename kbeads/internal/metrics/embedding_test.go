package metrics

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestClassifyEmbedError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{nil, ReasonOther},
		{context.DeadlineExceeded, ReasonTimeout},
		{errors.New("openai embed: status 401"), ReasonUnauthorized},
		{errors.New("openai: status 429 rate limit"), ReasonRateLimited},
		{errors.New("openai embed: empty response"), ReasonEmptyVector},
		{errors.New("openai embed decode: invalid char"), ReasonBadResponse},
		{errors.New("openai embed: status 500"), ReasonBadResponse},
		{errors.New("dial tcp: lookup host: no such host"), ReasonTransport},
		{errors.New("connection refused"), ReasonTransport},
		{errors.New("EOF"), ReasonTransport},
		{errors.New("something went wrong"), ReasonOther},
	}
	for _, tt := range tests {
		got := classifyEmbedError(tt.err)
		if got != tt.want {
			t.Errorf("classifyEmbedError(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestObserveEmbed_RecordsDurationAndError(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterEmbedding(t.Context(), reg, nil, time.Minute)

	start := time.Now().Add(-200 * time.Millisecond)
	ObserveEmbed(BackendOpenAI, OpEmbed, start, nil)
	ObserveEmbed(BackendOpenAI, OpEmbed, start, errors.New("connection refused"))

	body := scrape(t, reg)

	if !strings.Contains(body, `gardenio_beads_embed_duration_seconds_count{backend="openai",op="embed"} 2`) {
		t.Errorf("expected 2 duration observations, body:\n%s", body)
	}
	if !strings.Contains(body, `gardenio_beads_embed_errors_total{backend="openai",reason="transport"} 1`) {
		t.Errorf("expected 1 transport error, body:\n%s", body)
	}
}

type fakeStatsProvider struct {
	stats []EmbeddingStats
	err   error
	calls int
}

func (f *fakeStatsProvider) EmbeddingStats(_ context.Context) ([]EmbeddingStats, error) {
	f.calls++
	return f.stats, f.err
}

func TestStatsCollector_RatioAndStale(t *testing.T) {
	p := &fakeStatsProvider{
		stats: []EmbeddingStats{
			{Fleet: "default", TotalBeads: 200, EmbeddedBeads: 180, StaleCount: 5},
			{Fleet: "other", TotalBeads: 0, EmbeddedBeads: 0, StaleCount: 0},
		},
	}
	reg := prometheus.NewRegistry()
	RegisterEmbedding(t.Context(), reg, p, time.Minute)

	body := scrape(t, reg)

	if !strings.Contains(body, `gardenio_beads_embedding_coverage_ratio{fleet="default"} 0.9`) {
		t.Errorf("expected coverage ratio 0.9 for default, body:\n%s", body)
	}
	if !strings.Contains(body, `gardenio_beads_embedding_coverage_ratio{fleet="other"} 0`) {
		t.Errorf("expected coverage ratio 0 for other, body:\n%s", body)
	}
	if !strings.Contains(body, `gardenio_beads_embedding_stale_count{fleet="default"} 5`) {
		t.Errorf("expected stale count 5, body:\n%s", body)
	}
}

func TestStatsCollector_CollectNeverQueriesProvider(t *testing.T) {
	// Collect() must serve the cached snapshot only — the provider is queried
	// exclusively on the background refresher (off the scrape path). So many
	// scrapes trigger zero provider calls; only refresh() does.
	p := &fakeStatsProvider{
		stats: []EmbeddingStats{{Fleet: "default", TotalBeads: 10, EmbeddedBeads: 5}},
	}
	c := NewEmbeddingStatsCollector(p, time.Hour)

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	scrape(t, reg)
	scrape(t, reg)
	scrape(t, reg)
	if p.calls != 0 {
		t.Errorf("expected 0 provider calls from scrapes alone, got %d", p.calls)
	}

	// A single explicit refresh (what the background ticker does) is the only
	// thing that queries the provider.
	c.refresh(context.Background())
	if p.calls != 1 {
		t.Errorf("expected 1 provider call after one refresh, got %d", p.calls)
	}
	body := scrape(t, reg)
	if !strings.Contains(body, `gardenio_beads_embedding_coverage_ratio{fleet="default"} 0.5`) {
		t.Errorf("expected refreshed snapshot to be served, body:\n%s", body)
	}
	if p.calls != 1 {
		t.Errorf("scrape after refresh must not query provider; calls=%d", p.calls)
	}
}

func TestStatsCollector_ReusesSnapshotOnError(t *testing.T) {
	p := &fakeStatsProvider{
		stats: []EmbeddingStats{{Fleet: "default", TotalBeads: 10, EmbeddedBeads: 5}},
	}
	c := NewEmbeddingStatsCollector(p, time.Hour)

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	// Initial refresh populates snapshot.
	c.refresh(context.Background())
	body1 := scrape(t, reg)
	if !strings.Contains(body1, `gardenio_beads_embedding_coverage_ratio{fleet="default"} 0.5`) {
		t.Fatalf("initial scrape missing metric, body:\n%s", body1)
	}

	// Provider starts erroring — a refresh must keep the last good snapshot.
	p.stats = nil
	p.err = errors.New("database unreachable")
	c.refresh(context.Background())

	body2 := scrape(t, reg)
	if !strings.Contains(body2, `gardenio_beads_embedding_coverage_ratio{fleet="default"} 0.5`) {
		t.Errorf("expected stale snapshot reuse on provider error, body:\n%s", body2)
	}
}

// blockingStatsProvider blocks in EmbeddingStats until unblocked, simulating a
// saturated Postgres. It is the crux of ka-0000006261: a scrape must NOT wait
// on it.
type blockingStatsProvider struct {
	release chan struct{}
}

func (b *blockingStatsProvider) EmbeddingStats(ctx context.Context) ([]EmbeddingStats, error) {
	select {
	case <-b.release:
		return nil, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestStatsCollector_ScrapeNeverBlocksOnSlowProvider(t *testing.T) {
	// Regression for ka-0000006261: even while a refresh is stuck in a slow DB
	// query, Collect()/scrape must return promptly (in-memory only), because
	// /metrics backs the up{component="beads"} liveness signal.
	p := &blockingStatsProvider{release: make(chan struct{})}
	c := NewEmbeddingStatsCollector(p, time.Hour)

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	// Kick a refresh that will hang inside the provider.
	refreshDone := make(chan struct{})
	go func() {
		c.refresh(context.Background())
		close(refreshDone)
	}()

	// Scrape repeatedly while the refresh is blocked; each must return fast.
	deadline := time.After(2 * time.Second)
	for i := 0; i < 5; i++ {
		select {
		case <-deadline:
			t.Fatal("scrape blocked on the stuck provider — /metrics is coupled to DB latency")
		default:
		}
		start := time.Now()
		body := scrape(t, reg)
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("scrape took %v while refresh blocked — must be in-memory only", elapsed)
		}
		if !strings.Contains(body, "gardenio_beads_embedding_stats_staleness_seconds") {
			t.Errorf("staleness gauge missing during stuck refresh, body:\n%s", body)
		}
	}

	// Let the refresh complete so the goroutine exits cleanly.
	close(p.release)
	select {
	case <-refreshDone:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh did not complete after release")
	}
}

func TestStatsCollector_StalenessGaugeAlwaysEmitted(t *testing.T) {
	// The staleness gauge must be emitted even before the first successful
	// refresh, so a refresher that never completes is visible.
	p := &fakeStatsProvider{err: errors.New("db down from the start")}
	c := NewEmbeddingStatsCollector(p, time.Hour)

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	body := scrape(t, reg)
	if !strings.Contains(body, "gardenio_beads_embedding_stats_staleness_seconds") {
		t.Errorf("expected staleness gauge even before first refresh, body:\n%s", body)
	}
}

func TestNoopStatsProvider_DoesNotPanic(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterEmbedding(t.Context(), reg, NoopStatsProvider{}, time.Minute)

	// Scrape must succeed even when no metrics have been observed yet.
	// Prometheus exposition format only emits HELP/TYPE for metrics with
	// at least one sample — a fresh scrape with Noop stats will be mostly
	// empty, and that is correct behavior.
	_ = scrape(t, reg)
}

func TestSemanticSearchObservation(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterEmbedding(t.Context(), reg, nil, time.Minute)

	SemanticSearchDuration.WithLabelValues(PhaseEmbedQuery).Observe(0.05)
	SemanticSearchDuration.WithLabelValues(PhasePgvector).Observe(0.02)
	SemanticSearchDuration.WithLabelValues(PhaseTotal).Observe(0.08)
	SemanticSearchResults.Observe(12)

	body := scrape(t, reg)

	for _, want := range []string{
		`gardenio_beads_semantic_search_duration_seconds_count{phase="embed_query"} 1`,
		`gardenio_beads_semantic_search_duration_seconds_count{phase="pgvector_scan"} 1`,
		`gardenio_beads_semantic_search_duration_seconds_count{phase="total"} 1`,
		`gardenio_beads_semantic_search_results_total_count 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in body:\n%s", want, body)
		}
	}
}

func scrape(t *testing.T, reg *prometheus.Registry) string {
	t.Helper()
	h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})
	req := httptest.NewRequest("GET", "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("scrape failed: %d", rec.Code)
	}
	return rec.Body.String()
}

// Ensure package-scoped histogram/counter are usable without prior registration
// (common bug: referencing them before Register causes nil-vec panics).
func TestPackageMetricsUsableBeforeRegistration(t *testing.T) {
	// These must not panic even if RegisterEmbedding has not been called yet.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("panic using package metrics: %v", r)
		}
	}()
	EmbedDuration.WithLabelValues(BackendOpenAI, OpEmbedBatch).Observe(0.1)
	EmbedErrors.WithLabelValues(BackendOpenAI, ReasonRateLimited).Inc()
}

// Sanity: RegisterEmbedding is safe to call once per registry.
func TestRegisterEmbedding_IdempotentPerRegistry(t *testing.T) {
	reg := prometheus.NewRegistry()
	RegisterEmbedding(t.Context(), reg, nil, time.Minute)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on double-register of same collector")
		}
	}()
	RegisterEmbedding(t.Context(), reg, nil, time.Minute)
}

// Compile-time contract: classify the big-picture error buckets
// deterministically so the test fleet exactly matches the label set used in
// the Grafana dashboard.
func TestReasonLabelsAreStable(t *testing.T) {
	all := []string{
		ReasonTimeout,
		ReasonTransport,
		ReasonBadResponse,
		ReasonEmptyVector,
		ReasonUnauthorized,
		ReasonRateLimited,
		ReasonOther,
	}
	seen := map[string]struct{}{}
	for _, r := range all {
		if _, dup := seen[r]; dup {
			t.Fatalf("duplicate reason label %q", r)
		}
		seen[r] = struct{}{}
	}
	if len(seen) != 7 {
		t.Fatalf("expected 7 reason labels, got %d", len(seen))
	}
}

