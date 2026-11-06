package bridge

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// TestConcurrentThreadForwardOnDeadAgent fires concurrent handleThreadForward
// calls for the same thread whose bound agent has exited. Per gd-lUNTlpAVm5,
// the forward path no longer auto-respawns — so no agent beads should be
// created regardless of how many concurrent replies arrive. The resume hint
// is throttled so at most one hint posts.
func TestConcurrentThreadForwardOnDeadAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-fwd-race")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-fwd-race"
	threadTS := "7777.1111"
	agentName := "thread-7777-1111"

	_ = state.SetThreadAgent(channel, threadTS, agentName)
	_ = state.SetListenThread(channel, threadTS)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ev := &slackevents.MessageEvent{
				User:            "U-HUMAN",
				Channel:         channel,
				Text:            "concurrent forward message",
				TimeStamp:       "7777.2222",
				ThreadTimeStamp: threadTS,
			}
			bot.handleThreadForward(context.Background(), slackReplyInbound(ev, ""), agentName)
		}(i)
	}
	wg.Wait()

	daemon.mu.Lock()
	var agentCount int
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Title == agentName {
			agentCount++
		}
	}
	daemon.mu.Unlock()

	if agentCount != 0 {
		t.Errorf("REGRESSION: expected 0 agent beads from concurrent forwards after "+
			"auto-respawn removal (gd-lUNTlpAVm5), got %d", agentCount)
	}

	// Thread→agent mapping preserved so the mention-path resume still works.
	if _, ok := state.GetThreadAgent(channel, threadTS); !ok {
		t.Error("expected thread→agent mapping to be preserved")
	}
}

// TestReconcileThreadAgentsDuringActiveSpawn verifies that ReconcileThreadAgents
// does not corrupt state when called while a spawn is in-flight for the same thread.
func TestReconcileThreadAgentsDuringActiveSpawn(t *testing.T) {
	daemon := newMockDaemon()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:        daemon,
		state:         state,
		logger:        slog.Default(),
		agentCards:    make(map[string]MessageRef),
		spawnInFlight: make(map[string]bool),
	}

	channel := "C-reconcile-test"
	threadTS := "5555.1111"
	agentName := "thread-5555-1111"

	// Simulate an in-flight spawn for this thread.
	spawnKey := channel + ":" + threadTS
	b.mu.Lock()
	b.spawnInFlight[spawnKey] = true
	b.mu.Unlock()

	// Add an agent bead with thread fields that would normally be reconciled.
	daemon.mu.Lock()
	daemon.beads["bd-inflight"] = &beadsapi.BeadDetail{
		ID:    "bd-inflight",
		Title: agentName,
		Type:  "agent",
		Fields: map[string]string{
			"agent":                agentName,
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
		},
	}
	daemon.mu.Unlock()

	// Run reconciliation — it should set the mapping since no existing
	// state-level mapping exists (spawnInFlight is a separate concern;
	// reconciliation only checks state.GetThreadAgent).
	b.ReconcileThreadAgents(context.Background())

	// The mapping should be set from the daemon bead.
	agent, ok := state.GetThreadAgent(channel, threadTS)
	if !ok {
		t.Fatal("expected thread→agent mapping to be restored by reconciliation")
	}
	if agent != agentName {
		t.Errorf("reconciled agent = %q, want %q", agent, agentName)
	}

	// Now simulate a concurrent scenario: set a DIFFERENT mapping (as if
	// the in-flight spawn completed) and run reconciliation again.
	_ = state.SetThreadAgent(channel, threadTS, "spawned-agent")

	b.ReconcileThreadAgents(context.Background())

	// Reconciliation must NOT overwrite the existing mapping.
	agent, _ = state.GetThreadAgent(channel, threadTS)
	if agent != "spawned-agent" {
		t.Errorf("expected reconciliation to preserve existing mapping %q, got %q",
			"spawned-agent", agent)
	}

	// Clean up spawnInFlight.
	b.mu.Lock()
	delete(b.spawnInFlight, spawnKey)
	b.mu.Unlock()
}

