package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/slack-go/slack"
)

func testLogger() *slog.Logger { return slog.Default() }

// mockStreamClient records all streaming API calls for verification.
type mockStreamClient struct {
	mu    sync.Mutex
	calls []streamCall
}

type streamCall struct {
	method     string // "start", "append", "stop", "startWithMode", "appendChunks"
	channelID  string
	timestamp  string
	text       string //nolint:unused // extracted from MsgOptionMarkdownText
	mode       string // task_display_mode for startWithMode
	chunksJSON string // raw chunks JSON for appendChunks
}

func (m *mockStreamClient) StartStreamContext(_ context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, streamCall{method: "start", channelID: channelID})
	return channelID, "stream-ts-123", nil
}

func (m *mockStreamClient) AppendStreamContext(_ context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, streamCall{method: "append", channelID: channelID, timestamp: timestamp})
	return channelID, timestamp, nil
}

func (m *mockStreamClient) StopStreamContext(_ context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, streamCall{method: "stop", channelID: channelID, timestamp: timestamp})
	return channelID, timestamp, nil
}

func (m *mockStreamClient) StartStreamWithModeContext(_ context.Context, channelID, taskDisplayMode string, options ...slack.MsgOption) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, streamCall{method: "startWithMode", channelID: channelID, mode: taskDisplayMode})
	return channelID, "stream-ts-123", nil
}

func (m *mockStreamClient) AppendStreamChunksContext(_ context.Context, channelID, timestamp, chunksJSON string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, streamCall{method: "appendChunks", channelID: channelID, timestamp: timestamp, chunksJSON: chunksJSON})
	return nil
}

func (m *mockStreamClient) DeleteMessageContext(_ context.Context, channelID, timestamp string) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, streamCall{method: "delete", channelID: channelID, timestamp: timestamp})
	return channelID, timestamp, nil
}

func (m *mockStreamClient) getCalls() []streamCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]streamCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func TestStartSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	if !mgr.HasSession("worker-1") {
		t.Fatal("expected session for worker-1")
	}
	if mgr.SessionCount() != 1 {
		t.Fatalf("expected 1 session, got %d", mgr.SessionCount())
	}

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(calls))
	}
	if calls[0].method != "startWithMode" {
		t.Errorf("expected startWithMode call, got %s", calls[0].method)
	}
	if calls[0].channelID != "C123" {
		t.Errorf("expected channel C123, got %s", calls[0].channelID)
	}
	if calls[0].mode != "timeline" {
		t.Errorf("expected timeline mode, got %s", calls[0].mode)
	}
}

func TestStartSession_ReturnsStreamTS(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	ts := mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	if ts != "stream-ts-123" {
		t.Errorf("expected stream-ts-123, got %q", ts)
	}
}

func TestStartSessionWithText(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	ts := mgr.StartSessionWithText(ctx, "worker-1", "C123", "thread-ts-1", "U12345", ":zap: Custom starting text")

	if ts != "stream-ts-123" {
		t.Errorf("expected stream-ts-123, got %q", ts)
	}
	if !mgr.HasSession("worker-1") {
		t.Fatal("expected session for worker-1")
	}
}

func TestStartSessionWithText_EmptyFallback(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	ts := mgr.StartSessionWithText(ctx, "worker-1", "C123", "thread-ts-1", "U12345", "")

	if ts != "stream-ts-123" {
		t.Errorf("expected stream-ts-123, got %q", ts)
	}
}

func TestGetStreamTS(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	ts := mgr.GetStreamTS("worker-1")
	if ts != "stream-ts-123" {
		t.Errorf("expected stream-ts-123, got %q", ts)
	}

	// Non-existent agent returns "".
	ts = mgr.GetStreamTS("nonexistent")
	if ts != "" {
		t.Errorf("expected empty string for nonexistent, got %q", ts)
	}

	// Stopped session returns "".
	mgr.StopSession(ctx, "worker-1", "done")
	ts = mgr.GetStreamTS("worker-1")
	if ts != "" {
		t.Errorf("expected empty string after stop, got %q", ts)
	}
}

func TestStartSession_NoDuplicate(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	ts1 := mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	ts2 := mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345") // duplicate

	if ts1 == "" {
		t.Error("first StartSession should return stream TS")
	}
	if ts2 != "" {
		t.Errorf("duplicate StartSession should return empty, got %q", ts2)
	}

	if mgr.SessionCount() != 1 {
		t.Fatalf("expected 1 session, got %d", mgr.SessionCount())
	}

	calls := mock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected only 1 start call, got %d", len(calls))
	}
}

// TestStartSessionWithText_ConcurrentNoDuplicate verifies that concurrent
// calls to StartSessionWithText for the same agent result in exactly one
// session. Run with -race to detect data races.
func TestStartSessionWithText_ConcurrentNoDuplicate(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	const goroutines = 10
	results := make([]string, goroutines)
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			results[idx] = mgr.StartSessionWithText(ctx, "worker-1", "C123", "thread-ts-1", "U12345", "")
		}(i)
	}
	wg.Wait()

	// Exactly one goroutine should win.
	winners := 0
	for _, ts := range results {
		if ts != "" {
			winners++
		}
	}
	if winners != 1 {
		t.Errorf("expected exactly 1 winner, got %d", winners)
	}

	if mgr.SessionCount() != 1 {
		t.Errorf("expected 1 session, got %d", mgr.SessionCount())
	}
}

func TestHandleToolEvent(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")
	mgr.HandleToolEvent("worker-1", "Read", "PostToolUse", "")
	mgr.HandleToolEvent("worker-1", "Grep", "PreToolUse", "")
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")

	mgr.mu.Lock()
	s := mgr.sessions["worker-1"]
	totalTools := s.totalTools
	readCount := s.toolCounts["Read"]
	grepCount := s.toolCounts["Grep"]
	mgr.mu.Unlock()

	if totalTools != 3 {
		t.Errorf("expected 3 total tools, got %d", totalTools)
	}
	if readCount != 2 {
		t.Errorf("expected 2 Read calls, got %d", readCount)
	}
	if grepCount != 1 {
		t.Errorf("expected 1 Grep call, got %d", grepCount)
	}
}

