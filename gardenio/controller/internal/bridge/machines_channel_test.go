package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/conversation/surfacetest"

	"github.com/slack-go/slack/slackevents"
)

// Single voice (steward amendment 2026-10-23 §2): machine posts go to the
// land's #machines in the Commons, while their Slack copies stay where the
// bridge has always posted them.

// putGeneralAndMachines gives gardenio a #general bound to slackChannel
// (mirror both) and, with machines, an unbound #machines.
func putGeneralAndMachines(daemon *mockDaemon, slackChannel string, machines bool) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads["gd-ch-general"] = &beadsapi.BeadDetail{ID: "gd-ch-general", Type: "channel", Status: "open", Title: "#general", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "general", "channel_kind": "general", "slack_channel_id": slackChannel, "mirror": "both"}}
	if machines {
		daemon.beads["gd-ch-machines"] = &beadsapi.BeadDetail{ID: "gd-ch-machines", Type: "channel", Status: "open", Title: "#machines", Labels: []string{"project:gardenio"},
			Fields: map[string]string{"name": "machines", "channel_kind": "machines"}}
	}
}

// TestAgentCardInMachines: a top-level agent card's bead is in #machines and
// its Slack copy still goes to the Slack channel the bridge routed it to; a
// thread card stays in its thread.
func TestAgentCardInMachines(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", true)
	ctx := context.Background()

	ts, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW")
	if err != nil {
		t.Fatal(err)
	}
	cards := cardBeadsOf(daemon, "gd-agent-a")
	if len(cards) != 1 || cards[0].Fields["channel_id"] != "gd-ch-machines" ||
		cards[0].Fields["slack_channel"] != "C0CREW" || cards[0].Fields["slack_ts"] != ts {
		t.Fatalf("card beads of crew-a = %+v, want one in gd-ch-machines with its C0CREW copy", cards)
	}
	if calls := twin.Calls(); len(calls) != 1 || calls[0].Method != "chat.postMessage" {
		t.Fatalf("Slack calls = %+v, want the one card post", calls)
	}

	if _, err := b.ensureAgentCard(ctx, "crew-t", "C0THREAD", "1690000000.000100"); err != nil {
		t.Fatal(err)
	}
	if cards := cardBeadsOf(daemon, "gd-agent-t"); len(cards) != 1 || cards[0].Fields["thread_id"] != "gd-head-t" || cards[0].Fields["channel_id"] == "gd-ch-machines" {
		t.Fatalf("thread card beads = %+v, want one in its thread, not in #machines", cards)
	}
}

// TestAgentCardBeforeMachinesIsEditedInPlace: a card posted into #general
// before #machines existed is edited on its own bead after the roll (no hot
// cache), not upserted a second time into #machines.
func TestAgentCardBeforeMachinesIsEditedInPlace(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", false)
	ctx := context.Background()
	if _, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW"); err != nil {
		t.Fatal(err)
	}
	putGeneralAndMachines(daemon, "C0CREW", true)
	b.forgetAgentCard("crew-a")

	b.flushAgentCard(ctx, "crew-a")

	cards := cardBeadsOf(daemon, "gd-agent-a")
	if len(cards) != 1 || cards[0].Fields["channel_id"] != "gd-ch-general" {
		t.Fatalf("card beads of crew-a after the edit = %+v, want the one in gd-ch-general", cards)
	}
}

// TestAgentCardWithoutMachines: a land with no #machines places a card where
// it always did, in the channel bead bound to the Slack channel.
func TestAgentCardWithoutMachines(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", false)
	if _, err := b.ensureAgentCard(context.Background(), "crew-a", "C0CREW"); err != nil {
		t.Fatal(err)
	}
	if cards := cardBeadsOf(daemon, "gd-agent-a"); len(cards) != 1 || cards[0].Fields["channel_id"] != "gd-ch-general" {
		t.Fatalf("card beads = %+v, want one in gd-ch-general", cards)
	}
}

