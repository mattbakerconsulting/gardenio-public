package tests

import (
	"strings"
	"testing"
)

// --- beads.embedding: OpenAI/LiteLLM env vars (no sidecar) ---

func TestBeadsEmbedding_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	if strings.Contains(out, "BEADS_EMBED_BACKEND") {
		t.Error("BEADS_EMBED_BACKEND should NOT be present when embedding is disabled")
	}
}

func TestBeadsEmbedding_EnabledSetsOpenAIEnvVars(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"beads.embedding.enabled=true",
		"beads.embedding.apiKeySecret.name=litellm-master-key",
	)
	for _, want := range []string{
		"BEADS_EMBED_BACKEND",
		`value: "openai"`,
		"BEADS_EMBED_URL",
		`value: "https://litellm.example.com"`,
		"BEADS_EMBED_MODEL",
		`value: "bedrock-titan-embed-v2"`,
		"BEADS_EMBED_API_KEY",
		"secretKeyRef:",
		`name: "litellm-master-key"`,
		`key: "api-key"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected rendered deployment to contain %q when embedding is enabled", want)
		}
	}
}

func TestBeadsEmbedding_CustomModel(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"beads.embedding.enabled=true",
		"beads.embedding.model=text-embedding-3-small",
	)
	if !strings.Contains(out, `value: "text-embedding-3-small"`) {
		t.Error("BEADS_EMBED_MODEL should reflect the custom model")
	}
}
