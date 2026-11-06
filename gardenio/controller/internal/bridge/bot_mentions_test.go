package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

func TestStripBotMention(t *testing.T) {
	tests := []struct {
		name      string
		text      string
		botUserID string
		want      string
	}{
		{
			name:      "mention at start",
			text:      "<@U123BOT> check the logs",
			botUserID: "U123BOT",
			want:      "check the logs",
		},
		{
			name:      "mention in middle",
			text:      "hey <@U123BOT> check the logs",
			botUserID: "U123BOT",
			want:      "hey  check the logs",
		},
		{
			name:      "no mention",
			text:      "check the logs",
			botUserID: "U123BOT",
			want:      "check the logs",
		},
		{
			name:      "multiple mentions",
			text:      "<@U123BOT> hello <@U123BOT>",
			botUserID: "U123BOT",
			want:      "hello",
		},
		{
			name:      "only mention",
			text:      "<@U123BOT>",
			botUserID: "U123BOT",
			want:      "",
		},
		{
			name:      "different bot ID",
			text:      "<@UOTHER> check the logs",
			botUserID: "U123BOT",
			want:      "<@UOTHER> check the logs",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripBotMention(tt.text, tt.botUserID)
			if got != tt.want {
				t.Errorf("stripBotMention(%q, %q) = %q, want %q",
					tt.text, tt.botUserID, got, tt.want)
			}
		})
	}
}

func TestGetAgentByThread(t *testing.T) {
	b := &Bot{
		agentCards: map[string]MessageRef{
			"gardenio/crew/hq": {
				ChannelID: "C-agents",
				Timestamp: "1111.2222",
				Agent:     "gardenio/crew/hq",
			},
			"gardenio/crew/k8s": {
				ChannelID: "C-agents",
				Timestamp: "3333.4444",
				Agent:     "gardenio/crew/k8s",
			},
		},
	}

	t.Run("matching thread", func(t *testing.T) {
		got := b.getAgentByThread("C-agents", "1111.2222")
		if got != "gardenio/crew/hq" {
			t.Errorf("got %q, want %q", got, "gardenio/crew/hq")
		}
	})

	t.Run("different agent", func(t *testing.T) {
		got := b.getAgentByThread("C-agents", "3333.4444")
		if got != "gardenio/crew/k8s" {
			t.Errorf("got %q, want %q", got, "gardenio/crew/k8s")
		}
	})

	t.Run("non-matching channel", func(t *testing.T) {
		got := b.getAgentByThread("C-other", "1111.2222")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("non-matching timestamp", func(t *testing.T) {
		got := b.getAgentByThread("C-agents", "9999.0000")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("empty map", func(t *testing.T) {
		empty := &Bot{agentCards: map[string]MessageRef{}}
		got := empty.getAgentByThread("C-agents", "1111.2222")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})
}

func TestGetAgentByThread_StateFallback(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Agent card only in persisted state, not in hot cache.
	_ = state.SetAgentCard("gardenio/crew/ops", MessageRef{
		ChannelID: "C-ops",
		Timestamp: "5555.6666",
		Agent:     "gardenio/crew/ops",
	})

	b := &Bot{
		agentCards: map[string]MessageRef{}, // empty hot cache
		state:      state,
	}

	got := b.getAgentByThread("C-ops", "5555.6666")
	if got != "gardenio/crew/ops" {
		t.Errorf("got %q, want %q", got, "gardenio/crew/ops")
	}
}

func TestGetAgentByThread_ThreadAgents(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Set a thread→agent mapping (thread-spawned agent).
	_ = state.SetThreadAgent("C-support", "7777.8888", "gardenio/crew/support")

	b := &Bot{
		agentCards: map[string]MessageRef{}, // empty hot cache
		state:      state,
	}

	t.Run("found via threadAgents", func(t *testing.T) {
		got := b.getAgentByThread("C-support", "7777.8888")
		if got != "gardenio/crew/support" {
			t.Errorf("got %q, want %q", got, "gardenio/crew/support")
		}
	})

	t.Run("threadAgents takes priority over agentCards", func(t *testing.T) {
		// Same channel/ts exists in both threadAgents and agentCards.
		b.mu.Lock()
		b.agentCards["gardenio/crew/other"] = MessageRef{
			ChannelID: "C-support",
			Timestamp: "7777.8888",
			Agent:     "gardenio/crew/other",
		}
		b.mu.Unlock()

		got := b.getAgentByThread("C-support", "7777.8888")
		if got != "gardenio/crew/support" {
			t.Errorf("threadAgents should take priority: got %q, want %q", got, "gardenio/crew/support")
		}
	})

	t.Run("not found in threadAgents falls through to agentCards", func(t *testing.T) {
		b.mu.Lock()
		b.agentCards["gardenio/crew/k8s"] = MessageRef{
			ChannelID: "C-k8s",
			Timestamp: "9999.0000",
			Agent:     "gardenio/crew/k8s",
		}
		b.mu.Unlock()

		got := b.getAgentByThread("C-k8s", "9999.0000")
		if got != "gardenio/crew/k8s" {
			t.Errorf("got %q, want %q", got, "gardenio/crew/k8s")
		}
	})

	t.Run("matches card posted in-thread via ThreadTS", func(t *testing.T) {
		// Card posted as a reply in thread 5555.6666 — card's own timestamp
		// is 5555.7777 but ThreadTS points to the parent thread.
		b.mu.Lock()
		b.agentCards["thread-bound-agent"] = MessageRef{
			ChannelID: "C-thread",
			Timestamp: "5555.7777", // card's own ts
			ThreadTS:  "5555.6666", // parent thread ts
			Agent:     "thread-bound-agent",
		}
		b.mu.Unlock()

		// Should match when looking up the parent thread ts.
		got := b.getAgentByThread("C-thread", "5555.6666")
		if got != "thread-bound-agent" {
			t.Errorf("got %q, want %q", got, "thread-bound-agent")
		}

		// Should also match via card's own ts (replies under the card itself).
		got = b.getAgentByThread("C-thread", "5555.7777")
		if got != "thread-bound-agent" {
			t.Errorf("got %q, want %q", got, "thread-bound-agent")
		}
	})

	t.Run("matches persisted card via ThreadTS", func(t *testing.T) {
		// Store an in-thread card only in persisted state (not hot cache).
		_ = state.SetAgentCard("persisted-thread-agent", MessageRef{
			ChannelID: "C-persist",
			Timestamp: "6666.7777",
			ThreadTS:  "6666.5555",
			Agent:     "persisted-thread-agent",
		})

		got := b.getAgentByThread("C-persist", "6666.5555")
		if got != "persisted-thread-agent" {
			t.Errorf("got %q, want %q", got, "persisted-thread-agent")
		}
	})
}

func TestResolveAgentFromText(t *testing.T) {
	daemon := newMockDaemon()

	// Seed agent beads.
	daemon.beads["bd-agent-hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}
	daemon.beads["bd-agent-k8s"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-k8s",
		Title: "crew-gardenio-crew-k8s",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "k8s",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	b := &Bot{
		daemon: daemon,
		logger: slog.Default(),
	}

	tests := []struct {
		name          string
		text          string
		wantAgent     string
		wantRemaining string
	}{
		{
			name:          "bare agent name",
			text:          "hq check the logs",
			wantAgent:     "crew-gardenio-crew-hq",
			wantRemaining: "check the logs",
		},
		{
			name:          "bare agent name case insensitive",
			text:          "HQ check the logs",
			wantAgent:     "crew-gardenio-crew-hq",
			wantRemaining: "check the logs",
		},
		{
			name:          "full title match",
			text:          "crew-gardenio-crew-k8s deploy now",
			wantAgent:     "crew-gardenio-crew-k8s",
			wantRemaining: "deploy now",
		},
		{
			name:          "no match",
			text:          "hello everyone",
			wantAgent:     "",
			wantRemaining: "hello everyone",
		},
		{
			name:          "agent name only, no remaining text",
			text:          "k8s",
			wantAgent:     "crew-gardenio-crew-k8s",
			wantRemaining: "",
		},
		{
			name:          "empty text",
			text:          "",
			wantAgent:     "",
			wantRemaining: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent, remaining := b.resolveAgentFromText(context.Background(), tt.text)
			if agent != tt.wantAgent {
				t.Errorf("agent = %q, want %q", agent, tt.wantAgent)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestHandleAppMention_InAgentThread(t *testing.T) {
	daemon := newMockDaemon()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:    daemon,
		state:     state,
		logger:    slog.Default(),
		botUserID: "U-BOT",
		agentCards: map[string]MessageRef{
			"gardenio/crew/hq": {
				ChannelID: "C-agents",
				Timestamp: "1111.2222",
				Agent:     "gardenio/crew/hq",
			},
		},
	}

	ctx := context.Background()

	agent := b.getAgentByThread("C-agents", "1111.2222")
	if agent != "gardenio/crew/hq" {
		t.Fatalf("expected agent gardenio/crew/hq, got %q", agent)
	}

	text := stripBotMention("<@U-BOT> check the logs", b.botUserID)
	if text != "check the logs" {
		t.Fatalf("expected stripped text 'check the logs', got %q", text)
	}

	// Create bead via daemon (same as handleAppMention does).
	beadID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       truncateText("Mention: "+text, 80),
		Type:        "task",
		Description: "Mention from testuser in Slack:\n\ncheck the logs\n\n---\n[slack:C-agents:1111.2222]",
		Assignee:    extractAgentName(agent),
		Labels:      []string{"slack-mention"},
		Priority:    2,
	})
	if err != nil {
		t.Fatalf("CreateBead failed: %v", err)
	}

	if beadID == "" {
		t.Fatal("expected non-empty bead ID")
	}

	// Verify the bead was created with correct properties.
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil {
		t.Fatalf("GetBead failed: %v", err)
	}
	if bead.Assignee != "hq" {
		t.Errorf("bead assignee = %q, want %q", bead.Assignee, "hq")
	}
	if !hasLabel(bead.Labels, "slack-mention") {
		t.Errorf("bead labels = %v, want slack-mention", bead.Labels)
	}

	// Persist message ref.
	_ = state.SetChatMessage(beadID, MessageRef{
		ChannelID: "C-agents",
		Timestamp: "1111.2222",
		Agent:     "gardenio/crew/hq",
	})

	// Verify state was persisted.
	ref, ok := state.GetChatMessage(beadID)
	if !ok {
		t.Fatal("expected chat message in state")
	}
	if ref.ChannelID != "C-agents" || ref.Timestamp != "1111.2222" {
		t.Errorf("message ref = %+v, want C-agents/1111.2222", ref)
	}
}

func TestHandleAppMention_NotInThread_AgentChannel(t *testing.T) {
	daemon := newMockDaemon()

	router := NewRouter(RouterConfig{
		Overrides: map[string]string{
			"gardenio/crew/hq": "C-agents",
		},
	})

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:     daemon,
		state:      state,
		logger:     slog.Default(),
		botUserID:  "U-BOT",
		router:     router,
		agentCards: map[string]MessageRef{},
	}

	// Verify that the channel maps to the correct agent.
	agent := b.router.GetAgentByChannel("C-agents")
	if agent != "gardenio/crew/hq" {
		t.Fatalf("expected agent gardenio/crew/hq from channel mapping, got %q", agent)
	}

	ctx := context.Background()
	text := stripBotMention("<@U-BOT> please review this", b.botUserID)
	if text != "please review this" {
		t.Fatalf("expected stripped text 'please review this', got %q", text)
	}

	beadID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:    truncateText("Mention: "+text, 80),
		Type:     "task",
		Assignee: extractAgentName(agent),
		Labels:   []string{"slack-mention"},
		Priority: 2,
	})
	if err != nil {
		t.Fatalf("CreateBead failed: %v", err)
	}

	_ = state.SetChatMessage(beadID, MessageRef{
		ChannelID: "C-agents",
		Timestamp: "9999.0001",
		Agent:     agent,
	})

	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil {
		t.Fatalf("GetBead failed: %v", err)
	}
	if bead.Assignee != "hq" {
		t.Errorf("bead assignee = %q, want %q", bead.Assignee, "hq")
	}
	if !hasLabel(bead.Labels, "slack-mention") {
		t.Errorf("bead labels = %v, want slack-mention", bead.Labels)
	}

	ref, ok := state.GetChatMessage(beadID)
	if !ok {
		t.Fatal("expected chat message in state")
	}
	if ref.ChannelID != "C-agents" || ref.Timestamp != "9999.0001" {
		t.Errorf("message ref = %+v, want C-agents/9999.0001", ref)
	}
}

func TestHandleAppMention_NonAgentThread(t *testing.T) {
	b := &Bot{
		logger:     slog.Default(),
		botUserID:  "U-BOT",
		agentCards: map[string]MessageRef{},
	}

	// Thread exists but doesn't belong to any agent.
	agent := b.getAgentByThread("C-random", "9999.8888")
	if agent != "" {
		t.Errorf("expected empty agent for non-agent thread, got %q", agent)
	}
}

func TestHandleAppMention_ThreadInAgentChannel_RoutesToAgent(t *testing.T) {
	// Bug: mentioning @gardenio in a thread within an agent's break-out channel
	// would spawn a new thread runner instead of routing to the channel's agent.
	daemon := newMockDaemon()

	// Seed an active agent bead so FindAgentBead("hq") succeeds.
	// The mock looks up by map key, so use "hq" (the short name).
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	// Set up a router with the agent's break-out channel.
	router := NewRouter(RouterConfig{
		Overrides: map[string]string{
			"gardenio/crew/hq": "C-agent-hq",
		},
	})

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.router = router
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	// Mention @gardenio in a thread within the agent's channel,
	// but the thread is NOT the agent card thread (it's a random thread).
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> check the logs",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.0000", // some thread that isn't the agent's card
		Channel:         "C-agent-hq",
	})

	// Verify that a mention bead was created for the existing agent (hq),
	// NOT an agent bead for a new thread runner.
	agentBeads := filterAgentBeads(daemon.beads)
	for _, ab := range agentBeads {
		if ab.ID != "bd-agent-hq" {
			t.Errorf("unexpected new agent bead created: %s (%s) — should route to existing agent", ab.ID, ab.Title)
		}
	}

	// Should have created a task bead (mention tracking) assigned to hq.
	// Filter by slack-mention label to skip the canonical thread bead, which
	// is also a task but unassigned (created as a side effect of the M2d
	// triggered-by dep write).
	var mentionBeads []*beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "message" && hasLabel(b.Labels, "slack-mention") {
			mentionBeads = append(mentionBeads, b)
		}
	}
	if len(mentionBeads) == 0 {
		t.Fatal("expected a mention tracking bead to be created")
	}
	for _, tb := range mentionBeads {
		if tb.Assignee != "hq" {
			t.Errorf("mention bead assignee = %q, want %q", tb.Assignee, "hq")
		}
	}
}

// TestHandleAppMention_BoundThreadTracking_PopulatesSlackUserIDFields is a
// regression test for ka-0000001582: every mention- and thread-derived bead
// created by the slack-bridge must carry fields.slack_user_id and
// fields.operated_by so the Pass-1 A legacy-attribution heuristic can recover
// the originating Slack user even after the audit row's operated_by column is
// rebound on dibs handoffs.
func TestHandleAppMention_BoundThreadTracking_PopulatesSlackUserIDFields(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	router := NewRouter(RouterConfig{
		Overrides: map[string]string{
			"gardenio/crew/hq": "C-agent-hq",
		},
	})

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.router = router
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	channel := "C-agent-hq"
	threadTS := "4242.5555"

	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> investigate the alert",
		TimeStamp:       "4242.9999",
		ThreadTimeStamp: threadTS,
		Channel:         channel,
	})

	// Mention tracking bead must carry slack_user_id + operated_by.
	var tracking *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == "hq" && hasLabel(bead.Labels, "slack-mention") {
			tracking = bead
			break
		}
	}
	if tracking == nil {
		t.Fatal("expected mention tracking bead")
	}
	if got := tracking.Fields["slack_user_id"]; got != "U-USER" {
		t.Errorf("tracking bead fields.slack_user_id = %q, want %q", got, "U-USER")
	}
	if got := tracking.Fields["operated_by"]; got == "" {
		t.Errorf("tracking bead fields.operated_by must be non-empty, got %q", got)
	}

	// Canonical thread bead must also carry slack_user_id + operated_by.
	threadLabel := threadBeadLabel(channel, threadTS)
	var thread *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if hasLabel(bead.Labels, threadLabel) {
			thread = bead
			break
		}
	}
	if thread == nil {
		t.Fatal("expected canonical thread bead")
	}
	if got := thread.Fields["slack_user_id"]; got != "U-USER" {
		t.Errorf("thread bead fields.slack_user_id = %q, want %q", got, "U-USER")
	}
	if got := thread.Fields["operated_by"]; got == "" {
		t.Errorf("thread bead fields.operated_by must be non-empty, got %q", got)
	}
}

