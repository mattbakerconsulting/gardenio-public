package bridge

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"golang.org/x/time/rate"
)

// threadCardTerminalHarness wires a Bot whose thread agent already has a posted
// card, plus a fake Slack server that separately counts chat.update (card edit)
// and chat.postMessage (completion ping) calls. This lets the tests assert the
// Change-4 contract: at terminal state the card is edited in place exactly once
// and the one-line ping is posted only when the conditional gate opens.
type threadCardTerminalHarness struct {
	bot         *Bot
	daemon      *mockDaemon
	mu          *sync.Mutex
	updateCount *int // chat.update calls (card edits)
	postCount   *int // chat.postMessage calls (ping replies)
	lastPostTS  *string
	lastPosted  *string // thread_ts of the last postMessage
}

// newThreadCardTerminalHarness seeds a thread-bound agent bead and a cached
// card for it. createdAgo controls the run duration used by the conditional
// ping; wrapup is written to the bead's wrapup field.
func newThreadCardTerminalHarness(t *testing.T, mode string, createdAgo time.Duration, wrapup string, extraFields map[string]string) *threadCardTerminalHarness {
	t.Helper()

	fields := map[string]string{
		"agent":                "thread-1234-5678",
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts":      "1234.5678",
		"slack_card_channel":   "C-THREAD",
		"slack_card_ts":        "card.0001",
		"slack_user_id":        "U-OWNER",
		"slack_opener_user_id": "U-OWNER",
		"spawn_source":         "slack-thread",
		"project":              "gardenio",
	}
	if wrapup != "" {
		fields["wrapup"] = wrapup
	}
	maps.Copy(fields, extraFields)

	daemon := newMockDaemon()
	daemon.beads["thread-1234-5678"] = &beadsapi.BeadDetail{
		ID:        "thread-agent",
		Type:      "agent",
		Status:    "open",
		Title:     "thread-1234-5678",
		Fields:    fields,
		CreatedAt: time.Now().Add(-createdAgo),
	}

	var mu sync.Mutex
	updateCount, postCount := 0, 0
	var lastPostTS, lastPosted string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		switch r.URL.Path {
		case "/chat.update":
			updateCount++
		case "/chat.postMessage":
			postCount++
			lastPosted = r.FormValue("thread_ts")
			lastPostTS = "msg.0002"
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-THREAD", "ts": "msg.0002"})
	}))
	t.Cleanup(slackSrv.Close)

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"
	bot.threadCardsMode = mode
	bot.threadCardsConfig = DefaultThreadCardsConfig()
	bot.slackCardLimiter = rate.NewLimiter(rate.Inf, 1)
	// Pre-cache the card so resolveAgentCardRef resolves from the hot cache.
	bot.agentCards["thread-1234-5678"] = MessageRef{ChannelID: "C-THREAD", Timestamp: "card.0001", ThreadTS: "1234.5678", Agent: "thread-1234-5678"}
	bot.markThreadBound("thread-1234-5678")

	return &threadCardTerminalHarness{
		bot: bot, daemon: daemon, mu: &mu,
		updateCount: &updateCount, postCount: &postCount,
		lastPostTS: &lastPostTS, lastPosted: &lastPosted,
	}
}

func threadStateEvent(state, wrapup string) BeadEvent {
	fields := map[string]string{
		"agent":                "thread-1234-5678",
		"agent_state":          state,
		"slack_thread_channel": "C-THREAD",
		"slack_thread_ts":      "1234.5678",
		"slack_user_id":        "U-OWNER",
		"slack_opener_user_id": "U-OWNER",
		"spawn_source":         "slack-thread",
		"project":              "gardenio",
	}
	if wrapup != "" {
		fields["wrapup"] = wrapup
	}
	return BeadEvent{ID: "thread-agent", Type: "agent", Assignee: "thread-1234-5678", Title: "thread-1234-5678", Fields: fields}
}

// --- threadCardPingWanted (conditional ping gate, UX Q1 / D3) ---

