package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/podmanager"
)

// TestModelCostTag_EndToEnd_OverrideEffective proves the WHOLE build path — not
// just the helper — books spend under the override-effective model. A role model
// override on an opus-default account must produce x-litellm-tags model:<role
// model>, so LiteLLM's by-model cost view attributes fable spend to fable
// (ka-0000003806). Uses the shared model-override harness (newModelCfg /
// eventForModel).
func TestModelCostTag_EndToEnd_OverrideEffective(t *testing.T) {
	// Role "crew" overrides to fable on the opus-defaulted "gardenio" account.
	cfg := newModelCfg("", "claude-fable-5")
	spec := buildAgentPodSpec(cfg, eventForModel(nil))

	header := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]
	if !strings.Contains(header, "model:claude-fable-5") {
		t.Errorf("litellm tag not re-tagged to override model; header:\n%s", header)
	}
	if strings.Contains(header, "model:account-default-model") {
		t.Errorf("stale account-default model tag present after override; header:\n%s", header)
	}
	// Sanity: the pod really did resolve to the override model.
	assertModel(t, spec, "claude-fable-5", true)
}

// TestModelCostTag_EndToEnd_NoOverrideKeepsDefault proves the seeded account-default
// tag is preserved when NO override is in play — the fix only re-tags when an
// override actually changes the model, so the common path is unchanged.
func TestModelCostTag_EndToEnd_NoOverrideKeepsDefault(t *testing.T) {
	cfg := newModelCfg("", "") // no project/role override
	spec := buildAgentPodSpec(cfg, eventForModel(nil))
	header := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]
	if !strings.Contains(header, "model:account-default-model") {
		t.Errorf("account-default model tag missing on the no-override path:\n%s", header)
	}
}

// TestRetagLiteLLMModel_OverrideEffective proves the fable-cost-attribution fix
// (ka-0000003806): a role/agent model override rewrites the model: tag in
// x-litellm-tags to the override-effective model, so spend books under model:fable
// instead of the opus account default that applyAccountConfig seeded.
func TestRetagLiteLLMModel_OverrideEffective(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Env: map[string]string{
		// As applyAccountConfig would have baked it for an opus-default account.
		"ANTHROPIC_CUSTOM_HEADERS": "x-litellm-api-key: Bearer sk-key\n" +
			"x-litellm-tags: agent:thread-1,role:thread,project:gardenio,model:claude-opus-4-8",
	}}

	retagLiteLLMModel(spec, "claude-fable-5")

	got := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]
	if !strings.Contains(got, "model:claude-fable-5") {
		t.Errorf("expected model:claude-fable-5 in header, got:\n%s", got)
	}
	if strings.Contains(got, "model:claude-opus-4-8") {
		t.Errorf("stale account-default model tag still present:\n%s", got)
	}
	// The other tags and the api-key line must be untouched.
	for _, want := range []string{"agent:thread-1", "role:thread", "project:gardenio", "x-litellm-api-key: Bearer sk-key"} {
		if !strings.Contains(got, want) {
			t.Errorf("retag dropped %q:\n%s", want, got)
		}
	}
	// Exactly one model: tag survives.
	if n := strings.Count(got, "model:"); n != 1 {
		t.Errorf("model: tag count = %d, want 1:\n%s", n, got)
	}
}

// TestRetagLiteLLMModel_AppendsWhenAbsent proves that if the tags line carries no
// model: entry (older/edge specs), retag appends one rather than dropping the
// attribution dimension.
func TestRetagLiteLLMModel_AppendsWhenAbsent(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Env: map[string]string{
		"ANTHROPIC_CUSTOM_HEADERS": "x-litellm-api-key: Bearer sk-key\n" +
			"x-litellm-tags: agent:thread-1,role:thread",
	}}
	retagLiteLLMModel(spec, "claude-fable-5")
	got := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]
	if !strings.Contains(got, "model:claude-fable-5") {
		t.Errorf("expected appended model:claude-fable-5:\n%s", got)
	}
}

// TestRetagLiteLLMModel_Sanitizes proves the value is comma-sanitized like every
// other tag, so a comma in a model name can never split the tag list.
func TestRetagLiteLLMModel_Sanitizes(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Env: map[string]string{
		"ANTHROPIC_CUSTOM_HEADERS": "x-litellm-tags: model:old",
	}}
	retagLiteLLMModel(spec, "weird,model")
	got := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]
	want := "model:" + podmanager.SanitizeRole("weird,model")
	if !strings.Contains(got, want) {
		t.Errorf("expected sanitized %q:\n%s", want, got)
	}
	// The comma in the model name must not have created an extra tag.
	if strings.Count(got, ",") != 0 {
		t.Errorf("sanitization failed — stray comma split the tag list:\n%s", got)
	}
}

// TestRetagLiteLLMModel_NoopWithoutHeaders proves a keyless account (no
// ANTHROPIC_CUSTOM_HEADERS) or empty model is left untouched — no panic, no header
// materialized.
func TestRetagLiteLLMModel_NoopWithoutHeaders(t *testing.T) {
	// No headers at all.
	spec := &podmanager.AgentPodSpec{Env: map[string]string{}}
	retagLiteLLMModel(spec, "claude-fable-5")
	if _, ok := spec.Env["ANTHROPIC_CUSTOM_HEADERS"]; ok {
		t.Error("retag materialized a header on a keyless account")
	}
	// Empty model is a no-op even when a header exists.
	spec2 := &podmanager.AgentPodSpec{Env: map[string]string{
		"ANTHROPIC_CUSTOM_HEADERS": "x-litellm-tags: model:opus",
	}}
	retagLiteLLMModel(spec2, "")
	if got := spec2.Env["ANTHROPIC_CUSTOM_HEADERS"]; got != "x-litellm-tags: model:opus" {
		t.Errorf("empty model should be a no-op, got: %s", got)
	}
	// Nil Env must not panic.
	retagLiteLLMModel(&podmanager.AgentPodSpec{}, "claude-fable-5")
}
