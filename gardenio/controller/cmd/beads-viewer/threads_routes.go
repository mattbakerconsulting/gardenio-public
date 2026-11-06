package main

// Phase 2 — net-new interactive /threads view (epic ka-0000000679, task 2-3
// ka-0000003348). This file is the ROUTE / HANDLER layer: it wires two HTTP
// handlers on top of the route-free aggregator layer that landed in 2-1
// (threads.go, MR !4932) — threadList for the index, fuseThreadTimeline /
// threadMembers for the detail.
//
// Scope boundaries (so the Phase 2 slices don't collide):
//   - 2-3 (this file): register GET /threads + GET /threads/{id}, wire them to
//     the aggregators, and prove via a regression test that the two patterns do
//     not shadow (or get shadowed by) the existing route table — most pointedly
//     the catch-all GET /{id} (handleBeadShow). Both handlers are READ-ONLY: the
//     index lists wishes, the detail renders a fused timeline. There is NO write
//     surface here, so the rubric's operatorOnly-on-writes gate is satisfied by
//     construction — these are plain viewer-scoped HandleFuncs, exactly like
//     /gardeners, /roles, and /u/{slug}.
//   - 2-4 (ka-0000006184) OWNS the polished two-pane templates (conversation
//     first, work collapsed) and the curation default. The templates this file
//     ships are a deliberately minimal, real-data baseline so the routes render
//     end-to-end (rubric gate B10: "actually renders, not a stub"); 2-4 enriches
//     templates/threads.html + templates/thread_detail.html in place.
//   - 2-5 (ka-0000006034) OWNS the interactive composer fragment and ?since=
//     polling. This file intentionally does not implement either.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// handleThreads renders the /threads index: one row per wish (thread head),
// most-recently-active first. Read-only and viewer-scoped — no role gate, the
// same posture as the rest of the authenticated viewer surface. The optional
// ?project=<name> query narrows the enumeration to that project's
// project:<name> label (threadList honours an empty project as "fleet-wide").
func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(r.Context())
	project := r.URL.Query().Get("project")

	view := threadIndexView{Project: project}
	summaries, err := threadList(ctx, s.daemon, project)
	if err != nil {
		s.logger.Error("listing threads", "project", project, "error", err)
		view.Error = "Failed to load threads."
	} else {
		view.Threads = summaries
	}

	s.render(w, r, "threads.html", map[string]any{
		"Threads":     view,
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(ctx, email),
		// CanCompose gates the new-thread composer (ka-0000007860): viewerOnly
		// parity with POST /threads/compose (viewerCanCompose mirrors the route
		// gate) AND an authenticated identity to attribute the mint to. Without
		// this parity the index could show a composer the POST 403s, or hide one
		// the route would accept — the same gate the detail-page reply composer
		// uses (handleThreadDetail). Viewer-tier (not operator) since ka-0000003462:
		// posting to a thread is the same trust level as commenting on a bead.
		"CanCompose": viewerCanCompose(r) && email != "",
		// Project echoes the ?project= scope so the composer can pre-fill the
		// project field when the operator is already looking at a scoped index.
		"Project": project,
	})
}

// handleThreadDetail renders /threads/{id}: the wish head plus the fused
// timeline across every member bead (field ∪ parent-child closure, dep-first).
// Read-only and viewer-scoped. A missing head is a 404 (mirroring
// handleBeadShow); an aggregator error after the head resolves degrades to an
// error banner on a still-rendered page rather than a 500, so a transient
// daemon hiccup never blanks the surface.
func (s *Server) handleThreadDetail(w http.ResponseWriter, r *http.Request) {
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
		s.logger.Error("getting thread head", "id", id, "error", err)
		http.Error(w, "Failed to load thread", http.StatusInternalServerError)
		return
	}

	// On a timeline failure the head still renders: fall back to an empty
	// timeline (just the head, no member rows) and surface a banner, rather than
	// 500ing — a transient daemon hiccup must not blank a wish that resolved.
	tl, err := fuseThreadTimeline(ctx, s.daemon, id)
	var bannerErr string
	if err != nil {
		s.logger.Error("fusing thread timeline", "id", id, "error", err)
		tl = threadTimeline{ThreadID: id}
		bannerErr = "Failed to load thread timeline."
	}
	view := buildThreadDetailView(head, tl)
	view.Error = bannerErr

	// Produced MRs: the concrete code a wish's members shipped (gb-ux follow-up
	// ka-0000003947). MR beads are reached via forward `produces` edges out of
	// the resolved member set, not by membership, so the timeline never shows
	// them. Walk them here (two batched read queries over tl.Members) and hang
	// them on the view for the rail to render. A walk failure degrades to an
	// omitted section — losing the MR pills must never blank a wish that
	// resolved — so the error is logged, not surfaced as a banner.
	if len(tl.Members) > 0 {
		mrs, mErr := threadProducedMRs(ctx, s.daemon, tl.Members)
		if mErr != nil {
			s.logger.Warn("walking produced MRs for thread", "id", id, "error", mErr)
		} else {
			view.ProducedMRs = mrs
		}
	}

	s.render(w, r, "thread_detail.html", map[string]any{
		"Thread":      view,
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(ctx, email),
		// CanCompose drives the interactive composer (2-5): viewerOnly parity
		// (viewerCanCompose mirrors the route gate on POST /b/{id}/thread-reply)
		// AND an authenticated identity to attribute the send to. Viewer-tier
		// (not operator) since ka-0000003462 so live AVA users can see the box.
		"CanCompose": viewerCanCompose(r) && email != "",
	})
}