func TestHandleToolEvent_NoSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	// Should not panic when no session exists.
	mgr.HandleToolEvent("nonexistent", "Read", "PreToolUse", "")
}

func TestStopSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")
	mgr.HandleToolEvent("worker-1", "Edit", "PreToolUse", "")
	mgr.StopSession(ctx, "worker-1", "done")

	if mgr.HasSession("worker-1") {
		t.Fatal("expected session to be removed after stop")
	}
	if mgr.SessionCount() != 0 {
		t.Fatalf("expected 0 sessions, got %d", mgr.SessionCount())
	}

	calls := mock.getCalls()
	// Should have: startWithMode, appendChunks (final summary), stop
	if len(calls) != 3 {
		t.Fatalf("expected 3 calls (startWithMode+appendChunks+stop), got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks call for final summary, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"agent-status"`) {
		t.Errorf("expected agent-status chunk ID in final summary, got %s", calls[1].chunksJSON)
	}
	if !strings.Contains(calls[1].chunksJSON, `"complete"`) {
		t.Errorf("expected complete status in final summary, got %s", calls[1].chunksJSON)
	}
	if calls[2].method != "stop" {
		t.Errorf("expected stop call, got %s", calls[2].method)
	}
	if calls[2].timestamp != "stream-ts-123" {
		t.Errorf("expected stream ts stream-ts-123, got %s", calls[2].timestamp)
	}
}

func TestStopSession_Idempotent(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.StopSession(ctx, "worker-1", "done")
	mgr.StopSession(ctx, "worker-1", "done") // second stop should be no-op

	calls := mock.getCalls()
	// With 0 tools, the empty stream is deleted (not stopped).
	deleteCount := 0
	for _, c := range calls {
		if c.method == "delete" {
			deleteCount++
		}
	}
	if deleteCount != 1 {
		t.Errorf("expected 1 delete call, got %d", deleteCount)
	}
}

func TestStopSession_DeletesEmptyStream(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	// No tool events, no task updates — session is empty.
	mgr.StopSession(ctx, "worker-1", "done")

	calls := mock.getCalls()
	// Should have: startWithMode + delete (no appendChunks, no stop).
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (startWithMode+delete), got %d: %v", len(calls), calls)
	}
	if calls[1].method != "delete" {
		t.Errorf("expected delete call for empty stream, got %s", calls[1].method)
	}
	if calls[1].timestamp != "stream-ts-123" {
		t.Errorf("expected stream ts stream-ts-123, got %s", calls[1].timestamp)
	}
}

func TestStopSession_WithChunksDoesNotDelete(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	// Send a task update (which increments chunksAppended) but no tool events.
	mgr.HandleBeadClaimed(ctx, "worker-1", "bead-1", "Fix auth bug")
	mgr.StopSession(ctx, "worker-1", "done")

	calls := mock.getCalls()
	// Should have normal stop flow (not delete) because chunks were appended.
	hasStop := false
	hasDelete := false
	for _, c := range calls {
		if c.method == "stop" {
			hasStop = true
		}
		if c.method == "delete" {
			hasDelete = true
		}
	}
	if !hasStop {
		t.Error("expected stop call when chunks were appended")
	}
	if hasDelete {
		t.Error("did not expect delete call when chunks were appended")
	}
}

