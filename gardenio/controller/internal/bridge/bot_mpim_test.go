package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// Group-DM (mpim) inbound routing tests (ka-0000006051; epic ka-0000003709).
//
// A group DM (message.mpim) is routed by handleMessageEvent analogous to the
// "group" (private channel) branch, NOT the "im" (DM) branch: the bot is one of
// several human members, so a TOP-LEVEL message must @mention the bot to be
// acted on; unmentioned chatter is dropped (no spam). Thread replies in a bound
// mpim forward to the bound agent (channel-type-agnostic binding). Project/role
// overrides apply for free via handleAppMention's parseProjectOverride/
// parseRoleOverride.

// mpimSlackServer returns a Slack test server that satisfies the calls a mention
// spawn makes (conversations.replies for thread context, generic OK otherwise).
func mpimSlackServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
}

// TestHandleMessageEvent_MpimMention_SynthesizesAppMention asserts a top-level
// mpim message that @mentions the bot is routed through handleAppMention and
// spawns an agent — the same behavior as the "group" branch.
func TestHandleMessageEvent_MpimMention_SynthesizesAppMention(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "G-MPIM")

	slackSrv := mpimSlackServer(t)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U-HUMAN",
		Text:        "hey <@U-BOT> ship the feature",
		Channel:     "G-MPIM",
		ChannelType: "mpim",
		TimeStamp:   "1111.2222",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from mpim mention, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (from mpim channel mapping), got %q", b.Fields["project"])
		}
	}
}

// TestHandleMessageEvent_MpimNonMention_Ignored asserts a top-level mpim message
// WITHOUT an @mention is dropped — no agent is spawned. This is the group-branch
// behavior (require @mention), NOT the im branch (every message directed at the
// bot). Prevents gardenio spamming every message in a group DM it is a member of.
func TestHandleMessageEvent_MpimNonMention_Ignored(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "G-MPIM")

	slackSrv := mpimSlackServer(t)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U-HUMAN",
		Text:        "just chatting with the other humans here",
		Channel:     "G-MPIM",
		ChannelType: "mpim",
		TimeStamp:   "1111.2222",
	})

	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Errorf("expected 0 agent beads for unmentioned mpim message, got %d", n)
	}
}

// TestHandleMessageEvent_MpimMentionOverride asserts a project override typed in
// a top-level mpim mention is honored — parseProjectOverride runs inside
// handleAppMention for free, exactly as it does for a group-channel mention.
func TestHandleMessageEvent_MpimMentionOverride(t *testing.T) {
	daemon := newMockDaemon()
	// Two projects seeded; the mpim channel maps to neither, so only the
	// explicit override can pick "monorepo".
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	slackSrv := mpimSlackServer(t)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:        "U-HUMAN",
		Text:        "<@U-BOT> project:monorepo fix the flaky test",
		Channel:     "G-MPIM",
		ChannelType: "mpim",
		TimeStamp:   "3333.4444",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from mpim mention with override, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo (from mpim mention override), got %q", b.Fields["project"])
		}
	}
}

// TestHandleMessageEvent_MpimThreadReply_ForwardsToBoundAgent asserts a
// non-mention thread reply in a bound mpim forwards to the bound agent. The
// thread↔agent binding keys on (channel, thread_ts) and is channel-type-
// agnostic, so this works with no mpim-specific binding code.
func TestHandleMessageEvent_MpimThreadReply_ForwardsToBoundAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-1111-2222",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-1111-2222",
			"slack_thread_channel": "G-MPIM",
			"slack_thread_ts":      "1111.2222",
		},
	}

	slackSrv := mpimSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.lastThreadNudge = make(map[string]time.Time)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("G-MPIM", "1111.2222", "thread-1111-2222")
	bot.state = state

	// Non-mention thread reply in the bound mpim.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U-HUMAN",
		Text:            "follow up info",
		Channel:         "G-MPIM",
		ChannelType:     "mpim",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	})

	found := false
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == "thread-1111-2222" && hasLabel(bead.Labels, "slack-thread-reply") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a slack-thread-reply bead assigned to the bound agent for the mpim thread reply")
	}
}
