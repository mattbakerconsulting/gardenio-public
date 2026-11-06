package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// --- /spawn command tests (new behavior: auto-name, channel-project) ---

func TestHandleSpawnCommand_NoArgs_CreatesAgentWithAutoName(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C123")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created with auto-name, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if !isValidAgentName(b.Title) {
			t.Errorf("auto-generated name %q is not valid", b.Title)
		}
	}
}

func TestHandleSpawnCommand_NoProject_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "C-UNMAPPED",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for unmapped channel, got %d", len(agentBeads))
	}
}

// TestHandleSpawnCommand_NoProject_ShowsAvailableProjects verifies that
// /spawn in an unmapped channel sends an ephemeral message listing
// available projects so the user knows what --project values to use.
func TestHandleSpawnCommand_NoProject_ShowsAvailableProjects(t *testing.T) {
	daemon := newMockDaemon()
	// Seed projects that are NOT mapped to the test channel.
	daemon.seedProjectWithChannel("gardenio", "C-OTHER")
	daemon.seedProjectWithChannel("fsm-triage", "C-ANOTHER")

	var ephemeralText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			_ = r.ParseForm()
			ephemeralText = r.FormValue("text")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678", "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "C-UNMAPPED",
		UserID:    "U456",
	})

	if !strings.Contains(ephemeralText, "no project is mapped") {
		t.Errorf("expected ephemeral error about unmapped channel, got %q", ephemeralText)
	}
	if !strings.Contains(ephemeralText, "Available projects:") {
		t.Errorf("expected available projects list in error message, got %q", ephemeralText)
	}
	if !strings.Contains(ephemeralText, "fsm-triage") {
		t.Errorf("expected fsm-triage in available projects, got %q", ephemeralText)
	}
}

func TestHandleSpawnCommand_NoArgs_ChannelProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "C-GARDENIO",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio from channel mapping, got %s", b.Fields["project"])
		}
		// Name should be "gardenio-<4chars>"
		if !strings.HasPrefix(b.Title, "gardenio-") {
			t.Errorf("expected auto-generated name with prefix 'gardenio-', got %q", b.Title)
		}
	}
}

func TestHandleSpawnCommand_WithTicket_ChannelProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	daemon.mu.Lock()
	daemon.beads["gd-task-77"] = &beadsapi.BeadDetail{
		ID:     "gd-task-77",
		Title:  "Fix auth flow",
		Type:   "task",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "gd-task-77",
		ChannelID: "C-GARDENIO",
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
		if b.Description != "Assigned to task: gd-task-77" {
			t.Errorf("expected description referencing gd-task-77, got %q", b.Description)
		}
		// Name should include project prefix and task title slug.
		if !strings.HasPrefix(b.Title, "gardenio-fix-auth-flow-") {
			t.Errorf("expected name with project prefix derived from task title, got %q", b.Title)
		}
	}
}

func TestHandleSpawnCommand_WithTicket_InfersProjectFromTicket(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("monorepo")
	daemon.mu.Lock()
	daemon.beads["gd-task-88"] = &beadsapi.BeadDetail{
		ID:     "gd-task-88",
		Title:  "Optimize queries",
		Type:   "task",
		Labels: []string{"project:monorepo"},
		Fields: map[string]string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// No channel-project mapping, but ticket has project label.
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "gd-task-88",
		ChannelID: "C-RANDOM",
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
	}
}

func TestHandleSpawnCommand_TicketNotFound_NoBeadCreated(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "PE-9999",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads when ticket not found, got %d", len(agentBeads))
	}
}

func TestHandleSpawnCommand_WithRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C123")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "--role gardener",
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

func TestHandleSpawnCommand_ChannelRoleOverride(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-GARDENER", "gardener")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		ChannelID: "C-GARDENER",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "gardener" {
			t.Errorf("expected role=gardener from channel_roles, got %s", b.Fields["role"])
		}
	}
}

