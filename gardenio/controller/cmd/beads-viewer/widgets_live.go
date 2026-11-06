package main

// Phase 8 — live-fragment routes for the four "live" widgets.
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 8.
// Bead: ka-0000007379.
//
// Each route returns the same widget HTML the rich-report page renders, so
// htmx swap-in is bit-identical with the initial server render. D5: htmx
// polling per phase decision (ka-0000006041).
//
// Route shape: GET /widgets/{slug}/{bead}
//   slug — the widget renderer slug (e.g. "service-health"); must be in the
//          live-allowlist below so we don't accidentally expose every widget
//          renderer as a public endpoint.
//   bead — the widget-instance bead id; we fetch that bead's `data` field and
//          dispatch through the same registry the rich-report page uses.
//
// The cross-filter query string is forwarded as `?filter=<encoded>` and
// applied server-side for widget kinds that benefit (timeline). Other widgets
// filter purely client-side; the URL param is harmless if present.

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// liveWidgetPolicy describes how a live widget should be polled. The
// production rich-report page renders these as `hx-get="..." hx-trigger="every Xs"`
// attributes; the gallery dev page exercises the same routes against fixture
// beads so the swap path is testable without a real incident.
type liveWidgetPolicy struct {
	Slug          string
	IntervalSecs  int
	ServerFilters bool // true: server applies cross-filter to the payload
}

// liveWidgetPolicies enumerates the four live widgets named in the synthesis
// doc. Anything not in this list cannot be polled — the route returns 404.
// Defaults: 5s for header / service-health / activity-log; 10s for
// event-timeline (denser data, less benefit from sub-10s refresh).
var liveWidgetPolicies = map[string]liveWidgetPolicy{
	"header-incident-summary": {Slug: "header-incident-summary", IntervalSecs: 5, ServerFilters: false},
	"service-health":          {Slug: "service-health", IntervalSecs: 5, ServerFilters: false},
	"activity-log":            {Slug: "activity-log", IntervalSecs: 5, ServerFilters: false},
	"event-timeline":          {Slug: "event-timeline", IntervalSecs: 10, ServerFilters: true},
}

// liveAllowlist returns the slugs the live route serves. Used by tests +
// gallery to enumerate fixtures.
func liveAllowlist() []string {
	out := make([]string, 0, len(liveWidgetPolicies))
	for k := range liveWidgetPolicies {
		out = append(out, k)
	}
	return out
}

// handleLiveWidget serves a single widget's HTML fragment. Path:
// GET /widgets/{slug}/{bead}. Returns 404 for unknown slugs (not on the
// allowlist) or unknown beads. Errors render as a small static error chip
// rather than failing the request — htmx will retry on the next poll.
func (s *Server) handleLiveWidget(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	beadID := strings.TrimSpace(r.PathValue("bead"))
	if slug == "" || beadID == "" {
		http.NotFound(w, r)
		return
	}
	renderer, ok := lookupWidget(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}
	policy, allowlisted := liveWidgetPolicies[slug]

	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
	defer cancel()

	bead, err := s.daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		writeLiveError(w, "bead unavailable")
		return
	}
	mezmoRaw := bead.Fields["mezmo_source"]
	source, hasMezmo := parseMezmoSource(mezmoRaw)
	// A slug must be either on the legacy live allowlist OR carry a
	// mezmo_source on the bead. This keeps the surface tight (a random
	// renderer can't be polled just by guessing the URL) while still letting
	// the Mezmo path lift any registered widget into the live route.
	if !allowlisted && !hasMezmo {
		http.NotFound(w, r)
		return
	}
	raw := bead.Fields["data"]
	if raw == "" && !hasMezmo {
		writeLiveError(w, "no data on widget bead")
		return
	}
	wctx := s.newWidgetRenderCtx()
	if hasMezmo {
		raw = applyMezmoSource(ctx, raw, source, slug, wctx.mezmo, wctx.mezmoCache)
	}
	if policy.ServerFilters {
		raw = applyServerCrossFilter(raw, slug, r.URL.Query().Get("filter"))
	}

	html, rerr := renderer(raw, wctx)
	if rerr != nil {
		writeLiveError(w, "render error")
		return
	}

	// Cache-Control: no-store keeps proxies and browser bfcache from serving
	// stale fragments to the next poll cycle.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// writeLiveError renders a tiny inline error chip the widget host can show
// without breaking layout. The chip carries a `data-state="error"` attr so
// CSS can style it; htmx replaces it on the next successful poll.
func writeLiveError(w http.ResponseWriter, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<div class="ii-w-live-error" data-state="error">` + escapeText(msg) + `</div>`))
}

// escapeText is the small subset of html escaping we need for live error
// messages. Avoids pulling html/template overhead for a one-off plain string.
func escapeText(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// liveAttrs returns the htmx attribute string for a given widget instance
// when polling is on. Templates use this via the `liveAttrs` template func so
// the same code path produces both the rich-report page and the gallery demo.
func liveAttrs(slug, beadID, basePath string) string {
	policy, ok := liveWidgetPolicies[slug]
	if !ok || beadID == "" {
		return ""
	}
	return liveAttrsFor(slug, beadID, basePath, policy.IntervalSecs)
}

// liveAttrsFor renders the htmx polling attributes for the given cadence.
// Used by the rich-report template for cards with a per-card LiveIntervalSecs
// (mezmo-sourced widgets pick their own cadence; legacy live widgets still
// route through liveAttrs above for the gallery view).
func liveAttrsFor(slug, beadID, basePath string, intervalSecs int) string {
	if slug == "" || beadID == "" || intervalSecs <= 0 {
		return ""
	}
	return ` hx-get="` + basePath + `/widgets/` + slug + `/` + beadID +
		`" hx-trigger="every ` + itoa(intervalSecs) + `s"` +
		` hx-swap="innerHTML" hx-vals='js:{filter: window.IncidentIntel && window.IncidentIntel.crossFilter ? window.IncidentIntel.crossFilter.serialize() : ""}'`
}

// liveIntervalForCard picks the polling cadence for a widget card. Cards on
// the legacy Phase 8 allowlist keep their per-slug cadence. Cards sourced
// from Mezmo (ka-0000002519) use the active/archived mode default. Cards
// with neither return 0 and the template suppresses hx-* attrs.
func liveIntervalForCard(slug string, hasMezmo bool, mode mezmoLiveMode) int {
	if hasMezmo {
		return mezmoLiveIntervalSecs(mode)
	}
	if policy, ok := liveWidgetPolicies[slug]; ok {
		return policy.IntervalSecs
	}
	return 0
}

// itoa is a tiny string-int helper to avoid pulling fmt for one call site.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
