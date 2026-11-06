/* world.js layout under node, run by TestWorldLayoutUnderNode (page_test.go, which says what it covers):
     node world_layout_test.js <web dir>
   It prints a JSON report on its last line, as nodeScript reads. */
var W = require(require("path").join(process.argv[2], "world.js")), assert = require("assert");
function plan(id, project, created, n) {
  var tasks = []; for (var i = 0; i < (n || 0); i++) tasks.push({ id: id + "-t" + i, title: "T" + i, type: i % 3 ? "task" : "bug", state: ["open", "in_progress", "review", "blocked", "deferred"][i % 5], updated: 1 });
  return { id: id, title: "Plan " + id, project: project, created: created, activity: created, counts: { open: n || 0 }, phases: [{ id: id + "-p1", title: "Phase 1", counts: { open: n || 0, closed: 2 }, tasks: tasks }], unphased: { id: "", title: "Other", counts: {}, tasks: [] } };
}
function doc(plans, extra) { var d = { plans: plans, seeds: [], closed_plans: { total: 0, shown: [] }, meadow: { total: 0, shown: [] }, meta: { truncated: false } }; if (extra) Object.keys(extra).forEach(function (k) { d[k] = extra[k]; }); return d; }
function rects(w) { var out = []; w.biomes.forEach(function (b) { b.plots.forEach(function (p) { out.push([p.id, p.x, p.y, p.w, p.h]); p.beds.forEach(function (bd) { bd.cells.forEach(function (c) { out.push([c.id, c.x, c.y, c.w, c.h]); }); }); }); b.orchard.concat(b.meadow, b.nursery).forEach(function (it) { out.push([it.id, it.x, it.y, it.w, it.h]); }); out.push([b.id, b.used.x, b.used.y, b.used.w, b.used.h]); }); return out.sort(); }
// coordinates: every project's lattice index and every plot's {c, r}, the things N-D9 promises never change because of another Thing
function coords(w) { var out = {}; w.biomes.forEach(function (b) { out[b.id] = "k" + b.cell.k; b.plots.forEach(function (p) { out[p.id] = p.at.c + "," + p.at.r; }); }); return out; }
function rectOf(r) { return { x: r.x, y: r.y, w: r.w, h: r.h }; }
function changed(a, b) { return Object.keys(a).filter(function (k) { return b[k] !== undefined && b[k] !== a[k]; }); }
var gapOK = function (a, b) { return a.x + a.w + W.GAP <= b.x || b.x + b.w + W.GAP <= a.x || a.y + a.h + W.GAP <= b.y || b.y + b.h + W.GAP <= a.y; };
// 0. the coordinate helpers and their inverses
for (var k = 0; k < 40; k++) { var la = W.latticeAt(k); assert.ok(la.c >= 0 && la.c < W.LATTICE_W); assert.strictEqual(W.latticeIndex(la.c, la.r), k); }
assert.deepStrictEqual([W.latticeAt(0), W.latticeAt(1), W.latticeAt(4), W.latticeAt(5), W.latticeAt(12)], [{ c: 0, r: 0 }, { c: 1, r: 0 }, { c: 4, r: 0 }, { c: 0, r: 1 }, { c: 2, r: 2 }]);
var seenCell = {};
for (var i = 0; i < W.BED.cap; i++) { var ca = W.cellAt(i); assert.ok(ca.c >= 0 && ca.c < 9 && ca.r >= 0 && ca.r < 9); assert.ok(!seenCell[ca.c + "," + ca.r]); seenCell[ca.c + "," + ca.r] = 1; assert.strictEqual(W.cellIndex(ca.c, ca.r), i); }
assert.deepStrictEqual([0, 2, 3, 8, 9, 17, 18, 27, 80].map(W.cellAt), [{ c: 0, r: 0 }, { c: 2, r: 0 }, { c: 0, r: 1 }, { c: 2, r: 2 }, { c: 3, r: 0 }, { c: 5, r: 2 }, { c: 6, r: 0 }, { c: 0, r: 3 }, { c: 8, r: 8 }], "block-major: block 1 holds 0-8 row-major, block 2 (top-middle) 9-17");
assert.deepStrictEqual([0, 2, 3, 8].map(W.bedAt), [{ c: 0, r: 0 }, { c: 2, r: 0 }, { c: 0, r: 1 }, { c: 2, r: 2 }], "beds in reading order");
for (var s = 0; s < W.SLOT_CAP; s++) { var sa = W.slotAt(s); assert.strictEqual(W.slotIndex(sa.c, sa.r), s); }
assert.deepStrictEqual(W.slotAt(0), { c: 3, r: 2 }, "slot 0 is the centre of the 7x5"); assert.strictEqual(W.slotIndex(7, 0), -1);
// 1. the lattice golden (28.5): the Commons is cell 0 at the origin, taking no room while empty, projects from cell 1 oldest
// first; each region sits centred in its cell; columns are as wide as their widest region, GAP apart
var one = W.layout(doc([plan("a", "gardenio", 1)]), null, { now: 1 });
assert.deepStrictEqual(one.lattice.cells.map(function (c) { return [c.k, c.c, c.r, c.id]; }), [[0, 0, 0, null], [1, 1, 0, "project:gardenio"]]);
assert.deepStrictEqual([one.lattice.cells[0].rect.w, one.biomes[0].region.x, one.yard, one.pen], [0, 0, undefined, undefined], "one project: no empty cell 0, no shared Yard");
assert.deepStrictEqual(one.bbox, W.tiles(one.biomes[0].foot), "the bbox is the biomes' feet");
assert.deepStrictEqual(one.biomes[0].cell, { k: 1, c: 1, r: 0 }); assert.deepStrictEqual(one.biomes[0].grid, { c0: 3, r0: 2, cols: 2, rows: 1, g: 0 }, "one plan: the farm and its plot side by side (29.1)");
var five = []; for (i = 0; i < 6; i++) five.push(plan("f" + i, ["gardenio", "music-clonk", "moorpark", "quarry", "zed", "west"][i], 10 + i));
var w5 = W.layout(doc(five, { meadow: { total: 1, shown: [{ id: "m0", project: "", state: "open", updated: 1 }] } }), null, { now: 1 });
assert.deepStrictEqual(w5.biomes.map(function (b) { return [b.key, b.cell.k, b.cell.c, b.cell.r]; }),
  [["", 0, 0, 0], ["gardenio", 1, 1, 0], ["music-clonk", 2, 2, 0], ["moorpark", 3, 3, 0], ["quarry", 4, 4, 0], ["zed", 5, 0, 1], ["west", 6, 1, 1]], "world order is lattice order; the sixth project wraps under the Commons");
