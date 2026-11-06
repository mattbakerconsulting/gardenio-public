package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// Decision and permission cards through the conversation Router (design spec
// 2026-10-19-lands-commons-design.md, section 5.2, slice 3). A decision card
// and a permission prompt are posted through the Router, which writes the card
// bead (kind=card, subject_id = the decision or permission bead) in the
// subject's land with the Slack ref on it. A decision delivered DM-first has
// two cards: the DM copy, recorded on its own card bead, and the channel
// pointer, which is the Slack face of the channel's card. The Commons renders
// every card bead from its subject (internal/commons), so a resolve, dismiss
// or verdict re-renders there when the subject changes.
//
// The Slack faces do not change: the handlers build the Block Kit as they
// always did and the Slack surface sends it unchanged
// (TestDecisionCardCallsGolden). The edits after a post (resolved, superseded,
// dismissed, verdict) still go to the Slack copy the Bot tracks.

// postSubjectCard posts the card of subject, a bead of land, into slackChannel
// (as a reply in threadTS when set) through the Router, with blocks as its
// Slack face. With no land or no subject, or when the bead write fails, the
// card goes to Slack directly ("degrade, never drop").
func (b *Bot) postSubjectCard(ctx context.Context, land, subject string, author conversation.Principal, slackChannel, threadTS, text string, blocks []slack.Block) (MessageRef, error) {
	ref, _, err := b.postSubjectCardBead(ctx, land, subject, author, slackChannel, threadTS, text, blocks, false)
	return ref, err
}

// postSubjectCardBead is postSubjectCard that also returns the card bead the
// Router wrote, "" when the card went to Slack directly. The bead is returned
// when the Slack post failed too, so a caller that retries the post itself
// can stamp the copy it gets on that bead.
//
// machine says no person is asked to answer the card (an escalation the desk
// or the operator has): at the top level its Commons copy goes to the land's
// #machines when it has one (single voice, steward amendment 2026-10-23 §2).
// A card a person answers stays in the channel bound to slackChannel.
func (b *Bot) postSubjectCardBead(ctx context.Context, land, subject string, author conversation.Principal, slackChannel, threadTS, text string, blocks []slack.Block, machine bool) (MessageRef, string, error) {
	ctx = withSlackFace(ctx, blocks)
	m := conversation.Message{Kind: conversation.KindCard, SubjectID: subject, Text: text, Author: author}
	root := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: slackChannel, TS: threadTS}
	out := func(at conversation.Ref) MessageRef {
		return MessageRef{ChannelID: at.Channel, Timestamp: at.TS, ThreadTS: threadTS}
	}
	if b.daemon != nil && land != "" && subject != "" && slackChannel != "" {
		head := b.threadHeadFor(ctx, land, slackChannel, threadTS)
		ch := b.cardChannel(ctx, land, slackChannel, threadTS, head, machine)
		r := b.Conversation()
		var d conversation.Delivery
		var err error
		if threadTS != "" {
			th := conversation.Thread{Head: headID(head), Refs: []conversation.Ref{root}}
			d, err = r.Reply(ctx, ch, th, m, false)
		} else {
			d, err = r.Post(ctx, ch, m)
		}
		if err == nil {
			if at, ok := d.Ref(conversation.SurfaceSlack); ok {
				return out(at), d.BeadID, nil
			}
			if ferr := d.Failed[conversation.SurfaceSlack]; ferr != nil {
				return MessageRef{}, d.BeadID, ferr
			}
			return MessageRef{}, d.BeadID, fmt.Errorf("card of %s not projected to slack: %w", subject, conversation.ErrNoChannel)
		}
		b.logger.Warn("card: card bead write failed, posting to Slack directly",
			"subject", subject, "channel", slackChannel, "error", err)
	}
	// The direct post is exactly the call the handlers made before slice 3.
	var opts []slack.MsgOption
	if text != "" {
		opts = append(opts, slack.MsgOptionText(text, false))
	}
	opts = append(opts, slack.MsgOptionBlocks(blocks...))
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	gotChannel, ts, err := b.api.PostMessageContext(ctx, slackChannel, opts...)
	if err != nil {
		return MessageRef{}, "", err
	}
	if gotChannel == "" {
		gotChannel = slackChannel
	}
	return out(conversation.Ref{Surface: conversation.SurfaceSlack, Channel: gotChannel, TS: ts}), "", nil
}

// cardChannel is where a subject card's Commons copy goes. A card a person
// answers is in the channel bound to slackChannel (commonsChannel). A machine
// card is in #machines (machinesChannel) at the top level, and in a thread it
// is in the channel of head, the thread's head bead, as a reply is: escalation
// cards always post into the requesting agent's Slack thread, so without this
// every desk card landed in #general (gd-rar4hYRbGC). With no head, or one
// that names no channel, a machine card in a thread is in #machines too.
func (b *Bot) cardChannel(ctx context.Context, land, slackChannel, threadTS string, head *beadsapi.BeadDetail, machine bool) conversation.ChannelRef {
	if !machine {
		return b.commonsChannel(ctx, land, slackChannel)
	}
	ch := b.machinesChannel(ctx, land, slackChannel)
	if threadTS != "" && head != nil {
		ch = inCardChannel(ch, strings.TrimSpace(head.Fields[conversation.BeadChannelID]))
	}
	return ch
}

// recordDecisionDMCard records at, the DM-first copy of a decision's card the
// Bot posted into the addressee's Slack IM, on its own card bead in land: in
// the Commons DM channel between the requesting agent and the addressee when
// both are known (recordDMCard), else keyed by the Slack IM alone.
func (b *Bot) recordDecisionDMCard(ctx context.Context, land, subject string, author conversation.Principal, addresseeSlackID string, at MessageRef, text string) {
	p, _ := b.personBySlackID(ctx, addresseeSlackID)
	b.recordDMCard(ctx, land, subject, author, p, at, text)
}

