package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// feed_dedup_test.go — Phase A3 adjacent-duplicate suppression (ka-0000000291,
// epic ka-0000004907). Belt-and-suspenders on top of the Phase A2 server-side
// suppression: when one agent floods the feed with identical consecutive
// cards (same kind + same bead within a short window), the /feed seed/replay
// path collapses the run into a single "×N" card. Non-adjacent repeats,
// different kinds, different beads, and out-of-window repeats are NOT
// collapsed.

// TestFeedItemsAdjacentDuplicate covers the pure adjacency predicate: only a
// same-kind + same-bead-id event within feedDedupWindow of the previous card
// is an adjacent duplicate.
func TestFeedItemsAdjacentDuplicate(t *testing.T) {
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	prev := feedCardItem{kind: feedKindUpdated, bead: &feedEventBead{ID: "gd-a"}, ts: t0, count: 1}

	cases := []struct {
		name string
		kind feedKind
		bead *feedEventBead
		ts   time.Time
		want bool
	}{
		{"same kind+bead inside window", feedKindUpdated, &feedEventBead{ID: "gd-a"}, t0.Add(30 * time.Second), true},
		{"same kind+bead at window edge", feedKindUpdated, &feedEventBead{ID: "gd-a"}, t0.Add(feedDedupWindow), true},
		{"same kind+bead beyond window", feedKindUpdated, &feedEventBead{ID: "gd-a"}, t0.Add(feedDedupWindow + time.Second), false},
		{"different bead", feedKindUpdated, &feedEventBead{ID: "gd-b"}, t0.Add(time.Second), false},
		{"different kind", feedKindClaimed, &feedEventBead{ID: "gd-a"}, t0.Add(time.Second), false},
		{"nil bead", feedKindUpdated, nil, t0.Add(time.Second), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := feedItemsAdjacentDuplicate(prev, c.kind, c.bead, c.ts); got != c.want {
				t.Errorf("feedItemsAdjacentDuplicate = %v, want %v", got, c.want)
			}
		})
	}

	// A nil previous bead (empty accumulator sentinel) never matches.
	if feedItemsAdjacentDuplicate(feedCardItem{}, feedKindUpdated, &feedEventBead{ID: "gd-a"}, t0) {
		t.Errorf("nil prev bead should not be an adjacent duplicate")
	}
}

// TestAppendCollapsingAdjacent confirms the accumulator folds adjacent
// duplicates into one item (bumping count, advancing the representative
// timestamp) while keeping distinct/non-adjacent/different-kind events as
// separate items.
func TestAppendCollapsingAdjacent(t *testing.T) {
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	var items []feedCardItem

	// Three identical adjacent gate updates on gd-a within the window collapse.
	items = appendCollapsingAdjacent(items, feedKindGate, &feedEventBead{ID: "gd-a"}, "gardenio", "", t0)
	items = appendCollapsingAdjacent(items, feedKindGate, &feedEventBead{ID: "gd-a"}, "gardenio", "", t0.Add(time.Minute))
	items = appendCollapsingAdjacent(items, feedKindGate, &feedEventBead{ID: "gd-a"}, "gardenio", "", t0.Add(2*time.Minute))

	if len(items) != 1 {
		t.Fatalf("expected 1 collapsed item, got %d", len(items))
	}
	if items[0].count != 3 {
		t.Errorf("collapsed count = %d, want 3", items[0].count)
	}
	// Representative timestamp advances to the newest event in the run.
	if !items[0].ts.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("representative ts = %v, want newest (05:02)", items[0].ts)
	}

	// A different bead breaks the run -> a new item.
	items = appendCollapsingAdjacent(items, feedKindGate, &feedEventBead{ID: "gd-b"}, "gardenio", "", t0.Add(3*time.Minute))
	// gd-a again, but now NON-adjacent (gd-b sits between) -> a new item, not folded.
	items = appendCollapsingAdjacent(items, feedKindGate, &feedEventBead{ID: "gd-a"}, "gardenio", "", t0.Add(4*time.Minute))
	// gd-a same bead but DIFFERENT kind -> a new item.
	items = appendCollapsingAdjacent(items, feedKindClaimed, &feedEventBead{ID: "gd-a"}, "gardenio", "", t0.Add(5*time.Minute))
	// gd-a same kind as the previous claim, but OUTSIDE the window -> a new item.
	items = appendCollapsingAdjacent(items, feedKindClaimed, &feedEventBead{ID: "gd-a"}, "gardenio", "", t0.Add(5*time.Minute+feedDedupWindow+time.Second))

	if len(items) != 5 {
		t.Fatalf("expected 5 items (1 collapsed run + 4 distinct), got %d", len(items))
	}
	for i, it := range items[1:] {
		if it.count != 1 {
			t.Errorf("item %d count = %d, want 1 (not collapsed)", i+1, it.count)
		}
	}
}

