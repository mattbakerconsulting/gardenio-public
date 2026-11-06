package embedding

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/config"
	"gardenio/kbeads/internal/metrics"
)

func TestNewEmbedder_Noop(t *testing.T) {
	for _, backend := range []string{"", "noop"} {
		t.Run("backend="+backend, func(t *testing.T) {
			e, err := NewEmbedder(&config.Config{EmbedBackend: backend})
			if err != nil {
				t.Fatalf("NewEmbedder err = %v, want nil", err)
			}
			if _, ok := e.(*NoopEmbedder); !ok {
				t.Errorf("NewEmbedder returned %T, want *NoopEmbedder", e)
			}
		})
	}
}

func TestNewEmbedder_NilConfig(t *testing.T) {
	e, err := NewEmbedder(nil)
	if err != nil {
		t.Fatalf("NewEmbedder(nil) err = %v, want nil", err)
	}
	if _, ok := e.(*NoopEmbedder); !ok {
		t.Errorf("NewEmbedder(nil) returned %T, want *NoopEmbedder", e)
	}
}

func TestNewEmbedder_OpenAICompat(t *testing.T) {
	cfg := &config.Config{
		EmbedBackend: "openai",
		EmbedURL:     "https://api-litellm.app.devops.acme.example",
		EmbedModel:   "bedrock-titan-embed-v2",
		EmbedAPIKey:  "sk-test",
	}
	e, err := NewEmbedder(cfg)
	if err != nil {
		t.Fatalf("NewEmbedder err = %v, want nil", err)
	}
	// The openai backend is wrapped in a meteredEmbedder so its calls record
	// on the embed_duration/errors metrics; unwrap to inspect the concrete
	// OpenAICompatEmbedder underneath.
	me, ok := e.(*meteredEmbedder)
	if !ok {
		t.Fatalf("NewEmbedder returned %T, want *meteredEmbedder", e)
	}
	if me.backend != metrics.BackendOpenAI {
		t.Errorf("metered backend = %q, want %q", me.backend, metrics.BackendOpenAI)
	}
	oe, ok := me.inner.(*OpenAICompatEmbedder)
	if !ok {
		t.Fatalf("metered inner is %T, want *OpenAICompatEmbedder", me.inner)
	}
	if oe.ModelName() != "bedrock-titan-embed-v2" {
		t.Errorf("ModelName = %q, want bedrock-titan-embed-v2", oe.ModelName())
	}
	if oe.apiKey != "sk-test" {
		t.Errorf("apiKey = %q, want sk-test", oe.apiKey)
	}
}

func TestNewEmbedder_UnknownBackend(t *testing.T) {
	_, err := NewEmbedder(&config.Config{EmbedBackend: "bogus"})
	if err == nil {
		t.Fatal("NewEmbedder err = nil, want error for unknown backend")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("err = %v, want mention of %q", err, "bogus")
	}
}
