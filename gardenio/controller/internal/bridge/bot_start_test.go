package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"log/slog"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"golang.org/x/time/rate"
)

// newTestBot creates a Bot wired to a fake Slack API server for unit tests.
// The server responds OK to all requests (PostEphemeral, etc.).
func newTestBot(daemon BeadClient, slackSrv *httptest.Server) *Bot {
	api := slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"))
	return &Bot{
		api:                    api,
		daemon:                 daemon,
		logger:                 slog.Default(),
		messages:               make(map[string]MessageRef),
		agentCards:             make(map[string]MessageRef),
		agentPending:           make(map[string]int),
		agentState:             make(map[string]string),
		agentSchedulingBlocked: make(map[string]bool),
		agentSeen:              make(map[string]time.Time),
		agentPodName:           make(map[string]string),
		agentImageTag:          make(map[string]string),
		agentRole:              make(map[string]string),
		agentProject:           make(map[string]string),
		agentScheduleTitle:     make(map[string]string),
		agentSpawnedAt:         make(map[string]time.Time),
		agentSpawnChannel:      make(map[string]string),
		agentSpawnInheritedBy:  make(map[string]string),
		threadSpawnMsgs:        make(map[string]MessageRef),
		spawnInFlight:          make(map[string]bool),
		assistantCtxChannel:    make(map[string]string),
		agentCardLastUpdate:    make(map[string]time.Time),
		agentCardDirty:         make(map[string]bool),
		// Permissive limiter for tests; rate-limit behaviour has its own
		// dedicated tests that override this with a tighter limiter.
		slackCardLimiter:   rate.NewLimiter(rate.Inf, 1),
		conciergeDebouncer: newConciergeDebouncer(),
		reopenDebouncer:    newReopenDebouncer(),
	}
}

// newFakeSlackServer returns an httptest.Server that accepts any Slack API call
// and returns a generic OK response.
func newFakeSlackServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
}

// --- /start command tests (old /spawn behavior, requires agent name) ---

func TestHandleStartCommand_SpawnsAgentWithProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot gardenio",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Title != "my-bot" {
			t.Errorf("expected title=my-bot, got %s", b.Title)
		}
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %s", b.Fields["project"])
		}
		if b.Fields["role"] != "crew" {
			t.Errorf("expected default role=crew, got %s", b.Fields["role"])
		}
		if b.Fields["slack_user_id"] != "U456" {
			t.Errorf("expected slack_user_id=U456, got %s", b.Fields["slack_user_id"])
		}
	}
}

func TestHandleStartCommand_SpawnsAgentWithRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot gardenio --role gardener",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "gardener" {
			t.Errorf("expected role=gardener, got %s", b.Fields["role"])
		}
	}
}

func TestHandleStartCommand_SpawnsAgentWithRoleEquals(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot gardenio --role=jirafix",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "jirafix" {
			t.Errorf("expected role=jirafix, got %s", b.Fields["role"])
		}
	}
}

func TestHandleStartCommand_SpawnsAgentWithTask(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot gardenio gd-task-42",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Description != "Assigned to task: gd-task-42" {
			t.Errorf("expected description %q, got %q", "Assigned to task: gd-task-42", b.Description)
		}
	}
}

func TestHandleStartCommand_SpawnsAgentNoProject_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot",
		ChannelID: "C-UNMAPPED",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for unmapped channel, got %d", len(agentBeads))
	}
}

func TestHandleStartCommand_SpawnsAgentWithChannelProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C123")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
}

func TestHandleStartCommand_EmptyArgs_NoBeadCreated(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})

	if len(daemon.beads) != 0 {
		t.Errorf("expected no bead created for empty args, got %d", len(daemon.beads))
	}
}

// TestHandleStartCommand_FlagsOnly_NoPanic guards the empty-positional case:
// `/start --role crew` passes the len(args)==0 check but extractSpawnFlags
// strips every token, leaving positional empty. Without the len(positional)==0
// guard the subsequent positional[0] access (and the one in handleTaskFirstSpawn)
// panics with index-out-of-range. Assert it returns cleanly with no bead created.
func TestHandleStartCommand_FlagsOnly_NoPanic(t *testing.T) {
	for _, text := range []string{"--role crew", "--project gardenio", "--role crew --project gardenio", "--ultracode"} {
		t.Run(text, func(t *testing.T) {
			daemon := newMockDaemon()
			slackSrv := newFakeSlackServer(t)
			defer slackSrv.Close()

			bot := newTestBot(daemon, slackSrv)

			bot.handleStartCommand(context.Background(), slack.SlashCommand{
				Command:   "/start",
				Text:      text,
				ChannelID: "C123",
				UserID:    "U456",
			})

			if len(daemon.beads) != 0 {
				t.Errorf("expected no bead created for flags-only input %q, got %d", text, len(daemon.beads))
			}
		})
	}
}

