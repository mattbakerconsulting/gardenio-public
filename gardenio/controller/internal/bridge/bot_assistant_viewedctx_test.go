package bridge

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// seedIssue adds an open issue bead labeled for a project so the viewed-channel
// live-context builder has recent work to list.
func (m *mockDaemon) seedIssue(id, project, title, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Kind:   "issue",
		Type:   "task",
		Status: status,
		Labels: []string{"project:" + project},
	}
}

func TestBuildViewedChannelContextNote_EnrichesWithChannelAndBeads(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C_CHAN")
	daemon.seedIssue("gd-AAA", "gardenio", "Fix the flux capacitor", "open")
	daemon.seedIssue("gd-BBB", "gardenio", "Ship the widget", "in_progress")
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)

	note := bot.buildViewedChannelContextNote(context.Background(), "C_CHAN")

	// conversations.info name enrichment ran (fake server returns name-<id>).
	if !strings.Contains(note, "#name-C_CHAN") {
		t.Errorf("expected channel name enrichment from conversations.info, got: %q", note)
	}
	if !strings.Contains(note, "project `gardenio`") {
		t.Errorf("expected project label in note, got: %q", note)
	}
	// Recent open work is listed with ids + statuses.
	for _, want := range []string{"gd-AAA", "gd-BBB", "[open]", "[in_progress]", "Recent open work"} {
		if !strings.Contains(note, want) {
			t.Errorf("expected note to contain %q, got: %q", want, note)
		}
	}

	// conversations.info was actually called for the viewed channel.
	if calls := srv.callsByMethod("conversations.info"); len(calls) == 0 {
		t.Error("expected a conversations.info call for the viewed channel")
	}
}

func TestBuildViewedChannelContextNote_EmptyOrUnmapped(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv.srv)

	// No viewed channel → empty.
	if got := bot.buildViewedChannelContextNote(context.Background(), ""); got != "" {
		t.Errorf("expected empty note with no viewed channel, got: %q", got)
	}
	// Viewed channel maps to no project → empty (skipped).
	if got := bot.buildViewedChannelContextNote(context.Background(), "C_UNMAPPED"); got != "" {
		t.Errorf("expected empty note for a channel with no project mapping, got: %q", got)
	}
}

func TestBuildViewedChannelContextNote_NoOpenBeads(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C_CHAN")
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv.srv)

	note := bot.buildViewedChannelContextNote(context.Background(), "C_CHAN")
	if !strings.Contains(note, "No open issues") {
		t.Errorf("expected a no-open-issues line, got: %q", note)
	}
}

func TestHandleAssistantMessage_EnrichesDescriptionWithViewedContext(t *testing.T) {
	// End-to-end: a pane message whose cached viewed channel maps to a project
	// spawns an agent whose description carries that channel's live context.
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("monorepo", "C_MONO")
	daemon.seedProjectWithChannel("gardenio", "C_GARDENIO")
	daemon.seedIssue("gd-LIVE1", "monorepo", "Investigate the reaper deadlock", "in_progress")
	srv := newFakeSlackAssistantServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv.srv)
	bot.botUserID = "U_BOT"
	bot.state, _ = NewStateManager("")

	// Pane opened while viewing the monorepo channel.
	bot.setAssistantCtxChannel("D_ASSISTANT", "1111.2222", "C_MONO")

	ev := &slackevents.MessageEvent{
		User:            "U_USER",
		Text:            "what's happening here?",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.2222",
		Channel:         "D_ASSISTANT",
		AssistantThread: &slackevents.AssistantThreadActionToken{ActionToken: "tok"},
	}

	bot.handleAssistantMessage(context.Background(), ev)

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	var desc string
	for _, b := range agentBeads {
		desc = b.Description
	}
	if !strings.Contains(desc, "Viewed Channel (live context)") {
		t.Errorf("expected viewed-channel context block in description, got:\n%s", desc)
	}
	if !strings.Contains(desc, "gd-LIVE1") {
		t.Errorf("expected the project's recent open bead in description, got:\n%s", desc)
	}
}
