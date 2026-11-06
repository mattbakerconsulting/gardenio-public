package gardenhub

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func ev(event, data string) beadsapi.SSEEvent {
	return beadsapi.SSEEvent{Event: event, Data: []byte(data)}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		in   beadsapi.SSEEvent
		want Relevance
	}{
		{"agent usage write touches only the roster",
			ev("beads.bead.updated", `{"bead":{"type":"agent","status":"open"},"changes":{"fields":{}}}`), Relevance{Roster: true}},
		{"agent state change touches hub and roster",
			ev("beads.bead.updated", `{"bead":{"type":"agent","status":"open"},"changes":{"fields":{},"agent_state":"working"}}`), Relevance{Hub: true, Roster: true}},
		{"new agent touches hub and roster",
			ev("beads.bead.created", `{"bead":{"type":"agent","status":"open"}}`), Relevance{Hub: true, Roster: true}},
		{"trigger write touches nothing",
			ev("beads.bead.updated", `{"bead":{"type":"trigger","status":"open"},"changes":{"fields":{}}}`), Relevance{}},
		{"message create touches nothing",
			ev("beads.bead.created", `{"bead":{"type":"message","status":"open"}}`), Relevance{}},
		{"task update touches the farm",
			ev("beads.bead.updated", `{"bead":{"type":"task","status":"open"},"changes":{"title":"x"}}`), Relevance{Farm: true}},
		{"in_progress task touches hub and farm",
			ev("beads.bead.updated", `{"bead":{"type":"task","status":"in_progress"},"changes":{"notes":"x"}}`), Relevance{Hub: true, Farm: true}},
		{"a status change of any type touches the hub",
			ev("beads.bead.updated", `{"bead":{"type":"session","status":"closed"},"changes":{"status":"closed"}}`), Relevance{Hub: true}},
		{"decision touches hub and farm",
			ev("beads.bead.created", `{"bead":{"type":"decision","status":"open"}}`), Relevance{Hub: true, Farm: true}},
		{"report touches the hub (has_report) and the farm (a plan's produce bin)",
			ev("beads.bead.created", `{"bead":{"type":"report","status":"open"}}`), Relevance{Hub: true, Farm: true}},
		{"evidence touches the farm only (a closed step's harvest)",
			ev("beads.bead.created", `{"bead":{"type":"evidence","status":"open"}}`), Relevance{Farm: true}},
		{"close of any type touches the hub",
			ev("beads.bead.closed", `{"bead":{"type":"message","status":"closed"}}`), Relevance{Hub: true}},
		{"unreadable payload fails open", ev("beads.bead.updated", `not json`), relevantAll},
		{"payload without a bead fails open", ev("beads.bead.deleted", `{"bead_id":"gd-1"}`), relevantAll},
		{"empty payload fails open", ev("beads.bead.updated", ``), relevantAll},
		{"a comment moves only the open panels", ev("beads.comment.added", `{"comment":{"bead_id":"gd-1"}}`), Relevance{}},
		{"a link moves only the open panels", ev("beads.dependency.removed", `{"bead_id":"gd-1","depends_on_id":"gd-2"}`), Relevance{}},
		{"a label moves only the open panels", ev("beads.label.added", `{"bead_id":"gd-1","label":"x"}`), Relevance{}},
	}
	for _, c := range cases {
		if got := classify(c.in); got != c.want {
			t.Errorf("%s: classify = %+v, want %+v", c.name, got, c.want)
		}
	}
}
