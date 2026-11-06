package bridge

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// threadAgentNameRE matches the agent-name format used for Slack
// thread-bound agents: `thread-<parent_ts_seconds>-<parent_ts_micros>`.
// The parent ts can be reconstructed losslessly as `<seconds>.<micros>`.
var threadAgentNameRE = regexp.MustCompile(`^thread-(\d+)-(\d+)$`)

// deriveThreadTSFromAgentName reconstructs the parent Slack thread timestamp
// from a thread-bound agent's name. Returns "" if the name doesn't match the
// `thread-<digits>-<digits>` format. Used as a fallback when the thread
// binding on the agent bead is unavailable (closed, pruned, or bridge
// restarted with a cold cache).
func deriveThreadTSFromAgentName(agent string) string {
	m := threadAgentNameRE.FindStringSubmatch(extractAgentName(agent))
	if m == nil {
		return ""
	}
	return m[1] + "." + m[2]
}

// switchbackAlertCooldown is the minimum interval between repeated
// "account switchback" alerts for the same project + account pair. A primary
// that 429s on per-minute RPM gets failed over and switched back repeatedly
// (ka-0000003255); without this throttle every flap posts to Slack, producing
// hundreds of identical messages. The underlying flap is mitigated separately
// by the RPM hold-down in authFailover; this just keeps the channel readable.
const switchbackAlertCooldown = 30 * time.Minute

// resolveThreadedChannel determines the target channel and optional thread TS
// for posting a notification about an agent. It checks thread-bound agents
// first, then agent card threading, falling back to the resolved channel.
func (b *Bot) resolveThreadedChannel(ctx context.Context, agent string) (channel, threadTS string) {
	// Thread-bound agent: post in the originating thread.
	if ch, ts := b.resolveAgentThread(ctx, agent); ch != "" && ts != "" {
		return ch, ts
	}

	// Name-encoded fallback: agents spawned from a Slack thread are named
	// `thread-<seconds>-<micros>` where the parent ts reconstructs as
	// `<seconds>.<micros>`. When the thread binding can't be resolved (bead
	// closed, bridge restart with cold cache), derive the thread_ts from
	// the name so notifications still land inside the originating thread
	// rather than spamming the top level of the channel.
	if ts := deriveThreadTSFromAgentName(agent); ts != "" {
		return b.resolveChannel(agent), ts
	}

	// Agent card threading: ensure the card exists (resurrect from state or
	// create) so crash/jack notifications stay in-thread even after the hot
	// cache was pruned or bridge restarted.
	//
	// Skip for thread-bound agents: if resolveAgentThread failed transiently
	// (HTTP error), we must not create a stray top-level card. The cached
	// agentThreadBound flag (set by NotifyAgentSpawn/NotifyAgentState from
	// bead fields) catches this without an HTTP call.
	if b.agentThreadingEnabled() && agent != "" {
		b.mu.Lock()
		threadBound := b.isThreadBound(extractAgentName(agent))
		b.mu.Unlock()
		if threadBound {
			// Thread-bound but resolveAgentThread failed — post to channel
			// without threading rather than creating a stray card.
			return b.resolveChannel(agent), ""
		}
		// A known card wins over resolveChannel: if routing changed after the
		// card was posted, the current route is a channel where this card does
		// not exist, and a thread reply there would be orphaned.
		if cardCh, cardTS, ok := b.AgentCardLocation(agent); ok {
			return cardCh, cardTS
		}
		ch := b.resolveChannel(agent)
		if cardTS, err := b.ensureAgentCard(ctx, agent, ch); err == nil && cardTS != "" {
			return ch, cardTS
		}
	}

	// Fallback: post to channel without threading.
	return b.resolveChannel(agent), ""
}

