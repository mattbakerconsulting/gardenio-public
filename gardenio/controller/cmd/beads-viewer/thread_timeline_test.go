package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Phase 2, task 2-5 (ka-0000006034) tests. Three layers:
//
//  1. Pure curation/aggregator tests: a type=message member renders as a
//     conversation bubble (not work), the work lanes exclude it, and the
//     LatestActivity cursor spans the whole fused stream (so a work event
//     advances it as a reply does — rubric B12).
//  2. The fused-timeline fragment handler: GET /threads/{id}/timeline renders
//     the panes, honours ?since= with a 304 short-circuit, and degrades to a
//     banner (never 500) on a timeline-fetch failure.
//  3. Rendered-HTML contract over the real detail page + the composer partial:
//     the operator-gated composer is present and posts to the merged 2-2 write
//     path; a read-only viewer sees no write box.

// --- helpers ---------------------------------------------------------------

// getBodyWithRoles is getBody with an operator email + explicit OIDC roles, so
// the operator-gated composer (viewerCanOperate) renders. RequireRole passes
// through when no roles are present, but the page-level viewerCanOperate also
// keys off roles, so the composer-visible path must inject the operator role.
func getBodyWithRoles(t *testing.T, mux http.Handler, path, email string, roles []string) (string, int) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if email != "" {
		req.Header.Set("X-Auth-Request-Email", email)
	}
	if roles != nil {
		req = req.WithContext(oidcauth.ContextWithRoles(req.Context(), roles))
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Body.String(), w.Code
}

// seedSlackWish adds a Slack-bound wish head to the package fake.
func seedSlackWish(f *fakeDaemon, id, channel, ts string, now time.Time) {
	if f.beads == nil {
		f.beads = map[string]*beadsapi.BeadDetail{}
	}
	f.beads[id] = &beadsapi.BeadDetail{
		ID: id, Title: "Slack wish", Status: "open",
		Labels: []string{threadHeadLabel, threadBeadIdentityLabel(channel, ts)},
		Fields: map[string]string{
			threadSlackChannelField: channel,
			threadSlackTSField:      ts,
			threadRootTextField:     "please do the thing",
		},
		CreatedBy: "alice", CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-2 * time.Hour),
	}
}

// --- message-as-conversation curation (fuse + lanes) -----------------------

// TestFuseTimeline_MessageMemberRendersAsConversation proves a type=message
// member (the composer's reply) emits ONE message entry carrying its text — not
// a bead_created/bead_closed work pair.
func TestFuseTimeline_MessageMemberRendersAsConversation(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish", "Wish", "gardenio", ts("2026-06-20T09:00:00Z"), nil)
	// A closed type=message member bound by thread_id (exactly what
	// postThreadSayMessage writes: type=message, closed, text in fields.text).
	msg := f.addMemberByField("gd-msg", "msg title", "gd-wish",
		ts("2026-06-20T10:00:00Z"), ts("2026-06-20T10:00:00Z"))
	msg.Type = "message"
	msg.Status = "closed"
	msg.ClosedAt = "2026-06-20T10:00:01Z"
	msg.CreatedBy = "matt@example.com"
	msg.Fields["text"] = "hello from the composer"

	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	var msgEntries, closedEntries, createdForMsg int
	for _, e := range tl.Entries {
		switch {
		case e.Kind == threadEntryMessage && e.BeadID == "gd-msg":
			msgEntries++
			if e.Text != "hello from the composer" {
				t.Errorf("message entry text = %q, want the fields.text body", e.Text)
			}
			if e.Author != "matt@example.com" {
				t.Errorf("message entry author = %q, want the operator email", e.Author)
			}
		case e.Kind == threadEntryBeadClosed && e.BeadID == "gd-msg":
			closedEntries++
		case e.Kind == threadEntryBeadCreated && e.BeadID == "gd-msg":
			createdForMsg++
		}
	}
	if msgEntries != 1 {
		t.Errorf("message entries = %d, want exactly 1", msgEntries)
	}
	if closedEntries != 0 {
		t.Errorf("message member emitted %d bead_closed entries, want 0 (it is conversation, not work)", closedEntries)
	}
	if createdForMsg != 0 {
		t.Errorf("message member emitted %d bead_created entries, want 0", createdForMsg)
	}
}