func TestFlush(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")
	mgr.HandleToolEvent("worker-1", "Grep", "PreToolUse", "")

	// Manually trigger flush.
	mgr.flush(ctx)

	calls := mock.getCalls()
	// Flush sends a structured agent-status chunk: startWithMode + appendChunks.
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls (startWithMode + appendChunks), got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks from flush, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"agent-status"`) {
		t.Errorf("expected agent-status chunk ID, got %s", calls[1].chunksJSON)
	}
	if !strings.Contains(calls[1].chunksJSON, `"in_progress"`) {
		t.Errorf("expected in_progress status in chunk, got %s", calls[1].chunksJSON)
	}

	// Flush again without changes should not produce any new calls.
	mgr.flush(ctx)
	calls = mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected still 2 calls after clean flush, got %d", len(calls))
	}
}

func TestFlush_MultipleAgents(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.StartSession(ctx, "worker-2", "C456", "thread-ts-2", "U67890")

	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")
	mgr.HandleToolEvent("worker-2", "Bash", "PreToolUse", "")

	mgr.flush(ctx)

	calls := mock.getCalls()
	// Flush sends structured chunks — 2 startWithMode + 2 appendChunks.
	chunkCount := 0
	for _, c := range calls {
		if c.method == "appendChunks" {
			chunkCount++
		}
	}
	if chunkCount != 2 {
		t.Errorf("expected 2 appendChunks calls from flush, got %d", chunkCount)
	}
}

func TestBuildStopSummary(t *testing.T) {
	counts := map[string]int{"Read": 3, "Edit": 2}

	tests := []struct {
		outcome   string
		wantEmoji string
	}{
		{"done", "🌼"},
		{"failed", "🪨"},
		{"stopped", "🪓"},
	}

	for _, tt := range tests {
		t.Run(tt.outcome, func(t *testing.T) {
			summary := buildStopSummary(Garden, "worker-1", tt.outcome, 5, counts, 3*time.Minute+42*time.Second)
			if !contains(summary, tt.wantEmoji+" Agent `worker-1` — "+tt.outcome) {
				t.Errorf("expected emoji %s glued to the outcome word in summary for outcome %s: %q", tt.wantEmoji, tt.outcome, summary)
			}
			if !contains(summary, "🍅 5 tool calls") {
				t.Errorf("expected report glyph beside tool count in summary: %q", summary)
			}
			// formatDuration(3m42s) outputs "3m 42s"
			if !contains(summary, "3m") {
				t.Error("expected duration in summary")
			}
			// Tool glyphs ride next to the tool name.
			for _, want := range []string{"📖 Read (3)", "✂️ Edit (2)"} {
				if !contains(summary, want) {
					t.Errorf("summary missing %q: %q", want, summary)
				}
			}
		})
	}
}

// formatDuration is tested in bot_agents_test.go — we just verify it's available.
func TestFormatDurationAvailable(t *testing.T) {
	got := formatDuration(90 * time.Second)
	if got != "1m 30s" {
		t.Errorf("formatDuration(90s) = %q, want %q", got, "1m 30s")
	}
}

func TestBuildThreadStatusText_WithLabel(t *testing.T) {
	tc := map[string]int{"Bash": 3, "Read": 2}
	start := time.Now().Add(-2 * time.Minute)

	// With label: should show Bash(git status)
	got := buildThreadStatusText(Garden, "running commands", "Bash", "git status", 5, tc, start)
	if !contains(got, "Bash(git status)") {
		t.Errorf("expected 'Bash(git status)' in status, got %q", got)
	}

	// Without label: should show just Bash
	got = buildThreadStatusText(Garden, "running commands", "Bash", "", 5, tc, start)
	if !contains(got, "last: Bash") {
		t.Errorf("expected 'last: Bash' in status, got %q", got)
	}
	if contains(got, "Bash(") {
		t.Errorf("expected no parenthetical label when label is empty, got %q", got)
	}
}

func TestHandleToolEvent_StoresLabel(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleToolEvent("worker-1", "Bash", "PreToolUse", "show working tree status")

	mgr.mu.Lock()
	s := mgr.sessions["worker-1"]
	label := s.lastToolLabel
	mgr.mu.Unlock()

	if label != "show working tree status" {
		t.Errorf("expected lastToolLabel='show working tree status', got %q", label)
	}
}

func TestToolActivity(t *testing.T) {
	tests := map[string]string{
		"Read":  "reading files",
		"Grep":  "searching code",
		"Edit":  "editing code",
		"Bash":  "running commands",
		"Other": "working",
	}
	for tool, want := range tests {
		got := toolActivity(tool)
		if got != want {
			t.Errorf("toolActivity(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestGCStaleSessions(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	// Artificially age the session.
	mgr.mu.Lock()
	mgr.sessions["worker-1"].startedAt = time.Now().Add(-25 * time.Hour)
	mgr.mu.Unlock()

	mgr.gcStaleSessions()

	if mgr.HasSession("worker-1") {
		t.Fatal("expected stale session to be garbage collected")
	}
}

func TestRunContextCancel(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		mgr.Run(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// OK — Run exited
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancellation")
	}
}

func TestHandleBeadClaimed(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleBeadClaimed(ctx, "worker-1", "gd-ABC123", "Fix nil pointer in bridge")

	calls := mock.getCalls()
	// startWithMode + appendChunks (task update sent as structured chunk)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"gd-ABC123"`) {
		t.Errorf("expected bead ID in chunk, got %s", calls[1].chunksJSON)
	}
	if !strings.Contains(calls[1].chunksJSON, `"in_progress"`) {
		t.Errorf("expected in_progress status in chunk, got %s", calls[1].chunksJSON)
	}
}

func TestHandleBeadClosed(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleBeadClosed(ctx, "worker-1", "gd-ABC123", "Fix nil pointer in bridge")

	calls := mock.getCalls()
	// startWithMode + appendChunks (task update sent as structured chunk)
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"complete"`) {
		t.Errorf("expected complete status in chunk, got %s", calls[1].chunksJSON)
	}
}

func TestHandleBeadClaimed_NoSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	// Should not panic when no session exists.
	mgr.HandleBeadClaimed(ctx, "nonexistent", "gd-ABC", "Some task")

	calls := mock.getCalls()
	if len(calls) != 0 {
		t.Errorf("expected 0 calls for missing session, got %d", len(calls))
	}
}

func TestHandleBeadClaimed_StoppedSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.StopSession(ctx, "worker-1", "done")

	// Should not send task updates to a stopped session.
	mgr.HandleBeadClaimed(ctx, "worker-1", "gd-ABC", "Some task")

	calls := mock.getCalls()
	// Count appendChunks calls that are NOT the stop summary (which has agent-status).
	beadClaimChunks := 0
	for _, c := range calls {
		if c.method == "appendChunks" && strings.Contains(c.chunksJSON, `"gd-ABC"`) {
			beadClaimChunks++
		}
	}
	if beadClaimChunks != 0 {
		t.Errorf("expected 0 bead claim chunks for stopped session, got %d", beadClaimChunks)
	}
}

func TestAppendChunk_InProgress(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	// Send an in_progress task update via HandleBeadClaimed.
	mgr.HandleBeadClaimed(ctx, "worker-1", "gd-TEST", "Test task")

	calls := mock.getCalls()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(calls))
	}
	chunk := calls[1]
	if chunk.method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", chunk.method)
	}
	if !strings.Contains(chunk.chunksJSON, `"gd-TEST"`) {
		t.Error("expected bead ID in chunk JSON")
	}
	if !strings.Contains(chunk.chunksJSON, `"Test task"`) {
		t.Error("expected title in chunk JSON")
	}
	if !strings.Contains(chunk.chunksJSON, `"in_progress"`) {
		t.Error("expected in_progress status in chunk JSON")
	}
}

func TestAppendChunk_CompleteWithOutput(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	// Send a complete task update with output via HandleDecisionResolved.
	mgr.HandleDecisionResolved(ctx, "worker-1", "gd-DEC1", "jwt")

	calls := mock.getCalls()
	if len(calls) < 2 {
		t.Fatalf("expected at least 2 calls, got %d", len(calls))
	}
	chunk := calls[1]
	if !strings.Contains(chunk.chunksJSON, `"complete"`) {
		t.Error("expected complete status in chunk JSON")
	}
	if !strings.Contains(chunk.chunksJSON, `"Chosen: jwt"`) {
		t.Error("expected output in chunk JSON")
	}
}

func TestHandleSubagentStart(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleSubagentStart(ctx, "worker-1", "sub-abc", "Explore")

	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"subagent:sub-abc"`) {
		t.Errorf("expected subagent chunk ID, got %s", calls[1].chunksJSON)
	}
}

