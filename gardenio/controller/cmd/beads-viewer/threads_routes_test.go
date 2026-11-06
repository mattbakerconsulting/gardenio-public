package main

import (
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// errThreadTest is a sentinel forced into the fake's fetch paths to exercise the
// handlers' degraded-render branches.
var errThreadTest = errors.New("threads test: forced daemon error")

// TestThreadsRoutesDoNotShadow is the rubric gate-5 regression test (QC rubric
// gd-ekEoieO8YE §A.5): it proves the two net-new Phase-2 patterns —
// GET /threads and GET /threads/{id} — neither shadow nor are shadowed by the
// existing route table, most pointedly the catch-all GET /{id} (handleBeadShow).
//
// It asserts on the *matched pattern* returned by net/http.ServeMux.Handler,
// not on handler side-effects. That is the load-bearing fact: Go 1.22's mux
// scores every pattern by specificity and resolves the winner deterministically,
// so checking which pattern a path maps to proves the routing topology directly
// and survives any future change to the handler bodies. Registration ORDER is
// irrelevant to net/http (specificity wins), but a NEW overlapping pattern, a
// typo turning a literal into a wildcard, or removing the catch-all would all
// flip one of these assertions and fail loudly — which is exactly the guard the
// rubric demands.
func TestThreadsRoutesDoNotShadow(t *testing.T) {
	// Build the real, fully-populated mux — every application route, not a
	// hand-picked subset — so the test exercises the genuine precedence field
	// the production server runs with. Raw mux (no edgeAuth wrapper) so
	// mux.Handler(req) can inspect matched route patterns.
	mux := mkRawMux(t, newFake())

	cases := []struct {
		name        string
		method      string
		path        string
		wantPattern string
	}{
		// The new routes resolve to themselves.
		{"threads index", "GET", "/threads", "GET /threads"},
		{"thread detail", "GET", "/threads/gd-abc123", "GET /threads/{id}"},
		{"thread detail numeric-ish id", "GET", "/threads/123", "GET /threads/{id}"},
		// 2-5: the three-segment ?since= poll route must win for its own shape
		// and NOT be swallowed by the two-segment detail route or the catch-all.
		{"thread timeline poll", "GET", "/threads/gd-abc123/timeline", "GET /threads/{id}/timeline"},

		// The catch-all bead page must STILL win for a bare id — /threads did
		// not steal it. This is the shadow-in-reverse direction.
		{"bare bead id -> catch-all", "GET", "/gd-aaa", "GET /{id}"},

		// A path that merely shares the /threads prefix but is NOT exactly
		// "/threads" must fall through to the catch-all, proving the literal
		// match is exact and not a sneaky prefix swallow.
		{"threads-prefixed non-thread", "GET", "/threadsy", "GET /{id}"},

		// Pre-existing sibling routes are untouched: a representative spread of
		// the literals and single-segment wildcards the rubric calls out
		// (/feed, /agents, /gardeners, /u/{slug}, /b/{id}/..). If /threads or
		// /threads/{id} had been registered too greedily (e.g. as /{a}/{b}),
		// one of these would now resolve to a thread pattern instead.
		{"feed", "GET", "/feed", "GET /feed"},
		{"agents", "GET", "/agents", "GET /agents"},
		{"gardeners", "GET", "/gardeners", "GET /gardeners"},
		{"user home", "GET", "/u/alice", "GET /u/{slug}"},
		{"bundle", "GET", "/bundle/gd-xyz", "GET /bundle/{id}"},
		{"bead comments fragment", "GET", "/b/gd-aaa/comments", "GET /b/{id}/comments"},
		{"gardener detail", "GET", "/gardener/gd-cap", "GET /gardener/{id}"},
		{"roles index", "GET", "/roles", "GET /roles"},
		{"role hub", "GET", "/role/crew", "GET /role/{role}"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			h, pattern := mux.Handler(req)
			if h == nil {
				t.Fatalf("%s %s: no handler matched", tc.method, tc.path)
			}
			if pattern != tc.wantPattern {
				t.Errorf("%s %s routed to %q, want %q", tc.method, tc.path, pattern, tc.wantPattern)
			}
		})
	}
}

