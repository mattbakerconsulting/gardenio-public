package gardenhub

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// weeds.js is pure lint over the farm document (gd-ytTNc3Fu9M), so its contract runs under node: each rule fires on what it
// names and nothing else, every weed says why, the index places each one on the plot, cell, pot, flower or region it sits in,
// fog covers only dormant regions, and the drawing puts thistles where the level says. An open wish older than STALE_S that
// nothing tends is an untended wish (gd-2cPnm71cxT): its line is in the wish's own bead view, and its region tallies it.
func TestWeedsLintUnderNode(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	script := `
var W = require(process.argv[1]), WEEDS = require(process.argv[2]), assert = require("assert");
var now = 1792000000, H = 3600, D = 86400;
var task = function (id, state, upd, extra) { var t = { id: id, title: "Step " + id, type: "task", state: state, priority: 2, project: "gardenio", updated: now - upd }; for (var k in extra || {}) t[k] = extra[k]; return t; };
var counts = function (n) { return { in_progress: n, review: 0, blocked: 0, open: 0, deferred: 0, closed: 0 }; };
var plan = function (id, project, act, phases, unphased) {
  return { id: id, title: "Plan " + id, state: "building", status: "in_progress", project: project, priority: 2, created: now - 30 * D, updated: now - act, activity: now - act,
    phases: phases, unphased: unphased || { id: "", title: "", status: "", counts: counts(0), tasks: [] }, counts: counts(1), open_decisions: 0, open_escalations: 0, needs_human: 0 };
};
var phase = function (id, tasks) { return { id: id, title: "Phase " + id, status: "open", counts: counts(tasks.length), tasks: tasks }; };
var doc = {
  plans: [
    plan("gd-P1", "gardenio", H, [phase("gd-ph1", [task("gd-t1", "in_progress", 13 * H, { assignee: "mo" }), task("gd-t2", "open", H, { project: "" }), task("gd-t3", "in_progress", H)])]),
    plan("gd-P2", "", 2 * H, [], { id: "", title: "", status: "", counts: counts(1), tasks: [task("gd-t4", "in_progress", 20 * H, { project: "" })] }),
    plan("gd-P3", "old", 20 * D, [phase("gd-ph3", [task("gd-t5", "open", 20 * D, { project: "old" })])]),
    plan("gd-P4", "busy", H, [phase("gd-ph4", [1, 2, 3, 4, 5, 6].map(function (i) { return task("gd-b" + i, "in_progress", (13 + i) * H, { project: "busy" }); }))])
  ],
  seeds: [
    { id: "gd-S1", title: "Old draft", state: "frosted", status: "open", project: "gardenio", priority: 2, created: now - 20 * D, updated: now - 8 * D, phases: 1, tasks: 0 },
    { id: "gd-S2", title: "Fresh draft", state: "frosted", status: "open", project: "gardenio", priority: 2, created: now - 3 * D, updated: now - 2 * D, phases: 1, tasks: 0 },
    { id: "gd-S3", title: "Ready, unlabelled", state: "ready", status: "open", project: "", priority: 2, created: now - 3 * D, updated: now - D, phases: 2, tasks: 4 }
  ],
  closed_plans: { total: 1, shown: [{ id: "gd-C1", title: "Done", state: "complete", project: "gardenio", updated: now - 40 * D }] },
  meadow: { total: 2, with_mr: 0, shown: [task("gd-m1", "open", H, { project: "" }), task("gd-m2", "in_progress", 30 * H)] },
  piles: { closed_decisions: 0, closed_escalations: 0, closed_work: 0, closed_plans: 1, orphan_mr_total: 3, orphan_mrs: [
    { url: "https://github.com/o/r/pull/7", iid: "7", title: "Fix the thing", repo: "o/r", why: "bead_closed", bead: "gd-x1", project: "gardenio", since: now - 3 * D },
    { url: "https://github.com/o/r/pull/8", iid: "8", title: "Stray", repo: "o/r", why: "no_bead", since: now - D },
    { url: "https://github.com/o/r/pull/9", iid: "9", title: "Elsewhere", repo: "o/r", why: "no_bead", project: "no-region", since: now - D }
  ] },
  meta: { schema: 1, truncated: false, row_cap: 2500, rows: 30 }
};
var L = WEEDS.lint(doc, now);
var of = function (rule) { return L.weeds.filter(function (w) { return w.rule === rule; }).map(function (w) { return w.id; }).sort(); };
assert.deepStrictEqual(of("wilt"), ["gd-b1", "gd-b2", "gd-b3", "gd-b4", "gd-b5", "gd-b6", "gd-m2", "gd-t1", "gd-t4"], "wilt: in progress with no update past 12h, steps and loose beads");
assert.deepStrictEqual(of("unlabelled"), ["gd-P2", "gd-S3", "gd-m1", "gd-t2"], "unlabelled: a plan, seed or bead with no project label; not the steps of an unlabelled plan");
assert.deepStrictEqual(of("phaseless"), ["gd-P2"]);
assert.deepStrictEqual(of("overgrown"), [], "no plan has more than nine beds");
// overgrown (cursor-grid spec 5.3): more than nine beds, phases plus a non-empty Other, are more than a plot's 3x3 shows
var phases = function (n) { var o = []; for (var i = 0; i < n; i++) o.push(phase("gd-x" + n + "-" + i, [])); return o; };
var other = { id: "", title: "", status: "", counts: counts(1), tasks: [task("gd-xo", "open", H)] };
var LT = WEEDS.lint({ plans: [plan("gd-O10", "gardenio", H, phases(10)), plan("gd-O9", "gardenio", H, phases(9)), plan("gd-O9u", "gardenio", H, phases(9), other), plan("gd-O8u", "gardenio", H, phases(8), other)] }, now);
assert.deepStrictEqual(LT.weeds.filter(function (w) { return w.rule === "overgrown"; }).map(function (w) { return w.id; }).sort(), ["gd-O10", "gd-O9u"]);
assert.ok(/^more than 9 phases \(10 beds\); split the plan/.test(LT.weeds.filter(function (w) { return w.id === "gd-O10"; })[0].why));
assert.deepStrictEqual(of("frosted"), ["gd-S1"], "a draft is long-frosted after 7 days, not 2");
assert.deepStrictEqual(of("orphan"), ["https://github.com/o/r/pull/7", "https://github.com/o/r/pull/8", "https://github.com/o/r/pull/9"]);
assert.deepStrictEqual(Object.keys(L.fog), ["old"], "only the project with no activity for 14 days is dormant");
assert.strictEqual(L.counts.wilt, 9); assert.strictEqual(L.total, L.weeds.length); assert.strictEqual(L.orphanTotal, 3);
L.weeds.forEach(function (w) { assert.ok(w.why.length > 20 && w.since > 0 && w.score > 0, "every weed explains itself: " + JSON.stringify(w)); });
var w1 = L.weeds.filter(function (w) { return w.id === "gd-t1"; })[0];
assert.ok(/^in progress, but no change for 13 hours/.test(w1.why) && /Gardener: mo/.test(w1.why), w1.why);
assert.strictEqual(w1.key, "gardenio"); assert.strictEqual(w1.plan, "gd-P1");
var t4 = L.weeds.filter(function (w) { return w.id === "gd-t4"; })[0];
assert.strictEqual(t4.key, "", "a step sits in its plan's region"); assert.ok(/Nobody is assigned/.test(t4.why));
assert.ok(/^1 open step sits in one Other row/.test(L.weeds.filter(function (w) { return w.rule === "phaseless"; })[0].why));
assert.ok(/^no update for 8 days/.test(L.weeds.filter(function (w) { return w.rule === "frosted"; })[0].why));
var o7 = L.weeds.filter(function (w) { return w.id.slice(-2) === "/7"; })[0];
assert.ok(/PR #7 in o\/r is still open, but its bead gd-x1 is closed/.test(o7.why), o7.why); assert.strictEqual(o7.bead, "gd-x1");
assert.ok(/no bead links it/.test(L.weeds.filter(function (w) { return w.id.slice(-2) === "/8"; })[0].why));
// rank: an orphaned PR outweighs a fresher tidiness weed; ties fall to the older weed
var r = WEEDS.rank(L.weeds);
assert.strictEqual(r[0].rule, "orphan"); assert.ok(r.every(function (w, i) { return i === 0 || r[i - 1].score >= w.score; }));
assert.strictEqual(WEEDS.lint({}, now).total, 0, "an empty document has no weeds");
assert.strictEqual(WEEDS.age(5 * H), "5 hours"); assert.strictEqual(WEEDS.age(3 * D), "3 days");

// index: placed on the laid-out world
var world = W.layout(doc, null, { now: now * 1000 }), ix = WEEDS.index(L, world);
var ids = function (ws) { return (ws || []).map(function (w) { return w.rule + ":" + w.id; }).sort(); };
assert.deepStrictEqual(ids(ix.plots["gd-P1"]), ["unlabelled:gd-t2", "wilt:gd-t1"]);
assert.deepStrictEqual(ids(ix.plots["gd-P2"]), ["phaseless:gd-P2", "unlabelled:gd-P2", "wilt:gd-t4"]);
assert.deepStrictEqual(ids(ix.by["gd-t1"]), ["wilt:gd-t1"]); assert.deepStrictEqual(ids(ix.by["gd-S1"]), ["frosted:gd-S1"]); assert.deepStrictEqual(ids(ix.by["gd-m1"]), ["unlabelled:gd-m1"]);
assert.ok(ix.by["gd-P2"] && !ix.by["gd-P1"], "a plan that is itself a weed is in by[], one that only holds weedy steps is not");
assert.ok(ids(ix.biomes["project:gardenio"]).indexOf("orphan:https://github.com/o/r/pull/7") >= 0, "an orphaned PR sits in its bead's project");
assert.ok(ids(ix.biomes["project:"]).indexOf("orphan:https://github.com/o/r/pull/8") >= 0, "an unlabelled one in the Commons");
assert.deepStrictEqual(ix.unplaced.map(function (w) { return w.id; }), ["https://github.com/o/r/pull/9"], "a project with no region places nothing");
assert.deepStrictEqual(Object.keys(ix.fog), ["project:old"]);

// the bead view's weed lines and accessible names: why, in words
var href = function (x) { return "/garden?bead=" + x.id; };
var plotH = { kind: "plot", id: "gd-P1" }, ln = WEEDS.lines(ix, plotH, href);
assert.strictEqual(ln.length, 2);
assert.ok(ln[0][0] === "weed" && /^Wilted: Step gd-t1, in progress, but no change/.test(ln[0][1]) && ln[0][2] === "/garden?bead=gd-t1" && ln[0][3] === "gd-t1", JSON.stringify(ln));
var own = WEEDS.lines(ix, { kind: "plot", id: "gd-P2" }, href);
assert.ok(own.some(function (l) { return /^Plan without phases: 1 open step sits in one Other row\. Give the plan a phase_order\.$/.test(l[1]) && !l[2]; }), "the plan's own weed names no other bead: " + JSON.stringify(own));
var busy = WEEDS.lines(ix, { kind: "plotsign", id: "gd-P4" }, href);
assert.strictEqual(busy.length, 6, "under LINES_MAX, every weed: " + JSON.stringify(busy));
var two = WEEDS.lines(ix, { kind: "plot", id: "gd-P4" }, href, 2);
assert.strictEqual(two.length, 3); assert.deepStrictEqual(two[2], ["why", "+4 more weeds"]);
var five = WEEDS.lines(ix, { kind: "plot", id: "gd-P4" }, href, 5);
assert.deepStrictEqual(five[5], ["why", "+1 more weed"]);
assert.ok(/Step gd-b6/.test(busy[0][1]), "the most neglected first");
assert.deepStrictEqual(WEEDS.lines(ix, { kind: "task", id: "gd-t1" }, href).map(function (l) { return l[1].slice(0, 22); }), ["Wilted: in progress, b"]);
var bio = WEEDS.lines(ix, { kind: "biome", id: "project:old" }, href);
assert.ok(/^Dormant: no activity for 20 days/.test(bio[0][1]), JSON.stringify(bio));
var gb = WEEDS.lines(ix, { kind: "sign", id: "project:gardenio" }, href);
assert.ok(/^Weeds to tend: 1 orphaned PR, 2 wilted beads, 1 long-frosted draft, 1 unlabelled bead\.$/.test(gb[0][1]), gb[0][1]);
assert.ok(gb.some(function (l) { return /^Orphaned PR: PR #7/.test(l[1]) && l[2] === "/garden?bead=gd-x1"; }), "an orphaned PR links its closed bead inside the garden");
assert.deepStrictEqual(WEEDS.lines(ix, { kind: "home", id: "home:gardenio" }, href), [], "a home strip has no weeds");
assert.strictEqual(WEEDS.aria(ix, plotH), " Weeds: 1 wilted bead, 1 unlabelled bead.");
assert.strictEqual(WEEDS.aria(ix, { kind: "biome", id: "project:old" }), " Dormant for 20 days.");
assert.strictEqual(WEEDS.aria(null, plotH), "");

// drawing: fog at World and Project, thistles per level, nothing for a step that is only wilted at Bead
var mock = function () {
  var c = { imgs: [], fills: 0, alphaOut: [], globalAlpha: 1, fillStyle: "" };
  c.fillRect = function () { c.fills++; }; c.drawImage = function (img, x, y, w, h) { c.imgs.push([x, y, w, h]); };
  return c;
};
var pal = { k: "#000", W: "#fff", v: "#a0f", V: "#60a", L: "#060", m: "#0c0", q: "#ddd", j: "#fff", g: "#0a0", d: "#080" };
var bmp = function () { return {}; }, all = world.bbox;
var at = function (lv) { var c = mock(); WEEDS.draw(c, { ix: ix, vis: W.visible(world, all, lv), level: lv, z: [0.08, 0.3, 0.7, 2][lv], pal: pal, T: W.TILE, night: false, bmp: bmp }); assert.strictEqual(c.globalAlpha, 1, "alpha restored"); return c; };
var c0 = at(0), c1 = at(1), c2 = at(2), c3 = at(3);
assert.ok(c0.fills > 0 && c0.imgs.length === 0, "World: fog only");
var oldB = world.index["project:old"], fogged = mock(), noFog = WEEDS.index(WEEDS.lint(doc, now - 30 * D), world);
WEEDS.draw(fogged, { ix: noFog, vis: W.visible(world, all, 0), level: 0, z: 0.08, pal: pal, T: W.TILE, bmp: bmp });
assert.strictEqual(fogged.fills, 0, "nothing is dormant a month earlier");
assert.ok(oldB && WEEDS.fogBanks(oldB.seed, oldB.foot).length >= 3);
var weedyPlots = world.biomes.reduce(function (n, b) { return n + b.plots.filter(function (p) { return (ix.plots[p.id] || []).length; }).length; }, 0);
var weedyItems = function (lv) { return W.visible(world, all, lv).items.filter(function (it) { return ix.by[it.id]; }).length; };
assert.strictEqual(weedyPlots, 4, "gd-S3, ready, is staked out on a plot in the Commons"); assert.strictEqual(weedyItems(1), 3, "a seed pot and two flowers");
assert.deepStrictEqual(ids(ix.plots["gd-S3"]), ["unlabelled:gd-S3"], "a staked seed's weed is its plot's");
assert.ok(/^No project label: it waits in the Commons/.test(WEEDS.lines(ix, { kind: "plot", id: "gd-S3" }, href)[0][1]), "and names no other bead");
assert.strictEqual(c1.imgs.length, weedyPlots + weedyItems(1), "Project: one thistle per weedy plot, plus the weedy seed pots and flowers in view");
c1.imgs.forEach(function (g) { assert.ok(Math.abs(g[2] * 0.3 - WEEDS.GLYPH_PX) < 1e-9, "a constant screen size at Project"); });
c3.imgs.slice(0, 1).forEach(function (g) { assert.strictEqual(g[2], W.TILE, "a corner thistle is never under a tile, at Bead"); });
var cellWeeds = 0; world.biomes.forEach(function (b) { b.plots.forEach(function (p) { p.beds.forEach(function (bd) { bd.cells.forEach(function (c) { if (ix.by[c.id]) cellWeeds++; }); }); }); });
assert.strictEqual(c2.imgs.length, weedyPlots + cellWeeds + weedyItems(2), "Plan: each weedy plot's corner, step and item");
assert.strictEqual(cellWeeds, 9);
assert.strictEqual(c3.imgs.length, c2.imgs.length - 8, "Bead: the 8 steps that are only wilted show their wilted sprite instead of a thistle (gd-t2 keeps one; a flower has no wilted sprite, so gd-m2 keeps one too)");
WEEDS.THISTLE.forEach(function (row) { assert.strictEqual(row.length, 16); }); assert.strictEqual(WEEDS.THISTLE.length, 16);
var lg = mock(); WEEDS.drawLegend(lg, "thistle", pal); assert.ok(lg.fills > 20); WEEDS.drawLegend(lg, "fog", pal); assert.strictEqual(lg.globalAlpha, 1);

// untended: an open wish older than STALE_S (7 days) that nothing tends; not a tended one, nor a young one, nor one with no
// opening time
var wdoc = { plans: [plan("gd-W1", "gardenio", H, [phase("gd-wph", [task("gd-wt", "open", H)])])], wishes: { open: [
  { id: "gd-wa", title: "Old and alone", project: "gardenio", opened: now - 9 * D, kin_total: 0, tending: false },
  { id: "gd-wb", title: "Old, steps idle", project: "gardenio", opened: now - 8 * D, kin_total: 3, tending: false },
  { id: "gd-wc", title: "Old, tended", project: "gardenio", opened: now - 30 * D, kin_total: 1, tending: true },
  { id: "gd-wd", title: "Young", project: "gardenio", opened: now - 6 * D, tending: false },
  { id: "gd-we", title: "No time", project: "gardenio", tending: false },
  { id: "gd-wf", title: "Elsewhere", project: "no-region", opened: now - 9 * D, tending: false }
] } };
assert.strictEqual(WEEDS.STALE_S, 7 * D);
var WL = WEEDS.lint(wdoc, now), un = WL.weeds.filter(function (w) { return w.rule === "untended"; });
assert.deepStrictEqual(un.map(function (w) { return w.id; }).sort(), ["gd-wa", "gd-wb", "gd-wf"]);
assert.strictEqual(WL.counts.untended, 3);
var wa = un.filter(function (w) { return w.id === "gd-wa"; })[0], wb = un.filter(function (w) { return w.id === "gd-wb"; })[0];
assert.deepStrictEqual([wa.kind, wa.key, wa.plan, wa.since], ["wish", "gardenio", "", now - 2 * D]);
assert.strictEqual(wa.why, "open for 9 days and nothing tends it: no plan or step has come of it. Start on it, or close the wish.");
assert.strictEqual(wb.why, "open for 8 days and nothing tends it: none of its 3 steps is in progress. Work one, or close the wish.");
var wworld = W.layout(wdoc, null, { now: now * 1000 }), wix = WEEDS.index(WL, wworld);
assert.deepStrictEqual(Object.keys(wix.wishes).sort(), ["gd-wa", "gd-wb", "gd-wf"], "every untended wish, a region or not");
assert.deepStrictEqual(ids(wix.biomes["project:gardenio"]), ["untended:gd-wa", "untended:gd-wb"], "its region tallies it");
assert.ok(!(wix.plots["gd-W1"] || []).length && !wix.by["gd-wa"], "no thistle: a goat wanders");
assert.deepStrictEqual(WEEDS.lines(wix, { kind: "wish", id: "gd-wa" }, href), [["weed", "Untended wish: " + wa.why]], "the wish's own bead view");
assert.deepStrictEqual(WEEDS.lines(wix, { kind: "wish", id: "gd-wc" }, href), []);
assert.strictEqual(WEEDS.lines(wix, { kind: "sign", id: "project:gardenio" }, href)[0][1], "Weeds to tend: 2 untended wishes.");
assert.strictEqual(WEEDS.aria(wix, { kind: "wish", id: "gd-wb" }), " Weeds: 1 untended wish.");
`
	world, err := filepath.Abs(filepath.Join("web", "world.js"))
	if err != nil {
		t.Fatal(err)
	}
	weeds, err := filepath.Abs(filepath.Join("web", "weeds.js"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, "-e", script, world, weeds).CombinedOutput()
	if err != nil {
		t.Fatalf("node weeds test failed: %v\n%s", err, out)
	}
}

// The page wires the weeds in: the module is embedded before garden.js, which indexes it per document, draws it in the world
// layer and adds its words to the hot links' names (gd-ytTNc3Fu9M). The card that listed each weed is gone (cursor-grid spec
// N-D14); its lines are the bead view's "Weeds" section, which GARDEN.known hands web/workbench.js (build step 6).
func TestWeedsWiredIntoPage(t *testing.T) {
	html := strings.Index(pageHTML, `{{asset "weeds.js"}}`)
	if html < 0 || html > strings.Index(pageHTML, `{{asset "garden.js"}}`) {
		t.Fatal("weeds.js must be embedded before garden.js")
	}
	for _, hook := range []string{"WEEDS.index(WEEDS.lint(farmDoc", "WEEDS.draw(ctx,", "WEEDS.aria(weedIx,h)", "WEEDS.drawLegend(", "o.weeds=WEEDS.lines(weedIx,wh,beadHref);", "o.weeds=WEEDS.lines(weedIx,{kind:'wish',id:id},beadHref);"} {
		if !strings.Contains(pageAppJS+pageSceneJS, hook) {
			t.Errorf("garden.js and scene.js are missing the weeds hook %q", hook)
		}
	}
	if !strings.Contains(pageWorkbenchJS, "sec('Weeds', lineList('bv-weeds', k.weeds))") {
		t.Error("workbench.js does not list the weeds GARDEN.known hands it")
	}
}