// TestHandleAppMention_BoundThreadTracking_WritesTriggeredByDep exercises plan 3.4
// (epic ka-0000000933 / M2d): when a mention in a thread routes to an agent
// bound to the channel, the tracking task bead must record a `triggered-by`
// dep to the canonical Slack-thread bead.
func TestHandleAppMention_BoundThreadTracking_WritesTriggeredByDep(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	router := NewRouter(RouterConfig{
		Overrides: map[string]string{
			"gardenio/crew/hq": "C-agent-hq",
		},
	})

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.router = router
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	channel := "C-agent-hq"
	threadTS := "4242.5555"

	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> investigate the alert",
		TimeStamp:       "4242.9999",
		ThreadTimeStamp: threadTS,
		Channel:         channel,
	})

	// Find the mention tracking bead (assigned to hq, labelled slack-mention).
	var trackingID string
	for _, bead := range daemon.beads {
		if bead.Type == "message" && bead.Assignee == "hq" && hasLabel(bead.Labels, "slack-mention") {
			trackingID = bead.ID
			break
		}
	}
	if trackingID == "" {
		t.Fatal("expected mention tracking bead to be created")
	}

	// Find the thread bead by canonical label.
	threadLabel := threadBeadLabel(channel, threadTS)
	var threadID string
	for _, bead := range daemon.beads {
		if hasLabel(bead.Labels, threadLabel) {
			threadID = bead.ID
			break
		}
	}
	if threadID == "" {
		t.Fatal("expected canonical thread bead to be created")
	}

	var matched bool
	for _, d := range daemon.getDeps() {
		if d.BeadID == trackingID && d.DependsOnID == threadID && d.DepType == "triggered-by" {
			matched = true
			if d.CreatedBy != "slack-bridge" {
				t.Errorf("dep CreatedBy = %q, want slack-bridge", d.CreatedBy)
			}
			break
		}
	}
	if !matched {
		t.Errorf("expected triggered-by dep from %s to %s, got %+v", trackingID, threadID, daemon.getDeps())
	}
}

func TestChat_HandleClosed_SlackMention(t *testing.T) {
	daemon := newMockDaemon()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetChatMessage("bd-mention1", MessageRef{
		ChannelID: "C-agents",
		Timestamp: "1111.2222",
		Agent:     "gardenio/crew/hq",
	})

	daemon.beads["bd-mention1"] = &beadsapi.BeadDetail{
		ID:       "bd-mention1",
		Type:     "task",
		Status:   "closed",
		Assignee: "hq",
		Labels:   []string{"slack-mention"},
		Fields: map[string]string{
			"reason": "Checked the logs, all clear.",
		},
	}

	c := &Chat{
		daemon: daemon,
		state:  state,
		logger: slog.Default(),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:       "bd-mention1",
		Type:     "task",
		Assignee: "hq",
		Labels:   []string{"slack-mention"},
	})
	c.handleClosed(context.Background(), data)

	if _, ok := state.GetChatMessage("bd-mention1"); ok {
		t.Error("expected mention message to be removed from state after close")
	}
}

func TestChat_HandleClosed_IgnoresNonMentionBeads(t *testing.T) {
	daemon := newMockDaemon()
	c := &Chat{
		daemon: daemon,
		logger: slog.Default(),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-other",
		Type:   "task",
		Labels: []string{"bug"},
	})
	c.handleClosed(context.Background(), data)

	if daemon.getGetCalls() != 0 {
		t.Errorf("expected 0 GetBead calls for non-chat/mention bead, got %d", daemon.getGetCalls())
	}
}

