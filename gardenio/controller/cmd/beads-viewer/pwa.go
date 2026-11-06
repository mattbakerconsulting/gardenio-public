package main

import (
	"encoding/json"
	"net/http"
)

// PWA installability (task ka-0000008014, plan ka-0000000492 Step 6). This is
// the "no dedicated app" payoff: a web manifest + theme-color + touch icons let
// a phone "Add to Home Screen" and launch the Deck standalone (no browser
// chrome, own icon) while the thing shipping is still just a URL — no store, no
// review, no build, no install friction.
//
// The manifest is served dynamically (not from the embedded static FS) so
// start_url/scope/icon src carry the deployment's BASE_PATH. Under unifiedHost
// the viewer is mounted at a strip-prefix (e.g. /beads); a static file with
// hard-coded absolute paths would point the browser at the wrong origin path.
// The icon PNGs themselves are static (templates/static/pwa/*), served by the
// existing /assets/static/ FileServer.

// webManifestIcon is one entry of the manifest "icons" array.
type webManifestIcon struct {
	Src     string `json:"src"`
	Sizes   string `json:"sizes"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
}

// webManifest is the subset of the W3C web app manifest we emit. Fields are
// ordered for a readable served document; encoding/json preserves struct order.
type webManifest struct {
	Name            string            `json:"name"`
	ShortName       string            `json:"short_name"`
	Description     string            `json:"description"`
	StartURL        string            `json:"start_url"`
	Scope           string            `json:"scope"`
	Display         string            `json:"display"`
	Orientation     string            `json:"orientation"`
	ThemeColor      string            `json:"theme_color"`
	BackgroundColor string            `json:"background_color"`
	Icons           []webManifestIcon `json:"icons"`
}

// pwaThemeColor is the standalone status-bar / title-bar tint: the top band
// of the Homestead day sky (skin_pref.go, gd-Ah9Zr8CxYB), the default look, so
// the installed app's chrome runs straight into the sky under it. It is the
// server-rendered <meta name="theme-color"> in layout.html too; SKIN_CLOCK
// retints that meta to the night sky after dusk (skin_clock.go). The manifest
// has one colour, so it carries the day's.
const pwaThemeColor = homesteadSkyDay

// pwaBackgroundColor is the splash-screen background shown before first paint:
// the sky's fallback, which <html> paints before the scene layers arrive, so
// the splash hands over to the page without a flash.
const pwaBackgroundColor = homesteadSkyFallbackDay

// handleWebManifest serves the BASE_PATH-aware web app manifest.
// Content-Type application/manifest+json is what browsers expect for a
// <link rel="manifest">; a plain application/json is tolerated but not spec.
func (s *Server) handleWebManifest(w http.ResponseWriter, _ *http.Request) {
	bp := s.basePath // "" or a leading-slash prefix like "/beads" (never trailing-slash)
	m := webManifest{
		Name:        "Gardenio Deck",
		ShortName:   "Deck",
		Description: "Swipe through plans, decisions, and escalations that need a human — one tap per answer.",
		// start_url is the Deck itself — the whole point of installing is to
		// land on the triage surface, not the generic homepage.
		StartURL: bp + "/deck",
		// scope keeps in-app navigation (deep links to /plans/{id}, /b/{id})
		// inside the standalone window instead of kicking out to the browser.
		Scope:           bp + "/",
		Display:         "standalone",
		Orientation:     "portrait-primary",
		ThemeColor:      pwaThemeColor,
		BackgroundColor: pwaBackgroundColor,
		Icons: []webManifestIcon{
			{Src: bp + "/assets/static/pwa/icon-192.png", Sizes: "192x192", Type: "image/png", Purpose: "any"},
			{Src: bp + "/assets/static/pwa/icon-512.png", Sizes: "512x512", Type: "image/png", Purpose: "any"},
			// A maskable icon lets Android crop to its adaptive-icon shape
			// without clipping the card motif (the art is inside the safe zone).
			{Src: bp + "/assets/static/pwa/icon-maskable-512.png", Sizes: "512x512", Type: "image/png", Purpose: "maskable"},
		},
	}
	w.Header().Set("Content-Type", "application/manifest+json")
	// The manifest changes only on deploy; let the browser cache it briefly so a
	// home-screen launch doesn't refetch it every cold start.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	// Indent for a legible served document — it is fetched rarely (install +
	// occasional revalidate), so the few extra bytes are a non-issue.
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		// Encoding a fixed struct cannot fail in practice; if the client
		// connection dropped mid-write there is nothing to recover.
		s.logger.Error("failed to encode web manifest", "error", err)
	}
}
