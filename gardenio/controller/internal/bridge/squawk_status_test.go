package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// slackCallRecord tracks a single Slack API call with its method and parameters.
type slackCallRecord struct {
	method    string
	channel   string
	threadTS  string
	text      string
	status    string // assistant.threads.setStatus
	timestamp string // message ts
}

// recordingSlackServer captures all Slack API calls for verification.
type recordingSlackServer struct {
	mu    sync.Mutex
	calls []slackCallRecord
	srv   *httptest.Server
}

func newRecordingSlackServer(t *testing.T) *recordingSlackServer {
	t.Helper()
	r := &recordingSlackServer{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_ = req.ParseForm()
		path := strings.TrimPrefix(req.URL.Path, "/")

		call := slackCallRecord{
			method:    path,
			channel:   req.FormValue("channel") + req.FormValue("channel_id"),
			threadTS:  req.FormValue("thread_ts"),
			text:      req.FormValue("text"),
			timestamp: req.FormValue("ts"),
		}
		if path == "assistant.threads.setStatus" {
			call.status = req.FormValue("status")
		}

		r.mu.Lock()
		r.calls = append(r.calls, call)
		r.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":         true,
			"ts":         "1234.5678",
			"message_ts": "1234.5678",
			"channel":    call.channel,
		})
	}))
	return r
}

func (r *recordingSlackServer) Close() { r.srv.Close() }

func (r *recordingSlackServer) callsByMethod(method string) []slackCallRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []slackCallRecord
	for _, c := range r.calls {
		if c.method == method {
			result = append(result, c)
		}
	}
	return result
}

// TestSquawk_ThreadBound_RestoresStatus verifies that when a squawk message is
// posted to a thread-bound agent's thread, the assistant thread status bar is
// restored after posting — WHEN the operator has opted into holding the status
// footer (HoldAssistantStatus=true). Slack clears the status on every bot
// PostMessage, and the hold-on path re-applies it. (By default the status is
// NOT re-held — see TestSquawk_ThreadBound_DoesNotRestoreStatusByDefault — that
// is the composer-unlock fix, ka-0000000115.)
func TestSquawk_ThreadBound_RestoresStatus(t *testing.T) {
	srv := newRecordingSlackServer(t)
	defer srv.Close()

	daemon := newMockDaemon()
	// Seed the agent bead with thread binding so resolveAgentThread finds it.
	daemon.beads["squawk-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-squawk-agent",
		Title: "squawk-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "squawk-agent",
			"slack_thread_channel": "C_THREAD",
			"slack_thread_ts":      "parent.ts",
		},
	}
	bot := newTestBot(daemon, srv.srv)
	bot.state, _ = NewStateManager("")
	// Opt into holding the status footer so the restore path is exercised.
	bot.notifConfig.HoldAssistantStatus = true

	// Set up an active streaming session so RestoreThreadStatus has something
	// to restore. Create a minimal stream manager with a mock status setter.
	streamAPI := &mockStreamClient{}
	mgr := NewAgentStreamManager(streamAPI, slog.Default())
	mgr.SetThreadStatusSetter(bot)
	bot.streamMgr = mgr

	// Start a session and record some tool activity.
	mgr.sessions["squawk-agent"] = &agentStreamSession{
		channelID:  "C_THREAD",
		threadTS:   "parent.ts",
		toolCounts: map[string]int{"Read": 5, "Bash": 3},
		totalTools: 8,
		activity:   "reading files",
		lastTool:   "Read",
		startedAt:  time.Now().Add(-5 * time.Minute),
	}

	// Create a squawk handler and simulate a squawk event.
	sq := &Squawk{
		daemon: daemon,
		bot:    bot,
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-restore",
		Type:   "message",
		Labels: []string{"squawk", "from:squawk-agent"},
		Fields: map[string]string{
			"source_agent": "squawk-agent",
			"text":         "Build complete!",
		},
	})

	sq.handleClosed(context.Background(), data)

	// Verify: chat.postMessage was called (the squawk message).
	postCalls := srv.callsByMethod("chat.postMessage")
	if len(postCalls) == 0 {
		t.Fatal("expected at least 1 chat.postMessage call for squawk")
	}

	// Verify: assistant.threads.setStatus was called AFTER the post to restore status.
	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) == 0 {
		t.Error("expected assistant.threads.setStatus call to restore status after squawk post")
	}

	// Verify the restored status contains session info (not empty).
	for _, c := range statusCalls {
		if c.status != "" && strings.Contains(c.status, "tools") {
			return // Found a non-empty status restore — test passes.
		}
	}
	t.Error("expected restored status to contain tool stats (e.g. 'reading files...')")
}

