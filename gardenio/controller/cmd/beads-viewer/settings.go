package main

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxPrefStringLen caps individual preference string inputs. Generous enough
// for a project/role name or a multi-line prompt template, but short enough
// that an accidental paste of a whole document is rejected before hitting the
// daemon.
const maxPrefStringLen = 8192

// maxIdentityStringLen caps individual identity field inputs. Identity fields
// are short (display name, pronouns, team, slack channel id) — 256 chars is
// plenty without inviting accidental document pastes.
const maxIdentityStringLen = 256

// slackChannelIDPattern matches Slack channel/DM/group IDs: a single C/D/G
// prefix followed by one or more uppercase alphanumerics. Empty string is
// also allowed (handled separately in validateSlackChannelID).
var slackChannelIDPattern = regexp.MustCompile(`^[CDG][A-Z0-9]+$`)

// validSpawnPerSelection is the closed set of values the spawn_per_selection
// preference accepts. Anything else is coerced to "" (treated as the
// "single" default at render time).
var validSpawnPerSelection = map[string]bool{
	"single":   true,
	"per-bead": true,
}

// flexBool is a bool that survives the two ways a boolean preference reaches
// the `preferences` blob:
//
//   - the beads-viewer settings form marshals this struct directly, so it
//     would natively write `true` / `false`; but
//   - `gb prefs set <user> vibe_mode true` (cmd/gb/prefs.go) round-trips the
//     whole blob through map[string]string, so it writes the *string* "true".
//     `gb prime` and quick_spawn also decode the blob into map[string]string
//     and would error on a native JSON bool, silently dropping every pref.
//
// To keep all four consumers compatible, flexBool UNMARSHALS from either the
// string form ("true"/"1"/…) or a native JSON bool, and always MARSHALS to a
// JSON string. That keeps the stored blob an all-strings object — the shape
// the map[string]string readers require — while giving templates a real bool.
type flexBool bool

// UnmarshalJSON accepts a JSON bool (true/false), a quoted boolean string
// ("true"/"false"/"1"/"0"/…), JSON null, or an empty string. Anything it
// cannot parse is treated as false rather than failing the whole decode — a
// malformed value should not blank out the operator's other preferences.
func (b *flexBool) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	switch s {
	case "true":
		*b = true
		return nil
	case "false", "null", "":
		*b = false
		return nil
	}
	// Quoted string form, e.g. "true" written by `gb prefs set`.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			*b = false
			return nil
		}
		parsed, err := strconv.ParseBool(inner)
		if err != nil {
			// Unrecognized string (e.g. "yes") → false, don't reject the blob.
			*b = false
			return nil
		}
		*b = flexBool(parsed)
		return nil
	}
	*b = false
	return nil
}

// MarshalJSON always emits the STRING form ("true"/"false") so the persisted
// `preferences` object stays decodable by the map[string]string readers in
// gb prefs / gb prime / quick_spawn. omitempty on the struct field drops the
// key entirely when false, so a simple-mode blob carries no vibe_mode at all.
func (b flexBool) MarshalJSON() ([]byte, error) {
	if b {
		return []byte(`"true"`), nil
	}
	return []byte(`"false"`), nil
}

// Preferences mirrors the user bead's `preferences` JSON-typed field.
// JSON tags match the keys other agents look for (see ka-0000001673).
type Preferences struct {
	DefaultSpawnProject        string `json:"default_spawn_project,omitempty"`
	DefaultSpawnRole           string `json:"default_spawn_role,omitempty"`
	DefaultSpawnPromptTemplate string `json:"default_spawn_prompt_template,omitempty"`
	SpawnPerSelection          string `json:"spawn_per_selection,omitempty"`
	// VibeMode gates the full ("vibe") beads-viewer + Slack experience. Absent
	// or false ⇒ the simpler default surface (gb-ux epic ka-0000005059). Do NOT
	// invert the meaning: "doing nothing yields simple" is expressed by the
	// zero value (false), never by a simple_mode key.
	VibeMode flexBool `json:"vibe_mode,omitempty"`
	// Skin is the viewer's chosen look (skin_pref.go, gd-Ah9Zr8CxYB): "" when
	// never chosen, "auto" for the clock, else a skin name. The Look section
	// writes it on its own (POST /settings/skin); the preferences form below
	// carries it through unchanged.
	Skin string `json:"skin,omitempty"`
}

