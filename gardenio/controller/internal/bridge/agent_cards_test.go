package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

// cardBeadsOf returns the card beads whose subject is subject.
func cardBeadsOf(d *mockDaemon, subject string) []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Type == "message" && b.Fields["kind"] == "card" && b.Fields["subject_id"] == subject {
			out = append(out, b)
		}
	}
	return out
}

// TestAgentCardRefsDualWritten: a card posted through the Router writes its
// card bead in the agent's land with the Slack ref, and the state file keeps
// its ref (dual-write for one release).
func TestAgentCardRefsDualWritten(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	ctx := context.Background()

	ts, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW")
	if err != nil {
		t.Fatal(err)
	}
	cards := cardBeadsOf(daemon, "gd-agent-a")
	if len(cards) != 1 {
		t.Fatalf("card beads of crew-a = %d, want 1", len(cards))
	}
	c := cards[0]
	if c.Fields["slack_channel"] != "C0CREW" || c.Fields["slack_ts"] != ts ||
		len(c.Labels) != 1 || c.Labels[0] != "project:gardenio" || c.Fields["author"] != "agent:crew-a" {
		t.Fatalf("card bead = %+v", c)
	}
	if ref, ok := b.state.GetAgentCard("crew-a"); !ok || ref.Timestamp != ts {
		t.Fatalf("state file card = %+v, %v; want ts %s", ref, ok, ts)
	}

	// A thread card's bead is in its wish head's thread.
	if _, err := b.ensureAgentCard(ctx, "crew-t", "C0THREAD", "1690000000.000100"); err != nil {
		t.Fatal(err)
	}
	if cards := cardBeadsOf(daemon, "gd-agent-t"); len(cards) != 1 || cards[0].Fields["thread_id"] != "gd-head-t" {
		t.Fatalf("thread card beads = %+v", cards)
	}

	// Edits re-render the same card bead; none is written again.
	b.flushAgentCard(ctx, "crew-a")
	b.replaceAgentCardWithWrapUp(ctx, "crew-a", "done", `{"tldr":"ok"}`)
	if n := len(cardBeadsOf(daemon, "gd-agent-a")); n != 1 {
		t.Fatalf("card beads of crew-a after edits = %d, want 1", n)
	}
}

// TestAgentCardEditBackfillsCardBead: a card posted before slice 2 has no card
// bead; its first edit writes one recording the card's ref.
func TestAgentCardEditBackfillsCardBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	b.agentCards["crew-a"] = MessageRef{ChannelID: "C0CREW", Timestamp: "1690000000.000300", Agent: "crew-a"}

	b.flushAgentCard(context.Background(), "crew-a")

	cards := cardBeadsOf(daemon, "gd-agent-a")
	if len(cards) != 1 || cards[0].Fields["slack_ts"] != "1690000000.000300" || cards[0].Fields["slack_channel"] != "C0CREW" {
		t.Fatalf("card beads after the first edit = %+v", cards)
	}
	if calls := twin.Calls(); len(calls) != 1 || calls[0].Method != "chat.update" {
		t.Fatalf("Slack calls = %+v, want the one chat.update", calls)
	}
}

// TestAgentCardReadsBeadBeforeState: with the hot cache empty, the card bead's
// ref wins over a stale state-file ref, and no new card is posted.
func TestAgentCardReadsBeadBeforeState(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	ctx := context.Background()
	ts, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW")
	if err != nil {
		t.Fatal(err)
	}
	// A restart: the hot cache and the memo are gone, the state file is stale.
	b.agentCards = map[string]MessageRef{}
	b.conv.cards = nil
	if err := b.state.SetAgentCard("crew-a", MessageRef{ChannelID: "C0CREW", Timestamp: "1690000000.000999", Agent: "crew-a"}); err != nil {
		t.Fatal(err)
	}
	daemon.beads["crew-a"].Fields["slack_card_ts"] = ""
	twin.Reset()

	got, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW")
	if err != nil {
		t.Fatal(err)
	}
	if got != ts {
		t.Fatalf("ensureAgentCard = %s, want the card bead's %s", got, ts)
	}
	if ref, ok := b.resolveAgentCardRef(ctx, "crew-a"); !ok || ref.Timestamp != ts {
		t.Fatalf("resolveAgentCardRef = %+v, want the card bead's ts", ref)
	}
	if n := len(twin.Calls()); n != 0 {
		t.Fatalf("resurrecting from the card bead made %d Slack calls", n)
	}
}

// TestAgentCardWithoutLandGoesToSlackDirectly: with no agent bead there is no
// subject or land, so the card posts to Slack as before and writes no bead.
func TestAgentCardWithoutLandGoesToSlackDirectly(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	before := len(daemon.beads)
	if _, err := b.ensureAgentCard(context.Background(), "nobody", "C0CREW"); err != nil {
		t.Fatal(err)
	}
	if len(daemon.beads) != before {
		t.Fatal("a card with no subject wrote a bead")
	}
	if calls := twin.Calls(); len(calls) != 1 || calls[0].Method != "chat.postMessage" {
		t.Fatalf("Slack calls = %+v", calls)
	}
}

