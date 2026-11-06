package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// feedEventData builds the SSE wrapper payload kbeads publishes on a bead
// lifecycle event ({"bead":{...},"closed_by":...}).
func feedEventData(t *testing.T, id, typ, title, status string) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":     id,
			"type":   typ,
			"title":  title,
			"status": status,
			"labels": []string{"project:gardenio"},
		},
	})
	if err != nil {
		t.Fatalf("marshal feed event: %v", err)
	}
	return data
}

// TestBeadFeedHub_FanOutAndRing verifies one upstream stream is fanned out to
// multiple subscribers and that a late subscriber is warm-painted from the ring.
func TestBeadFeedHub_FanOutAndRing(t *testing.T) {
	upstream := make(chan beadsapi.SSEEvent, 8)
	fake := newFake()
	fake.eventStreamCh = upstream

	hub := NewBeadFeedHub(fake, testLogger())
	if hub == nil {
		t.Fatal("NewBeadFeedHub returned nil for a non-nil daemon")
	}
	hub.Start()
	defer hub.Close()

	// Subscriber A connects before any events.
	chA, cancelA := hub.Subscribe(context.Background())
	defer cancelA()

	// Publish two events upstream.
	upstream <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: feedEventData(t, "gd-1", "task", "first", "open")}
	upstream <- beadsapi.SSEEvent{Event: "beads.bead.closed", Data: feedEventData(t, "gd-2", "task", "second", "closed")}

	gotA1 := recvFeed(t, chA)
	gotA2 := recvFeed(t, chA)
	if gotA1.Event != "beads.bead.created" || gotA2.Event != "beads.bead.closed" {
		t.Fatalf("subscriber A got unexpected events: %q, %q", gotA1.Event, gotA2.Event)
	}

	// A late subscriber B should be warm-painted from the ring with both events.
	chB, cancelB := hub.Subscribe(context.Background())
	defer cancelB()
	gotB1 := recvFeed(t, chB)
	gotB2 := recvFeed(t, chB)
	if gotB1.Event != "beads.bead.created" || gotB2.Event != "beads.bead.closed" {
		t.Fatalf("subscriber B not warm-painted from ring: %q, %q", gotB1.Event, gotB2.Event)
	}

	// One upstream connection regardless of subscriber count.
	if fake.eventStreamCalls != 1 {
		t.Errorf("expected exactly 1 upstream EventStream call, got %d", fake.eventStreamCalls)
	}
}

// TestBeadFeedHub_NilSafe confirms a nil hub yields a closed channel so the SSE
// handler degrades cleanly.
func TestBeadFeedHub_NilSafe(t *testing.T) {
	var hub *BeadFeedHub
	ch, cancel := hub.Subscribe(context.Background())
	cancel()
	if _, ok := <-ch; ok {
		t.Error("nil hub Subscribe should return a closed channel")
	}
	if hub.Recent() != nil {
		t.Error("nil hub Recent should be nil")
	}
	hub.Start() // no panic
	hub.Close() // no panic
}

