package main

import (
	"errors"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// peopleFixture seeds two operators with slugs — Alice with a status message
// (M2 §3 forward-compat path: status_message in a type=user/kind=config bead's
// Fields), and Bob without one — plus a third operator that has no slug and
// must not appear (the daemon only returns slugged rows but we still seed it
// to catch any client-side regression).
func peopleFixture() *fakeDaemon {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 42, Kind: "oidc", Subject: "alice@example.com", Display: "Alice Anderson", Email: "alice@example.com", Slug: "alice"},
			{ID: 99, Kind: "oidc", Subject: "bob@example.com", Display: "", Email: "bob@example.com", Slug: "bob"},
		},
	}
	// Status messages live on type=user/kind=config beads; the fallthrough
	// branch of fakeDaemon.ListBeadsFiltered returns f.listResp for the
	// query the handler issues.
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:   "gd-user-alice",
				Type: "user",
				Kind: "config",
				Fields: map[string]string{
					"operator_id":    "42",
					"status_message": "Shipping the people directory.",
				},
			},
			{
				ID:   "gd-user-bob",
				Type: "user",
				Kind: "config",
				Fields: map[string]string{
					"operator_id": "99",
				},
			},
		},
	}
	// Slug lookup for the hover-card route.
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"alice": {OperatorID: 42, Found: true},
		"bob":   {OperatorID: 99, Found: true},
	}
	return f
}

