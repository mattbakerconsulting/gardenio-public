package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Phase 2, task 2-4 (ka-0000006184) tests. Two layers:
//
//   1. Pure-method tests over the curation layer (threads_curation.go): the
//      Conversation/Work lane split that implements rubric gate B11. No daemon,
//      no HTTP — just the view model in, lanes out.
//   2. Rendered-HTML contract tests over the real /threads/{id} handler + the
//      two-pane template, asserting the CURATION DEFAULT is correct: the
//      conversation leads (visible markup), the agent work sits inside a
//      <details> element that has NO `open` attribute (collapsed by default).
//      This is the headline gate — "conversation first, work collapsed" — and
//      the rendered assertion is what proves the default actually ships, not
//      just that the data split is right.

// --- helpers ---------------------------------------------------------------

// detailViewFixture builds a threadDetailView the way buildThreadDetailView
// would, but directly from a hand-authored timeline so the lane-split logic can
// be exercised without standing up the aggregator. mirror of the real shape:
// element 0 of Members is the head.
func detailViewFixture(headID string, members []threadMemberRow, entries []threadTimelineEntry) threadDetailView {
	return threadDetailView{
		ThreadID: headID,
		Title:    "wish",
		Status:   "open",
		Members:  members,
		Timeline: entries,
	}
}

func tEntry(at int64, kind, beadID, title, author, text string, commentID int64) threadTimelineEntry {
	return threadTimelineEntry{
		At: time.Unix(at, 0), Kind: kind, BeadID: beadID, BeadTitle: title,
		Author: author, Text: text, CommentID: commentID,
	}
}

// --- B11 lane-split unit tests ---------------------------------------------

// TestConversationLane_LeadsWithDialogueAndWishLifecycle proves the conversation
// lane carries exactly the human dialogue (comments) plus the WISH's own
// open/close lifecycle — and excludes every non-head member's spawn/close
// (those are work). Order must follow the fused timeline.
func TestConversationLane_LeadsWithDialogueAndWishLifecycle(t *testing.T) {
	head := "gd-wish"
	v := detailViewFixture(head,
		[]threadMemberRow{{ID: head, IsHead: true}, {ID: "gd-task", Title: "do the thing"}},
		[]threadTimelineEntry{
			tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0),            // wish opened -> convo marker
			tEntry(20, threadEntryBeadCreated, "gd-task", "do the thing", "bot", "", 0), // member spawn -> WORK, not convo
			tEntry(30, threadEntryComment, head, "wish", "alice", "please hurry", 1),    // reply -> convo
			tEntry(40, threadEntryBeadClosed, "gd-task", "do the thing", "bot", "", 0),  // member close -> WORK
			tEntry(50, threadEntryBeadClosed, head, "wish", "alice", "", 0),             // wish closed -> convo marker
		})

	convo := v.Conversation()
	if len(convo) != 3 {
		t.Fatalf("Conversation len = %d, want 3 (open marker, 1 reply, close marker); got %+v", len(convo), convo)
	}
	if convo[0].IsComment || convo[0].Marker != "opened this wish" {
		t.Errorf("convo[0] = %+v, want the 'opened this wish' marker", convo[0])
	}
	if !convo[1].IsComment || convo[1].Text != "please hurry" {
		t.Errorf("convo[1] = %+v, want the reply bubble", convo[1])
	}
	if convo[2].IsComment || convo[2].Marker != "closed this wish" {
		t.Errorf("convo[2] = %+v, want the 'closed this wish' marker", convo[2])
	}
	// The member's spawn/close must NOT have leaked into the conversation.
	for _, e := range convo {
		if strings.Contains(e.Text, "do the thing") || e.Marker == "" && !e.IsComment {
			t.Errorf("member work leaked into conversation: %+v", e)
		}
	}

	// ReplyCount counts only real replies, not lifecycle markers.
	if got := v.ReplyCount(); got != 1 {
		t.Errorf("ReplyCount = %d, want 1 (markers excluded)", got)
	}
	if !v.HasConversation() {
		t.Errorf("HasConversation = false, want true")
	}
}

