package server

import (
	"bytes"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

// topicEventsFixture is a bead history interleaving label events with the
// bead.updated rows that make an unfiltered scan expensive.
func topicEventsFixture(ms *mockStore) {
	ms.events = []*model.Event{
		{ID: 1, Topic: "beads.bead.created", BeadID: "gd-trig", Payload: json.RawMessage(`{}`)},
		{ID: 2, Topic: "beads.label.removed", BeadID: "gd-trig", Payload: json.RawMessage(`{"label":"needs-intake"}`)},
		{ID: 3, Topic: "beads.bead.updated", BeadID: "gd-trig", Payload: json.RawMessage(`{"big":"body"}`)},
		{ID: 4, Topic: "beads.label.removed", BeadID: "gd-trig", Payload: json.RawMessage(`{"label":"other"}`)},
		{ID: 5, Topic: "beads.label.added", BeadID: "gd-trig", Payload: json.RawMessage(`{"label":"needs-intake"}`)},
		{ID: 6, Topic: "beads.bead.updated", BeadID: "gd-trig", Payload: json.RawMessage(`{"big":"body"}`)},
		{ID: 7, Topic: "beads.label.removed", BeadID: "gd-other", Payload: json.RawMessage(`{"label":"needs-intake"}`)},
	}
}

func eventIDs(t *testing.T, body []byte) []int64 {
	t.Helper()
	var result struct {
		Events []model.Event `json:"events"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode: %v (body %s)", err, body)
	}
	ids := make([]int64, 0, len(result.Events))
	for _, e := range result.Events {
		ids = append(ids, e.ID)
	}
	return ids
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestHandleGetEvents_Topic pins the opt-in ?topic= filter (gd-cP9tODid8j):
// only that bead's events of that topic, chronological.
func TestHandleGetEvents_Topic(t *testing.T) {
	_, ms, h := newTestServer()
	topicEventsFixture(ms)

	rec := doJSON(t, h, "GET", "/v1/beads/gd-trig/events?topic=beads.label.removed", nil)
	requireStatus(t, rec, 200)
	if got, want := eventIDs(t, rec.Body.Bytes()), []int64{2, 4}; !equalIDs(got, want) {
		t.Fatalf("topic=beads.label.removed ids = %v, want %v", got, want)
	}

	rec = doJSON(t, h, "GET", "/v1/beads/gd-trig/events?topic=beads.no.such.topic", nil)
	requireStatus(t, rec, 200)
	if got := eventIDs(t, rec.Body.Bytes()); len(got) != 0 {
		t.Fatalf("unknown topic ids = %v, want none", got)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"events":[]`)) {
		t.Fatalf("unknown topic body = %s, want an empty events array (not null)", rec.Body.String())
	}
}

// TestHandleGetEvents_TopicWithLimit pins ?topic= combined with ?limit=N:
// the newest N events of that topic, still chronological — the shape the
// trigger-engine's label scans request.
func TestHandleGetEvents_TopicWithLimit(t *testing.T) {
	_, ms, h := newTestServer()
	topicEventsFixture(ms)

	rec := doJSON(t, h, "GET", "/v1/beads/gd-trig/events?topic=beads.label.removed&limit=1", nil)
	requireStatus(t, rec, 200)
	if got, want := eventIDs(t, rec.Body.Bytes()), []int64{4}; !equalIDs(got, want) {
		t.Fatalf("topic+limit=1 ids = %v, want %v", got, want)
	}

	// An invalid limit is still rejected when a topic is set.
	rec = doJSON(t, h, "GET", "/v1/beads/gd-trig/events?topic=beads.label.removed&limit=0", nil)
	requireStatus(t, rec, 400)
}

// TestHandleGetEvents_NoTopicUnchanged pins that the filter is opt-in: an
// empty ?topic= is byte-identical to omitting it, with and without ?limit.
func TestHandleGetEvents_NoTopicUnchanged(t *testing.T) {
	_, ms, h := newTestServer()
	topicEventsFixture(ms)

	for _, q := range []string{"", "limit=3"} {
		base := doJSON(t, h, "GET", "/v1/beads/gd-trig/events?"+q, nil)
		requireStatus(t, base, 200)
		withEmpty := doJSON(t, h, "GET", "/v1/beads/gd-trig/events?topic=&"+q, nil)
		requireStatus(t, withEmpty, 200)
		if !bytes.Equal(base.Body.Bytes(), withEmpty.Body.Bytes()) {
			t.Fatalf("?%s: empty topic changed the response:\n%s\nvs\n%s", q, base.Body.String(), withEmpty.Body.String())
		}
	}
	rec := doJSON(t, h, "GET", "/v1/beads/gd-trig/events", nil)
	if got := eventIDs(t, rec.Body.Bytes()); len(got) != 6 {
		t.Fatalf("no topic: %d events, want all 6 of gd-trig's", len(got))
	}
}