func TestHandlePeople_RendersDirectory(t *testing.T) {
	mux := mkServer(t, peopleFixture())

	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Users",
		"Alice Anderson",
		"alice@example.com",
		"/u/alice",
		"Shipping the people directory.",
		// Bob has no display — falls back to email.
		"bob@example.com",
		"/u/bob",
		"/htmx/operator-hover/alice",
		"/htmx/operator-hover/bob",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("people directory missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandlePeople_RendersStats seeds the daemon's real per-operator
// aggregate (OperatorStats) for two operators and verifies the directory
// renders both an "N open" count and a humanized "active <ago>" label per
// row. Alice has two open beads (last active ~5m ago), Bob one (~2h ago) — so
// both rows have distinct stats and Alice sorts ahead of Bob in the
// activity-sorted view. A zero-id stat entry must be ignored, not crash.
func TestHandlePeople_RendersStats(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			{OperatorID: 42, OpenBeads: 2, LastActive: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
			{OperatorID: 99, OpenBeads: 1, LastActive: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
			// Zero-id entry (unattributed sentinel) must be ignored.
			{OperatorID: 0, OpenBeads: 7, LastActive: now.UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"2 open",
		"1 open",
		"active 5m",
		"active 2h",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats render missing %q", want)
		}
	}
	// Alice (5m ago) must sort ahead of Bob (2h ago) in the activity-sorted
	// directory. Check by index of the row's data-operator-id attribute.
	aliceIdx := strings.Index(body, `data-operator-id="42"`)
	bobIdx := strings.Index(body, `data-operator-id="99"`)
	if aliceIdx < 0 || bobIdx < 0 {
		t.Fatalf("expected both operator rows in body, got alice=%d bob=%d", aliceIdx, bobIdx)
	}
	if aliceIdx >= bobIdx {
		t.Errorf("expected alice (more recent) to sort ahead of bob: alice=%d bob=%d", aliceIdx, bobIdx)
	}
}

// TestHandlePeople_DormantRecencyChipLabelOrder pins the recency chip's word
// order (gd-17bKVIeGzE): a fully-dormant operator (no stat entry → empty
// LastActiveAgo) must render the chip label-first as "active —", matching the
// "active <ago>" word order of active rows and the "<glyph> — <unit>" missing-
// value convention of the sibling stat chips. The old else-branch emitted
// "— active" (placeholder-then-label), flipping label/value order between
// active and dormant rows down the /people column.
func TestHandlePeople_DormantRecencyChipLabelOrder(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	// Only Alice has activity; Bob has no stat entry at all → fully dormant,
	// empty LastActiveAgo, so his recency chip takes the else-branch.
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			{OperatorID: 42, OpenBeads: 2, LastActive: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Dormant rows must read label-first, consistent with active rows.
	if !strings.Contains(body, "active —") {
		t.Errorf("expected dormant recency chip rendered label-first as \"active —\", body: %s", body)
	}
	// The old placeholder-then-label order must be gone entirely.
	if strings.Contains(body, "— active") {
		t.Errorf("dormant recency chip still renders the inconsistent \"— active\" order, body: %s", body)
	}
}

// TestHandlePeople_SortsByAllStatusRecency pins the root-cause fix: recency
// is the daemon's max(updated_at) across ALL statuses, not just
// open/in_progress. Zara has activity from a recent CLOSED bead (zero open
// count); Bob owns nothing. With an open-only count both would tie at the
// zero LastActive and fall through to the alphabetic tie-breaker. Here Bob's
// display sorts alphabetically BEFORE Zara ("Bob Baker" vs "Zara Zane"), so
// an alphabetic-only sort would put Bob first — the test only passes if the
// all-status last-active recency lifts Zara above Bob.
func TestHandlePeople_SortsByAllStatusRecency(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 42, Kind: "oidc", Subject: "zara@example.com", Display: "Zara Zane", Email: "zara@example.com", Slug: "zara"},
			{ID: 99, Kind: "oidc", Subject: "bob@example.com", Display: "Bob Baker", Email: "bob@example.com", Slug: "bob"},
		},
	}
	now := time.Now()
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			// Zara: zero open beads, but recent all-status activity.
			{OperatorID: 42, OpenBeads: 0, LastActive: now.Add(-10 * time.Minute).UTC().Format(time.RFC3339)},
			// Bob has no stat entry at all — zero-value LastActive.
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Zara's closed-bead activity must lift her above Bob even though "Bob"
	// sorts alphabetically first — proving recency uses all statuses.
	zaraIdx := strings.Index(body, `data-operator-id="42"`)
	bobIdx := strings.Index(body, `data-operator-id="99"`)
	if zaraIdx < 0 || bobIdx < 0 {
		t.Fatalf("expected both operator rows, got zara=%d bob=%d", zaraIdx, bobIdx)
	}
	if zaraIdx >= bobIdx {
		t.Errorf("expected zara (recent closed activity) to sort ahead of bob (no activity): zara=%d bob=%d", zaraIdx, bobIdx)
	}
	// Zara owns no open/in_progress beads, so the open-count chip is the
	// em-dash, but the activity chip must still show recency.
	if !strings.Contains(body, "active 10m") {
		t.Errorf("expected zara's all-status recency 'active 10m' chip, body: %s", body)
	}
}

// TestHandlePeople_RecentCreatedList verifies the per-operator "recently
// created" chip list rendered from the daemon's OperatorStats.Recent feed.
// The daemon does the grouping (by created_by_operator_id), the created_at
// desc ordering, the per-operator cap, and the agent/session exclusion; the
// viewer renders the rows it gets back in order, capped at peopleRecentBeadCap
// per operator, linking each to its canonical bead page.
func TestHandlePeople_RecentCreatedList(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	recent := []beadsapi.OperatorRecentBeadEntry{}
	// Alice's chips arrive already ordered newest-first from the daemon. Seed
	// peopleRecentBeadCap+1 so the viewer's defensive per-operator cap trims
	// the trailing one.
	for i := range peopleRecentBeadCap + 1 {
		recent = append(recent, beadsapi.OperatorRecentBeadEntry{
			OperatorID: 42,
			BeadID:     "gd-alice-c" + string(rune('a'+i)),
			Title:      "Alice task " + string(rune('a'+i)),
			CreatedAt:  now.Add(-time.Duration(i) * time.Minute).UTC().Format(time.RFC3339),
		})
	}
	// Bob created exactly one bead.
	recent = append(recent, beadsapi.OperatorRecentBeadEntry{
		OperatorID: 99,
		BeadID:     "gd-bob-created",
		Title:      "Bob feature",
		CreatedAt:  now.Add(-time.Hour).UTC().Format(time.RFC3339),
	})
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{Recent: recent}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Newest chip present (i=0).
	if !strings.Contains(body, "gd-alice-ca") {
		t.Errorf("expected newest created bead chip gd-alice-ca, body: %s", body)
	}
	// The cap+1'th chip (i=peopleRecentBeadCap) is trimmed by the viewer's
	// defensive per-operator cap.
	overCap := "gd-alice-c" + string(rune('a'+peopleRecentBeadCap))
	if strings.Contains(body, overCap) {
		t.Errorf("expected over-cap bead %s to be trimmed", overCap)
	}
	// Bob's single created bead renders.
	if !strings.Contains(body, "gd-bob-created") {
		t.Errorf("expected bob's created bead chip, body: %s", body)
	}
	// Chips are linked to the canonical bead page.
	if !strings.Contains(body, `href="/gd-bob-created"`) {
		t.Errorf("expected recent chip linked to /gd-bob-created, body: %s", body)
	}
}

// TestHandlePeople_RecentThreadRequests verifies the "recent requests" preview
// strip (ka-0000005614): each card shows a few of the human's most-recent
// thread-request messages (type=message slack-thread-message beads keyed on
// OperatedByID), truncated, each linking to /threads/{thread_id}. The handler
// pulls ONE bounded scan and groups by operator, capping at
// peopleRecentMessageCap and preferring newest-first.
func TestHandlePeople_RecentThreadRequests(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	ptr := func(n int64) *int64 { return &n }
	beads := map[string]*beadsapi.BeadDetail{}
	// Alice (op 42) sent peopleRecentMessageCap+1 thread-request messages; the
	// oldest must be trimmed by the per-operator cap. Newest-first ordering is
	// driven by CreatedAt (i=0 newest).
	for i := range peopleRecentMessageCap + 1 {
		id := "gd-alice-msg" + string(rune('a'+i))
		beads[id] = &beadsapi.BeadDetail{
			ID:           id,
			Type:         "message",
			Labels:       []string{"slack-thread-message"},
			OperatedByID: ptr(42),
			Fields: map[string]string{
				"text":      "alice request " + string(rune('a'+i)),
				"thread_id": "gd-thread-a" + string(rune('a'+i)),
			},
			CreatedAt: now.Add(-time.Duration(i) * time.Minute),
		}
	}
	// A very long ask that must be truncated to a preview.
	longAsk := strings.Repeat("please make the widget do the thing ", 20)
	beads["gd-bob-msg"] = &beadsapi.BeadDetail{
		ID:           "gd-bob-msg",
		Type:         "message",
		Labels:       []string{"slack-thread-message"},
		OperatedByID: ptr(99),
		Fields: map[string]string{
			"text":      longAsk,
			"thread_id": "gd-thread-bob",
		},
		CreatedAt: now.Add(-3 * time.Hour),
	}
	f.beads = beads
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// The strip label and the newest ask preview render.
	if !strings.Contains(body, "recent requests:") {
		t.Errorf("expected 'recent requests:' label, body: %s", body)
	}
	if !strings.Contains(body, "alice request a") {
		t.Errorf("expected alice's newest request preview, body: %s", body)
	}
	// Each preview links to the full thread.
	if !strings.Contains(body, `href="/threads/gd-thread-aa"`) {
		t.Errorf("expected alice's preview linked to /threads/gd-thread-aa, body: %s", body)
	}
	// The cap+1'th (oldest) ask is trimmed by the per-operator cap.
	overCap := "alice request " + string(rune('a'+peopleRecentMessageCap))
	if strings.Contains(body, overCap) {
		t.Errorf("expected over-cap request %q to be trimmed, body: %s", overCap, body)
	}
	// Bob's long ask is truncated: the full repeated body must NOT appear intact,
	// but its thread link must.
	if strings.Contains(body, longAsk) {
		t.Errorf("expected bob's long ask to be truncated, not rendered whole")
	}
	if !strings.Contains(body, `href="/threads/gd-thread-bob"`) {
		t.Errorf("expected bob's preview linked to /threads/gd-thread-bob, body: %s", body)
	}
}

// TestHandlePeople_RecentRequestWordBoundaryTruncation guards the fix for
// ka-0000003211: recent-request previews truncated MID-WORD (e.g.
// "…should be I…", "…ability to do a…") because the visible clip was the CSS
// `-webkit-line-clamp: 2`, not the server's word-boundary truncate(). The
// clamp truncates at a CHARACTER boundary. The fix has two coupled halves that
// this test pins together:
//
//  1. The server cap (peopleMessagePreviewLen) must be small enough that a
//     word-boundary-truncated ask fits within two lines of the preview, so the
//     CSS clamp never re-truncates a real prose ask. If someone raises the cap
//     back toward 140 the clamp re-engages and the mid-word regression returns.
//  2. .people-request-text must render at the smaller preview font (the strip
//     is documented as "muted, small"); at the inherited 16px body font even a
//     short ask spilled past two lines in a ~17rem card and the clamp had to
//     truncate it.
func TestHandlePeople_RecentRequestWordBoundaryTruncation(t *testing.T) {
	// Half 1: the server preview cap stays within the two-line budget. 40 is the
	// measured ceiling for a word-truncated ask at the 0.82rem preview font in
	// the TIGHTEST real card — the ~170px .people-request-text box inside a 17rem
	// grid track on a 1280px desktop (the flex recency tag + padding eat the rest
	// of the 296px card). Measured against an adversarial wide-word battery in a
	// headless Chromium render of the real /people page. This is the invariant
	// that keeps the CSS clamp a safety net rather than the truncator.
	const twoLineCharBudget = 40
	if peopleMessagePreviewLen > twoLineCharBudget {
		t.Errorf("peopleMessagePreviewLen=%d exceeds the ~%d-char two-line budget at the "+
			"preview font — a word-truncated ask no longer fits the .people-request-text "+
			"clamp, so -webkit-line-clamp re-truncates it MID-WORD (ka-0000003211)",
			peopleMessagePreviewLen, twoLineCharBudget)
	}

	// A word-boundary truncate of a long ask must end on a whole word (never
	// mid-token) and stay within the cap.
	longAsk := "analyzing the attached diagnostic videos should be considered impossible"
	got := truncate(longAsk, peopleMessagePreviewLen)
	if len(got) > peopleMessagePreviewLen {
		t.Errorf("truncate(%q, %d) = %q is longer than the cap", longAsk, peopleMessagePreviewLen, got)
	}
	// The cut must be a prefix that ends at a word boundary: the next rune in the
	// source is whitespace (or we consumed the whole string).
	if got != longAsk {
		if next := longAsk[len(got)]; next != ' ' && next != '\t' && next != '\n' {
			t.Errorf("truncate(%q, %d) = %q cut MID-WORD (next rune %q is not whitespace) "+
				"— previews must clip on a word boundary (ka-0000003211)",
				longAsk, peopleMessagePreviewLen, got, string(next))
		}
	}

	// Half 2: the preview text is rendered at the smaller "muted, small" font so
	// a word-truncated ask fits two lines rather than being clamped mid-word.
	mux := mkServer(t, peopleFixture())
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	reqText := sliceAfter(w.Body.String(), ".people-request-text {")
	if i := strings.Index(reqText, "}"); i >= 0 {
		reqText = reqText[:i]
	}
	if reqText == "" {
		t.Fatal(".people-request-text rule not found in rendered /people page")
	}
	if !strings.Contains(reqText, "font-size:") {
		t.Error(".people-request-text must set a smaller preview font-size — at the " +
			"inherited 16px body font a short ask spills past two lines in a 17rem card " +
			"and -webkit-line-clamp truncates it mid-word (ka-0000003211)")
	}
	// The clamp stays (it's the safety net), but word-break:break-word must NOT
	// be reintroduced: it would let the browser break normal words to fill the
	// line, re-creating the mid-word clip the server truncation is meant to own.
	if !strings.Contains(reqText, "-webkit-line-clamp: 2;") {
		t.Error(".people-request-text lost its two-line clamp safety net (ka-0000003211)")
	}
	if strings.Contains(reqText, "word-break: break-word;") {
		t.Error(".people-request-text sets word-break: break-word — that lets the browser " +
			"split normal words to fill a line, re-introducing the mid-word clip the " +
			"server-side word-boundary truncation is meant to own (ka-0000003211)")
	}
}

// TestHandlePeople_ResponsiveCardGrid is the static guardrail for the density
// + overflow fix (ka-0000005531 / ka-0000002686). The directory MUST render as
// a responsive multi-column card grid (so wide viewports carry 2-3 columns
// instead of one tall column with a ~87%-white right gutter), and the long,
// unbreakable identity tokens (gitlab bot emails, long oidc subjects) MUST be
// allowed to shrink+wrap (so a 480px viewport no longer overflows to 1126px).
//
// It asserts on the rendered <style>, so an edit that reverts the grid or drops
// the wrap rule (re-introducing the whitespace/overflow regressions) fails the
// build.
func TestHandlePeople_ResponsiveCardGrid(t *testing.T) {
	mux := mkServer(t, peopleFixture())
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Density fix: .people-list must be a multi-column responsive grid with a
	// shrinkable track floor (min(...) inside minmax so it collapses to one
	// column on narrow viewports rather than flooring at content width).
	grid := sliceAfter(body, ".people-list {")
	if grid == "" {
		t.Fatal(".people-list rule not found in rendered /people page")
	}
	if !strings.Contains(grid, "display: grid;") {
		t.Error(".people-list is not a grid — the single-column list leaves a wide " +
			"right gutter and a 6664px scroll for 42 people (ka-0000005531)")
	}
	if !strings.Contains(grid, "repeat(auto-fill, minmax(min(") {
		t.Error(".people-list grid track is not a responsive auto-fill minmax(min(...)) — " +
			"it must pack multiple cards per row on wide viewports yet collapse to one " +
			"column when narrow (ka-0000005531)")
	}

	// Density anchor guardrail (ka-0000000172): the first grid pass halved the
	// page height but left Density capped at 4/10 because desktop
	// near_white_ratio stuck at ~0.69 (>= the 0.55 anchor). Two roots: the grid
	// track floored at 22rem (only ~3 columns, acres of dead gutter) and the
	// card fill rgba(128,128,128,0.04) composited to ~250 over white — above the
	// audit's R,G,B>=240 near-white cutoff, so every card interior still scored
	// as blank pixels. Assert the tighter floor and a card fill opaque enough to
	// clear the cutoff, so neither root silently returns.
	cardRule := sliceAfter(body, ".people-list .people-row {")
	if i := strings.Index(cardRule, "}"); i >= 0 {
		cardRule = cardRule[:i]
	}
	if cardRule == "" {
		t.Fatal(".people-list .people-row card rule not found in rendered /people page")
	}
	if alpha := cardFillAlpha(cardRule); alpha < 0.13 {
		t.Errorf(".people-row card fill alpha %.3f is too translucent — an rgba fill "+
			"under ~0.13 composites to >=240 over white and the CDQR audit counts the "+
			"whole card interior as near-white, hard-capping Density at <=4 (ka-0000000172)", alpha)
	}
	// The auto-fill track floor must stay tight enough to pack >2 columns on a
	// 1280px desktop; a floor at/above 20rem regresses to the whitespace-heavy
	// 3-column grid that could not clear the density anchor.
	if !strings.Contains(grid, "minmax(min(17rem") {
		t.Error(".people-list track floor is not min(17rem, ...) — a wider floor packs " +
			"fewer columns and leaves the near-white gutter that caps Density (ka-0000000172)")
	}

	// Equal-height cards (ka-0000000689): the grid must NOT pin align-items to
	// `start`, which shrink-wraps each card to its own content and leaves ragged
	// column bottoms when a person carries fewer signals (no status/email/recent
	// strip). It must stretch cards to a uniform row height. Scope to just the
	// `.people-list {` declaration block — `align-items: start` is used by many
	// unrelated layout rules elsewhere in the rendered page.
	listRule := grid
	if i := strings.Index(listRule, "}"); i >= 0 {
		listRule = listRule[:i]
	}
	if strings.Contains(listRule, "align-items: start;") {
		t.Error(".people-list uses align-items: start — cards shrink-wrap to their " +
			"content and the 4-column grid bottoms come out ragged (ka-0000000689); " +
			"use align-items: stretch so cards equalize to the tallest in the row")
	}
	if !strings.Contains(listRule, "align-items: stretch;") {
		t.Error(".people-list must set align-items: stretch so shorter cards fill the " +
			"row height and the grid bottoms stay even (ka-0000000689)")
	}

	// Overflow fix (ka-0000002686): the recent-bead chip must be allowed to
	// shrink and wrap so an unbreakable long title cannot force horizontal page
	// overflow. .chip-type defaults to flex-shrink:0, so the rule must override
	// with min-width:0 + overflow-wrap:anywhere on the recent chip.
	recent := sliceAfter(body, ".people-row-recent .people-recent-bead {")
	if i := strings.Index(recent, "}"); i >= 0 {
		recent = recent[:i]
	}
	if recent == "" {
		t.Fatal(".people-row-recent .people-recent-bead rule not found in rendered /people page")
	}
	for _, want := range []string{"min-width: 0;", "overflow-wrap: anywhere;"} {
		if !strings.Contains(recent, want) {
			t.Errorf("recent-bead chip wrap rule missing %q — long unbreakable "+
				"titles will force narrow-viewport horizontal overflow (ka-0000002686)", want)
		}
	}

	// Identity-chip single-line fix (ka-0000005472): the primary "oidc <subject>"
	// identity chip wrapped to 2–3 lines on the ~273px 4-column desktop card
	// because the shared overflow-wrap:anywhere rule let the whole chip wrap.
	// The chip must now lay out as an inline-flex row that stays on ONE line and
	// clips the handle — but NOT re-introduce the old wrap. min-width:0 keeps the
	// chip shrinkable so a pathological unbreakable handle still cannot force
	// 480px horizontal overflow (ka-0000002686 stays fixed).
	identChip := sliceAfter(body, ".people-row-identities .people-identity {")
	if i := strings.Index(identChip, "}"); i >= 0 {
		identChip = identChip[:i]
	}
	if identChip == "" {
		t.Fatal(".people-row-identities .people-identity rule not found in rendered /people page")
	}
	for _, want := range []string{"display: inline-flex;", "white-space: nowrap;", "min-width: 0;"} {
		if !strings.Contains(identChip, want) {
			t.Errorf(".people-row-identities .people-identity rule missing %q — the "+
				"oidc <subject> chip wraps to a second line on the ~273px desktop card "+
				"instead of staying on one line (ka-0000005472)", want)
		}
	}
	if strings.Contains(identChip, "overflow-wrap: anywhere;") {
		t.Error(".people-row-identities .people-identity must not set overflow-wrap:anywhere — " +
			"that lets the oidc <subject> chip wrap to a second line, the layout this fix removes " +
			"(ka-0000005472); rely on inline-flex + nowrap and clip only the handle instead")
	}

	// Only the handle clips; the leading kind label and trailing /u/<slug> link
	// stay pinned at natural width (flex:0 0 auto) so a whole-chip ellipsis can
	// never clip the dashboard link off the widest cards (ka-0000005472).
	handle := sliceAfter(body, ".people-row-identities .people-identity-handle {")
	if i := strings.Index(handle, "}"); i >= 0 {
		handle = handle[:i]
	}
	if handle == "" {
		t.Fatal(".people-row-identities .people-identity-handle rule not found in rendered /people page")
	}
	for _, want := range []string{"overflow: hidden;", "text-overflow: ellipsis;", "min-width: 0;"} {
		if !strings.Contains(handle, want) {
			t.Errorf(".people-identity-handle rule missing %q — the oidc subject must "+
				"ellipsize (not wrap, not clip the trailing /u/ link) (ka-0000005472)", want)
		}
	}
	// The handle text node must be wrapped in the .people-identity-handle span so
	// the CSS above targets it — a bare text node cannot be ellipsized on its own.
	if !strings.Contains(body, `<span class="people-identity-handle">`) {
		t.Error("identity chip is missing the .people-identity-handle span wrapper — the " +
			"oidc <subject> cannot be ellipsized without it (ka-0000005472)")
	}

	// Heading wrap fix (ka-0000007601): when the resolved Display is still a raw
	// email/op-id, the single unbreakable token wraps MID-WORD at ~480px (e.g.
	// 'sam.rivers@acme.'/'ai'). .people-row-name must break at any
	// point (overflow-wrap:anywhere) and clamp to two lines so a residual long
	// token wraps cleanly instead of splitting a word or ballooning the card.
	name := sliceAfter(body, ".people-row-name {")
	if i := strings.Index(name, "}"); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		t.Fatal(".people-row-name rule not found in rendered /people page")
	}
	for _, want := range []string{"overflow-wrap: anywhere;", "-webkit-line-clamp: 2;"} {
		if !strings.Contains(name, want) {
			t.Errorf(".people-row-name heading rule missing %q — long raw email/handle "+
				"headings wrap mid-word at narrow widths (ka-0000007601)", want)
		}
	}

	// Canonical-email single-line fix (ka-0000007547): a full-length address
	// (sam.rivers@acme.example) wrapped to a second line in the ~273px
	// 4-column desktop card. The .people-row-email line must stay on ONE line
	// and ellipsize the overflow (nowrap + overflow:hidden + text-overflow) so
	// it reads as a compact secondary identifier, and must NOT re-introduce the
	// old overflow-wrap:anywhere/word-break wrap that let it fall to two lines.
	// The clip is overflow-safe: .bead-row-text carries min-width:0, so even a
	// pathological unbreakable address cannot force 480px horizontal overflow.
	email := sliceAfter(body, ".people-row-email {")
	if i := strings.Index(email, "}"); i >= 0 {
		email = email[:i]
	}
	if email == "" {
		t.Fatal(".people-row-email rule not found in rendered /people page")
	}
	for _, want := range []string{"white-space: nowrap;", "overflow: hidden;", "text-overflow: ellipsis;"} {
		if !strings.Contains(email, want) {
			t.Errorf(".people-row-email rule missing %q — a long canonical email wraps to "+
				"a second line in the ~273px desktop card instead of staying on one line "+
				"(ka-0000007547)", want)
		}
	}
	if strings.Contains(email, "overflow-wrap: anywhere;") || strings.Contains(email, "word-break: break-word;") {
		t.Error(".people-row-email must not set overflow-wrap:anywhere/word-break:break-word — " +
			"those let the canonical email wrap to a second line, the layout this fix removes " +
			"(ka-0000007547); rely on nowrap + ellipsis instead")
	}
}

