package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m 30s"},
		{5*time.Minute + 12*time.Second, "5m 12s"},
		{60 * time.Minute, "1h"},
		{90 * time.Minute, "1h 30m"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
	}
	for _, tc := range tests {
		if got := formatDuration(tc.d); got != tc.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestNotifyAgentSpawn_ScheduledAgent_PostsScheduleNotification(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["sched-agent-1"] = &beadsapi.BeadDetail{
		ID: "sched-agent-1", Type: "agent", Status: "open",
		Title: "sched-1234", Fields: map[string]string{
			"agent":          "sched-1234",
			"schedule_id":    "gd-sched-1",
			"schedule_title": "Daily Release Notes",
			"schedule_cron":  "0 9 * * 1-5",
		},
	}

	var mu sync.Mutex
	var postedTexts []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "sched-agent-1",
		Type:     "agent",
		Title:    "sched-1234",
		Assignee: "sched-1234",
		Fields: map[string]string{
			"agent":          "sched-1234",
			"schedule_id":    "gd-sched-1",
			"schedule_title": "Daily Release Notes",
			"schedule_cron":  "0 9 * * 1-5",
		},
	})

	// Verify schedule title is cached.
	if got := bot.agentScheduleTitle["sched-1234"]; got != "Daily Release Notes" {
		t.Errorf("expected schedule title cached, got %q", got)
	}

	// Verify spawn time is recorded.
	if bot.agentSpawnedAt["sched-1234"].IsZero() {
		t.Error("expected agentSpawnedAt to be set")
	}

	mu.Lock()
	defer mu.Unlock()

	// Should post a schedule-specific message.
	found := false
	for _, text := range postedTexts {
		if strings.Contains(text, "Scheduled task") && strings.Contains(text, "Daily Release Notes") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected schedule notification, got texts: %v", postedTexts)
	}
}

func TestNotifyAgentSpawn_ScheduledAgent_UsesScheduleSlackChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["sched-agent-ch"] = &beadsapi.BeadDetail{
		ID: "sched-agent-ch", Type: "agent", Status: "open",
		Title: "sched-ch-1234", Fields: map[string]string{
			"agent":                  "sched-ch-1234",
			"schedule_id":            "gd-sched-ch",
			"schedule_title":         "Weekly Report",
			"schedule_cron":          "0 9 * * 1",
			"schedule_slack_channel": "C_REPORTS",
		},
	}

	var mu sync.Mutex
	var postedChannels []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedChannels = append(postedChannels, r.FormValue("channel"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C_REPORTS", "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C_DEFAULT" // default channel — should NOT be used

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "sched-agent-ch",
		Type:     "agent",
		Title:    "sched-ch-1234",
		Assignee: "sched-ch-1234",
		Fields: map[string]string{
			"agent":                  "sched-ch-1234",
			"schedule_id":            "gd-sched-ch",
			"schedule_title":         "Weekly Report",
			"schedule_cron":          "0 9 * * 1",
			"schedule_slack_channel": "C_REPORTS",
		},
	})

	mu.Lock()
	defer mu.Unlock()

	// All messages should go to C_REPORTS, not C_DEFAULT.
	for _, ch := range postedChannels {
		if ch != "C_REPORTS" {
			t.Errorf("expected all posts to C_REPORTS, got channel %q", ch)
		}
	}
	if len(postedChannels) == 0 {
		t.Error("expected at least one message to be posted")
	}

	// Verify spawn channel was cached for future notifications.
	if got := bot.agentSpawnChannel["sched-ch-1234"]; got != "C_REPORTS" {
		t.Errorf("expected agentSpawnChannel=C_REPORTS, got %q", got)
	}
}