func TestHandleSpawnCommand_ChannelRoleOverride_ExplicitRoleWins(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-GARDENER", "gardener")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "--role crew",
		ChannelID: "C-GARDENER",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["role"] != "crew" {
			t.Errorf("expected explicit role=crew to override channel_roles, got %s", b.Fields["role"])
		}
	}
}

func TestHandleSpawnCommand_TaskFirstMode_CreatesTaskBeadAndSpawns(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `"fix the login bug"`,
		ChannelID: "C-GARDENIO",
		UserID:    "U456",
	})

	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(taskBeads))
	}
	for _, b := range taskBeads {
		if b.Title != "fix the login bug" {
			t.Errorf("expected task title=%q, got %q", "fix the login bug", b.Title)
		}
		if !containsLabel(b.Labels, "project:gardenio") {
			t.Errorf("expected task to have label project:gardenio, got %v", b.Labels)
		}
	}

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %s", b.Fields["project"])
		}
		if b.Description == "" {
			t.Error("expected agent description to reference task bead, got empty")
		}
	}
}

func TestHandleSpawnCommand_ProjectAndTaskDescription(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `gardenio "fix the auth flow"`,
		ChannelID: "C123",
		UserID:    "U456",
	})

	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(taskBeads))
	}
	for _, b := range taskBeads {
		if b.Title != "fix the auth flow" {
			t.Errorf("expected task title=%q, got %q", "fix the auth flow", b.Title)
		}
		if !containsLabel(b.Labels, "project:gardenio") {
			t.Errorf("expected task to have label project:gardenio, got %v", b.Labels)
		}
	}

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %s", b.Fields["project"])
		}
		// Name should include project prefix and task description slug.
		if !strings.HasPrefix(b.Title, "gardenio-fix-the-auth-") {
			t.Errorf("expected name with project prefix derived from task description, got %q", b.Title)
		}
		if b.Description == "" {
			t.Error("expected agent description to reference task bead, got empty")
		}
	}
}

func TestHandleSpawnCommand_ProjectAndTaskDescriptionWithRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `gardenio "deploy the new service" --role devops`,
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

	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(taskBeads))
	}
}

func TestHandleSpawnCommand_ProjectAndTaskDescriptionOverridesChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("monorepo", "C-MONO")
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Channel maps to monorepo, but explicit project arg says gardenio.
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `gardenio "fix something"`,
		ChannelID: "C-MONO",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (explicit arg), got %s", b.Fields["project"])
		}
	}
}

func TestHandleSpawnCommand_TaskFirstMode_NoProject_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `"fix the login bug"`,
		ChannelID: "C-UNMAPPED",
		UserID:    "U456",
	})

	// Task bead is created before spawnAndRespond, but agent should be rejected.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for unmapped channel, got %d", len(agentBeads))
	}
}

func TestHandleSpawnCommand_DMChannel_AutoSelectsSoleProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /spawn in a DM channel with no --project flag — should auto-select sole project.
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `"fix this bug"`,
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM /spawn auto-select, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (auto-selected in DM), got %s", b.Fields["project"])
		}
	}
}

func TestHandleSpawnCommand_DMChannel_MultipleProjects_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /spawn in DM with multiple projects and no --project → rejected.
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `"fix something"`,
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads for DM with multiple projects, got %d", len(agentBeads))
	}
}

func TestHandleSpawnCommand_DMChannel_ExplicitProjectFlag(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /spawn in DM with explicit --project flag → uses specified project.
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `--project monorepo "fix the tests"`,
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM /spawn with --project, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "monorepo" {
			t.Errorf("expected project=monorepo (explicit flag), got %s", b.Fields["project"])
		}
	}
}

func TestHandleSpawnCommand_DMChannel_ExplicitProjectEqualsSyntax(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /spawn in DM with --project= syntax.
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `--project=gardenio "deploy it"`,
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM /spawn with --project=, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (explicit --project=), got %s", b.Fields["project"])
		}
	}
}

// --- Shared helper functions ---

// filterAgentBeads returns only the agent-type beads from a beads map.
func filterAgentBeads(beads map[string]*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	var result []*beadsapi.BeadDetail
	for _, b := range beads {
		if b.Type == "agent" {
			result = append(result, b)
		}
	}
	return result
}