// cardFillAlpha extracts the alpha channel of the first rgba(...) background in
// a CSS declaration block, or 0 if there is none. Used by the density guardrail
// to reject a card fill so translucent it composites to near-white.
func cardFillAlpha(css string) float64 {
	bg := sliceAfter(css, "background:")
	_, rest, ok := strings.Cut(bg, "rgba(")
	if !ok {
		return 0
	}
	inner, _, ok := strings.Cut(rest, ")")
	if !ok {
		return 0
	}
	parts := strings.Split(inner, ",")
	if len(parts) < 4 {
		return 0
	}
	alpha, err := strconv.ParseFloat(strings.TrimSpace(parts[3]), 64)
	if err != nil {
		return 0
	}
	return alpha
}

// TestHandlePeople_RendersPlansCountAndLink verifies §4 Step 4 (ka-0000000236):
// each row carries a plans chip whose count is the number of ACTIVE plans that
// human CREATED (grouped from a single plan-universe scan by creator email, NOT
// a per-row involvement fan-out), and the chip LINKS to the person's involved
// set via ?operated_by=<canonical oidc alias>. Alice created two plans, Bob one.
func TestHandlePeople_RendersPlansCountAndLink(t *testing.T) {
	f := peopleFixture()
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkPlanEpic("gd-a1", "oidc:alice@example.com", "alice@example.com", "Alice Anderson"),
			mkPlanEpic("gd-a2", "oidc:alice@example.com", "alice@example.com", "Alice Anderson"),
			mkPlanEpic("gd-b1", "oidc:bob@example.com", "bob@example.com", "Bob"),
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"people-stat-plans",
		"🗺 2 plans", // Alice created two
		"🗺 1 plans", // Bob created one
		// Each chip links to that person's involved plans on /plans (Step 1's
		// involvement query resolves the alias live on the click).
		"/plans?operated_by=oidc%3Aalice%40example.com",
		"/plans?operated_by=oidc%3Abob%40example.com",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plans chip render missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandlePeople_PlansCountZeroEmDash verifies the em-dash placeholder when a
// human created no active plans — the chip still renders (with a link) so the
// directory reads consistently, but shows "— plans" rather than "0 plans".
func TestHandlePeople_PlansCountZeroEmDash(t *testing.T) {
	f := peopleFixture()
	// Only Alice created a plan; Bob created none.
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mkPlanEpic("gd-a1", "oidc:alice@example.com", "alice@example.com", "Alice Anderson"),
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "🗺 1 plans") {
		t.Errorf("expected Alice's '🗺 1 plans' chip\nbody: %s", body)
	}
	if !strings.Contains(body, "🗺 — plans") {
		t.Errorf("expected Bob's zero-plan em-dash '🗺 — plans' chip\nbody: %s", body)
	}
}

