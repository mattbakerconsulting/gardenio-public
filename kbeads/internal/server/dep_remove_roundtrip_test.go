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
// still ship the original bug (ka-0000001966).

// newRoundtripClient serves h on a real listener and returns a real HTTPClient
// pointed at it.
func newRoundtripClient(t *testing.T) (*client.HTTPClient, *mockStore) {
	t.Helper()
	_, ms, h := newTestServer()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return client.NewHTTPClient(srv.URL, ""), ms
}

// TestDepRemoveRoundtrip_MatchReportsRemoved proves a matching triple round-trips
// as removed=true through the real wire encoding.
func TestDepRemoveRoundtrip_MatchReportsRemoved(t *testing.T) {
	c, ms := newRoundtripClient(t)
	ms.beads["gd-rt1"] = &model.Bead{ID: "gd-rt1", Title: "A", Status: model.StatusOpen}
	ms.deps["gd-rt1"] = []*model.Dependency{
		{BeadID: "gd-rt1", DependsOnID: "gd-rt2", Type: model.DepBlocks},
	}

	removed, err := c.RemoveDependency(context.Background(), "gd-rt1", "gd-rt2", "blocks")
	if err != nil {
		t.Fatalf("RemoveDependency: %v", err)
	}
	if !removed {
		t.Error("removed = false over the wire for a matching edge; want true")
	}
	if got := len(ms.deps["gd-rt1"]); got != 0 {
		t.Errorf("len(deps) = %d; want the edge actually deleted", got)
	}
}

// TestDepRemoveRoundtrip_NoMatchReportsNotRemoved is the bug itself, end to end:
// the stored edge is `related`, the caller asks for `parent-child`, and the
// client must learn removed=false (previously an unconditional success) while the
// real edge survives.
func TestDepRemoveRoundtrip_NoMatchReportsNotRemoved(t *testing.T) {
	c, ms := newRoundtripClient(t)
	ms.beads["gd-rt1"] = &model.Bead{ID: "gd-rt1", Title: "A", Status: model.StatusOpen}
	ms.deps["gd-rt1"] = []*model.Dependency{
		{BeadID: "gd-rt1", DependsOnID: "gd-rt2", Type: model.DepRelated},
	}

	removed, err := c.RemoveDependency(context.Background(), "gd-rt1", "gd-rt2", "parent-child")
	if err != nil {
		t.Fatalf("a 0-match must not be a transport error: %v", err)
	}
	if removed {
		t.Error("removed = true over the wire for an edge that never existed; want false")
	}
	// The real edge survives — this is the data-corruption half of the bug.
	if got := len(ms.deps["gd-rt1"]); got != 1 {
		t.Fatalf("len(deps) = %d; want the related edge untouched", got)
	}
	if d := ms.deps["gd-rt1"][0]; d.Type != model.DepRelated {
		t.Errorf("surviving edge type = %q; want related", d.Type)
	}
	// And a no-op leaves no phantom history behind.
	if len(ms.events) != 0 {
		t.Errorf("no-op remove recorded %d event(s); want 0", len(ms.events))
	}
}