// TestCuration_MessageInConversationNotWork proves the lane split: a message
// member shows in Conversation() and counts in ReplyCount(), but is excluded
// from WorkMembers / WorkMemberCount / WorkEvents.
func TestCuration_MessageInConversationNotWork(t *testing.T) {
	head := "gd-wish"
	v := detailViewFixture(head,
		[]threadMemberRow{
			{ID: head, IsHead: true},
			{ID: "gd-task", Title: "real work"},
			{ID: "gd-msg", Title: "msg", IsMessage: true},
		},
		[]threadTimelineEntry{
			tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0),
			tEntry(20, threadEntryBeadCreated, "gd-task", "real work", "bot", "", 0),
			tEntry(30, threadEntryMessage, "gd-msg", "msg", "matt", "operator reply", 0),
		})

	convo := v.Conversation()
	var sawReply bool
	for _, e := range convo {
		if e.IsComment && e.Text == "operator reply" {
			sawReply = true
		}
	}
	if !sawReply {
		t.Errorf("message did not surface as a conversation bubble: %+v", convo)
	}
	if got := v.ReplyCount(); got != 1 {
		t.Errorf("ReplyCount = %d, want 1 (the message counts as a reply)", got)
	}
	// Work lane: the message member must NOT appear, only the real task.
	if got := v.WorkMemberCount(); got != 1 {
		t.Errorf("WorkMemberCount = %d, want 1 (message excluded)", got)
	}
	for _, m := range v.WorkMembers() {
		if m.IsMessage {
			t.Errorf("message member leaked into WorkMembers: %+v", m)
		}
	}
	for _, e := range v.WorkEvents() {
		if e.BeadID == "gd-msg" {
			t.Errorf("message member leaked into WorkEvents: %+v", e)
		}
	}
}

// TestCuration_LatestActivitySpansWholeStream proves the poll cursor advances on
// ANY activity — a spawned-work event moves it exactly as a reply does (B12).
func TestCuration_LatestActivitySpansWholeStream(t *testing.T) {
	head := "gd-wish"
	v := detailViewFixture(head, []threadMemberRow{{ID: head, IsHead: true}},
		[]threadTimelineEntry{
			tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0),
			tEntry(50, threadEntryComment, head, "wish", "alice", "reply", 1),
			// A later spawned-work event is the newest — the cursor must reach it.
			tEntry(90, threadEntryBeadCreated, "gd-task", "task", "bot", "", 0),
		})
	if got := v.LatestActivity(); !got.Equal(time.Unix(90, 0)) {
		t.Errorf("LatestActivity = %v, want the newest entry (a work event) at 90", got)
	}
	empty := threadDetailView{ThreadID: head}
	if got := empty.LatestActivity(); !got.IsZero() {
		t.Errorf("LatestActivity on empty timeline = %v, want zero", got)
	}
}

// TestThreadMessageText_FallsBackToTitle covers the body resolution for a
// message bead: fields.text wins, else the title, else empty.
func TestThreadMessageText_FallsBackToTitle(t *testing.T) {
	withText := &beadsapi.BeadDetail{Title: "title", Fields: map[string]string{"text": "  body  "}}
	if got := threadMessageText(withText); got != "body" {
		t.Errorf("threadMessageText with text = %q, want trimmed body", got)
	}
	titleOnly := &beadsapi.BeadDetail{Title: "just a title", Fields: map[string]string{}}
	if got := threadMessageText(titleOnly); got != "just a title" {
		t.Errorf("threadMessageText title fallback = %q, want title", got)
	}
	if got := threadMessageText(nil); got != "" {
		t.Errorf("threadMessageText(nil) = %q, want empty", got)
	}
}

