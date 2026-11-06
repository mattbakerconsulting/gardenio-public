// Package embedding provides text-to-vector embedding primitives used by
// kbeads' semantic search pipeline. See kbeads/docs/design-embedding-search.md
// for the overall design.
package embedding

import "context"

// Embedder generates vector embeddings from text.
type Embedder interface {
	// Embed returns a float32 vector for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch returns vectors for multiple texts. Implementations should
	// batch API calls where the provider supports it.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// ModelName returns the identifier stored in bead_embeddings.model.
	ModelName() string

	// Dimensions returns the vector dimensionality (e.g. 1024, 1536).
	Dimensions() int
}
