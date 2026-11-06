package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// Agent cards through the conversation Router (design spec
// 2026-10-19-lands-commons-design.md, section 5.2, slice 2). Every agent card
// post and in-place edit goes through the Router, which upserts the card bead
// (kind=card, subject_id = the agent bead) in the agent's land with the Slack
// ref on it. That is the dual-write: StateData.AgentCards and the agent bead's
// slack_card_* fields are still written, and a cache miss reads the card bead
// before the state file. The Commons renders the same card bead from its
// subject (internal/commons).
//
// The Slack face does not change: the notifier builds the Block Kit as it
// always did, from Bot state the surface does not hold (theme, tool stats,
// trigger, cost), and the Slack surface sends it unchanged
// (TestAgentCardCallsGolden).

// noCardBeadTTL is how long the Bot trusts that an agent has no card bead.
// updateAgentCard asks on every agent event while the hot cache has no card
// (a thread-bound agent never has one), so the answer is not re-read each time.
const noCardBeadTTL = 5 * time.Minute

// slackFaceKey carries a card's Block Kit to the Slack surface's renderer.
type slackFaceKey struct{}

// withSlackFace returns ctx carrying blocks, the Slack face of the card being
// routed under it.
func withSlackFace(ctx context.Context, blocks []slack.Block) context.Context {
	return context.WithValue(ctx, slackFaceKey{}, blocks)
}

// renderSlackFace is the Slack surface's renderer: the face withSlackFace put
// on ctx. A card routed without one is refused rather than posted bare; any
// other message without one is its text.
func renderSlackFace(ctx context.Context, _ string, m conversation.Message) ([]slack.Block, error) {
	blocks, ok := ctx.Value(slackFaceKey{}).([]slack.Block)
	if !ok && m.KindOrText() == conversation.KindCard {
		return nil, fmt.Errorf("no Slack face for card %s: %w", m.SubjectID, conversation.ErrUnsupported)
	}
	return blocks, nil
}

// agentCardMemo is what the Bot knows about an agent's card bead: the bead,
// its subject and channel, and the Slack copy the bead records. An edit of
// that copy needs no daemon read.
type agentCardMemo struct {
	bead    string
	subject string
	ch      conversation.ChannelRef
	at      conversation.Ref
	// head is the thread head the card bead is a reply under ("" for a
	// top-level card): a Commons wish's agent card sits in the wish's thread
	// (postCommonsWishCard), and a reply under the card goes there too.
	head string
}

func (b *Bot) rememberAgentCard(agent string, m agentCardMemo) {
	if m.bead == "" {
		return
	}
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	if b.conv.cards == nil {
		b.conv.cards = map[string]agentCardMemo{}
	}
	b.conv.cards[agent] = m
	delete(b.conv.noCard, agent)
}

// noCardBead reports whether agent was found to have no card bead within
// noCardBeadTTL.
func (b *Bot) noCardBead(agent string) bool {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	at, ok := b.conv.noCard[agent]
	return ok && time.Since(at) < noCardBeadTTL
}

func (b *Bot) rememberNoCardBead(agent string) {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	if b.conv.noCard == nil {
		b.conv.noCard = map[string]time.Time{}
	}
	b.conv.noCard[agent] = time.Now()
}

// forgetAgentCard drops what the Bot remembers about agent's card bead, once
// the prune has compressed the card for good.
func (b *Bot) forgetAgentCard(agent string) {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	delete(b.conv.cards, agent)
	delete(b.conv.noCard, agent)
}

func (b *Bot) agentCardMemo(agent string) (agentCardMemo, bool) {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	m, ok := b.conv.cards[agent]
	return m, ok
}

// agentCardTarget is where an agent's card lives in the conversation model.
type agentCardTarget struct {
	subject string // the agent bead id
	ch      conversation.ChannelRef
	head    string // the thread head the agent is bound to
	agent   *beadsapi.BeadDetail
}