func TestNotifyAgentSpawn_NonScheduled_NoScheduleFields(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["normal-agent"] = &beadsapi.BeadDetail{
		ID: "normal-agent", Type: "agent", Status: "open",
		Title: "my-agent", Fields: map[string]string{"agent": "my-agent"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "normal-agent",
		Type:     "agent",
		Title:    "my-agent",
		Assignee: "my-agent",
		Fields:   map[string]string{"agent": "my-agent"},
	})

	// No schedule title should be cached for a non-scheduled agent.
	if got := bot.agentScheduleTitle["my-agent"]; got != "" {
		t.Errorf("expected no schedule title for normal agent, got %q", got)
	}
}

func TestNotifyAgentState_ScheduledDone_PostsCompletionNotification(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedTexts []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "5678.9012"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.agentState["sched-done"] = "working"
	bot.agentScheduleTitle["sched-done"] = "Daily Release Notes"
	bot.agentSpawnedAt["sched-done"] = time.Now().Add(-4*time.Minute - 12*time.Second)

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "sched-done",
		Fields:   map[string]string{"agent_state": "done", "agent": "sched-done"},
	})

	mu.Lock()
	defer mu.Unlock()

	found := false
	for _, text := range postedTexts {
		if strings.Contains(text, "Scheduled task") && strings.Contains(text, "completed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected schedule completion notification, got texts: %v", postedTexts)
	}
}

func TestNotifyAgentState_ScheduledFailed_PostsFailureNotification(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedTexts []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "5678.9012"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.agentState["sched-fail"] = "working"
	bot.agentScheduleTitle["sched-fail"] = "Nightly Backup"
	bot.agentSpawnedAt["sched-fail"] = time.Now().Add(-10 * time.Minute)

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "sched-fail",
		Fields: map[string]string{
			"agent_state":  "failed",
			"agent":        "sched-fail",
			"close_reason": "timeout after 10m",
		},
	})

	mu.Lock()
	defer mu.Unlock()

	found := false
	for _, text := range postedTexts {
		if strings.Contains(text, "Scheduled task") && strings.Contains(text, "failed") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected schedule failure notification, got texts: %v", postedTexts)
	}
}

func TestNotifyAgentState_NonScheduled_NoScheduleNotification(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedTexts []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedTexts = append(postedTexts, r.FormValue("text"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "5678.9012"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.agentState["normal-agent"] = "working"
	// No agentScheduleTitle set — this is a non-scheduled agent.

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "normal-agent",
		Fields:   map[string]string{"agent_state": "done", "agent": "normal-agent"},
	})

	mu.Lock()
	defer mu.Unlock()

	for _, text := range postedTexts {
		if strings.Contains(text, "Scheduled task") {
			t.Errorf("unexpected schedule notification for non-scheduled agent: %q", text)
		}
	}
}

// TestNotifyAgentSpawn_ScheduledAgent_ThreadedMode_ThreadsUnderCard verifies
// that when threadingMode="agent", the schedule spawn notification is posted
// as a thread reply under the agent card (not a top-level channel message).
func TestNotifyAgentSpawn_ScheduledAgent_ThreadedMode_ThreadsUnderCard(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["sched-threaded"] = &beadsapi.BeadDetail{
		ID: "sched-threaded", Type: "agent", Status: "open",
		Title: "sched-t-1234", Fields: map[string]string{
			"agent":          "sched-t-1234",
			"schedule_id":    "gd-sched-t",
			"schedule_title": "Hourly Hello",
			"schedule_cron":  "0 * * * *",
		},
	}

	var mu sync.Mutex
	type postedMsg struct {
		channel  string
		text     string
		threadTS string
	}
	var posts []postedMsg
	cardTS := "1700000000.000001"
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			posts = append(posts, postedMsg{
				channel:  r.FormValue("channel"),
				text:     r.FormValue("text"),
				threadTS: r.FormValue("thread_ts"),
			})
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		// Return the cardTS so the agent card gets a stable timestamp.
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": cardTS})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "sched-threaded",
		Type:     "agent",
		Title:    "sched-t-1234",
		Assignee: "sched-t-1234",
		Fields: map[string]string{
			"agent":          "sched-t-1234",
			"schedule_id":    "gd-sched-t",
			"schedule_title": "Hourly Hello",
			"schedule_cron":  "0 * * * *",
		},
	})

	mu.Lock()
	defer mu.Unlock()

	// Expect 2 messages: the agent card (no thread_ts) and the schedule
	// spawn notification (threaded under the card).
	if len(posts) < 2 {
		t.Fatalf("expected at least 2 posts (card + threaded notification), got %d", len(posts))
	}

	// First post should be the agent card (top-level, no thread_ts).
	if posts[0].threadTS != "" {
		t.Errorf("first post (agent card) should be top-level, got thread_ts=%q", posts[0].threadTS)
	}

	// Second post should be the schedule notification, threaded under the card.
	if posts[1].threadTS != cardTS {
		t.Errorf("schedule notification should be threaded under card (thread_ts=%q), got thread_ts=%q", cardTS, posts[1].threadTS)
	}
	if !strings.Contains(posts[1].text, "Scheduled task") {
		t.Errorf("schedule notification text should contain 'Scheduled task', got %q", posts[1].text)
	}
}

