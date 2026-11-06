package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestChat_HandleClosed_IgnoresNonChatBeads(t *testing.T) {
	daemon := newMockDaemon()
	c := &Chat{
		daemon: daemon,
		logger: slog.Default(),
	}

	// Bead without slack-chat label should be ignored.
	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-task1",
		Type:   "task",
		Labels: []string{"bug"},
	})
	c.handleClosed(context.Background(), data)

	// No GetBead call should have been made (filtered before fetch).
	if daemon.getGetCalls() != 0 {
		t.Errorf("expected 0 GetBead calls, got %d", daemon.getGetCalls())
	}
}

func TestChat_HandleClosed_RelaysResponse(t *testing.T) {
	daemon := newMockDaemon()

	// Set up state with a chat message ref.
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetChatMessage("bd-chat1", MessageRef{
		ChannelID: "C-test",
		Timestamp: "1234.5678",
		Agent:     "gardenio/crew/test-bot",
	})

	// Daemon returns the full bead with close reason.
	daemon.beads["bd-chat1"] = &beadsapi.BeadDetail{
		ID:       "bd-chat1",
		Type:     "task",
		Status:   "closed",
		Assignee: "test-bot",
		Labels:   []string{"slack-chat"},
		Fields: map[string]string{
			"reason": "Here is the agent's response.",
		},
	}

	c := &Chat{
		daemon: daemon,
		state:  state,
		logger: slog.Default(),
		// bot is nil — Slack post will be skipped, but state cleanup still happens.
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "bd-chat1",
		Type:     "task",
		Assignee: "test-bot",
		Labels:   []string{"slack-chat"},
	})
	c.handleClosed(context.Background(), data)

	// State should be cleaned up.
	if _, ok := state.GetChatMessage("bd-chat1"); ok {
		t.Error("expected chat message to be removed from state after close")
	}
}

func TestChat_HandleClosed_FallbackToDescription(t *testing.T) {
	daemon := newMockDaemon()

	// No state entry — but bead description has [slack:CHANNEL:TS] tag.
	daemon.beads["bd-chat2"] = &beadsapi.BeadDetail{
		ID:       "bd-chat2",
		Type:     "task",
		Status:   "closed",
		Assignee: "test-bot",
		Labels:   []string{"slack-chat"},
		Fields: map[string]string{
			"description": "Message from user\n\n---\n[slack:C-fallback:9999.0001]",
			"reason":      "Got it!",
		},
	}

	c := &Chat{
		daemon: daemon,
		logger: slog.Default(),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "bd-chat2",
		Type:     "task",
		Assignee: "test-bot",
		Labels:   []string{"slack-chat"},
	})

	// Should not panic — fallback parsing from description.
	c.handleClosed(context.Background(), data)

	// Verify GetBead was called for description parsing fallback.
	if daemon.getGetCalls() < 1 {
		t.Error("expected at least 1 GetBead call for description fallback")
	}
}

func TestParseSlackMeta(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		wantCh    string
		wantTS    string
	}{
		{
			name:   "standard tag",
			text:   "Hello\n---\n[slack:C12345:1234.5678]",
			wantCh: "C12345",
			wantTS: "1234.5678",
		},
		{
			name:   "tag in middle of text",
			text:   "prefix [slack:CABC:999.111] suffix",
			wantCh: "CABC",
			wantTS: "999.111",
		},
		{
			name:   "no tag",
			text:   "no tag here",
			wantCh: "",
			wantTS: "",
		},
		{
			name:   "incomplete tag",
			text:   "[slack:missing-close",
			wantCh: "",
			wantTS: "",
		},
		{
			name:   "tag with no colon separator",
			text:   "[slack:nocolon]",
			wantCh: "",
			wantTS: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, ts := parseSlackMeta(tt.text)
			if ch != tt.wantCh {
				t.Errorf("channel: got %q, want %q", ch, tt.wantCh)
			}
			if ts != tt.wantTS {
				t.Errorf("timestamp: got %q, want %q", ts, tt.wantTS)
			}
		})
	}
}

func TestBuildChatResponse(t *testing.T) {
	tests := []struct {
		name     string
		detail   *beadsapi.BeadDetail
		assignee string
		contains string
	}{
		{
			name: "with reason",
			detail: &beadsapi.BeadDetail{
				Fields: map[string]string{"reason": "Done!"},
			},
			assignee: "test-bot",
			contains: "test-bot",
		},
		{
			name: "with notes fallback",
			detail: &beadsapi.BeadDetail{
				Notes: "Task completed successfully.",
			},
			assignee: "test-bot",
			contains: "completed successfully",
		},
		{
			name:     "no response text",
			detail:   &beadsapi.BeadDetail{},
			assignee: "test-bot",
			contains: "no response text",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := buildChatResponse(tt.detail, tt.assignee)
			if !containsString(result, tt.contains) {
				t.Errorf("response %q does not contain %q", result, tt.contains)
			}
		})
	}
}

func containsString(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsIndex(s, substr))
}

func containsIndex(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestExtractAgentName(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gardenio/crew/test-bot", "test-bot"},
		{"test-bot", "test-bot"},
		{"a/b", "b"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := extractAgentName(tt.input)
			if got != tt.want {
				t.Errorf("extractAgentName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestHasLabel(t *testing.T) {
	labels := []string{"bug", "slack-chat", "p2"}
	if !hasLabel(labels, "slack-chat") {
		t.Error("expected slack-chat to be found")
	}
	if hasLabel(labels, "missing") {
		t.Error("expected missing to not be found")
	}
	if hasLabel(nil, "anything") {
		t.Error("expected nil labels to return false")
	}
}

func TestTruncateText(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"hello", 10, "hello"},
		{"hello world", 5, "he..."},
		{"hi", 2, "hi"},
		{"hello", 3, "hel"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncateText(tt.input, tt.maxLen)
			if got != tt.want {
				t.Errorf("truncateText(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
			}
		})
	}
}

func TestStateChatMessages_CRUD(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Initially empty.
	if all := state.AllChatMessages(); len(all) != 0 {
		t.Fatalf("expected empty chat messages, got %d", len(all))
	}

	// Set.
	ref := MessageRef{ChannelID: "C1", Timestamp: "111.222", Agent: "test-bot"}
	if err := state.SetChatMessage("bd-1", ref); err != nil {
		t.Fatal(err)
	}

	// Get.
	got, ok := state.GetChatMessage("bd-1")
	if !ok {
		t.Fatal("expected chat message to exist")
	}
	if got.ChannelID != "C1" || got.Timestamp != "111.222" {
		t.Errorf("got %+v, want C1/111.222", got)
	}

	// Persists to disk.
	data, _ := os.ReadFile(filepath.Join(dir, "state.json"))
	var loaded StateData
	_ = json.Unmarshal(data, &loaded)
	if _, ok := loaded.ChatMessages["bd-1"]; !ok {
		t.Error("expected chat message in persisted state")
	}

	// Remove.
	if err := state.RemoveChatMessage("bd-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.GetChatMessage("bd-1"); ok {
		t.Error("expected chat message to be removed")
	}
}
