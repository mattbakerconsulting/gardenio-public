package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// VMAGENT_URL injection (context-crisis Part B, epic ka-0000003733). gb prime,
// gb session, and the session-resume check all gate their metric pushes on
// VMAGENT_URL; before this the controller never set it on agent pods, so those
// series never landed in VictoriaMetrics. buildAgentPodSpec must inject
// VMAGENT_URL into the pod env iff cfg.AgentVmagentURL is non-empty, and leave
// it unset otherwise (preserving the pre-existing silently-skipped behavior).

func eventForVmagent() subscriber.Event {
	return subscriber.Event{
		Project:   "gardenio",
		Role:      "thread",
		AgentName: "agent1",
		Metadata:  map[string]string{},
	}
}

// TestVmagentURL_InjectedWhenSet: a non-empty AgentVmagentURL is injected as
// VMAGENT_URL into the agent pod env verbatim.
func TestVmagentURL_InjectedWhenSet(t *testing.T) {
	cfg := &config.Config{AgentVmagentURL: "http://gardenio-vmagent:8429"}
	spec := buildAgentPodSpec(cfg, eventForVmagent())
	if got := spec.Env["VMAGENT_URL"]; got != "http://gardenio-vmagent:8429" {
		t.Errorf("VMAGENT_URL: got %q, want %q", got, "http://gardenio-vmagent:8429")
	}
}

// TestVmagentURL_AbsentWhenUnset: an empty AgentVmagentURL leaves VMAGENT_URL
// unset on the agent pod — the pushes stay silently skipped, exactly as before.
func TestVmagentURL_AbsentWhenUnset(t *testing.T) {
	cfg := &config.Config{}
	spec := buildAgentPodSpec(cfg, eventForVmagent())
	if v, ok := spec.Env["VMAGENT_URL"]; ok {
		t.Errorf("VMAGENT_URL: expected unset, got %q", v)
	}
}
