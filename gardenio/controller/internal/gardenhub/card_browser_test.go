package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons"
	"gardenio/controller/internal/commons/commonstest"
)

// The cursor-aimed text and the info card (gd-D3G9dMA1TU) in a real browser: Chromium under playwright-core hovers and
// focuses a plot on a desktop (the mouse opens no card since gd-D1QRStvmCh; focus does) and taps one on a Pixel 5, against /garden on an httptest server fed the fixture farm plus three
// long-titled plans so Project-level labels collide. It then walks the in-garden bead view (gd-BdhucNqv0i): a click opens
// the panel, ?bead= reopens it on reload, a cross-reference opens another bead, Back returns, and no request ever leaves
// /garden, and on a phone a bead opened from the Mailbox or the Porch shows the farm and closing it goes back (gd-e3LWfAZczq).
// A nine-phase plan checks that every phase row and the plan title carry a label on a plate at the Plan and Bead
// levels, desktop and phone, light and dark (gd-kIUS2y1z7I). An ask answered in the bead view leaves the screen and the
// next one takes its place, and the Mailbox row goes with it (gd-LEfzdQl057). What is asserted lives in
// testdata/world_card_browser.js.
func TestCardUnderChromium(t *testing.T) {
	underChromium(t, "world_card_browser.js", func() *httptest.Server {
		// One open decision in the Mailbox, for the phone walk from the Mailbox into the bead view and back (gd-e3LWfAZczq).
		mail := bead("gd-mailD1", "decision", 0)
		mail.Title, mail.Status, mail.UpdatedAt = "Ship the mailbox fix?", "open", time.Now().Add(-5*time.Minute)
		mail.Fields = map[string]string{"prompt": "Ship the mailbox fix?", "options": `[{"id":"y","label":"Yes"},{"id":"n","label":"No"}]`}
		// A second, older one behind it: the one the bead view moves on to once the first is answered there (gd-LEfzdQl057).
		next := bead("gd-mailD2", "decision", 0)
		next.Title, next.Status, next.UpdatedAt = "Second question?", "open", time.Now().Add(-10*time.Minute)
		next.Fields = map[string]string{"prompt": "Second question?", "options": `[{"id":"y","label":"Yes"},{"id":"n","label":"No"}]`}
		return cardWorld(t, time.Now, mail, next)
	})
}

// The map-window drawers scroll inside themselves (gd-8q27r5vXYv): with a dozen agents on the Porch and a dozen decisions
// in the Mailbox, the dock's panes and the Plans body each end inside their open drawer and the wheel scrolls the last item
// into view, at 1707x842; on a phone the Porch / Mailbox screen scrolls the same way. What is asserted lives in
// testdata/drawer_scroll_browser.js.
func TestDrawersScrollUnderChromium(t *testing.T) {
	underChromium(t, "drawer_scroll_browser.js", func() *httptest.Server {
		var porch []*beadsapi.BeadDetail
		for i := 0; i < 12; i++ {
			n := strconv.Itoa(i + 1)
			a := bead("a-"+n, "agent", int64(100+i))
			a.Title, a.Fields = "gardener-"+n, map[string]string{"agent_state": "working"}
			d := bead("d-"+n, "decision", int64(200+i))
			d.Status = "open"
			d.Fields = map[string]string{"prompt": "Question " + n + ": ship the change now, or wait for the next check-in walk with the owner?",
				"options": `[{"id":"a","short":"Now","label":"Ship now"},{"id":"b","short":"Wait","label":"Wait for the walk"}]`}
			porch = append(porch, a, d)
		}
		return cardWorld(t, time.Now, porch...)
	})
}

// underChromium runs testdata/<script> under node against the server srv starts, args after its base URL and node_modules
// dir (a walk on a frozen farm takes its clock there, as weedsNowArg). It needs node, playwright-core
// (gardenio/tests/gardenhub-browser/node_modules, or GARDENHUB_PW_MODULES) and a Chromium it can launch
// (PLAYWRIGHT_BROWSERS_PATH or ~/.cache/ms-playwright); `make test-gardenhub-browser` installs both and runs these tests.
// Without them the test skips, so the ordinary package run stays hermetic -- unless GARDENHUB_BROWSER_REQUIRED=1, which
// CI's gardenhub-browser job sets so a missing browser fails instead of going green on a skip (gd-DWQGQx9iep).
func underChromium(t *testing.T, script string, srv func() *httptest.Server, args ...string) {
	t.Helper()
	skip := t.Skipf
	if os.Getenv("GARDENHUB_BROWSER_REQUIRED") == "1" {
		skip = t.Fatalf
	}
	node, err := exec.LookPath("node")
	if err != nil {
		skip("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		skip("playwright-core not installed under %s (make test-gardenhub-browser)", mods)
	}

	base := srv().URL
	path, err := filepath.Abs(filepath.Join("testdata", script))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, append([]string{path, base, mods}, args...)...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
		skip("no Chromium for playwright-core: %s", strings.TrimSpace(string(out)))
	}
	if err != nil {
		t.Fatalf("browser checks failed: %v\n%s", err, out)
	}
	t.Logf("%s", strings.TrimSpace(string(out)))
}

