package gardenermode

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// liveEventBead is an event-mode plan gardener as the 11-01 flip left the live
// ones: plan-activity plus a daily watchdog, no steward-nudge.
func liveEventBead(t *testing.T) *beadsapi.BeadDetail {
	t.Helper()
	old := EventTriggers("gd-EPIC1", "", "5 7 * * *", "America/Los_Angeles", nil)
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	return &beadsapi.BeadDetail{
		ID:     "gd-TRIGGER1",
		Labels: []string{"trigger:multi", "plan-gardener", "gardener-epic:gd-EPIC1"},
		Fields: map[string]string{"project": "gardenio", "timezone": "America/Los_Angeles", "triggers": string(raw)},
	}
}

func labelsOf(entries []scheduler.TriggerEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Label)
	}
	return out
}

func TestEventTriggers_StewardNudgeShape(t *testing.T) {
	got := EventTriggers("gd-EPIC1", "gardenio", "5 7 * * *", "UTC", nil)
	if l := labelsOf(got); len(l) != 3 || l[0] != PlanActivityLabel || l[1] != StewardNudgeLabel || l[2] != "fallback-watchdog" {
		t.Fatalf("labels = %v", l)
	}
	n := got[1]
	if n.Kind != scheduler.TriggerKindBeadEvent || len(n.Events) != 1 || n.Events[0] != "comment.added" ||
		n.Throttle != "1h" || n.Debounce != "" || n.Preflight != nil {
		t.Fatalf("nudge entry = %+v", n)
	}
	want := &conditions.Expression{All: []conditions.Expression{
		{Leaf: conditions.Leaf{Field: "id", Op: "==", Value: "gd-EPIC1"}},
		{Leaf: conditions.Leaf{Field: "comment_author", Op: "contains", Value: "gardenio-steward-"}},
		{Leaf: conditions.Leaf{Field: "comment_text", Op: "contains", Value: "; nudging"}},
	}}
	gotJSON, _ := json.Marshal(n.Match)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("match = %s\nwant    %s", gotJSON, wantJSON)
	}

	// No project, no steward to name: the entry is left out.
	if l := labelsOf(EventTriggers("gd-EPIC1", "", "", "UTC", nil)); len(l) != 1 || l[0] != PlanActivityLabel {
		t.Fatalf("project-less labels = %v", l)
	}
}

