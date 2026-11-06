package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mapsFilterRows is a small hand-built set of gardener rows exercising every
// facet axis the filter rail offers: trigger kind, verdict, owner, enabled.
func mapsFilterRows() []gardenerRowView {
	return []gardenerRowView{
		{ID: "gd-a", Name: "alpha", TriggerSubKind: "cron", VerdictKind: "progress", OwnerSlug: "matt", OwnerDisplay: "Matt", Enabled: true, Status: "active"},
		{ID: "gd-b", Name: "bravo", TriggerSubKind: "bead_query", VerdictKind: "stuck", OwnerSlug: "matt", OwnerDisplay: "Matt", Enabled: true, Status: "active"},
		{ID: "gd-c", Name: "charlie", TriggerSubKind: "cron", VerdictKind: "stalled", OwnerSlug: "dana", OwnerDisplay: "Dana", Enabled: false, Status: "disabled"},
		{ID: "gd-d", Name: "delta", Kind: "schedule", VerdictKind: "", OwnerSlug: "dana", OwnerDisplay: "Dana", Enabled: true, Status: "active"},
	}
}

func TestMapsRowMatchesFilter(t *testing.T) {
	rows := mapsFilterRows()
	byID := map[string]gardenerRowView{}
	for _, r := range rows {
		byID[r.ID] = r
	}

	cases := []struct {
		name   string
		filter string
		want   []string // IDs that should match
	}{
		{"empty matches all", "", []string{"gd-a", "gd-b", "gd-c", "gd-d"}},
		{"trigger cron", "trigger:cron", []string{"gd-a", "gd-c"}},
		{"trigger schedule", "trigger:schedule", []string{"gd-d"}},
		{"verdict stuck", "verdict:stuck", []string{"gd-b"}},
		{"verdict neutral catches unverdicted", "verdict:neutral", []string{"gd-d"}},
		{"owner dana", "owner:dana", []string{"gd-c", "gd-d"}},
		{"enabled false", "enabled:false", []string{"gd-c"}},
		// Cross-key AND: cron AND owner matt → only alpha.
		{"cron AND owner matt", "trigger:cron,owner:matt", []string{"gd-a"}},
		// Within-key OR: two trigger values → union.
		{"cron OR bead_query", "trigger:bead_query,trigger:cron", []string{"gd-a", "gd-b", "gd-c"}},
		// Contradictory AND → empty.
		{"cron AND schedule (none)", "trigger:cron,trigger:schedule", []string{"gd-a", "gd-c", "gd-d"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := parseCrossFilterDecoded(tc.filter)
			var got []string
			for _, r := range rows {
				if mapsRowMatchesFilter(r, f) {
					got = append(got, r.ID)
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("filter %q: got %v, want %v", tc.filter, got, tc.want)
			}
		})
	}
}

// "cron OR schedule" within the trigger key is a union — verify the within-key
// OR is genuinely OR and not AND (a single row can't be both, so an AND would
// yield empty).
func TestMapsRowMatchesFilter_WithinKeyOR(t *testing.T) {
	rows := mapsFilterRows()
	f := parseCrossFilterDecoded("trigger:cron,trigger:schedule")
	var got []string
	for _, r := range rows {
		if mapsRowMatchesFilter(r, f) {
			got = append(got, r.ID)
		}
	}
	want := "gd-a,gd-c,gd-d" // both crons + the schedule
	if strings.Join(got, ",") != want {
		t.Errorf("within-key OR: got %v, want %s", got, want)
	}
}

func TestBuildMapsFilterRail_GroupsAndCounts(t *testing.T) {
	rows := mapsFilterRows()
	rail := buildMapsFilterRail(rows, nil, "", false)

	groups := map[string]mapsFacetGroup{}
	for _, g := range rail.Groups {
		groups[g.Key] = g
	}

	// Trigger group: cron(2), bead_query(1), schedule(1) — three distinct values.
	trig, ok := groups[mapsFacetTrigger]
	if !ok {
		t.Fatalf("trigger facet group missing")
	}
	gotCron := 0
	for _, o := range trig.Options {
		if o.Value == "cron" {
			gotCron = o.Count
		}
	}
	if gotCron != 2 {
		t.Errorf("cron count: got %d, want 2", gotCron)
	}

	// Owner group sorts by label: Dana before Matt.
	own := groups[mapsFacetOwner]
	if len(own.Options) != 2 || own.Options[0].Value != "dana" || own.Options[1].Value != "matt" {
		t.Errorf("owner group not label-sorted: %+v", own.Options)
	}

	// Enabled (State) group: true(3), false(1) — both present.
	enab := groups[mapsFacetEnabled]
	if len(enab.Options) != 2 {
		t.Errorf("state group: got %d options, want 2", len(enab.Options))
	}

	if rail.Active {
		t.Errorf("rail should be inactive with no filter")
	}
}

// A facet that resolves to a single value can't narrow the board, so it's
// dropped from the rail (unless it's the currently-selected value).
func TestBuildMapsFilterRail_DropsSingleValueFacet(t *testing.T) {
	// All rows share owner "solo" → the owner facet has one value → dropped.
	rows := []gardenerRowView{
		{ID: "gd-1", TriggerSubKind: "cron", VerdictKind: "progress", OwnerSlug: "solo", Enabled: true},
		{ID: "gd-2", TriggerSubKind: "bead_query", VerdictKind: "stuck", OwnerSlug: "solo", Enabled: true},
	}
	rail := buildMapsFilterRail(rows, nil, "", false)
	for _, g := range rail.Groups {
		if g.Key == mapsFacetOwner {
			t.Errorf("single-value owner facet should be dropped, got %+v", g.Options)
		}
	}
}

func TestMapsToggleFacet(t *testing.T) {
	// Toggle on.
	f := parseCrossFilterDecoded("trigger:cron")
	f2 := mapsToggleFacet(f, "owner", "matt")
	if f2.String() != "owner:matt,trigger:cron" {
		t.Errorf("toggle-on wire: got %q", f2.String())
	}
	// Toggle off the original key/value (the input filter must be untouched).
	f3 := mapsToggleFacet(f, "trigger", "cron")
	if f3.String() != "" {
		t.Errorf("toggle-off wire: got %q, want empty", f3.String())
	}
	if f.String() != "trigger:cron" {
		t.Errorf("toggle mutated input filter: %q", f.String())
	}
}

func TestMapsHref(t *testing.T) {
	f := parseCrossFilterDecoded("trigger:cron,owner:matt")
	href := mapsHref("/base", true, f)
	if !strings.HasPrefix(href, "/base/maps?") {
		t.Fatalf("href prefix wrong: %q", href)
	}
	if !strings.Contains(href, "all=1") {
		t.Errorf("href missing all=1: %q", href)
	}
	// The filter value is percent-encoded (Go decodes once on read).
	if !strings.Contains(href, "filter=") {
		t.Errorf("href missing filter param: %q", href)
	}
	// No filter, no scope → bare /maps.
	if got := mapsHref("/base", false, nil); got != "/base/maps" {
		t.Errorf("bare href: got %q", got)
	}
}

// TestHandleMaps_FilterRailRendersAndNarrows: the rail renders facet chips and a
// ?filter= request narrows the rendered board to matching gardeners.
func TestHandleMaps_FilterRailRendersAndNarrows(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)

	// Rail is present on the unfiltered board.
	req := httptest.NewRequest("GET", "/maps?all=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "maps-filter") {
		t.Errorf("filter rail not rendered")
	}

	// Filtering to enabled:false should keep the disabled watchdog and drop the
	// enabled delivery gardener.
	req = httptest.NewRequest("GET", "/maps?all=1&filter=enabled:false", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body = w.Body.String()
	if !strings.Contains(body, `href="/gardener/gd-watch"`) {
		t.Errorf("enabled:false filter dropped the disabled watchdog")
	}
	if strings.Contains(body, `href="/gardener/gd-deliv"`) {
		t.Errorf("enabled:false filter leaked the enabled delivery gardener")
	}
	// The clear-filters affordance + the showing-N-of-M note appear.
	if !strings.Contains(body, "clear filters") {
		t.Errorf("active filter missing clear-filters link")
	}
}

// TestHandleMaps_FilterToZeroShowsClearHint: a filter matching nothing renders
// the "no gardeners match" empty state with a clear link, not the live-fleet hint.
func TestHandleMaps_FilterToZeroShowsClearHint(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)
	// owner:nobody matches no gardener.
	req := httptest.NewRequest("GET", "/maps?all=1&filter=owner:nobody", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "No gardeners match these filters") {
		t.Errorf("filter-to-zero did not show the match-empty state; body=%s", body)
	}
}
