package bridge

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/deathreport"
	gardeniometrics "gardenio/controller/internal/metrics"
)

// deathReportAuthor is the comment author stamped on the forensic breadcrumb
// the auto-forensics pipeline attaches to a dead agent bead.
const deathReportAuthor = "gardenio-forensics"

// Death-storm volume gate (epic ka-0000005447, task ka-0000004746). When many
// agents die involuntarily in a short window — an account-exhaustion storm, a
// node drain, a bad rollout — posting one warning per death floods Slack with
// noise exactly when an operator most needs a clear signal. The gate posts up
// to deathStormBurst individual reports per deathStormWindow; once that burst
// is spent the remaining deaths in the window are counted, not posted, and a
// single "N more agents died" roll-up is emitted when the window closes.
//
// The gate is FLEET-WIDE (not per-project) on purpose: a death storm is a
// systemic event (shared account, shared node, shared release) whose blast
// radius crosses projects, and the operator reads one channel. The forensic
// breadcrumb is ALWAYS attached to the dead bead regardless of the gate — only
// the Slack post is throttled, so no forensic signal is ever lost.
const (
	deathStormWindow = 5 * time.Minute
	deathStormBurst  = 5
)

// NotifyAgentDeath posts a forensic "why did this agent die" report to Slack
// and attaches the same one-line cause as a comment on the dead agent bead.
//
// It is the terminal stage of the auto-forensics pipeline (epic ka-0000005447):
// the upstream stages detect an involuntary death off the close_reason
// `_involuntary` marker, map the dead bead to its S3 recording, and summarize
// the last turns. This stage delivers that signal to humans — a warning that
// NAMES the agent and states a ONE-LINE cause — and leaves a durable breadcrumb
// on the bead itself so the report survives Slack retention.
//
// Volume is gated against death storms: the bead comment is always written, but
// the Slack post is suppressed past a burst budget per window (see the
// deathStorm* constants) and replaced by a single roll-up.
func (b *Bot) NotifyAgentDeath(ctx context.Context, bead BeadEvent) error {
	agent := bead.Assignee
	if agent == "" {
		if n := bead.Fields["agent"]; n != "" {
			agent = n
		}
	}
	if agent == "" {
		agent = bead.Title
	}
	// Short name: the bridge's per-agent caches are keyed by it.
	agent = extractAgentName(agent)
	name := agent
	if name == "" {
		name = bead.ID
	}
	displayName := b.agentDisplayName(name)

	closeReason := bead.Fields["close_reason"]
	cause := deathCause(closeReason, bead.Fields["controller_restart_reason"], bead.Fields)

	// Always attach the forensic breadcrumb to the dead agent bead. This is the
	// durable record — it outlives Slack retention and travels with the bead in
	// the viewer. Do it before the volume gate so a storm never drops the
	// on-bead record, only the Slack noise.
	b.attachDeathReport(ctx, bead.ID, displayName, cause)
	land := b.agentEventLand(bead, agent)

	// Death-storm volume gate: only the first burst of reports per window posts
	// individually; the rest are counted. Crossing into a new window flushes the
	// previous window's suppressed count as a single roll-up so the operator sees
	// the true death toll without per-death spam.
	admit, rollup, held := b.admitDeathReport(time.Now(), heldDeath{land: land, subject: bead.ID, name: displayName})
	if rollup > 0 {
		b.postDeathStormRollup(ctx, rollup, held)
	}
	if !admit {
		b.logger.Info("suppressed agent-death report (death-storm gate)",
			"agent", displayName, "bead", bead.ID, "close_reason", closeReason)
		return nil
	}

	return b.postDeathReport(ctx, agent, land, displayName, bead.ID, cause, closeReason)
}

// admitDeathReport is the death-storm volume gate's admission decision. Given
// the current time it reports whether this death's report should post
// individually (admit) and how many deaths from a just-closed window must now be
// rolled up (rollup > 0 only when this call opened a fresh window that
// superseded a prior window with held-back deaths), with the held-back deaths
// it knows by name (at most deathStormHeldMax). d is this death, held back when
// it is not admitted.
//
// Semantics: within a deathStormWindow, the first deathStormBurst deaths post
// individually; the rest are counted in deathReportSuppressed. The first death
// that arrives after the window has elapsed flushes the prior window's
// suppressed count as the rollup and starts a fresh window (this death counting
// as the new window's first admitted post).
func (b *Bot) admitDeathReport(now time.Time, d heldDeath) (admit bool, rollup int, held []heldDeath) {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Open the first window, or roll over into a new one when the current window
	// has elapsed. On rollover, surface any suppressed deaths from the window we
	// are leaving.
	if b.deathReportWindowStart.IsZero() || now.Sub(b.deathReportWindowStart) >= deathStormWindow {
		rollup, held = b.deathReportSuppressed, b.deathReportHeld
		b.deathReportWindowStart = now
		b.deathReportPosted = 0
		b.deathReportSuppressed = 0
		b.deathReportHeld = nil
	}

	if b.deathReportPosted < deathStormBurst {
		b.deathReportPosted++
		return true, rollup, held
	}
	b.deathReportSuppressed++
	if len(b.deathReportHeld) < deathStormHeldMax {
		b.deathReportHeld = append(b.deathReportHeld, d)
	}
	return false, rollup, held
}

