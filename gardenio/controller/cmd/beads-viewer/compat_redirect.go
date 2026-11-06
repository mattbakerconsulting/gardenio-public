package main

import (
	"net/http"
	"strings"
)

// legacyUIPages is a CLOSED allowlist mapping a legacy `/ui/<file>.html` page —
// the URL shape every escalation notification and every pre-viewer bookmark was
// emitted with — to the canonical beads-viewer route it should heal to.
//
// Only these exact page names redirect; every other input 404s. The allowlist
// (not any transformation of the request) is what keeps this route from
// becoming an open redirect or a wildcard 200: the key is the bare page base
// (no ".html", no path separators) and the value is a fixed, in-repo route
// string, never anything derived from the request path or query.
//
// Every value here is a top-level GET route registered in RegisterRoutes
// (verified against server.go). Parameterized/detail templates (bead_show,
// decision_detail, plan_detail, gardener_detail, user_home, …) are deliberately
// OMITTED: they never had a static `/ui/<file>.html` form and have no bare
// top-level route to land on, so redirecting them would just 404 (or worse,
// feed a bogus id into the /{id} bead-show catch-all).
//
// Background: ka-0000005003 / epic ka-0000000887. The bridge historically
// emitted https://<web>/beads/ui/escalations.html#ka-X for every escalation
// (bot_escalations.go, bot_escalation_operator_dm.go), but beads-viewer
// registers no `/ui/` and no `.html` route — so that entire backlog of
// already-sent operator links (sitting in Slack history and operator DMs) 404s
// forever. The emitter is fixed forward-only on ka-0000005895; this route heals
// the links that were ALREADY sent. escalations is the only shape ever emitted;
// the rest are included so a stray human bookmark of any top-level page heals
// too, at no extra risk (closed allowlist).
var legacyUIPages = map[string]string{
	"escalations":  "/escalations", // the confirmed backlog (bot_escalations.go)
	"index":        "/index",
	"feed":         "/feed",
	"search":       "/search",
	"active":       "/active",
	"formulas":     "/formulas",
	"decisions":    "/decisions",
	"extern":       "/extern",
	"agents":       "/agents",
	"telepathy":    "/telepathy",
	"cassettes":    "/cassettes",
	"advice":       "/advice",
	"triggers":     "/triggers",
	"costs":        "/costs",
	"charts":       "/charts",
	"dashboards":   "/dashboards",
	"health":       "/health",
	"trust":        "/trust",
	"inbox":        "/inbox",
	"people":       "/people",
	"gardeners":    "/gardeners",
	"captains":     "/gardeners", // pre-rename name (gd-5O9QUlA8zr)
	"maps":         "/maps",
	"roles":        "/roles",
	"plans":        "/plans",
	"deck":         "/deck",
	"threads":      "/threads",
	"ratings":      "/ratings",
	"up":           "/up",
	"views_index":  "/views",        // template base differs from its route
	"fleet_index":  "/fleet",        // template base differs from its route
	"peel_catalog": "/peel-catalog", // template base differs from its route
}

// handleLegacyUIRedirect heals legacy `/ui/<file>.html` links with a permanent
// redirect to the page's canonical route. It is registered as `GET /ui/{page}`,
// so {page} is always a single, path-cleaned segment (ServeMux collapses any
// `..` traversal before matching) — there is no way for a slash or a parent-dir
// payload to reach the allowlist lookup.
//
// The URL fragment (e.g. #ka-X on escalations.html) is never sent to the
// server, so this handler cannot see it or route on it. That is fine and
// intended: on a 3xx the browser re-attaches the original fragment to the
// Location on its own, so `/ui/escalations.html#ka-X` arrives client-side at
// `/escalations#ka-X`. The escalations page then resolves #ka-X to the exact
// bead (see templates/escalations.html) — a bare #ka-X does not match any anchor
// on that page by itself, and resolved escalations are not in the pending list.
func (s *Server) handleLegacyUIRedirect(w http.ResponseWriter, r *http.Request) {
	base, isHTML := strings.CutSuffix(r.PathValue("page"), ".html")
	if !isHTML {
		http.NotFound(w, r)
		return
	}
	route, ok := legacyUIPages[base]
	if !ok {
		http.NotFound(w, r)
		return
	}
	target := s.basePath + route
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// legacyGardenerRedirect returns a handler that heals pre-rename `captain` UI links with a
// permanent redirect to the gardener route that replaced them (rename plan
// gd-5O9QUlA8zr, phase 4; same failure class as the bare plan-publish 404,
// gd-Ip5pI9fhN8). Slack messages, escalation notifications and operator DMs
// that already link to /captains, /captains/<id> or /captain/<id> keep
// resolving instead of falling through to the GET /{id} bead-show catch-all.
//
// `to` is the fixed gardener-route prefix. It is registered for a closed set of fixed-literal patterns (see
// RegisterRoutes); the only request-derived input is the path remainder the
// pattern captured, and it is appended after a fixed in-repo prefix, so the
// Location can never leave this host. ServeMux has already path-cleaned the
// request, so the remainder carries no `..` or empty segment.
func (s *Server) legacyGardenerRedirect(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		target := s.basePath + to
		if name := r.PathValue("name"); name != "" {
			target = s.basePath + "/b/project/" + name + "/gardeners"
		}
		if rest := r.PathValue("rest"); rest != "" {
			target += "/" + rest
		}
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	}
}
