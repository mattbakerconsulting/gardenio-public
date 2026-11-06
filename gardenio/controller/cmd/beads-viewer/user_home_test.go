package main

import (
	"context"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// fixedActivityNow is a deterministic mid-day UTC instant used by the
// activity-spine tests. The merged-stream date rails (Today / This week /
// Earlier) are calendar-anchored in UTC (see railBucket in
// user_home_activity.go), so a row seeded as now-2h off the real wall clock
// buckets under the PREVIOUS calendar day whenever the test runs between 00:00
// and ~02:00 UTC — flaking the ">Today<" assertion (gd-UTp3tkVC4r). Freezing
// both the seed timestamps AND the handler's clock (via mkServerWithNow →
// SetNow) to this mid-day instant makes every bucket deterministic regardless
// of the host's wall-clock hour. 12:00 is far from either midnight boundary,
// so now±a-few-hours stays on the same UTC date by construction.
var fixedActivityNow = time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)

// TestParseUserHomeProfilePref covers the three shapes the reader must
// tolerate: missing field, empty/garbage JSON, and a well-formed pin list.
func TestParseUserHomeProfilePref(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   int // expected len(PinnedBeadIDs)
	}{
		{"nil-fields", nil, 0},
		{"empty-string", map[string]string{"profile_prefs": ""}, 0},
		{"garbage", map[string]string{"profile_prefs": "not json"}, 0},
		{"empty-object", map[string]string{"profile_prefs": "{}"}, 0},
		{"pins", map[string]string{"profile_prefs": `{"pinned_bead_ids":["gd-a","gd-b","gd-c"]}`}, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProfilePref(tc.fields)
			if len(got.PinnedBeadIDs) != tc.want {
				t.Errorf("got %d pins, want %d", len(got.PinnedBeadIDs), tc.want)
			}
		})
	}
}

// TestExcludeTypeFilter verifies the row-level type-exclusion predicate.
func TestExcludeTypeFilter(t *testing.T) {
	t.Run("nil-when-empty", func(t *testing.T) {
		if excludeTypeFilter() != nil {
			t.Errorf("expected nil filter when no types passed")
		}
	})
	t.Run("denies-listed", func(t *testing.T) {
		fn := excludeTypeFilter("merge-request", "agent")
		if fn(&beadsapi.BeadDetail{Type: "merge-request"}) {
			t.Errorf("merge-request should be denied")
		}
		if fn(&beadsapi.BeadDetail{Type: "agent"}) {
			t.Errorf("agent should be denied")
		}
		if !fn(&beadsapi.BeadDetail{Type: "task"}) {
			t.Errorf("task should be accepted")
		}
		if fn(nil) {
			t.Errorf("nil bead should be denied")
		}
	})
}

// TestHandleUserHome_RendersMergedActivitySpine asserts /u/{slug} returns 200
// and renders ONE merged, reverse-chronological activity stream — the fix for
// bug ka-0000002041 (the page was a scatter of 11 disjoint section cards).
//
// The audit measure (tools/measure-flat.py) scores this as ONE row-bearing
// `user-home-section is-filled` with timestamped `<li class="bead-row">` rows
// in monotonic reverse-chron order. This test seeds a few dated beads and
// asserts: a single #activity spine, newest-first ordering, an absolute ISO
// title= on every row, and Today/Earlier date rails.
func TestHandleUserHome_RendersMergedActivitySpine(t *testing.T) {
	// Freeze the clock at a fixed mid-day UTC instant and seed every relative
	// timestamp off it so the "Today" date-rail bucket is deterministic at any
	// wall-clock hour (gd-UTp3tkVC4r — see fixedActivityNow).
	now := fixedActivityNow
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	// A generic (non-agent, non-MR) closed-work population feeds the stream.
	// The fake routes closed-status queries through listResp.
	newer := now.Add(-2 * time.Hour)      // Today (10:00 same UTC day)
	older := now.Add(-8 * 24 * time.Hour) // Earlier (>7d)
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-newer", Title: "Newer closed task", Type: "task", Status: "closed",
				Labels: []string{"project:gardenio"}, CreatedAt: older, UpdatedAt: newer,
			},
			{
				ID: "gd-older", Title: "Older closed task", Type: "task", Status: "closed",
				Labels: []string{"project:gardenio"}, CreatedAt: older, UpdatedAt: older,
			},
		},
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Exactly ONE row-bearing merged spine — the whole point of the fix.
	if !strings.Contains(body, `id="activity"`) {
		t.Fatalf("merged activity spine not rendered, body=%s", body)
	}
	if n := strings.Count(body, "user-home-section is-filled"); n != 1 {
		t.Errorf("expected exactly one is-filled section (merged spine), got %d; body=%s", n, body)
	}
	// The former lazy section-card placeholders must be GONE — no per-section
	// hx-get lazy shells, no perpetual Loading… bands.
	for _, gone := range []string{
		`hx-get="/u/alice/section/forwarded"`,
		`hx-get="/u/alice/section/recent-comments"`,
		"user-home-loading",
	} {
		if strings.Contains(body, gone) {
			t.Errorf("stale disjoint-section artifact %q still present; body=%s", gone, body)
		}
	}

	// Reverse-chronological: the newer row precedes the older row in the DOM.
	iNewer := strings.Index(body, "gd-newer")
	iOlder := strings.Index(body, "gd-older")
	if iNewer < 0 || iOlder < 0 {
		t.Fatalf("expected both closed-work rows in the stream; body=%s", body)
	}
	if iNewer > iOlder {
		t.Errorf("stream not reverse-chronological: gd-newer(%d) should precede gd-older(%d)", iNewer, iOlder)
	}

	// Every row emits an absolute ISO title= stamp for exact ordering on hover.
	// gd-newer was created 8d ago but last touched (closed) 2h ago, so the hover
	// carries BOTH stamps (ka-0000007477) and the row shows a secondary "created"
	// age so the real event time is visible next to the bumped time.
	if !strings.Contains(body, `updated `+newer.Format(time.RFC3339)+`"`) {
		t.Errorf("expected absolute ISO updated stamp in the newer row hover, body=%s", body)
	}
	if !strings.Contains(body, `created `+older.Format(time.RFC3339)+` · updated `) {
		t.Errorf("expected created+updated hover on the bumped row, body=%s", body)
	}
	if !strings.Contains(body, `bead-row-created`) {
		t.Errorf("expected a secondary created-age annotation on the bumped row, body=%s", body)
	}

	// Date rails segment the stream.
	if !strings.Contains(body, "timeline-rail") {
		t.Errorf("expected Today/This week/Earlier date rails, body=%s", body)
	}
	for _, rail := range []string{"Today", "Earlier"} {
		if !strings.Contains(body, ">"+rail+"<") {
			t.Errorf("expected %q date rail, body=%s", rail, body)
		}
	}
}

// activityHeaderRe extracts the integer N the Activity section header prints as
// "Activity (N)". Mirrors the audit's HEADER_COUNT regex family so the test
// guards the very number the users-camp measure tool reads off the page.
var activityHeaderRe = regexp.MustCompile(`Activity\s*<span[^>]*>\s*\((\d+)\)`)

// activityRowLiRe counts the rendered <li class="…activity-row…"> stream rows —
// the same selector the users-camp measure tool's ACTIVITY_ROW_RE uses. Date
// rails are <li class="timeline-rail"> and never carry activity-row, so they do
// not match.
var activityRowLiRe = regexp.MustCompile(`(?i)<li[^>]*class="[^"]*activity-row[^"]*"`)

