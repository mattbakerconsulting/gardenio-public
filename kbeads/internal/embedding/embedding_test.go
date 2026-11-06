package embedding

import (
	"context"
	"errors"
	"testing"
)

// Ensure NoopEmbedder satisfies the Embedder interface.
var _ Embedder = (*NoopEmbedder)(nil)

func TestNoopEmbedder(t *testing.T) {
	e := NewNoopEmbedder()
	if e.ModelName() != "noop" {
		t.Errorf("ModelName = %q, want noop", e.ModelName())
	}
	if e.Dimensions() != 0 {
		t.Errorf("Dimensions = %d, want 0", e.Dimensions())
	}
	if _, err := e.Embed(context.Background(), "x"); !errors.Is(err, ErrDisabled) {
		t.Errorf("Embed err = %v, want ErrDisabled", err)
	}
	if _, err := e.EmbedBatch(context.Background(), []string{"x"}); !errors.Is(err, ErrDisabled) {
		t.Errorf("EmbedBatch err = %v, want ErrDisabled", err)
	}
}
