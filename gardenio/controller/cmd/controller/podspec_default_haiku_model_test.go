package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// ANTHROPIC_DEFAULT_HAIKU_MODEL injection (ka-0000005599). The Claude Code
// harness `haiku` alias otherwise resolves — via its bundled model catalog — to
// the DATED snapshot id "claude-haiku-4-5-20251001" the fleet's LiteLLM
// virtual-key ACL rejects (401 "Tried to access claude-haiku-4-5-20251001"), so
// a haiku crew is dead on arrival (and the harness's small-model ops like
// WebFetch 401 silently). buildAgentPodSpec must inject
// ANTHROPIC_DEFAULT_HAIKU_MODEL from cfg.AgentDefaultHaikuModel iff non-empty,
// and leave it unset when the config is empty (the disable escape hatch).

func eventForHaikuModel() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "agent1",
		Metadata:  map[string]string{},
	}
}

// TestDefaultHaikuModel_InjectedWhenSet: a non-empty AgentDefaultHaikuModel is
// injected as ANTHROPIC_DEFAULT_HAIKU_MODEL into the agent pod env verbatim. The
// value is asserted verbatim: the point is that it must be the rolling
// "anthropic/claude-haiku-4-5" group the key ACL allows, NOT the dated snapshot.
func TestDefaultHaikuModel_InjectedWhenSet(t *testing.T) {
	cfg := &config.Config{AgentDefaultHaikuModel: "anthropic/claude-haiku-4-5"}
	spec := buildAgentPodSpec(cfg, eventForHaikuModel())
	if got := spec.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("ANTHROPIC_DEFAULT_HAIKU_MODEL: got %q, want %q", got, "anthropic/claude-haiku-4-5")
	}
}

// TestDefaultHaikuModel_AbsentWhenUnset: an empty AgentDefaultHaikuModel leaves
// ANTHROPIC_DEFAULT_HAIKU_MODEL unset on the agent pod, so the harness keeps its
// own alias resolution (the operator escape hatch).
func TestDefaultHaikuModel_AbsentWhenUnset(t *testing.T) {
	cfg := &config.Config{}
	spec := buildAgentPodSpec(cfg, eventForHaikuModel())
	if v, ok := spec.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"]; ok {
		t.Errorf("ANTHROPIC_DEFAULT_HAIKU_MODEL: expected unset, got %q", v)
	}
}

// TestDefaultHaikuModel_DoesNotTouchSessionModel: the haiku-alias pin is the
// alias→model mapping, NOT the session model — it must not overwrite
// ANTHROPIC_MODEL / CLAUDE_MODEL, which carry the account/role/agent model (and
// the BOAT_MODEL_PINNED pin rides those). Regression guard so a future edit
// can't accidentally couple the two.
func TestDefaultHaikuModel_DoesNotTouchSessionModel(t *testing.T) {
	cfg := &config.Config{
		AgentDefaultHaikuModel: "anthropic/claude-haiku-4-5",
		ClaudeModel:            "claude-opus-4-8-oauth",
	}
	spec := buildAgentPodSpec(cfg, eventForHaikuModel())
	if got := spec.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("ANTHROPIC_DEFAULT_HAIKU_MODEL: got %q, want %q", got, "anthropic/claude-haiku-4-5")
	}
	if got := spec.Env["CLAUDE_MODEL"]; got != "claude-opus-4-8-oauth" {
		t.Errorf("CLAUDE_MODEL: haiku pin must not overwrite the session model; got %q", got)
	}
}

// TestDefaultHaikuModel_IndependentOfFablePin: the haiku and fable pins are
// separate env vars driven by separate config fields; injecting one must not
// affect the other. Guards against a copy-paste regression that couples them.
func TestDefaultHaikuModel_IndependentOfFablePin(t *testing.T) {
	cfg := &config.Config{
		AgentDefaultHaikuModel: "anthropic/claude-haiku-4-5",
		AgentDefaultFableModel: "claude-fable-5-oauth",
	}
	spec := buildAgentPodSpec(cfg, eventForHaikuModel())
	if got := spec.Env["ANTHROPIC_DEFAULT_HAIKU_MODEL"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("ANTHROPIC_DEFAULT_HAIKU_MODEL: got %q, want %q", got, "anthropic/claude-haiku-4-5")
	}
	if got := spec.Env["ANTHROPIC_DEFAULT_FABLE_MODEL"]; got != "claude-fable-5-oauth" {
		t.Errorf("ANTHROPIC_DEFAULT_FABLE_MODEL: got %q, want %q", got, "claude-fable-5-oauth")
	}
}