// NotifyAgentCrash posts a crash alert to the agent's resolved Slack channel
// through the conversation Router.
// For thread-bound agents, the crash is posted as a reply in the bound thread.
func (b *Bot) NotifyAgentCrash(ctx context.Context, bead BeadEvent) error {
	agent := bead.Assignee
	if agent == "" {
		agent = bead.Title
	}
	// The bridge's per-agent caches (cards, spawn channel, project/role) are
	// keyed by the short name; a full project/role/name assignee misses them.
	agent = extractAgentName(agent)
	name := agent
	if name == "" {
		name = bead.ID
	}
	displayName := b.agentDisplayName(name)

	targetChannel, threadTS := b.resolveThreadedChannel(ctx, agent)
	th := b.themeFor(ctx, targetChannel)
	land := b.agentEventLand(bead, agent)
	ch, head, subject := b.agentNoticeTarget(ctx, agent, land, targetChannel, threadTS)
	if subject == "" {
		subject = bead.ID
	}

	text := fmt.Sprintf("%s *Agent crashed: %s*", th.Semantic(SemWarn), displayName)

	// Add reason from fields.
	reason := bead.Fields["agent_state"]
	podPhase := bead.Fields["pod_phase"]
	podName := bead.Fields["pod_name"]
	if podPhase == "failed" && reason != "failed" {
		text += fmt.Sprintf("\n> Pod phase: `%s`", podPhase)
	}
	if podName != "" {
		text += fmt.Sprintf("\n> Pod: `%s`", podName)
	}

	// Verbosity seam (gb-ux epic ka-0000005059, plan §5): a crash always posts
	// — it needs a human — but the SIMPLE default collapses it to a single
	// terse line with no rich block. Vibe mode restores today's full section +
	// context blocks. Either way it is a notice message bead in the agent's
	// land, projected through the conversation Router.
	m := conversation.Message{
		Kind:   conversation.KindNotice,
		Text:   fmt.Sprintf("Agent crashed: %s", displayName),
		Detail: text,
		Fields: map[string]string{noticeField: noticeCrash, noticeAgentField: displayName,
			noticePodField: podName, noticePodPhaseField: podPhase, noticeSubjectField: subject},
	}
	if b.vibeModeForAgent(ctx, agent) {
		m.Rich = true
		m.Footnote = fmt.Sprintf("Agent: %s", displayName)
	}

	err := b.routeAgentNotice(ctx, ch, head, threadTS, m)
	if err != nil {
		return fmt.Errorf("post agent crash to Slack: %w", err)
	}

	b.logger.Info("posted agent crash to Slack",
		"agent", name, "bead", bead.ID, "channel", targetChannel)
	return nil
}

// NotifyJackOn posts a jack-raised alert to Slack.
func (b *Bot) NotifyJackOn(ctx context.Context, bead BeadEvent) error {
	target := bead.Fields["target"]
	agent := extractAgentName(bead.Assignee)
	ttl := bead.Fields["ttl"]
	reason := bead.Fields["reason"]

	// Verbosity seam (plan §5): jack raise/lower/expire is routine lifecycle
	// chatter — suppressed in the simple default, restored in vibe mode. Keyed
	// off the owning agent's operator.
	if !b.vibeModeForAgent(ctx, agent) {
		b.logger.Debug("suppressed jack-on notification (simple mode)",
			"jack", bead.ID, "target", target, "agent", agent)
		return nil
	}

	targetChannel, threadTS := b.resolveThreadedChannel(ctx, agent)
	th := b.themeFor(ctx, targetChannel)

	text := fmt.Sprintf("%s *Jack Raised: %s*\nTarget: `%s`", th.Semantic(SemInfo), beadTitle(bead.ID, bead.Title), target)
	if agent != "" {
		text += fmt.Sprintf("\nAgent: `%s`", agent)
	}
	if ttl != "" {
		text += fmt.Sprintf("\nTTL: %s", ttl)
	}
	if reason != "" {
		text += fmt.Sprintf("\n> %s", reason)
	}

	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(fmt.Sprintf("Jack raised: %s on %s", bead.ID, target), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS))
	}

	_, _, err := b.api.PostMessageContext(ctx, targetChannel, msgOpts...)
	if err != nil {
		return fmt.Errorf("post jack on to Slack: %w", err)
	}
	b.logger.Info("posted jack raised to Slack", "jack", bead.ID, "target", target)
	return nil
}

