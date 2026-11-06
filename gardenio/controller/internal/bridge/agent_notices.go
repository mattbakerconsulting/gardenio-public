package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gardenio/controller/internal/conversation"
)

// Death and crash in the Commons (design spec 2026-10-19-lands-commons-design.md,
// section 7.2 step 10). The death report and crash notice were already notice
// message beads (slice 1); here they also carry the channel bead of the Slack
// channel they post to, the thread they reply in and their subject (the agent
// bead), so the Commons shows them where Slack does, and the fields the
// Commons renders their faces from. The Slack copies do not change
// (TestLeafOutboundCallsGolden).

// Notice bead fields the Commons renders a death, crash or storm roll-up from
// (internal/commons/notices.go reads the same names).
const (
	noticeField         = "notice"           // death | crash | death_storm
	noticeAgentField    = "notice_agent"     // the agent's display name
	noticeCauseField    = "notice_cause"     // death: the one-line cause
	noticePodField      = "notice_pod"       // crash: the pod
	noticePodPhaseField = "notice_pod_phase" // crash: the pod's phase
	noticeCountField    = "notice_count"     // death_storm: deaths held back in the land
	noticeWindowField   = "notice_window"    // death_storm: the storm window
	noticeDeadField     = "notice_dead"      // death_storm: JSON [{"id","name"}] of the held-back deaths
	// death, crash: the agent bead the notice is about. Never subject_id: the
	// daemon refuses subject_id on anything but a kind=card message, so a
	// notice carrying it was never written and went to Slack alone
	// (gd-JGmXQuSndB).
	noticeSubjectField = "notice_subject"

	noticeDeath      = "death"
	noticeCrash      = "crash"
	noticeDeathStorm = "death_storm"
)

// deathStormHeldMax bounds how many held-back deaths the gate remembers by
// name for the Commons roll-ups. Past it a land's roll-up undercounts; the
// fleet-wide Slack roll-up's count stays exact.
const deathStormHeldMax = 200

// heldDeath is a death the storm gate held back: its land, the dead agent
// bead and its name.
type heldDeath struct {
	land, subject, name string
}

// agentNoticeTarget places a notice about agent in the conversation model: the
// channel bead of land bound to slackChannel (none when the land has no
// Commons), the head of the thread threadTS names there, and the agent bead it
// is about. The head is the agent's card bead when the notice threads under the
// card, else the thread head the agent is bound to when it threads in that
// thread; "" when the head is not known, and the Commons shows the notice at
// the top of the channel.
//
// Single voice (steward amendment 2026-10-23 §2): a top-level notice is a
// machine post and goes to the land's #machines when it has one; a notice
// under the agent's card is in the card's channel, wherever the card is.
func (b *Bot) agentNoticeTarget(ctx context.Context, agent, land, slackChannel, threadTS string) (ch conversation.ChannelRef, head, subject string) {
	if threadTS == "" {
		ch = b.machinesChannel(ctx, land, slackChannel)
	} else {
		ch = b.commonsChannel(ctx, land, slackChannel)
	}
	if b.daemon == nil || land == "" {
		return ch, "", ""
	}
	detail := b.findAgentBeadForCard(ctx, agent)
	if detail != nil {
		subject = detail.ID
	}
	if threadTS == "" {
		return ch, "", subject
	}
	if memo, ok := b.agentCardMemo(agent); ok && memo.at.Channel == slackChannel && memo.at.TS == threadTS {
		return inCardChannel(ch, memo.ch.ID), cardThread(memo.bead, memo.head), subject
	}
	if detail == nil {
		return ch, "", subject
	}
	if detail.Fields["slack_thread_channel"] == slackChannel && detail.Fields["slack_thread_ts"] == threadTS {
		head = strings.TrimSpace(detail.Fields[conversation.BeadThreadID])
		if head == "" {
			head = strings.TrimSpace(detail.Fields["slack_thread_bead_id"])
		}
		return ch, head, subject
	}
	// The card is in #machines (posted since single voice) or in the channel
	// bound to slackChannel (posted before it).
	chans := []conversation.ChannelRef{ch}
	if mc := b.machinesChannel(ctx, land, slackChannel); mc.ID != "" && mc.ID != ch.ID {
		chans = []conversation.ChannelRef{mc, ch}
	}
	for _, c := range chans {
		card, err := b.Conversation().FindCard(ctx, c, subject)
		if err != nil {
			b.logger.Debug("agent notice: card bead lookup failed", "agent", agent, "error", err)
			continue
		}
		if at, ok := conversation.CardRef(conversation.SurfaceSlack, card); ok && at.Channel == slackChannel && at.TS == threadTS {
			return inCardChannel(c, strings.TrimSpace(card.Fields[conversation.BeadChannelID])),
				cardThread(card.ID, strings.TrimSpace(card.Fields[conversation.BeadThreadID])), subject
		}
	}
	return ch, "", subject
}