func TestHandleStartCommand_InvalidAgentName_NoBeadCreated(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "My_Bot!",
		ChannelID: "C123",
		UserID:    "U456",
	})

	if len(daemon.beads) != 0 {
		t.Errorf("expected no bead created for invalid name, got %d", len(daemon.beads))
	}
}

func TestHandleStartCommand_ResolvesJiraTicket(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	daemon.mu.Lock()
	daemon.beads["gd-resolved-1"] = &beadsapi.BeadDetail{
		ID:     "gd-resolved-1",
		Title:  "Fix login bug",
		Type:   "task",
		Labels: []string{"jira:PE-1234", "project:monorepo"},
		Fields: map[string]string{"jira_key": "PE-1234"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot PE-1234",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo (inferred from ticket), got %s", b.Fields["project"])
		}
		if b.Description != "Assigned to task: gd-resolved-1" {
			t.Errorf("expected description referencing resolved bead ID, got %q", b.Description)
		}
	}
}

func TestHandleStartCommand_ResolvesBeadID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.mu.Lock()
	daemon.beads["gd-task-99"] = &beadsapi.BeadDetail{
		ID:     "gd-task-99",
		Title:  "Implement feature X",
		Type:   "task",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot gd-task-99",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (inferred from bead labels), got %s", b.Fields["project"])
		}
		if b.Description != "Assigned to task: gd-task-99" {
			t.Errorf("expected description referencing gd-task-99, got %q", b.Description)
		}
	}
}

func TestHandleStartCommand_TicketNotFound_NoBeadCreated(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot PE-9999",
		ChannelID: "C123",
		UserID:    "U456",
	})

	if len(daemon.beads) != 0 {
		t.Errorf("expected no bead created when ticket not found, got %d", len(daemon.beads))
	}
}

func TestHandleStartCommand_TicketWithRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	daemon.mu.Lock()
	daemon.beads["gd-resolved-2"] = &beadsapi.BeadDetail{
		ID:     "gd-resolved-2",
		Title:  "Deploy service",
		Type:   "task",
		Labels: []string{"jira:DEVOPS-42", "project:monorepo"},
		Fields: map[string]string{"jira_key": "DEVOPS-42"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "deploy-bot DEVOPS-42 --role devops",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "devops" {
			t.Errorf("expected role=devops, got %s", b.Fields["role"])
		}
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo, got %s", b.Fields["project"])
		}
	}
}

func TestHandleStartCommand_TaskFirstMode(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      `"fix the login bug" gardenio`,
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %s", b.Fields["project"])
		}
		if b.Description == "" {
			t.Errorf("expected agent description to reference task, got empty")
		}
	}

	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(taskBeads))
	}
	for _, b := range taskBeads {
		if b.Title != "fix the login bug" {
			t.Errorf("expected task title=%q, got %q", "fix the login bug", b.Title)
		}
	}
}

func TestHandleStartCommand_TaskFirstModeWithRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      `"deploy the new service" gardenio --role devops`,
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "devops" {
			t.Errorf("expected role=devops, got %s", b.Fields["role"])
		}
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %s", b.Fields["project"])
		}
	}
}

func TestHandleStartCommand_TaskFirstModeNoProject_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      `"fix the login bug"`,
		ChannelID: "C-UNMAPPED",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for unmapped channel, got %d", len(agentBeads))
	}
}

func TestHandleStartCommand_TaskFirstModeInvalidProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      `"fix the login bug" nonexistent`,
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for invalid project, got %d", len(agentBeads))
	}
	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 0 {
		t.Errorf("expected no task beads for invalid project, got %d", len(taskBeads))
	}
}

// --- /start DM channel tests ---

func TestHandleStartCommand_DMChannel_AutoSelectsSoleProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /start in a DM channel with no --project flag — should auto-select sole project.
	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot",
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM /start auto-select, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (auto-selected in DM), got %s", b.Fields["project"])
		}
	}
}

func TestHandleStartCommand_DMChannel_MultipleProjects_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /start in DM with multiple projects and no --project → rejected.
	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot",
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for DM with multiple projects, got %d", len(agentBeads))
	}
}

func TestHandleStartCommand_DMChannel_TaskFirstMode_AutoSelectsSoleProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /start "task description" in DM — should auto-select sole project.
	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      `"fix the auth bug"`,
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM /start task-first auto-select, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (auto-selected in DM), got %s", b.Fields["project"])
		}
	}

	taskBeads := filterTaskBeads(daemon.beads)
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(taskBeads))
	}
	for _, b := range taskBeads {
		if !containsLabel(b.Labels, "project:gardenio") {
			t.Errorf("expected task to have label project:gardenio, got %v", b.Labels)
		}
	}
}

func TestHandleStartCommand_DMChannel_ExplicitProjectFlag(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /start in DM with explicit --project flag → uses specified project.
	bot.handleStartCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      `my-bot --project monorepo`,
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM /start with --project, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo (explicit flag), got %s", b.Fields["project"])
		}
	}
}
