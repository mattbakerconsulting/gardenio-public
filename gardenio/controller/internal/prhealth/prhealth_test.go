package prhealth

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

type fakeQuerier struct {
	triggers []*beadsapi.BeadDetail
	beads    map[string]*beadsapi.BeadDetail
	query    beadsapi.ListBeadsQuery
}

func (f *fakeQuerier) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.query = q
	return &beadsapi.ListBeadsResult{Beads: f.triggers, Total: len(f.triggers)}, nil
}

func (f *fakeQuerier) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	if b, ok := f.beads[id]; ok {
		return b, nil
	}
	return nil, errors.New("not found")
}

const sample = `{"at":"2026-10-30T14:17:00Z","open":22,"desk_queue":3,"orphans":9,` +
	`"oldest_orphan_hours":124.5,"oldest_open_hours":130,"oldest_open_pr":137,"train":"green","train_pr":1570}`

func TestParse(t *testing.T) {
	h, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	want := Health{
		At: time.Date(2026, 10, 30, 14, 17, 0, 0, time.UTC), Open: 22, DeskQueue: 3, Orphans: 9,
		OldestOrphanHours: 124.5, OldestOpenHours: 130, OldestOpenPR: 137, Train: TrainGreen, TrainPR: 1570,
	}
	if *h != want {
		t.Errorf("Parse = %+v, want %+v", *h, want)
	}
}

func TestParseRejectsMissingAtAndBlanksUnknownTrain(t *testing.T) {
	if _, err := Parse(`{"open":3}`); err == nil {
		t.Error("Parse without at: want an error")
	}
	if _, err := Parse(`not json`); err == nil {
		t.Error("Parse of junk: want an error")
	}
	h, err := Parse(`{"at":"2026-10-30T14:17:00Z","train":"Choo-choo"}`)
	if err != nil {
		t.Fatal(err)
	}
	if h.Train != "" {
		t.Errorf("unknown train state = %q, want empty", h.Train)
	}
}

func TestLatestReadsTheLastRunOfTheNewestTrigger(t *testing.T) {
	q := &fakeQuerier{
		triggers: []*beadsapi.BeadDetail{
			{ID: "gd-old", Fields: map[string]string{"last_run": "2026-10-29T10:17:00Z", "last_agent_id": "gd-agentold"}},
			{ID: "gd-new", Fields: map[string]string{"last_run": "2026-10-30T14:17:00Z", "last_agent_id": "gd-agentnew"}},
		},
		beads: map[string]*beadsapi.BeadDetail{
			"gd-agentold": {ID: "gd-agentold", Fields: map[string]string{Field: `{"at":"2026-10-29T10:30:00Z","open":50}`}},
			"gd-agentnew": {ID: "gd-agentnew", Fields: map[string]string{Field: sample}},
		},
	}
	h, err := Latest(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if h == nil || h.Open != 22 {
		t.Fatalf("Latest = %+v, want the newest trigger's run (open 22)", h)
	}
	if len(q.query.Labels) != 1 || q.query.Labels[0] != TriggerLabel || q.query.Types[0] != "trigger" {
		t.Errorf("query = %+v, want type trigger labelled %s", q.query, TriggerLabel)
	}
}

func TestLatestNilWhileTheRunHasNotWritten(t *testing.T) {
	q := &fakeQuerier{
		triggers: []*beadsapi.BeadDetail{{ID: "gd-t", Fields: map[string]string{"last_agent_id": "gd-running"}}},
		beads:    map[string]*beadsapi.BeadDetail{"gd-running": {ID: "gd-running", Fields: map[string]string{}}},
	}
	h, err := Latest(context.Background(), q)
	if err != nil || h != nil {
		t.Fatalf("Latest = %+v, %v; want nil, nil", h, err)
	}
	// A trigger that never fired has no last run either.
	q.triggers = []*beadsapi.BeadDetail{{ID: "gd-t", Fields: map[string]string{}}}
	if h, err := Latest(context.Background(), q); err != nil || h != nil {
		t.Fatalf("Latest (never fired) = %+v, %v; want nil, nil", h, err)
	}
}

func TestLine(t *testing.T) {
	h, _ := Parse(sample)
	want := "22 open PRs · 3 in the desk queue · 9 orphaned (oldest 5.2d) · train green #1570 · as of 14:17 UTC"
	if got := h.Line(); got != want {
		t.Errorf("Line =\n  %s\nwant\n  %s", got, want)
	}
	quiet := Health{At: h.At, Open: 4, DeskQueue: 4, Train: TrainNone, TrainPR: 99}
	want = "4 open PRs · 4 in the desk queue · 0 orphaned · train none · as of 14:17 UTC"
	if got := quiet.Line(); got != want {
		t.Errorf("Line =\n  %s\nwant\n  %s", got, want)
	}
	if got := (Health{At: h.At, Orphans: 1, OldestOrphanHours: 5.4}).Line(); got != "0 open PRs · 0 in the desk queue · 1 orphaned (oldest 5h) · train unknown · as of 14:17 UTC" {
		t.Errorf("Line = %s", got)
	}
}

// The conflict figures the sweeper reads off the PR beads (gd-L9herzgeZF)
// round-trip, show on the line, and read as 0 from a record written before
// the fields existed.
func TestConflictFigures(t *testing.T) {
	h, err := Parse(`{"at":"2026-11-04T10:17:00Z","open":12,"desk_queue":5,"orphans":0,` +
		`"conflicting":4,"conflicting_unscanned":1,"oldest_conflict_hours":7.2,"train":"none"}`)
	if err != nil {
		t.Fatal(err)
	}
	if h.Conflicting != 4 || h.ConflictingUnscanned != 1 || h.OldestConflictHours != 7.2 {
		t.Errorf("Parse = %+v, want conflicting 4, unscanned 1, oldest 7.2h", *h)
	}
	want := "12 open PRs · 5 in the desk queue · 0 orphaned · 4 conflicting (oldest 7h) · train none · as of 10:17 UTC"
	if got := h.Line(); got != want {
		t.Errorf("Line =\n  %s\nwant\n  %s", got, want)
	}
	// Only GitHub-read conflicts: no start time, so no age.
	unscanned := Health{At: h.At, Conflicting: 2, ConflictingUnscanned: 2, Train: TrainNone}
	want = "0 open PRs · 0 in the desk queue · 0 orphaned · 2 conflicting · train none · as of 10:17 UTC"
	if got := unscanned.Line(); got != want {
		t.Errorf("Line =\n  %s\nwant\n  %s", got, want)
	}
	old, err := Parse(sample)
	if err != nil {
		t.Fatal(err)
	}
	if old.Conflicting != 0 || old.ConflictingUnscanned != 0 || old.OldestConflictHours != 0 {
		t.Errorf("a record without conflict fields = %+v, want zeros", *old)
	}
}