// TestThreadsRoutesDistinctFromBeadShow nails the single most dangerous
// collision in isolation: /threads/{id} (the new two-segment detail route) and
// /{id} (the catch-all bead page) must resolve to DIFFERENT handler patterns.
// A regression that registered the detail route as bare /{id}-equivalent, or
// that dropped the segment count, would make these equal and silently route
// every thread detail through handleBeadShow.
func TestThreadsRoutesDistinctFromBeadShow(t *testing.T) {
	mux := mkRawMux(t, newFake())

	_, threadPat := mux.Handler(httptest.NewRequest("GET", "/threads/gd-1", nil))
	_, beadPat := mux.Handler(httptest.NewRequest("GET", "/gd-1", nil))

	if threadPat == beadPat {
		t.Fatalf("/threads/{id} and /{id} resolved to the same pattern %q — the detail route is shadowed by the catch-all", threadPat)
	}
	if threadPat != "GET /threads/{id}" {
		t.Errorf("/threads/gd-1 routed to %q, want \"GET /threads/{id}\"", threadPat)
	}
	if beadPat != "GET /{id}" {
		t.Errorf("/gd-1 routed to %q, want \"GET /{id}\"", beadPat)
	}
}

// TestRegisterRoutesDoesNotPanic guards the other ServeMux failure mode:
// net/http PANICS at registration time when two patterns conflict and neither
// is strictly more specific (an ambiguous overlap). RegisterRoutes building the
// full table without panicking proves /threads + /threads/{id} introduced no
// such ambiguity against any existing pattern.
func TestRegisterRoutesDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RegisterRoutes panicked (conflicting/ambiguous route pattern): %v", r)
		}
	}()
	srv := NewServer(newFake(), slog.Default(), "", "", "", nil)
	srv.RegisterRoutes(http.NewServeMux())
}

// --- /threads index handler -------------------------------------------------

func TestHandleThreads_ListsWishHeads(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {
			ID: "gd-wish1", Title: "Make the dashboard faster", Status: "open",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{threadRootTextField: "can the dashboard load quicker please"},
			UpdatedAt: now.Add(-1 * time.Hour),
			CreatedAt: now.Add(-3 * time.Hour),
		},
		"gd-wish2": {
			ID: "gd-wish2", Title: "Add dark mode", Status: "in_progress",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			UpdatedAt: now.Add(-10 * time.Minute),
			CreatedAt: now.Add(-2 * time.Hour),
		},
		// A member bound to wish1 by thread_id — must lift wish1's member count
		// but must NOT itself appear as a head row (no thread label).
		"gd-member": {
			ID: "gd-member", Title: "perf task", Status: "open",
			Labels:    []string{"project:gardenio"},
			Fields:    map[string]string{threadIDField: "gd-wish1"},
			UpdatedAt: now.Add(-30 * time.Minute),
		},
	}

	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads")
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d, want 200", code)
	}
	// Both wishes render; the member's verbatim wish-text leads for wish1.
	if !strings.Contains(body, "can the dashboard load quicker please") {
		t.Errorf("expected wish1 slack_root_text in body")
	}
	if !strings.Contains(body, "Add dark mode") {
		t.Errorf("expected wish2 title in body")
	}
	// The non-head member must not surface as its own row.
	if strings.Contains(body, ">perf task<") {
		t.Errorf("non-head member leaked into the thread index")
	}
	// Detail links are present and point at the new route.
	if !strings.Contains(body, `href="/threads/gd-wish1"`) {
		t.Errorf("expected detail link to /threads/gd-wish1, body:\n%s", body)
	}
	// wish2 (active 10m ago) sorts above wish1 (active 1h ago).
	if strings.Index(body, "Add dark mode") > strings.Index(body, "can the dashboard load quicker please") {
		t.Errorf("expected wish2 (more recent) to sort above wish1")
	}
}

func TestHandleThreads_Empty(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{} // no thread heads
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads")
	if code != http.StatusOK {
		t.Fatalf("GET /threads = %d, want 200", code)
	}
	// 2-4 replaced the baseline one-liner with a richer empty-state panel
	// ("No wishes yet" + guidance) so the zero-wish index is never a blank
	// white pane (rubric C15).
	if !strings.Contains(body, "No wishes yet") {
		t.Errorf("expected empty-state copy, body:\n%s", body)
	}
}

func TestHandleThreads_ProjectScope(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads?project=gardenio")
	if code != http.StatusOK {
		t.Fatalf("GET /threads?project=gardenio = %d, want 200", code)
	}
	// The scope echo renders the project name.
	if !strings.Contains(body, "gardenio") {
		t.Errorf("expected project scope echo in body")
	}
	// The head enumeration must carry BOTH the thread label and the project
	// label — proving ?project plumbs through to threadList.
	var sawScoped bool
	for _, q := range f.listAllQ {
		if slices.Contains(q.Labels, threadHeadLabel) && slices.Contains(q.Labels, "project:gardenio") {
			sawScoped = true
		}
	}
	if !sawScoped {
		t.Errorf("expected a head enumeration scoped to project:gardenio; queries: %+v", f.listAllQ)
	}
}