// TestEscalationCardInMachinesOnlyForTheDesk: a desk escalation's card is in
// #machines; an operator's and an owner's stay in the channel a person reads.
// Each Slack copy goes to the agent's channel as before.
func TestEscalationCardInMachinesOnlyForTheDesk(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newEscalationCardsBot(t, twin)
	daemon.mu.Lock()
	delete(daemon.beads, "gd-ch-crew")
	daemon.mu.Unlock()
	putGeneralAndMachines(daemon, "C0CREW", true)
	ctx := context.Background()

	noCreds := map[string]string{"credential_requirements": ""}
	desk := escalationCardEvent(daemon, "gd-esc-desk", "crew-x", "", 1, noCreds)
	op := escalationCardEvent(daemon, "gd-esc-op", "crew-x", "", 1, map[string]string{"credential_requirements": "", "operation_type": "k8s_rbac"})
	owner := escalationCardEvent(daemon, "gd-esc-owner", "crew-x", "", 1, map[string]string{"credential_requirements": "", "audience": "owner"})
	want := map[string]string{"gd-esc-desk": "gd-ch-machines", "gd-esc-op": "gd-ch-general", "gd-esc-owner": "gd-ch-general"}
	for _, ev := range []BeadEvent{desk, op, owner} {
		if err := b.NotifyNewEscalation(ctx, ev); err != nil {
			t.Fatalf("NotifyNewEscalation %s: %v", ev.ID, err)
		}
		cards := cardBeadsOf(daemon, ev.ID)
		if len(cards) != 1 || cards[0].Fields["channel_id"] != want[ev.ID] || cards[0].Fields["slack_channel"] != "C0CREW" {
			t.Errorf("%s card beads = %+v, want one in %s with its C0CREW copy", ev.ID, cards, want[ev.ID])
		}
	}
}

// TestMirrorSlackReplyUnderMachinesCard: a person's Slack reply under an agent
// card is in the card's channel, #machines, not the channel bound to the
// Slack one (a reply is always in its root's channel).
func TestMirrorSlackReplyUnderMachinesCard(t *testing.T) {
	b, daemon, _ := newMirrorBot(t)
	daemon.beads["gd-ch-machines"] = &beadsapi.BeadDetail{ID: "gd-ch-machines", Type: "channel", Status: "open", Title: "#machines", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"name": "machines", "channel_kind": "machines"}}
	daemon.beads["gd-card"] = &beadsapi.BeadDetail{
		ID: "gd-card", Type: "message", Status: "closed", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"kind": "card", "subject_id": "gd-agent-a", "channel_id": "gd-ch-machines", "slack_channel": "C0BOTH", "slack_ts": "1690000003.000100"},
	}
	b.handleMessageEvent(context.Background(), &slackevents.MessageEvent{
		User: "U0ADA", Channel: "C0BOTH", ChannelType: "channel", ThreadTimeStamp: "1690000003.000100", TimeStamp: "1690000003.000101", Text: "why did it stop?",
	})
	got := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["thread_id"] == "gd-card" })
	if len(got) != 1 || got[0].Fields["channel_id"] != "gd-ch-machines" || got[0].Fields["origin"] != "slack" {
		t.Fatalf("reply under the #machines card = %+v, want one in gd-ch-machines", got)
	}
}

