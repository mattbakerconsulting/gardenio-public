package main

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/oidcauth"
)

// inboxHomepageBannerDismissCookie is the cookie set when the viewer dismisses
// the /inbox "moved to your homepage" banner (ka-0000002514, M2 item 5). Per
// device — value is always "1", no PII. HttpOnly + SameSite=Lax + Path=/ +
// ~1y Max-Age. Mirrors migrationBannerDismissCookie semantics.
const inboxHomepageBannerDismissCookie = "gb_inbox_homepage_banner_dismissed"

// inboxHomepageBannerDismissMaxAgeSeconds matches the migration banner's
// ~1y horizon — long enough that the banner doesn't reappear before /u/me
// adoption stabilizes.
const inboxHomepageBannerDismissMaxAgeSeconds = 365 * 24 * 3600

// inboxHomepageBannerFlagKey is the config-bead tunable that operators flip
// to suppress the banner across all viewers without a deploy. Default true
// during M2 rollout.
const inboxHomepageBannerFlagKey = "viewer.show_inbox_homepage_banner"

// inboxHomepageBannerFlagEnv mirrors inboxHomepageBannerFlagKey at the env
// layer for the bead > env > compiled precedence stack ConfigWatcher applies.
const inboxHomepageBannerFlagEnv = "GARDENIO_VIEWER_SHOW_INBOX_HOMEPAGE_BANNER"

// shouldRenderInboxHomepageBanner reports whether the /inbox homepage-pointer
// banner should render. Gates:
//
//  1. Viewer is OIDC-authenticated (email non-empty) — /u/me redirects via
//     slug resolution, which needs an identity to point at.
//  2. Viewer has not dismissed on this device (cookie absent).
//  3. Operator-flippable flag viewer.show_inbox_homepage_banner is true.
func (s *Server) shouldRenderInboxHomepageBanner(r *http.Request) bool {
	if strings.TrimSpace(oidcauth.EmailFromContext(r.Context())) == "" {
		return false
	}
	if c, err := r.Cookie(inboxHomepageBannerDismissCookie); err == nil && c.Value != "" {
		return false
	}
	if !s.tunables.GetBool(inboxHomepageBannerFlagKey, inboxHomepageBannerFlagEnv, true) {
		return false
	}
	return true
}

// handleInboxHomepageBannerDismiss persists the dismissal as a HttpOnly
// per-device cookie and returns 204. The htmx button on the banner swaps
// outerHTML on the banner element, so the banner is removed from the page
// on success and stays absent on subsequent renders via the cookie gate.
func (s *Server) handleInboxHomepageBannerDismiss(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     inboxHomepageBannerDismissCookie,
		Value:    "1",
		Path:     "/",
		MaxAge:   inboxHomepageBannerDismissMaxAgeSeconds,
		HttpOnly: true,
		Secure:   cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	w.WriteHeader(http.StatusNoContent)
}
