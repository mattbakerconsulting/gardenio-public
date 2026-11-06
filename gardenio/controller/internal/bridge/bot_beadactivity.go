package bridge

import (
	"context"
	"fmt"
	"time"

	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// shouldPostBeadActivity reports whether a per-bead lifecycle message
// (created/claimed/closed) should be posted to the given agent's Slack thread.
//
// Two gates, both must pass (plan §5, operator scope-add "beads comments and
// updates such should be suppressed as well"):
//  1. The GLOBAL notifConfig.SuppressBeadActivity kill switch is off (an
//     operator can still mute this fleet-wide regardless of per-user prefs).
//  2. The owning operator has opted into vibe mode. In the simple default this
//     is false, so bead chatter is suppressed per-recipient — the streaming
//     status + reactions already convey activity.
//
// Note the callers still always push the task_update stream chunk; only the
// Slack thread message is gated here.
func (b *Bot) shouldPostBeadActivity(ctx context.Context, agent string) bool {
	if b.notifConfig.SuppressBeadActivity {
		return false
	}
	return b.vibeModeForAgent(ctx, agent)
}

// NotifyBeadCreated posts a notification in the creating agent's Slack thread.
func (b *Bot) NotifyBeadCreated(ctx context.Context, bead BeadEvent) {
	agent := extractAgentName(bead.CreatedBy)
	if agent == "" {
		return
	}

	b.recordActivity(agent)

	if b.shouldPostBeadActivity(ctx, agent) {
		title := truncateText(bead.Title, 200)
		link := b.beadTitleLinkArms(bead.ID, title, bead.Heraldry)
		text := fmt.Sprintf(":pencil2: Created %s: *%s*", bead.Type, link)
		b.postOrUpdateBeadMessage(ctx, agent, bead.ID, text)
	}
}

// NotifyBeadClaimed posts a notification in the claiming agent's Slack thread
// and sends a task_update(in_progress) chunk to the agent's stream.
func (b *Bot) NotifyBeadClaimed(ctx context.Context, bead BeadEvent) {
	agent := extractAgentName(bead.Assignee)
	if agent == "" {
		return
	}

	b.recordActivity(agent)

	if b.shouldPostBeadActivity(ctx, agent) {
		title := truncateText(bead.Title, 200)
		link := b.beadTitleLinkArms(bead.ID, title, bead.Heraldry)
		text := fmt.Sprintf(":arrow_right: Claimed %s: *%s*", bead.Type, link)
		b.postOrUpdateBeadMessage(ctx, agent, bead.ID, text)
	}

	// Push task_update chunk to the streaming session (always — not suppressed).
	if b.streamMgr != nil {
		b.streamMgr.HandleBeadClaimed(ctx, agent, bead.ID, bead.Title)
	}
}

// NotifyBeadCreatedAndClaimed posts a single combined notification when an
// agent creates a bead and immediately claims it, reducing thread noise.
// Also sends a task_update(in_progress) chunk to the agent's stream.
func (b *Bot) NotifyBeadCreatedAndClaimed(ctx context.Context, bead BeadEvent) {
	agent := extractAgentName(bead.Assignee)
	if agent == "" {
		agent = extractAgentName(bead.CreatedBy)
	}
	if agent == "" {
		return
	}

	b.recordActivity(agent)

	if b.shouldPostBeadActivity(ctx, agent) {
		title := truncateText(bead.Title, 60)
		link := b.beadTitleLinkArms(bead.ID, title, bead.Heraldry)
		text := fmt.Sprintf(":arrow_right: Created & claimed %s: *%s*", bead.Type, link)
		b.postOrUpdateBeadMessage(ctx, agent, bead.ID, text)
	}

	// Push task_update chunk to the streaming session (always — not suppressed).
	if b.streamMgr != nil {
		b.streamMgr.HandleBeadClaimed(ctx, agent, bead.ID, bead.Title)
	}
}

// NotifyBeadClosed posts a notification in the assigned agent's Slack thread
// and sends a task_update(complete) chunk to the agent's stream.
func (b *Bot) NotifyBeadClosed(ctx context.Context, bead BeadEvent) {
	agent := extractAgentName(bead.Assignee)
	if agent == "" {
		return
	}

	b.recordActivity(agent)

	if b.shouldPostBeadActivity(ctx, agent) {
		title := truncateText(bead.Title, 200)
		link := b.beadTitleLinkArms(bead.ID, title, bead.Heraldry)
		text := fmt.Sprintf(":white_check_mark: Closed %s: *%s*", bead.Type, link)
		b.postOrUpdateBeadMessage(ctx, agent, bead.ID, text)
	}

	// Push task_update chunk to the streaming session (always — not suppressed).
	if b.streamMgr != nil {
		b.streamMgr.HandleBeadClosed(ctx, agent, bead.ID, bead.Title)
	}

	// Clean up tracking — closed is a terminal state.
	b.mu.Lock()
	delete(b.beadMsgs, beadMsgKey(agent, bead.ID))
	b.mu.Unlock()
}

// beadMsgKey returns the map key for tracking a bead's Slack message per agent.
func beadMsgKey(agent, beadID string) string {
	return agent + ":" + beadID
}

// postOrUpdateBeadMessage posts a bead status message or updates an existing
// one in-place. If a previous message exists for this agent+bead, it is updated
// rather than posting a new reply, reducing thread noise.
func (b *Bot) postOrUpdateBeadMessage(ctx context.Context, agent, beadID, text string) {
	key := beadMsgKey(agent, beadID)

	// Check for an existing message to update in-place.
	b.mu.Lock()
	ref, hasRef := b.beadMsgs[key]
	b.mu.Unlock()

	if hasRef {
		_, _, _, err := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
			slack.MsgOptionText(text, false),
		)
		if err != nil {
			b.logger.Warn("bead-activity: failed to update bead message in-place",
				"agent", agent, "bead", beadID, "error", err)
			// Fall through to post a new message.
		} else {
			// Slack clears the assistant thread status on chat.update; restore
			// it for thread-bound agents with an active streaming session.
			// No-op when no session matches or ThreadTS is unknown.
			if ref.ThreadTS != "" {
				b.restoreThreadStatus(ctx, ref.ChannelID, ref.ThreadTS)
			}
			return
		}
	}

	// Post a new message and track it.
	b.postBeadThreadMessage(ctx, agent, beadID, text)
}

