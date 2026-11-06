package server

import (
	"encoding/json"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// TestHandleCloseBead_AlreadyClosed_NoOverwrite verifies that closing an
// already-closed bead is idempotent: existing fields (e.g. close_reason) are
// preserved, no second closed event is emitted, and closed_at/closed_by are
// not re-stamped. Regression guard for ka-0000000961.
func TestHandleCloseBead_AlreadyClosed_NoOverwrite(t *testing.T) {
	_, ms, h := newTestServer()
	closedAt := time.Date(2026, 4, 24, 12, 0, 0, 0, time.UTC)
	ms.beads["gd-already"] = &model.Bead{
		ID:       "gd-already",
		Title:    "Already closed",
		Kind:     model.KindIssue,
		Type:     model.TypeTask,
		Status:   model.StatusClosed,
		ClosedAt: &closedAt,
		ClosedBy: "original-actor",
		Fields:   json.RawMessage(`{"close_reason":"stop","agent_state":"done"}`),
	}

	rec := doJSON(t, h, "POST", "/v1/beads/gd-already/close", map[string]any{
		"close_reason": "context_cancelled",
		"agent_state":  "failed",
	})
	requireStatus(t, rec, 200)

	var got model.Bead
	decodeJSON(t, rec, &got)
	if got.Status != model.StatusClosed {
		t.Fatalf("status: got %q, want closed", got.Status)
	}
	if got.ClosedBy != "original-actor" {
		t.Fatalf("closed_by: got %q, want original-actor", got.ClosedBy)
	}
	if got.ClosedAt == nil || !got.ClosedAt.Equal(closedAt) {
		t.Fatalf("closed_at was re-stamped: got %v, want %v", got.ClosedAt, closedAt)
	}
	var fields map[string]string
	if err := json.Unmarshal(got.Fields, &fields); err != nil {
		t.Fatalf("unmarshal fields: %v", err)
	}
	if fields["close_reason"] != "stop" {
		t.Fatalf("close_reason overwritten: got %q, want stop", fields["close_reason"])
	}
	if fields["agent_state"] != "done" {
		t.Fatalf("agent_state overwritten: got %q, want done", fields["agent_state"])
	}
	for _, ev := range ms.events {
		if ev.Topic == "beads.bead.closed" {
			t.Fatalf("unexpected second close event emitted")
		}
	}
}