func TestHandleSubagentStop(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleSubagentStop(ctx, "worker-1", "sub-abc", "Explore", "Found the bug in auth.go:42")

	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"complete"`) {
		t.Errorf("expected complete status in chunk, got %s", calls[1].chunksJSON)
	}
	// The subagent's final message must be forwarded as the chunk Output —
	// mirroring HandleStopEvent for the host agent (gd-TdQnFDzJdo).
	if !strings.Contains(calls[1].chunksJSON, `"Found the bug in auth.go:42"`) {
		t.Errorf("expected subagent final message in chunk Output, got %s", calls[1].chunksJSON)
	}
}

// TestHandleSubagentStop_EmptyMessage verifies the completion chunk is still
// sent (with no Output) when the SubagentStop hook carries no final message.
func TestHandleSubagentStop_EmptyMessage(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleSubagentStop(ctx, "worker-1", "sub-abc", "Explore", "")

	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if !strings.Contains(calls[1].chunksJSON, `"complete"`) {
		t.Errorf("expected complete status in chunk, got %s", calls[1].chunksJSON)
	}
	// output is omitempty, so an empty final message must not emit an output key.
	if strings.Contains(calls[1].chunksJSON, `"output"`) {
		t.Errorf("expected no output key for empty final message, got %s", calls[1].chunksJSON)
	}
}

func TestHandleDecisionCreated(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleDecisionCreated(ctx, "worker-1", "gd-DEC1", "Should we use JWT or sessions?")

	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"gd-DEC1"`) {
		t.Errorf("expected decision ID in chunk, got %s", calls[1].chunksJSON)
	}
}

func TestHandleDecisionResolved(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleDecisionResolved(ctx, "worker-1", "gd-DEC1", "jwt")

	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", calls[1].method)
	}
	if !strings.Contains(calls[1].chunksJSON, `"complete"`) {
		t.Errorf("expected complete status in chunk, got %s", calls[1].chunksJSON)
	}
}

func TestHandleGitEvent(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	mgr.HandleGitEvent(ctx, "worker-1", "Committed abc1234: fix nil pointer")

	calls := mock.getCalls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(calls))
	}
	if calls[1].method != "appendChunks" {
		t.Errorf("expected appendChunks, got %s", calls[1].method)
	}
}

// --- ThreadStatusSetter tests ---

// mockThreadStatusSetter records SetThreadStatus calls.
type mockThreadStatusSetter struct {
	mu    sync.Mutex
	calls []threadStatusCall
}

type threadStatusCall struct {
	ChannelID string
	ThreadTS  string
	Status    string
}

func (m *mockThreadStatusSetter) SetThreadStatus(_ context.Context, channelID, threadTS, status string) {
	m.mu.Lock()
	m.calls = append(m.calls, threadStatusCall{ChannelID: channelID, ThreadTS: threadTS, Status: status})
	m.mu.Unlock()
}

func (m *mockThreadStatusSetter) getCalls() []threadStatusCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]threadStatusCall, len(m.calls))
	copy(out, m.calls)
	return out
}

func TestFlush_UpdatesThreadStatus(t *testing.T) {
	mock := &mockStreamClient{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetThreadStatusSetter(statusMock)

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	// Simulate a tool event that sets activity.
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")

	// Flush should update both the thread status and the stream shimmer.
	mgr.flush(ctx)

	statusCalls := statusMock.getCalls()
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 thread status call, got %d", len(statusCalls))
	}
	if statusCalls[0].ChannelID != "C123" {
		t.Errorf("expected channel C123, got %s", statusCalls[0].ChannelID)
	}
	if statusCalls[0].ThreadTS != "thread-ts-1" {
		t.Errorf("expected thread_ts thread-ts-1, got %s", statusCalls[0].ThreadTS)
	}
	// Status includes tool count, elapsed time, and last tool used.
	// Elapsed time varies, so check prefix and suffix.
	s := statusCalls[0].Status
	if !strings.HasPrefix(s, Garden.Glyph(LifecycleWorking)+" reading files... (1 tools) [") || !strings.HasSuffix(s, "] · last: Read") {
		t.Errorf("expected status like '<working glyph> reading files... (1 tools) [Xs] · last: Read', got %q", s)
	}

	// Verify the stream shimmer also received the same status via structured chunk.
	streamCalls := mock.getCalls()
	chunkFound := false
	for _, c := range streamCalls {
		if c.method == "appendChunks" && strings.Contains(c.chunksJSON, `"agent-status"`) {
			chunkFound = true
			// The chunk title should contain the same status text as the thread footer.
			if !strings.Contains(c.chunksJSON, "reading files") {
				t.Errorf("expected chunk to contain 'reading files', got %s", c.chunksJSON)
			}
		}
	}
	if !chunkFound {
		t.Error("expected appendChunks call with agent-status chunk from flush")
	}
}

func TestStopSession_ClearsThreadStatus(t *testing.T) {
	mock := &mockStreamClient{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetThreadStatusSetter(statusMock)

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.StopSession(ctx, "worker-1", "done")

	statusCalls := statusMock.getCalls()
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 thread status call, got %d", len(statusCalls))
	}
	if statusCalls[0].Status != "" {
		t.Errorf("expected empty status on stop, got %q", statusCalls[0].Status)
	}
}

