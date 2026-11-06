package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// These tests cover ka-0000000954: a prewarmed pool pod posted a top-level card
// in the default channel at boot (its SessionStart hook reaches updateAgentCard's
// create branch, the one card path the ka-0000007013 guards never covered), and
// the warm assignment that later grafted it onto a Slack thread posted no card
// at all — so the operator's thread stayed cardless while a stale card in
// #gardenio kept updating.

// poolCardRecorder is a fake Slack server that records chat.postMessage and
// chat.update calls so a test can tell "posted in thread" from "posted
// top-level" and see a stale card being compressed.
type poolCardRecorder struct {
	mu           sync.Mutex
	posts        []string // thread_ts of each post ("" = top-level)
	postChannels []string
	updates      int
}

func newPoolCardRecorder(t *testing.T) (*poolCardRecorder, *httptest.Server) {
	t.Helper()
	rec := &poolCardRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rec.mu.Lock()
		switch {
		case r.URL.Path == "/chat.postMessage":
			rec.posts = append(rec.posts, r.FormValue("thread_ts"))
			rec.postChannels = append(rec.postChannels, r.FormValue("channel"))
		case r.URL.Path == "/chat.update":
			rec.updates++
		}
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true, "channel": "C-THREAD", "ts": "card.0001", "message_ts": "card.0001",
		})
	}))
	t.Cleanup(srv.Close)
	return rec, srv
}

func (r *poolCardRecorder) snapshot() ([]string, []string, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	posts := append([]string(nil), r.posts...)
	channels := append([]string(nil), r.postChannels...)
	return posts, channels, r.updates
}

// seedPoolAgent registers a prewarmed-pool agent bead with the given extra
// fields (engagement markers), keyed by agent name so both FindAgentBead and
// FindAgentBeadAnyStatus resolve it.
func seedPoolAgent(daemon *mockDaemon, name string, extra map[string]string) {
	fields := map[string]string{"agent": name, "agent_state": "prewarmed", "role": "thread"}
	for k, v := range extra {
		fields[k] = v
	}
	daemon.beads[name] = &beadsapi.BeadDetail{
		ID: name, Type: "agent", Status: "open", Title: name,
		Labels: []string{"prewarmed", "project:gardenio", "role:thread"},
		Fields: fields,
	}
}

// An idle pool agent whose Claude session boots (SessionStart → RefreshAgentCard
// → updateAgentCard) must NOT get a card: nobody has engaged it yet.
func TestUpdateAgentCard_SkipsUnengagedPoolAgent(t *testing.T) {
	daemon := newMockDaemon()
	seedPoolAgent(daemon, "prewarmed-1", nil)

	rec, srv := newPoolCardRecorder(t)
	bot := newTestBot(daemon, srv)
	bot.channel = "C-DEFAULT"
	bot.threadingMode = "agent"

	bot.updateAgentCard(context.Background(), "prewarmed-1")

	if posts, _, _ := rec.snapshot(); len(posts) != 0 {
		t.Errorf("expected no card for an unengaged pool agent, got %d post(s)", len(posts))
	}
	if _, ok := bot.agentCards["prewarmed-1"]; ok {
		t.Error("expected no cached card for an unengaged pool agent")
	}
}

// Once the pool assigns the agent (here: to a task), the guard must stop
// applying — an engaged agent earns its card like any other.
func TestUpdateAgentCard_PostsForEngagedPoolAgent(t *testing.T) {
	daemon := newMockDaemon()
	seedPoolAgent(daemon, "prewarmed-2", map[string]string{"task_id": "gd-task-1", "agent_state": "working"})

	rec, srv := newPoolCardRecorder(t)
	bot := newTestBot(daemon, srv)
	bot.channel = "C-DEFAULT"
	bot.threadingMode = "agent"

	bot.updateAgentCard(context.Background(), "prewarmed-2")

	posts, _, _ := rec.snapshot()
	if len(posts) != 1 {
		t.Fatalf("expected one card post for an engaged pool agent, got %d", len(posts))
	}
	if posts[0] != "" {
		t.Errorf("expected a top-level card for a task-engaged agent, got thread_ts %q", posts[0])
	}
}

