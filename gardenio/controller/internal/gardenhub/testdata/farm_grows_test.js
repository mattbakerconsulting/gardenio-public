/* The farm grows with its cells (homestead-world-design.md 29.11, X-D14; plan gd-PJB3D7glwF step 15 gd-gFMObwPAzs), run by
   TestFarmGrowsUnderNode (page_test.go): node farm_grows_test.js <web dir>. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert"), web = process.argv[2], W = require(path.join(web, "world.js"));
function plan(id, project, created) { return { id: id, title: "Plan " + id, project: project, created: created || 1, activity: 1, counts: {}, phases: [], unphased: { id: "", counts: {}, tasks: [] } }; }
function escs(n, project) { var o = []; for (var i = 0; i < n; i++) o.push({ id: project + "-e" + (i < 10 ? "0" : "") + i, state: "pending", human: true, since: i, project: project, family: "merge" }); return o; }
function many(n, pre, project, extra) { var o = []; for (var i = 0; i < n; i++) { var it = { id: pre + i, title: pre + i, project: project, created: i + 1, opened: i + 1, closed: i + 1 }; Object.keys(extra || {}).forEach(function (k) { it[k] = extra[k]; }); o.push(it); } return o; }
var clone = function (o) { return JSON.parse(JSON.stringify(o)); };

// W at each row of 29.11's table, and at the step's edges: need 32, 33, 36 and 37 give 32, 36, 36 and 40
var TABLE = [[{ escalation: 3, mailbox: 5, pen: 8, growing: 8 }, 32, 22, 15], [{ escalation: 5, mailbox: 5, pen: 8, growing: 8 }, 36, 24, 19],
  [{ escalation: 7, mailbox: 8, pen: 15, growing: 16 }, 40, 23, 8], [{ escalation: 11, mailbox: 9, pen: 23, growing: 22 }, 48, 26, 2]];
TABLE.forEach(function (t) {
  var w = W.homeW(t[0]), h = W.farmstead({ x: 0, y: 0, w: w, h: 32 }, t[0]), c = h.cells;
  assert.deepStrictEqual([w, c.barn.w, c.growing.x - 1 - (c.pen.x + c.pen.w), h.h], [t[1], t[2], t[3], 32], "29.11's table: " + JSON.stringify(t[0]));
});
assert.deepStrictEqual([[3, 5], [3, 6], [7, 5], [8, 5]].map(function (em) { return W.homeW({ escalation: em[0], mailbox: em[1] }); }), [32, 36, 36, 40], "need 32, 33, 36, 37");
assert.deepStrictEqual([W.homeW({ pen: 23, growing: 8 }), W.homeW({ pen: 23, growing: 10 })], [32, 36], "the south half needs P + 1 + F");

// the slide: a biome at g deep-equals its g = 0 biome cut at the farm's base east edge, every rect east of the cut at x + g and
// every rect across it at w + g; no y or h changes, no rect west of the cut changes, and every band item keeps its cell
var full = function (n, plans) {   // n: [escalations, letters past them, open wishes, loose field beads]
  return W.layout({ plans: many(plans, "p", "zoo"), closed_plans: { shown: many(24, "c", "zoo") }, seeds: many(30, "s", "zoo", { state: "frosted" }),
    wishes: { closed: many(20, "g", "zoo"), open: many(n[2], "w", "zoo") }, escalations: { open: escs(n[0], "zoo") }, home: { zoo: { letters: n[0] + n[1], field: n[3] } } }, null, { now: 1, readonly: true }).index["project:zoo"];
};
var sides = {};
[[[12, 0, 0, 0], 4, 36], [[45, 0, 0, 0], 8, 36], [[88, 60, 0, 0], 12, 36], [[0, 0, 49, 80], 16, 36], [[12, 0, 0, 0], 4, 2], [[88, 60, 49, 80], 16, 2]].forEach(function (t) {
  var b0 = full([0, 0, 0, 0], t[2]), cut = b0.home.x + 32, b = full(t[0], t[2]), g = t[1];
  assert.strictEqual(b.home.w - 32, g, t[0] + " escalations grow the farm " + g);
  assert.deepStrictEqual([b.centre, b.grid.g], [b0.centre, g], "the farm's centre stays, grid.g is its growth");
  var moved = function (r0) { if (!r0) return r0; var r = clone(r0); if (r.x >= cut) r.x += g; else if (r.x + r.w > cut) r.w += g; return r; };
  var rect = function (r) { return r && { x: r.x, y: r.y, w: r.w, h: r.h }; };
  var pairs = [["region", b0.region, b.region], ["used", b0.used, b.used], ["foot", b0.foot, b.foot], ["overflow", rect(b0.overflow), rect(b.overflow)], ["sign", b0.sign, b.sign], ["bay", b0.home.bay, b.home.bay]];
  b0.lanes.forEach(function (l, k) { pairs.push(["lane " + l.name, l, b.lanes[k]]); });
  Object.keys(b0.bands).forEach(function (k) { pairs.push(["band " + k, rect(b0.bands[k]), rect(b.bands[k])]); });
  b0.plots.forEach(function (p, k) { pairs.push(["plot " + p.id, rect(p), rect(b.plots[k])]); });
  ["orchard", "nursery", "graveyard"].forEach(function (band) {
    b0[band].forEach(function (it) { sides[band + (it.x >= cut ? " east" : " west")] = 1; });
    assert.deepStrictEqual(b[band].map(function (it) { return it.id; }), b0[band].map(function (it) { return it.id; }), band + "'s items are the same");
    b0[band].forEach(function (it, k) { pairs.push([band + " " + it.id, rect(it), rect(b[band][k])]); });
  });
  pairs.forEach(function (p) { assert.deepStrictEqual(p[2], moved(p[1]), "slid by " + g + ": " + p[0]); });
  assert.deepStrictEqual(rect(b.home), { x: b0.home.x, y: b0.home.y, w: 32 + g, h: 32 }, "the farm is g wider in place");
  b0.plots.forEach(function (p) { sides["plot" + (p.x >= cut ? " east" : " west")] = 1; });
  assert.deepStrictEqual([b.region.h, b.bands.graveyard.h, b.bands.graveyard.cols, b.bands.graveyard.at], [b0.region.h, b0.bands.graveyard.h, b0.bands.graveyard.cols, b0.bands.graveyard.at], "the region's height and the Graveyard's rows at every g");
  b.plots.forEach(function (p) { assert.ok(W.SPIRAL[p.slot].c > 0 ? p.x >= cut + g : p.x + p.w <= cut, "a plot keeps clear of the grown farm: " + p.id); });
  var gx = W.slotRect(b.centre, 1, g);
  assert.deepStrictEqual([W.slotRect(b.centre, 0, g), gx.x], [rect(b.home), b.plots[0].x], "slotRect(c, k, g): the farm g wider, slot 1 g east");
  assert.deepStrictEqual(W.shape(b.centre.x, b.centre.y, b.grid, !!b.overflow, b.counts.graves).foot, b.foot, "shape() reads g from the grid");
});

assert.deepStrictEqual(Object.keys(sides).sort(), ["graveyard east", "graveyard west", "nursery east", "nursery west", "orchard east", "orchard west", "plot east", "plot west"], "items on both sides of the cut");

// the lattice: with every g at 0 a region is centred in its cell and a column is as wide as its widest region, as before 29.11
var proj = ["ant", "bee", "cat", "dog", "eel", "fox", "gnu"];   // lattice cells 1 to 7, so cell 6 shares column 1 with cell 1
var lat = function (n, plans) {
  var ps = [], extra = { escalations: { open: escs(n, "ant") }, home: { ant: { letters: n } } };
  proj.forEach(function (k, i) { for (var j = 0; j < (plans ? plans[i] : 1); j++) ps.push(plan(k + j, k, i + 1)); });
  extra.plans = ps; return W.layout(extra, null, { now: 1, readonly: true });
};
var at = function (w, k) { return w.index["project:" + k]; };
var l0 = lat(0, [1, 1, 3, 1, 1, 5, 2]);
assert.deepStrictEqual(proj.map(function (k) { return at(l0, k).cell.k; }), [1, 2, 3, 4, 5, 6, 7], "oldest first from cell 1");
l0.biomes.forEach(function (b) {
  var cr = l0.lattice.cells[b.cell.k].rect, col = l0.biomes.filter(function (o) { return o.cell.c === b.cell.c; });
  assert.strictEqual(b.region.x, cr.x + Math.floor((cr.w - b.region.w) / 2), "centred in its cell: " + b.id);
  assert.strictEqual(cr.w, Math.max.apply(null, col.map(function (o) { return o.region.w; })), "its column as wide as its widest: " + b.id);
});
// a step in column c moves no other project in c, and moves the columns east of c only by the change in colW[c]
[[1, 1, 1, 1, 1, 1, 1], [1, 1, 3, 1, 1, 5, 2], [1, 1, 3, 1, 1, 8, 2]].forEach(function (plans) {
  var a = lat(0, plans), z = lat(30, plans), d = z.lattice.cells[1].rect.w - a.lattice.cells[1].rect.w;
  assert.ok(at(z, "ant").grid.g === 4 && d >= 0 && d <= 4, "ant widens by 4, its column by at most that: " + plans);
  proj.forEach(function (k) {
    var p = at(a, k), q = at(z, k), dx = p.cell.c > 1 ? d : 0;
    assert.deepStrictEqual([q.centre.x, q.centre.y, q.region.y, q.region.h], [p.centre.x + dx, p.centre.y, p.region.y, p.region.h], k + " moves east by " + dx + " and never vertically: " + plans);
  });
  if (plans[5] === 1) assert.strictEqual(d, 4, "ant is the widest in its column, so its column widens by 4");
  if (plans[5] === 8) assert.strictEqual(d, 0, "fox is wider than ant's grown farm, so no column moves");
});

// the hysteresis through W: 11, 12, 11, 10 and 9 escalations with prev given, every other cell at its minimum
var prev = null, ws = [11, 12, 11, 10, 9].map(function (n) {
  prev = W.layout({ plans: [plan("zp", "zoo")], escalations: { open: escs(n, "zoo") }, home: { zoo: { letters: n } } }, null, { now: 1, readonly: true, prev: prev });
  return prev.index["home:zoo"].w;
});
assert.deepStrictEqual(ws, [32, 36, 36, 36, 32], "W follows the Escalations' hysteresis");

// a full Pen and a full field: P 23, F 22, W 48, no squeeze
var wp = W.layout({ plans: [plan("zp", "zoo")], wishes: { open: many(49, "w", "zoo") }, home: { zoo: { field: 80 } } }, null, { now: 1, readonly: true }), hp = wp.index["home:zoo"];
assert.deepStrictEqual([hp.cells.pen.w, hp.cells.growing.w, hp.w, hp.cells.pen.x + hp.cells.pen.w + 1, hp.cells.growing.x], [23, 22, 48, hp.x + 24, hp.x + 26], "both full, side by side: the Pen, its arm, the field");

// the sign at fx + 12 at every W, and the Escalations 11 a column at every Mailbox width
[0, 12, 34, 60, 88].forEach(function (n) {
  var h = W.layout({ plans: [plan("zp", "zoo")], escalations: { open: escs(n, "zoo") }, home: { zoo: { letters: n + 40 } } }, null, { now: 1, readonly: true }).index["home:zoo"];
  assert.deepStrictEqual([h.sign.x - h.x, h.sign.y, h.sign.w], [12, h.y, 8], "the sign at fx + 12, W " + h.w);
  assert.ok(!W.hit(h.sign, h.cells.escalation) && h.cells.barn.w >= 22, "the sign over the Barn's forecourt, W " + h.w);
});
[5, 6, 7, 8, 9].forEach(function (M) { W.ladderOf("escalation").forEach(function (E, k) { assert.strictEqual(W.capAt("escalation", E, M), 11 * (k + 1), "11 a column: E " + E + ", M " + M); }); });

console.log(JSON.stringify({ ok: true }));
