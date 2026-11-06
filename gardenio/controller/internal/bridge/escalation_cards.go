package bridge

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// Escalation cards through the conversation Router (design spec
// 2026-10-19-lands-commons-design.md, section 5.2, slice 3). The escalation
// card is posted through the Router like a decision card (decision_cards.go):
// a card bead (kind=card, subject_id = the escalation) in the escalation's land
// with the Slack ref on it. The operator DM is recorded on its own card bead,
// in the Commons DM channel between the requesting agent and the operator, as
// the DM-first decision card is (dm_channels.go). The Commons renders every card bead from
// its subject (internal/commons), so an approval, a result or a rejection
// re-renders there when the escalation changes.
//
// The Slack faces do not change (TestEscalationCardCallsGolden). The edits
// after a post (approved, result, rejected) and the pingback still go to the
// Slack copy the Bot tracks; the queue alert and the stale digest stay
// Slack-only ops notices.
//
// SENSITIVITY. NotifyNewEscalation and the operator DM keep the Slack channel
// and ts of a sensitive or secret escalation off every bead a bearer-token
// agent can read (ka-0000003667). A card bead is such a bead, so the card of a
// sensitive escalation is written without its Slack ref: the Commons copy is
// a card bead in the bound channel, and the Slack copy is posted directly. Its
// operator DM is not recorded at all, so it has no card in the operator's DM
// channel either.

// postEscalationCard posts the escalation card of bead, filed by agentName,
// into slackChannel (as a reply in threadTS when set), with blocks as its Slack
// face and text as its fallback. When Slack refuses the blocks, the card is
// posted again as text alone, so an escalation is never dropped for its face
// (ka-0000000674).
func (b *Bot) postEscalationCard(ctx context.Context, bead BeadEvent, agentName, slackChannel, threadTS, text string, blocks []slack.Block) (MessageRef, error) {
	land := b.landOf(bead.Labels, agentName)
	author := agentPrincipal(b.escalationRequester(ctx, bead))
	// A desk escalation (a merge the merge desk takes) asks no person: it is
	// machine traffic and goes to #machines (single voice). The owner's and
	// the operator's stay where a person reads them; in gardenio the
	// operator is the owner at the laptop.
	machine := beadsapi.EscalationAudience(bead.Fields) == beadsapi.AudienceDesk
	if escalationDMWithholdsBody(bead.Fields["sensitivity"]) {
		b.postCommonsOnlyCard(ctx, land, bead.ID, author, slackChannel, threadTS, text, machine)
		land = "" // the Slack copy goes direct, so its ref lands on no bead
	}
	ref, cardBead, err := b.postSubjectCardBead(ctx, land, bead.ID, author, slackChannel, threadTS, text, blocks, machine)
	if err == nil {
		return ref, nil
	}
	// slackBlockRenderError also unwraps Slack's response_metadata.messages,
	// which names the offending block. slack-go's SlackErrorResponse.Error()
	// returns only the bare code ("invalid_blocks"), discarding the detail —
	// which is why the root cause of these drops stayed invisible in logs.
	detail, isBlockErr := slackBlockRenderError(err)
	if !isBlockErr {
		return MessageRef{}, err
	}
	b.logger.Error("escalation blocks rejected by Slack — retrying as plain text",
		"id", bead.ID, "channel", slackChannel, "detail", detail)
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	channelID, ts, err := b.api.PostMessageContext(ctx, slackChannel, opts...)
	if err != nil {
		return MessageRef{}, err
	}
	if channelID == "" {
		channelID = slackChannel
	}
	ref = MessageRef{ChannelID: channelID, Timestamp: ts, ThreadTS: threadTS}
	if cardBead != "" && ts != "" {
		// The Router wrote the card bead before Slack refused it; the text
		// copy is the one later edits go to.
		stamp := map[string]string{conversation.SurfaceSlack + "_channel": channelID, conversation.SurfaceSlack + "_ts": ts}
		if err := b.daemon.UpdateBeadFields(ctx, cardBead, stamp); err != nil {
			b.logger.Warn("card: escalation text copy not recorded on its card bead", "bead", cardBead, "error", err)
		}
	}
	return ref, nil
}