// TestBeadFeedHub_SubscribeBackgroundCtx_NoWatcherLeak is the regression guard
// for bug ka-0000005687: Subscribe with a non-cancellable context
// (context.Background(), whose Done() is nil) must NOT park a watcher
// goroutine on a nil channel forever. With the old unconditional
// `go func() { <-ctx.Done(); ... }()` every such subscriber leaked a goroutine
// for the life of the process — the test suite accumulated them until the
// package timeout's goroutine dump showed "chan receive (nil chan)" at
// Subscribe. Assert the goroutine count returns to baseline after
// subscribe/unsubscribe cycles.
func TestBeadFeedHub_SubscribeBackgroundCtx_NoWatcherLeak(t *testing.T) {
	fake := newFake()
	hub := NewBeadFeedHub(fake, testLogger())
	hub.Start()
	defer hub.Close()

	before := runtime.NumGoroutine()
	const n = 50
	for range n {
		_, cancel := hub.Subscribe(context.Background())
		cancel()
	}
	// Poll: any leaked watcher would be parked permanently, so the count
	// stabilizing back near the baseline proves none was spawned.
	deadline := time.After(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("goroutines leaked by Subscribe(Background): before=%d after=%d", before, runtime.NumGoroutine())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestBeadFeedHub_UnsubscribeReleasesWatcher confirms the cancellable-context
// path also unwinds its watcher when the subscriber cancels explicitly —
// the watcher must not stay parked until the (long-lived) context fires.
func TestBeadFeedHub_UnsubscribeReleasesWatcher(t *testing.T) {
	fake := newFake()
	hub := NewBeadFeedHub(fake, testLogger())
	hub.Start()
	defer hub.Close()

	ctx, ctxCancel := context.WithCancel(context.Background())
	defer ctxCancel() // never fired during the subscribe/unsubscribe cycle below

	before := runtime.NumGoroutine()
	const n = 50
	for range n {
		_, unsub := hub.Subscribe(ctx)
		unsub()
	}
	deadline := time.After(2 * time.Second)
	for {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("watchers leaked after unsubscribe: before=%d after=%d", before, runtime.NumGoroutine())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestBeadFeedHub_CtxCancelUnsubscribes confirms the watcher still does its
// real job after the leak fix: cancelling the subscriber's context closes the
// event channel (auto-unsubscribe for dropped SSE connections).
func TestBeadFeedHub_CtxCancelUnsubscribes(t *testing.T) {
	fake := newFake()
	hub := NewBeadFeedHub(fake, testLogger())
	hub.Start()
	defer hub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := hub.Subscribe(ctx)
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel after context cancel, got an event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber channel not closed after context cancel")
	}
}

// TestBeadFeedHub_RingBounded confirms the ring never exceeds its capacity even
// when far more events than the ring size flow through the upstream.
func TestBeadFeedHub_RingBounded(t *testing.T) {
	n := beadFeedRingCapacity + 50
	upstream := make(chan beadsapi.SSEEvent, n)
	fake := newFake()
	fake.eventStreamCh = upstream
	hub := NewBeadFeedHub(fake, testLogger())
	hub.Start()
	defer hub.Close()

	for range n {
		upstream <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: feedEventData(t, "gd-x", "task", "t", "open")}
	}

	// Wait for the broadcast loop to drain the upstream into the ring, then
	// assert it never exceeded capacity. Poll on the ring length rather than
	// consuming via a subscriber (whose 64-deep buffer drops, not blocks).
	deadline := time.After(2 * time.Second)
	for {
		if len(hub.Recent()) == beadFeedRingCapacity {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("ring did not reach capacity; got %d, want %d", len(hub.Recent()), beadFeedRingCapacity)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := len(hub.Recent()); got > beadFeedRingCapacity {
		t.Errorf("ring grew past capacity: %d > %d", got, beadFeedRingCapacity)
	}
}

func recvFeed(t *testing.T, ch <-chan beadsapi.SSEEvent) beadsapi.SSEEvent {
	t.Helper()
	select {
	case evt, ok := <-ch:
		if !ok {
			t.Fatal("channel closed unexpectedly")
		}
		return evt
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for feed event")
		return beadsapi.SSEEvent{}
	}
}

// TestFeedSeedCards_FromDurableEvents verifies the first-paint seed is built
// from GET /v1/events, newest-first, with the live classifier applied.
func TestFeedSeedCards_FromDurableEvents(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventData(t, "gd-a", "task", "alpha", "open")},
			{ID: 2, Topic: "beads.bead.updated", CreatedAt: t0.Add(time.Minute), Payload: feedEventData(t, "gd-b", "task", "beta", "in_progress")},
			// A pure-noise meta type (schedule) must be filtered out. Phase 3
			// (ka-0000006994) re-admits agent/mail/decision as their own
			// families; schedule/advice/rating/generic/config stay filtered.
			{ID: 3, Topic: "beads.bead.created", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventData(t, "gd-c", "schedule", "noise", "open")},
		},
		HasMore: false,
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	cards := srv.feedSeedCards(context.Background(), 30)
	if len(cards) != 2 {
		t.Fatalf("expected 2 renderable cards (schedule filtered), got %d", len(cards))
	}
	// Newest-first: the in_progress claim (gd-b) leads.
	first := string(cards[0].HTML)
	if !strings.Contains(first, "gd-b") || !strings.Contains(first, "feed-card-claimed") {
		t.Errorf("first card should be the newest (gd-b claim):\n%s", first)
	}
	// data-ts carries the durable event time for reconnect ?since= tracking.
	if !strings.Contains(first, `data-ts="2026-06-15T05:01:00Z"`) {
		t.Errorf("card missing durable data-ts:\n%s", first)
	}
	if fake.listGlobalEventsCalls != 1 {
		t.Errorf("expected 1 ListGlobalEvents call, got %d", fake.listGlobalEventsCalls)
	}
}

// TestFeedSeedCards_PaintsNewestNotOldest is the regression guard for
// ka-0000004135: the first-paint seed must page the durable log NEWEST-first
// and paint the most-recent activity, not the oldest slice of the window. The
// old loop walked forward from the oldest edge and, on a busy fleet, hit its
// page cap before reaching recent events — every card rendered "2d" old. Here
// the window holds more distinct beads than the limit, so a forward walk would
// keep the OLDEST and drop the newest; we assert the opposite and that the
// daemon request carried order=desc.
func TestFeedSeedCards_PaintsNewestNotOldest(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	// Authored oldest-first (ascending id + CreatedAt), as the durable log is.
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventData(t, "gd-a", "task", "alpha", "open")},
			{ID: 2, Topic: "beads.bead.created", CreatedAt: t0.Add(1 * time.Minute), Payload: feedEventData(t, "gd-b", "task", "beta", "open")},
			{ID: 3, Topic: "beads.bead.created", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventData(t, "gd-c", "task", "gamma", "open")},
			{ID: 4, Topic: "beads.bead.created", CreatedAt: t0.Add(3 * time.Minute), Payload: feedEventData(t, "gd-d", "task", "delta", "open")},
			{ID: 5, Topic: "beads.bead.created", CreatedAt: t0.Add(4 * time.Minute), Payload: feedEventData(t, "gd-e", "task", "epsilon", "open")},
		},
		HasMore: false,
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	// limit=2 < 5 distinct beads, so the bounded tail must keep the NEWEST two.
	cards := srv.feedSeedCards(context.Background(), 2)
	if len(cards) != 2 {
		t.Fatalf("expected 2 cards (bounded tail), got %d", len(cards))
	}
	// Painted newest-first: gd-e (newest) leads, gd-d follows.
	if first := string(cards[0].HTML); !strings.Contains(first, "gd-e") {
		t.Errorf("first card should be the newest bead gd-e:\n%s", first)
	}
	if second := string(cards[1].HTML); !strings.Contains(second, "gd-d") {
		t.Errorf("second card should be gd-d:\n%s", second)
	}
	// The oldest beads must NOT be painted — that was the bug.
	all := string(cards[0].HTML) + string(cards[1].HTML)
	for _, stale := range []string{"gd-a", "gd-b", "gd-c"} {
		if strings.Contains(all, stale) {
			t.Errorf("stale bead %s should not appear in the newest-2 seed:\n%s", stale, all)
		}
	}
	// The seed must request the durable log newest-first.
	if !fake.listGlobalEventsLastReq.Desc {
		t.Errorf("feed seed must request order=desc (newest-first paging)")
	}
}

