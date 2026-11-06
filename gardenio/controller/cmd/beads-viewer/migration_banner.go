package main

import (
	"net/http"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/oidcauth"
)

// migrationBannerMattEmailEnv is the env var that names the one OIDC user the
// migration banner must NOT render for (V-R2 §5.3 rule 2). Phase 0 hard-codes
// this against Matt's email until the V-R4 sunset trigger fires (P7). When
// unset, the banner is suppressed entirely — fail-closed so a dev/test viewer
// without operator config does not flash the banner to Matt himself.
const migrationBannerMattEmailEnv = "GARDENIO_VIEWER_MATT_EMAIL"

// migrationBannerDismissCookie is the cookie name set when the viewer
// dismisses the migration banner (V-R2 §5.4). Presence is the entire signal —
// the value is always "1" and carries no PII. HttpOnly + SameSite=Lax + Path=/
// + 1y Max-Age. Per-device by design; cross-device persistence is deferred to
// the V-R3 user_pref bead.
const migrationBannerDismissCookie = "gb_migration_banner_dismissed"

// migrationBannerDismissMaxAgeSeconds is the Max-Age (~1 year) on the dismiss
// cookie. Long enough that the banner doesn't reappear before the V-R4 sunset
// fires; short enough that it won't outlive a browser profile by years.
const migrationBannerDismissMaxAgeSeconds = 365 * 24 * 3600

// migrationBannerFlagKey is the config-bead field name (V-R4 §3.4) that
// operators flip to declare migration done. Lives on the beads-viewer
// component's tunables bead. Default true; flipping to false sunsets the
// banner across all viewers on the next ConfigWatcher poll without a deploy.
const migrationBannerFlagKey = "viewer.show_migration_banner"

// migrationBannerFlagEnv mirrors migrationBannerFlagKey at the env layer for
// the bead > env > compiled precedence stack ConfigWatcher applies. Set to
// "false" in a viewer Deployment to override the compiled default without a
// config bead. Compiled default is true (banner on in Phase 0).
const migrationBannerFlagEnv = "GARDENIO_VIEWER_SHOW_MIGRATION_BANNER"

// migrationBannerBackstop is the V-R4 §3.2 calendar backstop. The banner
// stops rendering on or after this date regardless of the config flag — a
// "we forgot to flip the flag" failsafe. Per the P7 task description this
// is a HARD GATE, evaluated independently of the flag (logical AND).
//
// UTC midnight; the comparison is against s.now() (the Server clock seam,
// defaulted to time.Now in NewServer), so the viewer process timezone does
// not matter and unit tests can pin a deterministic instant instead of
// depending on the real wall-clock.
var migrationBannerBackstop = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

// shouldRenderMigrationBanner reports whether the V-R2 §5 migration banner
// should render on a default-on surface (today: /active, /search). All gates
// from V-R2 §5.3 plus the V-R4 §3 sunset gate are evaluated here; the
// surface gate is enforced by the caller (only handleActive and handleSearch
// invoke this method).
//
// Gates evaluated:
//
//  1. OIDC-authenticated (viewer email is non-empty).
//  2. The viewer email is NOT Matt's (configured via
//     GARDENIO_VIEWER_MATT_EMAIL). When the env var is unset the function
//     returns false — fail-closed so the banner does not render for Matt
//     in environments where his identity hasn't been declared.
//  3. The viewer has not dismissed the banner on this device (cookie
//     gb_migration_banner_dismissed absent).
//  4. V-R4 §3 sunset has NOT fired: the operator-flippable flag
//     viewer.show_migration_banner is true (default true) AND today's date
//     (per the s.now() clock seam) is before migrationBannerBackstop
//     (2026-08-01). Both must hold — the calendar is a hard gate
//     independent of the flag (P7 task spec, V-R4 §5).
func (s *Server) shouldRenderMigrationBanner(r *http.Request) bool {
	email := strings.TrimSpace(oidcauth.EmailFromContext(r.Context()))
	if email == "" {
		return false
	}
	matt := strings.TrimSpace(os.Getenv(migrationBannerMattEmailEnv))
	if matt == "" {
		return false
	}
	if strings.EqualFold(email, matt) {
		return false
	}
	if c, err := r.Cookie(migrationBannerDismissCookie); err == nil && c.Value != "" {
		return false
	}
	if !s.tunables.GetBool(migrationBannerFlagKey, migrationBannerFlagEnv, true) {
		return false
	}
	if !s.now().Before(migrationBannerBackstop) {
		return false
	}
	return true
}

// handleMigrationBannerDismiss persists the V-R2 §5.4 dismissal as a
// HttpOnly per-device cookie and returns 204. The htmx button on the banner
// targets the banner element with an outerHTML/delete swap; combined with the
// hx-on::after-request fallback in the partial, the banner is removed from
// the page on success and stays absent on subsequent renders via the cookie
// gate in shouldRenderMigrationBanner.
func (s *Server) handleMigrationBannerDismiss(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     migrationBannerDismissCookie,
		Value:    "1",
		Path:     "/",
		MaxAge:   migrationBannerDismissMaxAgeSeconds,
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
