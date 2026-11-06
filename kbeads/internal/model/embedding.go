package model

// SemanticResult is a bead paired with its similarity score from a vector search.
type SemanticResult struct {
	Bead     *Bead   `json:"bead"`
	Score    float64 `json:"score"`    // cosine similarity (0..1, higher = more similar)
	Distance float64 `json:"distance"` // cosine distance (1 - score)
}

// EmbeddingStats reports the coverage and freshness of the embedding index.
type EmbeddingStats struct {
	TotalBeads    int    `json:"total_beads"`
	EmbeddedBeads int    `json:"embedded_beads"`
	Model         string `json:"model"`
	StaleCount    int    `json:"stale_count"`
}
