package main

import (
	"testing"

	"gardenio/controller/internal/statusreporter"
	"gardenio/controller/internal/subscriber"
)

// TestTerminalReportPhase_FrozenStopReportsNothing is the regression guard for
// bug ka-0000007439: a SUCCESSFULLY-frozen graceful stop must report NO terminal
// phase, so the agent bead stays OPEN at agent_state=stopping (the name-keyed
// frozen-awaiting-thaw signal). Reporting "Stopped" would map to
// agent_state=done (see the companion assertion below), overwrite the signal,
// and defeat the freeze — the wake path would cold-start a differently-named pod
// and the name-keyed thaw (the sole RecordResumeLatency call site) would never
// fire, so gardenio_resume_latency_seconds would emit zero samples.
func TestTerminalReportPhase_FrozenStopReportsNothing(t *testing.T) {
	tests := []struct {
		name      string
		eventType subscriber.EventType
		frozen    bool
		want      string
	}{
		// The fix: a frozen stop reports nothing (bead stays at stopping).
		{"frozen stop reports nothing", subscriber.AgentStop, true, ""},
		// A stop whose freeze failed (or a non-runsc agent) still reports Stopped→done.
		{"non-frozen stop reports Stopped", subscriber.AgentStop, false, "Stopped"},
		// frozen is only ever set on AgentStop, but assert Done/Kill ignore it.
		{"done ignores frozen", subscriber.AgentDone, true, "Succeeded"},
		{"done reports Succeeded", subscriber.AgentDone, false, "Succeeded"},
		{"kill ignores frozen", subscriber.AgentKill, true, "Failed"},
		{"kill reports Failed", subscriber.AgentKill, false, "Failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := terminalReportPhase(tt.eventType, tt.frozen); got != tt.want {
				t.Errorf("terminalReportPhase(%q, frozen=%v) = %q, want %q",
					tt.eventType, tt.frozen, got, tt.want)
			}
		})
	}
}

// TestTerminalReportPhase_StoppedWouldClobberStopping pins the exact reason the
// frozen case must skip the report: the "Stopped" phase this function would
// otherwise return maps to agent_state=done, which overwrites the "stopping"
// thaw target. If PhaseToAgentState ever stops mapping "Stopped"→"done", the
// clobber (and this fix's necessity) changes — this test surfaces that.
func TestTerminalReportPhase_StoppedWouldClobberStopping(t *testing.T) {
	nonFrozen := terminalReportPhase(subscriber.AgentStop, false)
	if got := statusreporter.PhaseToAgentState(nonFrozen); got != "done" {
		t.Fatalf("PhaseToAgentState(%q) = %q, want \"done\" — a non-frozen stop must still land the bead at agent_state=done",
			nonFrozen, got)
	}
	// And the frozen case must NOT produce any phase (nothing to clobber with).
	if frozen := terminalReportPhase(subscriber.AgentStop, true); frozen != "" {
		t.Fatalf("frozen stop must report no phase, got %q (would clobber agent_state=stopping)", frozen)
	}
}