// cardWorld serves /garden on an httptest server fed the fixture farm plus three long-titled plans, so Project-level labels
// collide, and three more projects of five plans each. now is the farm's clock: TestCardUnderChromium passes time.Now, the
// goldens (golden_browser_test.go) a fixed instant so every relative time renders the same on every run.
func cardWorld(t *testing.T, now func() time.Time, porch ...*beadsapi.BeadDetail) *httptest.Server {
	t.Helper()
	return cardWorldAs(t, now, "", nil, porch...)
}

// cardWorldAs is cardWorld seen by viewer (an operator alias; "" is nobody signed in), with edit applied to the farm's beads
// before the farm starts (nil: none).
func cardWorldAs(t *testing.T, now func() time.Time, viewer string, edit func(*fakeFarmSrc), porch ...*beadsapi.BeadDetail) *httptest.Server {
	t.Helper()
	return cardWorldCut(t, now, viewer, nil, edit, porch...)
}

// cardWorldCut is cardWorldAs with the farm document cut to cut's projects (farm_cut.go): a project-scoped viewer's /garden.
// A nil cut is the whole farm.
func cardWorldCut(t *testing.T, now func() time.Time, viewer string, cut []string, edit func(*fakeFarmSrc), porch ...*beadsapi.BeadDetail) *httptest.Server {
	t.Helper()
	srv, _ := cardWorldRig(t, now, viewer, cut, edit, false, porch...)
	return srv
}

// cardRig is what cardWorldRig serves the page from, for a harness that moves the farm while a page watches it
// (netinv_browser_test.go): the mux to add routes to, the page's Handler (its hooks are read per request, so a harness sets
// one before the first), the farm's beads and the Farm, the Hub's daemon stand-in, the bead view's reader, the costs rollup,
// and the Commons land with its daemon stream (nil unless the Commons is live).
type cardRig struct {
	mux           *http.ServeMux
	page          *Handler
	src           *fakeFarmSrc
	farm          *Farm
	spend         *fakeSpend
	hub           *fakeSource
	beads         *fakeBeads
	commons       *commonstest.Daemon
	commonsEvents chan beadsapi.SSEEvent
}

