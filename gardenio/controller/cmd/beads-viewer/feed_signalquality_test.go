package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// feed_signalquality_test.go — the iteration-2026-06-17 feed signal-quality
// fixes (epic ka-0000004907), all in one coordinated pass:
//   - ka-0000003549: agent-lifecycle gate/spawn/stop churn is dropped from the
//     stream entirely (never becomes a card).
//   - ka-0000002530: an MR's opened→pipeline→merged lifecycle, and a work
//     bead's create/claim/close state changes, each collapse to ONE latest-state
//     card (target max_dup<=1).
//   - ka-0000001008: the seed cards carry a status Lane so feed.html can SSR the
//     same cards into the lanes view (?view=lanes is non-empty server-side).

// feedEventDataFields builds an SSE wrapper payload with a fields blob, so a
// merge-request bead can carry mr_state / mr_pipeline_status the classifier
// reads. fieldsJSON is raw JSON for the bead's "fields" object ("" for none).
func feedEventDataFields(t *testing.T, id, typ, title, status, fieldsJSON string) json.RawMessage {
	t.Helper()
	bead := map[string]any{
		"id":     id,
		"type":   typ,
		"title":  title,
		"status": status,
		"labels": []string{"project:gardenio"},
	}
	if fieldsJSON != "" {
		bead["fields"] = json.RawMessage(fieldsJSON)
	}
	data, err := json.Marshal(map[string]any{"bead": bead})
	if err != nil {
		t.Fatalf("marshal feed event: %v", err)
	}
	return data
}

// TestCollectFeedCards_DropsAgentGateChurn drives the durable-seed scan with a
// realistic interleaving of substantive work cards and agent-lifecycle churn
// (spawn / stop / gate transitions). After ka-0000003549 the agent family is
// dropped entirely, so ONLY the work/MR cards survive — no spawned/stopped/gate
// card ever renders.
func TestCollectFeedCards_DropsAgentGateChurn(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			// A genuine work card.
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventData(t, "gd-work", "task", "real work", "open")},
			// Agent spawn — churn, dropped.
			{ID: 2, Topic: "beads.bead.created", CreatedAt: t0.Add(1 * time.Minute), Payload: feedEventData(t, "gd-agent1", "agent", "crew-1", "open")},
			// Agent gate transition — churn, dropped (the noise this fixes).
			{ID: 3, Topic: "beads.bead.updated", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventDataFields(t, "gd-agent1", "agent", "crew-1", "in_progress", `{"gate_satisfied_by":"yield"}`)},
			// Another genuine work card (a close).
			{ID: 4, Topic: "beads.bead.closed", CreatedAt: t0.Add(3 * time.Minute), Payload: feedEventData(t, "gd-done", "bug", "fixed", "closed")},
			// Agent stop — churn, dropped.
			{ID: 5, Topic: "beads.bead.closed", CreatedAt: t0.Add(4 * time.Minute), Payload: feedEventData(t, "gd-agent1", "agent", "crew-1", "closed")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	cards := srv.collectFeedCards(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour), 30)

	// Only the two work cards survive; all three agent-lifecycle events are gone.
	if len(cards) != 2 {
		t.Fatalf("expected 2 work cards (agent churn dropped), got %d:\n%s", len(cards), strings.Join(cards, "\n"))
	}
	joined := strings.Join(cards, "\n")
	for _, agentID := range []string{"gd-agent1"} {
		if strings.Contains(joined, agentID) {
			t.Errorf("agent-lifecycle bead %s should be dropped, but a card rendered:\n%s", agentID, joined)
		}
	}
	for _, kind := range []string{"feed-card-gate", "feed-card-spawned", "feed-card-stopped"} {
		if strings.Contains(joined, kind) {
			t.Errorf("agent-family card class %q should never render:\n%s", kind, joined)
		}
	}
	if !strings.Contains(joined, "gd-work") || !strings.Contains(joined, "gd-done") {
		t.Errorf("genuine work cards should survive:\n%s", joined)
	}
}