func TestHandleThreads_DaemonErrorDegrades(t *testing.T) {
	f := newFake()
	f.listErr = errThreadTest
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads")
	// Degrades to a 200 page with an error banner rather than a 500.
	if code != http.StatusOK {
		t.Fatalf("GET /threads (daemon error) = %d, want 200 (degraded)", code)
	}
	if !strings.Contains(body, "Failed to load threads") {
		t.Errorf("expected error banner, body:\n%s", body)
	}
}

// --- /threads/{id} detail handler -------------------------------------------

func TestHandleThreadDetail_RendersFusedTimeline(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {
			ID: "gd-wish1", Title: "Ship the threads view", Status: "open",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			Fields:    map[string]string{threadRootTextField: "I want to see my wishes as threads"},
			CreatedBy: "alice",
			CreatedAt: now.Add(-2 * time.Hour),
		},
		"gd-task1": {
			ID: "gd-task1", Title: "register routes", Status: "open",
			Fields:    map[string]string{threadIDField: "gd-wish1"},
			CreatedBy: "bot-23",
			CreatedAt: now.Add(-1 * time.Hour),
		},
	}
	// task1 is a parent-child member of the wish (reverse-dep from the head).
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-wish1": {{BeadID: "gd-task1", DependsOnID: "gd-wish1", Type: "parent-child"}},
	}
	f.comments = map[string][]beadsapi.Comment{
		"gd-wish1": {{ID: 1, Author: "alice", Text: "please make it interactive", CreatedAt: now.Add(-90 * time.Minute)}},
	}

	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish1")
	if code != http.StatusOK {
		t.Fatalf("GET /threads/gd-wish1 = %d, want 200", code)
	}
	// Head title + wish text.
	if !strings.Contains(body, "Ship the threads view") {
		t.Errorf("expected head title in body")
	}
	if !strings.Contains(body, "I want to see my wishes as threads") {
		t.Errorf("expected wish text in body")
	}
	// Fused timeline carries the comment AND the member task's creation.
	if !strings.Contains(body, "please make it interactive") {
		t.Errorf("expected comment text in fused timeline")
	}
	if !strings.Contains(body, "register routes") {
		t.Errorf("expected member task to appear (timeline + work pane)")
	}
	// The work pane links members back to their bead pages.
	if !strings.Contains(body, `href="/gd-task1"`) {
		t.Errorf("expected member link to /gd-task1, body:\n%s", body)
	}
}

// TestHandleThreadDetail_RendersProducedMRs is the end-to-end gate for the
// gb-ux follow-up (ka-0000003947): a wish whose member task produced an MR bead
// (via a forward `produces` edge) must surface that MR as an mr_pill in the
// rail's "Produced MRs" section — the code the wish shipped, which the fused
// timeline never shows because MR beads are not thread members.
func TestHandleThreadDetail_RendersProducedMRs(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {
			ID: "gd-wish1", Title: "Ship the feature", Status: "open",
			Labels:    []string{threadHeadLabel, "project:gardenio"},
			CreatedBy: "alice",
			CreatedAt: now.Add(-2 * time.Hour),
		},
		"gd-task1": {
			ID: "gd-task1", Title: "do the work", Status: "closed",
			Fields:    map[string]string{threadIDField: "gd-wish1"},
			CreatedBy: "bot-9",
			CreatedAt: now.Add(-1 * time.Hour),
		},
		// The produced MR bead — a separate type=merge-request bead, NOT a
		// member of the thread. Reached only via task1 --produces--> mr1.
		"gd-mr1": {
			ID: "gd-mr1", Title: "feat: ship the feature", Type: "merge-request", Status: "open",
			Fields: map[string]string{
				"mr_url":             "https://gitlab.com/org/repo/-/merge_requests/321",
				"mr_iid":             "321",
				"mr_title":           "feat: ship the feature",
				"mr_state":           "opened",
				"mr_pipeline_status": "success",
			},
		},
	}
	// task1 is a parent-child member of the wish (reverse-dep from the head).
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-wish1": {{BeadID: "gd-task1", DependsOnID: "gd-wish1", Type: "parent-child"}},
	}
	// task1 --produces--> mr1 (forward dep; fakeDaemon serves forward from f.deps).
	f.deps = map[string][]beadsapi.Dependency{
		"gd-task1": {{BeadID: "gd-task1", DependsOnID: "gd-mr1", Type: "produces"}},
	}

	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish1")
	if code != http.StatusOK {
		t.Fatalf("GET /threads/gd-wish1 = %d, want 200", code)
	}
	// Assert on RENDERED markup only — the section element, the pill anchor with
	// its live URL, the IID/title/state/pipeline spans. Bare class names like
	// "mr-pill" or "mr-state-opened" are avoided because the page's inlined
	// <style> block always contains those selectors; these markers appear only
	// when the pill actually rendered.
	for _, want := range []string{
		`<section class="thread2-mrs"`, // the section element (markup, not CSS)
		`>Produced MRs<`,               // the eyebrow header text
		`class="mr-pill"`,              // the rendered pill anchor
		`href="https://gitlab.com/org/repo/-/merge_requests/321"`, // MR deep-link
		`!321`,                     // the IID span
		`feat: ship the feature`,   // the MR title span
		`mr-state mr-state-opened`, // the rendered state pill class pair
		`class="pipe-success"`,     // the rendered pipeline pill
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected produced-MR rendering to contain %q, body:\n%s", want, body)
		}
	}
}

