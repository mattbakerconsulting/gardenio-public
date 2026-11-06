package bridge

import (
	"context"
	"fmt"

	"github.com/slack-go/slack"
)

// decisionResolvePermission is the outcome of checking whether a Slack user
// may respond to a decision.
type decisionResolvePermission struct {
	// Allowed reports whether the clicking user is permitted to resolve the
	// decision.
	Allowed bool
	// OwnerUserID is the Slack user ID of the operator the decision is
	// addressed to (the owning agent's slack_user_id), when one could be
	// resolved. Empty when the decision has no resolvable owner.
	OwnerUserID string
}

// checkDecisionResolvePermission decides whether clickingUserID is permitted to
// respond to the decision identified by beadID.
//
// POLICY (permissive, operator ruling 2026-08-27): anyone may answer any
// decision. The previous owner-only gate (ka-0000001906) is retired here, on
// the operator's explicit instruction in the decisions-in-DMs thread
// (ka-0000006045):
//
//	"for 3, my ruling is this: loosen up decision answering restrictions right
//	 now, get the UX right. simultaneously, design and build the 'operated_by'
//	 logic for decisions since we need that"
//
// WHY THIS IS SAFE TO LOOSEN NOW. The owner-only gate was the wrong instrument
// for the problem it was pointed at, in both directions:
//
//   - It could not express the case that motivates the change — an agent
//     addressing a decision to a human who is NOT its own operator (e.g. the
//     author of the MR it wants to push a fix to). The addressee is DERIVED
//     from the requesting agent's owner, so such a human was both un-addressable
//     and, if the card reached them anyway, denied on click.
//   - It never bound the adversarial case it reads like it does. Any agent
//     fleet token already satisfies requireRole(RoleOperator) daemon-side and
//     can terminate a decision addressed to a human (see the header of
//     cross_operator_notify.go). This gate only ever stopped a HUMAN clicking a
//     button in Slack.
//
// Accountability is preserved by the detective control the operator chose over
// a preventive one: CrossOperatorNotify observes terminations and tells the
// addressed human when somebody else answered for them. That control is what
// makes a permissive gate the right default rather than a hole.
//
// OwnerUserID is still resolved on every call. It is not used to deny — it is
// the addressee identity that the caller logs, and the seam that the
// forthcoming decision `operated_by` work (ka-0000000024) re-arms a NARROWER,
// correctly-addressed rule against. Do not delete it as dead weight.
func (b *Bot) checkDecisionResolvePermission(ctx context.Context, beadID, clickingUserID string) decisionResolvePermission {
	bead, err := b.daemon.GetBead(ctx, beadID)
	if err != nil {
		b.logger.Warn("decision permission: failed to fetch bead, allowing",
			"bead", beadID, "error", err)
		return decisionResolvePermission{Allowed: true}
	}

	// The addressee is the decision's own `operated_by` when it has one,
	// else the requesting agent's operator (bot_decisions_operated_by.go).
	ownerUserID := b.resolveDecisionAddressee(ctx, bead.Fields, bead.Assignee).SlackID
	if ownerUserID != "" && clickingUserID != ownerUserID {
		// Answered by someone other than the addressee. Permitted — and the
		// addressed human is told by CrossOperatorNotify. Logged at Info so the
		// cross-answer rate is observable before `operated_by` lands.
		b.logger.Info("decision answered by non-addressee (permitted under permissive policy)",
			"bead", beadID, "user", clickingUserID, "addressee", ownerUserID)
	}

	return decisionResolvePermission{
		Allowed:     true,
		OwnerUserID: ownerUserID,
	}
}

// denyDecisionResolve posts a nice ephemeral error to the clicking user
// explaining that the decision is addressed to someone else. Best-effort.
// Delivered response_url-first (postInteractionEphemeral) so it reaches
// external / Slack Connect users too (ka-0000005533).
//
// Under the permissive policy above this is not reached. It is retained,
// wired at every resolve site, as the deny surface for the `operated_by` rule
// (ka-0000000024) — that work changes checkDecisionResolvePermission, not the
// five call sites.
func (b *Bot) denyDecisionResolve(ctx context.Context, beadID string, perm decisionResolvePermission, callback slack.InteractionCallback) {
	b.logger.Info("decision resolve denied: user lacks permission",
		"bead", beadID, "user", callback.User.ID, "owner", perm.OwnerUserID)
	msg := ":lock: This decision is for someone else — you don't have permission to respond to it."
	if perm.OwnerUserID != "" {
		msg = fmt.Sprintf(":lock: This decision is for <@%s> — you don't have permission to respond to it.", perm.OwnerUserID)
	}
	b.postInteractionEphemeral(ctx, callback, callback.Channel.ID, msg)
}