// The nudge match reads the comment's author and text, never the epic's
// created_by, and only a comment on the epic itself.
func TestStewardNudgeEntry_Matches(t *testing.T) {
	m := *StewardNudgeEntry("gd-EPIC1", "gardenio").Match
	nudge := "steward: gardener gd-TRIGGER1 quiet 3h with 2 ready steps; nudging"
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"steward nudge on the epic", map[string]any{"id": "gd-EPIC1", "comment_author": "gardenio-steward-k2f9", "comment_text": nudge}, true},
		{"crew comment", map[string]any{"id": "gd-EPIC1", "comment_author": "gardenio-fix-the-flake-x1z", "comment_text": nudge}, false},
		{"gardener comment", map[string]any{"id": "gd-EPIC1", "comment_author": "gardenio-plan-gardener-for-gd-epic1-tgwk", "comment_text": nudge}, false},
		{"owner comment", map[string]any{"id": "gd-EPIC1", "comment_author": "matthewbaker1110@gmail.com", "comment_text": nudge}, false},
		{"steward comment on a step", map[string]any{"id": "gd-STEP1", "comment_author": "gardenio-steward-k2f9", "comment_text": nudge}, false},
		{"steward note that is not a nudge", map[string]any{"id": "gd-EPIC1", "comment_author": "gardenio-steward-k2f9", "comment_text": "steward: asked the owner, decision gd-D1"}, false},
		{"steward-created epic, crew comment", map[string]any{"id": "gd-EPIC1", "created_by": "gardenio-steward-k2f9", "comment_author": "gardenio-crew-abc", "comment_text": nudge}, false},
		{"no comment fields (a label add)", map[string]any{"id": "gd-EPIC1", "created_by": "gardenio-steward-k2f9"}, false},
	}
	for _, tc := range cases {
		got, err := conditions.Evaluate(context.Background(), m, conditions.Env{Payload: tc.payload})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// `gb gardener mode --event` on a live event gardener adds steward-nudge, keeps
// the watchdog, and a second run writes nothing.
func TestSet_EventRefreshAddsStewardNudge(t *testing.T) {
	b := liveEventBead(t)
	w := &fakeWriter{}
	res, err := Set(context.Background(), w, b, ModeEvent, Opts{Operator: "matt"})
	if err != nil || !res.Changed || !res.Refreshed || res.Mode != ModeEvent || res.Cron != "5 7 * * *" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if len(w.calls) != 2 || w.calls[0].op != "fields" || w.calls[1].op != "comment" {
		t.Fatalf("calls = %v, want fields then comment (no label change)", w.calls)
	}
	if _, ok := w.fields["cron"]; ok || len(w.fields) != 1 {
		t.Fatalf("a refresh writes only triggers, got %+v", w.fields)
	}
	raw := w.fields["triggers"].(string)
	want, _ := json.Marshal(EventTriggers("gd-EPIC1", "gardenio", "5 7 * * *", "America/Los_Angeles", nil))
	if raw != string(want) {
		t.Fatalf("triggers = %s\nwant      %s", raw, want)
	}

	b.Fields["triggers"] = raw
	w = &fakeWriter{}
	res, err = Set(context.Background(), w, b, ModeEvent, Opts{Operator: "matt"})
	if err != nil || res.Changed || res.Refreshed || len(w.calls) != 0 {
		t.Fatalf("second run: err=%v res=%+v calls=%v", err, res, w.calls)
	}
}

func TestRefreshEventTriggers_WatchdogAndExtras(t *testing.T) {
	b := liveEventBead(t)
	entries, _ := scheduler.ParseTriggers(b.Fields["triggers"])
	custom := scheduler.TriggerEntry{Kind: scheduler.TriggerKindBeadEvent, Label: "hand-added", Events: []string{"closed"}}
	entries = append(entries, custom)
	raw, _ := json.Marshal(entries)
	b.Fields["triggers"] = string(raw)

	got, changed, err := RefreshEventTriggers(b, Opts{FallbackCron: "9 9 * * *"})
	if err != nil || !changed {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	if l := labelsOf(got); len(l) != 4 || l[0] != PlanActivityLabel || l[1] != StewardNudgeLabel || l[2] != "hand-added" || l[3] != "fallback-watchdog" {
		t.Fatalf("labels = %v", l)
	}
	if got[3].Cron != "9 9 * * *" {
		t.Fatalf("--fallback-cron must replace the watchdog, got %q", got[3].Cron)
	}

	got, _, err = RefreshEventTriggers(b, Opts{NoFallbackCron: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range got {
		if e.Kind == scheduler.TriggerKindCron {
			t.Fatalf("--no-fallback-cron must drop the watchdog, got %+v", got)
		}
	}

	// No epic label: nothing to scope, left alone.
	b.Labels = []string{"trigger:multi"}
	if _, changed, err := RefreshEventTriggers(b, Opts{}); err != nil || changed {
		t.Fatalf("epic-less: changed=%v err=%v", changed, err)
	}
}

// SetCadence keeps every non-cron entry, so a cadence edit keeps the nudge.
func TestSetCadence_KeepsStewardNudge(t *testing.T) {
	b := liveEventBead(t)
	raw, _ := json.Marshal(EventTriggers("gd-EPIC1", "gardenio", "5 7 * * *", "America/Los_Angeles", nil))
	b.Fields["triggers"] = string(raw)
	w := &fakeWriter{}
	if _, err := SetCadence(context.Background(), w, b, "0 */12 * * *", "", Opts{Operator: "matt"}); err != nil {
		t.Fatal(err)
	}
	got, _ := scheduler.ParseTriggers(w.fields["triggers"].(string))
	if l := labelsOf(got); len(l) != 3 || l[1] != StewardNudgeLabel || got[2].Cron != "0 */12 * * *" {
		t.Fatalf("labels = %v entries = %+v", l, got)
	}
}
