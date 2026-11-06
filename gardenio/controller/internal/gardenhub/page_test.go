package gardenhub

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func servedPage(t *testing.T) (*http.ServeMux, *httptest.ResponseRecorder) {
	t.Helper()
	f := newFake()
	b := bead("gd-w1", "bug", 5)
	b.HasReport = true
	a := bead("a-1", "agent", 6)
	a.Fields = map[string]string{"agent_state": "working"}
	f.set([]*beadsapi.BeadDetail{b, a}, []*beadsapi.BeadDetail{bead("d-1", "decision", 7)})
	h := New(f, "/v", nil)
	_ = h.refresh(context.Background())
	mux := http.NewServeMux()
	(&Handler{Hub: h, BasePath: "/v", Viewer: func(*http.Request) string { return "oidc:me@example.com" }}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
	return mux, rec
}

// The farm scene is painted from data-* on each list item, so the fragment must carry them.
func TestItemFragmentCarriesSceneData(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	for _, want := range []string{`data-ty="bug"`, `data-rp="1"`, `data-st="working"`, `data-pj="gardenio"`, `data-pr="0"`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
}

// inlined is a served /garden page with each asset it loads (assets.go) spliced back in its place, a script's text between
// <script> tags and a stylesheet's in <style>: the tests that read a script's text, or the order the page runs its scripts in,
// read what the page loads. The text is the asset's source as written, before it was minified, so a test can look for a
// line of web/ as it reads there. An asset the page names that the table does not hold fails the test.
func inlined(t *testing.T, body string) string {
	t.Helper()
	out := assetTag.ReplaceAllStringFunc(body, func(tag string) string {
		m := assetTag.FindStringSubmatch(tag)
		file := m[2] + m[4]
		a := pageAssets.byFile[file]
		if a == nil {
			t.Errorf("the page loads %s, which is not a page asset", file)
			return tag
		}
		if m[3] == "link" {
			return "<style>" + a.src + "</style>"
		}
		return "<script>" + a.src + "</script>"
	})
	// the bundles web/lazy.js loads after the World (P1.3), in the order it loads them: the page carries them too
	var lazy strings.Builder
	for _, m := range lazyAsset.FindAllStringSubmatch(body, -1) {
		a := pageAssets.byFile[m[1]]
		if a == nil {
			t.Errorf("the page names %s for lazy.js, which is not a page asset", m[1])
			continue
		}
		lazy.WriteString("<script>" + a.src + "</script>")
	}
	if lazy.Len() > 0 {
		i := strings.LastIndex(out, "</body>")
		if i < 0 {
			i = len(out)
		}
		out = out[:i] + lazy.String() + out[i:]
	}
	return out
}

var (
	assetTag  = regexp.MustCompile(`<(script) src="[^"]*/garden/assets/([^"]+)"></script>|<(link) rel="stylesheet" href="[^"]*/garden/assets/([^"]+)">`)
	lazyAsset = regexp.MustCompile(`\bdata-[a-z]+-js="[^"]*/garden/assets/([^"]+)"`)
)

// The page must stay self-contained: the prototype loaded Google Fonts, the live page may not reach out. Its scripts and
// stylesheets are its own assets, below the base path.
func TestPageHasNoExternalRequests(t *testing.T) {
	_, rec := servedPage(t)
	raw := rec.Body.String()
	for _, m := range regexp.MustCompile(`<(?:script|link)\b[^>]*\b(?:src|href)="([^"]*)"`).FindAllStringSubmatch(raw, -1) {
		if !strings.HasPrefix(m[1], "/v/garden/") {
			t.Errorf("the page loads %q, not below its base path", m[1])
		}
	}
	body := inlined(t, raw)
	if rec.Code != 200 || !strings.Contains(body, "var ART = (function") || !strings.Contains(body, `id="plans-slot"`) {
		t.Fatalf("page missing embedded art or plans slot (code %d)", rec.Code)
	}
	if m := regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//|url\(\s*["']?(https?:)?//|@import|fonts\.g`).FindString(body); m != "" {
		t.Fatalf("page makes an external request: %q", m)
	}
	for _, s := range []string{pageHTML, pageCSS, pageArtJS, pageWorldJS, pageWeedsJS, pageBeaconsJS, pageAppJS, pagePlansJS, pageChatJS} {
		if strings.Contains(s, "fonts.googleapis") || strings.Contains(s, "gstatic") {
			t.Fatal("embedded asset references a web font host")
		}
	}
}

// A browser holding a page from before a deploy must not get a 304 for old markup just because the
// snapshot version (which restarts with the process) happens to match.
func TestETagCoversAssets(t *testing.T) {
	_, rec := servedPage(t)
	if !strings.Contains(rec.Header().Get("ETag"), assetsTag) {
		t.Fatalf("ETag %q does not include the asset tag %q", rec.Header().Get("ETag"), assetsTag)
	}
}

// art.js only touches ctx.fillStyle/fillRect, so it runs under a node mock: paint a farmstead (ART.drawHome) in both skies
// with every plant kind in its Growing field, a letter in its Mailbox and a villager in every state in its Barn, each fill a
// real colour.
// Skipped when node is not installed.
func TestArtSceneUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), ART = require(process.argv[2]);
var rects = 0, ctx = { fillRect: function () { rects++; }, drawImage: function () {}, globalAlpha: 1, set fillStyle(v) { if (!/^#[0-9a-f]{6}$/i.test(v)) throw new Error("bad colour " + v); } };
var hm = W.layout({ plans: [{ id: "pa", title: "A", project: "gardenio", created: 1, activity: 1, counts: {}, phases: [], unphased: { id: "", counts: {}, tasks: [] } }] }, null, { now: 1, readonly: true }).index["project:gardenio"].home;
var g = hm.cells.growing, mb = hm.cells.mailbox, bn = hm.cells.barn;
var plants = ["task", "feature", "bug", "chore", "epic", ""].map(function (t, i) { return { kind: "plant", x: g.x + i, y: g.y + 1, item: { id: "x" + i, type: t, blocked: i % 2 === 0, report: i === 1, stale: i === 2 } }; });
var folk = ["working", "idle", "spawning", "failed", "weird"].map(function (s, i) { return { x: bn.x + i, y: bn.y + 10, item: { id: "a" + i, state: s, blocked: i === 1 } }; });
var fill = { mailbox: { items: [{ x: mb.x, y: mb.y + 4 }] }, growing: { items: plants }, barn: { items: folk, more: 3 } };
[false, true].forEach(function (night) { ART.drawHome(ctx, hm, { night: night, frame: 1, letters: 2, asks: true, release: true, fill: fill }, ART.palette(night)); });
["sprout", "fruit", "wilt", "blocked", "bug", "post", "working", "idle", "crashed", "ask"].forEach(function (k) { ART.drawLegend(ctx, k, false); });
if (rects === 0) throw new Error("nothing drawn");
`
	var args []string
	for _, f := range []string{"world.js", "art.js"} {
		p, err := filepath.Abs(filepath.Join("web", f))
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, p)
	}
	out, err := exec.Command(node, append([]string{"-e", script}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node scene smoke test failed: %v\n%s", err, out)
	}
}

// TestWorldLayoutStaked: a ready or asked seed is staked out (homestead-world-design.md sections 3 and 7, plan gd-2RdeAx8nnW
// step 2). It ranks beside the plans for a spiral slot, in (created, id) order; its plot carries the seed and one unphased bed
// of its steps, one "planned" cell each in step order; it keeps its slot when it is approved (seeds.rm + plans.up); drafts stay
// pots in the Nursery; it counts toward the slot cap and the overflow; and a farm whose seeds are all drafts lays out its plots
// exactly as with no seeds at all. art.js draws its string fence, pale soil and a stake marker per step under a mock.
func TestWorldLayoutStaked(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), ART = require(process.argv[2]), assert = require("assert");
function plan(id, project, created, n) {
  var tasks = []; for (var i = 0; i < (n || 0); i++) tasks.push({ id: id + "-t" + i, title: "T" + i, type: "task", state: "open", updated: 1 });
  return { id: id, title: "Plan " + id, project: project, created: created, activity: created, counts: { open: n || 0 }, phases: [{ id: id + "-p1", title: "Phase 1", counts: { open: n || 0 }, tasks: tasks }], unphased: { id: "", title: "Other", counts: {}, tasks: [] } };
}
function seed(id, project, created, state, steps, ask) {
  var st = []; for (var i = 0; i < (steps || 0); i++) st.push({ id: id + "-s" + i, title: "Step " + (i + 1), n: i + 1, status: "open" });
  var s = { id: id, title: "Seed " + id, project: project, created: created, updated: created, state: state, status: "open", phases: 1, tasks: steps || 0, operator: "oidc:owner@example.com" };
  if (state !== "frosted") s.steps = st;
  if (ask) s.decision = { id: id + "-ask", prompt: "Run it?", options: [{ id: "approve", short: "Run the plan" }] };
  return s;
}
function doc(plans, seeds) { return { plans: plans, seeds: seeds || [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] }, meta: { truncated: false } }; }
function plotsOf(w, key) { return w.biomes.filter(function (b) { return b.key === key; })[0].plots; }
function rects(w) { var out = []; w.biomes.forEach(function (b) { b.plots.forEach(function (p) { out.push([p.id, p.slot, p.x, p.y]); p.beds.forEach(function (bd) { bd.cells.forEach(function (c) { out.push([c.id, c.x, c.y]); }); }); }); out.push([b.id, b.used.x, b.used.y, b.used.w, b.used.h]); }); return out.sort(); }
var plans = [plan("p0", "gardenio", 10, 2), plan("p1", "gardenio", 20, 1), plan("p2", "gardenio", 30, 3)];
var seeds = [seed("r1", "gardenio", 25, "ready", 3), seed("a1", "gardenio", 50, "asked", 12, true), seed("d1", "gardenio", 5, "frosted", 4), seed("d2", "gardenio", 60, "frosted")];
var w1 = W.layout(doc(plans, seeds), null, { now: 1 }), b1 = w1.biomes[0], ps = plotsOf(w1, "gardenio");

// 1. slot order: the staked seeds rank beside the plans by (created, id), and take spiral slots 1..n (slot 0 is the farmstead, 29.1)
assert.deepStrictEqual(ps.map(function (p) { return [p.id, p.slot, !!p.staked]; }), [["p0", 1, false], ["p1", 2, false], ["r1", 3, true], ["p2", 4, false], ["a1", 5, true]]);
var r1 = w1.index.r1, a1 = w1.index.a1;
assert.strictEqual(r1.kind, "plot"); assert.strictEqual(r1.seed, seeds[0], "the plot carries its seed"); assert.strictEqual(r1.plan.staked, true);
assert.deepStrictEqual(r1.at, W.slotAt(3));
// its beds: one unphased bed of its steps, one planned cell each, block-major in step order
assert.strictEqual(r1.beds.length, 1); assert.strictEqual(r1.beds[0].other, true); assert.strictEqual(r1.beds[0].title, "Steps");
assert.deepStrictEqual(r1.beds[0].cells.map(function (c) { return [c.id, c.task.state, c.task.n, c.at.c, c.at.r]; }), [["r1-s0", "planned", 1, 0, 0], ["r1-s1", "planned", 2, 1, 0], ["r1-s2", "planned", 3, 2, 0]]);
assert.deepStrictEqual(a1.beds[0].cells.map(function (c) { return c.i; }), [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11]);
assert.strictEqual(a1.beds[0].cells[9].at.c, 3, "the tenth step starts the second block");
r1.beds[0].cells.forEach(function (c) { assert.strictEqual(w1.index[c.id], c, "a step is in the index: " + c.id); });
// what waits on a human: an asked seed's decision
assert.deepStrictEqual([r1.needs, a1.needs, b1.needs], [0, 1, 1]);
// drafts stay pots in the Nursery; the staked seeds are not pots
assert.deepStrictEqual(b1.nursery.map(function (s) { return s.id; }).sort(), ["d1", "d2"]);
assert.deepStrictEqual([b1.counts.plans, b1.counts.seeds, b1.counts.staked], [3, 4, 2]);
// the used rect holds every plot, staked ones too
ps.forEach(function (p) { var u = b1.used; assert.ok(p.x >= u.x && p.y >= u.y && p.x + p.w <= u.x + u.w && p.y + p.h <= u.y + u.h, "inside used: " + p.id); });
// the outline lists a staked plot as a plot and its steps as its tasks
var ol = W.outline(w1)[0].children.filter(function (c) { return c.id === "r1"; })[0];
assert.strictEqual(ol.kind, "plot"); assert.deepStrictEqual(ol.children.map(function (c) { return c.id; }), ["r1-s0", "r1-s1", "r1-s2"]);
// the memo holds the staked slots
assert.deepStrictEqual([w1.memo.slots.r1.s, w1.memo.slots.a1.s], [3, 5]);

// 2. approve: r1 leaves seeds for plans (seeds.rm + plans.up) and is planted in the very slot it was staked in; nothing moves
var planted = plan("r1", "gardenio", 25, 3), seeds2 = seeds.filter(function (s) { return s.id !== "r1"; });
var w2 = W.layout(doc(plans.concat([planted]), seeds2), JSON.parse(JSON.stringify(w1.memo)), { now: 2 });
assert.deepStrictEqual(plotsOf(w2, "gardenio").map(function (p) { return [p.id, p.slot, p.x, p.y, !!p.staked]; }), ps.map(function (p) { return [p.id, p.slot, p.x, p.y, p.id === "a1"]; }), "the same slots and pixels after approve");
assert.strictEqual(w2.index.r1.plan, planted); assert.strictEqual(w2.index.r1.seed, undefined);
// caught mid-approve (the plan already up, the seed not yet gone): one plot, the plan's
var wm = W.layout(doc(plans.concat([planted]), seeds), JSON.parse(JSON.stringify(w1.memo)), { now: 2 });
assert.deepStrictEqual(plotsOf(wm, "gardenio").map(function (p) { return p.id; }), ["p0", "p1", "r1", "p2", "a1"]); assert.strictEqual(wm.index.r1.plan, planted);
// a staked seed that goes back to draft becomes a pot; the plots after it step back, as when a plan finishes
var seeds3 = seeds.map(function (s) { return s.id === "r1" ? seed("r1", "gardenio", 25, "frosted", 3) : s; });
var w3 = W.layout(doc(plans, seeds3), JSON.parse(JSON.stringify(w1.memo)), { now: 3 });
assert.deepStrictEqual(plotsOf(w3, "gardenio").map(function (p) { return [p.id, p.slot]; }), [["p0", 1], ["p1", 2], ["p2", 3], ["a1", 4]]);
assert.ok(w3.biomes[0].nursery.some(function (s) { return s.id === "r1"; }));

// 3. a farm whose seeds are all drafts lays out its plots as with no seeds at all (only the Nursery holds them)
var drafts = [seed("d1", "gardenio", 5, "frosted", 4), seed("d2", "gardenio", 60, "frosted"), seed("d3", "gardenio", 1, "frosted")];
var plotRects = function (w) { return rects(w).filter(function (r) { return r[0].indexOf("project:") !== 0; }); };
assert.deepStrictEqual(plotRects(W.layout(doc(plans, drafts), null, { now: 1 })), plotRects(W.layout(doc(plans, []), null, { now: 1 })));

// 4. shuffled input, same map
var a = rects(W.layout(doc(plans, seeds), null, { now: 5 })), b = rects(W.layout(doc(plans.slice().reverse(), seeds.slice().reverse()), null, { now: 5 }));
assert.deepStrictEqual(a, b);

// 5. the slot cap and the overflow count staked plots like plans, in rank order
var many = []; for (var i = 0; i < W.SLOT_CAP - 1; i++) many.push(plan("m" + (i < 10 ? "0" : "") + i, "big", 100 + i));
var late = [seed("z1", "big", 900, "ready", 1), seed("z2", "big", 901, "asked", 2, true), seed("z3", "big", 50, "ready", 1)];
var wc = W.layout(doc(many, late), null, { now: 1 }), bc = wc.biomes[0];
assert.strictEqual(bc.plots.length, W.SLOT_CAP); assert.strictEqual(bc.plots[0].id, "z3", "the oldest takes slot 1");
assert.deepStrictEqual(bc.overflow.ids, ["z1", "z2"]); assert.strictEqual(bc.overflow.n, 2);
assert.strictEqual(W.board(bc, null, 0).plans, W.SLOT_CAP + 2);

// 6. the plan-shaped record a staked plot carries: its steps as one bed, its decision as the one human wait
var sp = W.stakedPlan(seeds[1]);
assert.deepStrictEqual([sp.id, sp.state, sp.needs_human, sp.counts.open, sp.phases.length, sp.unphased.id, sp.unphased.tasks.length], ["a1", "asked", 1, 12, 0, "", 12]);
assert.deepStrictEqual([W.isStaked(seeds[0]), W.isStaked(seeds[1]), W.isStaked(seeds[2]), W.isStaked(null)], [true, true, false, false]);
var we = W.layout(doc([], [seed("e", "x", 1, "ready")]), null, { now: 1 }).index.e;
assert.deepStrictEqual([we.beds.length, we.beds[0].cells.length], [1, 0], "a ready seed with no steps yet is staked with its empty bed");

// 7. art: the string fence, pale soil and a stake marker per step draw only fills of real colours; the frosted marker differs
var fills = [], ctx = { fillRect: function (x, y, w, h) { fills.push([this._c, x, y, w, h]); }, set fillStyle(v) { if (!/^#[0-9a-f]{6}$/i.test(v)) throw new Error("bad colour " + v); this._c = v; }, get fillStyle() { return this._c; } };
var pal = ART.palette(false), T = W.TILE;
ART.drawStakedBase(ctx, pal, r1.x * T, r1.y * T, r1.beds.map(function (bd) { return { x: bd.x * T, y: bd.y * T, w: bd.w * T, h: bd.h * T }; }));
assert.ok(fills.length > 1000, "a whole plot is painted");
var colours = {}; fills.forEach(function (f) { colours[f[0]] = 1; });
assert.ok(colours[pal.J] && colours[pal.N] && colours[pal.P], "string and pale soil colours are used");
assert.ok(!colours[pal.t], "no planted plot's dark soil");
var mark = function (fr) { fills = []; ART.drawStepStake(ctx, pal, 0, 0, fr); return JSON.stringify(fills); };
assert.notStrictEqual(mark(false), mark(true), "the frosted marker has its own colours");
fills = []; ART.drawPlotFlat(ctx, pal, 0, 0, "staked", [{ x: 32, y: 32, w: 144, h: 144 }], [{ x: 32, y: 32, state: "planned", type: "task" }]);
assert.strictEqual(fills[1][0], pal.J, "the Plan level's frame is the pale string");
`
	world, err := filepath.Abs(filepath.Join("web", "world.js"))
	if err != nil {
		t.Fatal(err)
	}
	art, err := filepath.Abs(filepath.Join("web", "art.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, world, art).CombinedOutput()
	if err != nil {
		t.Fatalf("node staked layout test failed: %v\n%s", err, out)
	}
}

// A closed step's cell (harvest spec 4, build step 4): beadRows picks the base by harvest.base (sheaf, trampled, bare
// stubble for anything else, a row with no harvest included) and layers the badges in priority order, two at 1x and four
// at 2x and above, each inside its 5x5 slot. At night only the lantern's flame stays lit; the rosette's and the seal's
// digits show only on a wide cell; the words say every badge, in the same order.
func TestHarvestCellsUnderNode(t *testing.T) {
	script := `
var ART = require(process.argv[1]), assert = require("assert");
function paint(b, pal) {
  var px = {}, ctx = { fillStyle: "", fillRect: function (x, y, w, h) { for (var j = y; j < y + h; j++) for (var i = x; i < x + w; i++) px[i + "," + j] = this.fillStyle; } };
  ART.drawHarvest(ctx, pal, b, 0, 0); return px;
}
var day = ART.palette(false), night = ART.palette(true);
var full = { state: "closed", harvest: { closed: 1, base: "sheaf", mr: "#825", ux: [5, 5], pics: 9, tests: 3, ci: "success", rounds: 2 } };
var stubble = ART.beadRows({ t: "task", s: "done", wild: true }, 0);

// the base, by harvest.base
var rowsOf = function (h) { return ART.beadRows(ART.harvestBead({ state: "closed", harvest: h }, false), 0); };
assert.deepStrictEqual(ART.beadRows(ART.harvestBead({ state: "closed" }, false), 0), stubble, "a closed row with no harvest is bare stubble");
assert.deepStrictEqual(rowsOf({ closed: 1 }), stubble, "a harvest with no base is bare stubble");
assert.notDeepStrictEqual(rowsOf({ closed: 1, base: "sheaf" }), stubble, "a sheaf is not stubble");
assert.notDeepStrictEqual(rowsOf({ closed: 1, base: "trampled" }), stubble, "trampled is not stubble");
assert.notDeepStrictEqual(rowsOf({ closed: 1, base: "sheaf" }), rowsOf({ closed: 1, base: "trampled" }));
[rowsOf({ base: "sheaf" }), rowsOf({ base: "trampled" })].forEach(function (m) {
  assert.strictEqual(m.length, 16); m.forEach(function (r) { assert.strictEqual(r.length, 16); });
  m.forEach(function (r, j) { for (var i = 0; i < 16; i++) if (r.charAt(i) !== ".") assert.ok(i < 11 && j >= 5, "a base keeps clear of the badge slots: " + i + "," + j); });
});

// the badges: priority crate, rosette, vial, lantern, photo, seal; 2 at 1x, 4 at 2x+
assert.deepStrictEqual(ART.BADGE_ORDER, ["crate", "rosette", "vial", "lantern", "photo", "seal"]);
assert.deepStrictEqual(ART.harvestBadges(full.harvest).map(function (b) { return b.k; }), ART.BADGE_ORDER);
assert.deepStrictEqual(ART.harvestBead(full, false).badges, ["crate", "rosette"]);
assert.deepStrictEqual(ART.harvestBead(full, true).badges, ["crate", "rosette", "vial", "lantern"]);
var part = { state: "closed", harvest: { closed: 1, base: "sheaf", pics: 2, tests: 1, ci: "failed", rounds: 1 } };
assert.deepStrictEqual(ART.harvestBead(part, false).badges, ["vial", "photo"], "a failed ci lights no lantern");
assert.deepStrictEqual(ART.harvestBead(part, true).badges, ["vial", "photo", "seal"]);
assert.deepStrictEqual(ART.harvestBead({ state: "closed", harvest: { closed: 1, ux: [0, 0], tests: 0, pics: 0, rounds: 0 } }, true).badges, [], "zero lights nothing");

// each badge lands inside its slot: the top row right to left, then down the right edge
var SLOTS = [[11, 0], [6, 0], [11, 5], [11, 10]], base = ART.beadRows({ t: "task", s: "done", wild: true, base: "sheaf" }, 0);
var lit = ART.beadRows(ART.harvestBead(full, true), 0);
lit.forEach(function (r, j) { for (var i = 0; i < 16; i++) if (r.charAt(i) !== base[j].charAt(i)) {
  assert.ok(SLOTS.some(function (s) { return i >= s[0] && i < s[0] + 5 && j >= s[1] && j < s[1] + 5; }), "a badge pixel outside its slot: " + i + "," + j);
} });
SLOTS.forEach(function (s) { var n = 0; for (var j = s[1]; j < s[1] + 5; j++) for (var i = s[0]; i < s[0] + 5; i++) if (lit[j].charAt(i) !== ".") n++; assert.ok(n >= 8, "slot " + s + " drawn"); });

// a wide cell is 32x32, with the gates passed on the rosette and the rounds on the seal; a 1x cell has no digit
var wide = paint(ART.harvestBead(full, true), day), maxX = 0;
Object.keys(wide).forEach(function (k) { maxX = Math.max(maxX, +k.split(",")[0]); });
assert.ok(maxX >= 16 && maxX < 32, "a wide cell paints into 32 px");
var three = { state: "closed", harvest: { closed: 1, base: "sheaf", mr: "#1", ux: [3, 5] } };
assert.notDeepStrictEqual(paint(ART.harvestBead(three, true), day), paint(ART.harvestBead({ state: "closed", harvest: { closed: 1, base: "sheaf", mr: "#1", ux: [5, 5] } }, true), day), "the rosette says the gates passed");
assert.deepStrictEqual(paint(ART.harvestBead(three, false), day), paint(ART.harvestBead({ state: "closed", harvest: { closed: 1, base: "sheaf", mr: "#1", ux: [5, 5] } }, false), day), "no digit at 1x");
var seal = function (r) { return paint(ART.harvestBead({ state: "closed", harvest: { closed: 1, base: "sheaf", rounds: r } }, true), day); };
assert.notDeepStrictEqual(seal(2), seal(3), "the seal says its rounds from 2");
assert.notDeepStrictEqual(seal(1), seal(2));

// night: every colour of a full wide cell is the night palette's, and only the lantern's flame keeps its day colour
var dayCols = {}; Object.keys(day).forEach(function (k) { if (k !== "id") dayCols[day[k]] = k; });
var lantern = SLOTS[ART.harvestBead(full, true).badges.indexOf("lantern")], g = 0;
var dark = paint(ART.harvestBead(full, true), night);
Object.keys(dark).forEach(function (p) {
  var c = dark[p], xy = p.split(",").map(Number);
  if (dayCols[c] === undefined) return;   // a dimmed colour
  assert.strictEqual(dayCols[c], "G", "only the lantern stays lit at night, not " + dayCols[c] + " at " + p);
  assert.ok(xy[0] >= lantern[0] * 2 && xy[0] < (lantern[0] + 5) * 2 && xy[1] >= lantern[1] * 2 && xy[1] < (lantern[1] + 5) * 2, "the lit flame is the lantern's: " + p);
  g++;
});
assert.ok(g > 0, "the lantern is lit at night");

// words, for the hot's accessible name
assert.deepStrictEqual(ART.harvestWords(full), ["harvested", "merged #825", "graded 5/5 gates", "3 tests", "checks green", "9 pictures", "merged after 2 rounds"]);
assert.deepStrictEqual(ART.harvestWords({ state: "closed", harvest: { closed: 1, base: "trampled", rounds: 1, pics: 1, tests: 1 } }), ["abandoned", "1 test", "1 picture", "merged first time"]);
assert.deepStrictEqual(ART.harvestWords({ state: "closed" }), ["closed"]);
`
	runNodeLogic(t, "art.js", script)
}

// plans.js keeps its patch/tile logic pure so it runs without a browser: apply a patch, reject a gap, size a tile.
// Skipped when node is not installed.
func TestPlansLogicUnderNode(t *testing.T) {
	script := `
var P = require(process.argv[1]), assert = require("assert");
var doc = P.normalize({ v: 10, plans: [{ id: "a", title: "A", activity: 5, operator: "oidc:me" }, { id: "b", title: "B", activity: 9 }], seeds: [] });
assert.deepStrictEqual(doc.plans.map(function (p) { return p.id; }), ["b", "a"], "sorted newest first");
assert.strictEqual(doc.meadow.total, 0, "missing singletons default");
assert.strictEqual(P.applyPatch(doc, { v: 12, prev: 11 }), null, "a gap returns null so the page reconnects");
var d2 = P.applyPatch(doc, { v: 11, prev: 10, plans: { up: [{ id: "a", title: "A2", activity: 20 }, { id: "c", title: "C", activity: 1 }], rm: ["b"] }, piles: { closed_work: 3 } });
assert.deepStrictEqual(d2.plans.map(function (p) { return p.id + p.title; }), ["aA2", "cC"]);
assert.strictEqual(d2.piles.closed_work, 3);
assert.deepStrictEqual(doc.releases, {}, "no releases yet is an empty blob");
assert.strictEqual(P.applyPatch(d2, { v: 12, prev: 11, releases: { gardenio: { shipping: true } } }).releases.gardenio.shipping, true, "a releases patch replaces the blob");
assert.strictEqual(P.applyPatch(d2, { v: 12, prev: 11 }).releases, d2.releases, "a patch without releases keeps the held blob");
assert.deepStrictEqual(doc.escalations, { open: [], total: 0 }, "no escalations yet is an empty blob");
var esc = { open: [{ id: "e", kind: "escalation", family: "merge", executor: "g" }], total: 1 }, d5 = P.applyPatch(d2, { v: 12, prev: 11, escalations: esc });
assert.strictEqual(d5.escalations, esc, "an escalations patch replaces the blob");
assert.strictEqual(P.applyPatch(d5, { v: 13, prev: 12 }).escalations, esc, "a patch without escalations keeps the held blob");
var down = { scope: "fleet", as_of: 1, projects: [] }, d4 = P.applyPatch(P.applyPatch(d2, { v: 12, prev: 11, cost: { scope: "fleet", has: true, today_usd: 2, projects: [] } }), { v: 13, prev: 12, cost: down });
assert.strictEqual(d4.cost, down, "a cost patch replaces the blob: the purse reads the rollup going down (gd-rtE07tg6Nh)");
assert.strictEqual(P.applyPatch(d4, { v: 14, prev: 13 }).cost, down, "a patch without cost keeps the held blob");
assert.strictEqual(d2.v, 11);
assert.strictEqual(doc.plans.length, 2, "the held document is not mutated");
// phase stubs (farm_delta.go planDelta): a phase the page holds comes from its copy; a stub it cannot fill is a gap
var ph = function (id, n) { return { id: id, title: id, tasks: [{ id: id + "-t", n: n }] }; };
var d3 = P.applyPatch(P.normalize({ v: 20, plans: [{ id: "p", phases: [ph("x", 1), ph("y", 1)], unphased: { title: "", tasks: [] } }] }),
  { v: 21, prev: 20, plans: { up: [{ id: "p", phases: [{ id: "x", same: 1 }, ph("y", 2)], unphased: { same: 1 } }] } });
assert.strictEqual(d3.plans[0].phases[0].tasks[0].n, 1, "a stubbed phase is the held one");
assert.strictEqual(d3.plans[0].phases[1].tasks[0].n, 2, "a sent phase replaces the held one");
assert.deepStrictEqual(d3.plans[0].unphased, { title: "", tasks: [] }, "a stubbed unphased row is the held one");
assert.strictEqual(P.applyPatch(d3, { v: 22, prev: 21, plans: { up: [{ id: "p", phases: [{ id: "z", same: 1 }] }] } }), null, "a stub for a phase the page does not hold is a gap");
assert.strictEqual(P.applyPatch(d3, { v: 22, prev: 21, plans: { up: [{ id: "q", phases: [{ id: "x", same: 1 }] }] } }), null, "a stub in a plan the page does not hold is a gap");
var now = 1000000;
var t = P.tile({ activity: now - 60, counts: { in_progress: 1, open: 2, closed: 1 }, open_decisions: 1 }, now);
assert.strictEqual(t.crop, "post"); assert.strictEqual(t.total, 4); assert.strictEqual(t.pct, 25); assert.strictEqual(t.needs, 1);
// what waits on whom (gd-TEl9BfDL2z): only a human wait is "needs you"; an approved escalation names its agent
var approved = { id: "e1", kind: "escalation", state: "approved", human: false, waits_on: "gardenio-ingarden-bead-view-cd1", since: now - 60 };
var onlyApproved = { activity: now, counts: { in_progress: 1 }, open_escalations: 1, needs_human: 0, waiting: [approved] };
assert.strictEqual(P.needs(onlyApproved), 0); assert.notStrictEqual(P.tile(onlyApproved, now).crop, "post"); assert.strictEqual(P.stand(onlyApproved, now), "growing");
assert.strictEqual(P.waitText(approved), "escalation approved · waiting on gardenio-ingarden-bead-view-cd1");
assert.strictEqual(P.waitShort(onlyApproved), "waiting on gardenio-ingarden-bead-view-cd1");
assert.strictEqual(P.waitLine(onlyApproved), "escalation approved · waiting on gardenio-ingarden-bead-view-cd1");
assert.strictEqual(P.waitText({ kind: "decision", state: "unanswered", human: true }), "decision waiting on you");
assert.strictEqual(P.waitText({ kind: "escalation", state: "pending", human: true }), "escalation to approve");
assert.strictEqual(P.waitText({ kind: "escalation", state: "rejected", human: false, waits_on: "crew-a" }), "escalation rejected, still open · waiting on crew-a");
assert.strictEqual(P.waitText({ kind: "escalation", state: "approved", human: false }), "escalation approved · waiting on its executor");
var mixed = { open_decisions: 1, open_escalations: 4, needs_human: 2, waiting: [{ kind: "decision", state: "unanswered", human: true }, { kind: "escalation", state: "pending", human: true },
  approved, { kind: "escalation", state: "approved", human: false, waits_on: "laptop-claude" }, { kind: "escalation", state: "failed", human: false, waits_on: "crew-a" }] };
assert.deepStrictEqual(P.waitPhrases(mixed), [{ text: "1 decision waiting on you", human: true }, { text: "1 escalation to approve", human: true },
  { text: "2 escalations approved · waiting on gardenio-ingarden-bead-view-cd1, laptop-claude", human: false }, { text: "escalation failed, still open · waiting on crew-a", human: false }]);
assert.strictEqual(P.waitShort(mixed), "2 need you", "a human wait wins the short sign");
// a wait that falls to the operator (gd-g7jfH27efD) is a human wait that names its party, with its due time when known
var opWait = { id: "e9", kind: "escalation", state: "approved", human: true, waits_on: "the operator", due: Date.UTC(2026, 9, 19, 21, 30) / 1000, since: now - 60 };
assert.strictEqual(P.waitText(opWait), "escalation approved · waiting on the operator · due 2026-10-19 21:30Z");
assert.strictEqual(P.waitText({ kind: "escalation", state: "approved", human: true, waits_on: "the operator" }), "escalation approved · waiting on the operator");
var opPlan = { open_escalations: 2, needs_human: 1, waiting: [opWait, approved] };
assert.deepStrictEqual(P.waitPhrases(opPlan), [{ text: "escalation approved · waiting on the operator · due 2026-10-19 21:30Z", human: true },
  { text: "escalation approved · waiting on gardenio-ingarden-bead-view-cd1", human: false }], "no phantom 'decision waiting on you' for the operator wait");
assert.strictEqual(P.waitShort(opPlan), "1 needs you", "the operator wait counts as needs you");
assert.strictEqual(P.waitPhrases({ open_escalations: 15, needs_human: 0, waiting: [approved] }).pop().text, "+14 more open", "a capped list says what it left out");
assert.strictEqual(P.needs({ open_decisions: 1, open_escalations: 1 }), 2, "a document without needs_human counts every open one");
assert.deepStrictEqual(P.waitPhrases({ activity: now, counts: {} }), [], "nothing open, nothing said");
assert.strictEqual(P.tile({ activity: now - 13 * 3600, counts: { open: 1 } }, now).crop, "wilt");
assert.strictEqual(P.tile({ activity: now, counts: { review: 1, open: 1 } }, now).crop, "fruit");
assert.strictEqual(P.tile({ activity: now, counts: { blocked: 1 } }, now).crop, "blocked");
assert.strictEqual(P.tile({ activity: now, counts: {} }, now).pct, 0, "no divide by zero");
var sm = P.summary([{ activity: now, counts: { in_progress: 1 } }, { activity: now, counts: { blocked: 1 } }, { activity: now - 13 * 3600, counts: { open: 1 } }, { activity: now, counts: { open: 1 }, open_escalations: 1 }], now);
assert.strictEqual(sm.total, 4); assert.deepStrictEqual(sm.by, { post: 1, blocked: 1, fruit: 0, sprout: 1, wilt: 1 });
assert.strictEqual(P.mine("oidc:me", "oidc:me"), true); assert.strictEqual(P.mine("x", "oidc:me"), false);
// the forest: one limb per phase (bottom first, alternating sides), one leaf per counted step, nothing dropped silently
var plan = { counts: { in_progress: 1 }, phases: [{ title: "P1 Build", counts: { closed: 2, in_progress: 1 } }, { title: "P2", counts: {} }], unphased: { counts: { open: 1 } } };
var tr = P.tree(plan);
assert.strictEqual(tr.branches.length, 3, "two phases plus the unphased limb");
assert.deepStrictEqual(tr.branches[0].leaves.map(function (l) { return l.state; }), ["closed", "closed", "in_progress"], "closed at the trunk end, live work at the tip");
assert.ok(tr.branches[0].y1 > tr.branches[1].y1, "phase 1 is the lowest limb"); assert.notStrictEqual(tr.branches[0].side, tr.branches[1].side);
assert.strictEqual(tr.branches[1].empty, true, "a phase with no steps is a bare limb"); assert.strictEqual(tr.branches[2].label, "", "the unphased limb is unlabelled");
assert.strictEqual(tr.crew, 1);
var big = P.phaseLeaves({ closed: 10, open: 5 });
assert.strictEqual(big.leaves.length, P.TREE.MAX_LEAVES); assert.strictEqual(big.over, 3, "the clip is reported"); assert.strictEqual(big.leaves[big.leaves.length - 1], "open", "the live leaves survive the clip");
var clip = P.phaseLeaves({ closed: 15, open: 14, blocked: 1, deferred: 2 });
assert.ok(clip.leaves.indexOf("blocked") >= 0 && clip.leaves.indexOf("closed") < 0, "a clip drops finished work, never the blocked step");
var many = []; for (var i = 0; i < 12; i++) many.push({ title: "p" + i, counts: { open: 1 } });
var mt = P.tree({ phases: many }); assert.strictEqual(mt.branches.length, P.TREE.MAX_BRANCHES); assert.strictEqual(mt.overflow, 3);
assert.strictEqual(P.tree({ phases: [] }).bare, true, "a plan with no phases is a seedling, not nothing");
assert.strictEqual(P.tree({ phases: [{ title: "x".repeat(40), counts: { open: 1 } }] }).branches[0].label.length, P.TREE.LABEL + 1, "long labels are budgeted");
assert.strictEqual(P.stand({ activity: now, counts: { in_progress: 1 }, open_escalations: 1 }, now), "needs");
assert.strictEqual(P.stand({ activity: now, counts: { review: 1 } }, now), "growing");
assert.strictEqual(P.stand({ activity: now, counts: { open: 1 } }, now), "planted");
assert.strictEqual(P.safeURL("javascript:alert(1)"), ""); assert.strictEqual(P.safeURL("https://x/y"), "https://x/y");
// what a plan has cost (gd-BL2JQbsfyy): the rollup said as /plans says it, a dash before it, stale when it stopped moving
var spent = { has: true, usd: 12.4, crew: 3, synced_at: now - 300 }, stale = { has: true, usd: 6.8, crew: 1, live: 1, stale: true, synced_at: now - 7200 }, none = { has: false, usd: null };
assert.strictEqual(P.spendText(spent), "$12.40, 3 gardeners");
assert.strictEqual(P.spendText({ has: true, usd: 0.4, crew: 1 }), "$0.40, 1 gardener");
assert.strictEqual(P.spendText(stale), "$6.80, 1 gardener · stale");
assert.strictEqual(P.spendText(none), "—", "no rollup yet is a dash, never $0");
assert.strictEqual(P.spendText(undefined), "—");
var pa = { id: "a", activity: now, cost: none }, pb = { id: "b", activity: now - 60, cost: spent }, pc = { id: "c", activity: now - 120, cost: stale }, pd = { id: "d", activity: now - 30, cost: none };
assert.deepStrictEqual(P.sortPlans([pa, pd, pb, pc], "spend").map(function (p) { return p.id; }), ["b", "c", "a", "d"], "most spent first, no figure last, newest among equals");
var keep = [pa, pb]; assert.strictEqual(P.sortPlans(keep, "newest"), keep, "newest keeps the document's order");
var sdoc = { plans: [pa, pb], seeds: [{ id: "s", cost: { has: true, usd: 1 } }], closed_plans: { shown: [{ id: "h", cost: none }] } };
assert.strictEqual(P.spendShown(sdoc), true);
assert.strictEqual(P.spendShown({ plans: [pa], seeds: [], closed_plans: { shown: [] } }), false, "a document with no figure anywhere says nothing about spend");
assert.strictEqual(P.costOf(sdoc, "b"), spent); assert.strictEqual(P.costOf(sdoc, "s").usd, 1); assert.strictEqual(P.costOf(sdoc, "h"), none);
assert.strictEqual(P.costOf(sdoc, "zz"), null, "a plan the farm does not hold");
var harvested = { has: true, usd: 5.8, crew: 4 }; sdoc.closed_plans.shown.push({ id: "h2", cost: harvested }); sdoc.seeds.push({ id: "s2", cost: { has: true, usd: 0.9, crew: 1 } });
assert.strictEqual(P.spendText(P.costOf(sdoc, "h2")), "$5.80, 4 gardeners", "a closed plan's slim cost carries its crew, as the /plans card counts it");
assert.strictEqual(P.spendText(P.costOf(sdoc, "s2")), "$0.90, 1 gardener", "so does a seed's");
`
	runNodeLogic(t, "plans.js", script)
}

// Phase 3 layout: the docked Porch/Mailbox panes and the phone tab bar are part of the served page, and the
// tab bar only exists as a phone layout (hidden by default, shown under the 900px breakpoint).
func TestPageHasDockAndTabBar(t *testing.T) {
	_, rec := servedPage(t)
	body := rec.Body.String()
	for _, want := range []string{
		`id="tabbar"`, `data-go="farm"`, `data-go="commons"`, `data-go="porch"`, `data-go="mail"`, `data-go="workbench"`,
		`id="dock"`, `id="pane-porch"`, `id="pane-mail"`, `id="dbtn-porch"`, `id="dbtn-mail"`,
		`id="chat-open"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	if strings.Contains(body, `data-go="glass"`) != strings.Contains(body, `data-glass="1"`) {
		t.Error("the Glass tab is offered exactly when the page offers the Glass (M1.4, gd-4Y2qOgfmJE)")
	}
	if strings.Contains(body, `id="cm-open"`) {
		t.Error("the phone's Commons is a tab (step M1, gd-4hal7KPuxv), not a button on the map")
	}
	if !strings.Contains(pageCSS, ".tabbar{display:none}") || !strings.Contains(pageCSS, "@media (max-width:899px)") {
		t.Error("tab bar must be hidden by default and enabled only in the phone media query")
	}
	if !strings.Contains(pageCSS, "env(safe-area-inset-bottom") {
		t.Error("tab bar must respect the bottom safe-area inset")
	}
}

// The escalation buttons are offered only to a viewer who may use them, and the page cache key knows the difference.
func TestEscalationButtonsFollowCapability(t *testing.T) {
	f := newFake()
	h := New(f, "/v", nil)
	_ = h.refresh(context.Background())
	can := false
	mux := http.NewServeMux()
	(&Handler{Hub: h, BasePath: "/v", CanApproveEscalation: func(*http.Request) bool { return can }}).Register(mux)
	get := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden", nil))
		return rec
	}
	no := get()
	can = true
	yes := get()
	if strings.Contains(no.Body.String(), `data-esc="1"`) || !strings.Contains(yes.Body.String(), `data-esc="1"`) {
		t.Error("data-esc must be set exactly when the viewer can approve")
	}
	if no.Header().Get("ETag") == yes.Header().Get("ETag") {
		t.Error("ETag must differ by capability, or a 304 would serve the wrong buttons")
	}
	for _, want := range []string{`id="toast"`, "Answer them here."} {
		if !strings.Contains(yes.Body.String(), want) {
			t.Errorf("page missing %s", want)
		}
	}
}

// The in-place answers (web/answer.js) post to the viewer's existing endpoints; pin the paths so a rename there fails here.
func TestAnswerScriptTargetsViewerEndpoints(t *testing.T) {
	for _, want := range []string{"'/b/' + id + '/decision/resolve'", "'/b/' + id + '/escalation/' + kind", "option_id", "response_text"} {
		if !strings.Contains(pageAnswerJS, want) {
			t.Errorf("answer.js missing %s", want)
		}
	}
	if !strings.Contains(pageAppJS, "ANSWER.install(") {
		t.Errorf("garden.js does not install web/answer.js")
	}
	// The viewer answers its writes in JSON when asked (gd-jgVYg6kH5K); the Mailbox no longer reads the HTML banner.
	if !strings.Contains(pageAnswerJS, "'Accept': 'application/json'") {
		t.Errorf("answer.js does not ask the viewer for JSON")
	}
	if strings.Contains(pageAnswerJS, "DOMParser") || strings.Contains(pageAnswerJS, "[role=alert]") {
		t.Errorf("answer.js still scrapes the viewer's HTML")
	}
}

// answer.js's failure(): what the Mailbox tells the viewer for each answer a viewer write route gives, under node.
func TestAnswerFailureUnderNode(t *testing.T) {
	script := `
var A = require(process.argv[1]), assert = require("assert");
assert.strictEqual(A.failure(200, { ok: true, bead: "gd-x" }), "", "went through");
assert.strictEqual(A.failure(204, null), "", "a 2xx with no body went through");
assert.strictEqual(A.failure(409, { ok: false, error: "Decision is already  resolved.\n", status: 409 }), "Decision is already resolved.", "the banner's words, tidied");
assert.strictEqual(A.failure(403, { ok: false, error: "", status: 403 }), "You are not allowed to do that.", "no words: the status says it");
assert.strictEqual(A.failure(502, null), "That did not go through (502).", "not JSON");
assert.strictEqual(A.failure(200, { ok: false, error: "Stop spec must be valid JSON.", status: 400 }), "Stop spec must be valid JSON.", "ok:false is a failure whatever the status");
`
	runNodeLogic(t, "answer.js", script)
}

// The page is gzipped for clients that accept it (the HTML is ~120 KB of inline assets), identical when decoded,
// and the cache keeps the compressed bytes stable for a given snapshot.
func TestPageIsGzippedWhenAccepted(t *testing.T) {
	mux, plain := servedPage(t)
	req := httptest.NewRequest("GET", "/garden", nil)
	req.Header.Set("Accept-Encoding", "gzip, br")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
		t.Fatalf("headers: encoding %q vary %q", rec.Header().Get("Content-Encoding"), rec.Header().Get("Vary"))
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(zr)
	if string(got) != plain.Body.String() {
		t.Fatalf("decoded page differs from the plain page (%d vs %d bytes)", len(got), plain.Body.Len())
	}
	if plain.Header().Get("Content-Encoding") != "" {
		t.Fatal("a client that did not ask for gzip got it")
	}
	if rec.Body.Len()*3 > len(got) {
		t.Fatalf("gzip only shrank %d -> %d bytes", len(got), rec.Body.Len())
	}
}

// world.js is the camera's pure half, so it runs without a browser: the golden hashes from the design doc, the zoom
// levels (a pure function of zoom), zoom-about-a-point, the centre clamp, fit(), chunk coverage and the DOM cap.
func TestWorldCameraLogicUnderNode(t *testing.T) {
	script := `
var W = require(process.argv[1]), assert = require("assert");
var near = function (a, b, m) { assert.ok(Math.abs(a - b) < 1e-9, (m || "") + " " + a + " vs " + b); };
// golden hashes (homestead-world-design.md section 3.2)
assert.deepStrictEqual([1, 2, 100].map(function (s) { return W.h32("gardenio", s); }), [3263587388, 1292538903, 3456188741]);
assert.deepStrictEqual([1, 2, 100].map(function (s) { return W.h32("music-clonk", s); }), [700575029, 474542740, 1277858624]);
assert.deepStrictEqual([1, 2, 100].map(function (s) { return W.h32("moorpark", s); }), [3971456384, 4134240447, 515575165]);
// levels
assert.deepStrictEqual([0.05, 0.11, 0.3, 0.41, 0.9, 0.92, 1, 4].map(function (z) { return W.lodFor(z); }), [0, 1, 1, 2, 2, 3, 3, 3]);
assert.deepStrictEqual([0.109, 0.409, 0.919].map(function (z) { return W.lodFor(z); }), [0, 1, 2], "just under an edge is the coarser level");
// the level is the same whichever way the camera came (gd-bwI1PZ6jnM): no band about an edge keeps the previous level
[0.115, 0.43, 0.95, 1.05].forEach(function (z) { assert.strictEqual(W.lodFor(z, 1), W.lodFor(z, 3), "zooming in or out to " + z + " draws the same level"); assert.strictEqual(W.lodFor(z, 2), W.lodFor(z), "with or without a previous level"); });
assert.strictEqual(W.lodFor(0.43, 1), 2, "a plan's phases at 0.43x are the Plan depth's beds, zooming in too");
assert.strictEqual(W.lodFor(0.04, 3), 0, "a big jump crosses several levels");
for (var l = 0; l < 4; l++) { var r = W.levelRange(l); assert.strictEqual(W.lodFor(r[0], l), l); assert.strictEqual(W.lodFor((r[0] + r[1]) / 2), l); assert.strictEqual(W.lodFor(r[1] * 0.999, l), l); }
// snap
assert.deepStrictEqual([0.5, 1, 1.2, 1.3, 2.4, 2.6, 3.4, 9].map(W.snapZ), [0.5, 1, 1, 1.5, 2, 3, 3, 4]);
assert.deepStrictEqual([[1, 1], [1.2, 1], [2.1, -1], [1, -1], [4, 1], [3.9, 1]].map(function (a) { return W.snapNext(a[0], a[1]); }), [1.5, 1.5, 1.5, 1, 4, 4], "one resting zoom on, clamped at the ends");
// coordinates round-trip, and zoom keeps the point under the pointer fixed
var vp = { w: 800, h: 600 }, cam = { cx: 100, cy: -50, z: 1.5 };
var wp = W.toWorld(cam, vp, 123, 456), sp = W.toScreen(cam, vp, wp.x, wp.y);
near(sp.x, 123); near(sp.y, 456);
var z2 = W.zoomAt(cam, 2, 123, 456, vp), w2 = W.toWorld(z2, vp, 123, 456);
near(w2.x, wp.x, "x anchor"); near(w2.y, wp.y, "y anchor"); near(z2.z, 3);
assert.strictEqual(W.zoomAt(cam, 1e6, 0, 0, vp).z, W.Z_MAX); assert.strictEqual(W.zoomAt(cam, 1e-9, 0, 0, vp).z, W.Z_MIN);
// panning drags the world with the pointer
var pn = W.panBy({ cx: 0, cy: 0, z: 2 }, 40, -20); near(pn.cx, -20); near(pn.cy, 10);
// the centre cannot leave the world box plus one domain of slack
var box = W.domainRect(0, 0), far = W.clampCam({ cx: 1e9, cy: -1e9, z: 1 }, box);
near(far.cx, box.x + box.w + W.DOM.w * W.TILE); near(far.cy, box.y - W.DOM.h * W.TILE);
near(box.x, -88 * 16); near(box.w, 176 * 16); near(box.y, -72 * 16);
// fit frames a rect, clamps into a level, and tolerates degenerate input
var f = W.fit({ x: -192, y: -120, w: 384, h: 240 }, { w: 960, h: 600 }); near(f.z, 2.5); near(f.cx, 0); near(f.cy, 0);
assert.strictEqual(W.lodFor(W.fit(box, { w: 960, h: 600 }, { level: 1 }).z), 1);
assert.strictEqual(W.lodFor(W.fit({ x: 0, y: 0, w: 4, h: 4 }, { w: 960, h: 600 }, { level: 2 }).z), 2, "zooming into a tiny rect stops at the level");
assert.ok(W.fit({ x: 0, y: 0, w: 0, h: 0 }, vp).z > 0);
// culling and the DOM cap
var items = []; for (var i = 0; i < 1000; i++) items.push({ x: (i % 40) * 20, y: Math.floor(i / 40) * 20, w: 16, h: 16, i: i });
var view = W.viewRect({ cx: 400, cy: 250, z: 1 }, { w: 200, h: 100 }, 0);
var vis = W.cull(items, view); assert.ok(vis.length > 0 && vis.length < 100); vis.forEach(function (it) { assert.ok(W.hit(it, view)); });
var capped = W.nearest(items, 400, 250, 300); assert.strictEqual(capped.length, 300);
assert.deepStrictEqual(capped.map(function (x) { return x.i; }), capped.map(function (x) { return x.i; }).sort(function (a, b) { return a - b; }), "original order kept");
var d = function (it) { return Math.hypot(it.x + 8 - 400, it.y + 8 - 250); };
var worstKept = Math.max.apply(null, capped.map(d)), kept = {}; capped.forEach(function (x) { kept[x.i] = 1; });
items.forEach(function (it) { if (!kept[it.i]) assert.ok(d(it) >= worstKept - 1e-9, "a dropped item is no nearer than any kept one"); });
assert.strictEqual(W.nearest(items.slice(0, 5), 0, 0, 300).length, 5);
// chunks: every point of the view is covered, at every mip, and coarser mips need fewer
var v = { x: -3000, y: -2000, w: 6000, h: 4000 }, last = Infinity;
[1, 4, 16].forEach(function (m) {
  var cs = W.chunks(v, m), s = W.chunkSpan(m); assert.ok(cs.length <= last); last = cs.length;
  [[v.x, v.y], [v.x + v.w, v.y + v.h], [0, 0], [-1, -1]].forEach(function (p) { assert.ok(cs.some(function (c) { return p[0] >= c.i * s && p[0] < (c.i + 1) * s && p[1] >= c.j * s && p[1] < (c.j + 1) * s; }), "covered " + p); });
});
assert.ok(W.chunks({ x: 0, y: 0, w: 1, h: 1 }, 1).length >= 1);
// LRU evicts the oldest, and a read refreshes
var c = W.lru(2); c.set("a", 1); c.set("b", 2); c.get("a"); c.set("c", 3);
assert.strictEqual(c.get("b"), undefined); assert.strictEqual(c.get("a"), 1); assert.strictEqual(c.size, 2);
`
	runNodeLogic(t, "world.js", script)
}

// world.js layout is pure, so the design contract (cursor-grid spec gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md
// sections 5.1-5.4, test plan 10.2) runs under node: the lattice golden (the Commons cell 0, projects from cell 1 oldest
// first), determinism under shuffling, coordinate stability with a memo (no project's index changes because of another Thing; a
// plot moves only when a plan ahead of it finishes, as packed slots close the gap), memo v1/v2 to v3 migration keeping slots, the coordinate helpers, the spiral, the plot cap and overflow
// marker, no overlap inside a region, the 3x3 beds with block-major 9x9 cells, the crates and the empty document.
func TestWorldLayoutUnderNode(t *testing.T) {
	nodeScript(t, "world_layout_test.js")
}

// The courtyard's cells sized by their counts (homestead-world-design.md 29.10, X-D13; step 12 gd-nOkEhUBDk4) run under node: an
// empty farm at every minimum, each cell's ladder a column a step, each cell full at its maximum and then one more (the
// Escalations' crate, the field's badge, the Pen's and the Mailbox's "+N"), a full Pen and field side by side, the shrink
// hysteresis with prev, one count change moving only that farm's cells, and walkLanes from a field stand when the arms differ.
func TestHomeCellsUnderNode(t *testing.T) {
	if r := nodeScript(t, "home_cells_test.js"); r["ok"] != true {
		t.Fatalf("home cells report: %v", r)
	}
}

// The farm grows with its cells (homestead-world-design.md 29.11, X-D14; step 15 gd-gFMObwPAzs) run under node: W at each row of
// 29.11's table and at the step's edges, the slide (a biome at g is its g = 0 biome with every rect east of the cut g east and
// every rect across it g wider, every band item on its cell), the region's height and the Graveyard's rows at every g, the
// lattice on base widths (a step moves no other project in its column and the columns east only by its column's change), the
// hysteresis through W, a full Pen and field with no squeeze, and the sign at fx + 12 at every W.
func TestFarmGrowsUnderNode(t *testing.T) {
	if r := nodeScript(t, "farm_grows_test.js"); r["ok"] != true {
		t.Fatalf("farm grows report: %v", r)
	}
}

// The outline (design section 8) is pure in world.js, so its contract runs under node: roots mirror the biomes in world order,
// a biome's plots and its farmstead come in grid order (29.6) with their task cells in bed order, every id in world.index appears exactly once with the
// same `kind:id` its hot link uses, the natural levels are project 1 / plan 2 / bead 3, and the empty world is an empty outline.
// wantHot is the 44 px rule for coarse pointers on top of the per-level masks.
func TestWorldOutlineUnderNode(t *testing.T) {
	script := `
var W = require(process.argv[1]), assert = require("assert");
function plan(id, project, created, n) {
  var tasks = []; for (var i = 0; i < (n || 0); i++) tasks.push({ id: id + "-t" + i, title: "T" + i, type: i % 3 ? "task" : "decision", state: ["open", "in_progress", "review", "blocked", "deferred"][i % 5], updated: 1, assignee: i % 2 ? "crew" : "" });
  return { id: id, title: "Plan " + id, project: project, created: created, activity: created, counts: { open: n || 0, in_progress: 1 }, open_decisions: id === "p1" ? 2 : 0,
    phases: [{ id: id + "-p1", title: "Phase 1", counts: { open: n || 0, closed: 2 }, tasks: tasks.slice(0, 2) }, { id: id + "-p2", title: "Phase 2", counts: { open: n || 0 }, tasks: tasks.slice(2) }], unphased: { id: "", title: "Other", counts: {}, tasks: [] } };
}
function doc(plans, extra) { var d = { plans: plans, seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] }, meta: { truncated: false } }; if (extra) Object.keys(extra).forEach(function (k) { d[k] = extra[k]; }); return d; }
var ps = []; for (var i = 0; i < 6; i++) ps.push(plan("p" + i, i % 2 ? "gardenio" : "music-clonk", 10 - i, 4));
var md = { total: 3, shown: [{ id: "m1", title: "Loose 1", project: "gardenio", state: "open", updated: 1 }, { id: "m2", title: "Loose 2", project: "", state: "in_progress", updated: 1 }, { id: "m3", title: "Loose 3", project: "gardenio", state: "open", updated: 1 }] };
var sd = [{ id: "s1", title: "Seed 1", project: "gardenio", state: "frosted", phases: 3 }, { id: "s2", title: "Seed 2", project: "music-clonk", state: "ready" }];
var cp = { total: 2, shown: [{ id: "c1", title: "Done 1", project: "gardenio", state: "complete", updated: 1 }, { id: "c2", title: "Done 2", project: "music-clonk", updated: 1 }] };
var es = { open: [{ id: "x1", title: "Merge it", kind: "escalation", state: "pending", human: true, since: 1, project: "gardenio", family: "merge" }], total: 1 };
var wo = { open: [{ id: "wg1", title: "Newer wish", project: "gardenio", opened: 2 }, { id: "wm1", title: "Clonk wish", project: "music-clonk", opened: 3 }, { id: "wg2", title: "Older wish", project: "Gardenio", opened: 1 }, { id: "wc1", title: "Loose wish", opened: 4 }] };
var w = W.layout(doc(ps, { meadow: md, seeds: sd, closed_plans: cp, escalations: es, wishes: wo }), null, { now: 5 }), out = W.outline(w);
// roots mirror the biomes, in world order; a biome's hot is its sign (the blob is World-level only)
assert.deepStrictEqual(out.map(function (r) { return r.id; }), w.biomes.map(function (b) { return b.id; }));
assert.ok(out.length === 3, "two projects and the Commons");
out.forEach(function (r) { assert.strictEqual(r.kind, "biome"); assert.strictEqual(r.hot, "sign:" + r.id); assert.strictEqual(r.level, 1); assert.strictEqual(r.title, w.index[r.id].name); });
// every id in the index appears exactly once, and nothing that is not in the index appears
var seen = {}; function walk(n) { (n.children || []).forEach(function (c) { seen[c.id] = (seen[c.id] || 0) + 1; walk(c); }); }
out.forEach(function (r) { seen[r.id] = (seen[r.id] || 0) + 1; walk(r); });
Object.keys(w.index).forEach(function (id) { assert.strictEqual(seen[id], 1, "once in the outline: " + id); });
Object.keys(seen).forEach(function (id) { assert.ok(w.index[id], "outline entry not in the index: " + id); });
// the farmstead among the plots in grid order (29.6), task cells in bed order, hot ids as the links use them, natural levels
out.forEach(function (r) {
  var b = w.index[r.id], plots = r.children.filter(function (c) { return c.kind === "plot"; }), grid = r.children.filter(function (c) { return c.kind === "plot" || c.kind === "home"; });
  assert.deepStrictEqual(plots.map(function (p) { return p.id; }).sort(), b.plots.map(function (p) { return p.id; }).sort(), "every plot");
  assert.deepStrictEqual(grid.map(function (c) { return c.id; }), [b.home].concat(b.plots).sort(function (p, q) { return p.y - q.y || p.x - q.x; }).map(function (c) { return c.id; }), "grid order: by row, then west to east");
  assert.deepStrictEqual(r.children.slice(0, grid.length), grid, "the grid's cells lead, the bands after them");
  for (var k = 1; k < grid.length; k++) assert.ok(grid[k - 1].rect.y < grid[k].rect.y || (grid[k - 1].rect.y === grid[k].rect.y && grid[k - 1].rect.x < grid[k].rect.x));
  plots.forEach(function (p) {
    var cells = []; w.index[p.id].beds.forEach(function (bd) { bd.cells.forEach(function (c) { cells.push(c.id); }); });
    assert.deepStrictEqual(p.children.map(function (t) { return t.id; }), cells, "bed order");
    assert.strictEqual(p.hot, "plot:" + p.id); assert.strictEqual(p.level, 2); assert.strictEqual(p.title, w.index[p.id].plan.title);
    p.children.forEach(function (t) { assert.strictEqual(t.hot, "task:" + t.id); assert.strictEqual(t.level, 3); assert.ok(t.phase === "Phase 1" || t.phase === "Phase 2"); assert.ok(typeof t.state === "string"); });
  });
  // the farmstead is the plot grid's centre cell, slot 0 (29.1); its zones in the courtyard's reading order (29.2, 29.6): Barn,
  // Escalations, Mailbox on the top row, Pen and Growing field below; the loose beads in the Growing field and the goats in the
  // Pen; the bands after the grid: seeds, then harvested plans, each left to right
  var hm = grid.filter(function (c) { return c.kind === "home"; })[0], kinds = r.children.slice(grid.length).map(function (c) { return c.kind; }), order = { overflow: 1, seed: 2, tree: 3, stone: 4 };
  assert.deepStrictEqual([hm.kind, hm.id, hm.level, hm.rect, hm.children.map(function (z) { return z.id; })], ["home", b.home.id, 1, { x: b.home.x, y: b.home.y, w: W.SLOT.w + b.grid.g, h: W.SLOT.h }, ["barn", "escalation", "mailbox", "pen", "growing"].map(function (z) { return b.home.id + "/" + z; })]);
  hm.children.forEach(function (z) { var c = b.home.cells[z.id.split("/")[1]]; assert.deepStrictEqual(z.rect, { x: c.x, y: c.y, w: c.w, h: c.h }, "a zone is its courtyard cell: " + z.id); });
  hm.children[4].children.forEach(function (f) { assert.deepStrictEqual([f.kind, f.level, f.hot], ["flower", 3, "flower:" + f.id]); });
  for (k = 1; k < kinds.length; k++) assert.ok(order[kinds[k - 1]] <= order[kinds[k]], "band order in " + r.id + ": " + kinds.join(","));
  ["seed", "tree"].forEach(function (kd) { var xs = r.children.filter(function (c) { return c.kind === kd; }).map(function (c) { return c.rect.x; }); for (var q = 1; q < xs.length; q++) assert.ok(xs[q - 1] <= xs[q], kd + " left to right"); });
  r.children.slice(grid.length).forEach(function (c) { assert.strictEqual(c.hot, c.kind + ":" + c.id); assert.strictEqual(c.level, c.kind === "overflow" ? 2 : 3); });
});
function homeOf(r) { return r.children.filter(function (c) { return c.kind === "home"; })[0]; }
function zoneOf(r, z) { return homeOf(r).children.filter(function (c) { return c.id === homeOf(r).id + "/" + z; })[0]; }
var g = out.filter(function (r) { return r.id === "project:gardenio"; })[0];
assert.strictEqual(g.needs, 2, "needs carried"); assert.strictEqual(g.children.filter(function (c) { return c.kind === "plot" && c.id === "p1"; })[0].needs, 2);
assert.strictEqual(g.children.filter(function (c) { return c.kind === "seed"; })[0].title, "Seed 1");
assert.strictEqual(g.children.filter(function (c) { return c.kind === "tree"; })[0].title, "Done 1");
assert.strictEqual(zoneOf(g, "growing").children.length, 2, "two loose beads in gardenio's Growing field");
assert.deepStrictEqual(zoneOf(g, "escalation").children.map(function (e) { return [e.kind, e.id, e.hot, e.level, e.title, e.state, e.family]; }), [["escalation", "esc:x1", "escalation:esc:x1", 3, "Merge it", "pending", "merge"]], "its escalation in the Escalations");
assert.strictEqual(zoneOf(out.filter(function (r) { return r.key === ""; })[0], "growing").children[0].id, "m2", "the Commons holds the project-less bead");
// two projects with wishes, two pens (28.2, step 8): each project's Pen zone lists only its own goats, oldest first; the Commons
// pens the project-less wish
var pens = {}; out.forEach(function (r) { pens[r.key] = zoneOf(r, "pen").children.map(function (e) { return [e.kind, e.id, e.hot, e.level, e.title]; }); });
assert.deepStrictEqual(pens, { "": [["goat", "wish:wc1", "goat:wish:wc1", 3, "Loose wish"]], "music-clonk": [["goat", "wish:wm1", "goat:wish:wm1", 3, "Clonk wish"]],
  gardenio: [["goat", "wish:wg2", "goat:wish:wg2", 3, "Older wish"], ["goat", "wish:wg1", "goat:wish:wg1", 3, "Newer wish"]] }, "a pen per project, each its own goats");
// overflow: 35 plans give 34 plots (slot 0 is the farmstead, 29.1) and one "+1 more plans" entry at the Plan level
var many = []; for (i = 0; i < 35; i++) many.push(plan("m" + i, "big", i));
var ob = W.outline(W.layout(doc(many), null, { now: 1 }))[0], ov = ob.children.filter(function (c) { return c.kind === "overflow"; });
assert.strictEqual(ob.children.filter(function (c) { return c.kind === "plot"; }).length, 34);
assert.deepStrictEqual(ob.children.slice(0, 35).map(function (c) { return c.kind; }).indexOf("home"), 2 * 7 + 3, "a full 7 x 5: the farmstead is the centre cell, row 2 column 3, in grid order");
assert.deepStrictEqual([ov.length, ov[0].n, ov[0].level, ov[0].hot, ov[0].title], [1, 1, 2, "overflow:overflow:big", "+1 more plans"]);
// a rect for every entry (tiles), so the camera can fit it
out.concat(ob.children).forEach(function (r) { (function chk(n) { assert.ok(n.rect && n.rect.w > 0 && n.rect.h > 0, "rect " + n.id); (n.children || []).forEach(chk); })(r); });
// empty worlds
assert.deepStrictEqual(W.outline(null), []); assert.deepStrictEqual(W.outline(W.layout(null, null, { now: 1 })), []);
// wantHot: per-level masks first ...
var task = { kind: "task", w: 16, h: 16, lv: 8 }, ask = { kind: "task", w: 16, h: 16, lv: 4 | 8 }, plot = { kind: "plot", w: 288, h: 192, lv: 2 | 4 }, sign = { kind: "sign", w: 128, h: 32, lv: 2 | 4 | 8 };
var pot = { kind: "seed", w: 48, h: 48, lv: 8 }, flower = { kind: "flower", w: 16, h: 16, lv: 8 }, bead = { kind: "bead", zone: "growing", w: 16, h: 16 }, dec = { kind: "bead", zone: "decision", w: 16, h: 16 };
assert.ok(W.wantHot(task, 3, 1, false) && !W.wantHot(task, 2, 0.5, false) && W.wantHot(ask, 2, 0.5, false) && !W.wantHot(ask, 1, 0.2, false));
assert.ok(W.wantHot(plot, 2, 0.5, false) && W.wantHot(plot, 1, 0.2, false) && !W.wantHot(plot, 3, 1, false) && !W.wantHot(plot, 0, 0.05, false));
assert.ok(W.wantHot(bead, 3, 1, false) && !W.wantHot(bead, 2, 0.5, false) && W.wantHot(dec, 2, 0.5, false) && !W.wantHot(dec, 1, 0.2, false));
// ... then the 44 px rule on a coarse pointer: a 16 px tile at 1x is 16 CSS px, so it is not a target and the plot stands in;
// at 2.75x the tile is 44 px and the task is back, the plot gone
assert.ok(!W.wantHot(task, 3, 1, true) && W.wantHot(plot, 3, 1, true), "plot stands in");
assert.ok(W.wantHot(task, 3, 2.75, true) && !W.wantHot(plot, 3, 2.75, true), "tile wide enough");
assert.ok(!W.wantHot(flower, 3, 2, true) && W.wantHot(flower, 3, 2, false));
assert.ok(W.wantHot(pot, 3, 1, true), "a 48 px pot is a target at 1x");
assert.ok(W.wantHot(sign, 1, 0.3, true) && W.wantHot(bead, 3, 1, true), "wide things and home plants stay; the CSS pads them");
assert.strictEqual(W.MIN_HOT, 44);
`
	runNodeLogic(t, "world.js", script)
}

// Deep links and the jump index (homestead-world-design.md section 7) are pure world.js, so the
// contract runs under node: at= parse/encode round trips, resolve for project, plan, task, phase, band item, overflow and
// unknown ids (fallback to the project), the Yard and the Commons, what locate() writes back for a camera position, the
// and the search ranking (exact id first, then prefixes, titles, multi-word, cap).
func TestWorldLinksUnderNode(t *testing.T) {
	script := `
var W = require(process.argv[1]), assert = require("assert");
function plan(id, project, created, n, title) {
  var tasks = []; for (var i = 0; i < (n || 0); i++) tasks.push({ id: id + "-t" + i, title: "Task " + i + " of " + id, type: "task", state: ["open", "in_progress", "review"][i % 3], updated: 1 });
  return { id: id, title: title || "Plan " + id, project: project, created: created, state: "ready", counts: { open: n || 0 }, phases: [{ id: id + "-p1", title: "Phase 1", counts: { open: n || 0 }, tasks: tasks }], unphased: { id: "", title: "Other", counts: {}, tasks: [] } };
}
var many = []; for (var i = 0; i < 36; i++) many.push(plan("m" + i, "big", i));
var doc = { plans: [plan("gd-plan1", "gardenio", 1, 4, "Minimap and fast travel"), plan("gd-plan2", "gardenio", 2, 2, "Project biomes"), plan("mc-plan", "Music-Clonk", 3, 1, "Scale picker")].concat(many),
  seeds: [{ id: "gd-seed1", title: "Seed one", project: "gardenio", state: "frosted" }],
  closed_plans: { total: 1, shown: [{ id: "gd-old", title: "Old harvest", project: "gardenio", state: "complete" }] },
  meadow: { total: 1, shown: [{ id: "x-loose", title: "Loose bead", project: "", state: "open", updated: 1 }] }, meta: { truncated: false } };
var w = W.layout(doc, null, { now: 1 });
// parse and encode
assert.deepStrictEqual(W.parseAt("gardenio/gd-plan1"), { raw: "gardenio", project: "gardenio", id: "gd-plan1" });
assert.deepStrictEqual(W.parseAt("Music-Clonk"), { raw: "Music-Clonk", project: "music-clonk", id: "" });
assert.deepStrictEqual(W.parseAt("~/x-loose").project, "", "~ is the Commons");
assert.deepStrictEqual(W.parseAt("a%20b%2Fc/id%2F1"), { raw: "a b/c", project: "a b/c", id: "id/1" }, "segments are decoded, an encoded slash is not a separator");
assert.strictEqual(W.parseAt(""), null); assert.strictEqual(W.parseAt(null), null);
assert.strictEqual(W.atFor(w, "plot", "gd-plan1"), "gardenio/gd-plan1");
assert.strictEqual(W.atFor(w, "task", "gd-plan1-t2"), "gardenio/gd-plan1-t2", "a task finds its project through its plot");
assert.strictEqual(W.atFor(w, "biome", "project:music-clonk"), "music-clonk");
assert.strictEqual(W.atFor(w, "flower", "x-loose"), "~/x-loose");
assert.strictEqual(W.atFor(w, "biome", "project:"), "~");
assert.deepStrictEqual([W.atFor(w, "home", "home:gardenio"), W.atFor(w, "home", "home:"), W.atFor(w, "zone", "home:gardenio/mailbox"), W.atFor(w, "yard", "yard")], ["gardenio/home", "~/home", "gardenio/mailbox", null], "a farmstead and its zones (28.7, 29.6); no link writes yard");
assert.strictEqual(W.atFor(w, "task", "nope"), null);
// the query string: at/z replaced, other parameters kept as written, nothing left when at is dropped
assert.strictEqual(W.linkQuery("", "gardenio/gd-plan1", 2), "?at=gardenio/gd-plan1&z=2");
assert.strictEqual(W.linkQuery("?debug=1&at=old&z=0&k=a%20b", "~", 1), "?debug=1&k=a%20b&at=~&z=1");
assert.strictEqual(W.linkQuery("?at=old&z=0", null, null), "");
assert.strictEqual(W.linkQuery("?x", "yard", null), "?x&at=yard");
assert.strictEqual(W.queryGet("?a=1&at=gardenio%2Fgd-plan1&z=2", "at"), "gardenio%2Fgd-plan1", "raw, so parseAt decodes once");
assert.strictEqual(W.queryGet("?a=1", "at"), null);
assert.deepStrictEqual(["0", "3", "2", "4", "-1", "x", "", null].map(W.levelParam), [0, 3, 2, null, null, null, null, null]);
// resolve: project, plan, task, phase, seed, closed plan, loose bead, overflow, yard
var b = w.index["project:gardenio"], r;
r = W.resolve(w, "gardenio"); assert.deepStrictEqual([r.kind, r.id, r.level, r.found, r.rect], ["biome", "project:gardenio", 1, true, { x: b.used.x, y: b.used.y, w: b.used.w, h: b.used.h }]);
r = W.resolve(w, "Gardenio/gd-plan1"); assert.deepStrictEqual([r.kind, r.id, r.level, r.found, r.biome.key], ["plot", "gd-plan1", 2, true, "gardenio"]);
var cell = w.index["gd-plan1-t2"]; r = W.resolve(w, "gardenio/gd-plan1-t2"); assert.deepStrictEqual([r.kind, r.level, r.rect], ["task", 3, { x: cell.x, y: cell.y, w: 1, h: 1 }]);
r = W.resolve(w, "gardenio/gd-plan1-p1"); assert.deepStrictEqual([r.kind, r.level, r.plot, r.rect.w], ["phase", 2, "gd-plan1", W.BED.size], "a phase id is its bed");
r = W.resolve(w, "gardenio/gd-seed1"); assert.deepStrictEqual([r.kind, r.level], ["seed", 3]);
r = W.resolve(w, "gardenio/gd-old"); assert.deepStrictEqual([r.kind, r.level], ["tree", 3]);
r = W.resolve(w, "~/x-loose"); assert.deepStrictEqual([r.kind, r.level, r.biome.key], ["flower", 3, ""]);
r = W.resolve(w, "big/m35"); assert.deepStrictEqual([r.kind, r.level, r.id], ["overflow", 2, "overflow:big"], "a plan past the cap is the overflow marker");
// home is the farmstead, the plot grid's centre cell (29.1, 29.6): slot 0, 32 tall and W = 32 + g wide (29.11), with the first
// plan's plot east of it
var hg = w.index["home:gardenio"], g1 = b.plots.filter(function (p) { return p.slot === 1; })[0]; r = W.resolve(w, "gardenio/home"); assert.deepStrictEqual([r.kind, r.id, r.level, r.rect, r.biome.key], ["home", "home:gardenio", 1, { x: hg.x, y: hg.y, w: W.SLOT.w + b.grid.g, h: W.SLOT.h }, "gardenio"]);
assert.deepStrictEqual([g1.x - hg.x, g1.y - hg.y], [W.SLOT.dx + b.grid.g, 0], "slot 1 is east of the farmstead");
// each zone word is its courtyard cell (29.2): Barn north-west, the Escalations and the Mailbox north-east, Pen south-west, Growing field south-east
["barn", "escalation", "mailbox", "pen", "growing"].forEach(function (z) { var c = hg.cells[z]; r = W.resolve(w, "gardenio/" + z); assert.deepStrictEqual([r.kind, r.id, r.level, r.rect], [z === "pen" ? "pen" : "zone", "home:gardenio/" + z, 2, { x: c.x, y: c.y, w: c.w, h: c.h }], z); });
assert.deepStrictEqual([hg.cells.barn.x, hg.cells.barn.y, hg.cells.growing.x + hg.cells.growing.w, hg.cells.growing.y + hg.cells.growing.h], [hg.x, hg.y, hg.x + hg.w, hg.y + hg.h], "the courtyard fills the farmstead");
r = W.resolve(w, "~/barn"); assert.deepStrictEqual([r.kind, r.id, r.level], ["zone", "home:/barn", 2]);
// a legacy yard link lands on the Commons's farmstead, or the thing it names wherever it is, and says it moved (H-D12)
var hc = w.index["home:"]; r = W.resolve(w, "yard"); assert.deepStrictEqual([r.kind, r.id, r.moved, r.rect], ["home", "home:", "yard", { x: hc.x, y: hc.y, w: W.SLOT.w, h: W.SLOT.h }]);
r = W.resolve(w, "yard/pen"); assert.deepStrictEqual([r.kind, r.id, r.moved], ["home", "home:", "yard"], "no Commons pen: its farmstead");
r = W.resolve(w, "yard/gd-plan1"); assert.deepStrictEqual([r.kind, r.id, r.moved], ["plot", "gd-plan1", "yard"]);
// fallbacks: unknown id -> its project, said so; bare id -> the thing; unknown project -> nothing; a task under the wrong project still resolves
r = W.resolve(w, "gardenio/gd-gone"); assert.deepStrictEqual([r.kind, r.found, r.missing, r.level], ["biome", false, "gd-gone", 1]);
r = W.resolve(w, "gd-plan2"); assert.deepStrictEqual([r.kind, r.id, r.found], ["plot", "gd-plan2", true]);
assert.strictEqual(W.resolve(w, "nowhere"), null); assert.strictEqual(W.resolve(w, "nowhere/gd-gone"), null, "unknown project and unknown id: nothing"); assert.strictEqual(W.resolve(w, "nowhere/gd-plan1").id, "gd-plan1", "a known id resolves even under a stale project segment"); assert.strictEqual(W.resolve(null, "gardenio"), null);
r = W.resolve(w, "music-clonk/gd-plan1"); assert.strictEqual(r.biome.key, "gardenio", "an id wins over the project segment");
// every indexed thing round-trips through atFor and resolve to the same rectangle
Object.keys(w.index).forEach(function (id) { var it = w.index[id], at = W.atFor(w, it.kind, id); assert.ok(at, "at for " + id); var rr = W.resolve(w, at); assert.ok(rr && rr.found && rr.id === id, "round trip " + id + " via " + at); });
// locate: what the URL says for a camera centre, per level
var p1 = w.index["gd-plan1"], T = W.TILE, px = (p1.x + W.BED.off[0] + 0.5) * T, py = (p1.y + W.BED.off[0] + 0.5) * T;   // inside the first task cell
assert.deepStrictEqual(W.locate(w, px, py, 3).at, "gardenio/gd-plan1-t0");
assert.deepStrictEqual(W.locate(w, px, py, 2).at, "gardenio/gd-plan1");
assert.deepStrictEqual(W.locate(w, px, py, 1).at, "gardenio");
assert.deepStrictEqual(W.locate(w, px, py, 0).at, "gardenio");
assert.deepStrictEqual([W.locate(w, (hg.x + 1) * W.TILE, (hg.y + 1) * W.TILE, 2).at, W.locate(w, (hg.x + 1) * W.TILE, (hg.y + 1) * W.TILE, 1).at], ["gardenio/home", "gardenio"], "over the farmstead from Plan in");
assert.strictEqual(W.locate(w, (hg.x + hg.w - 1) * W.TILE, (hg.y + hg.h - 1) * W.TILE, 3).at, "gardenio/home", "its Growing field's corner, at Bead too");
assert.strictEqual(W.locate(w, (g1.x + 1) * W.TILE, (g1.y + 1) * W.TILE, 2).at, "gardenio/" + g1.id, "and the plot east of it is the plot's");
assert.strictEqual(W.locate(w, 1e7, 1e7, 2), null, "the wild has no address");
assert.strictEqual(W.locate(null, 0, 0, 2), null);
// the jump index and ranking
var idx = W.searchIndex(w);
assert.ok(idx.some(function (e) { return e.kind === "home" && e.id === "home:gardenio" && e.aka.indexOf("yard") >= 0; }) && idx.some(function (e) { return e.kind === "biome" && e.title === "Music-Clonk"; }));
// the jump board's Go rows for a home land on the farmstead: its id resolves to the slot-0 cell
idx.filter(function (e) { return e.kind === "home"; }).forEach(function (e) { var it = w.index[e.id], rr = W.resolve(w, W.atFor(w, "home", e.id)); assert.deepStrictEqual([rr.id, rr.rect], [e.id, { x: it.x, y: it.y, w: W.SLOT.w, h: W.SLOT.h }], "Go lands on " + e.id); });
assert.ok(idx.some(function (e) { return e.kind === "task" && e.id === "gd-plan1-t2" && e.plan === "Minimap and fast travel"; }));
assert.ok(idx.some(function (e) { return e.kind === "seed" && e.id === "gd-seed1"; }) && idx.some(function (e) { return e.kind === "tree" && e.id === "gd-old"; }) && idx.some(function (e) { return e.kind === "flower" && e.id === "x-loose"; }));
var ids = function (list) { return list.map(function (e) { return e.id; }); };
assert.strictEqual(ids(W.search(idx, "GD-PLAN1"))[0], "gd-plan1", "exact id first, case-insensitive");
assert.deepStrictEqual(ids(W.search(idx, "gd-plan1")).slice(0, 2), ["gd-plan1", "gd-plan1-p1"].slice(0, 1).concat(ids(W.search(idx, "gd-plan1"))[1]), "then id prefixes");
assert.ok(ids(W.search(idx, "gd-plan1")).indexOf("gd-plan1-t0") > 0);
assert.strictEqual(W.search(idx, "minimap")[0].id, "gd-plan1", "a title prefix");
assert.strictEqual(W.search(idx, "travel")[0].id, "gd-plan1", "a word inside a title");
assert.strictEqual(W.search(idx, "music")[0].kind, "biome", "a project by name");
assert.strictEqual(W.search(idx, "task 1 gd-plan2")[0].id, "gd-plan2-t1", "several words, all must match");
assert.deepStrictEqual(W.search(idx, "", 4).map(function (e) { return e.kind + ":" + e.key; }), ["biome:", "home:", "biome:big", "home:big"], "empty: the projects in world order, each with its farmstead after it");
assert.deepStrictEqual(W.search(idx, "", 3, "home:big").map(function (e) { return e.kind + ":" + e.key; }), ["home:big", "biome:", "home:"], "empty: led by the cursor's project's farmstead (gd-cTKXpm8JH7)");
assert.strictEqual(W.search(idx, "big", 8, "home:").filter(function (e) { return e.kind === "home"; })[0].id, "home:big", "a typed query ignores the lead");
assert.ok(W.search(idx, "yard").every(function (e) { return e.kind === "home"; }), "yard finds the farmsteads");
assert.strictEqual(W.search(idx, "m").length, 8, "capped at 8"); assert.strictEqual(W.search(idx, "m", 3).length, 3);
assert.deepStrictEqual(W.search(idx, "zzz-nothing"), []);
assert.deepStrictEqual(W.search([], "x"), []); assert.strictEqual(W.searchIndex(null).length, 0);
`
	runNodeLogic(t, "world.js", script)
}

// The page carries the Go button and the '/' jump palette, and no minimap (3b R11), and the script wires them to the pure
// world.js functions (section 7). The jump input is a combobox over a listbox so a screen reader hears the matches.
func TestPageHasJump(t *testing.T) {
	h := New(newFake(), "/v", nil)
	mux := http.NewServeMux()
	(&Handler{Hub: h, BasePath: "/v"}).Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/garden?at=gardenio%2Fgd-x&z=2", nil))
	body := inlined(t, rec.Body.String())
	for _, want := range []string{
		`id="cam-go"`, `aria-label="Jump to a project, plan or bead"`,
		`id="jump" role="dialog"`, `id="jump-q" type="text" role="combobox"`, `aria-controls="jump-list"`, `id="jump-list" role="listbox"`,
		`WORLD.resolve(world,`, `WORLD.searchIndex(world,gspots)`, `WORLD.search(jumpIdx,`,
		`WORLD.queryGet(location.search,'at')`, `history.replaceState(history.state,'',url)`, `k === "/"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
	if !strings.Contains(body, "slash jumps to a project") {
		t.Errorf("the board's accessible name does not mention the slash key")
	}
	for _, gone := range []string{`id="mini"`, "minimap", "drawMini(", "WORLD.minimap(", "miniPick("} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(gone)) {
			t.Errorf("the page still carries the removed minimap: %q", gone)
		}
	}
}

// The garden installs to the home screen as its own app (Commons on a phone M6): the head links its manifest with the
// session cookie and carries the iOS meta, and the manifest and icons are served under the base path.
func TestPageInstallsToHomeScreen(t *testing.T) {
	mux, rec := servedPage(t)
	body := rec.Body.String()
	for _, want := range []string{
		`<link rel="manifest" href="/v/garden/manifest.webmanifest" crossorigin="use-credentials">`,
		`<link rel="apple-touch-icon" href="/v/garden/icon-180.png">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`viewport-fit=cover`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}

	get := func(p string) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		mux.ServeHTTP(r, httptest.NewRequest("GET", p, nil))
		return r
	}
	m := get("/garden/manifest.webmanifest")
	var got webManifest
	if err := json.Unmarshal(m.Body.Bytes(), &got); err != nil || m.Code != 200 {
		t.Fatalf("manifest: code %d, %v", m.Code, err)
	}
	if got.StartURL != "/v/garden" || got.Scope != "/v/garden" || got.Display != "standalone" || len(got.Icons) == 0 {
		t.Errorf("manifest = %+v", got)
	}
	for _, ic := range append(got.Icons, manifestIcon{Src: "/v/garden/icon-180.png", Sizes: "180x180"}) {
		r := get(strings.TrimPrefix(ic.Src, "/v"))
		size := r.Body.Len()
		img, err := png.Decode(r.Body)
		if r.Code != 200 || err != nil {
			t.Fatalf("%s: code %d, %v", ic.Src, r.Code, err)
		}
		if b := img.Bounds(); strconv.Itoa(b.Dx())+"x"+strconv.Itoa(b.Dy()) != ic.Sizes {
			t.Errorf("%s is %v, manifest says %s", ic.Src, b, ic.Sizes)
		}
		// Opaque: iOS paints a transparent corner black.
		if _, _, _, a := img.At(0, 0).RGBA(); a != 0xffff {
			t.Errorf("%s has a transparent corner", ic.Src)
		}
		// Cheap (P1.5): paletted pixel art is under 1 KB at every size (2 KB leaves room), and a revisit is a bodyless 304.
		if size > 2048 {
			t.Errorf("%s is %d bytes", ic.Src, size)
		}
		etag := r.Header().Get("ETag")
		again := httptest.NewRequest("GET", strings.TrimPrefix(ic.Src, "/v"), nil)
		again.Header.Set("If-None-Match", etag)
		r2 := httptest.NewRecorder()
		mux.ServeHTTP(r2, again)
		if etag == "" || r2.Code != http.StatusNotModified || r2.Body.Len() != 0 {
			t.Errorf("%s revisit with ETag %q: code %d, %d bytes", ic.Src, etag, r2.Code, r2.Body.Len())
		}
	}
}

// The sky (web/sky.js, gd-SwRqFcjWc6) is one setting for the whole page: auto is night by the clock from 19:00 to 07:00
// local, or on a dark OS; day and night pin it. The pure part runs under node: resolve() and the time to the next flip.
func TestSkyLogicUnderNode(t *testing.T) {
	script := `
var S = require(process.argv[1]), assert = require("assert");
assert.deepStrictEqual(S.MODES, ["auto", "day", "night"]);
assert.strictEqual(S.DUSK, 19); assert.strictEqual(S.DAWN, 7);
// auto: the clock, then the OS
assert.strictEqual(S.resolve("auto", 12, false), "day");
assert.strictEqual(S.resolve("auto", 12, true), "night", "a dark OS is night by day too");
assert.strictEqual(S.resolve("auto", 19, false), "night", "dusk is night");
assert.strictEqual(S.resolve("auto", 23, false), "night");
assert.strictEqual(S.resolve("auto", 6, false), "night", "before dawn is night");
assert.strictEqual(S.resolve("auto", 7, false), "day", "dawn is day");
// pinned: nothing else counts
assert.strictEqual(S.resolve("day", 23, true), "day");
assert.strictEqual(S.resolve("night", 12, false), "night");
assert.strictEqual(S.resolve("garbage", 12, false), "day", "an unknown mode behaves as auto");
// the timer lands on the boundary, in local wall-clock time
function mins(h, m) { return S.msToFlip(new Date(2026, 9, 20, h, m || 0, 0, 0)) / 60000; }
assert.strictEqual(mins(12), 7 * 60, "noon: dusk is 7 h away");
assert.strictEqual(mins(18, 30), 30);
assert.strictEqual(mins(19), 12 * 60, "at dusk the next flip is dawn");
assert.strictEqual(mins(20), 11 * 60);
assert.strictEqual(mins(3), 4 * 60);
assert.strictEqual(mins(6, 59), 1);
assert.strictEqual(mins(7), 12 * 60, "at dawn the next flip is dusk");
`
	runNodeLogic(t, "sky.js", script)
}

// The page carries the sky: the script runs in <head> after the stylesheet so html[data-sky] is set before the first
// paint, the Menu has the Sky item (3b R10 took it out of the header), and no other script reads the OS preference itself (the scene, the legend and the
// plans slot would disagree with the page on a pinned sky).
func TestPageHasSkyControl(t *testing.T) {
	_, rec := servedPage(t)
	body := inlined(t, rec.Body.String())
	css, sky, head := strings.Index(body, "<style>"), strings.Index(body, "var SKY = "), strings.Index(body, "</head>")
	if css < 0 || sky < 0 || head < 0 || !(css < sky && sky < head) {
		t.Errorf("web/sky.js must load in <head> after the stylesheet: style %d sky %d head %d", css, sky, head)
	}
	for _, want := range []string{`<button class="btn mi" id="cam-sky" type="button" aria-label="Sky: automatic. Press for always day">`, `data-sky=night`, `:not([data-sky=day])`} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %s", want)
		}
	}
	for name, src := range map[string]string{"garden.js": pageAppJS, "plans.js": pagePlansJS, "porch.js": pagePorchJS, "art.js": pageArtJS, "commons2.js": pageCommons2JS, "workbench.js": pageWorkbenchJS, "chat.js": pageChatJS} {
		if strings.Contains(src, "prefers-color-scheme") {
			t.Errorf("%s reads prefers-color-scheme itself; read SKY.night() and subscribe with SKY.on", name)
		}
	}
	for _, gone := range []string{`id="b-sky"`, `#b-sky`, `btn sky`} {
		if strings.Contains(body, gone) {
			t.Errorf("page still has the header's Sky button (%s); it is a Menu item (3b R10)", gone)
		}
	}
	for _, want := range []string{"SKY.night()", "SKY.on(", "SKY.cycle()", "getElementById('cam-sky')"} {
		if !strings.Contains(pageAppJS, want) {
			t.Errorf("garden.js missing %s", want)
		}
	}
}

// A card's buttons and its hidden resolve field are named "action", and a control with that name shadows the form's own
// action property: form.action is then the element, and the post goes to "[object HTMLInputElement]" (405, every Commons
// card action, 2026-10-21). Read the attribute instead (form.getAttribute('action')).
func TestPageJSNeverReadsFormActionProperty(t *testing.T) {
	read := regexp.MustCompile(`\bform\.action\b`)
	for name, src := range map[string]string{"garden.js": pageAppJS, "plans.js": pagePlansJS, "porch.js": pagePorchJS, "art.js": pageArtJS, "commons2.js": pageCommons2JS, "workbench.js": pageWorkbenchJS, "chat.js": pageChatJS} {
		for i, line := range strings.Split(src, "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "/*") || strings.HasPrefix(code, "*") || strings.HasPrefix(code, "//") {
				continue
			}
			if read.MatchString(code) {
				t.Errorf("%s:%d reads form.action; a control named action shadows it, read form.getAttribute('action'): %s", name, i+1, code)
			}
		}
	}
}