func TestParseProjectOverride(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantProject   string
		wantRemaining string
	}{
		{
			name:          "project: syntax",
			text:          "project:gardenio fix the helm chart",
			wantProject:   "gardenio",
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "--project syntax",
			text:          "--project gardenio fix the helm chart",
			wantProject:   "gardenio",
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "project: no remaining text",
			text:          "project:monorepo",
			wantProject:   "monorepo",
			wantRemaining: "",
		},
		{
			name:          "--project no remaining text",
			text:          "--project monorepo",
			wantProject:   "monorepo",
			wantRemaining: "",
		},
		{
			name:          "no override",
			text:          "fix the helm chart",
			wantProject:   "",
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "empty text",
			text:          "",
			wantProject:   "",
			wantRemaining: "",
		},
		{
			name:          "project: with empty value",
			text:          "project: something",
			wantProject:   "",
			wantRemaining: "project: something",
		},
		{
			name:          "--project with no value",
			text:          "--project",
			wantProject:   "",
			wantRemaining: "--project",
		},
		{
			name:          "--project= syntax",
			text:          "--project=gardenio fix the helm chart",
			wantProject:   "gardenio",
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "--project= no remaining text",
			text:          "--project=monorepo",
			wantProject:   "monorepo",
			wantRemaining: "",
		},
		{
			name:          "--project= with empty value",
			text:          "--project= something",
			wantProject:   "",
			wantRemaining: "--project= something",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			project, remaining := parseProjectOverride(tt.text)
			if project != tt.wantProject {
				t.Errorf("project = %q, want %q", project, tt.wantProject)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestParseRoleOverride(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantRole      string
		wantRemaining string
	}{
		{
			name:          "role: syntax",
			text:          "role:qa fix the flaky test",
			wantRole:      "qa",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "--role syntax",
			text:          "--role qa fix the flaky test",
			wantRole:      "qa",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "--role= syntax",
			text:          "--role=qa fix the flaky test",
			wantRole:      "qa",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "role: no remaining text",
			text:          "role:devops",
			wantRole:      "devops",
			wantRemaining: "",
		},
		{
			name:          "--role no remaining text",
			text:          "--role devops",
			wantRole:      "devops",
			wantRemaining: "",
		},
		{
			name:          "--role= no remaining text",
			text:          "--role=devops",
			wantRole:      "devops",
			wantRemaining: "",
		},
		{
			name:          "no override",
			text:          "fix the flaky test",
			wantRole:      "",
			wantRemaining: "fix the flaky test",
		},
		{
			name:          "empty text",
			text:          "",
			wantRole:      "",
			wantRemaining: "",
		},
		{
			name:          "role: with empty value",
			text:          "role: something",
			wantRole:      "",
			wantRemaining: "role: something",
		},
		{
			name:          "--role= with empty value",
			text:          "--role= something",
			wantRole:      "",
			wantRemaining: "--role= something",
		},
		{
			name:          "--role with no value",
			text:          "--role",
			wantRole:      "",
			wantRemaining: "--role",
		},
		{
			// The flag is only recognized as the leading token, mirroring
			// parseProjectOverride — a role: word later in the prompt is prose.
			name:          "role token not at start is left in place",
			text:          "please use role:qa here",
			wantRole:      "",
			wantRemaining: "please use role:qa here",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role, remaining := parseRoleOverride(tt.text)
			if role != tt.wantRole {
				t.Errorf("role = %q, want %q", role, tt.wantRole)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

func TestParseListenFlag(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantListen    bool
		wantRemaining string
	}{
		{
			name:          "no flag",
			text:          "fix the helm chart",
			wantListen:    false,
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "listen at start",
			text:          "--listen fix the helm chart",
			wantListen:    true,
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "listen at end",
			text:          "fix the helm chart --listen",
			wantListen:    true,
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "listen in middle",
			text:          "fix --listen the helm chart",
			wantListen:    true,
			wantRemaining: "fix the helm chart",
		},
		{
			name:          "listen only",
			text:          "--listen",
			wantListen:    true,
			wantRemaining: "",
		},
		{
			name:          "empty text",
			text:          "",
			wantListen:    false,
			wantRemaining: "",
		},
		{
			name:          "listen with project",
			text:          "--listen project:gardenio fix things",
			wantListen:    true,
			wantRemaining: "project:gardenio fix things",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			listen, remaining := parseListenFlag(tt.text)
			if listen != tt.wantListen {
				t.Errorf("listen = %v, want %v", listen, tt.wantListen)
			}
			if remaining != tt.wantRemaining {
				t.Errorf("remaining = %q, want %q", remaining, tt.wantRemaining)
			}
		})
	}
}

// --- parseMentionCommand tests ---

func TestParseMentionCommand_Kill(t *testing.T) {
	cmd, remaining := parseMentionCommand("kill")
	if cmd != "kill" {
		t.Errorf("expected cmd=kill, got %q", cmd)
	}
	if remaining != "" {
		t.Errorf("expected empty remaining, got %q", remaining)
	}
}

func TestParseMentionCommand_KillWithArgs(t *testing.T) {
	cmd, remaining := parseMentionCommand("kill --force")
	if cmd != "kill" {
		t.Errorf("expected cmd=kill, got %q", cmd)
	}
	if remaining != "--force" {
		t.Errorf("expected remaining=--force, got %q", remaining)
	}
}

func TestParseMentionCommand_CaseInsensitive(t *testing.T) {
	cmd, _ := parseMentionCommand("Kill")
	if cmd != "kill" {
		t.Errorf("expected cmd=kill, got %q", cmd)
	}
}

func TestParseMentionCommand_NotACommand(t *testing.T) {
	cmd, remaining := parseMentionCommand("fix the bug please")
	if cmd != "" {
		t.Errorf("expected empty cmd, got %q", cmd)
	}
	if remaining != "fix the bug please" {
		t.Errorf("expected text unchanged, got %q", remaining)
	}
}

func TestParseMentionCommand_Empty(t *testing.T) {
	cmd, remaining := parseMentionCommand("")
	if cmd != "" {
		t.Errorf("expected empty cmd, got %q", cmd)
	}
	if remaining != "" {
		t.Errorf("expected empty remaining, got %q", remaining)
	}
}

// --- handleMentionKill tests ---

func TestHandleMentionKill_KillsThreadAgent(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	// Bind agent to thread.
	_ = sm.SetThreadAgent("C123", "1111.2222", "thread-1111-2222")

	// Seed agent bead so killAgent can find it.
	daemon.mu.Lock()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:         "bd-thread-agent",
		Title:      "thread-1111-2222",
		Type:       "agent",
		OperatedBy: "oidc:opener@example.com",
		Fields:     map[string]string{"agent": "thread-1111-2222"},
	}
	daemon.mu.Unlock()

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		User:            "U456",
	}

	bot.handleMentionKill(asOpener(), ev, false)

	// Wait for background goroutine to complete the kill.
	deadline := time.After(5 * time.Second)
	for {
		daemon.mu.Lock()
		closed := len(daemon.closed)
		daemon.mu.Unlock()
		if closed > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for agent to be closed")
		case <-time.After(50 * time.Millisecond):
		}
	}

	// Thread mapping should be cleared by killAgent.
	if _, ok := sm.GetThreadAgent("C123", "1111.2222"); ok {
		t.Error("expected thread agent mapping to be removed after kill")
	}
}

func TestHandleMentionKill_NoAgentInThread(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		User:            "U456",
	}

	// Should not panic when no agent is bound.
	bot.handleMentionKill(context.Background(), ev, false)

	// No beads should be closed.
	daemon.mu.Lock()
	closedCount := len(daemon.closed)
	daemon.mu.Unlock()
	if closedCount != 0 {
		t.Errorf("expected 0 close calls, got %d", closedCount)
	}
}

// --- parseMentionCommand clear tests ---

func TestParseMentionCommand_Clear(t *testing.T) {
	cmd, remaining := parseMentionCommand("clear")
	if cmd != "clear" {
		t.Errorf("expected cmd=clear, got %q", cmd)
	}
	if remaining != "" {
		t.Errorf("expected empty remaining, got %q", remaining)
	}
}

func TestParseMentionCommand_ClearCaseInsensitive(t *testing.T) {
	cmd, _ := parseMentionCommand("CLEAR")
	if cmd != "clear" {
		t.Errorf("expected cmd=clear, got %q", cmd)
	}
}

// --- handleMentionClear tests ---

func TestHandleMentionClear_ClearsThreadMapping(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	// Bind agent to thread with listen mode.
	_ = sm.SetThreadAgent("C123", "1111.2222", "thread-1111-2222")
	_ = sm.SetListenThread("C123", "1111.2222")

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		User:            "U456",
	}

	bot.handleMentionClear(context.Background(), ev)

	// Thread mapping should be cleared.
	if _, ok := sm.GetThreadAgent("C123", "1111.2222"); ok {
		t.Error("expected thread agent mapping to be removed after clear")
	}

	// Listen mode should also be cleared.
	if sm.IsListenThread("C123", "1111.2222") {
		t.Error("expected listen thread to be removed after clear")
	}
}

func TestHandleMentionClear_NoAgentInThread(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		User:            "U456",
	}

	// Should not panic when no agent is bound.
	bot.handleMentionClear(context.Background(), ev)
}

func TestHandleMentionClear_AgentNotKilled(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	// Bind agent to thread.
	_ = sm.SetThreadAgent("C123", "1111.2222", "thread-1111-2222")

	// Seed agent bead.
	daemon.mu.Lock()
	daemon.beads["thread-1111-2222"] = &beadsapi.BeadDetail{
		ID:     "bd-thread-agent",
		Title:  "thread-1111-2222",
		Type:   "agent",
		Fields: map[string]string{"agent": "thread-1111-2222"},
	}
	daemon.mu.Unlock()

	ev := &slackevents.AppMentionEvent{
		Channel:         "C123",
		ThreadTimeStamp: "1111.2222",
		User:            "U456",
	}

	bot.handleMentionClear(context.Background(), ev)

	// Thread mapping should be cleared.
	if _, ok := sm.GetThreadAgent("C123", "1111.2222"); ok {
		t.Error("expected thread agent mapping to be removed after clear")
	}

	// Agent bead should NOT be closed — clear only unbinds, doesn't kill.
	daemon.mu.Lock()
	closedCount := len(daemon.closed)
	daemon.mu.Unlock()
	if closedCount != 0 {
		t.Errorf("expected 0 close calls (clear should not kill), got %d", closedCount)
	}
}