// NotifyJackOnBatch posts a batch summary of jack-raised events.
func (b *Bot) NotifyJackOnBatch(ctx context.Context, beads []BeadEvent) error {
	// Verbosity seam (plan §5): the batch summary is the lowest-signal jack
	// message ("N more raised"), posted to the default channel with no single
	// recipient. Suppress it in the simple default; only surface it when at
	// least one batched jack is owned by a vibe-mode operator.
	if !b.anyVibeAmongJacks(ctx, beads) {
		b.logger.Debug("suppressed jack batch notification (simple mode)",
			"count", len(beads))
		return nil
	}

	text := fmt.Sprintf("%s *%d additional jacks raised* (batch)\n", b.themeFor(ctx, b.channel).Semantic(SemInfo), len(beads))

	// Show first 5 individually.
	limit := 5
	if len(beads) < limit {
		limit = len(beads)
	}
	for _, bead := range beads[:limit] {
		target := bead.Fields["target"]
		line := fmt.Sprintf("• %s — target: `%s`", beadTitle(bead.ID, bead.Title), target)
		if bead.Assignee != "" {
			line += fmt.Sprintf(" (%s)", bead.Assignee)
		}
		text += line + "\n"
	}
	if len(beads) > limit {
		text += fmt.Sprintf("_...and %d more_\n", len(beads)-limit)
	}

	_, _, err := b.api.PostMessageContext(ctx, b.channel,
		slack.MsgOptionText(fmt.Sprintf("%d additional jacks raised", len(beads)), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("post jack batch to Slack: %w", err)
	}
	b.logger.Info("posted jack batch to Slack", "count", len(beads))
	return nil
}

// NotifyJackOff posts a jack-lowered alert to Slack.
func (b *Bot) NotifyJackOff(ctx context.Context, bead BeadEvent) error {
	target := bead.Fields["target"]
	agent := extractAgentName(bead.Assignee)
	reason := bead.Fields["reason"]

	// Verbosity seam (plan §5): routine jack lifecycle — suppressed in simple.
	if !b.vibeModeForAgent(ctx, agent) {
		b.logger.Debug("suppressed jack-off notification (simple mode)",
			"jack", bead.ID, "target", target, "agent", agent)
		return nil
	}

	targetChannel, threadTS := b.resolveThreadedChannel(ctx, agent)
	th := b.themeFor(ctx, targetChannel)

	text := fmt.Sprintf("%s *Jack Lowered: %s*\nTarget: `%s`", th.Semantic(SemOK), beadTitle(bead.ID, bead.Title), target)
	if agent != "" {
		text += fmt.Sprintf("\nAgent: `%s`", agent)
	}
	if reason != "" {
		text += fmt.Sprintf("\n> %s", reason)
	}

	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(fmt.Sprintf("Jack lowered: %s", bead.ID), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS))
	}

	_, _, err := b.api.PostMessageContext(ctx, targetChannel, msgOpts...)
	if err != nil {
		return fmt.Errorf("post jack off to Slack: %w", err)
	}
	b.logger.Info("posted jack lowered to Slack", "jack", bead.ID, "target", target)
	return nil
}

// resolveProjectOrDefaultChannel returns the Slack channel for a project,
// falling back to the bot's default channel.
func (b *Bot) resolveProjectOrDefaultChannel(project string) string {
	if project != "" {
		if ch := b.projectPrimaryChannel(project); ch != "" {
			return ch
		}
	}
	return b.channel
}

// NotifyAccountRotating posts a notification that an account is being rotated.
func (b *Bot) NotifyAccountRotating(ctx context.Context, evt RotationEvent) error {
	// Verbosity seam (plan §5): routine cred lifecycle — suppressed in the
	// simple default, full blocks in vibe mode.
	if !b.vibeModeForAccountEvent(ctx, evt) {
		b.logger.Debug("suppressed account-rotating notification (simple mode)",
			"project", evt.Project, "from", evt.FromAccount, "to", evt.ToAccount)
		return nil
	}
	channel := b.resolveProjectOrDefaultChannel(evt.Project)
	text := fmt.Sprintf("%s *Account Rotating*\n"+
		"Project: `%s`\n"+
		"Account `%s` exhausted — rotating to fallback `%s`",
		b.themeFor(ctx, channel).Glyph(LifecycleRestarted), evt.Project, evt.FromAccount, evt.ToAccount)

	_, _, err := b.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(fmt.Sprintf("Account rotating: %s → %s (%s)", evt.FromAccount, evt.ToAccount, evt.Project), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("post account rotating to Slack: %w", err)
	}
	b.logger.Info("posted account rotating to Slack",
		"project", evt.Project, "from", evt.FromAccount, "to", evt.ToAccount)
	return nil
}