// TestFuseTimeline_MessageMemberTitleFallback proves a message bead with no
// fields.text emits a message entry carrying its title as the body (the
// fallback path), so a title-only message still reads as a conversation line.
func TestFuseTimeline_MessageMemberTitleFallback(t *testing.T) {
	f := newThreadFake()
	f.addHead("gd-wish", "Wish", "gardenio", ts("2026-06-20T09:00:00Z"), nil)
	msg := f.addMemberByField("gd-msg", "title-only message", "gd-wish",
		ts("2026-06-20T10:00:00Z"), ts("2026-06-20T10:00:00Z"))
	msg.Type = "message"
	// No fields.text set — only the title carries the body.
	tl, err := fuseThreadTimeline(context.Background(), f, "gd-wish")
	if err != nil {
		t.Fatalf("fuseThreadTimeline: %v", err)
	}
	var found bool
	for _, e := range tl.Entries {
		if e.Kind == threadEntryMessage && e.BeadID == "gd-msg" {
			found = true
			if e.Text != "title-only message" {
				t.Errorf("message entry text = %q, want the title fallback", e.Text)
			}
		}
	}
	if !found {
		t.Errorf("title-only message member produced no message entry")
	}
}

// --- handler: GET /threads/{id}/timeline -----------------------------------

func TestThreadTimeline_RendersPanes(t *testing.T) {
	f := newFake()
	now := time.Now()
	seedSlackWish(f, "gd-wish", "C1", "1700000000.000100", now)
	f.comments = map[string][]beadsapi.Comment{
		"gd-wish": {{ID: 1, Author: "alice", Text: "any progress?", CreatedAt: now.Add(-time.Hour)}},
	}
	mux := mkServerFromFake(t, f)

	body, code := getBody(t, mux, "/threads/gd-wish/timeline")
	if code != http.StatusOK {
		t.Fatalf("GET timeline = %d, want 200; body=%s", code, body)
	}
	if !strings.Contains(body, `id="thread-timeline"`) {
		t.Errorf("timeline fragment missing the poll-region id")
	}
	if !strings.Contains(body, "any progress?") {
		t.Errorf("conversation reply missing from timeline fragment")
	}
	if !strings.Contains(body, `aria-label="Conversation"`) {
		t.Errorf("conversation lane missing")
	}
	// The poll region carries an hx-get back to itself (the ?since= loop).
	if !strings.Contains(body, `/threads/gd-wish/timeline`) {
		t.Errorf("timeline fragment missing its self hx-get")
	}
}

func TestThreadTimeline_NotFound_404(t *testing.T) {
	f := newFake()
	mux := mkServerFromFake(t, f)
	_, code := getBody(t, mux, "/threads/gd-missing/timeline")
	if code != http.StatusNotFound {
		t.Fatalf("GET timeline missing head = %d, want 404", code)
	}
}

