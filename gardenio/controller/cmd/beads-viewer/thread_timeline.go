package main

// Phase 2 — net-new interactive /threads view (epic ka-0000000679, task 2-5
// ka-0000006034). This file is the LIVE-UPDATE read path: the htmx polling
// fragment that refreshes the fused two-pane timeline (conversation + spawned
// work) WITHOUT a full page reload (rubric gate B12).
//
// Scope boundaries (so the Phase 2 slices stay decoupled):
//   - 2-3 (threads_routes.go) owns the full-page GET /threads/{id} render.
//   - 2-4 (threads_curation.go + thread_detail.html) owns the two-pane layout
//     and the conversation-first/work-collapsed curation default.
//   - 2-5 (this file + thread_compose.go's already-merged write path) owns the
//     interactive composer fragment AND ?since= polling. handleThreadTimeline
//     re-renders ONLY the timeline panes partial (thread_timeline.html), which
//     the full page also embeds, so the polled swap is byte-identical with the
//     initial server render — the same one-markup-path discipline widgets_live
//     and agents_row follow.
//
// The ?since= cursor and the no-op short-circuit follow handleBeadComments
// (server.go): the client carries the latest fused-entry timestamp in a
// data-attribute, echoes it on each poll, and the server skips re-rendering
// when nothing changed so an idle thread does no DOM swap and the access log
// doesn't fill with no-op renders. Because the cursor is the max timestamp
// across the WHOLE fused stream, a new spawned-work event advances it exactly as
// a new reply does — so the poll picks up conversation AND work activity (rubric
// B12), not just messages.
//
// We return 204 No Content (NOT 304) for the unchanged case. htmx 2.0.4's
// default responseHandling maps 204 to swap:false but classifies 304 under the
// [23].. "swap:true" rule — so a 304 with an empty body would swap NOTHING into
// the outerHTML target and ERASE the live region. (This is a real footgun the
// comments poller side-steps only because its 30s cadence rarely lands a 304 on
// a visible swap.) 204 is the htmx-blessed "leave the DOM alone" status, so the
// region survives an idle poll. The cursor still rides on the response header so
// the client keeps its data-attribute current.

import (
	"errors"
	"net/http"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// threadTimelineCursorHeader echoes the freshly-computed latest-activity cursor
// on every timeline response (including the 304), mirroring the
// X-Comments-Latest header the comments poller uses. The client reads it to keep
// its data-attribute cursor current even when the body is a 304 with no DOM.
const threadTimelineCursorHeader = "X-Thread-Latest"

// handleThreadTimeline serves the fused two-pane timeline as an htmx fragment
// for ?since= polling. Route: GET /threads/{id}/timeline. Read-only and
// viewer-scoped (same posture as the full /threads/{id} page); no write
// happens here, so it carries no operator gate — the composer's WRITE path
// (POST /b/{id}/thread-reply, thread_compose.go) is what stays behind
// operatorOnly.
//
// A missing head is a 404 (mirroring the full page). A timeline-fetch failure
// after the head resolves degrades to an error banner on the still-rendered
// panes rather than a 500, so a transient daemon hiccup never blanks the live
// region mid-poll.
func (s *Server) handleThreadTimeline(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(r.Context())
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing thread id", http.StatusBadRequest)
		return
	}

	head, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Thread not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting thread head for timeline poll", "id", id, "error", err)
		http.Error(w, "Failed to load thread", http.StatusInternalServerError)
		return
	}

	tl, ferr := fuseThreadTimeline(ctx, s.daemon, id)
	var bannerErr string
	if ferr != nil {
		s.logger.Error("fusing thread timeline for poll", "id", id, "error", ferr)
		tl = threadTimeline{ThreadID: id}
		bannerErr = "Failed to refresh thread timeline."
	}
	view := buildThreadDetailView(head, tl)
	view.Error = bannerErr

	latest := view.LatestActivity()
	latestAttr := commentsLatestAttr(latest)
	w.Header().Set(threadTimelineCursorHeader, latestAttr)

	// No-op short-circuit: the client's cursor already matches the server-side
	// latest, so there is nothing to swap. Only short-circuit a clean render —
	// an error banner must reach the client even when the timestamp is
	// unchanged. 204 (not 304) so htmx leaves the existing DOM intact instead of
	// swapping an empty body into the outerHTML target (see the file header).
	since := parseSinceParam(r.URL.Query().Get("since"))
	if bannerErr == "" && !latest.IsZero() && !since.IsZero() && latest.Equal(since) {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Cache-Control: no-store keeps proxies and bfcache from serving a stale
	// fragment to the next poll cycle (matches widgets_live).
	w.Header().Set("Cache-Control", "no-store")
	s.renderPartial(w, r, "templates/partials/thread_timeline.html", "thread_timeline", map[string]any{
		"Thread":      view,
		"ViewerEmail": email,
		// Viewer-tier compose gate (ka-0000003462), parity with the POST routes
		// and the full-page render in threads_routes.go.
		"CanCompose": viewerCanCompose(r) && email != "",
	})
}
