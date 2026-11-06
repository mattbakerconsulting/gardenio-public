package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func pausedUpdateEvent(t *testing.T, changes map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":     "gd-local1",
			"type":   "agent",
			"title":  "local-agent",
			"status": "open",
			"fields": map[string]string{"agent": "local-agent", "agent_state": "paused", "execution_target": "laptop", "runtime_id": "gd-r1"},
		},
		"changes": changes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The paused message goes out once, on the update that wrote
// agent_state=paused; a later update of a paused agent only re-renders the
// card.
func TestAgents_HandleUpdated_PausedNotifiesOnce(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{Notifier: notif, Logger: slog.Default()})
	ctx := context.Background()

	a.handleUpdated(ctx, pausedUpdateEvent(t, map[string]any{"agent_state": "paused"}))
	a.handleUpdated(ctx, pausedUpdateEvent(t, map[string]any{"last_seen_at": "2026-10-31T19:00:00Z"}))

	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.paused) != 1 {
		t.Fatalf("NotifyAgentPaused calls = %d, want 1", len(notif.paused))
	}
	if len(notif.stateChanges) != 2 {
		t.Fatalf("NotifyAgentState calls = %d, want 2 (the card re-renders on each update)", len(notif.stateChanges))
	}
}

func TestPausedMessage_NamesHostAndRelease(t *testing.T) {
	got := pausedMessage(Garden, "local-agent", "mbp<2>")
	for _, want := range []string{Garden.Semantic(SemPaused) + " local-agent is paused", "mbp&lt;2&gt; is offline", "Work is held", "`/kill local-agent`"} {
		if !strings.Contains(got, want) {
			t.Errorf("pausedMessage = %q, missing %q", got, want)
		}
	}
}

func TestAgentCard_PausedState(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden, "gardenio/local/local-agent", "", 0, "paused", "", time.Time{}, "", "", "local", "", "", nil, "", false, triggerCardInfo{}, nil)
	raw, _ := json.Marshal(blocks)
	if !strings.Contains(string(raw), "paused (laptop offline)") {
		t.Fatalf("paused card must say paused (laptop offline): %s", raw)
	}
}
