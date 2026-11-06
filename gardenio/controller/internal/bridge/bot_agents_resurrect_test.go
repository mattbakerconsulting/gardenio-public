package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestUpdateAgentCard_ResurrectsFromState verifies that when an agent card
// has been pruned from the hot cache but still exists in persisted state,
// updateAgentCard resurrects it instead of posting a duplicate card.
func TestUpdateAgentCard_ResurrectsFromState(t *testing.T) {
	// Track Slack API calls to detect new card creation.
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.0001"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	// Persist an agent card in the state file (simulating a card that was
	// created, then pruneStaleAgentCards removed it from the hot cache).
	stateDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	persistedRef := MessageRef{ChannelID: "C-TEST", Timestamp: "1111.2222"}
	if err := sm.SetAgentCard("test-agent", persistedRef); err != nil {
		t.Fatal(err)
	}
	bot.state = sm

	// Hot cache is empty — no card for "test-agent".
	bot.agentState["test-agent"] = "working"

	// Enable agent threading so the fallback path in updateAgentCard would
	// try to create a new card if resurrection fails.
	bot.threadingMode = "agent"

	// Call updateAgentCard — should resurrect from state, not post new.
	bot.updateAgentCard(context.Background(), "test-agent")

	// Verify the card was resurrected into the hot cache.
	bot.mu.Lock()
	ref, ok := bot.agentCards["test-agent"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected agent card to be resurrected into hot cache")
	}
	if ref.Timestamp != "1111.2222" {
		t.Errorf("expected timestamp 1111.2222, got %s", ref.Timestamp)
	}
	if ref.ChannelID != "C-TEST" {
		t.Errorf("expected channel C-TEST, got %s", ref.ChannelID)
	}

	// chat.postMessage should NOT have been called (no new card).
	if n := postCount.Load(); n != 0 {
		t.Errorf("expected 0 chat.postMessage calls (no new card), got %d", n)
	}
}

// TestEnsureAgentCard_ResurrectsFromState verifies that ensureAgentCard
// checks persisted state before posting a new Slack message.
func TestEnsureAgentCard_ResurrectsFromState(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.0002"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	// Persist an agent card in state.
	stateDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	persistedRef := MessageRef{ChannelID: "C-TEST", Timestamp: "3333.4444"}
	if err := sm.SetAgentCard("test-agent", persistedRef); err != nil {
		t.Fatal(err)
	}
	bot.state = sm

	// Hot cache is empty — no card for "test-agent".

	// Call ensureAgentCard with the same channel as the persisted card.
	ts, err := bot.ensureAgentCard(context.Background(), "test-agent", "C-TEST")
	if err != nil {
		t.Fatal(err)
	}

	// Should return the persisted card's timestamp, not a new one.
	if ts != "3333.4444" {
		t.Errorf("expected timestamp 3333.4444 from resurrected card, got %s", ts)
	}

	// Verify the card was resurrected into the hot cache.
	bot.mu.Lock()
	ref, ok := bot.agentCards["test-agent"]
	bot.mu.Unlock()
	if !ok {
		t.Fatal("expected agent card to be resurrected into hot cache")
	}
	if ref.Timestamp != "3333.4444" {
		t.Errorf("expected timestamp 3333.4444, got %s", ref.Timestamp)
	}

	// No new Slack message should have been posted.
	if n := postCount.Load(); n != 0 {
		t.Errorf("expected 0 chat.postMessage calls, got %d", n)
	}
}

