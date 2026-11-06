package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
	"golang.org/x/time/rate"
)

// TestPruneStaleAgentCards_CompressesClosedAgents verifies that agent cards for
// agents whose beads are no longer active (closed) are compressed on startup.
func TestPruneStaleAgentCards_CompressesClosedAgents(t *testing.T) {
	daemon := newMockDaemon()

	// Seed one active agent (bead is open, state=working).
	daemon.beads["bd-active"] = &beadsapi.BeadDetail{
		ID:    "bd-active",
		Title: "active-bot",
		Type:  "agent",
		Fields: map[string]string{
			"agent":       "active-bot",
			"project":     "gardenio",
			"role":        "crew",
			"agent_state": "working",
		},
	}
	// No bead for "dead-bot" — simulates a closed agent whose bead is gone.

	var updatedMessages []string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			updatedMessages = append(updatedMessages, r.FormValue("ts"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Pre-populate agent cards as if hydrated from state file.
	bot.agentCards["active-bot"] = MessageRef{ChannelID: "C123", Timestamp: "1111.1111"}
	bot.agentCards["dead-bot"] = MessageRef{ChannelID: "C123", Timestamp: "2222.2222"}

	// Seed the per-agent sidecar maps that must be swept alongside the card
	// (regression guard for gd-WL16L8LwxF: these three were leaking one entry
	// per agent forever because pruneStaleAgentCards never deleted them).
	bot.agentSchedulingBlocked["dead-bot"] = true
	bot.agentSeen["dead-bot"] = time.Now()
	bot.agentSpawnChannel["dead-bot"] = "C123"
	bot.rememberNoCardBead("dead-bot")

	bot.pruneStaleAgentCards(context.Background())

	// Active bot's card should remain unchanged.
	if _, ok := bot.agentCards["active-bot"]; !ok {
		t.Error("active agent card should not be pruned")
	}

	// Dead bot's card should be removed from in-memory map after compression.
	if _, ok := bot.agentCards["dead-bot"]; ok {
		t.Error("stale agent card should be removed from agentCards after compression")
	}

	// The sidecar per-agent maps must also be swept — otherwise they leak one
	// entry per agent forever on the always-on bridge (gd-WL16L8LwxF).
	if _, ok := bot.agentSchedulingBlocked["dead-bot"]; ok {
		t.Error("agentSchedulingBlocked entry should be removed for stale agent")
	}
	if _, ok := bot.agentSeen["dead-bot"]; ok {
		t.Error("agentSeen entry should be removed for stale agent")
	}
	if _, ok := bot.agentSpawnChannel["dead-bot"]; ok {
		t.Error("agentSpawnChannel entry should be removed for stale agent")
	}
	// So must the card-bead memo (bridge/conversation.go conv.cards / noCard).
	if _, ok := bot.agentCardMemo("dead-bot"); ok || bot.noCardBead("dead-bot") {
		t.Error("card-bead memo should be forgotten for stale agent")
	}

	// Slack update should have been called for the dead bot's message.
	if len(updatedMessages) != 1 {
		t.Fatalf("expected 1 Slack message updated, got %d", len(updatedMessages))
	}
	if updatedMessages[0] != "2222.2222" {
		t.Errorf("expected updated timestamp 2222.2222, got %s", updatedMessages[0])
	}
}

// TestPruneStaleAgentCards_CompressesDoneAgents verifies that agent cards for
// agents with agent_state=done (bead still open) are compressed on restart.
func TestPruneStaleAgentCards_CompressesDoneAgents(t *testing.T) {
	daemon := newMockDaemon()

	// Agent with state=done but bead still open.
	daemon.beads["bd-done"] = &beadsapi.BeadDetail{
		ID:    "bd-done",
		Title: "done-bot",
		Type:  "agent",
		Fields: map[string]string{
			"agent":       "done-bot",
			"project":     "gardenio",
			"role":        "crew",
			"agent_state": "done",
		},
	}
	// Agent with state=working (should be kept).
	daemon.beads["bd-working"] = &beadsapi.BeadDetail{
		ID:    "bd-working",
		Title: "working-bot",
		Type:  "agent",
		Fields: map[string]string{
			"agent":       "working-bot",
			"project":     "gardenio",
			"role":        "crew",
			"agent_state": "working",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["done-bot"] = MessageRef{ChannelID: "C123", Timestamp: "3333.3333"}
	bot.agentCards["working-bot"] = MessageRef{ChannelID: "C123", Timestamp: "4444.4444"}
	bot.agentState["done-bot"] = "done"

	bot.pruneStaleAgentCards(context.Background())

	// Done bot's card should be removed from in-memory map after compression.
	if _, ok := bot.agentCards["done-bot"]; ok {
		t.Error("done agent card should be removed from agentCards after compression")
	}
	if _, ok := bot.agentCards["working-bot"]; !ok {
		t.Error("working agent card should not be pruned")
	}
}

// TestPruneStaleAgentCards_PreservesWrapUpCard verifies that the periodic prune
// re-renders the rich wrap-up card (tldr banner) for a done agent that wrapped
// up, instead of flattening it to a bare one-liner. Regression guard for
// ka-0000006217: previously the prune clobbered the beautiful wrap-up card
// ~5 min after the agent finished, so cards "lost their wrap-up" in Slack even
// though the data still lived on the bead.
func TestPruneStaleAgentCards_PreservesWrapUpCard(t *testing.T) {
	daemon := newMockDaemon()

	// Done agent that wrapped up with a tldr — bead still open (terminal state).
	daemon.beads["bd-wrapped"] = &beadsapi.BeadDetail{
		ID:    "bd-wrapped",
		Title: "wrapped-bot",
		Type:  "agent",
		Fields: map[string]string{
			"agent":       "wrapped-bot",
			"project":     "gardenio",
			"role":        "crew",
			"agent_state": "done",
			"wrapup":      `{"tldr":"Shipped the gardener page","accomplishments":"Did the thing"}`,
		},
	}
	// Done agent with NO wrapup — should still get the compact one-liner.
	daemon.beads["bd-bare"] = &beadsapi.BeadDetail{
		ID:    "bd-bare",
		Title: "bare-bot",
		Type:  "agent",
		Fields: map[string]string{
			"agent":       "bare-bot",
			"project":     "gardenio",
			"role":        "crew",
			"agent_state": "done",
		},
	}

	// Capture the rendered blocks JSON per updated message ts.
	updates := make(map[string]string)
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			updates[r.FormValue("ts")] = r.FormValue("blocks")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["wrapped-bot"] = MessageRef{ChannelID: "C123", Timestamp: "7777.0001"}
	bot.agentCards["bare-bot"] = MessageRef{ChannelID: "C123", Timestamp: "7777.0002"}
	bot.agentState["wrapped-bot"] = "done"
	bot.agentState["bare-bot"] = "done"

	bot.pruneStaleAgentCards(context.Background())

	// The wrapped agent's card must carry the tldr banner: a header block plus
	// the tldr text. The compact builder emits neither.
	wrapped := updates["7777.0001"]
	if wrapped == "" {
		t.Fatal("expected the wrapped agent's card to be updated")
	}
	if !strings.Contains(wrapped, `"type":"header"`) {
		t.Errorf("wrapped card should keep its header (tldr banner) block, got: %s", wrapped)
	}
	if !strings.Contains(wrapped, "Shipped the gardener page") {
		t.Errorf("wrapped card should contain the tldr text, got: %s", wrapped)
	}

	// The bare agent (no wrapup) must still be compacted — no header block.
	bare := updates["7777.0002"]
	if bare == "" {
		t.Fatal("expected the bare agent's card to be updated")
	}
	if strings.Contains(bare, `"type":"header"`) {
		t.Errorf("bare card (no wrapup) should be compacted, not given a header, got: %s", bare)
	}
}

// TestPruneStaleAgentCards_PreservesWrapUpCardAfterBeadClosed is the regression
// guard for ka-0000004075: once an agent finishes and `gb stop` renders the
// wrap-up, the agent bead is CLOSED. ListAgentBeads filters to active statuses,
// so the closed bead drops out of that list — the prune guard's wrapup lookup
// (agentBead[short]) comes back empty and the beautiful wrap-up card was being
// flattened to a bare one-liner ~5 min later. The fix falls back to a
// status-agnostic daemon lookup (FindAgentBeadAnyStatus) so the wrap-up is
// recovered even for closed beads.
func TestPruneStaleAgentCards_PreservesWrapUpCardAfterBeadClosed(t *testing.T) {
	daemon := newMockDaemon()

	// Done agent that wrapped up — bead is CLOSED (the real post-stop state).
	// This bead is excluded from ListAgentBeads (mirrors activeStatuses), so the
	// only way to recover its wrapup is the status-agnostic FindAgentBeadAnyStatus.
	daemon.beads["closed-wrapped-bot"] = &beadsapi.BeadDetail{
		ID:     "bd-closed-wrapped",
		Title:  "closed-wrapped-bot",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":       "closed-wrapped-bot",
			"project":     "gardenio",
			"role":        "crew",
			"agent_state": "done",
			"wrapup":      `{"tldr":"Beautiful wrap-up survives bead close","accomplishments":"Shipped it"}`,
		},
	}

	updates := make(map[string]string)
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			updates[r.FormValue("ts")] = r.FormValue("blocks")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["closed-wrapped-bot"] = MessageRef{ChannelID: "C123", Timestamp: "8888.0001"}
	bot.agentState["closed-wrapped-bot"] = "done"

	bot.pruneStaleAgentCards(context.Background())

	card := updates["8888.0001"]
	if card == "" {
		t.Fatal("expected the closed wrapped agent's card to be updated")
	}
	if !strings.Contains(card, `"type":"header"`) {
		t.Errorf("closed wrapped card should keep its header (tldr banner) block, got: %s", card)
	}
	if !strings.Contains(card, "Beautiful wrap-up survives bead close") {
		t.Errorf("closed wrapped card should contain the tldr text recovered from the closed bead, got: %s", card)
	}
}

// TestPruneStaleAgentCards_KeepsRollupAfterBeadClosed is the regression guard for
// the operator-reported "sometimes we don't see it" (ka-0000003171): the
// cost/turns/context rollup renders correctly at `gb stop`, then VANISHES from
// the card ~5 minutes later when the prune cycle re-renders it.
//
// The cause: the prune loop built its rollup from agentMeta, indexed off
// ListAgentBeads — which filters to ACTIVE statuses. By the time the prune runs,
// the agent bead has been CLOSED, so the lookup missed, the rollup came out
// EMPTY, and that empty line was handed to buildWrapUpAgentCardBlocks. The prune
// therefore actively STRIPPED a rollup the card had displayed moments earlier.
//
// This asserts BOTH halves of the fix on a closed bead: the cost figure is
// recovered from the status-agnostic fetch, and the turns/context chips (which
// formatCostSummary never carried at all) now render too.
func TestPruneStaleAgentCards_KeepsRollupAfterBeadClosed(t *testing.T) {
	daemon := newMockDaemon()

	// Done agent that wrapped up, bead CLOSED — the real post-`gb stop` state,
	// carrying the full set of settled usage fields. Excluded from
	// ListAgentBeads, so every figure below must come from the status-agnostic
	// fetch or the assertions fail.
	daemon.beads["settled-bot"] = &beadsapi.BeadDetail{
		ID:     "bd-settled",
		Title:  "settled-bot",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":               "settled-bot",
			"project":             "gardenio",
			"role":                "crew",
			"agent_state":         "done",
			"wrapup":              `{"tldr":"Rollup survives the prune","accomplishments":"Shipped it"}`,
			"total_cost_usd":      "4.25",
			"total_input_tokens":  "50000",
			"total_output_tokens": "12000",
			"total_request_count": "37",
			"context_tokens":      "120000",
			// Stale live estimate: must NOT be added to the settled total on a
			// terminal card (the reconciler already folded the final session in).
			"live_cost_usd": "9.99",
		},
	}

	updates := make(map[string]string)
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			updates[r.FormValue("ts")] = r.FormValue("blocks")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["settled-bot"] = MessageRef{ChannelID: "C123", Timestamp: "9999.0001"}
	bot.agentState["settled-bot"] = "done"

	bot.pruneStaleAgentCards(context.Background())

	card := updates["9999.0001"]
	if card == "" {
		t.Fatal("expected the closed agent's card to be updated")
	}
	// Cost — the figure that used to disappear entirely.
	if !strings.Contains(card, `$4.25`) {
		t.Errorf("pruned card should keep the settled cost, got: %s", card)
	}
	// Turns + context — never rendered on a terminal card before this fix.
	if !strings.Contains(card, `37`) {
		t.Errorf("pruned card should carry the turns chip (total_request_count=37), got: %s", card)
	}
	if !strings.Contains(card, `120.0k ctx at exit`) {
		t.Errorf("pruned card should carry the frozen context chip, got: %s", card)
	}
	// The stale live estimate must never be folded into a terminal card's cost:
	// 4.25 + 9.99 = 14.24 would be a double-count.
	if strings.Contains(card, `$14.24`) {
		t.Errorf("terminal card must not add the stale live_cost_usd estimate, got: %s", card)
	}
}

// TestPruneStaleAgentCards_CompressesStopRequested verifies that agent cards for
// agents with stop_requested set are compressed on restart.
func TestPruneStaleAgentCards_CompressesStopRequested(t *testing.T) {
	daemon := newMockDaemon()

	// Agent with stop_requested but bead still open and state=working.
	daemon.beads["bd-stopping"] = &beadsapi.BeadDetail{
		ID:    "bd-stopping",
		Title: "stopping-bot",
		Type:  "agent",
		Fields: map[string]string{
			"agent":          "stopping-bot",
			"project":        "gardenio",
			"role":           "crew",
			"agent_state":    "working",
			"stop_requested": "true",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["stopping-bot"] = MessageRef{ChannelID: "C123", Timestamp: "5555.5555"}

	bot.pruneStaleAgentCards(context.Background())

	// Card should be removed from in-memory map after compression.
	if _, ok := bot.agentCards["stopping-bot"]; ok {
		t.Error("stop-requested agent card should be removed from agentCards after compression")
	}
}

// TestPruneStaleAgentCards_PreservesCardInState verifies that after pruning,
// the agent card is removed from the hot cache but preserved in persisted state.
// This allows findAgentCardByThread to still identify card threads for respawn.
func TestPruneStaleAgentCards_PreservesCardInState(t *testing.T) {
	daemon := newMockDaemon()
	// No active agents — dead-bot is gone.

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state

	cardRef := MessageRef{ChannelID: "C123", Timestamp: "2222.2222"}
	bot.agentCards["dead-bot"] = cardRef
	_ = state.SetAgentCard("dead-bot", cardRef)

	bot.pruneStaleAgentCards(context.Background())

	// Hot cache should be pruned.
	if _, ok := bot.agentCards["dead-bot"]; ok {
		t.Error("stale agent card should be removed from hot cache after pruning")
	}

	// Persisted state should still have the card (for thread identification).
	if ref, ok := state.GetAgentCard("dead-bot"); !ok {
		t.Error("agent card should be preserved in persisted state after pruning")
	} else if ref.ChannelID != "C123" || ref.Timestamp != "2222.2222" {
		t.Errorf("unexpected card ref in state: %+v", ref)
	}

	// findAgentCardByThread should still find it via state fallback.
	if agent := bot.findAgentCardByThread("C123", "2222.2222"); agent != "dead-bot" {
		t.Errorf("findAgentCardByThread should find agent via state, got %q", agent)
	}
}

// TestPruneStaleAgentCards_NoCards verifies that pruning is a no-op when
// there are no hydrated agent cards.
func TestPruneStaleAgentCards_NoCards(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Should not panic or error.
	bot.pruneStaleAgentCards(context.Background())

	if len(bot.agentCards) != 0 {
		t.Errorf("expected 0 agent cards, got %d", len(bot.agentCards))
	}
}

// TestNotifyAgentSpawn_SkipsClosedBead verifies that NotifyAgentSpawn does not
// post a card for an agent bead that is already closed (zombie prevention on
// SSE replay after restart).
func TestNotifyAgentSpawn_SkipsClosedBead(t *testing.T) {
	daemon := newMockDaemon()
	// Seed a closed agent bead.
	daemon.beads["agent-closed-1"] = &beadsapi.BeadDetail{
		ID:       "agent-closed-1",
		Type:     "agent",
		Status:   "closed",
		Title:    "dead-agent",
		Assignee: "gardenio/crew/dead-agent",
		Fields:   map[string]string{"agent": "dead-agent", "agent_state": "done"},
	}

	var postedMessages int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postedMessages++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.9999"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// Simulate SSE replay of a created event for an already-closed agent.
	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "agent-closed-1",
		Type:     "agent",
		Title:    "dead-agent",
		Assignee: "gardenio/crew/dead-agent",
		Fields:   map[string]string{"agent": "dead-agent"},
	})

	// No Slack message should have been posted.
	if postedMessages != 0 {
		t.Errorf("expected 0 Slack messages for closed bead, got %d", postedMessages)
	}

	// No agent state should have been recorded.
	if _, ok := bot.agentState["dead-agent"]; ok {
		t.Error("expected no agent state recorded for closed bead")
	}
}

// TestNotifyAgentSpawn_AllowsOpenBead verifies that NotifyAgentSpawn proceeds
// normally for an open (active) agent bead.
func TestNotifyAgentSpawn_AllowsOpenBead(t *testing.T) {
	daemon := newMockDaemon()
	// Seed an open agent bead.
	daemon.beads["agent-open-1"] = &beadsapi.BeadDetail{
		ID:       "agent-open-1",
		Type:     "agent",
		Status:   "open",
		Title:    "live-agent",
		Assignee: "gardenio/crew/live-agent",
		Fields:   map[string]string{"agent": "live-agent", "agent_state": "spawning"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "agent-open-1",
		Type:     "agent",
		Title:    "live-agent",
		Assignee: "gardenio/crew/live-agent",
		Fields:   map[string]string{"agent": "live-agent"},
	})

	// Agent state should have been recorded.
	if state, ok := bot.agentState["live-agent"]; !ok || state != "spawning" {
		t.Errorf("expected agent state 'spawning', got %q (exists=%v)", state, ok)
	}
}

// TestAgentIdentityDrift_FullPathNormalized verifies that agent identities
// are canonicalized to short names across spawn, state update, and task update
// events, preventing duplicate map entries from full paths vs short names.
func TestAgentIdentityDrift_FullPathNormalized(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-1"] = &beadsapi.BeadDetail{
		ID:       "agent-1",
		Type:     "agent",
		Status:   "open",
		Title:    "my-agent",
		Assignee: "gardenio/crew/my-agent",
		Fields:   map[string]string{"agent": "gardenio/crew/my-agent", "agent_state": "spawning"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// Spawn event uses full path in Assignee — should be stored under short name.
	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "agent-1",
		Type:     "agent",
		Title:    "my-agent",
		Assignee: "gardenio/crew/my-agent",
		Fields:   map[string]string{"agent": "gardenio/crew/my-agent"},
	})

	if _, ok := bot.agentState["my-agent"]; !ok {
		t.Error("expected agentState keyed by short name 'my-agent'")
	}
	if _, ok := bot.agentState["gardenio/crew/my-agent"]; ok {
		t.Error("agentState should not have full path key")
	}

	// State update event also uses full path — should update the same entry.
	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "gardenio/crew/my-agent",
		Fields:   map[string]string{"agent": "gardenio/crew/my-agent", "agent_state": "working"},
	})

	if state := bot.agentState["my-agent"]; state != "working" {
		t.Errorf("expected agentState[my-agent]=working, got %q", state)
	}
	if len(bot.agentState) != 1 {
		t.Errorf("expected exactly 1 agentState entry, got %d (identity drift!)", len(bot.agentState))
	}

	// Task update with full path — should still find the card by short name.
	bot.NotifyAgentTaskUpdate(context.Background(), "gardenio/crew/my-agent")

	// Verify no duplicate entries were created in any map.
	if len(bot.agentSeen) != 1 {
		t.Errorf("expected exactly 1 agentSeen entry, got %d (identity drift!)", len(bot.agentSeen))
	}
}