// parsePreferences extracts the preferences struct from a user bead's Fields
// map. The kbeads HTTP layer re-marshals nested JSON values to strings, so
// the `preferences` key (declared as a "json"-typed field on type:user) lands
// here as a serialized object. A missing or empty value yields the zero
// Preferences struct.
func parsePreferences(fields map[string]string) Preferences {
	var p Preferences
	raw := fields["preferences"]
	if raw == "" {
		return p
	}
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

// Identity mirrors the user bead's editable top-level identity fields. These
// are direct string fields on type:user (not nested in `preferences`); the
// settings page exposes them so a non-admin user can correct their own
// display name / pronouns / team / DM channel without an operator round-trip.
type Identity struct {
	DisplayName    string
	Pronouns       string
	Team           string
	SlackChannelID string
}

// parseIdentity extracts the identity struct from a user bead's Fields map.
// Each field is a plain string; missing keys yield the empty string.
func parseIdentity(fields map[string]string) Identity {
	return Identity{
		DisplayName:    fields["display_name"],
		Pronouns:       fields["pronouns"],
		Team:           fields["team"],
		SlackChannelID: fields["slack_channel_id"],
	}
}

// validateSlackChannelID accepts an empty string (clearing the field) or a
// canonical Slack ID — channel (C…), DM (D…), or group (G…) — composed of
// uppercase alphanumerics. Returns "" on success or a human-readable reason
// on failure.
func validateSlackChannelID(v string) string {
	if v == "" {
		return ""
	}
	if !slackChannelIDPattern.MatchString(v) {
		return `Slack channel id must look like a Slack ID (e.g. "C0123ABC" or "D0123ABC"), or be empty.`
	}
	return ""
}

// identityOversize returns the human-readable name of the first identity
// field that exceeds maxIdentityStringLen, or "" when all values are within
// bounds.
func identityOversize(i Identity) string {
	switch {
	case len(i.DisplayName) > maxIdentityStringLen:
		return "Display name"
	case len(i.Pronouns) > maxIdentityStringLen:
		return "Pronouns"
	case len(i.Team) > maxIdentityStringLen:
		return "Team"
	case len(i.SlackChannelID) > maxIdentityStringLen:
		return "Slack channel id"
	}
	return ""
}

// userBeadCache memoizes the viewer's user-bead lookup for the lifetime of a
// single request. A request may resolve the viewer's bead several times — a
// list-page handler for its default-project / pinned-set / VibeMode, plus the
// render seam injecting VibeMode into every full-page render (ka-0000007201) —
// and each resolution is an identical Types=[user] Kinds=[config] daemon scan.
// The cache collapses those to a single scan per email per request. It is
// installed on the request context by operatorMiddleware and shared as a
// pointer so every lookup within the request mutates the same map.
//
// A nil-but-present entry means "resolved, found no matching bead" — a real
// answer worth caching so anonymous-adjacent misses aren't re-scanned. Lookup
// errors are NOT cached: a transient daemon hiccup should not poison the rest
// of the request, and the caller falls back to the simple default anyway.
type userBeadCache struct {
	mu      sync.Mutex
	entries map[string]*beadsapi.BeadDetail
}

// userBeadCacheCtxKeyT keys the request-scoped userBeadCache. Pointer-keyed
// (matching agentResolverCtxKey) to avoid string-key collisions.
type userBeadCacheCtxKeyT struct{}

var userBeadCacheCtxKey = userBeadCacheCtxKeyT{}

// withUserBeadCache returns ctx with a fresh, empty userBeadCache attached.
// operatorMiddleware calls this once per request; findUserBeadByEmail then
// consults and populates it. Tests that exercise handlers directly (without
// the middleware chain) run with no cache — findUserBeadByEmail degrades to a
// direct, un-memoized lookup, which is correct, just not deduplicated.
func withUserBeadCache(ctx context.Context) context.Context {
	return context.WithValue(ctx, userBeadCacheCtxKey, &userBeadCache{
		entries: make(map[string]*beadsapi.BeadDetail),
	})
}

// userBeadCacheFromContext returns the request's cache, or nil when none was
// installed (e.g. background contexts, unit tests hitting handlers directly).
func userBeadCacheFromContext(ctx context.Context) *userBeadCache {
	if ctx == nil {
		return nil
	}
	c, _ := ctx.Value(userBeadCacheCtxKey).(*userBeadCache)
	return c
}

func (c *userBeadCache) get(email string) (*beadsapi.BeadDetail, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	b, ok := c.entries[email]
	return b, ok
}

func (c *userBeadCache) set(email string, bead *beadsapi.BeadDetail) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[email] = bead
}

