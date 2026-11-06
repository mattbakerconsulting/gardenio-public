package beadsapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestGetRecentEventsByTopic_SendsTopicAndLimit pins the request shape of the
// topic-filtered events read (gd-cP9tODid8j): ?topic= and ?limit= on the
// per-bead events route, with the bead id path-escaped.
func TestGetRecentEventsByTopic_SendsTopicAndLimit(t *testing.T) {
	var gotPath, gotTopic, gotLimit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotTopic = r.URL.Query().Get("topic")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"events":[{"id":7,"topic":"beads.label.removed","bead_id":"gd-a b","payload":{"label":"x"},"created_at":"2026-10-01T10:00:00Z"}]}`))
	}))
	defer srv.Close()
	c := &Client{baseURL: srv.URL, httpClient: srv.Client()}

	evts, err := c.GetRecentEventsByTopic(context.Background(), "gd-a b", "beads.label.removed", 5)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/beads/gd-a%20b/events" {
		t.Errorf("path = %q, want /v1/beads/gd-a%%20b/events", gotPath)
	}
	if gotTopic != "beads.label.removed" || gotLimit != "5" {
		t.Errorf("topic, limit = %q, %q; want beads.label.removed, 5", gotTopic, gotLimit)
	}
	if len(evts) != 1 || evts[0].ID != 7 || evts[0].Topic != "beads.label.removed" {
		t.Errorf("events = %+v, want the one decoded event", evts)
	}
}