// TestAgentIdentityDrift_KillAgentWithFullPath verifies that killAgent
// canonicalizes the agent name and correctly removes the card even when
// called with a full path identity.
func TestAgentIdentityDrift_KillAgentWithFullPath(t *testing.T) {
	daemon := newMockDaemon()
	// Mock FindAgentBead looks up by agent name — store under short name
	// because killAgent now canonicalizes before calling FindAgentBead.
	daemon.beads["kill-me"] = &beadsapi.BeadDetail{
		ID:       "agent-2",
		Type:     "agent",
		Status:   "open",
		Title:    "kill-me",
		Assignee: "gardenio/crew/kill-me",
		Fields:   map[string]string{"agent": "kill-me"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Card stored under short name (as it would be after the spawn fix).
	bot.agentCards["kill-me"] = MessageRef{ChannelID: "C123", Timestamp: "1111.1111"}
	bot.agentState["kill-me"] = "working"
	bot.agentPending["kill-me"] = 2

	// Kill with full path identity — should compress the card, not delete it.
	err := bot.killAgent(context.Background(), "gardenio/crew/kill-me", true)
	if err != nil {
		t.Fatalf("killAgent failed: %v", err)
	}

	// Card should still exist (compressed to single line).
	if _, ok := bot.agentCards["kill-me"]; !ok {
		t.Error("agent card should be compressed, not removed")
	}
	// Pending count should be cleaned up.
	if _, ok := bot.agentPending["kill-me"]; ok {
		t.Error("agent pending should have been removed")
	}
}

// TestAgentIdentityDrift_HydrationNormalizesKeys verifies that NewBot
// canonicalizes agent identity keys when hydrating from persisted state,
// preventing drift from stale full-path keys in the state file.
func TestAgentIdentityDrift_HydrationNormalizesKeys(t *testing.T) {
	state, err := NewStateManager("")
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	// Simulate persisted state with full-path keys (pre-fix data).
	_ = state.SetAgentCard("gardenio/crew/old-agent", MessageRef{
		ChannelID: "C123",
		Timestamp: "1111.1111",
		Agent:     "gardenio/crew/old-agent",
	})
	_ = state.SetDecisionMessage("dec-1", MessageRef{
		ChannelID: "C123",
		Timestamp: "2222.2222",
		Agent:     "gardenio/crew/old-agent",
	})

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := NewBot(BotConfig{
		BotToken:      "xoxb-test",
		AppToken:      "xapp-test",
		Channel:       "C123",
		ThreadingMode: "agent",
		State:         state,
		Logger:        slog.Default(),
	})
	// Override Slack API client to use our test server.
	bot.api = slack.New("xoxb-test", slack.OptionAPIURL(slackSrv.URL+"/"))

	// Agent card should be stored under short name after hydration.
	if _, ok := bot.agentCards["old-agent"]; !ok {
		t.Error("expected agentCards keyed by short name 'old-agent' after hydration")
	}
	if _, ok := bot.agentCards["gardenio/crew/old-agent"]; ok {
		t.Error("agentCards should not have full path key after hydration")
	}

	// Pending count should use short name.
	if count := bot.agentPending["old-agent"]; count != 1 {
		t.Errorf("expected agentPending[old-agent]=1, got %d", count)
	}
	if count := bot.agentPending["gardenio/crew/old-agent"]; count != 0 {
		t.Errorf("expected agentPending[gardenio/crew/old-agent]=0, got %d", count)
	}
}

// TestPruneStaleAgentCards_BatchesLargeBacklog verifies that prune work is
// processed in bounded batches with sleeps between them, so a large backlog
// of stale cards cannot OOM the bridge or starve other goroutines.
// Regression guard for ka-0000000496 (slack-bridge OOMKilled at 128Mi).
func TestPruneStaleAgentCards_BatchesLargeBacklog(t *testing.T) {
	daemon := newMockDaemon()
	// No active agents seeded — every card we add will be stale.

	var updateCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			updateCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Seed enough stale cards to span multiple batches.
	const total = 3*pruneBatchSize + 7
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("dead-bot-%d", i)
		bot.agentCards[name] = MessageRef{ChannelID: "C123", Timestamp: fmt.Sprintf("ts-%d", i)}
	}

	start := time.Now()
	bot.pruneStaleAgentCards(context.Background())
	elapsed := time.Since(start)

	// All stale cards should be processed.
	if got := int(updateCount.Load()); got != total {
		t.Errorf("expected %d Slack updates, got %d", total, got)
	}
	if remaining := len(bot.agentCards); remaining != 0 {
		t.Errorf("expected 0 cards remaining, got %d", remaining)
	}
	// We expect at least 3 inter-batch sleeps (between batches 1↔2, 2↔3, 3↔4).
	// Allow a generous lower bound to keep the test stable on slow CI.
	minSleep := 3 * pruneBatchSleep / 2
	if elapsed < minSleep {
		t.Errorf("expected prune to take at least %v across batches, got %v", minSleep, elapsed)
	}
}

// TestPruneStaleAgentCards_GatedBySlackCardLimiter verifies that the prune
// loop respects the shared chat.update token bucket so a large backlog
// cannot burst above Slack's Tier-3 budget. Regression guard for ka-0000000854:
// previously the loop issued 10 chat.updates per 1500ms (~6.6 req/s), well above
// Slack chat.update's ~50 req/min, which kept the loop in Retry-After backoff
// and prevented a 5700-card backlog from ever draining.
func TestPruneStaleAgentCards_GatedBySlackCardLimiter(t *testing.T) {
	daemon := newMockDaemon()

	var updateCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			updateCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Tight limiter (2 tokens/sec, burst 1) so the test can observe the
	// limiter shaping prune throughput without taking minutes to run.
	bot.slackCardLimiter = rate.NewLimiter(rate.Limit(2.0), 1)

	const total = 6
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("dead-bot-%d", i)
		bot.agentCards[name] = MessageRef{ChannelID: "C123", Timestamp: fmt.Sprintf("ts-%d", i)}
	}

	start := time.Now()
	bot.pruneStaleAgentCards(context.Background())
	elapsed := time.Since(start)

	if got := int(updateCount.Load()); got != total {
		t.Errorf("expected %d Slack updates, got %d", total, got)
	}

	// 6 calls at 2 tokens/sec with burst 1 = 1 immediate + 5 × 500ms = ~2.5s.
	// Without the limiter, 6 calls finish almost instantly (single batch <
	// pruneBatchSize triggers no inter-batch sleep). Assert a sane lower bound
	// well above the no-limiter path.
	const minElapsed = 2 * time.Second
	if elapsed < minElapsed {
		t.Errorf("expected prune to be limiter-gated and take at least %v, got %v",
			minElapsed, elapsed)
	}
}