// TestFeedSeedCards_DegradesOnError confirms a daemon error yields an empty
// seed rather than failing the page render.
func TestFeedSeedCards_DegradesOnError(t *testing.T) {
	fake := newFake()
	fake.listGlobalEventsErr = context.DeadlineExceeded
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	if cards := srv.feedSeedCards(context.Background(), 30); len(cards) != 0 {
		t.Errorf("expected empty seed on error, got %d cards", len(cards))
	}
}

// TestHandleFeedBackfill_ReplaysSince drives /feed/backfill and asserts it
// replays renderable events as SSE cards and clamps a too-old ?since=.
func TestHandleFeedBackfill_ReplaysSince(t *testing.T) {
	fake := newFake()
	// Use times relative to now so the ?since= stays inside the handler's 24h
	// seed window on any wall-clock date (it clamps anything older). The event
	// sits just after the requested since, both comfortably within the window.
	since := time.Now().Add(-1 * time.Hour).UTC().Truncate(time.Second)
	t0 := since.Add(time.Minute)
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{ID: 10, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventData(t, "gd-r", "task", "replayed", "open")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	sinceParam := since.Format(time.RFC3339)
	req := httptest.NewRequest(http.MethodGet, "/feed/backfill?since="+sinceParam, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /feed/backfill = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: backfill") {
		t.Errorf("backfill should emit cards under the 'backfill' event name:\n%s", body)
	}
	if !strings.Contains(body, "gd-r") {
		t.Errorf("backfill missing the replayed card:\n%s", body)
	}
	// The since passed to the daemon should be the request's (it's within the
	// window, so honored, not clamped to 24h-ago).
	if got := fake.listGlobalEventsLastReq.Since.UTC().Format(time.RFC3339); got != sinceParam {
		t.Errorf("backfill since = %q, want the request value %q", got, sinceParam)
	}
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ts
}
