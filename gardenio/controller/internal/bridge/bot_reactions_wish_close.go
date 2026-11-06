// bot_reactions_wish_close.go implements the Slack-native wish-close gesture
// (1b-11, ka-0000001476): a :black_square_for_stop: (⏹️) reaction by a human on
// the thread card closes the resolved wish HEAD bead.
//
// DECISION (Matt Baker, 2026-06-24, locked): the close gesture is a human
// REACTION on the THREAD CARD. CloseBead(close_reason='slack-thread-user-close',
// closed_by=<reactor>). Under thread=wish, the Slack user is the close
// authority; this is the Slack-side death gesture that lets the death-story
// idle auto-close demote to a hint (1b-6) without trading "closes things that
// weren't done" for "never closes anything".
//
// CLOSE GLYPH (Matt Baker, 2026-06-25, ka-0000002200): the gesture is
// :black_square_for_stop: (⏹️), DELIBERATELY distinct from :white_check_mark:
// (✅). ✅ is already the bridge's OUTBOUND 'done' lifecycle reaction stamped on
// the card when the bound agent finishes (bot_reactions_lifecycle.go
// reactionDone = white_check_mark); reusing it would overload one glyph with
// two meanings ("agent finished" vs "user declares the wish done"). ⏹️ — the
// human "stop / this is done" gesture — keeps the two unambiguous.
//
// Wiring: handleReactionAdded (bot_reactions.go) routes this built-in BEFORE
// loadReactionRules — exactly mirroring the :bar_chart: human-survey precedent —
// so a rule bead carrying emoji=black_square_for_stop can never collide with the
// built-in close. The bot already ignores its OWN reactions (ev.User ==
// b.botUserID, bot_reactions.go); combined with the distinct glyph, the bridge's
// outbound 'done' lifecycle reaction (✅) can never reach this path either by
// identity (bot self-react) or by glyph — do not regress either guard.
//
// closed_by attribution: handleReactionAdded already stamps the reactor as the
// per-call operator on ctx (beadsapi.WithOperator(operatorFromSlackUser(...))).
// The daemon derives closed_by from that authenticated identity and IGNORES any
// client-supplied closed_by (kbeads http_beads.go, AUDIT §C3), so closing with
// this ctx attributes the close to the reactor without any extra plumbing.
package bridge