// TestPruneStaleAgentCards_RespectsContextCancellation verifies that prune
// honors ctx cancellation between batches so a long-running backlog does
// not block shutdown. ka-0000000496.
func TestPruneStaleAgentCards_RespectsContextCancellation(t *testing.T) {
	daemon := newMockDaemon()

	var updateCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			updateCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Seed enough stale cards to span multiple batches.
	const total = 4 * pruneBatchSize
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("dead-bot-%d", i)
		bot.agentCards[name] = MessageRef{ChannelID: "C123", Timestamp: fmt.Sprintf("ts-%d", i)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first batch is processed but before the second
	// begins. Inter-batch sleep gives us a clean cancellation point.
	go func() {
		time.Sleep(pruneBatchSleep / 2)
		cancel()
	}()

	bot.pruneStaleAgentCards(ctx)

	// At least the first batch ran; not all batches should have completed.
	got := int(updateCount.Load())
	if got < pruneBatchSize {
		t.Errorf("expected at least %d updates from first batch, got %d", pruneBatchSize, got)
	}
	if got >= total {
		t.Errorf("expected ctx cancellation to interrupt prune; processed all %d cards", got)
	}
}

// TestPruneStaleAgentCards_HonorsRetryAfter verifies that when Slack returns
// a rate-limit response, the prune loop pauses for the Retry-After duration
// (clamped to a 5-second floor) instead of hammering chat.update at the full
// pruneBatchSize × pruneBatchSleep pace. Regression guard for ka-0000003678.
func TestPruneStaleAgentCards_HonorsRetryAfter(t *testing.T) {
	daemon := newMockDaemon()
	// No active agents — every seeded card is stale.

	var (
		updateCount atomic.Int32
		rateLimitOn atomic.Bool
	)
	rateLimitOn.Store(true)
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			n := updateCount.Add(1)
			// First two calls return 429 with a 1s Retry-After (the loop
			// clamps this to 5s, which is what the assertion below checks).
			if rateLimitOn.Load() && n <= 2 {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Three cards: the first two trigger 429 (and arm slackRateLimitUntil),
	// the third proves the loop resumes after the pause window.
	bot.agentCards["dead-1"] = MessageRef{ChannelID: "C123", Timestamp: "1.1"}
	bot.agentCards["dead-2"] = MessageRef{ChannelID: "C123", Timestamp: "2.2"}
	bot.agentCards["dead-3"] = MessageRef{ChannelID: "C123", Timestamp: "3.3"}

	// Cap test runtime: prune normally clamps Retry-After to 5s. Two 429s
	// → ~10s of pause + the inter-batch sleep, plus margin.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	bot.pruneStaleAgentCards(ctx)
	elapsed := time.Since(start)

	// Every card was attempted. The two rate-limited calls + the first
	// non-rate-limited retry path means we expect at least 3 chat.update
	// hits (one per card; rate-limited calls don't auto-retry within a
	// single prune pass — the card is dropped from agentCards above the
	// API call and will be re-processed by the next prune cycle).
	if got := int(updateCount.Load()); got != 3 {
		t.Errorf("expected 3 chat.update calls (one per card), got %d", got)
	}

	// The loop must have paused for the clamped 5s floor on each 429,
	// for a combined wait of ~10s. Allow slack for scheduling jitter.
	const minWait = 9 * time.Second
	if elapsed < minWait {
		t.Errorf("expected prune to pause at least %v honoring Retry-After, got %v",
			minWait, elapsed)
	}

	// slackRateLimitUntil must reflect the most recent 429 so other
	// Slack call paths (flushAgentCard / flushDirtyCards) also back off.
	bot.mu.Lock()
	until := bot.slackRateLimitUntil
	bot.mu.Unlock()
	if until.IsZero() {
		t.Error("expected slackRateLimitUntil to be set after a 429 response")
	}
}

// TestPruneStaleAgentCards_RateLimitWaitRespectsContext verifies that ctx
// cancellation cuts the Retry-After wait short instead of blocking the prune
// loop for the full window during shutdown. ka-0000003678.
func TestPruneStaleAgentCards_RateLimitWaitRespectsContext(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "30")
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "ratelimited"})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["dead-1"] = MessageRef{ChannelID: "C123", Timestamp: "1.1"}
	bot.agentCards["dead-2"] = MessageRef{ChannelID: "C123", Timestamp: "2.2"}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel shortly after the first 429 lands. The clamped wait would be
	// 30s, but cancellation must abort it well before that.
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	bot.pruneStaleAgentCards(ctx)
	elapsed := time.Since(start)

	// We must have unblocked far sooner than the clamped 30s pause.
	if elapsed > 5*time.Second {
		t.Errorf("expected ctx cancellation to abort Retry-After wait quickly, took %v", elapsed)
	}
}