func TestSanitizeTS(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"1234567890.123456", "1234567890-123456"},
		{"no.dots.here.ok", "no-dots-here-ok"},
		{"nodots", "nodots"},
	}
	for _, tt := range tests {
		got := sanitizeTS(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeTS(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestProjectFromAgentIdentity(t *testing.T) {
	tests := []struct {
		identity string
		want     string
	}{
		{"gardenio/crew/hq", "gardenio"},
		{"myproj/crew/agent", "myproj"},
		{"simple-agent", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := projectFromAgentIdentity(tt.identity)
		if got != tt.want {
			t.Errorf("projectFromAgentIdentity(%q) = %q, want %q", tt.identity, got, tt.want)
		}
	}
}

func TestParseThreadCommand(t *testing.T) {
	tests := []struct {
		text      string
		wantCmd   string
		wantForce bool
	}{
		{"kill", "kill", false},
		{"stop", "stop", false},
		{"restart", "restart", false},
		{"kill --force", "kill", true},
		{"stop --force", "stop", true},
		{"  Kill  ", "kill", false},
		{"RESTART", "restart", false},
		// Not commands — contain extra words.
		{"kill the deployment", "", false},
		{"restart the agent now", "", false},
		{"check the logs", "", false},
		{"", "", false},
		{"--force", "", false},
	}

	for _, tt := range tests {
		cmd, force := parseThreadCommand(tt.text)
		if cmd != tt.wantCmd || force != tt.wantForce {
			t.Errorf("parseThreadCommand(%q) = (%q, %v), want (%q, %v)",
				tt.text, cmd, force, tt.wantCmd, tt.wantForce)
		}
	}
}

func TestGetThreadAgentsByChannel(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Add thread agents in two different channels.
	_ = state.SetThreadAgent("C-general", "1111.2222", "thread-1111-2222")
	_ = state.SetThreadAgent("C-general", "3333.4444", "thread-3333-4444")
	_ = state.SetThreadAgent("C-other", "5555.6666", "thread-5555-6666")

	agents := state.GetThreadAgentsByChannel("C-general")
	if len(agents) != 2 {
		t.Fatalf("expected 2 agents in C-general, got %d", len(agents))
	}

	agents = state.GetThreadAgentsByChannel("C-other")
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent in C-other, got %d", len(agents))
	}
	if agents[0] != "thread-5555-6666" {
		t.Errorf("expected thread-5555-6666, got %s", agents[0])
	}

	agents = state.GetThreadAgentsByChannel("C-nonexistent")
	if len(agents) != 0 {
		t.Errorf("expected 0 agents in nonexistent channel, got %d", len(agents))
	}
}

func TestHandleMentionThreadCommand_CommandDispatch(t *testing.T) {
	daemon := newMockDaemon()
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:     daemon,
		state:      state,
		logger:     slog.Default(),
		botUserID:  "U-BOT",
		agentCards: map[string]MessageRef{},
	}

	ev := &slackevents.AppMentionEvent{
		Channel:         "C-test",
		ThreadTimeStamp: "1111.2222",
		User:            "U-USER",
		TimeStamp:       "9999.0001",
	}

	tests := []struct {
		text    string
		handled bool
	}{
		{"kill", true},
		{"stop", true},
		{"restart", true},
		{"kill --force", true},
		{"stop --force", true},
		{"check the logs", false},
		{"kill the deployment", false},
		{"restart the server now", false},
		{"", false},
	}

	for _, tt := range tests {
		handled := b.handleMentionThreadCommand(context.Background(), ev, "thread-agent-1", tt.text)
		if handled != tt.handled {
			t.Errorf("handleMentionThreadCommand(%q) = %v, want %v", tt.text, handled, tt.handled)
		}
		// Give goroutines time to finish to avoid panics (kill/restart goroutines
		// will fail on the mock daemon but that's expected).
		time.Sleep(10 * time.Millisecond)
	}
}

// --- Card thread exclusivity tests ---

func TestGetCardAgentByThread(t *testing.T) {
	b := &Bot{
		agentCards: map[string]MessageRef{
			"gardenio/crew/hq": {
				ChannelID: "C-agents",
				Timestamp: "1111.2222",
				Agent:     "gardenio/crew/hq",
			},
			"gardenio/crew/k8s": {
				ChannelID: "C-agents",
				Timestamp: "3333.4444",
				Agent:     "gardenio/crew/k8s",
			},
		},
	}

	t.Run("matching card thread", func(t *testing.T) {
		got := b.getCardAgentByThread("C-agents", "1111.2222")
		if got != "gardenio/crew/hq" {
			t.Errorf("got %q, want %q", got, "gardenio/crew/hq")
		}
	})

	t.Run("different agent card", func(t *testing.T) {
		got := b.getCardAgentByThread("C-agents", "3333.4444")
		if got != "gardenio/crew/k8s" {
			t.Errorf("got %q, want %q", got, "gardenio/crew/k8s")
		}
	})

	t.Run("non-matching channel", func(t *testing.T) {
		got := b.getCardAgentByThread("C-other", "1111.2222")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("non-matching timestamp", func(t *testing.T) {
		got := b.getCardAgentByThread("C-agents", "9999.0000")
		if got != "" {
			t.Errorf("got %q, want empty", got)
		}
	})

	t.Run("matches card posted in-thread via ThreadTS", func(t *testing.T) {
		b.mu.Lock()
		b.agentCards["thread-bound-agent"] = MessageRef{
			ChannelID: "C-thread",
			Timestamp: "5555.7777",
			ThreadTS:  "5555.6666",
			Agent:     "thread-bound-agent",
		}
		b.mu.Unlock()

		got := b.getCardAgentByThread("C-thread", "5555.6666")
		if got != "thread-bound-agent" {
			t.Errorf("got %q, want %q", got, "thread-bound-agent")
		}
	})

	t.Run("does not check thread-spawned agents", func(t *testing.T) {
		dir := t.TempDir()
		state, err := NewStateManager(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		_ = state.SetThreadAgent("C-spawned", "8888.9999", "thread-spawned-agent")
		b2 := &Bot{
			agentCards: map[string]MessageRef{},
			state:      state,
		}
		got := b2.getCardAgentByThread("C-spawned", "8888.9999")
		if got != "" {
			t.Errorf("got %q, want empty (should not find thread-spawned agents)", got)
		}
	})

	t.Run("falls back to persisted state", func(t *testing.T) {
		dir := t.TempDir()
		state, err := NewStateManager(filepath.Join(dir, "state.json"))
		if err != nil {
			t.Fatal(err)
		}
		_ = state.SetAgentCard("gardenio/crew/ops", MessageRef{
			ChannelID: "C-ops",
			Timestamp: "5555.6666",
			Agent:     "gardenio/crew/ops",
		})
		b2 := &Bot{
			agentCards: map[string]MessageRef{},
			state:      state,
		}
		got := b2.getCardAgentByThread("C-ops", "5555.6666")
		if got != "gardenio/crew/ops" {
			t.Errorf("got %q, want %q", got, "gardenio/crew/ops")
		}
	})
}

func TestHandleAppMention_CardThread_RoutesToCardAgent(t *testing.T) {
	// When @mentioning in a card thread, the message must always go to the
	// card agent, even if the user explicitly names a different agent.
	daemon := newMockDaemon()

	// Seed two active agents.
	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}
	daemon.beads["other-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-other",
		Title: "crew-gardenio-crew-other-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "other-agent",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	// Set up agent card for "hq" — this is the card thread parent.
	b.mu.Lock()
	b.agentCards["hq"] = MessageRef{
		ChannelID: "C-agents",
		Timestamp: "1111.2222",
		Agent:     "hq",
	}
	b.mu.Unlock()

	// Mention a DIFFERENT agent name in the card thread.
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> other-agent check the logs",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222", // hq's card thread
		Channel:         "C-agents",
	})

	// Verify the bead was assigned to "hq" (the card agent), NOT "other-agent".
	// Filter by slack-mention label to skip the canonical thread bead created
	// as a side effect of the M2d triggered-by dep write.
	var mentionBeads []*beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "message" && hasLabel(b.Labels, "slack-mention") {
			mentionBeads = append(mentionBeads, b)
		}
	}
	if len(mentionBeads) == 0 {
		t.Fatal("expected a mention tracking bead to be created")
	}
	for _, tb := range mentionBeads {
		if tb.Assignee != "hq" {
			t.Errorf("bead assignee = %q, want %q (card agent should win)", tb.Assignee, "hq")
		}
	}

	// Verify no new agent beads were spawned.
	agentBeads := filterAgentBeads(daemon.beads)
	for _, ab := range agentBeads {
		if ab.ID != "bd-agent-hq" && ab.ID != "bd-agent-other" {
			t.Errorf("unexpected new agent bead created: %s — card thread should not spawn agents", ab.ID)
		}
	}
}

func TestHandleAppMention_CardThread_PlainMention_RoutesToCardAgent(t *testing.T) {
	// When @mentioning in a card thread without naming any agent,
	// the message should route to the card agent.
	daemon := newMockDaemon()

	daemon.beads["hq"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-hq",
		Title: "crew-gardenio-crew-hq",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "hq",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	b.mu.Lock()
	b.agentCards["hq"] = MessageRef{
		ChannelID: "C-agents",
		Timestamp: "1111.2222",
		Agent:     "hq",
	}
	b.mu.Unlock()

	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> check the logs please",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222",
		Channel:         "C-agents",
	})

	// Filter by slack-mention label to skip the canonical thread bead created
	// as a side effect of the M2d triggered-by dep write.
	var mentionBeads []*beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "message" && hasLabel(b.Labels, "slack-mention") {
			mentionBeads = append(mentionBeads, b)
		}
	}
	if len(mentionBeads) == 0 {
		t.Fatal("expected a mention tracking bead to be created")
	}
	for _, tb := range mentionBeads {
		if tb.Assignee != "hq" {
			t.Errorf("bead assignee = %q, want %q", tb.Assignee, "hq")
		}
	}
}

func TestHandleThreadSpawn_BlockedByCardThread(t *testing.T) {
	// handleThreadSpawn must refuse to spawn a new agent in a card thread.
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.botUserID = "U-BOT"

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	b.state = state

	// Set up a card for "hq".
	b.mu.Lock()
	b.agentCards["hq"] = MessageRef{
		ChannelID: "C-agents",
		Timestamp: "1111.2222",
		Agent:     "hq",
	}
	b.mu.Unlock()

	// Attempt to spawn in the card thread.
	b.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> do something",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222",
		Channel:         "C-agents",
	}, "do something")

	// No agent beads should have been created.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads spawned in card thread, got %d", len(agentBeads))
	}
}

func TestHandleAppMention_ThreadMention_UsesParentThreadTS(t *testing.T) {
	// Verifies that when a mention arrives in a thread, the stored chat
	// message ref uses the parent thread TS (ThreadTimeStamp) so the
	// response threads correctly in the same thread, not under the
	// individual message.
	daemon := newMockDaemon()

	// Seed an active agent bead for the thread-bound agent.
	daemon.beads["thread-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-thread",
		Title: "crew-gardenio-crew-thread-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "thread-agent",
			"project": "gardenio",
			"role":    "crew",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Map the thread to the agent.
	_ = state.SetThreadAgent("C-test", "1000.0000", "thread-agent")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.lastThreadNudge = make(map[string]time.Time)

	// Mention @gardenio in a thread — the parent thread TS is 1000.0000,
	// the individual message TS is 2000.5555.
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> check the status",
		TimeStamp:       "2000.5555",
		ThreadTimeStamp: "1000.0000",
		Channel:         "C-test",
	})

	// Find the mention tracking bead created by handleAppMention.
	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) == 0 {
		t.Fatal("expected a mention tracking bead to be created")
	}

	// Check the stored chat message ref uses the parent thread TS.
	for _, tb := range taskBeads {
		ref, ok := state.GetChatMessage(tb.ID)
		if !ok {
			continue
		}
		if ref.Timestamp == "2000.5555" {
			t.Errorf("chat message ref uses individual message TS %q, want parent thread TS %q",
				ref.Timestamp, "1000.0000")
		}
		if ref.Timestamp != "1000.0000" {
			t.Errorf("chat message ref timestamp = %q, want parent thread TS %q",
				ref.Timestamp, "1000.0000")
		}
		if ref.ChannelID != "C-test" {
			t.Errorf("chat message ref channel = %q, want %q", ref.ChannelID, "C-test")
		}
	}
}

