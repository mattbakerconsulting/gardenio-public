package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// TestBuildSpecFromBeadInfo_SurfacesWakeReason verifies the reconciler recreate
// path surfaces a parked identity's wake_reason to the pod as BOAT_WAKE_REASON
// (persistent-agents Phase 3 event-wake, ka-0000000027). This is the primary
// path: an event-wake / scanner reopen leaves the bead carrying wake_reason, and
// the reconciler recreates the same-named pod from the bead's fields (copied into
// metadata by ListAgentBeads).
func TestBuildSpecFromBeadInfo_SurfacesWakeReason(t *testing.T) {
	cfg := &config.Config{Namespace: "test", CoopImage: "agent:latest"}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "persist-agent", map[string]string{
		beadsapi.FieldWakeReason: beadsapi.WakeReasonDecision,
	})
	if got := spec.Env["BOAT_WAKE_REASON"]; got != beadsapi.WakeReasonDecision {
		t.Errorf("BOAT_WAKE_REASON = %q; want %q", got, beadsapi.WakeReasonDecision)
	}
}

// TestBuildSpecFromBeadInfo_NoWakeReasonWhenAbsent confirms the env var is only
// set when a wake_reason is present — an ordinary (never-parked) spawn does not
// carry a spurious BOAT_WAKE_REASON.
func TestBuildSpecFromBeadInfo_NoWakeReasonWhenAbsent(t *testing.T) {
	cfg := &config.Config{Namespace: "test", CoopImage: "agent:latest"}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "fresh-agent", map[string]string{})
	if v, present := spec.Env["BOAT_WAKE_REASON"]; present {
		t.Errorf("BOAT_WAKE_REASON should be unset for a non-wake spawn, got %q", v)
	}
}

// TestBuildAgentPodSpec_SurfacesWakeReason verifies the events (spawn) path
// surfaces wake_reason identically, so a wake routed through either spec builder
// reaches the pod the same way.
func TestBuildAgentPodSpec_SurfacesWakeReason(t *testing.T) {
	cfg := &config.Config{Namespace: "test", CoopImage: "agent:latest"}
	ev := subscriber.Event{
		Project:   "gardenio",
		Role:      "crew",
		AgentName: "resumed-agent",
		Metadata:  map[string]string{beadsapi.FieldWakeReason: beadsapi.WakeReasonManual},
	}
	spec := buildAgentPodSpec(cfg, ev)
	if got := spec.Env["BOAT_WAKE_REASON"]; got != beadsapi.WakeReasonManual {
		t.Errorf("BOAT_WAKE_REASON = %q; want %q", got, beadsapi.WakeReasonManual)
	}
}