// TestEnsureAgentCard_DifferentChannel_DoesNotResurrect verifies that
// ensureAgentCard does NOT resurrect a card from state if the persisted
// card is in a different channel than the requested channel.
func TestEnsureAgentCard_DifferentChannel_DoesNotResurrect(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "5555.6666"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	bot := newTestBot(daemon, slackSrv)

	// Persist a card in channel C-OLD.
	stateDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(stateDir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SetAgentCard("test-agent", MessageRef{ChannelID: "C-OLD", Timestamp: "7777.8888"}); err != nil {
		t.Fatal(err)
	}
	bot.state = sm

	// Request a card in C-NEW — channel mismatch, should post a new card.
	ts, err := bot.ensureAgentCard(context.Background(), "test-agent", "C-NEW")
	if err != nil {
		t.Fatal(err)
	}

	// Should get the new message's timestamp from the fake server.
	if ts != "5555.6666" {
		t.Errorf("expected new card timestamp 5555.6666, got %s", ts)
	}

	// A new card SHOULD have been posted since channels differ.
	if n := postCount.Load(); n != 1 {
		t.Errorf("expected 1 chat.postMessage call for new card, got %d", n)
	}
}

// TestEnsureAgentCard_ResurrectsFromBeadFields is the redeploy regression
// (ka-0000005475). In prod the bridge state file lives on an emptyDir, so a
// redeploy wipes BOTH the hot cache and persisted state — the two tiers the
// tests above cover. The agent bead's slack_card_* fields are the only record
// that survives, and before this tier existed on the post path the bridge
// posted a fresh card per restart: the FSM-476 triage agent accumulated four
// cards across three bridge releases in 16 minutes.
func TestEnsureAgentCard_ResurrectsFromBeadFields(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.0003"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{
		ID:   "bead-test-agent",
		Type: "agent",
		Fields: map[string]string{
			"slack_card_channel": "C-TEST",
			"slack_card_ts":      "1212.3434",
		},
	}
	bot := newTestBot(daemon, slackSrv)

	// Post-redeploy state: empty hot cache AND no state manager at all, which
	// is what an emptyDir-backed state file looks like on a fresh pod.
	bot.state = nil

	ts, err := bot.ensureAgentCard(context.Background(), "test-agent", "C-TEST")
	if err != nil {
		t.Fatal(err)
	}
	if ts != "1212.3434" {
		t.Errorf("expected card ts 1212.3434 from bead fields, got %s", ts)
	}

	bot.mu.Lock()
	ref, ok := bot.agentCards["test-agent"]
	bot.mu.Unlock()
	if !ok || ref.Timestamp != "1212.3434" || ref.ChannelID != "C-TEST" {
		t.Fatalf("expected card resurrected into hot cache, got ok=%v ref=%+v", ok, ref)
	}

	// The whole point: no duplicate card.
	if n := postCount.Load(); n != 0 {
		t.Errorf("expected 0 chat.postMessage calls (card resurrected from bead), got %d", n)
	}
}

// TestEnsureAgentCard_BeadFieldsDifferentChannel_DoesNotResurrect verifies the
// bead tier honours the same channel guard as the state tier — a card recorded
// in another channel must not be adopted for a post into this one.
func TestEnsureAgentCard_BeadFieldsDifferentChannel_DoesNotResurrect(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "4141.5151"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{
		ID:   "bead-test-agent",
		Type: "agent",
		Fields: map[string]string{
			"slack_card_channel": "C-OLD",
			"slack_card_ts":      "1212.3434",
		},
	}
	bot := newTestBot(daemon, slackSrv)
	bot.state = nil

	ts, err := bot.ensureAgentCard(context.Background(), "test-agent", "C-NEW")
	if err != nil {
		t.Fatal(err)
	}
	if ts != "4141.5151" {
		t.Errorf("expected a newly posted card ts, got %s", ts)
	}
	if n := postCount.Load(); n != 1 {
		t.Errorf("expected 1 chat.postMessage call for the channel change, got %d", n)
	}
}

// TestEnsureAgentCard_ThreadFieldsAreNotACard guards the tier split: a
// thread-spawned agent bead carries slack_thread_* pointing at the TRIGGERING
// (often human) message. That is not a bridge-owned card, so the post path
// must never adopt it — doing so would make the bridge try to edit a user's
// message. Only slack_card_* counts here; resolveAgentCardRef keeps the
// thread fallback for the wrap-up path.
func TestEnsureAgentCard_ThreadFieldsAreNotACard(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "6161.7171"})
	}))
	defer slackSrv.Close()

	daemon := newMockDaemon()
	daemon.beads["test-agent"] = &beadsapi.BeadDetail{
		ID:   "bead-test-agent",
		Type: "agent",
		Fields: map[string]string{
			"slack_thread_channel": "C-TEST",
			"slack_thread_ts":      "8181.9191",
		},
	}
	bot := newTestBot(daemon, slackSrv)
	bot.state = nil

	ts, err := bot.ensureAgentCard(context.Background(), "test-agent", "C-TEST")
	if err != nil {
		t.Fatal(err)
	}
	if ts == "8181.9191" {
		t.Fatal("post path adopted slack_thread_ts as a card — it would edit the user's message")
	}
	if ts != "6161.7171" {
		t.Errorf("expected a newly posted card ts, got %s", ts)
	}
	if n := postCount.Load(); n != 1 {
		t.Errorf("expected 1 chat.postMessage call, got %d", n)
	}
}