// findUserBeadByEmail looks up a type=user kind=config bead whose `email`
// field matches the given email. Returns nil with no error when no match is
// found — the user provisioner only fires on the daemon's OIDC path, so a
// viewer-only request from a brand-new operator may legitimately precede the
// first user bead creation.
//
// The lookup is memoized per request via userBeadCache (installed by
// operatorMiddleware): the first caller in a request issues the daemon scan;
// every subsequent caller — including render()'s VibeMode injection — reuses
// the result instead of re-scanning. Absent a cache (unit tests, background
// contexts) it falls back to a direct lookup.
func (s *Server) findUserBeadByEmail(ctx context.Context, email string) (*beadsapi.BeadDetail, error) {
	if email == "" {
		return nil, nil
	}
	cache := userBeadCacheFromContext(ctx)
	if cache != nil {
		if bead, ok := cache.get(email); ok {
			return bead, nil
		}
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"user"},
		Kinds: []string{"config"},
		Limit: 200,
	})
	if err != nil {
		return nil, err
	}
	var match *beadsapi.BeadDetail
	if res != nil {
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			if b.Fields["email"] == email {
				match = b
				break
			}
		}
	}
	if cache != nil {
		cache.set(email, match)
	}
	return match, nil
}

// handleSettings renders the full settings page. Looks up the OIDC user's
// bead by email, parses the `preferences` field, and renders the read-only
// block. Anonymous viewers get a 401 — the page is a per-user surface and
// has no useful empty-state.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, err := s.findUserBeadByEmail(r.Context(), email)
	if err != nil {
		s.logger.Error("settings: looking up user bead", "email", email, "error", err)
		s.render(w, r, "settings.html", map[string]any{
			"Email":      email,
			"Error":      "Failed to load preferences.",
			"GardenHref": s.gardenBanner(r, retireSettings, gardenSettingsTarget),
		})
		return
	}
	data := map[string]any{
		"Email":      email,
		"Bead":       bead,
		"CanEdit":    bead != nil,
		"Skins":      viewerSkins,
		"GardenHref": s.gardenBanner(r, retireSettings, gardenSettingsTarget),
	}
	if bead != nil {
		data["Prefs"] = parsePreferences(bead.Fields)
		data["SkinChoice"] = skinChoice(parsePreferences(bead.Fields))
		data["Identity"] = parseIdentity(bead.Fields)
		data["Profile"] = buildProfileSectionView(parseProfilePref(bead.Fields))
	} else {
		data["Prefs"] = Preferences{}
		data["SkinChoice"] = skinChoice(Preferences{})
		data["Identity"] = Identity{}
		data["Profile"] = buildProfileSectionView(ProfilePref{})
	}
	s.render(w, r, "settings.html", data)
}

