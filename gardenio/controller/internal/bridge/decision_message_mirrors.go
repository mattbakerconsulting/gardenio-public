// decision_message_mirrors.go lets ONE decision be represented by MORE THAN ONE
// Slack message, and keeps every copy honest as the decision moves.
//
// WHY THIS EXISTS. b.messages[beadID] is a single MessageRef, and every update
// path — updateMessageResolved, DismissDecision, markDecisionSuperseded — edits
// exactly that one message. So the moment a decision is posted to a second
// place, that second copy is frozen at "open" forever: it keeps showing live
// option buttons for a decision that was answered, dismissed or superseded
// hours ago. That is the single blocking defect between here and the operator's
// ask (Matt Baker, 2026-08-27, thread ka-0000006045):
//
//	"decisions should DM their operators when they're created … if it has to
//	 live in just one place in slack i choose the DMs"
//
// SHAPE, AND WHY NOT A map[string][]MessageRef. Converting b.messages to a slice
// would touch every escalation path too (bot_escalations.go tracks escalation
// cards in the same map) for no benefit. Instead the PRIMARY ref stays exactly
// where it is, with exactly today's semantics, and mirrors are additive:
//
//   - The PRIMARY is the answerable card. It owns the interaction path, the
//     agent-pending accounting, and the thread that reports/tldrs are posted
//     into. Under C2 (ka-0000007942) the primary becomes the DM copy.
//   - A MIRROR is a secondary rendering of the same decision — under C2, the
//     one-line channel pointer that says the ask went to a human's DMs. Mirrors
//     carry no buttons and no accounting; they exist to not lie.
//
// Every existing call site is therefore unchanged in behaviour while a decision
// has no mirrors, which is every decision today. That is deliberate: this lands
// as a no-op and C2 turns it on.
package bridge

import (
	"context"

	"github.com/slack-go/slack"
)

// addMessageMirror registers an additional Slack message rendering decision
// beadID, so subsequent lifecycle updates reach it too. The primary ref
// (b.messages[beadID]) is untouched.
func (b *Bot) addMessageMirror(beadID string, ref MessageRef) {
	if beadID == "" || ref.ChannelID == "" || ref.Timestamp == "" {
		return
	}
	b.mu.Lock()
	if b.messageMirrors == nil {
		b.messageMirrors = make(map[string][]MessageRef)
	}
	for _, existing := range b.messageMirrors[beadID] {
		if existing.ChannelID == ref.ChannelID && existing.Timestamp == ref.Timestamp {
			b.mu.Unlock()
			return
		}
	}
	b.messageMirrors[beadID] = append(b.messageMirrors[beadID], ref)
	b.mu.Unlock()

	if b.state != nil {
		_ = b.state.SetDecisionMessageMirrors(beadID, b.lookupMessageMirrors(beadID))
	}
}

// lookupMessageMirrors returns the mirror refs for a decision, hot cache first,
// then persisted state (mirroring lookupMessage). The returned slice is a copy.
func (b *Bot) lookupMessageMirrors(beadID string) []MessageRef {
	b.mu.Lock()
	refs := b.messageMirrors[beadID]
	out := make([]MessageRef, len(refs))
	copy(out, refs)
	b.mu.Unlock()
	if len(out) > 0 {
		return out
	}
	if b.state != nil {
		return b.state.GetDecisionMessageMirrors(beadID)
	}
	return nil
}

// clearMessageMirrors drops all mirror tracking for a decision, in hot cache and
// in persisted state. Called from the paths that also drop the primary ref.
func (b *Bot) clearMessageMirrors(beadID string) {
	b.mu.Lock()
	delete(b.messageMirrors, beadID)
	b.mu.Unlock()
	if b.state != nil {
		_ = b.state.RemoveDecisionMessageMirrors(beadID)
	}
}

// fanoutToMirrors applies fn to every mirror of a decision, logging and
// continuing past individual failures.
//
// BEST-EFFORT BY DESIGN. A mirror is a secondary rendering; the primary has
// already been updated by the caller before we get here. Aborting the whole
// fanout because one DM channel 404'd would leave the remaining mirrors stale
// to save a mirror that is already gone, so each ref stands alone.
func (b *Bot) fanoutToMirrors(ctx context.Context, beadID, action string, fn func(context.Context, MessageRef) error) {
	for _, ref := range b.lookupMessageMirrors(beadID) {
		if err := fn(ctx, ref); err != nil {
			b.logger.Warn("decision mirror update failed",
				"bead", beadID, "action", action,
				"channel", ref.ChannelID, "ts", ref.Timestamp, "error", err)
			continue
		}
		b.logger.Debug("decision mirror updated",
			"bead", beadID, "action", action, "channel", ref.ChannelID)
	}
}

// updateMirrorsResolved rewrites every mirror of a resolved decision to the
// resolved summary, so a secondary copy cannot keep advertising an open ask.
func (b *Bot) updateMirrorsResolved(ctx context.Context, beadID, chosen, rationale string) {
	blocks := mirrorResolvedBlocks(chosen, rationale)
	fallback := "Decision resolved: " + chosen
	b.fanoutToMirrors(ctx, beadID, "resolved", func(ctx context.Context, ref MessageRef) error {
		_, _, _, err := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
			slack.MsgOptionText(fallback, false),
			slack.MsgOptionBlocks(blocks...),
		)
		return err
	})
}

// mirrorResolvedBlocks renders the resolved state shown on a mirror. It matches
// updateMessageResolved's rendering so the two copies read identically.
func mirrorResolvedBlocks(chosen, rationale string) []slack.Block {
	text := ":white_check_mark: *Resolved*: " + chosen
	if rationale != "" {
		text += "\n_" + rationale + "_"
	}
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}
}

// deleteMirrors removes every mirror of a dismissed decision and drops the
// tracking. Pairs with DismissDecision, which deletes the primary.
func (b *Bot) deleteMirrors(ctx context.Context, beadID string) {
	b.fanoutToMirrors(ctx, beadID, "dismissed", func(ctx context.Context, ref MessageRef) error {
		_, _, err := b.api.DeleteMessageContext(ctx, ref.ChannelID, ref.Timestamp)
		return err
	})
	b.clearMessageMirrors(beadID)
}

// updateMirrorsSuperseded rewrites every mirror of a superseded decision to
// point at its successor. Pairs with markDecisionSuperseded.
func (b *Bot) updateMirrorsSuperseded(ctx context.Context, predecessorID, newDecisionID string) {
	blocks := b.supersededBlocks(predecessorID, newDecisionID)
	b.fanoutToMirrors(ctx, predecessorID, "superseded", func(ctx context.Context, ref MessageRef) error {
		_, _, _, err := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
			slack.MsgOptionBlocks(blocks...),
		)
		return err
	})
}
