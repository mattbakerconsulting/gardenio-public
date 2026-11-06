package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// This file is the E2E acceptance for the first-class reopen debounce
// (ka-0000000034 / backfill ka-0000007443). It is the twin of the unit /
// integration coverage in bot_reopen_debounce_test.go, and it closes a real
// residual gap that those tests structurally cannot: they build the bot with
// the newTestBot fixture, which wires reopenDebouncer INDEPENDENTLY of the
// production NewBot constructor. So the exact regression the backfill bead
// names as the realistic one — a dropped `reopenDebouncer: newReopenDebouncer()`
// line in NewBot (bot.go), or a dropped `b.reopenDebouncer.Cleanup()` in the
// Run loop — makes the field nil, Allow() then returns true unconditionally
// (nil-receiver-allows), debounce silently disables in prod, and NOT ONE of the
// existing 12 tests reds, because none of them exercise NewBot.
//
// WHY A GO *_e2e_test.go, NOT A SHELL / PLAYWRIGHT MODULE
//
// The three reopen entrypoints (the "Restart Agent" button, the `@gardenio
// restart` mention, and `/kill-thread <agent> --restart`) all arrive over Slack
// Socket Mode on the deployed bridge — socketModeEnabled(cfg) is always true in
// prod/e2e overlays because both SLACK_BOT_TOKEN and SLACK_APP_TOKEN are
// required secretKeyRefs. The HTTP `/slack/interactions` route that could take a
// forged block_actions POST is registered ONLY in the webhook fallback
// (slack-bridge/main.go), which those deployments never enter. And slackmock
// (gardenio/tests/slackmock) mocks only the OUTBOUND Slack API the bridge calls;
// it has no inbound event/interaction injector. So no shell or Playwright e2e
// surface can drive these entrypoints at all — the "via the slackmock surface"
// phrasing in the bead's Scope describes a surface that does not exist. The
// honest system-level acceptance for this in-process bridge guard is a Go test
// that builds the bot the PRODUCTION way and drives the REAL inbound routing.
// (Sibling precedent this sprint: gd-MfK9UlebWg, "e2e for reverse MR->bead
// reconciler over real GitLabClient", landed as an in-package *_e2e_test.go.)
//
// WHAT THESE TESTS DRIVE
//
// The bot is constructed via NewBot(BotConfig{...}) — the same call
// cmd/slack-bridge/main.go makes — with only its Slack API client swapped for a
// fake server (mirroring bot_prune_test.go's production-constructor test). Then
// they push a genuine `restart_thread_agent` block_actions InteractionCallback
// through the real dispatch chain: handleInteraction -> handleBlockActions ->
// (case actionID == "restart_thread_agent") -> handleRestartThreadAgent. So the
// debounce is reached through exactly the production wiring, not a hand-set
// field. The assertions are behavioural — they count reminted agent beads the
// mock daemon actually saw — so they are independent of the debouncer's
// internals and would survive a refactor that preserved the behaviour.

// newE2EReopenBot builds a Bot through the PRODUCTION NewBot constructor (the
// path cmd/slack-bridge/main.go uses), with only the Slack API client swapped
// for a fake server. This is what makes the wiring of reopenDebouncer inside
// NewBot load-bearing for these tests: unlike newTestBot, nothing here sets the
// field, so a NewBot that forgets to construct it leaves it nil.
func newE2EReopenBot(t *testing.T, daemon BeadClient) *Bot {
	t.Helper()

	// users.info names the clicker opener@example.com, the seeded agents'
	// owner, so the restart button's owner check (bot_kill_owner.go) passes.
	slackSrv := newSlackServerWithUsersInfo(t, "opener@example.com")
	t.Cleanup(slackSrv.Close)

	bot := NewBot(BotConfig{
		BotToken:      "xoxb-test",
		AppToken:      "xapp-test",
		Channel:       "C-reopen",
		ThreadingMode: "agent",
		Daemon:        daemon,
		Logger:        slog.Default(),
	})
	// Route Slack API calls to the fake server (the constructor built a real
	// client against slack.com). Same override bot_prune_test.go uses.
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"))
	return bot
}

// seedE2EReopenAgent seeds a live thread agent bead the restart path can find,
// kill, and remint, and returns the coordinates. Kept parallel to
// seedRestartAgent in bot_reopen_debounce_test.go but independent of it so this
// file stands on its own production-constructor rig.
func seedE2EReopenAgent(daemon *mockDaemon, channel string) (agentName, threadTS string) {
	agentName = "thread-1700000900-909090"
	threadTS = "1700000900.909090"
	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:        "bd-e2e-reopen-existing",
		Title:     agentName,
		Type:      "agent",
		Status:    "open",
		CreatedAt: time.Now().Add(-10 * time.Minute),
		Labels:    []string{"slack-thread", "project:gardenio"},
		// The /kill-thread tests run as this owner (bot_kill_owner.go).
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
	return agentName, threadTS
}