// TestHandleThreadDetail_NoProducedMRsOmitsSection asserts the section is
// omitted entirely when the wish's members produced no MRs (rubric C15 — no
// hollow section).
func TestHandleThreadDetail_NoProducedMRsOmitsSection(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {
			ID: "gd-wish1", Title: "A wish with no MRs", Status: "open",
			Labels: []string{threadHeadLabel}, CreatedAt: now,
		},
	}
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish1")
	if code != http.StatusOK {
		t.Fatalf("GET /threads/gd-wish1 = %d, want 200", code)
	}
	// Ban only markers that appear EXCLUSIVELY in rendered markup — the page's
	// inlined <style> block always carries `.thread2-mrs` selectors and a
	// "Produced MRs" CSS comment, so those are not discriminating. The section
	// element, the eyebrow text node, and a rendered pill anchor are.
	for _, banned := range []string{`<section class="thread2-mrs"`, `>Produced MRs<`, `class="mr-pill"`} {
		if strings.Contains(body, banned) {
			t.Errorf("empty produced-MRs path unexpectedly rendered %q\nbody:\n%s", banned, body)
		}
	}
}

func TestHandleThreadDetail_NotFound(t *testing.T) {
	f := newFake()
	f.beads = map[string]*beadsapi.BeadDetail{}
	mux := mkServerFromFake(t, f)
	_, code := getBody(t, mux, "/threads/gd-missing")
	if code != http.StatusNotFound {
		t.Fatalf("GET /threads/gd-missing = %d, want 404", code)
	}
}

func TestHandleThreadDetail_HeadFetchError500(t *testing.T) {
	// A non-404 daemon failure resolving the head (e.g. the daemon is down) is a
	// hard 500 — distinct from the not-found 404 and from the post-head degraded
	// timeline path.
	f := newFake()
	f.getBeadErr = errThreadTest
	mux := mkServerFromFake(t, f)
	_, code := getBody(t, mux, "/threads/gd-anything")
	if code != http.StatusInternalServerError {
		t.Fatalf("GET /threads/gd-anything (head fetch error) = %d, want 500", code)
	}
}

func TestHandleThreadDetail_TimelineErrorDegrades(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-wish1": {
			ID: "gd-wish1", Title: "Wish with a flaky timeline", Status: "open",
			Labels: []string{threadHeadLabel}, CreatedAt: now,
		},
	}
	// The head resolves, but the membership/timeline fetch (a ListBeadsFiltered
	// for thread_id-bound members) fails. The page must still render the head
	// with an error banner, NOT 500.
	f.listErr = errThreadTest
	mux := mkServerFromFake(t, f)
	body, code := getBody(t, mux, "/threads/gd-wish1")
	if code != http.StatusOK {
		t.Fatalf("GET /threads/gd-wish1 (timeline error) = %d, want 200 (degraded)", code)
	}
	if !strings.Contains(body, "Wish with a flaky timeline") {
		t.Errorf("expected head to still render under degraded timeline")
	}
	if !strings.Contains(body, "Failed to load thread timeline") {
		t.Errorf("expected timeline error banner, body:\n%s", body)
	}
}

// --- buildThreadDetailView pure-projection unit tests ------------------------

