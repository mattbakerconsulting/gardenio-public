package main

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// arms_test.go — heraldry steps 3.1 and 5.5a (gd-nLVyw2i9fN): a bead's arms
// as the emoji run, <span class="arms">, on every viewer surface that names a
// bead, read through heraldry.Parse; no picture and no /arms route.

const testArms = "blue 🦊 . 🔥 . 📦"

const testArmsRun = "🦊🔥📦"

// armBeads gives every epic the fake daemon can return the test arms.
func armBeads(f *fakeDaemon) *fakeDaemon {
	arm := func(bs []*beadsapi.BeadDetail) {
		for _, b := range bs {
			if b != nil && b.Type == "epic" {
				b.Heraldry = testArms
			}
		}
	}
	for _, b := range f.beads {
		arm([]*beadsapi.BeadDetail{b})
	}
	for _, r := range []*beadsapi.ListBeadsResult{f.hotEpicsResp, f.listResp} {
		if r != nil {
			arm(r.Beads)
		}
	}
	return f
}

func getArms(t *testing.T, mux http.Handler, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// testArmsSpan is the component for testArms in slot.
func testArmsSpan(t *testing.T, slot string) string {
	t.Helper()
	a, err := heraldry.Parse(testArms)
	if err != nil {
		t.Fatal(err)
	}
	name := template.HTMLEscapeString(a.PlainName())
	return `<span class="arms arms-` + slot + `" role="img" aria-label="` + name + `" title="` + name + `">` + testArmsRun + `</span>`
}

func TestHeraldrySpan(t *testing.T) {
	for _, tc := range []struct {
		stored string
		slot   int
	}{
		{"", 40},
		{"not arms", 40},
		{"blue 🌱 . 🔥 . 📦", 40}, // theme glyphs, not library charges
		{testArms, 0},
		{testArms, 32},
	} {
		if got := heraldrySpan(tc.stored, tc.slot); got != "" {
			t.Errorf("heraldrySpan(%q, %d) = %q, want nothing", tc.stored, tc.slot, got)
		}
	}
	for _, slot := range []int{24, 40, 64} {
		want := testArmsSpan(t, strconv.Itoa(slot))
		if got := string(heraldrySpan(testArms, slot)); got != want {
			t.Errorf("slot %d:\n got %s\nwant %s", slot, got, want)
		}
	}
}

// The viewer draws no picture of arms, so it serves none (step 5.5a).
func TestNoArmsPictureRoute(t *testing.T) {
	mux := mkServer(t, newFake())
	w := getArms(t, mux, "/arms/"+strings.Repeat("a", 64)+".png?a="+url.QueryEscape(testArms)+"&w=80", nil)
	if ct := w.Header().Get("Content-Type"); w.Code == http.StatusOK && strings.HasPrefix(ct, "image/") {
		t.Errorf("GET /arms/<key>.png served %s; the picture route is gone", ct)
	}
}

// armsPictureRE matches a picture of arms: the old component, or any <img>
// drawn from an arms route.
var armsPictureRE = regexp.MustCompile(`<img class="heraldry|<img[^>]+src="[^"]*/arms[/.]`)

// assertArms checks a page draws the arms as the run at slot, and no picture.
func assertArms(t *testing.T, where, body, slot string) {
	t.Helper()
	if !strings.Contains(body, testArmsSpan(t, slot)) {
		t.Errorf("%s: want the arms run at the %s slot, %s", where, slot, testArmsSpan(t, slot))
	}
	if armsPictureRE.MatchString(body) {
		t.Errorf("%s: the page draws a picture of the arms", where)
	}
}

// The small slots carry the arms run or nothing: the Tier-0 monogram and its
// viewer.show_plan_badges flag are gone (badges step 9, gd-5qEMPflr4z), so
// setting the retired env var draws no monogram on the plan card, the deck,
// the peek or cmd-k, and a plan without arms draws none either.
func TestArmsSlotsDrawNoMonogram(t *testing.T) {
	t.Setenv("GARDENIO_VIEWER_SHOW_PLAN_BADGES", "1")
	now := func() time.Time { return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC) }
	retired := regexp.MustCompile(`plan-badge-svg|plan-card-badge|plan-peek-badge|deck-card-badge|cmdk-plan-badge`)
	for _, tc := range []struct {
		name, target string
		seed         func() *fakeDaemon
	}{
		{"plans index card", "/plans", seedPlansFake},
		{"plan peek", "/plans/gd-plan1/peek", seedPlansFake},
		{"deck card fragment", "/plans/gd-plan1/card", seedDeckCardFatFace},
		{"deck page", "/deck", seedDeck},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain := getWith(t, mkServerWithNow(t, tc.seed(), now), tc.target)
			if retired.MatchString(plain) {
				t.Errorf("a plan without arms drew the retired monogram")
			}
			if strings.Contains(plain, `class="arms `) {
				t.Errorf("a plan without arms drew arms")
			}
			body := getWith(t, mkServerWithNow(t, armBeads(tc.seed()), now), tc.target)
			assertArms(t, tc.name, body, "40")
			if retired.MatchString(body) {
				t.Errorf("the retired monogram rendered beside the arms")
			}
		})
	}

	t.Run("cmdk", func(t *testing.T) {
		f := newFake()
		f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
			{ID: "gd-arms", Title: "Armed plan", Type: "epic", Status: "open", Labels: []string{"plan"}, Heraldry: testArms},
		}, Total: 1}
		body := getWith(t, mkServer(t, f), "/cmdk/suggest?q=armed")
		assertArms(t, "cmdk", body, "40")
		if retired.MatchString(body) {
			t.Errorf("cmdk row drew the monogram beside the arms")
		}
	})
}

