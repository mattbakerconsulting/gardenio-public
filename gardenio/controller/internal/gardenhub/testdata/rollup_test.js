/* The plot rollup at the project depth (gardenio/docs/superpowers/specs/2026-10-20-lands-cursor-grid-design.md section 6.6), run by
   TestPlotRollup:  node rollup_test.js <web dir>
   WORLD.plotRollup over planBeds: the frame rules (current beats closed beats blocked-and-nothing-moving beats review), the fill,
   the marks, the Other bed, the ninth-position crate, the empty plan's bar and the gardeners. Then ART.drawRollupPlate, drawRollupStamp
   and drawRollupMarks under a recording canvas at the spec's sizes: the 36 px floor, the cells at 48 and 66 px, the beds under the
   stamp from 90 px, everything inside the plate but the marks that ride its fence. Prints a JSON report. */
"use strict";
var path = require("path"), assert = require("assert");
var W = require(path.join(process.argv[2], "world.js")), ART = require(path.join(process.argv[2], "art.js"));
var fixture = require(path.join(__dirname, "farm_fixture.json"));
var report = {};

function counts(c) { var o = { in_progress: 0, review: 0, blocked: 0, open: 0, deferred: 0, closed: 0 }; Object.keys(c || {}).forEach(function (k) { o[k] = c[k]; }); return o; }
function tasks(id, c) {
  var out = [];
  ["in_progress", "review", "blocked", "open", "deferred"].forEach(function (s) { for (var i = 0; i < (c[s] || 0); i++) out.push({ id: id + "-" + s + i, title: s, type: i % 2 ? "bug" : "task", state: s, updated: 1 }); });
  return out;
}
function phase(id, c, status) { return { id: id, title: "Phase " + id, status: status || "open", counts: counts(c), tasks: tasks(id, c) }; }
function plotOf(plan, needs) {
  var plot = { id: plan.id, kind: "plot", x: 0, y: 0, w: 32, h: 32, plan: plan, needs: needs || 0 };
  var pb = W.planBeds(plan, plot); plot.beds = pb.beds; plot.crate = pb.crate;
  return plot;
}
function sum(phases, u) { var c = counts(); phases.concat(u ? [u] : []).forEach(function (p) { Object.keys(c).forEach(function (k) { c[k] += p.counts[k] || 0; }); }); return c; }
function plan(id, phases, u) { u = u || { id: "", title: "Other", counts: counts(), tasks: [] }; return { id: id, title: id, phases: phases, unphased: u, counts: sum(phases, u) }; }

/* ---------- 1. the frame rules, the fill and the marks ---------- */
var ph = [
  phase("p0", { closed: 3 }, "closed"),                        // done: green, ticked
  phase("p1", { blocked: 1, review: 1, closed: 1 }),           // the current phase: gold beats blocked and review
  phase("p2", { blocked: 1, review: 1, open: 2 }),             // blocked and nothing moving beats review: red
  phase("p3", { blocked: 1, review: 1, in_progress: 1 }),      // blocked but something moving: review's orange
  phase("p4", { in_progress: 2, open: 2, closed: 4 }),         // growing: plain frame, half full, a sprout
  phase("p5", {}),                                             // no tasks yet: soil
  phase("p6", { closed: 2 })                                   // every task done but the phase still open: closed, not current
];
var other = { id: "", title: "Other", counts: counts({ open: 1, review: 1 }), tasks: tasks("u", { open: 1, review: 1 }) };
var A = plotOf(plan("gd-A", ph, other), 2), ra = W.plotRollup(A, 3);
assert.strictEqual(ra.cells.length, 9, "one cell per position of the 3x3");
ra.cells.forEach(function (c, i) { assert.deepStrictEqual(c.at, W.bedAt(i), "cells in reading order at the beds' positions"); });
assert.deepStrictEqual(ra.cells.map(function (c) { return c.frame; }), ["L", "G", "e", "o", "k", "k", "L", "o", ""]);
assert.strictEqual(ra.cells.filter(function (c) { return c.frame === "G"; }).length, 1, "one current phase, the only gold frame");
assert.ok(ra.cells[0].closed && ra.cells[0].fill === 1 && !ra.cells[0].rock, "a closed phase is full and carries no marks");
assert.ok(ra.cells[6].closed, "a phase whose tasks are all done reads closed");
assert.deepStrictEqual([ra.cells[1].rock, ra.cells[1].fruit, ra.cells[1].sprout], [true, true, false]);
assert.deepStrictEqual([ra.cells[3].rock, ra.cells[3].fruit, ra.cells[3].sprout], [true, true, true]);
assert.strictEqual(ra.cells[4].fill, 0.5, "done / total"); assert.ok(ra.cells[4].sprout);
assert.strictEqual(ra.cells[5].fill, 0, "a phase with no tasks stays soil"); assert.ok(!ra.cells[5].empty && !ra.cells[5].sprout);
assert.ok(ra.cells[7].other && ra.cells[7].fruit && ra.cells[7].frame === "o", "the Other bed takes its position, never current");
assert.ok(ra.cells[8].empty, "a position with no bed stays the plate face");
assert.strictEqual(ra.cells[4].tasks, A.beds[4].cells, "the bed's own task cells, for the beds under the stamp");
assert.strictEqual(ra.needs, 2); assert.strictEqual(ra.gardeners, 3); assert.deepStrictEqual(ra.hats, []);
/* current beats closed: a closed phase after the current one stays green, and the current is the first not closed */
var B = W.plotRollup(plotOf(plan("gd-B", [phase("b0", { open: 1 }), phase("b1", { closed: 1 }, "closed")])), 0);
assert.deepStrictEqual(B.cells.slice(0, 2).map(function (c) { return c.frame; }), ["G", "L"]);

