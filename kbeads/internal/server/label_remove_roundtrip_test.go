package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

// The tests in this file wire the REAL client.HTTPClient to the REAL HTTP
// handler over a real socket, so the rows-affected signal is proven across the
// actual wire encoding rather than by two independently-mocked halves. The store
// is the only mock. Without this, a mismatch between the JSON key the handler
// writes and the key the client decodes would pass both sides' unit tests and
// still ship the original bug (ka-0000000424).

// newLabelRoundtripClient serves h on a real listener and returns a real
// HTTPClient pointed at it.
func newLabelRoundtripClient(t *testing.T) (*client.HTTPClient, *mockStore) {
	t.Helper()
	_, ms, h := newTestServer()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client.NewHTTPClient(srv.URL, ""), ms
}

// TestLabelRemoveRoundtrip_MatchReportsRemoved proves a label the bead carries
// round-trips as removed=true through the real wire encoding.
func TestLabelRemoveRoundtrip_MatchReportsRemoved(t *testing.T) {
	c, ms := newLabelRoundtripClient(t)
	ms.beads["gd-rt1"] = &model.Bead{ID: "gd-rt1", Title: "A", Status: model.StatusOpen}
	ms.labels["gd-rt1"] = []string{"urgent"}

	removed, err := c.RemoveLabel(context.Background(), "gd-rt1", "urgent")
	if err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if !removed {
		t.Error("removed = false over the wire for a present label; want true")
	}
	if got := len(ms.labels["gd-rt1"]); got != 0 {
		t.Errorf("len(labels) = %d; want the label actually deleted", got)
	}
}

// TestLabelRemoveRoundtrip_NoMatchReportsNotRemoved is the bug itself, end to
// end: the bead carries `urgent`, the caller asks to remove a label it never
// had, and the client must learn removed=false (previously an unconditional
// success) while the real label survives.
func TestLabelRemoveRoundtrip_NoMatchReportsNotRemoved(t *testing.T) {
	c, ms := newLabelRoundtripClient(t)
	ms.beads["gd-rt1"] = &model.Bead{ID: "gd-rt1", Title: "A", Status: model.StatusOpen}
	ms.labels["gd-rt1"] = []string{"urgent"}

	removed, err := c.RemoveLabel(context.Background(), "gd-rt1", "not-a-real-label")
	if err != nil {
		t.Fatalf("a 0-match must not be a transport error: %v", err)
	}
	if removed {
		t.Error("removed = true over the wire for a label that never existed; want false")
	}
	// The real label survives.
	if got := len(ms.labels["gd-rt1"]); got != 1 || ms.labels["gd-rt1"][0] != "urgent" {
		t.Fatalf("labels = %v; want the urgent label untouched", ms.labels["gd-rt1"])
	}
	// And a no-op leaves no phantom history behind.
	if len(ms.events) != 0 {
		t.Errorf("no-op remove recorded %d event(s); want 0", len(ms.events))
	}
}