// postDeathStormRollup posts a single summary line accounting for deaths that
// were suppressed during a death storm, so the operator sees the true toll.
// A storm is fleet-wide, so the Slack roll-up belongs to no land: it posts
// straight to the Slack surface with no message bead (ops telemetry, design
// spec section 2.6). Each land with held-back deaths also gets its own
// roll-up in the Commons (rollUpDeathsInCommons), counting only its deaths.
func (b *Bot) postDeathStormRollup(ctx context.Context, suppressed int, held []heldDeath) {
	defer b.rollUpDeathsInCommons(ctx, held)
	text := fmt.Sprintf(":warning: *Death storm:* %d more agent(s) died in the last %s (individual reports suppressed to protect the channel). Forensic breadcrumbs were still attached to each dead bead.",
		suppressed, deathStormWindow)
	if err := b.routeToSlack(ctx, "", b.channel, "", conversation.Message{
		Kind:   conversation.KindNotice,
		Text:   fmt.Sprintf("Death storm: %d more agents died (reports suppressed)", suppressed),
		Detail: text,
		Rich:   true,
	}); err != nil {
		b.logger.Warn("failed to post death-storm rollup to Slack", "suppressed", suppressed, "error", err)
		return
	}
	b.logger.Info("posted death-storm rollup to Slack", "suppressed", suppressed)
}

// postDeathReport posts the single-agent death warning to the agent's resolved
// Slack channel (threaded under the agent's card / origin thread when possible)
// as a notice message bead in land, through the conversation Router.
func (b *Bot) postDeathReport(ctx context.Context, agent, land, displayName, beadID, cause, closeReason string) error {
	// A death always needs a human, so it always posts — but the SIMPLE default
	// collapses it to one terse line (matching the crash-notify verbosity seam,
	// gb-ux epic ka-0000005059 §5). Vibe mode restores the rich section block.
	text := fmt.Sprintf(":warning: *Agent died: %s*\n> Cause: %s", displayName, cause)

	targetChannel, threadTS := b.resolveThreadedChannel(ctx, agent)
	ch, head, subject := b.agentNoticeTarget(ctx, agent, land, targetChannel, threadTS)
	if subject == "" {
		subject = beadID
	}

	m := conversation.Message{
		Kind:   conversation.KindNotice,
		Text:   fmt.Sprintf("Agent died: %s — %s", displayName, cause),
		Detail: text,
		Author: agentPrincipal(deathReportAuthor),
		Fields: map[string]string{noticeField: noticeDeath, noticeAgentField: displayName, noticeCauseField: cause,
			noticeSubjectField: subject},
	}
	if b.vibeModeForAgent(ctx, agent) {
		m.Rich = true
		m.Footnote = fmt.Sprintf("Forensics on bead `%s`", beadID)
	}

	if err := b.routeAgentNotice(ctx, ch, head, threadTS, m); err != nil {
		return fmt.Errorf("post agent death to Slack: %w", err)
	}

	b.logger.Info("posted agent-death report to Slack",
		"agent", displayName, "bead", beadID, "channel", targetChannel,
		"close_reason", closeReason)
	return nil
}

// attachDeathReport writes the one-line forensic cause as a comment on the dead
// agent bead. Best-effort: a failure is logged, never fatal — the Slack post is
// the primary human signal, and a daemon blip must not swallow the whole
// notification path.
func (b *Bot) attachDeathReport(ctx context.Context, beadID, displayName, cause string) {
	if beadID == "" {
		return
	}
	comment := fmt.Sprintf(":warning: Auto-forensics: agent `%s` died involuntarily.\nCause: %s", displayName, cause)
	if err := b.daemon.AddComment(ctx, beadID, deathReportAuthor, comment); err != nil {
		b.logger.Warn("failed to attach death report to dead agent bead",
			"bead", beadID, "agent", displayName, "error", err)
	}
}

