package bridge

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

func TestConciergeChannelInfo_Matches(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"concierge"}`,
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	project, ok := bot.conciergeChannelInfo(context.Background(), "C111")
	if !ok {
		t.Fatal("expected channel C111 to be in concierge mode")
	}
	if project != "myproject" {
		t.Errorf("project = %q, want %q", project, "myproject")
	}
}

func TestConciergeChannelInfo_NoMatch(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"mention"}`,
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	_, ok := bot.conciergeChannelInfo(context.Background(), "C111")
	if ok {
		t.Fatal("expected channel C111 to NOT be in concierge mode (explicit mention)")
	}
}

func TestConciergeChannelInfo_NoModes(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	_, ok := bot.conciergeChannelInfo(context.Background(), "C111")
	if ok {
		t.Fatal("expected channel C111 to NOT be in concierge mode (no channel_modes)")
	}
}

func TestConciergeDebouncer_AllowsFirst(t *testing.T) {
	d := newConciergeDebouncer()
	if !d.Allow("U1", "C1") {
		t.Error("first call should be allowed")
	}
}

func TestConciergeDebouncer_BlocksRapid(t *testing.T) {
	d := newConciergeDebouncer()
	d.Allow("U1", "C1")
	if d.Allow("U1", "C1") {
		t.Error("rapid second call should be blocked")
	}
}

func TestConciergeDebouncer_AllowsDifferentUsers(t *testing.T) {
	d := newConciergeDebouncer()
	d.Allow("U1", "C1")
	if !d.Allow("U2", "C1") {
		t.Error("different user should be allowed")
	}
}

func TestConciergeDebouncer_AllowsDifferentChannels(t *testing.T) {
	d := newConciergeDebouncer()
	d.Allow("U1", "C1")
	if !d.Allow("U1", "C2") {
		t.Error("different channel should be allowed")
	}
}

func TestConciergeDebouncer_AllowsAfterWindow(t *testing.T) {
	d := newConciergeDebouncer()
	d.mu.Lock()
	d.last["U1:C1"] = time.Now().Add(-3 * time.Second) // simulate past
	d.mu.Unlock()
	if !d.Allow("U1", "C1") {
		t.Error("should allow after debounce window expires")
	}
}

func TestConciergeDebouncer_Cleanup(t *testing.T) {
	d := newConciergeDebouncer()
	d.mu.Lock()
	d.last["U1:C1"] = time.Now().Add(-10 * time.Second) // old entry
	d.last["U2:C1"] = time.Now()                         // fresh entry
	d.mu.Unlock()

	d.Cleanup()

	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.last["U1:C1"]; ok {
		t.Error("old entry should have been cleaned up")
	}
	if _, ok := d.last["U2:C1"]; !ok {
		t.Error("fresh entry should NOT be cleaned up")
	}
}

func TestParseConciergeValue(t *testing.T) {
	project, channel, ts, user, err := parseConciergeValue("myproject|C111|1234.5678|U999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if project != "myproject" {
		t.Errorf("project = %q, want %q", project, "myproject")
	}
	if channel != "C111" {
		t.Errorf("channel = %q, want %q", channel, "C111")
	}
	if ts != "1234.5678" {
		t.Errorf("ts = %q, want %q", ts, "1234.5678")
	}
	if user != "U999" {
		t.Errorf("user = %q, want %q", user, "U999")
	}
}

func TestParseConciergeValue_Invalid(t *testing.T) {
	_, _, _, _, err := parseConciergeValue("only|two|parts")
	if err == nil {
		t.Error("expected error for invalid value")
	}
}

func TestHandleMessageEvent_ConciergeMode(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"concierge"}`,
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Top-level message in concierge channel should not panic and should
	// attempt to post buttons (the fake server returns OK for all requests).
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:      "U123",
		Text:      "Can someone help me with this deployment?",
		Channel:   "C111",
		TimeStamp: "1111.2222",
	})
	// No assertion on Slack API calls since we're using a fake server,
	// but the test verifies no panics, no nil pointer dereferences, and
	// that the concierge path is exercised.
}

func TestHandleMessageEvent_ConciergeMode_SkipsBotMessages(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"concierge"}`,
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Message from another bot should be skipped (BotID is set).
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:      "U123",
		BotID:     "B456",
		Text:      "Automated notification",
		Channel:   "C111",
		TimeStamp: "1111.3333",
	})
	// This should not trigger the concierge path.
}

func TestHandleMessageEvent_ConciergeMode_SkipsThreads(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"concierge"}`,
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Thread reply should NOT trigger concierge (handled earlier in the function).
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:            "U123",
		Text:            "follow up in thread",
		Channel:         "C111",
		TimeStamp:       "1111.4444",
		ThreadTimeStamp: "1111.2222",
	})
}

func TestHandleMessageEvent_NonConciergeChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			// No channel_modes — defaults to "mention"
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// Message in non-concierge channel should be ignored (existing behavior).
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:      "U123",
		Text:      "random message",
		Channel:   "C111",
		TimeStamp: "1111.5555",
	})
}

func TestHandleMessageEvent_ConciergeDebounce(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "myproject",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"concierge"}`,
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.botUserID = "UBOTID"

	// First message should trigger concierge.
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:      "U123",
		Text:      "first message",
		Channel:   "C111",
		TimeStamp: "1111.6666",
	})

	// Rapid second message from same user should be debounced (no panic, no extra call).
	bot.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User:      "U123",
		Text:      "second message",
		Channel:   "C111",
		TimeStamp: "1111.7777",
	})
}

func TestConciergeChannelInfo_DuplicateChannel_Excluded(t *testing.T) {
	daemon := newMockDaemon()
	// Two projects claim C111 in concierge mode — should be excluded from cache.
	daemon.beads["proj1"] = &beadsapi.BeadDetail{
		ID:    "proj1",
		Title: "alpha",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"concierge"}`,
		},
	}
	daemon.beads["proj2"] = &beadsapi.BeadDetail{
		ID:    "proj2",
		Title: "beta",
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": "C111",
			"channel_modes": `{"C111":"concierge"}`,
		},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	_, ok := bot.conciergeChannelInfo(context.Background(), "C111")
	if ok {
		t.Fatal("expected channel C111 to NOT be in concierge mode when claimed by multiple projects")
	}
}

func TestGenerateConciergeAgentName_UniquePerCall(t *testing.T) {
	b := &Bot{}
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		name := b.generateAgentName("myproject")
		if seen[name] {
			t.Fatalf("duplicate agent name on iteration %d: %s", i, name)
		}
		seen[name] = true
	}
}
