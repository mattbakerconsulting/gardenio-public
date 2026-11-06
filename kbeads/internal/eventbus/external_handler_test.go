package eventbus

import (
	"context"
	"testing"
)

func TestNewExternalHandler(t *testing.T) {
	cfg := ExternalHandlerConfig{
		ID:      "test-ext",
		Command: "echo hello",
		Events:  []string{"PreToolUse", "Stop"},
	}
	h := NewExternalHandler(cfg)

	if h.ID() != "test-ext" {
		t.Errorf("ID = %q, want %q", h.ID(), "test-ext")
	}
	if h.Priority() != 50 {
		t.Errorf("Priority = %d, want 50 (default)", h.Priority())
	}
	if len(h.Handles()) != 2 {
		t.Fatalf("Handles = %d events, want 2", len(h.Handles()))
	}
	if h.Handles()[0] != EventPreToolUse {
		t.Errorf("Handles[0] = %s, want PreToolUse", h.Handles()[0])
	}
}

func TestExternalHandlerHandle(t *testing.T) {
	cfg := ExternalHandlerConfig{
		ID:      "echo-handler",
		Command: `echo '{"block":true,"reason":"blocked by echo"}'`,
		Events:  []string{"PreToolUse"},
	}
	h := NewExternalHandler(cfg)

	event := &Event{Type: EventPreToolUse}
	result := &Result{}

	err := h.Handle(context.Background(), event, result)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if !result.Block {
		t.Error("expected block=true from handler output")
	}
	if result.Reason != "blocked by echo" {
		t.Errorf("reason = %q, want %q", result.Reason, "blocked by echo")
	}
}

func TestExternalHandlerNoOutput(t *testing.T) {
	cfg := ExternalHandlerConfig{
		ID:      "silent-handler",
		Command: "true",
		Events:  []string{"Stop"},
	}
	h := NewExternalHandler(cfg)

	event := &Event{Type: EventStop}
	result := &Result{}

	err := h.Handle(context.Background(), event, result)
	if err != nil {
		t.Fatalf("Handle error: %v", err)
	}
	if result.Block {
		t.Error("silent handler should not block")
	}
}

func TestExternalHandlerError(t *testing.T) {
	cfg := ExternalHandlerConfig{
		ID:      "fail-handler",
		Command: "exit 1",
		Events:  []string{"Stop"},
	}
	h := NewExternalHandler(cfg)

	event := &Event{Type: EventStop}
	result := &Result{}

	err := h.Handle(context.Background(), event, result)
	if err == nil {
		t.Fatal("expected error from failing handler")
	}
}

func TestExternalHandlerConfig(t *testing.T) {
	cfg := ExternalHandlerConfig{
		ID:       "custom",
		Command:  "cat",
		Events:   []string{"Stop"},
		Priority: 10,
		Shell:    "bash",
	}
	h := NewExternalHandler(cfg)

	if h.Priority() != 10 {
		t.Errorf("Priority = %d, want 10", h.Priority())
	}
	if h.Config().Shell != "bash" {
		t.Errorf("Shell = %q, want %q", h.Config().Shell, "bash")
	}
}

func TestLoadPersistedHandlers(t *testing.T) {
	bus := New()
	configs := map[string]string{
		"bus.handler.test1": `{"id":"test1","command":"echo ok","events":["Stop"]}`,
		"bus.handler.test2": `{"id":"test2","command":"echo ok","events":["PreToolUse"],"priority":10}`,
		"other.config":      `ignored`,
		"bus.handler.bad":   `not json`,
		"bus.handler.empty": `{"id":"","command":"","events":[]}`,
	}

	count := bus.LoadPersistedHandlers(configs)
	if count != 2 {
		t.Errorf("loaded %d handlers, want 2", count)
	}
	if len(bus.Handlers()) != 2 {
		t.Errorf("bus has %d handlers, want 2", len(bus.Handlers()))
	}
}
