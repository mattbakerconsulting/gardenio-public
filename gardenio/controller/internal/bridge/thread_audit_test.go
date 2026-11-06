package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- Dedup TTL Tests ---

func TestDedup_Cleanup_RemovesExpiredEntries(t *testing.T) {
	d := NewDedup(slog.Default())

	// Manually inject old entries.
	d.mu.Lock()
	d.seen["old:dec-1"] = time.Now().Add(-3 * time.Hour)
	d.seen["old:dec-2"] = time.Now().Add(-3 * time.Hour)
	d.seen["fresh:dec-3"] = time.Now()
	d.mu.Unlock()

	d.cleanup()

	if d.Len() != 1 {
		t.Errorf("expected 1 entry after cleanup, got %d", d.Len())
	}
	// The fresh entry should survive.
	if !d.Seen("fresh:dec-3") {
		t.Error("fresh entry should still be seen after cleanup")
	}
	// The old entries should be gone — Seen should return false (and re-add them).
	d.mu.Lock()
	_, hasOld1 := d.seen["old:dec-1"]
	_, hasOld2 := d.seen["old:dec-2"]
	d.mu.Unlock()
	// After cleanup they were removed; we haven't called Seen on them yet.
	if hasOld1 || hasOld2 {
		t.Error("old entries should have been removed by cleanup")
	}
}

func TestDedup_Cleanup_NoOpWhenEmpty(t *testing.T) {
	d := NewDedup(slog.Default())
	d.cleanup() // should not panic
	if d.Len() != 0 {
		t.Errorf("expected 0 entries, got %d", d.Len())
	}
}

func TestDedup_Cleanup_PreservesRecentEntries(t *testing.T) {
	d := NewDedup(slog.Default())
	d.Mark("recent:1")
	d.Mark("recent:2")
	d.Mark("recent:3")

	d.cleanup()

	if d.Len() != 3 {
		t.Errorf("expected 3 entries after cleanup of recent entries, got %d", d.Len())
	}
}

func TestDedup_StartCleanup_RunsUntilCancelled(t *testing.T) {
	d := NewDedup(slog.Default())

	// Inject an expired entry.
	d.mu.Lock()
	d.seen["expired:1"] = time.Now().Add(-3 * time.Hour)
	d.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// We can't wait for the real 10min interval, so just verify it starts
		// and stops without hanging.
		d.StartCleanup(ctx)
	}()

	// Cancel immediately — StartCleanup should return.
	cancel()
	wg.Wait()
}

func TestDedup_Len(t *testing.T) {
	d := NewDedup(slog.Default())
	if d.Len() != 0 {
		t.Fatalf("expected 0, got %d", d.Len())
	}
	d.Mark("a")
	d.Mark("b")
	if d.Len() != 2 {
		t.Fatalf("expected 2, got %d", d.Len())
	}
}

// --- Thread Binding Validation Tests ---

func TestIsValidThreadBinding(t *testing.T) {
	tests := []struct {
		name    string
		channel string
		ts      string
		want    bool
	}{
		{"valid", "C1234567890", "1234567890.123456", true},
		{"empty channel", "", "1234567890.123456", false},
		{"empty ts", "C1234567890", "", false},
		{"both empty", "", "", false},
		{"no dot in ts", "C1234567890", "1234567890", false},
		{"short channel", "C", "1234567890.123456", false},
		{"single char channel", "X", "1234.5678", false},
		{"valid D channel", "D1234567890", "1234.5678", true},
		{"valid G channel", "G1234567890", "1234.5678", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isValidThreadBinding(tt.channel, tt.ts)
			if got != tt.want {
				t.Errorf("isValidThreadBinding(%q, %q) = %v, want %v",
					tt.channel, tt.ts, got, tt.want)
			}
		})
	}
}

func TestResolveAgentThread_InvalidFields(t *testing.T) {
	daemon := newMockDaemon()

	// Agent with malformed thread binding (no dot in timestamp).
	daemon.beads["bad-thread-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-bad-thread",
		Title: "bad-thread-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "bad-thread-agent",
			"slack_thread_channel": "C1234",
			"slack_thread_ts":      "nodot",
		},
	}

	b := &Bot{
		daemon: daemon,
		logger: slog.Default(),
	}

	channel, ts := b.resolveAgentThread(context.Background(), "bad-thread-agent")
	if channel != "" || ts != "" {
		t.Errorf("expected empty for invalid binding, got channel=%q ts=%q", channel, ts)
	}
}