func TestHandleAppMention_DM_TopLevel_SpawnsThreadAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	// Top-level mention in a DM channel (D-prefix, no ThreadTimeStamp).
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> --project gardenio help me debug this",
		TimeStamp: "5555.6666",
		Channel:   "D1234567890",
		// ThreadTimeStamp is intentionally empty — top-level DM message.
	})

	// handleThreadSpawn should have been called, creating an agent bead.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM spawn, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["slack_thread_channel"] != "D1234567890" {
			t.Errorf("expected slack_thread_channel=D1234567890, got %q", b.Fields["slack_thread_channel"])
		}
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %q", b.Fields["project"])
		}
	}
}

func TestHandleAppMention_Channel_TopLevel_SpawnsThreadAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	// Top-level mention in a regular channel (C-prefix, no ThreadTimeStamp).
	// This should auto-spawn a thread agent just like a DM mention.
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "7777.8888",
		Channel:   "C-GARDENIO",
		// ThreadTimeStamp is intentionally empty — top-level channel message.
	})

	// handleThreadSpawn should have been called, creating an agent bead.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from channel spawn, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["slack_thread_channel"] != "C-GARDENIO" {
			t.Errorf("expected slack_thread_channel=C-GARDENIO, got %q", b.Fields["slack_thread_channel"])
		}
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %q", b.Fields["project"])
		}
	}
}

// TestHandleAppMention_ModelOverride_StampsModelField locks in the fix for the
// half-wired --model flag: parseModelOverride/normalizeModel existed and were
// unit-tested, but handleThreadSpawn never called them, so `--model=fable` was
// silently dropped and the agent spawned on the account/role default. This
// proves a recognized --model override is resolved to its canonical alias-KEY
// and stamped into fields["model"] on the spawned agent bead (which podspec.go's
// applyAgentModelOverride reads to bake ANTHROPIC_MODEL/CLAUDE_MODEL).
func TestHandleAppMention_ModelOverride_StampsModelField(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantModel string
	}{
		{"equals syntax short alias", "<@U-BOT> --model=fable fix the bug", "claude-fable-5-1"},
		{"space syntax short alias", "<@U-BOT> --model opus fix the bug", "claude-opus-5-5"},
		{"colon syntax short alias", "<@U-BOT> model:sonnet fix the bug", "claude-sonnet-5"},
		{"full id passthrough", "<@U-BOT> --model=claude-opus-4-7 fix the bug", "claude-opus-4-7"},
		{"no override", "<@U-BOT> just fix the bug", ""},
		{"unrecognized falls back to default", "<@U-BOT> --model=llama3 fix the bug", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")

			slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/conversations.replies" {
					_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "messages": []map[string]any{}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
			}))
			defer slackSrv.Close()

			dir := t.TempDir()
			state, err := NewStateManager(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}

			bot := newTestBot(daemon, slackSrv)
			bot.state = state
			bot.botUserID = "U-BOT"

			bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
				User:      "U123",
				Text:      tc.text,
				TimeStamp: "7777.8888",
				Channel:   "C-GARDENIO",
			})

			agentBeads := filterAgentBeads(daemon.beads)
			if len(agentBeads) != 1 {
				t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
			}
			for _, b := range agentBeads {
				if got := b.Fields["model"]; got != tc.wantModel {
					t.Errorf("agent bead model field = %q, want %q", got, tc.wantModel)
				}
			}
		})
	}
}

// TestHandleAppMention_Channel_TopLevel_StampsThreadIDOnAgent locks in the fix
// for ka-0000007241: thread-agent spawns must stamp fields.thread_id (not just
// the legacy slack_thread_bead_id alias) on the agent bead. The /threads INDEX
// counts members via a single field-equality query on thread_id, so missing it
// here made every thread card render "no work yet" even when agents had run.
func TestHandleAppMention_Channel_TopLevel_StampsThreadIDOnAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	const (
		channel   = "C-GARDENIO"
		mentionTS = "8888.9999"
	)
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> stamp thread_id please",
		TimeStamp: mentionTS,
		Channel:   channel,
	})

	// Locate the agent bead and the canonical thread head.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from channel spawn, got %d", len(agentBeads))
	}
	var headID string
	threadLabel := threadBeadLabel(channel, mentionTS)
	for _, bead := range daemon.beads {
		if hasLabel(bead.Labels, threadLabel) {
			headID = bead.ID
			break
		}
	}
	if headID == "" {
		t.Fatal("expected canonical thread head bead to be created")
	}

	for _, b := range agentBeads {
		if got := b.Fields["thread_id"]; got != headID {
			t.Errorf("agent bead missing thread_id wish-pointer: got %q, want %q (head id) — /threads INDEX counts members via fields.thread_id only", got, headID)
		}
		if got := b.Fields["slack_thread_bead_id"]; got != headID {
			t.Errorf("agent bead missing slack_thread_bead_id alias: got %q, want %q", got, headID)
		}
	}
}

func TestHandleAppMention_DM_TopLevel_AutoSelectsSoleProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	// DM mention WITHOUT --project flag — should auto-select sole project.
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "5555.6666",
		Channel:   "D1234567890",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM auto-select, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (auto-selected), got %q", b.Fields["project"])
		}
	}
}

func TestHandleAppMention_DM_TopLevel_MultipleProjects_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	// DM mention with multiple projects and no --project → rejected.
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "5555.6666",
		Channel:   "D1234567890",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for DM with multiple projects, got %d", len(agentBeads))
	}
}

func TestHandleAppMention_DM_TopLevel_MultipleProjects_DefaultDMProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.defaultDMProject = "gardenio"

	// DM mention with multiple projects but defaultDMProject set → uses default.
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "7777.8888",
		Channel:   "D1234567890",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM with default project, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (default DM project), got %q", b.Fields["project"])
		}
	}
}

func TestHandleAppMention_DM_TopLevel_MultipleProjects_InvalidDefault(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.defaultDMProject = "nonexistent"

	// DM mention with invalid defaultDMProject → rejected (falls through).
	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "9999.0000",
		Channel:   "D1234567890",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for DM with invalid default project, got %d", len(agentBeads))
	}
}

func TestHandleAppMention_DM_TopLevel_MultipleProjects_DMDefaultBeadField(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithDMDefault("gardenio") // dm_default=true
	daemon.seedProject("monorepo")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"
	// No defaultDMProject env var set — should fall through to bead field.

	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "1111.2222",
		Channel:   "D1234567890",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM with dm_default bead field, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (dm_default bead field), got %q", b.Fields["project"])
		}
	}
}

func TestHandleAppMention_DM_TopLevel_EnvOverridesBeadField(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithDMDefault("gardenio") // dm_default=true on gardenio
	daemon.seedProject("monorepo")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.defaultDMProject = "monorepo" // Env var overrides bead field.

	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "3333.4444",
		Channel:   "D1234567890",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo (env var overrides bead field), got %q", b.Fields["project"])
		}
	}
}

func TestHandleAppMention_DM_TopLevel_UserHistoryFallback(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	// Seed a prior agent bead for user U123 with project=monorepo.
	daemon.mu.Lock()
	daemon.beads["agent-prior"] = &beadsapi.BeadDetail{
		ID:     "agent-prior",
		Title:  "prior-agent",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":         "prior-agent",
			"project":       "monorepo",
			"role":          "crew",
			"slack_user_id": "U123",
		},
	}
	daemon.mu.Unlock()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"
	// No defaultDMProject, no dm_default — falls through to user history.

	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U123",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "5555.6666",
		Channel:   "D1234567890",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	// Should have 2 agent beads: the prior one + the new one.
	var newBeads []*beadsapi.BeadDetail
	for _, b := range agentBeads {
		if b.ID != "agent-prior" {
			newBeads = append(newBeads, b)
		}
	}
	if len(newBeads) != 1 {
		t.Fatalf("expected 1 new agent bead from DM with user history, got %d", len(newBeads))
	}
	if newBeads[0].Fields["project"] != "monorepo" {
		t.Errorf("expected project=monorepo (from user history), got %q", newBeads[0].Fields["project"])
	}
}

// --- Mention permission gate tests ---

func TestCheckMentionPermission_AllowedUser(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a user bead with mention capability.
	daemon.mu.Lock()
	daemon.beads["user-alice"] = &beadsapi.BeadDetail{
		ID:     "user-alice",
		Title:  "Alice",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-ALICE",
			"display_name": "Alice",
			"capabilities": `["mention"]`,
		},
	}
	daemon.mu.Unlock()

	b := &Bot{
		daemon:             daemon,
		logger:             slog.Default(),
		mentionGateEnabled: true,
	}

	allowed := b.checkMentionPermission(context.Background(), "U-ALICE", "C-test", "", "1111.0000")
	if !allowed {
		t.Error("expected mention to be allowed for user with mention capability")
	}
}

func TestCheckMentionPermission_DeniedUnknownUser(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	allowed := b.checkMentionPermission(context.Background(), "U-UNKNOWN", "C-test", "", "2222.0000")
	if allowed {
		t.Error("expected mention to be denied for unknown user")
	}
}

func TestCheckMentionPermission_DeniedNoCap(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a user bead WITHOUT mention capability.
	daemon.mu.Lock()
	daemon.beads["user-bob"] = &beadsapi.BeadDetail{
		ID:     "user-bob",
		Title:  "Bob",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-BOB",
			"display_name": "Bob",
			"capabilities": `["view"]`,
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	allowed := b.checkMentionPermission(context.Background(), "U-BOB", "C-test", "", "3333.0000")
	if allowed {
		t.Error("expected mention to be denied for user without mention capability")
	}
}

func TestCheckMentionPermission_WildcardCapability(t *testing.T) {
	daemon := newMockDaemon()
	// User with wildcard mention:* capability.
	daemon.mu.Lock()
	daemon.beads["user-admin"] = &beadsapi.BeadDetail{
		ID:     "user-admin",
		Title:  "Admin",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-ADMIN",
			"display_name": "Admin",
			"capabilities": `["mention:*"]`,
		},
	}
	daemon.mu.Unlock()

	b := &Bot{
		daemon:             daemon,
		logger:             slog.Default(),
		mentionGateEnabled: true,
	}

	// "mention:*" should NOT match plain "mention" — wildcards match subtypes.
	// The HasCapability check is: does "mention:*" match "mention"?
	// "mention:*" pattern: prefix="mention:", target="mention" — doesn't start with "mention:".
	// So this should be denied.
	allowed := b.checkMentionPermission(context.Background(), "U-ADMIN", "C-test", "", "4444.0000")
	if allowed {
		t.Error("expected mention:* to not match plain 'mention' capability")
	}
}