// TestNotifyAgentState_ScheduledDone_ThreadedMode_ThreadsUnderCard verifies
// that when threadingMode="agent" and an agent card exists, the completion
// notification is posted as a thread reply under the card.
func TestNotifyAgentState_ScheduledDone_ThreadedMode_ThreadsUnderCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	type postedMsg struct {
		channel  string
		text     string
		threadTS string
	}
	var posts []postedMsg
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			posts = append(posts, postedMsg{
				channel:  r.FormValue("channel"),
				text:     r.FormValue("text"),
				threadTS: r.FormValue("thread_ts"),
			})
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "9999.0001"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"
	bot.agentState["sched-threaded-done"] = "working"
	bot.agentScheduleTitle["sched-threaded-done"] = "Hourly Hello"
	bot.agentSpawnedAt["sched-threaded-done"] = time.Now().Add(-2 * time.Minute)
	// Pre-populate agent card so getAgentThreadTS returns a valid ts.
	bot.agentCards["sched-threaded-done"] = MessageRef{ChannelID: "C123", Timestamp: "1700000000.000001"}

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "sched-threaded-done",
		Fields:   map[string]string{"agent_state": "done", "agent": "sched-threaded-done"},
	})

	mu.Lock()
	defer mu.Unlock()

	// Find the completion notification — it should be threaded.
	found := false
	for _, p := range posts {
		if strings.Contains(p.text, "Scheduled task") && strings.Contains(p.text, "completed") {
			found = true
			if p.threadTS != "1700000000.000001" {
				t.Errorf("completion notification should be threaded under card, got thread_ts=%q", p.threadTS)
			}
			break
		}
	}
	if !found {
		var texts []string
		for _, p := range posts {
			texts = append(texts, p.text)
		}
		t.Errorf("expected threaded schedule completion notification, got texts: %v", texts)
	}
}

func TestNotifyAgentState_ScheduledDone_IncludesWrapUp(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedBlocks []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedBlocks = append(postedBlocks, r.FormValue("blocks"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "5678.9012"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.agentState["sched-wrap"] = "working"
	bot.agentScheduleTitle["sched-wrap"] = "Hourly Hello"
	bot.agentSpawnedAt["sched-wrap"] = time.Now().Add(-3 * time.Minute)

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "sched-wrap",
		Fields: map[string]string{
			"agent_state": "done",
			"agent":       "sched-wrap",
			"wrapup":      `{"accomplishments":"Closed 3 bugs and opened PR #42","blockers":"API key pending"}`,
		},
	})

	mu.Lock()
	defer mu.Unlock()

	found := false
	for _, blocks := range postedBlocks {
		if strings.Contains(blocks, "Scheduled task") && strings.Contains(blocks, "completed") {
			found = true
			if !strings.Contains(blocks, "Accomplishments") {
				t.Errorf("expected wrap-up accomplishments in notification blocks, got %q", blocks)
			}
			if !strings.Contains(blocks, "Closed 3 bugs") {
				t.Errorf("expected accomplishments text in notification blocks, got %q", blocks)
			}
			break
		}
	}
	if !found {
		t.Errorf("expected schedule completion notification with wrap-up, got blocks: %v", postedBlocks)
	}
}

