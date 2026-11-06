package main

import (
	"testing"
	"time"
)

// TestAssembleActivityStream_SortDedupRails covers the pure merge: strict
// newest-first ordering, dedup-by-id (first-seen wins), zero-timestamp drop,
// date-rail insertion, and per-facet counts.
func TestAssembleActivityStream_SortDedupRails(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	mk := func(id, facet string, ts time.Time) activityEvent {
		return activityEvent{ID: id, Facet: facet, Timestamp: ts, Ago: fmtTimeAgo(ts, now)}
	}
	events := []activityEvent{
		mk("gd-a", facetDecisions, now.Add(-1*time.Hour)),     // Today
		mk("gd-b", facetClosed, now.Add(-3*24*time.Hour)),     // This week
		mk("gd-a", facetComments, now.Add(-1*time.Hour)),      // dup id — dropped
		mk("gd-c", facetMRsMerged, now.Add(-10*24*time.Hour)), // Earlier
		mk("gd-z", facetMessages, time.Time{}),                // zero ts — dropped
		mk("gd-d", facetMessages, now.Add(-2*time.Hour)),      // Today
	}
	got := assembleActivityStream(events, now)

	if got.Total != 4 {
		t.Fatalf("Total = %d, want 4 (dup + zero-ts dropped)", got.Total)
	}

	// WeekCount = rows in the trailing week (Today + This week buckets), which the
	// narrow spine seeds its initial reveal at (feature ka-0000003056). gd-a/gd-d
	// are Today, gd-b is This week, gd-c is Earlier → 3.
	if got.WeekCount != 3 {
		t.Errorf("WeekCount = %d, want 3 (Today+This-week rows, Earlier excluded)", got.WeekCount)
	}

	// Extract the event order (skipping rails) and assert reverse-chron.
	var order []string
	var rails []string
	for _, it := range got.Items {
		if it.IsRail {
			rails = append(rails, it.RailLabel)
			continue
		}
		order = append(order, it.Event.ID)
	}
	wantOrder := []string{"gd-a", "gd-d", "gd-b", "gd-c"}
	if len(order) != len(wantOrder) {
		t.Fatalf("event order len = %d, want %d (%v)", len(order), len(wantOrder), order)
	}
	for i := range wantOrder {
		if order[i] != wantOrder[i] {
			t.Errorf("event order[%d] = %s, want %s (full: %v)", i, order[i], wantOrder[i], order)
		}
	}

	// Rails appear once each, in stream order.
	wantRails := []string{railToday, railWeek, railEarlier}
	if len(rails) != len(wantRails) {
		t.Fatalf("rails = %v, want %v", rails, wantRails)
	}
	for i := range wantRails {
		if rails[i] != wantRails[i] {
			t.Errorf("rail[%d] = %s, want %s", i, rails[i], wantRails[i])
		}
	}

	// A rail always immediately precedes its first event (no orphan rails).
	if !got.Items[0].IsRail || got.Items[0].RailLabel != railToday {
		t.Errorf("stream must open with the Today rail, got %+v", got.Items[0])
	}

	// Facet chips: only non-zero facets, in activityFacetOrder. The deduped
	// gd-a kept its FIRST (decisions) facet, so comments has zero count.
	facetCounts := map[string]int{}
	for _, c := range got.Facets {
		facetCounts[c.Slug] = c.Count
	}
	if facetCounts[facetComments] != 0 {
		t.Errorf("comments facet should be absent (gd-a deduped to decisions), got %d", facetCounts[facetComments])
	}
	for _, want := range []string{facetDecisions, facetMessages, facetMRsMerged, facetClosed} {
		if facetCounts[want] != 1 {
			t.Errorf("facet %s count = %d, want 1", want, facetCounts[want])
		}
	}
}

// TestAssembleActivityStream_Monotonic asserts the rendered relative ages are
// non-decreasing down the item list — the invariant the time-series measure
// decodes into timeline_monotonic.
func TestAssembleActivityStream_Monotonic(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	var events []activityEvent
	for i, h := range []int{2, 1, 50, 5, 400, 30} { // deliberately unsorted
		ts := now.Add(-time.Duration(h) * time.Hour)
		events = append(events, activityEvent{
			ID:        "gd-" + itoa64(int64(i)),
			Facet:     facetClosed,
			Timestamp: ts,
			Ago:       fmtTimeAgo(ts, now),
		})
	}
	got := assembleActivityStream(events, now)
	prev := -1
	for _, it := range got.Items {
		if it.IsRail {
			continue
		}
		age := int(now.Sub(it.Event.Timestamp).Seconds())
		if age < prev {
			t.Errorf("stream not monotonic non-decreasing: age %d after %d", age, prev)
		}
		prev = age
	}
}

