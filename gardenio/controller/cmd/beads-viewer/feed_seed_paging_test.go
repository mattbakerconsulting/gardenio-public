package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestCollectFeedSeed_BoundsRawEventScanUnderCommentFlood reproduces the
// pathological case measured live in gd-4q76kkHR3V: a small set of beads
// generating heavy comment/squawk traffic keeps the distinct-bead count well
// under `limit` on every page, so the distinct-bead early break in
// collectFeedRenderedCardsAndUsers never fires and the loop would otherwise
// run the full feedSeedMaxPages pages (20 * maxEventsPerSeedPage = 20,000
// events, ~107MB, ~69s render — gd-QnY7HUw55v). feedSeedMaxRawEventsScanned
// bounds this independently of distinct-bead count; assert the loop actually
// stops there instead of paging to the end.
func TestCollectFeedSeed_BoundsRawEventScanUnderCommentFlood(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")

	// Only 3 distinct beads ever appear, cycled across every page, so
	// `distinct` never approaches feedBackfillLimit (200) no matter how many
	// pages run. Every page reports HasMore so nothing but the raw-event cap
	// stops the loop.
	fake.listGlobalEventsFn = func(req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error) {
		events := make([]beadsapi.Event, req.Limit)
		for i := range events {
			bead := fmt.Sprintf("gd-flood-%d", i%3)
			events[i] = beadsapi.Event{
				ID:        req.Cursor*int64(req.Limit) + int64(i),
				Topic:     "beads.bead.updated",
				CreatedAt: t0.Add(time.Duration(req.Cursor*int64(req.Limit)+int64(i)) * time.Second),
				Payload:   feedEventData(t, bead, "task", "flood", "in_progress"),
			}
		}
		return &beadsapi.ListGlobalEventsResponse{
			Events:     events,
			HasMore:    true,
			NextCursor: fmt.Sprintf("%d", req.Cursor+1),
		}, nil
	}

	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	cards, _ := srv.feedSeedCardsAndUsers(context.Background(), feedBackfillLimit)

	wantPages := feedSeedMaxRawEventsScanned / maxEventsPerSeedPage
	if fake.listGlobalEventsCalls != wantPages {
		t.Errorf("listGlobalEventsCalls = %d, want %d (feedSeedMaxRawEventsScanned=%d / maxEventsPerSeedPage=%d); the raw-event cap did not bound the loop",
			fake.listGlobalEventsCalls, wantPages, feedSeedMaxRawEventsScanned, maxEventsPerSeedPage)
	}
	if fake.listGlobalEventsCalls >= feedSeedMaxPages {
		t.Errorf("loop ran to feedSeedMaxPages (%d) — the comment-flood worst case (gd-4q76kkHR3V) is unbounded again", feedSeedMaxPages)
	}
	if len(cards) == 0 {
		t.Fatal("expected at least some cards from the scanned pages")
	}
}
