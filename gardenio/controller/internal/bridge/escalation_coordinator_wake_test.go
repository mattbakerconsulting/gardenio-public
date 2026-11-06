package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/scheduler"
)

// coordinatorWakeEntries is the escalation-coordinator trigger's triggers[]
// as gardenio/docs/escalation-coordinator-prompt.md gives it (gd-Khn7cXyokc):
// wake on an approval executed by coordinator:cluster, on a create held by it,
// and on a 2h backstop that skips an empty lane.
const coordinatorWakeEntries = `[
  {"kind": "bead_event", "label": "approved", "events": ["updated"],
   "match": {"all": [
     {"field": "type", "op": "==", "value": "escalation"},
     {"field": "execute_by", "op": "==", "value": "coordinator:cluster"},
     {"field": "approval_transition", "op": "==", "value": "true"}]}},
  {"kind": "bead_event", "label": "filed", "events": ["created"],
   "match": {"all": [
     {"field": "type", "op": "==", "value": "escalation"},
     {"field": "held_by", "op": "==", "value": "coordinator:cluster"}]}},
  {"kind": "cron", "label": "backstop", "cron": "0 */2 * * *",
   "preflight": {"type": "bead_query", "op": ">", "value": "0", "held_by": ["coordinator:cluster"]}}
]`

// TestCoordinatorWake_FiresOnLaneWorkOnly drives the coordinator trigger's
// entries with the events the daemon publishes. The approval event wakes it;
// a later cosmetic write to the same approved escalation, which still carries
// approved_by, does not (gd-TMYvBxNY6h).
func TestCoordinatorWake_FiresOnLaneWorkOnly(t *testing.T) {
	entries, err := scheduler.ParseTriggers(coordinatorWakeEntries)
	if err != nil {
		t.Fatalf("ParseTriggers: %v", err)
	}
	trig := TriggerDef{
		ID:         "gd-coord-trigger",
		Title:      "escalation coordinator",
		Triggers:   entries,
		SourceData: multiTriggerData{},
	}
	// now is off the backstop's 2h grid, so only the event entries can fire.
	now := time.Date(2026, 11, 2, 9, 17, 0, 0, time.UTC)
	for _, tc := range coordinatorWakeCases() {
		t.Run(tc.name, func(t *testing.T) {
			src := newMultiSource(now)
			src.InjectBeadEvent(tc.topic, tc.bead)
			matches, err := src.Evaluate(context.Background(), trig)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if tc.want == "" {
				if len(matches) != 0 {
					t.Fatalf("fired %d time(s) (key %q), want no wake", len(matches), matches[0].Key)
				}
				return
			}
			if len(matches) != 1 {
				t.Fatalf("got %d matches, want one wake from %q", len(matches), tc.want)
			}
			if !strings.Contains(matches[0].Key, ":"+tc.want+":") {
				t.Errorf("key = %q, want a fire from entry %q", matches[0].Key, tc.want)
			}
			if got := matches[0].TemplateVars["{{event_bead_id}}"]; got != tc.bead.ID {
				t.Errorf("event_bead_id = %q, want %q", got, tc.bead.ID)
			}
		})
	}
}

type coordinatorWakeCase struct {
	name  string
	topic string
	bead  *BeadEvent
	want  string // the firing entry's label, "" for no fire
}

// coordinatorWakeCases are the daemon events TestCoordinatorWake_FiresOnLaneWorkOnly
// drives through the coordinator trigger.
func coordinatorWakeCases() []coordinatorWakeCase {
	approved := map[string]string{
		"execute_by":  "coordinator:cluster",
		"held_by":     "coordinator:cluster",
		"approved_by": "oidc:owner@example.com",
		"approved_at": "2026-11-02T09:00:00Z",
	}
	return []coordinatorWakeCase{
		{
			name:  "approval executed by the cluster lane",
			topic: "beads.bead.updated",
			bead: &BeadEvent{ID: "gd-esc-1", Type: "escalation", Status: "in_progress", Fields: approved,
				Changes: map[string]any{"approved_by": "oidc:owner@example.com", "status": "in_progress"}},
			want: "approved",
		},
		{
			name:  "cosmetic update of the approved escalation",
			topic: "beads.bead.updated",
			bead: &BeadEvent{ID: "gd-esc-1", Type: "escalation", Status: "in_progress", Fields: approved,
				Changes: map[string]any{"short_title": "close four fixed bugs"}},
		},
		{
			name:  "approved update with no changes",
			topic: "beads.bead.updated",
			bead:  &BeadEvent{ID: "gd-esc-1", Type: "escalation", Status: "in_progress", Fields: approved},
		},
		{
			name:  "approval executed by the laptop lane",
			topic: "beads.bead.updated",
			bead: &BeadEvent{ID: "gd-esc-2", Type: "escalation", Status: "in_progress",
				Fields:  map[string]string{"execute_by": "coordinator:laptop", "approved_by": "oidc:owner@example.com"},
				Changes: map[string]any{"approved_by": "oidc:owner@example.com"}},
		},
		{
			name:  "a task set in_progress is not an approval",
			topic: "beads.bead.updated",
			bead: &BeadEvent{ID: "gd-task-1", Type: "task", Status: "in_progress",
				Fields:  map[string]string{"execute_by": "coordinator:cluster"},
				Changes: map[string]any{"status": "in_progress"}},
		},
		{
			name:  "create held by the cluster lane",
			topic: "beads.bead.created",
			bead: &BeadEvent{ID: "gd-esc-3", Type: "escalation", Status: "open",
				Fields: map[string]string{"held_by": "coordinator:cluster", "execute_by": "coordinator:cluster"}},
			want: "filed",
		},
		{
			name:  "create held by the operator",
			topic: "beads.bead.created",
			bead: &BeadEvent{ID: "gd-esc-4", Type: "escalation", Status: "open",
				Fields: map[string]string{"held_by": "operator", "execute_by": "coordinator:laptop"}},
		},
		{
			name:  "a bead field cannot claim the approval",
			topic: "beads.bead.updated",
			bead: &BeadEvent{ID: "gd-esc-5", Type: "escalation", Status: "in_progress",
				Fields:  map[string]string{"execute_by": "coordinator:cluster", "approval_transition": "true"},
				Changes: map[string]any{"short_title": "x"}},
		},
	}
}