// TestDeskEscalationThreadsUnderTheAgentCard: gb escalation create names the
// requesting agent by its bead id. The card of a desk escalation is a reply
// under the agent's card in #machines, and no second agent card is posted for
// the id (gd-rar4hYRbGC: 42 merge cards a day landed top-level in #general,
// each under a stray card for "gd-..."). An owner escalation in the same
// thread stays in the channel a person reads.
func TestDeskEscalationThreadsUnderTheAgentCard(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newEscalationCardsBot(t, twin)
	b.threadingMode = "agent"
	daemon.mu.Lock()
	delete(daemon.beads, "gd-ch-crew")
	daemon.mu.Unlock()
	putGeneralAndMachines(daemon, "C0CREW", true)
	// The agent bead under the id gb escalation create writes.
	daemon.mu.Lock()
	daemon.beads["gd-AgentX01"] = daemon.beads["crew-x"]
	daemon.mu.Unlock()
	ctx := context.Background()

	cardTS, err := b.ensureAgentCard(ctx, "crew-x", "C0CREW")
	if err != nil {
		t.Fatal(err)
	}
	agentCards := cardBeadsOf(daemon, "gd-agent-x")
	if len(agentCards) != 1 || agentCards[0].Fields["channel_id"] != "gd-ch-machines" {
		t.Fatalf("agent card beads = %+v, want one in gd-ch-machines", agentCards)
	}
	card := agentCards[0].ID

	noCreds := map[string]string{"credential_requirements": ""}
	desk := escalationCardEvent(daemon, "gd-esc-desk", "gd-AgentX01", "", 1, noCreds)
	owner := escalationCardEvent(daemon, "gd-esc-owner", "gd-AgentX01", "", 1, map[string]string{"credential_requirements": "", "audience": "owner"})
	want := map[string]string{"gd-esc-desk": "gd-ch-machines", "gd-esc-owner": "gd-ch-general"}
	for _, ev := range []BeadEvent{desk, owner} {
		if err := b.NotifyNewEscalation(ctx, ev); err != nil {
			t.Fatalf("NotifyNewEscalation %s: %v", ev.ID, err)
		}
		cards := cardBeadsOf(daemon, ev.ID)
		if len(cards) != 1 || cards[0].Fields["channel_id"] != want[ev.ID] {
			t.Errorf("%s card beads = %+v, want one in %s", ev.ID, cards, want[ev.ID])
		}
		if got := b.messages[ev.ID].ThreadTS; got != cardTS {
			t.Errorf("%s Slack copy threads under %q, want the agent card %q", ev.ID, got, cardTS)
		}
	}
	if got := cardBeadsOf(daemon, "gd-esc-desk"); len(got) == 1 && got[0].Fields["thread_id"] != card {
		t.Errorf("desk card thread_id = %q, want the agent card %s", got[0].Fields["thread_id"], card)
	}
	if n := len(cardBeadsOf(daemon, "gd-agent-x")); n != 1 {
		t.Errorf("agent card beads after the escalations = %d, want the one card", n)
	}
	for _, c := range twin.Calls() {
		if c.Method == "chat.postMessage" && c.Params["thread_ts"] == "" && c.Params["text"] != "Agent: crew-x" {
			t.Errorf("top-level Slack post %q, want only the agent card at the top", c.Params["text"])
		}
	}
}

// TestDeskEscalationInAThreadWithNoHead: a desk card in a Slack thread the
// Commons has no head bead for is in #machines, never top-level in #general.
func TestDeskEscalationInAThreadWithNoHead(t *testing.T) {
	b, daemon := newEscalationCardsBot(t, surfacetest.NewSlackTwin())
	daemon.mu.Lock()
	delete(daemon.beads, "gd-ch-crew")
	daemon.mu.Unlock()
	putGeneralAndMachines(daemon, "C0CREW", true)
	ctx := context.Background()
	if ch := b.cardChannel(ctx, "gardenio", "C0CREW", "1690000009.000100", nil, true); ch.ID != "gd-ch-machines" || ch.Slack != "C0CREW" {
		t.Fatalf("machine card with no head = %+v, want gd-ch-machines with its C0CREW copy", ch)
	}
	head := &beadsapi.BeadDetail{ID: "gd-wish", Fields: map[string]string{"channel_id": "gd-ch-general"}}
	if ch := b.cardChannel(ctx, "gardenio", "C0CREW", "1690000009.000100", head, true); ch.ID != "gd-ch-general" {
		t.Fatalf("machine card under a #general head = %+v, want the head's channel", ch)
	}
	if ch := b.cardChannel(ctx, "gardenio", "C0CREW", "1690000009.000100", nil, false); ch.ID != "gd-ch-general" {
		t.Fatalf("person card in a thread = %+v, want gd-ch-general", ch)
	}
}

