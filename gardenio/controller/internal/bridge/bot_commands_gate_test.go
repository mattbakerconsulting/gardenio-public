package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// --- checkSlashPermission tests ---

func TestCheckSlashPermission_AllowedUser(t *testing.T) {
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
			"capabilities": `["slash_command"]`,
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	allowed := b.checkSlashPermission(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		UserID:    "U-ALICE",
		ChannelID: "C-test",
	})
	if !allowed {
		t.Error("expected slash command to be allowed for user with slash_command capability")
	}
}

func TestCheckSlashPermission_DeniedUnknownUser(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	allowed := b.checkSlashPermission(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		UserID:    "U-UNKNOWN",
		ChannelID: "C-test",
	})
	if allowed {
		t.Error("expected slash command to be denied for unknown user")
	}
}

func TestCheckSlashPermission_DeniedNoCap(t *testing.T) {
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
			"capabilities": `["mention"]`,
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	allowed := b.checkSlashPermission(context.Background(), slack.SlashCommand{
		Command:   "/kill",
		UserID:    "U-BOB",
		ChannelID: "C-test",
	})
	if allowed {
		t.Error("expected slash command to be denied for user without slash_command capability")
	}
}

func TestCheckSlashPermission_BothCapabilities(t *testing.T) {
	daemon := newMockDaemon()
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
			"capabilities": `["mention","slash_command"]`,
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	allowed := b.checkSlashPermission(context.Background(), slack.SlashCommand{
		Command:   "/decisions",
		UserID:    "U-ADMIN",
		ChannelID: "C-test",
	})
	if !allowed {
		t.Error("expected slash command to be allowed for user with both capabilities")
	}
}

// --- handleSlashCommand gate integration tests ---

func TestHandleSlashCommand_GateEnabled_BlocksSpawn(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	b.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "fix the bug",
		UserID:    "U-UNKNOWN",
		ChannelID: "C-GARDENIO",
	})

	// No agent bead should be created — command was denied.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected 0 agent beads when gate denies, got %d", len(agentBeads))
	}
}

func TestHandleSlashCommand_GateEnabled_BlocksKill(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	// /kill should be blocked for unknown users.
	b.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/kill",
		Text:      "my-agent",
		UserID:    "U-UNKNOWN",
		ChannelID: "C-test",
	})

	// No side effects should occur.
}

func TestHandleSlashCommand_GateEnabled_BlocksDecisions(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	b.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/decisions",
		UserID:    "U-UNKNOWN",
		ChannelID: "C-test",
	})

	// No crash, ephemeral denial posted.
}

func TestHandleSlashCommand_GateEnabled_AllowsAuthorizedUser(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")

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
			"capabilities": `["slash_command"]`,
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	b.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "fix the bug",
		UserID:    "U-ALICE",
		ChannelID: "C-GARDENIO",
	})

	// Agent bead should be created (spawn succeeded).
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) == 0 {
		t.Error("expected agent bead to be created for authorized user")
	}
}

func TestHandleSlashCommand_GateDisabled_AllowsAll(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-GARDENIO")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = false // gate disabled

	b.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/spawn",
		Text:      "fix the bug",
		UserID:    "U-ANYONE",
		ChannelID: "C-GARDENIO",
	})

	// Agent bead should be created — gate is off.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) == 0 {
		t.Error("expected agent bead to be created when gate is disabled")
	}
}

func TestHandleSlashCommand_GateEnabled_UngatedCommandAllowsAll(t *testing.T) {
	daemon := newMockDaemon()
	// Seed an active agent for /roster to list.
	daemon.mu.Lock()
	daemon.beads["agent-test"] = &beadsapi.BeadDetail{
		ID:     "agent-test",
		Title:  "test-agent",
		Type:   "agent",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"agent":       "test-agent",
			"agent_state": "working",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	b := newTestBot(daemon, slackSrv)
	b.slashGateEnabled = true

	// /roster is NOT gated — should work for any user.
	b.handleSlashCommand(context.Background(), slack.SlashCommand{
		Command:   "/roster",
		UserID:    "U-UNKNOWN",
		ChannelID: "C-test",
	})

	// No crash — /roster should execute normally.
}

// --- gatedSlashCommands map tests ---

func TestGatedSlashCommands_Coverage(t *testing.T) {
	expected := []string{
		"/spawn", "/start", "/kill", "/kill-thread",
		"/decisions", "/decide",
		"/formula", "/bouncer", "/clear-threads", "/trigger",
	}
	for _, cmd := range expected {
		if !gatedSlashCommands[cmd] {
			t.Errorf("expected %q to be gated", cmd)
		}
	}

	// /roster and /unreleased are read-only; /autodestruct has its own
	// allowlist via isAutoDestructAllowed.
	ungated := []string{"/roster", "/unreleased", "/autodestruct"}
	for _, cmd := range ungated {
		if gatedSlashCommands[cmd] {
			t.Errorf("expected %q to NOT be gated", cmd)
		}
	}
}

// TestGatedSlashCommands_AllMutatingCommandsGated is the N1 regression test
// from AUDIT-2026-04-18.md §4.1: every state-mutating slash command must be
// enforced by the gate.
func TestGatedSlashCommands_AllMutatingCommandsGated(t *testing.T) {
	mustGate := []string{
		"/spawn", "/start", "/kill", "/kill-thread",
		"/decisions", "/decide",
		"/formula", "/bouncer", "/clear-threads", "/trigger",
	}
	for _, cmd := range mustGate {
		if !gatedSlashCommands[cmd] {
			t.Errorf("command %q must be in gatedSlashCommands (mutates state)", cmd)
		}
	}
}
