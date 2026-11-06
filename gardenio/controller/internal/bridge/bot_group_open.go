package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
)

// This file adds the group-DM (mpim) start endpoint to the Slack bridge:
// POST /api/slack/groups/open. It backs the `gb slack group open` CLI command
// (Phase 2 of epic ka-0000003709).
//
// The endpoint opens (or re-opens) a Slack group DM (mpim) with the supplied
// member user IDs, optionally spawns an agent bound to that mpim channel, and
// posts an intro message. Binding an agent means: SetThreadAgent(mpim, "",
// agent) plus stamping slack_thread_channel / spawn_source=group-dm / mode on
// the agent bead so inbound mpim messages route to it (the thread↔agent binding
// is already channel-type-agnostic — see bot_mpim inbound routing).
//
// Required Slack bot scope: mpim:write (added to gardenio/slack/manifest.json in
// Phase 0). Without it conversations.open returns missing_scope.

// GroupOpenAPI serves POST /api/slack/groups/open. It composes the Bot (for
// Slack + state) with the bead daemon (for spawning + binding the agent).
type GroupOpenAPI struct {
	bot    *Bot
	daemon BeadClient
}

// NewGroupOpenAPI creates the group-open API handler.
func NewGroupOpenAPI(bot *Bot, daemon BeadClient) *GroupOpenAPI {
	return &GroupOpenAPI{bot: bot, daemon: daemon}
}

// RegisterRoutes registers the group-open route on the given mux.
func (a *GroupOpenAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/groups/open", a.handleOpenGroup)
}

// groupOpenRequest is the JSON body for POST /api/slack/groups/open.
type groupOpenRequest struct {
	// Users are the mpim members, as Slack user IDs (U…/W…) or email addresses.
	// Emails are resolved to IDs via users.lookupByEmail, mirroring the invite
	// path (bot_channels_manage.go inviteUsers).
	Users []string `json:"users"`
	// Project scopes a spawned agent (defaults to the bot's default project when
	// empty). Ignored when Spawn is false.
	Project string `json:"project,omitempty"`
	// Role sets the spawned agent's role/mode (defaults to "crew"). Ignored when
	// Spawn is false.
	Role string `json:"role,omitempty"`
	// Prompt is the custom prompt for the spawned agent. Ignored when Spawn is false.
	Prompt string `json:"prompt,omitempty"`
	// Message is the intro message posted to the mpim. When empty a default
	// intro is posted (naming the bound agent, if any).
	Message string `json:"message,omitempty"`
	// Spawn controls whether an agent is spawned and bound to the mpim. When
	// false the endpoint only opens the mpim and posts the intro message.
	Spawn bool `json:"spawn,omitempty"`
}

// groupOpenResponse is the JSON response from POST /api/slack/groups/open.
type groupOpenResponse struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel,omitempty"` // mpim channel ID (G…)
	Agent   string `json:"agent,omitempty"`   // bound agent name, if spawned
	AgentID string `json:"agent_id,omitempty"`
	TS      string `json:"ts,omitempty"` // intro message timestamp
	Error   string `json:"error,omitempty"`
}

