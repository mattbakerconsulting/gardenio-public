package main

import (
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// cmdkResultLimit caps the number of suggestions in the command-palette
// fragment. Keeps responses small so each keystroke round-trip stays snappy.
const cmdkResultLimit = 10

// cmdkResult is the template view-model for one row in the palette overlay.
//
// Most rows are bead matches (Href => /<id>). Phase 3 adds agent-detail
// "action" rows that anchor to existing htmx-wired sections on the agent
// bead page (Href => /<id>#agent-keys etc.). The palette then navigates to
// that anchor; the section's own toolbar carries the hx-post — keeping the
// palette as sugar over the htmx routes rather than a parallel write API.
type cmdkResult struct {
	ID         string
	Title      string
	ShortTitle string // shown in place of Title when set (gd-V5nGseRS5f)
	Heraldry   string // the bead's arms, canonical; "" when none (arms.go)
	Type       string
	Status     string
	Href       string // optional override; default is /<ID>
}

// isPlanBead reports whether b is a plan: an epic carrying the `plan` marker
// label. Mirrors the daemon-side plan query in handlePlans (Types=[epic] AND
// Labels=[plan]) so a task/bug that merely carries a stray `plan` label — or a
// plain epic without it — never counts as a plan.
func isPlanBead(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Type == "epic" && hasLabel(b, planLabel)
}

// agentDetailActions returns the verb rows surfaced in the command palette
// while the operator is on an agent bead page. Each row anchors to the
// existing toolbar/form section on that page (type_agent.html), so the
// palette is sugar over the same htmx routes the toolbar already calls.
//
// The full agent page is served at the catch-all /<id> bead-show route
// (handleBeadShow -> type_agent.html) — there is NO GET /agents/{id} page —
// so the anchors target /<id>#agent-… where those sections actually live.
func agentDetailActions(basePath, beadID string) []cmdkResult {
	href := strings.TrimRight(basePath, "/") + "/" + beadID
	return []cmdkResult{
		{ID: "send-keys", Title: "Send named key (Esc/Tab/Ctrl-C…)", Type: "action", Href: href + "#agent-keys"},
		{ID: "nudge", Title: "Nudge agent (re-prompt parked agent)", Type: "action", Href: href + "#agent-nudge"},
		{ID: "respond", Title: "Respond to ask_user prompt", Type: "action", Href: href + "#agent-respond"},
		{ID: "open-transcript", Title: "Open agent transcript / events lane", Type: "action", Href: href + "#agent-events"},
		{ID: "catch-up", Title: "Catch up — scroll terminal to live tail", Type: "action", Href: href + "#agent-terminal"},
	}
}

// beadShowRefererID extracts the bead id from a Referer URL when the palette
// is invoked from a bead-show page (the catch-all /<id> route). Returns "" if
// the referer is absent, carries a scheme-only host, or isn't a single-segment
// /<id> path under basePath.
//
// A bead-show page URL is exactly <basePath>/<id> with no further path
// segments — so a nested path like /u/<slug>, /agents/<id>/prime, or
// /plans/<id> is rejected (it has an interior '/'). The looksLikeBeadID gate
// then filters out the literal single-segment routes (/agents, /feed, /plans,
// …) whose leaf isn't a gd-<id>. The caller confirms the bead is type=agent
// before surfacing agent-detail actions, so a task/bug/epic page never does.
func beadShowRefererID(basePath, referer string) string {
	if referer == "" {
		return ""
	}
	// Strip optional scheme://host prefix.
	if i := strings.Index(referer, "://"); i >= 0 {
		if j := strings.Index(referer[i+3:], "/"); j >= 0 {
			referer = referer[i+3+j:]
		} else {
			return ""
		}
	}
	// Strip query/hash.
	if i := strings.IndexAny(referer, "?#"); i >= 0 {
		referer = referer[:i]
	}
	prefix := strings.TrimRight(basePath, "/") + "/"
	rest, ok := strings.CutPrefix(referer, prefix)
	if !ok {
		return ""
	}
	// Bead-show is a single path segment (/<id>); anything with an interior
	// '/' is a nested route (/u/<slug>, /agents/<id>/prime, /plans/<id>, …).
	if strings.IndexByte(rest, '/') >= 0 {
		return ""
	}
	if rest == "" || !looksLikeBeadID(rest) {
		return ""
	}
	return rest
}

// matchesAgentAction returns true when q is a substring of any of the
// canonical agent-action ids/titles. Used to filter the action rows so a
// user typing "nud" still sees Nudge but typing "ka-12345…" doesn't see
// noise.
func matchesAgentAction(q string, a cmdkResult) bool {
	if q == "" {
		return true
	}
	ql := strings.ToLower(q)
	return strings.Contains(strings.ToLower(a.ID), ql) ||
		strings.Contains(strings.ToLower(a.Title), ql)
}

// handleCmdkSuggest serves GET /cmdk/suggest. Returns a small HTML fragment of
// <li>s for the command-palette overlay (Cmd-K / Ctrl-K). The fragment is
// rendered into the overlay's <ul> by htmx.
//
// Two probes feed the result list, deduped by ID:
//
//  1. When the query looks like a full bead identifier (gd-<10+ alnum>),
//     GetBead probes for an exact hit so it always lands at the top.
//  2. ListBeadsFiltered with Search:q reuses the same lexical machinery as
//     /search for title/description fragment matching.
//
// Empty query renders an empty-state hint without hitting the daemon.
func (s *Server) handleCmdkSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))

	// Agent-detail actions: when the palette is invoked from an agent bead
	// page, surface verb rows that anchor to the page's existing toolbars.
	// These always render at the top so the operator can hit Enter from a
	// fresh palette without typing. The agent page is served at the catch-all
	// /<id> route, so the referer is /<id> (never /agents/{id}); confirm the
	// referring bead is type=agent before offering agent-only actions so a
	// task/bug/epic page doesn't get send-keys/nudge/respond rows.
	var actions []cmdkResult
	if beadID := beadShowRefererID(s.basePath, r.Header.Get("Referer")); beadID != "" && s.refererIsAgent(r, beadID) {
		for _, a := range agentDetailActions(s.basePath, beadID) {
			if matchesAgentAction(q, a) {
				actions = append(actions, a)
			}
		}
	}

	if q == "" {
		s.renderPartial(w, r, "templates/partials/cmdk_results.html", "cmdk_results", map[string]any{
			"Query":   "",
			"Results": actions,
		})
		return
	}

	results := append(make([]cmdkResult, 0, cmdkResultLimit+len(actions)), actions...)
	seen := make(map[string]struct{}, cmdkResultLimit)
	for _, a := range actions {
		seen[a.ID] = struct{}{}
	}

	if looksLikeBeadID(q) {
		if b, err := s.daemon.GetBead(r.Context(), q); err == nil && b != nil {
			results = append(results, cmdkResult{ID: b.ID, Title: b.Title, ShortTitle: b.ShortTitle, Heraldry: bearerArms(b), Type: b.Type, Status: b.Status})
			seen[b.ID] = struct{}{}
		} else if err != nil {
			var apiErr *beadsapi.APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
				s.logger.Debug("cmdk suggest: id probe failed", "id", q, "error", err)
			}
		}
	}

	res, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Search: q,
		Limit:  cmdkResultLimit,
	})
	if err != nil {
		s.logger.Warn("cmdk suggest: lexical search failed", "query", q, "error", err)
	} else if res != nil {
		for _, b := range res.Beads {
			if b == nil {
				continue
			}
			if _, ok := seen[b.ID]; ok {
				continue
			}
			results = append(results, cmdkResult{ID: b.ID, Title: b.Title, ShortTitle: b.ShortTitle, Heraldry: bearerArms(b), Type: b.Type, Status: b.Status})
			seen[b.ID] = struct{}{}
			if len(results) >= cmdkResultLimit {
				break
			}
		}
	}

	s.renderPartial(w, r, "templates/partials/cmdk_results.html", "cmdk_results", map[string]any{
		"Query":   q,
		"Results": results,
	})
}

// refererIsAgent reports whether the referring bead (from a /<id> bead-show
// page) is a type=agent bead. Agent-detail palette actions (send-keys, nudge,
// respond, …) are only meaningful on an agent page, so this gates them out of
// the palette when the operator triggered it from a task/bug/epic bead page.
// A daemon error or a not-found bead degrades to false — no action rows —
// rather than surfacing dead anchors that would scroll to nothing.
func (s *Server) refererIsAgent(r *http.Request, beadID string) bool {
	b, err := s.daemon.GetBead(r.Context(), beadID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			s.logger.Debug("cmdk suggest: referer bead probe failed", "id", beadID, "error", err)
		}
		return false
	}
	return b != nil && b.Type == "agent"
}

// looksLikeBeadID returns true for queries that resemble a complete bead
// identifier (gd-<10+ alphanumerics>). Used to gate the GetBead probe so a
// lone "k" / "gd-" doesn't round-trip the daemon for every keystroke.
func looksLikeBeadID(q string) bool {
	rest, ok := strings.CutPrefix(q, "gd-")
	if !ok {
		rest, ok = strings.CutPrefix(q, "ka-")
	}
	if !ok || len(rest) < 10 {
		return false
	}
	for _, r := range rest {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}
