// bot_wish_herder_decision.go acts on the owner's answer about a stale wish
// (gd-g6lGiK8KXv). When the scheduled herder has judged a wish unfulfilled or
// unclear on two passes, at least 14 days after it was made, with no word from
// a person in the thread since, it asks the land's owner once: gb decision
// create --requesting-task <head> with three options (close it, revive it with
// a note, keep waiting), and stamps the decision's id in the head's
// herder_decision field. The answer is acted on here, when the decision
// closes, rather than by the herder:
//
//   - close: the head closes with close_reason=herder-owner-close. Like a
//     herder-fulfilled close, its card gets the ⏹️ and the next reply in the
//     thread reopens it (bot_wish_herder_close.go).
//   - revive: the thread's agent gets the owner's note, as gb say --wake gives
//     it the herder's words (wakeWishAgent), and herder_woke_at is stamped so
//     the herder does not wake it again within the day. A free-text answer that
//     matched no option is a note too.
//   - keep waiting, a dismissal or an expiry: nothing changes but the record.
//
// The herder is a scheduled role whose pod has long stopped by the time the
// owner answers. Left to the generic path, every answer would respawn a herder
// under the trigger's live-run lease (Decisions.nudgeAgent), so that path
// passes these decisions over (isHerderWishDecision).
//
// Only an answer read from the decision bead, and given by a person rather
// than the herder or another agent of the land (answeredByOwner), is acted on;
// anything else is left with no record for a later event or pass.
//
// Each answer leaves one "[herder] owner answered <decision>" comment on the
// head. It is written before the act and read before it, so a replayed close
// event does not act twice; the herder's next pass finishes a close that was
// recorded but did not land.
package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The option ids of the herder's stale-wish decision, and the head field that
// names the decision.
const (
	herderAskClose      = "close"
	herderAskRevive     = "revive"
	herderAskWait       = "wait"
	herderDecisionField = "herder_decision"
)

// herderDecisionActor authors the reactor's comments and is the system
// operator of its writes: the owner's answer is the judgement, and no person
// stands behind the write that carries it out.
const herderDecisionActor = "bridge-herder-decision-reactor"

// herderAnsweredMarker opens the comment an answer leaves on its head.
const herderAnsweredMarker = "[herder] owner answered "

// RegisterHerderDecisionHandlers acts on the owner's answers to the herder's
// stale-wish decisions.
func (b *Bot) RegisterHerderDecisionHandlers(stream *SSEStream) {
	stream.On("beads.bead.closed", b.handleHerderDecisionClosed)
	if b.logger != nil {
		b.logger.Info("herder decision registered SSE handlers", "topics", []string{"beads.bead.closed"})
	}
}

// herderWishOfDecision is the wish head a decision asks the owner about: its
// requesting task, when that is a wish head whose herder_decision names the
// decision. nil for any other decision. The head's field is what makes it the
// herder's: requesting_task alone is the filer's to set.
func herderWishOfDecision(ctx context.Context, daemon BeadClient, decision *BeadEvent) *beadsapi.BeadDetail {
	if daemon == nil || decision == nil || decision.Type != "decision" {
		return nil
	}
	task := strings.TrimSpace(decision.Fields["requesting_task"])
	if task == "" {
		return nil
	}
	head, err := daemon.GetBead(ctx, task)
	if err != nil || head == nil || !isWishHeadBead(head) || head.Fields[herderDecisionField] != decision.ID {
		return nil
	}
	return head
}

// isHerderWishDecision reports whether decision is a herder's stale-wish
// question, which this reactor answers in place of its filer.
func isHerderWishDecision(ctx context.Context, daemon BeadClient, decision *BeadEvent) bool {
	return herderWishOfDecision(ctx, daemon, decision) != nil
}

