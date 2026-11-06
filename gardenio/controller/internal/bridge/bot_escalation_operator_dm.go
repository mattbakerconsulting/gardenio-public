package bridge

import (
	"log/slog"
)

// EscalationOperatorDM builds the escalation-creation operator DM notifier bound
// to this bot's DM sender, bead-link renderer and web-UI base URL (operator ask
// 2026-08-26).
//
// Returns nil for a nil bot, which disables the control — a slack-bridge running
// without a Slack app keeps working exactly as before.
//
// ROUTING CHOICE: a DM to the human named in the escalation's operated_by, in
// addition to (never instead of) the existing channel card. NotifyNewEscalation
// routes by the REQUESTING AGENT, so today an escalation addressed to a human
// reaches them only if they happen to watch the channel that agent posts in.
// The DM is the delivery guarantee; the card remains the canonical record that
// UpdateEscalationApproved / NotifyEscalationResult keep up to date. See the
// header of escalation_operator_dm.go for the measured motivation.
func (b *Bot) EscalationOperatorDM(daemon BeadClient, logger *slog.Logger) *EscalationOperatorDM {
	if b == nil {
		return nil
	}
	// Resolved lazily: the linker may be installed after this notifier is built.
	linkedIDs := func(email string) []string {
		if l := b.slackLinker(); l != nil {
			return l.SlackIDsForEmail(email)
		}
		return nil
	}
	return NewEscalationOperatorDM(EscalationOperatorDMConfig{
		Daemon:   daemon,
		Sender:   b,
		Logger:   logger,
		LinkBead: b.beadTitleLink,
		// Creator-side provider-handle resolution, plus the last-resort email ->
		// slack_id fallback for the DM TARGET; the target is resolved first from
		// the operators alias table (see operatorSlackIndex).
		Users:          b.UserDirectory(),
		LinkedSlackIDs: linkedIDs,
		ReviewURL:      b.escalationReviewURL,
		RecordDM:       b.recordEscalationDM,
	})
}

// escalationReviewURL for the operator DM lives in bot_escalations.go — it is
// the single shared source of the escalation review-link shape (ka-0000005895),
// consumed here via b.escalationReviewURL in the ReviewURL config above.
