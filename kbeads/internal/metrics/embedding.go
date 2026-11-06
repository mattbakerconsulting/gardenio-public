// Package metrics defines cross-cutting Prometheus metrics shared between
// the embedding package and the HTTP server. Definitions live here so both
// the embedder implementation (internal/embedding) and the server layer can
// record against them without importing each other.
package metrics

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Embedding backend label values.
const (
	BackendNoop   = "noop"
	BackendOpenAI = "openai"
	BackendVoyage = "voyage"
)

// Embedding operation label values.
const (
	OpEmbed      = "embed"
	OpEmbedBatch = "embed_batch"
)

// Embedding error reason label values. Implementations should map transport
// and API errors onto these buckets so the "errors_total" counter has a small,
// bounded label cardinality.
const (
	ReasonTimeout      = "timeout"
	ReasonTransport    = "transport"
	ReasonBadResponse  = "bad_response"
	ReasonEmptyVector  = "empty_vector"
	ReasonUnauthorized = "unauthorized"
	ReasonRateLimited  = "rate_limited"
	ReasonOther        = "other"
)

// Semantic search phase label values for the split-duration histogram.
const (
	PhaseEmbedQuery = "embed_query"
	PhasePgvector   = "pgvector_scan"
	PhaseTotal      = "total"
)

// Package-scoped collectors. They are registered by RegisterEmbedding on a
// prometheus.Registerer. Tests reset by creating a fresh registry.
var (
	EmbedDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gardenio_beads_embed_duration_seconds",
			Help:    "Duration of embedding backend calls.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms .. ~20s
		},
		[]string{"backend", "op"},
	)

	EmbedErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "gardenio_beads_embed_errors_total",
			Help: "Count of embedding backend errors by reason.",
		},
		[]string{"backend", "reason"},
	)

	SemanticSearchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "gardenio_beads_semantic_search_duration_seconds",
			Help:    "End-to-end semantic search latency, split by phase (embed_query, pgvector_scan, total).",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 12),
		},
		[]string{"phase"},
	)

	SemanticSearchResults = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "gardenio_beads_semantic_search_results_total",
			Help:    "Distribution of result counts returned by semantic search.",
			Buckets: []float64{0, 1, 2, 5, 10, 20, 50, 100, 200},
		},
	)
)

// EmbeddingStats is the minimal snapshot needed to populate the coverage and
// stale gauges. It mirrors model.EmbeddingStats (which lives in the in-progress
// store work) so this package has no dependency on the model package yet —
// the server layer translates when wiring is done.
type EmbeddingStats struct {
	Fleet         string
	TotalBeads    int
	EmbeddedBeads int
	StaleCount    int
}

// StatsProvider returns per-fleet embedding statistics. Implementations MUST
// be non-blocking on the scrape path or rely on the collector's own cache.
type StatsProvider interface {
	EmbeddingStats(ctx context.Context) ([]EmbeddingStats, error)
}

// NoopStatsProvider reports no stats. Used as a default before the real
// store-backed provider is wired.
type NoopStatsProvider struct{}

func (NoopStatsProvider) EmbeddingStats(_ context.Context) ([]EmbeddingStats, error) {
	return nil, nil
}

// embeddingRefreshTimeout bounds a single background refresh of the embedding
// stats. Like the fleet collector's deadline this is NOT on the scrape path —
// Collect() only reads the cached snapshot — so a saturated DB cannot hang a
// scrape; it only makes the staleness gauge climb (ka-0000006261).
const embeddingRefreshTimeout = 25 * time.Second

// EmbeddingStatsCollector exposes coverage and stale-count gauges, computed
// from a StatsProvider. The provider is queried on a background ticker (see
// Start) — NOT on the scrape path. Collect() serves only the last cached
// snapshot plus a staleness gauge, so scrape latency is independent of DB
// latency. On provider error the previous snapshot is reused and staleness
// keeps climbing.
type EmbeddingStatsCollector struct {
	provider StatsProvider
	ttl      time.Duration
	timeout  time.Duration
	started  time.Time

	coverageDesc  *prometheus.Desc
	staleDesc     *prometheus.Desc
	stalenessDesc *prometheus.Desc

	mu       sync.RWMutex
	lastAt   time.Time // time of last SUCCESSFUL refresh (zero until first success)
	snapshot []EmbeddingStats
}

func NewEmbeddingStatsCollector(p StatsProvider, ttl time.Duration) *EmbeddingStatsCollector {
	if p == nil {
		p = NoopStatsProvider{}
	}
	return &EmbeddingStatsCollector{
		provider: p,
		ttl:      ttl,
		timeout:  embeddingRefreshTimeout,
		started:  time.Now(),
		coverageDesc: prometheus.NewDesc(
			"gardenio_beads_embedding_coverage_ratio",
			"Fraction of beads with up-to-date embeddings (embedded / total). 1.0 = fully covered.",
			[]string{"fleet"}, nil,
		),
		staleDesc: prometheus.NewDesc(
			"gardenio_beads_embedding_stale_count",
			"Number of beads whose embedding hash no longer matches the current text.",
			[]string{"fleet"}, nil,
		),
		stalenessDesc: prometheus.NewDesc(
			"gardenio_beads_embedding_stats_staleness_seconds",
			"Age of the embedding-stats snapshot in seconds (time since the last successful background refresh). Climbs without bound if the refresher is stalled — alert on it.",
			nil, nil,
		),
	}
}