// TestHandleUserHome_ActivityHeaderEqualsRenderedRows is the direct regression
// for bug ka-0000006350 — the Activity "(N)" header must equal the number of
// activity rows the page actually enumerates, in ONE render. The invariant is
// structural: activityView.Total = len(deduped) (user_home_activity.go) and the
// template emits exactly one <li class="activity-row"> per deduped event, so the
// header can never drift from the enumerable rows within a single render. The
// audit's reported "84 vs 83" was a cross-capture artifact (two captures of a
// live page taken moments apart), not a real per-render off-by-one. This test
// pins the invariant so a genuine drift — a header computed off a different
// population than the one rendered — reds CI instead of a beautiful screenshot.
//
// Seeds a deliberately odd row count (so an accidental +1/-1 off a hardcoded
// even fixture can't pass by luck) spanning all three date rails; rails are
// asserted NOT to inflate the header.
func TestHandleUserHome_ActivityHeaderEqualsRenderedRows(t *testing.T) {
	now := fixedActivityNow
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	// 7 closed-work rows (odd, spanning Today / This week / Earlier). The closed
	// population routes through listResp. CreatedAt older than UpdatedAt so each
	// row lands under "closed work" (a bumped/closed bead).
	const wantRows = 7
	offsets := []time.Duration{
		1 * time.Hour,       // Today
		5 * time.Hour,       // Today
		26 * time.Hour,      // This week
		3 * 24 * time.Hour,  // This week
		6 * 24 * time.Hour,  // This week
		9 * 24 * time.Hour,  // Earlier
		30 * 24 * time.Hour, // Earlier
	}
	beads := make([]*beadsapi.BeadDetail, 0, wantRows)
	for i, off := range offsets {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:        "gd-r" + itoa64(int64(i)),
			Title:     "closed thing",
			Type:      "task",
			Status:    "closed",
			Labels:    []string{"project:gardenio"},
			CreatedAt: now.Add(-off - 48*time.Hour),
			UpdatedAt: now.Add(-off),
		})
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: beads}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	hm := activityHeaderRe.FindStringSubmatch(body)
	if hm == nil {
		t.Fatalf("Activity (N) header not found; body=%s", body)
	}
	header := 0
	for _, c := range hm[1] {
		header = header*10 + int(c-'0')
	}
	rows := len(activityRowLiRe.FindAllString(body, -1))

	// The whole point of the bug: header integer == enumerable rows, exactly.
	if header != rows {
		t.Errorf("Activity header (%d) != rendered activity-row <li> count (%d) — page disagrees with itself (bug ka-0000006350)", header, rows)
	}
	// And both must equal the seeded population — a header derived from a
	// different count than the rendered rows would still be a defect even if it
	// happened to match a wrong row count.
	if rows != wantRows {
		t.Errorf("rendered activity rows = %d, want %d seeded", rows, wantRows)
	}
	// Date rails must NOT be counted into the header. All three rails render for
	// this seed; the header stays at the row count regardless.
	if railN := strings.Count(body, `class="timeline-rail"`); railN == 0 {
		t.Errorf("expected date rails in the stream, got 0 (seed spans Today/This week/Earlier)")
	}
}

// TestHandleUserHome_ActivityShowMoreCollapse is the regression for bugs
// ka-0000003169 then ka-0000000142 — the narrow (480px) /u/{slug} spine of a rich
// record renders a ~10k-px single-column mobile mega-scroll. The fix bounds the
// initial paint AND lazy-appends the rest on scroll (ka-0000000142), so the full
// record is reachable with no mandatory second click, while a server-rendered
// "Show more" button remains the JS-off / no-IntersectionObserver fallback. The
// critical invariant this test pins is that the collapse is a CLIENT-SIDE reveal
// affordance, NOT a server-side truncation: EVERY seeded row is still emitted into
// the merged stream, so the "Activity (N)" header, the header-vs-rows measure, and
// every measure-flat / measure-users-camp signal (all of which grep the serialized
// markup) stay intact. A regression that truncated rows server-side to shorten the
// page would red here.
func TestHandleUserHome_ActivityShowMoreCollapse(t *testing.T) {
	now := fixedActivityNow
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	// Seed a rich record (40 closed-work rows) — well past the narrow collapse
	// limit — so the pager is genuinely exercised. Every row must still render.
	const wantRows = 40
	beads := make([]*beadsapi.BeadDetail, 0, wantRows)
	for i := 0; i < wantRows; i++ {
		off := time.Duration(i+1) * time.Hour
		beads = append(beads, &beadsapi.BeadDetail{
			ID:        "gd-rich" + itoa64(int64(i)),
			Title:     "closed thing",
			Type:      "task",
			Status:    "closed",
			Labels:    []string{"project:gardenio"},
			CreatedAt: now.Add(-off - 48*time.Hour),
			UpdatedAt: now.Add(-off),
		})
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: beads}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The pager button is present and server-rendered `hidden` — the stream JS
	// unhides it only on the narrow breakpoint, so JS-off / desktop users get the
	// full spine untouched.
	if !strings.Contains(body, "data-activity-more") {
		t.Fatalf("expected the Show-more pager button, body=%s", snippetN(body, 2000))
	}
	if !regexp.MustCompile(`<button[^>]*data-activity-more[^>]*\shidden`).MatchString(body) {
		t.Errorf("Show-more button must be server-rendered hidden (JS unhides on narrow); body=%s", snippetN(body, 2000))
	}

	// Lazy-append sentinel (bug ka-0000000142): the narrow spine must reveal the
	// rest of the record AS THE OPERATOR SCROLLS (no mandatory second click), so
	// an IntersectionObserver watches this sentinel. It must be present and
	// server-rendered `hidden` (the JS unhides+observes it only on narrow), and it
	// must NOT carry the activity-row / bead-row class — otherwise it would inflate
	// the header-vs-rows row count checked below.
	if !strings.Contains(body, "data-activity-sentinel") {
		t.Fatalf("expected the lazy-append scroll sentinel, body=%s", snippetN(body, 2000))
	}
	if !regexp.MustCompile(`<[a-z]+[^>]*data-activity-sentinel[^>]*\shidden`).MatchString(body) {
		t.Errorf("scroll sentinel must be server-rendered hidden (JS unhides on narrow); body=%s", snippetN(body, 2000))
	}
	if regexp.MustCompile(`(?i)<[a-z]+[^>]*data-activity-sentinel[^>]*class="[^"]*activity-row`).MatchString(body) ||
		regexp.MustCompile(`(?i)<[a-z]+[^>]*data-activity-sentinel[^>]*class="[^"]*bead-row`).MatchString(body) {
		t.Errorf("scroll sentinel must NOT carry the activity-row/bead-row class (it would inflate the row count); body=%s", snippetN(body, 2000))
	}

	// The narrow initial reveal seeds at the trailing-WEEK row count (feature
	// ka-0000003056), injected into the stream JS as `var WEEK_COUNT = N`. Every
	// seeded row here is within the last ~40h (all Today/This-week buckets), so
	// WEEK_COUNT must equal the full population — the whole week paints without a
	// "Show all" click, which is exactly the bar the Users-CAMP judge held open.
	// html/template renders {{$act.WeekCount}} in the JS context as ` 40 ` (safe
	// integer literal, padded), so match the number tolerantly rather than pinning
	// exact whitespace.
	if !regexp.MustCompile(`WEEK_COUNT =\s+` + itoa64(int64(wantRows)) + `\s*;`).MatchString(body) {
		t.Errorf("expected the stream JS to inject WEEK_COUNT = %d (all seeded rows are within the trailing week); body=%s", wantRows, snippetN(body, 2500))
	}

	// The whole point of the fix: the collapse is CLIENT-side, so all rows are
	// still in the DOM. Header, enumerable rows, and the seeded population agree.
	hm := activityHeaderRe.FindStringSubmatch(body)
	if hm == nil {
		t.Fatalf("Activity (N) header not found; body=%s", snippetN(body, 2000))
	}
	header := 0
	for _, c := range hm[1] {
		header = header*10 + int(c-'0')
	}
	rows := len(activityRowLiRe.FindAllString(body, -1))
	if header != wantRows || rows != wantRows {
		t.Errorf("collapse must not drop rows server-side: header=%d rendered-rows=%d, want %d each (body serialized len=%d)", header, rows, wantRows, len(body))
	}
}

