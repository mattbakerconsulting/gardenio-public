package embedding

import (
	"context"
	"errors"
)

// ErrDisabled is returned by NoopEmbedder when embedding is not configured.
var ErrDisabled = errors.New("embedding: disabled (BEADS_EMBED_BACKEND=noop)")

// NoopEmbedder is the default Embedder used when embedding is disabled.
// All methods return ErrDisabled; ModelName reports "noop" and Dimensions is 0.
// Callers that wish to gracefully degrade should check for ErrDisabled and
// skip the embedding step.
type NoopEmbedder struct{}

// NewNoopEmbedder returns a NoopEmbedder.
func NewNoopEmbedder() *NoopEmbedder { return &NoopEmbedder{} }

func (NoopEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return nil, ErrDisabled
}

func (NoopEmbedder) EmbedBatch(_ context.Context, _ []string) ([][]float32, error) {
	return nil, ErrDisabled
}

func (NoopEmbedder) ModelName() string { return "noop" }

func (NoopEmbedder) Dimensions() int { return 0 }
