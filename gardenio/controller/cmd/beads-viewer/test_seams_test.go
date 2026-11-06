package main

// Test-only seams: thin compositions of production code that only tests call.
// They live here, not in the shipped binary, so deadcode stops reporting them
// (gd-R6phmQsoxD).

import (
	"context"
	"net/http"
	"sort"
	"time"
)

// buildRoleHub assembles the FULL role hub view-model for /role/<name>,
// agents included: the eager half (buildRoleHubEager) the page paints first
// plus the agents panel (buildRoleHubAgents) it lazy-loads as an HTMX
// fragment, so the agents-inclusive view-model stays testable in one call.
func (s *Server) buildRoleHub(ctx context.Context, role string, now time.Time) *roleHubView {
	view := s.buildRoleHubEager(ctx, role, now)
	if view.Name == "" {
		return view
	}
	view.AgentGroups, view.AgentCount = s.buildRoleHubAgents(ctx, role, now)
	return view
}

// feedSeedCards is feedSeedCardsAndUsers without the users.
func (s *Server) feedSeedCards(ctx context.Context, limit int) []feedBackfillCard {
	cards, _ := s.feedSeedCardsAndUsers(ctx, limit)
	return cards
}

// viewerVibeMode resolves the request's viewer to their VibeMode preference.
func (s *Server) viewerVibeMode(r *http.Request) bool {
	prefs, _ := s.viewerPrefs(r)
	return bool(prefs.VibeMode)
}

// auditThreadTimeline is auditThreadTimelineWithOptions with the zero-value
// options: the rubric's full reconciliation with no cursor scoping.
func auditThreadTimeline(ctx context.Context, store threadTimelineStore, headID string) (threadAuditReport, error) {
	return auditThreadTimelineWithOptions(ctx, store, headID, threadAuditOptions{})
}

// stats returns a snapshot of cache counters, so tests can confirm the cache
// is hitting on the second render.
func (h *codeHighlighter) stats() (hits, misses, size int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.hits, h.misses, len(h.cache)
}

// registeredWidgets returns the catalog slugs in lexical order.
func registeredWidgets() []string {
	out := make([]string, 0, len(widgetRegistry))
	for k := range widgetRegistry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// renderFeedCard renders one activity-feed card the way the live path does,
// deriving the project and relative time from the bead and now.
func renderFeedCard(basePath string, kind feedKind, b *feedEventBead, closedBy string, now time.Time) string {
	return renderFeedCardFull(basePath, kind, b, feedProjectFromBead(b), closedBy, formatFeedAgo(now), time.Time{}, nil)
}

// resetHtmxRendererCache empties the package-level htmx renderer cache so a
// malformed-template test doesn't poison subsequent runs.
func resetHtmxRendererCache() {
	htmxCache.mu.Lock()
	htmxCache.entries = map[string]htmxRendererCacheEntry{}
	htmxCache.mu.Unlock()
}

// threadMembers is threadMembersWithFieldSnapshot without the snapshots.
func threadMembers(ctx context.Context, store threadMemberStore, headID string) ([]string, error) {
	ids, _, err := threadMembersWithFieldSnapshot(ctx, store, headID)
	return ids, err
}

// SetNow overrides the server's wall-clock source so orderings derived from
// time.Now (e.g. /triggers next-fire sort) do not depend on the host clock.
func (s *Server) SetNow(now func() time.Time) {
	s.now = now
}

// MintHtmxWidgetToken returns a signed token binding (widget, instance, email)
// for the standard TTL, so tests can forge valid widget requests.
func (s *Server) MintHtmxWidgetToken(widget, instance, email string) string {
	if s.htmx == nil {
		return ""
	}
	return signHtmxToken(s.htmx.signer, widget, instance, email, time.Now().Add(htmxTokenTTL))
}
