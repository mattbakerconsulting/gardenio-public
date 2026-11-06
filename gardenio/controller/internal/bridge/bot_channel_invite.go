package bridge

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// handleMemberJoinedChannel reacts to member_joined_channel events. When the
// joining member is the bot itself AND GardenerOnInvite is enabled, it spawns a
// gardener agent bound to the channel so users can pull a gardener into a
// (private) collaboration channel just by inviting the bot.
//
// This ties channel collaboration to gardeners: invite the gardenio app to a
// channel → a gardener shows up and starts coordinating work there.
func (b *Bot) handleMemberJoinedChannel(ctx context.Context, ev *slackevents.MemberJoinedChannelEvent) {
	// Only act when the bot itself was added — ignore other members joining.
	if ev.User == "" || ev.User != b.botUserID {
		return
	}
	if !b.gardenerOnInvite {
		b.logger.Debug("bot invited to channel but GARDENER_ON_INVITE disabled",
			"channel", ev.Channel, "inviter", ev.Inviter)
		return
	}

	// Stamp the operator (the human who invited the bot) for downstream
	// daemon calls so the gardener bead records who pulled it in.
	if ev.Inviter != "" {
		ctx = beadsapi.WithOperator(ctx, b.operatorFromSlackUser(ctx, ev.Inviter))
	}

	// Race guard: a single join should spawn at most one gardener even if the
	// event is redelivered. Key on the channel (one gardener per channel).
	spawnKey := "invite:" + ev.Channel
	b.mu.Lock()
	if b.spawnInFlight[spawnKey] {
		b.mu.Unlock()
		return
	}
	b.spawnInFlight[spawnKey] = true
	b.mu.Unlock()
	// The in-flight flag is intentionally not cleared on a short timer here:
	// once a gardener is bound to the channel the SetThreadAgent check below
	// is the durable guard. We clear it only on spawn failure so a retry can
	// succeed.

	clearInFlight := func() {
		b.mu.Lock()
		delete(b.spawnInFlight, spawnKey)
		b.mu.Unlock()
	}

	// If a gardener is already bound to this channel, do nothing.
	if b.state != nil {
		if existing := b.getThreadSpawnedAgent(ev.Channel, ""); existing != "" {
			b.logger.Info("gardener already bound to channel, skipping invite spawn",
				"channel", ev.Channel, "agent", existing)
			return
		}
	}

	// Resolve the project for this channel. Prefer an explicit channel→project
	// mapping; fall back to the DM/default resolution used elsewhere.
	project := b.projectFromChannel(ctx, ev.Channel)
	if project == "" {
		project = b.resolveDMProject(ctx, ev.Channel, ev.Inviter, "gardener-on-invite")
	}
	if project == "" {
		b.logger.Warn("gardener-on-invite: could not resolve project for channel — skipping",
			"channel", ev.Channel)
		clearInFlight()
		_, _, _ = b.api.PostMessageContext(ctx, ev.Channel,
			slack.MsgOptionText(":warning: I was invited but couldn't tell which project this channel belongs to. "+
				"Map this channel to a project (set `slack_channels` on the project bead) and re-invite me, or @mention me with a task.", false))
		return
	}

	channelName := b.channelDisplayName(ctx, ev.Channel)
	prompt := fmt.Sprintf(
		"You are a gardener in the Slack channel #%s (%s). You were invited here to coordinate collaboration for project %q. "+
			"Introduce yourself briefly with `gb slack reply`, read the channel with `gb slack channel %s`, and help the members: "+
			"answer questions, spawn crew for concrete work, and keep the channel updated with `gb squawk`. "+
			"Stay alive for follow-ups; only `gb stop` when explicitly dismissed.",
		channelName, ev.Channel, project, ev.Channel)

	agentName := b.generateAgentName(project)
	// A channel invite has no prior agent to continue, so once gardener is a
	// privileged role the daemon denies this create as escalatable. Send
	// X-Escalate so the denial becomes an escalation awaiting approval instead of
	// a bare 403 that drops the invite (gd-VuYwGGYLLT).
	beadID, spawnErr := b.daemon.SpawnAgent(beadsapi.WithEscalate(ctx), agentName, project, "", "gardener", prompt)
	var escalated *beadsapi.EscalationCreated
	if errors.As(spawnErr, &escalated) {
		b.logger.Info("gardener-on-invite: spawn needs operator approval",
			"project", project, "channel", ev.Channel, "escalation", escalated.EscalationID)
		clearInFlight()
		_, _, _ = b.api.PostMessageContext(ctx, ev.Channel,
			slack.MsgOptionText(fmt.Sprintf("A gardener for this channel needs operator approval first (escalation `%s`). "+
				"Once it is approved the gardener starts, but it will not be bound to this channel automatically: re-invite me or @mention me after that.", escalated.EscalationID), false))
		return
	}
	if spawnErr != nil {
		b.logger.Error("gardener-on-invite: spawn failed",
			"project", project, "channel", ev.Channel, "error", spawnErr)
		clearInFlight()
		return
	}

	// Bind the channel to the gardener so channel messages route to it. We bind
	// at the channel level (empty thread_ts) so any top-level message in the
	// channel reaches the gardener.
	fields := map[string]string{
		"slack_thread_channel": ev.Channel,
		"spawn_source":         "channel-invite",
		"mode":                 "gardener",
	}
	if ev.Inviter != "" {
		fields["slack_user_id"] = ev.Inviter
		fields["operated_by"] = b.operatorFromSlackUser(ctx, ev.Inviter)
	}
	_ = b.daemon.UpdateBeadFields(ctx, beadID, fields)

	if b.state != nil {
		_ = b.state.SetThreadAgent(ev.Channel, "", agentName)
		_ = b.state.SetListenThread(ev.Channel, "")
	}

	b.logger.Info("gardener-on-invite: gardener spawned",
		"agent", agentName, "project", project, "channel", ev.Channel, "inviter", ev.Inviter)

	_, _, _ = b.api.PostMessageContext(ctx, ev.Channel,
		slack.MsgOptionText(fmt.Sprintf(":robot_face: Gardener *%s* is joining to help coordinate work here.", agentName), false))
}

// channelDisplayName best-effort resolves a channel ID to its name for use in
// prompts and messages. Falls back to the raw ID on error.
func (b *Bot) channelDisplayName(ctx context.Context, channelID string) string {
	info, err := b.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
	if err != nil || info == nil {
		return channelID
	}
	if name := strings.TrimSpace(info.Name); name != "" {
		return name
	}
	return channelID
}
