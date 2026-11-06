package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// seedGardener pre-populates the mock with a gardener trigger bead (a
// trigger:schedule bead carrying role:gardener).
func seedGardener(m *mockDaemon, id, title, project string, enabled bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	enabledStr := "true"
	if !enabled {
		enabledStr = "false"
	}
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener"},
		Fields: map[string]string{
			"enabled": enabledStr,
			"project": project,
			"role":    "gardener",
		},
	}
}

// TestScheduleCommand_Off_OpensBothGates verifies `/schedule off` opens BOTH
// the scheduler-pause gate (cron sources) and the gardeners-pause gate (gardener
// loops), and does not touch individual trigger/gardener enabled states.
func TestScheduleCommand_Off_OpensBothGates(t *testing.T) {
	daemon := newMockDaemon()
	seedTrigger(daemon, "sched-1", "Nightly triage", "gardenio", "schedule", true)
	seedGardener(daemon, "cap-1", "Delivery gardener", "gardenio", true)
	seedTrigger(daemon, "jira-1", "JIRA bug", "gardenio", "jira", true)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "off",
		ChannelID: "C123",
		UserID:    "U456",
	})

	if findGateBead(daemon, schedulerPauseTitleForTriggers) == nil {
		t.Error("expected scheduler-pause gate bead to be created")
	}
	if findGateBead(daemon, gardenersPauseTitle) == nil {
		t.Error("expected gardeners-pause gate bead to be created")
	}

	// Individual beads must be untouched so `/schedule on` restores exactly.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if daemon.beads["sched-1"].Fields["enabled"] != "true" {
		t.Error("sched-1 enabled field should not have changed")
	}
	if daemon.beads["cap-1"].Fields["enabled"] != "true" {
		t.Error("cap-1 enabled field should not have changed")
	}
	if daemon.beads["jira-1"].Fields["enabled"] != "true" {
		t.Error("jira-1 (event source) enabled field should not have changed")
	}
}

// TestScheduleCommand_On_ClosesBothGates verifies `/schedule on` closes both
// gates.
func TestScheduleCommand_On_ClosesBothGates(t *testing.T) {
	daemon := newMockDaemon()
	seedGate(daemon, "sched-gate", schedulerPauseTitleForTriggers)
	seedGate(daemon, "cap-gate", gardenersPauseTitle)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "on",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if daemon.beads["sched-gate"].Status != "closed" {
		t.Errorf("scheduler-pause gate: expected closed, got %q", daemon.beads["sched-gate"].Status)
	}
	if daemon.beads["cap-gate"].Status != "closed" {
		t.Errorf("gardeners-pause gate: expected closed, got %q", daemon.beads["cap-gate"].Status)
	}
}

// TestScheduleCommand_Off_Idempotent verifies a second `/schedule off` does not
// create duplicate gate beads.
func TestScheduleCommand_Off_Idempotent(t *testing.T) {
	daemon := newMockDaemon()
	seedGate(daemon, "sched-gate", schedulerPauseTitleForTriggers)
	seedGate(daemon, "cap-gate", gardenersPauseTitle)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "off",
		ChannelID: "C123",
		UserID:    "U456",
	})

	if n := countGateBeads(daemon, schedulerPauseTitleForTriggers); n != 1 {
		t.Errorf("expected 1 scheduler-pause gate, got %d", n)
	}
	if n := countGateBeads(daemon, gardenersPauseTitle); n != 1 {
		t.Errorf("expected 1 gardeners-pause gate, got %d", n)
	}
}

// TestScheduleCommand_Off_OpensMissingGateOnly verifies that when one gate is
// already open, `/schedule off` opens only the missing one (self-heals a
// partially-engaged kill switch).
func TestScheduleCommand_Off_OpensMissingGateOnly(t *testing.T) {
	daemon := newMockDaemon()
	seedGate(daemon, "sched-gate", schedulerPauseTitleForTriggers)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "off",
		ChannelID: "C123",
		UserID:    "U456",
	})

	if n := countGateBeads(daemon, schedulerPauseTitleForTriggers); n != 1 {
		t.Errorf("expected 1 scheduler-pause gate (unchanged), got %d", n)
	}
	if n := countGateBeads(daemon, gardenersPauseTitle); n != 1 {
		t.Errorf("expected gardeners-pause gate to be created, got %d", n)
	}
}

// TestScheduleCommand_On_AlreadyRunning is a no-op when no gates are open.
func TestScheduleCommand_On_AlreadyRunning(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "on",
		ChannelID: "C123",
		UserID:    "U456",
	})

	if countGateBeads(daemon, schedulerPauseTitleForTriggers) != 0 {
		t.Error("no scheduler-pause gate should exist")
	}
	if countGateBeads(daemon, gardenersPauseTitle) != 0 {
		t.Error("no gardeners-pause gate should exist")
	}
}