// filterBeadsByType returns beads matching the given type from a beads map.
func filterBeadsByType(beads map[string]*beadsapi.BeadDetail, typ string) []*beadsapi.BeadDetail {
	var result []*beadsapi.BeadDetail
	for _, b := range beads {
		if b.Type == typ {
			result = append(result, b)
		}
	}
	return result
}

// filterTaskBeads returns only the task-type beads from a beads map.
func filterTaskBeads(beads map[string]*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	var result []*beadsapi.BeadDetail
	for _, b := range beads {
		if b.Type == "task" {
			result = append(result, b)
		}
	}
	return result
}

// containsLabel checks if a label is present in the labels slice.
func containsLabel(labels []string, target string) bool {
	for _, l := range labels {
		if l == target {
			return true
		}
	}
	return false
}

// --- Helper function unit tests ---

func TestIsTicketRef(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"jira key", "PE-1234", true},
		{"jira multi-letter", "DEVOPS-42", true},
		{"bead id", "gd-abc123", true},
		{"project name", "gardenio", false},
		{"empty", "", false},
		{"no digits", "PE-abc", false},
		{"just prefix", "PE-", false},
		{"lowercase jira", "pe-123", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTicketRef(tc.input); got != tc.want {
				t.Errorf("isTicketRef(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestIsValidAgentName(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{"lowercase", "my-bot", true},
		{"digits", "bot2", true},
		{"hyphens", "a-b-c", true},
		{"empty", "", false},
		{"uppercase", "MyBot", false},
		{"underscore", "my_bot", false},
		{"space", "my bot", false},
		{"special", "bot!", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isValidAgentName(tc.input); got != tc.want {
				t.Errorf("isValidAgentName(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestGenerateAgentName(t *testing.T) {
	cases := []struct {
		name        string
		project     string
		description string
		wantPrefix  string
	}{
		{"with project", "gardenio", "fix the login bug", "gardenio-fix-the-login-"},
		{"no project", "", "fix the login bug", "fix-the-login-"},
		{"two words", "myproj", "fix login", "myproj-fix-login-"},
		{"one word", "gardenio", "deploy", "gardenio-deploy-"},
		{"strips punctuation", "gardenio", "fix the @#$% bug!", "gardenio-fix-the-bug-"},
		{"uppercase", "gardenio", "Fix The Login Bug", "gardenio-fix-the-login-"},
		{"empty desc no project", "", "", "agent-"},
		{"empty desc with project", "gardenio", "", "gardenio-agent-"},
		{"only punctuation", "gardenio", "!@#$%", "gardenio-agent-"},
		{"more than three words", "gardenio", "refactor the entire auth system", "gardenio-refactor-the-entire-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generateAgentName(tc.project, tc.description)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("generateAgentName(%q, %q) = %q, want prefix %q", tc.project, tc.description, got, tc.wantPrefix)
			}
			suffix := got[len(tc.wantPrefix):]
			if len(suffix) != 3 {
				t.Errorf("expected 3-char suffix, got %q (%d chars)", suffix, len(suffix))
			}
			if !isValidAgentName(got) {
				t.Errorf("generated name %q is not a valid agent name", got)
			}
		})
	}
}

func TestGenerateSpawnName(t *testing.T) {
	cases := []struct {
		name       string
		project    string
		wantPrefix string
	}{
		{"with project", "gardenio", "gardenio-"},
		{"empty project", "", "agent-"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := generateSpawnName(tc.project)
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("generateSpawnName(%q) = %q, want prefix %q", tc.project, got, tc.wantPrefix)
			}
			suffix := got[len(tc.wantPrefix):]
			if len(suffix) != 4 {
				t.Errorf("expected 4-char suffix, got %q (%d chars)", suffix, len(suffix))
			}
			if !isValidAgentName(got) {
				t.Errorf("generated name %q is not a valid agent name", got)
			}
		})
	}
}

func TestExtractSpawnFlags(t *testing.T) {
	cases := []struct {
		name          string
		args          []string
		wantPos       []string
		wantRole      string
		wantProject   string
		wantUltracode string
		wantBad       string
	}{
		{"no flags", []string{"gd-abc"}, []string{"gd-abc"}, "", "", "", ""},
		{"role flag", []string{"--role", "gardener"}, nil, "gardener", "", "", ""},
		{"role=", []string{"--role=gardener"}, nil, "gardener", "", "", ""},
		{"project flag", []string{"--project", "gardenio"}, nil, "", "gardenio", "", ""},
		{"project=", []string{"--project=gardenio"}, nil, "", "gardenio", "", ""},
		{"both flags", []string{"--project", "gardenio", "--role", "gardener"}, nil, "gardener", "gardenio", "", ""},
		{"flags with positional", []string{"gd-abc", "--project", "gardenio", "--role", "ops"}, []string{"gd-abc"}, "ops", "gardenio", "", ""},
		{"bare ultracode", []string{"--ultracode"}, nil, "", "", "on", ""},
		{"ultracode=on", []string{"--ultracode=on"}, nil, "", "", "on", ""},
		{"ultracode=off", []string{"--ultracode=off"}, nil, "", "", "off", ""},
		{"ultracode=true alias", []string{"--ultracode=true"}, nil, "", "", "on", ""},
		{"ultracode=bad", []string{"--ultracode=maybe"}, nil, "", "", "", "maybe"},
		{"ultracode with others", []string{"gd-abc", "--ultracode=off", "--role", "crew"}, []string{"gd-abc"}, "crew", "", "off", ""},
		{"empty", nil, nil, "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Make a copy of args since extractSpawnFlags reuses the backing array.
			argsCopy := make([]string, len(tc.args))
			copy(argsCopy, tc.args)
			pos, role, project, _, ultracode, bad := extractSpawnFlags(argsCopy)
			if len(pos) == 0 {
				pos = nil
			}
			if len(tc.wantPos) == 0 {
				tc.wantPos = nil
			}
			if len(pos) != len(tc.wantPos) {
				t.Errorf("positional: got %v (len %d), want %v (len %d)", pos, len(pos), tc.wantPos, len(tc.wantPos))
			} else {
				for i := range pos {
					if pos[i] != tc.wantPos[i] {
						t.Errorf("positional[%d]: got %q, want %q", i, pos[i], tc.wantPos[i])
					}
				}
			}
			if role != tc.wantRole {
				t.Errorf("role: got %q, want %q", role, tc.wantRole)
			}
			if project != tc.wantProject {
				t.Errorf("project: got %q, want %q", project, tc.wantProject)
			}
			if ultracode != tc.wantUltracode {
				t.Errorf("ultracode: got %q, want %q", ultracode, tc.wantUltracode)
			}
			if bad != tc.wantBad {
				t.Errorf("ultracodeBad: got %q, want %q", bad, tc.wantBad)
			}
		})
	}
}