// cardThreadHead returns the bead at the root of the Slack thread threadTS in
// slackChannel: the card or notice the Router wrote there (an agent card), or
// the wish head minted for the thread. nil when there is none; the card then
// has no thread in the Commons and shows at the top of its channel.
func (b *Bot) cardThreadHead(ctx context.Context, land, slackChannel, threadTS string) *beadsapi.BeadDetail {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Labels: []string{"project:" + land},
		Fields: map[string]string{conversation.SurfaceSlack + "_channel": slackChannel, conversation.SurfaceSlack + "_ts": threadTS},
		Limit:  1,
	})
	if err == nil && len(res.Beads) > 0 && res.Beads[0] != nil {
		return res.Beads[0]
	}
	if head := b.slackThreadHead(ctx, slackChannel, threadTS); head != nil && projectFromLabels(head.Labels) == land {
		return head
	}
	return nil
}

// headID is the id of a thread head bead, "" for none.
func headID(head *beadsapi.BeadDetail) string {
	if head == nil {
		return ""
	}
	return head.ID
}

// decisionLand is the land of a decision: its project label, else its
// requesting agent's.
func (b *Bot) decisionLand(bead BeadEvent, agent string) string {
	if land := projectFromLabels(bead.Labels); land != "" {
		return land
	}
	return b.landOf(nil, agent)
}

// deskRole is the role of the merge desk's agents: kbeads' default escalation
// policy names the merge-desk coordinator by the principal role:reviewer
// (escalation_policy.go defaultEscalationPolicy).
const deskRole = "reviewer"

// decisionFromDesk reports whether bead, a decision asked by agent, comes
// from the merge desk: its requesting agent has deskRole. Such a decision (a
// "Decision needed: PR #NNNN" ruling) is desk traffic, so its card is a
// machine card and its Commons copy goes to #machines, as a desk escalation's
// does (gd-GgRk2fLZKQ). The role is read from the requesting agent's bead,
// else the bridge's cache of agent roles; the question text is never read.
func (b *Bot) decisionFromDesk(ctx context.Context, bead BeadEvent, agent string) bool {
	if id := bead.Fields["requesting_agent_bead_id"]; id != "" && b.daemon != nil {
		if ab, err := b.daemon.GetBead(ctx, id); err == nil && ab != nil {
			if role := strings.TrimSpace(ab.Fields["role"]); role != "" {
				return role == deskRole
			}
		}
	}
	if agent = extractAgentName(agent); agent == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.agentRole[agent] == deskRole
}

// writePermissionBead records cb as a type=permission bead in the asking
// agent's land, the subject of its card. It returns the bead and its land, or
// "" when the agent has no land or the write fails; the prompt is posted
// either way. The write is the bridge's machine principal's: type:permission
// is admin-gated, granted to gardenio-machine-slack-dm-delivery only
// (gd-RuI4Ze6vAg), so without one the write is refused and the prompt has no
// Commons card.
func (b *Bot) writePermissionBead(ctx context.Context, cb channelPermissionCallback) (id, land string) {
	if b.daemon == nil || cb.Agent == "" {
		return "", ""
	}
	fields := map[string]string{
		"agent": cb.Agent, "request_id": cb.RequestID, "tool_name": cb.ToolName,
		"description": cb.Description, "input_preview": truncateRunes(cb.InputPreview, 2000),
	}
	// An agent's own token named its bead and land (askerOf); otherwise the
	// agent is looked up by name.
	fields["agent_bead_id"], land = cb.agentBeadID, cb.land
	if fields["agent_bead_id"] == "" || land == "" {
		beadID, byName := b.agentBeadAndLand(ctx, cb.Agent)
		if fields["agent_bead_id"] == "" {
			fields["agent_bead_id"] = beadID
		}
		if land == "" {
			land = byName
		}
	}
	if land == "" {
		return "", ""
	}
	for k, v := range fields {
		if v == "" {
			delete(fields, k)
		}
	}
	raw, _ := json.Marshal(fields) // a map[string]string always marshals
	id, err := b.daemon.CreateBead(beadsapi.WithMachinePrincipal(ctx, b.machine), beadsapi.CreateBeadRequest{
		Title:     truncateRunes(fmt.Sprintf("Permission: %s for %s", cb.ToolName, cb.Agent), 200),
		Type:      "permission",
		Kind:      "data",
		Labels:    []string{"project:" + land},
		CreatedBy: agentPrincipal(cb.Agent).ID,
		Fields:    raw,
	})
	if err != nil {
		b.logger.Warn("permission: request bead write failed", "agent", cb.Agent, "request_id", cb.RequestID, "error", err)
		return "", ""
	}
	return id, land
}

// closePermissionBead closes the permission bead id with the verdict, so its
// Commons card shows the answer: who answered is the person's principal and
// the Slack user id they clicked as, never the mutable Slack username. The
// close, which records the verdict, is the machine principal's
// (writePermissionBead).
func (b *Bot) closePermissionBead(ctx context.Context, id, behavior string, who permissionAnswerer, verdictErr error) {
	if b.daemon == nil || id == "" {
		return
	}
	fields := map[string]string{"behavior": behavior, "answered_by": who.Principal, "answered_by_slack_id": who.SlackID}
	if verdictErr != nil {
		fields["verdict_error"] = truncateRunes(verdictErr.Error(), 500)
	}
	if err := b.daemon.CloseBead(beadsapi.WithMachinePrincipal(ctx, b.machine), id, fields); err != nil {
		b.logger.Warn("permission: request bead close failed", "bead", id, "error", err)
	}
}
