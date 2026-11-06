package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack/slackevents"
)

// Slice 4b of the Commons migration (design spec
// 2026-10-19-lands-commons-design.md, sections 3.4 and 5.2): marking a wish
// done, from Slack or from the Commons, is one conversation.Inbound{React}
// with Reaction conversation.ReactionDone, and HandleReact runs it.
//
//   - Slack: handleReactionAdded is the adapter. A ⏹️ (black_square_for_stop)
//     on a message becomes Inbound{React, done} carrying the event in Raw;
//     HandleReact runs handleWishCloseReaction on it, unchanged. A ⏹️ that
//     resolves to no wish head is not taken (errReactionNotTaken), so it falls
//     through to the reaction rules as before. Every other reaction stays the
//     Slack adapter's: reaction rules are not ported.
//   - Commons: the Done button on a wish head calls POST
//     /api/conversation/inbound with {"kind":"react","message":<head>,
//     "reaction":"done"} (api_conversation.go). The bridge reads the head
//     itself, checks "done" against the caller's land role, and closes it on
//     the person's bearer through the same core (closeWish).
//
// Wish close posts nothing to Slack on either surface;
// TestInboundReactCallsGolden pins that, and the close.

// errReactionNotTaken is a Slack reaction no handler takes (a ⏹️ on a message
// that is no thread card): the adapter goes on to the reaction rules.
var errReactionNotTaken = errors.New("bridge: slack reaction not taken")

// slackReactInbound normalises a Slack wish-close reaction. operator is the
// reactor resolved through the account link (operatorFromSlackUser), "" when
// the event names no user.
func slackReactInbound(ev *slackevents.ReactionAddedEvent, operator string) conversation.Inbound {
	return conversation.Inbound{
		Kind:     conversation.InboundReact,
		Surface:  conversation.SurfaceSlack,
		Actor:    conversation.Principal{ID: operator, SlackUser: ev.User},
		Reaction: conversation.ReactionDone,
		At:       conversation.Ref{Surface: conversation.SurfaceSlack, Channel: ev.Item.Channel, TS: ev.Item.Timestamp},
		Raw:      ev,
	}
}

// HandleReact runs a reaction: today only done, which closes a wish.
func (b *Bot) HandleReact(ctx context.Context, in conversation.Inbound) (conversation.Answer, error) {
	if in.Reaction != conversation.ReactionDone {
		return conversation.Answer{}, fmt.Errorf("reaction %q: %w", in.Reaction, conversation.ErrNoHandler)
	}
	if in.Surface == conversation.SurfaceCommons {
		return b.commonsDone(ctx, in.Message, in.Actor.ID)
	}
	ev, ok := in.Raw.(*slackevents.ReactionAddedEvent)
	if !ok || ev == nil {
		return conversation.Answer{}, fmt.Errorf("slack reaction carries no event: %w", conversation.ErrNoHandler)
	}
	if !b.handleWishCloseReaction(ctx, ev) {
		return conversation.Answer{}, errReactionNotTaken
	}
	return conversation.Answer{}, nil
}

// readCommonsWish reads wish head id as the bridge, with its one land. A bead
// that is no wish head, or that carries no land or two, is refused (rule 1 of
// section 6.2).
func (b *Bot) readCommonsWish(ctx context.Context, id string) (*beadsapi.BeadDetail, string, error) {
	if b.daemon == nil {
		return nil, "", inboundRefuse(http.StatusServiceUnavailable, "The bridge has no beads daemon.")
	}
	head, err := b.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, "", inboundRefuse(http.StatusNotFound, "That wish is gone.")
		}
		return nil, "", fmt.Errorf("read wish %s: %w", id, err)
	}
	if head == nil {
		return nil, "", inboundRefuse(http.StatusNotFound, "That wish is gone.")
	}
	if head.Type == "message" || head.Type == "channel" || !hasLabel(head.Labels, threadKindLabel) {
		return nil, "", inboundRefuse(http.StatusUnprocessableEntity, "That is not a wish.")
	}
	land, ok := onlyLand(head.Labels)
	if !ok {
		return nil, "", inboundRefuse(http.StatusUnprocessableEntity, "That wish belongs to no single land.")
	}
	return head, land, nil
}

// commonsDone closes wish head id for principal, the Commons twin of a ⏹️ on
// its Slack thread card. The inbound endpoint has already checked "done"
// against the caller's land role.
func (b *Bot) commonsDone(ctx context.Context, id, principal string) (conversation.Answer, error) {
	if _, _, err := b.readCommonsWish(ctx, id); err != nil {
		return conversation.Answer{}, err
	}
	outcome, err := b.closeWish(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusConflict) {
			return conversation.Answer{}, inboundRefuse(apiErr.StatusCode, "The beads daemon refused that: %s", strings.TrimSpace(apiErr.Message))
		}
		return conversation.Answer{}, err
	}
	switch outcome {
	case wishAlreadyClosed:
		return conversation.Answer{}, inboundRefuse(http.StatusConflict, "This wish is done already.")
	case wishNeedsTriage:
		return conversation.Answer{}, inboundRefuse(http.StatusConflict,
			"This wish needs triage: its agent failed. Look at what went wrong before you close it.")
	}
	b.logger.Info("wish closed via the Commons", "head", id, "user", principal)
	return sayAnswer("Marked done."), nil
}