// TestSquawk_ThreadBound_DoesNotRestoreStatusByDefault verifies the
// composer-unlock default (ka-0000000115): posting a squawk to a thread-bound
// agent's thread does NOT re-hold a non-empty assistant status footer. The
// squawk message still posts; the continuously-held status (which is
// community-reported to lock the DM composer) is simply not re-applied.
func TestSquawk_ThreadBound_DoesNotRestoreStatusByDefault(t *testing.T) {
	srv := newRecordingSlackServer(t)
	defer srv.Close()

	daemon := newMockDaemon()
	daemon.beads["squawk-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-squawk-agent",
		Title: "squawk-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "squawk-agent",
			"slack_thread_channel": "C_THREAD",
			"slack_thread_ts":      "parent.ts",
		},
	}
	bot := newTestBot(daemon, srv.srv)
	bot.state, _ = NewStateManager("")
	// Default config: HoldAssistantStatus is false.

	streamAPI := &mockStreamClient{}
	mgr := NewAgentStreamManager(streamAPI, slog.Default())
	mgr.SetThreadStatusSetter(bot)
	bot.streamMgr = mgr

	mgr.sessions["squawk-agent"] = &agentStreamSession{
		channelID:  "C_THREAD",
		threadTS:   "parent.ts",
		toolCounts: map[string]int{"Read": 5, "Bash": 3},
		totalTools: 8,
		activity:   "reading files",
		lastTool:   "Read",
		startedAt:  time.Now().Add(-5 * time.Minute),
	}

	sq := &Squawk{
		daemon: daemon,
		bot:    bot,
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-nostatus",
		Type:   "message",
		Labels: []string{"squawk", "from:squawk-agent"},
		Fields: map[string]string{
			"source_agent": "squawk-agent",
			"text":         "Build complete!",
		},
	})

	sq.handleClosed(context.Background(), data)

	// The squawk itself must still post.
	if len(srv.callsByMethod("chat.postMessage")) == 0 {
		t.Fatal("expected at least 1 chat.postMessage call for squawk")
	}

	// No NON-EMPTY status should be re-applied (that is the held-status behavior
	// we removed). Empty clears, if any, are fine.
	for _, c := range srv.callsByMethod("assistant.threads.setStatus") {
		if c.status != "" {
			t.Errorf("expected no non-empty status restore by default (composer-unlock), got %q", c.status)
		}
	}
}

// TestSquawk_ThreadBound_UpdateSpawnMsg_RestoresStatus verifies that when a
// squawk updates the spawn message in-place (first squawk in a thread),
// the thread status is also restored.
func TestSquawk_ThreadBound_UpdateSpawnMsg_RestoresStatus(t *testing.T) {
	srv := newRecordingSlackServer(t)
	defer srv.Close()

	daemon := newMockDaemon()
	daemon.beads["update-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-update-agent",
		Title: "update-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "update-agent",
			"slack_thread_channel": "C_THREAD",
			"slack_thread_ts":      "parent.ts",
		},
	}
	bot := newTestBot(daemon, srv.srv)
	bot.state, _ = NewStateManager("")
	// Opt into holding the status footer so the restore path is exercised.
	bot.notifConfig.HoldAssistantStatus = true

	// Pre-populate a spawn message ref.
	bot.mu.Lock()
	bot.threadSpawnMsgs["update-agent"] = MessageRef{
		ChannelID: "C_THREAD",
		Timestamp: "spawn.msg.ts",
		ThreadTS:  "parent.ts",
	}
	bot.mu.Unlock()

	// Set up stream manager with active session.
	streamAPI := &mockStreamClient{}
	mgr := NewAgentStreamManager(streamAPI, slog.Default())
	mgr.SetThreadStatusSetter(bot)
	bot.streamMgr = mgr

	mgr.sessions["update-agent"] = &agentStreamSession{
		channelID:  "C_THREAD",
		threadTS:   "parent.ts",
		toolCounts: map[string]int{"Grep": 2},
		totalTools: 2,
		activity:   "searching code",
		startedAt:  time.Now().Add(-1 * time.Minute),
	}

	sq := &Squawk{
		daemon: daemon,
		bot:    bot,
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-update",
		Type:   "message",
		Labels: []string{"squawk", "from:update-agent"},
		Fields: map[string]string{
			"source_agent": "update-agent",
			"text":         "MR created",
		},
	})

	sq.handleClosed(context.Background(), data)

	// Verify: chat.update was called (spawn message update).
	updateCalls := srv.callsByMethod("chat.update")
	if len(updateCalls) == 0 {
		t.Fatal("expected chat.update call for spawn message update")
	}

	// Verify: assistant.threads.setStatus was called to restore status.
	statusCalls := srv.callsByMethod("assistant.threads.setStatus")
	if len(statusCalls) == 0 {
		t.Error("expected assistant.threads.setStatus call to restore status after spawn message update")
	}

	// Verify the spawn ref was consumed.
	bot.mu.Lock()
	_, still := bot.threadSpawnMsgs["update-agent"]
	bot.mu.Unlock()
	if still {
		t.Error("expected spawn message ref to be consumed after update")
	}
}