// TestHandleUserHome_ActivitySpineMonotonic asserts the rendered relative-age
// `.meta` tokens are monotonically non-decreasing down the DOM — the exact
// signal the time-series measure decodes for timeline_monotonic. Rails carry
// no `.meta`, so only row ages participate.
func TestHandleUserHome_ActivitySpineMonotonic(t *testing.T) {
	// Freeze the clock so the seed timestamps and the handler's relative-age
	// computation share one instant — otherwise a row seeded near a bucket/age
	// boundary can straddle it between seed time and render time (gd-UTp3tkVC4r).
	now := fixedActivityNow
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	beads := make([]*beadsapi.BeadDetail, 0, 6)
	for i, h := range []int{1, 3, 5, 30, 100, 200} {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:     "gd-h" + itoa64(int64(i)),
			Title:  "closed",
			Type:   "task",
			Status: "closed",
			Labels: []string{"project:gardenio"},
			// CreatedAt older than UpdatedAt so the row lands under "closed work".
			CreatedAt: now.Add(-time.Duration(h*2) * time.Hour),
			UpdatedAt: now.Add(-time.Duration(h) * time.Hour),
		})
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: beads}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Decode the bare `.meta` relative-age tokens in DOM order and assert the
	// implied ages are non-decreasing (newest-first ⇒ ascending age).
	metaRe := regexp.MustCompile(`<span class="meta">(\d+)([smhd])</span>`)
	unit := map[string]int{"s": 1, "m": 60, "h": 3600, "d": 86400}
	var secs []int
	for _, m := range metaRe.FindAllStringSubmatch(body, -1) {
		n := 0
		for _, c := range m[1] {
			n = n*10 + int(c-'0')
		}
		secs = append(secs, n*unit[m[2]])
	}
	if len(secs) < 2 {
		t.Fatalf("expected multiple relative-age rows, got %d; body=%s", len(secs), body)
	}
	for i := 1; i < len(secs); i++ {
		if secs[i] < secs[i-1] {
			t.Errorf("relative ages not monotonic at %d: %v", i, secs)
			break
		}
	}
}

// TestHandleUserHome_CrossViewIdenticalToSelf asserts the merged stream renders
// identically for a self viewer and a cross viewer — no redaction (acceptance
// criterion #4 / gardenio is FLAT). The activity builder does not branch on
// isViewerSelf, so the stream bytes must match modulo the "(you)" heading tag.
func TestHandleUserHome_CrossViewIdenticalToSelf(t *testing.T) {
	// Freeze the clock so the two independent renders (cross + self) compute
	// relative ages against the SAME instant — two real time.Now reads can
	// straddle an age/rail boundary and make the fragments differ spuriously
	// (gd-UTp3tkVC4r). Both servers below inject this same now.
	now := fixedActivityNow
	seed := func() *fakeDaemon {
		f := newFake()
		f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
			"alice": {OperatorID: 42, Found: true},
		}
		f.listResp = &beadsapi.ListBeadsResult{
			Beads: []*beadsapi.BeadDetail{{
				ID: "gd-cv", Title: "closed thing", Type: "task", Status: "closed",
				Labels:    []string{"project:gardenio"},
				CreatedAt: now.Add(-48 * time.Hour), UpdatedAt: now.Add(-3 * time.Hour),
			}},
		}
		return f
	}
	// Cross view: no resolver ⇒ isViewerSelf=false.
	fCross := seed()
	muxCross := mkServerWithNow(t, fCross, func() time.Time { return now })
	wCross := httptest.NewRecorder()
	muxCross.ServeHTTP(wCross, httptest.NewRequest("GET", "/u/alice", nil))
	if wCross.Code != 200 {
		t.Fatalf("cross view expected 200, got %d", wCross.Code)
	}
	crossBody := wCross.Body.String()

	// The stream content (the row) must be present and unredacted in the cross
	// view — no [hidden]/[redacted]/"bodies are hidden" affordance. Strip
	// <style>/<script> first (same as tools/measure-flat.py) so CSS attribute
	// selectors like `[hidden]` don't false-positive.
	if !strings.Contains(crossBody, "gd-cv") {
		t.Errorf("cross view missing the activity row; body=%s", crossBody)
	}
	crossContent := strings.ToLower(stripStyleScript(crossBody))
	for _, redaction := range []string{"[hidden]", "[redacted]", "bodies are hidden", "message bodies are hidden"} {
		if strings.Contains(crossContent, redaction) {
			t.Errorf("cross view leaked a redaction affordance %q", redaction)
		}
	}
	// The activity <section> markup is viewer-independent: extract it and assert
	// the self view produces the same fragment.
	fSelf := seed()
	muxSelf := mkServerWithNow(t, fSelf, func() time.Time { return now })
	wSelf := httptest.NewRecorder()
	muxSelf.ServeHTTP(wSelf, httptest.NewRequest("GET", "/u/alice", nil))
	selfBody := wSelf.Body.String()

	frag := func(b string) string {
		i := strings.Index(b, `id="activity"`)
		if i < 0 {
			return ""
		}
		j := strings.Index(b[i:], "</section>")
		if j < 0 {
			return b[i:]
		}
		return b[i : i+j]
	}
	if frag(selfBody) != frag(crossBody) {
		t.Errorf("activity spine differs between self and cross view (redaction leak?)")
	}
}

// TestHandleUserHome_TimestampCoverageFull is the static successor to the
// (now-disabled) daily Users-Page FLAT visual gardener (trigger ka-0000002785,
// retired per operator decision gd-y1QG437jVI → "disable + harness guard").
// It asserts the RENDERED /u/{slug} page has timestamp_coverage == 1.0: every
// activity row (<li class="bead-row">) carries a time anchor — a decodable
// relative `.meta` age AND/OR an absolute ISO `title=` stamp — exactly what the
// time-series measure (tools/measure-flat.py `timeseries()`) decodes. The
// anon==self byte-equality half of the guard lives in
// TestHandleUserHome_CrossViewIdenticalToSelf; together they replace the daily
// loop so a regression that drops a row's timestamp fails CI instead of only
// surfacing in the retired visual audit.
func TestHandleUserHome_TimestampCoverageFull(t *testing.T) {
	// Regexes mirror tools/measure-flat.py exactly so this Go guard and the
	// Python measure agree on what "time-anchored" means.
	beadRowRe := regexp.MustCompile(`(?s)<li[^>]*class="[^"]*bead-row[^"]*"[^>]*>(.*?)</li>`)
	metaRe := regexp.MustCompile(`<span class="meta">([^<]*)</span>`)
	relAgeRe := regexp.MustCompile(`(?i)^\s*(\d+)\s*(s|m|h|d|w|mo|y)\s*$`)
	absTSRe := regexp.MustCompile(`title="(20\d\d-\d\d-\d\dT?[^"]*)"`)

	now := fixedActivityNow
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	// Seed rows that fall across all three date rails so the stream is
	// non-trivial (multiple bead-rows, interleaved with rails that must NOT
	// count as un-timestamped rows).
	beads := make([]*beadsapi.BeadDetail, 0, 6)
	for i, h := range []int{1, 4, 26, 50, 120, 400} {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:        "gd-ts" + itoa64(int64(i)),
			Title:     "closed thing",
			Type:      "task",
			Status:    "closed",
			Labels:    []string{"project:gardenio"},
			CreatedAt: now.Add(-time.Duration(h*2) * time.Hour),
			UpdatedAt: now.Add(-time.Duration(h) * time.Hour),
		})
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: beads}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/u/alice", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	// Strip <style>/<script> first (as measure-flat.py's _strip does) so a CSS
	// `.bead-row`/`title=` literal can't be mistaken for a real row.
	body := stripStyleScript(w.Body.String())

	rows := beadRowRe.FindAllStringSubmatch(body, -1)
	if len(rows) < 2 {
		t.Fatalf("expected multiple bead-rows on the merged stream, got %d; body=%s", len(rows), body)
	}
	rowsWithTS := 0
	for i, m := range rows {
		row := m[1]
		hasRelAge := false
		for _, tok := range metaRe.FindAllStringSubmatch(row, -1) {
			if relAgeRe.MatchString(tok[1]) {
				hasRelAge = true
				break
			}
		}
		hasAbsTS := absTSRe.MatchString(row)
		if hasRelAge || hasAbsTS {
			rowsWithTS++
			continue
		}
		t.Errorf("row %d has no time anchor (no relative .meta age, no absolute title= stamp): %q", i, row)
	}
	coverage := float64(rowsWithTS) / float64(len(rows))
	if coverage != 1.0 {
		t.Errorf("timestamp_coverage = %.4f (%d/%d rows), want 1.0 — a stream row lost its timestamp",
			coverage, rowsWithTS, len(rows))
	}
}

