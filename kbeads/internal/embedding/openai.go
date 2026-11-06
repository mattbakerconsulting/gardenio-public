package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultOpenAIDims = 1024
)

// OpenAICompatEmbedder calls any OpenAI /embeddings-compatible endpoint.
// This covers direct OpenAI, LiteLLM (which re-exports Bedrock/Voyage/etc.
// under the same wire format), and any other proxy that speaks the
// OpenAI embeddings API shape.
type OpenAICompatEmbedder struct {
	baseURL string
	model   string
	apiKey  string
	dims    int
	client  *http.Client
}

// NewOpenAICompatEmbedder returns an Embedder for OpenAI-compatible endpoints.
// baseURL is the root URL of the provider (e.g. "https://api-litellm.app.devops.acme.example").
// The endpoint path "/embeddings" is appended internally.
// apiKey is sent as "Authorization: Bearer <apiKey>"; pass an empty string to omit.
func NewOpenAICompatEmbedder(baseURL, model, apiKey string) *OpenAICompatEmbedder {
	return &OpenAICompatEmbedder{
		baseURL: baseURL,
		model:   model,
		apiKey:  apiKey,
		dims:    defaultOpenAIDims,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

// WithDimensions overrides the reported dimensionality.
func (o *OpenAICompatEmbedder) WithDimensions(dims int) *OpenAICompatEmbedder {
	o.dims = dims
	return o
}

// WithHTTPClient overrides the default http.Client. Used by tests.
func (o *OpenAICompatEmbedder) WithHTTPClient(c *http.Client) *OpenAICompatEmbedder {
	o.client = c
	return o
}

func (o *OpenAICompatEmbedder) ModelName() string { return o.model }
func (o *OpenAICompatEmbedder) Dimensions() int   { return o.dims }

// Embed returns a vector for the given text.
func (o *OpenAICompatEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	vectors, err := o.callEmbed(ctx, text)
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, fmt.Errorf("openai embed: empty response")
	}
	return vectors[0], nil
}

// EmbedBatch returns vectors for the given texts. The OpenAI /embeddings API
// accepts an array input natively, so this is a single HTTP call.
func (o *OpenAICompatEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	vectors, err := o.callEmbed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, fmt.Errorf("openai embed: want %d vectors, got %d", len(texts), len(vectors))
	}
	return vectors, nil
}

func (o *OpenAICompatEmbedder) callEmbed(ctx context.Context, input any) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{
		"model": o.model,
		"input": input,
	})
	if err != nil {
		return nil, fmt.Errorf("openai embed: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai embed: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if o.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+o.apiKey)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai embed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("openai embed: status %d: %s", resp.StatusCode, string(snippet))
	}

	// OpenAI /embeddings response:
	//   { "data": [{ "embedding": [...], "index": 0 }, ...],
	//     "model": "...", "usage": {...} }
	// LiteLLM mirrors this shape exactly for all its proxied backends
	// (Bedrock, Voyage, OpenAI, etc.), so a single decoder covers them all.
	var result struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     int       `json:"index"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("openai embed: decode response: %w", err)
	}

	vectors := make([][]float32, len(result.Data))
	for _, d := range result.Data {
		if d.Index < 0 || d.Index >= len(result.Data) {
			return nil, fmt.Errorf("openai embed: out-of-range index %d (have %d items)", d.Index, len(result.Data))
		}
		vectors[d.Index] = d.Embedding
	}
	return vectors, nil
}