func (c *EmbeddingStatsCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.coverageDesc
	ch <- c.staleDesc
	ch <- c.stalenessDesc
}

// Collect serves ONLY the last cached snapshot — it never queries the
// provider, so scrape latency is bounded regardless of DB health.
func (c *EmbeddingStatsCollector) Collect(ch chan<- prometheus.Metric) {
	snap, lastAt := c.currentSnapshot()

	ch <- prometheus.MustNewConstMetric(c.stalenessDesc, prometheus.GaugeValue, c.stalenessSeconds(lastAt))

	for _, s := range snap {
		var ratio float64
		if s.TotalBeads > 0 {
			ratio = float64(s.EmbeddedBeads) / float64(s.TotalBeads)
		}
		ch <- prometheus.MustNewConstMetric(c.coverageDesc, prometheus.GaugeValue, ratio, s.Fleet)
		ch <- prometheus.MustNewConstMetric(c.staleDesc, prometheus.GaugeValue, float64(s.StaleCount), s.Fleet)
	}
}

func (c *EmbeddingStatsCollector) stalenessSeconds(lastAt time.Time) float64 {
	ref := lastAt
	if ref.IsZero() {
		ref = c.started
	}
	if ref.IsZero() {
		return 0
	}
	return time.Since(ref).Seconds()
}

func (c *EmbeddingStatsCollector) currentSnapshot() ([]EmbeddingStats, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot, c.lastAt
}

// Start populates the cache once SYNCHRONOUSLY (so the first scrape after boot
// — and the unit tests — see data immediately, off the scrape path), then
// launches a background goroutine that refreshes on every TTL tick until ctx
// is cancelled. This is the ONLY place the provider is queried.
func (c *EmbeddingStatsCollector) Start(ctx context.Context) {
	c.refresh(ctx)
	go c.tickLoop(ctx)
}

func (c *EmbeddingStatsCollector) tickLoop(ctx context.Context) {
	if c.ttl <= 0 {
		// A non-positive TTL means "no periodic refresh" (used by tests that
		// only exercise a single scrape). Start() already did the one-shot
		// refresh; time.NewTicker would panic on a non-positive interval.
		return
	}
	ticker := time.NewTicker(c.ttl)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.refresh(ctx)
		}
	}
}

func (c *EmbeddingStatsCollector) refresh(ctx context.Context) {
	fetchCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	snap, err := c.provider.EmbeddingStats(fetchCtx)
	if err != nil {
		slog.Warn("embedding stats refresh failed; retaining stale snapshot", "error", err)
		return
	}
	c.mu.Lock()
	c.snapshot = snap
	c.lastAt = time.Now()
	c.mu.Unlock()
}

// RegisterEmbedding registers all embedding-pipeline metrics and the stats
// collector on the given registry, and starts the stats collector's background
// refresher bound to ctx. Safe to call once per registry.
func RegisterEmbedding(ctx context.Context, reg prometheus.Registerer, provider StatsProvider, ttl time.Duration) {
	c := NewEmbeddingStatsCollector(provider, ttl)
	c.Start(ctx)
	reg.MustRegister(
		EmbedDuration,
		EmbedErrors,
		SemanticSearchDuration,
		SemanticSearchResults,
		c,
	)
}

// ObserveEmbed records a single embedding call. Intended to be called by the
// Embedder implementations (openai/bedrock-titan/voyage) via a timer.
func ObserveEmbed(backend, op string, start time.Time, err error) {
	EmbedDuration.WithLabelValues(backend, op).Observe(time.Since(start).Seconds())
	if err != nil {
		EmbedErrors.WithLabelValues(backend, classifyEmbedError(err)).Inc()
	}
}

// classifyEmbedError maps an error onto one of the ReasonXxx label values.
// Implementations can override by checking error types first and only falling
// back here when they want the default mapping.
func classifyEmbedError(err error) string {
	if err == nil {
		return ReasonOther
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "context deadline exceeded"), strings.Contains(s, "timeout"):
		return ReasonTimeout
	case strings.Contains(s, "401"), strings.Contains(s, "unauthorized"):
		return ReasonUnauthorized
	case strings.Contains(s, "429"), strings.Contains(s, "rate limit"):
		return ReasonRateLimited
	case strings.Contains(s, "empty"):
		return ReasonEmptyVector
	case strings.Contains(s, "status"), strings.Contains(s, "decode"):
		return ReasonBadResponse
	case strings.Contains(s, "connection"), strings.Contains(s, "dial"), strings.Contains(s, "eof"):
		return ReasonTransport
	}
	return ReasonOther
}