// TestHandleUserHome_SummaryBand asserts the top-of-fold accomplishment band
// renders with stat tiles for the eager sections, and that the renamed
// "Merged MRs" label is used instead of the old "Recent commits" mismatch
// (ka-0000007148 / ka-0000004155).
func TestHandleUserHome_SummaryBand(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The summary band exists and leads the section list.
	summaryIdx := strings.Index(body, `class="user-home-summary"`)
	if summaryIdx < 0 {
		t.Fatalf("summary band not rendered, body=%s", body)
	}
	firstSection := strings.Index(body, `class="user-home-section`)
	if firstSection >= 0 && summaryIdx > firstSection {
		t.Errorf("summary band must precede the section list (summary=%d first-section=%d)", summaryIdx, firstSection)
	}

	// Stat tiles label the accomplishments with recency framing.
	for _, want := range []string{"Merged MRs", "Closed work", "Needs you", "user-home-stat-num"} {
		if !strings.Contains(body, want) {
			t.Errorf("summary band missing %q, body=%s", want, body)
		}
	}

	// The old mislabeled "Recent commits" heading must be gone — the section
	// queries merged MRs, so it is titled "Merged MRs" (ka-0000004155).
	if strings.Contains(body, "Recent commits") {
		t.Errorf("stale 'Recent commits' label still present (commits/MRs mismatch), body=%s", body)
	}
}

// summaryTileHrefRe extracts the in-page anchor target of every summary stat
// tile (the clickable <a class="user-home-stat-face" href="#…" data-facet-link>
// inside a .user-home-stat <li>). Scoped to data-facet-link so it never matches
// the many other #anchors on the page. Zero-count tiles render as an inert <div>
// with NO href/data-facet-link (bug ka-0000003914), so this only ever matches the
// live (non-zero) tiles.
var summaryTileHrefRe = regexp.MustCompile(`<a class="user-home-stat-face" href="#([^"]+)" data-facet-link=`)

// seedFullSummaryPopulations seeds each summary-tile population NON-zero so all
// five tiles render as live (clickable) anchors: an active agent → "Active
// agents", an open decision → "Needs you", a merge-request → both "Merged MRs"
// and "Open MRs" (both route through the fake's mrResp), and a closed task →
// "Closed work" (routes through listResp). With every count > 0 no tile is inert
// (bug ka-0000003914), so all five carry data-facet-link anchors.
func seedFullSummaryPopulations(f *fakeDaemon) {
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{ID: "gd-agent", Title: "live agent", Type: "agent", Status: "in_progress"}},
		Total: 1,
	}
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{ID: "gd-dec", Title: "open decision", Type: "decision", Status: "open"}},
		Total: 1,
	}
	f.mrResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{ID: "gd-mr", Title: "a merge request", Type: "merge-request", Status: "open"}},
		Total: 1,
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-closed", Title: "closed task", Type: "task", Status: "closed",
			Labels: []string{"project:gardenio"},
		}},
		Total: 1,
	}
}

// TestHandleUserHome_SummaryTileAnchorsResolve is the regression guard for bug
// ka-0000000915: every summary stat tile is an <a href="#id">, and BEFORE this
// fix those ids (#recent-commits, #recent-merged, #open-mrs, #open-decisions,
// #active-agents) were rendered nowhere — the merged-spine rework
// (ka-0000002041) deleted the per-section cards that carried them, so all five
// clicks were silent no-ops that no screenshot rubric could catch. This asserts
// every rendered tile href="#target" resolves to an id="target" in the SAME
// response body. It would have failed on the buggy code and fails again if any
// future rework drops the #activity spine anchor or reintroduces per-section
// hrefs without the matching ids.
//
// All five populations are seeded NON-zero so every tile renders as a live
// anchor — a zero-count tile is now inert (no <a>, bug ka-0000003914), so the
// "all five anchors resolve" invariant only applies to the clickable tiles.
func TestHandleUserHome_SummaryTileAnchorsResolve(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	seedFullSummaryPopulations(f)
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	matches := summaryTileHrefRe.FindAllStringSubmatch(body, -1)
	if len(matches) != 5 {
		t.Fatalf("expected 5 summary stat-tile anchors, found %d (regex drift or tiles missing), body=%s", len(matches), body)
	}
	for _, m := range matches {
		target := m[1]
		if !strings.Contains(body, `id="`+target+`"`) {
			t.Errorf("summary tile links to #%s but no id=%q is rendered anywhere in the page (dead click — bug ka-0000000915)", target, target)
		}
	}
}

// summaryZeroTileLiRe matches a single summary <li> that carries the is-zero
// marker class (a zero-count tile).
var summaryZeroTileLiRe = regexp.MustCompile(`<li class="user-home-stat[^"]*\bis-zero\b[^"]*">((?s).*?)</li>`)

// TestHandleUserHome_ZeroTilesAreInert is the direct regression guard for bug
// ka-0000003914: a summary stat tile that reads 0 must NOT be clickable. Before
// the fix EVERY tile (including zero tiles) was an <a href="#activity"
// data-facet-link="<facet>">; clicking a zero tile fired activate(<facet>),
// which found no matching chip (the facet's population is empty) and fell through
// to currentFacet=” — i.e. it silently UN-filtered the stream and dumped the
// operator into the whole unfiltered activity record, the OPPOSITE of the filter
// the "0" promised. The fix renders a zero tile as an inert <div>, no <a>, no
// data-facet-link, so there is nothing to click and nothing to mislead.
//
// Seeds ONLY the active-agents population non-zero, leaving the other four tiles
// at 0, then asserts: (1) exactly ONE data-facet-link anchor renders (the live
// agents tile), (2) no is-zero tile contains an <a> or a data-facet-link, and
// (3) each zero tile still renders its figure via the inert .user-home-stat-face
// body. It fails on the buggy code (5 anchors, zero tiles clickable) and again if
// a future rework re-wraps zero tiles in a link.
func TestHandleUserHome_ZeroTilesAreInert(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	// Only the active-agents population is non-zero; the other four tiles read 0.
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{ID: "gd-agent", Title: "live agent", Type: "agent", Status: "in_progress"}},
		Total: 1,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Exactly one live (clickable) tile — the non-zero "Active agents" tile.
	anchors := summaryTileHrefRe.FindAllStringSubmatch(body, -1)
	if len(anchors) != 1 {
		t.Fatalf("expected exactly 1 clickable summary tile (only the non-zero one), found %d — a zero tile is still a link (bug ka-0000003914); body=%s", len(anchors), body)
	}
	// And that one anchor deep-links into the agents facet.
	if !strings.Contains(body, `data-facet-link="`+facetAgents+`"`) {
		t.Errorf("the one live tile should deep-link into the %q facet; body=%s", facetAgents, body)
	}

	// Four tiles read 0 (Merged MRs, Open MRs, Needs you, Closed work). Each must
	// be inert: no <a>, no data-facet-link anywhere inside the is-zero <li>.
	zeros := summaryZeroTileLiRe.FindAllStringSubmatch(body, -1)
	if len(zeros) != 4 {
		t.Fatalf("expected 4 zero-count tiles, found %d; body=%s", len(zeros), body)
	}
	for _, z := range zeros {
		inner := z[1]
		if strings.Contains(inner, "<a ") || strings.Contains(inner, "<a>") {
			t.Errorf("a zero-count tile is wrapped in an <a> — clicking it silently clears the filter (bug ka-0000003914); tile=%s", z[0])
		}
		if strings.Contains(inner, "data-facet-link") {
			t.Errorf("a zero-count tile carries data-facet-link — a dead deep-link into an empty facet (bug ka-0000003914); tile=%s", z[0])
		}
		// The figure still renders via the inert body wrapper.
		if !strings.Contains(inner, "user-home-stat-face") {
			t.Errorf("zero-count tile is missing its inert .user-home-stat-face body; tile=%s", z[0])
		}
	}
}