// getWith drives one GET and returns the body, asserting 200.
func getWith(t *testing.T, mux http.Handler, target string) string {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: got %d, want 200", target, w.Code)
	}
	return w.Body.String()
}

func TestArmsOnBeadPageAndPlanHeader(t *testing.T) {
	mux := mkServer(t, armBeads(seedPlansFake()))

	page := getWith(t, mux, "/gd-plan1?bead=1")
	assertArms(t, "bead page", page, "64")
	h1 := regexp.MustCompile(`(?s)<h1 class="bead-title[^>]*>(.*?)</h1>`).FindStringSubmatch(page)
	if h1 == nil || !strings.Contains(h1[1], testArmsSpan(t, "64")) {
		t.Errorf("the 64 px arms are not in the bead page H1")
	}

	plan := getWith(t, mux, "/plans/gd-plan1")
	assertArms(t, "plan header", plan, "64")
}

func TestArmsInRows(t *testing.T) {
	t.Run("search", func(t *testing.T) {
		f := newFake()
		f.semanticResp = &beadsapi.SemanticSearchResponse{Results: []*beadsapi.SemanticSearchResult{
			{Bead: &beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "epic", Status: "open", Heraldry: testArms, UpdatedAt: time.Now()}, Score: 0.9},
		}}
		mux := mkServer(t, f)
		assertArms(t, "search", getWith(t, mux, "/search?q=anything"), "24")
	})

	t.Run("project row", func(t *testing.T) {
		row := projectViewFromBead(&beadsapi.BeadDetail{ID: "gd-a", Title: "A", Type: "epic", Status: "open", Heraldry: testArms}, "", "gardenio", "")
		assertArms(t, "project row", renderProjectRow(t, row), "24")
	})
}