// TestHandlePeople_PlansCountFoldsAliasesAndSkipsNonHuman proves two things at
// once: (1) a plan created under a human's SLACK alias still counts toward their
// deduped (OIDC-primary) row, because the count folds by resolved creator email —
// the same email key the directory groups identities on; and (2) plans owned by
// a machine sentinel (fleet:/system:) or an agent (empty operated_by) do NOT
// count, since the directory counts PEOPLE's plans.
func TestHandlePeople_PlansCountFoldsAliasesAndSkipsNonHuman(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			// Casey: an OIDC primary + a Slack alias, folded by shared email.
			{ID: 10, Kind: "slack", Subject: "U123", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey-slack"},
			{ID: 11, Kind: "oidc", Subject: "casey@example.com", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey"},
		},
	}
	f.hotEpicsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			// One plan under Casey's OIDC id, one under a Slack alias id that still
			// resolves to casey@example.com — both must fold onto Casey's one row.
			mkPlanEpic("gd-c1", "oidc:casey@example.com", "casey@example.com", "Casey Jones"),
			mkPlanEpic("gd-c2", "slack:U123", "casey@example.com", "Casey Jones"),
			// A machine-sentinel plan and an agent-created plan — neither counts.
			mkPlanEpic("gd-sys", "fleet:slack-bridge", "", ""),
			mkPlanEpic("gd-agent", "", "", ""),
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Casey's two plans (OIDC + Slack alias) fold into one count of 2.
	if !strings.Contains(body, "🗺 2 plans") {
		t.Errorf("expected Casey's folded count of 2 (oidc + slack alias)\nbody: %s", body)
	}
	// The sentinel/agent plans must not inflate the count to 3 or 4.
	for _, absent := range []string{"🗺 3 plans", "🗺 4 plans"} {
		if strings.Contains(body, absent) {
			t.Errorf("non-human plans must not count: found %q\nbody: %s", absent, body)
		}
	}
}

// TestHandlePeople_DeEmphasizesQuietCards pins the signal-per-pixel Polish fix
// (ka-0000005627): a near-empty card — an operator with NO activity signal at
// all (every stat zero, no recent beads/requests, no status message) — is
// flagged data-quiet so the CSS can de-emphasize it, while a card with any real
// signal keeps full weight (no data-quiet). Alice has open beads + a status
// message (rich); Bob has nothing (quiet).
func TestHandlePeople_DeEmphasizesQuietCards(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	// Alice: real activity. Bob: no stat entry at all → all-zero, quiet.
	// (peopleFixture gives Alice a status_message and Bob none.)
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			{OperatorID: 42, OpenBeads: 3, LastActive: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Exactly one card is flagged quiet — Bob's. Alice's (open beads + status)
	// is not. Count the element form `data-quiet="true">` (the attribute closes
	// the <li> tag) so the CSS selectors `[data-quiet="true"]` in the <style>
	// block are not miscounted as rows.
	if got := strings.Count(body, `data-quiet="true">`); got != 1 {
		t.Errorf("expected exactly 1 quiet card, got %d\nbody: %s", got, body)
	}
	// Bob (id 99, no activity) is the quiet one; Alice (id 42) is not. Check by
	// locating each row and confirming the flag lands on Bob's <li>.
	bobRow := sliceAfter(body, `data-operator-id="99"`)
	if i := strings.Index(bobRow, "</li>"); i >= 0 {
		bobRow = bobRow[:i]
	}
	if !strings.Contains(bobRow, `data-quiet="true"`) {
		t.Errorf("expected Bob's near-empty row flagged data-quiet\nrow: %s", bobRow)
	}
	aliceRow := sliceAfter(body, `data-operator-id="42"`)
	if i := strings.Index(aliceRow, "</li>"); i >= 0 {
		aliceRow = aliceRow[:i]
	}
	if strings.Contains(aliceRow, `data-quiet="true"`) {
		t.Errorf("expected Alice's active row NOT flagged data-quiet\nrow: %s", aliceRow)
	}

	// The de-emphasis CSS must dim the quiet card's CONTENT and restore it on
	// hover/focus — and must NOT touch the base card fill (density guardrail
	// ka-0000000172 asserts the base fill alpha stays >= 0.13).
	quietRule := sliceAfter(body, `.people-list .people-row[data-quiet="true"] .bead-row-text {`)
	if quietRule == "" {
		t.Fatal("quiet-card de-emphasis rule not found in rendered /people page")
	}
	if !strings.Contains(quietRule, "opacity: 0.6;") {
		t.Error("quiet-card rule must dim its content via opacity so near-empty cards " +
			"read lighter than rich cards (ka-0000005627)")
	}
	if !strings.Contains(body, `.people-list .people-row[data-quiet="true"]:hover .bead-row-text`) {
		t.Error("quiet-card de-emphasis must restore full weight on hover so every card " +
			"stays legible on demand (weighting, not hide/collapse) (ka-0000005627)")
	}
	// The base card fill must NOT be reduced by a whole-card opacity — the
	// de-emphasis targets .bead-row-text, never a card-level opacity that would
	// composite the fill toward white and regress density (ka-0000000172).
	if strings.Contains(body, `.people-list .people-row[data-quiet="true"] {`) {
		quietCardRule := sliceAfter(body, `.people-list .people-row[data-quiet="true"] {`)
		if i := strings.Index(quietCardRule, "}"); i >= 0 {
			quietCardRule = quietCardRule[:i]
		}
		if strings.Contains(quietCardRule, "opacity:") {
			t.Error("quiet card must not set a whole-card opacity — it multiplies the card " +
				"fill toward white and re-opens the density regression (ka-0000000172); " +
				"dim .bead-row-text instead")
		}
	}
}

// TestHandlePeople_DormantStripAlwaysVisible pins the FLAT-directory lever
// (ka-0000000623, replacing the collapsed <details> of ka-0000004086 on top of
// the in-place de-emphasis of ka-0000005627): fully-dormant operators (server
// IsQuiet) are partitioned OUT of the primary directory grid and rendered in an
// ALWAYS-VISIBLE strip below it — NOT behind a disclosure control. The lane's
// north star is that gardenio is FLAT: every human is visible without a click.
// It must be a reorder + de-emphasis, never a hide or a collapse — dormant rows
// stay counted in the header totals and are visible on load. Alice has open
// beads (active); Bob has no activity signal at all (dormant).
func TestHandlePeople_DormantStripAlwaysVisible(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	// Alice: real activity → active grid. Bob: no stat entry → dormant strip.
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			{OperatorID: 42, OpenBeads: 3, LastActive: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The dormant strip exists as an always-visible <section> — NOT a <details>
	// disclosure — and names the dormant count. A residual <details>/<summary>
	// dormant control would mean the operators are hidden behind a click, the
	// exact thing this task removes.
	if !strings.Contains(body, `<section class="people-dormant"`) {
		t.Fatalf("expected an always-visible <section class=\"people-dormant\"> dormant strip\nbody: %s", body)
	}
	if strings.Contains(body, `<details class="people-dormant"`) ||
		strings.Contains(body, "people-dormant-summary") {
		t.Errorf("dormant operators must NOT sit behind a <details>/<summary> disclosure — " +
			"the strip must be visible without a click (ka-0000000623)")
	}
	heading := sliceAfter(body, `<section class="people-dormant"`)
	if i := strings.Index(heading, "</p>"); i >= 0 {
		heading = heading[:i]
	}
	if !strings.Contains(heading, "Dormant operators") || !strings.Contains(heading, "(1)") {
		t.Errorf("expected dormant heading to name the count (1), got: %s", heading)
	}

	// Bob (id 99) must render INSIDE the dormant strip; Alice (id 42) must render
	// in the primary grid ABOVE it. Split the body at the <section> open.
	stripIdx := strings.Index(body, `<section class="people-dormant"`)
	aliceIdx := strings.Index(body, `data-operator-id="42"`)
	bobIdx := strings.Index(body, `data-operator-id="99"`)
	if aliceIdx < 0 || bobIdx < 0 {
		t.Fatalf("expected both operator rows, got alice=%d bob=%d", aliceIdx, bobIdx)
	}
	if aliceIdx >= stripIdx {
		t.Errorf("expected active Alice (id 42) in the primary grid ABOVE the dormant strip: alice=%d strip=%d", aliceIdx, stripIdx)
	}
	if bobIdx <= stripIdx {
		t.Errorf("expected dormant Bob (id 99) INSIDE the always-visible dormant strip: bob=%d strip=%d", bobIdx, stripIdx)
	}

	// Not-a-hide: Bob stays reachable (his /u/ link renders) and stays counted
	// in the header — "2 people" reflects BOTH operators, not just the active one.
	if !strings.Contains(body, "/u/bob") {
		t.Error("dormant Bob must stay reachable — his /u/bob link must still render")
	}
	if !strings.Contains(body, "2 people") {
		t.Errorf("header must still count dormant operators — expected '2 people'\nbody: %s", body)
	}
}

// TestHandlePeople_NoDormantSectionWhenAllActive verifies the strip is
// conditional: with every operator active, no dormant <section> renders at all
// (no empty affordance) (ka-0000000623, was ka-0000004086).
func TestHandlePeople_NoDormantSectionWhenAllActive(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	// Both operators have open beads → both active, no dormant strip.
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			{OperatorID: 42, OpenBeads: 3, LastActive: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339)},
			{OperatorID: 99, OpenBeads: 1, LastActive: now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, `<section class="people-dormant"`) {
		t.Errorf("expected NO dormant strip when every operator is active\nbody: %s", body)
	}
	// Both rows still render in the primary grid.
	if !strings.Contains(body, `data-operator-id="42"`) || !strings.Contains(body, `data-operator-id="99"`) {
		t.Error("expected both active operator rows in the primary grid")
	}
}

// TestPeopleRowIsQuiet unit-tests the IsQuiet predicate directly: any single
// activity signal (a count, a recent bead/request, or a status message) keeps a
// row at full weight; only a fully-empty row is quiet.
func TestPeopleRowIsQuiet(t *testing.T) {
	if !(peopleRow{}).IsQuiet() {
		t.Error("a fully-empty row must be quiet")
	}
	cases := []struct {
		name string
		row  peopleRow
	}{
		{"open beads", peopleRow{OpenBeads: 1}},
		{"in progress", peopleRow{InProgress: 1}},
		{"created 7d", peopleRow{Created7d: 1}},
		{"closed 7d", peopleRow{Closed7d: 1}},
		{"mrs open", peopleRow{MRsOpen: 1}},
		{"mrs merged", peopleRow{MRsMerged7d: 1}},
		{"decisions", peopleRow{DecisionsPending: 1}},
		{"fleet active", peopleRow{FleetActive: 1}},
		{"plans", peopleRow{PlansCount: 1}},
		{"recent beads", peopleRow{RecentBeads: []peopleRecentBead{{ID: "gd-x"}}}},
		{"recent messages", peopleRow{RecentMessages: []peopleThreadMessage{{Text: "hi"}}}},
		{"status message", peopleRow{StatusMessage: "shipping"}},
	}
	for _, c := range cases {
		if c.row.IsQuiet() {
			t.Errorf("row with %s must NOT be quiet", c.name)
		}
	}
}

func TestHandlePeople_EmptyDirectory(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{Operators: nil}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "No operators have a public profile yet.") {
		t.Errorf("empty directory missing fallback copy\nbody: %s", body)
	}
}