func TestHandleBeadClaimed_UpdatesThreadStatus(t *testing.T) {
	mock := &mockStreamClient{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetThreadStatusSetter(statusMock)

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleBeadClaimed(ctx, "worker-1", "bead-1", "Fix auth bug")

	statusCalls := statusMock.getCalls()
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 thread status call, got %d", len(statusCalls))
	}
	if statusCalls[0].Status != "Working on: Fix auth bug" {
		t.Errorf("expected 'Working on: Fix auth bug', got %q", statusCalls[0].Status)
	}
}

func TestHandleDecisionCreated_UpdatesThreadStatus(t *testing.T) {
	mock := &mockStreamClient{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetThreadStatusSetter(statusMock)

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	mgr.HandleDecisionCreated(ctx, "worker-1", "dec-1", "Which approach?")

	statusCalls := statusMock.getCalls()
	if len(statusCalls) != 1 {
		t.Fatalf("expected 1 thread status call, got %d", len(statusCalls))
	}
	if statusCalls[0].Status != "Waiting for human input..." {
		t.Errorf("expected 'Waiting for human input...', got %q", statusCalls[0].Status)
	}
}

// --- helpers ---

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && containsStr(s, substr)
}

func TestGetSessionStats(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	// No session — should return nil.
	if stats := mgr.GetSessionStats("no-such-agent"); stats != nil {
		t.Fatal("expected nil for non-existent session")
	}

	// Start a session and record tool events.
	mgr.StartSession(ctx, "test-agent", "C123", "1234.5678", "U999")
	mgr.HandleToolEvent("test-agent", "Read", "PreToolUse", "")
	mgr.HandleToolEvent("test-agent", "Read", "PostToolUse", "")
	mgr.HandleToolEvent("test-agent", "Bash", "PreToolUse", "git status")

	stats := mgr.GetSessionStats("test-agent")
	if stats == nil {
		t.Fatal("expected non-nil stats for active session")
	}
	if stats.TotalTools != 2 {
		t.Errorf("expected 2 total tools, got %d", stats.TotalTools)
	}
	if stats.LastTool != "Bash" {
		t.Errorf("expected last tool Bash, got %s", stats.LastTool)
	}
	if stats.LastToolLabel != "git status" {
		t.Errorf("expected last tool label 'git status', got %s", stats.LastToolLabel)
	}
	if stats.Activity != "running commands" {
		t.Errorf("expected activity 'running commands', got %s", stats.Activity)
	}
	if stats.StartedAt.IsZero() {
		t.Error("expected non-zero StartedAt")
	}

	// After stopping, stats should be nil.
	mgr.StopSession(ctx, "test-agent", "done")
	if stats := mgr.GetSessionStats("test-agent"); stats != nil {
		t.Fatal("expected nil stats after session stopped")
	}
}

func TestFlushUpdatesAgentCard(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	statusMock := &mockThreadStatusSetter{}
	mgr.SetThreadStatusSetter(statusMock)

	cardMock := &mockCardRefresher{}
	mgr.SetAgentCardRefresher(cardMock)

	mgr.StartSession(ctx, "card-agent", "C456", "9999.0000", "U111")
	mgr.HandleToolEvent("card-agent", "Edit", "PreToolUse", "")

	// Flush should update both thread status and agent card.
	mgr.flush(ctx)

	statusCalls := len(statusMock.getCalls())

	cardMock.mu.Lock()
	cardCalls := len(cardMock.calls)
	cardMock.mu.Unlock()

	if statusCalls == 0 {
		t.Error("expected thread status to be updated")
	}
	if cardCalls == 0 {
		t.Error("expected agent card to be refreshed")
	}
	if cardCalls > 0 {
		cardMock.mu.Lock()
		if cardMock.calls[0] != "card-agent" {
			t.Errorf("expected card refresh for 'card-agent', got %q", cardMock.calls[0])
		}
		cardMock.mu.Unlock()
	}
}

// mockCardRefresher records RefreshAgentCard calls.
type mockCardRefresher struct {
	mu    sync.Mutex
	calls []string // agent names
}

func (m *mockCardRefresher) RefreshAgentCard(_ context.Context, agentName string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, agentName)
}

// --- StartCardSession tests ---