// TestStartPeriodicPrune_DoesNotBlockCaller is the readiness-path regression
// guard for ka-0000000496. The bridge must launch startPeriodicPrune in its
// own goroutine so a slow ListAgentBeads (large backlog or daemon contention)
// cannot delay Socket Mode connection and /readyz flipping to 200.
//
// We assert two properties:
//  1. Spawning startPeriodicPrune as a goroutine returns to the caller in
//     well under the worst-case prune duration (the daemon below blocks for
//     2s on the initial ListAgentBeads).
//  2. The goroutine drains cleanly when ctx is cancelled.
func TestStartPeriodicPrune_DoesNotBlockCaller(t *testing.T) {
	const slowDaemonDelay = 2 * time.Second

	daemon := &slowListDaemon{
		mockDaemon: newMockDaemon(),
		delay:      slowDaemonDelay,
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Hydrate a card so the prune path actually calls ListAgentBeads.
	bot.agentCards["some-bot"] = MessageRef{ChannelID: "C123", Timestamp: "1.1"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		bot.startPeriodicPrune(ctx)
		close(done)
	}()
	// The `go` itself returns immediately; this measures total scheduling
	// overhead. It must be well under the simulated prune delay.
	if elapsed := time.Since(start); elapsed > slowDaemonDelay/4 {
		t.Errorf("spawning startPeriodicPrune blocked caller for %v (expected << %v)",
			elapsed, slowDaemonDelay)
	}

	// Cancelling ctx should drain the goroutine. Allow some slack for the
	// blocked daemon call to return before the loop notices ctx.Done.
	cancel()
	select {
	case <-done:
	case <-time.After(slowDaemonDelay + 2*time.Second):
		t.Fatal("startPeriodicPrune did not return after ctx cancel")
	}
}

// slowListDaemon wraps mockDaemon to simulate a slow ListAgentBeads call,
// modeling the worst case where the beads daemon is under load and a large
// agent-card backlog has to be reconciled.
type slowListDaemon struct {
	*mockDaemon
	delay time.Duration
}

func (d *slowListDaemon) ListAgentBeads(ctx context.Context) ([]beadsapi.AgentBead, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.mockDaemon.ListAgentBeads(ctx)
}

// TestPruneStaleAgentCards_DoesNotStarveLiveCardRefresh is the regression test
// for ka-0000008156: recent agents showed "starting" in Slack while their beads
// correctly said agent_state=working.
//
// Both the prune loop and the live card path share one slackCardLimiter, but
// they used to take tokens differently: prune called Wait() (blocking — reserves
// the next token the instant it frees) while the live path calls Allow()
// (non-blocking — gives up at once). A prune draining a large backlog therefore
// held the bucket permanently empty, every live Allow() returned false, and
// cards froze at whatever they first rendered.
//
// Assert the live path can still acquire tokens WHILE a large prune runs.
func TestPruneStaleAgentCards_DoesNotStarveLiveCardRefresh(t *testing.T) {
	daemon := newMockDaemon()

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Mirror production's shape: a bucket slow enough that prune wants to hold
	// it continuously. 4 tokens/sec (250ms apart) with burst 3 means one
	// pruneBatchSize batch takes ~2.5s to drain — so the whole measurement
	// window below sits INSIDE a single batch drain, never in a pruneBatchSleep
	// gap. That matters: the batch sleep releases the bucket briefly, which is
	// exactly why a naive measurement across batches hides the starvation.
	bot.slackCardLimiter = rate.NewLimiter(rate.Limit(4.0), 3)

	for i := 0; i < 200; i++ {
		name := fmt.Sprintf("dead-bot-%d", i)
		bot.agentCards[name] = MessageRef{ChannelID: "C123", Timestamp: fmt.Sprintf("ts-%d", i)}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pruneDone := make(chan struct{})
	go func() {
		defer close(pruneDone)
		bot.pruneStaleAgentCards(ctx)
	}()

	// Let prune start and consume the initial burst.
	time.Sleep(250 * time.Millisecond)

	// Simulate the live card path: updateAgentCard / flushDirtyCards poll the
	// shared limiter with a NON-BLOCKING Allow(), so they lose every race
	// against a blocking Wait() that reserves tokens from the future.
	liveGranted := 0
	deadline := time.Now().Add(1750 * time.Millisecond)
	for time.Now().Before(deadline) {
		if bot.slackCardLimiter.Allow() {
			liveGranted++
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-pruneDone

	// ~2s at 4 tokens/sec is ~8 tokens of real capacity. Prune is entitled to
	// most of it, but the live path must get a fair share rather than zero.
	// With the old blocking Wait() this was 0: every token was reserved before
	// a live Allow() could ever see it.
	if liveGranted < 2 {
		t.Fatalf("live card refresh starved by the prune loop: got %d limiter grants "+
			"in ~1.75s of active pruning, want >= 2 (ka-0000008156)", liveGranted)
	}
	t.Logf("live card refresh acquired %d tokens while prune actively drained a backlog", liveGranted)
}

// TestPruneStaleAgentCards_MarksCompressedInState verifies prune records the
// compressed marker on the persisted card, so a later restart does not treat an
// already-compacted card as fresh work (ka-0000008156).
func TestPruneStaleAgentCards_MarksCompressedInState(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state

	cardRef := MessageRef{ChannelID: "C123", Timestamp: "2222.2222"}
	bot.agentCards["dead-bot"] = cardRef
	_ = state.SetAgentCard("dead-bot", cardRef)

	bot.pruneStaleAgentCards(context.Background())

	ref, ok := state.GetAgentCard("dead-bot")
	if !ok {
		t.Fatal("agent card should still be preserved in state for thread identification")
	}
	if !ref.Compressed {
		t.Error("prune should mark the persisted card Compressed so restarts skip it")
	}
	// The card ref itself must survive intact — findAgentCardByThread relies on it.
	if ref.ChannelID != "C123" || ref.Timestamp != "2222.2222" {
		t.Errorf("card ref should be unchanged apart from the marker, got %+v", ref)
	}
}

// TestNewBot_SkipsCompressedCardsOnHydration verifies the restart-amplification
// fix: cards already compressed by a previous prune are not rehydrated into the
// hot cache, so each restart no longer re-compresses the entire historical
// backlog (2782 cards / ~69 min of chat.update in prod). Thread identification
// still works via the persisted-state fallback (ka-0000008156).
func TestNewBot_SkipsCompressedCardsOnHydration(t *testing.T) {
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// One already-compressed historical card, one live card.
	_ = state.SetAgentCard("old-bot", MessageRef{ChannelID: "C1", Timestamp: "1.1", Compressed: true})
	_ = state.SetAgentCard("live-bot", MessageRef{ChannelID: "C1", Timestamp: "2.2"})

	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.state = state
	// Re-run the hydration NewBot performs, against this state.
	bot.agentCards = make(map[string]MessageRef)
	for agent, ref := range state.AllAgentCards() {
		if ref.Compressed {
			continue
		}
		bot.agentCards[extractAgentName(agent)] = ref
	}

	if _, ok := bot.agentCards["old-bot"]; ok {
		t.Error("already-compressed card should not be rehydrated into the hot cache")
	}
	if _, ok := bot.agentCards["live-bot"]; !ok {
		t.Error("live card must still be rehydrated")
	}
	// Thread identification for the compressed card still resolves via state.
	if agent := bot.findAgentCardByThread("C1", "1.1"); agent != "old-bot" {
		t.Errorf("findAgentCardByThread should still resolve compressed card via state, got %q", agent)
	}
}

// TestMarkAgentCardCompressed_MatchesLegacyFullPathKey verifies the marker lands
// on the EXISTING state entry even when that entry is keyed by a legacy
// full-path identity while prune passes the hydrated short name. Marking by
// message ref avoids inserting a short-named duplicate and leaving the original
// unmarked — which would re-compress that card on every restart forever
// (ka-0000008156).
func TestMarkAgentCardCompressed_MatchesLegacyFullPathKey(t *testing.T) {
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref := MessageRef{ChannelID: "C123", Timestamp: "9999.9999"}
	_ = state.SetAgentCard("gardenio/crew/old-agent", ref)

	// Prune iterates the hot cache, which is keyed by SHORT name.
	if err := state.MarkAgentCardCompressed("old-agent", ref); err != nil {
		t.Fatal(err)
	}

	all := state.AllAgentCards()
	if len(all) != 1 {
		t.Fatalf("expected the existing entry to be marked in place, not duplicated; got %d entries: %+v", len(all), all)
	}
	got, ok := all["gardenio/crew/old-agent"]
	if !ok {
		t.Fatalf("legacy full-path entry should be the one marked, got keys: %+v", all)
	}
	if !got.Compressed {
		t.Error("legacy full-path entry should be marked Compressed")
	}
}

// TestMarkAgentCardCompressed_InsertsWhenAbsent verifies the fallback path
// records the marker for a card that is not yet in persisted state.
func TestMarkAgentCardCompressed_InsertsWhenAbsent(t *testing.T) {
	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	ref := MessageRef{ChannelID: "C123", Timestamp: "5555.5555"}
	if err := state.MarkAgentCardCompressed("new-agent", ref); err != nil {
		t.Fatal(err)
	}
	got, ok := state.GetAgentCard("new-agent")
	if !ok {
		t.Fatal("card should be recorded when absent from state")
	}
	if !got.Compressed {
		t.Error("inserted card should be marked Compressed")
	}
}

// TestWaitForPruneToken_ClampKeepsPruneDrainingAtBurst1 is the regression guard
// for the ONE genuine fail-open in the prune-vs-live starvation fix (ka-0000008156,
// ka-0000004378): the floor clamp `min(pruneTokenFloor, Burst()-1)` in
// waitForPruneToken.
//
// pruneTokenFloor is 1.0. At the production burst of 3 the clamp is a no-op
// (min(1.0, 2) = 1.0). It only bites if the shared slackCardLimiter's burst is
// lowered to 1 — a plausible config change. WITHOUT the clamp the reserve check
// `Tokens() > floor` becomes `Tokens() > 1.0`, which a burst-1 bucket can NEVER
// satisfy (Tokens() never exceeds burst). waitForPruneToken would then spin
// forever and the prune loop would WEDGE on its first token acquisition —
// freezing every stale agent card in Slack indefinitely. The clamp degrades the
// floor to 0 at burst 1 ("just take a token"), so prune keeps draining.
//
// This asserts the user-visible property (prune keeps making progress) by driving
// the real waitForPruneToken through several successive acquisitions against a
// burst-1 limiter, all of which must complete well inside a bounded budget.
//
// MUTATION PROOF: replace the clamped floor with the naive `floor := pruneTokenFloor`
// and this test goes RED (every acquisition past the first initial-burst token
// blocks until ctx expires), while the burst-3 prune legs above stay GREEN —
// exactly because the clamp is a no-op at burst 3.
func TestWaitForPruneToken_ClampKeepsPruneDrainingAtBurst1(t *testing.T) {
	// Burst 1, fast refill so the drain below completes quickly under HEAD. The
	// refill rate is irrelevant to the wedge: with the clamp removed, `Tokens() >
	// 1.0` is unsatisfiable at burst 1 no matter how fast tokens refill.
	bot := &Bot{slackCardLimiter: rate.NewLimiter(rate.Limit(50.0), 1)}

	// Model a small prune backlog draining: several successive token acquisitions.
	// The first rides the initial burst token; each later one must wait for a
	// refill above the clamped floor. Under the naive (unclamped) floor the second
	// acquisition can never see Tokens() > 1.0 and wedges.
	const acquisitions = 4
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	start := time.Now()
	for i := 0; i < acquisitions; i++ {
		if err := bot.waitForPruneToken(ctx); err != nil {
			t.Fatalf("waitForPruneToken wedged at burst 1 on acquisition %d/%d after %v "+
				"(err=%v). The floor clamp min(pruneTokenFloor, Burst()-1) is what keeps a "+
				"burst-1 limiter from making Tokens() > floor unsatisfiable and freezing the "+
				"prune loop (ka-0000008156).", i+1, acquisitions, time.Since(start), err)
		}
	}
}

// TestPruneStaleAgentCards_DrainsBacklogAtBurst1 is the loop-level companion to
// TestWaitForPruneToken_ClampKeepsPruneDrainingAtBurst1: it asserts the
// operator-visible property (the stale-card backlog actually drains — cards do
// not freeze in Slack) end-to-end through the real pruneStaleAgentCards loop when
// the shared limiter's burst is at the degenerate value of 1.
//
// This is the honest analog of an e2e "backlog drains while cards stay live" leg.
// The deployed slack-bridge cannot carry this proof: the burst is hardcoded at
// bot.go's NewBot (rate.NewLimiter(40/60, 3)) with no env/config knob, so there
// is no way to exercise the burst-1 clamp against a running bridge without
// rebuilding and redeploying a mutated image — see the note on ka-0000004378.
//
// MUTATION PROOF: with the naive unclamped `floor := pruneTokenFloor`, this loop
// wedges on its second token acquisition (Tokens() > 1.0 is unsatisfiable at
// burst 1) and never drains the backlog, so the test times out RED. The existing
// burst-3 prune legs stay GREEN because the clamp is a no-op there.
func TestPruneStaleAgentCards_DrainsBacklogAtBurst1(t *testing.T) {
	daemon := newMockDaemon()

	var updateCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			updateCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Degenerate burst of 1 — the exact config that makes the floor clamp
	// load-bearing. Fast refill so a healthy drain finishes quickly; the wedge a
	// missing clamp causes is independent of refill rate.
	bot.slackCardLimiter = rate.NewLimiter(rate.Limit(200.0), 1)

	const total = pruneBatchSize + 3 // spans a batch boundary; all stale (no active agents)
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("dead-bot-%d", i)
		bot.agentCards[name] = MessageRef{ChannelID: "C123", Timestamp: fmt.Sprintf("ts-%d", i)}
	}

	// Bound the run: a healthy drain of `total` cards (one inter-batch sleep) is
	// well under this; a wedged loop would blow past it.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() { defer close(done); bot.pruneStaleAgentCards(ctx) }()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("prune loop wedged at burst 1: drained only %d/%d cards before the deadline. "+
			"The floor clamp min(pruneTokenFloor, Burst()-1) is what keeps waitForPruneToken "+
			"from spinning forever on a burst-1 limiter (ka-0000008156).",
			updateCount.Load(), total)
	}

	if got := int(updateCount.Load()); got != total {
		t.Errorf("expected the full backlog of %d stale cards to drain, got %d chat.update calls",
			total, got)
	}
	if remaining := len(bot.agentCards); remaining != 0 {
		t.Errorf("expected 0 cards remaining after drain, got %d", remaining)
	}
}
