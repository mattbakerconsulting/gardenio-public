package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"gardenio/controller/internal/oidcauth"
)

// The skin preference (gd-Ah9Zr8CxYB, plan gd-JaMxRKsc8z step 4.2). The skin
// a viewer picks is a server-side preference, the `skin` key of the user
// bead's `preferences` blob, so it follows them to every device. The browser's
// beads-viewer:skin key (skin_clock.go) stays the pre-paint cache: render()
// hands the stored value to SKIN_CLOCK.start in <head>, which adopts it into
// the cache before the first paint, and pages with no viewer (/login, /lands)
// keep painting from the cache alone.
//
// Values: "" means never chosen (the cache decides, as before 4.2), "auto"
// means day or night by the local clock, and anything else names a skin.
// Choosing on the Settings page's Look section or in the nav switcher stores
// it through POST /settings/skin.

// viewerSkin is one entry of the Look section and of the nav switcher. The
// four colours paint the Look card's little preview: the sky behind the
// plate, the plate's paper, its ink and its accent.
type viewerSkin struct {
	Value, Label, Blurb     string
	Sky, Paper, Ink, Accent string
}

// The Homestead sky, the top band of meadow.css's .gd-sky gradient by day and
// by night. It is the PWA theme-color (pwa.go): the status bar sits on the
// sky's top edge, so the installed app's chrome runs straight into it. The
// splash is the sky's fallback (homestead.css --gd-sky-fallback), the colour
// <html> paints before the scene layers arrive, so the launch is seamless.
const (
	homesteadSkyDay           = "#5f98d6"
	homesteadSkyNight         = "#0b1030"
	homesteadSkyFallbackDay   = "#9ccbea"
	homesteadSkyFallbackNight = "#1b2455"
)

// viewerSkins is the closed set, in switcher order: Auto, the Homestead pair,
// then Dense admin and friends (TestLayout_SkinSwitcherOffersHomestead). The
// nav <select> in layout.html lists the same values (TestSkinPref_SwitcherMatchesSet).
var viewerSkins = []viewerSkin{
	{Value: "auto", Label: "Auto", Blurb: "Homestead by day, Homestead night after dusk, by your clock.", Sky: homesteadSkyDay, Paper: "#fffdf6", Ink: "#1a2036", Accent: homesteadSkyNight},
	{Value: "homestead", Label: "Homestead", Blurb: "Sky, meadow and a vine round the page.", Sky: homesteadSkyDay, Paper: "#fffdf6", Ink: "#000000", Accent: "#1f6b45"},
	{Value: "dark-homestead", Label: "Homestead night", Blurb: "Moon, stars and fireflies.", Sky: homesteadSkyNight, Paper: "#1a2036", Ink: "#f3ecd8", Accent: "#ffd23f"},
	{Value: "baseline", Label: "Baseline", Blurb: "Plain white page, browser defaults.", Sky: "#ffffff", Paper: "#ffffff", Ink: "#000000", Accent: "#0645ad"},
	{Value: "minimal-refined", Label: "Minimal refined", Blurb: "Quiet serif, warm off-white.", Sky: "#f4f1ea", Paper: "#fefdfb", Ink: "#1a1a1a", Accent: "#3b5a7a"},
	{Value: "dense-admin", Label: "Dense admin", Blurb: "Tight rows, more on screen.", Sky: "#f3f4f6", Paper: "#ffffff", Ink: "#111827", Accent: "#1d4ed8"},
	{Value: "docs-notion", Label: "Docs / Notion", Blurb: "Roomy document style.", Sky: "#f7f6f3", Paper: "#ffffff", Ink: "#2f3135", Accent: "#b75f28"},
	{Value: "dark-terminal", Label: "Dark terminal", Blurb: "Monospace on near-black.", Sky: "#05080c", Paper: "#0b0f14", Ink: "#d6e0ec", Accent: "#6ee7b7"},
	{Value: "tufte", Label: "Tufte", Blurb: "Book serif on cream.", Sky: "#f2f1e4", Paper: "#fffff8", Ink: "#111111", Accent: "#8a1c1c"},
}

// validSkinPref reports whether v is a value POST /settings/skin accepts.
func validSkinPref(v string) bool {
	for _, s := range viewerSkins {
		if s.Value == v {
			return true
		}
	}
	return false
}

// skinChoice is the Look section's checked entry: the stored skin, or Auto
// when none is stored (the page's script corrects it to a cached pick).
func skinChoice(p Preferences) string {
	if validSkinPref(p.Skin) {
		return p.Skin
	}
	return "auto"
}

// handleUpdateSkin stores the viewer's skin: POST /settings/skin, form field
// skin. It answers 204 so the Look section and the nav switcher can fire it
// and keep going; the skin is already on screen (SKIN_CLOCK.set) before the
// request leaves. The bead is found by the OIDC requester's email, as on
// POST /settings, so no form value can aim the write at someone else.
//
// Only the `skin` key changes: the stored blob is merged as a map, not
// rebuilt from Preferences, so keys this viewer does not model (written by
// `gb prefs set`) survive.
func (s *Server) handleUpdateSkin(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	skin := strings.TrimSpace(r.PostForm.Get("skin"))
	if !validSkinPref(skin) {
		http.Error(w, "Unknown skin", http.StatusBadRequest)
		return
	}
	bead, err := s.findUserBeadByEmail(r.Context(), email)
	if err != nil {
		s.logger.Error("skin update: looking up user bead", "email", email, "error", err)
		http.Error(w, "Failed to load preferences", http.StatusInternalServerError)
		return
	}
	if bead == nil {
		http.Error(w, "User bead not found", http.StatusNotFound)
		return
	}
	blob := map[string]any{}
	if raw := bead.Fields["preferences"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &blob)
		if blob == nil {
			blob = map[string]any{}
		}
	}
	blob["skin"] = skin
	if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), bead.ID, map[string]any{"preferences": blob}); err != nil {
		s.logger.Error("skin update: writing preferences", "bead_id", bead.ID, "actor", email, "error", err)
		http.Error(w, "Failed to save the skin", http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