// postBeadThreadMessage posts a new bead status message and stores the ref
// for future in-place updates.
func (b *Bot) postBeadThreadMessage(ctx context.Context, agent, beadID, text string) {
	// Check for thread-bound agent first.
	if slackChannel, slackTS := b.resolveAgentThread(ctx, agent); slackChannel != "" && slackTS != "" {
		_, msgTS, err := b.api.PostMessageContext(ctx, slackChannel,
			slack.MsgOptionText(text, false),
			slack.MsgOptionTS(slackTS),
		)
		if err != nil {
			b.logger.Warn("bead-activity: failed to post to thread-bound agent",
				"agent", agent, "channel", slackChannel, "thread_ts", slackTS, "error", err)
			return
		}
		if msgTS != "" {
			b.mu.Lock()
			b.beadMsgs[beadMsgKey(agent, beadID)] = MessageRef{
				ChannelID: slackChannel, Timestamp: msgTS, ThreadTS: slackTS,
			}
			b.mu.Unlock()
		}
		// PostMessage clears the assistant thread status — restore it for
		// thread-bound agents with an active streaming session. No-op otherwise.
		b.restoreThreadStatus(ctx, slackChannel, slackTS)
		return
	}

	// Fall back to agent card thread (regular threading mode).
	if !b.agentThreadingEnabled() {
		b.logger.Debug("bead-activity: agent threading disabled, dropping notification",
			"agent", agent)
		return
	}

	threadTS := b.getAgentThreadTS(agent)
	if threadTS == "" {
		b.logger.Debug("bead-activity: no agent card found, dropping notification",
			"agent", agent)
		return
	}

	channel := b.resolveChannel(agent)
	if channel == "" {
		b.logger.Debug("bead-activity: no channel resolved, dropping notification",
			"agent", agent)
		return
	}
	_, msgTS, err := b.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(text, false),
		slack.MsgOptionTS(threadTS),
	)
	if err != nil {
		b.logger.Warn("bead-activity: failed to post to agent card thread",
			"agent", agent, "channel", channel, "thread_ts", threadTS, "error", err)
		return
	}
	if msgTS != "" {
		b.mu.Lock()
		b.beadMsgs[beadMsgKey(agent, beadID)] = MessageRef{
			ChannelID: channel, Timestamp: msgTS, ThreadTS: threadTS,
		}
		b.mu.Unlock()
	}
}