// TestThreadTimeline_SinceShortCircuit204 proves an unchanged cursor yields 204
// (no DOM swap — htmx leaves the region intact) and that a stale/empty cursor
// renders the full fragment. 204 (not 304) is load-bearing: htmx 2.0.4 swaps on
// 304 (it falls under the [23].. rule) which would erase the outerHTML target,
// but treats 204 as swap:false.
func TestThreadTimeline_SinceShortCircuit204(t *testing.T) {
	f := newFake()
	now := time.Now().UTC().Truncate(time.Second)
	seedSlackWish(f, "gd-wish", "C1", "1700000000.000100", now)
	// One comment at a known time is the latest activity.
	latest := now.Add(-30 * time.Minute)
	f.comments = map[string][]beadsapi.Comment{
		"gd-wish": {{ID: 1, Author: "alice", Text: "hi", CreatedAt: latest}},
	}
	mux := mkServerFromFake(t, f)

	// First poll with no cursor -> full render + the cursor header.
	req := httptest.NewRequest("GET", "/threads/gd-wish/timeline", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("first poll = %d, want 200", w.Code)
	}
	cursor := w.Header().Get(threadTimelineCursorHeader)
	if cursor == "" {
		t.Fatalf("missing %s header on first render", threadTimelineCursorHeader)
	}

	// Second poll echoing that cursor -> 204, no body (htmx swap:false; the
	// region survives). Must NOT be 304, which htmx would swap-erase.
	req2 := httptest.NewRequest("GET", "/threads/gd-wish/timeline?since="+cursor, nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNoContent {
		t.Fatalf("matched-cursor poll = %d, want 204; body=%s", w2.Code, w2.Body.String())
	}
	if w2.Code == http.StatusNotModified {
		t.Fatalf("must not 304 — htmx 2.0.4 swaps on 304 and would erase the live region")
	}
	if strings.TrimSpace(w2.Body.String()) != "" {
		t.Errorf("no-op response must have an empty body, got %q", w2.Body.String())
	}
	// The cursor header still rides along so the client keeps its attribute current.
	if w2.Header().Get(threadTimelineCursorHeader) == "" {
		t.Errorf("204 response must still carry the %s cursor header", threadTimelineCursorHeader)
	}
}

// TestThreadTimeline_NewMessageBreaksCursor is the B12 end-to-end: after the
// operator's message lands as a new member, the same cursor no longer matches,
// so the poll returns the fragment WITH the new message — a live refresh.
func TestThreadTimeline_NewMessageBreaksCursor(t *testing.T) {
	f := newFake()
	now := time.Now().UTC().Truncate(time.Second)
	seedSlackWish(f, "gd-wish", "C1", "1700000000.000100", now)
	// Cursor at the wish-open time (the only activity so far).
	openCursor := f.beads["gd-wish"].CreatedAt.Format(commentTimeFormat)
	mux := mkServerFromFake(t, f)

	// A new composer message arrives, newer than the cursor.
	f.beads["gd-msg"] = &beadsapi.BeadDetail{
		ID: "gd-msg", Type: "message", Status: "closed",
		Labels:    []string{threadSayLabel, threadMembershipLabel("gd-wish")},
		Fields:    map[string]string{threadIDField: "gd-wish", "text": "fresh reply"},
		CreatedBy: "matt@example.com", CreatedAt: now.Add(-time.Minute),
	}

	body, code := getBody(t, mux, "/threads/gd-wish/timeline?since="+openCursor)
	if code != http.StatusOK {
		t.Fatalf("poll after new message = %d, want 200 (cursor moved); body=%s", code, body)
	}
	if !strings.Contains(body, "fresh reply") {
		t.Errorf("live poll did not surface the new message; body:\n%s", body)
	}
}

// TestThreadTimeline_DegradesOnTimelineError proves a membership/timeline fetch
// failure renders the banner on a 200 (not a 500) so the live region never
// blanks mid-poll.
func TestThreadTimeline_DegradesOnTimelineError(t *testing.T) {
	f := newFake()
	now := time.Now()
	seedSlackWish(f, "gd-wish", "C1", "1700000000.000100", now)
	f.batchGetBeadsErr = errThreadTest // BatchGetBeads fails -> fuse errors out.
	mux := mkServerFromFake(t, f)

	body, code := getBody(t, mux, "/threads/gd-wish/timeline")
	if code != http.StatusOK {
		t.Fatalf("degraded timeline = %d, want 200 (banner, not 500)", code)
	}
	if !strings.Contains(body, "Failed to refresh thread timeline") {
		t.Errorf("expected the degraded-render banner, body:\n%s", body)
	}
}

// --- rendered detail page: composer presence + gating ----------------------

// TestThreadDetail_RendersOperatorComposer proves the operator-gated composer
// renders on the detail page for a Slack-bound wish, posting to the merged 2-2
// write path (POST /b/{id}/thread-reply) — NOT a new unauthenticated route.
func TestThreadDetail_RendersOperatorComposer(t *testing.T) {
	f := newFake()
	now := time.Now()
	seedSlackWish(f, "gd-wish", "C1", "1700000000.000100", now)
	mux := mkServerFromFake(t, f)

	body, code := getBodyWithRoles(t, mux, "/threads/gd-wish", "matt@example.com", []string{oidcauth.RoleOperator})
	if code != http.StatusOK {
		t.Fatalf("GET detail = %d, want 200", code)
	}
	if !strings.Contains(body, `id="thread-composer"`) {
		t.Errorf("operator composer form missing for a Slack-bound wish")
	}
	if !strings.Contains(body, `hx-post="/b/gd-wish/thread-reply"`) {
		t.Errorf("composer must post to the merged 2-2 write path /b/{id}/thread-reply, body:\n%s", body)
	}
	// The composer JS must be loaded for optimistic render.
	if !strings.Contains(body, "thread_composer.js") {
		t.Errorf("composer JS not loaded")
	}
}

// TestThreadDetail_ViewerSeesComposer proves a plain gardenio-viewer DOES see the
// write box on the detail page (ka-0000003462: composer lowered to viewerOnly).
// This is the detail-page regression guard for "I don't even see the UX": the
// live AVA default role is gardenio-viewer, so the reply box MUST render for a
// viewer on a Slack-bound wish or no live user can reach it.
func TestThreadDetail_ViewerSeesComposer(t *testing.T) {
	f := newFake()
	now := time.Now()
	seedSlackWish(f, "gd-wish", "C1", "1700000000.000100", now)
	mux := mkServerFromFake(t, f)

	body, code := getBodyWithRoles(t, mux, "/threads/gd-wish", "viewer@example.com", []string{oidcauth.RoleViewer})
	if code != http.StatusOK {
		t.Fatalf("GET detail = %d, want 200", code)
	}
	if !strings.Contains(body, `id="thread-composer"`) {
		t.Errorf("a gardenio-viewer MUST see the composer write box (viewer-gated since ka-0000003462)")
	}
	if !strings.Contains(body, `hx-post="/b/gd-wish/thread-reply"`) {
		t.Errorf("viewer composer must post to /b/{id}/thread-reply")
	}
	// The timeline still renders for the viewer.
	if !strings.Contains(body, `id="thread-timeline"`) {
		t.Errorf("timeline missing for viewer")
	}
}

// TestThreadDetail_UnboundWishShowsNoSlackNotice proves a wish with no Slack
// binding shows the disabled notice instead of a composer (the write handler
// would 409 such a post).
func TestThreadDetail_UnboundWishShowsNoSlackNotice(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "manual wish", Status: "open",
			Labels: []string{threadHeadLabel}, Fields: map[string]string{},
			CreatedBy: "alice", CreatedAt: now,
		},
	}
	mux := mkServerFromFake(t, f)

	body, code := getBodyWithRoles(t, mux, "/threads/gd-wish", "matt@example.com", []string{oidcauth.RoleOperator})
	if code != http.StatusOK {
		t.Fatalf("GET detail = %d, want 200", code)
	}
	if strings.Contains(body, `id="thread-composer"`) {
		t.Errorf("an unbound wish must not render a live composer form")
	}
	if !strings.Contains(body, "isn") || !strings.Contains(body, "Slack conversation") {
		t.Errorf("expected the 'not bound to a Slack conversation' notice, body:\n%s", body)
	}
}