// e2eRemintCount returns how many actual restarts (kill + CreateBead remint)
// have landed for agentName. It counts agent-typed beads whose id carries the
// mock daemon's CreateBead mint prefix ("bd-chat-", see mockDaemon.CreateBead),
// which is exactly the set of beads a restart minted — it excludes the
// PRE-SEEDED live bead (keyed by agent name / id "bd-e2e-reopen-existing*") and
// the thread-head TASK bead getOrCreateThreadBead mints. Keying on the mint
// prefix rather than a single hardcoded exclusion id is what keeps the
// coalescing and per-agent assertions non-vacuous: the seeded bead can never be
// mistaken for a restart.
func e2eRemintCount(daemon *mockDaemon, agentName string) int {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	n := 0
	for id, b := range daemon.beads {
		if b.Type == "agent" && b.Title == agentName && strings.HasPrefix(id, "bd-chat-") {
			n++
		}
	}
	return n
}

// waitForE2ERemints polls until at least want remints exist for agentName (the
// restart path mints in a goroutine) or the deadline passes, returning the
// final count.
func waitForE2ERemints(daemon *mockDaemon, agentName string, want int) int {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e2eRemintCount(daemon, agentName) >= want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return e2eRemintCount(daemon, agentName)
}

// restartButtonCallback builds the block_actions payload Slack delivers when a
// human clicks the "Restart Agent" button — action_id "restart_thread_agent",
// value = agent identity — carrying the thread coords the handler reads.
func restartButtonCallback(channel, threadTS, agentName string) slack.InteractionCallback {
	cb := slack.InteractionCallback{
		Type: slack.InteractionTypeBlockActions,
		User: slack.User{ID: "U-CLICKER"},
	}
	cb.Channel.GroupConversation.Conversation.ID = channel
	cb.Container.ThreadTs = threadTS
	cb.ActionCallback.BlockActions = []*slack.BlockAction{
		{ActionID: "restart_thread_agent", Value: agentName},
	}
	return cb
}

// TestE2EReopenDebounce_ButtonDoubleClickCoalesces is the headline acceptance:
// two "Restart Agent" clicks for the SAME agent inside the debounce window,
// pushed through the FULL production dispatch chain (handleInteraction ->
// handleBlockActions -> handleRestartThreadAgent) on a bot built by the real
// NewBot constructor, must produce exactly ONE reminted agent bead — the second
// click is coalesced, not a second competing kill+remint.
//
// This is the test that reds on the realistic regression the bead names: drop
// the `reopenDebouncer` line in NewBot and the field is nil, Allow() returns
// true for both clicks, and TWO remints fire → this fails. (Empirically
// verified during authoring: commenting that line out flips this test red while
// leaving every newTestBot-based test green.)
func TestE2EReopenDebounce_ButtonDoubleClickCoalesces(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-reopen")
	bot := newE2EReopenBot(t, daemon)
	agentName, threadTS := seedE2EReopenAgent(daemon, "C-reopen")

	cb := restartButtonCallback("C-reopen", threadTS, agentName)

	// First click: proceeds through the real routing and restarts (remint #1).
	bot.handleInteraction(context.Background(), cb)
	if got := waitForE2ERemints(daemon, agentName, 1); got != 1 {
		t.Fatalf("after first button click expected 1 reminted agent bead, got %d", got)
	}

	// Rapid second click within the window: must be coalesced — no 2nd remint.
	bot.handleInteraction(context.Background(), cb)
	// Give any erroneously-spawned goroutine time to mint a second bead.
	time.Sleep(300 * time.Millisecond)
	if got := e2eRemintCount(daemon, agentName); got != 1 {
		t.Errorf("rapid second button click must be debounced through the production NewBot wiring: expected still 1 reminted bead, got %d", got)
	}
}

