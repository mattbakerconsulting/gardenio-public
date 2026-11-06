package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestUpdateAgentCard_ThreadBound_SkipsCardCreation verifies that when an
// agent is thread-bound (has slack_thread_channel and slack_thread_ts fields),
// updateAgentCard returns early without creating a card. This prevents
// orphaned cards that leak into threads as unwanted compressed messages.
// Regression test for bae87864.
func TestUpdateAgentCard_ThreadBound_SkipsCardCreation(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.0001"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	// Seed an agent bead with thread binding fields.
	daemon.beads["thread-agent"] = &beadsapi.BeadDetail{
		ID:     "gd-thread-agent",
		Title:  "thread-agent",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":                "thread-agent",
			"slack_thread_channel": "C-THREAD-123",
			"slack_thread_ts":      "1700000000.000001",
		},
	}

	bot := newTestBot(daemon, slackSrv)
	bot.threadingMode = "agent"
	bot.agentState["thread-agent"] = "working"

	// No existing card in hot cache — updateAgentCard would normally create one.
	bot.updateAgentCard(context.Background(), "thread-agent")

	// Verify NO Slack message was posted (no card created).
	if n := postCount.Load(); n != 0 {
		t.Errorf("expected 0 chat.postMessage calls for thread-bound agent, got %d", n)
	}

	// Verify no card was added to the hot cache.
	bot.mu.Lock()
	_, hasCard := bot.agentCards["thread-agent"]
	bot.mu.Unlock()
	if hasCard {
		t.Error("thread-bound agent should not have an agent card in hot cache")
	}
}

// TestUpdateAgentCard_NonThreadBound_CreatesCard verifies that non-thread-bound
// agents still get a card created when threadingMode is enabled. This ensures
// the orphan fix only affects thread-bound agents, not all agents.
func TestUpdateAgentCard_NonThreadBound_CreatesCard(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "8888.0001"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	// Seed an agent bead WITHOUT thread binding fields.
	daemon.beads["regular-agent"] = &beadsapi.BeadDetail{
		ID:     "gd-regular-agent",
		Title:  "regular-agent",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":   "regular-agent",
			"project": "gardenio",
		},
	}

	bot := newTestBot(daemon, slackSrv)
	bot.threadingMode = "agent"
	// The default channel the card falls back to: the Slack surface refuses a
	// post with no channel rather than sending it.
	bot.channel = "C-GARDENIO"
	bot.agentState["regular-agent"] = "working"

	bot.updateAgentCard(context.Background(), "regular-agent")

	// Non-thread-bound agent SHOULD get a card created.
	if n := postCount.Load(); n == 0 {
		t.Error("expected at least 1 chat.postMessage call for non-thread-bound agent")
	}
}

// TestUpdateAgentCard_ThreadBound_ExistingCard_StillUpdates verifies that if
// a thread-bound agent somehow already has a card in the hot cache (e.g.,
// created before the thread binding was established), updateAgentCard still
// updates it — the orphan prevention only blocks NEW card creation.
func TestUpdateAgentCard_ThreadBound_ExistingCard_StillUpdates(t *testing.T) {
	var updateCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			updateCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	// Thread-bound agent bead.
	daemon.beads["hybrid-agent"] = &beadsapi.BeadDetail{
		ID:     "gd-hybrid-agent",
		Title:  "hybrid-agent",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":                "hybrid-agent",
			"slack_thread_channel": "C-THREAD-456",
			"slack_thread_ts":      "1700000000.000002",
		},
	}

	bot := newTestBot(daemon, slackSrv)
	bot.threadingMode = "agent"
	bot.agentState["hybrid-agent"] = "working"

	// Pre-populate an existing card in the hot cache.
	bot.agentCards["hybrid-agent"] = MessageRef{
		ChannelID: "C-EXISTING",
		Timestamp: "7777.0001",
	}

	bot.updateAgentCard(context.Background(), "hybrid-agent")

	// The existing card should be updated (chat.update), not skipped.
	if n := updateCount.Load(); n == 0 {
		t.Error("expected chat.update call for existing card on thread-bound agent")
	}
}

// TestUpdateAgentCard_CachedThreadBound_SkipsCardEvenWhenDaemonFails verifies
// that the cached agentThreadBound flag prevents stray card creation even when
// resolveAgentThread fails (daemon HTTP call returns an error). This is the
// core fix for thread agent cards appearing outside their spawning threads.
func TestUpdateAgentCard_CachedThreadBound_SkipsCardEvenWhenDaemonFails(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.0002"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	// Do NOT seed any bead — FindAgentBead will return an error,
	// simulating a transient daemon failure.

	bot := newTestBot(daemon, slackSrv)
	bot.threadingMode = "agent"
	bot.agentState["thread-agent"] = "working"

	// Set the cached thread-bound flag (normally set by NotifyAgentSpawn).
	bot.mu.Lock()
	bot.markThreadBound("thread-agent")
	bot.mu.Unlock()

	// Call updateAgentCard. Without the fix, this would fall through to
	// ensureAgentCard and create a stray top-level card.
	bot.updateAgentCard(context.Background(), "thread-agent")

	if n := postCount.Load(); n != 0 {
		t.Errorf("expected 0 chat.postMessage calls for cached thread-bound agent, got %d", n)
	}

	bot.mu.Lock()
	_, hasCard := bot.agentCards["thread-agent"]
	bot.mu.Unlock()
	if hasCard {
		t.Error("cached thread-bound agent should not have an agent card")
	}
}
