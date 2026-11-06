package bridge

import (
	"context"
	"fmt"
	"strings"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// /kill, /kill-thread, the card's Kill and Restart buttons and @gardenio
// kill/stop are the agent owner's off switch, held to the same rule as /runner
// revoke (gd-FebppYflAV, gd-jpzPamBtO3, gd-rkXElhEgNt). On a chart-default
// deploy the close reaches the daemon on the bridge's fleet token, which may
// close any agent, so the bridge checks that the Slack caller owns the agent
// before it acts. An admin is not let through here: a Slack interaction never
// carries gardenio-admin (slackRoleCeiling), so an admin kills someone else's
// agent with their own sign-in (gb agent kill --override-owner).

// agentOwners returns the operator ids ("oidc:<email>") that own agent bead a.
// A local agent is owned by its runner's owner (spec D6), read from the runner
// bead its runtime_id names; a runner that cannot be read owns nothing, so the
// check fails closed. A cluster agent is owned by its operated_by and by its
// spawned_by when that names a person rather than an agent or a trigger.
func (b *Bot) agentOwners(ctx context.Context, a *beadsapi.BeadDetail) []string {
	if isLaptopAgent(a) {
		rid := strings.TrimSpace(a.Fields["runtime_id"])
		if rid == "" {
			return nil
		}
		r, err := b.daemon.GetBead(ctx, rid)
		if err != nil || r == nil || r.Type != "runner" {
			b.logger.Warn("kill owner check: local agent's runner not readable", "agent", a.ID, "runtime_id", rid, "error", err)
			return nil
		}
		if o := personOperator(runnerOwner(r)); o != "" {
			return []string{o}
		}
		return nil
	}
	var owners []string
	for _, v := range []string{a.OperatedBy, a.OperatedByEmail, a.Fields["operated_by"], a.Fields["spawned_by"]} {
		if o := personOperator(v); o != "" {
			owners = append(owners, o)
		}
	}
	return owners
}

// personOperator normalizes v to "oidc:<email>" when it names a person (an
// oidc: id or a bare email), else "". Agent names, trigger:, slack: and fleet:
// ids name no person.
func personOperator(v string) string {
	v = strings.TrimSpace(v)
	email := strings.TrimPrefix(v, "oidc:")
	if email == "" || strings.Contains(email, ":") || !strings.Contains(email, "@") {
		return ""
	}
	return "oidc:" + email
}

// callerMayKillAgent reports whether the Slack caller owns agent bead a, and
// when not, tells them so in an ephemeral and logs the refusal.
func (b *Bot) callerMayKillAgent(ctx context.Context, cmd slack.SlashCommand, a *beadsapi.BeadDetail) bool {
	return b.slackUserMayKillAgent(ctx, cmd.Command, cmd.ChannelID, "", cmd.UserID, a)
}

// slackUserMayKillAgent is callerMayKillAgent for any Slack surface: a slash
// command, a card button or an @mention (gd-rkXElhEgNt). via names the
// surface in the log; a set threadTS posts the refusal in that thread.
func (b *Bot) slackUserMayKillAgent(ctx context.Context, via, channelID, threadTS, userID string, a *beadsapi.BeadDetail) bool {
	caller := b.runnerCaller(ctx, userID)
	owners := b.agentOwners(ctx, a)
	if strings.HasPrefix(caller, "oidc:") {
		for _, o := range owners {
			if strings.EqualFold(caller, o) {
				return true
			}
		}
	}
	name := a.Fields["agent"]
	if name == "" {
		name = a.Title
	}
	b.logger.Info("kill refused: caller does not own the agent", "command", via, "user", userID, "caller", caller, "agent", a.ID, "owners", owners)
	if b.api == nil {
		return false
	}
	th := b.themeFor(ctx, channelID)
	who := "has no owner I can match"
	if len(owners) > 0 {
		who = "is owned by " + owners[0]
	}
	opts := []slack.MsgOption{slack.MsgOptionText(fmt.Sprintf(th.Semantic(SemDenied)+" Only an agent's owner can kill it from Slack, and *%s* %s. An admin can kill it with their own sign-in: `gb agent kill %s --override-owner`.", name, who, name), false)}
	if threadTS != "" {
		opts = append(opts, slack.MsgOptionTS(threadTS))
	}
	_, _ = b.api.PostEphemeral(channelID, userID, opts...)
	return false
}

// slackKillTarget finds agentName's live bead for a kill or restart asked from
// Slack, and checks that userID owns it. It returns nil, having told the user
// why, when the agent is not found or is not theirs. The caller holds the
// kill to the returned bead, so a same-named successor is not the one stopped.
func (b *Bot) slackKillTarget(ctx context.Context, via, agentName, channelID, threadTS, userID string) *beadsapi.BeadDetail {
	bead, err := b.daemon.FindAgentBead(ctx, extractAgentName(agentName))
	if err != nil || bead == nil {
		b.logger.Info("kill: agent bead not found", "command", via, "agent", agentName, "user", userID, "error", err)
		if b.api != nil {
			opts := []slack.MsgOption{slack.MsgOptionText(fmt.Sprintf(b.themeFor(ctx, channelID).Semantic(SemFail)+" Agent *%s* is not running.", extractAgentName(agentName)), false)}
			if threadTS != "" {
				opts = append(opts, slack.MsgOptionTS(threadTS))
			}
			_, _ = b.api.PostEphemeral(channelID, userID, opts...)
		}
		return nil
	}
	if !b.slackUserMayKillAgent(ctx, via, channelID, threadTS, userID, bead) {
		return nil
	}
	return bead
}