// handleSettingsView returns the read-only settings block fragment — used by
// the editor's Cancel button to restore the original render without
// reloading the page.
func (s *Server) handleSettingsView(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, err := s.findUserBeadByEmail(r.Context(), email)
	if err != nil {
		s.logger.Error("settings view: looking up user bead", "email", email, "error", err)
		http.Error(w, "Failed to load preferences", http.StatusInternalServerError)
		return
	}
	if bead == nil {
		http.Error(w, "User bead not found", http.StatusNotFound)
		return
	}
	s.renderSettingsBlock(w, r, bead, parsePreferences(bead.Fields), parseIdentity(bead.Fields), parseProfilePref(bead.Fields), http.StatusOK)
}

// handleSettingsEdit returns the inline settings editor partial pre-filled
// with the current preferences. Anonymous viewers get 401 (defense in depth
// — the Edit button itself is also gated on a user bead being present).
func (s *Server) handleSettingsEdit(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	bead, err := s.findUserBeadByEmail(r.Context(), email)
	if err != nil {
		s.logger.Error("settings edit: looking up user bead", "email", email, "error", err)
		http.Error(w, "Failed to load preferences", http.StatusInternalServerError)
		return
	}
	if bead == nil {
		http.Error(w, "User bead not found", http.StatusNotFound)
		return
	}
	s.renderSettingsEdit(w, r, parsePreferences(bead.Fields), parseIdentity(bead.Fields), parseProfilePref(bead.Fields), "", http.StatusOK)
}

// handleUpdateSettings persists new preferences and identity fields via
// UpdateBeadFieldsTyped on the user bead. Form values are trimmed;
// spawn_per_selection is validated against a closed set and silently coerced
// to "single" (the default) when not recognized; slack_channel_id is
// validated against a Slack-ID shape (or empty). The bead is always located
// by the OIDC requester's email, so no form-supplied target id can route a
// write to another user's bead. On success the read-only block is
// re-rendered so htmx can swap the editor back out. Daemon failures
// re-render the editor with the draft preserved + an inline error.
func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	bead, err := s.findUserBeadByEmail(r.Context(), email)
	if err != nil {
		s.logger.Error("settings update: looking up user bead", "email", email, "error", err)
		http.Error(w, "Failed to load preferences", http.StatusInternalServerError)
		return
	}
	if bead == nil {
		http.Error(w, "User bead not found", http.StatusNotFound)
		return
	}

	prefs := Preferences{
		DefaultSpawnProject:        strings.TrimSpace(r.PostForm.Get("default_spawn_project")),
		DefaultSpawnRole:           strings.TrimSpace(r.PostForm.Get("default_spawn_role")),
		DefaultSpawnPromptTemplate: r.PostForm.Get("default_spawn_prompt_template"),
		SpawnPerSelection:          strings.TrimSpace(r.PostForm.Get("spawn_per_selection")),
		// Standard unchecked-checkbox semantics: the browser submits
		// vibe_mode=on (or any value) only when checked, and omits the field
		// entirely when unchecked. Presence ⇒ true; absence ⇒ false (simple).
		VibeMode: flexBool(r.PostForm.Get("vibe_mode") != ""),
		// The Look section saves the skin on its own; the form has no skin
		// field, so keep the stored one rather than wiping it.
		Skin: parsePreferences(bead.Fields).Skin,
	}
	identity := Identity{
		DisplayName:    strings.TrimSpace(r.PostForm.Get("display_name")),
		Pronouns:       strings.TrimSpace(r.PostForm.Get("pronouns")),
		Team:           strings.TrimSpace(r.PostForm.Get("team")),
		SlackChannelID: strings.TrimSpace(r.PostForm.Get("slack_channel_id")),
	}
	// Preserve any non-section fields (e.g. PinnedBeadIDs) already on the
	// stored blob — the form below only carries section visibility + order.
	existingProfile := parseProfilePref(bead.Fields)
	profile := parseProfileFromForm(r.PostForm, existingProfile)
	// The garden's settings panel (settings_garden.go, Lands WB-13) posts
	// scope=garden with only the fields it edits: every field it does not send
	// keeps its stored value, and the homepage layout is not written.
	garden := r.PostForm.Get("scope") == gardenSettingsScope
	if garden {
		prefs, identity = gardenSettingsFromForm(r.PostForm, parsePreferences(bead.Fields), parseIdentity(bead.Fields))
		profile = existingProfile
	}

	if oversize := preferencesOversize(prefs); oversize != "" {
		s.renderSettingsEdit(w, r, prefs, identity, profile, oversize+" is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	if oversize := identityOversize(identity); oversize != "" {
		s.renderSettingsEdit(w, r, prefs, identity, profile, oversize+" is too long.", http.StatusRequestEntityTooLarge)
		return
	}
	if reason := validateSlackChannelID(identity.SlackChannelID); reason != "" {
		s.renderSettingsEdit(w, r, prefs, identity, profile, reason, http.StatusBadRequest)
		return
	}
	if !validSpawnPerSelection[prefs.SpawnPerSelection] {
		// Coerce unrecognized values to "" — the read-only template renders an
		// empty value as the implicit default ("single"), so this collapses to
		// the same UX without surfacing a noisy error.
		prefs.SpawnPerSelection = ""
	}

	profile = normalizeProfilePref(profile)
	profileJSON, err := encodeProfilePref(profile)
	if err != nil {
		s.logger.Error("settings update: encoding profile prefs", "bead_id", bead.ID, "error", err)
		s.renderSettingsEdit(w, r, prefs, identity, profile, "Failed to save homepage preferences.", http.StatusInternalServerError)
		return
	}

	fields := map[string]any{
		"preferences":      prefs,
		"display_name":     identity.DisplayName,
		"pronouns":         identity.Pronouns,
		"team":             identity.Team,
		"slack_channel_id": identity.SlackChannelID,
	}
	if !garden {
		fields["profile_prefs"] = profileJSON
	}
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), bead.ID, fields); err != nil {
		s.logger.Error("settings update: writing preferences", "bead_id", bead.ID, "actor", email, "error", err)
		s.renderSettingsEdit(w, r, prefs, identity, profile, "Failed to save preferences.", http.StatusBadGateway)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), bead.ID)
	if err != nil {
		s.logger.Warn("settings update: re-fetching user bead", "bead_id", bead.ID, "error", err)
		updated = bead
	}
	s.renderSettingsBlock(w, r, updated, prefs, identity, profile, http.StatusOK)
}