// handleThreadAudit serves GET /threads/{id}/audit: a read-only, viewer-scoped
// JSON reconciliation that PROVES the fused timeline for the wish headed by {id}
// is complete, correct, and in-order (Phase 3 rubric ka-0000007635 §A, task
// ka-0000005790). It is the runtime counterpart to the aggregator's unit tests:
// pointable at any live thread, it returns a structured PASS/FAIL with the
// divergent entries called out.
//
// READ-ONLY by construction. Like /threads and /threads/{id} this is a plain
// viewer-scoped HandleFunc — NOT operator-gated — because it has NO write
// surface: auditThreadTimeline only issues the same read queries the detail
// page does (ListBeadsFiltered / BatchGetDependencies / BatchGetBeads /
// GetComments) and mutates nothing. The rubric's "operatorOnly on every write"
// gate (A6) is satisfied vacuously; the rubric explicitly requires this
// endpoint to be read-only and viewer-scoped, with no operator gate.
//
// The response is always JSON (this is an audit/diagnostic endpoint, not a
// rendered page). A missing head is a 404 (mirroring handleThreadDetail); a
// non-404 head-resolve failure is a 500; an aggregator failure AFTER the head
// resolves is a 502 (the daemon hiccuped mid-reconciliation) rather than a
// misleading PASS/FAIL over partial data.
func (s *Server) handleThreadAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing thread id", http.StatusBadRequest)
		return
	}

	// Resolve the head first so a non-existent wish is a clean 404 rather than
	// an empty PASS over zero members.
	if _, err := s.daemon.GetBead(ctx, id); err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Thread not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting thread head for audit", "id", id, "error", err)
		http.Error(w, "Failed to load thread", http.StatusInternalServerError)
		return
	}

	// Honour `?since=<rfc3339>` + `?prior_fingerprint=<hex>` so the prefix-
	// stability gate (rubric O5, ka-0000003162 3-4) can be exercised at a real
	// polling instant — the same cursor the client carried into a /threads/{id}/
	// timeline poll. A bad/absent `since` falls back to the zero-cursor full
	// snapshot audit (the rubric's default reconciliation), so a malformed
	// cursor never 500s the audit endpoint.
	opts := threadAuditOptions{
		Since:            parseSinceParam(r.URL.Query().Get("since")),
		PriorFingerprint: strings.TrimSpace(r.URL.Query().Get("prior_fingerprint")),
	}
	report, err := auditThreadTimelineWithOptions(ctx, s.daemon, id, opts)
	if err != nil {
		// A daemon read failed mid-reconciliation: the report would be over
		// partial data, so surface a hard error instead of a misleading verdict.
		s.logger.Error("auditing thread timeline", "id", id, "error", err)
		http.Error(w, "Failed to audit thread timeline", http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		s.logger.Error("encoding thread audit json", "id", id, "error", err)
	}
}

// --- View models -----------------------------------------------------------

// threadIndexView is the /threads index view model: the sorted wish rows plus a
// project scope echo and an optional degraded-render error banner.
type threadIndexView struct {
	// Threads are the wish rows, already sorted by threadList (most-recently
	// active first, tie-broken by id).
	Threads []threadSummary
	// Project echoes the ?project= scope so the template can show "threads in
	// <project>" and keep the filter in links; empty means fleet-wide.
	Project string
	// Error is a non-empty banner string when the enumeration failed; the page
	// still renders (degraded) rather than 500ing.
	Error string
}

