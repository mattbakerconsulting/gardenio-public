package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
)

// This file holds the Slack channel routing rules for an agent's posts (its
// card, decisions, escalations) and GET /api/slack/route, which explains them
// for `gb slack route <agent|bead>` (gd-XahY2dfWYP). resolveChannel is
// explainChannelRoute(agent, false).Channel, so the explanation cannot drift
// from the path the bridge really takes.

// Route rule names, in resolveChannel's order of precedence.
const (
	RouteRuleRouter         = "router"                  // router override or agent pattern
	RouteRuleSpawnChannel   = "spawn_channel"           // explicit pin: /spawn, task or schedule channel
	RouteRuleRoleChannel    = "role_channel"            // project channel_roles entry for the agent's role
	RouteRuleInheritedSpawn = "inherited_spawn_channel" // spawn channel copied from a different-role spawner
	RouteRuleProjectPrimary = "project_primary"         // first channel on the project bead
	RouteRuleRouterDefault  = "router_default"          // router's default channel
	RouteRuleBotDefault     = "bot_default"             // the bridge's own default channel
)

// RouteCandidate is one routing rule as explainChannelRoute evaluated it.
type RouteCandidate struct {
	Rule        string `json:"rule"`
	Channel     string `json:"channel,omitempty"`
	ChannelName string `json:"channel_name,omitempty"`
	// Applies is true when the rule produced a channel; only the first rule
	// that applies wins.
	Applies bool   `json:"applies"`
	Won     bool   `json:"won,omitempty"`
	Detail  string `json:"detail"`
}

// ChannelRoute is the channel resolveChannel picks for an agent, the rule that
// picked it and the inputs it read from the routing caches.
type ChannelRoute struct {
	Agent   string `json:"agent"`
	Channel string `json:"channel"`
	Rule    string `json:"rule"`
	Project string `json:"project,omitempty"`
	Role    string `json:"role,omitempty"`
	// SpawnChannel is the cached spawn or schedule channel; InheritedFromRole
	// is the spawner's role when gb spawn copied it rather than pinned it.
	SpawnChannel      string           `json:"spawn_channel,omitempty"`
	InheritedFromRole string           `json:"inherited_from_role,omitempty"`
	Candidates        []RouteCandidate `json:"candidates"`
}

