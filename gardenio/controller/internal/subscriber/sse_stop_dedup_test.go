package subscriber

import "testing"

// agentBead builds a minimal agent beadData for mapBeadEvent tests.
func agentBead(id, agentState, status string) beadData {
	return beadData{
		ID:         id,
		Type:       "agent",
		Status:     status,
		AgentState: agentState,
		Fields:     map[string]string{"project": "p", "role": "crew", "agent": "a1"},
	}
}

// TestMapBeadEvent_DuplicateStoppingEmitsOneAgentStop is the regression guard
// for bug ka-0000000194: the freeze/teardown path writes bead fields while
// agent_state is still "stopping", which re-emits `updated` events. Before the
// fix each one re-fired AgentStop → a second Freeze that overwrote the good
// snapshot with a degenerate (empty agent overlay-opts) one, making the bundle
// un-thawable. Only the FIRST transition into stopping must emit AgentStop;
// subsequent stopping updates must map to AgentUpdate (no second freeze).
func TestMapBeadEvent_DuplicateStoppingEmitsOneAgentStop(t *testing.T) {
	w := NewSSEWatcher(SSEConfig{}, testLogger())
	bead := agentBead("gd-stopdup", "stopping", "in_progress")
	payload := beadEventPayload{Bead: bead}

	ev1, ok1 := w.mapBeadEvent("updated", payload)
	if !ok1 || ev1.Type != AgentStop {
		t.Fatalf("first stopping update: want AgentStop, got type=%s ok=%v", ev1.Type, ok1)
	}

	// Second (and third) updated events while still stopping — the duplicate
	// the freeze path's own field writes produce — must NOT re-emit AgentStop.
	for i, want := range []EventType{AgentUpdate, AgentUpdate} {
		ev, ok := w.mapBeadEvent("updated", payload)
		if !ok || ev.Type != want {
			t.Fatalf("duplicate stopping update #%d: want %s, got type=%s ok=%v", i+2, want, ev.Type, ok)
		}
	}
}

// TestMapBeadEvent_StopReArmsAfterRespawn verifies the dedup marker is cleared
// on a re-spawn (status→in_progress with a status change) so a LATER genuine
// stop of the new session emits AgentStop again.
func TestMapBeadEvent_StopReArmsAfterRespawn(t *testing.T) {
	w := NewSSEWatcher(SSEConfig{}, testLogger())
	stopping := beadEventPayload{Bead: agentBead("gd-rearm", "stopping", "in_progress")}

	if ev, ok := w.mapBeadEvent("updated", stopping); !ok || ev.Type != AgentStop {
		t.Fatalf("initial stop: want AgentStop, got type=%s ok=%v", ev.Type, ok)
	}
	if ev, _ := w.mapBeadEvent("updated", stopping); ev.Type != AgentUpdate {
		t.Fatalf("duplicate stop pre-respawn: want AgentUpdate, got %s", ev.Type)
	}

	// Re-spawn: status change to in_progress re-arms the marker.
	respawn := beadEventPayload{
		Bead:    agentBead("gd-rearm", "", "in_progress"),
		Changes: map[string]any{"status": "in_progress"},
	}
	if ev, ok := w.mapBeadEvent("updated", respawn); !ok || ev.Type != AgentSpawn {
		t.Fatalf("respawn: want AgentSpawn, got type=%s ok=%v", ev.Type, ok)
	}

	// A genuine stop of the new session emits AgentStop again.
	if ev, ok := w.mapBeadEvent("updated", stopping); !ok || ev.Type != AgentStop {
		t.Fatalf("post-respawn stop: want AgentStop, got type=%s ok=%v", ev.Type, ok)
	}
}

// TestMapBeadEvent_StopReArmsAfterCreatedAndClosed verifies created/closed/
// deleted all re-arm the stop marker.
func TestMapBeadEvent_StopReArmsAfterCreatedAndClosed(t *testing.T) {
	for _, action := range []string{"created", "closed", "deleted"} {
		w := NewSSEWatcher(SSEConfig{}, testLogger())
		stopping := beadEventPayload{Bead: agentBead("gd-rearm2", "stopping", "in_progress")}

		if ev, _ := w.mapBeadEvent("updated", stopping); ev.Type != AgentStop {
			t.Fatalf("[%s] initial stop: want AgentStop, got %s", action, ev.Type)
		}
		if ev, _ := w.mapBeadEvent("updated", stopping); ev.Type != AgentUpdate {
			t.Fatalf("[%s] duplicate stop: want AgentUpdate, got %s", action, ev.Type)
		}

		// The re-arming action.
		w.mapBeadEvent(action, beadEventPayload{Bead: agentBead("gd-rearm2", "", "")})

		if ev, _ := w.mapBeadEvent("updated", stopping); ev.Type != AgentStop {
			t.Fatalf("[%s] stop after re-arm: want AgentStop, got %s", action, ev.Type)
		}
	}
}

// TestMapBeadEvent_NonStoppingUnaffected verifies the dedup does not perturb
// budget_exceeded / in_progress-spawn / plain-update mapping.
func TestMapBeadEvent_NonStoppingUnaffected(t *testing.T) {
	w := NewSSEWatcher(SSEConfig{}, testLogger())

	budget := beadEventPayload{Bead: agentBead("gd-x", "budget_exceeded", "in_progress")}
	if ev, _ := w.mapBeadEvent("updated", budget); ev.Type != BudgetExceeded {
		t.Fatalf("budget: want BudgetExceeded, got %s", ev.Type)
	}

	plain := beadEventPayload{Bead: agentBead("gd-y", "", "in_progress")}
	if ev, _ := w.mapBeadEvent("updated", plain); ev.Type != AgentUpdate {
		t.Fatalf("plain update (no status change): want AgentUpdate, got %s", ev.Type)
	}
}
