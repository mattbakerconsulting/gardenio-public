package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
)

// This file adds the group-DM (mpim) invite endpoint to the Slack bridge:
// POST /api/slack/groups/{channel}/invite. It backs the `gb slack group invite`
// CLI command (Phase 3 of epic ka-0000003709, task ka-0000006522).
//
// WHY THIS IS A RE-OPEN, NOT AN IN-PLACE INVITE:
//
// Slack has NO in-place mpim invite. conversations.invite is channels-only
// (public/private channels), not mpim; and conversations.open with a new member
// set yields a DISTINCT conversation with a distinct channel ID and no shared
// history — it does not mutate the existing mpim. So "invite user X into mpim G"
// is implemented as a SUPERSET RE-OPEN + BINDING MIGRATION:
//
//  1. Read G's current members (conversations.members, paginated).
//  2. Drop the bot's own user id (the bot is the implicit caller of open).
//  3. Resolve the invitee(s) (ID or email) and union with the current members.
//  4. conversations.open with the SUPERSET -> new channel G'. If the invitee is
//     already a member the superset equals the existing set and Slack returns
//     the SAME channel (G' == G, an idempotent no-op re-open); otherwise a NEW
//     mpim G' with fresh history.
//  5. MIGRATE the agent binding from G to G' when G' != G: move the thread->agent
//     state mapping (SetThreadAgent(G',"",agent) + RemoveThreadAgent(G,"")) and
//     update the agent bead's slack_thread_channel/slack_spawn_channel to G'.
//  6. Post a one-line context hand-off in BOTH the old (G) and new (G') mpim so
//     humans aren't lost across the channel move.
//
// LEGACY FALLBACK: if no agent is bound to G (a human-only or unbound legacy
// mpim), the superset is still re-opened and hand-offs posted, but there is no
// binding to migrate (moved=false, agent=""). A same-channel result (invitee
// already present) is an idempotent success with a single hand-off note.
//
// Required Slack bot scope: mpim:write (added in Phase 0) for conversations.open.

// GroupInviteAPI serves POST /api/slack/groups/{channel}/invite. It composes the
// Bot (for Slack + state) with the bead daemon (for migrating the agent binding).
type GroupInviteAPI struct {
	bot    *Bot
	daemon BeadClient
}

// NewGroupInviteAPI creates the group-invite API handler.
func NewGroupInviteAPI(bot *Bot, daemon BeadClient) *GroupInviteAPI {
	return &GroupInviteAPI{bot: bot, daemon: daemon}
}

// RegisterRoutes registers the group-invite route on the given mux. The channel
// (mpim) id is a path value so the endpoint reads as an action on a resource:
// POST /api/slack/groups/{channel}/invite.
func (a *GroupInviteAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/slack/groups/{channel}/invite", a.handleInviteGroup)
}

// groupInviteRequest is the JSON body for POST /api/slack/groups/{channel}/invite.
type groupInviteRequest struct {
	// Users are the new invitee(s), as Slack user IDs (U…/W…) or email
	// addresses. Emails are resolved via users.lookupByEmail, mirroring the open
	// path (bot_group_open.go resolveUsers).
	Users []string `json:"users"`
	// Message overrides the hand-off note posted in the NEW mpim. When empty a
	// default hand-off is posted. The OLD mpim always gets the standard
	// "continuing in a new group DM" pointer.
	Message string `json:"message,omitempty"`
}

// groupInviteResponse is the JSON response from POST /api/slack/groups/{channel}/invite.
type groupInviteResponse struct {
	OK         bool   `json:"ok"`
	OldChannel string `json:"old_channel,omitempty"` // the source mpim (G)
	Channel    string `json:"channel,omitempty"`     // the superset mpim (G')
	Moved      bool   `json:"moved"`                 // true when G' != G (a new conversation)
	Agent      string `json:"agent,omitempty"`       // migrated agent name, if any
	Members    int    `json:"members,omitempty"`     // member count of the superset mpim
	Error      string `json:"error,omitempty"`
}