func TestHandleAppMention_GateEnabled_DeniesUnknownUser(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.mentionGateEnabled = true

	// Mention from unknown user.
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U-UNKNOWN",
		Text:      "<@U-BOT> help me",
		TimeStamp: "1111.2222",
		Channel:   "C-test",
	})

	// No beads should be created — mention was denied.
	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) != 0 {
		t.Errorf("expected 0 task beads when gate denies, got %d", len(taskBeads))
	}
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected 0 agent beads when gate denies, got %d", len(agentBeads))
	}
}

func TestHandleAppMention_GateEnabled_AllowsAuthorizedUser(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")

	// Seed authorized user.
	daemon.mu.Lock()
	daemon.beads["user-alice"] = &beadsapi.BeadDetail{
		ID:     "user-alice",
		Title:  "Alice",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-ALICE",
			"display_name": "Alice",
			"capabilities": `["mention"]`,
		},
	}
	daemon.mu.Unlock()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.mentionGateEnabled = true

	// Mention from authorized user in a channel mapped to a project.
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U-ALICE",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "3333.4444",
		Channel:   "C-GARDENIO",
	})

	// An agent bead should be created (spawn succeeded).
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) == 0 {
		t.Error("expected agent bead to be created for authorized user")
	}
}

func TestHandleAppMention_GateDisabled_AllowsEveryone(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.mentionGateEnabled = false // gate disabled

	// Mention from unknown user with gate disabled — should pass.
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U-UNKNOWN",
		Text:      "<@U-BOT> help me debug this",
		TimeStamp: "5555.6666",
		Channel:   "C-GARDENIO",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) == 0 {
		t.Error("expected agent bead to be created when gate is disabled")
	}
}

func TestHandleAppMention_DM_TopLevel_NoHistory_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")
	// No prior agent beads, no defaults → should reject.

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	bot.botUserID = "U-BOT"

	bot.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:      "U999",
		Text:      "<@U-BOT> help me",
		TimeStamp: "6666.7777",
		Channel:   "D9999999999",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected 0 agent beads (DM rejected with no history), got %d", len(agentBeads))
	}
}

// --- Mention audit logging tests ---

// captureLogger returns a logger that writes JSON to the given buffer.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func TestMentionAuditLog_AllowedUser(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-alice"] = &beadsapi.BeadDetail{
		ID:     "user-alice",
		Title:  "Alice",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-ALICE",
			"display_name": "Alice",
			"team":         "platform",
			"capabilities": `["mention"]`,
		},
	}
	daemon.mu.Unlock()

	var buf bytes.Buffer
	b := &Bot{
		daemon:             daemon,
		logger:             captureLogger(&buf),
		mentionGateEnabled: true,
	}

	allowed := b.checkMentionPermission(context.Background(), "U-ALICE", "C-test", "", "1111.0000")
	if !allowed {
		t.Fatal("expected allowed")
	}

	log := buf.String()
	if !strings.Contains(log, "mention_audit: allowed") {
		t.Errorf("expected audit log with 'mention_audit: allowed', got: %s", log)
	}
	if !strings.Contains(log, `"outcome":"allowed"`) {
		t.Errorf("expected outcome=allowed in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"user_bead":"user-alice"`) {
		t.Errorf("expected user_bead in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"display_name":"Alice"`) {
		t.Errorf("expected display_name in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"team":"platform"`) {
		t.Errorf("expected team in audit log, got: %s", log)
	}
}

func TestMentionAuditLog_DeniedUnknownUser(t *testing.T) {
	daemon := newMockDaemon()

	var buf bytes.Buffer
	b := &Bot{
		daemon:             daemon,
		logger:             captureLogger(&buf),
		mentionGateEnabled: true,
	}

	allowed := b.checkMentionPermission(context.Background(), "U-UNKNOWN", "C-test", "", "2222.0000")
	if allowed {
		t.Fatal("expected denied")
	}

	log := buf.String()
	if !strings.Contains(log, "mention_audit: denied") {
		t.Errorf("expected audit log with 'mention_audit: denied', got: %s", log)
	}
	if !strings.Contains(log, `"outcome":"denied"`) {
		t.Errorf("expected outcome=denied in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"reason":"unknown_user"`) {
		t.Errorf("expected reason=unknown_user in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"slack_user_id":"U-UNKNOWN"`) {
		t.Errorf("expected slack_user_id in audit log, got: %s", log)
	}
	// Unknown user should NOT have user_bead or display_name.
	if strings.Contains(log, `"user_bead"`) {
		t.Errorf("expected no user_bead for unknown user, got: %s", log)
	}
}

func TestMentionAuditLog_DeniedMissingCapability(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-bob"] = &beadsapi.BeadDetail{
		ID:     "user-bob",
		Title:  "Bob",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-BOB",
			"display_name": "Bob",
			"team":         "infra",
			"capabilities": `["view"]`,
		},
	}
	daemon.mu.Unlock()

	var buf bytes.Buffer
	b := &Bot{
		daemon:             daemon,
		logger:             captureLogger(&buf),
		mentionGateEnabled: true,
	}

	allowed := b.checkMentionPermission(context.Background(), "U-BOB", "C-test", "", "3333.0000")
	if allowed {
		t.Fatal("expected denied")
	}

	log := buf.String()
	if !strings.Contains(log, "mention_audit: denied") {
		t.Errorf("expected audit log with 'mention_audit: denied', got: %s", log)
	}
	if !strings.Contains(log, `"reason":"missing_capability"`) {
		t.Errorf("expected reason=missing_capability in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"user_bead":"user-bob"`) {
		t.Errorf("expected user_bead in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"display_name":"Bob"`) {
		t.Errorf("expected display_name in audit log, got: %s", log)
	}
	if !strings.Contains(log, `"team":"infra"`) {
		t.Errorf("expected team in audit log, got: %s", log)
	}
}

// --- Denial UX tests: Block Kit messages and reactions ---

// slackAPICalls records the API paths and form bodies from Slack API calls.
type slackAPICalls struct {
	mu    sync.Mutex // guards calls against a handler goroutine still posting; see waitForBody
	calls []slackAPICall
}

type slackAPICall struct {
	Path string
	Body string
}

func newCapturingSlackServer(t *testing.T, captured *slackAPICalls) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		captured.mu.Lock()
		captured.calls = append(captured.calls, slackAPICall{
			Path: r.URL.Path,
			Body: r.Form.Encode(),
		})
		captured.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
}

func (c *slackAPICalls) hasPath(path string) bool {
	for _, call := range c.calls {
		if call.Path == path {
			return true
		}
	}
	return false
}

// waitForBody polls until a captured call's form body contains substr, so a
// test can join a handler's background goroutine by the last post it makes.
func (c *slackAPICalls) waitForBody(t *testing.T, substr string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, call := range c.calls {
			if strings.Contains(call.Body, substr) {
				c.mu.Unlock()
				return
			}
		}
		c.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no Slack call carried %q", substr)
}

func (c *slackAPICalls) bodyForPath(path string) string {
	for _, call := range c.calls {
		if call.Path == path {
			return call.Body
		}
	}
	return ""
}

func TestDenialUX_UnknownUser_PostsBlockKit(t *testing.T) {
	daemon := newMockDaemon()
	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	b.checkMentionPermission(context.Background(), "U-NOBODY", "C-test", "", "9999.0001")

	// Should have called PostEphemeral (chat.postEphemeral).
	if !captured.hasPath("/chat.postEphemeral") {
		t.Error("expected PostEphemeral call for unknown user denial")
	}
	body := captured.bodyForPath("/chat.postEphemeral")
	if !strings.Contains(body, "Access+Required") && !strings.Contains(body, "Access Required") {
		t.Errorf("expected 'Access Required' in ephemeral blocks, got: %s", body)
	}
	if !strings.Contains(body, "not+registered") && !strings.Contains(body, "not registered") {
		t.Errorf("expected 'not registered' in ephemeral body, got: %s", body)
	}
}

func TestDenialUX_NoCap_PostsBlockKit(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-eve"] = &beadsapi.BeadDetail{
		ID:     "user-eve",
		Title:  "Eve",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-EVE",
			"display_name": "Eve",
			"capabilities": `["view"]`,
		},
	}
	daemon.mu.Unlock()

	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	b.checkMentionPermission(context.Background(), "U-EVE", "C-test", "", "9999.0002")

	if !captured.hasPath("/chat.postEphemeral") {
		t.Error("expected PostEphemeral call for missing capability denial")
	}
	body := captured.bodyForPath("/chat.postEphemeral")
	if !strings.Contains(body, "Permission+Denied") && !strings.Contains(body, "Permission Denied") {
		t.Errorf("expected 'Permission Denied' in ephemeral blocks, got: %s", body)
	}
	if !strings.Contains(body, "Eve") {
		t.Errorf("expected user display name 'Eve' in ephemeral body, got: %s", body)
	}
}

func TestDenialUX_AddsReaction(t *testing.T) {
	daemon := newMockDaemon()
	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	b.checkMentionPermission(context.Background(), "U-NOBODY", "C-test", "", "9999.0003")

	if !captured.hasPath("/reactions.add") {
		t.Error("expected reactions.add call for denied mention")
	}
	body := captured.bodyForPath("/reactions.add")
	// No theme configured → Garden's "refused" reaction.
	if !strings.Contains(body, "fallen_leaf") {
		t.Errorf("expected fallen_leaf reaction, got: %s", body)
	}
}

// projectThemeDaemon serves a fixed project map so themeFor resolves a
// channel's card theme.
type projectThemeDaemon struct {
	*mockDaemon
	projects map[string]beadsapi.ProjectInfo
}

func (d *projectThemeDaemon) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return d.projects, nil
}

func TestDenialUX_ReactionFollowsChannelTheme(t *testing.T) {
	for channel, want := range map[string]string{"CBIZ": "no_entry_sign", "COTHER": "fallen_leaf"} {
		daemon := &projectThemeDaemon{
			mockDaemon: newMockDaemon(),
			projects: map[string]beadsapi.ProjectInfo{
				"proj": {Name: "proj", SlackChannels: []string{"CBIZ"}, CardTheme: "business"},
			},
		}
		var captured slackAPICalls
		slackSrv := newCapturingSlackServer(t, &captured)
		b := newTestBot(daemon, slackSrv)
		b.reactDenied(channel, "9999.0004")
		slackSrv.Close()

		body := captured.bodyForPath("/reactions.add")
		if !strings.Contains(body, want) {
			t.Errorf("%s: expected %s reaction, got: %s", channel, want, body)
		}
	}
}

func TestDenialUX_NoReactionWhenNoMessageTS(t *testing.T) {
	daemon := newMockDaemon()
	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	// Empty messageTS — should skip reaction.
	b.checkMentionPermission(context.Background(), "U-NOBODY", "C-test", "", "")

	if captured.hasPath("/reactions.add") {
		t.Error("expected no reactions.add call when messageTS is empty")
	}
}

