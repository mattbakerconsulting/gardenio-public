package bridge

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
)

// commentFrame is a beads.comment.added SSE frame as kbeads writes it:
// events.CommentAdded{Comment: *model.Comment} marshalled, so the comment sits
// under "comment" with bead_id, author (the daemon's authenticated subject)
// and text.
func commentFrame(t *testing.T, beadID, author, text string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{"comment": map[string]any{
		"id": 7, "bead_id": beadID, "author": author, "text": text,
		"created_at": "2026-11-01T12:00:00Z",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCommentAddedFields(t *testing.T) {
	got := CommentAddedFields([]byte(commentFrame(t, "gd-E", "gardenio-steward-k2f9", "hi")))
	if got["comment_author"] != "gardenio-steward-k2f9" || got["comment_text"] != "hi" {
		t.Fatalf("fields = %v", got)
	}
	for _, frame := range []string{`{"bead_id":"gd-E"}`, `not json`, `{"comment":{"bead_id":"gd-E"}}`} {
		if got := CommentAddedFields([]byte(frame)); got != nil {
			t.Errorf("%s: fields = %v, want nil", frame, got)
		}
	}
}

// TestMultiSource_StewardNudgeWakesPlanGardener drives a real comment.added
// frame through RegisterSSE's hydration path into a gardener carrying the
// gardenermode.EventTriggers shape: only the land steward's nudge comment on
// the epic itself fires it, through the steward-nudge entry. The epic's own
// created_by is a steward run in every case, so a match keyed on the hydrated
// bead's created_by (rather than the comment's author) would fire on all of
// them.
func TestMultiSource_StewardNudgeWakesPlanGardener(t *testing.T) {
	const nudge = "steward: gardener gd-CAP1 quiet 3h with 2 ready steps; nudging"
	daemon := &edgeHydrDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-EPIC": {ID: "gd-EPIC", Type: "epic", Status: "open", CreatedBy: "gardenio-steward-old1"},
			"gd-STEP": {ID: "gd-STEP", Type: "task", Status: "open", CreatedBy: "gardenio-steward-old1"},
		},
		parent: map[string]string{"gd-STEP": "gd-EPIC"},
	}
	trig := TriggerDef{
		ID:       "gd-CAP1",
		Project:  "gardenio",
		Triggers: gardenermode.EventTriggers("gd-EPIC", "gardenio", "", "UTC", nil),
	}

	cases := []struct {
		name, topic, frame string
		want               bool
	}{
		{"steward nudge on the epic", "beads.comment.added", commentFrame(t, "gd-EPIC", "gardenio-steward-k2f9", nudge), true},
		{"crew comment on the epic", "beads.comment.added", commentFrame(t, "gd-EPIC", "gardenio-fix-the-flake-x1z", nudge), false},
		{"gardener comment on the epic", "beads.comment.added", commentFrame(t, "gd-EPIC", "gardenio-plan-gardener-for-gd-epic-tgwk", "iteration done"), false},
		{"steward nudge text on a step", "beads.comment.added", commentFrame(t, "gd-STEP", "gardenio-steward-k2f9", nudge), false},
		{"steward note that is not a nudge", "beads.comment.added", commentFrame(t, "gd-EPIC", "gardenio-steward-k2f9", "steward: asked the owner"), false},
		{"label added to the epic", "beads.label.added", `{"bead_id":"gd-EPIC","label":"x"}`, false},
	}
	for _, tc := range cases {
		src := NewMultiTriggerSource(nil, nil)
		src.daemon = daemon
		edgeHydrEmit(src, tc.topic, tc.frame)
		matches, err := src.Evaluate(context.Background(), trig)
		if err != nil {
			t.Fatalf("%s: evaluate: %v", tc.name, err)
		}
		if !tc.want {
			if len(matches) != 0 {
				t.Errorf("%s: fired %d match(es) %+v, want none", tc.name, len(matches), matches)
			}
			continue
		}
		if len(matches) != 1 || matches[0].Fields["multi_entry_label"] != gardenermode.StewardNudgeLabel {
			t.Fatalf("%s: matches = %+v, want one steward-nudge fire", tc.name, matches)
		}
	}
}