// TestHandleUserHome_ZeroTilesHaveDisabledCue is the regression guard for bug
// ka-0000000523: an inert (zero-count) stat tile must be visually distinguishable
// from a clickable one WITHOUT hovering. The prior fix (ka-0000003914) made zero
// tiles inert <div> faces (functionally safe), but the only visual cue was the
// dimmed number — the border, background, label, and cursor were identical to a
// live tile, so an operator couldn't tell at a glance which tiles filter and
// which are dead weight. The fix gives the whole .is-zero tile a distinct
// muted/disabled treatment. The CSS is inline in layout.html so it ships in every
// rendered page body; assert the disabled-cue rules are present.
func TestHandleUserHome_ZeroTilesHaveDisabledCue(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Each of these rules is a distinct non-clickable cue an operator reads at a
	// glance: a dashed muted border + flat background sets the tile apart, and
	// cursor:default on the inert body means hovering never suggests it's
	// clickable. Losing any of them regresses the affordance ambiguity.
	for _, want := range []string{
		".user-home-stat.is-zero { border-style: dashed;",
		".user-home-stat.is-zero .user-home-stat-face { cursor: default; }",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered page missing inert-tile disabled cue %q — a zero-count tile is visually indistinguishable from a clickable one (bug ka-0000000523)", want)
		}
	}
}

// mrTileFacetRe pairs a live "Merged MRs"/"Open MRs" summary tile with the
// data-facet-link it deep-links into. The <a> emits the facet attribute BEFORE
// the label span (user_home.html), so the capture reads facet(1) then label(2);
// [^>]*? keeps it inside the single opening <a> tag. Restricting label(2) to the
// two MR tiles keeps it off the other three tiles.
var mrTileFacetRe = regexp.MustCompile(
	`data-facet-link="(mrs-[a-z]+)"[^>]*>\s*<span class="user-home-stat-num">\d+</span>\s*<span class="user-home-stat-label">(Merged MRs|Open MRs)`)

// activityRowFacetRe pairs an activity stream row's bead id with the data-facet
// it carries, so a test can assert which facet chip a given MR row answers to.
// The <li> emits data-facet before the row body; the bead id follows in the
// first <code> of the row's anchor.
var activityRowFacetRe = regexp.MustCompile(
	`data-facet="(mrs-[a-z]+)"[^>]*>(?s).*?<code>((?:gd|ka)-[A-Za-z0-9]+)</code>`)

// TestHandleUserHome_MergedVsOpenMRTilesDistinctFacets is the regression guard
// for bug ka-0000008262: the "Merged MRs (14d)" and "Open MRs" summary tiles
// counted different populations (closed vs open merge-requests) yet BOTH carried
// data-facet-link="mrs", and every activity row from either population was tagged
// data-facet="mrs" too. So clicking either tile filtered the stream to the
// identical union set — and "Merged MRs 0" clicked through to open drafts, the
// label and the result disagreeing.
//
// The fix gives the two tiles DISTINCT facet slugs (mrs-merged / mrs-open) and
// tags each stream row with the facet of the population it came from. This seeds
// the two MR populations DISJOINTLY (one closed/merged MR, two open MRs) and
// asserts: (1) the two tiles carry different data-facet-link values, (2) each
// tile's facet matches the count it displays (merged tile → the facet the merged
// MR row carries; open tile → the facet the open MR rows carry), and (3) no row
// is tagged with the coarse legacy "mrs" facet. It would fail on the buggy code
// (both tiles "mrs", collapsed) and fails again if a future rework re-merges the
// two facets.
func TestHandleUserHome_MergedVsOpenMRTilesDistinctFacets(t *testing.T) {
	now := fixedActivityNow
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	// Merged MRs (14d) population: one CLOSED merge-request, updated inside the
	// window. Routed via mergedMrResp (Statuses=[closed]).
	f.mergedMrResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-mrmerged", Title: "a merged MR", Type: "merge-request", Status: "closed",
			UpdatedAt: now.Add(-2 * time.Hour),
		}},
		Total: 1,
	}
	// Open MRs population: two OPEN merge-requests. Routed via mrResp
	// (Statuses=[open,in_progress]). Distinct count (2) from the merged tile (1)
	// makes the collapse observable — the bug report's 5 disagreeing ops.
	f.mrResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-mropenA", Title: "open draft A", Type: "merge-request", Status: "open", UpdatedAt: now.Add(-3 * time.Hour)},
			{ID: "gd-mropenB", Title: "open draft B", Type: "merge-request", Status: "open", UpdatedAt: now.Add(-4 * time.Hour)},
		},
		Total: 2,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// (1) Both MR tiles are live anchors and carry DISTINCT facet slugs.
	tileFacet := map[string]string{} // label -> facet
	for _, m := range mrTileFacetRe.FindAllStringSubmatch(body, -1) {
		tileFacet[m[2]] = m[1]
	}
	if tileFacet["Merged MRs"] == "" || tileFacet["Open MRs"] == "" {
		t.Fatalf("expected both MR tiles to be live facet anchors, got %#v; body=%s", tileFacet, body)
	}
	if tileFacet["Merged MRs"] == tileFacet["Open MRs"] {
		t.Fatalf("Merged MRs and Open MRs tiles share facet %q — clicking either shows the same rows (bug ka-0000008262)", tileFacet["Merged MRs"])
	}

	// (2) Each stream row is tagged with the facet of the population it came from,
	// and that facet matches the tile counting it. Build id -> facet from the rows,
	// then assert the merged MR answers to the Merged tile's facet and the open MRs
	// to the Open tile's facet.
	rowFacet := map[string]string{}
	for _, m := range activityRowFacetRe.FindAllStringSubmatch(body, -1) {
		rowFacet[m[2]] = m[1]
	}
	if got := rowFacet["gd-mrmerged"]; got != tileFacet["Merged MRs"] {
		t.Errorf("merged MR row facet = %q, want the Merged MRs tile facet %q — a click on Merged MRs would not surface this row", got, tileFacet["Merged MRs"])
	}
	for _, id := range []string{"gd-mropenA", "gd-mropenB"} {
		if got := rowFacet[id]; got != tileFacet["Open MRs"] {
			t.Errorf("open MR row %s facet = %q, want the Open MRs tile facet %q", id, got, tileFacet["Open MRs"])
		}
	}
	// The merged MR must NOT answer to the Open tile's chip, and vice-versa —
	// the disjointness that makes each tile's post-click count equal its own
	// number.
	if rowFacet["gd-mrmerged"] == tileFacet["Open MRs"] {
		t.Errorf("merged MR row is tagged with the Open MRs facet — the two populations still collapse (bug ka-0000008262)")
	}

	// (3) No row nor tile carries the coarse legacy "mrs" facet.
	if strings.Contains(body, `data-facet-link="mrs"`) || strings.Contains(body, `data-facet="mrs"`) {
		t.Errorf("the coarse legacy \"mrs\" facet is still emitted — merged/open MRs must split (bug ka-0000008262); body=%s", body)
	}
}