func TestResolveAgentThread_EmptyChannel(t *testing.T) {
	daemon := newMockDaemon()

	// Agent with empty channel.
	daemon.beads["empty-channel-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-empty-ch",
		Title: "empty-channel-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "empty-channel-agent",
			"slack_thread_channel": "",
			"slack_thread_ts":      "1234.5678",
		},
	}

	b := &Bot{
		daemon: daemon,
		logger: slog.Default(),
	}

	channel, ts := b.resolveAgentThread(context.Background(), "empty-channel-agent")
	if channel != "" || ts != "" {
		t.Errorf("expected empty for empty channel, got channel=%q ts=%q", channel, ts)
	}
}

// --- PostReport Fallback Tests ---

func TestPostReport_FallbackWhenMessageNotFound(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dec-1"] = &beadsapi.BeadDetail{
		ID:       "dec-1",
		Type:     "decision",
		Title:    "Deploy?",
		Priority: 2,
	}

	var postedPaths []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postedPaths = append(postedPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.history":
			// Return empty — message not found (deleted).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []any{},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"ts": "fallback-ts",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Pre-populate message ref (top-level, no thread).
	bot.messages["dec-1"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "1111.2222",
	}

	err := bot.PostReport(context.Background(), "rpt-1", "dec-1", "plan", "This is the plan content")
	if err != nil {
		t.Fatalf("PostReport should not error on fallback: %v", err)
	}

	// Should have called conversations.history (message not found) then chat.postMessage (fallback).
	hasHistory := false
	hasPost := false
	for _, p := range postedPaths {
		if p == "/conversations.history" {
			hasHistory = true
		}
		if p == "/chat.postMessage" {
			hasPost = true
		}
	}
	if !hasHistory {
		t.Error("expected conversations.history call")
	}
	if !hasPost {
		t.Error("expected fallback chat.postMessage call")
	}
}

func TestPostReport_FallbackWhenThreadMessageNotFound(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dec-2"] = &beadsapi.BeadDetail{
		ID:       "dec-2",
		Type:     "decision",
		Title:    "Review?",
		Priority: 2,
	}

	var postedPaths []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postedPaths = append(postedPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.replies":
			// Return empty replies — thread message not found.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []any{},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"ts": "fallback-ts",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Pre-populate message ref (in a thread).
	bot.messages["dec-2"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "2222.3333",
		ThreadTS:  "1111.0000",
	}

	err := bot.PostReport(context.Background(), "rpt-2", "dec-2", "checklist", "Checklist items")
	if err != nil {
		t.Fatalf("PostReport should not error on fallback: %v", err)
	}

	hasReplies := false
	hasPost := false
	for _, p := range postedPaths {
		if p == "/conversations.replies" {
			hasReplies = true
		}
		if p == "/chat.postMessage" {
			hasPost = true
		}
	}
	if !hasReplies {
		t.Error("expected conversations.replies call")
	}
	if !hasPost {
		t.Error("expected fallback chat.postMessage call")
	}
}

// TestPostReport_FallbackWhenDifferentMessageReturned verifies that PostReport
// falls back to a new message when conversations.history returns a different
// message (the target was deleted and Slack returns the nearest remaining one).
func TestPostReport_FallbackWhenDifferentMessageReturned(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dec-3"] = &beadsapi.BeadDetail{
		ID:       "dec-3",
		Type:     "decision",
		Title:    "Approve?",
		Priority: 2,
	}

	var postedPaths []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		postedPaths = append(postedPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.history":
			// Return a DIFFERENT message (the target was deleted, Slack
			// returns the nearest remaining message with a different ts).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []any{
					map[string]any{
						"ts":   "9999.0000",
						"text": "Some other message",
					},
				},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"ts": "fallback-ts",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Pre-populate message ref pointing to a message that was deleted.
	bot.messages["dec-3"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "1111.2222",
	}

	err := bot.PostReport(context.Background(), "rpt-3", "dec-3", "plan", "Plan content")
	if err != nil {
		t.Fatalf("PostReport should not error on fallback: %v", err)
	}

	// Should have called conversations.history (wrong ts) then chat.postMessage (fallback).
	hasHistory := false
	hasPost := false
	hasUpdate := false
	for _, p := range postedPaths {
		if p == "/conversations.history" {
			hasHistory = true
		}
		if p == "/chat.postMessage" {
			hasPost = true
		}
		if p == "/chat.update" {
			hasUpdate = true
		}
	}
	if !hasHistory {
		t.Error("expected conversations.history call")
	}
	if !hasPost {
		t.Error("expected fallback chat.postMessage call")
	}
	if hasUpdate {
		t.Error("should NOT have called chat.update (would modify wrong message)")
	}
}

func TestPostReport_NoMessageRef(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// No message ref for this decision — should return nil without error.
	err := bot.PostReport(context.Background(), "rpt-x", "nonexistent", "plan", "content")
	if err != nil {
		t.Errorf("expected nil error for missing message ref, got %v", err)
	}
}