// TestCardThreadRelayRepliesUnderTheCard: a comment or squawk relayed under
// an agent's card is a reply to the card bead in #machines. With no card bead
// it stays out of every Commons channel, as before, rather than top-level.
func TestCardThreadRelayRepliesUnderTheCard(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", true)
	b.agentSpawnChannel["crew-a"] = "C0CREW"
	ctx := context.Background()
	if _, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW"); err != nil {
		t.Fatal(err)
	}
	card := cardBeadsOf(daemon, "gd-agent-a")[0].ID

	b.routeAgentThreadMessage(ctx, "crew-a", "gardenio", conversation.Message{Text: ":memo: agent commented on gd-x", Author: agentPrincipal("crew-a")})
	got := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["text"] == ":memo: agent commented on gd-x" })
	if len(got) != 1 || got[0].Fields["channel_id"] != "gd-ch-machines" || got[0].Fields["thread_id"] != card {
		t.Fatalf("relay beads = %+v, want one under %s in gd-ch-machines", got, card)
	}

	// No card bead: the hot cache knows a card the Commons never saw.
	b.agentCards["crew-b"] = MessageRef{ChannelID: "C0CREW", Timestamp: "1690000008.000100", Agent: "crew-b"}
	b.agentSpawnChannel["crew-b"] = "C0CREW"
	b.routeAgentThreadMessage(ctx, "crew-b", "gardenio", conversation.Message{Text: "squawk from b", Author: agentPrincipal("crew-b")})
	got = mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["text"] == "squawk from b" })
	if len(got) != 1 || got[0].Fields["channel_id"] != "" {
		t.Fatalf("relay beads with no card bead = %+v, want one with no channel_id", got)
	}
}

// TestThreadBoundAgentCardInMachines: an agent bound to a thread (thread_id
// or slack_thread_bead_id) whose card is posted top-level has it in #machines,
// and the comment relays and edits under that card follow it there
// (gd-741PBuHpVU: 14 such cards, 73 relays and 9 desk cards in #general). A
// card posted in the Slack thread still stays in the thread's channel
// (TestAgentCardInMachines).
func TestThreadBoundAgentCardInMachines(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", true)
	daemon.mu.Lock()
	daemon.beads["crew-p"] = &beadsapi.BeadDetail{
		ID: "gd-agent-p", Type: "agent", Status: "open", Title: "crew-p",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "crew-p", "project": "gardenio", "role": "crew", "agent_state": "working", "thread_id": "gd-plan-thread"},
	}
	daemon.beads["gd-plan-thread"] = &beadsapi.BeadDetail{ID: "gd-plan-thread", Type: "task", Status: "open", Title: "Slack thread: plan", Labels: []string{"project:gardenio"}}
	daemon.mu.Unlock()
	b.agentProject["crew-p"] = "gardenio"
	ctx := context.Background()

	for _, tc := range []struct{ agent, subject string }{{"crew-p", "gd-agent-p"}, {"crew-t", "gd-agent-t"}} {
		if _, err := b.ensureAgentCard(ctx, tc.agent, "C0CREW"); err != nil {
			t.Fatal(err)
		}
		cards := cardBeadsOf(daemon, tc.subject)
		if len(cards) != 1 || cards[0].Fields["channel_id"] != "gd-ch-machines" || cards[0].Fields["slack_channel"] != "C0CREW" {
			t.Fatalf("%s card beads = %+v, want one in gd-ch-machines with its C0CREW copy", tc.agent, cards)
		}
	}
	card := cardBeadsOf(daemon, "gd-agent-p")[0].ID

	b.agentSpawnChannel["crew-p"] = "C0CREW"
	b.routeAgentThreadMessage(ctx, "crew-p", "gardenio", conversation.Message{Text: ":memo: crew-p commented on gd-x", Author: agentPrincipal("crew-p")})
	got := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["text"] == ":memo: crew-p commented on gd-x" })
	if len(got) != 1 || got[0].Fields["channel_id"] != "gd-ch-machines" || got[0].Fields["thread_id"] != card {
		t.Fatalf("relay beads = %+v, want one under %s in gd-ch-machines", got, card)
	}

	// An edit of a top-level card the bridge has no memo or card bead for
	// upserts it in #machines, not as a second card in #general.
	b.forgetAgentCard("crew-p")
	daemon.mu.Lock()
	for id, bead := range daemon.beads {
		if bead.Fields["subject_id"] == "gd-agent-p" && bead.Fields["kind"] == "card" {
			delete(daemon.beads, id)
		}
	}
	daemon.mu.Unlock()
	ref := MessageRef{ChannelID: "C0CREW", Timestamp: "1690000007.000100", Agent: "crew-p"}
	_ = b.editAgentCard(ctx, "crew-p", ref, "Agent: crew-p", nil)
	cards := cardBeadsOf(daemon, "gd-agent-p")
	if len(cards) != 1 || cards[0].Fields["channel_id"] != "gd-ch-machines" || cards[0].Fields["thread_id"] != "" {
		t.Fatalf("card beads after the edit = %+v, want one top-level in gd-ch-machines", cards)
	}
}