// explainChannelRoute walks the routing rules for agent (see resolveChannel for
// the precedence). With full false it stops at the winning rule, as the hot
// path needs; with full true it evaluates every rule so the caller can show
// the candidates that lost.
func (b *Bot) explainChannelRoute(agent string, full bool) ChannelRoute {
	route := ChannelRoute{Agent: agent}
	// consider records a rule and reports whether evaluation can stop.
	consider := func(rule, ch string, applies bool, detail string) bool {
		c := RouteCandidate{Rule: rule, Channel: ch, Applies: applies, Detail: detail}
		if applies && route.Rule == "" {
			route.Channel, route.Rule = ch, rule
			c.Won = true
		}
		route.Candidates = append(route.Candidates, c)
		return route.Rule != "" && !full
	}

	var spawnCh, inheritedFrom string
	var inherited bool
	if agent != "" {
		b.mu.Lock()
		spawnCh = b.agentSpawnChannel[agent]
		inheritedFrom, inherited = b.agentSpawnInheritedBy[agent]
		route.Project = b.agentProject[agent]
		route.Role = b.agentRole[agent]
		b.mu.Unlock()
	}
	route.SpawnChannel = spawnCh
	if inherited {
		route.InheritedFromRole = inheritedFrom
	}
	project, role := route.Project, route.Role

	// 1. Router override or pattern match. Router patterns are written
	// against project/role/name, so resolve with the cached project and role.
	var routerResult RouteResult
	switch {
	case b.router == nil:
		consider(RouteRuleRouter, "", false, "no router configured")
	case agent == "":
		consider(RouteRuleRouter, "", false, "no agent")
	default:
		routerResult = b.router.ResolveIdentity(project, role, agent)
		if routerResult.ChannelID != "" && !routerResult.IsDefault {
			if consider(RouteRuleRouter, routerResult.ChannelID, true, "matched "+routerResult.MatchedBy) {
				return route
			}
		} else {
			consider(RouteRuleRouter, "", false, "no override or pattern matches the agent")
		}
	}

	// 2. Prefer the channel where the agent was spawned, so the card appears
	// in the same channel as the /spawn command. A channel merely INHERITED
	// from a spawner of a different role is only a fallback: it must not
	// override the child role's own channel_roles mapping (rule 3).
	var inheritedSpawnCh string
	switch {
	case spawnCh == "":
		consider(RouteRuleSpawnChannel, "", false, "no spawn or schedule channel")
	case !inherited:
		if consider(RouteRuleSpawnChannel, spawnCh, true, "explicit pin (/spawn, --slack-spawn-channel, task or schedule channel)") {
			return route
		}
	case inheritedFrom == role:
		if consider(RouteRuleSpawnChannel, spawnCh, true, fmt.Sprintf("inherited from a spawner of the same role (%s)", inheritedFrom)) {
			return route
		}
	default:
		inheritedSpawnCh = spawnCh
		consider(RouteRuleSpawnChannel, "", false, fmt.Sprintf("spawn channel %s was inherited from a %s spawner; it ranks after the role channel", spawnCh, inheritedFrom))
	}

	// 3. Role-scoped override: if the project maps this agent's role to a
	// specific channel via channel_roles, prefer that channel. This routes
	// agents spawned without a Slack spawn channel (e.g. JIRA-triggered)
	// to the channel dedicated to their role instead of the project's
	// primary channel.
	if project != "" && role != "" {
		ch := b.projectRoleChannel(project, role)
		if ch != "" {
			if consider(RouteRuleRoleChannel, ch, true, fmt.Sprintf("channel_roles maps role %s in project %s", role, project)) {
				return route
			}
		} else {
			consider(RouteRuleRoleChannel, "", false, fmt.Sprintf("project %s has no channel_roles entry for role %s", project, role))
		}
	} else {
		consider(RouteRuleRoleChannel, "", false, "agent has no cached project and role")
	}

	// 3b. An inherited spawn channel (different-role spawner, no role channel
	// of its own) still beats the project's primary channel.
	if inheritedSpawnCh != "" {
		if consider(RouteRuleInheritedSpawn, inheritedSpawnCh, true, fmt.Sprintf("inherited from a %s spawner", inheritedFrom)) {
			return route
		}
	} else {
		consider(RouteRuleInheritedSpawn, "", false, "no spawn channel inherited from a different-role spawner")
	}

	// 4. The project's primary channel.
	if project != "" {
		ch := b.projectPrimaryChannel(project)
		if ch != "" {
			if consider(RouteRuleProjectPrimary, ch, true, fmt.Sprintf("first slack_channel of project %s", project)) {
				return route
			}
		} else {
			consider(RouteRuleProjectPrimary, "", false, fmt.Sprintf("project %s has no slack_channel", project))
		}
	} else {
		consider(RouteRuleProjectPrimary, "", false, "agent has no cached project")
	}

	// 5. Router default (if a router is configured), otherwise the bot
	// default channel.
	if routerResult.ChannelID != "" {
		if consider(RouteRuleRouterDefault, routerResult.ChannelID, true, "router default channel") {
			return route
		}
	} else {
		consider(RouteRuleRouterDefault, "", false, "no router default channel")
	}
	consider(RouteRuleBotDefault, b.channel, true, "bridge default channel")
	return route
}

// RouteExplainAPI serves GET /api/slack/route, the read-only explanation of
// which Slack channel the bridge would use for an agent and why.
type RouteExplainAPI struct {
	bot *Bot
}

// NewRouteExplainAPI creates the route explainer handler.
func NewRouteExplainAPI(bot *Bot) *RouteExplainAPI {
	return &RouteExplainAPI{bot: bot}
}

// RegisterRoutes registers the route explainer on the given mux.
func (a *RouteExplainAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/route", a.handleRoute)
}

// routeExplainResponse is the JSON body of GET /api/slack/route.
type routeExplainResponse struct {
	ChannelRoute
	ChannelName string `json:"channel_name,omitempty"`
	// Bead and BeadType are set when the caller named a bead; Agent is then
	// the agent the bead was resolved to.
	Bead     string `json:"bead,omitempty"`
	BeadType string `json:"bead_type,omitempty"`
	// CacheRefreshed is true when the bridge had no routing for the agent and
	// reloaded it from the agent bead first, as a decision or escalation card
	// does (resolveCardChannel).
	CacheRefreshed bool `json:"cache_refreshed,omitempty"`
	// ThreadBound agents keep their card in their Slack thread, not in the
	// routed channel.
	ThreadBound     bool   `json:"thread_bound"`
	CardFound       bool   `json:"card_found"`
	CardChannel     string `json:"card_channel,omitempty"`
	CardChannelName string `json:"card_channel_name,omitempty"`
	CardTS          string `json:"card_ts,omitempty"`
}