assert.deepStrictEqual([w5.lattice.cols, w5.lattice.rows], [5, 2]);
w5.lattice.cells.forEach(function (c) {
  if (!c.id) return;
  var b = w5.index[c.id], r = c.rect;
  assert.ok(b.region.x >= r.x && b.region.y >= r.y && b.region.x + b.region.w <= r.x + r.w && b.region.y + b.region.h <= r.y + r.h, "a region lies inside its cell: " + c.id);
  assert.ok(Math.abs((b.region.x - r.x) - (r.x + r.w - b.region.x - b.region.w)) <= 1, "and centred in it: " + c.id);
});
var cl = w5.lattice.cells;
assert.strictEqual(cl[1].rect.x - (cl[0].rect.x + cl[0].rect.w), W.GAP, "columns are GAP apart"); assert.strictEqual(cl[5].rect.y - (cl[0].rect.y + cl[0].rect.h), W.GAP, "rows are GAP apart");
assert.strictEqual(cl[0].rect.x + Math.floor(cl[0].rect.w / 2), 0, "cell 0's centre is the origin");
w5.biomes.forEach(function (b, i) { w5.biomes.forEach(function (o, j) { if (j > i) assert.ok(gapOK(b.region, o.region), "clear of each other: " + b.key + " " + o.key); }); });
assert.strictEqual(w5.biomes.filter(function (b) { return b.key === "music-clonk"; })[0].name, "music-clonk");
var three = W.layout(doc([plan("a", "gardenio", 1), plan("b", "music-clonk", 2), plan("c", "Moorpark", 3)]), null, { now: 1 });
assert.deepStrictEqual(three.biomes.map(function (b) { return b.key + "@" + b.cell.k; }), ["gardenio@1", "music-clonk@2", "moorpark@3"], "oldest first from cell 1");
assert.strictEqual(three.biomes[2].name, "Moorpark", "display name keeps its case");
assert.strictEqual(three.biomes[0].palette, W.h32(three.biomes[0].key, 2) % 8);
// the farmstead (29.1, 29.2, 29.10): slot 0 in every biome, a 2 x 2 courtyard either side of a 1-tile east-west arm on row 15 and
// a north and a south arm (Barn north-west, the Escalations and the Mailbox north-east a path apart, Pen south-west, Growing field
// south-east), the sign on its north edge at fx + 12, the bay under the Barn's keep on the farm's west edge (28.10); the farm's rect
// is fixed by shape(), and an empty farm's cells are at their minimums (MEADOW_SHARE: stand_test.js; the sizing: home_cells_test.js)
var hb = w5.biomes[1], hs = hb.home, hc = hs.cells, s0 = W.slotRect(hb.centre, 0);
assert.ok(!("home" in W.shape(0, 0, hb.grid, false, 0)) && !("home" in W.shape(0, 0, hb.grid, false, 0).bands), "the farmstead is a plot, not a band (29.1): shape() gives its rect, farm");
assert.deepStrictEqual([hs.x, hs.y, hs.w, hs.h, hb.home.id], [s0.x, s0.y, 32, 32, "home:gardenio"], "the farmstead is slot 0");
assert.ok(hb.plots.every(function (p) { return p.slot >= 1 && !W.hit(p, hs); }), "no plan takes it");
assert.deepStrictEqual(["mailbox", "escalation", "growing", "barn", "pen"].map(function (z) { return [hc[z].x - hs.x, hc[z].y - hs.y, hc[z].w, hc[z].h]; }), [[27, 0, 5, 15], [23, 0, 3, 15], [24, 16, 8, 16], [0, 0, 22, 15], [0, 16, 8, 16]], "an empty farm: every cell at its minimum, the Barn the rest");
assert.deepStrictEqual([hs.cross, hb.sign, hb.home.sign], [{ xn: hs.x + 22, xs: hs.x + 23, y: hs.y + 15 }, { x: hb.centre.x - 4, y: hs.y, w: 8, h: 2 }, hb.sign], "the arms on columns 22 and 23 and row 15; the sign at fx + 12, the farm's top two rows");
assert.ok(!W.hit(hb.sign, hs.keep.barn) && !W.hit(hb.sign, hs.keep.mailbox), "the sign clear of the keeps");
assert.deepStrictEqual([hs.keep.barn, hs.bay], [{ x: hs.x, y: hs.y, w: 7, h: 5 }, { x: hs.x, y: hs.y + 5, w: 6, h: 4 }], "the barn on its keep, its bay under it on the farm's west edge");
var today = { escalation: 7, mailbox: 8, pen: 15, growing: 16 }, td = W.farmstead(s0, today), tc = td.cells;
assert.deepStrictEqual(["mailbox", "escalation", "growing", "barn", "pen"].map(function (z) { return [tc[z].x - td.x, tc[z].y - td.y, tc[z].w, tc[z].h]; }).concat([[td.cross.xn - td.x, td.cross.xs - td.x, td.cross.y - td.y]]),
  [[24, 0, 8, 15], [16, 0, 7, 15], [16, 16, 16, 16], [0, 0, 15, 15], [0, 16, 15, 16], [15, 15, 15]], "29.2's widths give 29.2's rects: both arms on column 15");
