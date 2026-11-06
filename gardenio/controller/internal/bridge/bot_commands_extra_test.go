package bridge

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// --- projectFromTicketPrefix tests ---

func TestProjectFromTicketPrefix_MatchesPrefix(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["proj-monorepo"] = &beadsapi.BeadDetail{
		ID:    "proj-monorepo",
		Title: "monorepo",
		Type:  "project",
		Fields: map[string]string{
			"prefix": "pe",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	got := bot.projectFromTicketPrefix(context.Background(), "PE-1234")
	if got != "monorepo" {
		t.Errorf("expected project=monorepo, got %q", got)
	}
}

func TestProjectFromTicketPrefix_NoMatch(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["proj-monorepo"] = &beadsapi.BeadDetail{
		ID:    "proj-monorepo",
		Title: "monorepo",
		Type:  "project",
		Fields: map[string]string{
			"prefix": "pe",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	got := bot.projectFromTicketPrefix(context.Background(), "DEVOPS-42")
	if got != "" {
		t.Errorf("expected empty string for unmatched prefix, got %q", got)
	}
}

func TestProjectFromTicketPrefix_NoDash(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	got := bot.projectFromTicketPrefix(context.Background(), "nodash")
	if got != "" {
		t.Errorf("expected empty string for ticket without dash, got %q", got)
	}
}

func TestProjectFromTicketPrefix_CaseInsensitive(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["proj-gardenio"] = &beadsapi.BeadDetail{
		ID:    "proj-gardenio",
		Title: "gardenio",
		Type:  "project",
		Fields: map[string]string{
			"prefix": "GB",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	got := bot.projectFromTicketPrefix(context.Background(), "gb-999")
	if got != "gardenio" {
		t.Errorf("expected project=gardenio with case-insensitive match, got %q", got)
	}
}

// --- handleDecisionsCommand tests ---

func TestHandleDecisionsCommand_NoDecisions(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Should not panic and should post ephemeral with "No pending decisions".
	bot.handleDecisionsCommand(context.Background(), slack.SlashCommand{
		Command:   "/decisions",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// If we reach here without panic, the test passes. The fake server accepts all API calls.
}

func TestHandleDecisionsCommand_WithDecisions(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["d1"] = &beadsapi.BeadDetail{
		ID:       "d1",
		Title:    "Should we deploy?",
		Type:     "decision",
		Assignee: "my-bot",
		Fields:   map[string]string{"question": "Should we deploy to prod?"},
		Labels:   []string{},
	}
	daemon.beads["d2"] = &beadsapi.BeadDetail{
		ID:       "d2",
		Title:    "Auth approach?",
		Type:     "decision",
		Assignee: "other-bot",
		Fields:   map[string]string{"question": "Which auth approach?"},
		Labels:   []string{"escalated"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleDecisionsCommand(context.Background(), slack.SlashCommand{
		Command:   "/decisions",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Success = no panic, ephemeral posted via fake server.
}

func TestHandleDecisionsCommand_TruncatesLongQuestion(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	longQ := ""
	for i := 0; i < 120; i++ {
		longQ += "x"
	}
	daemon.beads["d1"] = &beadsapi.BeadDetail{
		ID:     "d1",
		Title:  "Long decision",
		Type:   "decision",
		Fields: map[string]string{"question": longQ},
		Labels: []string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleDecisionsCommand(context.Background(), slack.SlashCommand{
		Command:   "/decisions",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

func TestHandleDecisionsCommand_FallsBackToTitle(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["d1"] = &beadsapi.BeadDetail{
		ID:     "d1",
		Title:  "Fallback title",
		Type:   "decision",
		Fields: map[string]string{}, // no "question" field
		Labels: []string{},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleDecisionsCommand(context.Background(), slack.SlashCommand{
		Command:   "/decisions",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

func TestHandleDecisionsCommand_MoreThan15(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	for i := 0; i < 20; i++ {
		id := "d" + string(rune('A'+i))
		daemon.beads[id] = &beadsapi.BeadDetail{
			ID:     id,
			Title:  "Decision " + id,
			Type:   "decision",
			Fields: map[string]string{"question": "Q?"},
			Labels: []string{},
		}
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleDecisionsCommand(context.Background(), slack.SlashCommand{
		Command:   "/decisions",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
}

// --- handleKillCommand tests ---

func TestHandleKillCommand_NoArgs(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleKillCommand(context.Background(), slack.SlashCommand{
		Command:   "/kill",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Should post usage ephemeral, no panic.
}

func TestHandleKillCommand_OnlyForceFlag(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleKillCommand(context.Background(), slack.SlashCommand{
		Command:   "/kill",
		Text:      "--force",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Should post usage ephemeral since no agent name provided.
}

func TestHandleKillCommand_KillsAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["my-bot"] = &beadsapi.BeadDetail{
		ID:         "bd-agent-mybot",
		Title:      "my-bot",
		Type:       "agent",
		OperatedBy: "oidc:alice@example.com",
		Fields:     map[string]string{"agent": "my-bot"},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.operatorResolver = func(context.Context, string) string { return "oidc:Alice@example.com" }

	bot.handleKillCommand(context.Background(), slack.SlashCommand{
		Command:   "/kill",
		Text:      "my-bot --force",
		ChannelID: "C123",
		UserID:    "U456",
	})

	// The kill runs in a goroutine. Wait briefly for it to complete.
	time.Sleep(100 * time.Millisecond)

	closed := daemon.getClosed()
	if len(closed) != 1 {
		t.Fatalf("expected 1 close call, got %d", len(closed))
	}
	if closed[0].BeadID != "bd-agent-mybot" {
		t.Errorf("expected closed bead ID=bd-agent-mybot, got %s", closed[0].BeadID)
	}
}

func TestHandleKillCommand_AgentNotFound(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleKillCommand(context.Background(), slack.SlashCommand{
		Command:   "/kill",
		Text:      "nonexistent",
		ChannelID: "C123",
		UserID:    "U456",
	})

	// The kill runs async. Wait briefly.
	time.Sleep(100 * time.Millisecond)

	closed := daemon.getClosed()
	if len(closed) != 0 {
		t.Errorf("expected no close calls for nonexistent agent, got %d", len(closed))
	}
}

// --- handleClearThreadsCommand tests ---

func TestHandleClearThreadsCommand_NilState(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.state = nil // explicitly nil

	bot.handleClearThreadsCommand(context.Background(), slack.SlashCommand{
		Command:   "/clear-threads",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Should post ephemeral about state not available, no panic.
}

func TestHandleClearThreadsCommand_WithState(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	// Create a real StateManager backed by a temp file.
	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}

	// Pre-populate some thread→agent mappings.
	_ = sm.SetThreadAgent("C123", "1234.5678", "bot-a")
	_ = sm.SetThreadAgent("C123", "2345.6789", "bot-b")
	_ = sm.SetThreadAgent("C456", "3456.7890", "bot-c")

	bot := newTestBot(daemon, slackSrv)
	bot.state = sm
	bot.lastThreadNudge = map[string]time.Time{
		"key1": time.Now(),
		"key2": time.Now(),
	}

	bot.handleClearThreadsCommand(context.Background(), slack.SlashCommand{
		Command:   "/clear-threads",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})

	// Verify thread agents were cleared.
	sm.mu.Lock()
	remaining := len(sm.data.ThreadAgents)
	sm.mu.Unlock()

	if remaining != 0 {
		t.Errorf("expected all thread mappings cleared, got %d remaining", remaining)
	}

	// Verify lastThreadNudge was cleared.
	bot.mu.Lock()
	nudgeCount := len(bot.lastThreadNudge)
	bot.mu.Unlock()

	if nudgeCount != 0 {
		t.Errorf("expected lastThreadNudge cleared, got %d entries", nudgeCount)
	}
}

// --- handleRosterCommand tests ---

func TestHandleRosterCommand_NoAgents(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleRosterCommand(context.Background(), slack.SlashCommand{
		Command:   "/roster",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Should post "No active agents" ephemeral.
}

func TestHandleRosterCommand_WithAgents(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["a1"] = &beadsapi.BeadDetail{
		ID:    "a1",
		Title: "bot-alpha",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "bot-alpha",
			"project": "gardenio",
			"role":    "crew",
			"mode":    "crew",
		},
	}
	daemon.beads["a2"] = &beadsapi.BeadDetail{
		ID:    "a2",
		Title: "bot-beta",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "bot-beta",
			"project": "monorepo",
			"role":    "gardener",
			"mode":    "crew",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleRosterCommand(context.Background(), slack.SlashCommand{
		Command:   "/roster",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Should post roster with 2 agents. No panic = success.
}

func TestHandleRosterCommand_MoreThan20Agents(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("a%d", i)
		daemon.beads[id] = &beadsapi.BeadDetail{
			ID:    id,
			Title: fmt.Sprintf("bot-%d", i),
			Type:  "agent",
			Fields: map[string]string{
				"agent": fmt.Sprintf("bot-%d", i),
				"role":  "crew",
				"mode":  "crew",
			},
		}
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleRosterCommand(context.Background(), slack.SlashCommand{
		Command:   "/roster",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Should show 20 + "...and 5 more".
}

func TestHandleRosterCommand_AgentWithNoName(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["a1"] = &beadsapi.BeadDetail{
		ID:    "a1",
		Title: "", // empty title
		Type:  "agent",
		Fields: map[string]string{
			"agent": "", // empty agent name
			"role":  "crew",
			"mode":  "crew",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.handleRosterCommand(context.Background(), slack.SlashCommand{
		Command:   "/roster",
		Text:      "",
		ChannelID: "C123",
		UserID:    "U456",
	})
	// Should fall back to ID for display.
}
