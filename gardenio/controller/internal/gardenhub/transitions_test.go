package gardenhub

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The gardeners' transitions (gardenio/docs/homestead-agents-as-people.md section 7, gd-elQWxzYhdm): ANIM.plan and ANIM.at
// in web/transitions.js are pure, so the table in section 7.1 runs under node. Home is its field stand when homeFill drew one
// (homestead-world-design.md 28.12), else its own project's barn door (28.4; a thread agent's and one with no biome yet, the
// Commons's). A gardener that appears on a plot of its home biome (or leaves home for one) walks out of its stand; to another
// biome's plot a truck leaves the barn door, picks it up at its stand and drives it there; with no stand drawn the truck
// leaves the door. One that goes down bursts, its scarecrow rises and it flies back to its stand; one that recovers stands up;
// one that goes home walks back to its stand (or rides back by it); an exit "finished" walks home to its stand with its tldr,
// "interrupted" and "parked" burst with no scarecrow left, and a home gardener's exit puffs where its figure stood. The ride and the walk home keep to the lanes (homestead-world-design.md 29.3,
// X-D11): the truck down into the bay and west onto the west lane, the walker out at a gate of the farmstead's cross, along
// the lanes and in at the spot's column, so a way crosses only the farm, lane tiles, the plot gap on the farm's side of a
// plot outside the three middle rows, and the plot; the way home is the way out reversed. One whose task moved to another
// plot walks there along the 2-tile paths between the plots (World Phase 3, gd-gMMtrgIT1W): down off its fence, along the
// path under its plot, up or down the path beside it, along the path under the other plot and up onto its fence; between
// two projects, straight. A move to another column of the same plot is a short walk along the fence.
// Rules 2 to 4 (7.2): nothing below the Plan level, off the padded viewport, in a patch of more than eight, or from an exit
// older than 10 s; trucks and walks take distance / 100 px per s clamped to 2.5-7 s, the flight 2.5-4.5 s (a leisurely pace).
func TestTransitionsUnderNode(t *testing.T) {
	nodeScript(t, "transitions_test.js")
}

