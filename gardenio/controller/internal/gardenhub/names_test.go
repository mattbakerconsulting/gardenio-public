package gardenhub

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// GET /garden/names names the ids the page asks about by their short title, else their title (gd-QTukIA8Nvv): an id the
// daemon does not return is absent, a repeated or malformed id is asked once or not at all, and past namesMax the rest
// are dropped. The bead view's relations carry the short title too, so the panel shows it and keeps the title for the tip.
func TestNamesView(t *testing.T) {
	f := faceFixture()
	f.beads["gd-ShortOne01"] = &beadsapi.BeadDetail{ID: "gd-ShortOne01", Type: "task", Status: "open", Title: "P0: forward duration_ms through the relay", ShortTitle: " forward duration_ms via relay "}
	f.beads["gd-LongOnly01"] = &beadsapi.BeadDetail{ID: "gd-LongOnly01", Type: "task", Status: "open", Title: "A bead with no short title"}
	f.beads["gd-Untitled01"] = &beadsapi.BeadDetail{ID: "gd-Untitled01", Type: "task", Status: "open"}
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Beads: f}

	code, doc := getQueue(t, h, "/garden/names?id=gd-ShortOne01&id=gd-LongOnly01&id=gd-ShortOne01&id=gd-NotThere01&id=gd-Untitled01&id=..%2Fetc&id=")
	if code != http.StatusOK {
		t.Fatalf("GET /garden/names = %d", code)
	}
	want := map[string]any{"gd-ShortOne01": "forward duration_ms via relay", "gd-LongOnly01": "A bead with no short title"}
	if got := doc["names"]; !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}

	var path string
	for i := 0; i < namesMax+5; i++ {
		id := fmt.Sprintf("gd-Many%06d", i)
		f.beads[id] = &beadsapi.BeadDetail{ID: id, Type: "task", Status: "open", Title: id + " title"}
		path += "&id=" + id
	}
	if _, doc = getQueue(t, h, "/garden/names?"+path[1:]); len(doc["names"].(map[string]any)) != namesMax {
		t.Errorf("%d names, want the cap %d", len(doc["names"].(map[string]any)), namesMax)
	}

	rels := []*beadRel{{ID: "gd-ShortOne01"}, {ID: "gd-LongOnly01"}}
	hydrate(context.Background(), f, rels)
	if rels[0].ShortTitle != "forward duration_ms via relay" || rels[0].Title != f.beads["gd-ShortOne01"].Title || rels[1].ShortTitle != "" {
		t.Errorf("hydrate: %+v %+v, want the short title trimmed beside the title", *rels[0], *rels[1])
	}
	titles, _ := mentionTitles([]beadRel{*rels[0], *rels[1]}, map[string]*beadsapi.BeadDetail{})
	if want := map[string]string{"gd-ShortOne01": "forward duration_ms via relay", "gd-LongOnly01": "A bead with no short title"}; !reflect.DeepEqual(titles, want) {
		t.Errorf("mentions = %v, want the short title first", titles)
	}

	h.Beads = nil
	if code, _ := getQueue(t, h, "/garden/names?id=gd-ShortOne01"); code != http.StatusServiceUnavailable {
		t.Errorf("no reader: %d, want 503", code)
	}
}
