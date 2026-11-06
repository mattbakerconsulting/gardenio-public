package eventbus

import "testing"

func TestIsDecisionEvent(t *testing.T) {
	yes := []EventType{EventDecisionCreated, EventDecisionResponded, EventDecisionEscalated, EventDecisionExpired}
	no := []EventType{EventSessionStart, EventAgentStarted, EventMailSent, EventMutationCreate}

	for _, et := range yes {
		if !et.IsDecisionEvent() {
			t.Errorf("%s.IsDecisionEvent() = false, want true", et)
		}
	}
	for _, et := range no {
		if et.IsDecisionEvent() {
			t.Errorf("%s.IsDecisionEvent() = true, want false", et)
		}
	}
}

func TestIsAgentEvent(t *testing.T) {
	yes := []EventType{EventAgentStarted, EventAgentStopped, EventAgentCrashed, EventAgentIdle, EventAgentHeartbeat}
	no := []EventType{EventSessionStart, EventDecisionCreated, EventMailSent}

	for _, et := range yes {
		if !et.IsAgentEvent() {
			t.Errorf("%s.IsAgentEvent() = false, want true", et)
		}
	}
	for _, et := range no {
		if et.IsAgentEvent() {
			t.Errorf("%s.IsAgentEvent() = true, want false", et)
		}
	}
}

func TestIsMailEvent(t *testing.T) {
	yes := []EventType{EventMailSent, EventMailRead}
	no := []EventType{EventSessionStart, EventDecisionCreated}

	for _, et := range yes {
		if !et.IsMailEvent() {
			t.Errorf("%s.IsMailEvent() = false, want true", et)
		}
	}
	for _, et := range no {
		if et.IsMailEvent() {
			t.Errorf("%s.IsMailEvent() = true, want false", et)
		}
	}
}

func TestIsMutationEvent(t *testing.T) {
	yes := []EventType{EventMutationCreate, EventMutationUpdate, EventMutationDelete, EventMutationComment, EventMutationStatus}
	no := []EventType{EventSessionStart, EventDecisionCreated}

	for _, et := range yes {
		if !et.IsMutationEvent() {
			t.Errorf("%s.IsMutationEvent() = false, want true", et)
		}
	}
	for _, et := range no {
		if et.IsMutationEvent() {
			t.Errorf("%s.IsMutationEvent() = true, want false", et)
		}
	}
}

func TestIsGateEvent(t *testing.T) {
	yes := []EventType{EventGateSatisfied, EventGateCleared}
	no := []EventType{EventSessionStart, EventDecisionCreated}

	for _, et := range yes {
		if !et.IsGateEvent() {
			t.Errorf("%s.IsGateEvent() = false, want true", et)
		}
	}
	for _, et := range no {
		if et.IsGateEvent() {
			t.Errorf("%s.IsGateEvent() = true, want false", et)
		}
	}
}

func TestIsJackEvent(t *testing.T) {
	yes := []EventType{EventJackOn, EventJackOff, EventJackExpired, EventJackExtend}
	no := []EventType{EventSessionStart, EventDecisionCreated}

	for _, et := range yes {
		if !et.IsJackEvent() {
			t.Errorf("%s.IsJackEvent() = false, want true", et)
		}
	}
	for _, et := range no {
		if et.IsJackEvent() {
			t.Errorf("%s.IsJackEvent() = true, want false", et)
		}
	}
}

func TestIsConfigEvent(t *testing.T) {
	yes := []EventType{EventConfigSet, EventConfigUnset}
	no := []EventType{EventSessionStart, EventDecisionCreated}

	for _, et := range yes {
		if !et.IsConfigEvent() {
			t.Errorf("%s.IsConfigEvent() = false, want true", et)
		}
	}
	for _, et := range no {
		if et.IsConfigEvent() {
			t.Errorf("%s.IsConfigEvent() = true, want false", et)
		}
	}
}

func TestEventTypesAreExclusive(t *testing.T) {
	// Each event type should match exactly one category (or be a hook event).
	allTypes := []EventType{
		EventSessionStart, EventUserPromptSubmit, EventPreToolUse, EventPostToolUse,
		EventStop, EventPreCompact, EventPostCompact, EventSubagentStart, EventSubagentStop,
		EventNotification, EventSessionEnd,
		EventDecisionCreated, EventDecisionResponded, EventDecisionEscalated, EventDecisionExpired,
		EventAgentStarted, EventAgentStopped, EventAgentCrashed, EventAgentIdle, EventAgentHeartbeat,
		EventMailSent, EventMailRead,
		EventMutationCreate, EventMutationUpdate, EventMutationDelete, EventMutationComment, EventMutationStatus,
		EventConfigSet, EventConfigUnset,
		EventGateSatisfied, EventGateCleared,
		EventJackOn, EventJackOff, EventJackExpired, EventJackExtend,
	}

	for _, et := range allTypes {
		count := 0
		if et.IsDecisionEvent() {
			count++
		}
		if et.IsAgentEvent() {
			count++
		}
		if et.IsMailEvent() {
			count++
		}
		if et.IsMutationEvent() {
			count++
		}
		if et.IsConfigEvent() {
			count++
		}
		if et.IsGateEvent() {
			count++
		}
		if et.IsJackEvent() {
			count++
		}

		if count > 1 {
			t.Errorf("%s matches %d categories, should match at most 1", et, count)
		}
	}
}