// cardThread is the thread a reply under an agent card is in: the thread the
// card is itself a reply in when it has one (a Commons wish's agent card sits
// in the wish's thread, postCommonsWishCard, and the Commons has no thread
// under a reply), else the card's own.
func cardThread(card, head string) string {
	if head != "" {
		return head
	}
	return card
}

// inCardChannel is ch moved to the channel bead cardChannel, the channel of the
// card a notice replies under, keeping ch's Slack copy. ch is unchanged when
// the card names no channel bead.
func inCardChannel(ch conversation.ChannelRef, cardChannel string) conversation.ChannelRef {
	if cardChannel != "" {
		ch.ID = cardChannel
	}
	return ch
}

// routeAgentNotice routes an agent's death or crash notice into ch, as a reply
// under head in the Slack thread threadTS when set.
func (b *Bot) routeAgentNotice(ctx context.Context, ch conversation.ChannelRef, head, threadTS string, m conversation.Message) error {
	if b.daemon == nil {
		ch = conversation.ChannelRef{Slack: ch.Slack}
	}
	_, err := routeSlackIn(ctx, b.Conversation(), b.logger, ch, head, threadTS, m)
	return err
}

// rollUpDeathsInCommons writes one death-storm roll-up per land into that
// land's #machines, else its #crew (placement rule 3), naming the land's held-back deaths and
// nothing from another land. It is Commons-only: Slack has had the
// fleet-wide roll-up. A land with no Commons gets none.
func (b *Bot) rollUpDeathsInCommons(ctx context.Context, held []heldDeath) {
	if b.daemon == nil || len(held) == 0 {
		return
	}
	type dead struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	byLand := map[string][]dead{}
	var lands []string
	for _, d := range held {
		if d.land == "" {
			continue
		}
		if _, ok := byLand[d.land]; !ok {
			lands = append(lands, d.land)
		}
		byLand[d.land] = append(byLand[d.land], dead{ID: d.subject, Name: d.name})
	}
	r := b.Conversation()
	for _, land := range lands {
		p, err := conversation.Place(ctx, b.daemon, conversation.Subject{Type: "agent", Land: land})
		if err != nil || p.Channel.ID == "" {
			b.logger.Debug("death storm: no Commons channel for the roll-up", "land", land, "error", err)
			continue
		}
		ch := p.Channel
		ch.Slack = ""
		list, _ := json.Marshal(byLand[land]) // a slice of string pairs always marshals
		n := len(byLand[land])
		m := conversation.Message{
			Kind:   conversation.KindNotice,
			Text:   fmt.Sprintf("Death storm: %d more agents died in %s (reports held back)", n, land),
			Author: agentPrincipal(deathReportAuthor),
			Fields: map[string]string{noticeField: noticeDeathStorm, noticeCountField: strconv.Itoa(n),
				noticeWindowField: deathStormWindow.String(), noticeDeadField: string(list)},
		}
		if _, err := r.Post(ctx, ch, m); err != nil {
			b.logger.Warn("death storm: Commons roll-up failed", "land", land, "error", err)
		}
	}
}