// The gardener's barn (homestead-world-design.md 28.4, 28.3): WORLD.barnDoor is where the ride truck leaves its own project's
// barn, the Commons's for a thread agent or a project with no biome yet, none with neither; WORLD.homeTile is the one origin of
// a gardener's walk home, fly-back and birds: its figure at its field stand when homeFill placed one (not behind the field's
// "+N" badge, 28.12), else its barn's door.
func TestHomeTileUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), A = require(process.argv[2]), assert = require("assert"), T = W.TILE;
function plan(id, project) { return { id: id, title: id, project: project, created: 1, activity: 1, counts: {}, phases: [], unphased: { id: "", counts: {}, tasks: [] } }; }
function doc(ps) { return { plans: ps, seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] } }; }
function door(b) { var k = b.home.keep.barn; return { x: k.x * T + W.HOME.door[0] - 8, y: k.y * T + W.HOME.door[1] - 24, biome: b.id }; }
assert.deepStrictEqual(W.HOME.door, [8 + A.BARN_DOOR[0], 8 + A.BARN_DOOR[1]], "the door ART draws: the barn 8 px into its keep, BARN_DOOR off the barn sprite");
var w = W.layout(doc([plan("pc", ""), plan("pa", "gardenio")]), null, { now: 1, readonly: true }), gb = w.index["project:gardenio"], cb = w.index["project:"];
var d = W.barnDoor(w, "project:gardenio"), c = gb.home.cells.barn;
assert.deepStrictEqual(d, door(gb), "its own project's barn door");
assert.ok(d.x >= c.x * T && d.x + 16 <= (c.x + c.w) * T && d.y >= c.y * T && d.y + 24 <= (c.y + c.h) * T, "in its Barn cell");
assert.deepStrictEqual(W.barnDoor(w, W.homeFor({ project: "gardenio", role: "thread" })), door(cb), "a thread agent: the Commons's barn");
assert.deepStrictEqual(W.barnDoor(w, W.homeFor({ project: "music" })), door(cb), "no biome yet: the Commons's barn, not nowhere");
var w2 = W.layout(doc([plan("pa", "gardenio")]), null, { now: 1, readonly: true });
assert.strictEqual(W.barnDoor(w2, "project:music"), null, "no such biome and no Commons: no barn");
assert.strictEqual(W.homeTile(w2, { id: "m", project: "music" }), null);
// at home: its figure at its field stand, the east tile of its pair slot; past the field's places the last is the "+N" badge,
// so a gardener behind it is at the door
var gs = [], i;
for (i = 0; i < 80; i++) gs.push({ id: "a" + i, agent: "a" + i, role: "crew", project: "gardenio", state: "idle", bucket: "idle", since: i, href: "" });
var st = W.gardenerState(w, gs), fill = W.homeFill(w, { agent: W.gardenerField(st, null, "", 0) }, {})["project:gardenio"].growing;
assert.ok(fill.more > 0 && fill.badge, "the fixture fills the field");
var f0 = fill.items[0];
assert.deepStrictEqual(W.homeTile(w, f0.item.g), { x: (f0.x + f0.w - 1) * T, y: f0.y * T, biome: "project:gardenio", drawn: true }, "its figure at its stand");
assert.ok(!fill.items.some(function (h) { return h.id === W.FIELD + "a79"; }), "the last in need order is behind the badge");
assert.deepStrictEqual(W.homeTile(w, st.byId.a79), Object.assign({ drawn: false }, door(gb)), "behind the badge: its barn's door");
console.log("ok");
`
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	out, err := exec.Command(node, "-e", script, abs(filepath.Join("web", "world.js")), abs(filepath.Join("web", "art.js"))).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("home tile under node: %v\n%s", err, out)
	}
}

// The transitions are wired into the page: transitions.js is embedded after art.js (its sprites) and lanes.js (its ways) and before garden.js,
// which feeds it each farm document, skips the sprites it stands in for, draws it each render and keeps a moving gardener's
// link and barn slot back until it lands; plans.js says which documents are live patches and passes their exits; the
// legend has the truck row; reduced motion reaches it.
func TestTransitionsWired(t *testing.T) {
	art, tr, app := strings.Index(pageHTML, `{{asset "art.js"}}`), strings.Index(pageHTML, `{{asset "transitions.js"}}`), strings.Index(pageHTML, `{{asset "garden.js"}}`)
	if art < 0 || tr < art || app < tr {
		t.Fatal("transitions.js must be embedded after art.js and before garden.js")
	}
	if w, ln, tk := strings.Index(pageHTML, `{{asset "world.js"}}`), strings.Index(pageHTML, `{{asset "lanes.js"}}`), strings.Index(pageHTML, `{{asset "truck.js"}}`); w < 0 || ln < w || tr < ln || tk < ln {
		t.Fatal("lanes.js must be embedded after world.js, which it reads, and before transitions.js and truck.js, which ride its ways")
	}
	for _, want := range []string{"ANIM.update(prev,gspots,", "reduce:reduce", "ANIM.hides", "ANIM.draw(ctx,", "WORLD.gardenerField(gspots,scope==='mine'&&me,base,0,ANIM.still)", "ANIM.moving(g.id)", "ANIM.onLand(", "ANIM.finished()", "['truck','Truck: a gardener on its way to a plot']"} {
		if !strings.Contains(pageAppJS+pageSceneJS, want) {
			t.Errorf("garden.js and scene.js missing the transitions hook %q", want)
		}
	}
	for _, want := range []string{"tell('patch', p.exits)", "tell('open')", "tell('reset')", "tell('load')", "'farmlive'"} {
		if !strings.Contains(pagePlansJS, want) {
			t.Errorf("plans.js missing %q", want)
		}
	}
	if !strings.Contains(pageTransitionsJS, `"farmlive"`) || !strings.Contains(pageTransitionsJS, "o.reduce && !force") {
		t.Error("transitions.js: the farmlive listener or the reduced-motion cut is gone")
	}
	// each farm document is a fresh layout, so its field stands (28.12) must be placed before ANIM plans, or a spawn's walk and
	// a fly-back start from the barn door; under reduced motion the Glass's flight to the plot (H-D21) is a cut
	if fill, plan := strings.Index(pageAppJS, "model=buildModel();var hs=CURNAV.homeSpec(world,model,slots,barnCounts());"), strings.Index(pageAppJS, "ANIM.update(prev,gspots,"); fill < 0 || plan < fill || !strings.Contains(pageAppJS, "rebuildTree(hs)") {
		t.Error("garden.js: drawDoc must place the new field stands before ANIM.update and hand that fill to rebuildTree")
	}
	if !strings.Contains(pageAppJS, "if(reduce||!(ms>0)){setCam(to);return;}") {
		t.Error("garden.js: flyTo no longer cuts under reduced motion")
	}
}

// "Go to plot" (3b R13): the Porch's Go to plot and the outline's gardener row jump the cursor into the gardener's plot, as the
// bead view's "Show on the map" does, focus on the cursor; beside a band item or a goat (gd-6FCoWelnx2) they jump onto it
// (standGo). The ride-along they used to start (World Phase 3, gd-gMMtrgIT1W) is retired: nothing
// follows a walking gardener, and the camera strip has no Stop ride.
func TestGoToPlotJumps(t *testing.T) {
	for _, want := range []string{
		"function standGo(p,keyed){return p.kind==='plot'?cursorInto(p.id):cursorTo(p.id,keyed);}",
		"if(!p){cursorTo(id,true);return;}if(standGo(p,true))focusCursor();}",       // porchGo
		"if(s&&s.plot){if(standGo(world.index[s.plot],true))focusCursor();return;}", // the outline
		"fly:function(id){lift();var gs=gspots&&gspots[3].spots[id];return gs&&gs.plot?standGo(world.index[gs.plot],false):cursorTo(id,false);}",
	} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js missing the plain jump %q", want)
		}
	}
	for _, gone := range []string{"rideWith", "rideFollow", "rideEnd", "rideRect", "cam-ride", "Riding along", "ANIM.eta"} {
		if strings.Contains(pageAppJS, gone) {
			t.Errorf("garden.js still has the retired ride-along's %q", gone)
		}
	}
	if strings.Contains(pageHTML, `id="cam-ride"`) || strings.Contains(pageHTML, "Stop ride") {
		t.Error("garden.html: the camera strip still has the retired Stop ride button")
	}
	if strings.Contains(pageTransitionsJS, "eta: function (id)") {
		t.Error("transitions.js: ANIM.eta only served the retired ride-along")
	}
}

// Go to plot in a real Chromium (testdata/goto_browser.js): Go to plot on the Porch puts the cursor in g-ask-b's plot with
// the camera "Show on the map" gives; when its task moves to plan C it walks there (no link until it lands) while the
// cursor and the camera stay in plot B; Go to plot again puts the cursor in plot C. And the field stands (28.12): g-ask-b
// stands solid in gardenio's Growing field, its link naming plot B, and one key walks its three stops (H-D21): its Glass, its
// plot figure at full zoom, and home to the same field figure. A phone walks them by taps (gd-a628FpJRO5): Go lists the
// field stands under the home, the Glass is its sheet with Go to its plot, and a tap on the plot figure comes home.
func TestGoToPlotUnderChromium(t *testing.T) {
	underChromium(t, "goto_browser.js", func() *httptest.Server { return cardWorld(t, time.Now) })
}

// mountFarmScenes is the goldens' patch injector (homestead-agents-as-people.md section 8), on a test server only:
// POST /test/farm/scene?name=<scene>[&undo=1] edits cardWorld's fake daemon, has the farm refresh, and answers once the
// patch is published (the stream's version moved), so an open /garden has it; undo puts the beads back. ride: g-ride-c
// starts on plot C (absent -> on a plot: the walk out of its field stand, or the truck to another biome's plot). burst: g-ask-b goes failed on plot B. walkhome: the plan gardener
// g-foreman stops with a wrap-up, its task the plan itself, so the farm sends an exit "finished" with its tldr. walkto:
// g-ask-b's task moves from plan B to plan C (World Phase 3: it walks from plot B to plot C). crowd: coop's farmstead fills
// (crowdBeads): more stands than its field holds, so its last slot is the "+N", and more loose beads than its half share.
func mountFarmScenes(mux *http.ServeMux, src *fakeFarmSrc, f *Farm) {
	crowd := crowdBeads()
	ride := fb("gd-gRideC", "agent", "closed", "", 1) // closed until the scene: no gardener, no change to any other shot
	ride.Title, ride.Fields = "g-ride-c", map[string]string{"agent": "g-ride-c", "role": "crew", "agent_state": "spawning", "task_id": "gd-planC1a"}
	src.add(ride)
	// arms (heraldry step 5.9): two gardeners closed until their scene stand on plan P, whose arms are the fixture's picture:
	// g-arms-p on a step of its second phase, which bears none, so its badge is the plan's; g-arms-pa on the first phase's
	// step, whose stake's arms (goldenPhaseArms) are its badge, so the two differ on one plot.
	armsP := fb("gd-gArmsP", "agent", "closed", "", 1)
	armsP.Title, armsP.Fields = "g-arms-p", map[string]string{"agent": "g-arms-p", "role": "crew", "agent_state": "working", "task_id": "gd-planPb1"}
	armsPa := fb("gd-gArmsPa", "agent", "closed", "", 1)
	armsPa.Title, armsPa.Fields = "g-arms-pa", map[string]string{"agent": "g-arms-pa", "role": "infra", "agent_state": "working", "task_id": "gd-planPa1"}
	src.add(armsP, armsPa)
	scenes := map[string]struct {
		id       string
		do, undo func(b *beadsapi.BeadDetail)
	}{
		"ride":   {"gd-gRideC", func(b *beadsapi.BeadDetail) { b.Status = "in_progress" }, func(b *beadsapi.BeadDetail) { b.Status = "closed" }},
		"arms":   {"gd-gArmsP", func(b *beadsapi.BeadDetail) { b.Status = "in_progress" }, func(b *beadsapi.BeadDetail) { b.Status = "closed" }},
		"armspa": {"gd-gArmsPa", func(b *beadsapi.BeadDetail) { b.Status = "in_progress" }, func(b *beadsapi.BeadDetail) { b.Status = "closed" }},
		"burst": {"gd-gAskB", func(b *beadsapi.BeadDetail) { b.Fields["agent_state"] = "failed" },
			func(b *beadsapi.BeadDetail) { b.Fields["agent_state"] = "yielding" }},
		"crowd": {}, // no one bead: crowd's beads come and go whole
		"walkto": {"gd-gAskB", func(b *beadsapi.BeadDetail) { b.Fields["task_id"] = "gd-planC1a" },
			func(b *beadsapi.BeadDetail) { b.Fields["task_id"] = "gd-planB1a" }},
		"walkhome": {"gd-gForeman", func(b *beadsapi.BeadDetail) {
			b.Fields["agent_state"], b.Fields["stop_requested"] = "done", "true"
			b.Fields[beadsapi.WrapUpFieldName] = `{"tldr":"Planted phase 2 of the Edge IdP rollout; every step has a crew and the gate is open"}`
		}, func(b *beadsapi.BeadDetail) {
			b.Fields["agent_state"] = "working"
			delete(b.Fields, "stop_requested")
			delete(b.Fields, beadsapi.WrapUpFieldName)
		}},
	}
	mux.HandleFunc("POST /test/farm/scene", func(w http.ResponseWriter, r *http.Request) {
		sc, ok := scenes[r.URL.Query().Get("name")]
		if !ok {
			http.Error(w, "no such scene", http.StatusNotFound)
			return
		}
		v0, _, _ := f.Snapshot()
		switch undo := r.URL.Query().Get("undo") == "1"; {
		case sc.id == "" && undo:
			src.drop(crowd...)
		case sc.id == "":
			src.drop(crowd...) // a retried shot posts it again
			src.add(crowd...)
		case undo:
			src.mutate(sc.id, sc.undo)
		default:
			src.mutate(sc.id, sc.do)
		}
		f.Invalidate()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if v, _, _ := f.Snapshot(); v != v0 {
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		http.Error(w, "the farm published no patch", http.StatusGatewayTimeout)
	})
}

// crowdBeads is the crowd scene's coop farmstead (gd-a628FpJRO5): 24 gardeners working coop's five plan steps, 3 more on
// loose coop beads (a stand and its plant), 3 resting, and 26 loose beads, more than the 40-slot field's half share, so the
// field shows its "+N". Built without fb, so seq (and every other bead's created_at, the World layout) never moves.
func crowdBeads() []*beadsapi.BeadDetail {
	var out []*beadsapi.BeadDetail
	for i := range 26 {
		id := fmt.Sprintf("gd-crowdM%02d", i)
		out = append(out, &beadsapi.BeadDetail{ID: id, Title: fmt.Sprintf("Loose coop bead %d of the crowd", i+1), Type: "task", Status: "open",
			Priority: 2, Labels: []string{"project:coop"}, Fields: map[string]string{},
			CreatedAt: time.Unix(baseUnix+5_000+int64(i), 0), UpdatedAt: time.Unix(baseUnix+5_000+int64(i), 0), OperatedBy: "OIDC:Matt@Example.com"})
	}
	for i := range 30 {
		task, state := fmt.Sprintf("gd-coop%c1a", 'a'+i%5), "working"
		switch {
		case i >= 27:
			task, state = "", "idle"
		case i >= 24:
			task = fmt.Sprintf("gd-crowdM%02d", i-24)
		}
		g := agentBead(fmt.Sprintf("gd-gCrowd%02d", i), fmt.Sprintf("coop-crew-%02d", i+1), "crew", state, task, int64(6_000+i))
		g.Labels = []string{"project:coop"}
		out = append(out, g)
	}
	return out
}