// deathCause renders the one-line cause for a dead agent from the best available
// EVIDENCE, in strict precedence:
//
//  1. A CONTROLLER-recorded reason (controllerRestartReason). When the controller
//     itself deleted the pod (account rotation, stranded-account recovery) it
//     stamps WHY before deleting — that action record is ground truth and wins
//     (ka-0000001482 D3). Observed 2026-07-27: the account-rotation sweep deleted
//     5 working pods and the report blamed "node drain, rollout, or Spot reclaim",
//     all three false on an all-on-demand cluster.
//  2. A CREDENTIAL-failure signature in the bead's fields (error_category=
//     unauthorized / never-authenticated token shape). The generic SIGTERM
//     teardown closes the bead context_cancelled_involuntary regardless of the
//     real cause; when the fields show the credential was rejected, THAT is the
//     honest cause, and asserting an infra interruption instead would send triage
//     to the wrong place (ka-0000002985 DEFECT 1). Derived by the shared
//     deathreport.AuthFailureCause so the Slack notice and the forensic report
//     name the same cause from the same evidence.
//  3. The close_reason's own family, whose context_cancelled summary now honestly
//     says the interruption source was NOT determined rather than naming one.
//
// For (1) the close_reason still supplies the mid-turn/orphaned-task framing, so
// the two are composed: the operator learns both that the controller did it and
// that a turn was interrupted.
func deathCause(closeReason, controllerRestartReason string, fields map[string]string) string {
	if reason := strings.TrimSpace(controllerRestartReason); reason != "" {
		if _, involuntary := gardeniometrics.InvoluntaryDeathFamily(closeReason); involuntary {
			return reason + " — the agent was mid-turn and did not `gb done`, so its claimed task was returned to the queue"
		}
		return reason
	}

	// Evidence beats the generic guess: a credential-failure signature in the
	// fields overrides a generic context_cancelled close_reason. We only let it
	// override the generic teardown families — a specific close_reason family
	// (stop-gate, upstream_api_error, rate_limit_livelock) already carries an
	// evidenced cause and is left alone.
	if authCause := deathreport.AuthFailureCause(fields); authCause != "" {
		if family, involuntary := gardeniometrics.InvoluntaryDeathFamily(closeReason); !involuntary ||
			family == gardeniometrics.FamilyContextCancelled ||
			family == gardeniometrics.FamilyContextCancelledUnclaimed ||
			family == gardeniometrics.FamilyContextCancelledClaimLookupFailed {
			return authCause
		}
	}

	return involuntaryDeathCause(closeReason)
}

// involuntaryDeathCause renders a single human-readable sentence explaining why
// an agent died, derived from the close_reason's involuntary-death family. The
// family classification is the SHARED predicate the metric emitter uses
// (gardeniometrics.InvoluntaryDeathFamily) so the Slack cause and the
// gardenio_agent_involuntary_deaths_total series can never disagree about what
// counts as a death or which family it is.
func involuntaryDeathCause(closeReason string) string {
	family, ok := gardeniometrics.InvoluntaryDeathFamily(closeReason)
	if !ok {
		// Not a classified involuntary death — fall back to the raw reason so the
		// caller still emits *something* actionable rather than a blank cause.
		if closeReason == "" {
			return "unknown (no close_reason recorded)"
		}
		return closeReason
	}
	switch family {
	case gardeniometrics.FamilyStopGateCooldown:
		return "stop-gate cooldown killed the agent mid-work — it ended two turns inside one cooldown window and the debounce closed the bead. NOT a parked/awaiting-decision agent (the yield fast-path already exempts those), and NOT a hang: quiet turn-ends are ordinary for a working agent"
	case gardeniometrics.FamilyStopGateEscapeHatch:
		return "stop-gate escape-hatch closed the agent mid-work after repeated turn-ends without `gb done`"
	case gardeniometrics.FamilyStopGateRateLimited:
		return "stop-gate closed the agent mid-work while it was rate-limited"
	case gardeniometrics.FamilyContextCancelled, gardeniometrics.FamilyContextCancelledUnclaimed,
		gardeniometrics.FamilyContextCancelledClaimLookupFailed:
		// The interruption source is not recorded on a generic teardown, and no
		// credential-failure signature was present (deathCause checks that first),
		// so state the honesty rather than asserting Spot/node-drain as fact
		// (ka-0000002985 DEFECT 1). Defer to the shared deathreport summary so the
		// Slack notice and the forensic report read identically.
		return deathreport.ClassifyWithFields(closeReason, nil).Summary
	default: // FamilyOther
		return "involuntary mid-work death: " + closeReason
	}
}