// preferencesOversize returns the human-readable name of the first
// preferences field that exceeds maxPrefStringLen, or "" when all values
// are within bounds.
func preferencesOversize(p Preferences) string {
	switch {
	case len(p.DefaultSpawnProject) > maxPrefStringLen:
		return "Default project"
	case len(p.DefaultSpawnRole) > maxPrefStringLen:
		return "Default role"
	case len(p.DefaultSpawnPromptTemplate) > maxPrefStringLen:
		return "Default prompt template"
	case len(p.SpawnPerSelection) > maxPrefStringLen:
		return "Spawn per selection"
	}
	return ""
}

// renderSettingsBlock writes the read-only preferences partial. CanEdit is
// always true here — callers reach this function only after confirming a
// user bead exists.
func (s *Server) renderSettingsBlock(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, prefs Preferences, identity Identity, profile ProfilePref, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/settings_block.html", "settings_block", map[string]any{
		"Bead":     bead,
		"Prefs":    prefs,
		"Identity": identity,
		"Profile":  buildProfileSectionView(profile),
		"CanEdit":  true,
	})
}

// renderSettingsEdit writes the editor partial, pre-filling fields with the
// in-flight draft. errMsg surfaces inline above the form.
func (s *Server) renderSettingsEdit(w http.ResponseWriter, r *http.Request, prefs Preferences, identity Identity, profile ProfilePref, errMsg string, status int) {
	if errMsg != "" && respondAction(w, r, "", actionFailure(errMsg, status)) {
		return
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/settings_edit.html", "settings_edit", map[string]any{
		"Prefs":    prefs,
		"Identity": identity,
		"Profile":  buildProfileSectionView(profile),
		"Error":    errMsg,
	})
}