// TestDecisionCardsInMachinesOnlyForTheDesk: a decision the merge desk asks
// (its requesting agent has the reviewer role) has its card in #machines, and
// so does the pointer of a decision delivered DM-first; an ordinary agent's
// decision card stays in #general. Every Slack copy still goes to the agent's
// channel (gd-GgRk2fLZKQ). The owner's and the operator's escalation cards
// stay in #general too (TestEscalationCardInMachinesOnlyForTheDesk).
func TestDecisionCardsInMachinesOnlyForTheDesk(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newDecisionCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", true)
	daemon.beads["gd-agent-desk"] = &beadsapi.BeadDetail{ID: "gd-agent-desk", Type: "agent", Status: "open", Title: "merge-desk-a", Labels: []string{"project:gardenio", "role:reviewer"},
		Fields: map[string]string{"agent": "merge-desk-a", "project": "gardenio", "role": "reviewer", "agent_state": "working"}}
	daemon.beads["merge-desk-a"] = daemon.beads["gd-agent-desk"]
	b.agentProject["merge-desk-a"] = "gardenio"
	b.agentSpawnChannel["merge-desk-a"] = "C0CREW"
	ctx := context.Background()

	for _, ev := range []BeadEvent{
		decisionCardEvent(daemon, "gd-dec-x", "crew-x", "gd-agent-x", 1),
		decisionCardEvent(daemon, "gd-dec-desk", "merge-desk-a", "gd-agent-desk", 2),
		decisionCardEvent(daemon, "gd-dec-o", "crew-o", "gd-agent-o", 0),
	} {
		if err := b.NotifyDecision(ctx, ev); err != nil {
			t.Fatalf("NotifyDecision %s: %v", ev.ID, err)
		}
	}
	for id, want := range map[string]string{"gd-dec-x": "gd-ch-general", "gd-dec-desk": "gd-ch-machines"} {
		if cards := cardBeadsOf(daemon, id); len(cards) != 1 || cards[0].Fields["channel_id"] != want || cards[0].Fields["slack_channel"] != "C0CREW" {
			t.Errorf("%s card beads = %+v, want one in %s with its C0CREW copy", id, cards, want)
		}
	}
	var pointer *beadsapi.BeadDetail
	for _, c := range cardBeadsOf(daemon, "gd-dec-o") {
		if c.Fields["slack_channel"] == "C0CREW" {
			pointer = c
		}
	}
	if pointer == nil || pointer.Fields["channel_id"] != "gd-ch-machines" {
		t.Errorf("gd-dec-o pointer card = %+v, want it in gd-ch-machines with its C0CREW copy", pointer)
	}
}