/* ---------- 2. the bar ---------- */
assert.deepStrictEqual(ra.bar.map(function (b) { return b.state + ":" + b.n; }), ["closed:10", "in_progress:3", "review:4", "blocked:3", "open:5"], "plan.counts in the bar's order, zero counts left out");
assert.strictEqual(ra.total, 25);
var E = W.plotRollup(plotOf({ id: "gd-E", title: "Empty", phases: [], unphased: { id: "", counts: {}, tasks: [] }, counts: counts() }), 0);
assert.deepStrictEqual([E.bar, E.total], [[], 0], "an empty plan: the bar's outline only");
assert.ok(E.cells.every(function (c) { return c.empty; }), "and no cells");
var E2 = W.plotRollup(plotOf({ id: "gd-E2", title: "Old doc", phases: [phase("e0", {})] }), 0);
assert.deepStrictEqual([E2.bar, E2.total], [[], 0], "a plan without counts or unphased reads as empty");

/* ---------- 3. the ninth-position crate ---------- */
var many = []; for (var i = 0; i < 11; i++) many.push(phase("m" + i, { open: 1 }));
var M = plotOf(plan("gd-M", many)), rm = W.plotRollup(M, 0);
assert.ok(M.crate, "past nine phases the plot has a crate");
assert.strictEqual(rm.cells[8].crate, 3, "the ninth position holds the +N phases crate (11 - 8)");
assert.ok(rm.cells.slice(0, 8).every(function (c) { return !c.crate && !c.empty; }));

/* ---------- 4. the gardeners: a count, or their looks in need order ---------- */
var looks = [{ hat: 1, pose: "work" }, { hat: 2 }, { hat: 3 }, { hat: 4 }, { hat: 5 }], rg = W.plotRollup(A, looks);
assert.deepStrictEqual([rg.gardeners, rg.hats, rg.tint], [5, [1, 2, 3, 4], ""], "four hats at most; none down or asking: no tint");
assert.deepStrictEqual(W.plotRollup(A, [{}]).hats, [8], "no hat: the default");
assert.strictEqual(W.plotRollup(A).gardeners, 0);
assert.strictEqual(W.plotHeads([{ pose: "down" }, { pose: "ask" }]).tint, "down", "the neediest (need order: down first) tints the badge");
assert.strictEqual(W.plotHeads([{ pose: "ask" }, { pose: "work" }]).tint, "ask");
assert.deepStrictEqual(W.plotHeads(2), { gardeners: 2, hats: [], tint: "" }, "a count alone");

/* ---------- 5. the layout: every plot of the fixture farm rolls up, cells where the beds are ---------- */
var world = W.layout(fixture, null, { now: 1792001000000, readonly: true }), plots = 0;
world.biomes.forEach(function (b) {
  b.plots.forEach(function (p) {
    plots++;
    var r = W.plotRollup(p, 0);
    p.beds.forEach(function (bd) { var c = r.cells[bd.at.r * 3 + bd.at.c]; assert.ok(!c.empty && c.other === bd.other && c.closed === bd.closed, "cell at its bed: " + bd.id); });
    assert.strictEqual(r.total, ["in_progress", "review", "blocked", "open", "deferred", "closed"].reduce(function (a, k) { return a + ((p.plan.counts || {})[k] || 0); }, 0));
  });
});
assert.ok(plots > 0, "the fixture farm has plots");
report.fixturePlots = plots;