// agentCardTarget resolves the card of agent in the Slack channel the bridge
// routed it to: its subject (the agent bead), its land, the channel bead bound
// to that Slack channel (none yet when the land has no Commons), and the
// thread head the agent is bound to. ok is false with no daemon, no agent bead
// or no land; the card then goes to Slack directly, as before slice 2.
//
// A top-level card (not inThread) is a machine post: its channel is the
// land's #machines when it has one (machinesChannel), even for an agent bound
// to a thread, since a top-level post never threads under its head. A card in
// a thread stays in that thread's channel (gd-741PBuHpVU: crews a plan
// gardener bound to a thread put their cards, relays and desk cards top-level
// in #general).
func (b *Bot) agentCardTarget(ctx context.Context, agent, slackChannel string, inThread bool) (agentCardTarget, bool) {
	if b.daemon == nil || slackChannel == "" {
		return agentCardTarget{}, false
	}
	detail := b.findAgentBeadForCard(ctx, agent)
	if detail == nil || detail.ID == "" {
		return agentCardTarget{}, false
	}
	land := projectFromLabels(detail.Labels)
	if land == "" {
		land = strings.TrimSpace(detail.Fields["project"])
	}
	if land == "" {
		land = b.landOf(nil, agent)
	}
	if land == "" {
		return agentCardTarget{}, false
	}
	t := agentCardTarget{subject: detail.ID, agent: detail}
	t.head = strings.TrimSpace(detail.Fields[conversation.BeadThreadID])
	if t.head == "" {
		t.head = strings.TrimSpace(detail.Fields["slack_thread_bead_id"])
	}
	if inThread {
		t.ch = b.commonsChannel(ctx, land, slackChannel)
	} else {
		t.ch = b.machinesChannel(ctx, land, slackChannel)
	}
	return t, true
}

// agentCardFromBeads reads agent's Slack card off its card beads: the newest
// card of the agent in slackChannel, or in any channel when slackChannel is
// empty. It is the bead tier of the card lookups, read before the state file.
func (b *Bot) agentCardFromBeads(ctx context.Context, agent, slackChannel string) (MessageRef, bool) {
	if b.daemon == nil || b.noCardBead(agent) {
		return MessageRef{}, false
	}
	detail := b.findAgentBeadForCard(ctx, agent)
	if detail == nil || detail.ID == "" {
		if slackChannel == "" {
			b.rememberNoCardBead(agent)
		}
		return MessageRef{}, false
	}
	q := beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Fields: map[string]string{conversation.BeadKind: string(conversation.KindCard), conversation.BeadSubjectID: detail.ID},
		Sort:   "-created_at",
		Limit:  10,
	}
	if slackChannel != "" {
		q.Fields[conversation.SurfaceSlack+"_channel"] = slackChannel
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		b.logger.Debug("agent card: card bead lookup failed", "agent", agent, "error", err)
		return MessageRef{}, false
	}
	for _, card := range res.Beads {
		at, ok := conversation.CardRef(conversation.SurfaceSlack, card)
		if !ok {
			continue
		}
		ref := MessageRef{ChannelID: at.Channel, Timestamp: at.TS, Agent: agent}
		if b.state != nil {
			if stateRef, found := b.state.GetAgentCard(agent); found && stateRef.ChannelID == ref.ChannelID && stateRef.Timestamp != ref.Timestamp {
				// Dual state for one release (spec section 10): the bead wins and
				// the disagreement is logged, never fatal.
				b.logger.Warn("agent card: card bead and state file disagree",
					"agent", agent, "bead_ts", ref.Timestamp, "state_ts", stateRef.Timestamp)
			}
		}
		land := projectFromLabels(card.Labels)
		b.rememberAgentCard(agent, agentCardMemo{
			bead: card.ID, subject: detail.ID, at: at,
			ch:   conversation.ChannelRef{ID: card.Fields[conversation.BeadChannelID], Land: land, Slack: at.Channel},
			head: strings.TrimSpace(card.Fields[conversation.BeadThreadID]),
		})
		return ref, true
	}
	if slackChannel == "" {
		// Only "no card in any channel" is remembered: a card in another
		// channel says nothing about this one.
		b.rememberNoCardBead(agent)
	}
	return MessageRef{}, false
}