// TestWorkLane_CarriesOnlyNonHeadMemberLifecycle proves the work lane is exactly
// the non-head members' spawn/close events — never the wish's own lifecycle
// (which belongs to the conversation).
func TestWorkLane_CarriesOnlyNonHeadMemberLifecycle(t *testing.T) {
	head := "gd-wish"
	v := detailViewFixture(head,
		[]threadMemberRow{{ID: head, IsHead: true}, {ID: "gd-a", Title: "task a"}, {ID: "gd-b", Title: "task b"}},
		[]threadTimelineEntry{
			tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0),    // wish open -> NOT work
			tEntry(20, threadEntryBeadCreated, "gd-a", "task a", "bot1", "", 0), // spawn a -> work
			tEntry(30, threadEntryBeadCreated, "gd-b", "task b", "bot2", "", 0), // spawn b -> work
			tEntry(40, threadEntryBeadClosed, "gd-a", "task a", "bot1", "", 0),  // close a -> work
		})

	work := v.WorkEvents()
	if len(work) != 3 {
		t.Fatalf("WorkEvents len = %d, want 3 (2 spawns + 1 close); got %+v", len(work), work)
	}
	if work[0].Verb != "spawned" || work[0].BeadID != "gd-a" {
		t.Errorf("work[0] = %+v, want spawned gd-a", work[0])
	}
	if work[2].Verb != "closed" || work[2].BeadID != "gd-a" {
		t.Errorf("work[2] = %+v, want closed gd-a", work[2])
	}
	for _, e := range work {
		if e.BeadID == head {
			t.Errorf("wish-head lifecycle leaked into work lane: %+v", e)
		}
	}

	// WorkMembers excludes the head; WorkMemberCount agrees.
	wm := v.WorkMembers()
	if len(wm) != 2 {
		t.Fatalf("WorkMembers len = %d, want 2 (head excluded)", len(wm))
	}
	for _, m := range wm {
		if m.IsHead {
			t.Errorf("head leaked into WorkMembers: %+v", m)
		}
	}
	if got := v.WorkMemberCount(); got != 2 {
		t.Errorf("WorkMemberCount = %d, want 2", got)
	}
	if got := v.WorkDoneCount(); got != 1 {
		t.Errorf("WorkDoneCount = %d, want 1 (only gd-a closed)", got)
	}
	if !v.HasWork() {
		t.Errorf("HasWork = false, want true")
	}
}

// TestCuration_ConversationOnlyWish proves a wish with replies but no spawned
// work yields an empty work lane (HasWork false) while the conversation still
// leads — the template renders a "no work yet" line, never a hollow expandable.
func TestCuration_ConversationOnlyWish(t *testing.T) {
	head := "gd-wish"
	v := detailViewFixture(head,
		[]threadMemberRow{{ID: head, IsHead: true}},
		[]threadTimelineEntry{
			tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0),
			tEntry(20, threadEntryComment, head, "wish", "alice", "any progress?", 1),
		})
	if !v.HasConversation() {
		t.Errorf("HasConversation = false, want true")
	}
	if v.HasWork() {
		t.Errorf("HasWork = true, want false (no spawned members)")
	}
	if got := v.WorkMemberCount(); got != 0 {
		t.Errorf("WorkMemberCount = %d, want 0", got)
	}
}

// TestCuration_NoConversationYet proves a brand-new wish (only its open event,
// no replies, no work) reports HasConversation false so the template shows the
// "start the thread" empty state instead of a blank pane (rubric C15).
func TestCuration_NoConversationYet(t *testing.T) {
	head := "gd-wish"
	// Only the wish-open marker exists. That IS a conversation row (the anchor),
	// so HasConversation is true and the lead pane is never blank.
	v := detailViewFixture(head,
		[]threadMemberRow{{ID: head, IsHead: true}},
		[]threadTimelineEntry{tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0)})
	if !v.HasConversation() {
		t.Errorf("HasConversation = false, want true (the open marker anchors the lane)")
	}
	if got := v.ReplyCount(); got != 0 {
		t.Errorf("ReplyCount = %d, want 0", got)
	}
}

// TestCuration_StatusHelpers covers the rail's status-derived cues.
func TestCuration_StatusHelpers(t *testing.T) {
	for _, tc := range []struct {
		status                 string
		wantClosed, wantTriage bool
	}{
		{"open", false, false},
		{"in_progress", false, false},
		{"closed", true, false},
		{"done", true, false},
		{"needs_triage", false, true},
		{"needs-triage", false, true},
	} {
		v := threadDetailView{Status: tc.status}
		if got := v.IsClosed(); got != tc.wantClosed {
			t.Errorf("status %q IsClosed = %v, want %v", tc.status, got, tc.wantClosed)
		}
		if got := v.IsNeedsTriage(); got != tc.wantTriage {
			t.Errorf("status %q IsNeedsTriage = %v, want %v", tc.status, got, tc.wantTriage)
		}
	}
}