/* ---------- 6. the paint, under a recording canvas ---------- */
function paint(side, r, night, marks) {
  var pal = ART.palette(night), out = [], cur = null, z = side / 512, s = 1 / z;
  var ctx = { fillRect: function (x, y, w, h) { [x, y, w, h].forEach(function (v) { assert.ok(isFinite(v), "finite rect"); }); var q = function (v) { return Math.round(v / s * 1e6) / 1e6; }; out.push({ c: cur, x: q(x), y: q(y), w: q(w), h: q(h) }); }, set fillStyle(v) { if (!/^#[0-9a-f]{6}$/i.test(v)) throw new Error("bad colour " + v); cur = v; } };
  if (marks === "all") ART.drawPlotRollup(ctx, pal, 0, 0, side, s, r, "sprout");
  else if (marks) ART.drawRollupMarks(ctx, pal, 0, 0, side, s, r);
  else { ART.drawRollupPlate(ctx, pal, 0, 0, side, s, "sprout"); ART.drawRollupStamp(ctx, pal, 0, 0, side, s, r); }
  return { pal: pal, rects: out, has: function (ch) { return out.some(function (o) { return o.c === pal[ch]; }); } };
}
var inside = function (p, side) { return p.rects.every(function (o) { return o.x >= -1e-6 && o.y >= -1e-6 && o.x + o.w <= side + 1e-6 && o.y + o.h <= side + 1e-6; }); };
[30, 48, 66, 99, 132].forEach(function (side) {
  [false, true].forEach(function (night) {
    var p = paint(side, ra, night);
    assert.ok(inside(p, side), "the stamp stays on the plate at " + side + " px");
    assert.strictEqual(p.has("G"), side >= 36, "the gold current frame from 36 px only (" + side + ")");
    assert.strictEqual(p.has("j"), side >= 36, "the tick from 36 px only (" + side + ")");
    assert.ok(p.has("R") && p.has("o") && p.has("c"), "the bar at every size");
    var flat = p.rects.some(function (o) { return o.c === p.pal.k && Math.abs(o.w - o.h) < 1e-6 && o.w > 0 && o.w < 1.5 && o.x > 4; });
    assert.strictEqual(p.has("s"), side >= 90, "the beds under the stamp from 90 px (" + side + ")");
    if (side >= 90) assert.ok(flat, "with the plan depth's task squares");
  });
});
var m48 = paint(48, rg, false, true), heads = m48.rects.filter(function (o) { return o.w === 5 && o.h === 5; });
assert.strictEqual(heads.length, 1, "the badge: one head"); assert.deepStrictEqual([heads[0].x, heads[0].y], [39, 41], "in the bottom-right corner, standing on the fence from inside");
assert.ok(m48.rects.some(function (o) { return o.x === 37 && o.y === 39 && o.w === 9 && o.h === 9; }), "at 1x a plate just wide enough for the head");
assert.ok(m48.rects.some(function (o) { return o.c === m48.pal.N && o.w === 7 && o.y === 40; }), "a plain face with none down or asking");
assert.ok(!m48.rects.some(function (o) { return o.w === 1 && o.h === 1 && o.y >= 39; }), "no count at 1x, where a 3x5 digit is a smudge (gd-Rq3bxDdnuZ): the plot's tip says it");
var m66 = paint(66, rg, false, true);
assert.ok(m66.rects.some(function (o) { return o.x === 38 && o.y === 48 && o.w === 26 && o.h === 18; }), "at 2x from a 64 px plot");
assert.ok(m66.rects.some(function (o) { return o.x === 42 && o.y === 52 && o.w === 10 && o.h === 10; }), "its head at 2x");
assert.ok(m66.rects.some(function (o) { return o.y === 52 && o.w === 2 && o.h === 2 && o.x >= 54 && o.x <= 58; }), "the count beside the head at 2x");
assert.ok(m66.has("G") && m66.rects.some(function (o) { return o.y === -4 && o.w === 13; }), "the !N plate on the top-right fence");
["down", "ask"].forEach(function (t, i) {
  var m = paint(48, W.plotHeads([{ hat: 1, pose: t }]), false, true);
  assert.ok(m.rects.some(function (o) { return o.c === m.pal[i ? "G" : "f"] && o.w === 7 && o.y === 40; }), "the face tinted for a gardener " + t);
});
assert.strictEqual(paint(66, W.plotRollup(plotOf(plan("gd-Q", [phase("q", { open: 1 })])), 0), false, true).rects.length, 0, "no need, no gardeners: no marks");
assert.strictEqual(paint(66, rg, false, "all").rects.length, paint(66, rg, false).rects.length + m66.rects.length, "drawPlotRollup is the plate, the stamp and the marks");
var e66 = paint(66, E, false), bar = e66.rects.filter(function (o) { return o.y >= 60; });
assert.deepStrictEqual(bar.map(function (o) { return o.c; }), [e66.pal.k, e66.pal.P], "the empty plan's bar is its outline only");
var c66 = paint(66, rm, false);
assert.ok(c66.has("b") && c66.has("B"), "the crate mark is two stacked b/B boxes");
report.ok = true;
console.log(JSON.stringify(report));