// cardWorldRig is cardWorldCut and its parts. liveCommons gives the Commons a hub fed by rig.commonsEvents, so a message
// put on that stream reaches an open pane's event stream; the goldens' Commons has none.
func cardWorldRig(t *testing.T, now func() time.Time, viewer string, cut []string, edit func(*fakeFarmSrc), liveCommons bool, porch ...*beadsapi.BeadDetail) (*httptest.Server, *cardRig) {
	t.Helper()
	src := fixtureWorld()
	for i, title := range []string{
		"Second plan with a title long enough to cross its neighbours",
		"Third plan, also with a long title that would overlap",
		"Fourth plan whose title is likewise far wider than its plot",
	} {
		id := "gd-plan" + string(rune('B'+i))
		p := planBead(id, "building", "in_progress", id+"1")
		p.Title = title
		step := fb(id+"1a", "task", "in_progress", id+"1", 700)
		step.Assignee = "flo"
		src.add(p, fb(id+"1", "epic", "in_progress", id, 40), step)
	}
	// Crew spend for the Plans panel's spend lines (gd-BL2JQbsfyy), beside plan A's (fixtureWorld): plan B's crew finished and
	// its figure is settled; plan C's is still moving, last changed two hours ago, and is not stale, because plan A's stamp
	// five minutes ago says the rollup still runs (planspend.ReadWithPass). Plan D and the rest were never rolled up and show
	// a dash.
	src.mutate("gd-planB", func(b *beadsapi.BeadDetail) { setPlanSpend(b, "3.1000", now().Add(-8*time.Minute).Unix(), "1", "0") })
	src.mutate("gd-planC", func(b *beadsapi.BeadDetail) { setPlanSpend(b, "6.8000", now().Add(-2*time.Hour).Unix(), "2", "1") })
	// Plan B's only open item is an escalation the owner already approved (gd-TEl9BfDL2z): it waits on the agent that asked,
	// named by its agent bead id, and must not read as "needs you".
	esc := fb("gd-planB1e", "escalation", "in_progress", "gd-planB1", 710)
	esc.Fields["approved_at"], esc.Fields["requesting_agent"] = now().Add(-2*time.Hour).UTC().Format(time.RFC3339), "gd-agentBeadW"
	agent := fb("gd-agentBeadW", "agent", "open", "", 1)
	agent.Fields["agent"] = "gardenio-ingarden-bead-view-cd1"
	src.add(esc, agent)
	// A plan with nine phases, some named far wider than a soil row, so the Plan level has a full plot of phase rows to
	// name (gd-kIUS2y1z7I).
	var order []string
	for i, title := range []string{
		"0. Ground rules", "1a. Looking glass: golden screenshots gate every pull request", "1b. Full-screen farm",
		"1c. Check-in", "2. Commons: one conversation model for every bead, the porch and the mailbox", "3. Lands",
		"4. Seasons", "5. Harvest", "6. The closing walk-through with the owner at every device",
	} {
		id := "gd-planP" + string(rune('a'+i))
		ph, step := fb(id, "epic", "in_progress", "gd-planP", 40), fb(id+"1", "task", "open", id, int64(500+i))
		ph.Title, step.Title = title, "Step of "+title
		if i == 0 { // the first phase's stake and its step say their arms in words; neither draws a picture (Q16, R6)
			ph.Heraldry, step.Heraldry = goldenPhaseArms, goldenTaskArms
		}
		order = append(order, id)
		src.add(ph, step)
	}
	pp := planBead("gd-planP", "building", "in_progress", order...)
	pp.Title = "Homestead World: a large pan-and-zoom farm"
	pp.Heraldry = goldenPlanArms // the 24 px picture at its plot gate (heraldry step 4.4): the plan and armsgate goldens
	src.add(pp)
	// Two more plots for the rollup stamp at the project depth (cursor-grid spec 6.6): a plan with every phase done, and one
	// whose only phase has no tasks yet.
	pe := planBead("gd-planE", "building", "in_progress", "gd-planE1", "gd-planE2")
	pe.Title = "Harvested plan with every phase done"
	src.add(pe, fb("gd-planE1", "epic", "closed", "gd-planE", 30), fb("gd-planE1a", "task", "closed", "gd-planE1", 31),
		fb("gd-planE2", "epic", "closed", "gd-planE", 32), fb("gd-planE2a", "task", "closed", "gd-planE2", 33), fb("gd-planE2b", "task", "closed", "gd-planE2", 34))
	pf := planBead("gd-planF", "building", "open", "gd-planF1")
	pf.Title = "Fresh plan with no tasks yet"
	src.add(pf, fb("gd-planF1", "epic", "open", "gd-planF", 35))
	// Three more projects of five plans each, so the opening zoom-to-fit lands at the Project level with plots about as
	// small on screen as on the live farm (gd-N0AEWzgVsg).
	for _, pj := range []string{"kbeads", "coop", "music"} {
		for i := 0; i < 5; i++ {
			id := "gd-" + pj + string(rune('a'+i))
			p := planBead(id, "building", "in_progress", id+"1")
			p.Title = "The " + pj + " plan number " + string(rune('1'+i)) + " with a title wider than its plot"
			phase, step := fb(id+"1", "epic", "in_progress", id, 40), fb(id+"1a", "task", "open", id+"1", 600)
			for _, b := range []*beadsapi.BeadDetail{p, phase, step} {
				b.Labels[0] = "project:" + pj
			}
			src.add(p, phase, step)
		}
	}
	src.add(fixtureGardeners(now())...)
	// music's steward (Lands 9.8): its trigger, its project bead for the cost row it rides on, and its memory, whose first In
	// flight line is what the steward by music's sign says on hover and tap (the steward golden).
	src.add(projectBead("gd-projMusic", "music", map[string]string{}),
		stewardTrigger("gd-stwMusic", "music", `{"max_runs_per_day":24}`),
		notesBead("gd-ntsMusic", "music", "## Holds\n\n## Answers (newest first)\n\n## Promises\n- digest in #general at 18:00\n\n## In flight\n- The music plan number 3: a crew is on step 1; its PR is open for review\n"))
	// A visible harvest cell (harvest spec 4): gd-A1y, the fixture's merged step, is under the late gardener, so gd-A1x gets
	// a UX grade, green checks, a test and two review rounds (no MR, so the produce bin is unchanged). A sheaf with the
	// rosette and the vial at 1x, plus the lantern and the seal at 2x+, the rosette's 4 and the seal's 2 on a desktop.
	for _, b := range src.beads {
		if b.ID == "gd-A1x" {
			b.ClosedAt = time.Unix(baseUnix+20, 0).UTC().Format(time.RFC3339)
			b.Fields["ci_state"], b.Fields["tests_touched"], b.Fields["review_rounds"] = "success", "1", "2"
		}
	}
	grade := fb("gd-Axev", "evidence", "closed", "gd-A1x", 23)
	grade.Title = "UX grade 4/5: exchange login"
	grade.Fields["kind"], grade.Fields["step"], grade.Fields["subject"] = "ux", "gd-A1x", "exchange login"
	grade.Fields["score"], grade.Fields["of"], grade.Fields["percent"] = "4", "5", "80"
	src.add(grade)
	src.add(releasesPlan()...)
	if edit != nil {
		edit(src)
	}
	// porch is what the Hub lists (agents for the Porch, decisions for the Mailbox); the card walk passes none.
	hubSrc := newFake()
	hubSrc.events = src.events
	var active, decisions []*beadsapi.BeadDetail
	for _, b := range porch {
		if b.Type == "decision" {
			decisions = append(decisions, b)
		} else {
			active = append(active, b)
		}
	}
	hubSrc.set(active, decisions)
	src.add(active...) // the farm reads the same agents (one daemon in production): the Porch and the home strips draw from its gardeners[]
	f := NewFarm(src, nil)
	f.now = now
	spend := &fakeSpend{byWin: map[time.Duration]map[string]float64{}}
	f.SetSpendSource(spend) // the cost rows, which carry the stewards
	f.SetReleaseSource(src) // the fixture's releases, for the truck (farm_releases.go)
	// The bead view (gd-BdhucNqv0i) reads the same beads: parent-child links from each bead's parent, and a description
	// and comments on plan A for the panel to show.
	beads := &fakeBeads{beads: map[string]*beadsapi.BeadDetail{}, comments: map[string][]beadsapi.Comment{}}
	for _, b := range src.beads {
		beads.beads[b.ID] = b
		if b.ParentID != "" {
			beads.deps = append(beads.deps, beadsapi.Dependency{BeadID: b.ID, DependsOnID: b.ParentID, Type: "parent-child"})
		}
	}
	for _, ds := range src.deps { // the farm's blocks edges, so a step's bead view lists what it waits on (gd-CNhEh30Ykf)
		for _, d := range ds {
			if d.Type == "blocks" {
				beads.deps = append(beads.deps, d)
			}
		}
	}
	for _, b := range porch { // a Porch or Mailbox bead opens in the bead view too
		beads.beads[b.ID] = b
	}
	beads.beads["gd-planA"].Description = "Roll the edge IdP out.\nSecond line."
	beads.comments["gd-planA"] = []beadsapi.Comment{{Author: "mo", Text: "Phase one is in. Merged in https://github.com/acme/edge/pull/42; gd-EdgeIdP002 is next.", CreatedAt: time.Unix(1_790_000_000, 0)}}
	stressBead(beads)
	harvestStep(beads, now()) // a closed step's Harvest section (bead_harvest_test.go), on no plot like the stress bead
	faceBeads(beads, now())   // one bead per typed face (bead_face_test.go), on no plot like the stress bead
	faceStressBeads(beads, now())
	releaseBeads(beads, now()) // the release face's release and the one before it (bead_face_release_test.go)
	reportBeads(beads, now())  // a screenshot report (bead_face_test.go), for the report surface
	workbenchNamed(beads)      // the bead the workbench names through GET /garden/names (workbench_world_test.go)
	wish := wishFamily(now())  // the Slack wish and its kin, which the Commons reads too (commons_world_test.go)
	for _, b := range wish.Beads {
		beads.beads[b.ID] = b
	}
	beads.deps = append(beads.deps, wish.Deps...)
	maps.Copy(beads.comments, wish.Comments)
	// the #crew card's agent (commons_world_test.go), which the phone opens in the bead view over the Commons sheet (M4)
	beads.beads["gd-agentUnread"] = &beadsapi.BeadDetail{ID: "gd-agentUnread", Type: "agent", Title: "gardenio-45-read-markers", Status: "closed",
		Description: "Read markers for the Commons: a channel on screen is read up to its newest entry."}
	// The Mailbox decision's --evidence task (goldenPorch, put on the reader by faceBeads) reaches its row through the Hub's
	// bead and comment reads: the bead view's reader here, as the one daemon in production.
	h := New(evidenceSource{hubSrc, beads}, "", nil)
	h.OnEvent(f.InvalidateFor)
	h.Start()
	t.Cleanup(h.Close)
	f.Start()
	t.Cleanup(f.Close)
	fixtureTranscript(f, "gd-agentBeadW", now())                  // its transcript, for the transcript surface (transcript_fixture_test.go)
	fixturePlotCalls(f, []string{"gd-gAskB", "gd-gDownB"}, now()) // plot B's, for the transcriptplot surface

	mux := http.NewServeMux()
	// Meadow adoption (adopt.go): the bead view offers it, and the link it writes is one the next read of the bead sees.
	links := &adoptFake{fakeBeads: beads}
	// The bead view and the Workbench readers answer X-UX-Fixture's stress, empty and down worlds too (ux_fixture_test.go).
	page := &Handler{Hub: h, Farm: f, Beads: uxBeads{links}, Links: links, CanAdopt: func(*http.Request) bool { return true },
		Queues: uxQueues{fixtureQueues(now())}, Search: uxSearch{fixtureSearch(now())}, Bundles: uxBundles{fixtureBundles(now())}, Plans: uxPlans{fixturePlans(now())},
		Projects: uxProjects{fixtureProjects(now())}, Gardeners: uxGardeners{fixtureGardenerPanels(now())}, Settings: uxSettings{fixtureSettings()}, CanOperateGardeners: func(*http.Request) bool { return true },
		CanEdit: uxWriter, MaySpawn: func(r *http.Request, _ string) bool { return uxWriter(r) }, CanWake: uxWaker,
		CanSeeCosts: func(*http.Request) bool { return true }, // the Costs panel's openers (plan gd-gh5J0vVbJP step 2.3)
		IsAdmin:     func(*http.Request) bool { return true }, // Become beside a tenant (web/become.js, plan gd-0SUIdKXWV3 step 3)
		BecomeURL:   "https://auth.example/gardenio/become/",
		// a Become session's banner and its Stop (plan gd-0SUIdKXWV3 step 4), for a request carrying X-UX-Impersonator
		Impersonator: uxImpersonator, StopURL: "https://gardenio.example/oauth2/sign_out",
		Glass: withFixturePool(fixtureGlass(t, beads), now)} // the Glass (glass_panel_test.go): a recorded session, one whose stream is unavailable, and the credential pool
	if viewer != "" {
		page.Viewer = func(*http.Request) string { return viewer }
	} else {
		page.Viewer = func(r *http.Request) string { // an impersonated session is a signed-in tenant's
			if uxImpersonator(r) != "" {
				return uxTenant
			}
			return ""
		}
	}
	if cut != nil {
		page.Cut = func(*http.Request) FarmCut { return FarmCut{Scoped: true, Projects: cut} }
	}
	page.Register(mux)
	for _, id := range glassFixtureAgents {
		mux.HandleFunc("GET /garden/farm/activity/"+id, glassActivity) // the Glass's transcript section (glass_panel_test.go)
	}
	page.Live = mux                                            // every stream rides the page's one connection, as in beads-viewer (livemux.go)
	mux.HandleFunc("POST /garden/bead/{id}/adopt", page.Adopt) // beads-viewer mounts it behind its operator gate
	mux.HandleFunc("GET /image/{key...}", fixtureImage)        // the viewer's image proxy, for a grade's pictures
	// "Put an agent on this" (beadwrite.js spawner) fills its form from the viewer's GET /quick-spawn-inline as JSON
	// (cmd/beads-viewer quickSpawnForm); only a writer (uxWriter) is offered it.
	mux.HandleFunc("GET /quick-spawn-inline", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"project": r.URL.Query().Get("project"), "task_id": r.URL.Query().Get("task"),
			"role": "crew", "roles": []string{"crew", "gardener", "thread"}, "models": []string{"opus", "sonnet", "haiku"},
			"prompt": "Work on " + r.URL.Query().Get("task") + ": read it, claim it, land it."})
	})
	// The comment composer's Preview posts the draft to the viewer's /b/{id}/comments/preview (beadwrite.js draft()) and shows
	// the HTML it answers: here each paragraph of the draft escaped, inside the .comment-body the viewer's render wraps it in.
	mux.HandleFunc("POST /b/{id}/comments/preview", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		var b strings.Builder
		for _, p := range strings.Split(strings.TrimSpace(r.PostForm.Get("text")), "\n\n") {
			b.WriteString("<p>" + html.EscapeString(p) + "</p>")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<div class="comment-body">` + b.String() + `</div>`))
	})
	// The in-place answer posts to the viewer's /b/{id}/decision/resolve (garden.js act()). Here it closes the decision the
	// way the daemon does: the Hub's next read no longer lists it, so the stream removes its row (gd-LEfzdQl057).
	mux.HandleFunc("POST /b/{id}/decision/resolve", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		hubSrc.mu.Lock()
		kept := hubSrc.decision[:0:0]
		for _, d := range hubSrc.decision {
			if d.ID != id {
				kept = append(kept, d)
			}
		}
		hubSrc.decision = kept
		hubSrc.mu.Unlock()
		// A seed's plan decision (9.11b, pot_browser.js): the farm's copy closes too, and an approve moves the plan to
		// building as the steward's answer does (steward.AnswerPlan), so the pot leaves the Nursery for a plot.
		_ = r.ParseForm()
		plan := ""
		src.mutate(id, func(b *beadsapi.BeadDetail) {
			if b.Type == "decision" && b.Fields["requesting_plan"] != "" {
				b.Status, plan = "closed", b.Fields["requesting_plan"]
			}
		})
		if plan != "" {
			if r.PostForm.Get("option_id") == "approve" {
				src.mutate(plan, func(b *beadsapi.BeadDetail) { b.Status, b.Fields["plan_state"] = "in_progress", "building" })
			}
			f.Invalidate()
		}
		go func() { hubSrc.events <- beadsapi.SSEEvent{Event: "bead.closed"} }()
		w.WriteHeader(http.StatusOK)
	})
	// The project panel's config editor posts to the viewer's POST /p/{project}/config asked for JSON (project_config.go):
	// here a cpu_request that is no Kubernetes quantity is refused on its field, as the route refuses it, and the rest saves.
	mux.HandleFunc("POST /p/{project}/config", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if q := r.PostForm.Get("cpu_request"); q == "lots" {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"ok":false,"error":"Fix the fields marked.","status":422,"fields":{"cpu_request":"\"lots\" is not a valid Kubernetes quantity (e.g. 500m, 2Gi)."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"bead":"gd-ProjGarden1"}`))
	})
	// The project panel's Share tab (LW.5) reads and posts the viewer's /p/{project}/share (land_share.go): one live land
	// window and a revoked one; a mint shows its link once, a revoke says so.
	share := shareStub{}
	mux.HandleFunc("GET /p/{project}/share", share.serve)
	mux.HandleFunc("POST /p/{project}/share", share.serve)
	mux.HandleFunc("POST /p/{project}/share/{window}/revoke", share.serve)
	// The gardener panel's levers, config editor and successor form post to the viewer's /gardener/{id}/* asked for JSON
	// (gardener_ops.go): a run budget that is no number is refused with the route's message, as it refuses it; the rest
	// saves. The project panel's New gardener posts /create-gardener; an epic named "nope" is refused.
	mux.HandleFunc("POST /gardener/{id}/{act}", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PathValue("act") == "config" && r.PostForm.Get("run_budget") == "lots" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error":"Run budget must be a non-negative whole number.","status":400}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"bead":"` + r.PathValue("id") + `"}`))
	})
	mux.HandleFunc("POST /create-gardener", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("epic_id") == "nope" || r.PostForm.Get("project") != "gardenio" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error":"Failed to author gardener: no such epic.","status":400}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true,"bead":"gd-GardSched1","agent":"nightly sweep"}`))
	})
	// The settings panel's save posts the viewer's POST /settings with scope=garden asked for JSON (settings.go): a Slack
	// channel the route refuses is refused with its message; a save without scope=garden is refused here, so the panel is
	// seen to send it.
	mux.HandleFunc("POST /settings", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		if r.PostForm.Get("scope") != "garden" || r.PostForm.Has("vibe_mode") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error":"the garden's save sends scope=garden and only its own fields","status":400}`))
			return
		}
		if r.PostForm.Get("team") == "refuse" {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"ok":false,"error":"Failed to save preferences.","status":502}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	rig := &cardRig{mux: mux, page: page, src: src, farm: f, spend: spend, hub: hubSrc, beads: beads, commons: commonsWorld(now())}
	// The bead view reads the Commons' beads too, as one daemon serves both in production: a workbench node's document is
	// the bead view's (web/commons2.js beadDoc, gd-0GnBi7SRfK). The beads it held already stay its own, and what the
	// Commons writes (a reply, a test route's message: putBoth) it reads from then on.
	rig.commons.OnCreate = rig.beads.put
	for id, b := range rig.commons.Beads {
		if beads.beads[id] == nil {
			beads.beads[id] = b
			beads.comments[id] = rig.commons.Comments[id]
			for _, d := range rig.commons.Deps {
				if d.BeadID == id {
					beads.deps = append(beads.deps, d)
				}
			}
		}
	}
	var chub *commons.Hub
	if liveCommons {
		rig.commonsEvents = make(chan beadsapi.SSEEvent, 64)
		chub = commons.NewHub(func(context.Context) (<-chan beadsapi.SSEEvent, func()) { return rig.commonsEvents, func() {} }, rig.commons, nil)
		chub.Start()
		t.Cleanup(chub.Close)
	}
	mountCommons(mux, rig.commons, page, chub, commonsPresence())         // the Commons pane's land (commons_world_test.go)
	mountFarmScenes(mux, src, f)                                          // the goldens' patch injector (transitions_test.go)
	mux.HandleFunc("GET /test/grafana/d-solo/{uid}", fixtureGrafanaPanel) // the Costs panel's frames (fixtureGrafanaPanel)
	srv := httptest.NewServer(withUXFixture(mux))
	t.Cleanup(srv.Close)
	page.GrafanaBaseURL = srv.URL + "/test/grafana" // read per request; set before the first one
	return srv, rig
}