// TestThreadDetail_MessageRendersInConversationPane is the rendered-level proof
// that a composer reply (type=message member) shows in the conversation lane,
// not the work pane.
func TestThreadDetail_MessageRendersInConversationPane(t *testing.T) {
	f := newFake()
	now := time.Now()
	seedSlackWish(f, "gd-wish", "C1", "1700000000.000100", now)
	f.beads["gd-msg"] = &beadsapi.BeadDetail{
		ID: "gd-msg", Type: "message", Status: "closed",
		Labels:    []string{threadSayLabel, threadMembershipLabel("gd-wish")},
		Fields:    map[string]string{threadIDField: "gd-wish", "text": "operator says hello"},
		CreatedBy: "matt@example.com", CreatedAt: now.Add(-30 * time.Minute),
	}
	mux := mkServerFromFake(t, f)

	body, code := getBody(t, mux, "/threads/gd-wish")
	if code != http.StatusOK {
		t.Fatalf("GET detail = %d, want 200", code)
	}
	if !strings.Contains(body, "operator says hello") {
		t.Errorf("composer message missing from rendered conversation")
	}
	// It must render as a conversation bubble, and the message bead must NOT be
	// listed as a work member in the collapsed pane.
	convoIdx := strings.Index(body, "operator says hello")
	workIdx := strings.Index(body, `<details class="thread2-work"`)
	if workIdx >= 0 && convoIdx > workIdx {
		t.Errorf("composer message rendered after/inside the work pane; it must lead in the conversation")
	}
}
