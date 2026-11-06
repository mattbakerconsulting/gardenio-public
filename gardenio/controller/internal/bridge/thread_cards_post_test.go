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

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

// --- threadCardOwned (UX Q4 ownership gate) ---

func TestThreadCardOwned(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{
			name:   "owner == spawner is owned",
			fields: map[string]string{"slack_opener_user_id": "U1", "slack_user_id": "U1"},
			want:   true,
		},
		{
			name:   "owner != spawner is not owned",
			fields: map[string]string{"slack_opener_user_id": "U1", "slack_user_id": "U2"},
			want:   false,
		},
		{
			name:   "missing opener fails closed",
			fields: map[string]string{"slack_user_id": "U1"},
			want:   false,
		},
		{
			name:   "both empty fails closed",
			fields: map[string]string{},
			want:   false,
		},
	}
	b := &Bot{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := b.threadCardOwned(BeadEvent{Fields: tt.fields})
			if got != tt.want {
				t.Errorf("threadCardOwned(%v)=%v, want %v", tt.fields, got, tt.want)
			}
		})
	}
}

// threadCardSpawnHarness wires a Bot with a thread-bound agent bead and a fake
// Slack server that records whether a card was posted in-thread.
type threadCardSpawnHarness struct {
	bot          *Bot
	daemon       *mockDaemon
	mu           *sync.Mutex
	postedThread *string // thread_ts of any posted card; "" means top-level
	postCount    *int
}

func newThreadCardSpawnHarness(t *testing.T, mode string, fields map[string]string) *threadCardSpawnHarness {
	t.Helper()
	daemon := newMockDaemon()
	daemon.beads["thread-agent"] = &beadsapi.BeadDetail{
		ID: "thread-agent", Type: "agent", Status: "open",
		Title:  "thread-1234-5678",
		Fields: fields,
	}

	var mu sync.Mutex
	var postedThread string
	postCount := 0
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postCount++
			postedThread = r.FormValue("thread_ts")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-THREAD", "ts": "card.0001"})
	}))
	t.Cleanup(slackSrv.Close)

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"
	bot.threadCardsMode = mode
	bot.threadCardsConfig = DefaultThreadCardsConfig()
	bot.streamMgr = NewAgentStreamManager(&mockStreamClient{}, slog.Default())
	bot.reactions = NewReactionLifecycle(bot.api, slog.Default())

	return &threadCardSpawnHarness{
		bot: bot, daemon: daemon, mu: &mu,
		postedThread: &postedThread, postCount: &postCount,
	}
}

func ownedThreadFields() map[string]string {
	return map[string]string{
		"agent":                "thread-1234-5678",
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts":      "1234.5678",
		"slack_user_id":        "U-OWNER",
		"slack_opener_user_id": "U-OWNER",
		"spawn_source":         "slack-thread",
		"project":              "gardenio",
	}
}

func spawnEvent(fields map[string]string) BeadEvent {
	return BeadEvent{ID: "thread-agent", Type: "agent", Title: "thread-1234-5678", Fields: fields}
}

// Flag ON + owned thread → card posted in-thread, card session + reactions on card.
func TestNotifyAgentSpawn_ThreadCard_OwnedPostsCard(t *testing.T) {
	h := newThreadCardSpawnHarness(t, "owned", ownedThreadFields())

	h.bot.NotifyAgentSpawn(context.Background(), spawnEvent(ownedThreadFields()))

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.postCount != 1 {
		t.Fatalf("expected exactly one card post, got %d", *h.postCount)
	}
	if *h.postedThread != "1234.5678" {
		t.Errorf("expected card posted in thread 1234.5678, got %q", *h.postedThread)
	}
	// A card-anchored stream session should exist (tool stats follow the card).
	if h.bot.streamMgr.GetSessionStats("thread-1234-5678") == nil {
		t.Error("expected a card stream session for the thread card agent")
	}
	// The card should be cached as a thread reply.
	ref, ok := h.bot.agentCards["thread-1234-5678"]
	if !ok {
		t.Fatal("expected agent card cached")
	}
	if ref.ThreadTS != "1234.5678" {
		t.Errorf("expected cached ThreadTS 1234.5678, got %q", ref.ThreadTS)
	}
}