// TestScheduleCommand_OffProject_DisablesProjectSchedules verifies the
// per-project control sets schedules_enabled=false and records an audit
// comment, without opening the global gates.
func TestScheduleCommand_OffProject_DisablesProjectSchedules(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	seedTrigger(daemon, "sched-1", "Nightly triage", "gardenio", "schedule", true)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "off gardenio",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	proj := daemon.beads["proj-gardenio"]
	var hasAudit bool
	for _, c := range daemon.comments {
		if c.BeadID == "proj-gardenio" && c.Author == "U456" {
			hasAudit = true
			break
		}
	}
	daemon.mu.Unlock()

	if proj.Fields["schedules_enabled"] != "false" {
		t.Errorf("expected schedules_enabled=false, got %q", proj.Fields["schedules_enabled"])
	}
	if !hasAudit {
		t.Error("expected audit comment on project bead")
	}
	// Global gates must NOT be opened by a per-project off.
	if countGateBeads(daemon, schedulerPauseTitleForTriggers) != 0 {
		t.Error("per-project off should not open the global scheduler-pause gate")
	}
	if countGateBeads(daemon, gardenersPauseTitle) != 0 {
		t.Error("per-project off should not open the global gardeners-pause gate")
	}
}

// TestScheduleCommand_OnProject_EnablesProjectSchedules verifies re-enabling.
func TestScheduleCommand_OnProject_EnablesProjectSchedules(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	daemon.mu.Lock()
	if daemon.beads["proj-gardenio"].Fields == nil {
		daemon.beads["proj-gardenio"].Fields = make(map[string]string)
	}
	daemon.beads["proj-gardenio"].Fields["schedules_enabled"] = "false"
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "on gardenio",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	proj := daemon.beads["proj-gardenio"]
	daemon.mu.Unlock()
	if proj.Fields["schedules_enabled"] != "true" {
		t.Errorf("expected schedules_enabled=true, got %q", proj.Fields["schedules_enabled"])
	}
}

// TestScheduleCommand_OffProject_UnknownProject does not create a bead.
func TestScheduleCommand_OffProject_UnknownProject(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "off nonexistent",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	_, exists := daemon.beads["proj-nonexistent"]
	daemon.mu.Unlock()
	if exists {
		t.Error("project bead should not have been created for unknown project")
	}
}

// TestScheduleCommand_NoArg_ShowsStatus does not panic and does not mutate.
func TestScheduleCommand_NoArg_ShowsStatus(t *testing.T) {
	daemon := newMockDaemon()
	seedTrigger(daemon, "sched-1", "Nightly triage", "gardenio", "schedule", true)
	seedGardener(daemon, "cap-1", "Delivery gardener", "gardenio", true)
	seedTrigger(daemon, "jira-1", "JIRA bug", "gardenio", "jira", true)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})

	daemon.mu.Lock()
	stillEnabled := daemon.beads["sched-1"].Fields["enabled"] == "true"
	daemon.mu.Unlock()
	if !stillEnabled {
		t.Error("sched-1 should remain enabled after status view")
	}
	// countGateBeads takes daemon.mu itself — call it AFTER releasing the lock.
	if countGateBeads(daemon, schedulerPauseTitleForTriggers) != 0 {
		t.Error("status view must not open any gate")
	}
}

// TestScheduleCommand_NoSchedules handles an empty fleet without panicking.
func TestScheduleCommand_NoSchedules(t *testing.T) {
	daemon := newMockDaemon()
	// Only an event-driven trigger — nothing this command gates.
	seedTrigger(daemon, "jira-1", "JIRA bug", "gardenio", "jira", true)

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleScheduleCommand(context.Background(), slack.SlashCommand{
		Command:   "/schedule",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

// TestBeadIsGardenerBead covers the gardener-identification helper.
func TestBeadIsGardenerBead(t *testing.T) {
	tests := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"role field", &beadsapi.BeadDetail{Fields: map[string]string{"role": "gardener"}}, true},
		{"role label", &beadsapi.BeadDetail{Labels: []string{"role:gardener"}}, true},
		{"gardener blob", &beadsapi.BeadDetail{Fields: map[string]string{"gardener": "{...}"}}, true},
		{"plain schedule", &beadsapi.BeadDetail{Labels: []string{"trigger:schedule"}, Fields: map[string]string{"enabled": "true"}}, false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := beadIsGardenerBead(tt.bead); got != tt.want {
				t.Errorf("beadIsGardenerBead() = %v, want %v", got, tt.want)
			}
		})
	}
}
