package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// seedTrigger pre-populates the mock with a trigger bead.
func seedTrigger(m *mockDaemon, id, title, project, source string, enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	enabledStr := "true"
	if !enabled {
		enabledStr = "false"
	}
	labels := []string{}
	if source != "" {
		labels = append(labels, "trigger:"+source)
	}
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "trigger",
		Status: "open",
		Labels: labels,
		Fields: map[string]string{
			"enabled": enabledStr,
			"project": project,
		},
	}
}

func TestTriggerCommand_Off_PausesTriggers(t *testing.T) {
	daemon := newMockDaemon()
	seedTrigger(daemon, "trig-1", "JIRA new bug", "gardenio", "jira", true)
	seedTrigger(daemon, "trig-2", "GitLab PR merged", "gardenio", "gitlab", false)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "off",
		ChannelID: "C123",
		UserID:    "U456",
	})

	gate := findGateBead(daemon, triggersPauseTitle)
	if gate == nil {
		t.Fatal("expected triggers-pause gate bead to be created")
	}
	if gate.Type != "gate" {
		t.Errorf("gate bead type: expected gate, got %q", gate.Type)
	}

	// Individual trigger beads should NOT have been modified.
	daemon.mu.Lock()
	if daemon.beads["trig-1"].Fields["enabled"] != "true" {
		t.Error("trig-1 enabled field should not have changed")
	}
	if daemon.beads["trig-2"].Fields["enabled"] != "false" {
		t.Error("trig-2 enabled field should not have changed")
	}
	daemon.mu.Unlock()
}

func TestTriggerCommand_On_UnpausesTriggers(t *testing.T) {
	daemon := newMockDaemon()
	seedTrigger(daemon, "trig-1", "JIRA new bug", "gardenio", "jira", true)

	seedGate(daemon, "gate-1", triggersPauseTitle)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "on",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	gate := daemon.beads["gate-1"]
	daemon.mu.Unlock()
	if gate.Status != "closed" {
		t.Errorf("gate bead status: expected closed, got %q", gate.Status)
	}
}

func TestTriggerCommand_Off_AlreadyPaused(t *testing.T) {
	daemon := newMockDaemon()
	seedGate(daemon, "gate-1", triggersPauseTitle)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "off",
		ChannelID: "C123",
		UserID:    "U456",
	})

	gates := countGateBeads(daemon, triggersPauseTitle)
	if gates != 1 {
		t.Errorf("expected 1 gate bead, got %d", gates)
	}
}

func TestTriggerCommand_On_AlreadyRunning(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "on",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

func TestTriggerCommand_NoArg_ShowsStatus(t *testing.T) {
	daemon := newMockDaemon()
	seedTrigger(daemon, "trig-1", "JIRA bug", "gardenio", "jira", true)
	seedTrigger(daemon, "trig-2", "GitLab merge", "gardenio", "gitlab", false)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	if daemon.beads["trig-1"].Fields["enabled"] != "true" {
		t.Error("trig-1 should remain enabled")
	}
	if daemon.beads["trig-2"].Fields["enabled"] != "false" {
		t.Error("trig-2 should remain disabled")
	}
	daemon.mu.Unlock()
}

func TestTriggerCommand_NoArg_ShowsPausedState(t *testing.T) {
	daemon := newMockDaemon()
	seedTrigger(daemon, "trig-1", "JIRA bug", "gardenio", "jira", true)
	seedGate(daemon, "gate-1", triggersPauseTitle)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

func TestTriggerCommand_NoTriggers(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

// --- per-project trigger tests ---

func TestTriggerCommand_OffProject_DisablesProjectTriggers(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	seedTrigger(daemon, "trig-1", "JIRA bug", "gardenio", "jira", true)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "off gardenio",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	proj := daemon.beads["proj-gardenio"]
	daemon.mu.Unlock()
	if proj.Fields["triggers_enabled"] != "false" {
		t.Errorf("expected triggers_enabled=false, got %q", proj.Fields["triggers_enabled"])
	}

	// Audit comment must be recorded on the project bead.
	daemon.mu.Lock()
	var hasAudit bool
	for _, c := range daemon.comments {
		if c.BeadID == "proj-gardenio" && c.Author == "U456" {
			hasAudit = true
			break
		}
	}
	daemon.mu.Unlock()
	if !hasAudit {
		t.Error("expected audit comment on project bead")
	}

	// Individual trigger beads should NOT have been modified.
	daemon.mu.Lock()
	if daemon.beads["trig-1"].Fields["enabled"] != "true" {
		t.Error("trig-1 enabled field should not have changed")
	}
	daemon.mu.Unlock()
}

func TestTriggerCommand_OnProject_EnablesProjectTriggers(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	daemon.mu.Lock()
	if daemon.beads["proj-gardenio"].Fields == nil {
		daemon.beads["proj-gardenio"].Fields = make(map[string]string)
	}
	daemon.beads["proj-gardenio"].Fields["triggers_enabled"] = "false"
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "on gardenio",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	proj := daemon.beads["proj-gardenio"]
	daemon.mu.Unlock()
	if proj.Fields["triggers_enabled"] != "true" {
		t.Errorf("expected triggers_enabled=true, got %q", proj.Fields["triggers_enabled"])
	}
}

func TestTriggerCommand_OffProject_UnknownProject(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "off nonexistent",
		ChannelID: "C123",
		UserID:    "U456",
	})

	// Project bead should not have been created.
	daemon.mu.Lock()
	_, exists := daemon.beads["proj-nonexistent"]
	daemon.mu.Unlock()
	if exists {
		t.Error("project bead should not have been created for unknown project")
	}
}

func TestTriggerCommand_ProjectFilter_ShowsOnlyProjectTriggers(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	seedTrigger(daemon, "trig-1", "Gardenio JIRA", "gardenio", "jira", true)
	seedTrigger(daemon, "trig-2", "Monorepo GitLab", "monorepo", "gitlab", true)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Should not panic — shows filtered status for gardenio only.
	bot.handleTriggerCommand(context.Background(), slack.SlashCommand{
		Command:   "/trigger",
		Text:      "gardenio",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

func TestTriggerSourceFromBead(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   string
	}{
		{"jira", []string{"trigger:jira", "project:gardenio"}, "jira"},
		{"gitlab", []string{"trigger:gitlab"}, "gitlab"},
		{"webhook", []string{"trigger:webhook"}, "webhook"},
		{"none", []string{"project:gardenio"}, ""},
		{"empty", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := triggerSourceFromBead(&beadsapi.BeadDetail{Labels: tt.labels})
			if got != tt.want {
				t.Errorf("triggerSourceFromBead(%v) = %q, want %q", tt.labels, got, tt.want)
			}
		})
	}
}

// --- test helpers ---

func seedGate(m *mockDaemon, id, title string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "gate",
		Status: "open",
	}
}

func findGateBead(m *mockDaemon, title string) *beadsapi.BeadDetail {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b.Type == "gate" && b.Title == title && b.Status == "open" {
			return b
		}
	}
	return nil
}

func countGateBeads(m *mockDaemon, title string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, b := range m.beads {
		if b.Type == "gate" && b.Title == title && b.Status == "open" {
			n++
		}
	}
	return n
}
