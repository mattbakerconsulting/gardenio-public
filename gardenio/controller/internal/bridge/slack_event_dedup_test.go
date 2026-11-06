package bridge

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestSlackEventDedup_MarkSeen(t *testing.T) {
	d := newSlackEventDedup()

	// First sighting of an id is processed.
	if !d.MarkSeen("Ev123") {
		t.Fatal("first MarkSeen should return true (process)")
	}
	// A redelivery of the same id is dropped.
	if d.MarkSeen("Ev123") {
		t.Fatal("second MarkSeen of same id should return false (drop)")
	}
	// A different id is processed independently.
	if !d.MarkSeen("Ev456") {
		t.Fatal("distinct id should return true (process)")
	}
	// The original id is still remembered.
	if d.MarkSeen("Ev123") {
		t.Fatal("repeated id should still be deduped after an intervening distinct id")
	}
}

func TestSlackEventDedup_EmptyIDAlwaysProcessed(t *testing.T) {
	d := newSlackEventDedup()
	// Without a stable id we cannot dedup — never drop, or we lose the event.
	for i := range 3 {
		if !d.MarkSeen("") {
			t.Fatalf("empty event id must always be processed (iteration %d)", i)
		}
	}
}

func TestSlackEventDedup_BoundedEviction(t *testing.T) {
	d := newSlackEventDedup()
	// Insert well past the cap; the set must stay bounded.
	for i := range slackEventDedupMax * 2 {
		d.MarkSeen(fmt.Sprintf("Ev-%d", i))
	}
	d.mu.Lock()
	n := len(d.seen)
	d.mu.Unlock()
	if n > slackEventDedupMax {
		t.Fatalf("seen set grew to %d, expected <= cap %d", n, slackEventDedupMax)
	}
}

func TestEventIDFromRequestPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{
			name:    "event_callback with event_id",
			payload: `{"type":"event_callback","event_id":"Ev0A1B2C3D","event":{"type":"app_mention"}}`,
			want:    "Ev0A1B2C3D",
		},
		{
			name:    "missing event_id",
			payload: `{"type":"event_callback","event":{"type":"message"}}`,
			want:    "",
		},
		{
			name:    "unparseable payload",
			payload: `not json`,
			want:    "",
		},
		{
			name:    "empty payload",
			payload: ``,
			want:    "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := eventIDFromRequestPayload(json.RawMessage(tc.payload))
			if got != tc.want {
				t.Fatalf("eventIDFromRequestPayload(%q) = %q, want %q", tc.payload, got, tc.want)
			}
		})
	}
}