// TestDecisionNotificationRoutesToThreadBoundAgent verifies that a decision
// bead created by a thread-bound agent resolves the thread binding and routes
// the notification correctly. The decision's requesting_agent field should be
// used to look up thread metadata.
func TestDecisionNotificationRoutesToThreadBoundAgent(t *testing.T) {
	daemon := newMockDaemon()

	channel := "C-decision-thread"
	threadTS := "8888.1111"
	agentName := "thread-8888-1111"

	// Set up a thread-bound agent bead.
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-thread-decision-agent",
		Title: agentName,
		Type:  "agent",
		Fields: map[string]string{
			"agent":                agentName,
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
			"spawn_source":        "slack-thread",
			"slack_user_id":       "U-SPAWNER",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent(channel, threadTS, agentName)

	b := &Bot{
		daemon:     daemon,
		state:      state,
		logger:     slog.Default(),
		botUserID:  "U-BOT",
		agentCards: make(map[string]MessageRef),
	}

	// Verify resolveAgentThread returns the correct binding.
	resolvedChannel, resolvedTS := b.resolveAgentThread(context.Background(), agentName)
	if resolvedChannel != channel {
		t.Errorf("resolveAgentThread channel = %q, want %q", resolvedChannel, channel)
	}
	if resolvedTS != threadTS {
		t.Errorf("resolveAgentThread ts = %q, want %q", resolvedTS, threadTS)
	}

	// Verify the agent is reachable via getAgentByThread (for mention routing).
	found := b.getAgentByThread(channel, threadTS)
	if found != agentName {
		t.Errorf("getAgentByThread = %q, want %q", found, agentName)
	}

	// Verify resolveDecisionMentionUser finds the slack_user_id on the agent bead.
	userID := b.resolveDecisionMentionUser(context.Background(), agentName)
	if userID != "U-SPAWNER" {
		t.Errorf("resolveDecisionMentionUser = %q, want %q", userID, "U-SPAWNER")
	}
}

// TestConciergeDoubleClick verifies that two rapid clicks on the concierge
// Start button result in only one agent being spawned. The spawnInFlight
// guard should prevent the second click from creating a duplicate agent.
//
// handleConciergeStart uses spawnInFlight as a mutex-guarded check-and-set.
// This test uses handleThreadSpawn (which has the same guard) to exercise
// the real code path, since handleConciergeStart requires a full
// slack.InteractionCallback that is hard to construct in tests.
func TestConciergeDoubleClick(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-concierge-test")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-concierge-test"
	threadTS := "6666.1111"

	// Fire 5 concurrent spawn attempts for the same thread — simulates
	// rapid button clicks or multiple @mentions arriving simultaneously.
	// The spawnInFlight guard in handleThreadSpawn should ensure only 1 spawns.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
				User:            "U-USER",
				Text:            "<@U-BOT> help me",
				TimeStamp:       "6666.2222",
				ThreadTimeStamp: threadTS,
				Channel:         channel,
			}, "help me")
		}()
	}
	wg.Wait()

	// Count agent beads — should be exactly 1.
	daemon.mu.Lock()
	var agentCount int
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			agentCount++
		}
	}
	daemon.mu.Unlock()

	if agentCount != 1 {
		t.Errorf("expected exactly 1 agent bead from double-click, got %d", agentCount)
	}

	// S1.b coverage: concurrent mentions in the same thread must also produce
	// exactly one thread bead (resolve-or-create dedup, not two).
	if got := countThreadBeads(daemon, channel, threadTS); got != 1 {
		t.Errorf("expected exactly 1 thread bead from concurrent mentions, got %d", got)
	}
}

// TestConcurrentSpawnAndForwardSameThread verifies that a handleThreadForward
// arriving concurrently with a handleThreadSpawn for the same thread does not
// produce duplicate agents. The spawnInFlight guard protects both paths.
func TestConcurrentSpawnAndForwardSameThread(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-mixed-race")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-mixed-race"
	threadTS := "3333.4444"

	// Fire a spawn and multiple forwards concurrently.
	var wg sync.WaitGroup

	// Spawn goroutine.
	wg.Add(1)
	go func() {
		defer wg.Done()
		bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
			User:            "U-USER",
			Text:            "<@U-BOT> help me",
			TimeStamp:       "3333.5555",
			ThreadTimeStamp: threadTS,
			Channel:         channel,
		}, "help me")
	}()

	// Forward goroutines (these will see no agent bound yet and try respawn).
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// These will find no agent in state (or a just-set one) and either
			// forward normally or attempt respawn.
			agentName := bot.getAgentByThread(channel, threadTS)
			if agentName == "" {
				// No agent bound yet — this is expected for the race window.
				return
			}
			bot.handleThreadForward(context.Background(), slackReplyInbound(&slackevents.MessageEvent{
				User:            "U-USER",
				Channel:         channel,
				Text:            "follow up",
				TimeStamp:       "3333.6666",
				ThreadTimeStamp: threadTS,
			}, ""), agentName)
		}()
	}
	wg.Wait()

	// Count agent beads — should be exactly 1.
	daemon.mu.Lock()
	var agentCount int
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			agentCount++
		}
	}
	daemon.mu.Unlock()

	if agentCount != 1 {
		t.Errorf("expected exactly 1 agent bead from concurrent spawn+forward, got %d", agentCount)
	}
}