// --- Slash command routing test ---

func TestHandleSlashCommand_RoutesStartAndSpawn(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C123")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /start routes to handleStartCommand (requires agent name).
	bot.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/start",
		Text:      "my-bot",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected /start to create 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Title != "my-bot" {
			t.Errorf("expected /start agent title=my-bot, got %s", b.Title)
		}
	}

	// /spawn routes to handleSpawnCommand (auto-generates name).
	bot.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})

	agentBeads = filterAgentBeads(daemon.beads)
	if len(agentBeads) != 2 {
		t.Fatalf("expected /spawn to create a second agent bead, got %d total", len(agentBeads))
	}
}

func TestSplitQuotedArgs_SmartQuotes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"straight quotes", `"fix the helm chart"`, []string{"fix the helm chart"}},
		{"smart quotes", "\u201cfix the helm chart\u201d", []string{"fix the helm chart"}},
		{"mixed quotes", "\u201cfix the chart\u201d and more", []string{"fix the chart", "and", "more"}},
		{"no quotes", "fix the helm chart", []string{"fix", "the", "helm", "chart"}},
		{"empty", "", nil},
		{"single word", "deploy", []string{"deploy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitQuotedArgs(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("splitQuotedArgs(%q) = %v (len %d), want %v (len %d)", tt.input, got, len(got), tt.want, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitQuotedArgs(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestHandleSpawnCommand_SmartQuotes_PassesPrompt(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C123")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Simulate Slack sending smart quotes: /spawn "fix the helm chart"
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "\u201cfix the helm chart\u201d",
		ChannelID: "C123",
		UserID:    "U456",
	})

	// Should have created a task bead + agent bead.
	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) == 0 {
		t.Fatal("expected a task bead to be created for the prompt")
	}
	found := false
	for _, tb := range taskBeads {
		if tb.Title == "fix the helm chart" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected task bead with title 'fix the helm chart', got %v", taskBeads)
	}

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) == 0 {
		t.Fatal("expected an agent bead to be created")
	}
	// The agent bead should have the prompt in its fields.
	for _, ab := range agentBeads {
		if ab.Fields["prompt"] == "fix the helm chart" {
			return // success
		}
	}
	t.Errorf("expected agent bead to have prompt field 'fix the helm chart'")
}