func TestThreadCardPingWanted(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		createdAgo time.Duration
		threshold  int // PingThresholdSeconds; -1 keeps default
		wrapup     string
		want       bool
	}{
		{
			name:       "long run no MRs pings",
			createdAgo: 5 * time.Minute,
			threshold:  -1, // default 120s
			wrapup:     `{"accomplishments":"did work"}`,
			want:       true,
		},
		{
			name:       "short run no MRs does not ping",
			createdAgo: 10 * time.Second,
			threshold:  -1,
			wrapup:     `{"accomplishments":"quick"}`,
			want:       false,
		},
		{
			name:       "short run WITH MRs pings",
			createdAgo: 10 * time.Second,
			threshold:  -1,
			wrapup:     `{"accomplishments":"quick","pull_requests":["!1234"]}`,
			want:       true,
		},
		{
			name:       "threshold 0 mutes duration pings",
			createdAgo: 1 * time.Hour,
			threshold:  0,
			wrapup:     `{"accomplishments":"long but muted"}`,
			want:       false,
		},
		{
			name:       "threshold 0 still pings for MRs",
			createdAgo: 10 * time.Second,
			threshold:  0,
			wrapup:     `{"pull_requests":["!9"]}`,
			want:       true,
		},
		{
			name:       "missing duration fails closed",
			createdAgo: 0, // CreatedAt overwritten to zero below
			threshold:  -1,
			wrapup:     `{"accomplishments":"x"}`,
			want:       false,
		},
		{
			name:       "empty wrapup short run no ping",
			createdAgo: 5 * time.Second,
			threshold:  -1,
			wrapup:     "",
			want:       false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newThreadCardTerminalHarness(t, "owned", tt.createdAgo, tt.wrapup, nil)
			if tt.threshold >= 0 {
				h.bot.threadCardsConfig.PingThresholdSeconds = tt.threshold
			}
			if tt.name == "missing duration fails closed" {
				// Force an unavailable timestamp.
				h.daemon.beads["thread-1234-5678"].CreatedAt = time.Time{}
			}
			got := h.bot.threadCardPingWanted(context.Background(), "thread-1234-5678", threadStateEvent("done", tt.wrapup))
			if got != tt.want {
				t.Errorf("threadCardPingWanted = %v, want %v", got, tt.want)
			}
		})
	}
}

// --- reconcileThreadCardTerminal (edit-in-place + conditional ping) ---

// Long run → card edited in place once AND a single one-line ping posted.
func TestReconcileThreadCardTerminal_LongRunEditsAndPings(t *testing.T) {
	h := newThreadCardTerminalHarness(t, "owned", 5*time.Minute, `{"tldr":"shipped the fix","accomplishments":"did work"}`, nil)

	h.bot.reconcileThreadCardTerminal(context.Background(), "thread-1234-5678", "done",
		threadStateEvent("done", `{"tldr":"shipped the fix","accomplishments":"did work"}`),
		"C-THREAD", "1234.5678")

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.updateCount != 1 {
		t.Errorf("expected exactly 1 card edit (chat.update), got %d", *h.updateCount)
	}
	if *h.postCount != 1 {
		t.Errorf("expected exactly 1 completion ping (chat.postMessage), got %d", *h.postCount)
	}
	if *h.lastPosted != "1234.5678" {
		t.Errorf("expected ping posted in thread 1234.5678, got %q", *h.lastPosted)
	}
}

// Quick, MR-less run → card edited in place but NO ping (silent edit only).
func TestReconcileThreadCardTerminal_QuickRunEditsNoPing(t *testing.T) {
	h := newThreadCardTerminalHarness(t, "owned", 10*time.Second, `{"accomplishments":"quick"}`, nil)

	h.bot.reconcileThreadCardTerminal(context.Background(), "thread-1234-5678", "done",
		threadStateEvent("done", `{"accomplishments":"quick"}`),
		"C-THREAD", "1234.5678")

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.updateCount != 1 {
		t.Errorf("expected exactly 1 card edit, got %d", *h.updateCount)
	}
	if *h.postCount != 0 {
		t.Errorf("expected NO ping for quick MR-less run, got %d", *h.postCount)
	}
}

// Quick run WITH MRs → card edited AND ping posted (MRs always re-notify).
func TestReconcileThreadCardTerminal_MRsAlwaysPing(t *testing.T) {
	wrapup := `{"accomplishments":"quick","pull_requests":["!1234"]}`
	h := newThreadCardTerminalHarness(t, "owned", 5*time.Second, wrapup, nil)

	h.bot.reconcileThreadCardTerminal(context.Background(), "thread-1234-5678", "done",
		threadStateEvent("done", wrapup), "C-THREAD", "1234.5678")

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.updateCount != 1 {
		t.Errorf("expected exactly 1 card edit, got %d", *h.updateCount)
	}
	if *h.postCount != 1 {
		t.Errorf("expected a ping for an MR-bearing run, got %d", *h.postCount)
	}
}