// agentCardBeadAt returns the card bead that records at, its Slack copy,
// whatever agent bead it is the card of: false with no daemon, no such bead,
// or one with no subject or land.
func (b *Bot) agentCardBeadAt(ctx context.Context, at conversation.Ref) (agentCardMemo, bool) {
	if b.daemon == nil {
		return agentCardMemo{}, false
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"message"},
		Fields: map[string]string{
			conversation.BeadKind:                  string(conversation.KindCard),
			conversation.SurfaceSlack + "_channel": at.Channel,
			conversation.SurfaceSlack + "_ts":      at.TS,
		},
		Limit: 1,
	})
	if err != nil {
		b.logger.Debug("agent card: card bead lookup by Slack copy failed", "channel", at.Channel, "ts", at.TS, "error", err)
		return agentCardMemo{}, false
	}
	if res == nil || len(res.Beads) == 0 {
		return agentCardMemo{}, false
	}
	card := res.Beads[0]
	subject, land := card.Fields[conversation.BeadSubjectID], projectFromLabels(card.Labels)
	if subject == "" || land == "" {
		return agentCardMemo{}, false
	}
	return agentCardMemo{
		bead: card.ID, subject: subject, at: at,
		ch:   conversation.ChannelRef{ID: card.Fields[conversation.BeadChannelID], Land: land, Slack: at.Channel},
		head: strings.TrimSpace(card.Fields[conversation.BeadThreadID]),
	}, true
}

// commonsWishOf is the wish the Commons minted that agent (t.agent) is the
// thread agent of: the head its slack_thread_bead_id names, when that head is
// a wish in a Commons channel with no Slack thread. nil otherwise: a wish with
// a Slack thread gets its thread card as a reply there (ensureAgentCard with
// inThreadTS), and a crew bound to a wish by thread_id alone keeps a top-level
// card (TestThreadBoundAgentCardInMachines), as in Slack.
func (b *Bot) commonsWishOf(ctx context.Context, t agentCardTarget) *beadsapi.BeadDetail {
	if b.daemon == nil || t.agent == nil {
		return nil
	}
	id := strings.TrimSpace(t.agent.Fields["slack_thread_bead_id"])
	if id == "" {
		return nil
	}
	head, err := b.daemon.GetBead(ctx, id)
	if err != nil || head == nil || !isWishHeadBead(head) || strings.TrimSpace(head.Fields[conversation.BeadChannelID]) == "" {
		return nil
	}
	if _, ok := slackThreadRoot(head.Fields); ok {
		return nil
	}
	return head
}

// postCommonsWishCard posts the card of a Commons wish's own agent: its Slack
// copy top-level in channel (the wish has no Slack thread to reply in, and the
// bridge routed the card to #machines), and its card bead as a reply in the
// wish's thread, in the wish's channel, as a Slack wish's thread card is
// (TestAgentCardInMachines). Before this the bead was top-level in #machines
// with no thread_id, so a wish made in the Commons never showed its agent's
// card while one made in Slack did (gd-rHZ1ktOO1E). The Slack post is the
// record ("degrade, never drop"): a failed bead write is logged and the card
// still returns.
func (b *Bot) postCommonsWishCard(ctx context.Context, agent, channel string, t agentCardTarget, head *beadsapi.BeadDetail, m conversation.Message) (MessageRef, error) {
	at, err := b.conversationSlack().Post(ctx, conversation.ChannelRef{Slack: channel}, m)
	if err != nil {
		return MessageRef{}, err
	}
	ch := inCardChannel(t.ch, strings.TrimSpace(head.Fields[conversation.BeadChannelID]))
	th := conversation.Thread{Head: head.ID}
	if id := b.Conversation().RecordCard(ctx, ch, &th, m, at); id != "" {
		b.rememberAgentCard(agent, agentCardMemo{bead: id, subject: t.subject, ch: ch, at: at, head: head.ID})
	} else {
		b.logger.Warn("agent card: card bead of a Commons wish's agent not written; the wish shows no card",
			"agent", agent, "head", head.ID, "channel", channel)
	}
	return MessageRef{ChannelID: at.Channel, Timestamp: at.TS, Agent: agent}, nil
}

