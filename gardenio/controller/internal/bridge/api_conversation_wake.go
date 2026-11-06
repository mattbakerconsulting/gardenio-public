package bridge

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// wakeThreadAgent gives a say's words to the agent of the thread they were
// said in (gb say --thread <head> --wake), as @gardenio in that thread would
// (toThreadAgent): a nudge when it runs, a respawn with the thread's
// conversation when it has stopped, a fresh agent when the thread has none.
//
// The herder wakes the agent of a wish it judged unfulfilled this way
// (gd-y8LrXMOa46). An agent's say is never a mention: Slack does not deliver
// the bot's own posts, and the Commons composes only what a person wrote, so
// "@gardenio" in a say reaches no one. The woken agent is the asker's, not the
// speaker's: it runs as the head's operated_by and Slack opener. The speaker
// does not wake itself. It returns what was done, for gb to print.
func (a *ConversationAgentAPI) wakeThreadAgent(ctx context.Context, t sayTarget, speaker, text, msgID string) (string, error) {
	head, err := a.bot.daemon.GetBead(ctx, t.thread.Head)
	if err != nil {
		return "", fmt.Errorf("reading thread %s: %w", t.thread.Head, err)
	}
	if head == nil {
		return "", sayErrorf(http.StatusNotFound, "thread %s not found", t.thread.Head)
	}
	at := threadAt{
		Head:      head.ID,
		Land:      t.ch.Land,
		ChannelID: firstNonEmptyField(head.Fields, "channel_id"),
	}
	if at.ChannelID == "" {
		at.ChannelID = t.ch.ID
	}
	if root, ok := t.slackRoot(); ok {
		at.Channel, at.TS = root.Channel, root.TS
	}
	return a.bot.wakeWishAgent(ctx, at, head, speaker, "Wake from "+speaker, text, msgID), nil
}

// wakeWishAgent gives text to the agent of wish head's thread at (Head, Land,
// ChannelID and the Slack copy set), as the asker: the head's operated_by and
// Slack opener. speaker, when it is the thread's own agent, does not wake
// itself. It returns what was done ("Told x.", "Waking x with this thread's
// conversation.").
func (b *Bot) wakeWishAgent(ctx context.Context, at threadAt, head *beadsapi.BeadDetail, speaker, label, text, msgID string) string {
	s := &answerSay{logger: b.logger}
	at.Say = s.say
	if op := strings.TrimSpace(head.Fields["operated_by"]); strings.HasPrefix(op, "oidc:") {
		at.Actor = op
	}
	userID := firstNonEmptyField(head.Fields, "slack_opener_user_id", "slack_user_id")
	if userID == "" {
		userID = b.slackUserOf(ctx, at.Actor)
	}

	agent := b.headAgent(ctx, at)
	if agent != "" && speaker != "" && extractAgentName(agent) == speaker {
		return fmt.Sprintf("%s is this thread's agent; it does not wake itself.", speaker)
	}
	b.toThreadAgent(ctx, at, agent, label, text, msgID, userID, s)
	var said []string
	for _, m := range s.answer().Ephemeral {
		said = append(said, m.Text)
	}
	return strings.Join(said, " ")
}

// refuseWake refuses a wake before anything is posted: it is said in a thread
// named by its head (or a message in it), whose land holds its agent.
func refuseWake(req sayRequest, t sayTarget) error {
	if strings.TrimSpace(req.Thread) == "" || t.thread == nil || t.thread.Head == "" {
		return sayErrorf(http.StatusBadRequest, "wake needs thread: the head of the thread whose agent it wakes")
	}
	if t.ch.Land == "" {
		return sayErrorf(http.StatusUnprocessableEntity, "thread %s has no land, so it has no agent to wake", t.thread.Head)
	}
	return nil
}