// handleOpenGroup handles POST /api/slack/groups/open.
func (a *GroupOpenAPI) handleOpenGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	if a.bot == nil {
		a.writeError(w, http.StatusServiceUnavailable, "group DMs require the Slack bot (Socket Mode) — not available in webhook mode")
		return
	}

	var req groupOpenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if len(normalizeUserIDs(req.Users)) == 0 {
		http.Error(w, `{"error":"at least one user is required"}`, http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Resolve any email addresses to Slack user IDs (mirrors the invite path).
	users, resolveErr := a.resolveUsers(req.Users)
	if len(users) == 0 {
		a.writeError(w, http.StatusBadRequest, "no resolvable users: "+resolveErr)
		return
	}

	// Open (or re-open) the mpim with all members. mpim:write is required here.
	mpimCh, err := a.bot.openGroupConversation(ctx, users)
	if err != nil {
		a.bot.logger.Error("group open: failed to open mpim", "users", users, "error", err)
		a.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := groupOpenResponse{OK: true, Channel: mpimCh}

	// Optionally spawn an agent and bind it to the mpim channel.
	var agentName string
	if req.Spawn {
		var agentID string
		agentName, agentID, err = a.spawnAndBind(ctx, mpimCh, req)
		if err != nil {
			a.bot.logger.Error("group open: failed to spawn/bind agent", "channel", mpimCh, "error", err)
			a.writeError(w, http.StatusInternalServerError, "opened mpim "+mpimCh+" but agent spawn failed: "+err.Error())
			return
		}
		resp.Agent = agentName
		resp.AgentID = agentID
	}

	// Post the intro message.
	intro := strings.TrimSpace(req.Message)
	if intro == "" {
		intro = defaultGroupIntro(agentName)
	}
	if a.bot.api != nil {
		_, ts, postErr := a.bot.api.PostMessageContext(ctx, mpimCh, slack.MsgOptionText(intro, false))
		if postErr != nil {
			// The mpim is open and (if requested) the agent is bound — the intro
			// post failing is a soft error, surfaced as a warning, not a 500.
			a.bot.logger.Warn("group open: failed to post intro message", "channel", mpimCh, "error", postErr)
		} else {
			resp.TS = ts
		}
	}

	a.bot.logger.Info("group open: opened mpim",
		"channel", mpimCh, "members", len(users), "agent", agentName)
	writeJSONResponse(w, resp)
}

// spawnAndBind spawns an agent scoped to project/role/prompt and binds it to
// the mpim channel: it records the thread→agent mapping in state and stamps the
// binding fields (slack_thread_channel / spawn_source=group-dm / mode) on the
// agent bead so inbound mpim messages route to it. Returns the agent name and
// bead ID.
func (a *GroupOpenAPI) spawnAndBind(ctx context.Context, mpimCh string, req groupOpenRequest) (string, string, error) {
	project := req.Project
	if project == "" {
		project = a.bot.projectFromChannel(ctx, a.bot.channel)
	}
	role := req.Role
	if role == "" {
		role = "crew"
	}

	agentName := generateSpawnName(project)

	// Binding fields carried onto the agent bead at spawn. spawn_source=group-dm
	// records provenance; slack_thread_channel with an empty ts binds the whole
	// mpim (top-level) to the agent, matching the channel-type-agnostic thread
	// binding used by the mpim inbound path.
	extra := map[string]string{
		"slack_thread_channel": mpimCh,
		"slack_thread_ts":      "",
		"slack_spawn_channel":  mpimCh,
		"spawn_source":         "group-dm",
		"mode":                 role,
	}

	agentID, err := a.daemon.SpawnAgent(ctx, agentName, project, "", role, req.Prompt, extra)
	if err != nil {
		return "", "", fmt.Errorf("spawn agent: %w", err)
	}

	// Record the thread→agent mapping in state so inbound mpim events resolve
	// to this agent even before the bead-field binding is observed.
	if a.bot.state != nil {
		if err := a.bot.state.SetThreadAgent(mpimCh, "", agentName); err != nil {
			a.bot.logger.Warn("group open: failed to record thread→agent binding",
				"channel", mpimCh, "agent", agentName, "error", err)
		}
	}

	a.bot.logger.Info("group open: spawned + bound agent",
		"agent", agentName, "agent_id", agentID, "project", project, "role", role, "channel", mpimCh)
	return agentName, agentID, nil
}

// resolveUsers resolves a mix of Slack user IDs and email addresses to a
// de-duplicated list of Slack user IDs. Emails (values containing "@") are
// resolved via users.lookupByEmail; a failed lookup is skipped and recorded in
// the returned error string. Mirrors inviteUsers in bot_channels_manage.go.
func (a *GroupOpenAPI) resolveUsers(users []string) ([]string, string) {
	var ids []string
	var problems []string
	seen := make(map[string]bool)
	for _, u := range users {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		var id string
		if strings.Contains(u, "@") {
			user, err := a.bot.api.GetUserByEmail(u)
			if err != nil {
				problems = append(problems, "lookup "+u+": "+err.Error())
				continue
			}
			id = user.ID
		} else {
			id = strings.TrimPrefix(u, "@")
		}
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, strings.Join(problems, "; ")
}

// normalizeUserIDs trims, de-@-prefixes, and drops empties from a user list.
func normalizeUserIDs(users []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, u := range users {
		u = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(u), "@"))
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// defaultGroupIntro builds the default intro message posted to a new mpim.
func defaultGroupIntro(agentName string) string {
	if agentName == "" {
		return "This group DM was started by gardenio. Mention @gardenio to spawn an agent here."
	}
	return fmt.Sprintf("Hi — %s is now working in this group DM. Reply here to collaborate.",
		extractAgentName(agentName))
}

// writeError writes a JSON error with the given HTTP status in the group-open
// response shape.
func (a *GroupOpenAPI) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(groupOpenResponse{OK: false, Error: msg})
}
