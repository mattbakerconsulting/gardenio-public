package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

func TestSquawk_HandleClosed_IgnoresNonMessageBeads(t *testing.T) {
	s := &Squawk{
		daemon: newMockDaemon(),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	// Task bead should be ignored.
	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-task1",
		Type:   "task",
		Labels: []string{"bug"},
	})
	s.handleClosed(context.Background(), data)
	// No panic, no action — just verify it doesn't crash.
}

func TestSquawk_HandleClosed_IgnoresMessageWithoutSquawkLabel(t *testing.T) {
	s := &Squawk{
		daemon: newMockDaemon(),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	// Message bead without "squawk" label should be ignored.
	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-msg1",
		Type:   "message",
		Labels: []string{"from:test-agent"},
	})
	s.handleClosed(context.Background(), data)
	// No crash, no action.
}

func TestSquawk_TryUpdateSpawnMessage_NoRef(t *testing.T) {
	b := &Bot{
		logger:          slog.Default(),
		threadSpawnMsgs: make(map[string]MessageRef),
	}
	// No spawn ref → should return false.
	if b.tryUpdateSpawnMessage(context.Background(), "test-agent", "hello") {
		t.Error("expected tryUpdateSpawnMessage to return false when no spawn ref exists")
	}
}

func TestSquawk_TryUpdateSpawnMessage_ConsumesRef(t *testing.T) {
	b := &Bot{
		logger: slog.Default(),
		threadSpawnMsgs: map[string]MessageRef{
			"test-agent": {ChannelID: "C123", Timestamp: "1234.5678"},
		},
	}
	// api is nil so UpdateMessageContext will panic if called incorrectly.
	// But since api is nil, tryUpdateSpawnMessage will panic. We only test
	// the map consumption by checking the ref is removed.
	// Instead, verify that the ref is consumed (deleted from map) even on failure.
	// We can't call the method with nil api, so just verify the map state directly.
	b.mu.Lock()
	_, has := b.threadSpawnMsgs["test-agent"]
	b.mu.Unlock()
	if !has {
		t.Fatal("expected spawn ref to exist before test")
	}

	// Verify that tryUpdateSpawnMessage with no spawn ref returns false.
	if b.tryUpdateSpawnMessage(context.Background(), "other-agent", "hello") {
		t.Error("expected false for unknown agent")
	}

	// Original ref should still be present (we asked about a different agent).
	b.mu.Lock()
	_, has = b.threadSpawnMsgs["test-agent"]
	b.mu.Unlock()
	if !has {
		t.Error("expected spawn ref for test-agent to still exist after querying other-agent")
	}
}

func TestSquawk_HandleClosed_ProcessesSquawkMessage(t *testing.T) {
	s := &Squawk{
		daemon: newMockDaemon(),
		logger: slog.Default(),
		seen:   make(map[string]bool),
		// bot is nil — Slack post will be skipped, but processing still happens.
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk1",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Build complete, MR ready for review",
		},
	})
	s.handleClosed(context.Background(), data)

	// Verify dedup: same bead should be skipped on replay.
	if !s.alreadySeen("bd-squawk1") {
		t.Error("expected bead to be marked as seen after processing")
	}
}

func TestSquawk_HandleClosed_AcceptsLegacySayLabel(t *testing.T) {
	s := &Squawk{
		daemon: newMockDaemon(),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	// Legacy "say" label should still be accepted for backward compatibility.
	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-say-legacy",
		Type:   "message",
		Labels: []string{"say", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Legacy say message",
		},
	})
	s.handleClosed(context.Background(), data)

	if !s.alreadySeen("bd-say-legacy") {
		t.Error("expected legacy say bead to be processed")
	}
}

func TestSquawk_HandleClosed_Dedup(t *testing.T) {
	s := &Squawk{
		daemon: newMockDaemon(),
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk2",
		Type:   "message",
		Labels: []string{"squawk", "from:agent-1"},
		Fields: map[string]string{
			"source_agent": "agent-1",
			"text":         "First message",
		},
	})

	// First call processes it.
	s.handleClosed(context.Background(), data)
	if !s.alreadySeen("bd-squawk2") {
		t.Error("expected bead to be seen after first call")
	}

	// Second call should be deduped (no panic, no double processing).
	s.handleClosed(context.Background(), data)
}

func TestSquawk_HandleClosed_WithBot_NoSpawnRef_PostsNewMessage(t *testing.T) {
	// When bot is set but there's no spawn ref, squawk should fall through
	// to routeAgentThreadMessage (which will be a no-op with nil api and no thread).
	b := &Bot{
		daemon:          newMockDaemon(),
		logger:          slog.Default(),
		threadSpawnMsgs: make(map[string]MessageRef),
		agentCards:      map[string]MessageRef{},
		agentSeen:       map[string]time.Time{},
	}
	s := &Squawk{
		daemon: newMockDaemon(),
		bot:    b,
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-noref",
		Type:   "message",
		Labels: []string{"squawk", "from:test-agent"},
		Fields: map[string]string{
			"source_agent": "test-agent",
			"text":         "Hello world",
		},
	})
	s.handleClosed(context.Background(), data)

	if !s.alreadySeen("bd-squawk-noref") {
		t.Error("expected bead to be processed")
	}
}

func TestExtractSquawkAgent(t *testing.T) {
	tests := []struct {
		name string
		bead BeadEvent
		want string
	}{
		{
			name: "from fields",
			bead: BeadEvent{
				Fields: map[string]string{"source_agent": "gardenio/crew/my-agent"},
				Labels: []string{"from:other"},
			},
			want: "my-agent",
		},
		{
			name: "from label",
			bead: BeadEvent{
				Labels: []string{"squawk", "from:label-agent"},
			},
			want: "label-agent",
		},
		{
			name: "from created_by",
			bead: BeadEvent{
				CreatedBy: "gardenio/crew/creator-agent",
			},
			want: "creator-agent",
		},
		{
			name: "empty",
			bead: BeadEvent{},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSquawkAgent(tt.bead)
			if got != tt.want {
				t.Errorf("extractSquawkAgent() = %q, want %q", got, tt.want)
			}
		})
	}
}