// postAgentCard posts agent's card into channel (as a reply in parentTS when
// set) through the Router, which writes the card bead and stamps the Slack ref
// on it. With no target, or when the bead write fails, the card goes to Slack
// directly ("degrade, never drop").
func (b *Bot) postAgentCard(ctx context.Context, agent, channel, parentTS, text string, blocks []slack.Block) (MessageRef, error) {
	ctx = withSlackFace(ctx, blocks)
	m := conversation.Message{Kind: conversation.KindCard, Text: text, Author: agentPrincipal(agent)}
	root := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: parentTS}
	out := func(at conversation.Ref) MessageRef {
		return MessageRef{ChannelID: at.Channel, Timestamp: at.TS, Agent: agent, ThreadTS: parentTS}
	}
	if t, ok := b.agentCardTarget(ctx, agent, channel, parentTS != ""); ok {
		m.SubjectID = t.subject
		if parentTS == "" {
			if head := b.commonsWishOf(ctx, t); head != nil {
				return b.postCommonsWishCard(ctx, agent, channel, t, head, m)
			}
		}
		r := b.Conversation()
		var d conversation.Delivery
		var err error
		if parentTS != "" {
			d, err = r.Reply(ctx, t.ch, conversation.Thread{Head: t.head, Refs: []conversation.Ref{root}}, m, false)
		} else {
			d, err = r.Post(ctx, t.ch, m)
		}
		if err == nil {
			if at, ok := d.Ref(conversation.SurfaceSlack); ok {
				b.rememberAgentCard(agent, agentCardMemo{bead: d.BeadID, subject: t.subject, ch: t.ch, at: at})
				return out(at), nil
			}
			if ferr := d.Failed[conversation.SurfaceSlack]; ferr != nil {
				return MessageRef{}, ferr
			}
			return MessageRef{}, fmt.Errorf("card of %s not projected to slack: %w", agent, conversation.ErrNoChannel)
		}
		b.logger.Warn("agent card: card bead write failed, posting to Slack directly",
			"agent", agent, "channel", channel, "error", err)
	}
	s := b.conversationSlack()
	var at conversation.Ref
	var err error
	if parentTS != "" {
		at, err = s.Reply(ctx, root, m, false)
	} else {
		at, err = s.Post(ctx, conversation.ChannelRef{Slack: channel}, m)
	}
	if err != nil {
		return MessageRef{}, err
	}
	return out(at), nil
}

