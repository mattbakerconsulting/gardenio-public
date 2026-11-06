package embedding

import (
	"fmt"

	"gardenio/kbeads/internal/config"
	"gardenio/kbeads/internal/metrics"
)

// NewEmbedder returns an Embedder implementation selected by cfg.EmbedBackend.
//
// Supported backends:
//   - "noop" (or ""): NoopEmbedder — semantic search disabled.
//   - "openai":       OpenAICompatEmbedder, the canonical path. Speaks the
//     OpenAI /embeddings wire format and points at LiteLLM, which fronts AWS
//     Bedrock Titan (amazon.titan-embed-text-v2:0, 1024-dim) for production.
//     Auth via cfg.EmbedAPIKey → "Authorization: Bearer <key>".
//
// Unknown backend strings return an error so misconfiguration surfaces at
// startup rather than silently degrading to noop.
func NewEmbedder(cfg *config.Config) (Embedder, error) {
	if cfg == nil {
		return NewNoopEmbedder(), nil
	}
	switch cfg.EmbedBackend {
	case "", "noop":
		// NoopEmbedder is deliberately left unmetered: it always returns
		// ErrDisabled, and metering it would inflate _embed_errors_total on
		// every disabled-backend call with a meaningless "noop" backend label.
		return NewNoopEmbedder(), nil
	case "openai":
		return newMeteredEmbedder(
			NewOpenAICompatEmbedder(cfg.EmbedURL, cfg.EmbedModel, cfg.EmbedAPIKey),
			metrics.BackendOpenAI,
		), nil
	default:
		return nil, fmt.Errorf("embedding: unknown backend %q (supported: noop, openai)", cfg.EmbedBackend)
	}
}