// A non-pool agent is untouched by the guard (no "prewarmed" label at all).
func TestUpdateAgentCard_PostsForNonPoolAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["solo-1"] = &beadsapi.BeadDetail{
		ID: "solo-1", Type: "agent", Status: "open", Title: "solo-1",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "solo-1", "agent_state": "working"},
	}

	rec, srv := newPoolCardRecorder(t)
	bot := newTestBot(daemon, srv)
	bot.channel = "C-DEFAULT"
	bot.threadingMode = "agent"

	bot.updateAgentCard(context.Background(), "solo-1")

	if posts, _, _ := rec.snapshot(); len(posts) != 1 {
		t.Fatalf("expected one card post for a non-pool agent, got %d", len(posts))
	}
}

// newWarmCardBot wires a Bot with thread cards in the given mode.
func newWarmCardBot(t *testing.T, mode string, daemon *mockDaemon, srv *httptest.Server) *Bot {
	t.Helper()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-DEFAULT"
	bot.threadingMode = "agent"
	bot.threadCardsMode = mode
	bot.threadCardsConfig = DefaultThreadCardsConfig()
	bot.streamMgr = NewAgentStreamManager(&mockStreamClient{}, slog.Default())
	bot.reactions = NewReactionLifecycle(bot.api, slog.Default())
	return bot
}