// TestCollectFeedCards_CollapsesMRLifecycle drives the durable-seed scan with a
// single MR emitting its full lifecycle (mr-opened → pipeline → mr-merged) plus
// a work bead emitting a created+closed pair. After ka-0000002530 each entity
// collapses to ONE latest-state card (max_dup<=1): the MR shows merged, the work
// bead shows closed.
func TestCollectFeedCards_CollapsesMRLifecycle(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			// One MR's three lifecycle events — must collapse to one merged card.
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventDataFields(t, "gd-mr", "merge-request", "feat: thing", "open", `{"mr_state":"opened"}`)},
			{ID: 2, Topic: "beads.bead.updated", CreatedAt: t0.Add(1 * time.Minute), Payload: feedEventDataFields(t, "gd-mr", "merge-request", "feat: thing", "open", `{"mr_state":"opened","mr_pipeline_status":"success"}`)},
			{ID: 3, Topic: "beads.bead.updated", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventDataFields(t, "gd-mr", "merge-request", "feat: thing", "open", `{"mr_state":"merged","mr_pipeline_status":"success"}`)},
			// A work bead's create+close pair — must collapse to one closed card.
			{ID: 4, Topic: "beads.bead.created", CreatedAt: t0.Add(3 * time.Minute), Payload: feedEventData(t, "gd-task", "task", "do it", "open")},
			{ID: 5, Topic: "beads.bead.closed", CreatedAt: t0.Add(4 * time.Minute), Payload: feedEventData(t, "gd-task", "task", "do it", "closed")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	cards := srv.collectFeedCards(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour), 30)

	// Exactly two cards: one per bead, each at its latest state.
	if len(cards) != 2 {
		t.Fatalf("expected 2 collapsed cards (one per bead), got %d:\n%s", len(cards), strings.Join(cards, "\n"))
	}

	// No bead id appears in more than one card — max_dup<=1.
	for _, id := range []string{"gd-mr", "gd-task"} {
		if n := strings.Count(strings.Join(cards, ""), `data-bead-id="`+id+`"`); n != 1 {
			t.Errorf("bead %s should render exactly one card (max_dup<=1), got %d", id, n)
		}
	}

	// The MR's surviving card is the most-advanced state: merged, not opened or
	// pipeline. Cards are oldest-first; the MR collapses to its newest position
	// (t0+2), which still sorts before the work bead's close (t0+4).
	joined := strings.Join(cards, "\n")
	if !strings.Contains(joined, "feed-card-mr-merged") {
		t.Errorf("MR lifecycle should collapse to the merged card:\n%s", joined)
	}
	if strings.Contains(joined, "feed-card-mr-opened") {
		t.Errorf("MR opened card should be folded away by the lifecycle collapse:\n%s", joined)
	}
	// The work bead's surviving card is the close, not the create.
	if !strings.Contains(joined, "feed-card-closed") {
		t.Errorf("work bead create+close should collapse to the closed card:\n%s", joined)
	}
}