// editAgentCard re-renders agent's card in place at ref through the Router,
// which makes sure the card bead exists and records ref (a card posted before
// card beads existed gets its bead on its first edit). The error is the Slack
// edit's, so a caller can still see a rate limit (errors.As).
func (b *Bot) editAgentCard(ctx context.Context, agent string, ref MessageRef, text string, blocks []slack.Block) error {
	ctx = withSlackFace(ctx, blocks)
	at := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: ref.ChannelID, TS: ref.Timestamp}
	m := conversation.Message{Kind: conversation.KindCard, Text: text, Author: agentPrincipal(agent)}

	var ch conversation.ChannelRef
	var th *conversation.Thread
	routed := false
	memo, ok := b.agentCardMemo(agent)
	if !ok || memo.at != at {
		// A card bead posted before this bridge started (or before #machines
		// existed) is found by its Slack copy, so the edit lands on it rather
		// than upserting a second card in the channel the target names now.
		if _, found := b.agentCardFromBeads(ctx, agent, ref.ChannelID); found {
			memo, ok = b.agentCardMemo(agent)
		}
		if !ok || memo.at != at {
			// A card bead whose subject is an earlier bead of this agent (a
			// respawn mints a new agent bead) is found by its Slack copy too.
			if found, hit := b.agentCardBeadAt(ctx, at); hit {
				b.rememberAgentCard(agent, found)
				memo, ok = found, true
			}
		}
	}
	if ok && memo.at == at && memo.ch.Land != "" {
		m.BeadID, m.SubjectID, ch, routed = memo.bead, memo.subject, memo.ch, true
	} else if t, ok := b.agentCardTarget(ctx, agent, ref.ChannelID, ref.ThreadTS != ""); ok && !isAgentThreadRoot(t.agent, at) &&
		(ref.ThreadTS == "" || t.head != "") {
		// A card in a Slack thread with no thread head to put its bead under
		// is edited in Slack only: its bead would read as a top-level card in
		// the channel (gd-AflFRvDw7Z).
		m.SubjectID, ch, routed = t.subject, t.ch, true
		if ref.ThreadTS != "" {
			th = &conversation.Thread{Head: t.head}
		}
	}
	if routed {
		d, err := b.Conversation().UpdateCard(ctx, ch, th, m, at)
		if err == nil {
			if m.BeadID == "" && d.BeadID != "" {
				b.rememberAgentCard(agent, agentCardMemo{bead: d.BeadID, subject: m.SubjectID, ch: ch, at: at})
			}
			return d.Failed[conversation.SurfaceSlack]
		}
		b.logger.Warn("agent card: routed edit refused, editing Slack directly",
			"agent", agent, "channel", ref.ChannelID, "error", err)
	}
	return b.conversationSlack().Update(ctx, at, m)
}

// isAgentThreadRoot reports whether at is the message that started the
// agent's Slack thread. The wrap-up falls back to editing it when no card was
// ever recorded (resolveAgentCardRef); it is a person's message, not a card,
// so it never gets a card bead.
func isAgentThreadRoot(agent *beadsapi.BeadDetail, at conversation.Ref) bool {
	return agent != nil && agent.Fields["slack_thread_channel"] == at.Channel && agent.Fields["slack_thread_ts"] == at.TS
}

// isSlackRateLimit returns the Slack rate-limit error wrapped in err.
func isSlackRateLimit(err error) (*slack.RateLimitedError, bool) {
	var rl *slack.RateLimitedError
	if errors.As(err, &rl) {
		return rl, true
	}
	return nil, false
}

// postAgentNotice routes one of agent's lifecycle notices (spawn, schedule,
// terminal state, completion ping) into a Slack channel of land, as a reply in
// threadTS when set, writing it as a message bead first (routeSlackMessage).
// It returns the Slack copy, which the lifecycle reactions target.
func (b *Bot) postAgentNotice(ctx context.Context, agent, land, channel, threadTS string, m conversation.Message) (conversation.Ref, error) {
	if land == "" {
		land = b.landOf(nil, agent)
	}
	if b.daemon == nil {
		land = ""
	}
	return routeSlackMessage(ctx, b.Conversation(), b.logger, land, channel, threadTS, m)
}

// agentNotice is a notice whose Slack face is one mrkdwn section of detail
// under the fallback text, as the spawn and schedule notices have always been.
func agentNotice(fallback, detail string) conversation.Message {
	return conversation.Message{Kind: conversation.KindNotice, Text: fallback, Detail: detail, Rich: true}
}

// deleteSlackMessage deletes a Slack-only message the bridge posted (a thread
// agent's spawn or stream message, which has no bead).
func (b *Bot) deleteSlackMessage(ctx context.Context, ref MessageRef) error {
	return b.conversationSlack().Delete(ctx, conversation.Ref{Surface: conversation.SurfaceSlack, Channel: ref.ChannelID, TS: ref.Timestamp})
}