// Flag OFF (default) → NO card for a thread agent; behavior == baseline
// (regression contract, doc 03 §4).
func TestNotifyAgentSpawn_ThreadCard_OffNoCard(t *testing.T) {
	h := newThreadCardSpawnHarness(t, "off", ownedThreadFields())

	h.bot.NotifyAgentSpawn(context.Background(), spawnEvent(ownedThreadFields()))

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.postCount != 0 {
		t.Errorf("expected no card post when flag off, got %d", *h.postCount)
	}
	if _, ok := h.bot.agentCards["thread-1234-5678"]; ok {
		t.Error("expected no cached card when flag off")
	}
}

// Flag ON (owned) + NON-owned thread → no card (gating fails closed).
func TestNotifyAgentSpawn_ThreadCard_NonOwnedNoCard(t *testing.T) {
	fields := ownedThreadFields()
	fields["slack_opener_user_id"] = "U-SOMEONE-ELSE" // root authored by another user
	h := newThreadCardSpawnHarness(t, "owned", fields)

	h.bot.NotifyAgentSpawn(context.Background(), spawnEvent(fields))

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.postCount != 0 {
		t.Errorf("expected no card post for non-owned thread in owned mode, got %d", *h.postCount)
	}
}

// Flag ON (all) + NON-owned thread → card posted (ownership check bypassed).
func TestNotifyAgentSpawn_ThreadCard_AllModeBypassesOwnership(t *testing.T) {
	fields := ownedThreadFields()
	fields["slack_opener_user_id"] = "U-SOMEONE-ELSE"
	h := newThreadCardSpawnHarness(t, "all", fields)

	h.bot.NotifyAgentSpawn(context.Background(), spawnEvent(fields))

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.postCount != 1 {
		t.Errorf("expected card post in 'all' mode regardless of ownership, got %d", *h.postCount)
	}
	if *h.postedThread != "1234.5678" {
		t.Errorf("expected card in thread 1234.5678, got %q", *h.postedThread)
	}
}

// Flag ON (owned) but project not in allow-list → no card.
func TestNotifyAgentSpawn_ThreadCard_AllowListGates(t *testing.T) {
	h := newThreadCardSpawnHarness(t, "owned", ownedThreadFields())
	h.bot.threadCardsConfig = ThreadCardsConfig{Enabled: true, Projects: []string{"gardenio-e2e"}}

	h.bot.NotifyAgentSpawn(context.Background(), spawnEvent(ownedThreadFields()))

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.postCount != 0 {
		t.Errorf("expected no card post when project not in allow-list, got %d", *h.postCount)
	}
}

// --- Change 3: compact-while-working rendering for thread cards ---

func TestBuildCompactWorkingCardBlocks_HasToolStatsNoHeavyContext(t *testing.T) {
	stats := &AgentToolStats{TotalTools: 5, Activity: "running commands", LastTool: "Bash"}
	blocks := buildCompactWorkingCardBlocks(Garden, "my-agent", "gd-ABC", "working", "", "crew", "", stats)

	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (header + context), got %d", len(blocks))
	}
	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, "5 tools") {
		t.Errorf("expected tool stats in compact working header, got: %s", header.Text.Text)
	}
	// Heavy context (decisions wrench / cost / version lines) lives only in the
	// full card. The compact context block should carry just the slug line.
	ctxText := contextBlockText(t, blocks[1])
	if strings.Contains(ctxText, Garden.Tool(ToolDefault)) || strings.Contains(ctxText, Garden.Semantic(SemCost)) {
		t.Errorf("expected compact card to omit heavy context lines, got: %s", ctxText)
	}
	if !strings.Contains(ctxText, "Decisions thread below") {
		t.Errorf("expected compact context line, got: %s", ctxText)
	}
}

func TestBuildCompactWorkingCardBlocks_Spawning(t *testing.T) {
	blocks := buildCompactWorkingCardBlocks(Garden, "my-agent", "", "spawning", "", "", "", nil)
	header := blocks[0].(*slack.SectionBlock)
	if !strings.Contains(header.Text.Text, "starting") {
		t.Errorf("expected 'starting' status for spawning state, got: %s", header.Text.Text)
	}
}

