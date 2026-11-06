package bridge

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/slack-go/slack"
)

// This file adds the channel-management write endpoints to SlackChannelAPI:
// create, archive, unarchive, and invite. They back the `gb slack channel-*`
// CLI commands and tie into gardener-driven collaboration flows.
//
// Required Slack bot scopes (see gardenio/slack/manifest.json):
//   - channels:manage  — create/archive public channels, invite to them
//   - groups:write     — create/archive private channels, invite to them
//
// "Destroy" maps to Slack's archive (reversible); Slack has no hard channel
// delete in its public API.

// RegisterManageRoutes registers the channel-management write routes.
// Kept separate from RegisterRoutes so the read-only API surface stays
// obvious, but both register on the same mux from the slack-bridge wiring.
func (a *SlackChannelAPI) RegisterManageRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/channels/create", a.handleCreateChannel)
	mux.HandleFunc("/api/slack/channels/archive", a.handleArchiveChannel)
	mux.HandleFunc("/api/slack/channels/unarchive", a.handleUnarchiveChannel)
	mux.HandleFunc("/api/slack/channels/invite", a.handleInviteToChannel)
}

// ── create ─────────────────────────────────────────────────────────

// createChannelRequest is the JSON body for POST /api/slack/channels/create.
type createChannelRequest struct {
	Name    string   `json:"name"`
	Private bool     `json:"private"`
	Topic   string   `json:"topic,omitempty"`
	Purpose string   `json:"purpose,omitempty"`
	Invite  []string `json:"invite,omitempty"` // user IDs or emails
}