// TestSquawk_CardThread_PostsMessage verifies that squawk messages for
// non-thread-bound agents go to the agent card thread.
func TestSquawk_CardThread_PostsMessage(t *testing.T) {
	srv := newRecordingSlackServer(t)
	defer srv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, srv.srv)

	// Enable agent threading mode so routeAgentThreadMessage uses the card thread path.
	bot.threadingMode = "agent"

	// Set up agent card.
	bot.mu.Lock()
	bot.agentCards["card-agent"] = MessageRef{
		ChannelID: "C_CHANNEL",
		Timestamp: "card.ts",
	}
	bot.agentSpawnChannel["card-agent"] = "C_CHANNEL"
	bot.mu.Unlock()

	sq := &Squawk{
		daemon: daemon,
		bot:    bot,
		logger: slog.Default(),
		seen:   make(map[string]bool),
	}

	data := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-card",
		Type:   "message",
		Labels: []string{"squawk", "from:card-agent"},
		Fields: map[string]string{
			"source_agent": "card-agent",
			"text":         "Task done",
		},
	})

	sq.handleClosed(context.Background(), data)

	// Verify: chat.postMessage was called with the card thread_ts.
	postCalls := srv.callsByMethod("chat.postMessage")
	if len(postCalls) == 0 {
		t.Fatal("expected chat.postMessage call for card thread")
	}

	found := false
	for _, c := range postCalls {
		if c.threadTS == "card.ts" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected squawk to be posted in the agent card thread (thread_ts=card.ts)")
	}
}

// TestAgentStreamManager_RestoreStatus_RebuildFromSession verifies that
// RestoreThreadStatus rebuilds a full status string from the active session.
func TestAgentStreamManager_RestoreStatus_RebuildFromSession(t *testing.T) {
	statusMock := &mockThreadStatusSetter{}
	streamAPI := &mockStreamClient{}
	mgr := NewAgentStreamManager(streamAPI, slog.Default())
	mgr.SetThreadStatusSetter(statusMock)

	// Seed an active session with tool activity.
	mgr.sessions["restore-agent"] = &agentStreamSession{
		channelID:     "C_THREAD",
		threadTS:      "parent.ts",
		toolCounts:    map[string]int{"Read": 10, "Bash": 5},
		totalTools:    15,
		activity:      "running commands",
		lastTool:      "Bash",
		lastToolLabel: "make test",
		startedAt:     time.Now().Add(-3 * time.Minute),
	}

	mgr.RestoreThreadStatus(context.Background(), "C_THREAD", "parent.ts")

	calls := statusMock.getCalls()
	if len(calls) == 0 {
		t.Fatal("expected SetThreadStatus to be called")
	}

	lastCall := calls[len(calls)-1]
	if lastCall.ChannelID != "C_THREAD" {
		t.Errorf("expected ChannelID=C_THREAD, got %s", lastCall.ChannelID)
	}
	if lastCall.ThreadTS != "parent.ts" {
		t.Errorf("expected ThreadTS=parent.ts, got %s", lastCall.ThreadTS)
	}
	if !strings.Contains(lastCall.Status, "tools") {
		t.Errorf("expected status to contain tool count, got %q", lastCall.Status)
	}
	if !strings.Contains(lastCall.Status, "Bash") {
		t.Errorf("expected status to mention last tool Bash, got %q", lastCall.Status)
	}
}

// TestAgentStreamManager_RestoreStatus_WrongThread_NoOp verifies that
// RestoreThreadStatus is a no-op for unrelated threads.
func TestAgentStreamManager_RestoreStatus_WrongThread_NoOp(t *testing.T) {
	statusMock := &mockThreadStatusSetter{}
	streamAPI := &mockStreamClient{}
	mgr := NewAgentStreamManager(streamAPI, slog.Default())
	mgr.SetThreadStatusSetter(statusMock)

	mgr.sessions["some-agent"] = &agentStreamSession{
		channelID:  "C_OTHER",
		threadTS:   "other.ts",
		toolCounts: map[string]int{"Read": 1},
		totalTools: 1,
		startedAt:  time.Now(),
	}

	mgr.RestoreThreadStatus(context.Background(), "C_THREAD", "parent.ts")

	if len(statusMock.getCalls()) != 0 {
		t.Error("expected no SetThreadStatus call for non-matching thread")
	}
}
