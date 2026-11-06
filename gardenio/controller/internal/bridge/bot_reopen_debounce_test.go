package bridge

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// These tests cover the first-class reopen debounce (ka-0000000034): rapid
// successive restart/reopen requests for the SAME agent must coalesce into one
// actual kill+remint instead of each firing a competing restart cycle.
//
// The unit tests below exercise the reopenDebouncer coalescing primitive; the
// integration tests drive the two lifecycle-only reopen entrypoints (the
// "Restart Agent" button and the `@gardenio restart` command) and assert a
// second request inside the window mints NO second agent bead, while a request
// after the window does restart again.

// --- reopenDebouncer unit tests ---

func TestReopenDebouncer_AllowsFirst(t *testing.T) {
	d := newReopenDebouncer()
	if !d.Allow("thread-1-1") {
		t.Error("first restart should be allowed")
	}
}

func TestReopenDebouncer_BlocksRapid(t *testing.T) {
	d := newReopenDebouncer()
	d.Allow("thread-1-1")
	if d.Allow("thread-1-1") {
		t.Error("rapid second restart of the same agent should be coalesced (blocked)")
	}
}

func TestReopenDebouncer_AllowsDifferentAgents(t *testing.T) {
	d := newReopenDebouncer()
	d.Allow("thread-1-1")
	if !d.Allow("thread-2-2") {
		t.Error("a different agent must not be debounced by another agent's restart")
	}
}

func TestReopenDebouncer_AllowsAfterWindow(t *testing.T) {
	d := newReopenDebouncer()
	d.mu.Lock()
	// Simulate a restart that fired just outside the debounce window.
	d.last["thread-1-1"] = time.Now().Add(-reopenDebounceWindow - time.Second)
	d.mu.Unlock()
	if !d.Allow("thread-1-1") {
		t.Error("a restart after the debounce window elapses should be allowed")
	}
}

func TestReopenDebouncer_NilReceiverAllows(t *testing.T) {
	var d *reopenDebouncer
	if !d.Allow("thread-1-1") {
		t.Error("a nil debouncer must allow unconditionally (backward-compat for older fixtures)")
	}
	// Cleanup on nil must not panic.
	d.Cleanup()
}

func TestReopenDebouncer_Cleanup(t *testing.T) {
	d := newReopenDebouncer()
	d.mu.Lock()
	d.last["old-agent"] = time.Now().Add(-reopenDebounceWindow * 3) // stale
	d.last["fresh-agent"] = time.Now()                              // fresh
	d.mu.Unlock()

	d.Cleanup()

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.last["old-agent"]; ok {
		t.Error("stale entry should have been cleaned up")
	}
	if _, ok := d.last["fresh-agent"]; !ok {
		t.Error("fresh entry should NOT be cleaned up")
	}
}

// --- Restart button integration tests ---

// seedRestartAgent seeds a live thread agent bead so the restart paths have
// something to kill + remint, and returns the bot + coordinates. The bot is
// wired with a StateManager (the restart path touches listen-thread state).
func seedRestartAgent(t *testing.T, daemon *mockDaemon) (*Bot, string, string, string) {
	t.Helper()
	daemon.seedProjectWithChannel("gardenio", "C-reopen")

	slackSrv := newFakeSlackServer(t)
	t.Cleanup(slackSrv.Close)

	bot := newTestBot(daemon, slackSrv)
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = state

	channel := "C-reopen"
	threadTS := "1700000000.111111"
	agentName := "thread-1700000000-111111"

	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:        "bd-reopen-existing",
		Title:     agentName,
		Type:      "agent",
		Status:    "open",
		CreatedAt: time.Now().Add(-10 * time.Minute),
		Labels:    []string{"slack-thread", "project:gardenio"},
		// The restart button is the owner's (bot_kill_owner.go).
		OperatedBy: "oidc:opener@example.com",
		Fields: map[string]string{
			"agent":                agentName,
			"project":              "gardenio",
			"role":                 "thread",
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
			"slack_user_id":        "U-OPENER",
		},
	}
	daemon.mu.Unlock()

	return bot, channel, threadTS, agentName
}

