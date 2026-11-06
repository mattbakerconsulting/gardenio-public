package subscriber

import "testing"

// TestMapBeadEvent_LocalAgentEmitsNothing: a laptop agent (execution_target=
// local, gd-SzPJDNTBXS) is gb's to run, so no bead action on it may become a
// lifecycle event; a created one would otherwise spawn a pod for it.
func TestMapBeadEvent_LocalAgentEmitsNothing(t *testing.T) {
	w := NewSSEWatcher(SSEConfig{}, testLogger())
	for _, tc := range []struct{ action, state string }{
		{"created", "working"},
		{"updated", "stopping"},
		{"updated", "rate_limited"},
		{"closed", "done"},
		{"deleted", "working"},
	} {
		bead := agentBead("gd-local", tc.state, "open")
		bead.Fields["execution_target"] = "laptop"
		if ev, ok := w.mapBeadEvent(tc.action, beadEventPayload{Bead: bead}); ok {
			t.Errorf("%s/%s on a local agent emitted %s", tc.action, tc.state, ev.Type)
		}
	}

	// Control: the same bead with no execution_target spawns.
	if ev, ok := w.mapBeadEvent("created", beadEventPayload{Bead: agentBead("gd-pod", "working", "open")}); !ok || ev.Type != AgentSpawn {
		t.Fatalf("control: want AgentSpawn, got type=%s ok=%v", ev.Type, ok)
	}
}

// TestMapBeadEvent_EscalationExecutorStillSpawns: the escalation bridge's
// local-fallback executor is an ordinary fleet pod stamped
// execution_target=local (bridge/escalations.go spawnLocalAgent). Only the
// laptop marker is gb's, so this bead must still become an AgentSpawn, or the
// escalation's bead stays working forever with no pod (gd-SzPJDNTBXS review).
func TestMapBeadEvent_EscalationExecutorStillSpawns(t *testing.T) {
	w := NewSSEWatcher(SSEConfig{}, testLogger())
	bead := agentBead("gd-exec", "working", "open")
	bead.Fields["execution_target"] = "local"
	bead.Fields["escalation_id"] = "gd-esc"
	if ev, ok := w.mapBeadEvent("created", beadEventPayload{Bead: bead}); !ok || ev.Type != AgentSpawn {
		t.Fatalf("escalation executor: want AgentSpawn, got type=%s ok=%v", ev.Type, ok)
	}
}