func (b *Bot) handleHerderDecisionClosed(ctx context.Context, data []byte) {
	ev := ParseBeadEvent(data)
	if ev == nil || ev.Type != "decision" || b.daemon == nil {
		return
	}
	head := herderWishOfDecision(ctx, b.daemon, ev)
	if head == nil {
		return
	}
	// A close event may not carry the answer (chosen, response_text,
	// rationale, responded_by); the decision bead does. Without it nothing is
	// done and nothing recorded, so the next event or pass tries again: the
	// event's fields alone would read as a "wait" that no replay could undo.
	dec, err := b.daemon.GetBead(ctx, ev.ID)
	if err != nil || dec == nil || dec.Type != "decision" {
		b.logger.Warn("herder decision: reading the decision failed", "head", head.ID, "decision", ev.ID, "error", err)
		return
	}
	if err := b.answeredByOwner(ctx, dec, projectFromLabels(head.Labels)); err != nil {
		b.logger.Warn("herder decision: not the owner's answer; nothing done", "head", head.ID, "decision", ev.ID, "error", err)
		return
	}
	b.actOnHerderDecision(beadsapi.WithSystemOperator(ctx, herderDecisionActor), head, ev.ID, dec.Fields)
}

// answeredByOwner checks, as steward.CheckOwnerApproval does, that dec was
// answered by a person and not by the herder that asked it or another agent of
// land: the answer is acted on and announced in the thread as the owner's. It
// fails closed: no responder, a system responder, or agents that cannot be
// read are a refusal.
func (b *Bot) answeredByOwner(ctx context.Context, dec *beadsapi.BeadDetail, land string) error {
	by := strings.TrimSpace(dec.Fields["responded_by"])
	if by == "" {
		return fmt.Errorf("%s names no one who answered it", dec.ID)
	}
	if strings.HasPrefix(by, "system:") || by == herderDecisionActor {
		return fmt.Errorf("%s was answered by %s, not by a person", dec.ID, by)
	}
	for _, self := range []string{dec.CreatedBy, dec.Fields["requested_by"]} {
		if self = strings.TrimSpace(self); self != "" && by == self {
			return fmt.Errorf("%s was answered by %s, who asked it, not by the owner", dec.ID, by)
		}
	}
	if land == "" {
		return fmt.Errorf("%s asks about a wish of no land, so its agents cannot be checked", dec.ID)
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Labels: []string{"project:" + land}, Limit: 1000,
	})
	if err != nil || res == nil {
		return fmt.Errorf("reading %s's agents to check who answered %s: %w", land, dec.ID, err)
	}
	for _, a := range res.Beads {
		if by == a.Title || by == a.Fields["agent"] || by == a.ID {
			return fmt.Errorf("%s was answered by %s, an agent of the land, not by the owner", dec.ID, by)
		}
	}
	return nil
}

// herderAnswer is what an answer asks for: close, revive or wait. A free-text
// answer that matched no option is a note for the thread's agent.
func herderAnswer(answer map[string]string) string {
	switch chosen := strings.ToLower(strings.TrimSpace(answer["chosen"])); {
	case chosen == herderAskClose:
		return herderAskClose
	case chosen == herderAskRevive:
		return herderAskRevive
	case chosen == "" && strings.TrimSpace(answer["response_text"]) != "":
		return herderAskRevive
	default:
		return herderAskWait
	}
}

// herderOwnerNote is the owner's own words on an answer: the free text, else a
// rationale they wrote. The Slack card's "Chosen by @x via Slack" is no note.
func herderOwnerNote(answer map[string]string) string {
	if t := strings.TrimSpace(answer["response_text"]); t != "" {
		return t
	}
	if r := strings.TrimSpace(answer["rationale"]); r != "" && !strings.HasPrefix(r, "Chosen by ") {
		return r
	}
	return ""
}

// answeredBefore reports whether head already records an answer to decision,
// or its comments cannot be read.
func (b *Bot) answeredBefore(ctx context.Context, headID, decisionID string) bool {
	comments, err := b.daemon.GetComments(ctx, headID)
	if err != nil {
		// Unread is answered: acting twice (a second wake) is worse than
		// leaving the answer to the next event or pass.
		return true
	}
	for _, c := range comments {
		if strings.HasPrefix(c.Text, herderAnsweredMarker+decisionID) {
			return true
		}
	}
	return false
}