// countRemintedAgents returns how many NEW agent beads (distinct from the
// seeded bd-reopen-existing) exist for agentName — i.e. how many actual
// restarts have completed.
func countRemintedAgents(daemon *mockDaemon, agentName string) int {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	n := 0
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Title == agentName && b.ID != "bd-reopen-existing" {
			n++
		}
	}
	return n
}

// waitForReminted polls until at least want reminted agent beads exist, or the
// deadline passes, and returns the final count. The restart handler works in a
// goroutine, so a poll is required.
func waitForReminted(daemon *mockDaemon, agentName string, want int) int {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countRemintedAgents(daemon, agentName) >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return countRemintedAgents(daemon, agentName)
}

func restartCallback(channel, threadTS string) slack.InteractionCallback {
	cb := slack.InteractionCallback{User: slack.User{ID: "U-CLICKER"}}
	cb.Channel.GroupConversation.Conversation.ID = channel
	cb.Container.ThreadTs = threadTS
	return cb
}

// TestHandleRestartThreadAgent_DebouncesRapidClicks is the core regression: two
// "Restart Agent" clicks inside the debounce window must produce exactly ONE
// reminted agent bead — the second click is coalesced, not a second kill+remint.
func TestHandleRestartThreadAgent_DebouncesRapidClicks(t *testing.T) {
	daemon := newMockDaemon()
	bot, channel, threadTS, agentName := seedRestartAgent(t, daemon)
	cb := restartCallback(channel, threadTS)

	// First click: proceeds and restarts.
	bot.handleRestartThreadAgent(asOpener(), agentName, cb)
	if got := waitForReminted(daemon, agentName, 1); got != 1 {
		t.Fatalf("after first click expected 1 reminted agent bead, got %d", got)
	}

	// Rapid second click within the window: must be coalesced (no new bead).
	bot.handleRestartThreadAgent(asOpener(), agentName, cb)
	// Give any (erroneously-spawned) goroutine time to mint a second bead.
	time.Sleep(200 * time.Millisecond)
	if got := countRemintedAgents(daemon, agentName); got != 1 {
		t.Errorf("rapid second click should be debounced: expected still 1 reminted bead, got %d", got)
	}
}

// TestHandleRestartThreadAgent_AllowsRestartAfterWindow verifies the debounce is
// a coalescing window, not a permanent lock: once the window elapses, a genuine
// later restart is honored and mints a second agent bead.
func TestHandleRestartThreadAgent_AllowsRestartAfterWindow(t *testing.T) {
	daemon := newMockDaemon()
	bot, channel, threadTS, agentName := seedRestartAgent(t, daemon)
	cb := restartCallback(channel, threadTS)

	bot.handleRestartThreadAgent(asOpener(), agentName, cb)
	if got := waitForReminted(daemon, agentName, 1); got != 1 {
		t.Fatalf("after first click expected 1 reminted agent bead, got %d", got)
	}

	// Age the debounce stamp past the window (rather than sleeping 10s).
	bot.reopenDebouncer.mu.Lock()
	bot.reopenDebouncer.last[agentName] = time.Now().Add(-reopenDebounceWindow - time.Second)
	bot.reopenDebouncer.mu.Unlock()

	// A second restart after the window must proceed and mint a 2nd new bead.
	// The remint uses the live bead present at click time as its pre-kill
	// snapshot; the first restart left a fresh open bead under the same name.
	bot.handleRestartThreadAgent(asOpener(), agentName, cb)
	if got := waitForReminted(daemon, agentName, 2); got < 2 {
		t.Errorf("restart after window should be honored: expected >=2 reminted beads, got %d", got)
	}
}

// --- @mention restart integration tests ---
//
// The `@gardenio restart` command routes through respawnThreadAgent, which
// revives the thread agent IN PLACE (reopening the same bead id, not minting a
// new one), so a reminted-bead count is not the right observable here. Instead
// we assert on the DISTINCT Slack text each branch posts: the leading request
// posts ":arrows_counterclockwise: Restarting thread agent …", a coalesced one
// posts "… is already restarting — coalescing your request." The debounce
// decision is made synchronously in handleMentionThreadCommand before the
// goroutine, so the post is observable without racing the respawn.

