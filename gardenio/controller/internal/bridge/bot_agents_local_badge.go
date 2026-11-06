package bridge

import (
	"context"
	"fmt"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/podmanager"
)

// A laptop agent (`gb runner join`, execution_target=laptop) runs on its
// owner's machine under the owner's own Claude login, so Slack marks it: the
// agent card's header says whose machine it is on, and its cost line and the
// roster's say "owner-billed" instead of a dollar figure the fleet does not
// pay (local-agents spec §6, §9).

// localBadge is what the agent card shows for a laptop agent.
type localBadge struct {
	Owner    string // operator id (oidc:<email>), shown without its prefix
	Hostname string
}

// laptopAgentFields reports whether an agent bead's fields mark it as running
// on a laptop runner (isLaptopAgent, for a bead's fields alone).
func laptopAgentFields(fields map[string]string) bool {
	return podmanager.IsLaptopTarget(fields["execution_target"])
}

// localBadgeFor returns the badge for an agent bead's fields, or nil for an
// agent that does not run on a laptop. runnerHostname is the hostname of the
// runner its runtime_id names; empty when that runner is not registered.
func localBadgeFor(fields map[string]string, runnerHostname string) *localBadge {
	if !laptopAgentFields(fields) {
		return nil
	}
	host := runnerHostname
	if host == "" {
		host = "an unregistered machine"
	}
	return &localBadge{Owner: fields["operated_by"], Hostname: host}
}

// headerSuffix is the badge's segment of the agent card's header line.
func (lb *localBadge) headerSuffix(th Theme) string {
	return fmt.Sprintf(" · %s local, running as %s on %s",
		th.Semantic(SemLocal), localOwnerLabel(lb.Owner), escapeSlackMrkdwn(lb.Hostname))
}

// localOwnerLabel renders the owner, or "its owner" when the agent bead has
// no operated_by (a runner joined before owners were stamped).
func localOwnerLabel(owner string) string {
	if owner == "" {
		return "its owner"
	}
	return formatOwnerMention(owner)
}

// rosterLineSuffix is the /roster segment for a laptop agent, "" for any
// other. hostnames maps runner bead id to hostname (runnerHostnames).
func rosterLineSuffix(th Theme, a beadsapi.AgentBead, hostnames map[string]string) string {
	if !laptopAgentFields(a.Metadata) {
		return ""
	}
	host := hostnames[a.Metadata["runtime_id"]]
	if host == "" {
		host = "unknown host"
	}
	return fmt.Sprintf(" · %s local (%s on %s) · owner-billed",
		th.Semantic(SemLocal), localOwnerLabel(a.Metadata["operated_by"]), escapeSlackMrkdwn(host))
}

// localBadgeForAgent resolves the badge for an agent bead's fields, looking
// up its runner's hostname only when the agent runs on a laptop.
func (b *Bot) localBadgeForAgent(ctx context.Context, fields map[string]string) *localBadge {
	if !laptopAgentFields(fields) {
		return nil
	}
	return localBadgeFor(fields, b.runnerHostnames(ctx)[fields["runtime_id"]])
}

// NotifyAgentPaused posts the paused message in a laptop agent's thread when
// the stale reaper pauses it because its runner went away (local-agents D5,
// spec §9). The agent card re-renders through NotifyAgentState.
func (b *Bot) NotifyAgentPaused(ctx context.Context, bead BeadEvent) {
	agent := bead.Fields["agent"]
	if agent == "" {
		agent = bead.Title
	}
	agent = extractAgentName(agent)
	if agent == "" {
		return
	}
	host := b.runnerHostnames(ctx)[bead.Fields["runtime_id"]]
	if host == "" {
		host = "its laptop"
	}
	// Theme the glyph by the channel routeAgentThreadMessage posts to: the
	// bound thread's, else the agent card's.
	channel, _ := b.resolveAgentThread(ctx, agent)
	if channel == "" {
		channel = b.resolveChannel(agent)
	}
	b.routeAgentThreadMessage(ctx, agent, b.agentEventLand(bead, agent), conversation.Message{
		Text: pausedMessage(b.themeFor(ctx, channel), agent, host),
		// The bridge says it, not the agent: its laptop is offline.
		Author: agentPrincipal(NudgeSender),
	})
}

// pausedMessage is the thread message for a laptop agent paused because host
// is offline, led by th's paused glyph.
func pausedMessage(th Theme, agent, host string) string {
	return fmt.Sprintf("%s %s is paused: %s is offline. Work is held until it is back; `/kill %s` releases it.",
		th.Semantic(SemPaused), agent, escapeSlackMrkdwn(host), agent)
}
