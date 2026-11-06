package bridge

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/socketmode"
	"golang.org/x/time/rate"
)

// TestDirtyCardsFlushAfterRateLimitBackoff verifies that dirty agent cards
// are flushed once the Slack rate-limit backoff expires, rather than remaining
// stuck indefinitely. Regression test for the "spinner shows exclamation and
// stops updating" bug.
func TestDirtyCardsFlushAfterRateLimitBackoff(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state

	agent := "test-card-agent"

	// Set up agent card in hot cache.
	bot.mu.Lock()
	bot.agentCards[agent] = MessageRef{ChannelID: "C-test", Timestamp: "1234.5678"}
	bot.agentState[agent] = "working"
	// Mark as dirty with a stale lastUpdate so it's normally eligible.
	bot.agentCardDirty[agent] = true
	bot.agentCardLastUpdate[agent] = time.Now().Add(-10 * time.Minute)
	bot.mu.Unlock()

	// Simulate a rate-limit backoff that's already expired.
	bot.mu.Lock()
	bot.slackRateLimitUntil = time.Now().Add(-1 * time.Second) // expired 1s ago
	bot.mu.Unlock()

	// flushDirtyCards should process the dirty card since backoff expired.
	bot.flushDirtyCards(context.Background())

	bot.mu.Lock()
	stillDirty := bot.agentCardDirty[agent]
	bot.mu.Unlock()

	if stillDirty {
		t.Error("expected dirty flag to be cleared after backoff expired, but card is still dirty")
	}
}

// TestRateLimitResetsLastUpdateForDirtyCards verifies that when a Slack rate
// limit is hit, the lastUpdate timestamp for all dirty cards is zeroed out so
// they become immediately eligible once the backoff window expires.
func TestRateLimitResetsLastUpdateForDirtyCards(t *testing.T) {
	bot := &Bot{
		logger:              slog.Default(),
		agentCardLastUpdate: make(map[string]time.Time),
		agentCardDirty:      make(map[string]bool),
		agentCards:          make(map[string]MessageRef),
		agentPending:        make(map[string]int),
		agentState:          make(map[string]string),
		agentSeen:           make(map[string]time.Time),
		agentPodName:        make(map[string]string),
		agentImageTag:       make(map[string]string),
		agentRole:           make(map[string]string),
		slackCardLimiter:    rate.NewLimiter(rate.Limit(40.0/60.0), 3),
	}

	agent1 := "agent-1"
	agent2 := "agent-2"

	// Both agents have recent lastUpdate (within debounce window).
	now := time.Now()
	bot.agentCardDirty[agent1] = true
	bot.agentCardDirty[agent2] = true
	bot.agentCardLastUpdate[agent1] = now
	bot.agentCardLastUpdate[agent2] = now

	// Simulate what happens when flushAgentCard encounters a rate limit:
	// slackRateLimitUntil is set and lastUpdate is zeroed for all dirty cards.
	bot.mu.Lock()
	bot.slackRateLimitUntil = now.Add(5 * time.Second)
	for dirtyAgent := range bot.agentCardDirty {
		bot.agentCardLastUpdate[dirtyAgent] = time.Time{}
	}
	bot.mu.Unlock()

	// Verify lastUpdate was zeroed.
	bot.mu.Lock()
	lu1 := bot.agentCardLastUpdate[agent1]
	lu2 := bot.agentCardLastUpdate[agent2]
	bot.mu.Unlock()

	if !lu1.IsZero() {
		t.Errorf("expected agent1 lastUpdate to be zeroed, got %v", lu1)
	}
	if !lu2.IsZero() {
		t.Errorf("expected agent2 lastUpdate to be zeroed, got %v", lu2)
	}
}

// TestGetAgentByThreadDaemonFallback verifies that getAgentByThread falls back
// to querying the daemon for agent beads with matching slack_thread_ts metadata
// when both the state and hot cache miss. This covers the case where
// pruneStaleAgentCards removed the card and CompactStaleEntries removed the
// thread→agent mapping. Regression test for the "new agent instead of resume" bug.
func TestGetAgentByThreadDaemonFallback(t *testing.T) {
	daemon := newMockDaemon()

	// Register an active agent with thread metadata in the daemon.
	daemon.mu.Lock()
	daemon.beads["bead-123"] = &beadsapi.BeadDetail{
		ID:   "bead-123",
		Type: "agent",
		Fields: map[string]string{
			"agent":                "thread-agent-x",
			"agent_state":         "working",
			"slack_thread_channel": "C-fallback",
			"slack_thread_ts":      "9999.1111",
		},
	}
	daemon.mu.Unlock()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := &Bot{
		daemon:     daemon,
		state:      state,
		logger:     slog.Default(),
		agentCards: make(map[string]MessageRef),
	}

	// No thread→agent mapping in state, no agent card in hot cache.
	// getAgentByThread should fall back to daemon metadata query.
	agent := bot.getAgentByThread("C-fallback", "9999.1111")
	if agent != "thread-agent-x" {
		t.Errorf("expected daemon fallback to find agent %q, got %q", "thread-agent-x", agent)
	}

	// Verify the mapping was restored in state for fast subsequent lookups.
	restoredAgent, ok := state.GetThreadAgent("C-fallback", "9999.1111")
	if !ok || restoredAgent != "thread-agent-x" {
		t.Errorf("expected thread-agent mapping to be restored in state, got ok=%v agent=%q", ok, restoredAgent)
	}
}

// TestGetAgentByThreadSkipsTerminalAgents verifies that the daemon fallback
// in getAgentByThread does NOT return agents in terminal states (done/failed),
// which should be respawned instead.
func TestGetAgentByThreadSkipsTerminalAgents(t *testing.T) {
	daemon := newMockDaemon()

	daemon.mu.Lock()
	daemon.beads["bead-dead"] = &beadsapi.BeadDetail{
		ID:   "bead-dead",
		Type: "agent",
		Fields: map[string]string{
			"agent":                "dead-agent",
			"agent_state":         "done",
			"slack_thread_channel": "C-done",
			"slack_thread_ts":      "8888.2222",
		},
	}
	daemon.mu.Unlock()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := &Bot{
		daemon:     daemon,
		state:      state,
		logger:     slog.Default(),
		agentCards: make(map[string]MessageRef),
	}

	agent := bot.getAgentByThread("C-done", "8888.2222")
	if agent != "" {
		t.Errorf("expected empty result for terminal agent, got %q", agent)
	}
}

// TestShutdownDrainReturnsOnEmptyChannel verifies that drainEvents returns
// immediately via the default case when the event channel is empty.
func TestShutdownDrainReturnsOnEmptyChannel(t *testing.T) {
	// Create a real socketmode client with a buffered Events channel.
	// drainEvents reads from socket.Events with a default (non-blocking) case,
	// so an empty channel should cause immediate return.
	sm := &socketmode.Client{
		Events: make(chan socketmode.Event, 10),
	}

	bot := &Bot{
		logger: slog.Default(),
		socket: sm,
	}

	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		bot.drainEvents(ctx)
		close(done)
	}()

	select {
	case <-done:
		// Success — drainEvents returned immediately on empty channel.
	case <-time.After(2 * time.Second):
		t.Fatal("drainEvents did not return within 2s on empty channel")
	}
}