// Only epics and projects bear arms (R6): a task whose row holds arms anyway (an older row) draws none, on the bead
// page, in search and in cmd-k (heraldry step 3.4).
func TestArmsNotOnTasks(t *testing.T) {
	task := func() *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: "gd-armtask", Title: "Armed step", Type: "task", Status: "open", Heraldry: testArms, UpdatedAt: time.Now()}
	}
	none := func(where, body string) {
		t.Helper()
		if strings.Contains(body, `class="arms `) || strings.Contains(body, testArmsRun) {
			t.Errorf("%s: a task drew arms", where)
		}
	}

	f := seedPlansFake()
	f.beads["gd-armtask"] = task()
	none("bead page", getWith(t, mkServer(t, f), "/gd-armtask?bead=1"))

	f = newFake()
	f.semanticResp = &beadsapi.SemanticSearchResponse{Results: []*beadsapi.SemanticSearchResult{{Bead: task(), Score: 0.9}}}
	none("search", getWith(t, mkServer(t, f), "/search?q=anything"))

	f = newFake()
	f.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{task()}, Total: 1}
	none("cmdk", getWith(t, mkServer(t, f), "/cmdk/suggest?q=armed"))

	none("project row", renderProjectRow(t, projectViewFromBead(task(), "", "gardenio", "")))
}

// An agent bead bears no arms; its page draws its task's line arms (the nearest armed ancestor of fields.task_id)
// before the task's link on the assignment line and on the assigned edge in Dependencies, and nothing, with no empty
// slot, when the task's line bears none (gd-ASLjQPkHJz).
func TestAgentPageShowsItsTasksLineArms(t *testing.T) {
	f := &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-agent1": {ID: "gd-agent1", Title: "crew-a", Kind: "config", Type: "agent", Status: "open", Fields: map[string]string{"role": "crew", "task_id": "gd-task1"}},
			"gd-agent2": {ID: "gd-agent2", Title: "crew-b", Kind: "config", Type: "agent", Status: "open", Fields: map[string]string{"role": "crew", "task_id": "gd-lone1"}},
			"gd-task1":  {ID: "gd-task1", Title: "the task", Type: "task", Status: "in_progress"},
			"gd-phase1": {ID: "gd-phase1", Title: "the phase", Type: "epic", Status: "open", Heraldry: testArms},
			"gd-lone1":  {ID: "gd-lone1", Title: "a loose task", Type: "task", Status: "in_progress"},
		},
		deps: map[string][]beadsapi.Dependency{
			"gd-agent1": {{BeadID: "gd-agent1", DependsOnID: "gd-task1", Type: "assigned"}},
			"gd-agent2": {{BeadID: "gd-agent2", DependsOnID: "gd-lone1", Type: "assigned"}},
			"gd-task1":  {{BeadID: "gd-task1", DependsOnID: "gd-phase1", Type: "parent-child"}},
		},
	}
	mux := mkServer(t, f)
	body := getSpawnBadgeBody(t, mux, "/gd-agent1")
	run := `<span class="heraldry-slot"><span class="arms arms-24" role="img"`
	for _, w := range []string{
		`task <span class="heraldry-slot"><span class="arms arms-24" role="img"`,
		run + ` aria-label="` + mustPlainName(t, testArms) + `"`,
	} {
		if !strings.Contains(body, w) {
			t.Errorf("agent page lacks %q", w)
		}
	}
	if n := strings.Count(body, run); n != 2 {
		t.Errorf("the run sits before the task on the assignment line and its assigned edge: %d runs, want 2", n)
	}
	if i, j := strings.Index(body, `task <span class="heraldry-slot">`), strings.Index(body, `<a href="/gd-task1"><code>gd-task1</code>`); i < 0 || j < i {
		t.Errorf("the run comes before the task's link")
	}

	body = getSpawnBadgeBody(t, mux, "/gd-agent2")
	if strings.Contains(body, `class="heraldry-slot"`) {
		t.Errorf("an agent whose task has no armed line shows no arms")
	}
	if !strings.Contains(body, `<span class="agent-task">task <a href="/gd-lone1">`) {
		t.Errorf("the assignment line keeps its shape with no arms")
	}
}

func mustPlainName(t *testing.T, stored string) string {
	t.Helper()
	a, err := heraldry.Parse(stored)
	if err != nil {
		t.Fatal(err)
	}
	return template.HTMLEscapeString(a.PlainName())
}