// TestAssembleActivityStream_Empty returns an empty view (no rails, no crash).
func TestAssembleActivityStream_Empty(t *testing.T) {
	got := assembleActivityStream(nil, time.Now())
	if got.Total != 0 || len(got.Items) != 0 || len(got.Facets) != 0 || got.WeekCount != 0 {
		t.Errorf("expected empty view, got %+v", got)
	}
}

// TestAssembleActivityStream_WeekCount pins the trailing-week row count that
// seeds the narrow spine's initial reveal (feature ka-0000003056): it counts
// exactly the Today + This-week rows and excludes the older Earlier tail, so a
// deep-history record still bounds its initial paint while a busy week paints in
// full. Rails never count toward it.
func TestAssembleActivityStream_WeekCount(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	mk := func(id string, ts time.Time) activityEvent {
		return activityEvent{ID: id, Facet: facetClosed, Timestamp: ts, Ago: fmtTimeAgo(ts, now)}
	}

	// All rows inside the trailing week → WeekCount == Total (no click needed to
	// read the whole week).
	weekOnly := []activityEvent{
		mk("gd-a", now.Add(-1*time.Hour)),    // Today
		mk("gd-b", now.Add(-25*time.Hour)),   // This week
		mk("gd-c", now.Add(-6*24*time.Hour)), // This week (just inside 7d)
	}
	if got := assembleActivityStream(weekOnly, now); got.WeekCount != 3 || got.WeekCount != got.Total {
		t.Errorf("all-week: WeekCount=%d Total=%d, want both 3", got.WeekCount, got.Total)
	}

	// All rows older than a week → WeekCount 0 (only the Earlier tail exists, so
	// the initial paint falls back to the template's COLLAPSE_MIN floor).
	earlierOnly := []activityEvent{
		mk("gd-x", now.Add(-8*24*time.Hour)),
		mk("gd-y", now.Add(-40*24*time.Hour)),
	}
	if got := assembleActivityStream(earlierOnly, now); got.WeekCount != 0 || got.Total != 2 {
		t.Errorf("all-earlier: WeekCount=%d Total=%d, want 0 and 2", got.WeekCount, got.Total)
	}

	// Mixed: only the trailing-week prefix counts, the Earlier tail is excluded.
	mixed := []activityEvent{
		mk("gd-1", now.Add(-2*time.Hour)),     // Today
		mk("gd-2", now.Add(-4*24*time.Hour)),  // This week
		mk("gd-3", now.Add(-10*24*time.Hour)), // Earlier
		mk("gd-4", now.Add(-30*24*time.Hour)), // Earlier
	}
	if got := assembleActivityStream(mixed, now); got.WeekCount != 2 || got.Total != 4 {
		t.Errorf("mixed: WeekCount=%d Total=%d, want 2 and 4", got.WeekCount, got.Total)
	}
}

// TestRailBucket covers the calendar-anchored Today / This week / Earlier
// classification, including the future-timestamp (clock-skew) guard.
func TestRailBucket(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		ts   time.Time
		want string
	}{
		{"same-utc-day-earlier", now.Add(-5 * time.Hour), railToday},
		{"same-utc-day-midnight", time.Date(2026, 7, 2, 0, 1, 0, 0, time.UTC), railToday},
		{"future-skew", now.Add(2 * time.Hour), railToday},
		{"yesterday", now.Add(-24 * time.Hour), railWeek},
		{"six-days", now.Add(-6 * 24 * time.Hour), railWeek},
		{"eight-days", now.Add(-8 * 24 * time.Hour), railEarlier},
		{"month-ago", now.Add(-30 * 24 * time.Hour), railEarlier},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := railBucket(tc.ts, now); got != tc.want {
				t.Errorf("railBucket(%s) = %s, want %s", tc.name, got, tc.want)
			}
		})
	}
}

