package gardenhub

import (
	"context"
	"net/http"
	"time"
)

// The settings panel (Lands workbench-glass spec WB-13, step 5.12, gd-au7ywvWAG8): the viewer's own settings as a
// Workbench kind (web/settings.js, /garden?settings=me), opened from the Menu's Settings item and the retired /settings
// page (R10). It shows and edits what the viewer's user bead holds for the garden: their spawn preferences (the default
// project, role and prompt the bead view's "Put an agent on this" starts from, and one agent or one per bead for a
// selection) and who they are (display name, pronouns, team, the Slack channel their DMs go to). The reader is the
// viewer's own (cmd/beads-viewer settings_garden.go: the /settings page's user-bead lookup); the write is POST /settings
// asked for JSON with only the fields it edits (scope=garden), so the page's other settings, the homepage layout and the
// simple/vibe switch, are kept as stored.

// SettingsReader reads the requester's settings. A viewer with no user bead reads a document with HasBead false.
type SettingsReader interface {
	Settings(ctx context.Context) (*SettingsDoc, error)
}

const settingsTimeout = 10 * time.Second

// SettingsSpawn is the viewer's spawn preferences, as stored.
type SettingsSpawn struct {
	Project string `json:"default_spawn_project"`
	Role    string `json:"default_spawn_role"`
	Prompt  string `json:"default_spawn_prompt_template"`
	// PerSelection is "single" (one agent for a selection; also "") or "per-bead".
	PerSelection string `json:"spawn_per_selection"`
}

// SettingsIdentity is who the viewer is, as stored on their user bead.
type SettingsIdentity struct {
	DisplayName    string `json:"display_name"`
	Pronouns       string `json:"pronouns"`
	Team           string `json:"team"`
	SlackChannelID string `json:"slack_channel_id"`
}

// SettingsDoc is the viewer's settings as the panel shows and edits them.
type SettingsDoc struct {
	Email string `json:"email"`
	// HasBead: the viewer has a user bead to keep settings on; without one there is nothing to edit.
	HasBead  bool             `json:"has_bead"`
	BeadID   string           `json:"bead_id,omitempty"`
	Spawn    SettingsSpawn    `json:"spawn"`
	Identity SettingsIdentity `json:"identity"`
	// Projects and Roles are the choices for a default project and role (a stored value outside them is kept and shown).
	Projects []string `json:"projects"`
	Roles    []string `json:"roles"`
}

// settingsPanel serves GET /garden/settings.
func (h *Handler) settingsPanel(w http.ResponseWriter, r *http.Request) {
	if h.Settings == nil {
		http.Error(w, "settings not enabled", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), settingsTimeout)
	defer cancel()
	doc, err := h.Settings.Settings(ctx)
	if err != nil || doc == nil {
		http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		return
	}
	if doc.Email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if doc.Projects == nil {
		doc.Projects = []string{}
	}
	if doc.Roles == nil {
		doc.Roles = []string{}
	}
	writeDoc(w, "kbeads", doc)
}
