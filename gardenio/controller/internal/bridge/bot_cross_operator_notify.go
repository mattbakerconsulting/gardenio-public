package bridge

import "log/slog"

// CrossOperatorNotify builds the cross-operator resolution notifier bound to
// this bot's DM sender and bead-link renderer (ka-0000002131).
//
// Returns nil for a nil bot, which disables the control — a slack-bridge running
// without a Slack app keeps working exactly as before.
//
// ROUTING (ka-0000000294, superseding the DM-only routing this file originally
// shipped with): the notice goes into the decision's OWN Slack thread, and
// falls back to a DM only when the bridge has no message ref for the bead.
//
// The original choice was DM-only, on the reasoning that a DM always lands
// while a channel @-mention reaches only channel members. That is true, and it
// is why the fallback survives — but it answered the wrong question. It weighed
// DM against a fresh TOP-LEVEL channel post, which would indeed have been
// noise. Replying under the card the bridge already posted is neither: it is
// the thread the reader was already in.
//
// The operator ruled on the real-world result (Pat Stone, 2026-08-26):
//
//	"I really don't like getting DM's to say that one of your threads has
//	 updated a decision. That should just be captured in the actual slack
//	 conversation."
//
// Two things improve beyond taste. The notice becomes visible to everyone
// following that decision rather than to one recipient, which is what an
// ACCOUNTABILITY control wants. And it stops fragmenting one conversation
// across two surfaces — the card says what was decided, the thread reply says
// who decided it, in the same place.
//
// This does NOT re-post what the bridge already posts (advice ka-0000000409):
// UpdateDecision edits the card to show the outcome; it never names the
// resolving party, which is the entire content of this message.
//
// The `<@U…>` mention is still rendered inside the text so the addressed
// operator is notified wherever it lands.
func (b *Bot) CrossOperatorNotify(daemon BeadClient, logger *slog.Logger) *CrossOperatorNotify {
	if b == nil {
		return nil
	}
	return NewCrossOperatorNotify(CrossOperatorNotifyConfig{
		Daemon:   daemon,
		Sender:   b,
		Thread:   b,
		Logger:   logger,
		LinkBead: b.beadTitleLink,
		// Resolver-side provider-handle resolution only; see
		// sameOperator for why the mention target does NOT use this.
		Users: b.UserDirectory(),
	})
}