// handleInviteGroup handles POST /api/slack/groups/{channel}/invite.
func (a *GroupInviteAPI) handleInviteGroup(w http.ResponseWriter, r *http.Request) {
	if a.bot == nil {
		a.writeError(w, http.StatusServiceUnavailable, "group DMs require the Slack bot (Socket Mode) — not available in webhook mode")
		return
	}

	oldCh := strings.TrimSpace(r.PathValue("channel"))
	if oldCh == "" {
		a.writeError(w, http.StatusBadRequest, "channel (mpim id) is required in the path")
		return
	}

	var req groupInviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(normalizeUserIDs(req.Users)) == 0 {
		a.writeError(w, http.StatusBadRequest, "at least one user to invite is required")
		return
	}

	ctx := r.Context()

	// Resolve the invitee(s) (ID or email) to Slack user IDs.
	invitees, resolveErr := a.resolveUsers(req.Users)
	if len(invitees) == 0 {
		a.writeError(w, http.StatusBadRequest, "no resolvable users: "+resolveErr)
		return
	}

	// Read the current members of the source mpim.
	current, err := a.currentMembers(ctx, oldCh)
	if err != nil {
		a.bot.logger.Error("group invite: failed to read mpim members", "channel", oldCh, "error", err)
		a.writeError(w, http.StatusInternalServerError, "read members of "+oldCh+": "+err.Error())
		return
	}

	// Build the superset: current members (minus the bot) unioned with invitees.
	superset := a.buildSuperset(current, invitees)
	if len(superset) == 0 {
		a.writeError(w, http.StatusBadRequest, "superset member set is empty after excluding the bot")
		return
	}

	// Re-open with the superset. If the invitees are already members Slack
	// returns the SAME channel (idempotent no-op); otherwise a NEW mpim.
	newCh, err := a.bot.openGroupConversation(ctx, superset)
	if err != nil {
		a.bot.logger.Error("group invite: failed to re-open superset mpim", "old", oldCh, "superset", superset, "error", err)
		a.writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := groupInviteResponse{
		OK:         true,
		OldChannel: oldCh,
		Channel:    newCh,
		Moved:      newCh != oldCh,
		Members:    len(superset),
	}

	// Migrate the agent binding from oldCh to newCh, but only when the
	// conversation actually moved. A same-channel re-open keeps the existing
	// binding intact (nothing to move).
	if resp.Moved {
		resp.Agent = a.migrateBinding(ctx, oldCh, newCh)
	} else {
		// No move: report whatever agent is already bound so callers see it.
		if a.bot.state != nil {
			if bound, ok := a.bot.state.GetThreadAgent(oldCh, ""); ok {
				resp.Agent = bound
			}
		}
	}

	// Post the context hand-off in both conversations.
	a.postHandoffs(ctx, oldCh, newCh, resp.Moved, resp.Agent, req.Message, invitees)

	a.bot.logger.Info("group invite: re-opened superset mpim",
		"old", oldCh, "new", newCh, "moved", resp.Moved, "members", len(superset), "agent", resp.Agent)
	writeJSONResponse(w, resp)
}

// currentMembers returns the current member user IDs of a conversation,
// paginating through conversations.members.
func (a *GroupInviteAPI) currentMembers(ctx context.Context, channel string) ([]string, error) {
	if a.bot.api == nil {
		return nil, fmt.Errorf("slack api unavailable")
	}
	var members []string
	cursor := ""
	for {
		page, next, err := a.bot.api.GetUsersInConversationContext(ctx, &slack.GetUsersInConversationParameters{
			ChannelID: channel,
			Cursor:    cursor,
			Limit:     200,
		})
		if err != nil {
			return nil, err
		}
		members = append(members, page...)
		if next == "" {
			break
		}
		cursor = next
	}
	return members, nil
}

// buildSuperset unions the current members (excluding the bot's own user id)
// with the invitees, de-duplicated and order-stable (current first, then any
// new invitees). The bot is dropped because it is the implicit caller of
// conversations.open and re-adding it is both redundant and rejected by Slack.
func (a *GroupInviteAPI) buildSuperset(current, invitees []string) []string {
	seen := make(map[string]bool)
	var out []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || id == a.bot.botUserID || seen[id] {
			return
		}
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range current {
		add(id)
	}
	for _, id := range invitees {
		add(id)
	}
	return out
}

// migrateBinding moves the thread->agent binding from oldCh to newCh: it re-keys
// the bridge state map and updates the agent bead's slack_thread_channel /
// slack_spawn_channel fields so inbound messages on the new mpim route to the
// same agent and the durable bead-link points at the new channel. Returns the
// migrated agent name (empty when nothing was bound — the legacy fallback).
func (a *GroupInviteAPI) migrateBinding(ctx context.Context, oldCh, newCh string) string {
	if a.bot.state == nil {
		return ""
	}
	agent, ok := a.bot.state.GetThreadAgent(oldCh, "")
	if !ok || agent == "" {
		return ""
	}

	// Re-key the in-memory/persisted binding: bind the new channel, drop the old.
	if err := a.bot.state.SetThreadAgent(newCh, "", agent); err != nil {
		a.bot.logger.Warn("group invite: failed to bind new mpim to agent",
			"channel", newCh, "agent", agent, "error", err)
	}
	if err := a.bot.state.RemoveThreadAgent(oldCh, ""); err != nil {
		a.bot.logger.Warn("group invite: failed to remove old mpim binding",
			"channel", oldCh, "agent", agent, "error", err)
	}

	// Update the durable bead-link on the agent bead so a bridge restart (which
	// rebuilds bindings from bead fields) reconstructs the NEW channel.
	if detail, err := a.daemon.FindAgentBeadAnyStatus(ctx, agent); err == nil && detail != nil {
		if err := a.daemon.UpdateBeadFields(ctx, detail.ID, map[string]string{
			"slack_thread_channel": newCh,
			"slack_spawn_channel":  newCh,
		}); err != nil {
			a.bot.logger.Warn("group invite: failed to update agent bead binding fields",
				"agent", agent, "bead", detail.ID, "error", err)
		}
	} else if err != nil {
		a.bot.logger.Warn("group invite: could not resolve agent bead to update binding fields",
			"agent", agent, "error", err)
	}

	return agent
}

// postHandoffs posts the one-line context hand-off in both conversations. The
// old mpim gets a pointer forward (only when the conversation actually moved);
// the new/current mpim gets the invitee-added note (custom message overrides the
// default). Post failures are soft — the re-open + binding move already
// succeeded — so they are logged as warnings, never surfaced as a 500.
func (a *GroupInviteAPI) postHandoffs(ctx context.Context, oldCh, newCh string, moved bool, agent, custom string, invitees []string) {
	if a.bot.api == nil {
		return
	}

	newNote := strings.TrimSpace(custom)
	if newNote == "" {
		newNote = defaultInviteIntro(agent, invitees, moved)
	}
	if _, _, err := a.bot.api.PostMessageContext(ctx, newCh, slack.MsgOptionText(newNote, false)); err != nil {
		a.bot.logger.Warn("group invite: failed to post hand-off in new mpim", "channel", newCh, "error", err)
	}

	// Only point the OLD conversation forward when a genuinely new mpim was
	// created — a same-channel re-open would post the pointer into the very
	// conversation it points at.
	if moved {
		oldNote := fmt.Sprintf("Continuing this conversation in a new group DM with %s added.", invitesList(invitees))
		if _, _, err := a.bot.api.PostMessageContext(ctx, oldCh, slack.MsgOptionText(oldNote, false)); err != nil {
			a.bot.logger.Warn("group invite: failed to post hand-off in old mpim", "channel", oldCh, "error", err)
		}
	}
}

// resolveUsers resolves a mix of Slack user IDs and email addresses to a
// de-duplicated list of Slack user IDs. Mirrors GroupOpenAPI.resolveUsers.
func (a *GroupInviteAPI) resolveUsers(users []string) ([]string, string) {
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

// defaultInviteIntro builds the default hand-off note posted in the new/current
// mpim after an invite.
func defaultInviteIntro(agentName string, invitees []string, moved bool) string {
	who := invitesList(invitees)
	if !moved {
		// Idempotent no-op re-open (invitee already a member).
		if agentName != "" {
			return fmt.Sprintf("%s is already in this group DM. %s is here to help.", who, extractAgentName(agentName))
		}
		return fmt.Sprintf("%s is already in this group DM.", who)
	}
	if agentName != "" {
		return fmt.Sprintf("%s was invited — continuing from the previous group DM. %s is here to help.",
			who, extractAgentName(agentName))
	}
	return fmt.Sprintf("%s was invited — continuing from the previous group DM.", who)
}

// invitesList renders the invitee user IDs as Slack mentions for a hand-off note.
func invitesList(invitees []string) string {
	if len(invitees) == 0 {
		return "a new member"
	}
	mentions := make([]string, 0, len(invitees))
	for _, id := range invitees {
		mentions = append(mentions, fmt.Sprintf("<@%s>", id))
	}
	return strings.Join(mentions, ", ")
}

// writeError writes a JSON error with the given HTTP status in the group-invite
// response shape.
func (a *GroupInviteAPI) writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(groupInviteResponse{OK: false, Error: msg})
}