// TestCollapseBeadLifecycle covers the pure across-kind lifecycle collapse: a
// bead's multiple cards fold to its newest occurrence, while distinct beads stay
// separate and nil-bead items pass through.
func TestCollapseBeadLifecycle(t *testing.T) {
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	mk := func(kind feedKind, id string, ts time.Time) feedCardItem {
		return feedCardItem{kind: kind, bead: &feedEventBead{ID: id}, ts: ts, count: 1}
	}

	in := []feedCardItem{
		mk(feedKindMROpened, "gd-mr", t0),                    // MR opened
		mk(feedKindClaimed, "gd-task", t0.Add(time.Minute)),  // distinct bead
		mk(feedKindPipeline, "gd-mr", t0.Add(2*time.Minute)), // MR pipeline (same bead, diff kind)
		mk(feedKindMRMerged, "gd-mr", t0.Add(3*time.Minute)), // MR merged — the latest state
	}
	out := collapseBeadLifecycle(in)

	if len(out) != 2 {
		t.Fatalf("expected 2 items (gd-mr folded to latest, gd-task), got %d", len(out))
	}
	// gd-task keeps its position; gd-mr's representative is the merged card at the
	// run's newest position (after gd-task).
	if out[0].bead.ID != "gd-task" {
		t.Errorf("out[0] = %s, want gd-task", out[0].bead.ID)
	}
	if out[1].bead.ID != "gd-mr" || out[1].kind.Name != feedKindMRMerged.Name {
		t.Errorf("out[1] = %s/%s, want gd-mr/mr-merged (latest state)", out[1].bead.ID, out[1].kind.Name)
	}
	if !out[1].ts.Equal(t0.Add(3 * time.Minute)) {
		t.Errorf("gd-mr representative ts = %v, want newest occurrence (05:03)", out[1].ts)
	}

	// A nil-bead item passes through untouched.
	nilItems := []feedCardItem{
		{kind: feedKindUpdated, bead: nil, ts: t0, count: 1},
		{kind: feedKindUpdated, bead: nil, ts: t0.Add(time.Minute), count: 1},
	}
	if got := collapseBeadLifecycle(nilItems); len(got) != 2 {
		t.Errorf("nil-bead items should pass through untouched, got %d items", len(got))
	}

	// Fewer than 2 items short-circuits.
	if got := collapseBeadLifecycle(in[:1]); len(got) != 1 {
		t.Errorf("single-item input should pass through, got %d", len(got))
	}
}

// TestFeedSeedCards_CarriesLaneForSSR confirms the first-paint seed stamps each
// card with its status lane so feed.html can SSR the same cards into the lanes
// view (ka-0000001008). A claim lands in_progress, a close lands done, a create
// lands open — each card's Lane must match.
func TestFeedSeedCards_CarriesLaneForSSR(t *testing.T) {
	fake := newFake()
	t0 := mustTime(t, "2026-06-15T05:00:00Z")
	fake.listGlobalEventsResp = &beadsapi.ListGlobalEventsResponse{
		Events: []beadsapi.Event{
			{ID: 1, Topic: "beads.bead.created", CreatedAt: t0, Payload: feedEventData(t, "gd-open", "task", "new", "open")},
			{ID: 2, Topic: "beads.bead.updated", CreatedAt: t0.Add(time.Minute), Payload: feedEventData(t, "gd-claim", "task", "claimed", "in_progress")},
			{ID: 3, Topic: "beads.bead.closed", CreatedAt: t0.Add(2 * time.Minute), Payload: feedEventData(t, "gd-close", "task", "done", "closed")},
		},
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})

	cards, _ := srv.feedSeedCardsAndUsers(context.Background(), 30)
	if len(cards) != 3 {
		t.Fatalf("expected 3 seed cards, got %d", len(cards))
	}

	// Every card must carry a non-empty status lane, and it must match the lane
	// embedded in the card's data-lane attribute, so the SSR bucketing in
	// feed.html lands each card in the right column.
	for _, c := range cards {
		if c.Lane == "" {
			t.Errorf("seed card has empty Lane (lanes SSR would drop it):\n%s", c.HTML)
		}
		if !strings.Contains(string(c.HTML), `data-lane="`+c.Lane+`"`) {
			t.Errorf("card Lane %q does not match its data-lane attribute:\n%s", c.Lane, c.HTML)
		}
	}

	// Spot-check the expected status buckets are represented.
	lanes := map[string]bool{}
	for _, c := range cards {
		lanes[c.Lane] = true
	}
	for _, want := range []string{"open", "in_progress", "done"} {
		if !lanes[want] {
			t.Errorf("expected a card in the %q lane, got lanes %v", want, lanes)
		}
	}
}