// Failed/crash terminal state (UX Q7) → card edited to the failed terminal
// form; a long run still pings.
func TestReconcileThreadCardTerminal_FailedEditsCard(t *testing.T) {
	h := newThreadCardTerminalHarness(t, "owned", 10*time.Minute, "", nil)

	h.bot.reconcileThreadCardTerminal(context.Background(), "thread-1234-5678", "failed",
		threadStateEvent("failed", ""), "C-THREAD", "1234.5678")

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.updateCount != 1 {
		t.Errorf("expected the failed card to be edited in place once, got %d", *h.updateCount)
	}
	if *h.postCount != 1 {
		t.Errorf("expected a ping for a long failed run, got %d", *h.postCount)
	}
}

// --- End-to-end through NotifyAgentState ---

// Flag ON (owned) terminal transition → card edited in place; the legacy
// postThreadStateReply path is NOT taken (no full wrap-up reply, only the
// conditional one-line ping). This is the double-post-avoidance contract.
func TestNotifyAgentState_ThreadCard_OwnedEditsInPlace(t *testing.T) {
	wrapup := `{"tldr":"done","accomplishments":"work"}`
	h := newThreadCardTerminalHarness(t, "owned", 5*time.Minute, wrapup, nil)
	h.bot.agentState["thread-1234-5678"] = "working" // prevState

	h.bot.NotifyAgentState(context.Background(), threadStateEvent("done", wrapup))

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.updateCount < 1 {
		t.Errorf("expected the card to be edited in place, got %d updates", *h.updateCount)
	}
	// Exactly one postMessage: the one-line ping. The legacy path would have
	// posted a full wrap-up reply in addition — that's the double-post we avoid.
	if *h.postCount != 1 {
		t.Errorf("expected exactly 1 thread post (the ping), got %d", *h.postCount)
	}
}

// Flag OFF (default) terminal transition → legacy postThreadStateReply path;
// the thread-card reconciliation is NOT invoked (regression contract, doc 03 §4).
func TestNotifyAgentState_ThreadCard_OffUsesLegacyPath(t *testing.T) {
	wrapup := `{"accomplishments":"work"}`
	h := newThreadCardTerminalHarness(t, "off", 5*time.Minute, wrapup, nil)
	h.bot.agentState["thread-1234-5678"] = "working"

	h.bot.NotifyAgentState(context.Background(), threadStateEvent("done", wrapup))

	h.mu.Lock()
	defer h.mu.Unlock()
	// Legacy path posts a wrap-up reply (postThreadStateReply). It must fire;
	// the exact count is the legacy behavior we are preserving unchanged.
	if *h.postCount == 0 {
		t.Errorf("expected the legacy thread reply path to post when flag off, got %d", *h.postCount)
	}
}

// Flag ON (owned) but NON-owned thread → legacy path (gate fails closed), so the
// terminal reconciliation is skipped exactly as the spawn gate skipped posting.
func TestNotifyAgentState_ThreadCard_NonOwnedUsesLegacyPath(t *testing.T) {
	wrapup := `{"accomplishments":"work"}`
	h := newThreadCardTerminalHarness(t, "owned", 5*time.Minute, wrapup,
		map[string]string{"slack_opener_user_id": "U-SOMEONE-ELSE"})
	h.bot.agentState["thread-1234-5678"] = "working"

	ev := threadStateEvent("done", wrapup)
	ev.Fields["slack_opener_user_id"] = "U-SOMEONE-ELSE"
	h.bot.NotifyAgentState(context.Background(), ev)

	h.mu.Lock()
	defer h.mu.Unlock()
	if *h.postCount == 0 {
		t.Errorf("expected the legacy path for a non-owned thread, got %d posts", *h.postCount)
	}
}

// Sanity: the default ping threshold is 120s and round-trips through the
// config-bead JSON decode (ping_threshold_seconds).
func TestThreadCardsConfig_PingThresholdDefaultAndDecode(t *testing.T) {
	if got := DefaultThreadCardsConfig().PingThresholdSeconds; got != 120 {
		t.Errorf("default PingThresholdSeconds = %d, want 120", got)
	}
	// A config bead overriding only enabled leaves the threshold default intact.
	cfg := DefaultThreadCardsConfig()
	if err := json.Unmarshal([]byte(`{"enabled":true,"projects":["gardenio-e2e"]}`), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.PingThresholdSeconds != 120 {
		t.Errorf("threshold should stay default when JSON omits it, got %d", cfg.PingThresholdSeconds)
	}
	// An explicit override is honored.
	cfg2 := DefaultThreadCardsConfig()
	if err := json.Unmarshal([]byte(`{"enabled":true,"ping_threshold_seconds":30}`), &cfg2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg2.PingThresholdSeconds != 30 {
		t.Errorf("explicit threshold override = %d, want 30", cfg2.PingThresholdSeconds)
	}
}