// TestCuration_OpenedAt resolves the wish's own bead_created timestamp and
// returns zero when absent.
func TestCuration_OpenedAt(t *testing.T) {
	head := "gd-wish"
	v := detailViewFixture(head, []threadMemberRow{{ID: head, IsHead: true}},
		[]threadTimelineEntry{
			tEntry(20, threadEntryBeadCreated, "gd-a", "a", "bot", "", 0), // member, ignored
			tEntry(10, threadEntryBeadCreated, head, "wish", "alice", "", 0),
		})
	if got := v.OpenedAt(); !got.Equal(time.Unix(10, 0)) {
		t.Errorf("OpenedAt = %v, want %v", got, time.Unix(10, 0))
	}
	empty := threadDetailView{ThreadID: head}
	if got := empty.OpenedAt(); !got.IsZero() {
		t.Errorf("OpenedAt with no timeline = %v, want zero", got)
	}
}

// TestThreadSummary_IndexCuration covers the index-card helpers.
func TestThreadSummary_IndexCuration(t *testing.T) {
	open := threadSummary{Status: "open", MemberCount: 4}
	if got := open.WorkCount(); got != 3 {
		t.Errorf("WorkCount = %d, want 3 (head excluded)", got)
	}
	if open.IsClosed() || open.IsNeedsTriage() {
		t.Errorf("open wish wrongly flagged closed/triage")
	}
	headOnly := threadSummary{Status: "open", MemberCount: 1}
	if got := headOnly.WorkCount(); got != 0 {
		t.Errorf("WorkCount head-only = %d, want 0", got)
	}
	if !(threadSummary{Status: "closed"}).IsClosed() {
		t.Errorf("closed wish not flagged fulfilled")
	}
	if !(threadSummary{Status: "needs_triage"}).IsNeedsTriage() {
		t.Errorf("needs_triage wish not flagged")
	}
}

// --- B11 rendered-default contract test ------------------------------------

// TestThreadDetail_CurationDefault_ConversationFirstWorkCollapsed is the
// headline gate (rubric B11) asserted at the RENDERED level: a wish with both
// conversation and a fleet of agent work must render the conversation visibly
// in the lead pane while the agent work lives inside a <details> element that
// carries NO `open` attribute — i.e. collapsed by default. If a future edit
// flipped the default open, or moved work above the conversation, this fails.
func TestThreadDetail_CurationDefault_ConversationFirstWorkCollapsed(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "Make onboarding smoother", Status: "open",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{threadRootTextField: "onboarding takes too many steps, please simplify"},
			CreatedBy: "alice", CreatedAt: now.Add(-3 * time.Hour),
		},
		// Two member tasks (the agent "work") bound by thread_id AND parent-child.
		"gd-plan1": {
			ID: "gd-plan1", Title: "Cut the signup form to 2 fields", Status: "closed",
			Fields:    map[string]string{threadIDField: "gd-wish"},
			CreatedBy: "agent-alpha", CreatedAt: now.Add(-2 * time.Hour),
			ClosedBy: "agent-alpha", ClosedAt: now.Add(-1 * time.Hour).Format(time.RFC3339),
		},
		"gd-plan2": {
			ID: "gd-plan2", Title: "Autodetect timezone", Status: "open",
			Fields:    map[string]string{threadIDField: "gd-wish"},
			CreatedBy: "agent-beta", CreatedAt: now.Add(-90 * time.Minute),
		},
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-wish": {
			{BeadID: "gd-plan1", DependsOnID: "gd-wish", Type: "parent-child"},
			{BeadID: "gd-plan2", DependsOnID: "gd-wish", Type: "parent-child"},
		},
	}
	f.comments = map[string][]beadsapi.Comment{
		"gd-wish": {
			{ID: 1, Author: "alice", Text: "the timezone guess is the worst part", CreatedAt: now.Add(-2 * time.Hour)},
			{ID: 2, Author: "bob", Text: "agreed, that bit me too", CreatedAt: now.Add(-80 * time.Minute)},
		},
	}

	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish")
	if code != http.StatusOK {
		t.Fatalf("GET /threads/gd-wish = %d, want 200", code)
	}

	// (1) The conversation is rendered in the visible lead pane.
	if !strings.Contains(body, "the timezone guess is the worst part") {
		t.Errorf("conversation reply missing from rendered detail")
	}
	if !strings.Contains(body, "agreed, that bit me too") {
		t.Errorf("second reply missing from rendered detail")
	}
	if !strings.Contains(body, `aria-label="Conversation"`) {
		t.Errorf("conversation lane section missing")
	}

	// (2) The agent work is INSIDE a <details> that is NOT open by default —
	//     this is the literal "work collapsed" assertion.
	work := extractWorkDetails(t, body)
	if strings.Contains(work, " open") {
		// <details open ...> would mean the firehose shows on load — a B11 fail.
		t.Errorf("agent-work <details> is open by default — rubric B11 requires it COLLAPSED.\nopening tag region: %q", work)
	}
	// The member work titles must live within that collapsed block, not the lead.
	if !strings.Contains(body, "Cut the signup form to 2 fields") {
		t.Errorf("member work title missing from rendered detail (should be in the collapsed pane)")
	}

	// (3) Curation ORDER: the conversation section opens BEFORE the work
	//     <details> element in source order, so the conversation literally
	//     leads. Anchor on the actual element (not the bare "thread2-work"
	//     class name, which also appears earlier inside the <style> block).
	convoIdx := strings.Index(body, `aria-label="Conversation"`)
	workIdx := strings.Index(body, `<details class="thread2-work"`)
	if convoIdx < 0 || workIdx < 0 {
		t.Fatalf("missing lanes: convoIdx=%d workIdx=%d", convoIdx, workIdx)
	}
	if convoIdx > workIdx {
		t.Errorf("work pane precedes the conversation in source order — conversation must lead (B11)")
	}

	// (4) The rail summarizes the wish: text, fulfilled cue absent (open), work tally.
	if !strings.Contains(body, "onboarding takes too many steps") {
		t.Errorf("wish text missing from rail")
	}
	if !strings.Contains(body, "User wish") {
		t.Errorf("wish-card eyebrow missing")
	}
}

