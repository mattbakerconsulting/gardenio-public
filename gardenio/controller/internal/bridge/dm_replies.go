package bridge

import (
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack/slackevents"
)

// Reply capture for the Commons DM channel (design spec
// 2026-10-19-lands-commons-design.md, section 4.2: "reply capture moves from
// dm_delivery.go to the DM channel's reply handler"). An agent's gb say --to
// --ask is a kind=ask message bead in its DM channel (api_conversation_dm.go);
// the person answers on either surface:
//
//   - Slack: a reply in the thread of the ask's Slack IM is written as a
//     reply message bead under the ask in the DM channel (origin=slack), the
//     asking agent is nudged with it, and the thread is told the reply was
//     captured, as dm_delivery.go tells a gb dm ask's.
//   - Commons: a person's message in a DM channel, a reply under the ask or a
//     new message, is dispatched like any Commons reply (inbound_reply.go:
//     the inbound call, the SSE watcher below, the sweep) and nudges every
//     agent in the DM but its author.
//
// The agent reads the answer with gb thread <ask-id>. dm_delivery.go keeps
// capturing replies to the type=dm beads gb dm ask still writes.

// askBySlackThread returns the ask message bead whose Slack IM copy is the
// root of the Slack thread (channel, threadTS), or nil.
func (b *Bot) askBySlackThread(ctx context.Context, channel, threadTS string) *beadsapi.BeadDetail {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Fields: map[string]string{conversation.BeadKind: string(conversation.KindAsk), beadSlackChannel: channel, beadSlackTS: threadTS},
		Limit:  1,
	})
	if err != nil {
		b.logger.Warn("dm: ask lookup by Slack thread failed", "channel", channel, "thread_ts", threadTS, "error", err)
		return nil
	}
	if len(res.Beads) == 0 {
		return nil
	}
	return res.Beads[0]
}

// captureAskReply handles a Slack IM thread reply under an ask's Slack copy.
// It reports whether the reply was an answer to an ask.
func (b *Bot) captureAskReply(ctx context.Context, ev *slackevents.MessageEvent) bool {
	if b.daemon == nil || ev.ThreadTimeStamp == "" || ev.ThreadTimeStamp == ev.TimeStamp || ev.BotID != "" {
		return false
	}
	ask := b.askBySlackThread(ctx, ev.Channel, ev.ThreadTimeStamp)
	if ask == nil {
		return false
	}
	land, dm := projectFromLabels(ask.Labels), ask.Fields[conversation.BeadChannelID]
	asker := bareAgent(ask.Fields[conversation.BeadAuthor])

	p, linked := b.personBySlackID(ctx, ev.User)
	who := conversation.Principal{Name: p.Name}
	if linked {
		who.ID = p.principal()
	}
	if who.Name == "" {
		who.Name = ev.User
	}
	if land != "" && dm != "" {
		m := conversation.Message{
			Text:   ev.Text,
			Author: who,
			Origin: conversation.SurfaceSlack,
			Fields: map[string]string{beadSlackChannel: ev.Channel, beadSlackTS: ev.TimeStamp, "slack_user_id": ev.User},
		}
		if _, err := b.Conversation().Reply(ctx, conversation.ChannelRef{ID: dm, Land: land}, conversation.Thread{Head: ask.ID}, m, false); err != nil {
			b.logger.Warn("dm: Slack reply to an ask not written to its DM", "ask", ask.ID, "error", err)
		}
	}

	answerTo := ev.User
	if linked {
		answerTo = p.principal()
	}
	b.nudgeDMAgent(ctx, asker, fmt.Sprintf("Reply to your question (bead %s) from %s: %s\nRead it with: gb thread %s; answer with: gb say --to %s \"...\"",
		ask.ID, who.Name, truncateText(ev.Text, 500), ask.ID, answerTo))
	_ = b.replyInThread(ctx, ev.Channel, ev.ThreadTimeStamp,
		b.agentTheme(ctx, asker).Semantic(SemOK)+" Reply captured — the agent has been notified.")
	return true
}

