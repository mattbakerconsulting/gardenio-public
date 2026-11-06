package main

import (
	"context"
	"html/template"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestFeedSeedCacheGetSetTTL exercises the short-TTL seed cache directly: a
// fresh set is a hit within the TTL, a miss once the TTL elapses, and a miss
// when the requested limit differs from the cached one (ka-0000002634).
func TestFeedSeedCacheGetSetTTL(t *testing.T) {
	c := &feedSeedCache{}
	t0 := time.Date(2026, 6, 17, 12, 0, 0, 0, time.UTC)

	snap := feedSeedSnapshot{
		cards: []feedBackfillCard{{HTML: template.HTML("<div>card</div>")}},
		users: []feedUser{{Key: "matt@x", Label: "matt@x", LastActive: t0}},
	}

	// Cold cache: miss.
	if _, ok := c.get(t0, 60, feedSeedCacheTTL); ok {
		t.Fatal("cold cache returned a hit")
	}

	c.set(snap, 60, t0)

	// Within TTL, same limit: hit, equal contents.
	got, ok := c.get(t0.Add(feedSeedCacheTTL-time.Millisecond), 60, feedSeedCacheTTL)
	if !ok {
		t.Fatal("expected a hit within TTL")
	}
	if len(got.cards) != 1 || got.cards[0].HTML != snap.cards[0].HTML {
		t.Errorf("cached cards mismatch: got %+v", got.cards)
	}
	if len(got.users) != 1 || got.users[0].Key != "matt@x" {
		t.Errorf("cached users mismatch: got %+v", got.users)
	}

	// Past TTL: miss.
	if _, ok := c.get(t0.Add(feedSeedCacheTTL+time.Millisecond), 60, feedSeedCacheTTL); ok {
		t.Error("expected a miss past TTL")
	}

	// Within TTL but different limit: miss (wrong-sized snapshot must not leak).
	if _, ok := c.get(t0, 30, feedSeedCacheTTL); ok {
		t.Error("expected a miss for a different limit")
	}
}

// TestFeedSeedCardsAndUsersCachesHit verifies the wrapper folds a burst of
// /feed seed builds into a single upstream scan: the first call hits the daemon
// once, the second within the TTL is served from cache with no extra calls, and
// the returned cards match (ka-0000002634).
func TestFeedSeedCardsAndUsersCachesHit(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventData(t, "gd-a", "task", "a", "open")},
			{ID: 2, Topic: "beads.bead.updated", CreatedAt: t0.Add(time.Minute), Payload: feedEventData(t, "gd-b", "task", "b", "in_progress")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	cards1, _ := srv.feedSeedCardsAndUsers(context.Background(), feedBackfillLimit)
	if fake.listGlobalEventsCalls == 0 {
		t.Fatal("first seed build made no daemon calls")
	}
	calls1 := fake.listGlobalEventsCalls
	if len(cards1) == 0 {
		t.Fatal("first seed build returned no cards")
	}

	cards2, _ := srv.feedSeedCardsAndUsers(context.Background(), feedBackfillLimit)
	if fake.listGlobalEventsCalls != calls1 {
		t.Errorf("second seed call hit the daemon: calls went %d -> %d (expected cached)", calls1, fake.listGlobalEventsCalls)
	}
	if len(cards1) != len(cards2) {
		t.Errorf("cached card count drifted: %d -> %d", len(cards1), len(cards2))
	}
}

// TestFeedSeedCardsAndUsersEmptyNotCached confirms an empty build is NOT cached
// (so a daemon hiccup doesn't pin a blank feed for the whole TTL): the very next
// call re-attempts the upstream scan (ka-0000002634).
func TestFeedSeedCardsAndUsersEmptyNotCached(t *testing.T) {
	fake := newFake()
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{Events: nil}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	if cards, _ := srv.feedSeedCardsAndUsers(context.Background(), feedBackfillLimit); len(cards) != 0 {
		t.Fatalf("expected empty seed, got %d cards", len(cards))
	}
	calls1 := fake.listGlobalEventsCalls

	srv.feedSeedCardsAndUsers(context.Background(), feedBackfillLimit)
	if fake.listGlobalEventsCalls <= calls1 {
		t.Errorf("empty result was cached: second call did not re-fetch (calls stayed at %d)", calls1)
	}
}
