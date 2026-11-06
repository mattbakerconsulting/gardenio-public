package embedding

import (
	"context"
	"time"

	"gardenio/kbeads/internal/metrics"
)

// meteredEmbedder decorates an Embedder so every backend call records a sample
// on the gardenio_beads_embed_duration_seconds / _errors_total metric families.
// The concrete embedders (openai) deliberately do not import the metrics
// package; wrapping them here keeps the observation call-site in one place and
// carries the correct backend label (metrics.Backend*) supplied by the factory.
type meteredEmbedder struct {
	inner   Embedder
	backend string
}

// newMeteredEmbedder wraps inner so its Embed/EmbedBatch calls are timed and
// their errors classified. backend must be one of the metrics.Backend* label
// values (e.g. metrics.BackendOpenAI).
func newMeteredEmbedder(inner Embedder, backend string) *meteredEmbedder {
	return &meteredEmbedder{inner: inner, backend: backend}
}

func (m *meteredEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	start := time.Now()
	vec, err := m.inner.Embed(ctx, text)
	metrics.ObserveEmbed(m.backend, metrics.OpEmbed, start, err)
	return vec, err
}

func (m *meteredEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	start := time.Now()
	vecs, err := m.inner.EmbedBatch(ctx, texts)
	metrics.ObserveEmbed(m.backend, metrics.OpEmbedBatch, start, err)
	return vecs, err
}

func (m *meteredEmbedder) ModelName() string { return m.inner.ModelName() }
func (m *meteredEmbedder) Dimensions() int   { return m.inner.Dimensions() }