// flushAgentCard selects the compact layout for a thread card while working,
// and the full layout once terminal. We capture the chat.update payload and
// count its blocks: compact == 2 blocks, full terminal == 3 (incl. Clear).
func TestFlushAgentCard_ThreadCardCompactWhileWorking(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var lastBlockCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			var blocks []json.RawMessage
			_ = json.Unmarshal([]byte(r.FormValue("blocks")), &blocks)
			mu.Lock()
			lastBlockCount = len(blocks)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.streamMgr = NewAgentStreamManager(&mockStreamClient{}, slog.Default())
	// A thread card: ThreadTS set marks it as posted in a thread reply.
	bot.agentCards["tc-agent"] = MessageRef{ChannelID: "C-THREAD", Timestamp: "card.1", ThreadTS: "1234.5678"}

	// Working state → compact layout (2 blocks).
	bot.agentState["tc-agent"] = "working"
	bot.flushAgentCard(context.Background(), "tc-agent")
	mu.Lock()
	working := lastBlockCount
	mu.Unlock()
	if working != 2 {
		t.Errorf("expected 2 blocks (compact) for working thread card, got %d", working)
	}

	// Terminal state → full layout with Clear button (3 blocks).
	bot.agentState["tc-agent"] = "done"
	bot.flushAgentCard(context.Background(), "tc-agent")
	mu.Lock()
	terminal := lastBlockCount
	mu.Unlock()
	if terminal != 3 {
		t.Errorf("expected 3 blocks (full + Clear) for terminal thread card, got %d", terminal)
	}
}

// A non-thread card (no ThreadTS) keeps the full layout even while working —
// the compact-while-working behavior is scoped to thread cards only.
func TestFlushAgentCard_NonThreadCardStaysFull(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var lastBody string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			mu.Lock()
			lastBody = r.FormValue("blocks")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.streamMgr = NewAgentStreamManager(&mockStreamClient{}, slog.Default())
	// No ThreadTS → top-level card.
	bot.agentCards["flat-agent"] = MessageRef{ChannelID: "C123", Timestamp: "card.2"}
	bot.agentState["flat-agent"] = "working"
	// The image tag only renders in the FULL builder's context block; the
	// compact builder omits it. Its presence proves the full path was taken.
	bot.agentImageTag["flat-agent"] = "v2026.99.9"

	bot.flushAgentCard(context.Background(), "flat-agent")
	mu.Lock()
	body := lastBody
	mu.Unlock()
	if !strings.Contains(body, "v2026.99.9") {
		t.Errorf("expected full card (image tag present) for non-thread working card, got: %s", body)
	}
}

// handleThreadSpawn records the thread root author as slack_opener_user_id on
// the cold-start agent bead, so the thread-cards ownership gate (UX Q4) can tell
// whether the spawner owns the thread.
func TestHandleThreadSpawn_SetsOpenerFromRootAuthor(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			// Thread root authored by U-ROOT (different from the spawner U-SPAWN).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok": true,
				"messages": []map[string]any{
					{"user": "U-ROOT", "text": "original question", "ts": "5555.0000"},
					{"user": "U-SPAWN", "text": "<@U-BOT> help", "ts": "5555.0001"},
				},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot := newTestBot(daemon, slackSrv)
	bot.state = state

	bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-SPAWN",
		Text:            "<@U-BOT> project:gardenio help",
		TimeStamp:       "5555.0002",
		ThreadTimeStamp: "5555.0000",
		Channel:         "C-THREAD",
	}, "project:gardenio help")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["slack_opener_user_id"] != "U-ROOT" {
			t.Errorf("expected slack_opener_user_id=U-ROOT (thread root author), got %q", b.Fields["slack_opener_user_id"])
		}
		if b.Fields["slack_user_id"] != "U-SPAWN" {
			t.Errorf("expected slack_user_id=U-SPAWN (spawner), got %q", b.Fields["slack_user_id"])
		}
	}
}

// contextBlockText concatenates the text of all elements in a Slack context block.
func contextBlockText(t *testing.T, block slack.Block) string {
	t.Helper()
	cb, ok := block.(*slack.ContextBlock)
	if !ok {
		t.Fatalf("expected a context block, got %T", block)
	}
	var sb strings.Builder
	for _, elem := range cb.ContextElements.Elements {
		if textObj, ok := elem.(*slack.TextBlockObject); ok {
			sb.WriteString(textObj.Text)
		}
	}
	return sb.String()
}