// TestAgentCardNoBeadIsRemembered: the any-channel lookup that finds no card
// bead is not repeated on the next agent event, and a posted card clears it.
func TestAgentCardNoBeadIsRemembered(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	ctx := context.Background()
	if _, ok := b.agentCardFromBeads(ctx, "crew-a", ""); ok {
		t.Fatal("found a card bead before any card was posted")
	}
	before := daemon.getGetCalls()
	if _, ok := b.agentCardFromBeads(ctx, "crew-a", ""); ok || daemon.getGetCalls() != before {
		t.Fatalf("the second lookup read the daemon again (%d reads)", daemon.getGetCalls()-before)
	}
	ts, err := b.ensureAgentCard(ctx, "crew-a", "C0CREW")
	if err != nil {
		t.Fatal(err)
	}
	if ref, ok := b.agentCardFromBeads(ctx, "crew-a", ""); !ok || ref.Timestamp != ts {
		t.Fatalf("after the post: %+v %v, want the card bead's ts %s", ref, ok, ts)
	}
}

// respawnAgentWithoutHead replaces agent's bead with a newer one bound to the
// same Slack thread but carrying no thread head, as a decision-resume respawn
// minted before gd-AflFRvDw7Z did, and drops what the Bot remembers.
func respawnAgentWithoutHead(b *Bot, d *mockDaemon, agent, id string) {
	d.mu.Lock()
	d.beads[agent] = &beadsapi.BeadDetail{
		ID: id, Type: "agent", Status: "open", Title: agent,
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{
			"agent": agent, "project": "gardenio", "role": "thread", "agent_state": "working",
			"slack_thread_channel": "C0THREAD", "slack_thread_ts": "1690000000.000100",
		},
	}
	d.mu.Unlock()
	b.conv.cards, b.conv.noCard = nil, nil
}

// TestAgentCardEditOfRespawnLandsOnItsCardBead: a respawned thread agent's
// edit of its card finds the card bead by its Slack copy, though the bead's
// subject is the agent's earlier bead, and writes no second card bead.
func TestAgentCardEditOfRespawnLandsOnItsCardBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	ctx := context.Background()
	ts, err := b.ensureAgentCard(ctx, "crew-t", "C0THREAD", "1690000000.000100")
	if err != nil {
		t.Fatal(err)
	}
	respawnAgentWithoutHead(b, daemon, "crew-t", "gd-agent-t2")
	twin.Reset()

	ref := MessageRef{ChannelID: "C0THREAD", Timestamp: ts, ThreadTS: "1690000000.000100", Agent: "crew-t"}
	if err := b.editAgentCard(ctx, "crew-t", ref, "working", nil); err != nil {
		t.Fatal(err)
	}
	if cards := cardBeadsOf(daemon, "gd-agent-t2"); len(cards) != 0 {
		t.Fatalf("the respawn's edit wrote card beads %+v", cards)
	}
	if cards := cardBeadsOf(daemon, "gd-agent-t"); len(cards) != 1 || cards[0].Fields["thread_id"] != "gd-head-t" {
		t.Fatalf("card beads of the first agent bead = %+v, want the one threaded card", cards)
	}
	if calls := twin.Calls(); len(calls) != 1 || calls[0].Method != "chat.update" {
		t.Fatalf("Slack calls = %+v, want the one chat.update", calls)
	}
}

// TestAgentCardEditInThreadWithoutHeadWritesNoBead: a card in a Slack thread
// whose agent names no thread head is edited in Slack only, not upserted as a
// top-level card bead in the channel.
func TestAgentCardEditInThreadWithoutHeadWritesNoBead(t *testing.T) {
	twin := surfacetest.NewSlackTwin()
	b, daemon := newAgentCardsBot(t, twin)
	respawnAgentWithoutHead(b, daemon, "crew-t", "gd-agent-t2")

	ref := MessageRef{ChannelID: "C0THREAD", Timestamp: "1690000000.000200", ThreadTS: "1690000000.000100", Agent: "crew-t"}
	if err := b.editAgentCard(context.Background(), "crew-t", ref, "working", nil); err != nil {
		t.Fatal(err)
	}
	if cards := cardBeadsOf(daemon, "gd-agent-t2"); len(cards) != 0 {
		t.Fatalf("the edit wrote card beads %+v", cards)
	}
	if calls := twin.Calls(); len(calls) != 1 || calls[0].Method != "chat.update" {
		t.Fatalf("Slack calls = %+v, want the one chat.update", calls)
	}
}
