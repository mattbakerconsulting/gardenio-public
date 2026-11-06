package server

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestSweepHistory_DeletesOnlyTargetedRows: the sweep deletes old
// beads.bead.updated events and old field='fields' history of trigger and
// agent beads, and nothing else: a recent row, another topic or field, and
// any row of another type all survive.
func TestSweepHistory_DeletesOnlyTargetedRows(t *testing.T) {
	srv, ms, _ := testCtx(t)
	old := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	for id, typ := range map[string]model.BeadType{"gd-trg": "trigger", "gd-agt": "agent", "gd-tsk": model.TypeTask} {
		ms.beads[id] = &model.Bead{ID: id, Type: typ, Status: model.StatusOpen}
	}

	type evRow struct {
		bead, topic string
		at          time.Time
		keep        bool
	}
	evRows := []evRow{
		{"gd-trg", events.TopicBeadUpdated, old, false},
		{"gd-agt", events.TopicBeadUpdated, old, false},
		{"gd-trg", events.TopicBeadUpdated, recent, true},
		{"gd-trg", events.TopicBeadCreated, old, true},
		{"gd-trg", events.TopicBeadClosed, old, true},
		{"gd-tsk", events.TopicBeadUpdated, old, true},
	}
	for i, r := range evRows {
		ms.events = append(ms.events, &model.Event{ID: int64(i + 1), BeadID: r.bead, Topic: r.topic, CreatedAt: r.at})
	}

	type mutRow struct {
		bead, field string
		at          time.Time
		keep        bool
	}
	mutRows := []mutRow{
		{"gd-trg", "fields", old, false},
		{"gd-agt", "fields", old, false},
		{"gd-trg", "fields", recent, true},
		{"gd-trg", "status", old, true},
		{"gd-agt", "assignee", old, true},
		{"gd-trg", "*", old, true},
		{"gd-agt", "fields.agent_state", old, true},
		{"gd-tsk", "fields", old, true},
	}
	for i, r := range mutRows {
		ms.mutations = append(ms.mutations, &model.MutationRow{ID: int64(i + 1), BeadID: r.bead, Field: r.field, CreatedAt: r.at})
	}

	srv.sweepHistory(context.Background(), 14)

	var wantEv []int64
	for i, r := range evRows {
		if r.keep {
			wantEv = append(wantEv, int64(i+1))
		}
	}
	var gotEv []int64
	for _, e := range ms.events {
		gotEv = append(gotEv, e.ID)
	}
	if !equalIDs(gotEv, wantEv) {
		t.Errorf("events kept = %v, want %v", gotEv, wantEv)
	}

	var wantMut []int64
	for i, r := range mutRows {
		if r.keep {
			wantMut = append(wantMut, int64(i+1))
		}
	}
	var gotMut []int64
	for _, m := range ms.mutations {
		gotMut = append(gotMut, m.ID)
	}
	if !equalIDs(gotMut, wantMut) {
		t.Errorf("mutations kept = %v, want %v", gotMut, wantMut)
	}
}

// TestSweepHistory_DrainsInBatches: a backlog larger than one batch drains in
// one tick.
func TestSweepHistory_DrainsInBatches(t *testing.T) {
	prev := historyRetentionPause
	historyRetentionPause = 0
	t.Cleanup(func() { historyRetentionPause = prev })

	srv, ms, _ := testCtx(t)
	ms.beads["gd-trg"] = &model.Bead{ID: "gd-trg", Type: "trigger"}
	old := time.Now().Add(-30 * 24 * time.Hour)
	n := historyRetentionBatch*2 + 7
	for i := 0; i < n; i++ {
		ms.mutations = append(ms.mutations, &model.MutationRow{ID: int64(i + 1), BeadID: "gd-trg", Field: "fields", CreatedAt: old})
	}
	srv.sweepHistory(context.Background(), 14)
	if len(ms.mutations) != 0 {
		t.Errorf("%d of %d rows left after one tick", len(ms.mutations), n)
	}
}