func TestBuildThreadDetailView_HeadFlaggedAndTitlesResolved(t *testing.T) {
	head := &beadsapi.BeadDetail{
		ID: "gd-w", Title: "Wish head", Status: "open",
		Labels: []string{threadHeadLabel, "project:gardenio"},
		Fields: map[string]string{threadRootTextField: "the raw wish"},
	}
	tl := threadTimeline{
		ThreadID: "gd-w",
		Members:  []string{"gd-w", "gd-c1", "gd-gone"},
		Entries: []threadTimelineEntry{
			{At: time.Unix(10, 0), Kind: threadEntryBeadCreated, BeadID: "gd-w", BeadTitle: "Wish head"},
			{At: time.Unix(20, 0), Kind: threadEntryBeadCreated, BeadID: "gd-c1", BeadTitle: "child one"},
			// gd-gone has no bead_created entry (BatchGetBeads omitted it) — its
			// row must still exist, ordered after its dep-first siblings.
		},
	}
	v := buildThreadDetailView(head, tl)

	if v.ThreadID != "gd-w" {
		t.Errorf("ThreadID = %q, want gd-w", v.ThreadID)
	}
	if v.WishText != "the raw wish" {
		t.Errorf("WishText = %q, want the raw wish", v.WishText)
	}
	if v.Project != "gardenio" {
		t.Errorf("Project = %q, want gardenio", v.Project)
	}
	if got := v.MemberCount(); got != 3 {
		t.Fatalf("MemberCount = %d, want 3 (dep-first order preserved one-to-one)", got)
	}
	// Order must mirror tl.Members exactly.
	wantIDs := []string{"gd-w", "gd-c1", "gd-gone"}
	for i, m := range v.Members {
		if m.ID != wantIDs[i] {
			t.Errorf("member[%d].ID = %q, want %q", i, m.ID, wantIDs[i])
		}
	}
	if !v.Members[0].IsHead {
		t.Errorf("element 0 must be flagged as the head")
	}
	if v.Members[1].IsHead {
		t.Errorf("non-head member wrongly flagged as head")
	}
	if v.Members[1].Title != "child one" {
		t.Errorf("member title = %q, want child one", v.Members[1].Title)
	}
	// The bodyless member keeps an empty title (no false head fallback).
	if v.Members[2].Title != "" {
		t.Errorf("bodyless member title = %q, want empty", v.Members[2].Title)
	}
}

func TestBuildThreadDetailView_HeadFallbackTitleForBodylessHead(t *testing.T) {
	// Head present but the timeline produced NO bead_created entry for it
	// (e.g. BatchGetBeads omitted the head). The head row must still show the
	// head's own title via the head-fallback branch.
	head := &beadsapi.BeadDetail{ID: "gd-w", Title: "Fallback wish", Status: "open"}
	tl := threadTimeline{ThreadID: "gd-w", Members: []string{"gd-w"}}
	v := buildThreadDetailView(head, tl)
	if len(v.Members) != 1 {
		t.Fatalf("MemberCount = %d, want 1", len(v.Members))
	}
	if v.Members[0].Title != "Fallback wish" {
		t.Errorf("head row title = %q, want Fallback wish (head fallback)", v.Members[0].Title)
	}
}

func TestBuildThreadDetailView_NilHeadUsesTimelineID(t *testing.T) {
	// Defensive: a nil head (shouldn't happen via the handler, which 404s
	// first) must not panic — ThreadID falls back to the timeline's id and the
	// view is otherwise empty.
	v := buildThreadDetailView(nil, threadTimeline{ThreadID: "gd-w"})
	if v.ThreadID != "gd-w" {
		t.Errorf("ThreadID = %q, want gd-w", v.ThreadID)
	}
	if v.Title != "" || v.MemberCount() != 0 || len(v.Timeline) > 0 {
		t.Errorf("expected an empty view for a nil head, got %+v", v)
	}
}

// --- small test helpers ------------------------------------------------------

// mkServerFromFake builds a mux from an arbitrary pre-seeded fakeDaemon (mkServer
// is hard-wired to newFake()). Kept local to the threads tests.
func mkServerFromFake(t *testing.T, f *fakeDaemon) http.Handler {
	t.Helper()
	return edgeAuth(mkRawMux(t, f))
}

// mkRawMux builds the viewer's route mux WITHOUT the edgeAuth identity seam, for
// tests that need the concrete *http.ServeMux (e.g. to call mux.Handler(req) for
// route-pattern inspection). Request-serving tests should use mkServer* instead
// so a header-only request's identity reaches EmailFromContext (ka-0000002979).
func mkRawMux(t *testing.T, daemon beadAPI) *http.ServeMux {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", "", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux
}

func getBody(t *testing.T, mux http.Handler, path string) (string, int) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w.Body.String(), w.Code
}