// TestDecisionFromDeskByCachedRole: with no requesting agent bead to read, the
// bridge's cache of agent roles classifies the decision.
func TestDecisionFromDeskByCachedRole(t *testing.T) {
	b, _ := newDecisionCardsBot(t, surfacetest.NewSlackTwin())
	b.agentRole["merge-desk-a"] = "reviewer"
	ctx := context.Background()
	if !b.decisionFromDesk(ctx, BeadEvent{Fields: map[string]string{}}, "merge-desk-a") {
		t.Error("decision by a cached reviewer is not from the desk")
	}
	if b.decisionFromDesk(ctx, BeadEvent{Fields: map[string]string{}}, "crew-x") {
		t.Error("decision by a crew is from the desk")
	}
	if b.decisionFromDesk(ctx, BeadEvent{Fields: map[string]string{"requesting_agent_bead_id": "gd-agent-x"}}, "merge-desk-a") {
		t.Error("the requesting agent bead's crew role did not win over the cache")
	}
}

// TestDecisionPointerInAGeneralThreadStaysInTheThread: the pointer of a
// decision DMed to a thread-bound agent's person is a reply in the thread's
// channel, #general, not in #machines.
func TestDecisionPointerInAGeneralThreadStaysInTheThread(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newDecisionCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", true)
	const threadTS = "1690000005.000100"
	daemon.beads["gd-agent-ot"] = &beadsapi.BeadDetail{ID: "gd-agent-ot", Type: "agent", Status: "open", Title: "crew-ot", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": "crew-ot", "project": "gardenio", "role": "crew", "agent_state": "working", "slack_user_id": "UOWNER",
			"slack_thread_channel": "C0CREW", "slack_thread_ts": threadTS, "slack_thread_bead_id": "gd-head-ot"}}
	daemon.beads["crew-ot"] = daemon.beads["gd-agent-ot"]
	daemon.beads["gd-head-ot"] = &beadsapi.BeadDetail{ID: "gd-head-ot", Type: "message", Status: "closed", Labels: []string{"project:gardenio"},
		Fields: map[string]string{"kind": "wish", "channel_id": "gd-ch-general", "slack_channel": "C0CREW", "slack_ts": threadTS}}
	b.agentProject["crew-ot"] = "gardenio"
	b.agentRole["crew-ot"] = "crew"

	if err := b.NotifyDecision(context.Background(), decisionCardEvent(daemon, "gd-dec-ot", "crew-ot", "gd-agent-ot", 1)); err != nil {
		t.Fatal(err)
	}
	var pointer *beadsapi.BeadDetail
	for _, c := range cardBeadsOf(daemon, "gd-dec-ot") {
		if c.Fields["slack_channel"] == "C0CREW" {
			pointer = c
		}
	}
	if pointer == nil || pointer.Fields["channel_id"] != "gd-ch-general" || pointer.Fields["thread_id"] != "gd-head-ot" {
		t.Fatalf("pointer card = %+v, want a reply under gd-head-ot in gd-ch-general", pointer)
	}
}