// NotifyAccountRotated posts a notification that account rotation completed.
func (b *Bot) NotifyAccountRotated(ctx context.Context, evt RotationEvent) error {
	// Verbosity seam (plan §5): routine cred lifecycle — suppressed in simple.
	if !b.vibeModeForAccountEvent(ctx, evt) {
		b.logger.Debug("suppressed account-rotated notification (simple mode)",
			"project", evt.Project, "from", evt.FromAccount, "to", evt.ToAccount)
		return nil
	}
	channel := b.resolveProjectOrDefaultChannel(evt.Project)
	text := fmt.Sprintf("%s *Account Rotation Complete*\n"+
		"Project: `%s`\n"+
		"Rotated from `%s` to `%s`. Agents will be rebooted on new credentials.",
		b.themeFor(ctx, channel).Semantic(SemOK), evt.Project, evt.FromAccount, evt.ToAccount)

	_, _, err := b.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(fmt.Sprintf("Account rotation complete: %s → %s (%s)", evt.FromAccount, evt.ToAccount, evt.Project), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("post account rotated to Slack: %w", err)
	}
	b.logger.Info("posted account rotation complete to Slack",
		"project", evt.Project, "from", evt.FromAccount, "to", evt.ToAccount)
	return nil
}

// NotifyNoFallback routes a "no fallback account available" cred-exhaustion
// event through gardenio alerts instead of a bespoke Slack post (epic
// ka-0000003084, task ka-0000005341). It files a firing-alert bead keyed by
// project+account that dedups while open and auto-closes on account_recovered —
// see cred_exhaustion_alert.go. The old vibe-gated Slack block + in-memory
// cooldown are gone; the recovery ETA is folded into the alert bead body.
func (b *Bot) NotifyNoFallback(ctx context.Context, evt RotationEvent) error {
	return b.emitCredExhaustionAlert(ctx, credExhaustionKindNoFallback, evt)
}