// TestHandleUserHome_LinksToFullInbox is the regression guard for bug
// ka-0000004829: the full /u/{slug}/inbox page was reachable only by typing the
// URL. The merged-spine rework (ka-0000002041) dropped the lazy /section/inbox
// shell that set Embedded=true — the only place the partial's "Open inbox ›"
// link ever rendered — so nothing in the UI pointed at the full page. This
// asserts /u/{slug} renders an eager, Embedded-independent link to
// /u/{slug}/inbox. It would have failed on the buggy code (zero links to the
// full inbox) and fails again if a future rework drops the entry point.
func TestHandleUserHome_LinksToFullInbox(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The homepage must link to the full inbox page directly — no Embedded gate,
	// no lazy shell. The href is /u/{slug}/inbox, the same URL handleUserInbox
	// serves.
	m := inboxLinkRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("homepage does not link to the full inbox page /u/{slug}/inbox (orphaned full page — bug ka-0000004829); body=%s", body)
	}
	inboxHref := m[1]
	if inboxHref != "/u/alice/inbox" {
		t.Errorf("inbox link href = %q, want /u/alice/inbox", inboxHref)
	}

	// End-to-end click-through: following the rendered link must land on the
	// live full-page inbox (id="inbox"), not a 404 — closing the loop the bug
	// left open (the page was healthy but had no entry point).
	req2 := httptest.NewRequest("GET", inboxHref, nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != 200 {
		t.Fatalf("following the inbox link %q returned %d, want 200; body=%s", inboxHref, w2.Code, w2.Body.String())
	}
	if !strings.Contains(w2.Body.String(), `id="inbox"`) {
		t.Errorf("inbox link target did not render the full inbox page (no id=\"inbox\"); body=%s", w2.Body.String())
	}
}

// inboxLinkRe extracts the href of the profile-card "Messages ›" link that
// points at the full /u/{slug}/inbox page (bug ka-0000004829). Anchored on the
// /inbox suffix so it never matches the /search "All beads ›" sibling link.
var inboxLinkRe = regexp.MustCompile(`href="(/u/[^"]+/inbox)"`)

// TestHandleUserHome_EmptyActivityCollapses asserts that an operator with no
// activity renders ONE compact empty activity card — not a scatter of empty
// section bands (the repetitive-empty-band defect ka-0000002884 that the
// merged-spine fix, ka-0000002041, eliminates). With the empty fake the stream
// has no events, so the single #activity section renders is-empty.
func TestHandleUserHome_EmptyActivityCollapses(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Exactly one activity section, and it is the compact empty card — no
	// row-bearing is-filled band when there is nothing to show.
	if !strings.Contains(body, `id="activity"`) {
		t.Errorf("expected the merged activity section, body=%s", body)
	}
	if strings.Contains(body, "user-home-section is-filled") {
		t.Errorf("empty operator should not render an is-filled row band, body=%s", body)
	}
	if !strings.Contains(body, "No recent activity.") {
		t.Errorf("expected compact empty-activity message, body=%s", body)
	}
	// The old per-section empty cards (Open decisions / Active agents / …) are
	// gone entirely.
	if strings.Contains(body, `id="open-decisions"`) || strings.Contains(body, `id="active-agents"`) {
		t.Errorf("stale per-section empty cards still present, body=%s", body)
	}
}

// TestBuildUserHomeSummary covers the stat-tile derivation: counts pulled off
// the eager sections by slug, accent flags on accomplishments, and HasSignal.
func TestBuildUserHomeSummary(t *testing.T) {
	sections := []userHomeSection{
		{Slug: sectionRecentCommits, Count: 7},
		{Slug: sectionRecentMerged, Count: 40},
		{Slug: sectionOpenMRs, Count: 0},
		{Slug: sectionOpenDecisions, Count: 1},
		{Slug: sectionActiveAgents, Count: 0},
	}
	sum := buildUserHomeSummary(sections)
	if !sum.HasSignal {
		t.Errorf("expected HasSignal true when any count > 0")
	}
	if len(sum.Stats) != 5 {
		t.Fatalf("expected 5 stat tiles, got %d", len(sum.Stats))
	}
	byLabel := map[string]userHomeStatTile{}
	for _, st := range sum.Stats {
		byLabel[st.Label] = st
	}
	if byLabel["Merged MRs"].Count != 7 {
		t.Errorf("Merged MRs count = %d, want 7", byLabel["Merged MRs"].Count)
	}
	if !byLabel["Merged MRs"].Accent {
		t.Errorf("Merged MRs should be an accent (accomplishment) tile")
	}
	if !byLabel["Closed work"].Recency {
		t.Errorf("Closed work should be recency-framed")
	}
	if byLabel["Needs you"].Count != 1 {
		t.Errorf("Needs you count = %d, want 1", byLabel["Needs you"].Count)
	}

	// Every tile anchors at the single #activity spine and carries a facet
	// deep-link (bug ka-0000000915 — the old per-section hrefs pointed at
	// anchors the merged-spine rework deleted, so every click was a no-op).
	wantFacet := map[string]string{
		"Merged MRs":    facetMRsMerged,
		"Closed work":   facetClosed,
		"Open MRs":      facetMRsOpen, // distinct facet from Merged MRs (bug ka-0000008262) — each tile filters its own rows
		"Needs you":     facetDecisions,
		"Active agents": facetAgents,
	}
	for label, wantF := range wantFacet {
		st := byLabel[label]
		if st.Href != "#"+sectionActivitySpine {
			t.Errorf("%q tile Href = %q, want %q", label, st.Href, "#"+sectionActivitySpine)
		}
		if st.Facet != wantF {
			t.Errorf("%q tile Facet = %q, want %q", label, st.Facet, wantF)
		}
	}

	// All-zero sections ⇒ no signal.
	empty := buildUserHomeSummary([]userHomeSection{
		{Slug: sectionRecentCommits, Count: 0},
		{Slug: sectionOpenDecisions, Count: 0},
	})
	if empty.HasSignal {
		t.Errorf("expected HasSignal false when all counts are 0")
	}
}

// TestHandleUserHome_UnknownSlug returns 404 for a slug that doesn't resolve.
func TestHandleUserHome_UnknownSlug(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/u/ghost", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestHandleUserHomeSectionForwarded asserts the lazy fragment route renders
// when the operator has followed beads.
func TestHandleUserHomeSectionForwarded(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	f.followedBeadIDsByOp = map[int64][]string{
		42: {"gd-aaa"}, // newFake() seeds gd-aaa with title "Example bead".
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice/section/forwarded", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="forwarded-mentions"`,
		"gd-aaa",
		"Example bead",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q: %s", want, body)
		}
	}
}

// TestHandleUserHomeSectionForwarded_Empty asserts the empty-state message
// renders when the daemon returns no follows.
func TestHandleUserHomeSectionForwarded_Empty(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/u/alice/section/forwarded", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Nothing has been forwarded") {
		t.Errorf("expected empty message, got %s", w.Body.String())
	}
}

// TestHandleUserHomeSectionRecentComments asserts the comments-light fragment
// route renders. With no comment-index endpoint, the section approximates
// "touched since creation" — seed a bead whose UpdatedAt > CreatedAt and assert
// it lands in the section.
func TestHandleUserHomeSectionRecentComments(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
	}
	now := time.Now()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-touched",
				Title:     "Touched recently",
				Type:      "task",
				Status:    "in_progress",
				CreatedAt: now.Add(-3 * 24 * time.Hour),
				UpdatedAt: now.Add(-1 * time.Hour),
			},
			{
				ID:        "gd-fresh",
				Title:     "Created not touched",
				Type:      "task",
				Status:    "open",
				CreatedAt: now.Add(-2 * time.Hour),
				UpdatedAt: now.Add(-2 * time.Hour),
			},
		},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/alice/section/recent-comments", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "gd-touched") {
		t.Errorf("expected gd-touched in body: %s", body)
	}
	if strings.Contains(body, "gd-fresh") {
		t.Errorf("gd-fresh (untouched) should be filtered out: %s", body)
	}
}