// createChannelResponse is the JSON response from POST /api/slack/channels/create.
type createChannelResponse struct {
	OK      bool     `json:"ok"`
	ID      string   `json:"id,omitempty"`
	Name    string   `json:"name,omitempty"`
	Private bool     `json:"private,omitempty"`
	Invited []string `json:"invited,omitempty"`
	Warning string   `json:"warning,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// handleCreateChannel handles POST /api/slack/channels/create.
// Creates a public or private channel, optionally sets topic/purpose, and
// optionally invites an initial set of users (by ID or email).
func (a *SlackChannelAPI) handleCreateChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req createChannelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	name := normalizeChannelName(req.Name)
	if name == "" {
		http.Error(w, `{"error":"name is required"}`, http.StatusBadRequest)
		return
	}

	ch, err := a.api.CreateConversation(slack.CreateConversationParams{
		ChannelName: name,
		IsPrivate:   req.Private,
	})
	if err != nil {
		a.logger.Error("failed to create channel", "name", name, "private", req.Private, "error", err)
		a.writeManageError(w, err.Error())
		return
	}

	a.logger.Info("created channel", "id", ch.ID, "name", ch.Name, "private", req.Private)

	resp := createChannelResponse{
		OK:      true,
		ID:      ch.ID,
		Name:    ch.Name,
		Private: ch.IsPrivate,
	}

	var warnings []string

	if req.Topic != "" {
		if _, err := a.api.SetTopicOfConversation(ch.ID, req.Topic); err != nil {
			a.logger.Warn("failed to set channel topic", "id", ch.ID, "error", err)
			warnings = append(warnings, "set topic: "+err.Error())
		}
	}
	if req.Purpose != "" {
		if _, err := a.api.SetPurposeOfConversation(ch.ID, req.Purpose); err != nil {
			a.logger.Warn("failed to set channel purpose", "id", ch.ID, "error", err)
			warnings = append(warnings, "set purpose: "+err.Error())
		}
	}

	if len(req.Invite) > 0 {
		invited, inviteErr := a.inviteUsers(ch.ID, req.Invite)
		resp.Invited = invited
		if inviteErr != "" {
			warnings = append(warnings, "invite: "+inviteErr)
		}
	}

	if len(warnings) > 0 {
		resp.Warning = strings.Join(warnings, "; ")
	}

	a.writeJSON(w, resp)
}

// ── archive / unarchive ────────────────────────────────────────────

// channelActionRequest is the JSON body for archive/unarchive.
type channelActionRequest struct {
	Channel string `json:"channel"`
}

// channelActionResponse is the JSON response for archive/unarchive.
type channelActionResponse struct {
	OK      bool   `json:"ok"`
	Channel string `json:"channel,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handleArchiveChannel handles POST /api/slack/channels/archive.
// Archiving is Slack's reversible form of "destroy" — there is no hard
// channel delete in the public API.
func (a *SlackChannelAPI) handleArchiveChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req channelActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.Channel == "" {
		http.Error(w, `{"error":"channel is required"}`, http.StatusBadRequest)
		return
	}

	if err := a.api.ArchiveConversation(req.Channel); err != nil {
		a.logger.Error("failed to archive channel", "channel", req.Channel, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(channelActionResponse{OK: false, Error: err.Error()})
		return
	}

	a.logger.Info("archived channel", "channel", req.Channel)
	a.writeJSON(w, channelActionResponse{OK: true, Channel: req.Channel})
}

// handleUnarchiveChannel handles POST /api/slack/channels/unarchive.
func (a *SlackChannelAPI) handleUnarchiveChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req channelActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.Channel == "" {
		http.Error(w, `{"error":"channel is required"}`, http.StatusBadRequest)
		return
	}

	if err := a.api.UnArchiveConversation(req.Channel); err != nil {
		a.logger.Error("failed to unarchive channel", "channel", req.Channel, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(channelActionResponse{OK: false, Error: err.Error()})
		return
	}

	a.logger.Info("unarchived channel", "channel", req.Channel)
	a.writeJSON(w, channelActionResponse{OK: true, Channel: req.Channel})
}

// ── invite ─────────────────────────────────────────────────────────

// inviteRequest is the JSON body for POST /api/slack/channels/invite.
type inviteRequest struct {
	Channel string   `json:"channel"`
	Users   []string `json:"users"` // user IDs or emails
}

// inviteResponse is the JSON response for POST /api/slack/channels/invite.
type inviteResponse struct {
	OK      bool     `json:"ok"`
	Channel string   `json:"channel,omitempty"`
	Invited []string `json:"invited,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// handleInviteToChannel handles POST /api/slack/channels/invite.
// Users may be given as Slack user IDs (U…/W…) or email addresses; emails
// are resolved to IDs via users.lookupByEmail.
func (a *SlackChannelAPI) handleInviteToChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req inviteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}
	if req.Channel == "" || len(req.Users) == 0 {
		http.Error(w, `{"error":"channel and at least one user are required"}`, http.StatusBadRequest)
		return
	}

	invited, errMsg := a.inviteUsers(req.Channel, req.Users)
	if errMsg != "" && len(invited) == 0 {
		a.logger.Error("failed to invite users", "channel", req.Channel, "error", errMsg)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(inviteResponse{OK: false, Channel: req.Channel, Error: errMsg})
		return
	}

	a.logger.Info("invited users to channel", "channel", req.Channel, "count", len(invited))
	a.writeJSON(w, inviteResponse{OK: true, Channel: req.Channel, Invited: invited, Error: errMsg})
}

// ── helpers ────────────────────────────────────────────────────────

// inviteUsers resolves any email addresses to user IDs and invites the
// resulting set to the channel in a single conversations.invite call.
// Returns the resolved user IDs that were invited and a (possibly empty)
// error string describing any resolution or invite failure.
func (a *SlackChannelAPI) inviteUsers(channelID string, users []string) ([]string, string) {
	var ids []string
	var problems []string

	for _, u := range users {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if strings.Contains(u, "@") {
			user, err := a.api.GetUserByEmail(u)
			if err != nil {
				problems = append(problems, "lookup "+u+": "+err.Error())
				continue
			}
			ids = append(ids, user.ID)
		} else {
			ids = append(ids, strings.TrimPrefix(u, "@"))
		}
	}

	if len(ids) == 0 {
		if len(problems) > 0 {
			return nil, strings.Join(problems, "; ")
		}
		return nil, "no valid users to invite"
	}

	if _, err := a.api.InviteUsersToConversation(channelID, ids...); err != nil {
		problems = append(problems, err.Error())
		return nil, strings.Join(problems, "; ")
	}

	return ids, strings.Join(problems, "; ")
}

// normalizeChannelName lowercases and sanitizes a requested channel name to
// fit Slack's rules: lowercase, no spaces/periods, only letters/numbers/
// hyphens/underscores, max 80 chars.
func normalizeChannelName(name string) string {
	name = strings.TrimSpace(strings.TrimPrefix(name, "#"))
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '.':
			b.WriteRune('-')
		}
	}
	out := b.String()
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

// writeJSON writes a 200 JSON response.
func (a *SlackChannelAPI) writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeManageError writes a 500 JSON error in the create response shape.
func (a *SlackChannelAPI) writeManageError(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(w).Encode(createChannelResponse{OK: false, Error: msg})
}