// NotifySwitchback posts a notification that an account reverted to primary.
func (b *Bot) NotifySwitchback(ctx context.Context, evt RotationEvent) error {
	if b.isSwitchbackAlertSuppressed(evt.Project, evt.ToAccount) {
		b.logger.Debug("suppressing repeated switchback alert within cooldown",
			"project", evt.Project, "from", evt.FromAccount, "to", evt.ToAccount)
		return nil
	}

	// Verbosity seam (plan §5): routine cred lifecycle — suppressed in simple.
	if !b.vibeModeForAccountEvent(ctx, evt) {
		b.logger.Debug("suppressed switchback notification (simple mode)",
			"project", evt.Project, "from", evt.FromAccount, "to", evt.ToAccount)
		return nil
	}

	channel := b.resolveProjectOrDefaultChannel(evt.Project)
	text := fmt.Sprintf("%s *Account Switchback*\n"+
		"Project: `%s`\n"+
		"Primary account `%s` recovered — reverting from fallback `%s`.",
		b.themeFor(ctx, channel).Glyph(LifecycleContinued), evt.Project, evt.ToAccount, evt.FromAccount)

	_, _, err := b.api.PostMessageContext(ctx, channel,
		slack.MsgOptionText(fmt.Sprintf("Account switchback: %s → %s (%s)", evt.FromAccount, evt.ToAccount, evt.Project), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("post switchback to Slack: %w", err)
	}
	b.markSwitchbackAlertSent(evt.Project, evt.ToAccount)
	b.logger.Info("posted account switchback to Slack",
		"project", evt.Project, "from", evt.FromAccount, "to", evt.ToAccount)
	return nil
}

// switchbackAlertKey identifies a switchback alert by project + recovered
// account so a flapping primary on one project does not suppress a genuine
// switchback on another.
func switchbackAlertKey(project, toAccount string) string {
	return project + "\x00" + toAccount
}

// isSwitchbackAlertSuppressed reports whether a switchback alert for the given
// project + recovered account was already sent within switchbackAlertCooldown.
func (b *Bot) isSwitchbackAlertSuppressed(project, toAccount string) bool {
	if project == "" {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.switchbackAlertSent == nil {
		return false
	}
	last, ok := b.switchbackAlertSent[switchbackAlertKey(project, toAccount)]
	return ok && time.Since(last) < switchbackAlertCooldown
}

// markSwitchbackAlertSent records that a switchback alert was sent for a
// project + recovered account pair.
func (b *Bot) markSwitchbackAlertSent(project, toAccount string) {
	if project == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.switchbackAlertSent == nil {
		b.switchbackAlertSent = make(map[string]time.Time)
	}
	b.switchbackAlertSent[switchbackAlertKey(project, toAccount)] = time.Now()
}

// NotifyAccountThrottled posts a notification that the probe detected an account
// has been rate-limited by Anthropic (allowed→rejected transition). This is
// account-scoped — no project field — so it posts to the default channel.
func (b *Bot) NotifyAccountThrottled(ctx context.Context, evt RotationEvent) error {
	th := b.themeFor(ctx, b.channel)
	text := fmt.Sprintf("%s *Account Throttled (Anthropic Rate Limit)*\n"+
		"Account `%s` is now rate-limited by Anthropic.",
		th.Semantic(SemWarn), evt.FromAccount)
	if evt.ResetAt != "" {
		text += fmt.Sprintf("\nQuota resets at: `%s`", evt.ResetAt)
	}
	text += "\n_Probe-driven failover is triggering for all projects on this account._"

	// Verbosity seam (plan §5): account throttling is URGENT (a human should
	// know) — ALWAYS posts. Account-global events carry no agent link, so this
	// resolves simple by default and posts the terse 🚨 line; vibe keeps the
	// full block (reset ETA, failover note).
	fallbackText := fmt.Sprintf("%s Account throttled: %s (Anthropic rate limit)", th.Semantic(SemWarn), evt.FromAccount)
	msgOpts := []slack.MsgOption{slack.MsgOptionText(fallbackText, false)}
	if b.vibeModeForAccountEvent(ctx, evt) {
		msgOpts = append(msgOpts, slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		))
	}
	_, _, err := b.api.PostMessageContext(ctx, b.channel, msgOpts...)
	if err != nil {
		return fmt.Errorf("post account throttled to Slack: %w", err)
	}
	b.logger.Info("posted account throttled to Slack", "account", evt.FromAccount, "reset_at", evt.ResetAt)
	return nil
}

// NotifyAccountRecovered posts a notification that the probe detected an account
// has recovered from Anthropic rate-limiting (rejected→allowed transition).
// Auto-closes any open cred-exhaustion alert bead for the recovered account.
func (b *Bot) NotifyAccountRecovered(ctx context.Context, evt RotationEvent) error {
	// Auto-resolve cred-exhaustion alerts for this account (epic ka-0000003084,
	// task ka-0000005341) — the account_recovered counterpart to an
	// Alertmanager status=resolved. This runs in BOTH modes: it is alert-state
	// bookkeeping, not a Slack post, so the verbosity seam must not gate it.
	// Failure here is logged inside the helper, not fatal — recovery must not
	// wedge on a stuck close.
	if err := b.resolveCredExhaustionAlerts(ctx, evt.FromAccount); err != nil {
		b.logger.Warn("account-recovered: failed to resolve cred-exhaustion alerts",
			"account", evt.FromAccount, "error", err)
	}

	// Verbosity seam (plan §5): recovery is routine good-news lifecycle —
	// suppress the Slack post in the simple default (the state cleanup above
	// already happened). Vibe mode keeps the full block.
	if !b.vibeModeForAccountEvent(ctx, evt) {
		b.logger.Debug("suppressed account-recovered notification (simple mode)",
			"account", evt.FromAccount, "project", evt.Project)
		return nil
	}

	th := b.themeFor(ctx, b.channel)
	text := fmt.Sprintf("%s *Account Recovered (Anthropic Rate Limit Lifted)*\n"+
		"Account `%s` is no longer rate-limited by Anthropic.\n"+
		"_Probe-driven switchback is triggering for all affected projects._",
		th.Semantic(SemOK), evt.FromAccount)

	_, _, err := b.api.PostMessageContext(ctx, b.channel,
		slack.MsgOptionText(fmt.Sprintf("%s Account recovered: %s (rate limit lifted)", th.Semantic(SemOK), evt.FromAccount), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("post account recovered to Slack: %w", err)
	}
	b.logger.Info("posted account recovered to Slack", "account", evt.FromAccount)
	return nil
}

// NotifyAccountWarming posts an informational notification that an account has
// crossed its soft failover threshold. New sessions are being steered to the
// fallback — this is expected, graceful behavior.
func (b *Bot) NotifyAccountWarming(ctx context.Context, evt RotationEvent) error {
	// Verbosity seam (plan §5): soft-failover warming is expected, graceful
	// lifecycle — suppressed in simple.
	if !b.vibeModeForAccountEvent(ctx, evt) {
		b.logger.Debug("suppressed account-warming notification (simple mode)",
			"account", evt.FromAccount)
		return nil
	}

	utilPct := fmt.Sprintf("%.0f%%", evt.Utilization*100)
	threshPct := fmt.Sprintf("%.0f%%", evt.Threshold*100)

	text := fmt.Sprintf("%s *Account Warming: Soft Failover Active*\n"+
		"Account `%s` at %s utilization (threshold: %s)\n"+
		"_New sessions are steering to fallback. Existing sessions unaffected._",
		b.themeFor(ctx, b.channel).Semantic(SemWarn), evt.FromAccount, utilPct, threshPct)

	_, _, err := b.api.PostMessageContext(ctx, b.channel,
		slack.MsgOptionText(fmt.Sprintf("Account %s warming at %s — new sessions steering to fallback", evt.FromAccount, utilPct), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("post account warming to Slack: %w", err)
	}
	b.logger.Info("posted account warming to Slack",
		"account", evt.FromAccount, "utilization", evt.Utilization, "threshold", evt.Threshold)
	return nil
}

// NotifyAccountCooled posts a notification that an account's utilization has
// dropped back below the soft failover threshold. The account is restored for
// new session assignment.
func (b *Bot) NotifyAccountCooled(ctx context.Context, evt RotationEvent) error {
	// Verbosity seam (plan §5): cooldown/restore is routine lifecycle —
	// suppressed in simple.
	if !b.vibeModeForAccountEvent(ctx, evt) {
		b.logger.Debug("suppressed account-cooled notification (simple mode)",
			"account", evt.FromAccount)
		return nil
	}

	utilPct := fmt.Sprintf("%.0f%%", evt.Utilization*100)
	threshPct := fmt.Sprintf("%.0f%%", evt.Threshold*100)

	text := fmt.Sprintf("%s *Account Cooled: Restored as Primary*\n"+
		"Account `%s` utilization dropped to %s (threshold: %s)\n"+
		"_Restored for new session assignment._",
		b.themeFor(ctx, b.channel).Semantic(SemOK), evt.FromAccount, utilPct, threshPct)

	_, _, err := b.api.PostMessageContext(ctx, b.channel,
		slack.MsgOptionText(fmt.Sprintf("Account %s cooled to %s — restored for new sessions", evt.FromAccount, utilPct), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	)
	if err != nil {
		return fmt.Errorf("post account cooled to Slack: %w", err)
	}
	b.logger.Info("posted account cooled to Slack",
		"account", evt.FromAccount, "utilization", evt.Utilization, "threshold", evt.Threshold)
	return nil
}

// NotifyJackExpired posts a jack-expired warning to Slack.
func (b *Bot) NotifyJackExpired(ctx context.Context, bead BeadEvent) error {
	target := bead.Fields["target"]
	agent := extractAgentName(bead.Assignee)
	reason := bead.Fields["reason"]

	// Verbosity seam (plan §5): routine jack lifecycle — suppressed in simple.
	if !b.vibeModeForAgent(ctx, agent) {
		b.logger.Debug("suppressed jack-expired notification (simple mode)",
			"jack", bead.ID, "target", target, "agent", agent)
		return nil
	}

	targetChannel, threadTS := b.resolveThreadedChannel(ctx, agent)
	th := b.themeFor(ctx, targetChannel)

	text := fmt.Sprintf("%s *Jack Expired: %s*\nTarget: `%s`", th.Semantic(SemWarn), beadTitle(bead.ID, bead.Title), target)
	if agent != "" {
		text += fmt.Sprintf("\nAgent: `%s`", agent)
	}
	if reason != "" {
		text += fmt.Sprintf("\n> %s", reason)
	}
	text += fmt.Sprintf("\n_Review revert plan and close with_ `bd jack off %s`", bead.ID)

	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(fmt.Sprintf("Jack expired: %s on %s", bead.ID, target), false),
		slack.MsgOptionBlocks(
			slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", text, false, false),
				nil, nil),
		),
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS))
	}

	_, _, err := b.api.PostMessageContext(ctx, targetChannel, msgOpts...)
	if err != nil {
		return fmt.Errorf("post jack expired to Slack: %w", err)
	}
	b.logger.Info("posted jack expired to Slack", "jack", bead.ID, "target", target)
	return nil
}

// formatRecoveryETA returns a Slack-formatted recovery ETA line from an RFC3339
// reset_at timestamp, or "" if the timestamp is empty or unparseable.
func formatRecoveryETA(resetAt string) string {
	if resetAt == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, resetAt)
	if err != nil {
		return ""
	}
	remaining := time.Until(t).Truncate(time.Minute)
	if remaining < 0 {
		return fmt.Sprintf("Earliest recovery: `%s` _(should be imminent)_\n", resetAt)
	}
	return fmt.Sprintf("Earliest recovery: `%s` _(%s from now)_\n", resetAt, remaining)
}