// TestItoa64 covers the small allocation-free integer formatter that powers
// the op-<id> fallback slug.
// TestHandleUserHome_DisplayNameInHeading asserts the operator's display name
// (resolved from the user bead) renders in the <h1> and <title>, not the raw
// URL slug (gd-Yrg6RxA2oo). Slug-based links must stay slug-based.
func TestHandleUserHome_DisplayNameInHeading(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"op-229": {OperatorID: 229, Found: true},
	}
	userBead := &beadsapi.BeadDetail{
		ID:    "gd-user-matt",
		Type:  "user",
		Kind:  "config",
		Title: "Matt Baker",
		Fields: map[string]string{
			"operator_id":  "229",
			"display_name": "Matt Baker",
			"email":        "matthew.baker@acme.example",
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{userBead}}
	f.beads[userBead.ID] = userBead
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/op-229", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "<h1>Matt Baker") {
		t.Errorf("expected display name in <h1>, body=%s", body)
	}
	if !strings.Contains(body, "<title>Matt Baker · Beads Viewer</title>") {
		t.Errorf("expected display name in <title>, body=%s", body)
	}
	// The sentinel slug must NOT be the heading.
	if strings.Contains(body, "<h1>op-229") {
		t.Errorf("slug rendered in <h1> instead of display name, body=%s", body)
	}
	// Slug-based links are unchanged — the canonical search URL still uses the slug.
	if !strings.Contains(body, "operated_by_slug=op-229") {
		t.Errorf("expected slug-based SearchAllURL unchanged, body=%s", body)
	}
}

// TestHandleUserHome_FallsBackToSlug asserts that an operator with no user
// bead (hence no resolvable name) renders the slug in <h1>/<title> exactly as
// before — the regression guard for the fallback path.
func TestHandleUserHome_FallsBackToSlug(t *testing.T) {
	f := newFake()
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"op-307": {OperatorID: 307, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/u/op-307", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, "<h1>op-307") {
		t.Errorf("expected slug fallback in <h1>, body=%s", body)
	}
	if !strings.Contains(body, "<title>op-307 · Beads Viewer</title>") {
		t.Errorf("expected slug fallback in <title>, body=%s", body)
	}
}

// peopleCardNameRe extracts the rendered card heading text from a /people row
// (the .people-row-name anchor, whose inner text is {{$row.Display}}). The
// anchor spans several attribute lines, hence the [\s\S] class.
var peopleCardNameRe = regexp.MustCompile(`people-row-name"[\s\S]*?>([^<]*)</a>`)

// userHomeH1Re extracts the /u/{slug} <h1> heading text, stopping at the
// optional " <span>(you)</span>" self marker or the closing tag.
var userHomeH1Re = regexp.MustCompile(`<h1>([^<]*?)(?: <span|</h1>)`)

// TestCrossSurfaceIdentity_PeopleCardMatchesUserHomeH1 is the regression guard
// for ka-0000001547: /people got a sibling-directory + user-bead display-name
// join in MR !6238 (ka-0000006552), but /u/{slug} still resolved its h1 from a
// single user bead — so a name that lives only on a sibling slack row's
// directory Display (op-240 → "Micah Yates") or on the user bead (via the
// email join) rendered the raw slug/op-id as the h1 while /people showed the
// resolved name. The two surfaces MUST state the same name for one human.
//
// Each case renders BOTH surfaces off ONE directory + user-bead fixture and
// asserts the /people card heading == the /u/{slug} h1 == the expected name —
// pinning parity, not just "the name appears somewhere".
func TestCrossSurfaceIdentity_PeopleCardMatchesUserHomeH1(t *testing.T) {
	cases := []struct {
		name      string
		operators []beadsapi.OperatorDirectoryEntry
		userBeads []*beadsapi.BeadDetail
		slug      string
		wantName  string
	}{
		{
			// The verbatim op-240 shape from the bug: OIDC primary with an
			// empty Display and a SENTINEL op-<id> slug, real name only on the
			// sibling slack row's directory Display. No user bead at all.
			name: "sentinel_slug_name_on_slack_sibling",
			operators: []beadsapi.OperatorDirectoryEntry{
				{ID: 240, Kind: "oidc", Subject: "micah.yates@acme.example", Display: "", Email: "micah.yates@acme.example", Slug: "op-240"},
				{ID: 241, Kind: "slack", Subject: "U0MITCH", Display: "Micah Yates", Email: "micah.yates@acme.example", Slug: "op-241"},
			},
			slug:     "op-240",
			wantName: "Micah Yates",
		},
		{
			// The alex.novak shape: a name-shaped slug whose OIDC
			// row carries an empty Display, the real "First Last" living only
			// on the sibling slack row. /u/{slug} used to echo the slug.
			name: "named_slug_name_on_slack_sibling",
			operators: []beadsapi.OperatorDirectoryEntry{
				{ID: 250, Kind: "oidc", Subject: "alex.novak@acme.example", Display: "", Email: "alex.novak@acme.example", Slug: "alex.novak"},
				{ID: 251, Kind: "slack", Subject: "U0ALEKS", Display: "Alex Novak", Email: "alex.novak@acme.example", Slug: "op-251"},
			},
			slug:     "alex.novak",
			wantName: "Alex Novak",
		},
		{
			// Name lives ONLY on the user bead (email-keyed, no operator_id and
			// no sibling Display) — the /people email-join branch. /u/{slug}
			// already resolved this via displayNameFromUserBead; this case pins
			// that the shared join keeps agreeing with it.
			name: "name_on_user_bead_via_email_join",
			operators: []beadsapi.OperatorDirectoryEntry{
				{ID: 260, Kind: "oidc", Subject: "balu@acme.example", Display: "", Email: "balu@acme.example", Slug: "op-260"},
			},
			userBeads: []*beadsapi.BeadDetail{
				{ID: "gd-user-balu", Type: "user", Kind: "config", Fields: map[string]string{
					"email": "balu@acme.example", "display_name": "Balu",
				}},
			},
			slug:     "op-260",
			wantName: "Balu",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.listOperatorsResp = &beadsapi.ListOperatorsResponse{Operators: tc.operators}
			f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{}
			for _, op := range tc.operators {
				f.slugResp[op.Slug] = &beadsapi.LookupOperatorBySlugResponse{OperatorID: op.ID, Found: true}
			}
			f.listResp = &beadsapi.ListBeadsResult{Beads: tc.userBeads}
			mux := mkServer(t, f)

			// /people card heading.
			pw := httptest.NewRecorder()
			mux.ServeHTTP(pw, httptest.NewRequest("GET", "/people", nil))
			if pw.Code != 200 {
				t.Fatalf("/people: expected 200, got %d body=%s", pw.Code, pw.Body.String())
			}
			pm := peopleCardNameRe.FindStringSubmatch(pw.Body.String())
			if pm == nil {
				t.Fatalf("/people: could not extract card heading\nbody: %s", pw.Body.String())
			}
			cardName := strings.TrimSpace(pm[1])

			// /u/{slug} h1.
			uw := httptest.NewRecorder()
			mux.ServeHTTP(uw, httptest.NewRequest("GET", "/u/"+tc.slug, nil))
			if uw.Code != 200 {
				t.Fatalf("/u/%s: expected 200, got %d body=%s", tc.slug, uw.Code, uw.Body.String())
			}
			hm := userHomeH1Re.FindStringSubmatch(uw.Body.String())
			if hm == nil {
				t.Fatalf("/u/%s: could not extract h1\nbody: %s", tc.slug, uw.Body.String())
			}
			h1Name := strings.TrimSpace(hm[1])

			if cardName != tc.wantName {
				t.Errorf("/people card heading = %q, want %q", cardName, tc.wantName)
			}
			if h1Name != tc.wantName {
				t.Errorf("/u/%s h1 = %q, want %q (the raw slug/op-id leaking as the heading is the bug)", tc.slug, h1Name, tc.wantName)
			}
			if cardName != h1Name {
				t.Errorf("cross-surface identity mismatch: /people card %q != /u/%s h1 %q", cardName, tc.slug, h1Name)
			}
		})
	}
}