// --- /spawn --project flag tests ---

func TestHandleSpawnCommand_ProjectFlag_OverridesChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("monorepo", "C-MONO")
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Channel maps to monorepo, but --project says gardenio.
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "--project gardenio",
		ChannelID: "C-MONO",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (from --project flag), got %s", b.Fields["project"])
		}
		// Name should use gardenio prefix.
		if !strings.HasPrefix(b.Title, "gardenio-") {
			t.Errorf("expected name with prefix 'gardenio-', got %q", b.Title)
		}
	}
}

func TestHandleSpawnCommand_ProjectFlagEquals_OverridesChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("monorepo", "C-MONO")
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// --project=gardenio syntax
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "--project=gardenio",
		ChannelID: "C-MONO",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (from --project=), got %s", b.Fields["project"])
		}
	}
}

func TestHandleSpawnCommand_ProjectFlag_WithTask(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /spawn --project gardenio "fix the auth flow"
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      `--project gardenio "fix the auth flow"`,
		ChannelID: "C-RANDOM",
		UserID:    "U456",
	})

	taskBeads := filterBeadsByType(daemon.beads, "task")
	if len(taskBeads) != 1 {
		t.Fatalf("expected 1 task bead created, got %d", len(taskBeads))
	}
	for _, b := range taskBeads {
		if !containsLabel(b.Labels, "project:gardenio") {
			t.Errorf("expected task to have label project:gardenio, got %v", b.Labels)
		}
	}

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %s", b.Fields["project"])
		}
	}
}

func TestHandleSpawnCommand_ProjectFlag_WithRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /spawn --project gardenio --role gardener
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "--project gardenio --role gardener",
		ChannelID: "C-RANDOM",
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
		if b.Fields["role"] != "gardener" {
			t.Errorf("expected role=gardener, got %s", b.Fields["role"])
		}
	}
}

func TestHandleSpawnCommand_StoresSpawnChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "C-GARDENIO",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["slack_spawn_channel"] != "C-GARDENIO" {
			t.Errorf("expected slack_spawn_channel=C-GARDENIO, got %q", b.Fields["slack_spawn_channel"])
		}
	}
}

func TestHandleSpawnCommand_ProjectFlag_NoChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// /spawn --project gardenio from an unmapped channel
	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "--project gardenio",
		ChannelID: "C-UNMAPPED",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead created, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (from --project in unmapped channel), got %s", b.Fields["project"])
		}
	}
}

// --- resolveChannel spawn-channel-aware tests ---

func TestResolveChannel_SpawnChannel_TakesPrecedenceOverProjectPrimary(t *testing.T) {
	daemon := newMockDaemon()
	// Project has C-PRIMARY as its first (primary) channel.
	daemon.seedProjectWithChannel("monorepo", "C-PRIMARY")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	bot.agentProject["my-agent"] = "monorepo"
	// Agent was spawned from C-GARDENIO (a different channel than the primary).
	bot.agentSpawnChannel["my-agent"] = "C-GARDENIO"

	result := bot.resolveChannel("my-agent")
	if result != "C-GARDENIO" {
		t.Errorf("expected C-GARDENIO (spawn channel > project primary), got %q", result)
	}
}

