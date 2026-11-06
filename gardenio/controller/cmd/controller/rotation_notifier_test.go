package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
)

func testRotationLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestRotationEvent_Marshal(t *testing.T) {
	evt := rotationEvent{
		Event:       "rotating",
		Project:     "gardenio",
		FromAccount: "claude-max-main",
		ToAccount:   "claude-max-fallback",
		AgentBead:   "gd-abc123",
		Reason:      "rate_limited",
		Timestamp:   "2026-03-20T03:00:00Z",
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var decoded rotationEvent
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}

	if decoded.Event != "rotating" {
		t.Errorf("event = %q, want %q", decoded.Event, "rotating")
	}
	if decoded.Project != "gardenio" {
		t.Errorf("project = %q, want %q", decoded.Project, "gardenio")
	}
	if decoded.FromAccount != "claude-max-main" {
		t.Errorf("from_account = %q, want %q", decoded.FromAccount, "claude-max-main")
	}
	if decoded.ToAccount != "claude-max-fallback" {
		t.Errorf("to_account = %q, want %q", decoded.ToAccount, "claude-max-fallback")
	}
	if decoded.AgentBead != "gd-abc123" {
		t.Errorf("agent_bead = %q, want %q", decoded.AgentBead, "gd-abc123")
	}
}

func TestRotationEvent_OmitEmpty(t *testing.T) {
	evt := rotationEvent{
		Event:       "no_fallback",
		Project:     "gardenio",
		FromAccount: "claude-max-main",
		Timestamp:   "2026-03-20T03:00:00Z",
	}

	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal to map failed: %v", err)
	}

	// to_account, agent_bead, reason should be omitted when empty.
	for _, key := range []string{"to_account", "agent_bead", "reason"} {
		if _, ok := raw[key]; ok {
			t.Errorf("expected %q to be omitted when empty", key)
		}
	}
}

func TestNewRotationNotifier_NilWhenNoNats(t *testing.T) {
	n := newRotationNotifier("", "", "", testRotationLogger(), nil)
	if n != nil {
		t.Fatal("expected nil notifier when NATS URL is empty")
	}
}

func TestRotationNotifier_PublishNilSafe(t *testing.T) {
	// Calling publish on a nil notifier should not panic.
	var n *rotationNotifier
	n.publish(rotationEvent{Event: "test"})
}