func (b *Bot) actOnHerderDecision(ctx context.Context, head *beadsapi.BeadDetail, decisionID string, answer map[string]string) {
	if b.answeredBefore(ctx, head.ID, decisionID) {
		return
	}
	act := herderAnswer(answer)
	note := herderOwnerNote(answer)
	record := fmt.Sprintf("%s%s: %s.", herderAnsweredMarker, decisionID, act)
	if note != "" {
		record += " Note: " + note
	}
	if err := b.daemon.AddComment(ctx, head.ID, herderDecisionActor, record); err != nil {
		// Without the record a replay would act again; leave it to the next pass.
		b.logger.Warn("herder decision: recording the owner's answer failed", "head", head.ID, "decision", decisionID, "error", err)
		return
	}
	b.logger.Info("herder decision: the owner answered", "head", head.ID, "decision", decisionID, "answer", act)
	switch act {
	case herderAskClose:
		b.closeWishForOwner(ctx, head, decisionID)
	case herderAskRevive:
		b.reviveWishForOwner(ctx, head, decisionID, note)
	}
}

// closeWishForOwner closes head as the owner asked. The note goes in the Slack
// thread first, so the bot's own post can never land on a closed head.
func (b *Bot) closeWishForOwner(ctx context.Context, head *beadsapi.BeadDetail, decisionID string) {
	if head.Status == "closed" {
		return
	}
	b.herderThreadNote(ctx, head, fmt.Sprintf(
		"The land's owner closed this wish on decision %s. Reply here if it is not done, and it reopens.", decisionID))
	if err := b.daemon.CloseBead(ctx, head.ID, map[string]string{"close_reason": herderOwnerCloseReason}); err != nil {
		b.logger.Warn("herder decision: closing the wish failed", "head", head.ID, "decision", decisionID, "error", err)
	}
}

// reviveWishForOwner gives the owner's note to the thread's agent, reopening
// head first when it has closed since the herder asked.
func (b *Bot) reviveWishForOwner(ctx context.Context, head *beadsapi.BeadDetail, decisionID, note string) {
	if head.Status == "closed" {
		open := "open"
		if err := b.daemon.UpdateBead(ctx, head.ID, beadsapi.UpdateBeadRequest{
			Status: &open,
			Fields: map[string]any{"close_reason": nil},
		}); err != nil {
			b.logger.Warn("herder decision: reopening the wish to revive it failed", "head", head.ID, "decision", decisionID, "error", err)
			return
		}
	}
	text := fmt.Sprintf("The land's owner asked that this wish be revived (decision %s).", decisionID)
	if note != "" {
		text += " Their note: " + note
	}
	text += " Pick it up where it stopped, or say in the thread what blocks it."
	b.herderThreadNote(ctx, head, text)
	woke := b.wakeWishAgent(ctx, b.wishThreadAt(ctx, head), head, "", "Revive from the owner", text, decisionID)
	b.logger.Info("herder decision: revived the wish", "head", head.ID, "decision", decisionID, "woke", woke)
	if err := b.daemon.UpdateBeadFields(ctx, head.ID, map[string]string{
		"herder_woke_at": time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		b.logger.Warn("herder decision: stamping herder_woke_at failed", "head", head.ID, "error", err)
	}
}

// wishThreadAt is the anchor of head's thread: its land, its channel bead and
// its Slack copy, when that copy is in a channel of the land.
func (b *Bot) wishThreadAt(ctx context.Context, head *beadsapi.BeadDetail) threadAt {
	at := threadAt{
		Head:      head.ID,
		Land:      projectFromLabels(head.Labels),
		ChannelID: firstNonEmptyField(head.Fields, "channel_id"),
	}
	if root, ok := slackThreadRoot(head.Fields); ok && b.slackChannelOfLand(ctx, root.Channel, at.Land) {
		at.Channel, at.TS = root.Channel, root.TS
	}
	return at
}

// herderThreadNote posts text in head's Slack thread, where the asker sees
// it. A wish with no Slack copy shows its status in the Commons.
func (b *Bot) herderThreadNote(ctx context.Context, head *beadsapi.BeadDetail, text string) {
	at := b.wishThreadAt(ctx, head)
	if !at.slack() || b.api == nil {
		return
	}
	if err := b.replyInThread(ctx, at.Channel, at.TS, text); err != nil {
		b.logger.Warn("herder decision: posting in the thread failed", "head", head.ID, "error", err)
	}
}
