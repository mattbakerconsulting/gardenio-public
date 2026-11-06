package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// ANTHROPIC_DEFAULT_FABLE_MODEL injection (ka-0000003175). The Claude Code
// harness `fable` alias otherwise resolves — via its bundled model catalog — to
// a bare "claude-sonnet-5" id the fleet's LiteLLM virtual-key ACL rejects (401
// "Tried to access claude-sonnet-5"), so a fable crew is dead on arrival.
// buildAgentPodSpec must inject ANTHROPIC_DEFAULT_FABLE_MODEL from
// cfg.AgentDefaultFableModel iff non-empty, and leave it unset when the config
// is empty (the disable escape hatch).

func eventForFableModel() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "agent1",
		Metadata:  map[string]string{},
	}
}

// TestDefaultFableModel_InjectedWhenSet: a non-empty AgentDefaultFableModel is
// injected as ANTHROPIC_DEFAULT_FABLE_MODEL into the agent pod env verbatim.
func TestDefaultFableModel_InjectedWhenSet(t *testing.T) {
	cfg := &config.Config{AgentDefaultFableModel: "claude-fable-5-oauth"}
	spec := buildAgentPodSpec(cfg, eventForFableModel())
	if got := spec.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"]; got != "claude-fable-5-oauth" {
		t.Errorf("ANTHROPIC_DEFAULT_FABLE_MODEL: got %q, want %q", got, "claude-fable-5-oauth")
	}
}

// TestDefaultFableModel_AbsentWhenUnset: an empty AgentDefaultFableModel leaves
// ANTHROPIC_DEFAULT_FABLE_MODEL unset on the agent pod, so the harness keeps its
// own alias resolution (the operator escape hatch).
func TestDefaultFableModel_AbsentWhenUnset(t *testing.T) {
	cfg := &config.Config{}
	spec := buildAgentPodSpec(cfg, eventForFableModel())
	if v, ok := spec.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"]; ok {
		t.Errorf("ANTHROPIC_DEFAULT_FABLE_MODEL: expected unset, got %q", v)
	}
}

// TestDefaultFableModel_DoesNotTouchSessionModel: the fable-alias pin is the
// alias→model mapping, NOT the session model — it must not overwrite
// ANTHROPIC_MODEL / CLAUDE_MODEL, which carry the account/role/agent model
// (and the BOAT_MODEL_PINNED pin rides those). Regression guard so a future
// edit can't accidentally couple the two.
func TestDefaultFableModel_DoesNotTouchSessionModel(t *testing.T) {
	cfg := &config.Config{
		AgentDefaultFableModel: "claude-fable-5-oauth",
		ClaudeModel:            "claude-opus-4-8-oauth",
	}
	spec := buildAgentPodSpec(cfg, eventForFableModel())
	if got := spec.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"]; got != "claude-fable-5-oauth" {
		t.Errorf("ANTHROPIC_DEFAULT_FABLE_MODEL: got %q, want %q", got, "claude-fable-5-oauth")
	}
	if got := spec.Env["CLAUDE_MODEL"]; got != "claude-opus-4-8-oauth" {
		t.Errorf("CLAUDE_MODEL: fable pin must not overwrite the session model; got %q", got)
	}
}