// handleRoute handles GET /api/slack/route?agent=<name> or ?bead=<id>. For a
// bead, the agent is the agent bead's own agent, a decision's assignee, an
// escalation's requesting agent, or any other bead's assignee.
func (a *RouteExplainAPI) handleRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeInboundError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	ctx := r.Context()
	q := r.URL.Query()
	agent := strings.TrimSpace(q.Get("agent"))
	beadID := strings.TrimSpace(q.Get("bead"))
	if (agent == "") == (beadID == "") {
		writeInboundError(w, http.StatusBadRequest, "exactly one of agent or bead is required")
		return
	}

	var resp routeExplainResponse
	if beadID != "" {
		if a.bot.daemon == nil {
			writeInboundError(w, http.StatusServiceUnavailable, "bridge has no beads daemon")
			return
		}
		detail, err := a.bot.daemon.GetBead(ctx, beadID)
		if err != nil || detail == nil {
			writeInboundError(w, http.StatusNotFound, fmt.Sprintf("bead %s not found", beadID))
			return
		}
		resp.Bead, resp.BeadType = detail.ID, detail.Type
		agent = a.bot.routeAgentForBead(ctx, beadEventFromDetail(detail))
		if agent == "" {
			writeInboundError(w, http.StatusUnprocessableEntity, fmt.Sprintf("bead %s (%s) has no agent to route for", detail.ID, detail.Type))
			return
		}
	}
	agent = extractAgentName(agent)

	resp.CacheRefreshed = a.bot.warmAgentRouting(ctx, agent)
	resp.ChannelRoute = a.bot.explainChannelRoute(agent, true)

	a.bot.mu.Lock()
	resp.ThreadBound = a.bot.isThreadBound(agent)
	a.bot.mu.Unlock()
	resp.CardChannel, resp.CardTS, resp.CardFound = a.bot.AgentCardLocation(agent)

	names := map[string]string{}
	name := func(id string) string {
		if id == "" {
			return ""
		}
		if n, ok := names[id]; ok {
			return n
		}
		n := a.bot.slackChannelName(ctx, id)
		names[id] = n
		return n
	}
	resp.ChannelName = name(resp.Channel)
	resp.CardChannelName = name(resp.CardChannel)
	for i := range resp.Candidates {
		resp.Candidates[i].ChannelName = name(resp.Candidates[i].Channel)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// routeAgentForBead returns the agent whose routing a bead's cards follow.
func (b *Bot) routeAgentForBead(ctx context.Context, bead BeadEvent) string {
	switch bead.Type {
	case "agent":
		if n := strings.TrimSpace(bead.Fields["agent"]); n != "" {
			return n
		}
		return bead.Title
	case "escalation":
		return b.escalationRequester(ctx, bead)
	default:
		// A decision's card follows its assignee (NotifyDecision), as does
		// any other bead's.
		return bead.Assignee
	}
}

// warmAgentRouting reloads agent's routing caches from its agent bead when the
// bridge has none, as resolveCardChannel does, and reports whether it did.
func (b *Bot) warmAgentRouting(ctx context.Context, agent string) bool {
	if agent == "" || b.daemon == nil {
		return false
	}
	b.mu.Lock()
	cold := b.agentSpawnChannel[agent] == "" && (b.agentProject[agent] == "" || b.agentRole[agent] == "")
	b.mu.Unlock()
	if cold {
		b.refreshAgentRouting(ctx, agent)
	}
	return cold
}

// slackChannelName returns the channel's Slack name, or "" when it cannot be
// read (a DM, a channel the bot is not in, no Slack client).
func (b *Bot) slackChannelName(ctx context.Context, channelID string) string {
	if b.api == nil || channelID == "" {
		return ""
	}
	info, err := b.api.GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{ChannelID: channelID})
	if err != nil || info == nil {
		return ""
	}
	return info.Name
}