// TestCollectFeedCards_CollapsesInterleavedFlood drives the durable-seed scan
// end-to-end with the failure mode Matt hit on live /feed (2026-06-16): one
// bead's flood is INTERLEAVED with another bead's events, so adjacency alone
// never fires and the flood used to render as N separate cards. The global
// collapse pass folds every same-kind+bead repeat in the window — adjacent or
// not — into ONE "×N" card at the run's newest position, leaving the distinct
// bead its own card.
func TestCollectFeedCards_CollapsesInterleavedFlood(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		// Durable log is oldest-first. gd-flood's four "updated" events are
		// scattered between gd-other's events — never adjacent.
		Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.updated", CreatedAt: t0, Payload: feedEventData(t, "gd-flood", "task", "flood", "open")},
			{ID: 2, Topic: "beads.bead.created", CreatedAt: t0.Add(1 * time.Minute), Payload: feedEventData(t, "gd-other", "task", "other", "open")},
			{ID: 3, Topic: "beads.bead.updated", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventData(t, "gd-flood", "task", "flood", "open")},
			{ID: 4, Topic: "beads.bead.updated", CreatedAt: t0.Add(3 * time.Minute), Payload: feedEventData(t, "gd-flood", "task", "flood", "open")},
			{ID: 5, Topic: "beads.bead.updated", CreatedAt: t0.Add(4 * time.Minute), Payload: feedEventData(t, "gd-flood", "task", "flood", "open")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	cards := srv.collectFeedCards(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour), 30)

	// Expect 2 cards: the gd-other card and the single collapsed gd-flood ×4.
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (1 distinct + 1 collapsed interleaved flood), got %d:\n%s", len(cards), strings.Join(cards, "\n"))
	}

	// Cards are returned oldest-first. gd-other (t0+1) sorts before the flood's
	// representative, which takes the run's NEWEST position (t0+4).
	if !strings.Contains(cards[0], `data-bead-id="gd-other"`) {
		t.Errorf("first card should be gd-other, got:\n%s", cards[0])
	}
	if strings.Contains(cards[0], `class="feed-card-count"`) {
		t.Errorf("gd-other card should not carry a count badge, got:\n%s", cards[0])
	}

	// The flood collapses to one ×4 card carrying the SUM of all four events.
	flood := cards[1]
	if !strings.Contains(flood, `data-bead-id="gd-flood"`) {
		t.Errorf("second card should be the collapsed gd-flood, got:\n%s", flood)
	}
	if !strings.Contains(flood, `class="feed-card-count"`) || !strings.Contains(flood, `×4`) {
		t.Errorf("collapsed gd-flood card should carry the ×4 badge, got:\n%s", flood)
	}

	// Exactly one count badge across the whole render.
	if n := strings.Count(strings.Join(cards, ""), `class="feed-card-count"`); n != 1 {
		t.Errorf("expected exactly 1 collapse badge across all cards, got %d", n)
	}
}