func mentionRestartEvent(channel, threadTS, user string) *slackevents.AppMentionEvent {
	return &slackevents.AppMentionEvent{
		Channel:         channel,
		ThreadTimeStamp: threadTS,
		User:            user,
	}
}

// seedRestartAgentRecording is seedRestartAgent wired to a recording Slack
// server so tests can inspect the posted text of each restart branch.
func seedRestartAgentRecording(t *testing.T, daemon *mockDaemon) (*Bot, *recordingSlackServer, string, string, string) {
	t.Helper()
	daemon.seedProjectWithChannel("gardenio", "C-reopen")

	rec := newRecordingSlackServer(t)
	t.Cleanup(rec.Close)

	bot := newTestBot(daemon, rec.srv)
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = state

	channel := "C-reopen"
	threadTS := "1700000000.222222"
	agentName := "thread-1700000000-222222"

	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:        "bd-reopen-mention",
		Title:     agentName,
		Type:      "agent",
		Status:    "closed", // terminal so respawn revives in place rather than skipping
		CreatedAt: time.Now().Add(-10 * time.Minute),
		Labels:    []string{"slack-thread", "project:gardenio"},
		// The /kill-thread tests run as this owner (bot_kill_owner.go).
		OperatedBy: "oidc:opener@example.com",
		Fields: map[string]string{
			"agent":                agentName,
			"project":              "gardenio",
			"role":                 "thread",
			"agent_state":          "done",
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
			"slack_user_id":        "U-OPENER",
		},
	}
	daemon.mu.Unlock()

	return bot, rec, channel, threadTS, agentName
}

// countRestartingPosts returns how many "Restarting thread agent …" (leading)
// and "coalescing your request." (debounced) posts the recording server saw.
func countRestartingPosts(rec *recordingSlackServer) (restarting, coalesced int) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, c := range rec.calls {
		if !strings.Contains(c.text, "restarting") && !strings.Contains(c.text, "Restarting") {
			continue
		}
		if strings.Contains(c.text, "coalescing your request") {
			coalesced++
		} else if strings.Contains(c.text, "Restarting thread agent") {
			restarting++
		}
	}
	return restarting, coalesced
}

// TestHandleMentionThreadCommand_RestartDebounces verifies the `@gardenio
// restart` command path shares the same reopen debounce: the first restart
// mention posts the "Restarting…" notice, and a rapid second mention posts the
// "coalescing" notice instead of firing a second reopen. Both are handled.
func TestHandleMentionThreadCommand_RestartDebounces(t *testing.T) {
	daemon := newMockDaemon()
	bot, rec, channel, threadTS, agentName := seedRestartAgentRecording(t, daemon)

	if !bot.handleMentionThreadCommand(context.Background(),
		mentionRestartEvent(channel, threadTS, "U-HUMAN"), agentName, "restart") {
		t.Fatal("expected restart mention to be handled")
	}
	// Rapid second restart mention: handled, but coalesced (no second reopen).
	if !bot.handleMentionThreadCommand(context.Background(),
		mentionRestartEvent(channel, threadTS, "U-HUMAN"), agentName, "restart") {
		t.Fatal("expected rapid second restart mention to still be handled (true)")
	}

	// Let the async respawn from the first (allowed) mention settle.
	time.Sleep(300 * time.Millisecond)

	restarting, coalesced := countRestartingPosts(rec)
	if restarting != 1 {
		t.Errorf("expected exactly 1 \"Restarting thread agent\" post (leading request), got %d", restarting)
	}
	if coalesced != 1 {
		t.Errorf("expected exactly 1 coalesced-notice post (debounced second mention), got %d", coalesced)
	}
}

