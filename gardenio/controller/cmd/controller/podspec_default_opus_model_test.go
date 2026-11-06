package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// ANTHROPIC_DEFAULT_OPUS_MODEL injection (plan:opus55-release T1). Same
// escape-hatch mechanism as the fable pin (podspec_default_fable_model_test.go):
// the Claude Code harness `opus` alias otherwise resolves — via its bundled
// model catalog — to a bare id the fleet's LiteLLM virtual-key ACL rejects,
// so buildAgentPodSpec must inject ANTHROPIC_DEFAULT_OPUS_MODEL from
// cfg.AgentDefaultOpusModel iff non-empty, and leave it unset when the config
// is empty (the disable escape hatch).

func eventForOpusModel() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "agent1",
		Metadata:  map[string]string{},
	}
}

// TestDefaultOpusModel_InjectedWhenSet: a non-empty AgentDefaultOpusModel is
// injected as ANTHROPIC_DEFAULT_OPUS_MODEL into the agent pod env verbatim.
func TestDefaultOpusModel_InjectedWhenSet(t *testing.T) {
	cfg := &config.Config{AgentDefaultOpusModel: "claude-opus-5-5-oauth"}
	spec := buildAgentPodSpec(cfg, eventForOpusModel())
	if got := spec.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"]; got != "claude-opus-5-5-oauth" {
		t.Errorf("ANTHROPIC_DEFAULT_OPUS_MODEL: got %q, want %q", got, "claude-opus-5-5-oauth")
	}
}

// TestDefaultOpusModel_AbsentWhenUnset: an empty AgentDefaultOpusModel leaves
// ANTHROPIC_DEFAULT_OPUS_MODEL unset on the agent pod, so the harness keeps its
// own alias resolution (the operator escape hatch).
func TestDefaultOpusModel_AbsentWhenUnset(t *testing.T) {
	cfg := &config.Config{}
	spec := buildAgentPodSpec(cfg, eventForOpusModel())
	if v, ok := spec.Env["ANTHROPIC_DEFAULT_OPUS_MODEL"]; ok {
		t.Errorf("ANTHROPIC_DEFAULT_OPUS_MODEL: expected unset, got %q", v)
	}
}