import (
	"context"
	"fmt"

	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// wishCloseReason is the audited close_reason stamped on a wish HEAD bead when
// a human closes it via the wish-close reaction. It joins the herder's
// close_reason vocabulary (herderCloseReason, herderOwnerCloseReason) and the
// auto-closer's historical slack-thread-idle (it stopped closing wishes in
// gd-Zhcpn9IZVl) so close provenance is machine-readable.
const wishCloseReason = "slack-thread-user-close"

// herderCloseReason is the close_reason the scheduled herder stamps on a wish
// head it judged fulfilled, before it closes the head and says its evidence in
// the thread (gd-EfbganTatT). The bridge marks the thread card closed when one
// lands and reopens the head on the next reply (bot_wish_herder_close.go), so
// the asker can contest the verdict by answering it.
const herderCloseReason = "herder-fulfilled"

// herderOwnerCloseReason is the close_reason of a stale wish the land's owner
// closed on the herder's decision (gd-g6lGiK8KXv, bot_wish_herder_decision.go).
// It reads closed and reopens on a reply as a herder-fulfilled close does.
const herderOwnerCloseReason = "herder-owner-close"

// isHerderClose reports whether reason is one of the herder's closes, which
// mark the card closed and reopen on the next reply.
func isHerderClose(reason string) bool {
	return reason == herderCloseReason || reason == herderOwnerCloseReason
}

// wishCloseEmojis is the SET of reaction names that trigger a wish close.
// black_square_for_stop (⏹️) is the canonical glyph: a human "stop / this is
// done" gesture, deliberately distinct from the bot's outbound ✅ done-lifecycle
// reaction so the two never overload one glyph (ka-0000002200). It is a single
// codepoint with no skin-tone/client variants, so the set has one member; it
// stays a set so a future alias can be added without reworking callers. These
// reaction names arrive WITHOUT colons (Slack's reaction_added events use the
// bare name).
var wishCloseEmojis = map[string]bool{
	"black_square_for_stop": true,
}

// isWishCloseEmoji reports whether the given reaction name is in the
// wish-close set.
func isWishCloseEmoji(emoji string) bool {
	return wishCloseEmojis[emoji]
}

// handleWishCloseReaction is the reaction-path entry for a human
// :black_square_for_stop: (⏹️) on a thread card. It resolves the reacted-to
// message to the wish HEAD bead and closes it with
// close_reason=slack-thread-user-close. The close is attributed to the reactor
// via the operator already stamped on ctx by handleReactionAdded.
//
// Returns true when the reaction was consumed as a wish-close gesture — i.e.
// it resolved to a wish HEAD (whether the close fired, was idempotently
// skipped, or was refused for triage). Returns FALSE when the reacted-to
// message does NOT resolve to a wish head, so the caller falls through to the
// legacy reaction-rule and unified trigger:slack-reaction paths: a
// black_square_for_stop on an ordinary message must still drive a rule/trigger
// bead keyed on that emoji, if one exists. Only a reaction that actually lands
// on a thread card is claimed by the built-in.
//
// Idempotent: a re-react on an already-closed wish is a safe no-op (closeWish).
func (b *Bot) handleWishCloseReaction(ctx context.Context, ev *slackevents.ReactionAddedEvent) bool {
	// Reactions on non-message items never carry thread context. (The caller
	// already gates on item type; this is a defensive re-check.)
	if ev.Item.Type != "message" {
		return false
	}
	channel := ev.Item.Channel
	ts := ev.Item.Timestamp

	headID := b.resolveWishHeadFromReaction(ctx, channel, ts)
	if headID == "" {
		// The reacted-to message does not resolve to a wish head (e.g. a
		// ⏹️ react on some unrelated message). Not a wish-close gesture —
		// let the caller fall through to the rule/trigger paths.
		b.logger.Debug("wish-close reaction: no wish head resolved",
			"channel", channel, "ts", ts, "emoji", ev.Reaction, "user", ev.User)
		return false
	}

	// From here the reaction landed on a thread card: it is a wish-close
	// gesture and is claimed by the built-in regardless of outcome (so a
	// rule bead keyed on the same emoji can never double-handle the same card).
	switch outcome, err := b.closeWish(ctx, headID); {
	case err != nil:
		b.logger.Warn("wish-close reaction: closing the wish head failed",
			"head", headID, "channel", channel, "ts", ts, "user", ev.User, "error", err)
	case outcome == wishAlreadyClosed:
		b.logger.Debug("wish-close reaction: wish already closed, no-op",
			"head", headID, "channel", channel, "ts", ts, "user", ev.User)
	case outcome == wishNeedsTriage:
		b.logger.Info("wish-close reaction: wish needs triage (failed agent), refusing close",
			"head", headID, "channel", channel, "ts", ts, "user", ev.User)
	default:
		b.logger.Info("wish-close reaction: closed wish head",
			"head", headID, "channel", channel, "ts", ts, "emoji", ev.Reaction, "user", ev.User)
	}
	return true
}

// wishCloseOutcome is what closeWish did with a wish head.
type wishCloseOutcome int

const (
	wishClosed        wishCloseOutcome = iota // closed now
	wishAlreadyClosed                         // closed before: a safe no-op
	wishNeedsTriage                           // held open for triage: refused
)

// closeWish is the wish-close core both surfaces share (slice 4b): the Slack
// ⏹️ reaction and the Commons Done button. It closes wish head headID with
// close_reason=slack-thread-user-close, attributed to the person through the
// operator (and, from the Commons, the bearer) on ctx.
//
// Idempotent: an already-closed wish is a no-op (an explicit status guard here,
// plus the daemon's own idempotent-close backstop, which returns the existing
// bead without re-stamping closed_at/closed_by).
func (b *Bot) closeWish(ctx context.Context, headID string) (wishCloseOutcome, error) {
	head, err := b.daemon.GetBead(ctx, headID)
	if err != nil {
		return 0, fmt.Errorf("read wish head %s: %w", headID, err)
	}
	if head == nil {
		return 0, fmt.Errorf("read wish head %s: not found", headID)
	}

	// Idempotency guard: skip the close (and its second close event / Slack
	// notification) when the wish is already closed.
	if head.Status == "closed" {
		return wishAlreadyClosed, nil
	}

	// Triage guard (1b-7, ka-0000003060): a wish whose bound agent FAILED is
	// deliberately held OPEN and stamped with the needs-triage label by the
	// auto-closer (markNeedsTriage) — a crash is the OPPOSITE of "done". The
	// auto-closer refuses to clean-close such a wish; the user-close gesture
	// must honour the same invariant, or a ⏹️ react would silently
	// bury a failed task out of the triage queue. Refuse the close and leave
	// the wish open for triage. (We intentionally do NOT block a merely
	// in-flight/working wish: the locked decision makes the Slack user the
	// close authority, so "good enough, done" mid-flight is an intended close.)
	if hasLabel(head.Labels, triageLabel) {
		return wishNeedsTriage, nil
	}

	closeFields := map[string]string{
		"close_reason": wishCloseReason,
	}
	if err := b.daemon.CloseBead(ctx, headID, closeFields); err != nil {
		return 0, fmt.Errorf("close wish head %s: %w", headID, err)
	}
	return wishClosed, nil
}

// resolveWishHeadFromReaction maps a reacted-to Slack message (channel, ts) to
// the wish HEAD bead it belongs to, trying the resolution paths in order of
// robustness:
//
//  1. Direct indexed card-ts lookup (preferred; 1b-13/ka-0000000693): the wish
//     HEAD carries slack_card_ts stamped at card-post time, so card_ts->head is
//     a single indexed field-query that survives a bridge restart / cache
//     eviction and is independent of the agent's terminal state.
//  2. Thread-label lookup: a reaction on the thread ROOT ts resolves directly
//     via the canonical slack-thread:{channel}:{ts} label, which only the head
//     bead carries.
//  3. Agent fallback: getAgentByThread(channel, ts) -> agent bead ->
//     fields.slack_thread_bead_id (== wish head). This is the FRAGILE path (the
//     daemon metadata fallback skips done/failed agents — and a finished thread
//     is exactly when the bound agent is done), so it is tried last.
//
// Returns the head bead id, or "" when no path resolves a wish head.
func (b *Bot) resolveWishHeadFromReaction(ctx context.Context, channel, ts string) string {
	// 1. Direct card-ts lookup (the locked primary surface).
	if id := b.findThreadHeadByCardTS(ctx, channel, ts); id != "" {
		return id
	}

	// 2. Thread ROOT-ts via the canonical thread label.
	if id := b.findThreadHeadByThreadLabel(ctx, channel, ts); id != "" {
		return id
	}

	// 3. Fragile fallback via the bound agent's slack_thread_bead_id.
	agentName := b.getAgentByThread(channel, ts)
	if agentName == "" {
		return ""
	}
	agentBead, err := b.daemon.FindAgentBead(ctx, extractAgentName(agentName))
	if err != nil || agentBead == nil {
		return ""
	}
	return agentBead.Fields["slack_thread_bead_id"]
}

// findThreadHeadByThreadLabel resolves the wish HEAD bead for a reaction on the
// thread ROOT ts via the canonical slack-thread:{channel}:{ts} label. Only the
// head bead carries this label (derivatives carry thread_id / thread:{headID}),
// so the match is unambiguous. Returns the head id or "" if none matches.
func (b *Bot) findThreadHeadByThreadLabel(ctx context.Context, channel, ts string) string {
	if b.daemon == nil || channel == "" || ts == "" {
		return ""
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{threadBeadLabel(channel, ts)},
		Limit:  1,
	})
	if err != nil || res == nil || len(res.Beads) == 0 {
		return ""
	}
	return res.Beads[0].ID
}