// HasThreads reports whether any wish rows are present — a small template
// convenience so the empty state is a single {{if}} rather than a len pipeline.
func (v threadIndexView) HasThreads() bool { return len(v.Threads) > 0 }

// threadDetailView is the /threads/{id} detail view model. It is a flat
// projection of the head bead plus the fused timeline, shaped so the baseline
// template (and 2-4's two-pane successor) can render without touching the
// aggregator structs directly.
type threadDetailView struct {
	ThreadID string
	// Title is the head bead's title.
	Title string
	// WishText is the user's wish — slack_root_text when present, else the
	// title (threadWishText). The clearest "what was asked for" line.
	WishText string
	// Status is the head bead's lifecycle status.
	Status string
	// Project is the head's project (project:<name> with prefix stripped).
	Project string
	// Members are the member beads in dep-first order (the "work" pane that 2-4
	// collapses by default). Element 0 is the head itself.
	Members []threadMemberRow
	// Timeline is the fused, chronologically-sorted event stream across every
	// member (the "conversation" pane 2-4 leads with).
	Timeline []threadTimelineEntry
	// ProducedMRs are the merge requests this wish's members produced (via
	// forward `produces` dep edges), projected onto the shared mr_pill shape
	// (gb-ux follow-up ka-0000003947). MR beads are NOT thread members, so the
	// timeline never surfaces the code that shipped — this makes the wish rail a
	// richer mirror of Slack by showing the concrete work. Set by the handler
	// (it needs a daemon walk); the pure buildThreadDetailView leaves it nil, so
	// a degraded render simply omits the section. Rendered in the rail, OUTSIDE
	// the ?since= poll partial, so a poll never needs to re-walk the MR graph.
	ProducedMRs []wrapUpMRView
	// SlackBound reports whether the wish head carries a usable Slack
	// (channel, thread_ts) binding. The composer (2-5) only renders a live
	// reply box for a Slack-bound wish — the bridge-mediated send addresses the
	// reply by that binding, so an unbound head can take no reply (the write
	// handler returns 409). Drives the composer-vs-"no Slack thread" choice.
	SlackBound bool
	// Channel / ThreadTS echo the head's Slack binding so the composer form can
	// carry them (the compose-on-no-wish path needs them; the reply path reads
	// them off the head server-side, but exposing them keeps the form uniform).
	Channel  string
	ThreadTS string
	// Herder is the herder's last verdict on the open wish, shown in the rail
	// beside its status (threads_herder.go); zero before its first pass.
	Herder threadHerder
	// Error is a non-empty banner when the timeline fetch failed after the head
	// resolved; the head still renders.
	Error string
}

// MemberCount is the number of member beads (head included). Template
// convenience for the collapsed-work pane's "Work (N)" affordance.
func (v threadDetailView) MemberCount() int { return len(v.Members) }

// ProducedMRCount is the number of merge requests the wish's members produced —
// the headline figure on the rail's "Produced MRs (N)" section. Template
// convenience so the count is a single field access, not a len pipeline.
func (v threadDetailView) ProducedMRCount() int { return len(v.ProducedMRs) }

// HasProducedMRs reports whether any produced MRs are present, so the template
// renders the section only when there is code to show (rubric C15 — no hollow
// empty section).
func (v threadDetailView) HasProducedMRs() bool { return len(v.ProducedMRs) > 0 }

// threadMemberRow is one row of the detail page's collapsed "work" pane: a
// member bead's id, title, and flags marking the wish head itself and
// conversation (type=message) members. IsMessage members are the composer's
// replies / squawks bound to the wish — they belong to the conversation lane,
// not the agent-work roster, so the work pane (2-4 WorkMembers) filters them
// out (2-5).
type threadMemberRow struct {
	ID        string
	Title     string
	IsHead    bool
	IsMessage bool
}