func TestCheckMentionPermission_MultipleCapabilities(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-multi"] = &beadsapi.BeadDetail{
		ID:     "user-multi",
		Title:  "Multi",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-MULTI",
			"display_name": "Multi",
			"capabilities": `["view","mention","slash_command"]`,
		},
	}
	daemon.mu.Unlock()

	b := &Bot{
		daemon:             daemon,
		logger:             slog.Default(),
		mentionGateEnabled: true,
	}

	allowed := b.checkMentionPermission(context.Background(), "U-MULTI", "C-test", "", "5555.0000")
	if !allowed {
		t.Error("expected mention to be allowed for user with mention among multiple capabilities")
	}
}

func TestDenialUX_ThreadedMention_EphemeralInThread(t *testing.T) {
	daemon := newMockDaemon()
	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	// Deny with a non-empty threadTS — ephemeral should be posted into the thread.
	b.checkMentionPermission(context.Background(), "U-NOBODY", "C-test", "1000.0001", "9999.0004")

	if !captured.hasPath("/chat.postEphemeral") {
		t.Fatal("expected PostEphemeral call for threaded denial")
	}
	body := captured.bodyForPath("/chat.postEphemeral")
	// When threadTS is provided, the Slack API call should include thread_ts.
	if !strings.Contains(body, "thread_ts=1000.0001") {
		t.Errorf("expected thread_ts=1000.0001 in ephemeral body, got: %s", body)
	}
}

func TestPostMentionDeniedNoCap_EmptyDisplayName(t *testing.T) {
	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(newMockDaemon(), slackSrv)

	// Call the denial helper directly with empty displayName.
	b.postMentionDeniedNoCap("C-test", "U-NONAME", "", "")

	if !captured.hasPath("/chat.postEphemeral") {
		t.Fatal("expected PostEphemeral call for no-cap denial")
	}
	body := captured.bodyForPath("/chat.postEphemeral")
	// With empty displayName, the message should use "your" not "Name's".
	if !strings.Contains(body, "your+profile") && !strings.Contains(body, "your profile") {
		t.Errorf("expected 'your profile' when displayName is empty, got: %s", body)
	}
}

func TestPostMentionDeniedNoCap_WithDisplayName(t *testing.T) {
	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(newMockDaemon(), slackSrv)

	// Call the denial helper with a displayName.
	b.postMentionDeniedNoCap("C-test", "U-ALICE", "", "Alice")

	if !captured.hasPath("/chat.postEphemeral") {
		t.Fatal("expected PostEphemeral call for no-cap denial")
	}
	body := captured.bodyForPath("/chat.postEphemeral")
	if !strings.Contains(body, "Alice") {
		t.Errorf("expected display name 'Alice' in denial message, got: %s", body)
	}
}

func TestHandleAppMention_GateEnabled_InThread_DeniesUnknownUser(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = "U-BOT"
	b.mentionGateEnabled = true

	// In-thread mention from unknown user — should be denied before routing.
	b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-UNKNOWN",
		Text:            "<@U-BOT> help me",
		TimeStamp:       "7777.8888",
		ThreadTimeStamp: "6666.7777",
		Channel:         "C-GARDENIO",
	})

	// No beads should be created.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected 0 agent beads when gate denies in-thread mention, got %d", len(agentBeads))
	}
}

func TestNudgeAgentForMention_NoRetryOnOtherErrors(t *testing.T) {
	daemon := newMockDaemon()

	// Seed an agent bead in a terminal state — should fail immediately, no retry.
	daemon.mu.Lock()
	daemon.beads["dead-agent"] = &beadsapi.BeadDetail{
		ID:     "agent-bead-dead",
		Title:  "dead-agent",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{"agent_state": "done"},
		Notes:  "backend: coop\ncoop_url: http://127.0.0.1:9999",
	}
	daemon.mu.Unlock()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	b := &Bot{
		daemon: daemon,
		logger: logger,
	}

	start := time.Now()
	err := b.nudgeAgentForMention(context.Background(), "dead-agent", "hello", "gd-test3")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error for dead agent")
	}
	if !strings.Contains(err.Error(), "not nudgeable") {
		t.Errorf("expected 'not nudgeable' in error, got: %v", err)
	}
	// Should fail fast — no retries for non-coop_url errors.
	if elapsed > 100*time.Millisecond {
		t.Errorf("expected fast failure, took %v", elapsed)
	}
}

// --- Email fallback in mention gate ---

// TestCheckMentionPermission_ProfileEmailDoesNotGrant is the regression for
// gd-W4FkF80BlX: a Slack user whose profile email equals a user bead's email
// (the bead has no slack_id) must not pass the gate with that bead's
// capabilities, is told to link, and is still unknown afterwards, so
// resolveSlackPerson cannot act as the bead's person either.
func TestCheckMentionPermission_ProfileEmailDoesNotGrant(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a user bead with email but NO slack_id (Keycloak-provisioned).
	daemon.mu.Lock()
	daemon.beads["user-keycloak-user"] = &beadsapi.BeadDetail{
		ID:     "user-keycloak-user",
		Title:  "Keycloak User",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"email":        "kc-user@example.com",
			"display_name": "Keycloak User",
			"capabilities": `["mention"]`,
		},
	}
	daemon.mu.Unlock()

	var captured slackAPICalls
	slackSrv := newCapturingSlackServer(t, &captured)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	dir := b.UserDirectory()
	dir.SetSlackEmailResolver(&mockSlackEmailResolver{
		emails: map[string]string{"U-SPOOF": "kc-user@example.com"},
	})

	for i := 0; i < 2; i++ {
		if b.checkMentionPermission(context.Background(), "U-SPOOF", "C-test", "", "1111.0000") {
			t.Fatalf("attempt %d: mention allowed on a matching Slack profile email alone", i+1)
		}
	}
	if u := dir.LookupBySlackID(context.Background(), "U-SPOOF"); u != nil {
		t.Errorf("Slack id was mapped to %s by its profile email", u.BeadID)
	}
	body := captured.bodyForPath("/chat.postEphemeral")
	if !strings.Contains(body, "Link+Your+Account") && !strings.Contains(body, "Link Your Account") {
		t.Errorf("expected the link-your-account denial, got: %s", body)
	}
	if strings.Contains(body, "Keycloak") {
		t.Errorf("denial names the matched person: %s", body)
	}
}

func TestCheckMentionPermission_EmailFallback_StillDeniedWhenNoMatch(t *testing.T) {
	daemon := newMockDaemon()
	// No user beads at all.

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true

	dir := b.UserDirectory()
	dir.SetSlackEmailResolver(&mockSlackEmailResolver{
		emails: map[string]string{"U-STRANGER": "stranger@example.com"},
	})

	// Slack returns an email but no user bead matches — still denied.
	allowed := b.checkMentionPermission(context.Background(), "U-STRANGER", "C-test", "", "3333.0000")
	if allowed {
		t.Error("expected mention to be denied when email fallback finds no user bead")
	}
}

// --- Keycloak role check in mention gate ---

func TestCheckMentionPermission_KeycloakRole_Allowed(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-kc-alice"] = &beadsapi.BeadDetail{
		ID:     "user-kc-alice",
		Title:  "KC Alice",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-KC-ALICE",
			"email":        "alice@example.com",
			"display_name": "KC Alice",
			// No "capabilities" — authorization comes from Keycloak.
		},
	}
	daemon.mu.Unlock()

	fk := newFakeKeycloakServer()
	defer fk.Close()
	fk.users["alice@example.com"] = fakeKCUser{ID: "kc-alice", Email: "alice@example.com"}
	fk.roles["kc-alice"] = []string{"gardenio-mention", "gardenio-viewer"}

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL: fk.URL(), Realm: "gardenio",
		ClientID: "test", ClientSecret: "secret",
	}, slog.Default())

	b := &Bot{
		daemon:              daemon,
		logger:              slog.Default(),
		mentionGateEnabled:  true,
		keycloakChecker:     checker,
		keycloakMentionRole: "gardenio-mention",
	}

	allowed := b.checkMentionPermission(context.Background(), "U-KC-ALICE", "C-test", "", "1111.0000")
	if !allowed {
		t.Error("expected mention to be allowed via Keycloak role check")
	}
}

func TestCheckMentionPermission_KeycloakRole_DeniedMissingRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-kc-bob"] = &beadsapi.BeadDetail{
		ID:     "user-kc-bob",
		Title:  "KC Bob",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-KC-BOB",
			"email":        "bob@example.com",
			"display_name": "KC Bob",
		},
	}
	daemon.mu.Unlock()

	fk := newFakeKeycloakServer()
	defer fk.Close()
	fk.users["bob@example.com"] = fakeKCUser{ID: "kc-bob", Email: "bob@example.com"}
	fk.roles["kc-bob"] = []string{"gardenio-viewer"} // No gardenio-mention role.

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL: fk.URL(), Realm: "gardenio",
		ClientID: "test", ClientSecret: "secret",
	}, slog.Default())

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true
	b.keycloakChecker = checker
	b.keycloakMentionRole = "gardenio-mention"

	allowed := b.checkMentionPermission(context.Background(), "U-KC-BOB", "C-test", "", "2222.0000")
	if allowed {
		t.Error("expected mention to be denied — user lacks Keycloak gardenio-mention role")
	}
}

func TestCheckMentionPermission_KeycloakRole_DeniedNoEmail(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-no-email"] = &beadsapi.BeadDetail{
		ID:     "user-no-email",
		Title:  "No Email",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-NO-EMAIL",
			"display_name": "No Email",
			// No email field.
		},
	}
	daemon.mu.Unlock()

	fk := newFakeKeycloakServer()
	defer fk.Close()

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL: fk.URL(), Realm: "gardenio",
		ClientID: "test", ClientSecret: "secret",
	}, slog.Default())

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.mentionGateEnabled = true
	b.keycloakChecker = checker
	b.keycloakMentionRole = "gardenio-mention"

	allowed := b.checkMentionPermission(context.Background(), "U-NO-EMAIL", "C-test", "", "3333.0000")
	if allowed {
		t.Error("expected mention to be denied — user has no email for Keycloak lookup")
	}
}

func TestCheckMentionPermission_KeycloakError_FallsBackToCapability(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-fallback"] = &beadsapi.BeadDetail{
		ID:     "user-fallback",
		Title:  "Fallback User",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-FALLBACK",
			"email":        "fallback@example.com",
			"display_name": "Fallback User",
			"capabilities": `["mention"]`,
		},
	}
	daemon.mu.Unlock()

	// Keycloak server that always returns 500.
	errSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer errSrv.Close()

	checker := NewKeycloakRoleChecker(KeycloakConfig{
		BaseURL: errSrv.URL, Realm: "gardenio",
		ClientID: "test", ClientSecret: "secret",
	}, slog.Default())

	b := &Bot{
		daemon:              daemon,
		logger:              slog.Default(),
		mentionGateEnabled:  true,
		keycloakChecker:     checker,
		keycloakMentionRole: "gardenio-mention",
	}

	// Keycloak fails → falls back to capability check → user has "mention" → allowed.
	allowed := b.checkMentionPermission(context.Background(), "U-FALLBACK", "C-test", "", "4444.0000")
	if !allowed {
		t.Error("expected mention to be allowed via capability fallback when Keycloak fails")
	}
}