// extractWorkDetails returns the opening <details ...> tag of the agent-work
// pane so the test can assert on the presence/absence of the `open` attribute.
func extractWorkDetails(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `<details class="thread2-work"`)
	if i < 0 {
		t.Fatalf("agent-work <details> element not found in body")
	}
	end := strings.Index(body[i:], ">")
	if end < 0 {
		t.Fatalf("malformed <details> tag")
	}
	return body[i : i+end+1]
}

// TestThreadDetail_NeedsTriageBanner proves a needs_triage wish renders the
// loud triage banner (rubric C15 — an actionable, non-blank state).
func TestThreadDetail_NeedsTriageBanner(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "Flaky deploy", Status: "needs_triage",
			Labels: []string{threadHeadLabel}, CreatedAt: now,
		},
	}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish")
	if code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", code)
	}
	if !strings.Contains(body, "Needs triage") {
		t.Errorf("expected needs-triage banner for a needs_triage wish")
	}
}

// TestThreadDetail_FulfilledWishShowsRailCue proves a closed wish gets the
// "fulfilled" rail treatment.
func TestThreadDetail_FulfilledWishShowsRailCue(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "Add CSV export", Status: "closed",
			Labels: []string{threadHeadLabel}, CreatedAt: now.Add(-time.Hour),
			ClosedAt: now.Format(time.RFC3339), ClosedBy: "alice",
		},
	}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish")
	if code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", code)
	}
	if !strings.Contains(body, "is-fulfilled") {
		t.Errorf("expected the fulfilled rail treatment on a closed wish")
	}
	if !strings.Contains(body, "fulfilled") {
		t.Errorf("expected a 'fulfilled' chip on a closed wish")
	}
}

