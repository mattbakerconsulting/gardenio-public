// bot_decisions_operated_by.go gives a decision its OWN addressed human.
//
// Operator ruling (Matt Baker, 2026-08-27, thread ka-0000006045):
//
//	"design and build the 'operated_by' logic for decisions since we need
//	 that; and we should also create an exemption so that a user can author a
//	 decision that is operated_by someone else; essentially giving it to them."
//
// THE GAP THIS CLOSES. Until now the human on a decision was DERIVED, never
// stated: resolveDecisionMentionUser walked the requesting agent's bead and
// read its `slack_user_id`, so a decision could only ever reach the requesting
// agent's own operator. The motivating case — an agent that fixed a broken
// pipeline asking the MR's AUTHOR whether to push the fix — cannot be expressed
// that way at all, because the MR author is somebody else's operator or nobody's.
//
// A decision now carries `operated_by` (the escalation vocabulary: `oidc:<email>`,
// `slack:U…`), and that value WINS over the derived owner. Nothing is required
// to set it: a decision without `operated_by` behaves exactly as before.
//
// THE GIFTING EXEMPTION. `operated_by` on a decision is deliberately NOT
// constrained to the author's own operator — authoring a decision for someone
// else is the feature, not an abuse to guard against. When it names a different
// human the author is recorded in `gifted_by` so the handover is auditable
// rather than silent.
package bridge

import (
	"context"
	"strings"
)

// decisionAddressee is the human a decision is addressed to, resolved far
// enough to DM and to @-mention.
type decisionAddressee struct {
	SlackID string // "" when unresolvable — callers fall back to channel posting
	Raw     string // operated_by verbatim, "" when the addressee was derived
	Gifted  bool   // true when this decision was authored FOR someone else
}

// resolveDecisionAddressee returns the human this decision is addressed to.
//
// Order is deliberate:
//
//  1. The decision's OWN `operated_by`, resolved through the operator alias feed
//     (the same resolver CrossOperatorNotify and the escalation operator-DM use
//     — email-keyed and case-insensitive, refusing ambiguous rows rather than
//     picking one).
//  2. The requesting agent's operator (`slack_user_id` on the agent bead) —
//     today's behaviour, and still the right answer for the overwhelming
//     majority of decisions, which an agent files for its own operator.
//
// A decision that names an operator the feed cannot resolve degrades to the
// derived owner rather than to nobody: an undeliverable address must not make a
// decision undeliverable.
func (b *Bot) resolveDecisionAddressee(ctx context.Context, fields map[string]string, assignee string) decisionAddressee {
	out := decisionAddressee{}
	operatedBy := strings.TrimSpace(fields["operated_by"])
	if operatedBy != "" {
		out.Raw = operatedBy
		out.Gifted = strings.TrimSpace(fields["gifted_by"]) != ""
		if idx := b.operatorIndex(); idx != nil {
			if r := idx.Resolve(ctx, operatedBy, nil, ""); r.SlackID != "" {
				out.SlackID = r.SlackID
				return out
			}
		}
		b.logger.Warn("decision operated_by did not resolve to a Slack id; falling back to the requesting agent's operator",
			"operated_by", operatedBy)
	}
	out.SlackID = b.resolveDecisionMentionUser(ctx, assignee)
	return out
}

// operatorIndex returns the Bot's shared operator alias index, building it on
// first use. Returns nil when there is no daemon to fetch the feed from.
func (b *Bot) operatorIndex() *operatorSlackIndex {
	if b == nil || b.daemon == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.operatorIdx == nil {
		b.operatorIdx = newOperatorSlackIndex(b.daemon, b.logger)
	}
	return b.operatorIdx
}
