package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- getAgentThreadTS tests ---

func TestGetAgentThreadTS_Found(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.agentCards["test-agent"] = MessageRef{ChannelID: "C-123", Timestamp: "1234.5678"}

	ts := bot.getAgentThreadTS("test-agent")
	if ts != "1234.5678" {
		t.Errorf("expected 1234.5678, got %s", ts)
	}
}

func TestGetAgentThreadTS_NotFound(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)

	ts := bot.getAgentThreadTS("nonexistent")
	if ts != "" {
		t.Errorf("expected empty string, got %s", ts)
	}
}

// --- NotifyBeadCreated tests ---

func TestNotifyBeadCreated_PostsToAgentThread(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["test-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "1000.0001"}
	enableVibeForAgent(daemon, "test-agent") // full-verbosity path

	bead := BeadEvent{
		ID:        "bd-task-1",
		Type:      "task",
		Title:     "Fix the bug",
		CreatedBy: "gardenio/crew/test-agent",
	}

	bot.NotifyBeadCreated(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if got[0].Method != "chat.postMessage" {
		t.Errorf("expected chat.postMessage, got %s", got[0].Method)
	}
	if got[0].ThreadTS != "1000.0001" {
		t.Errorf("expected thread_ts 1000.0001, got %s", got[0].ThreadTS)
	}
	if !strings.Contains(got[0].Text, "Created task") {
		t.Errorf("expected text to contain 'Created task', got %s", got[0].Text)
	}
	if !strings.Contains(got[0].Text, "Fix the bug") {
		t.Errorf("expected text to contain bead title, got %s", got[0].Text)
	}
}

func TestNotifyBeadCreated_NoAgent_Noop(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)

	bead := BeadEvent{
		ID:        "bd-task-2",
		Type:      "task",
		Title:     "Orphan task",
		CreatedBy: "",
	}

	bot.NotifyBeadCreated(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 0 {
		t.Errorf("expected no Slack calls for empty agent, got %d", len(got))
	}
}

func TestNotifyBeadCreated_RecordsActivity(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["test-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "1000.0001"}

	before := time.Now()
	bot.NotifyBeadCreated(context.Background(), BeadEvent{
		ID:        "bd-task-3",
		Type:      "task",
		Title:     "Test",
		CreatedBy: "test-agent",
	})

	bot.mu.Lock()
	seen, ok := bot.agentSeen["test-agent"]
	bot.mu.Unlock()

	if !ok {
		t.Fatal("expected agentSeen entry for test-agent")
	}
	if seen.Before(before) {
		t.Error("expected agentSeen to be updated")
	}
}

func TestNotifyBeadCreated_ThreadBoundAgent(t *testing.T) {
	daemon := newMockDaemon()
	// Seed agent bead with thread binding.
	daemon.mu.Lock()
	daemon.beads["thread-creator"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-tc",
		Title: "thread-creator",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-creator",
			"slack_thread_channel": "C-bound",
			"slack_thread_ts":      "8888.0001",
		},
	}
	daemon.mu.Unlock()

	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-default"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	enableVibeForAgent(daemon, "thread-creator") // stamps operated_by on the seeded agent bead

	bead := BeadEvent{
		ID:        "bd-task-tc",
		Type:      "task",
		Title:     "Thread task",
		CreatedBy: "thread-creator",
	}

	bot.NotifyBeadCreated(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call, got %d", len(got))
	}
	if got[0].Channel != "C-bound" {
		t.Errorf("expected channel C-bound, got %s", got[0].Channel)
	}
	if got[0].ThreadTS != "8888.0001" {
		t.Errorf("expected thread_ts 8888.0001, got %s", got[0].ThreadTS)
	}
}

func TestNotifyBeadCreated_NoThreading_Dropped(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "" // flat mode
	bot.beadMsgs = make(map[string]MessageRef)

	bead := BeadEvent{
		ID:        "bd-task-flat",
		Type:      "task",
		Title:     "Flat mode task",
		CreatedBy: "some-agent",
	}

	bot.NotifyBeadCreated(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 0 {
		t.Errorf("expected no calls in flat mode without agent card, got %d", len(got))
	}
}

// --- NotifyBeadClaimed tests ---

func TestNotifyBeadClaimed_PostsToAgentThread(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["claim-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "2000.0001"}
	enableVibeForAgent(daemon, "claim-agent") // full-verbosity path

	bead := BeadEvent{
		ID:       "bd-claim-1",
		Type:     "task",
		Title:    "Important task",
		Assignee: "gardenio/crew/claim-agent",
	}

	bot.NotifyBeadClaimed(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "Claimed task") {
		t.Errorf("expected text to contain 'Claimed task', got %s", got[0].Text)
	}
	if !strings.Contains(got[0].Text, "Important task") {
		t.Errorf("expected text to contain title, got %s", got[0].Text)
	}
	if got[0].ThreadTS != "2000.0001" {
		t.Errorf("expected thread_ts 2000.0001, got %s", got[0].ThreadTS)
	}
}

func TestNotifyBeadClaimed_NoAssignee_Noop(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)

	bead := BeadEvent{
		ID:   "bd-claim-2",
		Type: "task",
	}

	bot.NotifyBeadClaimed(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 0 {
		t.Errorf("expected no calls for empty assignee, got %d", len(got))
	}
}

func TestNotifyBeadClaimed_RecordsActivity(t *testing.T) {
	daemon := newMockDaemon()
	srv := newFakeSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["activity-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "3000.0001"}

	before := time.Now()
	bot.NotifyBeadClaimed(context.Background(), BeadEvent{
		ID:       "bd-claim-act",
		Type:     "task",
		Title:    "Activity test",
		Assignee: "activity-agent",
	})

	bot.mu.Lock()
	seen, ok := bot.agentSeen["activity-agent"]
	bot.mu.Unlock()

	if !ok {
		t.Fatal("expected agentSeen entry for activity-agent")
	}
	if seen.Before(before) {
		t.Error("expected agentSeen to be updated")
	}
}

// --- NotifyBeadCreatedAndClaimed tests ---

func TestNotifyBeadCreatedAndClaimed_PostsToAgentThread(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["combo-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "4000.0001"}
	enableVibeForAgent(daemon, "combo-agent") // full-verbosity path

	bead := BeadEvent{
		ID:        "bd-combo-1",
		Type:      "task",
		Title:     "Self-assigned task",
		Assignee:  "combo-agent",
		CreatedBy: "combo-agent",
	}

	bot.NotifyBeadCreatedAndClaimed(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "Created & claimed task") {
		t.Errorf("expected text to contain 'Created & claimed task', got %s", got[0].Text)
	}
	if got[0].ThreadTS != "4000.0001" {
		t.Errorf("expected thread_ts 4000.0001, got %s", got[0].ThreadTS)
	}
}

func TestNotifyBeadCreatedAndClaimed_FallsBackToCreatedBy(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["fallback-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "5000.0001"}
	enableVibeForAgent(daemon, "fallback-agent") // full-verbosity path

	bead := BeadEvent{
		ID:        "bd-combo-2",
		Type:      "task",
		Title:     "Fallback task",
		Assignee:  "",
		CreatedBy: "fallback-agent",
	}

	bot.NotifyBeadCreatedAndClaimed(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
}

func TestNotifyBeadCreatedAndClaimed_NoAgent_Noop(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)

	bead := BeadEvent{
		ID:   "bd-combo-3",
		Type: "task",
	}

	bot.NotifyBeadCreatedAndClaimed(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 0 {
		t.Errorf("expected no calls for empty agent, got %d", len(got))
	}
}

func TestNotifyBeadCreatedAndClaimed_TruncatesTitle(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["trunc-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "6000.0001"}
	enableVibeForAgent(daemon, "trunc-agent") // full-verbosity path

	// Title longer than 60 chars should be truncated.
	longTitle := strings.Repeat("A", 100)

	bead := BeadEvent{
		ID:       "bd-combo-trunc",
		Type:     "task",
		Title:    longTitle,
		Assignee: "trunc-agent",
	}

	bot.NotifyBeadCreatedAndClaimed(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	// The title should have been truncated to 60 chars with "..."
	if strings.Contains(got[0].Text, longTitle) {
		t.Error("expected title to be truncated, but full title was found")
	}
	if !strings.Contains(got[0].Text, "...") {
		t.Error("expected truncation marker '...' in text")
	}
}

// --- postOrUpdateBeadMessage (update path) tests ---

// --- regression: bead-ID linking via beadTitleLink ---

func TestNotifyBeadActivity_LinksBeadTitleWhenViewerURLSet(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadsViewerURL = "https://viewer.example"
	bot.beadMsgs = make(map[string]MessageRef)
	bot.agentCards["link-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "9000.0001"}
	enableVibeForAgent(daemon, "link-agent") // full-verbosity path

	cases := []struct {
		name  string
		call  func(BeadEvent)
		bead  BeadEvent
		phase string
	}{
		{
			name:  "created",
			call:  func(b BeadEvent) { bot.NotifyBeadCreated(context.Background(), b) },
			bead:  BeadEvent{ID: "gd-linktest1", Type: "task", Title: "Link me", CreatedBy: "link-agent"},
			phase: "Created",
		},
		{
			name:  "claimed",
			call:  func(b BeadEvent) { bot.NotifyBeadClaimed(context.Background(), b) },
			bead:  BeadEvent{ID: "gd-linktest2", Type: "task", Title: "Link me too", Assignee: "link-agent"},
			phase: "Claimed",
		},
		{
			name:  "created_and_claimed",
			call:  func(b BeadEvent) { bot.NotifyBeadCreatedAndClaimed(context.Background(), b) },
			bead:  BeadEvent{ID: "gd-linktest3", Type: "task", Title: "Combo link", Assignee: "link-agent"},
			phase: "Created & claimed",
		},
		{
			name:  "closed",
			call:  func(b BeadEvent) { bot.NotifyBeadClosed(context.Background(), b) },
			bead:  BeadEvent{ID: "gd-linktest4", Type: "task", Title: "Closed link", Assignee: "link-agent"},
			phase: "Closed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Reset calls between sub-tests.
			mu.Lock()
			*calls = (*calls)[:0]
			mu.Unlock()

			tc.call(tc.bead)

			got := getCalls(calls, mu)
			if len(got) == 0 {
				t.Fatalf("expected at least one Slack call")
			}
			expected := "<https://viewer.example/" + tc.bead.ID + "|" + tc.bead.Title + ">"
			if !strings.Contains(got[0].Text, expected) {
				t.Errorf("expected text to contain link %q, got %q", expected, got[0].Text)
			}
			if !strings.Contains(got[0].Text, tc.phase) {
				t.Errorf("expected text to contain %q, got %q", tc.phase, got[0].Text)
			}
		})
	}
}

func TestPostOrUpdateBeadMessage_UpdatesExisting(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.beadMsgs = make(map[string]MessageRef)

	// Pre-populate a tracked bead message.
	bot.beadMsgs["update-agent:bd-update-1"] = MessageRef{
		ChannelID: "C-test",
		Timestamp: "7000.0001",
	}

	bot.agentCards["update-agent"] = MessageRef{ChannelID: "C-test", Timestamp: "7000.0000"}
	enableVibeForAgent(daemon, "update-agent") // full-verbosity path

	// Claiming a bead that was already created should update in-place.
	bead := BeadEvent{
		ID:       "bd-update-1",
		Type:     "task",
		Title:    "Updated task",
		Assignee: "update-agent",
	}

	bot.NotifyBeadClaimed(context.Background(), bead)

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 call (update), got %d", len(got))
	}
	if got[0].Method != "chat.update" {
		t.Errorf("expected chat.update for in-place update, got %s", got[0].Method)
	}
}
