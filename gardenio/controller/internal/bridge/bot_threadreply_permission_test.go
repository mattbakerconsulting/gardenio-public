package bridge

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	slackevents "github.com/slack-go/slack/slackevents"
)

// seedOpenDecisionWithOwner stores an OPEN decision bead assigned to agentName
// plus an agent bead carrying the owning operator's slack_user_id, and tracks
// the decision's card (channel, thread_ts) so getDecisionByThread maps a reply
// back to the bead. Mirrors seedDecisionWithOwner but sets Status so the
// thread-reply resolve branch is reachable.
func seedOpenDecisionWithOwner(t *testing.T, daemon *mockDaemon, bot *Bot, decisionID, agentName, ownerUserID, channel, threadTS string) {
	t.Helper()
	daemon.mu.Lock()
	daemon.beads[decisionID] = &beadsapi.BeadDetail{
		ID:       decisionID,
		Type:     "decision",
		Status:   "open",
		Assignee: agentName,
		Fields:   map[string]string{},
	}
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     agentName,
		Type:   "agent",
		Fields: map[string]string{"slack_user_id": ownerUserID},
	}
	daemon.mu.Unlock()
	if err := bot.state.SetDecisionMessage(decisionID, MessageRef{
		ChannelID: channel,
		Timestamp: threadTS,
		Agent:     agentName,
	}); err != nil {
		t.Fatalf("SetDecisionMessage: %v", err)
	}
}

// TestHandleThreadReply_NonOwnerAllowed pins the permissive resolve policy on
// the thread-reply path (operator ruling 2026-08-27, thread ka-0000006045:
// "loosen up decision answering restrictions right now, get the UX right").
//
// This deliberately inverts the former ka-0000001906 assertion. That gate was
// owner-only and could not express the case that matters — a decision addressed
// to a human who is not the requesting agent's operator — so it is retired in
// favour of the detective control (CrossOperatorNotify DMs the addressee when
// somebody else answers). The narrower, correctly-addressed rule arrives with
// decision `operated_by` (ka-0000000024).
func TestHandleThreadReply_NonOwnerAllowed(t *testing.T) {
	daemon := newMockDaemon()
	sm, err := NewStateManager(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	ec := newEphemeralCapture(t)
	bot := newTestBot(daemon, ec.srv)
	bot.state = sm

	seedOpenDecisionWithOwner(t, daemon, bot, "dec-1", "agent-x", "UOWNER", "C123", "1700.0001")

	ev := &slackevents.MessageEvent{
		User:            "USTRANGER",
		Channel:         "C123",
		ThreadTimeStamp: "1700.0001",
		Text:            "blue",
	}
	bot.dispatchInbound(context.Background(), slackReplyInbound(ev, ""))

	closed := daemon.getClosed()
	if len(closed) != 1 || closed[0].BeadID != "dec-1" {
		t.Fatalf("a non-addressee thread reply should resolve the decision under the permissive policy; got closes: %+v", closed)
	}
	for _, m := range ec.all() {
		if strings.Contains(m.text, "permission") {
			t.Errorf("non-addressee must not be told they lack permission, got %q", m.text)
		}
	}
}

// TestHandleThreadReply_OwnerAllowed asserts the owner replying in the decision
// thread still resolves the decision (the gate does not regress the happy path).
func TestHandleThreadReply_OwnerAllowed(t *testing.T) {
	daemon := newMockDaemon()
	sm, err := NewStateManager(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	bot := newTestBot(daemon, newFakeSlackServer(t))
	bot.state = sm

	seedOpenDecisionWithOwner(t, daemon, bot, "dec-1", "agent-x", "UOWNER", "C123", "1700.0002")

	ev := &slackevents.MessageEvent{
		User:            "UOWNER",
		Channel:         "C123",
		ThreadTimeStamp: "1700.0002",
		Text:            "blue",
	}
	bot.dispatchInbound(context.Background(), slackReplyInbound(ev, ""))

	closed := daemon.getClosed()
	if len(closed) != 1 || closed[0].BeadID != "dec-1" {
		t.Fatalf("owner thread reply should resolve the decision; got closes: %+v", closed)
	}
}

// TestHandleThreadReply_OwnerlessFailsOpen asserts a decision with no resolvable
// human owner keeps today's behavior: any channel member may resolve it via a
// thread reply (checkDecisionResolvePermission fail-opens for ownerless
// decisions).
func TestHandleThreadReply_OwnerlessFailsOpen(t *testing.T) {
	daemon := newMockDaemon()
	sm, err := NewStateManager(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	bot := newTestBot(daemon, newFakeSlackServer(t))
	bot.state = sm

	// Decision assigned to an agent with no slack_user_id → ownerless.
	daemon.mu.Lock()
	daemon.beads["dec-1"] = &beadsapi.BeadDetail{
		ID:       "dec-1",
		Type:     "decision",
		Status:   "open",
		Assignee: "agent-x",
		Fields:   map[string]string{},
	}
	daemon.beads["agent-x"] = &beadsapi.BeadDetail{
		ID:     "agent-x",
		Type:   "agent",
		Fields: map[string]string{}, // no slack_user_id
	}
	daemon.mu.Unlock()
	if err := sm.SetDecisionMessage("dec-1", MessageRef{ChannelID: "C123", Timestamp: "1700.0003", Agent: "agent-x"}); err != nil {
		t.Fatalf("SetDecisionMessage: %v", err)
	}

	ev := &slackevents.MessageEvent{
		User:            "USTRANGER",
		Channel:         "C123",
		ThreadTimeStamp: "1700.0003",
		Text:            "blue",
	}
	bot.dispatchInbound(context.Background(), slackReplyInbound(ev, ""))

	closed := daemon.getClosed()
	if len(closed) != 1 || closed[0].BeadID != "dec-1" {
		t.Fatalf("ownerless decision should still resolve via any thread reply; got closes: %+v", closed)
	}
}