func TestReportContentBlock_ShortContent(t *testing.T) {
	block := reportContentBlock("short report", "rpt-1")
	text := block.Text.Text
	if !strings.Contains(text, "short report") {
		t.Error("short content should be included verbatim")
	}
	if strings.Contains(text, "truncated") {
		t.Error("short content should not be truncated")
	}
}

func TestReportContentBlock_LongContentTruncated(t *testing.T) {
	// Build content that exceeds slackSectionTextMax when wrapped in code fences.
	long := strings.Repeat("x", slackSectionTextMax)
	block := reportContentBlock(long, "rpt-long")
	text := block.Text.Text
	if len(text) > slackSectionTextMax {
		t.Errorf("block text length %d exceeds Slack max %d", len(text), slackSectionTextMax)
	}
	if !strings.Contains(text, "truncated") {
		t.Error("long content should contain truncation notice")
	}
	if !strings.Contains(text, "rpt-long") {
		t.Error("truncation notice should reference the report bead ID")
	}
}

func TestPostReport_LongContentDoesNotFail(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["dec-long"] = &beadsapi.BeadDetail{
		ID:       "dec-long",
		Type:     "decision",
		Title:    "Long report decision",
		Priority: 2,
	}

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/conversations.history":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []any{},
			})
		case "/chat.postMessage":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"ts": "fallback-ts",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		}
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.messages["dec-long"] = MessageRef{
		ChannelID: "C123",
		Timestamp: "1111.2222",
	}

	longContent := strings.Repeat("This is a very long report. ", 200)
	err := bot.PostReport(context.Background(), "rpt-long", "dec-long", "report", longContent)
	if err != nil {
		t.Fatalf("PostReport with long content should not error: %v", err)
	}
}

// --- Periodic Pruning Tests ---

func TestStartPeriodicPrune_CancelStops(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		bot.startPeriodicPrune(ctx)
	}()

	// Cancel immediately — should return without hanging.
	cancel()
	wg.Wait()
}

// --- Agent Card BuildBlocks Tests ---

func TestBuildAgentCardBlocks_AllStates(t *testing.T) {
	states := []struct {
		state    string
		pending  int
		wantText string
	}{
		{"spawning", 0, "starting"},
		{"working", 0, "working"},
		{"working", 1, "decision pending"},
		{"working", 2, "2 decisions pending"},
		{"done", 0, "done"},
		{"failed", 0, "failed"},
		{"rate_limited", 0, "rate limited"},
		{"", 0, "idle"},
	}

	for _, tt := range states {
		t.Run(tt.state, func(t *testing.T) {
			blocks := buildAgentCardBlocks(Garden, "test-agent", "", tt.pending, tt.state, "", time.Now(), "", "", "", "", "", nil, "", false, triggerCardInfo{}, nil)
			if len(blocks) == 0 {
				t.Fatal("expected blocks")
			}
		})
	}
}

// --- State Thread Agent Tests (additional coverage) ---

func TestStateManager_ThreadAgents_ConcurrentAccess(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ch := "C-test"
			ts := "1111.2222"
			_ = state.SetThreadAgent(ch, ts, "agent")
			state.GetThreadAgent(ch, ts)
		}(i)
	}
	wg.Wait()
}

// --- Extract Helpers Tests ---

// --- fetchThreadContext Tests ---

func TestFetchThreadContext_EmptyThreadTS_ReturnsPlaceholder(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	result, rootAuthor := bot.fetchThreadContext(context.Background(), "D1234567890", "")
	if result != "(no thread context — top-level message)" {
		t.Errorf("expected placeholder for empty threadTS, got: %s", result)
	}
	if rootAuthor != "" {
		t.Errorf("expected empty rootAuthor for empty threadTS, got: %s", rootAuthor)
	}
}

func TestFetchThreadContext_DMChannel_FetchesReplies(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{"user": "U111", "text": "help me with this", "ts": "1234.0001"},
					{"user": "U222", "text": "sure thing", "ts": "1234.0002"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	result, rootAuthor := bot.fetchThreadContext(context.Background(), "D1234567890", "1234.0001")
	if result == "(could not fetch thread context)" {
		t.Error("expected successful fetch for DM channel, got error placeholder")
	}
	if result == "(empty thread)" {
		t.Error("expected non-empty thread context")
	}
	if result == "" {
		t.Error("expected non-empty result")
	}
	// The root author is the first message's user (the thread opener).
	if rootAuthor != "U111" {
		t.Errorf("expected rootAuthor U111 (thread root), got: %s", rootAuthor)
	}
}
