package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// reactionTriggerBead builds a new-style trigger:slack-reaction bead for the
// bot-wired engine tests. last_checked is left to the mockTriggerDaemon's
// auto-injection (past timestamp) so the runtime-bootstrap guard does not
// suppress first-eval firing.
func reactionTriggerBead(id, emoji, action, prompt string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     id,
		Type:   "trigger",
		Title:  "React trigger: " + emoji,
		Status: "open",
		Fields: map[string]string{
			"emoji":   emoji,
			"action":  action,
			"prompt":  prompt,
			"project": "gardenio",
			"role":    "crew",
			"enabled": "true",
		},
		Labels: []string{"trigger:slack-reaction"},
	}
}

// TestHandleReactionAdded_BuffersIntoTriggerSource is the core acceptance test:
// a reaction_added event delivered to handleReactionAdded reaches the wired
// SlackReactionTriggerSource buffer (BufferReaction), and the bot-owned engine
// then evaluates it into a spawn for a matching trigger:slack-reaction bead.
func TestHandleReactionAdded_BuffersIntoTriggerSource(t *testing.T) {
	daemon := newMockDaemon() // bot.daemon: serves loadReactionRules (no legacy rules)
	srv := newFakeSlackServerWithHistory(t, "E2E results: e2e-test-summary MR !789")
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	// Wire the unified trigger path with a separate TriggerBeadClient holding
	// one new-style trigger:slack-reaction bead.
	triggerDaemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			reactionTriggerBead("react-1", "runner", "spawn_agent", "Re-run e2e for MR {{mr_iid}}"),
		},
	}
	bot.EnableReactionTriggers(triggerDaemon, nil, nil)

	if bot.reactionTrigger == nil || bot.triggerEngine == nil {
		t.Fatal("EnableReactionTriggers did not wire reactionTrigger/triggerEngine")
	}

	// Deliver the reaction event.
	ev := makeReactionEvent("U_USER", "runner", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	// 1. The event must have reached the source buffer.
	if got := bot.reactionTrigger.PendingCount(); got != 1 {
		t.Fatalf("expected 1 buffered reaction event, got %d", got)
	}

	// 2. Evaluating the source must fire the matching trigger → one spawn.
	bot.triggerEngine.EvaluateSource(context.Background(), bot.reactionTrigger.Name())

	triggerDaemon.mu.Lock()
	spawned := len(triggerDaemon.spawnedNames)
	prompt := triggerDaemon.lastPrompt
	triggerDaemon.mu.Unlock()

	if spawned != 1 {
		t.Fatalf("expected 1 agent spawned via the trigger engine, got %d", spawned)
	}
	// The fetched message text carried "MR !789"; the {{mr_iid}} template var
	// should have been interpolated, proving the message text rode along with
	// the buffered event (not just the bare reaction).
	if want := "789"; !strings.Contains(prompt, want) {
		t.Errorf("expected spawned prompt to contain MR IID %q, got %q", want, prompt)
	}
}

// TestHandleReactionAdded_LegacyAndTriggerCoexist proves the unified trigger
// path is additive: when both a legacy reaction-rule (handled inline by
// bot_reactions.go via bot.daemon) and the unified source are wired, a single
// reaction fires the legacy spawn exactly once AND buffers for the engine.
// The unified source runs with SkipLegacy, so the same legacy rule is not
// double-spawned through the engine.
func TestHandleReactionAdded_LegacyAndTriggerCoexist(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	srv := newFakeSlackServerWithHistory(t, "E2E results: e2e-test-summary MR !789")
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.botUserID = "BOT"

	// Legacy reaction-rule served to bot_reactions.go via bot.daemon cache.
	bot.reactionRuleCache = &reactionRuleCache{
		rules: []*beadsapi.BeadDetail{
			makeReactionRule("rule-1", "runner", "e2e-test-summary", "monorepo", "crew",
				"Legacy: re-run e2e MR {mr_iid}"),
		},
		fetchedAt: time.Now(),
		ttl:       5 * time.Minute,
	}

	// Unified path wired with an empty trigger daemon (no trigger beads); we are
	// only asserting the legacy path still fires AND the buffer receives.
	triggerDaemon := &mockTriggerDaemon{}
	bot.EnableReactionTriggers(triggerDaemon, nil, nil)

	ev := makeReactionEvent("U_USER", "runner", "C123", "1234567890.123456")
	bot.handleReactionAdded(context.Background(), ev)

	// Legacy spawn fired exactly once via bot.daemon.
	if got := len(filterAgentBeads(daemon.beads)); got != 1 {
		t.Fatalf("expected 1 legacy agent spawn, got %d", got)
	}
	// Unified source received the event.
	if got := bot.reactionTrigger.PendingCount(); got != 1 {
		t.Fatalf("expected 1 buffered reaction event for unified path, got %d", got)
	}
}

// TestEnableReactionTriggers_NilDaemonIsNoOp guards the defensive nil-daemon
// path: the bot must remain usable (legacy-only) with no unified wiring.
func TestEnableReactionTriggers_NilDaemonIsNoOp(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	bot.EnableReactionTriggers(nil, nil, nil)
	if bot.reactionTrigger != nil || bot.triggerEngine != nil {
		t.Fatal("EnableReactionTriggers(nil, ...) must not wire the unified path")
	}

	// runReactionTriggerLoop must return immediately (not block) when unwired.
	done := make(chan struct{})
	go func() {
		bot.runReactionTriggerLoop(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runReactionTriggerLoop did not return for an unwired bot")
	}
}
