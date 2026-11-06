package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
)

// collectSink records every line for assertions.
type collectSink struct {
	lines   []string
	flushes int
}

func (c *collectSink) add(line string) error { c.lines = append(c.lines, line); return nil }
func (c *collectSink) flush() error          { c.flushes++; return nil }

func mkEvent(id int64, topic string, created time.Time, bead *model.Bead, changes map[string]any) *model.Event {
	payload := payloadView{Bead: bead, Changes: changes}
	raw, _ := json.Marshal(payload)
	return &model.Event{ID: id, Topic: topic, CreatedAt: created, Payload: raw}
}

func TestBackfillRunningTotalsAndTimestamps(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	gardenio := &model.Bead{Type: model.TypeTask, Labels: []string{"project:gardenio"}}
	evs := []*model.Event{
		mkEvent(1, "beads.bead.created", t0, gardenio, nil),
		mkEvent(2, "beads.bead.created", t0.Add(time.Minute), gardenio, nil),
		mkEvent(3, "beads.bead.updated", t0.Add(2*time.Minute), gardenio, map[string]any{"assignee": "agent-x"}),
	}
	sink := &collectSink{}
	bf := &backfiller{
		fetch: func(_ context.Context, _ *client.ListGlobalEventsRequest) (*client.ListGlobalEventsResponse, error) {
			return &client.ListGlobalEventsResponse{Events: evs, HasMore: false}, nil
		},
		sink:       sink,
		totals:     make(map[string]float64),
		pageSize:   1000,
		flushLines: 5000,
	}
	stats, err := bf.run(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.eventsScanned != 3 || stats.samplesEmitted != 3 {
		t.Fatalf("scanned=%d emitted=%d, want 3/3", stats.eventsScanned, stats.samplesEmitted)
	}
	if len(sink.lines) != 3 {
		t.Fatalf("got %d lines, want 3", len(sink.lines))
	}

	// created counter: cumulative 1 then 2; claim counter: 1.
	wantPrefix := backfillMetricName + `{event="` + metrics.BeadEventCreated + `",project="gardenio",bead_type="task"} `
	if !strings.HasPrefix(sink.lines[0], wantPrefix+"1 ") {
		t.Errorf("line0 = %q, want cumulative 1", sink.lines[0])
	}
	if !strings.HasPrefix(sink.lines[1], wantPrefix+"2 ") {
		t.Errorf("line1 = %q, want cumulative 2", sink.lines[1])
	}
	if !strings.Contains(sink.lines[2], `event="`+metrics.BeadEventClaimed+`"`) || !strings.HasSuffix(sink.lines[2], " "+itoa(t0.Add(2*time.Minute).UnixMilli())) {
		t.Errorf("line2 = %q, want claim at created_at+2m", sink.lines[2])
	}
	// First sample timestamp is t0 in epoch millis.
	if !strings.HasSuffix(sink.lines[0], " "+itoa(t0.UnixMilli())) {
		t.Errorf("line0 timestamp = %q, want %d", sink.lines[0], t0.UnixMilli())
	}
}

func TestBackfillPagination(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	b := &model.Bead{Type: model.TypeBug, Labels: nil} // no project label → "unknown"
	page1 := []*model.Event{mkEvent(1, "beads.bead.closed", t0, b, nil), mkEvent(2, "beads.bead.closed", t0, b, nil)}
	page2 := []*model.Event{mkEvent(3, "beads.bead.closed", t0, b, nil)}

	var seenCursors []int64
	sink := &collectSink{}
	bf := &backfiller{
		fetch: func(_ context.Context, req *client.ListGlobalEventsRequest) (*client.ListGlobalEventsResponse, error) {
			seenCursors = append(seenCursors, req.Cursor)
			if req.Cursor == 0 {
				return &client.ListGlobalEventsResponse{Events: page1, HasMore: true, NextCursor: "2"}, nil
			}
			return &client.ListGlobalEventsResponse{Events: page2, HasMore: false}, nil
		},
		sink:       sink,
		totals:     make(map[string]float64),
		pageSize:   2,
		flushLines: 5000,
	}
	stats, err := bf.run(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats.pages != 2 || stats.eventsScanned != 3 {
		t.Fatalf("pages=%d scanned=%d, want 2/3", stats.pages, stats.eventsScanned)
	}
	if len(seenCursors) != 2 || seenCursors[0] != 0 || seenCursors[1] != 2 {
		t.Fatalf("cursors=%v, want [0 2]", seenCursors)
	}
	// Cumulative across pages: 1,2,3 for the same series.
	if !strings.HasSuffix(strings.Fields(sink.lines[2])[1], "") || !strings.Contains(sink.lines[2], `project="unknown",bead_type="bug"`) {
		t.Errorf("line2 = %q, want unknown/bug series", sink.lines[2])
	}
	if v := strings.Fields(sink.lines[2])[1]; v != "3" {
		t.Errorf("third cumulative value = %q, want 3", v)
	}
}

func TestBackfillMaxPagesCap(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	b := &model.Bead{Type: model.TypeTask}
	sink := &collectSink{}
	var nextID int64 = 1
	bf := &backfiller{
		fetch: func(_ context.Context, _ *client.ListGlobalEventsRequest) (*client.ListGlobalEventsResponse, error) {
			// Always claims more pages, with a strictly-advancing cursor so the
			// run stops on the max-pages cap rather than the non-advancing guard.
			id := nextID
			nextID++
			return &client.ListGlobalEventsResponse{Events: []*model.Event{mkEvent(id, "beads.bead.created", t0, b, nil)}, HasMore: true, NextCursor: itoa(id)}, nil
		},
		sink:       sink,
		totals:     make(map[string]float64),
		pageSize:   1,
		flushLines: 5000,
		maxPages:   3,
	}
	stats, err := bf.run(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !stats.pagesCapped || stats.pages != 3 {
		t.Fatalf("pages=%d capped=%v, want 3/true", stats.pages, stats.pagesCapped)
	}
}

func TestBackfillNonAdvancingCursorStops(t *testing.T) {
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	b := &model.Bead{Type: model.TypeTask}
	bf := &backfiller{
		fetch: func(_ context.Context, _ *client.ListGlobalEventsRequest) (*client.ListGlobalEventsResponse, error) {
			return &client.ListGlobalEventsResponse{Events: []*model.Event{mkEvent(1, "beads.bead.created", t0, b, nil)}, HasMore: true, NextCursor: "0"}, nil
		},
		sink:       &collectSink{},
		totals:     make(map[string]float64),
		pageSize:   1,
		flushLines: 5000,
	}
	if _, err := bf.run(context.Background(), t0.Add(-time.Hour), t0.Add(time.Hour)); err == nil {
		t.Fatal("expected error on non-advancing cursor, got nil")
	}
}

func TestDecodeEventPayload(t *testing.T) {
	bead := &model.Bead{Type: model.TypeFeature, Labels: []string{"area:x", "project:demo"}}
	raw, _ := json.Marshal(payloadView{Bead: bead, Changes: map[string]any{"status": "closed"}})
	proj, bt, changes := decodeEventPayload(raw)
	if proj != "demo" || bt != "feature" {
		t.Errorf("decode = (%q,%q), want (demo,feature)", proj, bt)
	}
	if _, ok := changes["status"]; !ok {
		t.Errorf("changes missing status: %v", changes)
	}

	// Bead-less payload (e.g. dependency.added) → empty project/type.
	if p, b, c := decodeEventPayload(json.RawMessage(`{"from":"gd-a","to":"gd-b"}`)); p != "" || b != "" || c != nil {
		t.Errorf("beadless decode = (%q,%q,%v), want empty", p, b, c)
	}
	// Garbage/empty payload degrades gracefully.
	if p, b, c := decodeEventPayload(nil); p != "" || b != "" || c != nil {
		t.Errorf("nil decode = (%q,%q,%v), want empty", p, b, c)
	}
	if p, _, _ := decodeEventPayload(json.RawMessage(`not json`)); p != "" {
		t.Errorf("garbage decode project = %q, want empty", p)
	}
}

func TestFormatSampleShape(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	got := formatSample(backfillMetricName, "created", "gardenio", "task", 7, ts)
	want := backfillMetricName + `{event="created",project="gardenio",bead_type="task"} 7 ` + itoa(ts.UnixMilli())
	if got != want {
		t.Errorf("formatSample = %q, want %q", got, want)
	}
}

func TestEscapeLabelValue(t *testing.T) {
	if got := escapeLabelValue("plain"); got != "plain" {
		t.Errorf("plain = %q", got)
	}
	if got := escapeLabelValue(`a"b\c` + "\n"); got != `a\"b\\c\n` {
		t.Errorf("escaped = %q", got)
	}
}

func itoa(v int64) string {
	return strings.TrimSpace(jsonNumber(v))
}

func jsonNumber(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