// TestCommonsWishAgentCardInItsThread: the thread agent of a wish the Commons
// minted (a head in a Commons channel with no Slack thread) has its card bead
// as a reply in the wish's thread, in the wish's channel, while its Slack copy
// is top-level in the channel the bridge routed it to; a relay under the card
// is a reply in the wish's thread too, and an edit after a roll (no memo)
// lands on the same bead. Before gd-rHZ1ktOO1E the bead was top-level in
// #machines with no thread_id, so a wish made in the Commons never showed its
// agent's card. A crew bound to the wish by thread_id alone keeps its
// top-level card (TestThreadBoundAgentCardInMachines).
func TestCommonsWishAgentCardInItsThread(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	putGeneralAndMachines(daemon, "C0CREW", true)
	daemon.mu.Lock()
	daemon.beads["thread-c"] = &beadsapi.BeadDetail{
		ID: "gd-agent-c", Type: "agent", Status: "open", Title: "thread-c",
		Labels: []string{"project:gardenio", "role:thread", "commons-thread"},
		Fields: map[string]string{
			"agent": "thread-c", "project": "gardenio", "role": "thread", "agent_state": "working",
			"spawn_source": "commons-thread", "thread_id": "gd-wish-c", "slack_thread_bead_id": "gd-wish-c",
		},
	}
	daemon.beads["gd-wish-c"] = &beadsapi.BeadDetail{
		ID: "gd-wish-c", Type: "task", Status: "open", Title: "Commons thread: @gardenio tend the pen",
		Labels: []string{"thread", "project:gardenio", "source:commons"},
		Fields: map[string]string{"channel_id": "gd-ch-general", "spawn_source": "commons-thread"},
	}
	daemon.mu.Unlock()
	b.agentProject["thread-c"] = "gardenio"
	b.agentRole["thread-c"] = "thread"
	b.agentState["thread-c"] = "working"
	ctx := context.Background()

	ts, err := b.ensureAgentCard(ctx, "thread-c", "C0CREW")
	if err != nil {
		t.Fatal(err)
	}
	cards := cardBeadsOf(daemon, "gd-agent-c")
	if len(cards) != 1 || cards[0].Fields["channel_id"] != "gd-ch-general" || cards[0].Fields["thread_id"] != "gd-wish-c" ||
		cards[0].Fields["slack_channel"] != "C0CREW" || cards[0].Fields["slack_ts"] != ts {
		t.Fatalf("card beads = %+v, want one under gd-wish-c in gd-ch-general with its top-level C0CREW copy", cards)
	}
	if calls := twin.Calls(); len(calls) != 1 || calls[0].Method != "chat.postMessage" {
		t.Fatalf("Slack calls = %+v, want the one card post", calls)
	}
	card := cards[0].ID

	b.agentSpawnChannel["thread-c"] = "C0CREW"
	b.routeAgentThreadMessage(ctx, "thread-c", "gardenio", conversation.Message{Text: ":memo: thread-c commented on gd-x", Author: agentPrincipal("thread-c")})
	got := mirroredBeads(daemon, func(m *beadsapi.BeadDetail) bool { return m.Fields["text"] == ":memo: thread-c commented on gd-x" })
	if len(got) != 1 || got[0].Fields["channel_id"] != "gd-ch-general" || got[0].Fields["thread_id"] != "gd-wish-c" {
		t.Fatalf("relay beads = %+v, want one in the wish's thread gd-wish-c in gd-ch-general, not under the card %s", got, card)
	}

	// After a roll the bridge has no memo: the edit finds the card bead by its
	// Slack copy and does not upsert a second, top-level card in #machines.
	b.forgetAgentCard("thread-c")
	ref := MessageRef{ChannelID: "C0CREW", Timestamp: ts, Agent: "thread-c"}
	_ = b.editAgentCard(ctx, "thread-c", ref, "Agent: thread-c (done)", nil)
	if cards := cardBeadsOf(daemon, "gd-agent-c"); len(cards) != 1 || cards[0].ID != card || cards[0].Fields["thread_id"] != "gd-wish-c" {
		t.Fatalf("card beads after the edit = %+v, want the one card %s still under gd-wish-c", cards, card)
	}
	if _, head, _ := b.agentNoticeTarget(ctx, "thread-c", "gardenio", "C0CREW", ts); head != "gd-wish-c" {
		t.Fatalf("notice target head after the roll = %q, want the wish gd-wish-c", head)
	}
}