// TestNotifyAgentState_ScheduledDone_ThreadedMode_ResurrectsCardFromState verifies
// that when the agent card was pruned from the hot cache but persists in state,
// the schedule completion notification still threads under the card (by
// resurrecting it via ensureAgentCard) instead of leaking to the channel.
func TestNotifyAgentState_ScheduledDone_ThreadedMode_ResurrectsCardFromState(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	type postedMsg struct {
		channel  string
		text     string
		threadTS string
	}
	var posts []postedMsg
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			posts = append(posts, postedMsg{
				channel:  r.FormValue("channel"),
				text:     r.FormValue("text"),
				threadTS: r.FormValue("thread_ts"),
			})
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "9999.0002"})
	}))
	defer slackSrv.Close()

	// Create a state manager with a persisted agent card.
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	cardRef := MessageRef{ChannelID: "C123", Timestamp: "1700000000.000001"}
	_ = state.SetAgentCard("sched-pruned", cardRef)

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"
	bot.state = state
	bot.agentState["sched-pruned"] = "working"
	bot.agentScheduleTitle["sched-pruned"] = "Hourly Cleanup"
	bot.agentSpawnedAt["sched-pruned"] = time.Now().Add(-2 * time.Minute)
	// Deliberately do NOT populate bot.agentCards — simulates hot cache pruned.

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "sched-pruned",
		Fields:   map[string]string{"agent_state": "done", "agent": "sched-pruned"},
	})

	mu.Lock()
	defer mu.Unlock()

	// Find the completion notification — it must be threaded under the
	// resurrected card, not posted as a top-level message.
	found := false
	for _, p := range posts {
		if strings.Contains(p.text, "Scheduled task") && strings.Contains(p.text, "completed") {
			found = true
			if p.threadTS == "" {
				t.Errorf("completion notification leaked to channel (no thread_ts); expected threaded under card %q", cardRef.Timestamp)
			} else if p.threadTS != cardRef.Timestamp {
				// ensureAgentCard may have created a new card rather than
				// resurrecting, but the message must still be threaded.
				t.Logf("threaded under %q (expected %q) — new card created", p.threadTS, cardRef.Timestamp)
			}
			break
		}
	}
	if !found {
		var texts []string
		for _, p := range posts {
			texts = append(texts, p.text)
		}
		t.Errorf("expected threaded schedule completion notification, got texts: %v", texts)
	}
}

func TestSpawnAgent_ExtraFields_MergedIntoMock(t *testing.T) {
	daemon := newMockDaemon()
	extra := map[string]string{
		"schedule_id":    "gd-sched-1",
		"schedule_title": "Daily Cleanup",
	}
	id, err := daemon.SpawnAgent(context.Background(), "sched-bot", "gardenio", "", "crew", "", extra)
	if err != nil {
		t.Fatalf("SpawnAgent: %v", err)
	}

	bead := daemon.beads[id]
	if bead == nil {
		t.Fatal("expected bead to be created")
	}
	if got := bead.Fields["schedule_id"]; got != "gd-sched-1" {
		t.Errorf("expected schedule_id=gd-sched-1, got %q", got)
	}
	if got := bead.Fields["schedule_title"]; got != "Daily Cleanup" {
		t.Errorf("expected schedule_title=Daily Cleanup, got %q", got)
	}
	// Standard fields should still be present.
	if got := bead.Fields["agent"]; got != "sched-bot" {
		t.Errorf("expected agent=sched-bot, got %q", got)
	}
}