func TestResolveChannel_SpawnChannel_NoSpawnChannel_FallsBackToProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	bot.agentProject["my-agent"] = "gardenio"
	// No spawn channel set — should fall back to project primary.

	result := bot.resolveChannel("my-agent")
	if result != "C-GARDENIO" {
		t.Errorf("expected C-GARDENIO (project primary, no spawn channel), got %q", result)
	}
}

func TestResolveChannel_RouterOverride_TakesPrecedenceOverSpawnChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-ROUTER-DEFAULT",
		Overrides: map[string]string{
			"my-agent": "C-OVERRIDE",
		},
	})

	bot.agentProject["my-agent"] = "gardenio"
	bot.agentSpawnChannel["my-agent"] = "C-GARDENIO"

	result := bot.resolveChannel("my-agent")
	if result != "C-OVERRIDE" {
		t.Errorf("expected C-OVERRIDE (router override > spawn channel), got %q", result)
	}
}

// --- resolveChannel project-aware tests ---

func TestResolveChannel_AgentProject_UsesProjectPrimaryChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	// Cache the project for the agent.
	bot.agentProject["my-agent"] = "gardenio"

	result := bot.resolveChannel("my-agent")
	if result != "C-GARDENIO" {
		t.Errorf("expected C-GARDENIO (project primary channel), got %q", result)
	}
}

func TestResolveChannel_AgentProject_NoChannelConfig_FallsBack(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio") // No channel configured
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	bot.agentProject["my-agent"] = "gardenio"

	result := bot.resolveChannel("my-agent")
	if result != "C-DEFAULT" {
		t.Errorf("expected C-DEFAULT (no project channel), got %q", result)
	}
}

func TestResolveChannel_AgentProject_UnknownProject_FallsBack(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	bot.agentProject["my-agent"] = "nonexistent"

	result := bot.resolveChannel("my-agent")
	if result != "C-DEFAULT" {
		t.Errorf("expected C-DEFAULT (unknown project), got %q", result)
	}
}

func TestResolveChannel_RouterOverride_TakesPrecedenceOverProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-ROUTER-DEFAULT",
		Overrides: map[string]string{
			"my-agent": "C-OVERRIDE",
		},
	})

	bot.agentProject["my-agent"] = "gardenio"

	result := bot.resolveChannel("my-agent")
	if result != "C-OVERRIDE" {
		t.Errorf("expected C-OVERRIDE (router override > project), got %q", result)
	}
}

func TestResolveChannel_RouterPatternMatch_TakesPrecedenceOverProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-ROUTER-DEFAULT",
		Channels: map[string]string{
			"gardenio/crew/*": "C-CREW",
		},
	})

	bot.agentProject["my-agent"] = "gardenio"

	// Pattern match "gardenio/crew/*" should take precedence over project channel
	result := bot.resolveChannel("gardenio/crew/my-agent")
	if result != "C-CREW" {
		t.Errorf("expected C-CREW (router pattern > project), got %q", result)
	}
}

func TestResolveChannel_ProjectChannel_OverridesRouterDefault(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"
	bot.router = NewRouter(RouterConfig{
		DefaultChannel: "C-ROUTER-DEFAULT",
	})

	bot.agentProject["my-agent"] = "gardenio"

	// No router pattern matches, project channel should override router default
	result := bot.resolveChannel("my-agent")
	if result != "C-GARDENIO" {
		t.Errorf("expected C-GARDENIO (project channel > router default), got %q", result)
	}
}