func TestCheckMentionPermission_NoKeycloak_UsesCapability(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["user-cap"] = &beadsapi.BeadDetail{
		ID:     "user-cap",
		Title:  "Cap User",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"slack_id":     "U-CAP",
			"display_name": "Cap User",
			"capabilities": `["mention"]`,
		},
	}
	daemon.mu.Unlock()

	// No Keycloak checker configured.
	b := &Bot{
		daemon:             daemon,
		logger:             slog.Default(),
		mentionGateEnabled: true,
	}

	allowed := b.checkMentionPermission(context.Background(), "U-CAP", "C-test", "", "5555.0000")
	if !allowed {
		t.Error("expected mention to be allowed via capability check (no Keycloak)")
	}
}

// errBeadClient wraps mockDaemon to make GetBead return an error when the
// requested ID is not seeded — simulating the "bead does not exist" branch
// of tryHandleEpicGardenerStart.
type errBeadClient struct {
	*mockDaemon
}

func (e *errBeadClient) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if b, ok := e.beads[beadID]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("bead not found: %s", beadID)
}

func TestTryHandleEpicGardenerStart(t *testing.T) {
	type args struct {
		text string
	}

	// Common stub: capture invocations of `gb gardener start`.
	type runCall struct {
		bead       string
		operatedBy string
	}

	// Seed a daemon with both an epic and a non-epic bead.
	makeDaemon := func() *mockDaemon {
		d := newMockDaemon()
		d.mu.Lock()
		d.beads["gd-EPIC0001"] = &beadsapi.BeadDetail{
			ID:     "gd-EPIC0001",
			Type:   "epic",
			Title:  "Test epic",
			Status: "open",
		}
		d.beads["gd-TASK0001"] = &beadsapi.BeadDetail{
			ID:     "gd-TASK0001",
			Type:   "task",
			Title:  "Test task",
			Status: "open",
		}
		d.mu.Unlock()
		return d
	}

	t.Run("matched epic shells out to gb gardener start", func(t *testing.T) {
		var calls []runCall
		orig := gardenerStartRunner
		gardenerStartRunner = func(_ context.Context, beadID, operatedBy string) ([]byte, error) {
			calls = append(calls, runCall{bead: beadID, operatedBy: operatedBy})
			return []byte("created schedule sched-1\n"), nil
		}
		defer func() { gardenerStartRunner = orig }()

		b := &Bot{
			daemon: makeDaemon(),
			logger: slog.Default(),
		}
		ev := &slackevents.AppMentionEvent{
			Channel:   "C-ops",
			User:      "U1",
			TimeStamp: "1700000000.000100",
		}

		got := b.tryHandleEpicGardenerStart(context.Background(), ev, "gd-EPIC0001 spin up the gardener")
		if !got {
			t.Fatalf("tryHandleEpicGardenerStart returned false, want true")
		}
		if len(calls) != 1 || calls[0].bead != "gd-EPIC0001" {
			t.Errorf("expected one gardener-start call for gd-EPIC0001, got %+v", calls)
		}
		// The triggering Slack user (ev.User="U1") must be forwarded as the
		// operator so the gardener trigger bead is owner-attributed to the
		// human, not the bridge bot identity (ka-0000003284).
		if got := calls[0].operatedBy; got != "slack:U1" {
			t.Errorf("gardener-start operatedBy = %q, want slack:U1", got)
		}
	})

	t.Run("idempotent: already exists exit treated as handled", func(t *testing.T) {
		var calls []runCall
		orig := gardenerStartRunner
		gardenerStartRunner = func(_ context.Context, beadID, operatedBy string) ([]byte, error) {
			calls = append(calls, runCall{bead: beadID, operatedBy: operatedBy})
			return []byte("gardener schedule already exists for gd-EPIC0001: sched-7\n"),
				fmt.Errorf("exit status 1")
		}
		defer func() { gardenerStartRunner = orig }()

		b := &Bot{
			daemon: makeDaemon(),
			logger: slog.Default(),
		}
		ev := &slackevents.AppMentionEvent{Channel: "C-ops", TimeStamp: "1700000000.000200"}

		got := b.tryHandleEpicGardenerStart(context.Background(), ev, "gd-EPIC0001")
		if !got {
			t.Fatalf("expected handled=true on already-exists, got false")
		}
		if len(calls) != 1 {
			t.Errorf("expected exactly one gardener-start call, got %d", len(calls))
		}
	})

	t.Run("non-bead leading token falls through", func(t *testing.T) {
		var calls []runCall
		orig := gardenerStartRunner
		gardenerStartRunner = func(_ context.Context, beadID, operatedBy string) ([]byte, error) {
			calls = append(calls, runCall{bead: beadID, operatedBy: operatedBy})
			return nil, nil
		}
		defer func() { gardenerStartRunner = orig }()

		b := &Bot{
			daemon: makeDaemon(),
			logger: slog.Default(),
		}
		ev := &slackevents.AppMentionEvent{Channel: "C-ops", TimeStamp: "1700000000.000300"}

		cases := []args{
			{"hello world"},
			{""},
			{"please review gd-EPIC0001"}, // bead ID is not the leading token
			{"epic gd-EPIC0001"},
		}
		for _, tc := range cases {
			t.Run(fmt.Sprintf("text=%q", tc.text), func(t *testing.T) {
				if got := b.tryHandleEpicGardenerStart(context.Background(), ev, tc.text); got {
					t.Errorf("tryHandleEpicGardenerStart(%q) = true, want false (fall through)", tc.text)
				}
			})
		}
		if len(calls) != 0 {
			t.Errorf("expected no gardener-start calls for non-bead leads, got %+v", calls)
		}
	})

	t.Run("non-epic bead falls through", func(t *testing.T) {
		var calls []runCall
		orig := gardenerStartRunner
		gardenerStartRunner = func(_ context.Context, beadID, operatedBy string) ([]byte, error) {
			calls = append(calls, runCall{bead: beadID, operatedBy: operatedBy})
			return nil, nil
		}
		defer func() { gardenerStartRunner = orig }()

		b := &Bot{
			daemon: makeDaemon(),
			logger: slog.Default(),
		}
		ev := &slackevents.AppMentionEvent{Channel: "C-ops", TimeStamp: "1700000000.000400"}

		got := b.tryHandleEpicGardenerStart(context.Background(), ev, "gd-TASK0001 do something")
		if got {
			t.Fatalf("tryHandleEpicGardenerStart returned true for non-epic bead, want false")
		}
		if len(calls) != 0 {
			t.Errorf("expected no gardener-start invocations for non-epic, got %+v", calls)
		}
	})

	t.Run("missing bead falls through", func(t *testing.T) {
		var calls []runCall
		orig := gardenerStartRunner
		gardenerStartRunner = func(_ context.Context, beadID, operatedBy string) ([]byte, error) {
			calls = append(calls, runCall{bead: beadID, operatedBy: operatedBy})
			return nil, nil
		}
		defer func() { gardenerStartRunner = orig }()

		// errBeadClient returns an error for unseeded IDs, which is the realistic
		// behavior of the daemon when a typo'd bead ID is passed in.
		b := &Bot{
			daemon: &errBeadClient{mockDaemon: newMockDaemon()},
			logger: slog.Default(),
		}
		ev := &slackevents.AppMentionEvent{Channel: "C-ops", TimeStamp: "1700000000.000500"}

		got := b.tryHandleEpicGardenerStart(context.Background(), ev, "gd-DOESNOTEXIST")
		if got {
			t.Fatalf("tryHandleEpicGardenerStart returned true for missing bead, want false")
		}
		if len(calls) != 0 {
			t.Errorf("expected no gardener-start invocations for missing bead, got %+v", calls)
		}
	})

	t.Run("nil daemon falls through", func(t *testing.T) {
		var calls []runCall
		orig := gardenerStartRunner
		gardenerStartRunner = func(_ context.Context, beadID, operatedBy string) ([]byte, error) {
			calls = append(calls, runCall{bead: beadID, operatedBy: operatedBy})
			return nil, nil
		}
		defer func() { gardenerStartRunner = orig }()

		b := &Bot{logger: slog.Default()}
		ev := &slackevents.AppMentionEvent{Channel: "C-ops", TimeStamp: "1700000000.000600"}

		if got := b.tryHandleEpicGardenerStart(context.Background(), ev, "gd-EPIC0001"); got {
			t.Errorf("expected fall-through with nil daemon, got handled=true")
		}
		if len(calls) != 0 {
			t.Errorf("expected no gardener-start calls with nil daemon, got %+v", calls)
		}
	})
}

// TestGardenerStartArgs pins the argv the bridge shells out with. This is the
// seam bug ka-0000006141 slipped through: the read-the-guide gate (ka-0000007415,
// gardener.go) made --i-read-the-gardener-instructions unconditionally required,
// which broke this programmatic caller because its argv omitted the flag. Every
// other test in this file stubs gardenerStartRunner wholesale, so the argv it
// builds was never asserted — a future refactor could drop the flag again and
// silently re-break the @gardenio <epic-id> entrypoint. Assert the flag directly
// on the pure builder so that regression fails loudly here.
func TestGardenerStartArgs(t *testing.T) {
	requireAckFlag := func(t *testing.T, args []string) {
		t.Helper()
		if !slices.Contains(args, "--i-read-the-gardener-instructions") {
			t.Fatalf("gardenerStartArgs is missing --i-read-the-gardener-instructions "+
				"(the read-the-guide gate refuses without it, breaking the Slack "+
				"epic-mention entrypoint — bug ka-0000006141): got %v", args)
		}
	}

	t.Run("with operator", func(t *testing.T) {
		got := gardenerStartArgs("gd-EPIC0001", "slack:U1")
		want := []string{
			"gardener", "start", "gd-EPIC0001",
			"--i-read-the-gardener-instructions",
			"--operated-by", "slack:U1",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("gardenerStartArgs = %v, want %v", got, want)
		}
		requireAckFlag(t, got)
	})

	t.Run("ownerless (no operator)", func(t *testing.T) {
		got := gardenerStartArgs("gd-EPIC0001", "")
		want := []string{
			"gardener", "start", "gd-EPIC0001",
			"--i-read-the-gardener-instructions",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("gardenerStartArgs = %v, want %v", got, want)
		}
		requireAckFlag(t, got)
	})
}