// TestCollapseGlobalDuplicates covers the pure global-collapse pass: same
// kind+bead folds regardless of adjacency (summing counts, taking the newest
// position), while different kinds / different beads / nil-bead items stay
// distinct.
func TestCollapseGlobalDuplicates(t *testing.T) {
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	mk := func(kind feedKind, id string, count int, ts time.Time) feedCardItem {
		return feedCardItem{kind: kind, bead: &feedEventBead{ID: id}, ts: ts, count: count}
	}

	// flood (gd-a gate) appears at positions 0 and 2, interleaved with gd-b.
	in := []feedCardItem{
		mk(feedKindGate, "gd-a", 3, t0),               // adjacent-collapsed run of 3
		mk(feedKindClaimed, "gd-b", 1, t0.Add(time.Minute)),
		mk(feedKindGate, "gd-a", 1, t0.Add(2*time.Minute)), // non-adjacent gd-a repeat
		mk(feedKindGate, "gd-c", 1, t0.Add(3*time.Minute)),
	}
	out := collapseGlobalDuplicates(in)

	if len(out) != 3 {
		t.Fatalf("expected 3 items (gd-a folded, gd-b, gd-c), got %d", len(out))
	}
	// gd-b keeps its original position (idx 0), gd-a's representative moves to
	// the run's newest position (after gd-b), gd-c stays last.
	if out[0].bead.ID != "gd-b" {
		t.Errorf("out[0] = %s, want gd-b", out[0].bead.ID)
	}
	if out[1].bead.ID != "gd-a" {
		t.Errorf("out[1] = %s, want gd-a (collapsed)", out[1].bead.ID)
	}
	if out[1].count != 4 {
		t.Errorf("gd-a collapsed count = %d, want 4 (3+1)", out[1].count)
	}
	if !out[1].ts.Equal(t0.Add(2 * time.Minute)) {
		t.Errorf("gd-a representative ts = %v, want newest occurrence (05:02)", out[1].ts)
	}
	if out[2].bead.ID != "gd-c" {
		t.Errorf("out[2] = %s, want gd-c", out[2].bead.ID)
	}

	// Same bead, DIFFERENT kind must not fold together.
	mixed := []feedCardItem{
		mk(feedKindGate, "gd-x", 1, t0),
		mk(feedKindClaimed, "gd-x", 1, t0.Add(time.Minute)),
	}
	if got := collapseGlobalDuplicates(mixed); len(got) != 2 {
		t.Errorf("different kinds on same bead should stay distinct, got %d items", len(got))
	}

	// A nil-bead item passes through untouched and never merges.
	nilItems := []feedCardItem{
		{kind: feedKindUpdated, bead: nil, ts: t0, count: 1},
		{kind: feedKindUpdated, bead: nil, ts: t0.Add(time.Minute), count: 1},
	}
	if got := collapseGlobalDuplicates(nilItems); len(got) != 2 {
		t.Errorf("nil-bead items should pass through untouched, got %d items", len(got))
	}

	// Fewer than 2 items short-circuits.
	if got := collapseGlobalDuplicates(in[:1]); len(got) != 1 {
		t.Errorf("single-item input should pass through, got %d", len(got))
	}
}

// TestBuildFeedCard_NoBadgeForSingle confirms a count<=1 renders no badge, so
// the default (non-collapsed) render is byte-identical to the prior behaviour.
func TestBuildFeedCard_NoBadgeForSingle(t *testing.T) {
	b := &feedEventBead{ID: "gd-x", Type: "task", Title: "x", Status: "open"}
	ts := fixedFeedNow(t)
	plain := buildFeedCard("/b", feedKindUpdated, b, "gardenio", "", "1m", ts, nil, 1)
	if strings.Contains(plain, `feed-card-count`) {
		t.Errorf("count=1 card should not carry a badge, got:\n%s", plain)
	}
	// renderFeedCardFull (the count=1 wrapper) must match buildFeedCard count=1.
	if got := renderFeedCardFull("/b", feedKindUpdated, b, "gardenio", "", "1m", ts, nil); got != plain {
		t.Errorf("renderFeedCardFull should equal buildFeedCard count=1\n got=%s\nwant=%s", got, plain)
	}
}