func TestHandlePeople_DaemonError(t *testing.T) {
	f := newFake()
	f.listOperatorsErr = errors.New("boom")
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 500 {
		t.Fatalf("expected 500, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleOperatorHoverCard_RendersPartial(t *testing.T) {
	mux := mkServer(t, peopleFixture())

	req := httptest.NewRequest("GET", "/htmx/operator-hover/alice", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"operator-hover-card",
		"Alice Anderson",
		"alice@example.com",
		"/u/alice",
		"Shipping the people directory.",
		"#42",
		"operated_by_slug=alice",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hover card missing %q\nbody: %s", want, body)
		}
	}
}

func TestHandleOperatorHoverCard_UnknownSlug(t *testing.T) {
	mux := mkServer(t, peopleFixture())

	req := httptest.NewRequest("GET", "/htmx/operator-hover/charlie", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 404 {
		t.Fatalf("expected 404 for unknown slug, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestHandleOperatorHoverCard_NoStatusMessageStillRenders(t *testing.T) {
	mux := mkServer(t, peopleFixture())

	req := httptest.NewRequest("GET", "/htmx/operator-hover/bob", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Bob has no display name and no status_message — render falls back to
	// email and omits the status paragraph entirely.
	if !strings.Contains(body, "bob@example.com") {
		t.Errorf("bob hover card missing email fallback\nbody: %s", body)
	}
	if strings.Contains(body, "operator-hover-card-status") {
		t.Errorf("bob hover card should not include status paragraph\nbody: %s", body)
	}
}

// TestHandlePeople_DedupsAliasIdentities seeds three operator rows for the
// same human — an OIDC login and two Slack aliases — that share an identity
// key (email), plus a fourth distinct human. The directory must collapse the
// three into ONE row (the operator's "lots of dupe users" complaint) keyed on
// the OIDC primary, render the other two as labeled chips, and render exactly
// two people-rows total. (Sentinel kinds — system/fleet/gitlab/jira/agent —
// are filtered upstream and covered by TestHandlePeople_FiltersSentinelKinds.)
func TestHandlePeople_DedupsAliasIdentities(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			// Same human, three identities: shared email ties them together.
			{ID: 10, Kind: "slack", Subject: "U123", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey-slack"},
			{ID: 11, Kind: "oidc", Subject: "casey@example.com", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey"},
			{ID: 12, Kind: "slack", Subject: "U999", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey-slack-alt"},
			// A different human — must stay its own row.
			{ID: 20, Kind: "oidc", Subject: "dana@example.com", Display: "Dana Scully", Email: "dana@example.com", Slug: "dana"},
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Exactly two rendered rows (Casey collapsed, Dana standalone).
	if got := strings.Count(body, `class="bead-row people-row"`); got != 2 {
		t.Errorf("expected 2 deduped rows, got %d\nbody: %s", got, body)
	}
	// Casey's rendered primary is the OIDC identity (id 11 / slug casey).
	if !strings.Contains(body, `data-operator-id="11"`) {
		t.Errorf("expected Casey's OIDC identity (id 11) as the primary row")
	}
	if strings.Contains(body, `data-operator-id="10"`) || strings.Contains(body, `data-operator-id="12"`) {
		t.Errorf("Slack aliases must not render as their own rows")
	}
	// Every folded identity appears as a labeled chip on the one row — not as
	// separate rows. The full set (oidc primary + two slack aliases) renders
	// with each kind label and its /u/{slug} link, so the directory
	// disambiguates which handles map to this one human.
	for _, want := range []string{
		"people-identity-primary", // the canonical oidc chip is marked primary
		"U123",                    // first slack subject surfaced as the handle
		"U999",                    // second slack subject surfaced as the handle
		"/u/casey", "/u/casey-slack", "/u/casey-slack-alt",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dedup row missing identity chip %q\nbody: %s", want, body)
		}
	}
	// The old single "also:" alias affordance is gone — we render the full set.
	if strings.Contains(body, "also:") {
		t.Errorf("expected full identity set, not the legacy 'also:' alias chip")
	}
}

// TestHandlePeople_FiltersSentinelKinds pins the kind allowlist: /people must
// surface oidc and slack identities, and silently drop sentinel/bot kinds
// (system, fleet, gitlab, jira, agent, email) — those belong on /fleet. The
// header aggregates also exclude sentinel rows so the "identities merged"
// count is humans-only and the open-bead total is not skewed by op-9-style
// legacy attribution buckets (slack thread context: gd-PTSg1sCyNs).
func TestHandlePeople_FiltersSentinelKinds(t *testing.T) {
	f := newFake()
	now := time.Now()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			// Two humans surface.
			{ID: 11, Kind: "oidc", Subject: "casey@example.com", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey"},
			{ID: 20, Kind: "slack", Subject: "U222", Display: "Dana Scully", Email: "dana@example.com", Slug: "dana"},
			// Six sentinel rows must NOT surface.
			{ID: 100, Kind: "system", Subject: "scheduler", Email: "system-scheduler@noreply.kbeads.local", Slug: "op-100"},
			{ID: 101, Kind: "fleet", Subject: "slack-bridge", Email: "fleet-slack-bridge@noreply.kbeads.local", Slug: "op-101"},
			{ID: 102, Kind: "gitlab", Subject: "gardeniobot", Email: "gitlab-gardeniobot@noreply.kbeads.local", Slug: "op-102"},
			{ID: 103, Kind: "jira", Subject: "trigger", Email: "jira-trigger@noreply.kbeads.local", Slug: "op-103"},
			{ID: 104, Kind: "agent", Subject: "agent-bot", Email: "agent-bot@noreply.kbeads.local", Slug: "op-104"},
			{ID: 105, Kind: "email", Subject: "noreply@example.com", Email: "email-noreply@noreply.kbeads.local", Slug: "op-105"},
		},
	}
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			{OperatorID: 11, OpenBeads: 2, LastActive: now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)},
			{OperatorID: 20, OpenBeads: 3, LastActive: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)},
			// op-100 is the legacy-pre-attribution sink that owns ~240k beads
			// in prod — its count must not appear in the header total.
			{OperatorID: 100, OpenBeads: 240478, LastActive: now.UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Exactly two HUMAN rows (people-row) — the two humans. Sentinel/machine
	// identities render in the separate machine section, never as people-rows.
	if got := strings.Count(body, `class="bead-row people-row"`); got != 2 {
		t.Errorf("expected 2 human rows after sentinel filter, got %d", got)
	}
	// The six sentinel/machine identities now render in the collapsed "Service &
	// machine accounts" section (ka-0000002163) — one machine-row each — rather
	// than being silently dropped from the directory as before.
	if got := strings.Count(body, `class="machine-row"`); got != 6 {
		t.Errorf("expected 6 machine rows in the service-accounts section, got %d\nbody: %s", got, body)
	}
	// Sentinel markers must appear ONLY inside the machine section, never in a
	// people-row: split the body at the machine <details> and assert the human
	// half (above the split) carries none of them.
	humanHalf := body
	if idx := strings.Index(body, `class="people-machines"`); idx >= 0 {
		humanHalf = body[:idx]
	} else {
		t.Fatalf("expected a people-machines section in the body\nbody: %s", body)
	}
	for _, want := range []string{
		`data-operator-id="100"`, `data-operator-id="101"`, `data-operator-id="102"`,
		`data-operator-id="103"`, `data-operator-id="104"`, `data-operator-id="105"`,
		"/u/op-100", "/u/op-101", "/u/op-102", "/u/op-103", "/u/op-104", "/u/op-105",
		"slack-bridge", "gardeniobot", "240478",
	} {
		if strings.Contains(humanHalf, want) {
			t.Errorf("expected sentinel marker %q to be kept out of the human directory\nhumanHalf: %s", want, humanHalf)
		}
	}
	// The machine section DOES surface the identities + the legacy sink's count.
	for _, want := range []string{
		`data-operator-id="100"`, "/u/op-100", "slack-bridge", "gardeniobot", "240478",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected machine marker %q to render in the service-accounts section\nbody: %s", want, body)
		}
	}
	// Header aggregates count HUMANS only — not the 240478-bead sentinel.
	// "identities merged" only renders when raw identity count > rendered
	// people; with no dedup collisions (one OIDC + one Slack), the chip is
	// omitted, and that's the correct behavior post-filter. The machine count
	// is broken out separately.
	for _, want := range []string{
		"2 people",
		"5 open beads",         // 2 + 3, NOT 5 + 240478
		"6 machine identities", // the sentinel/machine split, counted separately
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats header missing %q (sentinel rows must not skew human aggregates)\nbody: %s", want, body)
		}
	}
}

// TestIsMachineOperator pins the human/machine classifier (ka-0000002163): a
// human login kind with a human subject is a human; every sentinel kind is a
// machine; and a human kind carrying a bot/group_/project_ subject is a
// machine. filterHumanOperators and filterMachineOperators must be exact
// complements — no row is dropped from both, none appears in both.
func TestIsMachineOperator(t *testing.T) {
	cases := []struct {
		name    string
		op      beadsapi.OperatorDirectoryEntry
		machine bool
	}{
		{"oidc human", beadsapi.OperatorDirectoryEntry{Kind: "oidc", Subject: "alice@example.com"}, false},
		{"slack human", beadsapi.OperatorDirectoryEntry{Kind: "slack", Subject: "U0123ABC"}, false},
		{"system sentinel", beadsapi.OperatorDirectoryEntry{Kind: "system", Subject: "scheduler"}, true},
		{"fleet sentinel", beadsapi.OperatorDirectoryEntry{Kind: "fleet", Subject: "slack-bridge"}, true},
		{"gitlab sentinel", beadsapi.OperatorDirectoryEntry{Kind: "gitlab", Subject: "gardeniobot"}, true},
		{"jira sentinel", beadsapi.OperatorDirectoryEntry{Kind: "jira", Subject: "trigger"}, true},
		{"agent sentinel", beadsapi.OperatorDirectoryEntry{Kind: "agent", Subject: "agent-bot"}, true},
		{"email sentinel", beadsapi.OperatorDirectoryEntry{Kind: "email", Subject: "noreply@example.com"}, true},
		{"oidc with gitlab bot subject", beadsapi.OperatorDirectoryEntry{Kind: "oidc", Subject: "group_14176856_bot_259bb18f"}, true},
		{"oidc with project bot subject", beadsapi.OperatorDirectoryEntry{Kind: "oidc", Subject: "project_42_bot_abc"}, true},
		{"slack with _bot_ subject", beadsapi.OperatorDirectoryEntry{Kind: "slack", Subject: "svc_bot_1"}, true},
		// A human whose email happens to contain the substring "robot" is NOT a
		// machine — \bbot\b requires "bot" as a whole word.
		{"human with robotics email", beadsapi.OperatorDirectoryEntry{Kind: "oidc", Subject: "pat@robotics.example.com"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isMachineOperator(tc.op); got != tc.machine {
				t.Errorf("isMachineOperator(%+v) = %v, want %v", tc.op, got, tc.machine)
			}
		})
	}

	// Complement invariant over the whole case set.
	all := make([]beadsapi.OperatorDirectoryEntry, 0, len(cases))
	for _, tc := range cases {
		all = append(all, tc.op)
	}
	humans := filterHumanOperators(all)
	machines := filterMachineOperators(all)
	if len(humans)+len(machines) != len(all) {
		t.Errorf("human(%d) + machine(%d) != total(%d): filters are not complements",
			len(humans), len(machines), len(all))
	}
}

// TestHandleOperatorHoverCard_404sOnSentinelSlug pins that the hover-card
// route mirrors /people's kind filter — a sentinel slug (op-100, etc.)
// must 404 rather than render a synthetic "human" card.
func TestHandleOperatorHoverCard_404sOnSentinelSlug(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 11, Kind: "oidc", Subject: "casey@example.com", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey"},
			{ID: 100, Kind: "system", Subject: "scheduler", Email: "system-scheduler@noreply.kbeads.local", Slug: "op-100"},
		},
	}
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"op-100": {OperatorID: 100, Found: true},
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/htmx/operator-hover/op-100", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 404 {
		t.Errorf("expected 404 for sentinel slug, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandlePeople_RendersTopLevelStatsHeader verifies the aggregate banner
// (the operator's "no top level stats displayed" complaint): a people count,
// the merged-identity count, an active-now count, and a total open-beads
// count computed across the deduped row set.
func TestHandlePeople_RendersTopLevelStatsHeader(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 10, Kind: "slack", Subject: "U123", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey-slack"},
			{ID: 11, Kind: "oidc", Subject: "casey@example.com", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey"},
			{ID: 20, Kind: "oidc", Subject: "dana@example.com", Display: "Dana Scully", Email: "dana@example.com", Slug: "dana"},
		},
	}
	now := time.Now()
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			// One open bead attributed to Casey's *Slack* alias (id 10) — must
			// still count toward Casey's collapsed OIDC row via the
			// render-time group fold (group-level aggregation).
			{OperatorID: 10, OpenBeads: 1, LastActive: now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)},
			{OperatorID: 20, OpenBeads: 1, LastActive: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"2 people",            // Casey + Dana after dedup
		"3 identities merged", // 3 raw operator rows
		"2 active now",        // both have open work
		"2 open beads",        // one each, Casey's via the Slack alias
	} {
		if !strings.Contains(body, want) {
			t.Errorf("stats header missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandlePeople_SingletonRendersIdentityChip verifies that even a human
// with a single identity (no folded aliases) still renders one labeled
// identity chip marked primary — the directory consistently labels which
// handle each row is, not only when dedup fired.
func TestHandlePeople_SingletonRendersIdentityChip(t *testing.T) {
	mux := mkServer(t, peopleFixture())
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"people-row-identities",
		"people-identity-primary",
		// Alice's oidc subject is her email; the chip carries it + the slug link.
		`title="oidc identity (canonical login): alice@example.com"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("singleton row missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandleOperatorHoverCard_RendersFullIdentitySet verifies the hover card
// is in sync with the page row: hovering ANY alias slug renders the human's
// full identity set (oidc + slack + agent) and the canonical email — proving
// the page row and the card share one source of truth.
func TestHandleOperatorHoverCard_RendersFullIdentitySet(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 10, Kind: "slack", Subject: "U123", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey-slack"},
			{ID: 11, Kind: "oidc", Subject: "casey@example.com", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey"},
			{ID: 12, Kind: "slack", Subject: "U999", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey-slack-alt"},
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{Beads: nil}
	// The hover route first resolves the slug → operator id via slugResp.
	f.slugResp = map[string]*beadsapi.LookupOperatorBySlugResponse{
		"casey-slack": {OperatorID: 10, Found: true},
	}
	mux := mkServer(t, f)
	// Hover the *slack alias* slug — the card must still resolve to the human's
	// canonical (oidc) primary and render every folded identity.
	req := httptest.NewRequest("GET", "/htmx/operator-hover/casey-slack", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"operator-hover-card-identities",
		"casey@example.com", // canonical email, prominent
		"U123",              // first slack handle chip
		"U999",              // second slack handle chip
		"/u/casey",          // primary slug
		"/u/casey-slack", "/u/casey-slack-alt",
		`data-operator-id="11"`, // resolved to the oidc primary, not the hovered slack alias
	} {
		if !strings.Contains(body, want) {
			t.Errorf("hover card missing full-identity item %q\nbody: %s", want, body)
		}
	}
}

// TestDedupOperators_NoOverMergeWithoutKey ensures two operators with neither
// email nor display name are NOT collapsed — we only merge on a positive
// identity tie, never on a missing key.
func TestDedupOperators_NoOverMergeWithoutKey(t *testing.T) {
	ops := []beadsapi.OperatorDirectoryEntry{
		{ID: 1, Kind: "agent", Subject: "bot-a", Slug: "bot-a"},
		{ID: 2, Kind: "agent", Subject: "bot-b", Slug: "bot-b"},
	}
	groups := dedupOperators(ops)
	if len(groups) != 2 {
		t.Fatalf("expected 2 singleton groups (no key => no merge), got %d", len(groups))
	}
}

// TestHandlePeople_RendersRicherStatStrip verifies the §2.2 per-row stat strip:
// the daemon's superset OperatorStats fields (in_progress, created_7d,
// closed_7d, mrs_open, mrs_merged_7d, decisions_pending, agents_active,
// gardeners_active) all render on the row, with the agents+gardeners fold into a
// single 🤖 chip and merged-MR detail surfaced on the MR chip.
func TestHandlePeople_RendersRicherStatStrip(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			{
				OperatorID: 42, OpenBeads: 12, InProgress: 3,
				Created7d: 7, Closed7d: 5, MRsOpen: 4, MRsMerged7d: 2,
				DecisionsPending: 1, AgentsActive: 6, GardenersActive: 2,
				LastActive: now.Add(-5 * time.Minute).UTC().Format(time.RFC3339),
			},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"12 open",      // open count
		"3 wip",        // in-progress split
		"↑ 7 created",  // created 7d
		"↓ 5 closed",   // closed 7d
		"⇄ 4 MR",       // MRs open
		"↳2 merged 7d", // merged-in-window detail on the MR chip
		"⚑ 1 dec",      // decisions pending
		"🤖 8",          // agents (6) + gardeners (2) folded into one chip
		"people-stat-fleet",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("richer stat strip missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandlePeople_ZeroStatsRenderEmDash verifies the §2.2 "em-dash for zero"
// rule: an operator with no created/closed/MR/decision/fleet work renders the
// em-dash placeholder in each stat chip rather than a bare "0", and the new
// header throughput chips are omitted entirely when their fleet sum is zero.
func TestHandlePeople_ZeroStatsRenderEmDash(t *testing.T) {
	f := peopleFixture()
	now := time.Now()
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			// Alice has only open beads — every other count is zero.
			{OperatorID: 42, OpenBeads: 2, LastActive: now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"↑ — created",
		"↓ — closed",
		"⇄ — MR",
		"⚑ — dec",
		"🤖 —",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected em-dash placeholder %q for zero stat\nbody: %s", want, body)
		}
	}
	// With no merged MRs, the MR chip carries no "(↳N merged 7d)" detail.
	if strings.Contains(body, "merged 7d") {
		t.Errorf("expected no merged-7d detail when MRsMerged7d is zero")
	}
	// Header throughput chips omitted entirely when the fleet sum is zero —
	// keyed on the header chip class so the per-row chips/titles don't match.
	for _, absent := range []string{
		"people-summary-created", "people-summary-closed",
		"people-summary-mrs", "people-summary-decisions", "people-summary-gardeners",
	} {
		if strings.Contains(body, absent) {
			t.Errorf("expected header chip %q omitted when its fleet sum is zero", absent)
		}
	}
}

// TestHandlePeople_RendersRicherHeaderSums verifies §2.3: the header throughput
// + backlog chips are render-time sums of the per-row stats (never a separate
// query), so they aggregate across the deduped group set — including a stat
// attributed to a folded Slack alias rather than the rendered OIDC primary.
func TestHandlePeople_RendersRicherHeaderSums(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			// Casey: OIDC primary + a Slack alias that carries some of the work.
			{ID: 10, Kind: "slack", Subject: "U123", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey-slack"},
			{ID: 11, Kind: "oidc", Subject: "casey@example.com", Display: "Casey Jones", Email: "casey@example.com", Slug: "casey"},
			{ID: 20, Kind: "oidc", Subject: "dana@example.com", Display: "Dana Scully", Email: "dana@example.com", Slug: "dana"},
		},
	}
	now := time.Now()
	f.operatorStatsResp = &beadsapi.OperatorStatsResponse{
		Stats: []beadsapi.OperatorStatEntry{
			// Work on Casey's OIDC + Slack alias must both fold into Casey's row.
			{OperatorID: 11, Created7d: 4, Closed7d: 2, MRsOpen: 1, DecisionsPending: 1, GardenersActive: 1, LastActive: now.Add(-1 * time.Minute).UTC().Format(time.RFC3339)},
			{OperatorID: 10, Created7d: 3, MRsOpen: 2, GardenersActive: 1, LastActive: now.Add(-2 * time.Minute).UTC().Format(time.RFC3339)},
			{OperatorID: 20, Created7d: 1, Closed7d: 1, DecisionsPending: 2, LastActive: now.Add(-1 * time.Hour).UTC().Format(time.RFC3339)},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"↑8 created 7d",        // 4 + 3 (Casey, both identities) + 1 (Dana)
		"↓3 closed 7d",         // 2 (Casey) + 1 (Dana)
		"⇄3 open MRs",          // 1 + 2 (Casey, both identities)
		"⚑3 pending decisions", // 1 (Casey) + 2 (Dana)
		"⛵2 gardeners",         // 1 + 1 (Casey, both identities)
	} {
		if !strings.Contains(body, want) {
			t.Errorf("richer header sums missing %q\nbody: %s", want, body)
		}
	}
}

// TestHandlePeople_HeadingResolvesNameNotEmail pins the Defect-1 fix
// (ka-0000006552): a human whose chosen OIDC primary carries an EMPTY Display
// must render the real "First Last" the page /u/{slug} shows — from a sibling
// slack row and/or the user bead — NOT the raw email/handle. This reproduces the
// exact prod shape (GET /v1/operators): the oidc row that dedupOperators picks as
// primary has a blank Display while the slack row in the same group holds the name.
// Mirrors user_home_test.go's TestHandleUserHome_DisplayNameInHeading so both
// surfaces are pinned to the same expectation.
func TestHandlePeople_HeadingResolvesNameNotEmail(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			// Matt: OIDC primary with EMPTY Display (as in prod), plus a slack
			// sibling that carries the real name — same email ties them together.
			{ID: 229, Kind: "oidc", Subject: "matthew.baker@acme.example", Display: "", Email: "matthew.baker@acme.example", Slug: "matthew.baker"},
			{ID: 230, Kind: "slack", Subject: "U0OPERATOR1", Display: "Matt Baker", Email: "matthew.baker@acme.example", Slug: "op-229"},
			// Val: OIDC primary with empty Display, NO sibling name — the real
			// name lives ONLY on the user bead (resolved via the email join below).
			{ID: 300, Kind: "oidc", Subject: "val.reyes@acme.example", Display: "", Email: "val.reyes@acme.example", Slug: "val.reyes"},
		},
	}
	// User beads carry `email` + `display_name` but NO `operator_id` (the real
	// prod shape — the missing join key is the whole bug). Val's name resolves
	// only through the email index.
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-user-val", Type: "user", Kind: "config",
				Fields: map[string]string{
					"email":        "val.reyes@acme.example",
					"display_name": "Val Reyes",
				},
			},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Both humans render their resolved "First Last" as the row heading. The
	// heading anchor closes with `{{.Display}}</a>` (people.html), so asserting
	// `<name></a>` pins the resolved name AS the heading, not merely present
	// somewhere on the card (the email still renders in the people-row-email line).
	for _, want := range []string{
		`class="bead-row-title people-row-name"`,
		"Matt Baker</a>", // from the slack sibling's Display
		"Val Reyes</a>",  // from the user bead, joined by email
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected resolved heading %q\nbody: %s", want, body)
		}
	}
}

// TestHandlePeople_StatusMessageResolvesViaEmailJoin pins the Defect-2 fix
// (ka-0000006552): status_message must surface on /people even when the user
// bead carries `email` but NO `operator_id` — the real prod shape that made the
// former operator_id-only join return an empty map for everyone. The message is
// the same one /u/{slug} already shows.
func TestHandlePeople_StatusMessageResolvesViaEmailJoin(t *testing.T) {
	f := newFake()
	f.listOperatorsResp = &beadsapi.ListOperatorsResponse{
		Operators: []beadsapi.OperatorDirectoryEntry{
			{ID: 229, Kind: "oidc", Subject: "matthew.baker@acme.example", Display: "", Email: "matthew.baker@acme.example", Slug: "matthew.baker"},
			{ID: 230, Kind: "slack", Subject: "U0OPERATOR1", Display: "Matt Baker", Email: "matthew.baker@acme.example", Slug: "op-229"},
		},
	}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-user-matt", Type: "user", Kind: "config",
				Fields: map[string]string{
					// email but NO operator_id — the join key the old code required.
					"email":          "matthew.baker@acme.example",
					"display_name":   "Matt Baker",
					"status_message": "Shipping the people directory.",
				},
			},
		},
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Shipping the people directory.") {
		t.Errorf("status_message did not surface via the email join\nbody: %s", body)
	}
	// The status renders in its dedicated row-status block, not smeared elsewhere.
	if !strings.Contains(body, "people-row-status") {
		t.Errorf("expected people-row-status block to render the joined status message")
	}
}

// TestHandlePeople_RendersDirectoryFilter pins the client-side, directory-scoped
// filter box (ka-0000000978): an operator jumps to a colleague by typing a name /
// email / handle / role, filtering the already-rendered rows without a server
// round-trip. The server's job is to (a) render the (progressively-enhanced,
// initially-hidden) filter input, (b) stamp each people-row with a lowercased
// data-search haystack the client matches against, and (c) expose the original
// header total so the JS can restore it when the query clears. The live
// header-reflects-matches behavior is client-JS and not exercised here; this
// test guards the SERVER contract the JS relies on — and, critically, that the
// default (unfiltered) capture the Users-CAMP audit greps is unchanged: every
// row stays in the DOM and the <h1> count still equals the rendered row count.
func TestHandlePeople_RendersDirectoryFilter(t *testing.T) {
	mux := mkServer(t, peopleFixture())
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// (a) The filter box renders, ships hidden (progressive enhancement — a no-JS
	// client keeps the full directory), and is directory-scoped (its own input,
	// not the global bead search in the nav).
	for _, want := range []string{
		`class="people-filter"`,
		`id="people-filter-input"`,
		`data-people-filter`,
		`data-people-filter-count`,
		`data-people-filter-empty`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("directory filter missing %q\nbody: %s", want, body)
		}
	}
	filterDiv := sliceAfter(body, `<div class="people-filter"`)
	if i := strings.Index(filterDiv, ">"); i >= 0 {
		filterDiv = filterDiv[:i]
	}
	if !strings.Contains(filterDiv, "hidden") {
		t.Errorf("filter box must ship server-rendered `hidden` (revealed by JS) so no-JS clients keep the full list; got: %s", filterDiv)
	}

	// (b) Each people-row carries a lowercased data-search haystack the client
	// matches against — name, email, and the identity handle (oidc subject) are
	// all present so typing any of them narrows to Alice.
	aliceRow := sliceAfter(body, `data-operator-id="42"`)
	if i := strings.Index(aliceRow, "</li>"); i >= 0 {
		aliceRow = aliceRow[:i]
	}
	dsIdx := strings.Index(aliceRow, `data-search="`)
	if dsIdx < 0 {
		t.Fatalf("Alice's row missing data-search haystack\nrow: %s", aliceRow)
	}
	haystack := aliceRow[dsIdx+len(`data-search="`):]
	if i := strings.Index(haystack, `"`); i >= 0 {
		haystack = haystack[:i]
	}
	for _, want := range []string{"alice anderson", "alice@example.com", "alice"} {
		if !strings.Contains(haystack, want) {
			t.Errorf("Alice's data-search haystack missing %q\nhaystack: %q", want, haystack)
		}
	}
	// The haystack is lowercased — the display name is title-case in the DOM but
	// must not leak an uppercase token into the substring index.
	if strings.Contains(haystack, "Alice") {
		t.Errorf("data-search haystack must be lowercased, got: %q", haystack)
	}

	// (c) The header carries the original total so the JS can restore it after a
	// cleared query — AND the default capture is still consistent: the <h1> "(2)"
	// equals the two rendered rows (header_vs_rows_delta==0, the audit invariant).
	if !strings.Contains(body, `data-people-total="2"`) {
		t.Errorf("header must expose data-people-total for the JS to restore\nbody: %s", body)
	}
	if !strings.Contains(body, `data-people-header-count`) {
		t.Errorf("header count span must carry data-people-header-count for the live update")
	}
	if got := strings.Count(body, `class="bead-row people-row"`); got != 2 {
		t.Errorf("expected both rows to stay in the DOM (filter hides via CSS, never removes), got %d", got)
	}
	if !strings.Contains(body, "<h1>Users <span") || !strings.Contains(body, "(2)") {
		t.Errorf("default capture header must render Users (2), matching the rendered row count\nbody: %s", body)
	}
}

// TestPeopleRowSearchText unit-tests the server-built haystack directly: it is
// lowercased and folds display / email / status / slug plus every identity's
// kind (the "role" an operator types), handle, and slug — so a substring test
// against it matches any of those tokens (ka-0000000978).
func TestPeopleRowSearchText(t *testing.T) {
	row := peopleRow{
		Display:       "Casey Jones",
		Email:         "casey@example.com",
		StatusMessage: "Reviewing MRs",
		Slug:          "casey",
		Identities: []peopleIdentity{
			{Kind: "oidc", Handle: "casey@example.com", Slug: "casey", Primary: true},
			{Kind: "slack", Handle: "U123", Slug: "casey-slack"},
		},
	}
	got := row.SearchText()
	for _, want := range []string{
		"casey jones",       // display name
		"casey@example.com", // email
		"reviewing mrs",     // status message (lowercased)
		"slack",             // identity kind — the "role" token
		"u123",              // slack handle (lowercased)
		"casey-slack",       // secondary identity slug
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SearchText missing %q\ngot: %q", want, got)
		}
	}
	if got != strings.ToLower(got) {
		t.Errorf("SearchText must be fully lowercased, got: %q", got)
	}
}

// TestHandlePeople_ActivitySignalsAreAgeBounded pins the fix for the operator
// report that "old issues and threads seem to show up more prominently than
// they should, seemingly remaining current despite irrelevance"
// (ka-0000001991). Both activity strips under a /people card must be bounded
// to peopleActivityWindow:
//
//   - the recently-created chips, via the recent_window_hours argument the
//     viewer passes to the daemon's OperatorStats aggregate, and
//   - the recent-request previews, via CreatedAfter on the thread-message scan.
//
// Asserting on the OUTGOING QUERIES is the point. The age bound is enforced
// daemon-side, so a test that only inspected the rendered HTML would pass just
// as happily against an unbounded query with recent fixture data — it would
// pin nothing. Checking what the viewer asks for is what actually catches a
// regression that drops the window.
func TestHandlePeople_ActivitySignalsAreAgeBounded(t *testing.T) {
	f := peopleFixture()
	mux := mkServer(t, f)
	before := time.Now()
	req := httptest.NewRequest("GET", "/people", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	// (1) The recently-created chips must carry a positive, finite window —
	// 0 would mean "daemon default" (7d), which is wider than this page's
	// 24h scope and would let week-old work read as current.
	wantHours := int(peopleActivityWindow.Hours())
	if got := f.operatorStatsWindowHours; got != wantHours {
		t.Errorf("OperatorStats recent-window = %dh, want %dh (peopleActivityWindow)", got, wantHours)
	}

	// (2) The thread-request scan must carry CreatedAfter ≈ now-window.
	// Bracket it against the request's own wall clock rather than pinning an
	// exact instant, which would be flaky.
	var msgQ *beadsapi.ListBeadsQuery
	f.listMu.Lock()
	for i := range f.listAllQ {
		q := f.listAllQ[i]
		if slices.Contains(q.Types, "message") && slices.Contains(q.Labels, "slack-thread-message") {
			msgQ = &q
			break
		}
	}
	f.listMu.Unlock()
	if msgQ == nil {
		t.Fatal("expected a slack-thread-message scan for the recent-requests strip")
	}
	if msgQ.CreatedAfter.IsZero() {
		t.Fatal("thread-message scan has no CreatedAfter: an unbounded scan lets a months-old ask render as a current request (ka-0000001991)")
	}
	earliest := before.Add(-peopleActivityWindow)
	latest := time.Now().Add(-peopleActivityWindow)
	if msgQ.CreatedAfter.Before(earliest) || msgQ.CreatedAfter.After(latest) {
		t.Errorf("CreatedAfter = %v, want ~now-%v (within [%v, %v])",
			msgQ.CreatedAfter, peopleActivityWindow, earliest, latest)
	}
}