// postCommonsOnlyCard writes the card bead of subject in the Commons channel
// bound to slackChannel (#machines for a top-level machine card, cardChannel)
// without projecting it to Slack, so the bead carries no Slack ref. In
// threadTS it is a reply under the thread's head when the Commons has one.
// Nothing is written when the land has no channel bead bound to slackChannel:
// such a card would show nowhere.
func (b *Bot) postCommonsOnlyCard(ctx context.Context, land, subject string, author conversation.Principal, slackChannel, threadTS, text string, machine bool) {
	if b.daemon == nil || land == "" || subject == "" || slackChannel == "" {
		return
	}
	head := b.threadHeadFor(ctx, land, slackChannel, threadTS)
	ch := b.cardChannel(ctx, land, slackChannel, threadTS, head, machine)
	if ch.ID == "" {
		return
	}
	ch.Slack = "" // binds no Slack surface: the Router writes the bead alone
	m := conversation.Message{Kind: conversation.KindCard, SubjectID: subject, Text: text, Author: author}
	r := b.Conversation()
	var err error
	if head != nil {
		_, err = r.Reply(ctx, ch, conversation.Thread{Head: head.ID}, m, false)
	} else {
		_, err = r.Post(ctx, ch, m)
	}
	if err != nil {
		b.logger.Warn("card: Commons copy of a sensitive card not written", "subject", subject, "error", err)
	}
}

// threadHeadFor is cardThreadHead, nil outside a thread.
func (b *Bot) threadHeadFor(ctx context.Context, land, slackChannel, threadTS string) *beadsapi.BeadDetail {
	if threadTS == "" || b.daemon == nil {
		return nil
	}
	return b.cardThreadHead(ctx, land, slackChannel, threadTS)
}

// escalationRequester is the name of the agent that filed escalation bead.
// gb escalation create writes the agent's BEAD id into requesting_agent
// (older rows carry the name), so an id-shaped value is resolved to its agent
// bead's agent field, as decisions use the name: the Commons DM between an
// agent and a person is keyed by agent:<name>, and a bead id there would
// open a second DM with the same two members (gd-DyGO2Z7zh9). A value that
// does not resolve is kept as it is.
func (b *Bot) escalationRequester(ctx context.Context, bead BeadEvent) string {
	agent := extractAgentName(strings.TrimSpace(bead.Fields["requesting_agent"]))
	if b.daemon == nil || !beadIDPattern.MatchString(agent) {
		return agent
	}
	ab, err := b.daemon.GetBead(ctx, agent)
	if err != nil || ab == nil {
		b.logger.Warn("escalation: requesting agent bead not resolved to a name",
			"escalation", bead.ID, "agent_bead", agent, "error", err)
		return agent
	}
	if name := extractAgentName(strings.TrimSpace(ab.Fields["agent"])); name != "" {
		return name
	}
	return agent
}

// recordEscalationDM records the operator DM of escalation bead, sent to the
// operator at email and posted at (channelID, ts) with text, on its own card
// bead in the escalation's land: in the Commons DM channel between the
// requesting agent and the operator when both are known (recordDMCard). It is
// the EscalationOperatorDM's RecordDM hook. A sensitive or secret escalation's
// DM is never recorded: its DM channel stays off every bead.
func (b *Bot) recordEscalationDM(ctx context.Context, bead BeadEvent, email, channelID, ts, text string) {
	if escalationDMWithholdsBody(bead.Fields["sensitivity"]) || channelID == "" || ts == "" {
		return
	}
	agent := b.escalationRequester(ctx, bead)
	var p dmPerson
	if email != "" {
		p = b.personByEmail(ctx, email)
	}
	b.recordDMCard(ctx, b.landOf(bead.Labels, agent), bead.ID, agentPrincipal(agent), p,
		MessageRef{ChannelID: channelID, Timestamp: ts}, text)
}
