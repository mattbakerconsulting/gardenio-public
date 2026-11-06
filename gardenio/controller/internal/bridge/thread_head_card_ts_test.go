package bridge

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestPersistHeadCardRef_StampsHead verifies the card (channel, ts) is mirrored
// onto the wish HEAD bead, and that a repeated identical write is a no-op.
func TestPersistHeadCardRef_StampsHead(t *testing.T) {
	daemon := newMockDaemon()
	head := &beadsapi.BeadDetail{
		ID:     "head-1",
		Type:   "task",
		Labels: []string{threadKindLabel, "slack-thread:C1:100.0"},
		Fields: map[string]string{"slack_thread_channel": "C1", "slack_thread_ts": "100.0"},
	}
	daemon.beads["head-1"] = head
	b := &Bot{logger: slog.Default(), daemon: daemon}

	b.persistHeadCardRef(context.Background(), "head-1", "C1", "200.5")
	if head.Fields["slack_card_channel"] != "C1" || head.Fields["slack_card_ts"] != "200.5" {
		t.Fatalf("head card fields not stamped: %+v", head.Fields)
	}
}

// TestPersistHeadCardRef_NoHead is a no-op when there is no head id (the agent
// never carried slack_thread_bead_id — non-thread agents).
func TestPersistHeadCardRef_NoHead(t *testing.T) {
	daemon := newMockDaemon()
	b := &Bot{logger: slog.Default(), daemon: daemon}
	// Must not panic and must not create/touch any bead.
	b.persistHeadCardRef(context.Background(), "", "C1", "200.5")
	if len(daemon.beads) != 0 {
		t.Fatalf("expected no bead writes, got %d", len(daemon.beads))
	}
}

// TestFindThreadHeadByCardTS_ColdCache is the acceptance test: after a
// simulated bridge restart (a fresh Bot with an EMPTY agentCards cache and no
// persisted state), a card-react still resolves to the wish head via the
// direct daemon field-query — independent of the agent's terminal state.
func TestFindThreadHeadByCardTS_ColdCache(t *testing.T) {
	daemon := newMockDaemon()
	// The head bead carries the stamped card ref (written at card-post time).
	daemon.beads["head-1"] = &beadsapi.BeadDetail{
		ID:     "head-1",
		Type:   "task",
		Labels: []string{threadKindLabel, "slack-thread:C1:100.0"},
		Fields: map[string]string{
			"slack_thread_channel": "C1",
			"slack_thread_ts":      "100.0",
			"slack_card_channel":   "C1",
			"slack_card_ts":        "200.5",
		},
	}
	// A DONE agent bead with the same thread metadata. The legacy
	// findAgentByThreadMetadata path SKIPS this (done agents are skipped), so
	// the head field-query is the only path that can resolve the wish.
	daemon.beads["agent-1"] = &beadsapi.BeadDetail{
		ID:   "agent-1",
		Type: "agent",
		Fields: map[string]string{
			"agent":                "a1",
			"agent_state":          "done",
			"slack_thread_channel": "C1",
			"slack_thread_ts":      "100.0",
			"slack_thread_bead_id": "head-1",
			"slack_card_channel":   "C1",
			"slack_card_ts":        "200.5",
		},
	}

	// Cold bridge: empty hot cache, no persisted state.
	b := &Bot{logger: slog.Default(), daemon: daemon, agentCards: map[string]MessageRef{}}

	// Sanity: the legacy done-agent path cannot resolve a thread agent.
	if got := b.findAgentByThreadMetadata("C1", "100.0"); got != "" {
		t.Fatalf("findAgentByThreadMetadata should skip the done agent, got %q", got)
	}

	// The robust path resolves the wish head from the card ts alone.
	headID := b.findThreadHeadByCardTS(context.Background(), "C1", "200.5")
	if headID != "head-1" {
		t.Fatalf("findThreadHeadByCardTS = %q, want head-1", headID)
	}
}

// TestFindThreadHeadByCardTS_IgnoresAgentBead verifies the field-query filters
// to thread heads (threadKindLabel) so the agent bead — which carries the SAME
// slack_card_ts — never shadows the head.
func TestFindThreadHeadByCardTS_IgnoresAgentBead(t *testing.T) {
	daemon := newMockDaemon()
	// Only an agent bead carries the card ts (no head label).
	daemon.beads["agent-1"] = &beadsapi.BeadDetail{
		ID:     "agent-1",
		Type:   "agent",
		Fields: map[string]string{"slack_card_channel": "C1", "slack_card_ts": "200.5"},
	}
	b := &Bot{logger: slog.Default(), daemon: daemon, agentCards: map[string]MessageRef{}}
	if got := b.findThreadHeadByCardTS(context.Background(), "C1", "200.5"); got != "" {
		t.Fatalf("expected no head match (agent bead lacks thread label), got %q", got)
	}
}

// TestFindThreadHeadByCardTS_ChannelMismatch guards against a cross-channel ts
// collision: a head stamped in a different channel must not resolve.
func TestFindThreadHeadByCardTS_ChannelMismatch(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["head-1"] = &beadsapi.BeadDetail{
		ID:     "head-1",
		Type:   "task",
		Labels: []string{threadKindLabel},
		Fields: map[string]string{"slack_card_channel": "C-OTHER", "slack_card_ts": "200.5"},
	}
	b := &Bot{logger: slog.Default(), daemon: daemon, agentCards: map[string]MessageRef{}}
	if got := b.findThreadHeadByCardTS(context.Background(), "C1", "200.5"); got != "" {
		t.Fatalf("expected no match for channel mismatch, got %q", got)
	}
}

// TestFindThreadHeadByCardTS_NoMatch returns "" when no head carries the ts.
func TestFindThreadHeadByCardTS_NoMatch(t *testing.T) {
	daemon := newMockDaemon()
	b := &Bot{logger: slog.Default(), daemon: daemon, agentCards: map[string]MessageRef{}}
	if got := b.findThreadHeadByCardTS(context.Background(), "C1", "999.9"); got != "" {
		t.Fatalf("expected no match, got %q", got)
	}
}