func TestStartCardSession_TracksToolEvents(t *testing.T) {
	mock := &mockStreamClient{}
	cardMock := &mockCardRefresher{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetAgentCardRefresher(cardMock)

	// Start a card-only session — should NOT call StartStreamContext.
	ok := mgr.StartCardSession("worker-1", "C123", "card-ts-1")
	if !ok {
		t.Fatal("expected StartCardSession to return true")
	}

	// Verify no Slack streaming API calls were made.
	mock.mu.Lock()
	if len(mock.calls) != 0 {
		t.Errorf("expected 0 stream API calls, got %d", len(mock.calls))
	}
	mock.mu.Unlock()

	// Tool events should be tracked.
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")
	mgr.HandleToolEvent("worker-1", "Read", "PostToolUse", "")
	mgr.HandleToolEvent("worker-1", "Bash", "PreToolUse", "")

	stats := mgr.GetSessionStats("worker-1")
	if stats == nil {
		t.Fatal("expected non-nil stats")
	}
	if stats.TotalTools != 2 {
		t.Errorf("expected 2 total tools, got %d", stats.TotalTools)
	}
	if stats.LastTool != "Bash" {
		t.Errorf("expected last tool 'Bash', got %q", stats.LastTool)
	}
}

func TestStartCardSession_FlushRefreshesCard(t *testing.T) {
	mock := &mockStreamClient{}
	cardMock := &mockCardRefresher{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetAgentCardRefresher(cardMock)
	mgr.SetThreadStatusSetter(statusMock)

	mgr.StartCardSession("worker-1", "C123", "card-ts-1")
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")

	ctx := context.Background()
	mgr.flush(ctx)

	// Card should be refreshed.
	cardMock.mu.Lock()
	if len(cardMock.calls) != 1 || cardMock.calls[0] != "worker-1" {
		t.Errorf("expected 1 card refresh for worker-1, got %v", cardMock.calls)
	}
	cardMock.mu.Unlock()

	// Thread status should NOT be updated (card-only = no assistant thread).
	calls := statusMock.getCalls()
	if len(calls) != 0 {
		t.Errorf("expected 0 thread status calls for card-only session, got %d", len(calls))
	}
}

func TestStartCardSession_StopSkipsStreamAPI(t *testing.T) {
	mock := &mockStreamClient{}
	cardMock := &mockCardRefresher{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetAgentCardRefresher(cardMock)

	mgr.StartCardSession("worker-1", "C123", "card-ts-1")
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")

	ctx := context.Background()
	mgr.StopSession(ctx, "worker-1", "completed")

	// Verify no StopStreamContext was called.
	mock.mu.Lock()
	for _, call := range mock.calls {
		if call.method == "stop" {
			t.Error("StopStreamContext should not be called for card-only sessions")
		}
	}
	mock.mu.Unlock()

	// Card should get a final refresh.
	cardMock.mu.Lock()
	if len(cardMock.calls) != 1 || cardMock.calls[0] != "worker-1" {
		t.Errorf("expected 1 final card refresh, got %v", cardMock.calls)
	}
	cardMock.mu.Unlock()
}

func TestStartCardSession_DuplicateReturnsFalse(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())

	ok := mgr.StartCardSession("worker-1", "C123", "card-ts-1")
	if !ok {
		t.Fatal("first call should return true")
	}
	ok = mgr.StartCardSession("worker-1", "C123", "card-ts-1")
	if ok {
		t.Error("duplicate call should return false")
	}
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- RestoreThreadStatus tests ---

func TestRestoreThreadStatus_ActiveSession(t *testing.T) {
	mock := &mockStreamClient{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetThreadStatusSetter(statusMock)

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	// Simulate some tool activity so there's a meaningful status.
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")
	mgr.HandleToolEvent("worker-1", "Read", "PostToolUse", "")
	mgr.HandleToolEvent("worker-1", "Bash", "PreToolUse", "")

	// RestoreThreadStatus should re-set the status from the session data.
	mgr.RestoreThreadStatus(ctx, "C123", "thread-ts-1")

	calls := statusMock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 thread status call, got %d", len(calls))
	}
	if calls[0].ChannelID != "C123" || calls[0].ThreadTS != "thread-ts-1" {
		t.Errorf("wrong channel/thread: %s/%s", calls[0].ChannelID, calls[0].ThreadTS)
	}
	// Status should include tool count and last tool.
	if !strings.Contains(calls[0].Status, "2 tools") {
		t.Errorf("expected status to contain '2 tools', got %q", calls[0].Status)
	}
	if !strings.Contains(calls[0].Status, "last: Bash") {
		t.Errorf("expected status to contain 'last: Bash', got %q", calls[0].Status)
	}
}

func TestRestoreThreadStatus_NoSession(t *testing.T) {
	mock := &mockStreamClient{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetThreadStatusSetter(statusMock)

	ctx := context.Background()

	// No session exists — RestoreThreadStatus should be a no-op.
	mgr.RestoreThreadStatus(ctx, "C123", "thread-ts-1")

	calls := statusMock.getCalls()
	if len(calls) != 0 {
		t.Fatalf("expected 0 thread status calls, got %d", len(calls))
	}
}

func TestRestoreThreadStatus_NoToolsYet(t *testing.T) {
	mock := &mockStreamClient{}
	statusMock := &mockThreadStatusSetter{}
	mgr := NewAgentStreamManager(mock, testLogger())
	mgr.SetThreadStatusSetter(statusMock)

	ctx := context.Background()
	mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")

	// No tool events yet — should show a generic starting status.
	mgr.RestoreThreadStatus(ctx, "C123", "thread-ts-1")

	calls := statusMock.getCalls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 thread status call, got %d", len(calls))
	}
	if want := Garden.Glyph(LifecycleSpawning) + " Starting up..."; calls[0].Status != want {
		t.Errorf("expected %q status, got %q", want, calls[0].Status)
	}
}

// errStreamClient is a mock that returns a configurable error from AppendStreamChunksContext.
type errStreamClient struct {
	mockStreamClient
	appendErr error // error returned by AppendStreamChunksContext
}

func (m *errStreamClient) AppendStreamChunksContext(_ context.Context, channelID, timestamp, chunksJSON string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, streamCall{method: "appendChunks", channelID: channelID, timestamp: timestamp, chunksJSON: chunksJSON})
	return m.appendErr
}

func TestAppendChunk_StopsSessionOnStreamFinalized(t *testing.T) {
	mock := &errStreamClient{
		appendErr: fmt.Errorf("message_not_in_streaming_state"),
	}
	mgr := NewAgentStreamManager(mock, testLogger())

	ctx := context.Background()
	streamTS := mgr.StartSession(ctx, "worker-1", "C123", "thread-ts-1", "U12345")
	if streamTS == "" {
		t.Fatal("expected stream to start")
	}

	// Session should be active.
	if !mgr.HasSession("worker-1") {
		t.Fatal("expected active session before append")
	}

	// Record a tool event to make the session dirty.
	mgr.HandleToolEvent("worker-1", "Bash", "PreToolUse", "git status")

	// Flush triggers appendChunk which should detect the finalized error.
	mgr.flush(ctx)

	// Session should now be stopped — no longer active.
	if mgr.HasSession("worker-1") {
		t.Fatal("expected session to be stopped after message_not_in_streaming_state error")
	}

	// A second flush should NOT produce any more appendChunks calls.
	callsBefore := len(mock.getCalls())
	mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "file.go")
	mgr.flush(ctx)
	callsAfter := len(mock.getCalls())

	if callsAfter != callsBefore {
		t.Errorf("expected no more API calls after session stopped, got %d new calls", callsAfter-callsBefore)
	}
}

func TestIsStreamFinalized(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"unrelated error", fmt.Errorf("network timeout"), false},
		{"finalized error", fmt.Errorf("message_not_in_streaming_state"), true},
		{"wrapped finalized error", fmt.Errorf("slack API: message_not_in_streaming_state (code 123)"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStreamFinalized(tt.err); got != tt.want {
				t.Errorf("isStreamFinalized(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// coldRespawnSampleCount reads the running sample count of the
// gardenio_mention_cold_respawn_seconds histogram. CollectAndCount counts
// series (always 1 here), so we read the histogram's SampleCount directly.
func coldRespawnSampleCount(t *testing.T) uint64 {
	t.Helper()
	m := &dto.Metric{}
	if err := mentionColdRespawnSeconds.(prometheus.Metric).Write(m); err != nil {
		t.Fatalf("write cold-respawn histogram: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestHandleSessionStart_ClosesColdRespawnHistogram verifies that when an agent
// was marked as a cold respawn (MarkColdRespawn), its first SessionStart hook
// records exactly one observation into gardenio_mention_cold_respawn_seconds and
// clears the pending stamp so a subsequent SessionStart does not double-count.
// ka-0000001565.
func TestHandleSessionStart_ClosesColdRespawnHistogram(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	before := coldRespawnSampleCount(t)

	// Cold respawn began 3s ago; a stream session then started; SessionStart fires.
	mgr.MarkColdRespawn("cold-agent", time.Now().Add(-3*time.Second))
	mgr.StartSession(ctx, "cold-agent", "C123", "thread-ts-1", "U1")
	mgr.HandleSessionStart(ctx, "cold-agent", "resume")

	if after := coldRespawnSampleCount(t); after != before+1 {
		t.Fatalf("cold-respawn histogram sample count = %d, want %d (one observation)", after, before+1)
	}

	// A second SessionStart for the same agent must NOT add another sample —
	// the pending stamp was consumed by the first close.
	mgr.HandleSessionStart(ctx, "cold-agent", "resume")
	if after := coldRespawnSampleCount(t); after != before+1 {
		t.Errorf("cold-respawn histogram double-counted: sample count = %d, want %d", after, before+1)
	}
}

// TestHandleSessionStart_WarmDoesNotObserveColdHistogram verifies that an agent
// that never went through a cold respawn (no MarkColdRespawn) does not record a
// cold-respawn latency observation when its SessionStart fires. ka-0000001565.
func TestHandleSessionStart_WarmDoesNotObserveColdHistogram(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	before := coldRespawnSampleCount(t)

	mgr.StartSession(ctx, "warm-agent", "C123", "thread-ts-1", "U1")
	mgr.HandleSessionStart(ctx, "warm-agent", "startup")

	if after := coldRespawnSampleCount(t); after != before {
		t.Errorf("warm session recorded a cold-respawn observation: count went %d -> %d", before, after)
	}
}

// TestHandleSessionStart_BuffersLiveBeforeSession verifies that a SessionStart
// hook arriving BEFORE the streaming session is registered is buffered and
// applied when StartSession later creates the session — so the session is born
// live and the card promotes spawning→working immediately instead of waiting
// for the first tool event or the 60s SyncAll poll. ka-0000002206.
func TestHandleSessionStart_BuffersLiveBeforeSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	// SessionStart fires first — no session exists yet (the hook beat
	// NotifyAgentSpawn / prewarmed assignment / a bridge restart).
	mgr.HandleSessionStart(ctx, "race-agent", "startup")
	if stats := mgr.GetSessionStats("race-agent"); stats != nil {
		t.Fatalf("expected no session before StartSession, got %+v", stats)
	}

	// Now the session is registered — it must inherit the buffered live signal.
	mgr.StartSession(ctx, "race-agent", "C123", "thread-ts-1", "U1")
	stats := mgr.GetSessionStats("race-agent")
	if stats == nil {
		t.Fatal("expected a session after StartSession")
	}
	if !stats.Live {
		t.Error("session not marked live: buffered SessionStart signal was dropped")
	}
	// effectiveAgentState must promote the still-"spawning" bead to "working".
	if got := effectiveAgentState("spawning", stats); got != "working" {
		t.Errorf("effectiveAgentState(spawning, buffered-live) = %q, want working", got)
	}
}

// TestHandleSessionStart_BuffersLiveBeforeCardSession verifies the same buffer
// applies to card-only sessions (non-thread spawns). ka-0000002206.
func TestHandleSessionStart_BuffersLiveBeforeCardSession(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	ctx := context.Background()

	mgr.HandleSessionStart(ctx, "card-agent", "startup")
	mgr.StartCardSession("card-agent", "C123", "card-ts-1")

	stats := mgr.GetSessionStats("card-agent")
	if stats == nil {
		t.Fatal("expected a session after StartCardSession")
	}
	if !stats.Live {
		t.Error("card session not marked live: buffered SessionStart signal was dropped")
	}
}

// fakeResumeObserver records every RecordResumeLatency call so tests can assert
// the resume-latency tee's presence, label set, and count.
type fakeResumeObserver struct {
	calls []resumeObservation
}

type resumeObservation struct {
	seconds float64
	regime  string
	role    string
	runtime string
	path    string
}

func (f *fakeResumeObserver) RecordResumeLatency(seconds float64, regime, role, runtime, path string) bool {
	f.calls = append(f.calls, resumeObservation{seconds, regime, role, runtime, path})
	return true
}

// TestHandleSessionStart_TeesColdResumeLatency verifies that when a cold
// thread-agent resume is armed (MarkColdResume) and the shared resume histogram
// is wired (SetResumeMetrics), the first SessionStart tees exactly one
// gardenio_resume_latency_seconds{path=cold,regime=R0} sample carrying the
// captured role/runtime — alongside the cold-respawn histogram — and does not
// double-count on a second SessionStart. task ka-0000004119.
func TestHandleSessionStart_TeesColdResumeLatency(t *testing.T) {
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	fake := &fakeResumeObserver{}
	mgr.SetResumeMetrics(fake)
	ctx := context.Background()

	beforeCold := coldRespawnSampleCount(t)

	mgr.MarkColdResume("resume-agent", time.Now().Add(-2*time.Second), "thread", "runsc", "", "")
	mgr.StartSession(ctx, "resume-agent", "C1", "ts-1", "U1")
	mgr.HandleSessionStart(ctx, "resume-agent", "resume")

	// Cold-respawn histogram still gets its sample (the tee is additive).
	if after := coldRespawnSampleCount(t); after != beforeCold+1 {
		t.Fatalf("cold-respawn sample count = %d, want %d", after, beforeCold+1)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("resume-latency observations = %d, want 1: %+v", len(fake.calls), fake.calls)
	}
	got := fake.calls[0]
	if got.path != "cold" || got.regime != "R0" || got.role != "thread" || got.runtime != "runsc" {
		t.Errorf("resume-latency labels = %+v, want path=cold regime=R0 role=thread runtime=runsc", got)
	}
	if got.seconds < 1.5 || got.seconds > 60 {
		t.Errorf("resume-latency seconds = %v, want ~2s elapsed", got.seconds)
	}

	// A second SessionStart must NOT tee again — the armed stamp was consumed.
	mgr.HandleSessionStart(ctx, "resume-agent", "resume")
	if len(fake.calls) != 1 {
		t.Errorf("resume-latency double-teed: observations = %d, want 1", len(fake.calls))
	}
}

// TestHandleSessionStart_ColdResumeRuntimeDefaultsRunc verifies an empty runtime
// (a non-runsc channel resume) is teed as runtime=runc, the class that path runs
// under. task ka-0000004119.
func TestHandleSessionStart_ColdResumeRuntimeDefaultsRunc(t *testing.T) {
	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	fake := &fakeResumeObserver{}
	mgr.SetResumeMetrics(fake)
	ctx := context.Background()

	mgr.MarkColdResume("runc-agent", time.Now().Add(-1*time.Second), "", "", "", "")
	mgr.StartSession(ctx, "runc-agent", "C1", "ts-1", "U1")
	mgr.HandleSessionStart(ctx, "runc-agent", "resume")

	if len(fake.calls) != 1 {
		t.Fatalf("resume-latency observations = %d, want 1", len(fake.calls))
	}
	if got := fake.calls[0]; got.runtime != "runc" {
		t.Errorf("empty runtime should default to runc, got %q", got.runtime)
	}
	// role empty is passed through verbatim — RecordResumeLatency collapses it to
	// "unknown", so the tee must not pre-fill it.
	if got := fake.calls[0]; got.role != "" {
		t.Errorf("role should pass through empty (metric collapses to unknown), got %q", got.role)
	}
}

// TestHandleSessionStart_PlainColdRespawnDoesNotTee verifies a plain
// MarkColdRespawn (the frozen-awaiting-thaw case, whose thaw the controller
// records separately) closes the cold-respawn histogram but does NOT tee a
// resume-latency sample — so the bridge and controller emitters stay disjoint
// and never double-count the same wake. task ka-0000004119.
func TestHandleSessionStart_PlainColdRespawnDoesNotTee(t *testing.T) {
	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	fake := &fakeResumeObserver{}
	mgr.SetResumeMetrics(fake)
	ctx := context.Background()

	beforeCold := coldRespawnSampleCount(t)

	mgr.MarkColdRespawn("frozen-agent", time.Now().Add(-1*time.Second))
	mgr.StartSession(ctx, "frozen-agent", "C1", "ts-1", "U1")
	mgr.HandleSessionStart(ctx, "frozen-agent", "resume")

	if after := coldRespawnSampleCount(t); after != beforeCold+1 {
		t.Fatalf("cold-respawn sample count = %d, want %d", after, beforeCold+1)
	}
	if len(fake.calls) != 0 {
		t.Errorf("plain cold respawn must not tee resume-latency, got %+v", fake.calls)
	}
}

// TestMarkColdRespawnClearsStaleResumeArm verifies that a plain MarkColdRespawn
// overwriting a prior armed MarkColdResume for the same agent clears the arm, so
// the later respawn cannot leak a cold resume-latency sample it never intended.
// task ka-0000004119.
func TestMarkColdRespawnClearsStaleResumeArm(t *testing.T) {
	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	fake := &fakeResumeObserver{}
	mgr.SetResumeMetrics(fake)
	ctx := context.Background()

	// First a cold resume is armed, then the same agent takes a plain (frozen)
	// respawn before SessionStart — the arm must be cleared.
	mgr.MarkColdResume("agent-x", time.Now().Add(-5*time.Second), "thread", "runsc", "", "")
	mgr.MarkColdRespawn("agent-x", time.Now().Add(-1*time.Second))
	mgr.StartSession(ctx, "agent-x", "C1", "ts-1", "U1")
	mgr.HandleSessionStart(ctx, "agent-x", "resume")

	if len(fake.calls) != 0 {
		t.Errorf("stale resume arm leaked a sample after a plain respawn: %+v", fake.calls)
	}
}

// TestHandleSessionStart_NilResumeMetricsNoPanic verifies that with no resume
// metric wired (the default), an armed cold resume still closes the cold-respawn
// histogram and simply skips the tee. task ka-0000004119.
func TestHandleSessionStart_NilResumeMetricsNoPanic(t *testing.T) {
	mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
	ctx := context.Background()

	beforeCold := coldRespawnSampleCount(t)
	mgr.MarkColdResume("no-metric-agent", time.Now().Add(-1*time.Second), "thread", "runc", "", "")
	mgr.StartSession(ctx, "no-metric-agent", "C1", "ts-1", "U1")
	mgr.HandleSessionStart(ctx, "no-metric-agent", "resume")

	if after := coldRespawnSampleCount(t); after != beforeCold+1 {
		t.Errorf("cold-respawn sample count = %d, want %d (tee absence must not skip the cold histogram)", after, beforeCold+1)
	}
}