assert.deepStrictEqual(W.farmstead({ x: 10, y: 20, w: 32, h: 32 }, today).cells.growing, { x: 26, y: 36, w: 16, h: 16 }, "pure in its rect and widths");
assert.deepStrictEqual([W.slotsOf(tc.escalation, "escalation").length, W.slotsOf(tc.escalation, "escalation", td.sign).length], [44, 41], "29.2's Escalations hold 44 less the sign's corner");
assert.deepStrictEqual([W.slotsOf(tc.mailbox, "mailbox").length, W.slotsOf(tc.growing, "growing").length, W.slotsOf(tc.barn, "barn").length, W.slotsOf(tc.barn, "barn", td.bay).length, W.penOf({ open: [] }, "", tc.pen).cols * W.penOf({ open: [] }, "", tc.pen).rows], [55, 49, 104, 89, 28], "capacity at the pitches: 7 x 7 pair slots in the field, 4 x 7 goats in the pen");
assert.deepStrictEqual(W.slotsOf(tc.growing, "growing").slice(0, 2).concat([W.slotsOf(tc.growing, "growing")[7]]).map(function (r) { return [r.x - tc.growing.x, r.y - tc.growing.y, r.w]; }), [[1, 1, 2], [3, 1, 2], [1, 3, 2]], "a pair slot is 2 wide on a 2-tile pitch, rows 2 tiles apart");
assert.deepStrictEqual([td.keep.mailbox, W.slotsOf(tc.mailbox, "mailbox", [td.keep.mailbox, td.sign]).length, W.slotsOf(tc.barn, "barn", [td.keep.barn, td.bay, td.sign]).length], [{ x: tc.mailbox.x, y: td.y, w: 4, h: 3 }, 49, 74], "the mailbox's and the barn's pictures take their keeps' slots (ART.drawHome), the sign its own");
// the lanes (29.3): four, 3 tiles wide, in the gaps either side of the farm's column and row, from foot edge to foot edge; the
// plot area holds the lane frame, so one plan is 70 x 38 tiles of it and its region 70 x 53 before the fringe (29.8)
var lf = hb.foot, ln = {}; hb.lanes.forEach(function (l) { ln[l.name] = [l.x, l.y, l.w, l.h]; });
assert.deepStrictEqual(ln, { west: [hs.x - 3, lf.y, 3, lf.h], east: [hs.x + 32, lf.y, 3, lf.h], north: [lf.x, hs.y - 3, lf.w, 3], south: [lf.x, hs.y + 32, lf.w, 3] });
assert.deepStrictEqual([one.biomes[0].bands.nursery.w, one.biomes[0].used.w, one.biomes[0].used.h, W.shape(0, 0, one.biomes[0].grid, false, 0).plotArea], [70, 70, 53, { x: -19, y: -19, w: 70, h: 38 }]);
var homey = { wishes: { open: [{ id: "w1", project: "gardenio", opened: 1 }, { id: "w2", project: "solo", opened: 2 }], open_more: { gardenio: 3 } }, home: { gardenio: { letters: 9, barn: 2 }, quiet: { letters: 1 } },
  escalations: { open: [{ id: "e-old", kind: "escalation", state: "pending", human: true, since: 5, project: "gardenio", family: "merge" }, { id: "e-desk", kind: "escalation", state: "pending", waits_on: "the merge desk", since: 9, project: "gardenio", family: "merge" },
    { id: "e-new", kind: "escalation", state: "pending", human: true, since: 7, project: "Gardenio", family: "deploy" }, { id: "e-loose", kind: "escalation", state: "approved", since: 3, family: "other" }], total: 4 },
  gardeners: [{ id: "g1", project: "gardenio", role: "crew" }, { id: "g2", role: "crew" }, { id: "g3", project: "gardenio", role: "thread" }] };