func TestSpawnAndRespond_NoProject_ListsAvailableProjects(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("kbeads")

	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postEphemeral" {
			_ = r.ParseForm()
			postedText = r.FormValue("text")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.spawnAndRespond(context.Background(), slack.SlashCommand{
		ChannelID: "C-UNMAPPED",
		UserID:    "U456",
	}, "test-agent", "", "", "", "", "")

	if postedText == "" {
		t.Fatal("expected ephemeral error message to be posted")
	}
	if !strings.Contains(postedText, "no project is mapped") {
		t.Errorf("expected 'no project is mapped' in message, got: %s", postedText)
	}
	if !strings.Contains(postedText, "Available projects:") {
		t.Errorf("expected 'Available projects:' in message, got: %s", postedText)
	}
	if !strings.Contains(postedText, "gardenio") {
		t.Errorf("expected 'gardenio' in available projects, got: %s", postedText)
	}
	if !strings.Contains(postedText, "kbeads") {
		t.Errorf("expected 'kbeads' in available projects, got: %s", postedText)
	}
}

func TestHandleSpawnCommand_DuplicateChannel_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	// Two projects claim the same channel — should refuse to spawn.
	daemon.seedProjectWithChannel("alpha", "C-SHARED")
	daemon.seedProjectWithChannel("beta", "C-SHARED")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "C-SHARED",
		UserID:    "U456",
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected no agent beads when channel is claimed by multiple projects, got %d", len(agentBeads))
	}
}

// --- projectFromUserHistory tests ---

func TestProjectFromUserHistory_MatchesSlackUserID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	// Seed a closed agent bead for user U-ALICE with project=monorepo.
	daemon.mu.Lock()
	daemon.beads["agent-old"] = &beadsapi.BeadDetail{
		ID:     "agent-old",
		Title:  "old-agent",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":         "old-agent",
			"project":       "monorepo",
			"role":          "crew",
			"slack_user_id": "U-ALICE",
		},
	}
	daemon.mu.Unlock()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	projects, _ := daemon.ListProjectBeads(context.Background())
	got := bot.projectFromUserHistory(context.Background(), "U-ALICE", projects)
	if got != "monorepo" {
		t.Errorf("expected monorepo, got %q", got)
	}
}

func TestProjectFromUserHistory_NoMatch(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	projects, _ := daemon.ListProjectBeads(context.Background())
	got := bot.projectFromUserHistory(context.Background(), "U-NOBODY", projects)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestProjectFromUserHistory_IgnoresDeletedProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	// "oldproject" is NOT seeded as a project bead → it was deleted.

	// Seed agent bead referencing a project that no longer exists.
	daemon.mu.Lock()
	daemon.beads["agent-stale"] = &beadsapi.BeadDetail{
		ID:     "agent-stale",
		Title:  "stale-agent",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":         "stale-agent",
			"project":       "oldproject",
			"role":          "crew",
			"slack_user_id": "U-BOB",
		},
	}
	daemon.mu.Unlock()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	projects, _ := daemon.ListProjectBeads(context.Background())
	got := bot.projectFromUserHistory(context.Background(), "U-BOB", projects)
	if got != "" {
		t.Errorf("expected empty (project deleted), got %q", got)
	}
}

func TestSpawnCommand_DM_UserHistoryFallback(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	// Seed a prior agent for user U456.
	daemon.mu.Lock()
	daemon.beads["agent-prior"] = &beadsapi.BeadDetail{
		ID:     "agent-prior",
		Title:  "prior-agent",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":         "prior-agent",
			"project":       "gardenio",
			"role":          "crew",
			"slack_user_id": "U456",
		},
	}
	daemon.mu.Unlock()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleSpawnCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "",
		ChannelID: "D1234567890",
		UserID:    "U456",
	})

	var newBeads []*beadsapi.BeadDetail
	for _, b := range filterAgentBeads(daemon.beads) {
		if b.ID != "agent-prior" {
			newBeads = append(newBeads, b)
		}
	}
	if len(newBeads) != 1 {
		t.Fatalf("expected 1 new agent bead from DM with user history, got %d", len(newBeads))
	}
	if newBeads[0].Fields["project"] != "gardenio" {
		t.Errorf("expected project=gardenio (from user history), got %q", newBeads[0].Fields["project"])
	}
}
