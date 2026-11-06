// bot_wish_herder_close.go is the bridge's side of a herder close
// (gd-EfbganTatT). The scheduled herder judges each open wish; on a fulfilled
// verdict it stamps close_reason=herder-fulfilled on the head, closes it, and
// says its one-line evidence in the thread (gb say --thread <head>). Two things
// follow here:
//
//   - The thread card reads closed. A person's close leaves their ⏹️ on the
//     card; the herder has no hand to react with, so the bridge puts the same
//     ⏹️ there for it when the close event lands. The bot's own reactions never
//     reach handleWishCloseReaction (bot_reactions.go skips them), so this
//     cannot re-enter the close path.
//   - A reply reopens it. The herder can be wrong, and the asker answers its
//     note to say so: the next reply in the thread, from Slack or the Commons,
//     reopens the head, clears the close reason and lifts the ⏹️. Only the
//     herder's closes reopen this way (herder-fulfilled, and herder-owner-close
//     when the owner closed a stale wish on the herder's decision); a person's
//     ⏹️ or Done stays closed, as an idle autoclose does. kbeads counts on this undo when it lets a crew
//     spawn into the herder role (spawn_authz.go roleIsPrivileged).
package bridge

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// herderCloseEmoji is the reaction the bridge puts on a herder-closed wish's
// card: the wish-close glyph a person's close leaves (wishCloseEmojis).
const herderCloseEmoji = "black_square_for_stop"

// RegisterHerderCloseHandlers marks a wish's card closed when the herder
// closes it.
func (b *Bot) RegisterHerderCloseHandlers(stream *SSEStream) {
	stream.On("beads.bead.closed", b.handleHerderCloseEvent)
	if b.logger != nil {
		b.logger.Info("herder close registered SSE handlers", "topics", []string{"beads.bead.closed"})
	}
}

func (b *Bot) handleHerderCloseEvent(ctx context.Context, data []byte) {
	bead := ParseBeadEvent(data)
	if bead == nil || bead.Status != "closed" || !isHerderClose(bead.Fields["close_reason"]) {
		return
	}
	if bead.Type == "message" || bead.Type == "channel" || !hasLabel(bead.Labels, threadKindLabel) {
		return
	}
	b.markHerderClose(ctx, bead.ID, bead.Fields, true)
}

// herderCloseCard is the Slack message a herder close is marked on: the
// thread card, else the thread root. false for a wish with no Slack copy,
// whose Commons view reads the head's status itself.
func herderCloseCard(fields map[string]string) (conversation.Ref, bool) {
	if channel, ts := fields["slack_card_channel"], fields["slack_card_ts"]; isValidThreadBinding(channel, ts) {
		return conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: ts}, true
	}
	return slackThreadRoot(fields)
}

// markHerderClose puts the herder's ⏹️ on the wish's card (on) or lifts it.
// Best-effort: a reaction already there, or already gone, is no failure.
func (b *Bot) markHerderClose(ctx context.Context, headID string, fields map[string]string, on bool) {
	at, ok := herderCloseCard(fields)
	if !ok {
		return
	}
	s := b.conversationSlack()
	if s == nil {
		return
	}
	if err := s.React(ctx, at, herderCloseEmoji, on); err != nil {
		if msg := err.Error(); strings.Contains(msg, "already_reacted") || strings.Contains(msg, "no_reaction") {
			return
		}
		b.logger.Warn("herder close: marking the thread card failed",
			"head", headID, "channel", at.Channel, "ts", at.TS, "on", on, "error", err)
	}
}

// reopenHerderClosedWish reopens wish head headID when the herder closed it
// (isHerderClose), because someone replied in its thread. Any other head, open
// or closed by someone else, is left be. Best-effort: the reply is recorded
// and reaches its agent whether or not the reopen lands.
func (b *Bot) reopenHerderClosedWish(ctx context.Context, headID string) {
	if headID == "" || b.daemon == nil {
		return
	}
	head, err := b.daemon.GetBead(ctx, headID)
	if err != nil || head == nil || !isWishHeadBead(head) {
		return
	}
	if head.Status != "closed" || !isHerderClose(head.Fields["close_reason"]) {
		return
	}
	open := "open"
	if err := b.daemon.UpdateBead(ctx, headID, beadsapi.UpdateBeadRequest{
		Status: &open,
		Fields: map[string]any{"close_reason": nil},
	}); err != nil {
		b.logger.Warn("herder close: reopening the wish on a reply failed", "head", headID, "error", err)
		return
	}
	b.logger.Info("herder close: a reply reopened the wish", "head", headID)
	b.markHerderClose(ctx, headID, head.Fields, false)
}
