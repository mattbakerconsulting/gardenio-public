package bridge

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/slack-go/slack/slackevents"
)

func TestHandleThreadSpawn_WithRouter(t *testing.T) {
	daemon := newMockDaemon()

	router := NewRouter(RouterConfig{
		Overrides: map[string]string{
			"gardenio/crew/hq": "C-agents",
		},
	})

	b := &Bot{
		daemon:     daemon,
		state:      nil, // no state persistence in this test
		logger:     slog.Default(),
		botUserID:  "U-BOT",
		router:     router,
		agentCards: map[string]MessageRef{},
	}

	// Verify project inference from router.
	mapped := router.GetAgentByChannel("C-agents")
	if mapped != "gardenio/crew/hq" {
		t.Fatalf("expected gardenio/crew/hq, got %q", mapped)
	}

	project := projectFromAgentIdentity(mapped)
	if project != "gardenio" {
		t.Errorf("project = %q, want gardenio", project)
	}

	// For unmapped channel, project should be empty.
	mapped = router.GetAgentByChannel("C-random")
	if mapped != "" {
		t.Errorf("expected empty agent for unmapped channel, got %q", mapped)
	}
	_ = b // used
}

func TestHandleThreadSpawn_ProjectFromProjectBead(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a project bead with slack_channel — this is how /spawn resolves projects.
	daemon.seedProjectWithChannel("monorepo", "C-DEVOPS")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"
	// No router overrides for C-DEVOPS — project must come from project bead.

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-DEVOPS",
		ThreadTimeStamp: "9999.1111",
		TimeStamp:       "9999.2222",
		Text:            "<@U-BOT> help with this",
	}

	bot.handleThreadSpawn(context.Background(), ev, "help with this")

	// Find the agent bead that was created.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo from project bead slack_channel, got %q", b.Fields["project"])
		}
	}
}

func TestHandleThreadSpawn_ChannelRoleOverride(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a project with channel_roles: C-K6 → "k6-perf"
	daemon.seedProjectWithChannelRole("monorepo", "C-K6", "k6-perf")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-K6",
		ThreadTimeStamp: "8888.1111",
		TimeStamp:       "8888.2222",
		Text:            "<@U-BOT> run perf tests",
	}

	bot.handleThreadSpawn(context.Background(), ev, "run perf tests")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "k6-perf" {
			t.Errorf("expected role=k6-perf from channel_roles override, got %q", b.Fields["role"])
		}
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo, got %q", b.Fields["project"])
		}
	}
}

func TestHandleThreadSpawn_ChannelRuntimeClassOverride(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a project whose gb-turbo channel pins thread agents onto runsc.
	daemon.seedProjectWithChannelRuntimeClass("gardenio", "C-TURBO", "runsc")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-TURBO",
		ThreadTimeStamp: "6666.1111",
		TimeStamp:       "6666.2222",
		Text:            "<@U-BOT> cook me up a turbo agent",
	}

	bot.handleThreadSpawn(context.Background(), ev, "cook me up a turbo agent")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["runtime_class"] != "runsc" {
			t.Errorf("expected runtime_class=runsc from channel_runtime_class override, got %q", b.Fields["runtime_class"])
		}
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %q", b.Fields["project"])
		}
	}
}

func TestHandleThreadSpawn_NoChannelRuntimeClass_NoPin(t *testing.T) {
	daemon := newMockDaemon()
	// A channel with no channel_runtime_class override leaves runtime_class unset.
	daemon.seedProjectWithChannel("gardenio", "C-PLAIN")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-PLAIN",
		ThreadTimeStamp: "6666.3333",
		TimeStamp:       "6666.4444",
		Text:            "<@U-BOT> normal request",
	}

	bot.handleThreadSpawn(context.Background(), ev, "normal request")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if rc, ok := b.Fields["runtime_class"]; ok && rc != "" {
			t.Errorf("expected no runtime_class pin for plain channel, got %q", rc)
		}
	}
}

func TestHandleThreadSpawn_NoChannelRole_DefaultsToThread(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a project with no channel_roles — should fall back to "thread".
	daemon.seedProjectWithChannel("monorepo", "C-GENERAL")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-GENERAL",
		ThreadTimeStamp: "7777.1111",
		TimeStamp:       "7777.2222",
		Text:            "<@U-BOT> help me",
	}

	bot.handleThreadSpawn(context.Background(), ev, "help me")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "thread" {
			t.Errorf("expected role=thread (default), got %q", b.Fields["role"])
		}
	}
}

func TestRespawnThreadAgent_ChannelRoleOverride(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-K6", "k6-perf")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "U-BOT"

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state

	bot.respawnThreadAgent(context.Background(), "C-K6", "6666.1111", "thread-6666-1111", "help", "U-HUMAN")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "k6-perf" {
			t.Errorf("expected role=k6-perf from channel_roles override on respawn, got %q", b.Fields["role"])
		}
	}
}
