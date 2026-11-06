/* Gardeners beside their band item or goat (gd-6FCoWelnx2, WORLD.standItem / standBeside), and an escalation's executor beside
   its icon (gd-B5aDmNhmjz, WORLD.escStands), run by TestStandBesideUnderNode:
   node stand_test.js <web dir>. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert"), W = require(path.join(process.argv[2], "world.js"));
function plan(id, project, n) {
  var tasks = []; for (var i = 0; i < n; i++) tasks.push({ id: id + "-t" + i, title: "T" + i, type: "task", state: "in_progress", updated: 1 });
  return { id: id, title: "Plan " + id, project: project, created: 1, activity: 1, counts: { in_progress: n },
    phases: [{ id: id + "-p1", title: "Phase 1", counts: { in_progress: n }, tasks: tasks }], unphased: { id: "", counts: {}, tasks: [] } };
}
function g(id, state, bucket, task, plot, since, extra) { var o = { id: id, agent: "name-" + id, role: "crew", project: "gardenio", state: state, bucket: bucket, task: task, plot: plot ? { plan: plot, phase: "" } : null, since: since || 0, href: "/garden?bead=" + id }; if (extra) Object.keys(extra).forEach(function (k) { o[k] = extra[k]; }); return o; }
var i;
// beside its work drawn elsewhere (gd-6FCoWelnx2, "workers in the orchard and other areas should show up there"): a loose
// bead's wildflower (the home strip's Growing field), a draft seed's pot, a closed plan's tree, a wish's goat (open) or
// headstone (closed), the bead it was sent to; never on another of the band's items
var doc2 = { plans: [plan("pa", "gardenio", 8)], seeds: [{ id: "s1", title: "Seed one", project: "gardenio", state: "draft", created: 2 }],
  closed_plans: { total: 1, shown: [{ id: "cp", title: "Closed plan", project: "gardenio", state: "closed" }] },
  meadow: { total: 2, shown: [{ id: "lb", title: "Loose bead", project: "gardenio", type: "task", state: "in_progress" }, { id: "lb2", title: "Loose two", project: "gardenio", type: "bug", state: "open" }] },
  wishes: { open: [{ id: "wo", title: "Open wish", project: "gardenio" }], closed: [{ id: "wc", title: "Closed wish", project: "gardenio", closed: 5, outcome: "done" }] } };
var bs = [
  g("fw", "working", "alive", "lb", null, 10), g("fw2", "yielding", "alive", "lb", null, 20),   // two on one wildflower: the next free columns past its neighbour
  g("pw", "working", "alive", "s1", null, 10), g("tw", "failed", "stuck", "cp", null, 10),
  g("gw", "working", "alive", "wo", null, 10), g("sw", "working", "alive", "wc", null, 10),   // a wish: by its goat (open) or its headstone (closed)
  g("tend", "working", "alive", "", null, 10, { tends: "lb2", tends_title: "Loose two" }),   // sent to a loose bead, no task of its own
  g("fidle", "idle", "idle", "lb", null, 10), g("gone2", "working", "alive", "nope", null, 10)   // idle: home; a task drawn nowhere: home
];
doc2.gardeners = bs;   // the farm document carries them, so a flower one is out at keeps its pair's east tile (28.12)
var w2 = W.layout(doc2, null, { now: 1, readonly: true }), fl = w2.index.lb, fl2 = w2.index.lb2, pot = w2.index.s1, tree = w2.index.cp, goat = w2.index["wish:wo"], stone = w2.index["wish:wc"], gb2 = w2.index["project:gardenio"], field = gb2.home.zones.growing;
assert.deepStrictEqual([fl.kind, pot.kind, tree.kind, goat.kind, stone.kind], ["flower", "seed", "tree", "goat", "stone"], "the fixture draws every kind of stand");
assert.deepStrictEqual([fl2.x, fl2.y], [fl.x + 2, fl.y], "the two tended wildflowers are in neighbouring pair slots, each a whole one (28.12)");
var b3 = W.gardenerSpots(w2, bs, 3), b2 = W.gardenerSpots(w2, bs, 2), sp = function (id) { return b3.spots[id]; };
assert.deepStrictEqual(b3.home, ["gone2", "fidle"], "only an idle one or one whose task is drawn nowhere goes home");
assert.deepStrictEqual(b3.fields["project:gardenio"], ["tw", "fw2", "fw", "gone2", "gw", "pw", "sw", "tend", "fidle"], "every one has a field stand in its project's field, in need order (28.12)");
assert.ok(!("ghosts" in b3) && !("barns" in b3), "nothing has a ghost");
assert.deepStrictEqual([sp("fw").plot, sp("fw").field], ["lb", { home: "project:gardenio", task: "lb" }], "one out beside its flower has both a spot there and a field stand");
assert.deepStrictEqual([sp("tend").field.task, sp("fidle").home, sp("fidle").field], ["lb2", "project:gardenio", { home: "project:gardenio", task: "lb" }], "a stand's plant is its task, else what it tends; one at home stands in the field too");
assert.deepStrictEqual(b3.plots.lb.ids, ["fw2", "fw"], "need first on one flower: the asker, then the worker");
assert.deepStrictEqual([sp("fw2").plot, sp("fw2").col, sp("fw2").row, sp("fw2").face, sp("fw2").fence, sp("fw2").shown], ["lb", fl.x + 1, fl.y, "l", true, true], "feet on the flower's row, the first column right of it not another flower (its pair's east tile), facing it");
assert.deepStrictEqual([sp("fw").plot, sp("fw").col, sp("fw").row], ["lb", fl.x + 3, fl.y], "the second in the next free column, past the neighbouring flower");
assert.deepStrictEqual([sp("fw2").x, sp("fw2").y], [16 * (fl.x + 1), 16 * fl.y - 8]);
assert.deepStrictEqual([sp("tend").plot, sp("tend").row], ["lb2", fl2.y], "sent to a loose bead with no task: by its flower");
var cols = ["fw", "fw2", "tend"].map(function (id) { return sp(id).col; });
assert.ok(cols.every(function (c, i) { return cols.indexOf(c) === i && c !== fl.x && c !== fl2.x && c >= field.x + 1 && c < field.x + field.w - 1; }), "three beside two neighbouring flowers share no tile, nor a flower's, all inside the field: " + cols);
assert.deepStrictEqual([sp("pw").plot, sp("pw").col, sp("pw").row], ["s1", pot.x + pot.w, pot.y + pot.h - 1], "right of the pot, feet on its bottom row");
assert.ok(sp("tw").plot === "cp" && sp("tw").row === tree.y && sp("tw").col > tree.x && sp("tw").col < gb2.bands.orchard.x + gb2.bands.orchard.w, "a scarecrow by its tree in the Orchard");
assert.deepStrictEqual([sp("gw").plot, sp("gw").col, sp("gw").row], ["wish:wo", goat.x + goat.w, goat.y + goat.h - 1], "beside its goat, inside the pen");
assert.ok(sp("gw").col > gb2.pen.x && sp("gw").col < gb2.pen.x + gb2.pen.w - 1, "inside the pen's fence");
assert.deepStrictEqual([sp("sw").plot, sp("sw").col, sp("sw").row], ["wish:wc", stone.x + 1, stone.y], "beside its headstone");
assert.strictEqual(b3.plots.lb.lane, null, "no lane beside an item: a walk goes straight");
assert.deepStrictEqual(b3.biomes["project:gardenio"], { all: 7, work: 5, ask: 1, down: 1 }, "counted on the biome sign, the one by the pen's goat too");
["fw", "fw2", "pw", "tw", "gw", "sw", "tend"].forEach(function (id) { assert.deepStrictEqual(b2.spots[id], b3.spots[id], "one place at both levels: " + id); });
var bgo = W.gardenerOutline(w2, bs, b2, b3), bo = bgo["home:gardenio"].children, by = {}; bo.forEach(function (e) { by[e.id] = e; });   // all gardenio's, in its home cell (28.7)
assert.deepStrictEqual([by.fw.plot, by.fw.hot, by.fw.level, by.pw.plot, by.tw.plot, by.gw.plot, by.sw.plot, by.tend.plot, by.fidle.plot],
  ["In the Meadow: Loose bead", "gardener:fw", 2, "In the Nursery: Seed one", "In the Orchard: Closed plan", "In the pen: Open wish", "In the Graveyard: Closed wish", "In the Meadow: Loose two", "In the field of gardenio"]);
var bst = W.gardenerState(w2, bs), bcard = W.gardenerCard(bs[0], bst, w2, "/b", function () { return "4m"; });
assert.ok(bcard.lines.some(function (l) { return l[1] === "Tending: Loose bead in the Meadow"; }), JSON.stringify(bcard.lines));
assert.ok(bst.hots.some(function (h) { return h.kind === "gardener" && h.id === "fw" && h.plot === fl && h.lv === 12; }), "a link at Plan and Bead beside the flower");
var fs = W.gardenerField(bst, "", "/b", 0, null), fsw = fs.filter(function (it) { return it.id === W.FIELD + "fw"; })[0];
assert.deepStrictEqual(fs.map(function (it) { return it.id; }), ["tw", "fw2", "fw", "gw", "pw", "sw", "tend", "gone2", "fidle"].map(function (id) { return W.FIELD + id; }), "those out first in need order, then those at home");
assert.deepStrictEqual([fsw.kind, fsw.task, fsw.plot, fsw.plant.id, fsw.where, fsw.href], ["stand", "lb", "lb", "lb", "In the Meadow: Loose bead", "/b/garden?bead=fw"]);
assert.deepStrictEqual([fsw.line, W.fieldWhere(bst, bs[0])], [(bs[0].agent || "fw") + " — working: Loose bead", "In the field of gardenio and in the Meadow: Loose bead"], "out by its flower: its outline row's words, and the Porch's place words name both places (28.12)");
assert.ok(fs.every(function (it) { return !it.ghost; }), "nothing has a ghost");
var crowd2 = []; for (i = 0; i < 9; i++) crowd2.push(g("f" + i, "working", "alive", "lb", null, i));
var w2c = W.layout(Object.assign({}, doc2, { gardeners: crowd2, home: { gardenio: { field: 32 } } }), null, { now: 1, readonly: true });   // 9 stands and 32 loose beads, one tended (a whole slot): a 12-tile field (29.10), room for six and the badge right of the flower
assert.deepStrictEqual([w2c.index.lb.x, w2c.index.lb.y, w2c.index["project:gardenio"].home.cells.growing.w], [fl.x - 4, fl.y, 12], "the field grows west from the farm's east edge");
fl = w2c.index.lb;
var cst = W.gardenerState(w2c, crowd2), crowdCols = cst[3].plots.lb.shown.map(function (id) { return cst[3].spots[id].col; });
assert.deepStrictEqual([cst[3].plots.lb.shown.length, cst[3].plots.lb.more, cst[2].plots.lb.shown.length, cst[2].plots.lb.more], [6, 3, 3, 6], "the fence caps apply beside an item too");
assert.deepStrictEqual(crowdCols, [fl.x + 1, fl.x + 3, fl.x + 4, fl.x + 5, fl.x + 6, fl.x + 7], "a row to the right, past the neighbouring flower");
assert.deepStrictEqual([cst[3].plots.lb.moreX, cst[3].plots.lb.moreY], [16 * (fl.x + 8), 16 * fl.y], "the +N badge in the next free column");
assert.deepStrictEqual(W.gardenerTip(cst.hots.filter(function (h) { return h.kind === "gmore" && h.id === "lb@3"; })[0], cst, w2, "/b", function () { return "4m"; }), { title: "Loose bead", text: "+3 more gardeners here. Open the Porch to see every one." });
assert.strictEqual(W.standItem(w2, g("x", "working", "alive", "pa-t0", "pa", 1)), null, "a plan's step is no band item");
assert.strictEqual(W.standWhere(w2.index.pa), "Plan pa", "on a plot the words are the plan's title");
// the Growing field, the courtyard's south-east quarter (28.12, 29.2): pair slots, the loose beads in at most half of them (MEADOW_SHARE), the rest counted in
// more, and kept in their sticky slots by id
var bigPlans = []; for (i = 0; i < 9; i++) bigPlans.push(plan("bp" + i, "big", 1));
var loose = function (n, pre) { var o = []; for (var k = 0; k < n; k++) o.push({ id: pre + (k < 10 ? "0" : "") + k, title: "L" + k, project: "big", type: "task", state: "in_progress" }); return o; };
var w33 = W.layout({ plans: bigPlans, meadow: { total: 10, shown: loose(10, "mb") }, home: { big: { field: 47 } } }, null, { now: 1, readonly: true }), b33 = w33.index["project:big"], f33 = b33.home.cells.growing;
assert.deepStrictEqual([f33.w, W.slotsOf(f33, "growing").length, b33.meadow.length, b33.home.zones.growing.more], [16, 49, 10, 0], "a 16-tile field of 7 x 7 pairs, the narrowest whose half holds the farm's 47 loose beads two to a slot (29.10)");
var sl = {}, hf33 = W.homeFill(w33, { work: loose(50, "wk") }, sl)["project:big"].growing;
assert.deepStrictEqual([hf33.items.length, hf33.items[0].slot, hf33.items[5].slot, hf33.more, hf33.badge.n], [38, 5, 7, 12, 12], "the loose work takes the halves after the flowers up to 48, the rest in more");
assert.deepStrictEqual([hf33.items[0].w, hf33.items[1].x - hf33.items[0].x, b33.meadow[1].x - b33.meadow[0].x], [1, 1, 1], "a loose bead is a tile, two to a 2-tile pair slot");
var wFull = W.layout({ plans: bigPlans, meadow: { total: 70, shown: loose(70, "mb") }, home: { big: { field: 47 } } }, null, { now: 1, readonly: true }), bFull = wFull.index["project:big"];
assert.deepStrictEqual([bFull.meadow.length, bFull.home.zones.growing.more, W.homeFill(wFull, { work: loose(3, "wk") }, {})["project:big"].growing.more], [48, 22, 3], "49 slots sow at most 48 flowers in their half; the rest, and the work past them, in more");
assert.deepStrictEqual(bFull.meadow.slice(0, 10).map(function (f) { return [f.id, f.x, f.y]; }), b33.meadow.map(function (f) { return [f.id, f.x, f.y]; }), "a flower that still fits keeps its slot");
var again = W.homeFill(w33, { work: loose(50, "wk").reverse() }, sl)["project:big"].growing;
assert.deepStrictEqual(again.items.map(function (h) { return h.id + ":" + h.x; }), hf33.items.map(function (h) { return h.id + ":" + h.x; }), "sticky slots by id");
// an escalation's executor stands beside its icon in the Escalations cell too (step 7 gd-B5aDmNhmjz, WORLD.escStands), a second
// stand: its plot stand (the plot, else the barn) is unchanged. Twelve escalations make a 5-tile cell of two columns (29.10):
// e-new (on you, newest) at (1, 1), e-old right of it at (2.0625, 1), e-desk under e-new at (1, 2.1875), the in-hand fillers after.
var escs = { open: [{ id: "e-old", title: "Old merge", state: "pending", human: true, since: 5, project: "gardenio", family: "merge" }, { id: "e-desk", title: "Desk merge", state: "pending", waits_on: "the merge desk", since: 9, project: "gardenio", family: "merge" },
  { id: "e-new", title: "New deploy", state: "pending", human: true, since: 7, project: "gardenio", family: "deploy" }], total: 12 };
for (i = 0; i < 9; i++) escs.open.push({ id: "e-f" + i, title: "Filler " + i, state: "approved", since: i / 10, project: "gardenio", family: "other" });
var w4 = W.layout({ plans: [plan("pa", "gardenio", 3)], escalations: escs }, null, { now: 1, readonly: true }), ez = w4.index["home:gardenio/escalation"], ec = { x: ez.x, y: ez.y };
assert.deepStrictEqual(ez.items.map(function (it) { return [it.id, it.x - ec.x, it.y - ec.y]; }), [["esc:e-new", 1, 1], ["esc:e-old", 2.0625, 1], ["esc:e-desk", 1, 2.1875], ["esc:e-f8", 2.0625, 2.1875]].concat(ez.items.slice(4).map(function (it) { return [it.id, it.x - ec.x, it.y - ec.y]; })), "the fixture's slots");
assert.strictEqual(ez.w, 5, "two columns");
var xs = [
  g("x1", "working", "alive", "", null, 50, { escalation: "e-old", escalation_title: "Merge PR #1114" }),   // at the barn: e-old's plot is not drawn
  g("x2", "working", "alive", "", null, 40, { escalation: "e-desk" }),                                       // untitled: the farm document's title
  g("x3", "working", "alive", "", "pa", 30, { escalation: "e-new", escalation_title: "Roll 2026.308" }),    // on the plot of the step that filed it
  g("x4", "working", "alive", "", null, 20, { escalation: "e-gone" }),                                       // closed: no icon, so only the barn
  g("x5", "working", "alive", "pa-t0", "pa", 10, { escalation: "e-old" }),                                   // a task of its own: tending, not running
  g("x6", "working", "alive", "", null, 60, { escalation: "e-old" })                                         // a second on e-old, after x1 in need order: its spot is taken
];
var xst = W.gardenerState(w4, xs), yd = xst.esc, ys = function (id) { var e = yd.byId[id]; return e && [e.esc, e.col - ec.x, e.row - ec.y, e.y, e.face, e.x === 16 * e.col]; };
assert.deepStrictEqual(yd.list.map(function (e) { return e.id; }), ["xs:x3", "xs:x2", "xs:x1"], "the executors whose escalation is drawn, need first (here by since); none for a closed one, one with a task, or a taken spot");
assert.deepStrictEqual(ys("x1"), ["esc:e-old", 3.0625, 1, 16 * (ec.y + 2) - 24, "l", true], "right of its slot, clear of the cell's other icons");
assert.deepStrictEqual(ys("x2"), ["esc:e-desk", 4, 2.1875, 16 * (ec.y + 3.1875) - 24, "l", true], "a filler is right of e-desk: on the cell's east margin, feet on the icon's foot, facing it");
assert.deepStrictEqual(ys("x3"), ["esc:e-new", 4, 1, 16 * (ec.y + 2) - 24, "l", true], "e-old is right of e-new: on the east margin in row 0");
assert.ok(yd.list.every(function (e) { return e.col + 1 <= ec.x + ez.w + 0.25 && ez.items.every(function (it) { return !W.hit({ x: e.col, y: e.row, w: 1, h: 1 }, it); }); }), "no Escalations stand covers an icon, nor leaves the cell");
assert.deepStrictEqual([xst[3].spots.x1.home, xst[3].spots.x2.home, xst[3].spots.x4.home, xst[3].spots.x3.plot, xst[2].spots.x3.plot, xst[3].spots.x1.plot], ["project:gardenio", "project:gardenio", "project:gardenio", "pa", "pa", undefined], "the plot stand is unchanged: at home, else the plot");
assert.deepStrictEqual(yd.hots.map(function (h) { return [h.kind, h.id, h.lv, h.esc.id, h.rect.x - ec.x]; }), [["gesc", "x3", 12, "esc:e-new", 4], ["gesc", "x2", 12, "esc:e-desk", 4], ["gesc", "x1", 12, "esc:e-old", 3.0625]], "a link at Plan and Bead, apart from the plot's");
assert.ok(xst.hots.every(function (h) { return h.kind !== "gesc"; }), "the plot's links ride with the truck; the executors' stands do not (garden.js landGardeners)");
assert.deepStrictEqual(yd.byId.x1.look, W.gardenerLook(xs[0]), "its usual sprite and pose");
assert.strictEqual(W.gardenerState(W.layout({ plans: [plan("pa", "gardenio", 3)] }, null, { now: 1, readonly: true }), xs).esc.list.length, 0, "the escalations closed: their icons and the Escalations stands go together");
// the words: its card, Porch row and outline row say what it runs, with the family from the farm document
var xc = function (id) { var gg = xs.filter(function (x) { return x.id === id; })[0]; return W.gardenerCard(gg, xst, w4, "/b", function () { return "4m"; }); };
var line = function (id) { return xc(id).lines.filter(function (l) { return typeof l[1] === "string" && /^(Running|Tending)/.test(l[1]); }).map(function (l) { return l[1]; }); };
assert.deepStrictEqual([line("x1"), line("x2"), line("x3"), line("x4"), line("x5")], [["Running merge escalation: Merge PR #1114"], ["Running merge escalation: Desk merge"], ["Running deploy escalation: Roll 2026.308 in Plan pa"], ["Running escalation: e-gone"], ["Tending Plan pa"]]);
assert.ok(xc("x1").tip.indexOf(" · running merge escalation: Merge PR #1114 · ") > 0, xc("x1").tip);
assert.deepStrictEqual(W.gardenerTip(yd.hots[2], xst, w4, "/b", function () { return "4m"; }).title, "name-x1", "its name on hover");
var xo = {}, xg = W.gardenerOutline(w4, xs, xst[2], xst[3]); Object.keys(xg).forEach(function (h) { xg[h].children.forEach(function (e) { xo[e.id] = e; }); });   // every project's group (28.7)
assert.deepStrictEqual([xo.x1.plot, xo.x3.plot, xo.x4.plot, xo.x5.plot], ["Running merge escalation: Merge PR #1114", "Running deploy escalation: Roll 2026.308", "Running escalation: e-gone", "Plan pa"]);
assert.deepStrictEqual([W.runningWords(xs[0], null), W.runningWords(xs[4], escs.open[0]), W.runningWords({ escalation: "e9" }, { id: "e9", family: "other", title: "Hand edit" })], ["Running escalation: Merge PR #1114", "", "Running escalation: Hand edit"]);
// the stands go first, in need order and sticky slots; the loose beads after them in at most half; one "+N" counts both (28.12)
var crew = function (n) { var o = []; for (var k = 0; k < n; k++) o.push(g("c" + (k < 10 ? "0" : "") + k, "working", "alive", "", null, k + 1, { project: "big" })); return o; };
var standsOf = function (gs) { return W.gardenerField(W.gardenerState(w33, gs), "", "/b", 0, null); }, kinds = function (z) { var c = {}; z.items.forEach(function (h) { c[h.kind] = (c[h.kind] || 0) + 1; }); return c; };
var f20 = W.homeFill(w33, { agent: standsOf(crew(20)), work: loose(5, "wk") }, {})["project:big"].growing;
assert.deepStrictEqual([kinds(f20).stand, b33.meadow.length + kinds(f20).plant, f20.more, f20.badge], [20, 15, 0, undefined], "49 slots and 20 stands: 20 stands and 15 flowers, no badge");
var f20o = W.homeFill(w33, { agent: standsOf(crew(20)), work: loose(50, "wk") }, {})["project:big"].growing;
assert.deepStrictEqual([kinds(f20o).stand, b33.meadow.length + kinds(f20o).plant, f20o.more, f20o.badge.n, f20o.badge.slot], [20, 48, 12, 12, 48], "past the field: its last slot is the badge, its N the loose beads left out");
var c30 = crew(60); c30.push(g("cdown", "failed", "stuck", "", null, 99, { project: "big" }));
var f30 = W.homeFill(w33, { agent: standsOf(c30), work: loose(3, "wk") }, {})["project:big"].growing, s30 = f30.items.filter(function (h) { return h.kind === "stand"; });
assert.deepStrictEqual([s30.length, kinds(f30).plant, f30.badge.n], [43, undefined, 21], "the stands fill the field before any work; the +N counts the stands and the flowers together");
assert.ok(s30.some(function (h) { return h.id === W.FIELD + "cdown"; }), "a crash is never behind the badge");
var fs3 = {}, one = W.homeFill(w33, { agent: standsOf(crew(3)), work: loose(4, "wk") }, fs3)["project:big"].growing, at = function (z) { var o = {}; z.items.forEach(function (h) { o[h.id] = h.slot; }); return o; };
var two = W.homeFill(w33, { agent: standsOf(crew(4)), work: loose(4, "wk") }, fs3)["project:big"].growing, a1 = at(one), a2 = at(two);
assert.ok(Object.keys(a1).every(function (id) { return a2[id] === a1[id]; }), "a newcomer stand moves nobody: " + JSON.stringify([a1, a2]));
var mid = W.gardenerSpots(w2, [g("pw2", "working", "alive", "pa-t0", "pa", 1)], 3).spots.pw2;
assert.deepStrictEqual([mid.plot, mid.shown, mid.field], ["pa", true, { home: "project:gardenio", task: "pa-t0" }], "a gardener on a plot has both a plot spot and a field stand");
console.log(JSON.stringify({ ok: true }));