// fixtureGrafanaPanel stands in for a Grafana solo panel (GET <base>/d-solo/<uid>?panelId=N, grafanaembed), so the Costs
// panel's frames load the same picture every time without a Grafana: a dark panel of bars whose heights follow panelId.
func fixtureGrafanaPanel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("panelId"))
	var bars strings.Builder
	for i := 0; i < 12; i++ {
		h := 20 + (i*37+id*53)%70
		bars.WriteString(`<rect x="` + strconv.Itoa(4+i*8) + `" y="` + strconv.Itoa(100-h) + `" width="6" height="` + strconv.Itoa(h) + `" fill="#73bf69"/>`)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><html><body style="margin:0;background:#181b1f"><svg viewBox="0 0 100 104" preserveAspectRatio="none" style="display:block;width:100vw;height:100vh">` +
		`<path d="M0 100H100" stroke="#2c3235" stroke-width="1"/>` + bars.String() + `</svg></body></html>`))
}

// stressBead adds gd-StressF1F2 to the bead view's reader only: it is on no plot, so the farm, the card walk and the goldens
// never see it, but /garden?bead=gd-StressF1F2 opens it. It carries the Commons UX rubric's stress fixtures
// (gardenio/docs/commons-ux-rubric.md §1) for the commons-ux-acceptance skill's beadstress surface: F1 long human text, F2
// unbroken tokens, F4 emoji and non-Latin, F6 a child that is not in the daemon.
func stressBead(beads *fakeBeads) {
	url := "https://gitlab.example.com/gardenio/controller/-/merge_requests/4242/diffs?commit_id=0123456789abcdef0123456789abcdef01234567"
	b := fb("gd-StressF1F2", "task", "in_progress", "", 900)
	b.Title = "Stress: " + strings.Repeat("a long human title that keeps going ", 6)[:192]
	b.Assignee = "gardenio-crew-" + strings.Repeat("very-long-agent-name-", 4)[:76]
	b.Description = "Merge request " + url + " is the change. Head SHA 0123456789abcdef0123456789abcdef01234567 on branch " +
		"agent/some-very-long-branch-name/gd-AbCdEfGhIj; the bare id gd-AbCdEfGhIj names its task, and the file is " +
		"gardenio/controller/internal/gardenhub/web/garden.css:364.\n\n" +
		"🧑‍🌾 Farmer note, 🇳🇿 flag, ⚠️ warning with VS16, 田中花子 and مريم reviewed it; café with a combining accent.\n\n" +
		strings.Repeat("A long body sentence so the description runs well past the fold of a phone screen. ", 11)
	b.Fields["pr_url"] = url
	beads.beads[b.ID] = b
	child := fb("gd-StressKid1", "task", "open", b.ID, 900)
	child.Title = strings.Repeat("A child with a very long title as well ", 3)
	beads.beads[child.ID] = child
	beads.deps = append(beads.deps,
		beadsapi.Dependency{BeadID: child.ID, DependsOnID: b.ID, Type: "parent-child"},
		beadsapi.Dependency{BeadID: "gd-MissingBd1", DependsOnID: b.ID, Type: "parent-child"}) // F6: not in the daemon
	beads.comments[b.ID] = []beadsapi.Comment{{Author: "田中花子", Text: "LGTM 👍 — see " + url, CreatedAt: time.Unix(1_790_000_000, 0)}}
}

// fixtureGardeners add to the gardeners fixtureWorld already stands on plot A (nine, so the Plan level shows three and "+6",
// the Bead level six and "+3", the foreman at the gate; four more at the barn): one asking and one down on plot B, and an
// infra gardener starting on plot C (gardenio/docs/homestead-agents-as-people.md section 8). g-down-b names a thread, so its
// card has a Thread in the Commons link. Times are against now, so the goldens' frozen clock reads the same ages on every run.
func fixtureGardeners(now time.Time) []*beadsapi.BeadDetail {
	g := func(id, name, state, role, task string, ago time.Duration) *beadsapi.BeadDetail {
		b := fb(id, "agent", "in_progress", "", 1)
		b.Title = name
		b.Fields = map[string]string{"agent": name, "role": role, "agent_state": state, "task_id": task, "last_progress_at": now.Add(-ago).UTC().Format(time.RFC3339)}
		return b
	}
	down := g("gd-gDownB", "g-down-b", "failed", "crew", "gd-planB1a", 2*time.Hour)
	down.Fields["thread_id"] = "gd-thread-g-down-b"
	return []*beadsapi.BeadDetail{
		g("gd-gAskB", "g-ask-b", "yielding", "crew", "gd-planB1a", 12*time.Minute),
		down,
		g("gd-gStartC", "g-start-c", "spawning", "infra", "gd-planC1a", time.Minute),
	}
}

// Every text panel on the farm scrolls under a finger on a phone (gd-O24RP2gSmc): the Outline, the Plans and Legend
// sheets, the Jump list, the bead view and the Mailbox screen, each given more content than fits and scrolled
// with a synthesized touch gesture at 390x844. What is asserted lives in testdata/touch_scroll_browser.js.
func TestTextPanelsTouchScrollUnderChromium(t *testing.T) {
	underChromium(t, "touch_scroll_browser.js", func() *httptest.Server {
		var porch []*beadsapi.BeadDetail
		for i := 0; i < 12; i++ {
			n := strconv.Itoa(i + 1)
			a := bead("a-"+n, "agent", int64(100+i))
			a.Title, a.Fields = "gardener-"+n, map[string]string{"agent_state": "working"}
			d := bead("d-"+n, "decision", int64(200+i))
			d.Status = "open"
			d.Fields = map[string]string{"prompt": "Question " + n + ": ship the change now, or wait for the next check-in walk with the owner?",
				"options": `[{"id":"a","short":"Now","label":"Ship now"},{"id":"b","short":"Wait","label":"Wait for the walk"}]`}
			porch = append(porch, a, d)
		}
		return cardWorld(t, time.Now, porch...)
	})
}

// A two-finger pinch zooms the free camera about the fingers' midpoint on a phone (cursor-grid spec revision 3): a fourfold
// spread zooms 4x with the point under the midpoint kept, and across the Plan edge the cursor goes into the plot under it; a
// 20% spread zooms 1.2x and moves no cursor; a pinch in across the World edge comes out to the farm. It keeps working after a
// finger has been lost (gd-iYlxddKzuu): a spread whose fingers land on hot-links removed under them mid-gesture, and one
// after two ghost pointers that never came up, must each still go in. What is asserted lives in testdata/touch_pinch_browser.js.
func TestPinchZoomUnderChromium(t *testing.T) {
	underChromium(t, "touch_pinch_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}

// uxWriter is the fixture farm's writer: a request carrying X-UX-Writer: 1 is a viewer who may edit beads and start agents
// (the bead view's write bar, the cockpit's and the queues' writes, quick-spawn), so the commons-ux-acceptance skill can shoot
// them (its shoot.js rows set the header). The goldens never send it, so what they guard is the read-only viewer's panel.
func uxWriter(r *http.Request) bool { return r.Header.Get("X-UX-Writer") == "1" }

// uxWaker is whether a fixture request may restart gardeners (Handler.CanWake): one carrying X-UX-Waker: 1, so a walk
// sees the workbench's Wake / Restart both offered and not (workbench_wake_browser.js).
func uxWaker(r *http.Request) bool { return r.Header.Get("X-UX-Waker") == "1" }

// uxImpersonator is the fixture farm's Become session: a request carrying X-UX-Impersonator: <admin> is uxTenant's session
// impersonated by that admin, so the page draws the banner (plan gd-0SUIdKXWV3 step 4). In beads-viewer the name is the
// verified token's claim alone; only this fixture reads it from a header.
func uxImpersonator(r *http.Request) string { return r.Header.Get("X-UX-Impersonator") }

// uxTenant is who an impersonated fixture session is signed in as.
const uxTenant = "oidc:phono@example.com"

// releasesPlan is a ready plan staked out in the fields with the steward's "run it?" decision open on it (staked plots
// step 4, gd-zcUua6XeEJ), modelled on the real releases plan gd-UZKZ9H25UD: ten steps in step_order, the third one's body
// longer than the bead view's 280-rune fold, and a narrative. It is created last, so it takes gardenio's next plot slot
// and the opening, cursorproject and cursorplan goldens show a staked plot; the staked rows of shoot.js read it.
func releasesPlan() []*beadsapi.BeadDetail {
	p := planBead("gd-planR", "ready", "open")
	p.Title = "Releases of a project: a release record in kbeads, a release truck on /garden, a Release page in beads-viewer"
	p.ShortTitle = "release record, truck and release page"
	p.Description = "Gardenio knows what a project has released, what each release carries, and where it is running. Research first, " +
		"then backend, then read model and contract, then the two surfaces, with a check-in after each surface."
	out := []*beadsapi.BeadDetail{p}
	var order []string
	for i, st := range [][2]string{
		{"Research: what a release is today, and the design", "Read release-cut.sh, release-deploy.sh and the release-deployer CronJob; write down what a release leaves behind today."},
		{"Backend: the release bead, its deployments, and kd release", "A type=release bead parented to its project bead, keyed by project and tag."},
		{"Capture at cut time, and the backfill", "A tag push on the project's forge, seen by github-bridge and matched against the project's tag pattern, writes the release bead " +
			"and its contents from the forge's compare of the previous tag to this one. The backfill walks every calver tag on main once, oldest first, " +
			"so the truck and the Release page show real gardenio releases from the first deploy, and a second run writes nothing new."},
		{"Capture at deploy time: where a release runs", "kd release deploy, gl release deployed, or a deployment_status webhook adds a row per target."},
		{"Read model: releases on the farm document", "The newest cut release per project, its contents grouped by plan, and where it runs."},
		{"Lands contract and beads-viewer API: releases", "GET /releases and the deployment write, in the contract with golden fixtures."},
		{"Frontend: the release truck on /garden", "The Yard's pickup is loaded with crates from the produce bins and drives out when a release goes live."},
		{"Frontend: the Release page in beads-viewer", "One page per release: its tag, what it carries, where it runs since when."},
		{"Check-in: deploy, screenshots, and the owner reads it", "Deployed at the next release window; the owner looks at the truck and the page."},
		{"Acceptance: UX grades, goldens promoted, docs", "Grade both surfaces, promote them to goldens, and write the runbook."},
	} {
		id := fmt.Sprintf("gd-planR%02d", i+1)
		s := fb(id, "task", "open", "gd-planR", int64(800+i))
		s.Title, s.Description = fmt.Sprintf("%d. %s", i+1, st[0]), st[1]
		order = append(order, id)
		out = append(out, s)
	}
	raw, _ := json.Marshal(order)
	p.Fields["step_order"] = string(raw)
	dec := fb("gd-planRask", "decision", "open", "", 810)
	dec.Title = "Plan gd-planR: " + p.Title + " - run it?" // as steward.WritePlan words it, so the card asks "Run it?"
	dec.Fields["requesting_plan"], dec.Fields["requesting_task"], dec.Fields["prompt"] = "gd-planR", "gd-planR", dec.Title
	dec.Fields["options"] = `[{"id":"approve","short":"Run the plan","label":"Run the plan: the steward starts its first step"},{"id":"change","short":"Change it first","label":"Change the plan first: say what to change, and the steward writes it again"},{"id":"drop","short":"Drop it","label":"Drop the plan: nothing runs"}]`
	return append(out, dec)
}

// shareStub answers the viewer's land Share routes as land_share.go does, for the project panel's Share tab.
type shareStub struct{}

func (shareStub) serve(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	days := r.Form.Get("days")
	if days == "" {
		days = "30"
	}
	land, live := r.PathValue("project"), "live"
	p := map[string]any{
		"ok": true, "land": land, "grants": "Anyone who has this link can watch the whole land.", "expiries": []int{7, 30, 90, 365}, "days": 30,
		"preview": "/p/" + land + "/share/preview?days=" + days, "plans": 4, "withheld": []map[string]string{{"path": "plans[1].title", "rule": "denylist"}},
		"withheld_text": "withheld: contains an internal reference",
	}
	if r.Method == http.MethodPost && r.PathValue("window") != "" {
		p["revoked"], live = r.PathValue("window"), "revoked"
	}
	p["windows"] = []map[string]any{
		{"id": "gd-LandWin001", "target_kind": "land", "land": land, "state": live, "view_count": 12, "expires_at": "2026-11-28T00:00:00Z", "created_by": "owner@orchard.example"},
		{"id": "gd-LandWin000", "target_kind": "land", "land": land, "state": "revoked", "view_count": 0, "expires_at": "2026-11-20T00:00:00Z"},
	}
	if r.Method == http.MethodPost && r.PathValue("window") == "" {
		p["minted"] = map[string]any{"url": "https://garden.example/w/gw_AAAAAAAAAAAAAAAAAAAAAA", "id": "gd-LandWin002", "expires_at": "2026-12-29T00:00:00Z"}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p)
}