// TestMessagesToEvents projects the inbox chat log into stream events, parsing
// the RFC stamp back for the merge sort and dropping undatable lines.
func TestMessagesToEvents(t *testing.T) {
	inbox := userInboxView{
		HasMessages: true,
		Chat: mapChatView{
			Messages: []chatMessage{
				{ID: "gd-m1", Author: "agent-x", Kind: "squawk", Ago: "2h",
					TimestampRFC: "2026-07-02T10:00:00Z", BodyHTML: "hello"},
				{ID: "gd-m2", Author: "agent-y", Kind: "mail", Ago: "",
					TimestampRFC: "", BodyHTML: "no-ts drop"}, // undatable → dropped
			},
		},
	}
	got := messagesToEvents(inbox, "/beads")
	if len(got) != 1 {
		t.Fatalf("expected 1 datable message event, got %d", len(got))
	}
	ev := got[0]
	if ev.ID != "gd-m1" || !ev.IsMessage || ev.Facet != facetMessages {
		t.Errorf("unexpected message event: %+v", ev)
	}
	if ev.Href != "/beads/gd-m1" {
		t.Errorf("Href = %q, want /beads/gd-m1", ev.Href)
	}
	if ev.Timestamp.IsZero() {
		t.Errorf("expected parsed timestamp, got zero")
	}
}

// TestMessagesToEvents_NoMessages returns nil when the inbox has none.
func TestMessagesToEvents_NoMessages(t *testing.T) {
	if got := messagesToEvents(userInboxView{HasMessages: false}, "/beads"); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

// TestBeadRowToEvent_CreatedTime covers the created-time annotation
// (ka-0000007477): the stream still sorts/ages on UpdatedAt, but a row whose
// creation is meaningfully older than its last activity ALSO surfaces the
// created age + absolute stamp, while an untouched-since-creation row (created
// ≈ updated) does not render a redundant "created = updated" line.
func TestBeadRowToEvent_CreatedTime(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	created := now.Add(-72 * time.Hour) // 3 days ago
	updated := now.Add(-1 * time.Hour)  // bumped/closed 1 hour ago

	t.Run("distinct created surfaces both", func(t *testing.T) {
		row := userHomeBeadRow{ID: "gd-1", CreatedAt: created, UpdatedAt: updated}
		ev := beadRowToEvent(row, facetClosed, "closed", now)
		// Age + sort still key off updated.
		if !ev.Timestamp.Equal(updated) {
			t.Errorf("Timestamp = %v, want updated %v", ev.Timestamp, updated)
		}
		if ev.Ago != "1h" {
			t.Errorf("Ago = %q, want 1h (off updated)", ev.Ago)
		}
		// Created annotation present.
		if ev.CreatedAgo != "3d" {
			t.Errorf("CreatedAgo = %q, want 3d", ev.CreatedAgo)
		}
		if ev.CreatedRFC != created.UTC().Format(time.RFC3339) {
			t.Errorf("CreatedRFC = %q, want %q", ev.CreatedRFC, created.UTC().Format(time.RFC3339))
		}
	})

	t.Run("untouched-since-creation hides redundant created line", func(t *testing.T) {
		row := userHomeBeadRow{ID: "gd-2", CreatedAt: created, UpdatedAt: created.Add(5 * time.Second)}
		ev := beadRowToEvent(row, facetClosed, "closed", now)
		if ev.CreatedAgo != "" || ev.CreatedRFC != "" {
			t.Errorf("expected no created annotation for untouched row, got Ago=%q RFC=%q", ev.CreatedAgo, ev.CreatedRFC)
		}
	})

	t.Run("zero created has no annotation", func(t *testing.T) {
		row := userHomeBeadRow{ID: "gd-3", UpdatedAt: updated}
		ev := beadRowToEvent(row, facetClosed, "closed", now)
		if ev.CreatedAgo != "" || ev.CreatedRFC != "" {
			t.Errorf("expected no created annotation when CreatedAt is zero, got Ago=%q RFC=%q", ev.CreatedAgo, ev.CreatedRFC)
		}
	})
}

// TestCreatedDistinctFromUpdated covers the one-minute distinctness threshold
// and the created-only (zero updated) short-circuit.
func TestCreatedDistinctFromUpdated(t *testing.T) {
	base := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name             string
		created, updated time.Time
		want             bool
	}{
		{"equal", base, base, false},
		{"sub-minute delta", base, base.Add(30 * time.Second), false},
		{"exactly one minute", base, base.Add(time.Minute), true},
		{"hours apart", base, base.Add(3 * time.Hour), true},
		{"updated before created (skew)", base, base.Add(-2 * time.Hour), true},
		{"zero updated (created only)", base, time.Time{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := createdDistinctFromUpdated(tc.created, tc.updated); got != tc.want {
				t.Errorf("createdDistinctFromUpdated = %v, want %v", got, tc.want)
			}
		})
	}
}