// TestReopenDebounce_ButtonAndMentionShareWindow verifies the button and the
// @mention command coalesce against EACH OTHER — they debounce per agent name,
// so a mention restart immediately after a button click for the same agent is
// coalesced (posts the "coalescing" notice) rather than firing a second reopen.
func TestReopenDebounce_ButtonAndMentionShareWindow(t *testing.T) {
	daemon := newMockDaemon()
	bot, rec, channel, threadTS, agentName := seedRestartAgentRecording(t, daemon)

	// Button click first — consumes the leading slot in the window.
	bot.handleRestartThreadAgent(asOpener(), agentName, restartCallback(channel, threadTS))

	// A restart mention immediately after must be coalesced by the shared window.
	if !bot.handleMentionThreadCommand(context.Background(),
		mentionRestartEvent(channel, threadTS, "U-HUMAN"), agentName, "restart") {
		t.Fatal("expected restart mention to be handled")
	}

	time.Sleep(300 * time.Millisecond)

	_, coalesced := countRestartingPosts(rec)
	if coalesced != 1 {
		t.Errorf("mention restart after a button restart should be coalesced: expected 1 coalescing-notice post, got %d", coalesced)
	}
}

// --- /kill-thread --restart integration tests ---

// TestKillThreadRestart_DebouncesRapidRestarts verifies the /kill-thread
// --restart slash command shares the reopen window: when a restart is already
// in flight for the agent, a rapid `/kill-thread <agent> --restart` is
// coalesced. Crucially, the command gate skips the WHOLE command (both kill AND
// respawn) when coalesced — so the agent is NOT killed. We open the window
// directly (rather than via a button restart, whose own kill+remint would run
// asynchronously and muddy the CloseBead observable), so any CloseBead here is
// attributable solely to the command — and there must be none.
func TestKillThreadRestart_DebouncesRapidRestarts(t *testing.T) {
	daemon := newMockDaemon()
	bot, rec, channel, _, agentName := seedRestartAgentRecording(t, daemon)

	// Open a restart window for this agent (as if a restart just fired) — no
	// kill, so the CloseBead counter cleanly isolates the command's behavior.
	bot.reopenDebouncer.Allow(agentName)

	closesBefore := len(daemon.closed)
	bot.handleKillThreadCommand(beadsapi.WithOperator(context.Background(), "oidc:opener@example.com"), slack.SlashCommand{
		Command:   "/kill-thread",
		Text:      agentName + " --restart",
		ChannelID: channel,
		UserID:    "U-CLICKER",
	})
	time.Sleep(300 * time.Millisecond)

	_, coalesced := countRestartingPosts(rec)
	if coalesced < 1 {
		t.Errorf("a coalesced /kill-thread --restart should post a coalescing notice, got %d", coalesced)
	}
	daemon.mu.Lock()
	closesAfter := len(daemon.closed)
	daemon.mu.Unlock()
	if closesAfter != closesBefore {
		t.Errorf("coalesced /kill-thread --restart must NOT kill the agent: CloseBead calls went %d→%d", closesBefore, closesAfter)
	}
}

// TestKillThreadKill_NotDebounced locks in that a PLAIN kill (no --restart) is
// never debounced — it is a deliberate terminal action, not a reopen. Even
// while a restart window is open for the agent, `/kill-thread <agent>` must
// proceed to kill it.
func TestKillThreadKill_NotDebounced(t *testing.T) {
	daemon := newMockDaemon()
	bot, _, channel, _, agentName := seedRestartAgentRecording(t, daemon)

	// Open a restart window for this agent (as if a restart just fired).
	bot.reopenDebouncer.Allow(agentName)

	closesBefore := len(daemon.closed)
	// Plain kill (no --restart) must NOT be coalesced by the open window.
	bot.handleKillThreadCommand(beadsapi.WithOperator(context.Background(), "oidc:opener@example.com"), slack.SlashCommand{
		Command:   "/kill-thread",
		Text:      agentName,
		ChannelID: channel,
		UserID:    "U-CLICKER",
	})
	// Poll for the kill to land (killAgent + CloseBead run in a goroutine).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		daemon.mu.Lock()
		n := len(daemon.closed)
		daemon.mu.Unlock()
		if n > closesBefore {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	daemon.mu.Lock()
	closesAfter := len(daemon.closed)
	daemon.mu.Unlock()
	if closesAfter <= closesBefore {
		t.Errorf("a plain /kill-thread must not be debounced by an open restart window: expected a CloseBead, calls stayed at %d", closesBefore)
	}
}
