package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mapsGroupCards is a small set of cards spanning three owner states: two named
// owners (one with two cards) and one un-owned card, exercising the bucketing,
// the label-sort, and the Unowned-last rule.
func mapsGroupCards() []mapsCardView {
	return []mapsCardView{
		{ID: "gd-1", Name: "alpha", OwnerSlug: "matt", OwnerDisplay: "Matt Baker"},
		{ID: "gd-2", Name: "bravo", OwnerSlug: "dana", OwnerDisplay: "Dana"},
		{ID: "gd-3", Name: "charlie", OwnerSlug: "matt", OwnerDisplay: "Matt Baker"},
		{ID: "gd-4", Name: "delta"}, // no owner → Unowned bucket
	}
}

func TestGroupMapsCardsByOwner_BucketsAndOrders(t *testing.T) {
	groups := groupMapsCardsByOwner(mapsGroupCards())

	// Three sections: Dana, Matt Baker (label-sorted), then Unowned last.
	if len(groups) != 3 {
		t.Fatalf("got %d groups, want 3: %+v", len(groups), groups)
	}
	wantOrder := []string{"Dana", "Matt Baker", mapsUnownedLabel}
	for i, want := range wantOrder {
		if groups[i].Label != want {
			t.Errorf("group[%d].Label = %q, want %q", i, groups[i].Label, want)
		}
	}

	// Matt's section holds both his cards, in input order.
	matt := groups[1]
	if matt.Count != 2 || len(matt.Cards) != 2 {
		t.Fatalf("matt count = %d, want 2", matt.Count)
	}
	if matt.Cards[0].ID != "gd-1" || matt.Cards[1].ID != "gd-3" {
		t.Errorf("matt cards order = %s,%s, want gd-1,gd-3", matt.Cards[0].ID, matt.Cards[1].ID)
	}

	// Unowned bucket carries the synthetic slug + the lone card.
	un := groups[2]
	if un.Slug != mapsUnownedSlug || un.Count != 1 || un.Cards[0].ID != "gd-4" {
		t.Errorf("unowned bucket wrong: %+v", un)
	}
}

func TestGroupMapsCardsByOwner_Empty(t *testing.T) {
	if g := groupMapsCardsByOwner(nil); g != nil {
		t.Errorf("empty input should yield nil groups, got %+v", g)
	}
}

// A card with a slug but no display label falls back to the slug as its label.
func TestMapsOwnerBucket_LabelFallback(t *testing.T) {
	slug, label := mapsOwnerBucket(mapsCardView{OwnerSlug: "solo"})
	if slug != "solo" || label != "solo" {
		t.Errorf("got (%q,%q), want (solo,solo)", slug, label)
	}
	slug, label = mapsOwnerBucket(mapsCardView{})
	if slug != mapsUnownedSlug || label != mapsUnownedLabel {
		t.Errorf("got (%q,%q), want unowned bucket", slug, label)
	}
}

func TestMapsGroupHref(t *testing.T) {
	f := parseCrossFilterDecoded("owner:matt")
	// Grouped, all-scope, with a filter → all three params present.
	href := mapsGroupHref("/base", true, f, true)
	for _, want := range []string{"/base/maps?", "all=1", "filter=", "group=owner"} {
		if !strings.Contains(href, want) {
			t.Errorf("grouped href %q missing %q", href, want)
		}
	}
	// Not grouped, no filter, live scope → bare path.
	if got := mapsGroupHref("/base", false, nil, false); got != "/base/maps" {
		t.Errorf("bare href = %q, want /base/maps", got)
	}
	// Not grouped but filtered → no group param.
	if got := mapsGroupHref("/base", false, f, false); strings.Contains(got, "group=") {
		t.Errorf("ungrouped href should omit group=: %q", got)
	}
}

// TestHandleMaps_GroupByOwnerRendersSections: ?group=owner renders the
// per-owner collapsible sections instead of one flat grid, with each gardener's
// owner name heading its section.
func TestHandleMaps_GroupByOwnerRendersSections(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)

	// Ungrouped: no owner-group sections.
	req := httptest.NewRequest("GET", "/maps?all=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), `<div class="maps-owner-groups">`) {
		t.Errorf("ungrouped board should not render owner sections")
	}

	// Grouped: owner-group sections present, and the group toggle marks By owner active.
	req = httptest.NewRequest("GET", "/maps?all=1&group=owner", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `<div class="maps-owner-groups">`) {
		t.Errorf("grouped board missing owner sections")
	}
	if !strings.Contains(body, `<summary class="maps-owner-summary">`) {
		t.Errorf("grouped board missing section summary")
	}
	// Cards still render inside the sections.
	if !strings.Contains(body, `href="/gardener/gd-deliv"`) {
		t.Errorf("grouped board dropped the delivery card")
	}
}