// commonsDM returns the channel bead id names, when it is a live DM.
func (b *Bot) commonsDM(ctx context.Context, id string) (*beadsapi.BeadDetail, bool) {
	if b.daemon == nil || id == "" {
		return nil, false
	}
	ch, err := b.daemon.GetBead(ctx, id)
	if err != nil || !conversation.IsDM(ch) {
		return nil, false
	}
	return ch, true
}

// handleCommonsDM nudges every agent in the DM channel dm but the author of
// in, a person's message there: as the answer to an ask when in replies under
// one, else as a DM.
func (b *Bot) handleCommonsDM(ctx context.Context, in conversation.Inbound, dm *beadsapi.BeadDetail) error {
	ask, asker := "", ""
	if in.Thread != "" {
		if head, err := b.daemon.GetBead(ctx, in.Thread); err == nil && head != nil && head.Fields[conversation.BeadKind] == string(conversation.KindAsk) {
			ask, asker = head.ID, head.Fields[conversation.BeadAuthor]
		}
	}
	who := strings.TrimPrefix(in.Actor.ID, "oidc:")
	for _, member := range conversation.MembersOf(dm.Fields) {
		agent, ok := strings.CutPrefix(member, "agent:")
		if !ok || member == in.Actor.ID {
			continue
		}
		msg := fmt.Sprintf("DM from %s in the Commons (bead %s): %s", who, in.Message, truncateText(in.Text, 500))
		if ask != "" && member == asker {
			msg = fmt.Sprintf("Reply to your question (bead %s) from %s in the Commons: %s\nRead it with: gb thread %s",
				ask, who, truncateText(in.Text, 500), ask)
		}
		b.nudgeDMAgent(ctx, agent, msg+fmt.Sprintf("\nAnswer with: gb say --to %s \"...\"", in.Actor.ID))
	}
	return nil
}

// nudgeDMAgent nudges agent with a DM's words. Best-effort: they are in the DM
// channel either way, for gb thread to read.
func (b *Bot) nudgeDMAgent(ctx context.Context, agent, msg string) {
	if agent == "" || b.daemon == nil {
		return
	}
	if err := NudgeAgent(ctx, b.daemon, b.logger, agent, msg); err != nil {
		b.logger.Warn("dm: nudging the agent failed", "agent", agent, "error", err)
	}
}

// RegisterCommonsDMHandlers dispatches a person's Commons message in a DM
// channel, and a reply in a Commons thread, as soon as its bead is written, so
// the agent hears it without waiting for the sweep. The dispatch is the one the inbound call and
// the sweep make (dispatchCommonsMessage), so each message reaches its agents
// once whichever comes first.
func (b *Bot) RegisterCommonsDMHandlers(stream *SSEStream) {
	stream.On("beads.bead.created", b.handleCommonsDMEvent)
	stream.On("beads.bead.closed", b.handleCommonsDMEvent)
	if b.logger != nil { // a zero-value Bot (the wiring test) has none
		b.logger.Info("commons DMs registered SSE handlers", "topics", []string{"beads.bead.created", "beads.bead.closed"})
	}
}

func (b *Bot) handleCommonsDMEvent(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Status != "closed" {
		return
	}
	m := commonsMessageFromEvent(bead)
	if !m.fromCommons() || m.Fields[commonsHandledAt] != "" {
		return
	}
	if _, ok := b.commonsDM(ctx, m.Fields[conversation.BeadChannelID]); !ok {
		// A reply in a Commons thread reaches its agent now, not at the sweep
		// (slice 4d); a thread-say reply is ThreadMessageForward's, and a
		// top-level message is said to the channel.
		if m.Fields[conversation.BeadThreadID] == "" || hasLabel(m.Labels, ThreadSayLabel) {
			return
		}
	}
	if b.commonsComposes(ctx, m) {
		return // a mention is the inbound call's alone (inbound_compose.go)
	}
	if _, _, err := b.dispatchCommonsMessage(ctx, m); err != nil {
		b.logger.Warn("dm: commons DM dispatch failed", "message", m.ID, "error", err)
	}
}