// routeAgentThreadMessage posts m in the agent's thread through the
// conversation Router — the bound Slack thread for a thread-spawned agent,
// else its agent card thread — as a message bead in land.
func (b *Bot) routeAgentThreadMessage(ctx context.Context, agent, land string, m conversation.Message) {
	// Check for thread-bound agent first.
	if slackChannel, slackTS := b.resolveAgentThread(ctx, agent); slackChannel != "" && slackTS != "" {
		if err := b.routeToSlack(ctx, land, slackChannel, slackTS, m); err != nil {
			b.logger.Warn("bead-activity: failed to post to thread-bound agent",
				"agent", agent, "channel", slackChannel, "thread_ts", slackTS, "error", err)
		}
		// Restore thread status — PostMessage clears the assistant status bar.
		b.restoreThreadStatus(ctx, slackChannel, slackTS)
		return
	}

	// Fall back to agent card thread (regular threading mode).
	if !b.agentThreadingEnabled() {
		b.logger.Debug("bead-activity: agent threading disabled, dropping notification",
			"agent", agent)
		return
	}

	threadTS := b.getAgentThreadTS(agent)
	if threadTS == "" {
		b.logger.Debug("bead-activity: no agent card found, dropping notification",
			"agent", agent)
		return
	}

	channel := b.resolveChannel(agent)
	if channel == "" {
		b.logger.Debug("bead-activity: no channel resolved, dropping notification",
			"agent", agent)
		return
	}
	// Under the card the message is a reply to the card bead, in the card's
	// channel (#machines since single voice), as a death or crash notice is
	// (agentNoticeTarget). With no card bead found it stays out of every
	// Commons channel, as before: a channel_id with no thread would put it at
	// the top of the channel (gd-rar4hYRbGC).
	ch := conversation.ChannelRef{Land: land, Slack: channel}
	var head string
	if b.daemon == nil {
		ch.Land = ""
	} else if land != "" {
		if card, h, _ := b.agentNoticeTarget(ctx, agent, land, channel, threadTS); h != "" {
			ch, head = card, h
		}
	}
	if _, err := routeSlackIn(ctx, b.Conversation(), b.logger, ch, head, threadTS, m); err != nil {
		b.logger.Warn("bead-activity: failed to post to agent card thread",
			"agent", agent, "channel", channel, "thread_ts", threadTS, "error", err)
	}
}

// tryUpdateSpawnMessage attempts to update the spawn confirmation message
// in-place with the given text. Returns true if a spawn message existed and
// was successfully updated (consuming the ref). Returns false if no spawn
// message is tracked or has no persistent message (status-only spawn),
// allowing the caller to fall back to posting a new reply.
func (b *Bot) tryUpdateSpawnMessage(ctx context.Context, agent, text string) bool {
	agent = extractAgentName(agent)

	b.mu.Lock()
	spawnRef, hasSpawn := b.threadSpawnMsgs[agent]
	if hasSpawn {
		delete(b.threadSpawnMsgs, agent)
	}
	b.mu.Unlock()

	if !hasSpawn || spawnRef.Timestamp == "" {
		return false
	}

	// The spawn message is a Slack-only status line, not a message bead, so
	// it is edited on the Slack surface directly rather than through the
	// router.
	err := b.conversationSlack().Update(ctx, conversation.Ref{
		Surface: conversation.SurfaceSlack, Channel: spawnRef.ChannelID, TS: spawnRef.Timestamp,
	}, conversation.Message{Text: text})
	if err != nil {
		b.logger.Warn("failed to update spawn message with squawk, posting new reply",
			"agent", agent, "error", err)
		return false
	}
	// Restore thread status — message updates may clear the assistant status bar.
	b.restoreThreadStatus(ctx, spawnRef.ChannelID, spawnRef.ThreadTS)
	return true
}

// getAgentThreadTS returns the thread timestamp for an agent's card,
// or "" if no card exists. Does not create a card.
func (b *Bot) getAgentThreadTS(agent string) string {
	b.mu.Lock()
	ref, ok := b.agentCards[agent]
	b.mu.Unlock()
	if ok {
		return ref.Timestamp
	}
	return ""
}

// recordActivity updates the agent's last-seen timestamp so the agent card
// reflects recent activity.
func (b *Bot) recordActivity(agent string) {
	b.mu.Lock()
	b.agentSeen[agent] = time.Now()
	b.mu.Unlock()
}
