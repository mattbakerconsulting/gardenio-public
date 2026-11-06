package main

import (
	"context"
	"net/url"
	"sort"
	"strings"

	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// gardenSettings is the settings panel's reader (gardenhub.SettingsReader, Lands workbench-glass spec WB-13, step 5.12):
// the requester's user bead as /settings finds it (findUserBeadByEmail), its spawn preferences and identity fields, and
// the choices for a default project (the projects the daemon lists to this viewer) and role (quick-spawn's roles).
type gardenSettings struct{ s *Server }

// Settings implements gardenhub.SettingsReader.
func (g gardenSettings) Settings(ctx context.Context) (*gardenhub.SettingsDoc, error) {
	s := g.s
	email := oidcauth.EmailFromContext(ctx)
	doc := &gardenhub.SettingsDoc{Email: email, Roles: append([]string(nil), defaultQuickSpawnRoles...)}
	if email == "" {
		return doc, nil
	}
	bead, err := s.findUserBeadByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	if bead != nil {
		p, id := parsePreferences(bead.Fields), parseIdentity(bead.Fields)
		doc.HasBead, doc.BeadID = true, bead.ID
		doc.Spawn = gardenhub.SettingsSpawn{Project: p.DefaultSpawnProject, Role: p.DefaultSpawnRole, Prompt: p.DefaultSpawnPromptTemplate, PerSelection: p.SpawnPerSelection}
		doc.Identity = gardenhub.SettingsIdentity{DisplayName: id.DisplayName, Pronouns: id.Pronouns, Team: id.Team, SlackChannelID: id.SlackChannelID}
	}
	// The project choices are a convenience: a daemon that does not list them leaves the field free text.
	if projects, err := s.daemon.ListProjectBeads(ctx); err == nil {
		for name := range projects {
			doc.Projects = append(doc.Projects, name)
		}
		sort.Strings(doc.Projects)
	} else {
		s.logger.Warn("garden settings: projects", "error", err)
	}
	return doc, nil
}

// gardenSettingsScope is POST /settings's scope for the settings panel's save: only the fields posted are written.
const gardenSettingsScope = "garden"

// gardenSettingsFromForm is the stored preferences and identity with each field the form carries put over them (trimmed,
// as the full form trims, all but the prompt template); a field the form does not carry keeps its stored value.
func gardenSettingsFromForm(form url.Values, p Preferences, id Identity) (Preferences, Identity) {
	set := func(dst *string, key string, trim bool) {
		if !form.Has(key) {
			return
		}
		v := form.Get(key)
		if trim {
			v = strings.TrimSpace(v)
		}
		*dst = v
	}
	set(&p.DefaultSpawnProject, "default_spawn_project", true)
	set(&p.DefaultSpawnRole, "default_spawn_role", true)
	set(&p.DefaultSpawnPromptTemplate, "default_spawn_prompt_template", false)
	set(&p.SpawnPerSelection, "spawn_per_selection", true)
	if form.Has("vibe_mode") {
		p.VibeMode = flexBool(form.Get("vibe_mode") != "")
	}
	set(&id.DisplayName, "display_name", true)
	set(&id.Pronouns, "pronouns", true)
	set(&id.Team, "team", true)
	set(&id.SlackChannelID, "slack_channel_id", true)
	return p, id
}