var wh = W.layout(doc([plan("a", "gardenio", 1)], homey), null, { now: 1 });
var rel = function (b) { var r = b.home; return [r.x - b.x, r.y - b.y, r.w, r.h, b.w, b.h]; };
assert.deepStrictEqual(rel(wh.index["project:gardenio"]), rel(one.biomes[0]), "counts never move a farmstead or size a region");
assert.deepStrictEqual(wh.biomes.map(function (b) { return b.key + "@" + b.cell.k + ":" + b.plots.length; }), ["@0:0", "gardenio@1:1", "solo@2:0", "quiet@3:0"], "home items make a biome (ties on age by h32); the Commons for a gardener with no project or a thread agent (28.3)");
assert.deepStrictEqual([wh.index["home:gardenio/pen"].items.map(function (g) { return g.id; }), wh.index["home:gardenio/pen"].more, wh.index["home:solo/pen"].items.length, wh.index["home:quiet"].counts], [["wish:w1"], 3, 1, { letters: 1 }], "each pen holds its own goats");
// the Escalations cell (28.13): the project's open escalations, by project key, in ESCKIND's order (those on you first, newest
// first), one per Mailbox-pitch slot; the unprojected ones are the Commons's; each in world.index as esc:<id>, apart from its bead
var ez = wh.index["home:gardenio/escalation"], ec = wh.index["project:gardenio"].home.cells.escalation;
assert.deepStrictEqual(ez.items.map(function (it) { return [it.id, it.kind, it.wait.id, it.slot, it.x - ec.x, it.y - ec.y]; }), [["esc:e-new", "escalation", "e-new", 0, 1, 1], ["esc:e-old", "escalation", "e-old", 1, 1, 2.1875], ["esc:e-desk", "escalation", "e-desk", 2, 1, 3.375]], "three fit the one column of a 3-tile cell, clear of the sign (home_cells_test.js: the sign's slots)");
assert.deepStrictEqual([ez.more, wh.index["esc:e-old"] === ez.items[1], wh.index["home:/escalation"].items.map(function (it) { return it.id; })], [0, true, ["esc:e-loose"]], "indexed; an unprojected one in the Commons");
assert.deepStrictEqual([W.atFor(wh, "escalation", "esc:e-old"), W.atFor(wh, "zone", "home:gardenio/escalation"), W.resolve(wh, "gardenio/escalation").id], ["gardenio/esc%3Ae-old", "gardenio/escalation", "home:gardenio/escalation"]);
assert.ok(W.layout({ escalations: { open: [{ id: "e1", project: "lone", state: "pending" }] } }, null, { now: 1 }).index["project:lone"], "an escalation alone makes its project a biome (28.3)");
// ties on age go by h32 then name
var tie = W.layout(doc([plan("a", "bbb", 5), plan("b", "aaa", 5)]), null, { now: 1 }), hk = W.h32("aaa", 1) < W.h32("bbb", 1) ? ["aaa", "bbb"] : ["bbb", "aaa"];
assert.deepStrictEqual(tie.biomes.map(function (b) { return b.key; }), hk);
// 2. determinism: shuffled input gives byte-identical output
var ps = []; for (i = 0; i < 12; i++) ps.push(plan("p" + i, ["gardenio", "music-clonk", ""][i % 3], 100 - i, i % 4));
var md = { total: 3, shown: [{ id: "m1", project: "gardenio", state: "open", updated: 1 }, { id: "m2", project: "", state: "in_progress", updated: 1 }, { id: "m3", project: "gardenio", state: "open", updated: 1 }] };
var sd = [{ id: "s1", project: "gardenio", state: "frosted" }, { id: "s2", project: "music-clonk", state: "ready" }];
var cp = { total: 2, shown: [{ id: "c1", project: "gardenio" }, { id: "c2", project: "music-clonk" }] };
var a = rects(W.layout(doc(ps, { meadow: md, seeds: sd, closed_plans: cp }), null, { now: 5 }));
var b = rects(W.layout(doc(ps.slice().reverse(), { meadow: { total: 3, shown: md.shown.slice().reverse() }, seeds: sd.slice().reverse(), closed_plans: { total: 2, shown: cp.shown.slice().reverse() } }), null, { now: 5 }));
assert.deepStrictEqual(a, b, "shuffled input, same map");
assert.ok(a.length > 20);
// the Commons holds the project-less beads in cell 0 and leads the world order; a loose bead grows in its strip's Growing field
var w12 = W.layout(doc(ps, { meadow: md }), null, { now: 5 }), commons = w12.biomes.filter(function (x) { return x.key === ""; })[0];
assert.deepStrictEqual([commons.cell.k, commons.name], [0, "Commons"]);
assert.ok(commons.meadow.every(function (f) { return W.hit(f, commons.home.cells.growing); }), "in the Growing field");
assert.strictEqual(w12.biomes[0].key, "", "the Commons leads the world order");
assert.ok(commons.meadow.some(function (f) { return f.id === "m2"; }));
// 3. stability with a memo. A state change moves nothing at all; adding a project or a plan changes no project's index and no
// plot's {c, r}, and closing a plan changes no index and moves only the plots after it in the spiral, one step back each (packed
// slots, decision gd-VfOCGDpxg2; N-D9: pixels may shift by whole column widths)
var memo = w12.memo, before = {}; rects(w12).forEach(function (r) { before[r[0]] = r.slice(1).join(","); });
var ps1 = JSON.parse(JSON.stringify(ps)); ps1[0].phases[0].tasks.forEach(function (t) { t.state = "blocked"; t.title = "changed"; }); ps1[4].title = "renamed";
var w1s = W.layout(doc(ps1, { meadow: md }), memo, { now: 6 }), moved = [];
rects(w1s).forEach(function (r) { if (before[r[0]] !== r.slice(1).join(",")) moved.push(r[0]); });
assert.deepStrictEqual(moved, [], "a state change moves no rectangle");
var ps2 = ps.filter(function (p) { return p.id !== "p3"; }).concat([plan("new", "gardenio", 999, 2), plan("q", "quarry", 1, 1)]);
var w13 = W.layout(doc(ps2, { meadow: md }), memo, { now: 6 });
assert.deepStrictEqual(changed(coords(w12), coords(w13)), ["p0"], "no index changes; only the plot after the closed one moves");
assert.deepStrictEqual(w13.index["project:gardenio"].plots.map(function (p) { return p.id + ":" + p.slot; }), ["p9:1", "p6:2", "p0:3", "new:4"], "p0 steps back into p3's slot; the new plan takes the next one");
assert.strictEqual(w13.index["project:quarry"].cell.k, 3, "the newcomer takes the lowest free index, though it is the oldest project");
assert.ok(!w13.index["p3"] && w13.index["new"], "closed plan gone, new plan placed");
assert.strictEqual(w13.memo.slots["p3"].t, 5, "a departed plan keeps its memo entry (30 days)");
assert.deepStrictEqual(w13.memo.cells, { "music-clonk": { k: 1, t: 6 }, gardenio: { k: 2, t: 6 }, quarry: { k: 3, t: 6 } }, "memo v5 cells (music-clonk holds the oldest plan); the Commons is not stored");
// packed slots: the middle plan of five closes and the two after it step back one slot each, in order, so no fallow hole is left;
// a new plan takes the next slot even when it is the oldest, and a plan that comes back with its memo entry gets its place back
var p5 = []; for (i = 0; i < 5; i++) p5.push(plan("v" + i, "gardenio", 10 + i));
var slotsOf = function (w) { return w.index["project:gardenio"].plots.map(function (p) { return p.id + ":" + p.slot; }); };
var wv5 = W.layout(doc(p5), null, { now: 1 }), wv4 = W.layout(doc(p5.filter(function (p) { return p.id !== "v2"; })), wv5.memo, { now: 2 });
assert.deepStrictEqual(slotsOf(wv5), ["v0:1", "v1:2", "v2:3", "v3:4", "v4:5"], "plans from slot 1: slot 0 is the farmstead");
assert.deepStrictEqual(slotsOf(wv4), ["v0:1", "v1:2", "v3:3", "v4:4"], "the gap closes in order");
["v3", "v4"].forEach(function (id) { assert.deepStrictEqual(wv4.index[id].at, W.slotAt(wv5.index[id].slot - 1), id + " moves one spiral step"); });
assert.deepStrictEqual(changed(coords(wv5), coords(wv4)), ["v3", "v4"], "the plots ahead of the closed one stay");
var wv6 = W.layout(doc(p5.filter(function (p) { return p.id !== "v2"; }).concat([plan("v5", "gardenio", 1)])), wv4.memo, { now: 3 });
assert.deepStrictEqual(slotsOf(wv6), ["v0:1", "v1:2", "v3:3", "v4:4", "v5:5"], "a new plan takes the next slot, though it is the oldest");
assert.deepStrictEqual(changed(coords(wv4), coords(wv6)), [], "and moves nobody");
assert.deepStrictEqual(slotsOf(W.layout(doc(p5), wv4.memo, { now: 3 })), ["v0:1", "v1:2", "v2:3", "v3:4", "v4:5"], "a reopened plan gets its place back");
var w7 = W.layout(doc(p5.concat([plan("v5", "gardenio", 15), plan("v6", "gardenio", 16)])), null, { now: 1 }).index["project:gardenio"];
assert.deepStrictEqual([w7.grid.cols, w7.grid.rows, w7.plots[6].slot], [3, 3, 7], "seven plans sit in a 3x3 round the farm with the last slot free");
// a memo'd index and slot are honoured even when cold order would say otherwise
var m2 = W.emptyMemo(); m2.slots["late"] = { s: 1, t: 1 }; m2.cells["gardenio"] = { k: 7, t: 1 };
var wm = W.layout(doc([plan("early", "gardenio", 1), plan("late", "gardenio", 2), plan("x", "other", 0)]), m2, { now: 2 });
assert.deepStrictEqual(wm.index["project:gardenio"].plots.map(function (p) { return p.id + ":" + p.slot; }), ["late:1", "early:2"]);
assert.deepStrictEqual([wm.index["project:gardenio"].cell.k, wm.index["project:other"].cell.k], [7, 1], "memo'd index honoured; the newcomer takes the lowest free one");
assert.strictEqual(wm.lattice.cells.length, 8); assert.strictEqual(wm.lattice.cells[5].id, null, "the holes are empty cells");
// an index stays taken while it is remembered, even when its project is not in the document; 30 days unseen frees it
var gone = W.layout(doc([plan("x", "other", 0), plan("n", "newer", 5)]), wm.memo, { now: 3 });
assert.deepStrictEqual([gone.index["project:other"].cell.k, gone.index["project:newer"].cell.k], [1, 2]);
assert.strictEqual(gone.memo.cells.gardenio.k, 7, "remembered");
var gone2 = W.layout(doc([plan("x", "other", 0), plan("n", "newer", 5), plan("g", "gardenio", 9)]), gone.memo, { now: 4 });
assert.strictEqual(gone2.index["project:gardenio"].cell.k, 7, "a project that comes back gets its index back");
var late = W.layout(doc([plan("x", "other", 0)]), gone.memo, { now: 31 * 86400000 });
assert.ok(!late.memo.cells.gardenio, "a project gone for 30 days frees its index");
// a memo naming the Commons' cell, or two projects on one index, is repaired: the most recently seen keeps it
var bad = { v: 5, cells: { a: { k: 0, t: 1 }, b: { k: 1, t: 1 }, c: { k: 3, t: 1 }, d: { k: 3, t: 2 } }, slots: {} };
var wbad = W.layout(doc([plan("a", "a", 1), plan("b", "b", 2), plan("c", "c", 3), plan("d", "d", 4)]), bad, { now: 5 }), kb = {};
wbad.biomes.forEach(function (x) { kb[x.key] = x.cell.k; });
assert.deepStrictEqual(kb, { a: 2, b: 1, c: 4, d: 3 }, "d (seen last) keeps 3, b its 1; a and c are newcomers on the lowest free indices");
// memo v5 (29.5): a v4 memo keeps its cells and its slots shifted up by one, so the first load re-packs the plans once in the
// order they had, the rank-1 plan east of the farm; v1, v2 and v3 memos keep their plot slots, shifted the same, and drop their
// cells (H-D8); the memo is written as v5 and says it changed
var v1 = { v: 1, domains: { gardenio: { i: -1, j: 1, t: 1 } }, slots: { late: { s: 0, t: 1 } } };
var v2 = { v: 2, domains: { gardenio: { x: 200, y: -200, r: { x: 1, y: 1, w: 1, h: 1 }, t: 1 } }, slots: { late: { s: 0, t: 1 } } };
var v3 = { v: 3, cells: { gardenio: { k: 4, t: 1 } }, slots: { late: { s: 0, t: 1 } } };
var v4 = { v: 4, cells: { gardenio: { k: 4, t: 1 } }, slots: { late: { s: 0, t: 1 }, early: { s: 1, t: 1 } } };
[v1, v2, v3, v4].forEach(function (old) {
  var wv = W.layout(doc([plan("early", "gardenio", 1), plan("late", "gardenio", 2)]), old, { now: 1 });
  assert.deepStrictEqual(wv.biomes[0].plots.map(function (p) { return p.id + ":" + p.slot; }), ["late:1", "early:2"], "v" + old.v + " slots survive, one up");
  assert.strictEqual(wv.biomes[0].cell.k, old.v === 4 ? 4 : 1, old.v === 4 ? "v4 cells are kept" : "v" + old.v + " cells are dropped: the project is packed into the lattice");
  assert.deepStrictEqual([wv.memo.v, wv.memo.slots.late.s, wv.memoChanged], [5, 1, true]); assert.ok(!wv.memo.domains, "no domains in v5");
});
var v5 = W.layout(doc([plan("early", "gardenio", 1), plan("late", "gardenio", 2)]), v4, { now: 1 });
assert.strictEqual(W.layout(doc([plan("early", "gardenio", 1), plan("late", "gardenio", 2)]), v5.memo, { now: 1 }).memoChanged, false, "a v5 memo read back is unchanged");
// growth: a project going from 1 to 10 plans keeps its index and every plot's coordinate, and so does its neighbour
var g1 = W.layout(doc([plan("a", "gardenio", 1), plan("n", "nextdoor", 2)]), null, { now: 1 }), gplans = [plan("a", "gardenio", 1), plan("n", "nextdoor", 2)];
for (var gi = 1; gi < 10; gi++) gplans.push(plan("g" + gi, "gardenio", 10 + gi));
var g10 = W.layout(doc(gplans), g1.memo, { now: 2 });
assert.deepStrictEqual(changed(coords(g1), coords(g10)), []);
assert.deepStrictEqual(g10.index["project:gardenio"].grid, { c0: 1, r0: 0, cols: 4, rows: 4, g: 0 }, "ten plans open the second ring from its top-left: 4x4 slots");
assert.ok(gapOK(g10.index["project:gardenio"].region, g10.index["project:nextdoor"].region), "the grown region keeps its gap: its column widened");
// tidy re-numbers from scratch (slots kept): the result is the cold layout of the same document
var tidy = W.layout(doc(gplans), gone2.memo, { now: 4, tidy: true }), coldT = W.layout(doc(gplans), null, { now: 4 });
assert.deepStrictEqual(rects(tidy), rects(coldT), "tidy gives the cold map");
// readonly never writes the memo
var ro = W.layout(doc([plan("a", "gardenio", 1)]), null, { now: 1, readonly: true });
assert.strictEqual(ro.memoChanged, false); assert.deepStrictEqual(ro.memo.slots, {}); assert.deepStrictEqual(ro.memo.cells, {});
// 4. the spiral: 35 unique slots inside the 7x5 grid, slot 0 (the farmstead) at the centre, the first ring clockwise from the
// east (X-D2), the second from its top-left; square plots at a 35 pitch, so every gap is a 3-tile lane's width (X-D5)
assert.strictEqual(W.SPIRAL.length, 35); var seen = {};
W.SPIRAL.forEach(function (s) { assert.ok(Math.abs(s.c) <= 3 && Math.abs(s.r) <= 2); assert.ok(!seen[s.c + "," + s.r]); seen[s.c + "," + s.r] = 1; });
assert.deepStrictEqual(W.SPIRAL.slice(0, 10), [{ c: 0, r: 0 }, { c: 1, r: 0 }, { c: 1, r: 1 }, { c: 0, r: 1 }, { c: -1, r: 1 }, { c: -1, r: 0 }, { c: -1, r: -1 }, { c: 0, r: -1 }, { c: 1, r: -1 }, { c: -2, r: -2 }]);
assert.deepStrictEqual(W.slotRect({ x: 0, y: 0 }, 0), { x: -16, y: -16, w: 32, h: 32 }); assert.deepStrictEqual(W.slotRect({ x: 0, y: 0 }, 1), { x: 19, y: -16, w: 32, h: 32 }); assert.deepStrictEqual(W.slotRect({ x: 0, y: 0 }, 7), { x: -16, y: -51, w: 32, h: 32 });
// the plot grid is the box of the farm, slot 1 and the used slots (29.1, X-D3; sizes 29.8)
var gridFor = function (n) { var l = []; for (var k = 0; k < n; k++) l.push(plan("q" + k, "grow", k)); var bb = W.layout(doc(l, { wishes: { open: [{ id: "w", project: "grow" }] } }), null, { now: 1 }).index["project:grow"]; return [bb.grid.cols, bb.grid.rows, W.shape(0, 0, bb.grid, false, 0).plotArea.w, W.shape(0, 0, bb.grid, false, 0).plotArea.h]; };
assert.deepStrictEqual([0, 1, 2, 3, 4, 5, 6, 8, 34].map(gridFor), [[2, 1, 70, 38], [2, 1, 70, 38], [2, 2, 70, 70], [2, 2, 70, 70], [3, 2, 102, 70], [3, 2, 102, 70], [3, 3, 102, 102], [3, 3, 102, 102], [7, 5, 242, 172]]);
// 5. capacity: 35 plans in one project gives 34 plots plus one overflow marker, and bands step out with the used rows; a full
// project is 242 tiles of plots wide and 172 tall
var many = []; for (i = 0; i < 35; i++) many.push(plan("m" + i, "big", i));
var wb = W.layout(doc(many), null, { now: 1 }).biomes[0];
assert.strictEqual(W.SLOT_CAP, 34); assert.strictEqual(wb.plots.length, 34); assert.strictEqual(wb.overflow.n, 1); assert.deepStrictEqual(wb.overflow.ids, ["m34"]);
assert.deepStrictEqual(wb.grid, { c0: 0, r0: 0, cols: 7, rows: 5, g: 0 });
var px = wb.plots.map(function (p) { return p.x; }), py = wb.plots.map(function (p) { return p.y; });
assert.deepStrictEqual([Math.max.apply(null, px) + 32 - Math.min.apply(null, px), Math.max.apply(null, py) + 32 - Math.min.apply(null, py)], [242, 172]);
wb.plots.forEach(function (p) { assert.deepStrictEqual(p.at, W.slotAt(p.slot)); });
assert.ok(wb.overflow.y > wb.bands.nursery.y + wb.bands.nursery.h, "the overflow crate is in its own row under the Nursery");
var rowsFor = function (n) { var l = []; for (var k = 0; k < n; k++) l.push(plan("r" + k, "big", k)); var bb = W.layout(doc(l), null, { now: 1 }).biomes[0]; return bb.bands.nursery.y - bb.bands.orchard.y; };
assert.ok(rowsFor(1) < rowsFor(2) && rowsFor(2) === rowsFor(5) && rowsFor(5) < rowsFor(6) && rowsFor(6) === rowsFor(8) && rowsFor(8) < rowsFor(9), "bands step at the 2nd, 6th and 9th plan only");
// 6. no overlap inside a biome, and everything inside its region
function overlaps(w) {
  w.biomes.forEach(function (bm) {
    var all = []; bm.plots.forEach(function (p) { all.push(p); }); all = all.concat(bm.orchard, bm.nursery, bm.graveyard, bm.pen.items, ["mailbox", "barn"].map(function (z) { return bm.home.zones[z]; })); if (bm.overflow) all.push(bm.overflow);
    // the sign stands on the farm's north edge (29.2), clear of the keeps and of every plot
    assert.ok(W.hit(bm.sign, bm.home) && !W.hit(bm.sign, bm.home.keep.barn) && !W.hit(bm.sign, bm.home.keep.mailbox) && bm.plots.every(function (p) { return !W.hit(bm.sign, p); }), "the sign on the farm: " + bm.id);
    // no plot, band item or farm tile stands on a lane, and every lane runs across the foot (29.3, X-D6)
    assert.strictEqual(bm.lanes.length, 4);
    bm.lanes.forEach(function (l) { assert.ok(l.x >= bm.foot.x && l.y >= bm.foot.y && l.x + l.w <= bm.foot.x + bm.foot.w && l.y + l.h <= bm.foot.y + bm.foot.h, "a lane inside the foot: " + l.name); all.concat([bm.home]).forEach(function (it) { assert.ok(!W.hit(it, l), "on the " + l.name + " lane: " + it.id); }); });
    bm.meadow.forEach(function (f) { assert.ok(W.hit(f, bm.home.cells.growing), "a loose bead in the Growing field: " + f.id); });
    for (var x = 0; x < all.length; x++) {
      assert.ok(all[x].x >= bm.x && all[x].y >= bm.y && all[x].x + all[x].w <= bm.x + bm.w && all[x].y + all[x].h <= bm.y + bm.h, "inside the region: " + all[x].id);
      for (var y = x + 1; y < all.length; y++) assert.ok(!W.hit(all[x], all[y]), "overlap " + all[x].id + " " + all[y].id);
    }
    assert.deepStrictEqual(bm.foot, { x: bm.region.x, y: bm.region.y, w: bm.region.w, h: bm.region.h }, "the region is the foot: nothing is reserved for growth");
  });
}
var full = []; for (i = 0; i < 40; i++) full.push({ id: "f" + i, project: "big", state: "open", updated: 1 });
var seeds = []; for (i = 0; i < 40; i++) seeds.push({ id: "s" + i, project: "big", state: "frosted" });   // drafts: a ready seed is staked on a plot (TestWorldLayoutStaked)
var closed = []; for (i = 0; i < 24; i++) closed.push({ id: "c" + i, project: "big" });
var wf = W.layout(doc(many, { meadow: { total: 40, shown: full }, seeds: seeds, closed_plans: { total: 24, shown: closed } }), null, { now: 1 });
overlaps(wf); overlaps(w13); overlaps(three); overlaps(w5);
// slot 0 is home in every biome (29.1, X-D1): the Commons, a farm-only biome, one plan and a full one alike
[wf, w13, three, w5, wh].forEach(function (w) { w.biomes.forEach(function (bm) {
  assert.deepStrictEqual(rectOf(bm.home), W.slotRect(bm.centre, 0), "the farmstead is slot 0: " + bm.id);
  assert.ok(bm.plots.every(function (p) { return p.slot >= 1; }), "no plan on the farm: " + bm.id);
  assert.ok(bm.grid.c0 <= 3 && bm.grid.c0 + bm.grid.cols > 4 && bm.grid.r0 <= 2 && bm.grid.r0 + bm.grid.rows > 2, "the grid holds the farm and slot 1: " + bm.id);
}); });
assert.ok(["", "solo", "quiet"].every(function (k) { return wh.index["project:" + k].plots.length === 0; }), "the farm-only biomes are among them");
assert.deepStrictEqual([wf.biomes[0].meadow.length, wf.biomes[0].home.zones.growing.more], [20, 20], "the loose beads in half the field, two to a slot (28.12): its minimum's 21 slots, as the farm counts none (home.field, 29.10)"); overlaps(wh); assert.strictEqual(wf.biomes[0].orchard.length, 24); assert.ok(wf.biomes[0].nursery.length >= 26);
// 7. beds (spec 5.3, 5.4): a 3x3 of 9x9 beds at offsets 2, 12 and 22; tasks block-major; the crate after the listed tasks, then stubble
function phase(id, n, extra) { var ts = []; for (var j = 0; j < n; j++) ts.push({ id: id + "-t" + j, title: "T", type: "task", state: "open", updated: 1 }); var o = { id: id, title: "Ph " + id, status: "open", counts: { open: n }, tasks: ts }; if (extra) Object.keys(extra).forEach(function (k) { o[k] = extra[k]; }); return o; }
function harvested(id, n) { var ts = []; for (var j = 0; j < n; j++) ts.push({ id: id + "-c" + j, title: "C", type: "task", state: "closed", updated: 1 }); return ts; }
var bp = { id: "bp", title: "Beds", project: "beds", created: 1, counts: {}, phases: [phase("h1", 0, { status: "closed", counts: { closed: 4 } }), phase("h2", 5, { counts: { open: 5, closed: 3 }, closed: harvested("h2", 2) }), phase("h3", 12), phase("h4", 60, { more: 7, counts: { open: 67, closed: 30 }, closed: harvested("h4", 25) })],
  unphased: { id: "", title: "Other", counts: { open: 1 }, tasks: [{ id: "u1", title: "U", type: "task", state: "open", updated: 1 }] } };
