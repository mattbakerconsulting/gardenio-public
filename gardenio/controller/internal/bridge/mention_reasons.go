package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/conversation"
)

// noteOnceTTL is how long a one-line reason stays said: within it the same
// reason (by noteOnce key) is not posted again, so a busy thread hears why
// once rather than on every message (gd-ByBchQbx8O).
const noteOnceTTL = time.Hour

// noteOnce reports whether the reason keyed by key may be posted now, and
// marks it posted when it may.
func (b *Bot) noteOnce(key string) bool {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.notedAt == nil {
		b.notedAt = make(map[string]time.Time)
	}
	if last, ok := b.notedAt[key]; ok && now.Sub(last) < noteOnceTTL {
		return false
	}
	for k, t := range b.notedAt {
		if now.Sub(t) >= noteOnceTTL {
			delete(b.notedAt, k)
		}
	}
	b.notedAt[key] = now
	return true
}

// slackNotInChannel reports whether err is Slack refusing a post because the
// bot cannot see the channel: it is not a member, or the channel is private
// to it (channel_not_found).
func slackNotInChannel(err error) bool {
	var resp slack.SlackErrorResponse
	if errors.As(err, &resp) {
		return resp.Err == "not_in_channel" || resp.Err == "channel_not_found"
	}
	return err != nil && (strings.Contains(err.Error(), "not_in_channel") || strings.Contains(err.Error(), "channel_not_found"))
}

// noteCardRefused tells the Slack thread agent was started from that its card
// could not go to channel because the bot is not a member there. The thread is
// the agent bead's own Slack thread, else the one its thread head records.
// Nothing is posted when the thread is in channel itself (the bot cannot post
// there either), when no thread is known, or when the same thread was already
// told about channel within noteOnceTTL.
func (b *Bot) noteCardRefused(ctx context.Context, agent, channel string, err error) {
	if b.api == nil || b.daemon == nil || !slackNotInChannel(err) {
		return
	}
	detail := b.findAgentBeadForCard(ctx, agent)
	if detail == nil {
		return
	}
	origin, ts := detail.Fields["slack_thread_channel"], detail.Fields["slack_thread_ts"]
	if origin == "" || ts == "" {
		head := strings.TrimSpace(detail.Fields[conversation.BeadThreadID])
		if head == "" {
			head = strings.TrimSpace(detail.Fields["slack_thread_bead_id"])
		}
		if head == "" {
			return
		}
		hb, herr := b.daemon.GetBead(ctx, head)
		if herr != nil || hb == nil {
			return
		}
		origin, ts = hb.Fields["slack_thread_channel"], hb.Fields["slack_thread_ts"]
	}
	if origin == "" || ts == "" || origin == channel {
		return
	}
	if !b.noteOnce("card-refused:" + origin + ":" + ts + ":" + channel) {
		return
	}
	b.logger.Info("agent card refused: bot not in channel, telling the origin thread",
		"agent", agent, "channel", channel, "thread_channel", origin, "thread_ts", ts)
	_, _, _ = b.api.PostMessage(origin,
		slack.MsgOptionText(fmt.Sprintf("%s I couldn't post *%s*'s card in <#%s>: I'm not a member of that channel. Invite me there with `/invite @gardenio`.",
			b.themeFor(ctx, origin).Semantic(SemWarn), extractAgentName(agent), channel), false),
		slack.MsgOptionTS(ts))
}
