// bot_decisions_dm.go makes the DM the HOME of a decision, not a second copy of
// it.
//
// Operator ruling (Matt Baker, 2026-08-27, thread ka-0000006045):
//
//	"decisions should DM their operators when they're created and such, the
//	 decision should live there; if it has to live in just one place in slack i
//	 choose the DMs"
//	"but top priority is the decision DMing"
//
// WHY DM-FIRST RATHER THAN DM-ALSO. Two live interactive copies of one decision
// is where the update-fanout bugs live: whichever copy is not clicked has to be
// chased down and rewritten on resolve, dismiss, supersede and every report. So
// exactly ONE copy carries buttons — the DM — and the channel keeps a one-line
// pointer registered as a MIRROR (decision_message_mirrors.go), which means the
// channel still shows the outcome without ever competing for the answer.
//
// This differs deliberately from the escalation twin (escalation_operator_dm.go),
// which DMs a NOTIFICATION and leaves the answerable card in the channel. That
// asymmetry is the operator's explicit choice for decisions, not an oversight.
//
// WHY THE BUTTONS JUST WORK IN A DM. handleBlockActions dispatches on
// action_id `resolve_<beadID>_<n>` with value `<beadID>:<n>` and never reads the
// channel to identify the decision, so a card posted into a DM is answerable
// with no change to the interaction path. Verified before this was written; it
// is the reason this change is delivery-only.
//
// FAIL-SAFE. Every failure — no addressee, unresolvable user, Slack refusing the
// DM — falls back to exactly today's behaviour: the full card in the channel.
// A decision that cannot be DMed must never be a decision that was not asked.
package bridge

import (
	"context"
	"fmt"

	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// decisionDMEnabled reports whether decisions are delivered DM-first.
//
// Default ON (the operator's stated default). SLACK_DECISION_DM=off is the
// kill switch: it restores channel-only posting without a revert, for the case
// where DM delivery misbehaves in prod against a workspace we cannot test here.
func (b *Bot) decisionDMEnabled() bool {
	return b.decisionDMMode != "off"
}

// deliverDecisionToDM posts the answerable decision card into the addressed
// human's DM. Returns the resulting ref and true on success; false means the
// caller must post the card to the channel exactly as before.
func (b *Bot) deliverDecisionToDM(ctx context.Context, beadID, userID, fallbackText string, blocks []slack.Block) (MessageRef, bool) {
	if !b.decisionDMEnabled() || userID == "" || beadID == "" {
		return MessageRef{}, false
	}
	dmChannel, dmTS, err := b.SendDMBlocks(ctx, userID, fallbackText, blocks...)
	if err != nil {
		// Fall back to the channel. This is a WARN, not an error return: the
		// decision still gets asked, just in the old place.
		b.logger.Warn("decision DM delivery failed, falling back to channel post",
			"bead", beadID, "user", userID, "error", err)
		return MessageRef{}, false
	}
	b.logger.Info("posted decision card to operator DM",
		"bead", beadID, "user", userID, "channel", dmChannel, "ts", dmTS)
	return MessageRef{ChannelID: dmChannel, Timestamp: dmTS}, true
}

// postDecisionPointer posts the one-line channel/thread notice that stands in
// for a decision now living in someone's DMs, and registers it as a mirror so
// the resolve/dismiss/supersede fanout keeps it truthful.
//
// The pointer is the Slack face of the decision's card in that channel of land
// (decision_cards.go), so the Commons shows the whole decision there. It asks
// nobody anything (the decision is in the DM), so it is a machine card: at the
// top level its Commons copy is in the land's #machines, and in a thread it
// stays in the thread's channel (gd-GgRk2fLZKQ).
//
// Best-effort: a decision that reached its human is delivered whether or not
// the pointer lands, so a failure here is logged and swallowed.
func (b *Bot) postDecisionPointer(ctx context.Context, land, beadID, channelID, threadTS, userID, question string) {
	if channelID == "" {
		return
	}
	text := fmt.Sprintf(":envelope_with_arrow: *Decision sent to <@%s>* — %s\n_Asked and answered in DMs._ %s",
		userID, truncateRunes(question, decisionPointerQuestionLimit), b.beadLink(beadID, beadID))

	blocks := []slack.Block{slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", text, false, false), nil, nil)}
	ref, _, err := b.postSubjectCardBead(ctx, land, beadID, conversation.Principal{}, channelID, threadTS, text, blocks, true)
	if err != nil {
		b.logger.Warn("decision channel pointer failed (decision itself was DMed)",
			"bead", beadID, "channel", channelID, "error", err)
		return
	}
	b.addMessageMirror(beadID, ref)
}

// decisionPointerQuestionLimit bounds how much of the question the channel
// pointer quotes. The pointer is a breadcrumb, not the ask — the full decision
// is in the DM and on the bead.
const decisionPointerQuestionLimit = 160