// stripStyleScript removes <style> and <script> blocks so content-only greps
// don't false-positive on CSS attribute selectors / JS string literals. Mirrors
// the _strip() step in tools/measure-flat.py.
var styleScriptRe = regexp.MustCompile(`(?is)<(style|script)[^>]*>.*?</(style|script)>`)

func stripStyleScript(html string) string {
	return styleScriptRe.ReplaceAllString(html, "")
}

// --- Step 3 (ka-0000003002): "Their plans" homepage section -----------------
//
// These pin the /u/{slug} "their plans" section: it reuses Step 1's
// plansInvolvingOperator query (creator ∪ step-owner ∪ decision-resolver ∪
// gardener-owner), resolves the page-owner's alias via emailForOperator, links
// each row to /plans/{id}, and folds into the merged activity stream under the
// "plans" facet — suppressed (no chip, no rows) when the operator drives none.

// seedTheirPlansOperator wires the fake so operatorID resolves to `email` via
// the operator directory (emailForOperator) and `slug` maps to operatorID. This
// is what lets buildTheirPlansSection bridge the numeric homepage id to the
// canonical oidc alias the involvement query keys on.
func seedTheirPlansOperator(f *fakeDaemon, slug, email string, operatorID int64) {
	if f.slugResp == nil {
		f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{}
	}
	f.slugResp[slug] = &beadsapi.LookupOperatorBySlugResponse{OperatorID: operatorID, Found: true}
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: operatorID, Kind: "oidc", Subject: email, Email: email, Slug: slug},
		},
	}
}

// TestBuildTheirPlansSection_CreatorPlan proves the section surfaces a plan the
// operator created, links it to /plans/{id}, and scopes its "See all" link to
// that operator on the /plans index (not /search).
func TestBuildTheirPlansSection_CreatorPlan(t *testing.T) {
	f := seedPlansFake() // creator facet (hotEpicsResp) serves gd-plan1
	seedTheirPlansOperator(f, "matt", "matt@acme.example", 42)
	s := planAssistantTestServer(f)

	sec := s.buildTheirPlansSection(context.Background(), 42, "matt")
	if sec.Slug != sectionTheirPlans {
		t.Errorf("slug = %q, want %q", sec.Slug, sectionTheirPlans)
	}
	if sec.Count != 1 || len(sec.Beads) != 1 {
		t.Fatalf("expected exactly one involved plan, got count=%d rows=%+v", sec.Count, beadIDs2(sec.Beads))
	}
	row := sec.Beads[0]
	if row.ID != "gd-plan1" {
		t.Errorf("row id = %q, want gd-plan1", row.ID)
	}
	if row.Href != "/plans/gd-plan1" {
		t.Errorf("row href = %q, want /plans/gd-plan1 (plan-agent view, not the generic bead page)", row.Href)
	}
	// See-all scopes /plans to the operator alias — the shared ?operated_by= param,
	// NOT the /search-only ?operated_by_slug= (which /plans does not handle).
	if want := "/plans?operated_by=" + "oidc%3Amatt%40acme.example"; sec.MoreURL != want {
		t.Errorf("MoreURL = %q, want %q", sec.MoreURL, want)
	}
}

// TestBuildTheirPlansSection_InvolvedPlanFoldedIn proves the section is a strict
// superset of "plans I created": a plan the operator gardeners but did not create
// is folded in via the involvement query's gardener facet.
func TestBuildTheirPlansSection_InvolvedPlanFoldedIn(t *testing.T) {
	f := seedPlansFake()
	extra := seedInvolvementExtraPlan(f, "gd-capplan", "gardenio", time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC))
	seedPlanGardener(f, "gd-cap-extra", extra.ID, "true")
	seedTheirPlansOperator(f, "matt", "matt@acme.example", 42)
	s := planAssistantTestServer(f)

	sec := s.buildTheirPlansSection(context.Background(), 42, "matt")
	got := beadIDs2(sec.Beads)
	if !containsStr(got, "gd-plan1") || !containsStr(got, "gd-capplan") {
		t.Fatalf("their-plans should fold the gardenered-but-not-created plan in alongside the creator plan; got %v", got)
	}
	// Every row links into the plan-agent view.
	for _, r := range sec.Beads {
		if r.Href != "/plans/"+r.ID {
			t.Errorf("row %s href = %q, want /plans/%s", r.ID, r.Href, r.ID)
		}
	}
}

// TestBuildTheirPlansSection_NoEmailDegradesEmpty proves an operator with no
// resolvable directory email (so no alias) yields an empty section rather than
// erroring — the involvement query has nothing to key on, and the homepage must
// still render.
func TestBuildTheirPlansSection_NoEmailDegradesEmpty(t *testing.T) {
	f := seedPlansFake() // hotEpicsResp would serve a plan IF the alias resolved
	// Deliberately DO NOT seed the operator directory: emailForOperator → "".
	s := planAssistantTestServer(f)

	sec := s.buildTheirPlansSection(context.Background(), 99, "ghost")
	if sec.Count != 0 || len(sec.Beads) != 0 {
		t.Errorf("unresolvable operator should yield an empty section, got count=%d rows=%v", sec.Count, beadIDs2(sec.Beads))
	}
	if sec.MoreURL != "" {
		t.Errorf("empty section should carry no See-all link, got %q", sec.MoreURL)
	}
}

// TestHandleUserHome_TheirPlansInStream is the end-to-end wiring proof: the
// homepage renders the involved plan in the merged activity stream, under a
// "Plans" facet chip, linking to /plans/{id}.
func TestHandleUserHome_TheirPlansInStream(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	f := seedPlansFake() // creator facet (hotEpicsResp) serves gd-plan1
	seedTheirPlansOperator(f, "matt", "matt@acme.example", 42)
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/u/matt", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "/plans/gd-plan1") {
		t.Errorf("their-plans row should link to /plans/gd-plan1; body=%s", snippetN(body, 2000))
	}
	// The plan folds into the stream under the "plans" facet chip.
	if !strings.Contains(body, `data-facet="plans"`) {
		t.Errorf("expected a plans-facet row in the merged stream; body=%s", snippetN(body, 2000))
	}
	if !strings.Contains(body, ">Plans <span") {
		t.Errorf("expected a Plans facet chip; body=%s", snippetN(body, 2000))
	}
}

// TestHandleUserHome_NoPlansNoFacet proves the section is suppressed when the
// operator drives no plans: no plans-facet row, no Plans chip — keeping the
// non-power-user homepage compact.
func TestHandleUserHome_NoPlansNoFacet(t *testing.T) {
	now := time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)
	f := newFake() // no hotEpicsResp → creator facet empty; no involvement facets
	seedTheirPlansOperator(f, "matt", "matt@acme.example", 42)
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/u/matt", nil))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, `data-facet="plans"`) {
		t.Errorf("no involved plans ⇒ no plans-facet row should render; body=%s", snippetN(body, 1500))
	}
	if strings.Contains(body, ">Plans <span") {
		t.Errorf("no involved plans ⇒ no Plans facet chip should render; body=%s", snippetN(body, 1500))
	}
}

// beadIDs2 projects userHomeBeadRow slices to their ids (beadIDs in
// plans_involvement_test.go operates on *beadsapi.BeadDetail; containsStr lives
// in plan_edit_test.go).
func beadIDs2(rows []userHomeBeadRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

func TestItoa64(t *testing.T) {
	cases := map[int64]string{
		0:     "0",
		1:     "1",
		42:    "42",
		-7:    "-7",
		12345: "12345",
	}
	for in, want := range cases {
		got := itoa64(in)
		if got != want {
			t.Errorf("itoa64(%d) = %q, want %q", in, got, want)
		}
	}
}
