package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// closedAgentBead builds a terminal agent bead in the state the double-card bug
// was observed in: CLOSED, carrying a wrap-up and a recorded card location.
func closedAgentBead(agent, channel, ts string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     "bead-" + agent,
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":              agent,
			"agent_state":        "done",
			"slack_card_channel": channel,
			"slack_card_ts":      ts,
			"wrapup":             `{"tldr":"triaged FSM-481"}`,
		},
	}
}

// TestEnsureAgentCard_ResurrectsFromClosedBead is the double-wrap-up-card
// regression (ka-0000005302), reproduced from the FSM-481 triage agent
// (gd-LZ9EXYcWOt): it wrapped up at 07:05, and a SECOND top-level card appeared
// at 07:46 for the same agent.
//
// The sequence: `gb stop` renders the wrap-up and CLOSES the bead. Minutes later
// the card is gone from the hot cache. Then any post-close field write to the
// bead — the controller's 30-minute spend/xcheck sweeps re-stamp
// spend_logs_cost_usd et al. — re-emits bead.updated, and NotifyAgentState
// re-renders the terminal card. The bead tier is supposed to make that
// re-render edit the ORIGINAL card in place, and it is the only tier that
// survives losing cache + state. But it resolved the bead with FindAgentBead,
// which is scoped to ACTIVE statuses and so goes blind on the very beads whose
// cards are being rendered — the tier silently found nothing and a fresh card
// was posted.
//
// The bug's own fingerprint proves it: the agent bead still recorded card #1
// while the bridge state file recorded card #2, i.e. the second post updated
// cache + state but persistCardRef's identically-scoped lookup no-op'd.
func TestEnsureAgentCard_ResurrectsFromClosedBead(t *testing.T) {
	var postCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			postCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.0004"})
	}))
	defer slackSrv.Close()

	const agent = "monorepo-fsm-triage-agent-s5q4"

	daemon := newMockDaemon()
	// Faithful to the real client: a closed bead is invisible to FindAgentBead.
	daemon.enforceActiveStatusScoping = true
	daemon.beads[agent] = closedAgentBead(agent, "C-FSM", "1785308406.192239")

	bot := newTestBot(daemon, slackSrv)
	// Post-prune, post-restart: no hot cache, no state file. The bead's
	// slack_card_* fields are the only surviving record of the card.
	bot.state = nil

	ts, err := bot.ensureAgentCard(context.Background(), agent, "C-FSM")
	if err != nil {
		t.Fatal(err)
	}

	if ts != "1785308406.192239" {
		t.Errorf("expected the original card ts resurrected from the CLOSED bead, got %s", ts)
	}
	// The whole point: no second card in the channel.
	if n := postCount.Load(); n != 0 {
		t.Errorf("posted %d duplicate card(s) for a closed agent — the double wrap-up card is back", n)
	}
}

// TestResolveAgentCardRef_FindsClosedBeadCard verifies the wrap-up EDIT path
// resolves a closed agent's card from bead fields. This is the path
// replaceAgentCardWithWrapUp depends on; when it fails it falls through to
// updateAgentCard, which is what actually posted the duplicate.
func TestResolveAgentCardRef_FindsClosedBeadCard(t *testing.T) {
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	const agent = "monorepo-fsm-triage-agent-s5q4"

	daemon := newMockDaemon()
	daemon.enforceActiveStatusScoping = true
	daemon.beads[agent] = closedAgentBead(agent, "C-FSM", "1785308406.192239")

	bot := newTestBot(daemon, slackSrv)
	bot.state = nil

	ref, ok := bot.resolveAgentCardRef(context.Background(), agent)
	if !ok {
		t.Fatal("could not resolve a closed agent's card ref — wrap-up would fall through and post a new card")
	}
	if ref.Timestamp != "1785308406.192239" || ref.ChannelID != "C-FSM" {
		t.Errorf("resolved wrong card ref: %+v", ref)
	}
}

// TestPersistCardRef_StampsClosedBead guards the tier that kept the evidence:
// the second card updated cache + state but never reached the bead, leaving the
// bead pointing at a card that was no longer the live one. A terminal-state
// stamp must land even though the bead is closed.
func TestPersistCardRef_StampsClosedBead(t *testing.T) {
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	const agent = "monorepo-fsm-triage-agent-s5q4"

	daemon := newMockDaemon()
	daemon.enforceActiveStatusScoping = true
	bead := closedAgentBead(agent, "C-FSM", "old.111")
	// Seed under BOTH keys: the name (how FindAgentBead* resolves) and the bead
	// ID (how UpdateBeadFields resolves), pointing at the same struct so the
	// write is observable through either.
	daemon.beads[agent] = bead
	daemon.beads[bead.ID] = bead

	bot := newTestBot(daemon, slackSrv)
	bot.persistCardRef(context.Background(), agent, "C-FSM", "new.222")

	if got := bead.Fields["slack_card_ts"]; got != "new.222" {
		t.Errorf("closed bead card ref not updated: slack_card_ts=%q, want new.222", got)
	}
}

// TestReplaceAgentCardWithWrapUp_EditsClosedBeadCard is the end-to-end
// assertion: a terminal re-render of a closed agent must EDIT (chat.update) the
// existing card, never POST a new one — however many times the sweeps re-fire.
func TestReplaceAgentCardWithWrapUp_EditsClosedBeadCard(t *testing.T) {
	var postCount, updateCount atomic.Int32
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat.postMessage":
			postCount.Add(1)
		case "/chat.update":
			updateCount.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "9999.0005"})
	}))
	defer slackSrv.Close()

	const agent = "monorepo-fsm-triage-agent-s5q4"

	daemon := newMockDaemon()
	daemon.enforceActiveStatusScoping = true
	daemon.beads[agent] = closedAgentBead(agent, "C-FSM", "1785308406.192239")

	bot := newTestBot(daemon, slackSrv)
	bot.state = nil

	// Re-render twice, as the 30-minute spend and xcheck sweeps would.
	for range 2 {
		bot.replaceAgentCardWithWrapUp(context.Background(), agent, "done",
			`{"tldr":"triaged FSM-481"}`)
	}

	if n := postCount.Load(); n != 0 {
		t.Errorf("expected 0 new cards on terminal re-render, got %d", n)
	}
	if n := updateCount.Load(); n != 2 {
		t.Errorf("expected 2 in-place chat.update edits, got %d", n)
	}
}
