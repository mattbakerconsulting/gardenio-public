package eventbus

import (
	"context"
	"encoding/json"
	"testing"
)

// testHandler is a minimal Handler implementation for testing.
type testHandler struct {
	id       string
	events   []EventType
	priority int
	fn       func(ctx context.Context, event *Event, result *Result) error
}

func (h *testHandler) ID() string          { return h.id }
func (h *testHandler) Handles() []EventType { return h.events }
func (h *testHandler) Priority() int        { return h.priority }
func (h *testHandler) Handle(ctx context.Context, event *Event, result *Result) error {
	if h.fn != nil {
		return h.fn(ctx, event, result)
	}
	return nil
}

func TestNew(t *testing.T) {
	b := New()
	if b == nil {
		t.Fatal("New() returned nil")
	}
	if b.JetStreamEnabled() {
		t.Error("new bus should not have JetStream enabled")
	}
	if len(b.Handlers()) != 0 {
		t.Error("new bus should have no handlers")
	}
}

func TestRegisterUnregister(t *testing.T) {
	b := New()
	h := &testHandler{id: "test-1", events: []EventType{EventPreToolUse}}

	b.Register(h)
	if len(b.Handlers()) != 1 {
		t.Fatalf("expected 1 handler, got %d", len(b.Handlers()))
	}

	removed := b.Unregister("test-1")
	if !removed {
		t.Error("Unregister should return true for existing handler")
	}
	if len(b.Handlers()) != 0 {
		t.Error("expected 0 handlers after unregister")
	}

	removed = b.Unregister("nonexistent")
	if removed {
		t.Error("Unregister should return false for nonexistent handler")
	}
}

func TestDispatchNilEvent(t *testing.T) {
	b := New()
	_, err := b.Dispatch(context.Background(), nil)
	if err == nil {
		t.Error("Dispatch(nil) should return error")
	}
}

func TestDispatchCallsMatchingHandlers(t *testing.T) {
	b := New()
	var calls []string

	b.Register(&testHandler{
		id:       "h1",
		events:   []EventType{EventPreToolUse},
		priority: 10,
		fn: func(_ context.Context, _ *Event, _ *Result) error {
			calls = append(calls, "h1")
			return nil
		},
	})
	b.Register(&testHandler{
		id:       "h2",
		events:   []EventType{EventPostToolUse}, // different event
		priority: 5,
		fn: func(_ context.Context, _ *Event, _ *Result) error {
			calls = append(calls, "h2")
			return nil
		},
	})
	b.Register(&testHandler{
		id:       "h3",
		events:   []EventType{EventPreToolUse},
		priority: 5, // lower priority = called first
		fn: func(_ context.Context, _ *Event, _ *Result) error {
			calls = append(calls, "h3")
			return nil
		},
	})

	event := &Event{Type: EventPreToolUse}
	result, err := b.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if result == nil {
		t.Fatal("Dispatch returned nil result")
	}

	// h3 (priority 5) should be called before h1 (priority 10); h2 should not be called.
	if len(calls) != 2 {
		t.Fatalf("expected 2 handler calls, got %d: %v", len(calls), calls)
	}
	if calls[0] != "h3" || calls[1] != "h1" {
		t.Errorf("expected [h3, h1], got %v", calls)
	}
}

func TestDispatchHandlerCanModifyResult(t *testing.T) {
	b := New()
	b.Register(&testHandler{
		id:     "blocker",
		events: []EventType{EventPreToolUse},
		fn: func(_ context.Context, _ *Event, result *Result) error {
			result.Block = true
			result.Reason = "blocked by test"
			return nil
		},
	})

	result, err := b.Dispatch(context.Background(), &Event{Type: EventPreToolUse})
	if err != nil {
		t.Fatalf("Dispatch error: %v", err)
	}
	if !result.Block {
		t.Error("expected result.Block to be true")
	}
	if result.Reason != "blocked by test" {
		t.Errorf("expected reason 'blocked by test', got %q", result.Reason)
	}
}

func TestDispatchContextCanceled(t *testing.T) {
	b := New()
	b.Register(&testHandler{
		id:     "slow",
		events: []EventType{EventPreToolUse},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := b.Dispatch(ctx, &Event{Type: EventPreToolUse})
	if err == nil {
		t.Error("expected error for canceled context")
	}
}

func TestInjectActorIntoRaw(t *testing.T) {
	raw := json.RawMessage(`{"hook_event_name":"SessionStart","session_id":"abc"}`)
	result := injectActorIntoRaw(raw, "bright-hog")

	var obj map[string]any
	if err := json.Unmarshal(result, &obj); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if obj["actor"] != "bright-hog" {
		t.Errorf("expected actor 'bright-hog', got %v", obj["actor"])
	}
	if obj["session_id"] != "abc" {
		t.Error("original fields should be preserved")
	}
}

func TestInjectActorIntoRawInvalidJSON(t *testing.T) {
	raw := json.RawMessage(`not json`)
	result := injectActorIntoRaw(raw, "test")
	if string(result) != "not json" {
		t.Error("invalid JSON should be returned unchanged")
	}
}

func TestIsHookEvent(t *testing.T) {
	hookEvents := []EventType{
		EventSessionStart, EventPreToolUse, EventPostToolUse, EventStop,
	}
	for _, et := range hookEvents {
		if !isHookEvent(et) {
			t.Errorf("expected %s to be a hook event", et)
		}
	}

	nonHookEvents := []EventType{
		EventDecisionCreated, EventAgentStarted, EventMailSent,
		EventMutationCreate, EventConfigSet, EventGateSatisfied, EventJackOn,
		EventAssistantText, EventThinking, EventUsage,
	}
	for _, et := range nonHookEvents {
		if isHookEvent(et) {
			t.Errorf("expected %s to NOT be a hook event", et)
		}
	}
}