// buildThreadDetailView projects the head bead and a fused timeline into the
// flat detail view model. It is a pure function (no daemon, no request) so the
// projection logic — member-row derivation, head flagging, title resolution —
// is exhaustively unit-testable without standing up a server.
//
// Member titles are resolved from the timeline's bead_created entries (every
// member the daemon returned contributes exactly one). A member with no
// bead_created entry (deleted / cross-fleet, so BatchGetBeads omitted it) still
// gets a row — its membership is real even if its body is gone — falling back
// to the head's title for the head and an empty title otherwise, so the
// dep-first member order from threadMembers is preserved one-to-one.
func buildThreadDetailView(head *beadsapi.BeadDetail, tl threadTimeline) threadDetailView {
	v := threadDetailView{ThreadID: tl.ThreadID}
	if v.ThreadID == "" && head != nil {
		v.ThreadID = head.ID
	}
	if head != nil {
		v.Title = head.Title
		v.WishText = threadWishText(head)
		v.Status = head.Status
		v.Project = projectFromBeadLabels(head.Labels)
		v.Channel = strings.TrimSpace(head.Fields[threadSlackChannelField])
		v.ThreadTS = strings.TrimSpace(head.Fields[threadSlackTSField])
		v.SlackBound = validThreadBinding(v.Channel, v.ThreadTS)
		v.Herder = threadHerderOf(head)
	}

	// Index member titles AND message membership from the fused stream. A work
	// member contributes a bead_created entry (the canonical title carrier); a
	// conversation member (type=message) contributes a message entry instead
	// (2-5) and carries no bead_created, so both kinds must be scanned to title
	// every row and to flag which members are messages (so the work pane filters
	// them out).
	titleByID := make(map[string]string, len(tl.Members))
	isMsgByID := make(map[string]bool, len(tl.Members))
	headCreatedPresent := false
	for _, e := range tl.Entries {
		switch e.Kind {
		case threadEntryBeadCreated:
			titleByID[e.BeadID] = e.BeadTitle
			if e.BeadID == v.ThreadID {
				headCreatedPresent = true
			}
		case threadEntryMessage:
			titleByID[e.BeadID] = e.BeadTitle
			isMsgByID[e.BeadID] = true
		}
	}

	v.Members = make([]threadMemberRow, 0, len(tl.Members))
	for _, id := range tl.Members {
		row := threadMemberRow{
			ID:        id,
			Title:     titleByID[id],
			IsHead:    id == v.ThreadID,
			IsMessage: isMsgByID[id],
		}
		if row.Title == "" && row.IsHead && head != nil {
			row.Title = head.Title
		}
		v.Members = append(v.Members, row)
	}

	// Copy the fused entries verbatim — fuseThreadTimeline already sorted them
	// (At asc, dep-first member position, lifecycle-kind, comment-id). Defensive
	// stable re-sort by timestamp only would break the established tie-break, so
	// we trust the aggregator's ordering and just hand the slice through.
	v.Timeline = tl.Entries

	// Robustness (ka-0000006057 gap 2): the head's "opened this wish" marker — the
	// conversation anchor Conversation() renders, and the C15 guarantee that the
	// lead pane is never an empty void — is derived SOLELY from the head's
	// bead_created timeline entry. That entry exists only if fuseThreadTimeline saw
	// the head's body via BatchGetBeads. But a daemon split / deletion race can
	// omit the head body from that batch fetch even though the handler's own
	// GetBead resolved it (so head != nil here). Left unhandled, Conversation()
	// would emit nothing for a reply-less wish and the lead pane would show the
	// exact empty void the guarantee forbids. Synthesize the head-created entry
	// from the resolved head so the anchor (and OpenedAt) survive the race. Only
	// this rendered VIEW is touched; the /threads/{id}/audit endpoint re-derives
	// the fused stream independently from the fuser and is unaffected.
	if head != nil && !headCreatedPresent {
		v.Timeline = insertHeadCreatedEntry(v.Timeline, threadTimelineEntry{
			At:        head.CreatedAt,
			Kind:      threadEntryBeadCreated,
			BeadID:    v.ThreadID,
			BeadTitle: head.Title,
			Author:    head.CreatedBy,
		})
	}
	return v
}

// insertHeadCreatedEntry inserts a synthesized head bead_created entry into an
// already-sorted fused timeline at its chronological position, without mutating
// the caller's backing slice. The head opens before its derivatives, so it sorts
// at (or before) the earliest entry; inserting before the first entry whose
// timestamp is not strictly earlier keeps the head marker ahead of any
// same-instant derivative event (matching fuseThreadTimeline's dep-first tie-
// break, where the head is member position 0). Used only on the degraded path
// where BatchGetBeads omitted the head body (ka-0000006057 gap 2).
func insertHeadCreatedEntry(entries []threadTimelineEntry, head threadTimelineEntry) []threadTimelineEntry {
	i := 0
	for i < len(entries) && entries[i].At.Before(head.At) {
		i++
	}
	out := make([]threadTimelineEntry, 0, len(entries)+1)
	out = append(out, entries[:i]...)
	out = append(out, head)
	out = append(out, entries[i:]...)
	return out
}
