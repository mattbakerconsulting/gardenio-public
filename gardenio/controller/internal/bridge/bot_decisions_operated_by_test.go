package bridge

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestResolveDecisionAddressee_OperatedByWins is the whole point of decision
// `operated_by` (ka-0000000024): a decision may be addressed to a human who is
// NOT the filing agent's operator. Before this, the addressee could only ever be
// derived from the agent bead, so the motivating case — asking an MR's author
// whether to push a fix — was inexpressible.
func TestResolveDecisionAddressee_OperatedByWins(t *testing.T) {
	daemon := newMockDaemon()
	daemon.slackAliases = []beadsapi.SlackOperatorAlias{
		{OperatorID: 1, Subject: "UAUTHOR", Email: "Mr.Author@acme.example", Display: "MR Author"},
	}
	seedDecisionAgent(daemon, "agent-x", "UAGENTOWNER")
	bot := newTestBot(daemon, newFakeSlackServer(t))

	got := bot.resolveDecisionAddressee(context.Background(), map[string]string{
		"operated_by": "oidc:mr.author@acme.example",
		"gifted_by":   "oidc:someone.else@acme.example",
	}, "agent-x")

	if got.SlackID != "UAUTHOR" {
		t.Errorf("expected the decision's own operated_by to win, got %q (agent owner is UAGENTOWNER)", got.SlackID)
	}
	if !got.Gifted {
		t.Errorf("a decision carrying gifted_by should report Gifted=true")
	}
}

// TestResolveDecisionAddressee_FallsBackToAgentOwner asserts the overwhelming
// majority case is untouched: no operated_by → today's derived owner.
func TestResolveDecisionAddressee_FallsBackToAgentOwner(t *testing.T) {
	daemon := newMockDaemon()
	seedDecisionAgent(daemon, "agent-x", "UAGENTOWNER")
	bot := newTestBot(daemon, newFakeSlackServer(t))

	got := bot.resolveDecisionAddressee(context.Background(), map[string]string{}, "agent-x")

	if got.SlackID != "UAGENTOWNER" {
		t.Errorf("expected the derived agent owner, got %q", got.SlackID)
	}
	if got.Gifted {
		t.Errorf("an un-addressed decision is not a gift")
	}
}

// TestResolveDecisionAddressee_UnresolvableOperatedByDegrades pins the fail-safe:
// an operated_by the alias feed cannot resolve must NOT make the decision
// undeliverable — it falls back to the derived owner.
func TestResolveDecisionAddressee_UnresolvableOperatedByDegrades(t *testing.T) {
	daemon := newMockDaemon() // empty alias feed
	seedDecisionAgent(daemon, "agent-x", "UAGENTOWNER")
	bot := newTestBot(daemon, newFakeSlackServer(t))

	got := bot.resolveDecisionAddressee(context.Background(), map[string]string{
		"operated_by": "oidc:nobody.here@acme.example",
	}, "agent-x")

	if got.SlackID != "UAGENTOWNER" {
		t.Errorf("unresolvable operated_by should degrade to the derived owner, got %q", got.SlackID)
	}
}

// TestNotifyDecision_DMsTheAddressedHuman is the end-to-end shape of the
// operator's opening ask: an agent asks the MR's author — not its own operator —
// whether to push a fix, and the card lands in THAT human's DMs.
func TestNotifyDecision_DMsTheAddressedHuman(t *testing.T) {
	daemon := newMockDaemon()
	daemon.slackAliases = []beadsapi.SlackOperatorAlias{
		{OperatorID: 1, Subject: "UAUTHOR", Email: "mr.author@acme.example", Display: "MR Author"},
	}
	seedDecisionAgent(daemon, "agent-x", "UAGENTOWNER")
	f := newDMSlackFixture(t)
	bot := newTestBot(daemon, f.srv)
	bindDecisionChannel(bot, "agent-x", "C123")

	ev := decisionEvent("dec-1", "agent-x")
	ev.Fields["operated_by"] = "oidc:mr.author@acme.example"
	ev.Fields["gifted_by"] = "oidc:fleet.agent@acme.example"

	if err := bot.NotifyDecision(context.Background(), ev); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}

	posts, _, _ := f.snapshot()
	var dm, pointer *dmSlackCall
	for i := range posts {
		if posts[i].channel == fixtureDMChannel {
			dm = &posts[i]
		} else if strings.Contains(posts[i].text, "Decision sent to") {
			pointer = &posts[i]
		}
	}
	if dm == nil {
		t.Fatalf("expected the card in the addressed human's DM; posts=%+v", posts)
	}
	if !strings.Contains(dm.blocks, "resolve_dec-1_1") {
		t.Errorf("the DMed card must be answerable, got %s", dm.blocks)
	}
	if !strings.Contains(dm.blocks, "UAUTHOR") {
		t.Errorf("the card should @-mention the ADDRESSED human, got %s", dm.blocks)
	}
	if strings.Contains(dm.blocks, "UAGENTOWNER") {
		t.Errorf("the agent's own operator must not be the addressee here, got %s", dm.blocks)
	}
	if pointer == nil || !strings.Contains(pointer.text, "<@UAUTHOR>") {
		t.Errorf("channel pointer should name the addressed human, got %+v", pointer)
	}
}