var wbeds = W.layout(doc([bp]), null, { now: 1 }), P = wbeds.index.bp, B = P.beds;
assert.deepStrictEqual(B.map(function (bd) { return [bd.id, bd.n, bd.at.c, bd.at.r, bd.x - P.x, bd.y - P.y, bd.w, bd.h]; }),
  [["h1", 1, 0, 0, 2, 2, 9, 9], ["h2", 2, 1, 0, 12, 2, 9, 9], ["h3", 3, 2, 0, 22, 2, 9, 9], ["h4", 4, 0, 1, 2, 12, 9, 9], ["bp/other", 5, 1, 1, 12, 12, 9, 9]], "Other follows the phases; its id is <plan>/other");
for (var x = 0; x < B.length; x++) for (var y = x + 1; y < B.length; y++) assert.ok(!W.hit(B[x], B[y]), "beds do not overlap");
B.forEach(function (bd) { assert.ok(bd.x > P.x && bd.y > P.y && bd.x + bd.w < P.x + P.w && bd.y + bd.h < P.y + P.h, "inside the fence"); bd.cells.forEach(function (c) { assert.ok(W.hit(c, bd) && c.x >= bd.x && c.y >= bd.y && c.x < bd.x + 9 && c.y < bd.y + 9, "a cell inside its bed"); assert.deepStrictEqual([c.x - bd.x, c.y - bd.y], [c.at.c, c.at.r]); assert.deepStrictEqual(c.at, W.cellAt(c.i)); }); });
assert.deepStrictEqual(B.map(function (bd) { return [bd.closed, bd.current, bd.pips]; }), [[true, false, 4], [false, true, 1], [false, false, 0], [false, false, 1], [false, false, 0]], "the first phase not closed is current; pips are done/total in quarters");
assert.deepStrictEqual(B.map(function (bd) { return bd.used; }), [{ c0: 0, r0: 0, cols: 0, rows: 0 }, { c0: 0, r0: 0, cols: 3, rows: 3 }, { c0: 0, r0: 0, cols: 6, rows: 3 }, { c0: 0, r0: 0, cols: 9, rows: 9 }, { c0: 0, r0: 0, cols: 3, rows: 3 }], "used bounds are the used blocks: 5 tasks 3x3, 12 tasks 6x3");
assert.deepStrictEqual([B[0].stubble, B[1].stubble, B[1].stubbleAt, B[1].crate], [4, 1, 7, null], "a closed phase is stubble; the listed closed steps are cells after the tasks, the rest of the count stubble after them");
assert.deepStrictEqual(B[1].cells.map(function (c) { return c.at.c + "," + c.at.r; }), ["0,0", "1,0", "2,0", "0,1", "1,1", "2,1", "0,2"], "a 5-task phase and its 2 listed closed steps are a tight cluster in block 1");
assert.deepStrictEqual(B[1].cells.slice(5).map(function (c) { return [c.id, c.kind, c.task.state, c.i]; }), [["h2-c0", "task", "closed", 5], ["h2-c1", "task", "closed", 6]], "a listed closed step is a task cell; undated plants keep the farm's order, tasks then closed");
assert.deepStrictEqual(B[2].cells.slice(8, 11).map(function (c) { return c.at.c + "," + c.at.r; }), ["2,2", "3,0", "4,0"], "the tenth task opens block 2, top-middle");
// 9x9 capacity: 60 listed tasks plus a crate for the 7 more fit, then 20 of the 25 listed closed steps fill the bed (the server
// sends at most what fits, world.js cuts too), and there is no room left for stubble
assert.strictEqual(B[3].cells.length, 80); assert.deepStrictEqual([B[3].crate.id, B[3].crate.n, B[3].crate.i, B[3].crate.at], ["h4/more", 7, 80, W.cellAt(80)]);
assert.deepStrictEqual([B[3].cells[60].id, B[3].cells[60].i, B[3].cells[60].at, B[3].cells[79].id], ["h4-c0", 60, W.cellAt(60), "h4-c19"], "the crate follows the plants, the closed steps among them");
assert.deepStrictEqual([B[3].stubble, B[3].stubbleAt], [0, 81]);
// planted by created then id (gd-wDdun2AVwq): the listed tasks and closed steps share one order, so a step keeps its cell when
// its status changes or it closes, and the crate follows the plants
var dated = { id: "dp", title: "Dated", project: "beds", created: 1, counts: {}, unphased: null, phases: [{ id: "d1", title: "D1", status: "open", counts: { open: 3, closed: 2 }, more: 1,
  tasks: [{ id: "t-new", title: "N", type: "task", state: "in_progress", created: 40, updated: 9 }, { id: "t-old", title: "O", type: "task", state: "open", created: 10, updated: 1 }],
  closed: [{ id: "c-mid", title: "M", type: "task", state: "closed", created: 20, updated: 8 }, { id: "c-tie", title: "T", type: "task", state: "closed", created: 10, updated: 7 }] }] };