// The warm-assign card lands IN the thread, anchors the stream session, and is
// cached as a thread reply — parity with the cold path's NotifyAgentSpawn.
func TestPostWarmThreadCard_PostsInThread(t *testing.T) {
	daemon := newMockDaemon()
	seedPoolAgent(daemon, "prewarmed-3", map[string]string{
		"spawn_source":         "slack-thread-pool",
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts":      "1234.5678",
	})
	rec, srv := newPoolCardRecorder(t)
	bot := newWarmCardBot(t, "all", daemon, srv)

	bot.postWarmThreadCard(context.Background(), "prewarmed-3", "gardenio", "C-THREAD", "1234.5678", "U-OWNER", "U-OWNER")

	posts, channels, _ := rec.snapshot()
	if len(posts) != 1 {
		t.Fatalf("expected exactly one card post, got %d", len(posts))
	}
	if posts[0] != "1234.5678" {
		t.Errorf("expected card posted in thread 1234.5678, got thread_ts %q", posts[0])
	}
	if channels[0] != "C-THREAD" {
		t.Errorf("expected card in the thread channel, got %q", channels[0])
	}
	ref, ok := bot.agentCards["prewarmed-3"]
	if !ok {
		t.Fatal("expected the warm-assign card to be cached")
	}
	if ref.ThreadTS != "1234.5678" {
		t.Errorf("expected cached ThreadTS 1234.5678, got %q", ref.ThreadTS)
	}
	if bot.streamMgr.GetSessionStats("prewarmed-3") == nil {
		t.Error("expected a card-anchored stream session for the warm-assigned agent")
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if !bot.isThreadBound("prewarmed-3") {
		t.Error("expected the warm-assigned agent to be marked thread-bound")
	}
}

// A stale top-level card (posted before the pool guard landed, or by an older
// bridge) is MOVED into the thread, not duplicated: the old one is compressed.
func TestPostWarmThreadCard_MovesStaleTopLevelCard(t *testing.T) {
	daemon := newMockDaemon()
	seedPoolAgent(daemon, "prewarmed-4", map[string]string{"spawn_source": "slack-thread-pool"})
	rec, srv := newPoolCardRecorder(t)
	bot := newWarmCardBot(t, "all", daemon, srv)
	bot.agentCards["prewarmed-4"] = MessageRef{ChannelID: "C-DEFAULT", Timestamp: "stale.0001", Agent: "prewarmed-4"}

	bot.postWarmThreadCard(context.Background(), "prewarmed-4", "gardenio", "C-THREAD", "1234.5678", "U-OWNER", "U-OWNER")

	posts, _, updates := rec.snapshot()
	if updates != 1 {
		t.Errorf("expected the stale top-level card to be compressed once, got %d chat.update calls", updates)
	}
	if len(posts) != 1 || posts[0] != "1234.5678" {
		t.Fatalf("expected one in-thread card post, got %v", posts)
	}
	if ref := bot.agentCards["prewarmed-4"]; ref.ChannelID != "C-THREAD" {
		t.Errorf("expected the cached card to move to the thread channel, got %q", ref.ChannelID)
	}
}

// "owned" mode with a thread the spawner does not own: no card — but the agent
// is still marked thread-bound, so nothing posts a top-level card in its place.
func TestPostWarmThreadCard_OwnedGateFailsClosed(t *testing.T) {
	daemon := newMockDaemon()
	seedPoolAgent(daemon, "prewarmed-5", map[string]string{"spawn_source": "slack-thread-pool"})
	rec, srv := newPoolCardRecorder(t)
	bot := newWarmCardBot(t, "owned", daemon, srv)

	bot.postWarmThreadCard(context.Background(), "prewarmed-5", "gardenio", "C-THREAD", "1234.5678", "U-SOMEONE-ELSE", "U-OWNER")

	if posts, _, _ := rec.snapshot(); len(posts) != 0 {
		t.Errorf("expected no card for a non-owned thread in owned mode, got %d", len(posts))
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if !bot.isThreadBound("prewarmed-5") {
		t.Error("expected the agent to be marked thread-bound even when the card is gated off")
	}
}

// Thread cards off → no card at all (the pre-feature baseline), thread-bound
// still marked.
func TestPostWarmThreadCard_FeatureOff(t *testing.T) {
	daemon := newMockDaemon()
	seedPoolAgent(daemon, "prewarmed-6", map[string]string{"spawn_source": "slack-thread-pool"})
	rec, srv := newPoolCardRecorder(t)
	bot := newWarmCardBot(t, "off", daemon, srv)

	bot.postWarmThreadCard(context.Background(), "prewarmed-6", "gardenio", "C-THREAD", "1234.5678", "U-OWNER", "U-OWNER")

	if posts, _, _ := rec.snapshot(); len(posts) != 0 {
		t.Errorf("expected no card when thread cards are off, got %d", len(posts))
	}
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if !bot.isThreadBound("prewarmed-6") {
		t.Error("expected the agent to be marked thread-bound with the feature off")
	}
}

// End-to-end: a first mention that hits the warm pool posts the agent card in
// the operator's thread. This is the regression the operator reported — the
// pool-assign branch of handleThreadSpawn never posted one.
func TestThreadSpawn_PoolAssign_PostsThreadCard(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-THREAD")

	rec, slackSrv := newPoolCardRecorder(t)

	var gotRole string
	var mu sync.Mutex
	var called bool
	ctrlSrv := fakeControllerForAssign(t, true, &gotRole, &mu, &called)
	defer ctrlSrv.Close()

	bot := newWarmCardBot(t, "all", daemon, slackSrv)
	bot.botUserID = "U-BOT"
	bot.controllerURL = ctrlSrv.URL

	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Channel:         "C-THREAD",
		ThreadTimeStamp: "1234.5678",
		TimeStamp:       "1234.9999",
		Text:            "<@U-BOT> look at the card bug",
	}
	bot.handleThreadSpawn(context.Background(), ev, "look at the card bug")

	mu.Lock()
	assigned := called
	mu.Unlock()
	if !assigned {
		t.Fatal("expected the warm pool assign to be attempted")
	}

	posts, _, _ := rec.snapshot()
	found := false
	for _, ts := range posts {
		if ts == "1234.5678" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an in-thread agent card after a warm pool assign, got posts %v", posts)
	}
	ref, ok := bot.agentCards["monorepo-k6-perf-warm-1"]
	if !ok {
		t.Fatal("expected the warm-assigned agent to have a cached card")
	}
	if ref.ThreadTS != "1234.5678" {
		t.Errorf("expected the card cached as a thread reply, got ThreadTS %q", ref.ThreadTS)
	}
}
