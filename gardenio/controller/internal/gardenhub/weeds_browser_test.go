package gardenhub

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The weeds and the fog in a real Chromium (gd-ytTNc3Fu9M): on a farm with one of each weed, the thistles are on the canvas
// where the level says, every weedy plot, sign and bead names its weeds in its accessible name, the card (hover on desktop,
// tap on a phone) says why each one is a weed, a dormant project's sign says it is dormant, the orphaned PR links its closed
// bead inside the garden, and nothing leaves /garden. What is asserted lives in testdata/weeds_browser.js.
func TestWeedsUnderChromium(t *testing.T) {
	underChromium(t, "weeds_browser.js", func() *httptest.Server { return weedsWorld(t, weedsNow) }, weedsNowArg())
}

// weedsNow is the instant the weeds and tour walks run at: the farm's clock on the server and Date.now() in the browser. The
// fixture farm's beads sit at fixed times, so on the wall clock they kept ageing past the weeds these walks count: gd-seed1
// turned long-frosted on 2026-10-21, gd-A1b's wilt outweighed the orphaned PR on 2026-10-25 (gd-UkmO0q7qdq) and music and
// zeta would have gone dormant on 2026-10-28 (gd-h7219gXZyi). Frozen here, the farm is a week old and every weed's age holds.
var weedsNow = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// weedsNowArg is weedsNow for a walk's argv: unix ms, for page.clock.setFixedTime.
func weedsNowArg() string { return strconv.FormatInt(weedsNow.UnixMilli(), 10) }

// weedsWorld serves /garden on the fixture farm plus one of each weed, its times relative to now: a Commons plan with no
// project label and no phases whose one step is wilted, a loose bead with no label, a draft untouched for ten days, a PR
// still open after its bead closed, and a project, attic, with nothing changed for twenty days. extra routes go on the same
// mux (the chore tour's stand-ins for the viewer's write endpoints).
func weedsWorld(t *testing.T, now time.Time, extra ...func(*http.ServeMux)) *httptest.Server {
	t.Helper()
	src := fixtureWorld()
	at := func(b *beadsapi.BeadDetail, ago time.Duration) *beadsapi.BeadDetail {
		b.UpdatedAt = now.Add(-ago)
		if b.CreatedAt.After(b.UpdatedAt) {
			b.CreatedAt = b.UpdatedAt
		}
		return b
	}
	commons := at(planBead("gd-weedP", "building", "in_progress"), 20*time.Hour)
	commons.Title, commons.Labels = "Tidy the shed", []string{"plan"}
	step := at(fb("gd-weedP1", "task", "in_progress", "gd-weedP", 0), 20*time.Hour)
	step.Title, step.Labels, step.Assignee = "Sort the seed tins", nil, "flo"
	loose := at(fb("gd-weedM", "task", "open", "", 0), time.Hour)
	loose.Title, loose.Labels = "A bead nobody labelled", nil
	seed := at(planBead("gd-weedS", "draft", "open"), 10*24*time.Hour)
	seed.Title = "Greenhouse heating"
	gone := at(fb("gd-orph", "task", "closed", "", 0), 3*24*time.Hour)
	gone.Title, gone.Assignee, gone.Fields["mr_url"] = "Fix the gate latch", "rae", "https://github.com/o/r/pull/77"
	pr := at(prBead("gd-orphMR", "https://github.com/o/r/pull/77", "open", 0), 2*24*time.Hour)
	pr.Title = "Fix the gate latch"
	src.add(commons, step, loose, seed, gone, pr)
	attic := at(planBead("gd-attic", "building", "in_progress", "gd-attic1"), 20*24*time.Hour)
	attic.Title = "Clear out the attic"
	phase := at(fb("gd-attic1", "epic", "open", "gd-attic", 0), 20*24*time.Hour)
	box := at(fb("gd-attic1a", "task", "open", "gd-attic1", 0), 20*24*time.Hour)
	box.Title = "Label the boxes"
	for _, b := range []*beadsapi.BeadDetail{attic, phase, box} {
		b.Labels[0] = "project:attic"
	}
	src.add(attic, phase, box)

	hubSrc := newFake()
	hubSrc.events = src.events
	f := NewFarm(src, nil)
	f.now = func() time.Time { return now }
	h := New(hubSrc, "", nil)
	h.OnEvent(f.InvalidateFor)
	h.Start()
	t.Cleanup(h.Close)
	f.Start()
	t.Cleanup(f.Close)
	beads := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{}, comments: map[string][]beadsapi.Comment{}}
	for _, b := range src.beads {
		beads.beads[b.ID] = b
	}
	mux := http.NewServeMux()
	page := &Handler{Hub: h, Farm: f, Beads: beads}
	page.Register(mux)
	mountCommons(mux, commonsWorld(now), page, nil, nil) // the page's Commons pane, as in cardWorld
	for _, route := range extra {
		route(mux)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}