var DB = W.layout(doc([dated]), null, { now: 1 }).index.dp.beds[0];
assert.deepStrictEqual(DB.cells.map(function (c) { return [c.id, c.i]; }), [["c-tie", 0], ["t-old", 1], ["c-mid", 2], ["t-new", 3]], "by created, a tie by id; closed steps among the tasks");
assert.deepStrictEqual([DB.crate.i, DB.crate.n, DB.used, DB.stubble, DB.stubbleAt], [4, 1, { c0: 0, r0: 0, cols: 3, rows: 3 }, 0, 5], "the crate follows the plants, stubble after it");
var flipped = JSON.parse(JSON.stringify(dated)); flipped.phases[0].tasks[0].state = "blocked"; flipped.phases[0].tasks[1].state = "review"; flipped.phases[0].tasks[1].updated = 99;
assert.deepStrictEqual(W.layout(doc([flipped]), null, { now: 1 }).index.dp.beds[0].cells.map(function (c) { return c.id; }), ["c-tie", "t-old", "c-mid", "t-new"], "a status or updated change moves nothing");
// a bed carries its phase's arms in words for its stake's hot (heraldry step 4.3); none on a phase without, none on Other
var ap = JSON.parse(JSON.stringify(bp)); ap.phases[1].heraldry = "sand . . x . ."; ap.phases[1].heraldry_name = "arms on sand: compass centre"; ap.unphased.heraldry_name = "arms on red: key centre";
assert.deepStrictEqual(W.layout(doc([ap]), null, { now: 1 }).index.bp.beds.map(function (bd) { return bd.arms; }), ["", "arms on sand: compass centre", "", "", ""]);
// past 81 listed and more: 80 tasks and a crate
var huge = { id: "hg", title: "H", project: "beds", created: 1, counts: {}, phases: [phase("hx", 85)], unphased: null };
var HB = W.layout(doc([huge]), null, { now: 1 }).index.hg.beds[0];
assert.deepStrictEqual([HB.cells.length, HB.crate.n, HB.crate.i, HB.stubble], [80, 5, 80, 0]);
// past 9 phases (plus Other): eight beds, a "+N phases" crate in position 9, and the plot is overgrown
var tall = { id: "tl", title: "T", project: "beds", created: 1, counts: {}, phases: [], unphased: { id: "", counts: { open: 1 }, tasks: [] } };
for (i = 0; i < 10; i++) tall.phases.push(phase("tl" + i, 1));
var TP = W.layout(doc([tall]), null, { now: 1 }).index.tl;
assert.strictEqual(TP.beds.length, 8); assert.deepStrictEqual([TP.crate.id, TP.crate.kind, TP.crate.n, TP.crate.at, TP.crate.x - TP.x, TP.crate.y - TP.y], ["tl/phases", "phases", 3, { c: 2, r: 2 }, 22, 22]);
assert.strictEqual(TP.overgrown, true); assert.strictEqual(TP.moreRows, 3); assert.strictEqual(P.overgrown, false); assert.strictEqual(P.crate, null);
var nine = { id: "nn", title: "N", project: "beds", created: 1, counts: {}, phases: tall.phases.slice(0, 9), unphased: null };
assert.strictEqual(W.layout(doc([nine]), null, { now: 1 }).index.nn.beds.length, 9, "nine phases fit");
// 8. empty and tiny documents
var e = W.layout(null, null, { now: 1 }); assert.deepStrictEqual(e.biomes, []); assert.deepStrictEqual(e.bbox, W.domainRect(0, 0));
assert.deepStrictEqual(e.lattice.cells.map(function (c) { return c.id; }), [null], "the Commons's cell, empty");
assert.strictEqual(W.layout({ plans: [] }, { v: 9, garbage: true }, { now: 1 }).biomes.length, 0);
// visible: the two-level cull returns tasks only from level 2
var vw = W.layout(doc([plan("v", "gardenio", 1, 5)]), null, { now: 1 }), vb = vw.biomes[0], view = W.tiles(vb.plots[0]);
assert.strictEqual(W.visible(vw, view, 1).tasks.length, 0); assert.strictEqual(W.visible(vw, view, 2).tasks.length, 5); assert.strictEqual(W.visible(vw, { x: 1e7, y: 1e7, w: 1, h: 1 }, 3).biomes.length, 0);
// the footprint fringe: inside the used rect always, never past 5 tiles out
assert.ok(W.inFoot(vb, vb.used.x, vb.used.y)); assert.ok(!W.inFoot(vb, vb.used.x - 6, vb.used.y));
console.log(JSON.stringify({ ok: true }));