// TestThreadsIndex_CurationCues proves the index cards carry the curated cues:
// fulfilled wishes are flagged, work tallies exclude the head, and triage is
// surfaced.
func TestThreadsIndex_CurationCues(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-done": {
			ID: "gd-done", Title: "Shipped feature", Status: "closed",
			Labels: []string{threadHeadLabel}, UpdatedAt: now.Add(-5 * time.Minute), CreatedAt: now.Add(-time.Hour),
		},
		"gd-triage": {
			ID: "gd-triage", Title: "Broken thing", Status: "needs_triage",
			Labels: []string{threadHeadLabel}, UpdatedAt: now.Add(-10 * time.Minute), CreatedAt: now.Add(-time.Hour),
		},
		"gd-mem": {
			ID: "gd-mem", Title: "work item", Status: "open",
			Fields: map[string]string{threadIDField: "gd-triage"}, UpdatedAt: now.Add(-12 * time.Minute),
		},
	}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads")
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d, want 200", code)
	}
	if !strings.Contains(body, "fulfilled") {
		t.Errorf("closed wish should show a 'fulfilled' cue on the index")
	}
	if !strings.Contains(body, "needs triage") {
		t.Errorf("needs_triage wish should show a 'needs triage' cue on the index")
	}
	// gd-triage has one bound member -> "1 bead of work"; the head is excluded.
	if !strings.Contains(body, "1 bead of work") {
		t.Errorf("expected '1 bead of work' for the triage wish (head excluded), body:\n%s", body)
	}
}

// TestThreadsIndex_EnrichedCard is the rendered contract for the index-card
// enrichment (ka-0000000064): a card must surface the participant avatars, the
// starter's name, the last message preview, and the associated MR pills — the
// four things Matt asked for.
func TestThreadsIndex_EnrichedCard(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish": {
			ID: "gd-wish", Title: "Make onboarding smoother", Status: "open",
			Labels: []string{threadHeadLabel, "project:gardenio"},
			Fields: map[string]string{threadRootTextField: "onboarding takes too many steps"},
			// The wish opener is a resolvable human → starter + avatar.
			OperatedBy: "slack:UALICE", OperatedByDisplay: "Alice Ant", OperatedByEmail: "alice@acme.example",
			UpdatedAt: now.Add(-5 * time.Minute), CreatedAt: now.Add(-time.Hour),
		},
		// A message member from Bob — second participant + the last-message preview.
		"gd-msg": {
			ID: "gd-msg", Title: "reply", Type: "message", Status: "closed",
			Fields:     map[string]string{threadIDField: "gd-wish", "text": "shipping the timezone fix now"},
			OperatedBy: "slack:UBOB", OperatedByDisplay: "Bob Bee", OperatedByEmail: "bob@acme.example",
			CreatedAt: now.Add(-4 * time.Minute), UpdatedAt: now.Add(-4 * time.Minute),
		},
		// A work member that produced an MR.
		"gd-task": {
			ID: "gd-task", Title: "cut the form", Status: "closed",
			Fields:    map[string]string{threadIDField: "gd-wish"},
			CreatedBy: "agent-alpha", CreatedAt: now.Add(-30 * time.Minute), UpdatedAt: now.Add(-10 * time.Minute),
		},
		"gd-mr1": {
			ID: "gd-mr1", Title: "the fix", Type: "merge-request", Status: "merged",
			Fields: map[string]string{"mr_url": "https://gitlab.example/mr/42", "mr_iid": "42", "mr_title": "Cut signup form", "mr_state": "merged"},
		},
	}
	// gd-task --produces--> gd-mr1 (forward dep).
	f.deps = map[string][]beadsapi.Dependency{
		"gd-task": {{BeadID: "gd-task", DependsOnID: "gd-mr1", Type: "produces"}},
	}

	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads")
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d, want 200", code)
	}

	// (1) Participant avatars — both humans' initials render as avatar bubbles.
	if !strings.Contains(body, `class="thread-avatar"`) {
		t.Errorf("participant avatars missing from the card")
	}
	if !strings.Contains(body, ">AA<") { // Alice Ant initials
		t.Errorf("Alice's avatar initials (AA) missing")
	}
	if !strings.Contains(body, ">BB<") { // Bob Bee initials
		t.Errorf("Bob's avatar initials (BB) missing")
	}

	// (2) Starter name.
	if !strings.Contains(body, "started by") || !strings.Contains(body, "Alice Ant") {
		t.Errorf("starter attribution ('started by Alice Ant') missing")
	}

	// (3) Last-message preview.
	if !strings.Contains(body, "shipping the timezone fix now") {
		t.Errorf("last-message preview missing")
	}

	// (4) Associated MR pill.
	if !strings.Contains(body, "https://gitlab.example/mr/42") {
		t.Errorf("associated MR pill missing")
	}
	if !strings.Contains(body, "mr-pill") {
		t.Errorf("MR rendered without the shared mr_pill chrome")
	}
}