// TestE2EReopenDebounce_RestartAfterWindowIsHonored is the anti-vacuity control
// the bead's Scope explicitly requires: without it, a bot that simply NEVER
// restarts anything (e.g. an over-eager guard that coalesces every request)
// would pass the coalescing test above vacuously. Here a third click AFTER the
// window elapses must produce a SECOND remint — proving the debounce is a
// coalescing window, not a permanent lock, and that the restart path really
// fires when it should.
func TestE2EReopenDebounce_RestartAfterWindowIsHonored(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-reopen")
	bot := newE2EReopenBot(t, daemon)
	agentName, threadTS := seedE2EReopenAgent(daemon, "C-reopen")

	cb := restartButtonCallback("C-reopen", threadTS, agentName)

	bot.handleInteraction(context.Background(), cb)
	if got := waitForE2ERemints(daemon, agentName, 1); got != 1 {
		t.Fatalf("after first click expected 1 reminted agent bead, got %d", got)
	}

	// Age the debounce stamp past the window instead of sleeping the full 10s.
	// This reaches through the production-wired debouncer (nil here would be the
	// regression), so it also asserts NewBot actually constructed one.
	if bot.reopenDebouncer == nil {
		t.Fatal("production NewBot must construct a non-nil reopenDebouncer (wiring regression)")
	}
	bot.reopenDebouncer.mu.Lock()
	bot.reopenDebouncer.last[agentName] = time.Now().Add(-reopenDebounceWindow - time.Second)
	bot.reopenDebouncer.mu.Unlock()

	// A click after the window must proceed and mint a 2nd new bead.
	bot.handleInteraction(context.Background(), cb)
	if got := waitForE2ERemints(daemon, agentName, 2); got < 2 {
		t.Errorf("a restart after the debounce window must be honored: expected >=2 reminted beads, got %d", got)
	}
}

// TestE2EReopenDebounce_DifferentAgentsNotCoalesced is the per-agent-key
// negative control: the window is keyed on agent name, so a rapid restart of a
// DIFFERENT agent must NOT be coalesced by another agent's in-flight restart.
// If the key ever collapsed to a global one, this reds. Driven end-to-end
// through the production routing for both agents.
func TestE2EReopenDebounce_DifferentAgentsNotCoalesced(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-reopen")
	bot := newE2EReopenBot(t, daemon)

	agentA, threadA := seedE2EReopenAgent(daemon, "C-reopen")

	// Seed a second, distinct thread agent.
	agentB := "thread-1700000901-808080"
	threadB := "1700000901.808080"
	daemon.mu.Lock()
	daemon.beads[agentB] = &beadsapi.BeadDetail{
		ID:         "bd-e2e-reopen-existing-b",
		Title:      agentB,
		Type:       "agent",
		Status:     "open",
		CreatedAt:  time.Now().Add(-10 * time.Minute),
		Labels:     []string{"slack-thread", "project:gardenio"},
		OperatedBy: "oidc:opener@example.com",
		Fields: map[string]string{
			"agent":                agentB,
			"project":              "gardenio",
			"role":                 "thread",
			"slack_thread_channel": "C-reopen",
			"slack_thread_ts":      threadB,
			"slack_user_id":        "U-OPENER",
		},
	}
	daemon.mu.Unlock()

	// Restart A, then immediately restart B — B is a different agent, so its
	// restart must be honored despite A's window being open.
	bot.handleInteraction(context.Background(), restartButtonCallback("C-reopen", threadA, agentA))
	if got := waitForE2ERemints(daemon, agentA, 1); got != 1 {
		t.Fatalf("agent A first click expected 1 reminted bead, got %d", got)
	}
	bot.handleInteraction(context.Background(), restartButtonCallback("C-reopen", threadB, agentB))
	if got := waitForE2ERemints(daemon, agentB, 1); got != 1 {
		t.Errorf("a different agent's restart must NOT be coalesced by another agent's window: expected 1 reminted bead for agent B, got %d", got)
	}
}

// TestE2EReopenDebounce_PlainKillNeverDebounced locks in the other negative
// control from the bead's Scope, driven through the real slash-command handler:
// a PLAIN `/kill-thread <agent>` (no --restart) is a deliberate terminal
// action, NOT a reopen, so it is NEVER debounced — even while a restart window
// is open for that agent, the kill must proceed. If the debounce ever
// over-applied to plain kills, this reds. Uses the production-NewBot bot so the
// debouncer under test is the one NewBot wired.
func TestE2EReopenDebounce_PlainKillNeverDebounced(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-reopen")
	bot := newE2EReopenBot(t, daemon)
	agentName, _ := seedE2EReopenAgent(daemon, "C-reopen")

	// Open a restart window for this agent (as if a restart just fired). This
	// touches the production-wired debouncer.
	if bot.reopenDebouncer == nil {
		t.Fatal("production NewBot must construct a non-nil reopenDebouncer (wiring regression)")
	}
	bot.reopenDebouncer.Allow(agentName)

	daemon.mu.Lock()
	closesBefore := len(daemon.closed)
	daemon.mu.Unlock()

	// Plain kill (no --restart) must NOT be coalesced by the open window.
	bot.handleKillThreadCommand(beadsapi.WithOperator(context.Background(), "oidc:opener@example.com"), slack.SlashCommand{
		Command:   "/kill-thread",
		Text:      agentName,
		ChannelID: "C-reopen",
		UserID:    "U-CLICKER",
	})

	// killAgent + CloseBead run in a goroutine — poll for the kill to land.
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
		t.Errorf("a plain /kill-thread must never be debounced by an open restart window: expected a CloseBead, calls stayed at %d", closesBefore)
	}
}
